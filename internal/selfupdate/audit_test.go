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
	"runtime"
	"strings"
	"sync"
	"testing"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
func TestReplaceBinaryClearsAStaleAside(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "quarry")
	if err := os.WriteFile(exe, fakeBinary("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".old", fakeBinary("ANCIENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, fakeBinary("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(staged, exe); err != nil {
		t.Fatalf("replaceBinary with a stale .old: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, fakeBinary("NEW")) {
		t.Errorf("binary content = %q, want the new one", got)
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
