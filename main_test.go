package main

import (
	"os"
	"path/filepath"
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

func TestRunHelp(t *testing.T) {
	for _, a := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if err := run(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
}

func TestRunVersion(t *testing.T) {
	for _, a := range [][]string{{"version"}, {"--version"}} {
		if err := run(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
}

// A bare `quarry` serves, so the first argument may legitimately be a flag. Reading
// it as a subcommand would reject every flag-only invocation.
func TestLeadingFlagIsNotASubcommand(t *testing.T) {
	t.Chdir(t.TempDir())
	err := run([]string{"--root", filepath.Join(t.TempDir(), "nope"), "--config", t.TempDir()})
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
	cfgDir := t.TempDir()
	envRoot := t.TempDir()
	flagRoot := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("root = "+strconv.Quote(t.TempDir())+"\n"), 0o644)
	t.Setenv("QUARRY_ROOT", envRoot)

	var got string
	served = func(root, _, _ string, _ bool, _ string) error {
		got = root
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
