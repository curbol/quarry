// Package browse serves a local web UI to search and preview a game-asset library.
// It queries an assetindex.Index and streams asset bytes and thumbnails; the
// frontend (embedded here) renders results, 3D previews (three.js), and copy-path.
package browse

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/curbol/quarry/internal/assetindex"
	"github.com/curbol/quarry/internal/tagstore"
)

//go:embed assets
var assetsFS embed.FS

const defaultLimit = 200
const maxLimit = 500

type server struct {
	ix *assetindex.Index
	// facets counts cards, ungroupedFacets counts assets: which one a response carries
	// has to follow the same group= the results did, or the dropdown advertises a
	// number clicking it cannot reach. The page reads facets once, from the first
	// response, so a wrong one is not corrected by a later query either.
	facets          facets
	ungroupedFacets facets
	static          http.Handler

	// Tagging is enabled only when a tag-store path was resolved (a project
	// manifest neighborhood exists). tagsMu guards every access to store and to
	// tagsGeneration, which counts store edits so a memoized result set built against
	// an older palette is not served after one.
	tagsEnabled    bool
	tagsPath       string
	tagsMu         sync.RWMutex
	store          *tagstore.Store
	tagsGeneration uint64

	results resultCache

	// byFP indexes assets by content fingerprint so link expansion can resolve a
	// related fingerprint back to its asset(s) without scanning the whole library.
	byFP map[string][]int32

	// rmSibling maps an in-place animation asset id to its root-motion sibling id;
	// rmSuppressed marks the RM assets a sibling covers, hidden from the grid. Both
	// are computed once (the index is static per run). See pairing.go.
	rmSibling    map[string]string
	rmSuppressed map[string]bool

	// cardOfFP maps a content fingerprint to the group key of the card it lands on, so
	// a tag's count can be the number of cards ?tag= returns rather than the number of
	// fingerprints carrying it. Built once beside the facets, from the same static
	// index, and excluding the same suppressed siblings.
	cardOfFP map[string]string
}

// newServer wires an index, its precomputed facets, and the tag store to the
// embedded frontend. An empty tagsPath disables tagging (the UI still browses
// read-only), and is the only thing a nil store pairs with.
//
// The two are not independent: a store nobody read from tagsPath has never seen the
// file, so the staleness check that makes a save refuse to clobber an edit has nothing
// to compare against, and the first tag click would rename an empty store over
// whatever the user had. Tagging on means a store that was loaded.
func newServer(ix *assetindex.Index, store *tagstore.Store, tagsPath string) (*server, error) {
	static, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}
	if store == nil {
		if tagsPath != "" {
			return nil, fmt.Errorf("tagging is enabled for %s but no store was loaded from it", tagsPath)
		}
		store = tagstore.New()
	}
	byFP := map[string][]int32{}
	for i := range ix.Assets {
		if fp := ix.Assets[i].Fingerprint; fp != "" {
			byFP[fp] = append(byFP[fp], int32(i))
		}
	}
	rmSibling, rmSuppressed := buildRootMotionPairs(ix.Assets)
	cardOfFP := map[string]string{}
	for i := range ix.Assets {
		fp := ix.Assets[i].Fingerprint
		if fp == "" || rmSuppressed[ix.Assets[i].ID] {
			continue
		}
		cardOfFP[fp] = groupKey(ix.Assets[i])
	}
	grouped, ungrouped := buildFacets(ix.Assets, rmSuppressed)
	return &server{
		ix: ix, facets: grouped, ungroupedFacets: ungrouped, static: http.FileServerFS(static),
		tagsEnabled: tagsPath != "", tagsPath: tagsPath, store: store, byFP: byFP,
		rmSibling: rmSibling, rmSuppressed: rmSuppressed, cardOfFP: cardOfFP,
	}, nil
}

// loopbackHosts are the Host values a browser sends for a server bound to loopback.
// Anything else reaching a loopback listener arrived by a name that resolves here,
// which is the DNS-rebinding shape.
var loopbackHosts = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true,
}

// guardHost rejects requests whose Host is not one this server could legitimately be
// reached by. The write endpoints are protected by requiring a JSON content-type,
// which forces a CORS preflight this server does not answer — but that only stops a
// *cross-origin* page. A page served from a domain whose DNS is re-pointed at
// 127.0.0.1 is same-origin with quarry: no preflight, and it can both write to the
// tag store and read every response. Checking Host is what closes that, because the
// browser still sends the attacker's domain there.
//
// Only applied when the listener is on loopback: --addr on a routable interface is a
// deliberate choice to serve other machines, which have their own names for this one.
func guardHost(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browsers send a lowercase host without the trailing dot, but a hand-typed
		// curl or an FQDN written "localhost." does not, and a 403 there reads as a bug
		// rather than as the protection it is. The port comes off first: the dot is at
		// the end of the *host*, which is the middle of a "localhost.:8788" — and a
		// listener always has a port, so trimming first would never fire.
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(host, ".")
		if !loopbackHosts[host] {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// writeRoute is one endpoint that mutates the tag store.
type writeRoute struct {
	method  string
	pattern string
	handler http.HandlerFunc
}

// writeRoutes is every endpoint that writes to the tag store. It is a list rather than
// a run of registrations because the two guards each write endpoint depends on — a
// JSON content-type, and a refusal when tagging is off — are per-handler, so what
// holds them is a test that iterates every route. Registering the mux from the same
// list is what stops a new handler reaching the server without reaching those tests.
func (s *server) writeRoutes() []writeRoute {
	return []writeRoute{
		{http.MethodPost, "/api/tags", s.handleTagCreate},
		{http.MethodPatch, "/api/tags", s.handleTagPatch},
		{http.MethodDelete, "/api/tags", s.handleTagDelete},
		{http.MethodPost, "/api/assign", s.handleAssign},
		{http.MethodPost, "/api/link", s.handleLink},
	}
}

// handler builds the route mux; shared by Serve and tests.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.Handle("/static/", http.StripPrefix("/static/", noCache(s.static)))
	mux.HandleFunc("/api/assets", s.handleAssets)
	mux.HandleFunc("/api/content", s.handleContent)
	mux.HandleFunc("/api/thumb", s.handleThumb)
	mux.HandleFunc("GET /api/tags", s.handleTags)
	mux.HandleFunc("GET /api/related", s.handleRelated)
	for _, r := range s.writeRoutes() {
		mux.HandleFunc(r.method+" "+r.pattern, r.handler)
	}
	return mux
}

// Serve runs the browse UI at addr until ctx is cancelled (Ctrl-C). tagsPath, when
// non-empty, is the tag store loaded at startup and rewritten as tags change.
func Serve(ctx context.Context, addr string, ix *assetindex.Index, tagsPath string) error {
	store := tagstore.New()
	if tagsPath != "" {
		loaded, err := tagstore.Load(tagsPath)
		if err != nil {
			return fmt.Errorf("load tags %s: %w", tagsPath, err)
		}
		store = loaded
	}
	s, err := newServer(ix, store, tagsPath)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	h := s.handler()
	if isLoopback(ln.Addr()) {
		h = guardHost(h)
	} else {
		// Deliberate, and the guard steps aside for it — but stepping aside silently is
		// what makes it easy to leave on. There is no authentication anywhere here, so
		// anyone who can reach this port can read any file under the scan root, and curl
		// is not bound by the preflight the JSON content-type forces, so they can write
		// the tag store too.
		fmt.Fprintf(os.Stderr, "warning: %s is reachable from other machines, and quarry has no authentication:\n"+
			"  anyone who can reach it can read every file under %s and edit your tags.\n", ln.Addr(), ix.Root)
	}
	srv := &http.Server{Handler: h}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	addrURL := "http://" + ln.Addr().String()
	fmt.Printf("browse %d assets at %s  (Ctrl-C to stop)\n", len(ix.Assets), addrURL)
	if s.tagsEnabled {
		fmt.Printf("tags: %s\n", tagsPath)
	}
	openBrowser(addrURL)

	// Serving can stop on its own — the listener dies, the port is pulled away. Waiting
	// only on ctx there would leave quarry sitting silently on a URL that answers
	// nothing until the user gives up and interrupts it.
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", addr, err)
		}
		return nil
	}
	// A grid page can be mid-download of a 65MB model when Ctrl-C lands; give those
	// responses a moment to finish rather than cutting the connection.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// isLoopback reports whether the listener is bound to a loopback address, i.e. only
// reachable from this machine.
func isLoopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// noCache stops the browser serving stale embedded JS/CSS. The embedded FS has a zero
// modtime, so http.FileServer emits no Last-Modified/ETag and the browser would cache
// heuristically — silently running old code after a rebuild until a hard refresh.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		h.ServeHTTP(w, r)
	})
}

// ungrouped reports a query that wants a row per asset rather than one per card. It is
// one function because the results and the facet counts have to agree on the answer.
func ungrouped(query url.Values) bool { return query.Get("group") == "0" }

func (s *server) handleAssets(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset := atoiDefault(query.Get("offset"), 0)
	limit := atoiDefault(query.Get("limit"), defaultLimit)
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	items := s.resultsFor(query)

	total := len(items)
	lo := min(max(offset, 0), total)
	hi := min(lo+limit, total)

	f := s.facets
	if ungrouped(query) {
		f = s.ungroupedFacets
	}
	writeJSON(w, map[string]any{
		"total":  total,
		"offset": lo,
		"items":  items[lo:hi],
		"facets": f,
	})
}

// resultCache memoizes the answer to one query. The grid pages through a query at a
// walking offset, but everything that produces the answer — matching, grouping, tag
// resolution, sorting — scans the whole library and does not vary with the offset,
// so recomputing per page makes one full scroll quadratic in library size. A single
// entry covers that: a different query supersedes it, and holding several would
// retain several library-sized result sets at once.
type resultCache struct {
	mu    sync.Mutex
	key   string
	gen   uint64
	items []assetDTO
	valid bool
}

func (c *resultCache) lookup(key string, gen uint64) ([]assetDTO, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.key != key || c.gen != gen {
		return nil, false
	}
	return c.items, true
}

func (c *resultCache) store(key string, gen uint64, items []assetDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.gen, c.items, c.valid = key, gen, items, true
}

// resultsFor answers a query, reusing the previous answer while the grid pages
// through it. The returned slice is shared with the cache and with any concurrent
// request, so callers must only read it.
func (s *server) resultsFor(query url.Values) []assetDTO {
	key, gen := resultKey(query), s.tagsGen()
	if items, ok := s.results.lookup(key, gen); ok {
		return items
	}
	items := s.computeResults(query)
	s.results.store(key, gen, items)
	return items
}

// resultKey identifies a query by everything that shapes its result set. offset and
// limit choose a window into that set rather than changing it, so they are left out
// — that is what lets successive pages share one computation.
func resultKey(query url.Values) string {
	shape := make(url.Values, len(query))
	for k, v := range query {
		if k == "offset" || k == "limit" {
			continue
		}
		shape[k] = v
	}
	return shape.Encode()
}

func (s *server) tagsGen() uint64 {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	return s.tagsGeneration
}

func (s *server) computeResults(query url.Values) []assetDTO {
	matcher := parseQuery(query.Get("q"))
	types := valueSet(query["type"])
	vendors := valueSet(query["vendor"])
	variants := valueSet(query["variant"])
	guids := valueSet(query["guid"])

	// Positions into ix.Assets, not copies of them: an unfiltered browse of a 150k-asset
	// library would otherwise copy every struct twice on the way to the cards.
	var matched []int32
	for i := range s.ix.Assets {
		a := &s.ix.Assets[i]
		if s.rmSuppressed[a.ID] {
			continue // folded into its in-place sibling's card (root-motion toggle)
		}
		if types != nil && !types[string(a.Category)] {
			continue
		}
		if vendors != nil && !vendors[a.Vendor] {
			continue
		}
		if variants != nil && !variants[a.Variant] {
			continue
		}
		if guids != nil && !guids[a.Source.Guid] {
			continue
		}
		if !matcher.match(a) {
			continue
		}
		matched = append(matched, int32(i))
	}

	// Group identical copies (same file name + size) into one entry unless disabled,
	// so the same asset shipped across variants/packs shows once with all its paths.
	var grouped []assetDTO
	if ungrouped(query) {
		grouped = make([]assetDTO, len(matched))
		for i, ai := range matched {
			grouped[i] = toDTO(s.ix.Assets[ai])
		}
	} else {
		grouped = groupItems(s.ix.Assets, matched)
	}
	// Resolve each card's tags (the union over its fingerprints) and its linked
	// companions, then filter by the requested tags, so a card matches on its whole
	// tag set. The count/total below reflect the post-filter result set.
	s.decorate(grouped)
	// Expansion relaxes the tag filter, so with no tag filter there is nothing to
	// relax and every card is already present. Checking here keeps the copy below from
	// duplicating a library-sized result set to guarantee a no-op.
	includeRelated := query.Get("includeRelated") == "1" && len(query["tag"]) > 0
	var preTag []assetDTO
	if includeRelated {
		preTag = append([]assetDTO(nil), grouped...)
	}
	grouped = filterByTags(grouped, query["tag"], query.Get("tagmode"))
	if includeRelated {
		grouped = expandRelated(grouped, preTag)
	}
	sortItems(grouped, query.Get("sort"))
	return grouped
}

// decorate fills the per-card fields that depend on server state rather than on the
// card's own assets, so every path that builds cards produces the same shape. The
// lightbox reads rootMotionId and bakedMotion off whatever card it was handed, and a
// card built one way and a card built another have to agree.
func (s *server) decorate(cards []assetDTO) {
	for i := range cards {
		d := &cards[i]
		// Over every copy, not just the representative. Pairing groups by (vendor,
		// pack, base) while cards group by name and size, so one card routinely spans
		// packs and the copy that owns the RM sibling need not be the one thumbRank
		// picked to represent it. Reading only the representative left the card with no
		// toggle while its RM stayed suppressed from the grid — the file unreachable in
		// browse, which is the outcome per-card suppression exists to avoid.
		d.RootMotionID = s.rmSibling[d.ID]
		for _, c := range d.Copies {
			if d.RootMotionID != "" {
				break
			}
			d.RootMotionID = s.rmSibling[c.ID]
		}
		if _, isRM := assetindex.RootMotionVariant(assetFileBase(d.Source)); isRM {
			d.BakedMotion = true
		}
	}
	// Both store passes under one acquisition. Taken separately, a tag write landing
	// between them gives a card its tags from one state and its companions from the
	// next; and Go's RWMutex queues a new reader behind a waiting writer, so the second
	// acquisition is a second chance for a whole-library query to stall on one.
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	s.resolveTagsLocked(cards)
	s.resolveRelatedLocked(cards)
}

func (s *server) handleContent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ix.Lookup(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rc, size, err := s.ix.Open(a)
	if err != nil {
		// The id resolved, so the asset is in the index and this is not a miss. A
		// corrupt archive, a full cache disk, or a stale index pointing outside the
		// library all reach here, and answering 404 for every one of them tells a user
		// whose disk filled that their models do not exist.
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "asset not found", http.StatusNotFound)
			return
		}
		log.Printf("open %s: %v", a.RelPath, err)
		http.Error(w, "cannot open asset: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentType(a.Ext))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	n, err := io.Copy(w, rc)
	logShortBody(r, a, n, err)
}

func (s *server) handleThumb(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ix.Lookup(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rc, size, err := s.ix.OpenThumbnail(a)
	if err != nil {
		// Most assets simply have no thumbnail, so the answer is 404 either way: one
		// missing thumbnail must not fail the grid around it. A corrupt archive, a full
		// cache disk, or a stale index pointing outside the library reach here too, and
		// answering those the same way silently is what leaves a user with a grid of
		// icons and nothing anywhere saying why.
		if !errors.Is(err, assetindex.ErrNoThumbnail) {
			log.Printf("thumbnail %s: %v", a.RelPath, err)
		}
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	n, err := io.Copy(w, rc)
	logShortBody(r, a, n, err)
}

// logShortBody reports a body that stopped early. The Content-Length is already sent
// by then, so the client sees a network error and nothing else would say why — the
// same silence the Open failures above are logged to avoid. A client that navigated
// away mid-download is the ordinary case and says nothing: it cancels the request
// context, and a scrolling grid does it constantly.
func logShortBody(r *http.Request, a assetindex.Asset, n int64, err error) {
	if err == nil || r.Context().Err() != nil {
		return
	}
	log.Printf("serve %s: stopped after %d bytes: %v", a.RelPath, n, err)
}

// contentType maps an extension to a response type. Model formats are served as
// application/octet-stream so nothing mangles the binary the three.js loaders read
// as an ArrayBuffer; browser-native images get their real image type.
func contentType(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// valueSet turns the repeated values of a filter param into a membership set, or nil
// when the param was absent (meaning no filter on that facet). A present-but-empty
// value ("", e.g. variant=) stays in the set and selects the loose/unknown bucket.
func valueSet(vals []string) map[string]bool {
	if vals == nil {
		return nil
	}
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
