package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
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

// writeEndpoints is every route that mutates the tag store, with a body each accepts.
// The protections below are per-handler, so testing one of them proves nothing about
// the other four: a new handler that forgot decodeJSON or requireEnabled would ship
// green against a single-endpoint test.
var writeEndpoints = []struct {
	method, path, body string
}{
	{http.MethodPost, "/api/tags", `{"id":"hero","color":"#112233"}`},
	{http.MethodPatch, "/api/tags", `{"id":"hero","newId":"champion"}`},
	{http.MethodDelete, "/api/tags", `{"id":"hero"}`},
	{http.MethodPost, "/api/assign", `{"fingerprints":["crc32:1:1"],"tag":"hero","on":true}`},
	{http.MethodPost, "/api/link", `{"fingerprints":["crc32:1:1","crc32:2:2"],"on":true}`},
}

func request(t *testing.T, srv *httptest.Server, method, path, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// browse has no session by design, so its write surface is reachable from any page the
// user has open. Requiring a JSON content-type forces a CORS preflight the server never
// answers, which is what closes the drive-by path — on every endpoint, not just one.
func TestEveryWriteEndpointRequiresJSONContentType(t *testing.T) {
	srv, _ := enabledServer(t)
	for _, e := range writeEndpoints {
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
			resp := request(t, srv, e.method, e.path, ct, e.body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s with content-type %q = %d, want 415", e.method, e.path, ct, resp.StatusCode)
			}
		}
		// The same request with the right content-type has to work, or the check above
		// would pass for a route that is simply broken.
		resp := request(t, srv, e.method, e.path, "application/json", e.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s with application/json = %d, want 200", e.method, e.path, resp.StatusCode)
		}
	}
}

// With no tag store there is nothing to write to, and every endpoint has to say so
// rather than accept the edit into a store that is never persisted.
func TestEveryWriteEndpointRefusesWhenTaggingIsDisabled(t *testing.T) {
	srv := serverWith(t, fixtureLibrary(t))
	for _, e := range writeEndpoints {
		resp := request(t, srv, e.method, e.path, "application/json", e.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s while disabled = %d, want 409", e.method, e.path, resp.StatusCode)
		}
	}
}

// The forced-preflight defence only stops a cross-origin page. A domain whose DNS is
// re-pointed at 127.0.0.1 is same-origin with quarry — no preflight, and it can read
// every response. The Host header is what still carries the attacker's domain.
func TestHostGuardRejectsARebindingHost(t *testing.T) {
	guarded := guardHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(guarded)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		host string
		want int
	}{
		{"", http.StatusOK}, // whatever httptest dialled, i.e. 127.0.0.1:port
		{"localhost:8788", http.StatusOK},
		{"127.0.0.1:8788", http.StatusOK},
		{"[::1]:8788", http.StatusOK},
		{"localhost", http.StatusOK},
		{"evil.example:8788", http.StatusForbidden},
		{"quarry.attacker.test", http.StatusForbidden},
		{"192.168.1.9:8788", http.StatusForbidden},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.host != "" {
			req.Host = tc.host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q = %d, want %d", tc.host, resp.StatusCode, tc.want)
		}
	}
}

// Facet counts are a promise: click a value and get that many rows. Results are cards,
// not assets, so a pack shipping one file in both a zip and a unitypackage produces two
// assets and one card — counting assets advertised roughly double what clicking
// returned. The earlier fixture happened to make grouping a no-op, which is why this
// went unseen.
func TestFacetCountsAreReachableWhenCopiesGroup(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		// The same file in two archives of one pack: two assets, one card.
		writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Heart.fbx": "FBXHEART",
		})
		writeUnity(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/Foo/Heart.fbx", asset: "FBXHEART"},
		})
		os.WriteFile(mk("other", "Pack", "Rock.fbx"), []byte("ROCK"), 0o644)
	})

	var first assetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=1", nil), &first)

	check := func(kind, param string, values []struct {
		Value string
		Count int
	}) {
		for _, f := range values {
			if f.Value == "" {
				continue
			}
			var got assetsResp
			decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=500&"+param+"="+url.QueryEscape(f.Value), nil), &got)
			if got.Total != f.Count {
				t.Errorf("%s %q advertises %d but filtering returns %d", kind, f.Value, f.Count, got.Total)
			}
		}
	}
	check("vendor", "vendor", first.Facets.Vendors)
	check("category", "type", first.Facets.Categories)
	check("variant", "variant", first.Facets.Variants)
}

// Groups merge transitively: linking {A,B} then {B,C} yields {A,B,C}. The store's own
// tests cover the merge; this pins that it survives the HTTP layer, which is the only
// way a user ever reaches it.
func TestLinkMergesTransitivelyOverHTTP(t *testing.T) {
	srv, _ := taggedLibrary(t, func(mk func(...string) string) {
		writeZip(t, mk("synty", "P", "P_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Frame.fbx": "FRAME",
			"SourceFiles/Fill.fbx":  "FILLX",
			"SourceFiles/Trim.fbx":  "TRIMX",
		})
	})

	var all taggedAssetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=50", nil), &all)
	fp := map[string]string{}
	for _, it := range all.Items {
		if len(it.Fingerprints) > 0 {
			fp[it.Name] = it.Fingerprints[0]
		}
	}
	for _, n := range []string{"Frame.fbx", "Fill.fbx", "Trim.fbx"} {
		if fp[n] == "" {
			t.Fatalf("%s has no fingerprint; cards: %+v", n, fp)
		}
	}

	post(t, srv, "/api/link", `{"fingerprints":["`+fp["Frame.fbx"]+`","`+fp["Fill.fbx"]+`"],"on":true}`, http.StatusOK)
	post(t, srv, "/api/link", `{"fingerprints":["`+fp["Fill.fbx"]+`","`+fp["Trim.fbx"]+`"],"on":true}`, http.StatusOK)

	// Frame was never linked to Trim directly; the merge is what makes it a companion.
	related := relatedItems(t, srv, []string{fp["Frame.fbx"]})
	got := map[string]bool{}
	for _, it := range related.Items {
		got[it.Name] = true
	}
	if !got["Trim.fbx"] {
		t.Errorf("related to Frame = %v, want Trim.fbx via the transitive merge", got)
	}
	if !got["Fill.fbx"] {
		t.Errorf("related to Frame = %v, want the direct companion Fill.fbx too", got)
	}
	if got["Frame.fbx"] {
		t.Error("a card is its own companion")
	}
}

// Paging over a library bigger than both the default page and the cap, so "clamped to
// maxLimit" and "replaced with the default" are actually distinguishable — the previous
// fixture had a handful of assets, where every hypothesis produces the same answer.
func TestAssetsPagingOverALibraryBiggerThanTheLimits(t *testing.T) {
	const total = 600
	srv := serverWith(t, func(mk func(...string) string) {
		entries := map[string]string{}
		for i := 0; i < total; i++ {
			entries[fmt.Sprintf("SourceFiles/Asset%04d.fbx", i)] = fmt.Sprintf("BODY%04d", i)
		}
		writeZip(t, mk("synty", "P", "P_SourceFiles_v3.zip"), entries)
	})

	for _, tc := range []struct {
		name       string
		qs         string
		wantOffset int
		wantItems  int
	}{
		{"default page", "", 0, defaultLimit},
		{"explicit limit", "&limit=50", 0, 50},
		{"limit at the cap", fmt.Sprintf("&limit=%d", maxLimit), 0, maxLimit},
		{"limit past the cap falls back to the default", fmt.Sprintf("&limit=%d", maxLimit+1), 0, defaultLimit},
		{"negative limit falls back to the default", "&limit=-1", 0, defaultLimit},
		{"zero limit falls back to the default", "&limit=0", 0, defaultLimit},
		{"non-numeric limit falls back to the default", "&limit=lots", 0, defaultLimit},
		{"negative offset clamps to the start", "&offset=-5&limit=10", 0, 10},
		{"offset past the end clamps to the end", fmt.Sprintf("&offset=%d&limit=10", total+50), total, 0},
		{"last partial page", fmt.Sprintf("&offset=%d&limit=100", total-10), total - 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got assetsResp
			decode(t, doJSON(t, "GET", srv.URL+"/api/assets?group=0"+tc.qs, nil), &got)
			if got.Total != total {
				t.Errorf("total = %d, want %d", got.Total, total)
			}
			if got.Offset != tc.wantOffset {
				t.Errorf("offset = %d, want %d", got.Offset, tc.wantOffset)
			}
			if len(got.Items) != tc.wantItems {
				t.Errorf("items = %d, want %d", len(got.Items), tc.wantItems)
			}
		})
	}
}
