package tagstore

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// colorOf answers the two questions the tests ask of the palette — is this tag
// defined, and what colour did it end up — through Tags(), which is the accessor
// browse's palette actually reads. Asserting through it means a change that breaks
// the palette breaks these too.
func colorOf(s *Store, id string) (string, bool) {
	for _, t := range s.Tags() {
		if t.ID == id {
			return t.Color, true
		}
	}
	return "", false
}

// hasTag reports whether the palette defines a tag, through the same accessor.
func hasTag(s *Store, id string) bool {
	_, ok := colorOf(s, id)
	return ok
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.tags.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tags()) != 0 || len(s.FingerprintsByTag()) != 0 {
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
	if c, _ := colorOf(got, "hero"); c != "#e11d48" {
		t.Errorf("hero color = %q, want normalized #e11d48", c)
	}
	if !reflect.DeepEqual(got.TagsFor("crc32:abc:10"), []string{"hero", "wip"}) {
		t.Errorf("tags for crc32:abc:10 = %v", got.TagsFor("crc32:abc:10"))
	}
	if n := len(got.FingerprintsByTag()["hero"]); n != 2 {
		t.Errorf("hero is on %d fingerprints, want 2", n)
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
	if hasTag(s, "wip") {
		t.Error("old id still present after rename")
	}
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"in-progress"}) || !reflect.DeepEqual(s.TagsFor("fp2"), []string{"in-progress"}) {
		t.Errorf("rename did not rewrite assignments: fp1=%v fp2=%v", s.TagsFor("fp1"), s.TagsFor("fp2"))
	}
	if c, _ := colorOf(s, "in-progress"); c != "#123456" {
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
	if hasTag(s, "a") {
		t.Error("merged-away id still present")
	}
	// fp1 collapses a+b to a single b; fp2's a becomes b.
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"b"}) {
		t.Errorf("fp1 after merge = %v, want [b]", s.TagsFor("fp1"))
	}
	if !reflect.DeepEqual(s.TagsFor("fp2"), []string{"b"}) {
		t.Errorf("fp2 after merge = %v, want [b]", s.TagsFor("fp2"))
	}
	if c, _ := colorOf(s, "b"); c != "#bbbbbb" {
		t.Errorf("merge should keep target color, got %q", c)
	}
	if n := len(s.FingerprintsByTag()["b"]); n != 2 {
		t.Errorf("b is on %d fingerprints, want 2", n)
	}
}

func TestDeletePurgesAssignments(t *testing.T) {
	s := New()
	s.Assign("fp1", "gone")
	s.Assign("fp1", "stay")
	s.Assign("fp2", "gone")
	s.Delete("gone")
	if hasTag(s, "gone") {
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
	if !hasTag(s, "solo") {
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
	// Linking a pair already in the group rebuilds the same set rather than splitting
	// one off or duplicating the group. The UI sends the whole selection on every
	// click, so this is the ordinary case, not an edge one.
	s.Link([]string{"A", "C"})
	if g := s.Groups(); len(g) != 1 || !reflect.DeepEqual(g[0], []string{"A", "B", "C"}) {
		t.Errorf("re-linking a pair already in the group gave %v, want [[A B C]]", g)
	}
	// And a fingerprint linked to itself is not a group: a group of one is what Groups
	// filters out, and forming one would put a row in the file nothing can reach.
	s.Link([]string{"D", "D"})
	if s.Related("D") != nil {
		t.Errorf("Related(D) = %v after linking D to itself", s.Related("D"))
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

// The walk has to terminate at "/" rather than loop on the root's own parent, whose
// Dir is itself. Asserting that a temp dir turns up nothing would test the machine
// instead: Discover walks the real tree, so anyone with a store at or above $TMPDIR —
// TMPDIR set inside a project, or one `quarry --tags /tmp/...` run once — gets a
// failure naming a path nothing here wrote. What is actually quarry's to promise is
// that the walk ends, and that whatever it returns is a real store above where it
// started.
func TestDiscoverStopsAtFilesystemRoot(t *testing.T) {
	dir := t.TempDir()
	done := make(chan struct{})
	var got string
	var ok bool
	go func() {
		defer close(done)
		got, ok = Discover(dir)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Discover did not terminate: the walk is looping at the filesystem root")
	}
	if !ok {
		return // the ordinary case: nothing above the temp dir
	}
	// A hit is only legitimate if it is a store this machine really has above dir.
	if filepath.Base(got) != FileName {
		t.Errorf("Discover returned %q, which is not a %s", got, FileName)
	}
	if fi, err := os.Stat(got); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("Discover returned %q, which is not a readable file: %v", got, err)
	}
	if rel, err := filepath.Rel(filepath.Dir(got), dir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("Discover returned %q, which is not above %q", got, dir)
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

// A store from New() has read nothing, so it has no earlier state to compare a file
// against and no business rewriting one whole. The whole rule in one place: refuse an
// existing file, adopt one that is not there yet, and report a path that cannot be
// written rather than reporting success.
func TestSaveFromANeverLoadedStoreRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()

	t.Run("an existing file is refused and left alone", func(t *testing.T) {
		existing := filepath.Join(dir, FileName)
		original := "# someone else's store\n"
		if err := os.WriteFile(existing, []byte(original), 0o644); err != nil {
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
		if string(b) != original {
			t.Errorf("the file was rewritten: %q", b)
		}
	})

	// Saving to a path that is not there yet is the ordinary first save, and the store
	// adopts it: this is how a user-wide store comes into existence.
	t.Run("a path that does not exist is adopted", func(t *testing.T) {
		fresh := filepath.Join(dir, "new.toml")
		s := New()
		s.Assign("crc32:1:1", "hero")
		if err := s.Save(fresh); err != nil {
			t.Fatalf("first save to a path that does not exist: %v", err)
		}
		if err := s.Save(fresh); err != nil {
			t.Fatalf("second save to the file it just wrote: %v", err)
		}
	})

	// A directory at the destination never reaches the write at all: os.Stat succeeds
	// on it, so it is an existing path like any other and the export rule turns it
	// down. Asserted as ErrStale rather than as "some error", because a bare non-nil
	// check passes whether this lands here or in safewrite, and the two are different
	// claims — the failed-write path is TestFailedSaveLeavesNoTempFile's, which
	// asserts its error is specifically *not* ErrStale.
	t.Run("a directory in the way is refused like any other existing path", func(t *testing.T) {
		blocked := filepath.Join(dir, "sub", FileName)
		if err := os.MkdirAll(blocked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := New().Save(blocked); !errors.Is(err, ErrStale) {
			t.Errorf("Save onto a directory = %v, want ErrStale", err)
		}
	})
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

	// Both ways Load can refuse a file, since Reload is Load applied in place and each
	// one has to leave the store whole.
	for _, bad := range []struct{ name, body string }{
		{"a key this version does not know", "unknown_key = true\n"},
		{"a file that is not TOML at all", "this is not toml ["},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(bad.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := s.Reload(p); err == nil {
				t.Fatal("Reload accepted a file Load refuses")
			}
			if _, ok := colorOf(s, "hero"); !ok {
				t.Error("the palette lost a tag to a reload that failed")
			}
			if got := s.TagsFor("crc32:aa:1"); len(got) != 1 || got[0] != "hero" {
				t.Errorf("TagsFor = %v, want the assignment untouched by a reload that failed", got)
			}
			if got := s.Related("crc32:aa:1"); len(got) != 1 || got[0] != "crc32:bb:2" {
				t.Errorf("Related = %v, want the link untouched by a reload that failed", got)
			}
		})
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
	got, ok := colorOf(s, "hero")
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

// A row the next save would not put back is refused rather than skipped, because a save
// rewrites the file whole: accepted, a hand-typed row disappears on the user's first tag
// click with nothing said. An unknown key, an unreadable color and a duplicate id were
// already loud; these two were silently dropped, so no rule covered a new case.
//
// A degenerate group stays the documented exception (see above): a group of fewer than
// two is not a partial row, it is a group that means nothing.
func TestLoadRefusesARowTheNextSaveWouldDrop(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"a tag with no id", "[[tag]]\n  color = \"#e11d48\"\n", "no id"},
		{"an assignment with no fingerprint", "[[assignment]]\n  fingerprint = \"\"\n  tags = [\"hero\"]\n", "empty fingerprint"},
		{"an empty tag inside an assignment", "[[assignment]]\n  fingerprint = \"crc32:1:2\"\n  tags = [\"\"]\n", "empty fingerprint, no tags, or an empty tag"},
		// A fingerprint with no tags applies nothing, so the next save writes no row for
		// it and the line the user typed is gone. Both spellings, because the omitted key
		// and the empty list decode to the same thing and only one of them looks wrong.
		{"an assignment with no tags key", "[[assignment]]\n  fingerprint = \"crc32:1:2\"\n", "no tags"},
		{"an assignment with an empty tag list", "[[assignment]]\n  fingerprint = \"crc32:1:2\"\n  tags = []\n", "no tags"},
		// Link drops an empty member, which is right for the endpoint and silent here:
		// with two real members beside it the row survives minus a line the user wrote,
		// and with one it is the whole group that goes.
		{"an empty member in a group that survives", "[[group]]\n  fingerprints = [\"crc32:1:2\", \"crc32:3:4\", \"\"]\n", "empty fingerprint"},
		{"an empty member in a group that does not", "[[group]]\n  fingerprints = [\"\", \"crc32:3:4\"]\n", "empty fingerprint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatal("Load accepted a row the next save would erase without a word")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say what is wrong (want it to mention %q)", err, tc.want)
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
	// And a well-formed store still loads, so the refusal is not catching real ones.
	p := filepath.Join(t.TempDir(), FileName)
	body := "[[tag]]\n  id = \"hero\"\n  color = \"#e11d48\"\n\n[[assignment]]\n  fingerprint = \"crc32:1:2\"\n  tags = [\"hero\"]\n\n[[group]]\n  fingerprints = [\"crc32:1:2\", \"crc32:3:4\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatalf("a well-formed store was refused: %v", err)
	}
	if got := s.TagsFor("crc32:1:2"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("TagsFor = %v, want [hero]", got)
	}
	if got := s.Related("crc32:1:2"); len(got) != 1 || got[0] != "crc32:3:4" {
		t.Errorf("Related = %v, want [crc32:3:4]", got)
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
// The zero stamp means two things and the difference matters. Load records it for a
// file that was not there; stampOf returns it for a file that is not there any more —
// a branch switched to one that does not carry the store, an rm, a sync tool unlinking
// it for a moment. Both have to answer ErrStale, because a save of either rewrites a
// file this store never read, and "a file that does not exist destroys nothing" is the
// reading that would let a stale palette land on top of what the next checkout
// restores. Pinned in both directions: the refusal, and what a recovery Reload then
// leaves behind.
func TestSaveRefusesAStoreFileThatHasSinceBeenDeleted(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	seed := New()
	seed.Define("hero", "#112233")
	seed.Assign("fp-a", "hero")
	if err := seed.Save(p); err != nil {
		t.Fatal(err)
	}
	mine, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	mine.Assign("fp-b", "hero")

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := mine.Save(p); !errors.Is(err, ErrStale) {
		t.Fatalf("Save onto a deleted store = %v, want ErrStale", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("the refused save wrote the file anyway")
	}
	// And the recovery the browse server makes on that refusal: reloading re-homes the
	// store onto the same path, finds nothing there, and leaves the edit nowhere.
	if err := mine.Reload(p); err != nil {
		t.Fatalf("Reload after the refusal: %v", err)
	}
	if got := mine.TagsFor("fp-b"); len(got) != 0 {
		t.Errorf("TagsFor(fp-b) = %v after the reload, want the unsaved edit gone", got)
	}
	if len(mine.Tags()) != 0 {
		t.Errorf("palette = %v after reloading from a file that is gone, want empty", mine.Tags())
	}
	// Re-homed, not export: the next save adopts the path it just reloaded from.
	if err := mine.Save(p); err != nil {
		t.Errorf("Save after the reload = %v, want the store to have re-homed onto it", err)
	}
}

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
	if _, ok := colorOf(after, "villain"); !ok {
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
	if _, ok := colorOf(mine, "villain"); !ok {
		t.Error("Reload did not pick up the other writer's tag")
	}
	if err := mine.Save(p); err != nil {
		t.Errorf("Save after Reload: %v, want it to succeed now that the store matches disk", err)
	}
}

// Loading a path that is not there yet records a zero stamp deliberately: something
// creating the file before the first save is the same lost edit as an outside write.
//
// That pairing — a store that *did* load, holding a zero stamp — is the ordinary shape
// of the user-wide store on a fresh machine, since browse.Serve always calls Load. It
// is also the one shape neither neighbouring test reaches: the never-loaded case takes
// the export branch instead, and the clobber case above loads a file that exists.
func TestSaveRefusesAFileCreatedSinceAMissingLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	mine, err := Load(p) // nothing there yet
	if err != nil {
		t.Fatal(err)
	}
	mine.Assign("fp-a", "hero")

	// A second quarry, a checkout of a project store, an editor.
	theirs := New()
	theirs.Define("villain", "#445566")
	if err := theirs.Save(p); err != nil {
		t.Fatal(err)
	}

	if err := mine.Save(p); !errors.Is(err, ErrStale) {
		t.Fatalf("Save = %v, want ErrStale: the file appeared after an empty load", err)
	}
	after, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := colorOf(after, "villain"); !ok {
		t.Error("the other writer's store was overwritten by one that had read nothing")
	}
	// And the recovery still works from here, so the refusal is not a dead end.
	if err := mine.Reload(p); err != nil {
		t.Fatal(err)
	}
	if err := mine.Save(p); err != nil {
		t.Errorf("Save after Reload: %v", err)
	}
}

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
				if _, ok := colorOf(s, tc.neu); ok {
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
	// Checked, unlike every other fixture write here it used not to be: a write that
	// failed leaves Load returning an empty store and this test reporting "a duplicate
	// tag id was accepted", which points at the wrong thing entirely.
	if err := os.WriteFile(p, []byte(`
[[tag]]
  id = "hero"
  color = "#e11d48"
[[tag]]
  id = "hero"
  color = "#0ea5e9"
`), 0o644); err != nil {
		t.Fatal(err)
	}

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
	// The values, not just the count: this is the test whose job is pinning the
	// escaping, and a label that round-trips to a *different* string satisfies a length
	// check exactly as well as one that survives. TagsFor is sorted, so the wanted set
	// is too.
	want := append([]string(nil), labels...)
	sort.Strings(want)
	for _, fp := range fps {
		if got := back.TagsFor(fp); !reflect.DeepEqual(got, want) {
			t.Errorf("TagsFor(%q) = %#v, want %#v", fp, got, want)
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

// Only the file this store read may be rewritten. A save onto a file this store has
// never seen cannot tell what it is destroying — the same total loss ErrStale exists to
// prevent, minus even the chance to notice — so an export is allowed onto a path that
// does not exist and refused onto one that does.
func TestSaveRefusesAFileThisStoreNeverRead(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, FileName)
	if err := os.WriteFile(real, []byte("[[tag]]\n  id = \"hero\"\n  color = \"#e11d48\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(real)
	if err != nil {
		t.Fatal(err)
	}

	// Exporting to a fresh path is not a rewrite, and stays allowed.
	backup := filepath.Join(dir, "backup.toml")
	if err := s.Save(backup); err != nil {
		t.Fatalf("export to a path that does not exist: %v", err)
	}

	// Somebody else's file, which this store has never read.
	other := filepath.Join(dir, "someone-elses.toml")
	if err := os.WriteFile(other, []byte("[[tag]]\n  id = \"villain\"\n  color = \"#00ff00\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(other); !errors.Is(err, ErrStale) {
		t.Errorf("Save onto an unread existing file = %v, want ErrStale", err)
	}
	b, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "villain") {
		t.Errorf("the refused save still overwrote the file: %q", b)
	}

	// Re-exporting over the backup this store just wrote is the same rule: it is not
	// the file this store read, so it is not the file this store may rewrite.
	if err := s.Save(backup); !errors.Is(err, ErrStale) {
		t.Errorf("second export over the same backup = %v, want ErrStale", err)
	}

	// And the home never moved, in both directions. It still accepts the file this store
	// read, and still guards it: if an export had moved the home, the real store would be
	// unguarded from then on and the next tag click would overwrite whatever an editor or
	// a checkout had put there.
	if err := s.Save(real); err != nil {
		t.Errorf("saving the file this store loaded = %v, want success", err)
	}
	if err := os.WriteFile(real, []byte("[[tag]]\n  id = \"villain\"\n  color = \"#00ff00\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(real); !errors.Is(err, ErrStale) {
		t.Fatalf("Save after an outside edit = %v, want ErrStale; the export moved the guard", err)
	}
	if b, err := os.ReadFile(real); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(b), "villain") {
		t.Errorf("the outside edit was overwritten: %s", b)
	}
}
func TestOverlappingGroupRowsMergeOnLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, []byte(`
[[group]]
  fingerprints = ["A", "B"]
[[group]]
  fingerprints = ["B", "C"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatalf("overlapping groups must merge, not error: %v", err)
	}
	groups := s.Groups()
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want the single merged {A,B,C}", groups)
	}
	if !reflect.DeepEqual(groups[0], []string{"A", "B", "C"}) {
		t.Errorf("group = %v, want [A B C]", groups[0])
	}
	if !reflect.DeepEqual(s.Related("A"), []string{"B", "C"}) {
		t.Errorf("Related(A) = %v, want [B C]", s.Related("A"))
	}
}

// FingerprintsByTag is what the palette folds onto cards, so it has to name every
// assignment exactly once and see tags with none.
func TestFingerprintsByTag(t *testing.T) {
	s := New()
	s.Define("hero", "#112233")
	s.Define("unused", "#445566")
	s.Assign("fp1", "hero")
	s.Assign("fp2", "hero")
	s.Assign("fp2", "villain")
	got := s.FingerprintsByTag()
	sort.Strings(got["hero"])
	if !reflect.DeepEqual(got["hero"], []string{"fp1", "fp2"}) {
		t.Errorf("hero = %v, want [fp1 fp2]", got["hero"])
	}
	if !reflect.DeepEqual(got["villain"], []string{"fp2"}) {
		t.Errorf("villain = %v, want [fp2]", got["villain"])
	}
	if len(got["unused"]) != 0 {
		t.Errorf("unused = %v, want nothing", got["unused"])
	}
}

// Creating a tag and applying it later is the ordinary UI flow — POST /api/tags with
// no assign — and a palette entry that carries nothing has no [[assignment]] row to be
// recovered from. Every other round-trip test here assigns the tag it defines, so a
// save that skipped unused entries, or a load that dropped them, would leave the whole
// suite green while the user's new tag vanished on the next restart.
func TestAnUnassignedPaletteEntrySurvivesTheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	if err := s.Define("unused", "#abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := s.Define("used", "#112233"); err != nil {
		t.Fatal(err)
	}
	s.Assign("crc32:abc:10", "used")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := colorOf(got, "unused")
	if !ok {
		t.Fatal("the unassigned tag is gone after a reload; the user's new tag disappears on restart")
	}
	if c != "#abcdef" {
		t.Errorf("unassigned tag colour = %q, want #abcdef", c)
	}
}

// Discover answers with a path Load is then asked to read. A directory of that name at
// or above the working directory is not a store, and answering with it made Load fail
// with "is a directory" — so quarry refused to start instead of walking past to a real
// store, or to the user-wide one that is the documented fallback.
func TestDiscoverWalksPastADirectoryOfThatName(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, FileName)
	if err := os.WriteFile(real, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "project", "sub")
	if err := os.MkdirAll(filepath.Join(deep, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := Discover(deep)
	if !ok {
		t.Fatal("Discover found nothing; the directory in the way stopped the walk")
	}
	if got != real {
		t.Errorf("Discover = %q, want the regular file at %q", got, real)
	}
	if _, err := Load(got); err != nil {
		t.Errorf("what Discover returned does not load: %v", err)
	}
}

// The header is the only warning a user gets before their first tag click erases the
// comments they wrote in a store meant to be hand-edited and committed. Nothing pinned
// it: replacing storeHeader with "" left this package and browse green, because every
// round-trip test compares parsed state or two saves of the same store, and both sides
// lose the line together.
func TestASavedStoreWarnsThatItIsRewrittenWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Assign("crc32:1:2", "hero")
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(first, "#") {
		t.Fatalf("a saved store opens with %q, not a comment; a hand-editor gets no warning at all", first)
	}
	// The substance, not the wording: a reader has to learn that what they type here
	// does not survive an edit made in the UI.
	for _, want := range []string{"comment", "whole"} {
		if !strings.Contains(strings.ToLower(first), want) {
			t.Errorf("the header %q does not mention %q; it has to say what is lost, not just that it is a header", first, want)
		}
	}
	// And it is a comment, so it must survive the round trip it is warning about.
	back, err := Load(p)
	if err != nil {
		t.Fatalf("a store carrying its own header did not load: %v", err)
	}
	if got := back.TagsFor("crc32:1:2"); len(got) != 1 || got[0] != "hero" {
		t.Errorf("TagsFor after a round trip = %v, want [hero]", got)
	}
}

// A [[tag]] row with no color is accepted and given a generated one. This is not an
// incidental branch: the error for an unreadable color tells the user to "use #rrggbb,
// or remove the color to get a generated one", so it is behavior the store advertises
// to someone hand-editing the file it just refused. Ordered under the successful
// NormalizeColor case and above the refusal, it is one reorder away from turning the
// file quarry just told them to write into a hard Load error that stops the server
// starting, with a message contradicting the one that sent them there.
func TestLoadGeneratesAColorForARowThatOmitsOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("[[tag]]\n  id = \"hero\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load on a row with no color: %v — the error for a bad color tells the user to write exactly this", err)
	}
	got, ok := colorOf(s, "hero")
	if !ok || got != DefaultColor("hero") {
		t.Errorf("color(hero) = %q (defined %v), want the generated %q", got, ok, DefaultColor("hero"))
	}
	// And the next save puts the generated color back, so the file round-trips rather
	// than being refused on the load after it.
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), DefaultColor("hero")) {
		t.Errorf("the saved file does not carry the generated color:\n%s", b)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the file this store wrote does not load back: %v", err)
	}
}

// Reload re-homes the store: path becomes the file Save guards from then on. Save's
// mirror rule — a home is adopted once and never moves — is pinned exhaustively; this
// half was pinned by nothing, so a second Reload call site reading from a different
// file (a "revert to the project store", a backup) would move the guard while the
// server kept saving to s.tagsPath. Every save after that takes the export branch,
// finds the real file present, and answers ErrStale: the UI 409s on every tag click
// until restart, and recovery reloads from the wrong file each time.
func TestReloadRehomesTheStoreOntoThePathItRead(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	b := filepath.Join(dir, "b.toml")
	for _, p := range []string{a, b} {
		s := New()
		s.Assign("crc32:aa:1", "hero")
		if err := s.Save(p); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Load(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(b); err != nil {
		t.Fatal(err)
	}
	// b is now the home, so saving to it re-checks the stamp and succeeds.
	if err := s.Save(b); err != nil {
		t.Errorf("Save onto the path Reload read = %v, want it to be the home now", err)
	}
	// a is no longer guarded by this store, so rewriting it whole is an export onto an
	// existing file — refused, rather than silently destroying what a never read.
	if err := s.Save(a); !errors.Is(err, ErrStale) {
		t.Errorf("Save onto the path this store was loaded from = %v, want ErrStale after a Reload moved the home", err)
	}
}
