// Package config resolves quarry's user-scoped settings. The config and cache
// directories follow XDG with fallbacks; an optional config.toml in the config
// directory supplies defaults, and environment variables then flags override it. No
// machine-specific path is baked into the tool.
package config

import (
	"errors"
	"fmt"
	"io/fs"
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
	// FollowSymlinks indexes symlinked directories pointing outside the root, for a
	// library assembled across several drives. Off unless asked for.
	FollowSymlinks bool
}

type fileConfig struct {
	Root           string `toml:"root"`
	FollowSymlinks bool   `toml:"follow_symlinks"`
}

// ResolveDir picks the user config directory, which holds config.toml and the
// user-wide tag store: an explicit flag, else $QUARRY_CONFIG_DIR, else
// $XDG_CONFIG_HOME/quarry, else ~/.config/quarry.
func ResolveDir(flag string) (string, error) {
	return resolveXDG(flag, "QUARRY_CONFIG_DIR", "XDG_CONFIG_HOME", ".config",
		"cannot locate your config directory: %w; pass --config <dir> or set QUARRY_CONFIG_DIR")
}

// ResolveCacheDir picks the directory for regenerable state (the asset index and
// unpacked archive contents): an explicit flag, else $QUARRY_CACHE_DIR, else
// $XDG_CACHE_HOME/quarry, else ~/.cache/quarry. This is expendable data, so it lives
// under the cache home, away from config and from the asset library itself.
func ResolveCacheDir(flag string) (string, error) {
	return resolveXDG(flag, "QUARRY_CACHE_DIR", "XDG_CACHE_HOME", ".cache",
		"cannot locate your cache directory: %w; pass --cache <dir> or set QUARRY_CACHE_DIR")
}

// resolveXDG walks the documented precedence and reports failure rather than falling
// back to a name in the working directory. A relative fallback reads as harmless and
// is not: quarry is most naturally run from inside the library, and the cache dir is
// where the index and every unpacked archive get written — into the read-only tree,
// for the next run to then index.
func resolveXDG(flag, ownEnv, xdgEnv, homeSub, failure string) (string, error) {
	if flag != "" {
		return ExpandHome(flag), nil
	}
	if v := os.Getenv(ownEnv); v != "" {
		return ExpandHome(v), nil
	}
	if v := os.Getenv(xdgEnv); v != "" {
		return filepath.Join(ExpandHome(v), "quarry"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(failure, err)
	}
	return filepath.Join(home, homeSub, "quarry"), nil
}

// Load reads an optional config.toml in dir, then applies the QUARRY_ROOT override. A
// missing config.toml is fine: every setting has a flag. Anything else about the file
// is reported rather than absorbed — a config that cannot be read is not a config that
// is absent, and a key this version does not know is a setting the user believes is in
// effect. A silently ignored `follow_symlink` typo is a whole drive missing from the
// index with nothing said.
func Load(dir string) (Config, error) {
	var c Config
	p := filepath.Join(dir, "config.toml")
	var fc fileConfig
	md, err := toml.DecodeFile(p, &fc)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("reading %s: %w", p, err)
	default:
		if unknown := md.Undecoded(); len(unknown) > 0 {
			keys := make([]string, len(unknown))
			for i, k := range unknown {
				keys[i] = k.String()
			}
			return Config{}, fmt.Errorf("%s sets keys this version of quarry does not understand (%s)", p, strings.Join(keys, ", "))
		}
		c.Root, c.FollowSymlinks = fc.Root, fc.FollowSymlinks
	}
	if v := os.Getenv("QUARRY_ROOT"); v != "" {
		c.Root = v
	}
	c.Root = ExpandHome(c.Root)
	return c, nil
}

// ExpandHome resolves a leading "~" against the user's home directory. It is
// exported because --root overrides the value Load has already expanded, and a path
// typed into a systemd unit or a wrapper script reaches the flag with its "~" intact
// — no shell having been there to expand it.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
