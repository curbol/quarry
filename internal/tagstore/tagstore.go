// Package tagstore is the tag store (quarry.tags.toml): a palette of user-defined
// tags (each a label plus a color), per-asset assignments, and link groups, all keyed
// by an asset's content fingerprint. A store found by walking up from the working
// directory belongs to that project and travels with it in source control; otherwise
// the store is the user-wide one in the config directory. Either way it carries no
// account identity and no machine paths.
//
// A link group is an undirected set of fingerprints that "belong together" (a UI
// frame and its background fill, say), so the browse layer can surface companions
// alongside a match. Groups merge transitively: linking {A,B} then {B,C} yields
// {A,B,C}.
//
// The store round-trips faithfully: it has no knowledge of any asset index and
// never prunes assignments or links to a "currently-scanned" set, so they survive a
// resync, a disabled pack, a narrowed browse root, or a move to another machine.
// Since a save rewrites the file whole, a key Load cannot account for is refused
// rather than dropped. A tag's id is its label text (identity); "key:value" labels
// are ordinary ids by convention, not enforced.
package tagstore

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/curbol/quarry/internal/safewrite"
)

// FileName is the tag-store filename, looked for by Discover and used for the
// user-wide store in the config directory.
const FileName = "quarry.tags.toml"

// Discover walks up from startDir looking for a store named FileName, returning its
// path and true on the first hit, or ("", false) at the filesystem root. A hit means
// the working directory sits inside a project that keeps its own tags.
//
// The walk is lexical, which is what makes it terminate: resolving each parent would
// let a symlink cycle run forever. That means startDir has to be absolute for the
// walk to have anywhere to go, so it is made absolute here rather than assumed.
func Discover(startDir string) (string, bool) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// TagDef is one palette entry: a tag's label (id) and its display color (#rrggbb).
type TagDef struct {
	ID    string `toml:"id"`
	Color string `toml:"color"`
}

// assignment is the set of tag ids applied to one content fingerprint.
type assignment struct {
	Fingerprint string   `toml:"fingerprint"`
	Tags        []string `toml:"tags"`
}

// group is one link group: a set of content fingerprints that travel together.
type group struct {
	Fingerprints []string `toml:"fingerprints"`
}

// fileTOML is the on-disk shape; sections serialize as [[tag]], then [[assignment]],
// then [[group]].
type fileTOML struct {
	Tags        []TagDef     `toml:"tag"`
	Assignments []assignment `toml:"assignment"`
	Groups      []group      `toml:"group"`
}

// Store is an in-memory tag store. The palette (colors), assignments, and link
// groups are the source of truth; Load/Save convert to and from the TOML
// representation.
//
// A Store is not safe for concurrent use: callers sharing one across goroutines
// must synchronize externally, around reads as well as writes.
//
// New or Load builds one. The zero value is not usable — its maps are nil, so the
// first Assign panics rather than reporting anything.
type Store struct {
	colors map[string]string          // tag id -> color
	assign map[string]map[string]bool // fingerprint -> set of tag ids
	// groups maps a fingerprint to the member set it belongs to. Every member of a
	// group points at the same set instance (which includes the member itself), so a
	// merge is a repoint and membership/lookup is O(1).
	groups map[string]map[string]bool
	// loadedFrom and stamp describe the file this store was read from, so Save can tell
	// whether anything else has written it since. A zero stamp means the file did not
	// exist at the time, which is itself worth detecting: something creating it before
	// the first save is the same lost edit. Saving to a different path is an export
	// rather than a rewrite, so the stamp check does not apply there — and neither does
	// the path: an export must not become the file this store guards.
	loadedFrom string
	stamp      fileStamp
}

// fileStamp is the cheap identity of a file on disk. Size and mtime are what a
// concurrent write changes, and reading them costs one stat.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), modTime: fi.ModTime()}
}

// ErrStale reports that the store on disk changed after this one was loaded, so
// saving would silently discard whatever made that change.
var ErrStale = errors.New("the tag store on disk has changed since it was loaded")

// New returns an empty store.
func New() *Store {
	return &Store{
		colors: map[string]string{},
		assign: map[string]map[string]bool{},
		groups: map[string]map[string]bool{},
	}
}

var colorRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// NormalizeColor lower-cases and validates a #rrggbb color. It is exported so a
// caller can reject a bad color before mutating the store, rather than discovering
// it partway through a multi-step edit.
func NormalizeColor(c string) (string, error) {
	c = strings.ToLower(strings.TrimSpace(c))
	if !colorRe.MatchString(c) {
		return "", fmt.Errorf("invalid color %q: want #rrggbb", c)
	}
	return c, nil
}

// DefaultColor derives a stable, evenly-spread color from a label, so a new tag
// gets an arbitrary-but-consistent color until the user picks one.
func DefaultColor(label string) string {
	h := fnv.New32a()
	h.Write([]byte(label))
	return hslHex(float64(h.Sum32()%360), 0.62, 0.55)
}

// Define upserts a tag definition with an explicit color.
func (s *Store) Define(id, color string) error {
	if id == "" {
		return fmt.Errorf("empty tag id")
	}
	c, err := NormalizeColor(color)
	if err != nil {
		return err
	}
	s.colors[id] = c
	return nil
}

// ensure guarantees id has a palette entry, giving a new tag its default color.
func (s *Store) ensure(id string) {
	if _, ok := s.colors[id]; !ok {
		s.colors[id] = DefaultColor(id)
	}
}

// Has reports whether a tag is defined.
func (s *Store) has(id string) bool { _, ok := s.colors[id]; return ok }

// Color returns a tag's color and whether it is defined.
func (s *Store) color(id string) (string, bool) { c, ok := s.colors[id]; return c, ok }

// Assign applies a tag to a fingerprint, defining the tag (default color) if new.
func (s *Store) Assign(fp, id string) {
	if fp == "" || id == "" {
		return
	}
	s.ensure(id)
	if s.assign[fp] == nil {
		s.assign[fp] = map[string]bool{}
	}
	s.assign[fp][id] = true
}

// Unassign removes a tag from a fingerprint. The tag stays in the palette.
func (s *Store) Unassign(fp, id string) {
	set := s.assign[fp]
	if set == nil {
		return
	}
	delete(set, id)
	if len(set) == 0 {
		delete(s.assign, fp)
	}
}

// Rename changes a tag's id everywhere. Renaming onto an existing id merges: the
// two tags' assignments collapse (deduped), the surviving def keeps the target's
// color, and the old def is dropped — the store never holds two defs with one id.
func (s *Store) Rename(old, neu string) error {
	if neu == "" {
		return fmt.Errorf("empty tag id")
	}
	oldColor, ok := s.colors[old]
	if !ok {
		// Reported rather than treated as a no-op: a caller that follows a rename with
		// a color edit would otherwise define the new id from nothing, and answer
		// "renamed" while inventing a tag no asset carries. Checked ahead of the
		// identity case below, or renaming a tag to its own name would report success
		// for a tag that does not exist while renaming it to any other name reports the
		// miss.
		return fmt.Errorf("no tag %q to rename", old)
	}
	if old == neu {
		return nil
	}
	if _, exists := s.colors[neu]; !exists {
		s.colors[neu] = oldColor
	}
	delete(s.colors, old)
	for _, set := range s.assign {
		if set[old] {
			delete(set, old)
			set[neu] = true
		}
	}
	return nil
}

// Delete removes a tag from the palette and from every assignment.
func (s *Store) Delete(id string) {
	delete(s.colors, id)
	for fp, set := range s.assign {
		if set[id] {
			delete(set, id)
			if len(set) == 0 {
				delete(s.assign, fp)
			}
		}
	}
}

// TagsFor returns the sorted tag ids applied to a fingerprint.
func (s *Store) TagsFor(fp string) []string { return sortedKeys(s.assign[fp]) }

// HasGroups reports whether any link group exists, so a caller resolving companions
// across a library-sized result set can skip the whole pass when there is nothing to
// resolve — which is the common case, since links are made deliberately and rarely.
func (s *Store) HasGroups() bool { return len(s.groups) > 0 }

// Link groups the given fingerprints so they travel together, absorbing any groups
// they already belong to into one (so linking {A,B} then {B,C} yields {A,B,C}).
// Fewer than two distinct non-empty fingerprints is a no-op: a link needs at least
// two members.
func (s *Store) Link(fps []string) {
	union := map[string]bool{}
	for _, fp := range fps {
		if fp == "" {
			continue
		}
		union[fp] = true
		for m := range s.groups[fp] {
			union[m] = true
		}
	}
	if len(union) < 2 {
		return
	}
	for m := range union {
		s.groups[m] = union
	}
}

// Unlink removes the given fingerprints from their group, dissolving a group that
// would drop below two members.
func (s *Store) Unlink(fps []string) {
	for _, fp := range fps {
		set := s.groups[fp]
		if set == nil {
			continue
		}
		delete(set, fp)
		delete(s.groups, fp)
		if len(set) < 2 {
			for m := range set {
				delete(s.groups, m)
			}
		}
	}
}

// Related returns the other fingerprints grouped with fp, sorted; nil when fp is in
// no group.
func (s *Store) Related(fp string) []string {
	set := s.groups[fp]
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set))
	for m := range set {
		if m != fp {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// Groups returns the link groups, each a sorted member list, ordered by first
// member, for stable persistence and tests.
func (s *Store) Groups() [][]string {
	var out [][]string
	for fp, set := range s.groups {
		if len(set) < 2 {
			continue
		}
		min := fp
		for m := range set {
			if m < min {
				min = m
			}
		}
		if fp != min { // emit each group once, from its lowest member
			continue
		}
		out = append(out, sortedKeys(set))
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// FingerprintsByTag returns the fingerprints carrying each tag. Counts answers "how
// many assignments"; a caller that has to fold those onto something else — the cards
// they land on, or the subset the current index actually holds — needs the
// fingerprints themselves, and building that per tag from the outside would be a pass
// over every assignment per tag rather than one pass in total.
func (s *Store) FingerprintsByTag() map[string][]string {
	m := make(map[string][]string, len(s.colors))
	for fp, set := range s.assign {
		for id := range set {
			m[id] = append(m[id], fp)
		}
	}
	return m
}

// Counts returns the number of fingerprints each tag is applied to.
func (s *Store) Counts() map[string]int {
	m := map[string]int{}
	for _, set := range s.assign {
		for id := range set {
			m[id]++
		}
	}
	return m
}

// Tags returns the palette sorted by id.
func (s *Store) Tags() []TagDef {
	out := make([]TagDef, 0, len(s.colors))
	for id, c := range s.colors {
		out = append(out, TagDef{ID: id, Color: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Load reads the tag store at path, returning an empty store if it does not exist.
// Assignments are preserved verbatim; a tag referenced by an assignment but missing
// a definition (a hand-edited file) is given a default color so the palette stays
// complete.
//
// A key Load does not recognize is an error rather than something to skip. Save
// rewrites the whole file from what Load produced, so anything silently dropped here
// is destroyed on the next edit — and the store is meant to travel between machines
// that may not run the same version, which is exactly when an unknown key shows up.
func Load(path string) (*Store, error) {
	s := New()
	s.loadedFrom = path
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return s, nil
	}
	// Stamped before the contents are read, not after. A write landing between the two
	// would otherwise leave this store holding the old file under the new file's stamp,
	// and the next Save would pass its staleness check and destroy that write with no
	// trace. Stamping first fails the other way — a spurious ErrStale, which the caller
	// already recovers from by reloading.
	stamp := stampOf(path)
	var f fileTOML
	md, err := toml.DecodeFile(path, &f)
	if err != nil {
		// Named, like every other failure here: a TOML parse error carries a line
		// number but not a file, and this one surfaces during a save's recovery reload
		// where the user has no other clue which store was being read.
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		keys := make([]string, len(unknown))
		for i, k := range unknown {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s holds keys this version of quarry does not understand (%s); "+
			"it was likely written by a newer quarry — update, or remove those keys, "+
			"rather than let the next edit drop them", path, strings.Join(keys, ", "))
	}
	// A color this version cannot parse is refused for the same reason an unknown key
	// is: the next save rewrites the file whole, so quietly substituting a default
	// would overwrite what the user typed with something they never chose.
	// A repeated [[tag]] id is refused for the same reason: the last row would win,
	// and the next save would rewrite the file without the color the user typed on the
	// other. Duplicate assignments and overlapping groups both merge losslessly, so
	// this is the one duplicate that destroys something.
	var badColors, dupes []string
	seenTag := map[string]bool{}
	for _, t := range f.Tags {
		if t.ID == "" {
			continue
		}
		if seenTag[t.ID] {
			dupes = append(dupes, t.ID)
			continue
		}
		seenTag[t.ID] = true
		switch c, err := NormalizeColor(t.Color); {
		case err == nil:
			s.colors[t.ID] = c
		case t.Color == "":
			s.colors[t.ID] = DefaultColor(t.ID)
		default:
			badColors = append(badColors, fmt.Sprintf("%s = %q", t.ID, t.Color))
		}
	}
	if len(dupes) > 0 {
		return nil, fmt.Errorf("%s defines the tag(s) %s more than once; keep one [[tag]] row per id, or the next edit drops all but the last",
			path, strings.Join(dupes, ", "))
	}
	if len(badColors) > 0 {
		return nil, fmt.Errorf("%s holds colors quarry cannot read (%s); use #rrggbb, or remove the color to get a generated one",
			path, strings.Join(badColors, ", "))
	}
	for _, a := range f.Assignments {
		if a.Fingerprint == "" {
			continue
		}
		for _, id := range a.Tags {
			s.Assign(a.Fingerprint, id)
		}
	}
	for _, g := range f.Groups {
		s.Link(g.Fingerprints)
	}
	s.stamp = stamp
	return s, nil
}

// Reload replaces the store's contents with what is on disk, in place. It is what a
// caller uses to recover after a failed Save: the mutation that did not reach the
// file must not survive in memory, or the UI keeps showing an edit the store does
// not have. Reloading in place rather than handing back a new *Store is deliberate —
// a pointer swap is a step a caller can forget while still holding the write lock.
// On error the store is left exactly as it was; the caller then knows memory and
// disk have diverged and can refuse further writes.
//
// path is load-bearing beyond what is read: it becomes the file Save will guard, so
// reloading from somewhere else re-homes the store there and leaves the file it was
// actually editing unguarded. Recovery passes the path it saved to, which is the
// same one, and nothing else should call this.
func (s *Store) Reload(path string) error {
	fresh, err := Load(path)
	if err != nil {
		return err
	}
	*s = *fresh
	return nil
}

// storeHeader leads every saved store. See Save for why it is there.
const storeHeader = "# quarry tag store. Rewritten whole on every edit, so comments are not preserved.\n\n"

// Save writes the store at path atomically, with tags sorted by id, assignments
// sorted by fingerprint, groups sorted by first member, and every member list
// sorted, for minimal diffs.
//
// A save rewrites the whole file, so it first checks that the file is still the one
// Load read and returns ErrStale if not. Without that, an edit made meanwhile — by
// hand, by a checkout of a committed project store, or by a second quarry sharing
// the user-wide one — is destroyed with no trace. Stat-then-rename leaves a window
// too small to matter here and too expensive to close: this is one user's file, and
// the failure being guarded against is measured in minutes, not microseconds.
// The rule is that only the file this store read may be rewritten. A save anywhere
// else is an export, so it is allowed onto a path that does not exist and refused onto
// one that does — a store that never read that file cannot tell what it would be
// destroying, which is the same thing the staleness check is for. The first save of a
// store that has never read a file adopts that file, and a store that has one keeps
// it: an export leaves the guard where it was. Either way a successful save re-stamps
// the guarded file, which is what the check reads next time — so this mutates the
// store, and a caller sharing a Store across goroutines must hold the lock it holds
// for an edit, not the one it holds for a read.
func (s *Store) Save(path string) error {
	switch {
	case s.loadedFrom == path:
		if s.stamp != stampOf(path) {
			return fmt.Errorf("%s: %w", path, ErrStale)
		}
	default:
		// Only the file this store read may be rewritten. Everything else — a store from
		// New() that has read nothing, and a store saving somewhere other than where it
		// loaded from — is writing a file it has never seen, which is the same total loss
		// ErrStale exists to prevent, minus even the chance to notice: there is no earlier
		// state to compare against because there was never a read. An export to a path
		// that does not exist yet is the one shape that is not a rewrite, so that is the
		// one this allows.
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s: %w", path, ErrStale)
		}
	}
	f := fileTOML{Tags: s.Tags()}
	fps := make([]string, 0, len(s.assign))
	for fp, set := range s.assign {
		if len(set) > 0 {
			fps = append(fps, fp)
		}
	}
	sort.Strings(fps)
	for _, fp := range fps {
		f.Assignments = append(f.Assignments, assignment{Fingerprint: fp, Tags: sortedKeys(s.assign[fp])})
	}
	for _, g := range s.Groups() {
		f.Groups = append(f.Groups, group{Fingerprints: g})
	}

	if err := safewrite.Atomic(path, ".quarry-tags-*", func(w io.Writer) error {
		// The store is meant to be hand-edited and committed, and a save rewrites it
		// whole from a model that has no place to keep a comment. Saying so in the file
		// is the only warning a user gets before their first tag click removes what
		// they wrote; refusing to save over a comment would be worse.
		if _, err := io.WriteString(w, storeHeader); err != nil {
			return err
		}
		return toml.NewEncoder(w).Encode(f)
	}); err != nil {
		return err
	}
	// A home is adopted once and never moves. Re-homing onto whatever was written last
	// would leave the store the user is actually editing unguarded from then on: an
	// export elsewhere, then an outside edit to the real file, then a save that sails
	// through its staleness check because it is no longer checking that file.
	if s.loadedFrom == "" {
		s.loadedFrom = path
	}
	if s.loadedFrom == path {
		s.stamp = stampOf(path)
	}
	return nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hslHex converts an HSL color (h in degrees, s and l in [0,1]) to #rrggbb.
func hslHex(h, s, l float64) string {
	c := (1 - abs(2*l-1)) * s
	x := c * (1 - abs(mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return fmt.Sprintf("#%02x%02x%02x", to255(r+m), to255(g+m), to255(b+m))
}

func to255(v float64) int { return int(v*255 + 0.5) }
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func mod(a, b float64) float64 {
	m := a - b*float64(int(a/b))
	if m < 0 {
		m += b
	}
	return m
}
