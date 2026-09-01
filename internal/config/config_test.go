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
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`root = "/from/file"`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/from/file" {
		t.Errorf("root from file = %q", c.Root)
	}

	t.Setenv("QUARRY_ROOT", "/from/env")
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/from/env" {
		t.Errorf("QUARRY_ROOT should override file, got %q", c.Root)
	}
}

func TestRootExpandsHome(t *testing.T) {
	clearQuarryEnv(t)
	t.Setenv("QUARRY_ROOT", "~/assets/library")
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != filepath.Join(home, "assets", "library") {
		t.Errorf("root ~ not expanded: %q", c.Root)
	}
}

func TestLoadRejectsMalformedConfig(t *testing.T) {
	clearQuarryEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = "), 0o644); err != nil {
		t.Fatal(err)
	}
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
				// The XDG spec calls a relative value invalid and says to ignore it, which
				// is also the only reading that keeps the promise below: joined instead, it
				// resolves against whatever directory quarry was run from.
				{name: "relative xdg is ignored", env: map[string]string{r.xdgEnv: ".cache"}, want: filepath.Join(r.homeSub, "quarry"), isDir: true},
				{name: "relative xdg with a segment is ignored", env: map[string]string{r.xdgEnv: "a/b"}, want: filepath.Join(r.homeSub, "quarry"), isDir: true},
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

			// Every branch returns an absolute path, which is the promise. A flag is the
			// one place a relative value is honoured rather than refused — it is this
			// invocation saying "here" — but it is resolved, not passed through.
			t.Run("a relative flag resolves against the working directory", func(t *testing.T) {
				clearQuarryEnv(t)
				t.Setenv(r.xdgEnv, "")
				got, err := r.resolve(filepath.Join(".", "local-state"))
				if err != nil {
					t.Fatal(err)
				}
				if !filepath.IsAbs(got) {
					t.Errorf("got %q, want an absolute path", got)
				}
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if got != filepath.Join(wd, "local-state") {
					t.Errorf("got %q, want it resolved against %q", got, wd)
				}
			})

			// An own env var is not one invocation's choice: it sits in a shell rc and
			// follows the user into every directory, which is the case the XDG branch
			// refuses a relative value for. QUARRY_CACHE_DIR=.quarry would give each
			// directory quarry is run from its own index and unpacked-archive tree.
			t.Run("a relative own env var is refused", func(t *testing.T) {
				clearQuarryEnv(t)
				t.Setenv(r.xdgEnv, "")
				t.Setenv(r.ownEnv, ".quarry")
				got, err := r.resolve("")
				if err == nil {
					t.Fatalf("resolved to %q; want an error naming %s", got, r.ownEnv)
				}
				if !strings.Contains(err.Error(), r.ownEnv) {
					t.Errorf("error %q does not name the variable at fault", err)
				}
			})

			// os.UserHomeDir hands back $HOME unexamined, so a relative one arrives here
			// as a relative result from the branch least able to look like it produced one.
			t.Run("a relative home is an error, not a relative path", func(t *testing.T) {
				clearQuarryEnv(t)
				t.Setenv(r.xdgEnv, "")
				t.Setenv("HOME", "relative-home")
				got, err := r.resolve("")
				if err == nil {
					t.Fatalf("resolved to %q with a relative $HOME; want an error", got)
				}
			})

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

			// Every branch, not just the last one: a "~" reaching the flag or the env
			// needs the same home the fallback does, and returning it unexpanded is the
			// cwd-relative name this whole function exists to refuse.
			t.Run("a tilde with no home is an error on every branch", func(t *testing.T) {
				for _, tc := range []struct{ name, flag, own, xdg string }{
					{name: "flag", flag: "~/state"},
					{name: "own env", own: "~/state"},
					{name: "xdg env", xdg: "~/state"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						clearQuarryEnv(t)
						t.Setenv("HOME", "")
						t.Setenv(r.ownEnv, tc.own)
						t.Setenv(r.xdgEnv, tc.xdg)
						got, err := r.resolve(tc.flag)
						if err == nil {
							t.Fatalf("resolved to %q with no home; want an error", got)
						}
						if got != "" {
							t.Errorf("got %q alongside the error; want no path at all", got)
						}
					})
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
		got, err := ExpandHome(tc.in)
		if err != nil {
			t.Errorf("ExpandHome(%q) = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An unexpandable "~" must not pass through as an ordinary path. Nothing downstream
// treats it as special, so it resolves against the working directory: a literal "~"
// directory holding the tags or the cache the user meant to put in their home.
func TestExpandHomeReportsAMissingHome(t *testing.T) {
	t.Setenv("HOME", "")
	for _, in := range []string{"~", "~/lib/assets"} {
		got, err := ExpandHome(in)
		if err == nil {
			t.Errorf("ExpandHome(%q) = %q with no home; want an error", in, got)
		}
		if got != "" {
			t.Errorf("ExpandHome(%q) returned %q alongside the error; want no path at all", in, got)
		}
	}
	// A path with no "~" in it needs no home and must still resolve.
	if got, err := ExpandHome("/abs/path"); err != nil || got != "/abs/path" {
		t.Errorf("ExpandHome(/abs/path) = %q, %v; want the path unchanged", got, err)
	}
}

// A key quarry does not know is a setting the user believes is in effect. Silently
// ignoring `follow_symlink` (singular) means a whole drive missing from the index
// with nothing said about it.
// A key this version does not know is a setting the user believes is in effect, and
// the shapes it arrives in differ: a misspelled sibling, a whole table from a newer
// quarry, and a known key given the wrong type. Only the first was covered, and the
// second rides on Undecoded() reporting nested keys — which is not obvious from the
// call site.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantIn string
	}{
		{"misspelled sibling", "root = \"/x\"\nfollow_symlink = true\n", "follow_symlink"},
		{"table from a newer version", "root = \"/x\"\n\n[index]\n  workers = 4\n", "index"},
		{"known key, wrong type", "root = \"/x\"\nfollow_symlinks = \"yes\"\n", "follow_symlinks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearQuarryEnv(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if err == nil {
				t.Fatal("the config was accepted silently")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name %q", err, tc.wantIn)
			}
		})
	}
}

// A config that cannot be read is not a config that is absent: the settings the user
// wrote are not in effect and nothing else will say so.
func TestLoadRejectsAnUnreadableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file regardless")
	}
	clearQuarryEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("root = \"/x\"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("an unreadable config.toml was treated as an absent one")
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

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("root = \"/x\"\nfollow_symlinks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.FollowSymlinks {
		t.Error("follow_symlinks = true in config.toml was not read")
	}
}
