package main

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/curbol/quarry/internal/assetindex"
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
		// Bounded too, for the same reason as the three above: without it
		// resolveTagsPath walks up from the working directory looking for a project
		// store, and serve() then creates the directory it settles on. Harmless only
		// because LoadOrBuild fails on the nonexistent root first, which is one
		// reordering away from not being true.
		"--tags", filepath.Join(t.TempDir(), "quarry.tags.toml"),
	})
	if err != nil && strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("leading flag parsed as a subcommand: %v", err)
	}
}

// clearQuarryEnv isolates run() from the machine it is running on. A test that passes
// --config but not --cache still resolves the cache dir from the environment, so a
// maintainer with a relative QUARRY_CACHE_DIR exported gets "must be an absolute path"
// where the test asserts something else entirely — a failure about their shell, in a
// test about flags. config_test.go has had this for the same reason.
func clearQuarryEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"QUARRY_ROOT", "QUARRY_CONFIG_DIR", "QUARRY_CACHE_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

// Indexing whatever directory the user happened to be standing in would be a slow,
// surprising accident, so an unset root has to fail loudly instead.
func TestRunWithoutRootFails(t *testing.T) {
	clearQuarryEnv(t)
	t.Chdir(t.TempDir())
	err := run([]string{"--config", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no scan root") {
		t.Errorf("got %v, want a no-scan-root error", err)
	}
}

func TestStrayPositionalIsRejected(t *testing.T) {
	clearQuarryEnv(t)
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

// chdirCleanTree moves into a fresh temp dir and confirms no tag store sits above it.
// Discover walks to the filesystem root, so a stray quarry.tags.toml anywhere up the
// real tree — someone having run quarry from /tmp once — decides these tests, and the
// failure names a path nothing in the test wrote.
func chdirCleanTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if p, ok := tagstore.Discover(dir); ok {
		t.Skipf("a tag store above the temp tree (%s) would decide this test", p)
	}
	return dir
}

func TestResolveTagsPath(t *testing.T) {
	cfgDir := t.TempDir()

	mustResolve := func(flag string) string {
		t.Helper()
		got, err := resolveTagsPath(flag, cfgDir)
		if err != nil {
			t.Fatalf("resolveTagsPath(%q) = %v", flag, err)
		}
		return got
	}

	// An explicit --tags wins outright.
	if got := mustResolve("/custom/tags.toml"); got != "/custom/tags.toml" {
		t.Errorf("explicit --tags = %q", got)
	}

	// A leading ~ is expanded here rather than left to a shell that was never there:
	// --tags reaches quarry verbatim from a systemd unit or a wrapper script. Unexpanded
	// it survives all the way to serve's MkdirAll, which makes a directory literally
	// named "~" beside the working directory and writes the tag store — the one thing
	// quarry keeps that it cannot regenerate — into it, with nothing reported.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := mustResolve("~/tags.toml"), filepath.Join(home, "tags.toml"); got != want {
		t.Errorf("--tags ~/tags.toml = %q, want %q", got, want)
	}

	// With no project store in sight, the user-wide store in the config dir is used
	// rather than tagging being switched off.
	chdirCleanTree(t)
	if got, want := mustResolve(""), filepath.Join(cfgDir, tagstore.FileName); got != want {
		t.Errorf("fallback tags path = %q, want %q", got, want)
	}

	// A store up the tree is a project store and takes precedence over the user one.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, tagstore.FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if got, want := mustResolve(""), filepath.Join(dir, tagstore.FileName); got != want {
		t.Errorf("discovered tags path = %q, want %q", got, want)
	}
}

// The documented precedence is config.toml, then QUARRY_ROOT, then --root. config
// covers the first two hops; the last one lives here, so only a run through the CLI
// proves the flag actually wins.
func TestRootFlagBeatsEnvironment(t *testing.T) {
	chdirCleanTree(t) // resolveTagsPath walks up from cwd
	cfgDir := t.TempDir()
	cacheDir := t.TempDir() // passed explicitly, or the run resolves the caller's real one
	envRoot := t.TempDir()
	flagRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("root = "+strconv.Quote(t.TempDir())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUARRY_ROOT", envRoot)

	var got string
	served = func(s settings) error {
		got = s.Root
		return nil
	}
	t.Cleanup(func() { served = serve })

	if err := run([]string{"--config", cfgDir, "--cache", cacheDir, "--root", flagRoot}); err != nil {
		t.Fatal(err)
	}
	if got != flagRoot {
		t.Errorf("resolved root = %q, want the --root value %q", got, flagRoot)
	}

	got = ""
	if err := run([]string{"--config", cfgDir, "--cache", cacheDir}); err != nil {
		t.Fatal(err)
	}
	if got != envRoot {
		t.Errorf("with no --root, resolved root = %q, want QUARRY_ROOT %q", got, envRoot)
	}

	// --root arrives verbatim from a unit file or a wrapper script, with no shell in
	// between to have expanded it. Left alone, the ~ reaches the scan as a directory
	// name and the run fails naming a path that does not exist.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	got = ""
	if err := run([]string{"--config", cfgDir, "--cache", cacheDir, "--root", "~/assets"}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "assets"); got != want {
		t.Errorf("--root ~/assets resolved to %q, want %q", got, want)
	}
}

// A bool flag's value cannot say whether it was passed, so config.toml has to win
// until --follow-symlinks actually appears on the command line.
func TestFollowSymlinksFlagOverridesConfig(t *testing.T) {
	chdirCleanTree(t) // resolveTagsPath walks up from cwd
	t.Setenv("QUARRY_ROOT", "")
	cfgDir := t.TempDir()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("root = "+strconv.Quote(root)+"\nfollow_symlinks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got settings
	served = func(s settings) error { got = s; return nil }
	t.Cleanup(func() { served = serve })

	if err := run([]string{"--config", cfgDir, "--cache", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !got.FollowSymlinks {
		t.Error("config.toml's follow_symlinks was lost when the flag was absent")
	}
	// browse.Serve reads an empty tags path as "tagging disabled", and the CLI is
	// documented never to pass one. Asserted on the settings a real run produced,
	// because resolveTagsPath being correct in isolation says nothing about it still
	// being wired into the struct below.
	if got.TagsPath == "" {
		t.Error("the CLI passed an empty tagsPath, which browse.Serve reads as tagging disabled")
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
	//
	// The address is a port already held rather than an unparseable one: an address
	// that is not an IP goes to the resolver, which is a real DNS query in a suite
	// that is otherwise entirely offline, and makes the test hang behind a blackholed
	// one. Port 0 asks the kernel for a free port, so this cannot collide with
	// anything else on the machine, and a privileged low port would not fail for a
	// container running as root.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	err = run([]string{
		"--root", root,
		"--config", t.TempDir(),
		"--cache", cacheDir,
		"--tags", tagsPath,
		"--addr", busy.Addr().String(),
	})
	if err == nil {
		t.Fatal("expected the occupied address to fail the listen")
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

// A relative --root is the one deliberate exception to the absolute-root rule, and two
// separate pieces of code have to keep it working: config.Load must not apply the rule
// to the flag, and assetindex must resolve it against the working directory before
// anything is keyed on it. Neither was covered — every --root in this file was a
// t.TempDir() and every Options.Root in assetindex was pre-absolute — so an IsAbs check
// added here for symmetry, or the filepath.Abs dropped there, passed the whole suite.
//
// The second half is what makes it worth a run through the CLI rather than a unit test:
// stateDir hashes the root, so a relative one gives every working directory its own
// cache entry, and PruneUnpacked run from elsewhere sweeps extractions another instance
// is serving.
func TestARelativeRootFlagResolvesAgainstTheWorkingDirectory(t *testing.T) {
	parent := t.TempDir()
	// Resolved, because macOS hands out /var/... temp dirs behind a /private symlink
	// and stateDir hashes the path assetindex resolved, not the one typed here.
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "lib", "synty", "Pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "lib", "synty", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)
	clearQuarryEnv(t)
	cacheDir := t.TempDir()

	var got string
	served = func(s settings) error { got = s.Root; return nil }
	t.Cleanup(func() { served = serve })

	if err := run([]string{"--config", t.TempDir(), "--cache", cacheDir, "--root", "lib"}); err != nil {
		t.Fatalf("a relative --root was refused: %v", err)
	}
	if got != "lib" {
		t.Errorf("settings.Root = %q, want the flag verbatim; resolving it is assetindex's job", got)
	}

	// And the run it drives keys its state on the absolute path. served is stubbed
	// above, so this asks assetindex directly, the way serve would.
	ix, err := assetindex.Build(assetindex.Options{Root: "lib", CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realParent, "lib"); ix.Root != want {
		t.Errorf("index root = %q, want the working directory resolution %q", ix.Root, want)
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

// usage() is hand-written prose describing a flag set it has no connection to, and the
// two had already drifted: -version was registered for a release and never mentioned.
// An undocumented flag is one nobody finds; the help text is the only place they are
// listed, since the flag package's own dump is silenced.
func TestHelpDocumentsEveryFlag(t *testing.T) {
	var help strings.Builder
	usageTo(&help)
	var names []string
	newFlagSet().set.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	if len(names) == 0 {
		t.Fatal("no flags found on the flag set; this guard has stopped checking anything")
	}
	for _, n := range names {
		if !strings.Contains(help.String(), "-"+n+" ") && !strings.Contains(help.String(), "-"+n+"\n") {
			t.Errorf("help text does not document -%s", n)
		}
	}
	// Naming the flag is not the whole of documenting it. The help text is the only
	// place a user sees these — the flag package's own dump is silenced — so a default
	// written out beside a flag has to be the default, and a copy of one drifts with
	// nothing failing. Read off the flag set rather than restated, so a flag that grows
	// a default is covered the day it does.
	var withDefaults int
	newFlagSet().set.VisitAll(func(f *flag.Flag) {
		if f.DefValue == "" || f.DefValue == "false" {
			return
		}
		withDefaults++
		if !strings.Contains(help.String(), f.DefValue) {
			t.Errorf("help text does not carry -%s's actual default %q", f.Name, f.DefValue)
		}
	})
	if withDefaults == 0 {
		t.Fatal("no flag on the set carries a default; this half of the guard has stopped checking anything")
	}
}

// help and version answer and exit, and they used to do so before anything looked at
// what else was on the command line. "quarry version 1.2.3" — the fumble for "quarry
// update 1.2.3" — printed the installed version and exited 0, so a script chaining on
// && read it as the version having been checked, while the bare "quarry 1.2.3" it is a
// slip of has always been an error.
func TestASubcommandThatTakesNoArgumentsRefusesOne(t *testing.T) {
	for _, cmd := range []string{"version", "help"} {
		t.Run(cmd, func(t *testing.T) {
			err := run([]string{cmd, "1.2.3"})
			if err == nil {
				t.Fatalf("quarry %s 1.2.3 succeeded; a stray positional is an error everywhere else", cmd)
			}
			if !strings.Contains(err.Error(), "1.2.3") {
				t.Errorf("error %q does not name the argument it refused", err)
			}
		})
	}
	// And the two still work with nothing after them.
	for _, cmd := range []string{"version", "help"} {
		if err := run([]string{cmd}); err != nil {
			t.Errorf("quarry %s = %v, want success", cmd, err)
		}
	}
	// update keeps its one argument.
	if err := run([]string{"update", "1", "2"}); err == nil {
		t.Error("quarry update 1 2 succeeded; update takes at most one version")
	}
}
