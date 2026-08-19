package safewrite

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if got := readFile(t, real); got != "edited" {
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
		if got := readFile(t, real); got != "first save" {
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
		if got := readFile(t, filepath.Join(realDir, "store.toml")); got != "through the dir" {
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
		if got := readFile(t, real); got != "edited" {
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
