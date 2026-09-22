package safewrite

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dirEntries lists a directory's entries by name, so a test can assert that nothing
// was left behind beside the file it was writing.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Name()
	}
	return names
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The whole point of Atomic is that a reader never sees a partial file and a failure
// never costs the previous one. Both halves are asserted here because the failing half
// is silent: a truncated tag store still parses, and the loss reads as "my tags are
// gone" rather than as an error.
func TestAtomicLeavesEitherTheOldFileOrTheNewOne(t *testing.T) {
	boom := errors.New("encode blew up")
	for _, tc := range []struct {
		name    string
		before  string // "" means the file does not exist yet
		encode  func(io.Writer) error
		wantErr error
		want    string // expected contents afterwards; "" means still absent
	}{
		{
			name:   "writes a new file",
			encode: func(w io.Writer) error { _, err := io.WriteString(w, "fresh"); return err },
			want:   "fresh",
		},
		{
			name:   "replaces an existing file",
			before: "old contents",
			encode: func(w io.Writer) error { _, err := io.WriteString(w, "new contents"); return err },
			want:   "new contents",
		},
		{
			name:    "an encode failure leaves the previous contents",
			before:  "old contents",
			encode:  func(w io.Writer) error { io.WriteString(w, "half a fi"); return boom },
			wantErr: boom,
			want:    "old contents",
		},
		{
			name:    "an encode failure on a new file leaves no file",
			encode:  func(w io.Writer) error { io.WriteString(w, "half a fi"); return boom },
			wantErr: boom,
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "store.toml")
			if tc.before != "" {
				if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := Atomic(path, ".tmp-*", tc.encode)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Atomic: %v", err)
			}

			if tc.want == "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("a file exists at %s after a failed write of a new file", path)
				}
			} else if got := read(t, path); got != tc.want {
				t.Errorf("contents = %q, want %q", got, tc.want)
			}

			// No temp file on any path: one failed write per extracted archive would
			// otherwise litter the cache dir on a library-sized run.
			for _, name := range dirEntries(t, dir) {
				if strings.HasPrefix(name, ".tmp-") {
					t.Errorf("temp file %q left behind", name)
				}
			}
		})
	}
}

// A store the user (or a checkout) left group-readable must not silently become
// owner-only on the first tag click, which is what inheriting os.CreateTemp's 0600
// through the rename did.
func TestAtomicKeepsTheTargetsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.toml")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Atomic(path, ".tmp-*", func(w io.Writer) error { _, err := io.WriteString(w, "new"); return err }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o after a rewrite, want the target's own 0640", got)
	}

	// A file that did not exist gets the usual default rather than CreateTemp's 0600.
	fresh := filepath.Join(dir, "new.toml")
	if err := Atomic(fresh, ".tmp-*", func(w io.Writer) error { _, err := io.WriteString(w, "x"); return err }); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %04o for a newly created file, want 0644", got)
	}
}

func TestAtomicReportsAnUnwritableDestination(t *testing.T) {
	// A directory where the file should be: the rename cannot land.
	blocked := filepath.Join(t.TempDir(), "store.toml")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Atomic(blocked, ".tmp-*", func(w io.Writer) error { _, err := io.WriteString(w, "x"); return err })
	if err == nil {
		t.Fatal("Atomic reported success writing over a directory")
	}
	for _, name := range dirEntries(t, filepath.Dir(blocked)) {
		if strings.HasPrefix(name, ".tmp-") {
			t.Errorf("temp file %q left behind after a failed rename", name)
		}
	}
}

// errReader fails partway through, standing in for a download that drops.
type errReader struct{ n int }

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection reset")
	}
	n := copy(p, strings.Repeat("x", min(len(p), r.n)))
	r.n -= n
	return n, nil
}

// Stream writes in place, so an interrupted copy must not leave a partial file that
// later reads treat as complete — selfupdate stages a downloaded binary through it.
func TestStreamLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "download.bin")

	if err := Stream(dst, strings.NewReader("complete payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != "complete payload" {
		t.Errorf("contents = %q", got)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %04o, want the 0755 asked for", got)
	}

	if err := Stream(dst, &errReader{n: 4096}, 0o755); err == nil {
		t.Fatal("Stream reported success on a read that failed partway")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a partial %s survives a failed copy; a later read cannot tell it from a whole one", dst)
	}
}

// A destination reached through a symlink is written through it. os.Rename replaces
// the path it is given rather than what that path points at, so renaming onto a link
// silently swaps the link for a regular file and strands the real file with its old
// contents — a store linked into a synced folder or shared between checkouts stops
// receiving edits with nothing reported.
func TestAtomicWritesThroughASymlink(t *testing.T) {
	write := func(path, body string) error {
		return Atomic(path, ".t-*", func(w io.Writer) error {
			_, err := io.WriteString(w, body)
			return err
		})
	}

	t.Run("link to an existing file", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.toml")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.toml")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := write(link, "edited"); err != nil {
			t.Fatal(err)
		}
		if got := read(t, real); got != "edited" {
			t.Errorf("the real file says %q, want %q — the write did not reach it", got, "edited")
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the symlink was replaced by a regular file (err=%v)", err)
		}
	})

	t.Run("link whose target does not exist yet", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "notyet.toml")
		link := filepath.Join(dir, "link.toml")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := write(link, "first save"); err != nil {
			t.Fatal(err)
		}
		if got := read(t, real); got != "first save" {
			t.Errorf("the real file says %q; a store linked into place before its first save must still be created there", got)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the dangling symlink was replaced by a regular file (err=%v)", err)
		}
	})

	t.Run("a symlinked parent directory", func(t *testing.T) {
		dir := t.TempDir()
		realDir := filepath.Join(dir, "real")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(dir, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := write(filepath.Join(linkDir, "store.toml"), "through the dir"); err != nil {
			t.Fatal(err)
		}
		if got := read(t, filepath.Join(realDir, "store.toml")); got != "through the dir" {
			t.Errorf("the real directory holds %q, want %q", got, "through the dir")
		}
	})

	t.Run("a chain of links", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.toml")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		mid, outer := filepath.Join(dir, "mid.toml"), filepath.Join(dir, "outer.toml")
		if err := os.Symlink(real, mid); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink(mid, outer); err != nil {
			t.Fatal(err)
		}
		if err := write(outer, "edited"); err != nil {
			t.Fatal(err)
		}
		if got := read(t, real); got != "edited" {
			t.Errorf("the file at the end of the chain says %q, want %q", got, "edited")
		}
	})

	t.Run("a cycle is not followed forever", func(t *testing.T) {
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
		if err := os.Symlink(b, a); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink(a, b); err != nil {
			t.Fatal(err)
		}
		// The write may fail; what it must not do is hang or recurse without end.
		_ = write(a, "whatever")
	})
}

// A killed write leaves its temp file behind, and the next write clears it. The temp
// is created in the directory the destination resolves to, so a sweep run against the
// path as given never finds it when that path is a symlink pointing elsewhere — the
// leftover then sits in a real project directory forever, under a name .gitignore
// does not cover.
func TestAtomicSweepsAnAbandonedTempInTheResolvedDirectory(t *testing.T) {
	write := func(path string) error {
		return Atomic(path, ".t-*", func(w io.Writer) error {
			_, err := io.WriteString(w, "body")
			return err
		})
	}
	aged := func(t *testing.T, path string, age time.Duration) string {
		t.Helper()
		if err := os.WriteFile(path, []byte("abandoned"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "store.toml")
	if err := os.Symlink(filepath.Join(realDir, "store.toml"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stale := aged(t, filepath.Join(realDir, ".t-stale"), 2*StaleTempAge)
	// Young enough to belong to another process writing this file right now.
	fresh := aged(t, filepath.Join(realDir, ".t-fresh"), time.Minute)
	unrelated := aged(t, filepath.Join(realDir, "notes.txt"), 2*StaleTempAge)

	if err := write(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("an abandoned temp survived beside the resolved file (err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a temp another writer may still be using was removed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("a file that does not match the temp pattern was removed: %v", err)
	}
}

// Stream applies perm only when it creates dst, because O_CREATE does. selfupdate's
// cross-device fallback depends on knowing that: replaceBinary removes the staging
// path and chmods it afterwards precisely because streaming over an existing 0600 file
// would land a binary nobody can execute — including the update that would replace it.
// Nothing here pinned it, so a "perm is honoured anyway" simplification of that caller
// left the whole suite green and shipped a self-update that can brick an install.
func TestStreamLeavesAnExistingFilesModeAlone(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "download.bin")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stream(dst, strings.NewReader("new payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst); got != "new payload" {
		t.Errorf("contents = %q, want the streamed bytes", got)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600 kept: a caller that needs 0755 has to chmod, and one that assumes Stream did is shipping an unrunnable binary", got)
	}
}

// A tmpPattern with no "*" is a write that works and a sweep that silently matches
// nothing, so every interrupted write leaves a file behind forever — in a user's
// project directory, often under source control. Refused rather than documented.
func TestAtomicRefusesATempPatternThatCouldNeverBeSwept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.toml")
	err := Atomic(path, ".quarry-tags", func(w io.Writer) error {
		_, e := io.WriteString(w, "x")
		return e
	})
	if err == nil {
		t.Fatal("Atomic accepted a pattern with no \"*\"; nothing it abandons would ever be swept")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the refused write still created the destination")
	}
	// The other end of the same range, which was open. The sweep splits on the last
	// "*", so a pattern beginning with one leaves an empty prefix — and an empty
	// prefix matches every name in the destination directory, which for the tag store
	// is a user's project. What is supposed to clear this writer's own leavings would
	// delete everything beside them older than StaleTempAge.
	neighbour := filepath.Join(filepath.Dir(path), "notes.md")
	if err := os.WriteFile(neighbour, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * StaleTempAge)
	if err := os.Chtimes(neighbour, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Atomic(path, "*.tmp", func(w io.Writer) error {
		_, e := io.WriteString(w, "x")
		return e
	}); err == nil {
		t.Error("Atomic accepted a pattern with no prefix; its sweep matches every file beside the destination")
	}
	if _, statErr := os.Stat(neighbour); statErr != nil {
		t.Errorf("a file beside the destination was swept away: %v", statErr)
	}

	// And the ordinary pattern still works, so the guard is not refusing real callers.
	if err := Atomic(path, ".quarry-tags-*", func(w io.Writer) error {
		_, e := io.WriteString(w, "x")
		return e
	}); err != nil {
		t.Fatalf("a valid pattern was refused: %v", err)
	}
}

// The destination directory is data, not a pattern. Pasted into a filepath.Glob the
// way the sweep used to, a directory name holding a glob metacharacter — "[archive]",
// "Season [2]" — became a character class matching no real path, and an unterminated
// "[" returned ErrBadPattern, which the sweep swallows. Either way an interrupted
// write's leftovers accumulated in a source-controlled project directory forever, with
// no error anywhere: exactly the outcome the no-"*" refusal above exists to prevent,
// reached by the other half of the same mechanism.
func TestTheSweepReadsTheDestinationDirectoryRatherThanGlobbingIt(t *testing.T) {
	for _, dirName := range []string{"plain", "[archive]", "Season [2]", "half[open", "star*dir", "q?mark"} {
		t.Run(dirName, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), dirName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// What an update killed between CreateTemp and the rename leaves behind.
			stale := filepath.Join(dir, ".quarry-tags-1234567")
			if err := os.WriteFile(stale, []byte("abandoned"), 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * StaleTempAge)
			if err := os.Chtimes(stale, old, old); err != nil {
				t.Fatal(err)
			}
			// And one young enough to belong to another process writing right now.
			fresh := filepath.Join(dir, ".quarry-tags-7654321")
			if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := Atomic(filepath.Join(dir, "quarry.tags.toml"), ".quarry-tags-*", func(w io.Writer) error {
				_, e := io.WriteString(w, "tags = []\n")
				return e
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Errorf("the abandoned temp survived a write in %q; it will never be swept", dirName)
			}
			if _, err := os.Stat(fresh); err != nil {
				t.Errorf("a temp younger than StaleTempAge was removed in %q; it may belong to a live writer", dirName)
			}
		})
	}
}

// The durability half of Atomic, which is the whole of what separates it from Stream and
// the reason the tag store writes through it. Every other test here asserts post-rename
// state, and all of them stay green with both fsyncs deleted: a crash right after a save
// then lands the rename in the journal with the data blocks unwritten, quarry.tags.toml
// comes back zero-length, and a zero-length store parses cleanly as one holding nothing.
// The loss reads as "my tags are gone" with nothing reporting it.
//
// Structural because the behaviour needs a crashing machine to observe. A test with a
// filesystem seam would change Atomic's signature for one caller that is a test.
func TestAtomicFsyncsTheBytesBeforeTheRenameAndTheDirectoryAfter(t *testing.T) {
	src, err := os.ReadFile("safewrite.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "\nfunc Atomic(")
	if start < 0 {
		t.Fatal("no func Atomic in safewrite.go; this guard has stopped reading it")
	}
	body := string(src[start:])
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of func Atomic; this guard has stopped reading it")
	}
	body = body[:end]

	fileSync := strings.Index(body, "tmp.Sync()")
	rename := strings.Index(body, "os.Rename(")
	dirSync := strings.Index(body, "d.Sync()")
	switch {
	case fileSync < 0:
		t.Error("Atomic no longer fsyncs the temp file; a crash after the rename can bring the target back empty")
	case rename < 0:
		t.Fatal("no os.Rename in Atomic; this guard has stopped reading it")
	case fileSync > rename:
		t.Error("Atomic fsyncs the temp file after renaming it into place, which is not a barrier at all")
	}
	if dirSync < 0 || dirSync < rename {
		t.Error("Atomic no longer fsyncs the destination directory after the rename; " +
			"the bytes are then durable but the directory entry pointing at them is not, so a crash can restore the previous contents")
	}
}

// os.CreateTemp replaces the *last* "*" in a pattern, so that is the one the sweep has
// to split on to recognise what an interrupted write left behind. Split on the first,
// a pattern holding several matched nothing at all — the same silent never-sweeps
// outcome Atomic refuses a pattern with no "*" outright for, reached by the other half
// of the same mechanism, and in a directory the leftovers are a user's to notice.
func TestAtomicSweepsATempFromAPatternHoldingSeveralStars(t *testing.T) {
	const pattern = ".quarry-*-tags-*.tmp"
	dir := t.TempDir()
	// Named the way CreateTemp names one, so the sweep is asked the real question.
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := f.Name()
	f.Close()
	when := time.Now().Add(-2 * StaleTempAge)
	if err := os.Chtimes(abandoned, when, when); err != nil {
		t.Fatal(err)
	}
	// Something of the user's that merely shares the leading text.
	mine := filepath.Join(dir, ".quarry-notes")
	if err := os.WriteFile(mine, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(mine, when, when); err != nil {
		t.Fatal(err)
	}

	if err := Atomic(filepath.Join(dir, "store.toml"), pattern, func(w io.Writer) error {
		_, err := io.WriteString(w, "body")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned temp from a multi-star pattern survived (%v); the sweep is splitting on the wrong star", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the sweep removed a file that only shares the pattern's prefix: %v", err)
	}
}

// Atomic does not create the destination's directory, and the contract is worth having
// a test rather than being whatever CreateTemp happens to do: every caller MkdirAlls
// first, so nothing here establishes whose job it is, and a caller that started relying
// on Atomic to do it would find out at the first write to a fresh config dir.
func TestAtomicRefusesADestinationWithNoDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-made-yet", "store.toml")
	err := Atomic(missing, ".t-*", func(w io.Writer) error {
		_, err := io.WriteString(w, "body")
		return err
	})
	if err == nil {
		t.Fatal("Atomic reported success writing into a directory that does not exist")
	}
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want one a caller can read as the missing directory it is", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Error("something was left at the destination")
	}
	if _, statErr := os.Stat(filepath.Dir(missing)); !os.IsNotExist(statErr) {
		t.Error("Atomic created the destination's directory; the caller decides its mode")
	}
}
