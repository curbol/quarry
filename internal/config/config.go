// Package config resolves quarry's user-scoped settings. The config and cache
// directories follow XDG with fallbacks; an optional config.toml in the config
// directory supplies defaults, and environment variables then flags override it. No
// machine-specific path is baked into the tool.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the resolved machine configuration. It holds only where to look: what to
// do with what is found belongs to the index and the tag store, which key on content
// rather than on any setting here.
type Config struct {
	Root string // asset scan root; empty until the user sets one
}

type fileConfig struct {
	Root string `toml:"root"`
}

// ResolveDir picks the user config directory, which holds config.toml and the
// user-wide tag store: an explicit flag, else $QUARRY_CONFIG_DIR, else
// $XDG_CONFIG_HOME/quarry, else ~/.config/quarry.
func ResolveDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("QUARRY_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "quarry")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "quarry")
	}
	return "quarry"
}

// ResolveCacheDir picks the directory for regenerable state (the asset index and
// unpacked archive contents): an explicit flag, else $QUARRY_CACHE_DIR, else
// $XDG_CACHE_HOME/quarry, else ~/.cache/quarry. This is expendable data, so it lives
// under the cache home, away from config and from the asset library itself.
func ResolveCacheDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("QUARRY_CACHE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "quarry")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "quarry")
	}
	return "quarry-cache"
}

// Load reads an optional config.toml in dir, then applies the QUARRY_ROOT override. A
// missing config.toml is fine: every setting has a flag.
func Load(dir string) (Config, error) {
	var c Config
	p := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(p); err == nil {
		var fc fileConfig
		if _, err := toml.DecodeFile(p, &fc); err != nil {
			return Config{}, err
		}
		if fc.Root != "" {
			c.Root = fc.Root
		}
	}
	if v := os.Getenv("QUARRY_ROOT"); v != "" {
		c.Root = v
	}
	c.Root = expandHome(c.Root)
	return c, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
