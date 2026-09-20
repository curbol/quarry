package selfupdate

import (
	"strings"
	"testing"
)

// releaseAssets is a release publishing exactly the assets quarry knows how to ask
// for, named the way the workflow names them. Built from releaseSuffix rather than
// typed out: that the map agrees with what the workflow actually publishes is
// TestReleaseSuffixMatchesTheWorkflowLabels's job, and a hand-written fourth copy of
// the label list here would need editing every time one is renamed. The URL is the
// label, so the assertions below can name which asset they expect without a table of
// their own.
func releaseAssets() *release {
	seen := map[string]bool{}
	r := &release{}
	for _, suffix := range releaseSuffix {
		if seen[suffix] {
			continue // windows/arm64 deliberately aliases the win build
		}
		seen[suffix] = true
		label := strings.TrimSuffix(suffix, ".zip")
		r.Assets = append(r.Assets, releaseAsset{Name: "quarry-1.0.0-" + suffix, URL: "u/" + label})
	}
	return r
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
	rel := &release{Assets: []releaseAsset{
		{Name: "quarry-1.0.0-solaris-sparc.zip", URL: "u/nope"},
	}}
	if _, err := platformAsset(rel, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("expected no-asset error, got %v", err)
	}
}
