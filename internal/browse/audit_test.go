package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/curbol/quarry/internal/tagstore"
)

// The paging window is request-controlled, so it has to survive values the UI never
// sends: a negative offset used to slice out of range and take the handler down.
func TestAssetsPagingClampsOutOfRangeOffsets(t *testing.T) {
	srv := testServer(t)
	total := getAssets(t, srv, "").Total

	for _, q := range []string{"offset=-1", "offset=-1000&limit=5", fmt.Sprintf("offset=%d", total+50)} {
		resp, err := http.Get(srv.URL + "/api/assets?" + q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		body := resp.Body
		code := resp.StatusCode
		body.Close()
		if code != http.StatusOK {
			t.Errorf("%s: status %d", q, code)
		}
	}
	if got := getAssets(t, srv, "offset=-1&limit=1").Offset; got != 0 {
		t.Errorf("negative offset resolved to %d, want 0", got)
	}
}

// Every tag write goes through one mutex and re-saves the store, so concurrent
// assignments must all land and the file on disk must match what the server holds.
func TestConcurrentTagAssignmentsAllPersist(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	fps := []string{"crc32:aa:1", "crc32:bb:2", "crc32:cc:3", "crc32:dd:4"}
	tags := []string{"hero", "prop", "vfx", "wip"}

	var wg sync.WaitGroup
	for _, fp := range fps {
		for _, tag := range tags {
			wg.Add(1)
			go func(fp, tag string) {
				defer wg.Done()
				body, _ := json.Marshal(map[string]any{
					"fingerprints": []string{fp}, "tag": tag, "on": true,
				})
				resp, err := http.Post(srv.URL+"/api/assign", "application/json", bytes.NewReader(body))
				if err != nil {
					t.Errorf("assign %s/%s: %v", fp, tag, err)
					return
				}
				resp.Body.Close()
			}(fp, tag)
		}
	}
	wg.Wait()

	saved, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fp := range fps {
		got := saved.TagsFor(fp)
		if len(got) != len(tags) {
			t.Errorf("%s has tags %v, want all %v", fp, got, tags)
		}
	}
}

// The limit is request-controlled too, and unlike the offset it is not clamped into
// range but replaced with the default. Values the UI never sends still have to come
// back as a well-formed page.
func TestAssetsPagingHandlesOutOfRangeLimits(t *testing.T) {
	srv := testServer(t)
	total := getAssets(t, srv, "").Total
	if total == 0 {
		t.Fatal("fixture library is empty")
	}
	for _, q := range []string{"limit=-1", "limit=0", "limit=99999", "limit=abc"} {
		r := getAssets(t, srv, q)
		if r.Total != total {
			t.Errorf("%s: total = %d, want %d", q, r.Total, total)
		}
		if len(r.Items) != total {
			t.Errorf("%s: got %d items, want the whole %d-asset library on one page", q, len(r.Items), total)
		}
	}
	if got := len(getAssets(t, srv, "limit=1").Items); got != 1 {
		t.Errorf("limit=1 returned %d items", got)
	}
}

// A patch is two edits — a rename, then a color. Validating the color only when it
// reaches the store let the rename land and then answer "rejected", so the palette
// held a name the file never got and a later save would have persisted it.
func TestRejectedPatchLeavesNothingBehindInMemory(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	post(t, srv, "/api/tags", `{"id":"hero","color":"#112233"}`, http.StatusOK)
	post(t, srv, "/api/assign", `{"fingerprints":["crc32:aa:1"],"tag":"hero","on":true}`, http.StatusOK)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/api/tags", map[string]any{
		"id": "hero", "newId": "villain", "color": "not-a-color",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch with a bad color status = %d, want 400", resp.StatusCode)
	}

	var p paletteResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
	ids := []string{}
	for _, tg := range p.Tags {
		ids = append(ids, tg.ID)
	}
	if len(ids) != 1 || ids[0] != "hero" {
		t.Errorf("palette in memory = %v, want just the unchanged hero", ids)
	}

	saved, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.TagsFor("crc32:aa:1"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("assignment on disk = %v, want [hero]", got)
	}
}

// A root-motion sibling is folded into its in-place card and can never come back from
// /api/assets under any filter. Counting it in the facets left a number the user could
// not reach: clicking "animation (4)" would show three.
func TestFacetCountsExcludeSuppressedRootMotionSiblings(t *testing.T) {
	// The pack name is what promotes these to the animation category, which is what
	// makes the group eligible for root-motion pairing at all.
	srv := serverWith(t, func(mk func(...string) string) {
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk.glb"), []byte("GLBBYTES"), 0o644)
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk_RM.glb"), []byte("GLBBYTESRM"), 0o644)
	})

	r := getAssets(t, srv, "")
	if r.Total != 1 {
		t.Fatalf("grid shows %d cards, want the RM sibling folded into one", r.Total)
	}
	var vendorCount int
	for _, f := range r.Facets.Vendors {
		if f.Value == "quaternius" {
			vendorCount = f.Count
		}
	}
	if vendorCount != r.Total {
		t.Errorf("vendor facet counts %d assets but the query returns %d; the difference is unreachable",
			vendorCount, r.Total)
	}
	// Selecting the facet has to produce exactly what it advertised.
	if got := getAssets(t, srv, "vendor=quaternius").Total; got != vendorCount {
		t.Errorf("filtering by the facet returned %d, want the advertised %d", got, vendorCount)
	}
}

// Paging is answered from a memoized result set, so every page has to describe the
// same query the first one did: a consistent total, no repeats, and no gaps.
func TestPagingIsConsistentAcrossPages(t *testing.T) {
	srv := testServer(t)
	whole := getAssets(t, srv, "limit=500")
	if whole.Total < 4 {
		t.Fatalf("fixture has %d assets, too few to page", whole.Total)
	}

	seen := map[string]bool{}
	var order []string
	for offset := 0; offset < whole.Total; offset += 2 {
		page := getAssets(t, srv, fmt.Sprintf("offset=%d&limit=2", offset))
		if page.Total != whole.Total {
			t.Fatalf("page at %d reports total %d, want %d", offset, page.Total, whole.Total)
		}
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Errorf("asset %s appeared on more than one page", it.Name)
			}
			seen[it.ID] = true
			order = append(order, it.ID)
		}
	}
	if len(order) != whole.Total {
		t.Errorf("paging yielded %d assets, want %d", len(order), whole.Total)
	}
	for i, it := range whole.Items {
		if i < len(order) && order[i] != it.ID {
			t.Errorf("paged order diverges from the single-page order at %d", i)
			break
		}
	}
}

// The memoized result set carries the tags each card had when it was built, so a tag
// write has to retire it — otherwise the grid keeps serving the palette from before
// the edit until something else changes the query.
func TestResultsReflectATagWrittenSinceTheLastQuery(t *testing.T) {
	srv, _ := enabledServer(t)
	// Warm the memo for the unfiltered query, then tag one of its cards. The re-query
	// has to come next: only one result set is held, so any query in between would
	// evict the entry and hide a missing invalidation.
	fp := firstFingerprint(t, srv)
	post(t, srv, "/api/assign", `{"fingerprints":["`+fp+`"],"tag":"hero","on":true}`, http.StatusOK)

	var tagged int
	for _, it := range taggedAssets(t, srv, "").Items {
		for _, tg := range it.Tags {
			if tg == "hero" {
				tagged++
			}
		}
	}
	if tagged != 1 {
		t.Errorf("the unfiltered grid shows the tag on %d cards, want 1", tagged)
	}

	// And removing it has to be visible on the same query for the same reason.
	post(t, srv, "/api/assign", `{"fingerprints":["`+fp+`"],"tag":"hero","on":false}`, http.StatusOK)
	for _, it := range taggedAssets(t, srv, "").Items {
		if len(it.Tags) != 0 {
			t.Errorf("%s still shows %v after the tag was removed", it.Name, it.Tags)
		}
	}
}

func firstFingerprint(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	for _, it := range taggedAssets(t, srv, "").Items {
		if len(it.Fingerprints) > 0 {
			return it.Fingerprints[0]
		}
	}
	t.Fatal("no fingerprinted asset in the fixture")
	return ""
}
