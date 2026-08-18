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
