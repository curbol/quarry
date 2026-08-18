package selfupdate

import (
	"strings"
	"testing"
)

// releaseAssets are the labels the release workflow builds (see
// .github/workflows/release.yml). Every one has to be reachable: CI runs on a single
// platform, so a mapping that drifts from these labels is only caught here.
func releaseAssets() *release {
	return &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "quarry-1.0.0-mac-intel.zip", URL: "u/mac-intel"},
		{Name: "quarry-1.0.0-mac-apple.zip", URL: "u/mac-apple"},
		{Name: "quarry-1.0.0-linux-intel.zip", URL: "u/linux-intel"},
		{Name: "quarry-1.0.0-linux-arm64.zip", URL: "u/linux-arm64"},
		{Name: "quarry-1.0.0-win.zip", URL: "u/win"},
	}}
}

func TestPlatformAssetPerPlatform(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "amd64", "u/mac-intel"},
		{"darwin", "arm64", "u/mac-apple"},
		{"linux", "amd64", "u/linux-intel"},
		{"linux", "arm64", "u/linux-arm64"},
		{"windows", "amd64", "u/win"},
		{"windows", "arm64", "u/win"},
	}
	for _, c := range cases {
		got, err := platformAsset(releaseAssets(), c.goos, c.goarch)
		if err != nil {
			t.Errorf("platformAsset(%s/%s): %v", c.goos, c.goarch, err)
			continue
		}
		if got != c.want {
			t.Errorf("platformAsset(%s/%s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// An architecture the release does not build must be refused, not quietly served the
// x86-64 build. checkExecutable only reads the ELF magic, which every architecture
// shares, so a wrong-arch binary passes every check and replaces a working install
// with one that cannot run — including the `update` needed to recover from it.
func TestPlatformAssetUnsupportedPlatform(t *testing.T) {
	for _, c := range []struct{ goos, goarch string }{
		{"plan9", "amd64"},
		{"linux", "arm"},     // 32-bit Pi
		{"linux", "386"},     //
		{"linux", "riscv64"}, //
		{"linux", "ppc64le"}, //
		{"darwin", "386"},
	} {
		got, err := platformAsset(releaseAssets(), c.goos, c.goarch)
		if err == nil {
			t.Errorf("platformAsset(%s/%s) = %q; want a refusal, not another platform's build", c.goos, c.goarch, got)
			continue
		}
		if !strings.Contains(err.Error(), "no release build") {
			t.Errorf("platformAsset(%s/%s) error = %v, want it to say no build exists", c.goos, c.goarch, err)
		}
	}
}

func TestPlatformAssetMissing(t *testing.T) {
	rel := &release{Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}{
		{Name: "quarry-1.0.0-solaris-sparc.zip", URL: "u/nope"},
	}}
	if _, err := platformAsset(rel, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("expected no-asset error, got %v", err)
	}
}
