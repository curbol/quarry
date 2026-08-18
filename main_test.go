package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/curbol/quarry/internal/tagstore"
)

func TestRunUnknownSubcommand(t *testing.T) {
	err := run([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("got %v, want unknown-subcommand error", err)
	}
}

// captureStdout redirects the tool's output for the duration of a test. stdout is a
// package variable precisely so this is possible; asserting only that run() returned
// nil would pass for a command that printed nothing, printed to the wrong stream, or
// dropped the version — and `quarry version` is a scriptable contract install.sh
// depends on.
func captureStdout(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = prev })
	return &buf
}

func TestRunHelp(t *testing.T) {
	for _, a := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		out := captureStdout(t)
		if err := run(a); err != nil {
			t.Errorf("%v: %v", a, err)
			continue
		}
		got := out.String()
		// An explicitly requested help goes to stdout so it can be piped or paged.
		for _, want := range []string{"usage:", "-root", "-tags", "quarry update"} {
			if !strings.Contains(got, want) {
				t.Errorf("%v: help output does not mention %q:\n%s", a, want, got)
			}
		}
	}
}

func TestRunVersion(t *testing.T) {
	for _, a := range [][]string{{"version"}, {"--version"}} {
		out := captureStdout(t)
		if err := run(a); err != nil {
			t.Errorf("%v: %v", a, err)
			continue
		}
		got := strings.TrimSpace(out.String())
		if !strings.HasPrefix(got, "quarry "+version) {
			t.Errorf("%v printed %q, want it to start with \"quarry %s\"", a, got, version)
		}
		if !strings.Contains(got, runtime.GOOS) || !strings.Contains(got, runtime.GOARCH) {
			t.Errorf("%v printed %q, want the platform named", a, got)
		}
	}
}

// A bare `quarry` serves, so the first argument may legitimately be a flag. Reading
// it as a subcommand would reject every flag-only invocation.
//
// This is the one test that reaches the real serve, so it has to name every
// directory it might touch: without --cache it resolves the caller's actual XDG
// cache, reads whatever index.json is there, and would write over it for any root
// that happens to exist.
func TestLeadingFlagIsNotASubcommand(t *testing.T) {
	t.Chdir(t.TempDir())
	err := run([]string{
		"--root", filepath.Join(t.TempDir(), "nope"),
		"--config", t.TempDir(),
		"--cache", t.TempDir(),
	})
	if err != nil && strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("leading flag parsed as a subcommand: %v", err)
	}
}

// Indexing whatever directory the user happened to be standing in would be a slow,
// surprising accident, so an unset root has to fail loudly instead.
func TestRunWithoutRootFails(t *testing.T) {
	t.Setenv("QUARRY_ROOT", "")
	t.Chdir(t.TempDir())
	err := run([]string{"--config", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no scan root") {
		t.Errorf("got %v, want a no-scan-root error", err)
	}
}

func TestStrayPositionalIsRejected(t *testing.T) {
	err := run([]string{"--config", t.TempDir(), "/some/dir"})
	if err == nil || !strings.Contains(err.Error(), "positional") {
		t.Errorf("got %v, want a positional-argument error", err)
	}
}

func TestUpdateRejectsExtraArguments(t *testing.T) {
	err := run([]string{"update", "1.0.0", "2.0.0"})
	if err == nil || !strings.Contains(err.Error(), "at most one version") {
		t.Errorf("got %v, want an argument-count error", err)
	}
}

func TestResolveTagsPath(t *testing.T) {
	cfgDir := t.TempDir()

	// An explicit --tags wins outright.
	if got := resolveTagsPath("/custom/tags.toml", cfgDir); got != "/custom/tags.toml" {
		t.Errorf("explicit --tags = %q", got)
	}

	// With no project store in sight, the user-wide store in the config dir is used
	// rather than tagging being switched off.
	t.Chdir(t.TempDir())
	if got, want := resolveTagsPath("", cfgDir), filepath.Join(cfgDir, tagstore.FileName); got != want {
		t.Errorf("fallback tags path = %q, want %q", got, want)
	}

	// A store up the tree is a project store and takes precedence over the user one.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, tagstore.FileName), []byte("\n"), 0o644)
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	if got, want := resolveTagsPath("", cfgDir), filepath.Join(dir, tagstore.FileName); got != want {
		t.Errorf("discovered tags path = %q, want %q", got, want)
	}
}

// The documented precedence is config.toml, then QUARRY_ROOT, then --root. config
// covers the first two hops; the last one lives here, so only a run through the CLI
// proves the flag actually wins.
func TestRootFlagBeatsEnvironment(t *testing.T) {
	t.Chdir(t.TempDir()) // resolveTagsPath walks up from cwd; keep it off the real tree
	cfgDir := t.TempDir()
	envRoot := t.TempDir()
	flagRoot := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("root = "+strconv.Quote(t.TempDir())+"\n"), 0o644)
	t.Setenv("QUARRY_ROOT", envRoot)

	var got string
	served = func(s settings) error {
		got = s.Root
		return nil
	}
	t.Cleanup(func() { served = serve })

	if err := run([]string{"--config", cfgDir, "--root", flagRoot}); err != nil {
		t.Fatal(err)
	}
	if got != flagRoot {
		t.Errorf("resolved root = %q, want the --root value %q", got, flagRoot)
	}

	got = ""
	if err := run([]string{"--config", cfgDir}); err != nil {
		t.Fatal(err)
	}
	if got != envRoot {
		t.Errorf("with no --root, resolved root = %q, want QUARRY_ROOT %q", got, envRoot)
	}
}

// A bool flag's value cannot say whether it was passed, so config.toml has to win
// until --follow-symlinks actually appears on the command line.
func TestFollowSymlinksFlagOverridesConfig(t *testing.T) {
	t.Chdir(t.TempDir()) // resolveTagsPath walks up from cwd; keep it off the real tree
	t.Setenv("QUARRY_ROOT", "")
	cfgDir := t.TempDir()
	root := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("root = "+strconv.Quote(root)+"\nfollow_symlinks = true\n"), 0o644)

	var got settings
	served = func(s settings) error { got = s; return nil }
	t.Cleanup(func() { served = serve })

	if err := run([]string{"--config", cfgDir, "--cache", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !got.FollowSymlinks {
		t.Error("config.toml's follow_symlinks was lost when the flag was absent")
	}

	if err := run([]string{"--config", cfgDir, "--cache", t.TempDir(), "--follow-symlinks=false"}); err != nil {
		t.Fatal(err)
	}
	if got.FollowSymlinks {
		t.Error("an explicit --follow-symlinks=false did not override config.toml")
	}
}

// serve() is where the flags become an actual index, prune and tag store, and nothing
// exercised it end to end: the one test that reached it bailed inside LoadOrBuild on a
// root that does not exist. Everything it touches is a temp dir, so a failure here is a
// real failure rather than a machine's state leaking in.
func TestServeIndexesAndPreparesTheTagStore(t *testing.T) {
	t.Chdir(t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "synty", "Pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "synty", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tag store under a directory that does not exist yet: serve has to create it,
	// which is the case a machine that never wrote a config.toml is in.
	tagsPath := filepath.Join(t.TempDir(), "nested", tagstore.FileName)
	cacheDir := t.TempDir()

	// browse.Serve blocks until interrupted, so the run is stopped at the listen with an
	// address that cannot bind. Everything under test happens before that point.
	err := run([]string{
		"--root", root,
		"--config", t.TempDir(),
		"--cache", cacheDir,
		"--tags", tagsPath,
		"--addr", "256.256.256.256:1",
	})
	if err == nil {
		t.Fatal("expected the bogus address to fail the listen")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("failed before listening: %v", err)
	}

	// Everything serve does ahead of listening must have happened.
	if _, statErr := os.Stat(filepath.Dir(tagsPath)); statErr != nil {
		t.Errorf("the tag store's directory was not created: %v", statErr)
	}
	entries, readErr := os.ReadDir(filepath.Join(cacheDir, "roots"))
	if readErr != nil {
		t.Fatalf("no per-root cache state was written: %v", readErr)
	}
	if len(entries) != 1 {
		t.Errorf("cache roots = %d, want one for this scan root", len(entries))
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "roots", entries[0].Name(), "index.json")); statErr != nil {
		t.Errorf("the index cache was not written: %v", statErr)
	}
}

// The cache holds the index and every unpacked archive. Under the scan root it would be
// written into a tree quarry promises to leave alone, and indexed as library content on
// the next run.
func TestServeRefusesACacheDirInsideTheScanRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	root := t.TempDir()
	err := run([]string{
		"--root", root,
		"--config", t.TempDir(),
		"--cache", filepath.Join(root, "cache"),
		"--tags", filepath.Join(t.TempDir(), tagstore.FileName),
	})
	if err == nil || !strings.Contains(err.Error(), "inside the scan root") {
		t.Errorf("got %v, want a refusal naming the scan root", err)
	}
}
