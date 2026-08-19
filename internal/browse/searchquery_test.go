package browse

import (
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/curbol/quarry/internal/assetindex"
)

func TestSearchQueryMatch(t *testing.T) {
	anim := assetindex.Asset{
		Name:     "HumanF@TurnLeft_Loop01.fbx",
		Pack:     "KevDev Anims",
		RelPath:  "kevdev/anims.zip::HumanF@TurnLeft_Loop01.fbx",
		Vendor:   "kevdev",
		Category: assetindex.CategoryAnimation,
		Ext:      "fbx",
		Variant:  "SourceFiles",
	}
	dash := assetindex.Asset{
		Name:     "Explosive@Dash-Left.fbx",
		Pack:     "RPG Combat",
		RelPath:  "explosive/rpg.zip::Explosive@Dash-Left.fbx",
		Vendor:   "explosive",
		Category: assetindex.CategoryAnimation,
		Ext:      "fbx",
	}

	cases := []struct {
		query string
		asset assetindex.Asset
		want  bool
	}{
		{"", anim, true},
		{"   ", anim, true},
		{"turn", anim, true},
		{"TURN", anim, true},
		{"walk", anim, false},

		{"turn loop", anim, true},
		{"turn jump", anim, false},
		{"turnleft loop01", anim, true},

		{"loop OR jump", anim, true},
		{"jump OR walk", anim, false},
		{"jump | loop", anim, true},

		// OR binds looser than the implicit AND. Both of these separate that from a
		// reading where OR binds tighter: "walk turn OR loop" is (walk AND turn) OR
		// loop, which the asset satisfies through "loop" alone, where walk AND (turn
		// OR loop) would fail on "walk"; the second pins the same rule on the right
		// of the OR.
		{"walk turn OR loop", anim, true},
		{"walk turn OR loop jump", anim, false},

		{"turn -idle", anim, true},
		{"turn -loop", anim, false},

		{`"TurnLeft_Loop01"`, anim, true},
		{`"Loop01_TurnLeft"`, anim, false},

		{"vendor:kevdev", anim, true},
		{"vendor:synty", anim, false},
		{"type:animation", anim, true},
		{"type:model", anim, false},
		{"ext:fbx", anim, true},
		{"ext:glb", anim, false},
		{"variant:sourcefiles", anim, true},

		{"anims", anim, true},
		{"zip", anim, true},

		{"(jump OR turn) loop", anim, true},
		{"(jump OR walk) loop", anim, false},

		{"dash-left", dash, true},
		{"dash -left", dash, false},
		{`pack:"RPG Combat"`, dash, true},
		{"-vendor:synty", dash, true},
		{"-vendor:explosive", dash, false},
	}

	for _, c := range cases {
		got := parseQuery(c.query).match(c.asset)
		if got != c.want {
			t.Errorf("parseQuery(%q).match(%q) = %v, want %v", c.query, c.asset.Name, got, c.want)
		}
	}
}

// TestSearchQueryEndpoint drives the query language through /api/assets against
// the fixture library (Heart.fbx/png/prefab, Rock.fbx, Sword.glb).
func TestSearchQueryEndpoint(t *testing.T) {
	srv := testServer(t)
	names := func(q string) []string {
		r := getAssets(t, srv, "q="+url.QueryEscape(q))
		var out []string
		for _, it := range r.Items {
			out = append(out, it.Name)
		}
		sort.Strings(out)
		return out
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"heart", []string{"Heart.fbx", "Heart.png", "Heart.prefab"}},
		{"heart -png", []string{"Heart.fbx", "Heart.prefab"}},
		{"vendor:explosive", []string{"Sword.glb"}},
		{"ext:glb", []string{"Sword.glb"}},
		{"rock OR sword", []string{"Rock.fbx", "Sword.glb"}},
		{"(rock OR sword) vendor:synty", []string{"Rock.fbx"}},
	}
	for _, c := range cases {
		if got := names(c.query); !eq(got, c.want) {
			t.Errorf("q=%q returned %v, want %v", c.query, got, c.want)
		}
	}
}

func TestSearchQueryEmptyIsNil(t *testing.T) {
	if parseQuery("").match(assetindex.Asset{Name: "anything"}) != true {
		t.Error("empty query must match everything")
	}
}

// An empty conjunction must contribute nothing, not truth. Search is debounced, so
// every OR query passes through its half-typed form on the way to being complete;
// if that matches everything, the grid floods with the whole library mid-keystroke.
func TestMalformedQueriesDoNotMatchEverything(t *testing.T) {
	unrelated := assetindex.Asset{Name: "barrel", Pack: "polygon-dungeon", Category: "props"}
	// A real term OR'd with an empty branch must not widen to everything.
	for _, q := range []string{"sword OR ", "sword |", "sword OR", "(sword OR )", "sword OR ()", "sword OR (", "(sword OR "} {
		if parseQuery(q).match(unrelated) {
			t.Errorf("query %q matched an unrelated asset", q)
		}
	}
	// The terms that are present still have to work.
	sword := assetindex.Asset{Name: "sword", Pack: "polygon-fantasy", Category: "models"}
	for _, q := range []string{"sword OR ", "sword |", "sword OR axe"} {
		if !parseQuery(q).match(sword) {
			t.Errorf("query %q dropped the term it did have", q)
		}
	}
	// A query carrying no terms at all is no filter, whether it is blank or the user
	// has typed only operators so far.
	for _, q := range []string{"", "   ", "OR", "|", ")", "()"} {
		if !parseQuery(q).match(unrelated) {
			t.Errorf("term-free query %q should be treated as no filter", q)
		}
	}
	// A term the tokenizer produced with nothing in it is the same hazard one branch
	// further in, and it arrives through two ordinary mid-keystroke states: an
	// unterminated quote, and a field prefix whose value has not been typed yet. An
	// empty term is the AND identity, so leaving one in the tree widened the query to
	// the whole library.
	for _, q := range []string{`sword OR "`, `sword OR ""`, "sword OR name:", "sword OR -name:", `"axe" OR vendor:`} {
		if parseQuery(q).match(unrelated) {
			t.Errorf("query %q matched an unrelated asset", q)
		}
	}
	for _, q := range []string{`sword "`, "sword name:", "sword -name:"} {
		if !parseQuery(q).match(sword) {
			t.Errorf("query %q dropped the term it did have", q)
		}
	}
	// An empty term on its own constrains nothing, so it is no filter rather than a
	// grid that blanks the moment a quote or a colon is typed.
	for _, q := range []string{`"`, `""`, "name:", "-name:"} {
		if !parseQuery(q).match(unrelated) {
			t.Errorf("query %q with only an empty term should be no filter", q)
		}
	}
}

// A stray ")" closes an expression that was never opened. Left in the token stream it
// ends parsing where it sits and every term after it goes unread — ")sword" compiled to
// no terms at all, which matches every asset in the library: the exact opposite of what
// was typed. These pin that a mistyped query still means what the rest of it says.
func TestMalformedGroupingKeepsTheTermsThatWereTyped(t *testing.T) {
	sword := assetindex.Asset{Name: "Sword", Pack: "Weapons", RelPath: "w/sword.fbx"}
	rock := assetindex.Asset{Name: "Rock", Pack: "Nature", RelPath: "n/rock.fbx"}
	for _, tc := range []struct {
		q                   string
		wantSword, wantRock bool
	}{
		{"sword", true, false},
		{")sword", true, false},
		{"sword)", true, false},
		{"((sword", true, false},
		{"()sword", true, false},
		{"sword OR rock", true, true},
		{"sword OR )rock", true, true}, // the alternative survives the stray close
		{")sword OR rock(", true, true},
		{"(sword OR rock)", true, true},
		{"sword rock", false, false}, // implicit AND, nothing is both
		{"-sword", false, true},
		{"-(sword OR rock)", false, false},
		{"-(sword)", false, true},
		{`"sword"`, true, false},
		{"name:sword", true, false},
		{"-name:sword", false, true},
		{"pack:weapons", true, false},
	} {
		q := parseQuery(tc.q)
		if got := q.match(sword); got != tc.wantSword {
			t.Errorf("parseQuery(%q).match(Sword) = %v, want %v", tc.q, got, tc.wantSword)
		}
		if got := q.match(rock); got != tc.wantRock {
			t.Errorf("parseQuery(%q).match(Rock) = %v, want %v", tc.q, got, tc.wantRock)
		}
	}
}

// parsePrimary recurses per "(", and a Go stack overflow is a fatal runtime error, not
// a panic: recover cannot catch it and net/http's per-request recovery does not apply,
// so one request of nothing but open parens took the whole server down with it. "(" is
// a valid URL character, so ~900k of them fit inside the default 1MB header budget.
func TestPathologicalQueriesDoNotBlowTheStack(t *testing.T) {
	asset := assetindex.Asset{Name: "Sword", Pack: "Weapons", RelPath: "w/sword.fbx"}
	for _, tc := range []struct{ name, q string }{
		{"deep nesting", strings.Repeat("(", 1_000_000) + "sword"},
		{"deep closes", strings.Repeat(")", 1_000_000) + "sword"},
		{"alternating", strings.Repeat("(sword ", 200_000)},
		{"many terms", strings.Repeat("sword ", 200_000)},
		{"unterminated quote", `"sword`},
		{"trailing operator", "sword OR "},
		{"leading operator", "OR sword"},
		{"only operators", "OR | OR"},
		{"empty group", "()"},
		{"field with no value", "name:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := parseQuery(tc.q) // must return, not recurse without bound
			q.match(asset)        // and must be evaluable
		})
	}
}

// `q` arrives in a URL, so it is bounded only by the server's header limit — about a
// megabyte — and everything downstream is sized from it. Past the cap the tail is cut,
// which narrows the query rather than widening it: the terms that survive still apply,
// and a truncated one ANDs in as the partial word it is. What must not happen is the
// query degrading to an all-match.
func TestOverlongQueryIsTruncatedNotWidened(t *testing.T) {
	sword := assetindex.Asset{Name: "Sword", Pack: "Weapons", RelPath: "w/sword.fbx"}
	rock := assetindex.Asset{Name: "Rock", Pack: "Nature", RelPath: "n/rock.fbx"}

	// Padded with spaces, the surviving text is exactly the leading term.
	q := parseQuery("sword" + strings.Repeat(" ", maxQueryBytes*4))
	if q == nil || !q.match(sword) || q.match(rock) {
		t.Errorf("an overlong but otherwise ordinary query did not behave like %q", "sword")
	}

	// Padded with a word, the truncated remainder is a term of its own and narrows the
	// result. Either way nothing unrelated comes back.
	q = parseQuery("sword " + strings.Repeat("x", maxQueryBytes*4))
	if q == nil {
		t.Fatal("an overlong query compiled to nil, which matches the whole library")
	}
	if q.match(rock) {
		t.Error("an overlong query matched an unrelated asset")
	}
}
