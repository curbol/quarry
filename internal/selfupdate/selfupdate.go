// Package selfupdate implements `quarry update`: it fetches a release from the
// GitHub API, downloads the binary for the current platform, and atomically replaces
// the running executable. The repo is private, so downloads use a token resolved
// from GITHUB_TOKEN / GH_TOKEN / the gh CLI, hitting the asset API URL with an
// octet-stream Accept header (the same model as the install script).
package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/curbol/quarry/internal/safewrite"
)

const binaryName = "quarry"

// releasesAPIURL is a var so tests can point it at a stub server.
var releasesAPIURL = "https://api.github.com/repos/curbol/quarry/releases"

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

// releaseAsset is one downloadable file on a release. Named rather than anonymous so
// the platform matching over it can be exercised directly.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// DevVersion is the version a locally-built binary carries. It is exported because
// the guard below and the value main stamps in have to be the same string: if they
// drift, `update` silently starts replacing dev builds with releases.
const DevVersion = "dev"

// Run updates the binary to target (a version like "0.2.0"), or to the latest
// release when target is empty. current is the running binary's version.
func Run(current, target string) error {
	current, target = strings.TrimSpace(current), strings.TrimSpace(target)
	if current == "" || current == DevVersion {
		return fmt.Errorf("this is a dev build (version %q); `update` only works on release builds — install one with install.sh", current)
	}
	token := resolveToken()

	rel, err := fetchRelease(token, target)
	if err != nil {
		return err
	}
	relVer := strings.TrimPrefix(rel.TagName, "v")
	if relVer == strings.TrimPrefix(current, "v") {
		label := "latest"
		if target != "" {
			label = "requested"
		}
		fmt.Fprintf(os.Stderr, "already on the %s version (%s)\n", label, relVer)
		return nil
	}

	assetURL, err := platformAsset(rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := downloadAndReplace(token, assetURL); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "updated %s to version %s\n", binaryName, relVer)
	return nil
}

// resolveToken finds a GitHub token: env first, then the gh CLI. Empty is allowed
// (public assets), but this repo is private so a token is normally required.
func resolveToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newRequest(token, method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	return req, nil
}

// versionArg is what may be pasted into the release API's path. The argument is
// interpolated into a URL, and Go sends a path as parsed rather than cleaned, so
// `update ../../someone/repo/releases/latest` would resolve server-side to another
// repository's release — fetched with this user's token and reported as if it were
// quarry's. Anchoring the shape also rejects the untrimmed " 1.2.3" that would
// otherwise become the tag "v 1.2.3" and 404 with nothing explaining why.
var versionArg = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.+-]*$`)

func fetchRelease(token, target string) (*release, error) {
	url := releasesAPIURL + "/latest"
	if target != "" {
		if !versionArg.MatchString(target) {
			return nil, fmt.Errorf("%q is not a version; pass one like 1.2.3", target)
		}
		tag := target
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		url = releasesAPIURL + "/tags/" + tag
	}
	req, err := newRequest(token, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		hint := ""
		if token == "" {
			hint = " (no GitHub token found; set GITHUB_TOKEN or run `gh auth login`)"
		}
		if target != "" {
			return nil, fmt.Errorf("version %s not found%s", target, hint)
		}
		return nil, fmt.Errorf("no releases found%s", hint)
	}
	if resp.StatusCode != http.StatusOK {
		// Bounded: this body goes straight into an error the user sees, and it is
		// whatever the far end chose to send.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r release
	// Bounded like the error path above, and for the same reason: the size of this is
	// the far end's choice, and a release listing is kilobytes.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBytes)).Decode(&r); err != nil {
		return nil, fmt.Errorf("parsing release: %w", err)
	}
	return &r, nil
}

// releaseSuffix maps a platform to the asset the release workflow builds for it. The
// mapping is exhaustive rather than defaulting per OS: an architecture with no build
// (linux/arm on a Pi, linux/386, riscv64) would otherwise be handed the x86-64
// binary, and since checkExecutable only reads the ELF magic — which every
// architecture shares — the wrong build would pass every check and replace a working
// install with one that cannot run, including the `update` needed to recover.
var releaseSuffix = map[string]string{
	"darwin/amd64":  "mac-intel.zip",
	"darwin/arm64":  "mac-apple.zip",
	"linux/amd64":   "linux-intel.zip",
	"linux/arm64":   "linux-arm64.zip",
	"windows/amd64": "win.zip",
	// Windows on ARM runs x86-64 binaries under emulation, and no arm64 build ships.
	"windows/arm64": "win.zip",
}

// platformAsset returns the asset API URL for the given OS/arch, matching the label
// suffix the release workflow uses. The platform is a parameter rather than read from
// runtime so every entry is assertable: the release builds a handful and CI runs on
// one, so a mapping that drifts from the workflow's labels would otherwise surface
// only when a user on that platform ran `quarry update`.
func platformAsset(rel *release, goos, goarch string) (string, error) {
	suffix, ok := releaseSuffix[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("no release build for %s/%s", goos, goarch)
	}
	// The separator is part of the match. A bare suffix test makes "win.zip" a suffix
	// of "darwin.zip", so a Windows user could be handed the macOS build — and since
	// checkExecutable only sniffs a magic number, the wrong build would replace a
	// working install. Nothing in the current labels collides; this is what keeps that
	// from being the only reason it works.
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, "-"+suffix) {
			return a.URL, nil
		}
	}
	names := make([]string, len(rel.Assets))
	for i, a := range rel.Assets {
		names[i] = a.Name
	}
	return "", fmt.Errorf("no asset matching %s; available: %v", suffix, names)
}

// sweepStaleStaging removes staging directories abandoned by an interrupted update.
// Failures are ignored: this is tidying, not part of the update, and a directory that
// cannot be removed must not stop the one thing that repairs a broken install.
func sweepStaleStaging(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, ".quarry-update-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && time.Since(fi.ModTime()) > safewrite.StaleTempAge {
			os.RemoveAll(m)
		}
	}
}

// currentExecutable is a seam. os.Executable answers for the test binary, so without
// it nothing could drive Run past the version check — leaving the fetch, the platform
// match, the symlink resolution and the replacement joined together only in production.
var currentExecutable = os.Executable

func downloadAndReplace(token, assetURL string) error {
	exe, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("locating current binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}
	fmt.Fprintln(os.Stderr, "downloading update…")
	return installTo(token, assetURL, exe)
}

// installTo downloads the release asset and swaps its binary over exe. Every step
// happens beside exe and the target is only ever replaced by a rename, so a failure
// at any point leaves the working binary exactly as it was.
func installTo(token, assetURL, exe string) error {
	// An interrupted update leaves its staging dir behind: there is no signal handler on
	// this path, so Ctrl-C during a download kills the process before the defer below,
	// and nothing else ever clears it. Swept the way replaceBinary clears a stale aside
	// and safewrite clears an abandoned temp — by age, since anything younger could be
	// another update running right now.
	sweepStaleStaging(filepath.Dir(exe))
	// Stage next to the target binary so the final rename stays on one filesystem
	// (a temp dir under /tmp is often a separate device, and rename can't cross it).
	tmp, err := os.MkdirTemp(filepath.Dir(exe), ".quarry-update-*")
	if err != nil {
		// The usual cause is a binary living somewhere the invoking user cannot write —
		// /usr/local/bin, a system package dir, a read-only mount — and a bare mkdir
		// error says nothing about quarry needing to stage an update there.
		return fmt.Errorf("staging the update beside %s (is that directory writable?): %w", exe, err)
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "download.zip")
	if err := download(token, assetURL, zipPath); err != nil {
		return err
	}
	binPath, err := extractBinary(zipPath, tmp)
	if err != nil {
		return err
	}
	if err := checkExecutable(binPath); err != nil {
		return err
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return fmt.Errorf("making the downloaded binary executable: %w", err)
	}
	return replaceBinary(binPath, exe)
}

// executableMagic is the leading signature of a native binary per platform. The zip
// reader already verifies each entry's CRC, so this catches the other way an update
// goes wrong: a release that shipped something that is not a binary at all (an
// error page, a script, the wrong artifact) landing on top of a working install.
var executableMagic = map[string][][]byte{
	"linux":   {[]byte("\x7fELF")},
	"darwin":  {{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}},
	"windows": {[]byte("MZ")},
}

func checkExecutable(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	switch {
	case n == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF):
		return fmt.Errorf("downloaded binary is empty")
	case n == 0 && err != nil:
		return fmt.Errorf("reading the downloaded binary: %w", err)
	}
	head = head[:n]
	magics, known := executableMagic[runtime.GOOS]
	if !known {
		return nil
	}
	for _, m := range magics {
		if bytes.HasPrefix(head, m) {
			return nil
		}
	}
	return fmt.Errorf("downloaded file is not a %s executable", runtime.GOOS)
}

// installRename is the rename that puts the new binary in place. It is a variable so
// a test can reach the copy fallback below: both paths are in one directory, where a
// rename does not fail on a working filesystem, and that fallback is precisely what
// has to keep a binary in place when it does.
var installRename = os.Rename

// replaceBinary puts newPath at exe, which is the image currently executing.
// Windows refuses to rename over a running .exe but does permit renaming it aside,
// so the current binary is moved out of the way first and restored if the install
// then fails. exe is never truncated in place, on any platform.
func replaceBinary(newPath, exe string) error {
	// One rename where one is enough. Renaming over a running image is legal on POSIX
	// — it is what install.sh relies on — and moving the old one aside first opens a
	// window in which there is no quarry at all: a crash there leaves the user with
	// nothing to run and no `quarry update` to recover with.
	if runtime.GOOS != "windows" {
		if err := installRename(newPath, exe); err == nil {
			return nil
		}
	}
	aside := exe + ".old"
	os.Remove(aside) // a leftover from an interrupted update must not block this one
	if err := os.Rename(exe, aside); err != nil {
		return fmt.Errorf("moving the current binary aside: %w", err)
	}
	if err := installRename(newPath, exe); err != nil {
		// Cross-device or an exotic mount. The copy lands beside exe and is renamed
		// on, rather than written over exe directly: a copy interrupted by a signal or
		// a power cut would otherwise leave a half-written binary that looks whole.
		staged := exe + ".new"
		// Cleared first, and the mode set after: a copy writes into an existing file
		// without touching its mode, so a leftover from an update killed between the
		// copy and the rename below would carry its own mode onto exe. A 0600 one lands
		// a binary nobody can execute, including the update that would replace it.
		os.Remove(staged)
		if err := copyFile(newPath, staged); err != nil {
			os.Remove(staged)
			return restoreAside(aside, exe, fmt.Errorf("installing the new binary: %w", err))
		}
		if err := os.Chmod(staged, 0o755); err != nil {
			os.Remove(staged)
			return restoreAside(aside, exe, fmt.Errorf("installing the new binary: %w", err))
		}
		if err := os.Rename(staged, exe); err != nil {
			os.Remove(staged)
			return restoreAside(aside, exe, fmt.Errorf("installing the new binary: %w", err))
		}
	}
	// Removing the running image fails on Windows; the next update clears it.
	os.Remove(aside)
	return nil
}

// restoreAside puts the binary moved out of the way back, and reports cause. A
// restore that itself fails is the one case that leaves nothing runnable, so it is
// named along with where the working binary actually is: the next invocation is
// otherwise "command not found", with the error the user already saw saying only that
// the install failed.
func restoreAside(aside, exe string, cause error) error {
	if err := os.Rename(aside, exe); err != nil {
		return fmt.Errorf("%w; the previous binary could not be put back either (%v) and is still at %s", cause, err, aside)
	}
	return cause
}

func download(token, url, dst string) error {
	req, err := newRequest(token, http.MethodGet, url)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The same failure fetchRelease explains, arriving one request later: a 404 on an
		// asset URL of a private repo is a token without the scope to read it, and
		// "HTTP 404" on its own sends the user looking for a missing release.
		hint := ""
		if resp.StatusCode == http.StatusNotFound && token == "" {
			hint = " (no GitHub token found; set GITHUB_TOKEN or run `gh auth login`)"
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("download failed: HTTP %d%s: %s", resp.StatusCode, hint, strings.TrimSpace(string(body)))
	}
	// Bounded for the same reason the extraction below is: this writes beside the
	// running binary, and a response that never ends would fill the user's disk.
	return safewrite.Stream(dst, io.LimitReader(resp.Body, maxBinaryBytes), 0o644)
}

// maxReleaseBytes caps the release listing. It is JSON from the far end and a listing
// of a handful of releases is kilobytes.
const maxReleaseBytes = 8 << 20

// maxBinaryBytes caps what is unpacked from a release archive. quarry builds are a
// few tens of MB; this is generous enough to never bite a real one. A var so a test
// can lower it: the refusal it guards is unreachable otherwise, and a bound nothing
// exercises is a bound that silently stops working.
var maxBinaryBytes int64 = 512 << 20

func extractBinary(zipPath, dir string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()
	want := binaryName
	if runtime.GOOS == "windows" {
		want = binaryName + ".exe"
	}
	for _, f := range r.File {
		if f.Name != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		out := filepath.Join(dir, want)
		// Bounded by what a plausible build could be, because this unpacks beside the
		// running binary and an inflated entry would fill the user's disk before
		// anything noticed. Enforced from the declared size rather than by truncating
		// the read: archive/zip verifies the entry's CRC only on reaching EOF, so a
		// LimitReader that stops short returns a nil error and skips the check
		// entirely. The truncated binary still carries the right magic number, so it
		// would pass checkExecutable and replace a working install — including the
		// `update` needed to recover. With no published checksum, that CRC is the whole
		// integrity story.
		if f.UncompressedSize64 > uint64(maxBinaryBytes) {
			return "", fmt.Errorf("release binary is %d bytes, over the %d-byte limit", f.UncompressedSize64, maxBinaryBytes)
		}
		if err := safewrite.Stream(out, rc, 0o755); err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("binary %q not found in release archive", want)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	return safewrite.Stream(dst, in, info.Mode())
}
