// Guard tests accumulated from audits. Two rules they are written to, both learned
// from guards that had quietly stopped checking anything:
//
// A guard over a value it does not own derives that value rather than restating it. A
// restated copy narrows the guard to whatever someone remembered to add to the copy,
// and it must also assert its own parsing found something, so a format change fails
// loudly rather than matching nothing.
//
// A guard over a constant straddles it: two inputs either side of it that land on
// different answers, then an assertion that the constant lies between them. Written
// well clear of it, a test pins only the direction and leaves the value free to drift.

package browse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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

	p := palette(t, srv)
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
	// And again with grouping off. buildFacets fills both sets in one pass, so today
	// the ungrouped counters are incremented beside the grouped ones — but nothing held
	// the second half, and an ungrouped count that still included the suppressed sibling
	// advertises a card no query in either mode can return.
	u := getAssets(t, srv, "group=0")
	if u.Total != 1 {
		t.Errorf("ungrouped grid shows %d rows, want 1: the RM sibling is suppressed in both modes", u.Total)
	}
	var ungroupedVendor int
	for _, f := range u.Facets.Vendors {
		if f.Value == "quaternius" {
			ungroupedVendor = f.Count
		}
	}
	if ungroupedVendor != u.Total {
		t.Errorf("ungrouped vendor facet counts %d but the query returns %d", ungroupedVendor, u.Total)
	}
	if got := getAssets(t, srv, "group=0&vendor=quaternius").Total; got != ungroupedVendor {
		t.Errorf("filtering the ungrouped facet returned %d, want the advertised %d", got, ungroupedVendor)
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
	bound := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8788}
	guarded := guardHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), bound)
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
		// Not this listener's address: 127.0.0.2 is loopback, but a request naming it
		// did not reach a server bound to 127.0.0.1 by dialling it.
		{"127.0.0.2:8788", http.StatusForbidden},
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

// isLoopback accepts all of 127.0.0.0/8 while loopbackHosts lists only 127.0.0.1, so a
// server bound anywhere else in that range installed the guard and then refused the
// address it was bound to — including the URL quarry prints and hands to the browser,
// which 403'd on every request with nothing working and no rebinding to defend against.
func TestHostGuardAcceptsTheAddressItIsBoundTo(t *testing.T) {
	bound := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 8788}
	guarded := guardHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), bound)
	srv := httptest.NewServer(guarded)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		host string
		want int
	}{
		{"127.0.0.2:8788", http.StatusOK},
		{"127.0.0.2", http.StatusOK},
		// The standard names stay admissible, and a rebound one stays refused: the
		// listener's own address is added to the set, not substituted for it.
		{"localhost:8788", http.StatusOK},
		{"127.0.0.1:8788", http.StatusOK},
		{"evil.example:8788", http.StatusForbidden},
		{"127.0.0.3:8788", http.StatusForbidden},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q on a listener bound to %s = %d, want %d", tc.host, bound, resp.StatusCode, tc.want)
		}
	}
}
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
	// Pack A's copy is the only one with a sibling, so that is the one the card has to
	// take. Asserting merely that the id is non-empty passes just as well when pairing
	// hands the card some other pack's RM, which is the failure that matters: a whole
	// FBX card carries no clip name, so the frontend plays every clip in whatever file
	// it is pointed at.
	rmID := out.Items[walk].RootMotionID
	if rmID == "" {
		t.Fatal("the card carries no rootMotionId, so the lightbox shows no root-motion toggle")
	}
	// The id is asked to serve, because that is what the toggle does with it and it is
	// the only handle on which file was chosen: the RM is suppressed from every
	// listing, so there is no card to compare ids against. Pack A's RM is the only copy
	// in the fixture with these bytes.
	resp := mustGet(t, srv.URL+"/api/content?id="+url.QueryEscape(rmID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the card's rootMotionId does not serve: %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "WALKRMBYTE" {
		t.Errorf("rootMotionId serves %q, want pack A's Walk_RM.fbx", got)
	}
	// Unnested, so it runs on a green build: the RM the card plays must not also stand
	// as a card of its own beside it.
	if visible["Walk_RM.fbx"] {
		t.Error("Walk_RM.fbx is both played by the card and shown beside it")
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

	p := palette(t, srv)
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
	// Both spellings of the property. Matching the literal "method:" saw only the one
	// that names its verb inline and walked straight past `{ method, headers: … }`,
	// where the verb is a variable — which is every tag write the page makes.
	method := regexp.MustCompile(`\bmethod\s*[:,}]`)
	var mutating int
	for name, src := range frontendSources(t) {
		for _, m := range call.FindAllStringSubmatch(src, -1) {
			args := m[1]
			if !method.MatchString(args) {
				continue
			}
			mutating++
			if !strings.Contains(args, "application/json") {
				t.Errorf("%s: a fetch with a method does not send an application/json content-type, "+
					"which is what forces the preflight that protects the write endpoints:\n\tfetch(%s)", name, args)
			}
		}
	}
	// The frontend makes two mutating fetches, and they duplicate their method, headers
	// and body verbatim — the obvious thing to fold into a postJSON helper, which this
	// regex (keyed on `method:` inside a fetch call) would then match none of. Silent,
	// it would go on passing while nothing checked the rule at all; loud, it has to be
	// re-pointed at the helper.
	if mutating < 2 {
		t.Errorf("found %d mutating fetches in the frontend; this guard has stopped reading it", mutating)
	}
}

// jsFunc is one named function in a frontend module: where its header sits, and the
// parameters it takes. Both spellings the frontend uses, because the two hops that
// carry markup today are one of each — a `function` that assigns the sink, called from
// an arrow that takes the markup itself.
type jsFunc struct {
	name   string
	params []string
	at     int
}

var jsFuncDecl = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(([^)]*)\)|^\s*(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?\(([^)]*)\)\s*=>`)

func jsFuncs(src string) []jsFunc {
	var out []jsFunc
	for _, m := range jsFuncDecl.FindAllStringSubmatchIndex(src, -1) {
		name, params := 1, 2
		if m[2*name] < 0 {
			name, params = 3, 4
		}
		var ps []string
		for _, p := range strings.Split(src[m[2*params]:m[2*params+1]], ",") {
			if p = strings.TrimSpace(p); p != "" {
				ps = append(ps, p)
			}
		}
		out = append(out, jsFunc{name: src[m[2*name]:m[2*name+1]], params: ps, at: m[0]})
	}
	return out
}

// enclosing is the function an offset sits inside: the last header before it.
func enclosing(fns []jsFunc, off int) (jsFunc, bool) {
	best, ok := jsFunc{}, false
	for _, f := range fns {
		if f.at <= off {
			best, ok = f, true
		}
	}
	return best, ok
}

// splitTopLevel breaks a call's argument list on the commas that belong to it, leaving
// the ones inside a nested call, a literal or a template alone.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '(' || r == '[' || r == '{':
			depth++
		case r == ')' || r == ']' || r == '}':
			depth--
		case r == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// callSites returns the argument lists of every call to name in src, with the offset
// of each call so the function it sits inside can be found. The declaration's own
// header is not a call: counted as one, a function that assigns its parameter to a
// sink resolves that parameter to itself and the walk never terminates.
func callSites(src, name string) (args [][]string, at []int) {
	call := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
	decl := regexp.MustCompile(`function\s+$`)
	for _, m := range call.FindAllStringIndex(src, -1) {
		if decl.MatchString(src[:m[0]]) {
			continue
		}
		depth, end := 0, -1
		var quote rune
		for i, r := range src[m[1]-1:] {
			if quote != 0 {
				if r == quote {
					quote = 0
				}
				continue
			}
			switch r {
			case '\'', '"', '`':
				quote = r
			case '(':
				depth++
			case ')':
				if depth--; depth == 0 {
					end = m[1] - 1 + i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			continue
		}
		args = append(args, splitTopLevel(src[m[1]:end]))
		at = append(at, m[0])
	}
	return args, at
}

// Asset names come from a user's filesystem, so a crafted one reaching the DOM as
// markup is a real, if small, injection. Enforced statically because the dangerous
// version renders identically to the safe one, and no request-level test can see it.
//
// Every sink the DOM offers, not just the one the frontend happens to use today:
// insertAdjacentHTML and outerHTML parse markup exactly as innerHTML does, and a guard
// that names one of them reads as though it covers the rule while leaving the others
// open.
//
// The rule is about what gets interpolated, not about the shape of the assignment:
// every value spliced into markup must either be an ALL-CAPS constant this repo wrote
// or go through escapeHTML. A bare identifier is resolved — to its declaration in the
// file, or, when it is a parameter, to the arguments every caller passes at that
// position, in whichever module the call lives. Stopping at the parameter is what left
// icons.js's exported protoClone(cache, key, markup) unchecked, two hops from a call
// site anywhere in the page, with the sink itself inside a module holding nothing but
// constants.
func TestNothingUserDerivedReachesAnHTMLSink(t *testing.T) {
	// The sinks, and which capture holds the markup.
	sinks := []struct {
		re  *regexp.Regexp
		arg int // 0 = the whole capture is the value; >0 = that argument of the call
	}{
		{regexp.MustCompile(`\.innerHTML\s*=\s*([^;\n]+)`), 0},
		{regexp.MustCompile(`\.outerHTML\s*=\s*([^;\n]+)`), 0},
		{regexp.MustCompile(`\.insertAdjacentHTML\s*\(([^;\n]+)\)`), 2},
		{regexp.MustCompile(`document\.write(?:ln)?\s*\(([^;\n]+)\)`), 1},
	}
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

	srcs := frontendSources(t)
	funcs := map[string][]jsFunc{}
	for name, src := range srcs {
		funcs[name] = jsFuncs(src)
	}

	var checked int
	var safeValue func(file, expr string, off, depth int) (bool, string)
	// safeParam asks what every caller passes at one parameter position.
	safeParam := func(fn, param string, idx, depth int) (bool, string) {
		seen := false
		for caller, src := range srcs {
			args, at := callSites(src, fn)
			for i, a := range args {
				if idx >= len(a) {
					continue
				}
				seen = true
				if ok, why := safeValue(caller, a[idx], at[i], depth+1); !ok {
					return false, fmt.Sprintf("%s calls %s with %s for %s, and %s", caller, fn, a[idx], param, why)
				}
			}
		}
		if !seen {
			return false, fmt.Sprintf("nothing calls %s, so what reaches %s is unknown", fn, param)
		}
		return true, ""
	}
	safeValue = func(file, expr string, off, depth int) (bool, string) {
		if depth > 6 {
			return false, "the markup passes through more hops than this guard follows"
		}
		expr = strings.TrimSpace(expr)
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
		if ident.MatchString(expr) {
			// A declaration in this file resolves it; otherwise it is a parameter, and
			// the callers decide.
			decl := regexp.MustCompile(`(?:const|let|var)\s+` + regexp.QuoteMeta(expr) + `\s*=\s*([^;\n]+)`)
			if d := decl.FindStringSubmatch(srcs[file]); d != nil {
				return safeValue(file, d[1], off, depth+1)
			}
			fn, ok := enclosing(funcs[file], off)
			if !ok {
				return false, fmt.Sprintf("%q is neither declared here nor a parameter of anything", expr)
			}
			for i, p := range fn.params {
				if p == expr {
					return safeParam(fn.name, expr, i, depth)
				}
			}
			return false, fmt.Sprintf("%q is not resolvable to a constant", expr)
		}
		for _, e := range interp.FindAllStringSubmatch(expr, -1) {
			if !safeExpr(e[1]) {
				return false, fmt.Sprintf("it interpolates %q, which is neither an ALL-CAPS constant nor escaped", strings.TrimSpace(e[1]))
			}
		}
		if strings.Contains(expr, "${") || safeExpr(expr) ||
			strings.HasPrefix(expr, "'") || strings.HasPrefix(expr, `"`) || strings.HasPrefix(expr, "`") {
			return true, ""
		}
		return false, fmt.Sprintf("%q is neither a constant nor escaped", expr)
	}

	for name, src := range srcs {
		for _, sink := range sinks {
			for _, m := range sink.re.FindAllStringSubmatchIndex(src, -1) {
				value := src[m[2]:m[3]]
				if sink.arg > 0 {
					args := splitTopLevel(value)
					if len(args) < sink.arg {
						continue
					}
					value = args[sink.arg-1]
				}
				checked++
				if ok, why := safeValue(name, value, m[0], 0); !ok {
					t.Errorf("%s: markup reaching an HTML sink is not safe: %s", name, why)
				}
			}
		}
	}
	// Derived guards go quiet when their parsing stops matching, and this one enforces
	// a rule nothing else does. The frontend has three sinks today; fewer than that
	// means the regexes above are reading past them rather than that the page stopped
	// building markup.
	if checked < 3 {
		t.Errorf("inspected %d HTML sinks; this guard has stopped reading the frontend", checked)
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
	// An allow-list rather than a search for non-GET literals. A pattern registered
	// with no method prefix matches every method in Go's mux, and every read-only route
	// here is spelled that way, so a mutating handler added in the same style was
	// invisible to a regex keyed on "METHOD ".
	readOnly := map[string]bool{
		"/": true, "/static/": true, "/api/assets": true, "/api/content": true, "/api/thumb": true,
	}
	// Only literal registrations are visible here; the writeRoutes loop builds its
	// pattern from variables and is the one form this must not flag.
	reg := regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`)
	found := reg.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no literal mux registrations matched; this guard has stopped reading server.go")
	}
	for _, m := range found {
		pattern := m[1]
		if strings.HasPrefix(pattern, "GET ") || readOnly[pattern] {
			continue
		}
		t.Errorf("server.go registers %q directly on the mux. Every mutating endpoint must come "+
			"from writeRoutes(), which is what the JSON-content-type and tagging-disabled guard "+
			"tests iterate; a new read-only one belongs in this test's allow-list, deliberately", pattern)
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

// The end of the same wire, through the production path: a loopback listener refuses a
// rebound Host, and a routable one — which --addr is an explicit request for — serves
// the machines that have their own names for this one, and says so.
//
// serveOn, not a listener of this test's own with the branch repeated on it. Repeating
// it asserted against a copy: replacing the condition in Serve with `if false` left the
// whole suite green, because nothing else drove Serve at all. The same held for the
// warning, which the invariant names ("the routable branch made silent") and which no
// test could observe while it went straight to os.Stderr.
func TestServeAppliesTheHostGuardOnlyOnLoopback(t *testing.T) {
	for _, c := range []struct {
		name, addr string
		wantStatus int
		wantWarn   bool
	}{
		{"loopback refuses a rebound host, silently", "127.0.0.1:0", http.StatusForbidden, false},
		{"routable serves it, and warns", "127.0.0.1:0", http.StatusOK, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", c.addr)
			if err != nil {
				t.Skipf("cannot listen on %s here: %v", c.addr, err)
			}
			// A routable bind needs an interface this machine may not have and a port a
			// sandbox may not open, so the routable branch is reached by handing serveOn a
			// loopback listener that reports a routable address. isLoopback reads Addr()
			// and nothing else, which is the invariant ("isLoopback is the only thing
			// deciding which branch runs") stated as a fixture.
			var addr net.Listener = ln
			if c.wantWarn {
				addr = routableAddr{ln}
			}
			ix := &assetindex.Index{Root: "/library"}
			s, err := newServer(ix, tagstore.New(), "")
			if err != nil {
				t.Fatal(err)
			}
			var out, warn bytes.Buffer
			opened := make(chan string, 1)
			// Not the real opener: the listener is closed as soon as this test returns,
			// so a browser launched here arrives at a dead ephemeral port — a window in
			// the developer's face reporting a connection failure, on every run of the
			// suite.
			env := serveEnv{out: &out, warn: &warn, open: func(u string) { opened <- u }}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.serveOn(ctx, addr, ix, "", env) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("serveOn: %v", err)
				}
			})

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
				t.Errorf("Host: evil.example on %s = %d, want %d", addr.Addr(), resp.StatusCode, c.wantStatus)
			}
			if got := warn.Len() > 0; got != c.wantWarn {
				t.Errorf("warned = %v, want %v (warning was %q)", got, c.wantWarn, warn.String())
			}
			if c.wantWarn && !strings.Contains(warn.String(), ix.Root) {
				t.Errorf("the warning does not name the scan root it is exposing: %q", warn.String())
			}
			// Whatever quarry prints has to be the address it also hands the browser, or
			// the one URL it opens is the one thing that does not work.
			select {
			case got := <-opened:
				if !strings.Contains(out.String(), got) {
					t.Errorf("opened %q, but printed %q", got, out.String())
				}
			default:
				t.Error("serveOn did not open the browser at all")
			}
		})
	}
}

// routableAddr reports a routable address for a listener that is really on loopback,
// so the branch --addr takes for other machines is reachable without one.
type routableAddr struct{ net.Listener }

func (r routableAddr) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 168, 1, 9), Port: r.Listener.Addr().(*net.TCPAddr).Port}
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

	read := func() paletteResp {
		p := palette(t, srv)
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

// A tag's palette numbers sit in the same slot as the facet counts, so they carry the
// same promise: what clicking returns. Two things broke it independently. A fingerprint
// is content alone while a card is name and size, so byte-identical files under two
// names share one print and land on two cards — and keeping one card key per print
// counted such a tag once where the filter returns both. And with grouping off a result
// is a row per asset, which the card count under-reports by however many copies a pack
// ships, exactly as it did for the facets before they started returning two sets.
func TestTagCountsAreReachableInBothGroupingModes(t *testing.T) {
	srv, _ := taggedLibrary(t, func(mk func(...string) string) {
		// One print, two names: two cards, and two rows either way.
		writeZip(t, mk("synty", "P", "P_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/T_Grid_A.png": "SAMEBYTES",
			"SourceFiles/T_Grid_B.png": "SAMEBYTES",
		})
		// One file in two archives: two prints, two assets, one card.
		writeZip(t, mk("synty", "Q", "Q_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Heart.fbx": "FBXHEART",
		})
		writeUnity(t, mk("synty", "Q", "Q_Unity_2022_3_v1_0_0.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/Q/Heart.fbx", asset: "FBXHEART"},
		})
	})

	var listed taggedAssetsResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=50", nil), &listed)
	var fps []string
	for _, it := range listed.Items {
		fps = append(fps, it.Fingerprints...)
	}
	resp := doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
		"fingerprints": fps, "tag": "hero", "on": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign = %d", resp.StatusCode)
	}
	resp.Body.Close()

	p := palette(t, srv)
	if len(p.Tags) != 1 || p.Tags[0].ID != "hero" {
		t.Fatalf("palette = %+v, want one hero tag", p.Tags)
	}

	for _, mode := range []struct {
		name, param string
		advertised  int
	}{
		{"grouped", "", p.Tags[0].Count},
		{"ungrouped", "&group=0", p.Tags[0].Assets},
	} {
		t.Run(mode.name, func(t *testing.T) {
			var got taggedAssetsResp
			decode(t, doJSON(t, "GET", srv.URL+"/api/assets?limit=500&tag=hero"+mode.param, nil), &got)
			if got.Total != mode.advertised {
				t.Errorf("hero advertises %d but ?tag=hero%s returns %d", mode.advertised, mode.param, got.Total)
			}
		})
	}
	// The fixture has to exercise both asymmetries, or the two numbers could agree by
	// accident and the test would pass on a build that reports one of them wrongly.
	if p.Tags[0].Count == p.Tags[0].Assets {
		t.Errorf("cards (%d) and rows (%d) are equal; this fixture is meant to separate them",
			p.Tags[0].Count, p.Tags[0].Assets)
	}
}

// An asset whose content could not be read has no fingerprint — a zip writer that
// leaves the CRC field unset over non-empty bytes is enough — and both the grouped and
// the ungrouped path build its card from a different function. Returning nil from
// either made "fingerprints" and "tags" null on one path and [] on the other, a
// difference in the public response shape that says nothing about the card and reaches
// the page as a call on a null. No fixture here produces a blank fingerprint, because
// archive/zip always writes a real CRC, so this asserts the shape directly.
func TestCardsSerializeEmptySetsAsArraysOnBothPaths(t *testing.T) {
	blank := assetindex.Asset{Name: "X.fbx", Size: 3}
	if blank.Fingerprint != "" {
		t.Fatal("this test needs an asset with no fingerprint")
	}
	// The same rule over the card the library is mostly made of: one fingerprint, no
	// tags. Both cases above take unionTagsLocked's general branch, which ends in
	// sortedSet right here; a single fingerprint takes the fast path instead, and its
	// non-nil answer comes from tagstore.sortedKeys — another package, a dozen lines from
	// Store.Related, which returns a bare nil for the same kind of miss.
	one := assetindex.Asset{Name: "Y.fbx", Size: 3, Fingerprint: "crc32:1:3"}
	empty := &server{store: tagstore.New()}
	for _, c := range []struct {
		name string
		dto  assetDTO
		want []string
	}{
		{"ungrouped", toDTO(blank), []string{`"fingerprints":[]`, `"tags":[]`}},
		{"grouped", groupItems([]assetindex.Asset{blank}, []int32{0})[0], []string{`"fingerprints":[]`, `"tags":[]`}},
		{"one untagged fingerprint", toDTO(one), []string{`"fingerprints":["crc32:1:3"]`, `"tags":[]`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.dto
			d.Tags = empty.unionTagsLocked(d.Fingerprints)
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !bytes.Contains(b, []byte(want)) {
					t.Errorf("missing %s in %s", want, b)
				}
			}
		})
	}
}

// The same rule one level up, at the envelope. A tag filter that matches nothing is an
// ordinary click — the ALL toggle over two tags no card shares — and app.js rejects a
// response whose items is not an array, so a nil slice there reads on screen as a
// failed load rather than as an empty grid. Asserted against the raw body because the
// response struct decodes null and [] into the same nil slice.
func TestAZeroMatchTagFilterAnswersAnEmptyArray(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	sword := itemByName(t, srv, "q=Sword", "Sword.glb")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "a", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": sword.Fingerprints, "tag": "b", "on": true}).Body.Close()

	// Each card carries one of the two tags, so requiring both matches nothing.
	for _, q := range []string{"tag=a&tag=b&tagmode=and", "tag=a&q=Sword"} {
		t.Run(q, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/api/assets?" + q)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(`"total":0`)) {
				t.Fatalf("this case must match nothing, got %s", b)
			}
			if !bytes.Contains(b, []byte(`"items":[]`)) {
				t.Errorf(`items is not an empty array, so the grid reports a failed load: %s`, b)
			}
		})
	}
}

// The URL quarry prints is also the one it opens, so it has to be dialable. A wildcard
// bind is how someone serves other machines, and it reports as "[::]:port" — an address
// a browser will not open, on a branch where the Host guard is off and localhost would
// have worked.
func TestTheAdvertisedURLIsOneABrowserCanOpen(t *testing.T) {
	for _, c := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"loopback v4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8788}, "127.0.0.1:8788"},
		{"loopback v6", &net.TCPAddr{IP: net.IPv6loopback, Port: 8788}, "[::1]:8788"},
		{"wildcard v6", &net.TCPAddr{IP: net.IPv6unspecified, Port: 8788}, "localhost:8788"},
		{"wildcard v4", &net.TCPAddr{IP: net.IPv4zero, Port: 8788}, "localhost:8788"},
		{"routable", &net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 8788}, "192.168.1.5:8788"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := browsableHost(c.addr); got != c.want {
				t.Errorf("browsableHost(%s) = %q, want %q", c.addr, got, c.want)
			}
		})
	}
}

// frontendImports returns the module specifiers one asset file imports, separated
// because the two mean different things here: a static import is on the importer's own
// critical path, a dynamic one is fetched when it is first used. Relative and /static/
// specifiers come back resolved to a path under assets/; anything else — a bare "three",
// the import map's "three/addons/" prefix — comes back as written, for the caller to
// resolve through the map.
func frontendImports(t *testing.T, file string) (static, dynamic []string) {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/" + file)
	if err != nil {
		t.Fatalf("assets/%s is not embedded: %v", file, err)
	}
	resolve := func(spec string) string {
		switch {
		case strings.HasPrefix(spec, "/static/"):
			return strings.TrimPrefix(spec, "/static/")
		case strings.HasPrefix(spec, "./"):
			return path.Join(path.Dir(file), strings.TrimPrefix(spec, "./"))
		}
		return spec
	}
	// Multiline: a module's imports are one per line and none of them is the first. An
	// empty result is legitimate — most of the Node-tested modules import nothing at all —
	// so the callers assert that the files which do import parsed.
	for _, m := range regexp.MustCompile(`(?m)^import[^;]*?from\s+'([^']+)'`).FindAllStringSubmatch(string(b), -1) {
		static = append(static, resolve(m[1]))
	}
	for _, m := range regexp.MustCompile(`\bimport\(\s*'([^']+)'`).FindAllStringSubmatch(string(b), -1) {
		dynamic = append(dynamic, resolve(m[1]))
	}
	return static, dynamic
}

// importMap is index.html's map, as {specifier or prefix: path under assets/}.
func importMap(t *testing.T) map[string]string {
	t.Helper()
	index, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, g := range regexp.MustCompile(`"([^"]+)"\s*:\s*"/static/([^"]+)"`).FindAllStringSubmatch(string(index), -1) {
		m[g[1]] = g[2]
	}
	if len(m) == 0 {
		t.Fatal("index.html's import map did not parse; this guard has stopped checking anything")
	}
	return m
}

// mapSpec resolves a bare specifier through the import map, longest prefix first, and
// reports whether the map covers it at all.
func mapSpec(m map[string]string, spec string) (string, bool) {
	if target, ok := m[spec]; ok {
		return target, true
	}
	best := ""
	for name := range m {
		if strings.HasSuffix(name, "/") && strings.HasPrefix(spec, name) && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return "", false
	}
	return m[best] + strings.TrimPrefix(spec, best), true
}

// There is no bundler, so a module specifier is resolved by the browser against this
// server and a wrong one is a blank page with one console line. Nothing in the Go tests
// fetches /static/, and the Node tests load only the modules that import nothing, so a
// renamed or unembedded file is invisible until someone opens the UI.
//
// The import map is checked alongside so the pair cannot drift: scene.js reaches three
// by absolute path precisely because a worker has no import map, and it has to be the
// same file the map names or the page loads two three instances.
func TestEveryFrontendImportResolvesToAnEmbeddedFile(t *testing.T) {
	entries, err := assetsFS.ReadDir("assets")
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			modules = append(modules, e.Name())
		}
	}
	if len(modules) == 0 {
		t.Fatal("no .js files embedded; this guard has stopped checking anything")
	}
	m := importMap(t)

	checked := 0
	for _, file := range modules {
		static, dynamic := frontendImports(t, file)
		for _, spec := range append(append([]string{}, static...), dynamic...) {
			checked++
			resolved := spec
			if !strings.Contains(spec, "/") || !strings.HasSuffix(spec, ".js") || !exists(resolved) {
				if target, ok := mapSpec(m, spec); ok {
					resolved = target
				}
			}
			if !exists(resolved) {
				t.Errorf("%s imports %q, which resolves to assets/%s and is not embedded", file, spec, resolved)
			}
		}
	}
	// Enough of them, or a regex that stopped matching passes this vacuously. The five
	// import-free modules are the exception, held by their own invariant.
	if checked < len(modules) {
		t.Errorf("parsed %d imports across %d modules; this guard has stopped reading them", checked, len(modules))
	}

	// The map's own targets have to exist too, or viewer.js's bare "three" resolves to
	// nothing while every absolute-path importer keeps working.
	for name, target := range m {
		p := strings.TrimSuffix(target, "/")
		if !exists(p) {
			if _, err := assetsFS.ReadDir("assets/" + p); err != nil {
				t.Errorf("index.html maps %q to /static/%s, which is not embedded", name, target)
			}
		}
	}
}

func exists(p string) bool {
	_, err := assetsFS.ReadFile("assets/" + p)
	return err == nil
}

// app.js draws a grid of icons and spinners and needs three for none of it. Statically
// importing scene.js or viewer.js puts three, both loaders and OrbitControls in front of
// the first /api/assets request, because a module body runs only once its whole graph has
// been fetched and instantiated — and the regression is invisible, since the page still
// works and is merely slower to show anything.
func TestTheGridDoesNotLoadThreeToRenderItself(t *testing.T) {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(file string) {
		if seen[file] {
			return
		}
		seen[file] = true
		static, _ := frontendImports(t, file) // dynamic imports are the point, so not followed
		for _, spec := range static {
			if exists(spec) {
				walk(spec)
				continue
			}
			t.Errorf("app.js statically reaches %q, via %s: the grid's first paint waits on it", spec, file)
		}
	}
	walk("app.js")
	if len(seen) < 2 {
		t.Fatal("app.js's import graph came out empty; this guard has stopped reading it")
	}
	for _, banned := range []string{"scene.js", "viewer.js", "thumbworker.js"} {
		if seen[banned] {
			t.Errorf("%s is in app.js's static import graph; import it dynamically where it is used", banned)
		}
	}
}

// A query string the server cannot read has to narrow, never answer. url.URL.Query()
// discards its parse error and drops only the pair it failed on, so a bad percent
// escape or a bare semicolon in `q` left the text search empty — the all-match — while
// every other filter in the same URL still applied. The user shares a filtered link,
// the recipient's `q` silently evaporates, and the grid reports the whole library as
// though that were the answer to what was asked.
//
// Read over every handler that takes a query string rather than over handleAssets
// alone: `id` dropped the same way resolves to the empty asset, and a new handler that
// reaches for r.URL.Query() directly gets the same hole back.
func TestAnUnreadableQueryStringIsRefusedNotPartiallyApplied(t *testing.T) {
	srv := testServer(t)
	for _, path := range []string{
		"/api/assets?q=50%off&group=0",
		"/api/assets?q=a;b",
		"/api/content?id=%zz",
		"/api/thumb?id=%zz",
		"/api/related?fingerprint=%zz",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: what survived the parse was answered with", path, resp.StatusCode)
		}
	}
	// The same shapes, escaped the way the frontend sends them, still reach the handler.
	resp, err := http.Get(srv.URL + "/api/assets?" + url.Values{"q": {"50%off"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a properly escaped query answered %d; the guard is refusing valid requests", resp.StatusCode)
	}
}

// Every handler that reads a query string must go through requestQuery. Reaching for
// r.URL.Query() directly is not a compile error and not a test failure anywhere else:
// it silently reinstates the partial-parse hole above for that one endpoint.
// The file list is globbed rather than written out. Restated, the guard covered whatever
// someone remembered to add to it: pairing.go and searchquery.go were already outside it,
// and a future handler file would have shipped green, reinstating the hole for exactly the
// endpoint nobody thought to list. Its old found==0 sentinel could not fire either — the
// counter was bumped unconditionally and a missing file t.Fatal'd on the read first.
func TestNoHandlerReadsTheQueryStringDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		found++
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "r.URL.Query()") {
				t.Errorf("%s:%d reads r.URL.Query() directly; use requestQuery so a malformed query string is refused", file, i+1)
			}
		}
	}
	// The package has had at least these since this guard was written, so a glob that
	// stops matching fails loudly rather than reporting a clean sweep of nothing.
	if found < 7 {
		t.Fatalf("globbed %d non-test source files in this package; this guard has stopped reading it", found)
	}
}

// handleContent tells a miss from a failure by fs.ErrNotExist alone: a corrupt
// archive, a full cache disk and a stale index pointing outside the library all reach
// the same place, and answering 404 for every one of them tells a user whose disk
// filled that their models do not exist. Only the unknown-id half was covered, so an
// error that stopped wrapping fs.ErrNotExist anywhere in assetindex would silently turn
// every one of those back into "not found".
func TestContentSeparatesAMissFromAFailure(t *testing.T) {
	srv := testServer(t)
	items := getAssets(t, srv, "").Items
	if len(items) == 0 {
		t.Fatal("the fixture library came back empty")
	}
	// An id nothing resolves: not an asset at all.
	resp, err := http.Get(srv.URL + "/api/content?id=nosuchid")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", resp.StatusCode)
	}
	// An id that resolves to an asset whose bytes are gone: the index is right, the
	// library moved under it. Still a miss, because that is what fs.ErrNotExist means.
	at := -1
	for i, it := range items {
		if it.Source.ArchivePath != "" {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatal("no archive-backed asset in the fixture; this test is not exercising the branch it claims")
	}
	zip := items[at]
	if err := os.Remove(zip.Source.ArchivePath); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(srv.URL + "/api/content?id=" + url.QueryEscape(zip.ID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an asset whose archive is gone = %d, want 404: it wraps fs.ErrNotExist like every other miss", resp.StatusCode)
	}
}

// sort=path had no test at all, so swapping its comparator's operands shipped green.
// It is the ordering a user reaches for to see a pack's files in the order they sit on
// disk, and reversed it is wrong in a way that looks deliberate.
func TestAssetsSortByPathIsAscending(t *testing.T) {
	srv := testServer(t)
	items := getAssets(t, srv, "sort=path").Items
	if len(items) < 2 {
		t.Fatalf("got %d items, need at least 2 to check an ordering", len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].RelPath > items[i].RelPath {
			t.Errorf("sort=path is not ascending at %d: %q then %q", i, items[i-1].RelPath, items[i].RelPath)
		}
	}
}

// app.js decides whether a card is a bitmap the lightbox can show and measure, and it
// does so from its own regex over the extension. classify.go decides the same thing at
// scan time, and the two are independent lists of the same extensions.
//
// Add one to classify.go alone and the card is categorised as an image and given a
// worker-rendered grid thumbnail, while the lightbox falls to the category icon and
// reports no dimensions — a format that works everywhere except where the user looks
// at it. Derived from classify.go's list rather than restated, so this fails on the
// change rather than on someone remembering to update a third copy.
func TestTheLightboxKnowsEveryExtensionClassifiedAsABitmap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "assetindex", "classify.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The one case arm that returns ThumbImage: the formats something can actually
	// rasterise, as opposed to the CategoryImage arm beside it that returns ThumbNone.
	arm := regexp.MustCompile(`(?s)case ([^:]+):\s*\n\s*return CategoryImage, ThumbImage`).FindStringSubmatch(string(b))
	if arm == nil {
		t.Fatal("no ThumbImage case found in classify.go; this guard has stopped reading it")
	}
	var exts []string
	for _, q := range regexp.MustCompile(`"([a-z0-9]+)"`).FindAllStringSubmatch(arm[1], -1) {
		exts = append(exts, q[1])
	}
	if len(exts) == 0 {
		t.Fatal("the ThumbImage case listed no extensions; this guard has stopped reading it")
	}

	app, err := os.ReadFile(filepath.Join("assets", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const bitmap = /\^\(([^)]*)\)\$/i`).FindStringSubmatch(string(app))
	if m == nil {
		t.Fatal("no bitmap test found in app.js; this guard has stopped reading it")
	}
	re, err := regexp.Compile("(?i)^(" + m[1] + ")$")
	if err != nil {
		t.Fatalf("app.js's bitmap pattern does not compile as a Go regexp (%q); adjust this guard: %v", m[1], err)
	}
	for _, ext := range exts {
		if !re.MatchString(ext) {
			t.Errorf("classify.go gives %q a rendered thumbnail but app.js does not treat it as a bitmap: the grid shows it and the lightbox does not", ext)
		}
	}
}

// A library is a stranger's files: a pack ships whatever its author put in it, and
// /api/content hands those bytes to the browser under a type this map chooses. Every
// type here has to be inert, because a response the browser will run as a document runs
// it in this server's origin — where the two write guards step aside by design. The
// Host header is genuinely this server's, so guardHost passes; a same-origin fetch may
// set application/json freely, so no preflight is involved. Such a script reads every
// file under the scan root and rewrites the tag store.
//
// .svg is the one that was here and is the reason this test is. Derived from
// classify.go rather than restated, so an extension taught to the classifier as an
// image cannot quietly acquire a scriptable type here.
func TestNoContentTypeIsOneTheBrowserWouldRunAsADocument(t *testing.T) {
	scriptable := map[string]bool{
		"image/svg+xml": true, "text/html": true, "application/xhtml+xml": true,
		"text/xml": true, "application/xml": true, "application/pdf": true,
	}
	src, err := os.ReadFile(filepath.Join("..", "assetindex", "classify.go"))
	if err != nil {
		t.Fatal(err)
	}
	exts := regexp.MustCompile(`"([a-z0-9]{1,8})"`).FindAllStringSubmatch(string(src), -1)
	if len(exts) < 40 {
		t.Fatalf("parsed %d extensions out of classify.go; this test has stopped reading it", len(exts))
	}
	for _, m := range exts {
		if ct := contentType(m[1]); scriptable[ct] {
			t.Errorf("contentType(%q) = %q, which a browser will run as a document in this origin", m[1], ct)
		}
	}
	// And the ones the grid actually renders still get their real type, or the lightbox
	// falls back to a category icon for every bitmap in the library.
	for ext, want := range map[string]string{
		"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
		"gif": "image/gif", "webp": "image/webp", "bmp": "image/bmp",
		"fbx": "application/octet-stream", "glb": "application/octet-stream",
		"svg": "application/octet-stream", "tga": "application/octet-stream",
	} {
		if got := contentType(ext); got != want {
			t.Errorf("contentType(%q) = %q, want %q", ext, got, want)
		}
	}
}

// nosniff is what keeps the declared type binding. Without it the browser is free to
// re-read a response it finds unconvincing, and the type it can arrive at that way is
// the one the test above exists to keep out of the map.
func TestServedBytesAreNotSniffable(t *testing.T) {
	srv := testServer(t)
	for _, tc := range []struct{ path, asset string }{
		{"/api/content?id=", "Rock.fbx"},
		{"/api/thumb?id=", "Heart.prefab"}, // the unitypackage member carrying a preview.png
	} {
		path := tc.path
		resp := mustGet(t, srv.URL+tc.path+idByName(t, srv, tc.asset))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d; this guard is asserting against an error page", path, resp.StatusCode)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}

// The default ordering — no sort= at all — is what every user sees on first load, and
// it was the one arm of sortItems with no test. Reversing its comparator left the whole
// suite green: the query tests sort the names themselves before comparing, and the
// paging tests compare paged order against single-page order, which is the same
// comparator on both sides.
//
// Both properties at once, because each hides the other: ascending-but-case-sensitive
// passes a fold-blind check, and folded-but-descending passes an ordering-blind one.
func TestAssetsDefaultSortIsAscendingAndCaseInsensitive(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		for _, n := range []string{"apple.fbx", "Banana.fbx", "cherry.fbx"} {
			os.WriteFile(mk("v", "Pack", n), []byte("BYTES-"+n), 0o644)
		}
	})
	var got []string
	for _, it := range getAssets(t, srv, "").Items {
		got = append(got, it.Name)
	}
	want := []string{"apple.fbx", "Banana.fbx", "cherry.fbx"}
	if !slices.Equal(got, want) {
		t.Errorf("default order = %v, want %v (ascending, case-folded)", got, want)
	}
}

// A root-motion file with no in-place sibling in its group is never paired and never
// suppressed, and decorate marks it bakedMotion so the lightbox offers the strip-motion
// toggle for it — `moveBtn.hidden = !(rmClips.length || asset.bakedMotion)`. That is the
// only consumer of RootMotionVariant with no test on either side of the wire, and the
// regression is a control that silently stops appearing.
func TestAStandaloneRootMotionCardOffersTheStripToggle(t *testing.T) {
	srv := serverWith(t, func(mk func(...string) string) {
		// Downloaded without its in-place partner, which is how a Quaternius clip pack
		// commonly arrives.
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk_RM.glb"), []byte("GLBBYTESRM"), 0o644)
	})
	r := getAssets(t, srv, "")
	if r.Total != 1 {
		t.Fatalf("grid shows %d cards, want the unpaired RM file visible", r.Total)
	}
	it := r.Items[0]
	if it.RootMotionID != "" {
		t.Errorf("rootMotionId = %q, want empty: there is no sibling to toggle to", it.RootMotionID)
	}
	if !it.BakedMotion {
		t.Error("bakedMotion is false, so the lightbox hides the strip-motion toggle for a file that is nothing but root motion")
	}
	// And the paired case keeps it off the in-place card, where the toggle comes from
	// the sibling instead.
	paired := serverWith(t, func(mk func(...string) string) {
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk.glb"), []byte("GLBBYTES"), 0o644)
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk_RM.glb"), []byte("GLBBYTESRM"), 0o644)
	})
	p := getAssets(t, paired, "").Items[0]
	if p.BakedMotion {
		t.Error("the in-place card is marked bakedMotion; its motion is in the sibling, not baked into it")
	}
	if p.RootMotionID == "" {
		t.Error("the in-place card lost its sibling")
	}
}

// /api/related resolves a link group to whole cards, and it skips the root-motion
// siblings the grid hides: surfacing one there shows the strip a card the grid never
// has, which cannot be opened from anywhere else. The UI cannot create such a link, but
// the tag store is a file meant to be hand-edited and committed, and an older quarry
// wrote groups without this rule.
func TestRelatedDoesNotSurfaceASuppressedRootMotionSibling(t *testing.T) {
	srv, _ := taggedLibrary(t, func(mk func(...string) string) {
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk.glb"), []byte("GLBBYTES"), 0o644)
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk_RM.glb"), []byte("GLBBYTESRM"), 0o644)
		os.WriteFile(mk("quaternius", "RPG_Animations", "Jump.glb"), []byte("GLBJUMP"), 0o644)
	})
	// The RM sibling is hidden from the grid, so its fingerprint is not reachable
	// through /api/assets at all — which is the whole point. It is the content print of
	// the bytes, so it is derivable here the same way the index derives it.
	rmFP := crcPrint("GLBBYTESRM")
	jump := itemByName(t, srv, "q=Jump", "Jump.glb")
	if len(jump.Fingerprints) == 0 {
		t.Fatal("the fixture card carries no fingerprint to link")
	}
	// The derived print has to be one the index actually holds, or the link below joins
	// two strings nothing resolves, /api/related finds no suppressed card because there
	// is none to find, and the whole test passes having exercised nothing. Tagging it is
	// how that is asked: the palette counts an assignment the index does not hold as
	// OffIndex rather than as a card.
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{rmFP}, "tag": "probe", "on": true}).Body.Close()
	for _, tg := range palette(t, srv).Tags {
		if tg.ID == "probe" && tg.OffIndex != 0 {
			t.Fatalf("the derived root-motion print is off-index (%d), so it names no asset and this guard is asserting against nothing", tg.OffIndex)
		}
	}
	fps := append([]string{rmFP}, jump.Fingerprints...)
	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": fps, "on": true}).Body.Close()

	for _, it := range relatedItems(t, srv, jump.Fingerprints).Items {
		if it.Name == "Walk_RM.glb" {
			t.Error("the related strip offers a card the grid suppresses; it cannot be opened from anywhere else")
		}
	}
	// The link itself is real — it is only the suppressed card that is withheld — so a
	// companion that *is* in the grid still comes back, and this test is not passing
	// because /api/related returned nothing at all.
	walk := itemByName(t, srv, "q=Walk", "Walk.glb")
	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": append(append([]string{}, jump.Fingerprints...), walk.Fingerprints...), "on": true}).Body.Close()
	var sawWalk bool
	for _, it := range relatedItems(t, srv, jump.Fingerprints).Items {
		if it.Name == "Walk.glb" {
			sawWalk = true
		}
	}
	if !sawWalk {
		t.Fatal("/api/related returned no visible companion either; this guard is asserting against an empty response")
	}
}

// tagmode is read as `and := mode == "and"`, so every other spelling is OR. That is the
// right fallback — a filter that narrows to nothing for a typo looks like an empty
// library — but nothing held it, and reading an unknown value as AND is a blank grid
// for a query the UI can produce by version skew alone.
//
// The ordinary readings are here too, on the same fixture: one tag, both under OR, both
// under AND. They were a second test building the same two-tag library to assert the
// same two totals, which is a fixture and a pair of assertions to keep in step for
// nothing.
func TestAnUnknownTagModeFallsBackToOr(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	sword := itemByName(t, srv, "q=Sword", "Sword.glb")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "a", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": sword.Fingerprints, "tag": "b", "on": true}).Body.Close()

	if one := taggedAssets(t, srv, "tag=a"); one.Total != 1 || one.Items[0].Name != "Heart.fbx" {
		t.Fatalf("a single-tag filter returned %+v, want only the card carrying it", one)
	}
	both := "tag=a&tag=b"
	if got := taggedAssets(t, srv, both).Total; got != 2 {
		t.Fatalf("the default mode returned %d cards, want both: OR is the default", got)
	}
	if got := taggedAssets(t, srv, both+"&tagmode=or").Total; got != 2 {
		t.Fatalf("tagmode=or returned %d cards, want both", got)
	}
	if got := taggedAssets(t, srv, both+"&tagmode=and").Total; got != 0 {
		t.Fatalf("tagmode=and returned %d, want 0: no card carries both", got)
	}
	for _, mode := range []string{"AND", "all", "both", "x"} {
		if got := taggedAssets(t, srv, both+"&tagmode="+mode).Total; got != 2 {
			t.Errorf("tagmode=%s returned %d cards, want 2: anything but the exact \"and\" is OR", mode, got)
		}
	}
}

// The write surface has three rules, and two of them had a guard that walks every
// route: a JSON content-type, and a refusal when tagging is off. The third — that a
// handler goes through writeguard rather than reaching for the store — had none, and
// it is the one that decides whether an edit survives the process. A handler that
// mutates the store outside writeUnderLock passes both of the others and is registered
// in the same list they read, so nothing here would have said anything: it takes no
// lock, races every concurrent reader, and is gone on restart.
//
// Asserted through the file rather than through the response, because the response is
// rendered from memory either way. This is the positive twin of the malformed-body
// test below, which already reads the store before and after for the negative case.
func TestEveryWriteEndpointPersistsAnAcceptedEdit(t *testing.T) {
	read := func(t *testing.T, p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	for _, e := range writeEndpoints(t) {
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			srv, tagsPath := enabledServer(t)
			// Every body but the create's edits a tag that has to exist first, or the
			// request is accepted and correctly changes nothing.
			if e.method != http.MethodPost || e.path != "/api/tags" {
				request(t, srv, http.MethodPost, "/api/tags", "application/json", writeBodies["POST /api/tags"]).Body.Close()
			}
			before := read(t, tagsPath)

			resp := request(t, srv, e.method, e.path, "application/json", e.body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200; the body in writeBodies has to be one this route accepts", e.method, e.path, resp.StatusCode)
			}
			if after := read(t, tagsPath); bytes.Equal(before, after) {
				t.Errorf("%s %s answered 200 and the store on disk is unchanged; the edit lives only in memory", e.method, e.path)
			}
		})
	}
}

// A body that is not JSON has to be a 400 naming the body, not a 500 and not a partial
// write. Oversized bodies are covered; a well-formed content-type carrying malformed
// JSON was not, and it is the shape a half-finished fetch actually arrives in.
func TestAMalformedJSONBodyIsRefusedWithoutTouchingTheStore(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	before, err := os.ReadFile(tagsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range writeEndpoints(t) {
		resp := httpDo(t, e.method, srv.URL+e.path, "application/json", `{"fingerprints": [`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s with a truncated body = %d, want 400: %s", e.method, e.path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "JSON") {
			t.Errorf("%s %s: error body %q does not say what was wrong", e.method, e.path, body)
		}
	}
	after, err := os.ReadFile(tagsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused request rewrote the tag store")
	}
}

// The modules the Node tests cover have to load with nothing installed: `node --test`
// resolves them off the filesystem, with no import map, no bundler and no package.json.
// A bare "three", an absolute "/static/..." specifier, or anything else reaching out of
// this directory makes them unloadable there, and the whole JS half of the suite stops
// running — which shows up as a green CI job that checked nothing.
//
// Derived from the test directory rather than restated, so a module given a test is
// held to this without anyone remembering to list it here.
func TestEveryNodeTestedModuleLoadsWithNothingInstalled(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("jstest"))
	if err != nil {
		t.Fatal(err)
	}
	// Each test file names the modules it imports from ../assets/.
	covered := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".test.mjs") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("jstest", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`from '\.\./assets/([^']+)'`).FindAllStringSubmatch(string(b), -1) {
			covered[m[1]] = true
		}
	}
	if len(covered) < 5 {
		t.Fatalf("found %d modules under test; this guard has stopped reading jstest/", len(covered))
	}
	for file := range covered {
		static, dynamic := frontendImports(t, file)
		for _, spec := range append(append([]string{}, static...), dynamic...) {
			// frontendImports resolves a "./x.js" to "x.js" and leaves everything else
			// as written, so a specifier that still names a sibling in this directory is
			// the only shape Node can resolve on its own.
			if !strings.HasSuffix(spec, ".js") || strings.Contains(spec, "/") || !covered[spec] {
				t.Errorf("%s imports %q; a module under test may only import a sibling module that is itself under test, relatively — otherwise node --test cannot load it",
					file, spec)
			}
		}
	}
}

// A class the page adds with no rule behind it is feedback that does not happen. The
// copy buttons carry no text, so `.failed` was the whole of their answer to a rejected
// clipboard write — and there was no such rule: the icon did not change, nothing was
// logged, and the stale clipboard is what got pasted.
//
// Only literal class names are checked; a computed one is out of reach here and rare.
func TestEveryClassTheFrontendAddsHasARule(t *testing.T) {
	css, err := assetsFS.ReadFile("assets/style.css")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range []string{"app.js", "viewer.js", "thumbs.js", "icons.js"} {
		b, err := assetsFS.ReadFile("assets/" + file)
		if err != nil {
			t.Fatal(err)
		}
		// classList.add('x'), .toggle('x', cond), and the ternary both arms of which are
		// literals — which is how the copy buttons pick between done and failed.
		for _, m := range regexp.MustCompile(`classList\.(?:add|toggle)\(\s*(?:[^)]*\?\s*)?'([a-z0-9-]+)'(?:\s*:\s*'([a-z0-9-]+)')?`).FindAllStringSubmatch(string(b), -1) {
			names = append(names, m[1])
			if m[2] != "" {
				names = append(names, m[2])
			}
		}
	}
	if len(names) < 3 {
		t.Fatalf("found %d literal class names; this guard has stopped reading the frontend", len(names))
	}
	for _, n := range names {
		if !strings.Contains(string(css), "."+n) {
			t.Errorf("the page adds the class %q and style.css has no rule for it: whatever it was meant to show, nothing happens", n)
		}
	}
}

// A tag's three numbers each have to mean what the client says they mean. cardsOfFP is
// built over what the grid can return, so a miss in it covers two unrelated things:
// content this library does not hold, and a root-motion sibling that is held but folded
// into the in-place card that plays it. Counted together, the palette told a user that a
// tag sat on "content outside this library" for a file in the library.
//
// Count stays 0 either way — no filter returns a suppressed asset, which is the rule
// that matters — so nothing but this separates the explanation from the truth.
func TestASuppressedSiblingIsNotCountedAsContentThisLibraryLacks(t *testing.T) {
	srv, tagsPath := taggedLibrary(t, func(mk func(...string) string) {
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk.glb"), []byte("GLBBYTES"), 0o644)
		os.WriteFile(mk("quaternius", "RPG_Animations", "Walk_RM.glb"), []byte("GLBBYTESRM"), 0o644)
	})
	_ = tagsPath

	// The RM file's own print. It has no card, so it can only be reached the way a
	// committed store or another machine reaches it: by fingerprint.
	rmFP := crcPrint("GLBBYTESRM")
	// And one for content this library genuinely does not hold.
	absentFP := "crc32:deadbeef:99"

	for _, fp := range []string{rmFP, absentFP} {
		resp := doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{
			"fingerprints": []string{fp}, "tag": "hero", "on": true,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("assign %s: %d", fp, resp.StatusCode)
		}
	}

	p := palette(t, srv)
	var hero *paletteTag
	for i := range p.Tags {
		if p.Tags[i].ID == "hero" {
			hero = &p.Tags[i]
		}
	}
	if hero == nil {
		t.Fatalf("no hero tag in the palette: %+v", p.Tags)
	}
	if hero.OffIndex != 1 {
		t.Errorf("offIndex = %d, want 1: only the absent fingerprint is content this library lacks; the suppressed root-motion sibling is right here", hero.OffIndex)
	}
	// Both are unreachable by a filter, which is the invariant offIndex must not break.
	if hero.Count != 0 || hero.Assets != 0 {
		t.Errorf("count/assets = %d/%d, want 0/0: neither assignment can be returned by ?tag=hero", hero.Count, hero.Assets)
	}
	if r := getAssets(t, srv, "tag=hero"); r.Total != 0 {
		t.Errorf("?tag=hero returns %d cards, want 0; a count the query cannot reach is the thing being guarded", r.Total)
	}
}

// palette reads /api/tags, decoded through the one struct tags_test declares for it:
// three local copies each held a subset of the numbers a tag carries, so a fourth
// number added to tagView had to be threaded into four decode targets and the local
// ones were the ones that would be forgotten.
func palette(t *testing.T, srv *httptest.Server) paletteResp {
	t.Helper()
	var p paletteResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
	return p
}

// crcPrint is the loose/zip fingerprint of some bytes, derived the way assetindex
// derives it rather than restated, so a change to the scheme fails here loudly.
func crcPrint(content string) string {
	return fmt.Sprintf("crc32:%x:%d", crc32.ChecksumIEEE([]byte(content)), len(content))
}

// The strip beside the lightbox opens whatever card /api/related names, by id. A card
// groups copies that share a name and a size, so a file shipped in two archives of one
// pack is one card with two fingerprints and two ids — and which of them the response
// carries decides the bytes the strip previews and the path it copies.
//
// computeResults hands groupItems a selection built by walking the index in order, so
// the grid's answer is fixed. /api/related built its selection by ranging a map, and
// groupItems keeps the first position it saw whenever the copies tie on thumb rank, so
// the same request answered with a different file from one call to the next: the
// preview loaded other geometry, with nothing on screen to say so.
func TestRelatedNamesTheSameCopyTheGridDoes(t *testing.T) {
	srv, _ := taggedLibrary(t, func(mk func(...string) string) {
		// One card, two fingerprints (a zip crc32 and a unitypackage guid), two ids, and
		// different bytes behind each so a flip is observable as content, not just as a
		// path. Same name and size, which is what makes them one card.
		writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
			"SourceFiles/Heart.fbx": "FBXHEART",
		})
		writeUnity(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/Foo/Heart.fbx", asset: "HEARTFBX"},
		})
		// A single-copy card on the other end of the link.
		writeZip(t, mk("synty", "Bar_Pack", "Bar_Pack_SourceFiles_v1.zip"), map[string]string{
			"SourceFiles/Anchor.fbx": "ANCHORBYTES",
		})
	})

	var heart, anchor taggedItem
	for _, it := range taggedAssets(t, srv, "limit=50").Items {
		switch it.Name {
		case "Heart.fbx":
			heart = it
		case "Anchor.fbx":
			anchor = it
		}
	}
	if len(heart.Fingerprints) != 2 || len(anchor.Fingerprints) != 1 {
		t.Fatalf("fixture: Heart has %d fingerprints and Anchor %d; this test needs 2 and 1",
			len(heart.Fingerprints), len(anchor.Fingerprints))
	}
	// The id the grid reports for that card, which the strip has to agree with.
	var want string
	for _, it := range getAssets(t, srv, "limit=50").Items {
		if it.Name == "Heart.fbx" {
			want = it.ID
		}
	}
	if want == "" {
		t.Fatal("the grid reports no Heart.fbx card")
	}

	fps := append(append([]string{}, heart.Fingerprints...), anchor.Fingerprints...)
	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": fps, "on": true}).Body.Close()

	q := url.Values{}
	for _, fp := range anchor.Fingerprints {
		q.Add("fingerprint", fp)
	}
	// Repeated, because the order that decided this was a map's: one call agrees with the
	// grid by chance roughly four times in five.
	for i := 0; i < 40; i++ {
		var out assetsResp
		decode(t, doJSON(t, "GET", srv.URL+"/api/related?"+q.Encode(), nil), &out)
		if len(out.Items) != 1 {
			t.Fatalf("call %d: /api/related returned %d cards, want 1", i, len(out.Items))
		}
		if out.Items[0].ID != want {
			t.Fatalf("call %d: /api/related names id %q (%s), but the grid names %q for that card; the strip would preview a different file",
				i, out.Items[0].ID, out.Items[0].RelPath, want)
		}
	}
}

// resultKey leaves offset and limit out, which is the whole reason scrolling a large
// library is linear: every page of one query shares one computation over the index.
// Folded back in, each page recomputes the full result set and nothing anywhere fails —
// the answers stay correct and the cost stops being visible in a test.
func TestPagingSharesOneComputationAndNothingElseDoes(t *testing.T) {
	base := url.Values{"q": {"sword"}, "type": {"model"}, "limit": {"60"}, "offset": {"0"}}
	page2 := url.Values{"q": {"sword"}, "type": {"model"}, "limit": {"60"}, "offset": {"60"}}
	if resultKey(base) != resultKey(page2) {
		t.Errorf("two pages of one query key differently (%q vs %q); each page would rebuild the whole result set",
			resultKey(base), resultKey(page2))
	}
	// Everything that does shape the set has to separate, or a query serves another's
	// results. Derived from the base rather than listed, so a parameter added to the
	// query language is covered by adding it here alone.
	for _, k := range []string{"q", "type", "vendor", "variant", "guid", "group", "tag", "tagmode", "includeRelated", "sort"} {
		other := url.Values{}
		for bk, bv := range base {
			other[bk] = bv
		}
		other.Set(k, "something-else")
		if resultKey(other) == resultKey(base) {
			t.Errorf("%s does not reach the key, so two queries differing only in it share one memoized result set", k)
		}
	}
}

// A split GLB's clips all carry the file's own size, so the normalized name is the
// only thing left separating them in the group key — and groupNameKey is deliberately
// lossy, dropping every rune that is not a letter, digit or ".". The clip labels it
// folds were made distinct by the scan precisely so each clip tags on its own
// fingerprint, so folding two of them onto one card loses a card under every filter
// and, because a card's fingerprints are the union, writes a tag the user put on one
// clip onto the other as well — into a file they commit.
//
// The pairs are the three shapes groupNameKey erases: the disambiguator's own output
// meeting a real name, a separator difference, and a case difference.
func TestTwoClipsOfOneFileAreTwoCards(t *testing.T) {
	const size = 4096
	clip := func(label string) assetindex.Asset {
		i := 0
		return assetindex.Asset{
			Name: label, Size: size, Category: assetindex.CategoryAnimation,
			Fingerprint: "crc32:dead:4096#" + label,
			Source: assetindex.Source{
				Kind: assetindex.SourceLoose, FilePath: "/lib/Anims.glb",
				Clip: label, ClipIndex: &i,
			},
		}
	}
	for _, pair := range [][2]string{
		{"Walk (2)", "Walk 2"},
		{"Run_01", "Run 01"},
		{"Walk", "walk"},
	} {
		assets := []assetindex.Asset{clip(pair[0]), clip(pair[1])}
		items := groupItems(assets, []int32{0, 1})
		if len(items) != 2 {
			t.Errorf("clips %q and %q of one file grouped into %d card(s), want 2: one clip is unreachable under every filter",
				pair[0], pair[1], len(items))
			continue
		}
		for i, it := range items {
			if len(it.Fingerprints) != 1 || it.Fingerprints[0] != assets[i].Fingerprint {
				t.Errorf("card for clip %q carries fingerprints %v, want only its own %q: a tag on this card lands on the other clip too",
					pair[i], it.Fingerprints, assets[i].Fingerprint)
			}
		}
	}
	// The fold groupKey exists for still has to happen: one animation library shipped
	// in two packs is one card, clip label and all.
	same := []assetindex.Asset{clip("Walk"), clip("Walk")}
	same[1].Pack = "Other"
	if items := groupItems(same, []int32{0, 1}); len(items) != 1 {
		t.Errorf("the same clip of the same file shipped in two packs made %d cards, want 1", len(items))
	}
}

// An author-origin `display` beats the user agent's `[hidden] { display: none }`
// whatever its specificity, so an element with a display rule of its own needs a
// matching [hidden] rule or `el.hidden = true` does nothing to it at all. The failure
// is silent in both directions: nothing throws, nothing logs, and the element simply
// stays on screen — the related strip kept the previous asset's companions under the
// next asset's panel, which is what the generation check in renderLbRelated exists to
// prevent.
//
// Derived from the JS rather than listed, so an element hidden by a new call site is
// covered by adding nothing. Only ids and class literals the page can be traced to a
// selector by are checked; a local variable naming an element built elsewhere is not
// resolvable from here and is skipped rather than guessed at.
func TestEveryElementThePageHidesCanActuallyBeHidden(t *testing.T) {
	css, err := assetsFS.ReadFile("assets/style.css")
	if err != nil {
		t.Fatal(err)
	}
	html, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	// The elements the page hides, as the selectors they are styled through: an id in
	// index.html carrying a `hidden` attribute is one the JS toggles, and its class is
	// what style.css addresses it by.
	tags := regexp.MustCompile(`<[a-z]+[^>]*\bid="([a-z-]+)"[^>]*\bclass="([^"]*)"[^>]*\bhidden\b`).FindAllStringSubmatch(string(html), -1)
	if len(tags) < 3 {
		t.Fatalf("found %d hidden elements in index.html; this guard has stopped reading it", len(tags))
	}
	for _, m := range tags {
		id, classes := m[1], strings.Fields(m[2])
		for _, c := range classes {
			// The rule that gives this class a display of its own, if any. A class with
			// no display rule falls through to the UA sheet and is hidden correctly.
			decl := regexp.MustCompile(`(?m)^\.` + regexp.QuoteMeta(c) + `\s*\{[^}]*\bdisplay\s*:`)
			if !decl.Match(css) {
				continue
			}
			guard := regexp.MustCompile(`(?m)^\.` + regexp.QuoteMeta(c) + `\[hidden\]\s*\{[^}]*\bdisplay\s*:\s*none`)
			if !guard.Match(css) {
				t.Errorf("#%s is hidden by the page and .%s sets its own display, but there is no `.%s[hidden] { display: none }`: setting .hidden on it does nothing",
					id, c, c)
			}
		}
	}
}

// The embedded FS has a zero modtime, so http.FileServerFS sends no Last-Modified and
// no ETag and the browser is free to cache /static/ heuristically. Drop the wrapper
// and nothing anywhere fails: the suite stays green, the page keeps working, and the
// cost lands after `quarry update`, as a UI silently running the previous build's
// app.js against the new server until someone thinks to hard-refresh.
func TestStaticAssetsAreNotHeuristicallyCacheable(t *testing.T) {
	srv := testServer(t)
	for _, p := range []string{"/static/app.js", "/static/style.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d; this guard is not reaching the handler", p, resp.StatusCode)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") {
			t.Errorf("%s Cache-Control = %q, want no-cache: with no Last-Modified or ETag "+
				"the browser caches it by guess and keeps the previous build after an update", p, cc)
		}
	}
}

// handleThumb's twin of TestContentSeparatesAMissFromAFailure. Most assets simply have
// no thumbnail, so both answers are 404 and the split is visible only in the log —
// which makes both regressions silent. Dropping the errors.Is logs a line for every
// ordinary thumbnail-less asset, burying the real ones under a grid's worth of noise;
// dropping the log leaves a full cache disk or a corrupt archive as a grid of category
// icons with nothing anywhere saying why.
func TestThumbLogsAFailureAndStaysQuietAboutAMiss(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// OpenThumbnail serves one thing — a unitypackage member's own preview.png — so the
	// library needs both a member carrying one and something that carries none.
	srv := serverWith(t, func(mk func(...string) string) {
		writeUnity(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityMember{
			{guid: "aaa", pathname: "Assets/Foo/Heart.prefab", asset: "PREFABBYTES", preview: true},
		})
		os.WriteFile(mk("synty", "Foo_Pack", "Plain.mat"), []byte("MATBYTES"), 0o644)
	})
	items := getAssets(t, srv, "").Items

	pick := func(want bool) int {
		t.Helper()
		for i, it := range items {
			if it.Source.HasPreview == want {
				return i
			}
		}
		t.Fatalf("no asset with HasPreview=%v in the fixture; this test is not exercising the branch it claims", want)
		return -1
	}
	thumb := func(i int) int {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/thumb?id=" + url.QueryEscape(items[i].ID))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// An asset with no thumbnail at all: a miss, and not worth a word.
	if got := thumb(pick(false)); got != http.StatusNotFound {
		t.Errorf("an asset with no thumbnail = %d, want 404", got)
	}
	if logged.Len() != 0 {
		t.Errorf("an ordinary thumbnail-less asset logged %q; every card in a grid would", logged.String())
	}

	// One that has a thumbnail whose archive is gone: still 404 to the grid, because a
	// broken preview must not fail the cards around it, but the run has to say so.
	withPreview := pick(true)
	if err := os.Remove(items[withPreview].Source.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if got := thumb(withPreview); got != http.StatusNotFound {
		t.Errorf("a thumbnail whose archive is gone = %d, want 404: the grid must not break around it", got)
	}
	if logged.Len() == 0 {
		t.Error("a thumbnail that failed for a reason other than having none logged nothing: " +
			"a full cache disk or a corrupt archive reaches here and leaves a grid of icons with no explanation")
	}
}

// The guards in this file that read a source file rather than drive a server all rest
// on a regex matching something, and every one of them can stop matching without
// failing: a refactor renames what it keys on, the pattern finds nothing, the loop runs
// zero times and the test passes having checked nothing. Two in this file had already
// reached that state — the one holding the CSRF content-type and the one holding the
// injection rule, which are the two enforcing Tier-1 invariants.
//
// So the convention is that such a guard asserts its own parsing found something, and
// this is what holds the convention. Structural because the rule is about the shape of
// a test rather than about any behaviour: a new guard written without the sentinel is
// exactly the one nobody will notice is asleep.
//
// It derives the set it checks rather than listing it, so a guard added tomorrow is
// covered without being named here.
func TestEveryStaticGuardAssertsItFoundSomething(t *testing.T) {
	src, err := os.ReadFile("audit_test.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// One entry per top-level func, so a match can be attributed to the test it is in.
	heads := regexp.MustCompile(`(?m)^func (\w+)`).FindAllStringSubmatchIndex(body, -1)
	if len(heads) < 40 {
		t.Fatalf("found %d top-level funcs in audit_test.go; this guard has stopped reading it", len(heads))
	}
	// A guard is static when it reads a file of this repo's own source. Reading a file
	// as such is not the trait — a behavioural test reads the tag store to compare it
	// before and after — so what counts is that the path is named here rather than
	// handed in: a helper, a glob, or a literal the read itself carries.
	reads := regexp.MustCompile(`frontendSources\(|frontendImports\(|filepath\.Glob\(|(?:os|assetsFS)\.ReadFile\(\s*(?:"|filepath\.Join)`)
	// The sentinel, in any of the shapes the file already uses: something counted,
	// compared against a floor, and reported. Requiring one exact spelling would only
	// move the problem.
	sentinel := regexp.MustCompile(`(?s)if\s+[^\n{]*(?:len\([^)]*\)|\w+)\s*(?:<|<=|==|!=)\s*(?:\d+|len\()[^\n{]*\{\s*\n\s*t\.(?:Fatalf?|Errorf?)\(`)
	var checked int
	for i, h := range heads {
		name := body[h[2]:h[3]]
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		end := len(body)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		fn := body[h[0]:end]
		if !reads.MatchString(fn) {
			continue
		}
		checked++
		if !sentinel.MatchString(fn) {
			t.Errorf("%s reads a repo file but never asserts its own parsing matched anything; "+
				"add a count check that fails loudly, or it stops checking the rule the day its regex stops matching", name)
		}
	}
	if checked < 5 {
		t.Errorf("found %d static guards in audit_test.go; this guard has stopped recognising them", checked)
	}
}

// scene.js is shared between the worker and the lightbox, and one decision in it turns
// on which: loadingManager blanks every texture URL, which the worker must do — a
// scroll touches thousands of models and every map behind one would stay resident for
// the life of the page — and the lightbox must not, because the user is looking at one
// model and its maps are what it looks like. gltfManager is that switch.
//
// Collapsing it to the one manager named three lines above is the obvious tidy-up, and
// nothing in this repo would notice: the Go suite never loads scene.js, node --test
// cannot (it imports three), and the symptom is a preview that renders fine and flat.
// So the switch is asserted here, where the frontend's other structural rules are.
func TestTheLightboxKeepsItsGlTFTextures(t *testing.T) {
	src, ok := frontendSources(t)["scene.js"]
	if !ok {
		t.Fatal("scene.js is not among the frontend sources; this guard has stopped reading it")
	}
	// Every glTF load goes through the switch, not through the blanking manager.
	loaders := regexp.MustCompile(`new GLTFLoader\(\s*([A-Za-z_$][\w$]*)\s*\)`).FindAllStringSubmatch(src, -1)
	if len(loaders) < 1 {
		t.Fatal("no `new GLTFLoader(<manager>)` in scene.js; this guard has stopped reading it")
	}
	for _, m := range loaders {
		if m[1] != "gltfManager" {
			t.Errorf("GLTFLoader built with %s; it has to take gltfManager, or the lightbox loses every glTF texture", m[1])
		}
	}
	// And the switch is chosen at runtime off the realm, not fixed for the module.
	decl := regexp.MustCompile(`const gltfManager\s*=\s*([^\n;]+)`).FindStringSubmatch(src)
	if decl == nil {
		t.Fatal("no `const gltfManager =` in scene.js; this guard has stopped reading it")
	}
	if !strings.Contains(decl[1], "inWorker") || !strings.Contains(decl[1], "?") {
		t.Errorf("gltfManager = %s; it has to be a conditional on inWorker, since the same module serves both realms", decl[1])
	}
	inWorker := regexp.MustCompile(`const inWorker\s*=\s*([^\n;]+)`).FindStringSubmatch(src)
	if inWorker == nil {
		t.Fatal("no `const inWorker =` in scene.js; this guard has stopped reading it")
	}
	if !strings.Contains(inWorker[1], "typeof WorkerGlobalScope") {
		t.Errorf("inWorker = %s; the realm is what decides this, and thumbworker.js shims document and window but not WorkerGlobalScope", inWorker[1])
	}
	// The blanking manager still has to reach the FBX loader in both realms: it is what
	// revokes the object URLs that loader mints per embedded texture and never revokes.
	if !regexp.MustCompile(`new FBXLoader\(\s*loadingManager\s*\)`).MatchString(src) {
		t.Error("FBXLoader no longer takes loadingManager; the object URLs it mints per texture are revoked nowhere else")
	}
}
