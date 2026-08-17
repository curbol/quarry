package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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
