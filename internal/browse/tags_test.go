package browse

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/quarry/internal/tagstore"
)

type paletteResp struct {
	Enabled bool
	Tags    []struct {
		ID, Color string
		Count     int
	}
}

type assignResp struct {
	Tags    []string
	Palette paletteResp
}

type taggedItem struct {
	Name         string
	Fingerprints []string
	Tags         []string
	Related      []string
}

type taggedAssetsResp struct {
	Total int
	Items []taggedItem
}

// enabledServer builds a small library and an enabled tag server over a temp tag
// store, returning the httptest server and the tag-store path on disk.
func enabledServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	return taggedLibrary(t, func(mk func(...string) string) {
		writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Heart.fbx": "FBXHEART",
		})
		os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	})
}

// doJSON marshals body (nil sends no body and no content-type) to a full URL.
func doJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	if body == nil {
		return httpDo(t, method, url, "", "")
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return httpDo(t, method, url, "application/json", string(b))
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func taggedAssets(t *testing.T, srv *httptest.Server, qs string) taggedAssetsResp {
	t.Helper()
	resp := doJSON(t, "GET", srv.URL+"/api/assets?"+qs, nil)
	var out taggedAssetsResp
	decode(t, resp, &out)
	return out
}

func itemByName(t *testing.T, srv *httptest.Server, qs, name string) taggedItem {
	t.Helper()
	for _, it := range taggedAssets(t, srv, qs).Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("item %q not found (qs=%q)", name, qs)
	return taggedItem{}
}

func TestTagsDisabled(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		writeZip(t, mk("synty", "P", "P_SourceFiles_v3.zip"), map[string]string{"SourceFiles/A.fbx": "AAA"})
	})

	var p paletteResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
	if p.Enabled {
		t.Error("tags should report disabled with no tagsPath")
	}
	resp := doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{"x"}, "tag": "hero", "on": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("assign while disabled status = %d, want 409", resp.StatusCode)
	}
}

func TestTagCreateAndPalette(t *testing.T) {
	srv, _ := enabledServer(t)
	var p paletteResp
	decode(t, doJSON(t, "POST", srv.URL+"/api/tags", map[string]any{"id": "hero", "color": "#E11D48"}), &p)
	if !p.Enabled || len(p.Tags) != 1 || p.Tags[0].ID != "hero" || p.Tags[0].Color != "#e11d48" {
		t.Fatalf("palette after create = %+v", p)
	}
	// Invalid color rejected.
	resp := doJSON(t, "POST", srv.URL+"/api/tags", map[string]any{"id": "bad", "color": "red"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad color status = %d, want 400", resp.StatusCode)
	}
}

func TestAssignToggleAndDTO(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	if len(heart.Fingerprints) == 0 {
		t.Fatal("Heart.fbx has no fingerprints in the DTO")
	}
	if len(heart.Tags) != 0 {
		t.Fatalf("Heart.fbx starts with tags: %v", heart.Tags)
	}

	// Assign a brand-new tag via assign: it is auto-created and the response palette
	// carries its default color (so the client can render the sliver immediately).
	var ar assignResp
	decode(t, doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "fresh", "on": true}), &ar)
	if len(ar.Tags) != 1 || ar.Tags[0] != "fresh" {
		t.Fatalf("assign response tags = %v, want [fresh]", ar.Tags)
	}
	var freshColor string
	for _, tg := range ar.Palette.Tags {
		if tg.ID == "fresh" {
			freshColor = tg.Color
		}
	}
	if freshColor != tagstore.DefaultColor("fresh") {
		t.Errorf("auto-created tag color = %q, want default %q", freshColor, tagstore.DefaultColor("fresh"))
	}

	// The DTO now reports the tag on the card.
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 1 || got[0] != "fresh" {
		t.Errorf("Heart.fbx tags after assign = %v, want [fresh]", got)
	}

	// Persisted to disk.
	reloaded, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(reloaded.FingerprintsByTag()["fresh"]); n != 1 {
		t.Errorf("tag not persisted: fresh is on %d fingerprints, want 1", n)
	}

	// Toggle off removes it.
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "fresh", "on": false}).Body.Close()
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 0 {
		t.Errorf("Heart.fbx tags after toggle-off = %v, want none", got)
	}
}

func TestRenameRecolorDelete(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "wip", "on": true}).Body.Close()

	// Rename wip -> in-progress and recolor in one PATCH.
	var p paletteResp
	decode(t, doJSON(t, "PATCH", srv.URL+"/api/tags", map[string]any{"id": "wip", "newId": "in-progress", "color": "#00ff00"}), &p)
	if len(p.Tags) != 1 || p.Tags[0].ID != "in-progress" || p.Tags[0].Color != "#00ff00" {
		t.Fatalf("palette after rename+recolor = %+v", p)
	}
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 1 || got[0] != "in-progress" {
		t.Errorf("assignment not rewritten by rename: %v", got)
	}

	// Delete removes it from the palette and the card.
	decode(t, doJSON(t, "DELETE", srv.URL+"/api/tags", map[string]any{"id": "in-progress"}), &p)
	if len(p.Tags) != 0 {
		t.Errorf("palette after delete = %+v", p)
	}
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 0 {
		t.Errorf("Heart.fbx still tagged after delete: %v", got)
	}
}

func TestTagFilterAndOr(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	sword := itemByName(t, srv, "q=Sword", "Sword.glb")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "a", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": sword.Fingerprints, "tag": "b", "on": true}).Body.Close()

	// OR: either tag matches → both.
	or := taggedAssets(t, srv, "tag=a&tag=b&tagmode=or")
	if or.Total != 2 {
		t.Errorf("OR filter total = %d, want 2", or.Total)
	}
	// AND: needs both on one card → neither (each has only one).
	and := taggedAssets(t, srv, "tag=a&tag=b&tagmode=and")
	if and.Total != 0 {
		t.Errorf("AND filter total = %d, want 0", and.Total)
	}
	// Single tag narrows to its asset.
	one := taggedAssets(t, srv, "tag=a")
	if one.Total != 1 || one.Items[0].Name != "Heart.fbx" {
		t.Errorf("single-tag filter = %+v", one)
	}
}

// A card that groups two distinct fingerprints (same normalized name + size,
// different bytes) is a single tag unit: its Tags is the union over both, so an AND
// filter matches on the union even though no single file carries both tags.
func TestCardUnionAndFilter(t *testing.T) {
	srv, _ := taggedLibrary(t, func(mk func(...string) string) {
		// Two "Coin.fbx" of equal byte length but different content in different packs:
		// same group key (name+size), different fingerprints (crc differs).
		writeZip(t, mk("synty", "A", "A_SourceFiles_v3.zip"), map[string]string{"SourceFiles/Coin.fbx": "COINDAT1"})
		writeZip(t, mk("synty", "B", "B_SourceFiles_v3.zip"), map[string]string{"SourceFiles/Coin.fbx": "COINDAT2"})
	})

	coin := itemByName(t, srv, "q=Coin", "Coin.fbx")
	if len(coin.Fingerprints) != 2 {
		t.Fatalf("Coin card fingerprints = %v, want 2 distinct", coin.Fingerprints)
	}
	// hero on fp1 only, wip on fp2 only.
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{coin.Fingerprints[0]}, "tag": "hero", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{coin.Fingerprints[1]}, "tag": "wip", "on": true}).Body.Close()

	// The card's union carries both, so hero AND wip matches it.
	and := taggedAssets(t, srv, "q=Coin&tag=hero&tag=wip&tagmode=and")
	if and.Total != 1 {
		t.Fatalf("card-union AND total = %d, want 1", and.Total)
	}
	got := and.Items[0].Tags
	if len(got) != 2 || got[0] != "hero" || got[1] != "wip" {
		t.Errorf("card union tags = %v, want [hero wip]", got)
	}
}

// A body big enough to exhaust memory must be cut off rather than decoded.
func TestTagWritesBoundTheRequestBody(t *testing.T) {
	srv, _ := enabledServer(t)
	huge := `{"fingerprints":["` + strings.Repeat("a", 4<<20) + `"],"tag":"x","on":true}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/assign", strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("an oversized body was accepted with %d", resp.StatusCode)
	}
}

// When the save fails the mutation must not survive in memory: the UI would keep
// reporting a tag that does not exist on disk until a restart silently undid it.
// The one case where memory and disk genuinely diverge: the save was refused, and the
// reload meant to undo the in-memory edit was refused too. Nothing can put that right,
// so it has to be named in the response — the client has just been told the write did
// not land, and the UI would otherwise keep showing an edit that is not real with no
// way to know. Rewriting the file under the server does both at once: the save sees a
// file that changed since it was loaded, and the reload sees content Load refuses.
func TestAFailedReloadAfterARefusedSaveIsNamed(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	post(t, srv, "/api/assign", `{"fingerprints":["crc32:1:1"],"tag":"hero","on":true}`, http.StatusOK)

	if err := os.WriteFile(tagsPath, []byte("unknown_key = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := post(t, srv, "/api/assign", `{"fingerprints":["crc32:1:1"],"tag":"villain","on":true}`, http.StatusConflict)
	if !strings.Contains(string(body), "reloading the store from disk failed") {
		t.Errorf("response = %s, want it to say the store could not be reloaded either", body)
	}
	if !strings.Contains(string(body), "may be ahead of the file") {
		t.Errorf("response = %s, want it to warn that what is shown is not what is stored", body)
	}
}

func TestFailedSaveDoesNotLeaveMemoryAheadOfDisk(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	post(t, srv, "/api/assign", `{"fingerprints":["crc32:1:1"],"tag":"hero","on":true}`, http.StatusOK)

	// Make the tag store unwritable by turning its directory read-only.
	dir := filepath.Dir(tagsPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the tags dir read-only: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	post(t, srv, "/api/assign", `{"fingerprints":["crc32:1:1"],"tag":"villain","on":true}`, http.StatusInternalServerError)

	os.Chmod(dir, 0o700)
	resp, err := http.Get(srv.URL + "/api/tags")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "villain") {
		t.Errorf("a tag that failed to save is still reported in memory: %s", b)
	}
}

// post returns the response body, so a test can assert on what a rejection actually
// told the client rather than only on its status.
func post(t *testing.T, srv *httptest.Server, path, body string, wantStatus int) []byte {
	t.Helper()
	resp := request(t, srv, http.MethodPost, path, "application/json", body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s = %d, want %d: %s", path, resp.StatusCode, wantStatus, b)
	}
	return b
}
