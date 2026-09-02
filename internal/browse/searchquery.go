package browse

import (
	"strings"
	"unicode/utf8"

	"github.com/curbol/quarry/internal/assetindex"
)

// searchQuery is a compiled Google-style search expression evaluated against an
// asset. Whitespace is AND, `OR` (or `|`) alternates, a leading `-` negates,
// `"…"` quotes a literal phrase, `( )` groups, and `field:value` scopes a term to
// one asset field. `OR` binds looser than the implicit AND, so `a b OR c` reads as
// `(a AND b) OR c`. Malformed input never errors: it degrades to a best effort.
type searchQuery struct{ root searchNode }

// maxQueryBytes bounds the raw query. `q` arrives in a URL, so it is bounded only by
// the server's header limit — about a megabyte — and everything downstream is sized
// from it. No query anyone types comes close.
const maxQueryBytes = 4 << 10

// maxQueryDepth bounds group nesting. parsePrimary recurses per "(", and Go's stack
// overflow is a fatal runtime error rather than a panic: it cannot be recovered, so
// one request of nothing but open parens would take the whole server down with it.
const maxQueryDepth = 32

// parseQuery compiles a raw query string, or returns nil when it holds no terms
// (an all-match). A nil *searchQuery matches every asset.
func parseQuery(s string) *searchQuery {
	if len(s) > maxQueryBytes {
		s = s[:maxQueryBytes]
		// Back to a rune boundary. The cut is by byte, and []rune turns a trailing
		// partial one into U+FFFD, which no asset name contains — so the last term
		// matches nothing and the implicit AND makes the whole query return nothing.
		// Truncation is meant to narrow the query, not answer it.
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	toks := dropUnmatchedClose(tokenize(s))
	if len(toks) == 0 {
		return nil
	}
	p := &parser{toks: toks}
	// Parsing runs to the end of the input rather than stopping at the first thing it
	// cannot place, so no term the user typed is silently discarded. With unmatched
	// closes already gone this is a single pass; the loop is what keeps that true if
	// the tokenizer ever grows a token the grammar does not expect.
	var kids []searchNode
	for p.pos < len(p.toks) {
		before := p.pos
		if k := p.parseOr(); k != nil {
			kids = append(kids, k)
		}
		if p.pos == before {
			p.pos++ // an unmatched ")" that no branch consumed; skip it and carry on
		}
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return &searchQuery{root: kids[0]}
	}
	return &searchQuery{root: andNode{kids: kids}}
}

func (q *searchQuery) match(a *assetindex.Asset) bool {
	if q == nil || q.root == nil {
		return true
	}
	return q.root.eval(a)
}

type searchNode interface {
	eval(a *assetindex.Asset) bool
}

type andNode struct{ kids []searchNode }
type orNode struct{ kids []searchNode }
type notNode struct{ kid searchNode }

// termNode is one leaf: a case-insensitive substring test. An empty field scopes
// the match to name, pack, and path; a set field scopes it to that asset field.
type termNode struct {
	field string
	value string
}

func (n andNode) eval(a *assetindex.Asset) bool {
	for _, k := range n.kids {
		if !k.eval(a) {
			return false
		}
	}
	return true
}

func (n orNode) eval(a *assetindex.Asset) bool {
	for _, k := range n.kids {
		if k.eval(a) {
			return true
		}
	}
	return false
}

func (n notNode) eval(a *assetindex.Asset) bool { return !n.kid.eval(a) }

func (n termNode) eval(a *assetindex.Asset) bool {
	if n.field == "" {
		return containsFold(a.Name, n.value) ||
			containsFold(a.Pack, n.value) ||
			containsFold(a.RelPath, n.value)
	}
	return containsFold(searchFields[n.field](a), n.value)
}

// containsFold reports whether s contains lower, case-insensitively. lower is already
// lowercased, which tokenize guarantees.
//
// This is strings.Contains(strings.ToLower(s), lower) without the copy of s. It runs
// once per term per asset, and an unfielded term runs it three times: over a
// 150k-asset library that is half a million throwaway strings for one query, and
// every keystroke is its own query with its own memo key, so nothing amortizes it.
// Asset names and paths are mixed case in practice, which is exactly when ToLower
// allocates rather than returning s.
func containsFold(s, lower string) bool {
	if lower == "" {
		return true
	}
	if !isASCII(s) || !isASCII(lower) {
		// Case folding is not a per-byte operation outside ASCII — İ, ẞ and the Turkish
		// dotless i fold across lengths — so anything carrying a high byte pays for the
		// copy rather than getting a wrong answer.
		return strings.Contains(strings.ToLower(s), lower)
	}
	for i := 0; i+len(lower) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(lower)], lower) {
			return true
		}
	}
	return false
}

func equalFoldASCII(s, lower string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// searchFields maps a field:value operator name to the asset field it scopes to.
var searchFields = map[string]func(*assetindex.Asset) string{
	"name":    func(a *assetindex.Asset) string { return a.Name },
	"pack":    func(a *assetindex.Asset) string { return a.Pack },
	"vendor":  func(a *assetindex.Asset) string { return a.Vendor },
	"type":    func(a *assetindex.Asset) string { return string(a.Category) },
	"variant": func(a *assetindex.Asset) string { return a.Variant },
	"ext":     func(a *assetindex.Asset) string { return a.Ext },
	"guid":    func(a *assetindex.Asset) string { return a.Source.Guid },
	"path":    func(a *assetindex.Asset) string { return a.RelPath },
}

type tokKind int

const (
	tokTerm tokKind = iota
	tokOr
	tokLParen
	tokRParen
)

type token struct {
	kind  tokKind
	neg   bool
	field string
	value string
}

func isSpace(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// tokenize splits a query into terms and operators. Quotes protect their span
// from operator interpretation (spaces, `-`, `:`, `OR`, parens are literal
// inside); a `field:` prefix and a leading `-` are only recognized outside quotes.
func tokenize(s string) []token {
	var toks []token
	r := []rune(s)
	i, n := 0, len(r)
	for i < n {
		switch {
		case isSpace(r[i]):
			i++
			continue
		case r[i] == '(':
			toks = append(toks, token{kind: tokLParen})
			i++
			continue
		case r[i] == ')':
			toks = append(toks, token{kind: tokRParen})
			i++
			continue
		}

		var field string
		var val strings.Builder
		split := false // a field: prefix has been taken
		leadingDash := false
		anyQuote := false
		started := false
		for i < n && !isSpace(r[i]) && r[i] != '(' && r[i] != ')' {
			c := r[i]
			switch {
			case c == '"':
				anyQuote = true
				started = true
				i++
				for i < n && r[i] != '"' {
					val.WriteRune(r[i])
					i++
				}
				if i < n {
					i++ // closing quote
				}
			case !started && c == '-':
				leadingDash = true
				started = true
				i++
			case c == ':' && !split && isSearchField(strings.ToLower(val.String())):
				field = strings.ToLower(val.String())
				val.Reset()
				split = true
				started = true
				i++
			default:
				started = true
				val.WriteRune(c)
				i++
			}
		}

		value := val.String()
		if !anyQuote && !leadingDash && !split && (value == "OR" || value == "|") {
			toks = append(toks, token{kind: tokOr})
			continue
		}
		if leadingDash && field == "" && value == "" {
			// The term loop stops at "(", so a dash directly before a group arrives here
			// with nothing attached. It negates the group: reading it as a literal "-"
			// instead would AND a term the user never typed onto the group they meant to
			// exclude, and "-(a OR b)" would match nothing rather than everything else.
			if i < n && r[i] == '(' {
				toks = append(toks, token{kind: tokLParen, neg: true})
				i++
				continue
			}
			value = "-" // a lone '-' is a literal term, not a negation
			leadingDash = false
		}
		toks = append(toks, token{kind: tokTerm, neg: leadingDash, field: field, value: strings.ToLower(value)})
	}
	return toks
}

// dropUnmatchedClose removes ")" tokens that close nothing. Left in the stream one
// ends the expression where it sits and everything after it goes unread — ")sword"
// would compile to no terms at all and match the whole library, and "a OR )b" would
// lose the alternative. Removing them is what lets the rest of a mistyped query
// still mean what it says.
func dropUnmatchedClose(toks []token) []token {
	out := toks[:0]
	depth := 0
	for _, t := range toks {
		switch t.kind {
		case tokLParen:
			depth++
		case tokRParen:
			if depth == 0 {
				continue
			}
			depth--
		}
		out = append(out, t)
	}
	return out
}

func isSearchField(name string) bool {
	_, ok := searchFields[name]
	return ok
}

type parser struct {
	toks  []token
	pos   int
	depth int
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

// parseOr and parseAnd return nil for a branch that carries no terms, rather than
// an empty node. An empty andNode evaluates to true (the identity for AND), so a
// half-typed "sword OR " would otherwise compile to "sword OR everything" and match
// the whole library on the way to being finished.
func (p *parser) parseOr() searchNode {
	var kids []searchNode
	if k := p.parseAnd(); k != nil {
		kids = append(kids, k)
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOr {
			break
		}
		p.pos++ // consume OR
		if k := p.parseAnd(); k != nil {
			kids = append(kids, k)
		}
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	return orNode{kids: kids}
}

func (p *parser) parseAnd() searchNode {
	var kids []searchNode
	for {
		t, ok := p.peek()
		if !ok || t.kind == tokOr || t.kind == tokRParen {
			break
		}
		if k := p.parsePrimary(); k != nil {
			kids = append(kids, k)
		}
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	return andNode{kids: kids}
}

func (p *parser) parsePrimary() searchNode {
	t := p.toks[p.pos]
	p.pos++
	if t.kind == tokLParen {
		if p.depth >= maxQueryDepth {
			// Past the depth cap the group is read but not built: skip to its close so
			// the rest of the query still parses, rather than recursing without bound.
			p.skipGroup()
			return nil
		}
		p.depth++
		inner := p.parseOr()
		p.depth--
		if nt, ok := p.peek(); ok && nt.kind == tokRParen {
			p.pos++
		}
		if t.neg && inner != nil {
			return notNode{kid: inner}
		}
		return inner // nil when the group held no terms
	}
	if t.kind == tokRParen {
		return nil // unbalanced close; nothing to match on
	}
	if t.value == "" {
		// A term with nothing in it constrains nothing, and a node here would be the
		// AND identity — so `sword OR "` and `sword OR vendor:` would each widen to the
		// whole library on the way to being typed. An unterminated quote and a bare
		// field prefix are both ordinary states of a query mid-keystroke.
		return nil
	}
	var node searchNode = termNode{field: t.field, value: t.value}
	if t.neg {
		node = notNode{kid: node}
	}
	return node
}

// skipGroup consumes tokens through the close of a group whose open paren was just
// read, tracking nesting so an inner group's close does not end the outer one.
func (p *parser) skipGroup() {
	for depth := 1; p.pos < len(p.toks); p.pos++ {
		switch p.toks[p.pos].kind {
		case tokLParen:
			depth++
		case tokRParen:
			if depth--; depth == 0 {
				p.pos++
				return
			}
		}
	}
}
