package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/curbol/quarry/internal/assetindex"
	"github.com/curbol/quarry/internal/tagstore"
)

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

// The browser produces reads and writes at once: the grid re-queries while a tag click
// is still in flight. Only that shape reaches the read path at all — the write-only
// test above never calls resolveTagsLocked or resolveRelatedLocked, which read the
// store under a lock decorate takes on their behalf. Under -race this is what would
// catch a store read that lost its lock, or a card decorated outside decorate.
func TestConcurrentQueriesAndTagWrites(t *testing.T) {
	srv, _ := enabledServer(t)
	fps := []string{"crc32:aa:1", "crc32:bb:2", "crc32:cc:3"}

	post := func(path string, body any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// A link group makes resolveRelatedLocked do real work: it short-circuits on a
	// store with no groups at all, which is every other test here.
	post("/api/link", map[string]any{"fingerprints": fps[:2], "on": true})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				qs := []string{"", "q=heart", "tag=hero&includeRelated=1", "sort=size"}[(i+n)%4]
				resp, err := http.Get(srv.URL + "/api/assets?" + qs)
				if err != nil {
					t.Errorf("query %q: %v", qs, err)
					return
				}
				var out assetsResp
				err = json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				if err != nil {
					t.Errorf("query %q: %v", qs, err)
					return
				}
			}
		}(i)
	}
	for _, fp := range fps {
		for _, tag := range []string{"hero", "prop"} {
			wg.Add(1)
			go func(fp, tag string) {
				defer wg.Done()
				post("/api/assign", map[string]any{"fingerprints": []string{fp}, "tag": tag, "on": true})
				post("/api/link", map[string]any{"fingerprints": fps, "on": true})
				post("/api/assign", map[string]any{"fingerprints": []string{fp}, "tag": tag, "on": false})
			}(fp, tag)
		}
	}
	wg.Wait()
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

// writeBodies gives each write route a body it accepts. The routes themselves come
// from the server (see writeEndpoints), so this only has to answer "what does it
// take", never "which routes are there".
var writeBodies = map[string]string{
	"POST /api/tags":   `{"id":"hero","color":"#112233"}`,
	"PATCH /api/tags":  `{"id":"hero","newId":"champion"}`,
	"DELETE /api/tags": `{"id":"hero"}`,
	"POST /api/assign": `{"fingerprints":["crc32:1:1"],"tag":"hero","on":true}`,
	"POST /api/link":   `{"fingerprints":["crc32:1:1","crc32:2:2"],"on":true}`,
}

type writeEndpoint struct{ method, path, body string }

// writeEndpoints pairs every route the server registers as a write with a body it
// accepts. The routes are read off the server rather than restated here, so a handler
// added to the mux arrives in the two tests below as well: both guards are per-handler,
// and a route neither covers is an open write surface. A route with no body listed
// fails rather than being quietly skipped.
func writeEndpoints(t *testing.T) []writeEndpoint {
	t.Helper()
	var out []writeEndpoint
	for _, r := range (&server{}).writeRoutes() {
		key := r.method + " " + r.pattern
		body, ok := writeBodies[key]
		if !ok {
			t.Fatalf("write route %s has no body in writeBodies; add one so the guards are exercised against it", key)
		}
		out = append(out, writeEndpoint{r.method, r.pattern, body})
	}
	return out
}

// browse has no session by design, so its write surface is reachable from any page the
// user has open. Requiring a JSON content-type forces a CORS preflight the server never
// answers, which is what closes the drive-by path — on every endpoint, not just one.
func TestEveryWriteEndpointRequiresJSONContentType(t *testing.T) {
	srv, _ := enabledServer(t)
	for _, e := range writeEndpoints(t) {
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

		// A charset parameter is what a fetch with an explicit encoding sends, so the
		// gate has to admit it: tightening the check to an exact match would reject real
		// clients and still ship green. Asserted as "the gate let it through" rather
		// than on the outcome, because these bodies are spent by the call above.
		resp = request(t, srv, e.method, e.path, "application/json; charset=utf-8", e.body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnsupportedMediaType {
			t.Errorf("%s %s with a charset parameter was refused as the wrong media type", e.method, e.path)
		}
	}
}

// With no tag store there is nothing to write to, and every endpoint has to say so
// rather than accept the edit into a store that is never persisted.
func TestEveryWriteEndpointRefusesWhenTaggingIsDisabled(t *testing.T) {
	srv := serverWith(t, fixtureLibrary(t))
	for _, e := range writeEndpoints(t) {
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
		// An FQDN written absolute. The dot sits at the end of the host, which is the
		// middle of the header once a port is on it — and a listener always has a port,
		// so a trim that runs before the split never fires and this 403s.
		{"localhost.", http.StatusOK},
		{"localhost.:8788", http.StatusOK},
		{"127.0.0.1.:8788", http.StatusOK},
		{"evil.example:8788", http.StatusForbidden},
		{"evil.example.:8788", http.StatusForbidden},
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
		{"a wildly negative offset clamps too", "&offset=-100000&limit=5", 0, 5},
		{"non-numeric offset falls back to the start", "&offset=nowhere&limit=5", 0, 5},
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

// Pairing groups by (vendor, pack, base) while cards group by name and size, so one
// card spans packs and the copy owning the RM sibling need not be the representative.
// Reading the sibling off the representative alone left the card with no toggle while
// its RM stayed hidden — the file unreachable in browse from any card.
func TestGroupedCardKeepsTheRootMotionSiblingOfAnyCopy(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		// Pack A ships the in-place animation and its RM sibling.
		writeZip(t, mk("synty", "A_Animations", "A_SourceFiles.zip"), map[string]string{
			"SourceFiles/Walk.fbx":    "WALKBYTES",
			"SourceFiles/Walk_RM.fbx": "WALKRMBYTE",
		})
		// Pack B ships the same in-place animation only, as a unitypackage whose
		// preview outranks pack A's copy for the representative slot.
		writeUnity(t, mk("synty", "B_Animations", "B_Unity_2022_3.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/B/Walk.fbx", asset: "WALKBYTES", preview: true},
		})
	})

	out := getAssets(t, srv, "limit=200")
	visible := map[string]bool{}
	walk := -1
	for i, it := range out.Items {
		visible[it.Name] = true
		if it.Name == "Walk.fbx" {
			walk = i
		}
	}
	if walk < 0 {
		t.Fatal("no Walk.fbx card")
	}
	if got := out.Items[walk].Count; got != 2 {
		t.Fatalf("Walk.fbx card has %d copies, want 2 (the fixture must group them)", got)
	}
	if out.Items[walk].RootMotionID == "" {
		t.Error("the card carries no rootMotionId, so the lightbox shows no root-motion toggle")
		if !visible["Walk_RM.fbx"] {
			t.Error("and Walk_RM.fbx is suppressed from the grid, so the file is unreachable in browse")
		}
	}
}

// The facet counts and the type filter have to agree about what one card is. Category
// comes from a file's path, so a card's copies can classify differently; counting only
// the representative's advertised a zero that ?type= returned results for.
func TestFacetCategoryCountsCoverEveryCopy(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		writeZip(t, mk("synty", "P", "P_SourceFiles.zip"), map[string]string{
			"SourceFiles/Icons/Gem.png": "PNGBYTES",
		})
		writeUnity(t, mk("synty", "P", "P_Unity_2022_3.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/P/Textures/Gem.png", asset: "PNGBYTES"},
		})
	})

	counts := map[string]int{}
	for _, f := range getAssets(t, srv, "limit=200").Facets.Categories {
		counts[f.Value] = f.Count
	}
	for _, typ := range []string{"ui", "texture"} {
		if got := getAssets(t, srv, "limit=200&type="+typ).Total; counts[typ] != got {
			t.Errorf("facet advertises %s:%d but ?type=%s returns %d", typ, counts[typ], typ, got)
		}
	}
}

// The store is meant to be hand-edited and committed, so an edit arriving from an
// editor, a git checkout, or a second quarry sharing the user-wide store is real. A
// save that would overwrite one is refused, and the refusal has to reach the client:
// the rejected edit must be gone from what the UI then renders, not merely absent
// from disk while memory still shows it.
func TestAnEditRefusedAsStaleLeavesNeitherDiskNorMemoryAhead(t *testing.T) {
	srv, tagsPath := enabledServer(t)

	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	resp := doJSON(t, "POST", srv.URL+"/api/tags", map[string]any{"id": "mine", "color": "#e11d48"})
	resp.Body.Close()

	// Someone else rewrites the store between that load and the next save.
	external, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := external.Define("theirs", "#0ea5e9"); err != nil {
		t.Fatal(err)
	}
	if err := external.Save(tagsPath); err != nil {
		t.Fatal(err)
	}

	resp = doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
		"fingerprints": heart.Fingerprints, "tag": "mine", "on": true,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("assign over an externally edited store = %d, want 409", resp.StatusCode)
	}

	var p paletteResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
	ids := map[string]bool{}
	for _, tg := range p.Tags {
		ids[tg.ID] = true
	}
	if !ids["theirs"] {
		t.Error("the external edit is missing from the palette; the server did not reload from disk")
	}
	after := itemByName(t, srv, "q=Heart", "Heart.fbx")
	for _, tg := range after.Tags {
		if tg == "mine" {
			t.Error("the rejected assignment is still on the card; memory is ahead of the file")
		}
	}
}

// Expansion relaxes only the tag filter. Every other facet and the text search still
// apply, so a companion the query itself excludes must not be folded back in.
func TestIncludeRelatedStillHonoursTheNonTagFilters(t *testing.T) {
	srv, _ := enabledServer(t)
	items := taggedAssets(t, srv, "limit=50").Items
	var heartFP, swordFP []string
	for _, it := range items {
		switch it.Name {
		case "Heart.fbx":
			heartFP = it.Fingerprints
		case "Sword.glb":
			swordFP = it.Fingerprints
		}
	}
	if len(heartFP) == 0 || len(swordFP) == 0 {
		t.Fatal("fixture did not produce both assets with fingerprints")
	}

	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{
		"fingerprints": append(append([]string{}, heartFP...), swordFP...), "on": true,
	}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
		"fingerprints": heartFP, "tag": "love", "on": true,
	}).Body.Close()

	// Without the text search, the companion is folded in.
	got := taggedAssets(t, srv, "tag=love&includeRelated=1")
	names := map[string]bool{}
	for _, it := range got.Items {
		names[it.Name] = true
	}
	if !names["Sword.glb"] {
		t.Fatal("the linked companion was not expanded in at all; the fixture is not exercising expansion")
	}

	// With one, the companion no longer matches and must stay out.
	got = taggedAssets(t, srv, "q=Heart&tag=love&includeRelated=1")
	for _, it := range got.Items {
		if it.Name == "Sword.glb" {
			t.Error("expansion relaxed the text search as well as the tag filter")
		}
	}
}

// frontendSources returns the JS this repo maintains. vendor/ is a third-party drop.
func frontendSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := assetsFS.ReadDir("assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := assetsFS.ReadFile("assets/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no frontend sources found")
	}
	return out
}

// The write endpoints are protected by requiring a JSON content-type, which forces a
// CORS preflight the server never answers. That only holds while the frontend actually
// sends one: a fetch that omits it, or uses a form encoding, silently downgrades to a
// simple request and takes the protection with it. Checked here because it is a
// property of the shipped JS that no request-level test can observe.
func TestEveryMutatingFetchSendsAJSONContentType(t *testing.T) {
	// A fetch with a method is a mutating one; a bare fetch(url) is a GET.
	call := regexp.MustCompile(`(?s)fetch\((.{0,400}?)\)\s*[,;)]`)
	for name, src := range frontendSources(t) {
		for _, m := range call.FindAllStringSubmatch(src, -1) {
			args := m[1]
			if !strings.Contains(args, "method:") {
				continue
			}
			if !strings.Contains(args, "application/json") {
				t.Errorf("%s: a fetch with a method does not send an application/json content-type, "+
					"which is what forces the preflight that protects the write endpoints:\n\tfetch(%s)", name, args)
			}
		}
	}
}

// Asset names come from a user's filesystem, so a crafted one reaching innerHTML as
// markup is a real, if small, injection. Enforced statically because the dangerous
// version renders identically to the safe one, and no request-level test can see it.
//
// The rule is about what gets interpolated, not about the shape of the assignment:
// every value spliced into markup must either be an ALL-CAPS constant this repo wrote
// or go through escapeHTML. A bare identifier is resolved to its declaration once, so
// markup assembled a line earlier is checked rather than waved through.
func TestNothingUserDerivedReachesInnerHTML(t *testing.T) {
	assign := regexp.MustCompile(`innerHTML\s*=\s*([^;\n]+)`)
	interp := regexp.MustCompile(`\$\{([^}]*)\}`)
	ident := regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	// The root of an expression: the identifier everything else hangs off.
	root := regexp.MustCompile(`[A-Za-z_$][\w$]*`)
	allCaps := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

	// Property names and index keys are not values: ICONS[category] yields whatever
	// ICONS holds no matter what category is, so indexing a constant table stays
	// constant. Stripping them first leaves only the identifiers whose contents
	// actually reach the markup.
	prop := regexp.MustCompile(`\.[A-Za-z_$][\w$]*`)
	index := regexp.MustCompile(`\[[^\]]*\]`)
	safeExpr := func(e string) bool {
		e = strings.TrimSpace(e)
		if strings.Contains(e, "escapeHTML(") {
			return true
		}
		e = index.ReplaceAllString(e, "")
		e = prop.ReplaceAllString(e, "")
		for _, r := range root.FindAllString(e, -1) {
			if !allCaps.MatchString(r) {
				return false
			}
		}
		return true
	}

	for name, src := range frontendSources(t) {
		for _, m := range assign.FindAllStringSubmatch(src, -1) {
			rhs := strings.TrimSpace(m[1])
			check := rhs
			if ident.MatchString(rhs) {
				// Resolve the identifier to its declaration in this file, if it has one.
				decl := regexp.MustCompile(`(?:const|let|var)\s+` + regexp.QuoteMeta(rhs) + `\s*=\s*([^;\n]+)`)
				if d := decl.FindStringSubmatch(src); d != nil {
					check = d[1]
				} else {
					continue // a parameter; its call sites supply the markup
				}
			}
			for _, e := range interp.FindAllStringSubmatch(check, -1) {
				if !safeExpr(e[1]) {
					t.Errorf("%s: innerHTML markup interpolates %q, which is neither an ALL-CAPS constant "+
						"nor escaped, so a crafted file name would reach the DOM as markup", name, strings.TrimSpace(e[1]))
				}
			}
			if !ident.MatchString(check) && !strings.Contains(check, "${") && !safeExpr(check) &&
				!strings.HasPrefix(check, "'") && !strings.HasPrefix(check, `"`) {
				t.Errorf("%s: innerHTML is assigned %q, which is neither a constant nor escaped", name, check)
			}
		}
	}
}

// Card grouping folds separators and case so a renamed copy of one file collapses.
// What it must not fold is the name itself: an allow-list of a-z0-9 erases every
// script but one, and two files left with nothing but their extension collapse onto a
// single card if their sizes agree. A library not named in English would browse as a
// handful of cards.
func TestGroupingKeepsNonASCIINamesApart(t *testing.T) {
	if groupNameKey("武器_剣.fbx") == groupNameKey("盾.fbx") {
		t.Error("two distinct non-ASCII names share a group key")
	}
	if got := groupNameKey("Épée.fbx"); got != "épée.fbx" {
		t.Errorf("groupNameKey(Épée.fbx) = %q, want the name folded rather than stripped", got)
	}
	// The Synty case this exists for still folds.
	if groupNameKey("SPR_Gem09.png") != groupNameKey("SPR_Gem_09.png") {
		t.Error("the separator fold regressed")
	}
}

// Tagging on means a store that was loaded from the path. A store nobody read from it
// has never seen the file, so the check that makes a save refuse to clobber an outside
// edit has nothing to compare against and the first write renames an empty store over
// whatever was there. The two arguments are not independent, and the constructor is
// where that is enforceable.
func TestServerRefusesTaggingWithoutALoadedStore(t *testing.T) {
	ix, err := assetindex.Build(assetindex.Options{Root: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(ix, nil, filepath.Join(t.TempDir(), tagstore.FileName)); err == nil {
		t.Error("a nil store paired with a tags path was accepted; the first write would overwrite the file")
	}
	if _, err := newServer(ix, nil, ""); err != nil {
		t.Errorf("a nil store with tagging off is the documented read-only mode: %v", err)
	}
}

// includeRelated widens a tag match with its linked companions. With no tag filter
// there is nothing to widen, and the short-circuit that says so exists to avoid copying
// a library-sized slice to guarantee a no-op — so what it must guarantee is that the
// result is the same set as no parameter at all.
func TestIncludeRelatedWithoutTagsChangesNothing(t *testing.T) {
	srv, _ := enabledServer(t)
	var plain, widened assetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets", nil), &plain)
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?includeRelated=1", nil), &widened)
	if plain.Total != widened.Total || len(plain.Items) != len(widened.Items) {
		t.Fatalf("includeRelated=1 alone returned %d/%d, want %d/%d", widened.Total, len(widened.Items), plain.Total, len(plain.Items))
	}
	for i := range plain.Items {
		if plain.Items[i].ID != widened.Items[i].ID {
			t.Errorf("item %d = %s, want %s", i, widened.Items[i].ID, plain.Items[i].ID)
		}
	}
}

// The facet counts and the results have to agree on what one result is. group=0 turns
// grouping off and returns a row per asset, so serving the grouped counts alongside it
// advertised a number no click could reach — roughly half, over a library where nearly
// every pack ships as both a SourceFiles zip and a unitypackage. The page reads facets
// once, from the first response, so a later query does not correct it either.
func TestFacetCountsAreReachableInBothGroupingModes(t *testing.T) {
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

	for _, mode := range []struct{ name, param string }{
		{"grouped", ""},
		{"ungrouped", "&group=0"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			var first assetsResp
			decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=1"+mode.param, nil), &first)
			check := func(kind, param string, values []struct {
				Value string
				Count int
			}) {
				for _, f := range values {
					if f.Value == "" {
						continue
					}
					var got assetsResp
					decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=500"+mode.param+"&"+param+"="+url.QueryEscape(f.Value), nil), &got)
					if got.Total != f.Count {
						t.Errorf("%s %q advertises %d but filtering returns %d", kind, f.Value, f.Count, got.Total)
					}
				}
			}
			check("vendor", "vendor", first.Facets.Vendors)
			check("category", "type", first.Facets.Categories)
			check("variant", "variant", first.Facets.Variants)
		})
	}

	// And the two modes genuinely differ, so the test above is not passing because
	// both sides happen to be the same number.
	var grouped, ungrouped assetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=1", nil), &grouped)
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=1&group=0", nil), &ungrouped)
	countOf := func(r assetsResp, v string) int {
		for _, f := range r.Facets.Vendors {
			if f.Value == v {
				return f.Count
			}
		}
		return -1
	}
	if countOf(grouped, "synty") == countOf(ungrouped, "synty") {
		t.Errorf("both modes report synty = %d; the fixture no longer has a card with two copies", countOf(grouped, "synty"))
	}
}

// writeRoutes is the single source of mutating registrations, and the two guard tests
// above iterate it — so a handler registered straight into handler() reaches the mux
// without reaching either. Checked against the source because the failure is an absence:
// nothing about the new route looks wrong, it is simply not in the list.
func TestNoMutatingRouteIsRegisteredOutsideWriteRoutes(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	// Only literal registrations are visible here; the writeRoutes loop builds its
	// pattern from variables and is the one form this must not flag.
	reg := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) `)
	for _, m := range reg.FindAllStringSubmatch(string(src), -1) {
		if m[1] != "GET" {
			t.Errorf("server.go registers a %s route directly on the mux: every mutating endpoint "+
				"must come from writeRoutes(), which is what the JSON-content-type and "+
				"tagging-disabled guard tests iterate", m[1])
		}
	}
	// The list itself must not be empty, or the guards above pass vacuously.
	s, err := newServer(&assetindex.Index{}, tagstore.New(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.writeRoutes()) == 0 {
		t.Error("writeRoutes() is empty; the write-endpoint guard tests would iterate nothing")
	}
}

// The Host guard is the only defence against DNS rebinding, and it is wired by a single
// condition in Serve. Tested only as bare middleware, deleting or inverting that
// condition left the whole suite green — in both directions the invariant names.
func TestIsLoopbackDecidesTheHostGuard(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want bool
	}{
		{"IPv4 loopback", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8788}, true},
		{"IPv4 loopback, other host bits", &net.TCPAddr{IP: net.IPv4(127, 1, 2, 3), Port: 8788}, true},
		{"IPv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 8788}, true},
		{"wildcard v4", &net.TCPAddr{IP: net.IPv4zero, Port: 8788}, false},
		{"wildcard v6", &net.TCPAddr{IP: net.IPv6unspecified, Port: 8788}, false},
		{"routable", &net.TCPAddr{IP: net.IPv4(192, 168, 1, 9), Port: 8788}, false},
		{"not a TCP addr", &net.UnixAddr{Name: "/tmp/x", Net: "unix"}, false},
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Errorf("isLoopback(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The end of the same wire, through Serve itself: a loopback listener refuses a
// rebound Host, and a routable one — which --addr is an explicit request for — serves
// the machines that have their own names for this one.
func TestServeAppliesTheHostGuardOnlyOnLoopback(t *testing.T) {
	for _, c := range []struct {
		name, addr string
		wantStatus int
	}{
		{"loopback refuses a rebound host", "127.0.0.1:0", http.StatusForbidden},
		{"routable serves it", "0.0.0.0:0", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", c.addr)
			if err != nil {
				t.Skipf("cannot listen on %s here: %v", c.addr, err)
			}
			s, err := newServer(&assetindex.Index{}, tagstore.New(), "")
			if err != nil {
				t.Fatal(err)
			}
			h := s.handler()
			if isLoopback(ln.Addr()) {
				h = guardHost(h)
			}
			srv := &http.Server{Handler: h}
			go srv.Serve(ln)
			t.Cleanup(func() { srv.Close() })

			req, err := http.NewRequest("GET", "http://"+ln.Addr().String()+"/api/assets", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "evil.example"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Errorf("Host: evil.example on %s = %d, want %d", ln.Addr(), resp.StatusCode, c.wantStatus)
			}
		})
	}
}

// A tag's palette count sits in the same slot as the vendor and category facet counts,
// so it has to mean the same thing: results a click returns. Counting fingerprints made
// it the number of copies instead, and folded in assignments for content this library
// does not hold at all — which no filter can reach, and which the store deliberately
// keeps forever.
func TestTagCountIsCardsAndNamesWhatIsOffIndex(t *testing.T) {
	srv, tagsPath := taggedLibrary(t, func(mk func(...string) string) {
		// The same file in two archives of one pack: two assets, two fingerprints, one card.
		writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Heart.fbx": "FBXHEART",
		})
		writeUnity(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/Foo/Heart.fbx", asset: "FBXHEART"},
		})
	})

	var listed taggedAssetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=10", nil), &listed)
	if len(listed.Items) != 1 || len(listed.Items[0].Fingerprints) != 2 {
		t.Fatalf("fixture = %+v; this test needs one card carrying two fingerprints", listed.Items)
	}
	resp := doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
		"fingerprints": listed.Items[0].Fingerprints, "tag": "hero", "on": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign = %d", resp.StatusCode)
	}
	resp.Body.Close()

	type paletteWithOff struct {
		Tags []struct {
			ID       string
			Count    int
			OffIndex int `json:"offIndex"`
		}
	}
	read := func() paletteWithOff {
		var p paletteWithOff
		decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
		if len(p.Tags) != 1 || p.Tags[0].ID != "hero" {
			t.Fatalf("palette = %+v, want one hero tag", p.Tags)
		}
		return p
	}
	p := read()
	var filtered taggedAssetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=500&tag=hero", nil), &filtered)
	if p.Tags[0].Count != filtered.Total {
		t.Errorf("hero advertises %d but ?tag=hero returns %d cards", p.Tags[0].Count, filtered.Total)
	}
	if p.Tags[0].Count != 1 {
		t.Errorf("hero count = %d, want 1: two copies of one file are one card", p.Tags[0].Count)
	}
	if p.Tags[0].OffIndex != 0 {
		t.Errorf("offIndex = %d, want 0: every tagged fingerprint is in this library", p.Tags[0].OffIndex)
	}

	// An assignment naming content this library does not hold — a narrowed --root, a
	// disabled pack, another machine. It must not raise the reachable count, and it
	// must not vanish either.
	st, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Assign("crc32:deadbeef:123", "hero")
	if err := st.Save(tagsPath); err != nil {
		t.Fatal(err)
	}
	resp = doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
		"fingerprints": listed.Items[0].Fingerprints, "tag": "hero", "on": true,
	})
	resp.Body.Close()

	p = read()
	if p.Tags[0].Count != 1 {
		t.Errorf("hero count = %d after an off-index assignment; the count must stay what a filter returns", p.Tags[0].Count)
	}
	if p.Tags[0].OffIndex != 1 {
		t.Errorf("offIndex = %d, want 1: the assignment is kept, and saying so is how it does not read as lost", p.Tags[0].OffIndex)
	}
}
