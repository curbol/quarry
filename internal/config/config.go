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
		"config directory", "pass --config <dir> or set QUARRY_CONFIG_DIR")
}

// ResolveCacheDir picks the directory for regenerable state (the asset index and
// unpacked archive contents): an explicit flag, else $QUARRY_CACHE_DIR, else
// $XDG_CACHE_HOME/quarry, else ~/.cache/quarry. This is expendable data, so it lives
// under the cache home, away from config and from the asset library itself.
func ResolveCacheDir(flag string) (string, error) {
	return resolveXDG(flag, "QUARRY_CACHE_DIR", "XDG_CACHE_HOME", ".cache",
		"cache directory", "pass --cache <dir> or set QUARRY_CACHE_DIR")
}

// resolveXDG walks the documented precedence and always returns an absolute path,
// reporting failure rather than falling back to a name in the working directory. A
// relative result reads as harmless and is not: quarry is most naturally run from
// inside the library, and the cache dir is where the index and every unpacked archive
// get written — into the read-only tree, for the next run to then index. A relative
// flag is the one exception, resolved against the working directory rather than
// refused, because a flag is one invocation saying "here" rather than a setting that
// follows the user into every directory.
func resolveXDG(flag, ownEnv, xdgEnv, homeSub, what, hint string) (string, error) {
	// The message is assembled here from two plain strings rather than taken as a
	// format string, so the %w stays where go vet's printf check can see it. Passed in,
	// an edit that dropped it would put "%!(EXTRA *errors.errorString=...)" in front of
	// a user with nothing reporting it.
	failure := func(err error) error {
		return fmt.Errorf("cannot locate your %s: %w; %s", what, err, hint)
	}
	// Expanding a "~" needs the same home directory the last branch does, so it fails
	// the same way and carries the same hint. Returning the path unexpanded instead
	// would hand back a cwd-relative "~/…" with no error — the fallback this function
	// exists to refuse, arriving through the branches that look like they cannot fail.
	expand := func(p string) (string, error) {
		v, err := ExpandHome(p)
		if err != nil {
			return "", failure(err)
		}
		return v, nil
	}
	// A flag is one invocation's explicit choice, so a relative one means "here, now"
	// and is resolved against the working directory rather than refused — but resolved,
	// not passed through, so what comes back is still never a bare name.
	if flag != "" {
		v, err := expand(flag)
		if err != nil {
			return "", err
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", failure(err)
		}
		return abs, nil
	}
	// An own env var is not one invocation's choice: it lives in a shell rc and applies
	// to every directory quarry is run from, which is the case the XDG branch below
	// refuses a relative value for. QUARRY_CACHE_DIR=.quarry would give each directory
	// its own full index and unpacked-archive tree.
	if v := os.Getenv(ownEnv); v != "" {
		p, err := expand(v)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", ownEnv, v)
		}
		return p, nil
	}
	// A relative value is invalid per the XDG base-directory spec and is ignored rather
	// than joined, which is also what keeps this function's promise: XDG_CACHE_HOME=.cache
	// otherwise resolves to a different directory for every directory quarry is run
	// from, each holding its own full re-index — and quarry is most naturally run from
	// inside the library.
	if v := os.Getenv(xdgEnv); v != "" {
		base, err := expand(v)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(base) {
			return filepath.Join(base, "quarry"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", failure(err)
	}
	// UserHomeDir hands back $HOME unexamined, so a relative one lands here as a
	// relative result from the branch that looks least able to produce one.
	if !filepath.IsAbs(home) {
		return "", failure(fmt.Errorf("$HOME is %q, which is not an absolute path", home))
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
	root, err := ExpandHome(c.Root)
	if err != nil {
		return Config{}, err
	}
	c.Root = root
	return c, nil
}

// ExpandHome resolves a leading "~" against the user's home directory. It is
// exported because --root overrides the value Load has already expanded, and a path
// typed into a systemd unit or a wrapper script reaches the flag with its "~" intact
// — no shell having been there to expand it.
//
// A home directory that cannot be found is reported rather than left in the path. The
// unexpanded form is not a harmless passthrough: nothing downstream treats "~" as
// special, so it becomes an ordinary directory name resolved against the working
// directory — a literal "~" created beside whatever quarry was run from, holding the
// tags or the cache the user meant to put in their home. A systemd unit with no
// Environment=HOME= is exactly the setting this function was added for, and exactly
// where UserHomeDir fails.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
