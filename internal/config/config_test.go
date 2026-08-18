package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearQuarryEnv isolates a test from the maintainer's own environment. Load and the
// resolvers read these, so anyone who actually uses the tool would otherwise see
// these tests fail on their own machine.
func clearQuarryEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"QUARRY_ROOT", "QUARRY_CONFIG_DIR", "QUARRY_CACHE_DIR"} {
		t.Setenv(k, "")
	}
}

func TestRootUnsetWithoutConfig(t *testing.T) {
	clearQuarryEnv(t)
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// There is no sensible default scan root, so an unset root must stay empty for
	// the caller to reject; a guessed one would index whatever it landed in.
	if c.Root != "" {
		t.Errorf("default root = %q, want empty", c.Root)
	}
}

func TestRootPrecedence(t *testing.T) {
	clearQuarryEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`root = "/from/file"`), 0o644)

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/from/file" {
		t.Errorf("root from file = %q", c.Root)
	}

	t.Setenv("QUARRY_ROOT", "/from/env")
	c, _ = Load(dir)
	if c.Root != "/from/env" {
		t.Errorf("QUARRY_ROOT should override file, got %q", c.Root)
	}
}

func TestRootExpandsHome(t *testing.T) {
	clearQuarryEnv(t)
	t.Setenv("QUARRY_ROOT", "~/code/raw-assets")
	c, _ := Load(t.TempDir())
	home, _ := os.UserHomeDir()
	if c.Root != filepath.Join(home, "code", "raw-assets") {
		t.Errorf("root ~ not expanded: %q", c.Root)
	}
}

func TestLoadRejectsMalformedConfig(t *testing.T) {
	clearQuarryEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = "), 0o644)
	if _, err := Load(dir); err == nil {
		t.Error("malformed config.toml should be an error, not silently ignored")
	}
}

// The two resolvers share one precedence rule, so they share one table. An env var
// set to "" must not count as set (the XDG spec says so, and a wrapper script that
// exports an unset variable would otherwise resolve the tool somewhere unintended).
func TestResolveDirs(t *testing.T) {
	for _, r := range []struct {
		name           string
		resolve        func(string) (string, error)
		ownEnv, xdgEnv string
		homeSub        string
	}{
		{"config", ResolveDir, "QUARRY_CONFIG_DIR", "XDG_CONFIG_HOME", ".config"},
		{"cache", ResolveCacheDir, "QUARRY_CACHE_DIR", "XDG_CACHE_HOME", ".cache"},
	} {
		t.Run(r.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				flag  string
				env   map[string]string
				want  string
				isDir bool // want is a suffix of the home fallback rather than exact
			}{
				{name: "flag wins", flag: "/explicit", env: map[string]string{r.ownEnv: "/env", r.xdgEnv: "/xdg"}, want: "/explicit"},
				{name: "own env beats xdg", env: map[string]string{r.ownEnv: "/env", r.xdgEnv: "/xdg"}, want: "/env"},
				{name: "xdg", env: map[string]string{r.xdgEnv: "/xdg"}, want: filepath.Join("/xdg", "quarry")},
				{name: "empty env is not set", env: map[string]string{r.ownEnv: "", r.xdgEnv: ""}, want: filepath.Join(r.homeSub, "quarry"), isDir: true},
				{name: "home fallback", want: filepath.Join(r.homeSub, "quarry"), isDir: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					clearQuarryEnv(t)
					t.Setenv(r.xdgEnv, "")
					for k, v := range tc.env {
						t.Setenv(k, v)
					}
					got, err := r.resolve(tc.flag)
					if err != nil {
						t.Fatalf("resolve: %v", err)
					}
					if tc.isDir {
						if !strings.HasSuffix(got, tc.want) {
							t.Errorf("got %q, want a path ending in %q", got, tc.want)
						}
						return
					}
					if got != tc.want {
						t.Errorf("got %q, want %q", got, tc.want)
					}
				})
			}

			// With no home and no env there is nowhere to resolve to. Returning a
			// cwd-relative name instead would put the cache — and every unpacked archive
			// — inside whatever directory quarry happened to be run from, which is most
			// naturally the library it promises not to write to.
			t.Run("no home is an error, not a relative path", func(t *testing.T) {
				clearQuarryEnv(t)
				t.Setenv(r.xdgEnv, "")
				t.Setenv("HOME", "")
				got, err := r.resolve("")
				if err == nil {
					t.Fatalf("resolved to %q with no home; want an error", got)
				}
				if got != "" {
					t.Errorf("got %q alongside the error; want no path at all", got)
				}
			})
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for _, tc := range []struct{ in, want string }{
		{"~", home},
		{"~/lib/assets", filepath.Join(home, "lib", "assets")},
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		// Only a leading "~/" (or a bare "~") is ours to expand: "~user" is another
		// account's home, which this does not resolve, and a mid-path "~" is a literal
		// directory name someone chose.
		{"~otheruser/lib", "~otheruser/lib"},
		{"/a/~/b", "/a/~/b"},
		{"./~", "./~"},
	} {
		if got := ExpandHome(tc.in); got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A key quarry does not know is a setting the user believes is in effect. Silently
// ignoring `follow_symlink` (singular) means a whole drive missing from the index
// with nothing said about it.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	clearQuarryEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = \"/x\"\nfollow_symlink = true\n"), 0o644)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("an unrecognized key was accepted silently")
	}
	if !strings.Contains(err.Error(), "follow_symlink") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestFollowSymlinksFromFile(t *testing.T) {
	clearQuarryEnv(t)
	dir := t.TempDir()

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.FollowSymlinks {
		t.Error("following must be off unless asked for: it decides whether the scan leaves the root")
	}

	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = \"/x\"\nfollow_symlinks = true\n"), 0o644)
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.FollowSymlinks {
		t.Error("follow_symlinks = true in config.toml was not read")
	}
}
