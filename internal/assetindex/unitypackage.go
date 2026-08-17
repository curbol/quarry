package assetindex

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// unityEntry accumulates the members of one GUID directory as the tar streams by.
// head holds the asset payload's leading bytes so image dimensions can be recovered
// once the pathname (and thus the extension) is known — the two members arrive in
// either order across Unity versions, so the bytes are buffered rather than decoded
// inline.
type unityEntry struct {
	pathname   string
	hasAsset   bool
	assetSize  int64
	head       []byte
	hasPreview bool
}

// splitUnityName splits a tar member name into its GUID dir and member, tolerating
// a leading "./". It returns ok=false for names that aren't a two-part
// <guid>/<member> or whose guid is unsafe.
// maxPathnameBytes bounds the `pathname` member, which holds one asset path. The
// tar comes from a downloaded archive, so it is not trusted to be small.
const maxPathnameBytes = 64 << 10

func splitUnityName(name string) (guid, member string, ok bool) {
	name = strings.TrimPrefix(name, "./")
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	guid, member = parts[0], parts[1]
	if guid == "" || guid == ".." || strings.ContainsAny(guid, `/\`) {
		return "", "", false
	}
	return guid, member, true
}

// extOf is the lower-case, dotless extension of a unity pathname.
func extOf(p string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
}

// walkUnityTar streams a .unitypackage's gzip+tar once, calling visit for each member
// whose name parses as <guid>/<member>. tr is positioned at that member's bytes and
// is only valid for the duration of the call. A visit error stops the walk. Every
// pass over a unitypackage goes through here: enumeration, extraction, and the
// Sidekick definition read all need the same setup and the same name handling.
func walkUnityTar(archivePath string, visit func(guid, member string, hdr *tar.Header, tr *tar.Reader) error) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip %s: %w", filepath.Base(archivePath), err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar %s: %w", filepath.Base(archivePath), err)
		}
		guid, member, ok := splitUnityName(hdr.Name)
		if !ok {
			continue
		}
		if err := visit(guid, member, hdr, tr); err != nil {
			return err
		}
	}
}

// unityAssets enumerates the payload-bearing entries of a .unitypackage. It streams
// the gzip+tar once, resolving each GUID's real path from its `pathname` member and
// noting an optional `preview.png`. GUIDs with no `asset` payload (Unity directory
// placeholders) are dropped so they never become phantom index rows.
func unityAssets(archivePath, displayRel, vendor, pack, variant string) ([]Asset, error) {
	entries := map[string]*unityEntry{}
	var order []string
	// Only images consume the head buffer, and the two members arrive in either order
	// across Unity versions: buffer while the extension is still unknown, and drop the
	// buffer as soon as either member proves the entry is not an image. A large pack
	// holds thousands of non-image entries, whose buffers would otherwise be retained
	// for the whole archive pass.
	err := walkUnityTar(archivePath, func(guid, member string, hdr *tar.Header, tr *tar.Reader) error {
		e := entries[guid]
		if e == nil {
			e = &unityEntry{}
			entries[guid] = e
			order = append(order, guid)
		}
		switch member {
		case "asset":
			e.hasAsset = true
			e.assetSize = hdr.Size
			if e.pathname == "" || isDimExt(extOf(e.pathname)) {
				e.head = readHead(tr)
			}
		case "pathname":
			b, err := io.ReadAll(io.LimitReader(tr, maxPathnameBytes))
			if err != nil {
				return err
			}
			e.pathname = strings.TrimSpace(firstLine(string(b)))
			if !isDimExt(extOf(e.pathname)) {
				e.head = nil
			}
		case "preview.png":
			e.hasPreview = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var assets []Asset
	for _, guid := range order {
		e := entries[guid]
		if !e.hasAsset || e.pathname == "" || !safeEntry(e.pathname) || skipEntry(e.pathname) {
			continue
		}
		src := Source{Kind: SourceUnityPackage, ArchivePath: archivePath, Guid: guid, Pathname: e.pathname, HasPreview: e.hasPreview}
		a := newAsset(src,
			path.Base(e.pathname),
			archiveRel(displayRel, e.pathname),
			vendor, pack, variant,
			e.assetSize,
			unityFingerprint(guid),
		)
		if isDimExt(a.Ext) {
			a.Width, a.Height = imageDims(e.head, a.Ext)
		}
		assets = append(assets, a)
	}
	return applySidekick(archivePath, entries, order, assets)
}

// extractUnityPackage decompresses a .unitypackage once, writing each GUID's
// `asset` and `preview.png` payloads to <destDir>/<guid>/. Metadata members
// (asset.meta, pathname) are not written. destDir is expected to be a temp dir the
// caller renames into place atomically.
func extractUnityPackage(archivePath, destDir string) error {
	return walkUnityTar(archivePath, func(guid, member string, _ *tar.Header, tr *tar.Reader) error {
		if member != "asset" && member != "preview.png" {
			return nil
		}
		dir := filepath.Join(destDir, guid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dir, member))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		// Checked, not deferred: a flush failure would leave a silently truncated
		// payload that later reads treat as the asset's real bytes.
		return out.Close()
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
