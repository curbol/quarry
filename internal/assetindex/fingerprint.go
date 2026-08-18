package assetindex

import (
	"hash/crc32"
	"io"
	"os"
	"strconv"
)

// Content fingerprints give an asset a stable identity for tagging that survives a
// resync and travels across machines (see Asset.Fingerprint). Byte-identical files
// share one, so a tag set on a file bundled across packs applies to every copy.
//
// Zip and loose files use CRC32 of the content plus the byte size: the CRC is read
// for free from a zip's central directory and computed in one pass for a loose
// file. Unity-package entries use the package's stable GUID (free during
// enumeration, and preserved by Unity across re-exports). CRC32 is not
// cryptographic; paired with the exact size a collision between two genuinely
// distinct files in one library is negligible, and the only cost of one would be a
// spurious shared tag.

// crcFingerprint builds the print for a known CRC. A zero CRC over non-empty bytes
// is not a fingerprint but the absence of one — some writers leave the field unset
// in the central directory — so it degrades to "" (untaggable) rather than to a
// constant every such entry of the same size would share, and silently tag together.
// An empty file's CRC is genuinely zero, and its size says so.
func crcFingerprint(crc uint32, size int64) string {
	if crc == 0 && size > 0 {
		return ""
	}
	return "crc32:" + strconv.FormatUint(uint64(crc), 16) + ":" + strconv.FormatInt(size, 10)
}

func unityFingerprint(guid string) string {
	return "uguid:" + guid
}

// looseFingerprint reads a loose file once to CRC32 its bytes. A read failure is
// returned rather than swallowed: an asset with no fingerprint is silently
// untaggable and unlinkable, and the caller records the file as a skip instead of
// indexing a card that cannot be tagged and would not open either.
func looseFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	return crcFingerprint(h.Sum32(), n), nil
}
