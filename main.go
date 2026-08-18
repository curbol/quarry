// Command quarry indexes a local game-asset library and serves a web UI to search,
// preview, and tag it. See README.md.
//
//	quarry                # index the configured root and open the browser
//	quarry --root <dir>   # index somewhere else for this run
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/curbol/quarry/internal/assetindex"
	"github.com/curbol/quarry/internal/browse"
	"github.com/curbol/quarry/internal/config"
	"github.com/curbol/quarry/internal/selfupdate"
	"github.com/curbol/quarry/internal/tagstore"
)

// version is the release version, set at build time via
// -ldflags "-X main.version=<v>". A local build carries the sentinel selfupdate
// refuses to replace.
var version = selfupdate.DevVersion

// defaultAddr is where the UI serves when --addr is not given.
const defaultAddr = "localhost:8788"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quarry:", err)
		os.Exit(1)
	}
}

// stdout is where the tool's actual output goes. It is a package variable so tests
// can capture and assert it; progress and diagnostics stay on stderr.
var stdout io.Writer = os.Stdout

// settings is everything a run resolves before it serves. It is a struct because
// every one of these is a plain scalar that would otherwise be a positional argument
// next to three others of the same type.
type settings struct {
	Root           string
	Addr           string
	CacheDir       string
	TagsPath       string
	Reindex        bool
	FollowSymlinks bool
}

// served indexes and serves. It is a package variable for the same reason as stdout:
// a test can then assert what the config/env/flag chain resolved without standing up
// a server and blocking on it.
var served = serve

// run dispatches the command line. Serving is the whole point of the tool, so it is
// what a bare `quarry` does; update and version are the only subcommands.
func run(args []string) error {
	cmd := ""
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "update", "version", "help":
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}

	fs := flag.NewFlagSet("quarry", flag.ContinueOnError)
	// Silence the flag package's own dump: help prints usage() below, and a bad flag
	// comes back as an error that main reports once.
	fs.SetOutput(io.Discard)
	cfgDir := fs.String("config", "", "user config dir holding config.toml (default: $XDG_CONFIG_HOME/quarry or ~/.config/quarry)")
	root := fs.String("root", "", "asset scan root (overrides config.toml / QUARRY_ROOT)")
	addr := fs.String("addr", defaultAddr, "server address (host:port)")
	reindex := fs.Bool("reindex", false, "rebuild the asset index from scratch")
	cacheFlag := fs.String("cache", "", "cache dir for the index and unpacked archives (default: $XDG_CACHE_HOME/quarry)")
	tagsFlag := fs.String("tags", "", "tag store path (default: the nearest quarry.tags.toml walking up from cwd, else the one in the config dir)")
	follow := fs.Bool("follow-symlinks", false, "index symlinked directories pointing outside the scan root")
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usageTo(stdout)
			return nil
		}
		return err
	}
	if cmd == "help" {
		usageTo(stdout)
		return nil
	}
	if cmd == "version" || *showVersion {
		printVersion()
		return nil
	}

	// flag.Parse stops at the first non-flag argument, so an unchecked positional
	// silently swallows every flag after it.
	if cmd == "update" {
		if fs.NArg() > 1 {
			return fmt.Errorf("update takes at most one version argument, got %d", fs.NArg())
		}
		return selfupdate.Run(version, fs.Arg(0))
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("quarry takes no positional arguments (got %q); to index elsewhere use --root %s", fs.Arg(0), fs.Arg(0))
	}

	configDir, err := config.ResolveDir(*cfgDir)
	if err != nil {
		return err
	}
	cacheDir, err := config.ResolveCacheDir(*cacheFlag)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}
	if *root != "" {
		cfg.Root = config.ExpandHome(*root)
	}
	// A bool flag cannot distinguish unset from false by its value, so only an
	// explicitly passed --follow-symlinks overrides config.toml.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "follow-symlinks" {
			cfg.FollowSymlinks = *follow
		}
	})
	if cfg.Root == "" {
		return fmt.Errorf("no scan root: pass --root <dir>, set QUARRY_ROOT, or add\n  root = \"/path/to/your/assets\"\nto %s", filepath.Join(configDir, "config.toml"))
	}
	return served(settings{
		Root:           cfg.Root,
		Addr:           *addr,
		CacheDir:       cacheDir,
		TagsPath:       resolveTagsPath(*tagsFlag, configDir),
		Reindex:        *reindex,
		FollowSymlinks: cfg.FollowSymlinks,
	})
}

// isFlag reports whether an argument is a flag rather than a subcommand, so that a
// bare `quarry --root x` is not read as a subcommand named "--root".
func isFlag(arg string) bool {
	return len(arg) > 1 && arg[0] == '-'
}

// resolveTagsPath locates the tag store. An explicit --tags wins. Otherwise a
// quarry.tags.toml found by walking up from the working directory is a project store
// that travels with that project; with none in sight the store is the user-wide one
// in the config dir, so tagging works from anywhere and is never silently off.
func resolveTagsPath(tagsFlag, configDir string) string {
	if tagsFlag != "" {
		return config.ExpandHome(tagsFlag)
	}
	if wd, err := os.Getwd(); err == nil {
		if p, ok := tagstore.Discover(wd); ok {
			return p
		}
	}
	return filepath.Join(configDir, tagstore.FileName)
}

// withinRoot reports whether dir sits inside root, comparing resolved absolute
// paths so a symlinked cache dir cannot slip past the check.
func withinRoot(root, dir string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return filepath.Clean(p)
	}
	rel, err := filepath.Rel(resolve(root), resolve(dir))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// serve indexes the asset root and runs the UI until interrupted.
func serve(s settings) error {
	// The cache holds the index and every unpacked archive. Under the scan root it
	// would be written into a tree quarry promises to leave alone, and indexed as
	// library content on the next run.
	if withinRoot(s.Root, s.CacheDir) {
		return fmt.Errorf("cache dir %s is inside the scan root %s; pick one outside it with --cache", s.CacheDir, s.Root)
	}
	warn := func(m string) { fmt.Fprintln(os.Stderr, "warning:", m) }
	fmt.Fprintf(os.Stderr, "indexing %s …\n", s.Root)
	opt := assetindex.Options{Root: s.Root, CacheDir: s.CacheDir, FollowSymlinks: s.FollowSymlinks}
	ix, err := assetindex.LoadOrBuild(opt, s.Reindex, warn)
	if err != nil {
		return fmt.Errorf("build asset index: %w", err)
	}
	for _, s := range ix.Skipped {
		warn(fmt.Sprintf("skipped %s: %s", s.RelPath, s.Reason))
	}
	// Each pack update extracts to a new fingerprint-keyed dir; without this the
	// previous extraction stays in the cache forever.
	if err := ix.PruneUnpacked(); err != nil {
		warn(fmt.Sprintf("could not prune stale unpacked archives: %v", err))
	}
	// The user-wide store lives in the config dir, which need not exist yet on a
	// machine that has never written a config.toml.
	if err := os.MkdirAll(filepath.Dir(s.TagsPath), 0o755); err != nil {
		return fmt.Errorf("prepare tag store dir: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return browse.Serve(ctx, s.Addr, ix, s.TagsPath)
}

func printVersion() {
	fmt.Fprintf(stdout, "quarry %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// usage writes the help text to w. An explicitly requested `quarry help` goes to
// stdout so it can be piped; the same text on an error path goes to stderr.
func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprint(w, `quarry - search and 3D-preview a local game-asset library

usage:
  quarry [flags]          index the asset root and serve the UI
  quarry update [ver]     update to the latest release (or a specific version)
  quarry version          print the version
  quarry help             print this message

flags:
  -root <dir>         asset scan root (overrides config.toml / QUARRY_ROOT)
  -addr <host:port>   server address (default: localhost:8788)
  -reindex            rebuild the asset index from scratch
  -cache <dir>        index / unpacked-archive cache dir (default: $XDG_CACHE_HOME/quarry)
  -tags <path>        tag store path (default: nearest quarry.tags.toml, else the one in the config dir)
  -follow-symlinks    index symlinked dirs pointing outside the scan root
  -config <dir>       user config dir with config.toml (default: $XDG_CONFIG_HOME/quarry or ~/.config/quarry)

The scan root is the one setting with no default; set it once in config.toml. Tags
are stored by content fingerprint, so they survive a pack update, a re-index, and a
move to another machine. Run from a directory holding a quarry.tags.toml to use that
project's tags instead of your user-wide ones.
`)
}
