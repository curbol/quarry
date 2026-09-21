// Guard tests accumulated from audits. Two rules they are written to, both learned
// from guards that had quietly stopped checking anything:
//
// A guard over a value it does not own derives that value rather than restating it. A
// restated copy narrows the guard to whatever someone remembered to add to the copy,
// and it must also assert its own parsing found something, so a format change fails
// loudly rather than matching nothing.
//
// A guard over a constant straddles it: two inputs either side of it that land on
// different answers, then an assertion that the constant lies between them. Written
// well clear of it, a test pins only the direction and leaves the value free to drift.

package selfupdate

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/curbol/quarry/internal/safewrite"
)

// fakeBinary is bytes that pass the executable sniff on the running platform, so a
// test can exercise the install path without shipping a real binary.
func fakeBinary(tag string) []byte {
	var magic []byte
	switch runtime.GOOS {
	case "darwin":
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	case "windows":
		magic = []byte("MZ")
	default:
		magic = []byte("\x7fELF")
	}
	return append(magic, []byte(tag)...)
}

func zipWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The captured header travels over a channel rather than a shared variable: the
// handler runs on the server's goroutine and the assertion on the test's, and a TCP
// round trip is not a happens-before edge the race detector (or the memory model)
// recognises.
func assetServer(t *testing.T, status int, body []byte) (*httptest.Server, func() http.Header) {
	t.Helper()
	hdr := make(chan http.Header, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hdr <- r.Header.Clone():
		default:
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		select {
		case h := <-hdr:
			return h
		default:
			return nil
		}
	}
}

func installedBinaryName() string {
	if runtime.GOOS == "windows" {
		return binaryName + ".exe"
	}
	return binaryName
}

// The running binary is replaced by renaming a fully-written file into place, so a
// failure never leaves a half-written executable and nothing is left in its dir.
func TestInstallReplacesBinaryInPlace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, hdr := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), fakeBinary("NEW")))

	if err := installTo("tok", srv.URL, exe); err != nil {
		t.Fatalf("installTo: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the downloaded one", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("update left %v behind, want only the binary", names)
	}
	sent := hdr()
	if sent == nil {
		t.Fatal("the asset server was never reached")
	}
	if auth := sent.Get("Authorization"); auth != "token tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if acc := sent.Get("Accept"); acc != "application/octet-stream" {
		t.Errorf("Accept = %q", acc)
	}
}

// A release asset that is not an executable (an error page, the wrong file) must be
// refused rather than swapped over a working binary.
func TestInstallRejectsNonExecutableAsset(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), []byte("<html>not found</html>")))

	if err := installTo("tok", srv.URL, exe); err == nil {
		t.Fatal("expected the non-executable asset to be refused")
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the working binary was replaced anyway")
	}
}

func TestInstallLeavesBinaryOnFailedDownload(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := assetServer(t, http.StatusForbidden, nil)

	if err := installTo("tok", srv.URL, exe); err == nil {
		t.Fatal("expected the failed download to surface")
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the working binary was disturbed by a failed download")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("staging left %d files behind", len(entries))
	}
}

// A release archive without the expected binary must not silently install nothing.
func TestExtractBinaryRequiresTheNamedBinary(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "download.zip")
	if err := os.WriteFile(zipPath, zipWith(t, "README.md", []byte("hi")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(zipPath, dir); err == nil {
		t.Error("expected an error when the archive has no binary")
	}
}

func TestResolveTokenPrefersGithubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "primary")
	t.Setenv("GH_TOKEN", "secondary")
	if got := resolveToken(); got != "primary" {
		t.Errorf("resolveToken = %q, want GITHUB_TOKEN to win", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := resolveToken(); got != "secondary" {
		t.Errorf("resolveToken = %q, want the GH_TOKEN fallback", got)
	}
}

// The API error body is echoed to the user; the token must not travel with it.
func TestFetchReleaseErrorOmitsToken(t *testing.T) {
	// Over a channel rather than a variable: the handler runs on the server's goroutine
	// and the assertion on the test's, and a TCP round trip is not a happens-before
	// edge the detector recognises.
	auth := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case auth <- r.Header.Get("Authorization"):
		default:
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	_, err := fetchRelease("s3cr3t-token", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Errorf("token leaked into the error: %q", err)
	}
	// The listing is authenticated too, which nothing asserted: only the asset download
	// did. This repo is private, so unauthenticated the listing 404s and `quarry update`
	// answers "no releases found (no GitHub token found…)" — advice that is wrong, to a
	// user who is authenticated, on the path that is itself the recovery from a broken
	// install.
	select {
	case got := <-auth:
		if got != "token s3cr3t-token" {
			t.Errorf("release listing sent Authorization %q, want the resolved token", got)
		}
	default:
		t.Error("the release listing made no request to assert against")
	}
}

// replaceBinary has to work when the target is the image currently executing.
// Windows refuses os.Rename onto a running .exe but does allow renaming it aside,
// which is why the current binary is moved out of the way first. The observable
// contract on every platform: the new bytes land, and no staging file survives.
func TestReplaceBinaryLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "quarry" {
			t.Errorf("left %q behind", e.Name())
		}
	}
}

// A leftover .old from an interrupted update must not block the next one.
// The aside is only reached when the single rename cannot do the job — on Windows
// always, and elsewhere on a cross-device staging dir. Without forcing that, this test
// returned at the first rename on the platform CI runs, asserted the new bytes landed,
// and passed identically with the os.Remove(aside) it is named for deleted.
func TestReplaceBinaryClearsAStaleAside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	aside := exe + ".old"
	// A leftover from an update interrupted between the move aside and the install. It
	// must not block this one: os.Rename onto an existing file replaces it on POSIX but
	// fails on Windows, which is the platform that takes this path every time.
	if err := os.WriteFile(aside, fakeBinary("ANCIENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceInstallRenameFailure(t)

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary with a stale .old: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
	// No aside survives a successful install, stale or fresh. This is the removal at the
	// end of replaceBinary; the pre-clear before the move aside is Windows-only and not
	// reachable from here, because POSIX rename replaces an existing destination and so
	// clears the stale file on its own. Asserting the end state covers what this
	// platform can actually decide.
	if _, err := os.Stat(aside); err == nil {
		t.Error("a .old survived a successful update; the next one restores a two-updates-old binary over it")
	}
	// And the copy fallback this took cleans up after itself.
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Error("the copy fallback left its staging file beside the binary")
	}
}

// The rename that installs the new binary fails on a cross-device staging dir, and
// the copy fallback that covers it was the one path in the whole update no test
// reached. It has to land the new bytes and leave nothing behind, exactly as the
// rename does.
func TestReplaceBinaryFallsBackToACopy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceInstallRenameFailure(t)

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %v, want the owner execute bit: the copy installed a binary nothing can run", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "quarry" && e.Name() != "staged" {
			t.Errorf("left %q behind", e.Name())
		}
	}
}

// The install failed and the previous binary was moved aside, so a failed restore is
// the case where nothing is runnable at all. The error has to say so and name where
// the working binary is, or the next invocation is "command not found" with no lead.
func TestRestoreAsideNamesAFailedRestore(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	missing := filepath.Join(dir, "nothing-was-moved-aside")
	cause := errors.New("installing the new binary: disk full")

	err := restoreAside(missing, exe, cause)
	if !errors.Is(err, cause) {
		t.Errorf("error = %v, want it to wrap the original cause", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name %s, where the working binary still is", err, missing)
	}
}

// A restore that works reports only what actually went wrong, so the common failure
// does not read as if the install had also destroyed the installed binary.
func TestRestoreAsidePutsTheBinaryBack(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	aside := exe + ".old"
	if err := os.WriteFile(aside, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("installing the new binary: disk full")

	err := restoreAside(aside, exe, cause)
	if err != cause {
		t.Errorf("error = %v, want exactly the cause", err)
	}
	got, readErr := os.ReadFile(exe)
	if readErr != nil {
		t.Fatalf("the previous binary was not put back: %v", readErr)
	}
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Errorf("binary content = %q, want the previous one", got)
	}
}

// The gh CLI is the last tier of token resolution and the one most likely to break,
// and it was the only tier with no coverage. A stub gh on PATH exercises both the
// success path and the "gh present but not logged in" path.
func TestResolveTokenFallsBackToGhCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is POSIX-only")
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	stubDir := t.TempDir()
	writeGh := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir)

	writeGh("#!/bin/sh\necho gh-cli-token\n")
	if got := resolveToken(); got != "gh-cli-token" {
		t.Errorf("token = %q, want the gh CLI's", got)
	}

	// Not logged in: gh exits non-zero, and the caller must get "" rather than gh's
	// error text masquerading as a token.
	writeGh("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n")
	if got := resolveToken(); got != "" {
		t.Errorf("token = %q, want empty when gh fails", got)
	}

	// No gh at all.
	t.Setenv("PATH", t.TempDir())
	if got := resolveToken(); got != "" {
		t.Errorf("token = %q, want empty with no gh on PATH", got)
	}
}

// The env tiers must still win over the CLI.
func TestResolveTokenPrefersEnvOverGhCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is POSIX-only")
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte("#!/bin/sh\necho gh-cli-token\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv("GITHUB_TOKEN", "")
	if got := resolveToken(); got != "from-gh-token" {
		t.Errorf("token = %q, want GH_TOKEN to beat the CLI", got)
	}
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	if got := resolveToken(); got != "from-github-token" {
		t.Errorf("token = %q, want GITHUB_TOKEN to win outright", got)
	}
}

// A dev build has no release to compare against, so `update` has nothing to do but
// say so. Reaching the download path from one would replace a locally-built binary
// with whatever the newest release happens to be.
func TestRunRejectsADevBuild(t *testing.T) {
	for _, current := range []string{"", "dev", "  dev  "} {
		err := Run(current, "")
		if err == nil {
			t.Fatalf("Run(%q) succeeded; want a dev-build error", current)
		}
		if !strings.Contains(err.Error(), "dev build") {
			t.Errorf("Run(%q) error = %q, want it to name the dev build", current, err)
		}
	}
}

// Being current is the common case: `quarry update` on an up-to-date install has to
// stop after the version check. Everything past it locates and overwrites the running
// executable — which, under `go test`, is the test binary.
func TestRunStopsWhenAlreadyCurrent(t *testing.T) {
	for _, tc := range []struct{ name, tag, target string }{
		{"latest", "v1.2.3", ""},
		{"a requested version", "v1.2.3", "1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run resolves a token before anything else, and an unpinned environment
			// sends it to fork the real `gh` and hand a live credential to the stub
			// server below. The suite is offline; this keeps it that way.
			t.Setenv("GITHUB_TOKEN", "test-token")
			var mu sync.Mutex
			var assetHits int
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				if strings.Contains(r.URL.Path, "/assets/") {
					assetHits++
				}
				mu.Unlock()
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"quarry-1.2.3-linux-intel.zip","url":%q}]}`,
					tc.tag, "http://"+r.Host+"/assets/1")
			}))
			defer srv.Close()

			old := releasesAPIURL
			releasesAPIURL = srv.URL
			defer func() { releasesAPIURL = old }()

			// The running binary is this test's own, so a Run that does not stop here
			// would try to replace it.
			if err := Run("1.2.3", tc.target); err != nil {
				t.Fatalf("Run on an already-current install returned %v, want nil", err)
			}
			mu.Lock()
			gotHits, gotPaths := assetHits, append([]string(nil), paths...)
			mu.Unlock()
			if gotHits != 0 {
				t.Errorf("Run downloaded an asset %d time(s) despite being current", gotHits)
			}
			// Which URL was asked for is the whole difference between the two cases, and
			// nothing else asserts it: a regression that prepended "v" twice, or that
			// fetched /latest for a specific version, would pass on the tag alone.
			want := "/latest"
			if tc.target != "" {
				want = "/tags/v" + tc.target
			}
			if len(gotPaths) == 0 || gotPaths[0] != want {
				t.Errorf("release request path = %v, want %s", gotPaths, want)
			}
		})
	}
}

// The tag is compared with its "v" stripped from either side, so a release tagged
// v1.2.3 and a binary built as 1.2.3 (or v1.2.3) are the same version.
func TestRunTreatsAVPrefixAsTheSameVersion(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v0.4.0","assets":[]}`)
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	if err := Run("v0.4.0", ""); err != nil {
		t.Errorf("Run(v0.4.0) = %v, want nil (no assets are listed, so anything past the version check fails)", err)
	}
}

// The label suffix has to match on a separator. "win.zip" is a literal suffix of
// "darwin.zip", so a bare suffix test hands a Windows user the macOS build — and
// checkExecutable only sniffs a magic number every platform shares, so the wrong
// build would replace a working install, including the update needed to recover.
func TestPlatformAssetDoesNotMatchAcrossALabelBoundary(t *testing.T) {
	rel := &release{TagName: "v1.0.0", Assets: []releaseAsset{
		{Name: "quarry-1.0.0-darwin.zip", URL: "u/darwin"},
	}}
	if got, err := platformAsset(rel, "windows", "amd64"); err == nil {
		t.Errorf("windows/amd64 matched %q, whose label merely ends in the windows suffix", got)
	}

	rel.Assets = append(rel.Assets, releaseAsset{Name: "quarry-1.0.0-win.zip", URL: "u/win"})
	got, err := platformAsset(rel, "windows", "amd64")
	if err != nil {
		t.Fatalf("windows/amd64 with a real win asset present: %v", err)
	}
	if got != "u/win" {
		t.Errorf("windows/amd64 resolved to %q, want u/win", got)
	}
}

// archive/zip checks an entry's CRC only on reaching EOF, so bounding the unpack with
// a LimitReader converted an oversize entry into a silently truncated binary that
// still passed the magic-number check. The bound is enforced from the declared size
// instead, and the read runs to EOF so the CRC is actually verified.
func TestExtractRefusesAnOversizeEntryRatherThanTruncatingIt(t *testing.T) {
	orig := maxBinaryBytes
	maxBinaryBytes = 64
	t.Cleanup(func() { maxBinaryBytes = orig })

	dir := t.TempDir()
	zipPath := filepath.Join(dir, "rel.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(binaryName)
	if err != nil {
		t.Fatal(err)
	}
	// An ELF magic so a truncated write would still pass checkExecutable.
	w.Write(append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte("A"), 4096)...))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := extractBinary(zipPath, dir)
	if err == nil {
		t.Fatalf("extracted %s over the size limit instead of refusing", out)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %v, want one naming the limit", err)
	}
}

// forceInstallRenameFailure makes the install rename fail for one test, which is how
// the cross-device copy fallback is reached on a filesystem that has no such boundary.
func forceInstallRenameFailure(t *testing.T) {
	t.Helper()
	prev := installRename
	installRename = func(string, string) error { return errors.New("invalid cross-device link") }
	t.Cleanup(func() { installRename = prev })
}

// The target is interpolated into the release API's path, and Go sends a path as
// parsed rather than cleaned. Without this, `update ../../someone/repo/releases/latest`
// resolves server-side to another repository's release, fetched with this user's token
// and reported as if it were quarry's.
func TestFetchReleaseRefusesATargetThatIsNotAVersion(t *testing.T) {
	// Guarded: the append runs on the handler's goroutine and the reads below run on
	// this one, with only a TCP round trip between them — which is not a happens-before
	// edge the memory model recognises, as assetServer's own comment says.
	var mu sync.Mutex
	var asked []string
	askedSoFar := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `{"tag_name":"v9.9.9","assets":[]}`)
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	for _, target := range []string{
		"../../someone-else/repo/releases/latest",
		"latest",
		" 1.2.3",
		"1.2.3/../../other",
		"?per_page=1",
		"-1.2.3",
	} {
		if _, err := fetchRelease("", target); err == nil {
			t.Errorf("target %q was accepted; requests so far: %v", target, askedSoFar())
		}
	}
	if got := askedSoFar(); len(got) != 0 {
		t.Errorf("a rejected target still reached the network: %v", got)
	}

	// Ordinary versions, with and without the "v", still resolve.
	for _, target := range []string{"1.2.3", "v1.2.3", "0.4.0-rc.1", "1.2.3+build5"} {
		if _, err := fetchRelease("", target); err != nil {
			t.Errorf("target %q was refused: %v", target, err)
		}
	}
	if got := askedSoFar(); len(got) != 4 {
		t.Errorf("release requests = %v, want one per accepted version", got)
	}
}

// The copy fallback writes into exe+".new" and renames it on. A leftover from an
// update killed between those two steps keeps its own mode, because a copy into an
// existing file does not touch one — and a 0600 leftover lands a binary the user
// cannot execute, which takes `quarry update` with it.
func TestCopyFallbackDoesNotInheritALeftoverMode(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".new", []byte("half a binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	forceInstallRenameFailure(t)

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is mode %v; nothing can run it, including the next update", fi.Mode().Perm())
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
}

// Renaming over a running image is legal on POSIX, and doing it in one step is what
// keeps a crash from leaving no binary at all. Moving the old one aside first is a
// Windows requirement, not a general one.
func TestReplaceBinaryDoesNotMoveTheOldOneAsideOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot rename over a running image")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	var renames int
	orig := installRename
	installRename = func(from, to string) error { renames++; return orig(from, to) }
	t.Cleanup(func() { installRename = orig })

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatal(err)
	}
	if renames != 1 {
		t.Errorf("installRename called %d times, want 1", renames)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".old") || strings.HasSuffix(e.Name(), ".new") {
			t.Errorf("left %s behind", e.Name())
		}
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
	}
}

// The asset-name contract lives in three places — the release workflow's platform
// table, releaseSuffix here, and install.sh's uname mapping — and drifting them apart
// is invisible until a release ships: everything builds, every test passes, and then
// `quarry update` cannot find its asset on the platforms CI never runs. Since update is
// itself the recovery path, the tool cannot fix that for the user.
//
// Checked against the workflow as text rather than against a second hand-copy of it,
// because a hand-copy asserts the mapping against itself.
func TestReleaseSuffixMatchesTheWorkflowLabels(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)
	found := map[string]string{}
	for _, m := range entry.FindAllStringSubmatch(string(b), -1) {
		found[m[1]+"/"+m[2]] = m[3] + ".zip"
	}
	if len(found) == 0 {
		t.Fatal("no platform entries found in release.yml; this test has stopped checking anything")
	}
	for platform, want := range found {
		got, ok := releaseSuffix[platform]
		if !ok {
			t.Errorf("release.yml builds %s but releaseSuffix has no entry: `quarry update` on that platform cannot find its asset", platform)
			continue
		}
		if got != want {
			t.Errorf("%s: releaseSuffix says %q, release.yml publishes %q", platform, got, want)
		}
	}
	// Every suffix quarry will ask for has to be one the workflow actually publishes.
	// windows/arm64 is the documented exception: it runs the x86-64 build under
	// emulation, so it deliberately points at a label of its own that is published.
	published := map[string]bool{}
	for _, suffix := range found {
		published[suffix] = true
	}
	for platform, suffix := range releaseSuffix {
		if !published[suffix] {
			t.Errorf("%s asks for %q, which release.yml does not publish", platform, suffix)
		}
	}
	// The regex above is the only thing deciding what counts as a triple, and a label
	// outside its character class is skipped rather than reported — the remaining four
	// keep every assertion green while the fifth platform ships an asset `quarry
	// update` cannot name. Counting closes that: every triple in the file is one of
	// these entries, and windows/arm64 is the single documented alias with no triple of
	// its own.
	if len(found) != len(releaseSuffix)-1 {
		t.Errorf("parsed %d platform triples from release.yml but releaseSuffix holds %d entries (one alias expected): %v vs %v",
			len(found), len(releaseSuffix), found, releaseSuffix)
	}
	// The stamp check reads one published artifact by name. A label renamed in the
	// build loop leaves that step unzipping a file the release does not contain, which
	// fails the release rather than passing it — but only after the tag is pushed.
	stamp := regexp.MustCompile(`unzip [^\n]*"dist/quarry-\$\{VERSION\}-([a-z0-9-]+)\.zip"`).FindStringSubmatch(string(b))
	if stamp == nil {
		t.Fatal("no version-stamp unzip found in release.yml; the stamp check is not reading a published artifact")
	}
	if !published[stamp[1]+".zip"] {
		t.Errorf("the stamp check reads %q, which the build loop does not publish", stamp[1]+".zip")
	}

	// ci.yml keeps a fourth copy of the same list: it cross-compiles every release
	// platform on the PR, so a platform-specific compile error is found before the tag
	// rather than by a failed release that needs a new one. Nothing read it, and its own
	// comment claimed these tests did — so a platform added to release.yml and to
	// releaseSuffix passed here, failed in install.sh (which points the author at that
	// file), and left the cross-compile loop still building the original set.
	ci, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	loop := regexp.MustCompile(`for p in ([^;]+);`).FindStringSubmatch(string(ci))
	if loop == nil {
		t.Fatal("no cross-compile loop found in ci.yml; this test has stopped checking it")
	}
	built := map[string]bool{}
	for _, p := range strings.Fields(loop[1]) {
		built[p] = true
	}
	if len(built) == 0 {
		t.Fatal("ci.yml's cross-compile loop parsed to nothing")
	}
	for platform := range found {
		if !built[platform] {
			t.Errorf("release.yml publishes %s but ci.yml does not cross-compile it: a compile error there is found by a failed release, after the tag is pushed", platform)
		}
	}
	for platform := range built {
		if _, ok := found[platform]; !ok {
			t.Errorf("ci.yml cross-compiles %s, which release.yml does not publish", platform)
		}
	}
}

// install.sh is the only install path, and the only recovery when `quarry update`
// refuses a dev build. It documents its own rule — `err "..."` alone exits 0, because
// the status is printf's, and the documented invocation pipes this into bash where a
// caller chaining on `&&` reads that as a clean install — and nothing enforced it.
// CI runs `bash -n`, which is a parse and blind to exit status.
func TestEveryFatalPathInTheInstallScriptExits(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	// A call, whatever quoting it uses. Matching the literal `err "` counted only
	// double-quoted ones, so `err 'unsupported arch'` or `err $msg` was a fatal path the
	// guard never saw. The leading class keeps it from firing on a word ending in "err".
	call := regexp.MustCompile(`(^|[;&|{(\s])err[ \t]`)
	// The exit has to be this err's own next statement, not merely somewhere nearby:
	// either on the same line after a separator, or as the whole of the next one.
	// Accepting an `exit 1` anywhere on the following line let an unrelated
	// `bar || exit 1` vouch for an err that falls through to it and carries on.
	sameLine := regexp.MustCompile(`(;|&&|\|\|)\s*(exit|return)\s+[1-9]`)
	ownLine := regexp.MustCompile(`^(exit|return)\s+[1-9]\b`)
	var calls int
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		// A comment, or the helper's own definition. The script explains this very rule
		// in prose, quoting `|| err ...`, so a guard that reads comments reports it.
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "err()") {
			continue
		}
		if !call.MatchString(ln) {
			continue
		}
		calls++
		if sameLine.MatchString(ln) {
			continue
		}
		var next string
		for j := i + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" && !strings.HasPrefix(t, "#") {
				next = t
				break
			}
		}
		if ownLine.MatchString(next) {
			continue
		}
		t.Errorf("install.sh:%d reports an error and its next statement is not a non-zero exit, so `curl | bash && ...` reads a failed install as a clean one:\n  %s\n  next: %s", i+1, trimmed, next)
	}
	if calls == 0 {
		t.Fatal("no err call sites found in install.sh; this guard has stopped checking anything")
	}
	// The script is almost all fatal paths; far fewer than this and the call regexp has
	// stopped matching the style install.sh is actually written in.
	if calls < 8 {
		t.Errorf("matched only %d err call sites in install.sh; the call regexp has drifted from the script", calls)
	}
}

// install.sh composes the same label from uname, so it drifts the same way and breaks
// the first install rather than the first update.
func TestInstallScriptComposesPublishedLabels(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	oses := regexp.MustCompile(`os="([a-z0-9]+)"`).FindAllStringSubmatch(src, -1)
	arches := regexp.MustCompile(`arch="([a-z0-9]+)"`).FindAllStringSubmatch(src, -1)
	if len(oses) == 0 || len(arches) == 0 {
		t.Fatal("install.sh's platform mapping did not parse; this test has stopped checking anything")
	}
	published := map[string]bool{}
	for _, suffix := range releaseSuffix {
		published[suffix] = true
	}
	// The script builds "${os}-${arch}"; not every combination is real (mac-arm64,
	// linux-apple), so this asserts that each one it *can* produce is either published
	// or an impossible pairing, and that at least the real ones are covered.
	//
	// Derived, never restated. Both loops below are gated on this set, so a hand-written
	// copy narrows what they check to the platforms someone remembered to add to it: a
	// new triple in release.yml with a matching releaseSuffix entry passed both guards
	// while install.sh could not compose its label at all, which is the one failure this
	// test exists for. Windows is excluded because install.sh sends it to the release
	// zip directly rather than composing a label.
	real := map[string]bool{}
	for platform, suffix := range releaseSuffix {
		if strings.HasPrefix(platform, "windows/") {
			continue
		}
		real[strings.TrimSuffix(suffix, ".zip")] = true
	}
	if len(real) == 0 {
		t.Fatal("no non-Windows platform in releaseSuffix; this test has stopped checking anything")
	}
	composable := map[string]bool{}
	for _, o := range oses {
		for _, a := range arches {
			label := o[1] + "-" + a[1]
			composable[label] = true
			if !real[label] {
				continue
			}
			if !published[label+".zip"] {
				t.Errorf("install.sh can ask for %s.zip, which is not a label the release publishes", label)
			}
		}
	}
	for label := range real {
		// Both directions, or the script is only ever narrowing the set this checks and
		// is never itself checked: renaming an arch here alone made every label it
		// composes unreal, so the loop above skipped them all and this one still passed
		// against releaseSuffix — while the next first install on that platform failed.
		if !composable[label] {
			t.Errorf("install.sh can no longer compose %s from its uname mapping, so a platform "+
				"the release publishes has no installer path", label)
		}
		if !published[label+".zip"] {
			t.Errorf("%s.zip is a platform install.sh supports but the release does not publish", label)
		}
	}
	// The asset *name* is a contract too. release.yml composes it, install.sh composes
	// it again, and platformAsset matches on "-"+suffix; changing the separator in one
	// place breaks install and update with nothing failing until a release ships.
	wf, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ file, pattern, in string }{
		{"release.yml", `quarry-\$\{VERSION\}-\$\{label\}\.zip`, string(wf)},
		{"install.sh", `\$\{BINARY_NAME\}-\$\{VERSION\}-\$\{PLATFORM\}\.zip`, src},
	} {
		if !regexp.MustCompile(want.pattern).MatchString(want.in) {
			t.Errorf("%s no longer composes the asset name as <name>-<version>-<label>.zip; "+
				"platformAsset matches on \"-\"+suffix and would stop finding it", want.file)
		}
	}
	// So is the name of the binary *inside* the zip. extractBinary looks it up by exact
	// name and install.sh unpacks to the same one, so renaming the entry in release.yml
	// alone breaks `quarry update` for every existing user and a fresh install at the
	// same moment — with nothing failing until a release has already shipped, and both
	// recovery paths gone together.
	for _, want := range []struct{ file, literal, in string }{
		{"release.yml", `bin="` + binaryName + `"`, string(wf)},
		{"release.yml", `bin="` + binaryName + `.exe"`, string(wf)},
		{"install.sh", `BINARY_NAME="` + binaryName + `"`, src},
	} {
		if !strings.Contains(want.in, want.literal) {
			t.Errorf("%s no longer names the archive entry %q (looked for %s); extractBinary and "+
				"install.sh both find the binary by that exact name", want.file, binaryName, want.literal)
		}
	}
}

// The whole of Run, end to end: fetch, version compare, platform match, download,
// symlink resolution, replacement. Nothing drove it past the version check before,
// because os.Executable answers for the test binary — so every one of those joins
// existed only in production, on the path whose failure leaves a user with no working
// binary and no working `update` to recover with.
func TestRunReplacesTheBinaryItIsPointedAt(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	dir := t.TempDir()
	exe := filepath.Join(dir, installedBinaryName())
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reached through a symlink, which is how a ~/.local/bin install commonly sits:
	// Run must replace the real file, not turn the link into a regular one.
	link := filepath.Join(dir, "link-to-quarry")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	defer swapExecutable(t, func() (string, error) { return link, nil })()

	asset := zipWith(t, installedBinaryName(), fakeBinary("NEW"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/assets/") {
			w.Write(asset)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"quarry-9.9.9-%s","url":%q}]}`,
			releaseSuffix[runtime.GOOS+"/"+runtime.GOARCH], "http://"+r.Host+"/assets/1")
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	if err := Run("1.2.3", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("the real binary was not replaced: %q", got)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file; the real install is now stale")
	}
}

// A download that fails after the version check leaves the old binary in place — the
// state a user can still run, and still run `update` from.
func TestRunLeavesAWorkingBinaryWhenTheDownloadFails(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	dir := t.TempDir()
	exe := filepath.Join(dir, installedBinaryName())
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer swapExecutable(t, func() (string, error) { return exe, nil })()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/assets/") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"quarry-9.9.9-%s","url":%q}]}`,
			releaseSuffix[runtime.GOOS+"/"+runtime.GOARCH], "http://"+r.Host+"/assets/1")
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	if err := Run("1.2.3", ""); err == nil {
		t.Fatal("Run over a failed download returned nil")
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("the binary is gone after a failed update: %v", err)
	}
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Errorf("binary = %q, want the old one left intact", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a failed update left %d entries in the install dir, want only the binary", len(entries))
	}
}

// A platform the release does not build for must fail before anything is replaced, and
// say what it looked for — a wrong-architecture binary passes checkExecutable, since
// every architecture shares the ELF magic.
func TestRunRefusesAPlatformTheReleaseDoesNotBuild(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	dir := t.TempDir()
	exe := filepath.Join(dir, installedBinaryName())
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer swapExecutable(t, func() (string, error) { return exe, nil })()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"quarry-9.9.9-solaris-sparc.zip","url":%q}]}`,
			"http://"+r.Host+"/assets/1")
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	err := Run("1.2.3", "")
	if err == nil {
		t.Fatal("Run with no matching asset returned nil")
	}
	if !strings.Contains(err.Error(), releaseSuffix[runtime.GOOS+"/"+runtime.GOARCH]) {
		t.Errorf("error %q does not name the asset it looked for", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || !bytes.Equal(got, fakeBinary("OLD")) {
		t.Errorf("the binary was disturbed by a failure that happened before any download")
	}
}

// swapExecutable points the update path at a chosen binary and returns the restore.
func swapExecutable(t *testing.T, fn func() (string, error)) func() {
	t.Helper()
	old := currentExecutable
	currentExecutable = fn
	return func() { currentExecutable = old }
}

// Nothing installs a signal handler on the update path, so Ctrl-C during a download kills
// the process before installTo's defer and strands its staging directory beside the
// binary — one more per interrupted attempt, forever, since nothing else looks there.
//
// Driven inside a directory whose name holds glob metacharacters, because the sweep runs
// wherever the user put the binary and that name is not quarry's to choose. Joined into a
// glob pattern, as this once was, the directory was itself read as one: an unterminated
// "[" returns ErrBadPattern, which the sweep swallows, so it did nothing for the life of
// the process with no error anywhere, and a "*" in the path reached across sibling
// installs to delete staging dirs belonging to another copy.
func TestAnAbandonedStagingDirectoryIsSweptByAge(t *testing.T) {
	// The prefix comes from the constant the unpack creates with, so the sweep and the
	// unpack cannot drift apart behind this test.
	dir := filepath.Join(t.TempDir(), "tools [old]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, stagingPrefix+"oldone")
	fresh := filepath.Join(dir, stagingPrefix+"running")
	mine := filepath.Join(dir, "my-notes")
	// install.sh stages here too, under its own prefix, and a first install killed
	// outright leaves one behind with the release zip in it. The script clears them
	// only at the start of a later run, and after a first install succeeds there is no
	// reason to run it again — so this is the sweep that has to reach them.
	install := filepath.Join(dir, installStagingPrefix+"abandoned")
	for _, d := range []string{old, fresh, mine, install} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file carrying the prefix is not a staging directory, and the sweep removes trees.
	notADir := filepath.Join(dir, stagingPrefix+"notes.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-safewrite.StaleTempAge - time.Hour)
	for _, p := range []string{old, notADir, install} {
		if err := os.Chtimes(p, aged, aged); err != nil {
			t.Fatal(err)
		}
	}

	sweepStaleStaging(dir)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the abandoned staging dir survived: %v", err)
	}
	// Anything younger could be another update in flight, and racing one is worse than
	// leaving it.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a staging dir young enough to be someone's running update was removed: %v", err)
	}
	// The sweep runs in a directory the user owns — often ~/.local/bin.
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the sweep removed something that is not quarry's: %v", err)
	}
	if _, err := os.Stat(notADir); err != nil {
		t.Errorf("the sweep removed a file rather than a staging directory: %v", err)
	}
	if _, err := os.Stat(install); !os.IsNotExist(err) {
		t.Errorf("an install.sh staging dir abandoned in the binary's own directory survived: %v", err)
	}
}

// The prefix install.sh stages under is spelled once here and once in the script, in
// two languages, and the sweep above is the only thing that clears what the script
// abandons. Renamed on either side they drift in silence: the install keeps working,
// and its leftovers accumulate in the directory the binary lives in forever.
func TestTheInstallStagingPrefixMatchesTheScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// mktemp's template, which is what names the directory.
	m := regexp.MustCompile(`mktemp\s+-d\s+"\$\{INSTALL_DIR\}/([.\w-]*?)X+"`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("install.sh's staging template did not parse; this guard has stopped checking anything")
	}
	if m[1] != installStagingPrefix {
		t.Errorf("install.sh stages under %q, selfupdate sweeps %q", m[1], installStagingPrefix)
	}
	// And the script's own sweep looks for what it creates.
	if !strings.Contains(string(b), "'"+installStagingPrefix+"*'") {
		t.Errorf("install.sh does not sweep %q itself; the two sweeps have to look for the same name", installStagingPrefix)
	}
}

// The name the sweep looks for has to be the name the unpack writes. Spelled twice they
// drift silently: the unpack keeps working and the sweep quietly stops matching anything.
func TestTheStagingSweepLooksForWhatTheUnpackCreates(t *testing.T) {
	src, err := os.ReadFile("selfupdate.go")
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile(`"\.quarry-update-[^"]*"`).FindAllString(string(src), -1)
	if len(lit) != 1 {
		t.Errorf("found %d spellings of the staging prefix in selfupdate.go (%v), want exactly one — the stagingPrefix constant", len(lit), lit)
	}
	if !strings.HasPrefix(stagingPrefix, ".") {
		t.Errorf("stagingPrefix = %q; a staging dir beside the binary has to be hidden, or PATH lookup finds what is inside it", stagingPrefix)
	}
}

// The repository is private, so for most first-time users a missing token is *the*
// failure, and this hint is the only thing that tells them so: a bare 404 reads as "no
// releases exist" for a repo they cannot see. It is the message most likely to be the
// one a real user meets, and nothing asserted it — the token-leak test above covers the
// 401 path only, which is what a *wrong* token gets.
func TestMissingTokenIsNamedOnANotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	old := releasesAPIURL
	releasesAPIURL = srv.URL
	defer func() { releasesAPIURL = old }()

	for _, tc := range []struct{ name, target, want string }{
		{"latest", "", "no releases found"},
		{"a specific version", "1.2.3", "version 1.2.3 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchRelease("", tc.target)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "gh auth login") {
				t.Errorf("error %q does not name what is missing; a private repo answers 404 and reads as having no releases", err)
			}
			// With a token, the same 404 is a real miss and the hint would be wrong.
			if _, err := fetchRelease("a-token", tc.target); err == nil {
				t.Fatal("expected an error")
			} else if strings.Contains(err.Error(), "GITHUB_TOKEN") {
				t.Errorf("the missing-token hint is given to a caller that had one: %q", err)
			}
		})
	}
}

// An empty asset takes a different branch of checkExecutable than a wrong-content one,
// with its own message, and only the wrong-content branch was covered. Empty is the
// shape a truncated download or a zero-length release asset arrives in, and "downloaded
// binary is empty" is what tells the user that rather than sending them looking at
// their platform.
func TestInstallRejectsAnEmptyAsset(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := assetServer(t, http.StatusOK, zipWith(t, installedBinaryName(), nil))

	err := installTo("tok", srv.URL, exe)
	if err == nil {
		t.Fatal("an empty asset replaced the working binary")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not say the asset was empty", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the working binary was replaced anyway")
	}
}

// The asset URL is the second request, and a token that can read a private repo's
// metadata but not its release assets fails there rather than at the listing. The hint
// is worded for exactly that, and only fetchRelease's copy of it had a test — so the
// one a real first-time user is most likely to meet was the untested one.
func TestMissingTokenIsNamedOnANotFoundAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out.zip")

	err := download("", srv.URL+"/assets/1", dst)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("error %q does not name what is missing", err)
	}
	// With a token in hand the same 404 is a real miss, and the hint would send the
	// user to fix something that is not broken.
	if err := download("a-token", srv.URL+"/assets/1", dst); err == nil {
		t.Fatal("expected an error")
	} else if strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("the missing-token hint is given to a caller that had one: %q", err)
	}
	// And nothing is left behind under the name the caller asked for, or the next step
	// would unzip a saved error page.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed download left a file at %s (%v)", dst, err)
	}
}

// download bounds what it writes beside the running binary, because a response that
// never ends would otherwise fill the user's disk. Truncating is safe here and not in
// extractBinary, and the reason is load-bearing: what lands here is a zip, and one cut
// short has no end-of-central-directory record, so zip.OpenReader refuses it and
// nothing reaches the binary. extractBinary's own bound is tested; this one was not,
// and neither was the claim the difference rests on.
func TestAnOversizeDownloadIsCutAndRefusedAsAnArchive(t *testing.T) {
	old := maxBinaryBytes
	maxBinaryBytes = 64
	defer func() { maxBinaryBytes = old }()

	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	w, err := zw.Create("quarry")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(fakeBinary(strings.Repeat("PADDING", 200)))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if zipped.Len() <= int(maxBinaryBytes) {
		t.Fatal("the fixture archive fits inside the bound; this test is asserting nothing")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipped.Bytes())
	}))
	defer srv.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "download.zip")
	if err := download("", srv.URL, dst); err != nil {
		t.Fatalf("download of an oversize body should truncate, not fail: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != maxBinaryBytes {
		t.Errorf("wrote %d bytes, want the bound %d", fi.Size(), maxBinaryBytes)
	}
	// The part that matters: the truncated archive is unusable, so no binary comes out
	// of it and no working install is replaced by a short one.
	if _, err := extractBinary(dst, dir); err == nil {
		t.Error("a truncated archive yielded a binary; the whole reason truncating is safe here is that it cannot")
	}
}

// The likeliest real `quarry update` failure is a binary in a directory the invoking
// user cannot write — /usr/local/bin, a system package dir, a read-only mount. A bare
// mkdir error says nothing about quarry needing to stage an update there, and this is
// the one message that does.
func TestAnUnwritableInstallDirIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory modes are not enforced")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	err := installTo("", "http://127.0.0.1:1/never-reached", exe)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), exe) || !strings.Contains(err.Error(), "writable") {
		t.Errorf("error %q neither names the binary nor says the directory has to be writable", err)
	}
	// And the binary that is there still runs.
	got, readErr := os.ReadFile(exe)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, fakeBinary("OLD")) {
		t.Error("the old binary was disturbed by an update that never started")
	}
}
