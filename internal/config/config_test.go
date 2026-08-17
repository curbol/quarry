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

func TestResolveCacheDir(t *testing.T) {
	clearQuarryEnv(t)
	t.Setenv("XDG_CACHE_HOME", "")
	if got := ResolveCacheDir("/explicit"); got != "/explicit" {
		t.Errorf("flag: got %q", got)
	}
	t.Setenv("QUARRY_CACHE_DIR", "/from/quarry-cache-env")
	if got := ResolveCacheDir(""); got != "/from/quarry-cache-env" {
		t.Errorf("QUARRY_CACHE_DIR: got %q", got)
	}
	t.Setenv("QUARRY_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/xdgcache")
	if got := ResolveCacheDir(""); got != filepath.Join("/xdgcache", "quarry") {
		t.Errorf("XDG_CACHE_HOME: got %q", got)
	}
	t.Setenv("XDG_CACHE_HOME", "")
	if got := ResolveCacheDir(""); !strings.HasSuffix(got, filepath.Join(".cache", "quarry")) {
		t.Errorf("home fallback: got %q", got)
	}
}

func TestResolveDir(t *testing.T) {
	clearQuarryEnv(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := ResolveDir("/explicit"); got != "/explicit" {
		t.Errorf("flag: got %q", got)
	}
	t.Setenv("QUARRY_CONFIG_DIR", "/from/quarry-env")
	if got := ResolveDir(""); got != "/from/quarry-env" {
		t.Errorf("QUARRY_CONFIG_DIR: got %q", got)
	}
	t.Setenv("QUARRY_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := ResolveDir(""); got != filepath.Join("/xdg", "quarry") {
		t.Errorf("XDG: got %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := ResolveDir(""); !strings.HasSuffix(got, filepath.Join(".config", "quarry")) {
		t.Errorf("home fallback: got %q", got)
	}
}
