package tagstore

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.tags.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tags()) != 0 || len(s.Counts()) != 0 {
		t.Errorf("missing file should load empty, got %d tags", len(s.Tags()))
	}
}

func TestDefineAssignRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	if err := s.Define("hero", "#E11D48"); err != nil { // upper-case normalizes
		t.Fatal(err)
	}
	s.Assign("crc32:abc:10", "hero")
	s.Assign("crc32:abc:10", "wip")
	s.Assign("uguid:xyz", "hero")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := got.Color("hero"); c != "#e11d48" {
		t.Errorf("hero color = %q, want normalized #e11d48", c)
	}
	if !reflect.DeepEqual(got.TagsFor("crc32:abc:10"), []string{"hero", "wip"}) {
		t.Errorf("tags for crc32:abc:10 = %v", got.TagsFor("crc32:abc:10"))
	}
	if got.Counts()["hero"] != 2 {
		t.Errorf("hero count = %d, want 2", got.Counts()["hero"])
	}
}

// The store never prunes to a scanned set: an assignment for any fingerprint
// survives a save+load, which is the "tags survive resync / travel across
// machines" guarantee.
func TestAssignmentsPreservedRegardlessOfIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Assign("crc32:notinanyindex:999", "keep")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.TagsFor("crc32:notinanyindex:999"), []string{"keep"}) {
		t.Errorf("assignment for an unknown fingerprint was dropped: %v", got.TagsFor("crc32:notinanyindex:999"))
	}
}

func TestRenameRewritesAssignments(t *testing.T) {
	s := New()
	s.Define("wip", "#123456")
	s.Assign("fp1", "wip")
	s.Assign("fp2", "wip")
	if err := s.Rename("wip", "in-progress"); err != nil {
		t.Fatal(err)
	}
	if s.Has("wip") {
		t.Error("old id still present after rename")
	}
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"in-progress"}) || !reflect.DeepEqual(s.TagsFor("fp2"), []string{"in-progress"}) {
		t.Errorf("rename did not rewrite assignments: fp1=%v fp2=%v", s.TagsFor("fp1"), s.TagsFor("fp2"))
	}
	if c, _ := s.Color("in-progress"); c != "#123456" {
		t.Errorf("renamed tag lost its color: %q", c)
	}
}

func TestRenameOntoExistingMerges(t *testing.T) {
	s := New()
	s.Define("a", "#aaaaaa")
	s.Define("b", "#bbbbbb")
	s.Assign("fp1", "a")
	s.Assign("fp1", "b") // fp1 has both
	s.Assign("fp2", "a") // fp2 has only a

	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if s.Has("a") {
		t.Error("merged-away id still present")
	}
	// fp1 collapses a+b to a single b; fp2's a becomes b.
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"b"}) {
		t.Errorf("fp1 after merge = %v, want [b]", s.TagsFor("fp1"))
	}
	if !reflect.DeepEqual(s.TagsFor("fp2"), []string{"b"}) {
		t.Errorf("fp2 after merge = %v, want [b]", s.TagsFor("fp2"))
	}
	if c, _ := s.Color("b"); c != "#bbbbbb" {
		t.Errorf("merge should keep target color, got %q", c)
	}
	if s.Counts()["b"] != 2 {
		t.Errorf("b count = %d, want 2", s.Counts()["b"])
	}
}

func TestDeletePurgesAssignments(t *testing.T) {
	s := New()
	s.Assign("fp1", "gone")
	s.Assign("fp1", "stay")
	s.Assign("fp2", "gone")
	s.Delete("gone")
	if s.Has("gone") {
		t.Error("deleted tag still in palette")
	}
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"stay"}) {
		t.Errorf("fp1 = %v, want [stay]", s.TagsFor("fp1"))
	}
	if len(s.TagsFor("fp2")) != 0 {
		t.Errorf("fp2 should have no tags after delete, got %v", s.TagsFor("fp2"))
	}
}

func TestUnassignKeepsPaletteEntry(t *testing.T) {
	s := New()
	s.Assign("fp1", "solo")
	s.Unassign("fp1", "solo")
	if !s.Has("solo") {
		t.Error("unassign should keep the tag in the palette")
	}
	if len(s.TagsFor("fp1")) != 0 {
		t.Error("unassign left the assignment")
	}
}

// Ordering only. That a save is reproducible is TestLoadSaveRoundTripIsByteIdentical's
// job, and it proves more: saving the same in-memory store twice only shows map
// iteration does not leak, where a load-then-save shows the file survives the trip.
func TestSaveIsSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := New()
	s.Define("zebra", "#000000")
	s.Define("alpha", "#ffffff")
	s.Assign("fp-b", "zebra")
	s.Assign("fp-a", "zebra")
	s.Assign("fp-a", "alpha")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(path)
	text := string(b1)

	// Tags sorted by id: alpha before zebra.
	if strings.Index(text, `id = "alpha"`) > strings.Index(text, `id = "zebra"`) {
		t.Errorf("tags not sorted by id:\n%s", text)
	}
	// Assignments sorted by fingerprint: fp-a before fp-b.
	if strings.Index(text, "fp-a") > strings.Index(text, "fp-b") {
		t.Errorf("assignments not sorted by fingerprint:\n%s", text)
	}
	// Each assignment's tags are sorted: fp-a lists alpha before zebra.
	if !strings.Contains(text, `tags = ["alpha", "zebra"]`) {
		t.Errorf("assignment tags not sorted (want [\"alpha\", \"zebra\"]):\n%s", text)
	}
}

func TestDefaultColorDeterministicAndValid(t *testing.T) {
	a := DefaultColor("biome:forest")
	b := DefaultColor("biome:forest")
	if a != b {
		t.Errorf("DefaultColor not deterministic: %q vs %q", a, b)
	}
	if !colorRe.MatchString(a) {
		t.Errorf("DefaultColor %q is not #rrggbb", a)
	}
	if DefaultColor("hero") == DefaultColor("villain") {
		t.Error("distinct labels should generally get distinct default colors")
	}
}

func TestLinkMergesTransitively(t *testing.T) {
	s := New()
	s.Link([]string{"A", "B"})
	s.Link([]string{"B", "C"}) // overlaps on B, so all three merge
	if !reflect.DeepEqual(s.Related("A"), []string{"B", "C"}) {
		t.Errorf("Related(A) = %v, want [B C]", s.Related("A"))
	}
	if !reflect.DeepEqual(s.Related("C"), []string{"A", "B"}) {
		t.Errorf("Related(C) = %v, want [A B]", s.Related("C"))
	}
	if g := s.Groups(); len(g) != 1 || !reflect.DeepEqual(g[0], []string{"A", "B", "C"}) {
		t.Errorf("Groups() = %v, want [[A B C]]", g)
	}
}

func TestLinkNeedsTwoMembers(t *testing.T) {
	s := New()
	s.Link([]string{"solo"})
	s.Link([]string{"", "x"}) // empty filtered out, leaves a single member
	if s.Related("solo") != nil || s.Related("x") != nil {
		t.Errorf("a single distinct fingerprint must not form a group")
	}
	if len(s.Groups()) != 0 {
		t.Errorf("Groups() = %v, want none", s.Groups())
	}
}

func TestUnlinkDissolves(t *testing.T) {
	s := New()
	s.Link([]string{"A", "B", "C"})
	s.Unlink([]string{"B"})
	if s.Related("B") != nil {
		t.Errorf("Related(B) after unlink = %v, want nil", s.Related("B"))
	}
	if !reflect.DeepEqual(s.Related("A"), []string{"C"}) {
		t.Errorf("Related(A) = %v, want [C]", s.Related("A"))
	}
	// Removing A leaves only C, which cannot be a group on its own: it dissolves.
	s.Unlink([]string{"A"})
	if s.Related("C") != nil {
		t.Errorf("Related(C) = %v, want nil after group dissolves", s.Related("C"))
	}
	if len(s.Groups()) != 0 {
		t.Errorf("Groups() = %v, want none", s.Groups())
	}
}

// Link groups round-trip and, like assignments, are never pruned to a scanned set.
func TestLinksRoundTripSortedAndPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Link([]string{"crc32:zzz:9", "uguid:aaa"}) // unsorted input, unknown to any index
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), `fingerprints = ["crc32:zzz:9", "uguid:aaa"]`) {
		t.Errorf("group members not sorted in file:\n%s", text)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Related("crc32:zzz:9"), []string{"uguid:aaa"}) {
		t.Errorf("link lost across save/load: Related = %v", got.Related("crc32:zzz:9"))
	}
}

func TestDefineRejectsBadColor(t *testing.T) {
	s := New()
	if err := s.Define("t", "red"); err == nil {
		t.Error("expected error for non-hex color")
	}
	if err := s.Define("t", "#12345"); err == nil {
		t.Error("expected error for short hex")
	}
	if err := s.Define("", "#123456"); err == nil {
		t.Error("expected error for empty id")
	}
}

func TestDiscoverWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := Discover(sub)
	if !ok || got != filepath.Join(root, FileName) {
		t.Errorf("Discover(%q) = %q, %v; want the store at the root", sub, got, ok)
	}
}

func TestDiscoverStopsAtFilesystemRoot(t *testing.T) {
	// A temp dir has no store above it up to /, so the walk must terminate rather
	// than loop on the root's own parent.
	if got, ok := Discover(t.TempDir()); ok {
		t.Errorf("Discover found %q, want no hit", got)
	}
}

// A project store sits inside the user's own repo, so a failed save must not strand
// a temp file there: .gitignore covers the store's name, not the temp pattern.
//
// The store is loaded and up to date, so the staleness guards pass and the failure is
// the one this is about: safewrite.Atomic itself. Reaching it is the whole point —
// a store that never got that far would satisfy every assertion below by writing
// nothing at all.
func TestFailedSaveLeavesNoTempFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into an unwritable directory anyway")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	seed := New()
	seed.Define("hero", "#112233")
	if err := seed.Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Assign("crc32:aa:1", "hero")

	// The directory, not the file: the store's own size and mtime have to stay put or
	// the stale check fires first and the write is never attempted.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	saveErr := s.Save(p)
	os.Chmod(dir, 0o755)
	if saveErr == nil {
		t.Fatal("Save reported success writing into an unwritable directory")
	}
	if errors.Is(saveErr, ErrStale) {
		t.Fatalf("Save = %v, want a write failure: the staleness guard fired instead of safewrite.Atomic", saveErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".quarry-tags-") {
			t.Errorf("failed save left %s behind", e.Name())
		}
	}
	if got := s.TagsFor("crc32:aa:1"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("TagsFor = %v after the failed save, want the edit still in memory for the caller to Reload away", got)
	}
	after, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TagsFor("crc32:aa:1"); len(got) != 0 {
		t.Errorf("on disk TagsFor = %v, want the failed write to have changed nothing", got)
	}
}

// A store from New() has read nothing, so rewriting a file that already exists would
// destroy it whole with no earlier state to have noticed the loss against. That is the
// same total loss ErrStale guards a loaded store from, and it is refused the same way —
// including when what is already there is a directory rather than a store.
func TestSaveFromANeverLoadedStoreRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, FileName)
	if err := os.WriteFile(existing, []byte("# someone else's store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.Assign("crc32:aa:1", "hero")
	if err := s.Save(existing); !errors.Is(err, ErrStale) {
		t.Fatalf("Save = %v, want ErrStale: a store that read nothing rewrote a file it never saw", err)
	}
	b, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "# someone else's store\n" {
		t.Errorf("the file was rewritten: %q", b)
	}

	blocked := filepath.Join(dir, "sub", FileName)
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(blocked); err == nil {
		t.Error("Save reported success onto a directory")
	}
}

// Reload is the recovery every failed write goes through, so its own failure has to
// leave the store exactly as it was: browse reports that case to the user rather than
// swallowing it, and a half-applied reload would make what it reports untrue.
func TestFailedReloadLeavesTheStoreUntouched(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Define("hero", "#112233")
	s.Assign("crc32:aa:1", "hero")
	s.Link([]string{"crc32:aa:1", "crc32:bb:2"})
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	// A key this version does not know is one of the things Load refuses outright.
	if err := os.WriteFile(p, []byte("unknown_key = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(p); err == nil {
		t.Fatal("Reload accepted a file Load refuses")
	}

	if _, ok := s.Color("hero"); !ok {
		t.Error("the palette lost a tag to a reload that failed")
	}
	if got := s.TagsFor("crc32:aa:1"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("TagsFor = %v, want the assignment untouched by a reload that failed", got)
	}
	if got := s.Related("crc32:aa:1"); len(got) != 1 || got[0] != "crc32:bb:2" {
		t.Errorf("Related = %v, want the link untouched by a reload that failed", got)
	}
}

// Save rewrites the file whole from what Load produced, so a key Load quietly
// skipped would be destroyed by the next tag edit. The store is meant to travel
// between machines that may not run the same quarry, which is exactly when a key
// this version has never heard of turns up.
func TestLoadRefusesUnknownKeys(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{"unknown field on a tag", "[[tag]]\n  id = \"hero\"\n  color = \"#e11d48\"\n  icon = \"sword\"\n", "tag.icon"},
		{"unknown field on an assignment", "[[assignment]]\n  fingerprint = \"crc32:aa:1\"\n  tags = [\"hero\"]\n  note = \"x\"\n", "assignment.note"},
		{"unknown section", "[[collection]]\n  name = \"favourites\"\n", "collection"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(p, []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatal("Load accepted a file whose keys it would drop on the next save")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// A store may be hand-edited to tag something without spelling out the palette
// entry. That is a complete file, not a broken one: the tag gets its default color
// so the palette still describes every tag in use.
func TestLoadGivesAnUndefinedTagItsDefaultColor(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	body := "[[assignment]]\n  fingerprint = \"crc32:aa:1\"\n  tags = [\"hero\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Color("hero")
	if !ok {
		t.Fatal("hero is assigned but absent from the palette")
	}
	if got != DefaultColor("hero") {
		t.Errorf("color = %q, want the default %q", got, DefaultColor("hero"))
	}
}

// The store is committed to source control, so a load/save cycle that changes even a
// byte turns every quarry run into a spurious diff. Saving the same in-memory store
// twice only proves map iteration doesn't leak; the drift that matters would come in
// on the Load side.
func TestLoadSaveRoundTripIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, FileName)
	s := New()
	s.Define("zebra", "#000000")
	s.Define("alpha", "#ffffff")
	s.Assign("fp-b", "zebra")
	s.Assign("fp-a", "zebra")
	s.Assign("fp-a", "alpha")
	s.Link([]string{"fp-a", "fp-b"})
	s.Link([]string{"fp-c", "fp-d"})
	if err := s.Save(first); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(first)
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "round-trip-"+FileName)
	if err := reloaded.Save(second); err != nil {
		t.Fatal(err)
	}
	a, b := readFile(t, first), readFile(t, second)
	if a != b {
		t.Errorf("a load/save round trip rewrote the file:\n--- saved ---\n%s\n--- reloaded and saved ---\n%s", a, b)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Groups emits each group once, from its lowest member. With a single group that
// holds however it is written, so the ordering and the emit-once rule are only really
// exercised by two.
func TestGroupsEmitsEachGroupOnceInOrder(t *testing.T) {
	s := New()
	s.Link([]string{"c-fp", "d-fp"})
	s.Link([]string{"b-fp", "a-fp"})
	want := [][]string{{"a-fp", "b-fp"}, {"c-fp", "d-fp"}}
	if got := s.Groups(); !reflect.DeepEqual(got, want) {
		t.Errorf("Groups() = %v, want %v", got, want)
	}

	// A transitive merge collapses two into one, still emitted once.
	s.Link([]string{"b-fp", "c-fp"})
	wantMerged := [][]string{{"a-fp", "b-fp", "c-fp", "d-fp"}}
	if got := s.Groups(); !reflect.DeepEqual(got, wantMerged) {
		t.Errorf("after a transitive link, Groups() = %v, want %v", got, wantMerged)
	}
}

// A group needs two members to mean anything. These shapes can only arrive by hand,
// and dropping them is the documented behavior — pinned here because silently
// dropping user data is exactly what the unknown-key refusal exists to prevent.
func TestLoadDropsDegenerateGroups(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"one member", "[[group]]\n  fingerprints = [\"solo\"]\n"},
		{"no members", "[[group]]\n  fingerprints = []\n"},
		{"one member repeated", "[[group]]\n  fingerprints = [\"same\", \"same\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := s.Groups(); len(got) != 0 {
				t.Errorf("Groups() = %v, want none: a group of fewer than two is not a group", got)
			}
		})
	}
}

// Links are result expansion, nothing more: they travel companions into a query's
// results without ever changing what tags a fingerprint carries. Both the package doc
// and the design doc promise this and nothing checked it.
func TestLinkingNeverChangesTags(t *testing.T) {
	s := New()
	s.Assign("fp-a", "hero")
	s.Link([]string{"fp-a", "fp-b"})

	if got := s.TagsFor("fp-b"); len(got) != 0 {
		t.Errorf("TagsFor(fp-b) = %v after linking to a tagged fingerprint, want none", got)
	}
	if got := s.TagsFor("fp-a"); !reflect.DeepEqual(got, []string{"hero"}) {
		t.Errorf("TagsFor(fp-a) = %v, want [hero] unchanged by the link", got)
	}
	if got := s.Related("fp-b"); !reflect.DeepEqual(got, []string{"fp-a"}) {
		t.Errorf("Related(fp-b) = %v, want [fp-a]", got)
	}

	s.Unlink([]string{"fp-a", "fp-b"})
	if got := s.TagsFor("fp-a"); !reflect.DeepEqual(got, []string{"hero"}) {
		t.Errorf("TagsFor(fp-a) = %v after unlinking, want [hero] still", got)
	}
	if got := s.Related("fp-a"); len(got) != 0 {
		t.Errorf("Related(fp-a) = %v after unlinking, want none", got)
	}
}

// A save rewrites the file whole, so it must not overwrite an edit made since the load
// — by hand, by a checkout of a committed store, or by a second quarry sharing the
// user-wide one. Losing that edit is total and leaves no trace.
func TestSaveRefusesToClobberAnEditMadeSinceLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	seed := New()
	seed.Define("hero", "#112233")
	if err := seed.Save(p); err != nil {
		t.Fatal(err)
	}

	mine, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	mine.Assign("fp-a", "hero")

	// Someone else writes the file in the meantime.
	theirs, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	theirs.Define("villain", "#445566")
	if err := theirs.Save(p); err != nil {
		t.Fatal(err)
	}

	err = mine.Save(p)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Save = %v, want ErrStale: the other edit would have been destroyed", err)
	}
	// Save does not roll back, which is exactly why the caller has to Reload: the
	// browse server's recovery is written against this, and a Save that quietly undid
	// its own mutation would make that recovery look unnecessary.
	if got := mine.TagsFor("fp-a"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("TagsFor(fp-a) = %v right after the refused save, want the unsaved edit still in memory", got)
	}
	after, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Color("villain"); !ok {
		t.Error("the other writer's tag is gone from the file")
	}

	// Reload is the recovery: it brings this store back in line with disk in place, so
	// the caller keeps its pointer and the rejected edit does not survive in memory.
	if err := mine.Reload(p); err != nil {
		t.Fatal(err)
	}
	if got := mine.TagsFor("fp-a"); len(got) != 0 {
		t.Errorf("TagsFor(fp-a) = %v after a reload, want the unsaved edit gone", got)
	}
	if _, ok := mine.Color("villain"); !ok {
		t.Error("Reload did not pick up the other writer's tag")
	}
	if err := mine.Save(p); err != nil {
		t.Errorf("Save after Reload: %v, want it to succeed now that the store matches disk", err)
	}
}

// Saving to a path this store was not loaded from is a fresh write, not a rewrite of
// something that could have changed underneath — exporting a store must not be
// mistaken for clobbering one.
func TestSaveToADifferentPathIsNotStale(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	s := New()
	s.Define("hero", "#112233")
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(filepath.Join(dir, "copy-"+FileName)); err != nil {
		t.Errorf("Save to a second path: %v", err)
	}
}

// Rename must say when there is nothing to rename, whatever the new name is.
// Reporting success let a caller that follows a rename with a color edit define the
// new id from nothing, answering "renamed" while inventing a tag no asset carries —
// and short-circuiting the identity case first hid the miss for `Rename(x, x)`.
func TestRenameReportsAMissingTag(t *testing.T) {
	for _, tc := range []struct {
		name, old, neu string
		define         string
		wantErr        bool
	}{
		{name: "missing source", old: "ghost", neu: "villain", wantErr: true},
		{name: "missing source, unchanged name", old: "ghost", neu: "ghost", wantErr: true},
		{name: "missing source, empty name", old: "ghost", neu: "", wantErr: true},
		{name: "present source", define: "hero", old: "hero", neu: "champion"},
		{name: "present source, unchanged name", define: "hero", old: "hero", neu: "hero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			if tc.define != "" {
				if err := s.Define(tc.define, "#112233"); err != nil {
					t.Fatal(err)
				}
			}
			err := s.Rename(tc.old, tc.neu)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Rename(%q, %q) = %v, wantErr %v", tc.old, tc.neu, err, tc.wantErr)
			}
			if tc.wantErr && tc.neu != "" {
				if _, ok := s.Color(tc.neu); ok {
					t.Errorf("%q was defined despite the source not existing", tc.neu)
				}
			}
		})
	}
}

// A color quarry cannot parse is refused rather than quietly replaced: the next save
// rewrites the file whole, so substituting a default would overwrite what the user
// typed with something they never chose.
func TestLoadRefusesAnUnreadableColor(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	body := "[[tag]]\n  id = \"hero\"\n  color = \"red\"\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("a color quarry cannot read was accepted and would be rewritten on the next save")
	}
	if !strings.Contains(err.Error(), "hero") {
		t.Errorf("error %q does not name the offending tag", err)
	}
}

// A repeated [[tag]] id would silently keep the last row's color and the next save
// would rewrite the file without the other. Duplicate assignments and overlapping
// groups merge losslessly, so this is the one duplicate that destroys what was typed.
func TestLoadRefusesADuplicateTagID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	os.WriteFile(p, []byte(`
[[tag]]
  id = "hero"
  color = "#e11d48"
[[tag]]
  id = "hero"
  color = "#0ea5e9"
`), 0o644)

	_, err := Load(p)
	if err == nil {
		t.Fatal("a duplicate tag id was accepted; one of the two colors is silently dropped")
	}
	if !strings.Contains(err.Error(), "hero") || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error = %v, want one naming the repeated id", err)
	}
}

// Groups are orthogonal to tags: a link never changes what tags a fingerprint
// carries, and neither does a tag edit change what a fingerprint is linked to.
// Rename and Delete are the two that sweep broadly enough to break this.
func TestTagEditsLeaveLinkGroupsAlone(t *testing.T) {
	s := New()
	s.Link([]string{"fp-a", "fp-b"})
	s.Assign("fp-a", "hero")

	if err := s.Rename("hero", "champion"); err != nil {
		t.Fatal(err)
	}
	if got := s.Related("fp-a"); !reflect.DeepEqual(got, []string{"fp-b"}) {
		t.Errorf("Related(fp-a) = %v after a rename, want [fp-b]", got)
	}
	s.Delete("champion")
	if got := s.Related("fp-a"); !reflect.DeepEqual(got, []string{"fp-b"}) {
		t.Errorf("Related(fp-a) = %v after a delete, want [fp-b]", got)
	}
	if got := s.Related("fp-b"); !reflect.DeepEqual(got, []string{"fp-a"}) {
		t.Errorf("Related(fp-b) = %v after a delete, want [fp-a]", got)
	}
}

// A file that is not TOML at all is the one load failure whose alternative outcome is
// silent: returning an empty store would let the next edit rewrite the file with
// nothing in it. Every other "refuses" test here feeds syntactically valid TOML, so
// the parse-error path — and the reason it names the file — went unexercised.
func TestLoadRefusesMalformedTOML(t *testing.T) {
	for _, body := range []string{
		"[[tag\n  id = \"hero\"\n",
		"id = \n",
		"[[tag]]\n  id = \"unterminated\n",
	} {
		p := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load(p)
		if err == nil {
			t.Errorf("Load(%q) returned a store with %d tags; a file quarry cannot parse must not read as an empty one", body, len(s.Tags()))
			continue
		}
		// A TOML parse error carries a line number but not a file, and this one
		// surfaces during a failed save's recovery reload, where nothing else says
		// which store was being read.
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not name the file", err)
		}
	}
}

// Every user string lands in the file as a TOML value, and the encoder escapes them —
// but a fingerprint already carries ":" and "#", a split-GLB clip name is an arbitrary
// vendor string, and a tag id is whatever the user typed. This pins the round trip so
// a future change to fileTOML's shape (in particular, moving a fingerprint into a key
// position, where the escaping rules differ) cannot pass unnoticed.
func TestAwkwardLabelsAndFingerprintsRoundTrip(t *testing.T) {
	labels := []string{`say "hi"`, `back\slash`, "line\nbreak", "tab\there", "emoji 🎯", "key:value"}
	fps := []string{
		`crc32:abc:12#Walk "fast"`,
		`crc32:def:34#back\slash`,
		"crc32:aaa:1#new\nline",
		"uguid:0123456789abcdef",
	}
	s := New()
	for _, fp := range fps {
		for _, l := range labels {
			s.Assign(fp, l)
		}
	}
	s.Link(fps)

	p := filepath.Join(t.TempDir(), FileName)
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatalf("reloading a store with awkward strings: %v", err)
	}
	for _, fp := range fps {
		got := back.TagsFor(fp)
		if len(got) != len(labels) {
			t.Errorf("TagsFor(%q) = %v, want all %d labels", fp, got, len(labels))
		}
	}
	if got := len(back.Groups()); got != 1 {
		t.Errorf("groups = %d, want the one link group", got)
	}
	// And the file it wrote is the file it writes again: an escape that decoded to
	// something else would show up here rather than as a silently altered tag.
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Save(p); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("re-saving changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// The staleness check is what stands between an outside edit and a rewrite that
// destroys it, and the file it checks has to stay the file the store reads. A save
// elsewhere — a backup, an export — is not a change of home: if it were, the real
// store would be unguarded from then on, and the very next tag click would overwrite
// whatever an editor or a checkout had put there.
func TestSaveElsewhereDoesNotMoveTheGuardedFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, FileName)
	if err := os.WriteFile(real, []byte("[[tag]]\n  id = \"hero\"\n  color = \"#e11d48\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(real)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(filepath.Join(dir, "backup.toml")); err != nil {
		t.Fatal(err)
	}
	// Someone else edits the store this one is actually for.
	if err := os.WriteFile(real, []byte("[[tag]]\n  id = \"villain\"\n  color = \"#00ff00\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(real); !errors.Is(err, ErrStale) {
		t.Fatalf("Save after an outside edit = %v, want ErrStale; the export moved the guard", err)
	}
	b, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "villain") {
		t.Errorf("the outside edit was overwritten: %s", b)
	}
}

// A store from New() has read nothing, so it has no earlier state to compare a file
// against and no business rewriting one whole. Saving to a path that is not there yet
// is the ordinary first save and has to keep working.
func TestNewStoreWillNotOverwriteAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, FileName)
	original := "[[tag]]\n  id = \"hero\"\n  color = \"#e11d48\"\n"
	if err := os.WriteFile(occupied, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New().Save(occupied); !errors.Is(err, ErrStale) {
		t.Fatalf("New().Save over an existing store = %v, want ErrStale", err)
	}
	b, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Errorf("the existing store was rewritten: %s", b)
	}

	fresh := filepath.Join(dir, "new.toml")
	s := New()
	s.Assign("crc32:1:1", "hero")
	if err := s.Save(fresh); err != nil {
		t.Fatalf("first save to a path that does not exist: %v", err)
	}
	// Having adopted it, the same store keeps writing there.
	if err := s.Save(fresh); err != nil {
		t.Fatalf("second save to the file it just wrote: %v", err)
	}
}

// Groups() is the only funnel between the shared-set representation and the file, so
// the persistence half of unlinking lives there: a shrunk group has to come back
// shrunk, and a dissolved one has to leave no row at all — a one-member [[group]]
// would load as a fingerprint linked to nothing.
func TestUnlinkSurvivesARoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Link([]string{"A", "B", "C"})
	s.Unlink([]string{"B"})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Related("A"); !reflect.DeepEqual(got, []string{"C"}) {
		t.Errorf("Related(A) after a round trip = %v, want [C]", got)
	}
	if got := back.Related("B"); len(got) != 0 {
		t.Errorf("Related(B) = %v, want nothing; B was unlinked", got)
	}

	back.Unlink([]string{"A"})
	if err := back.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "[[group]]") {
		t.Errorf("a dissolved group still wrote a row:\n%s", b)
	}
}
