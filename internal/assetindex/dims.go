package assetindex

import (
	"bytes"
	"encoding/binary"
	"image"
	"io"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// dimsHeadBytes is how much of an image's leading bytes are read to recover its
// pixel dimensions. The header carrying width/height sits at the front of every
// format handled here (a JPEG's SOF marker can trail large embedded EXIF, which is
// why this is generous rather than a few dozen bytes).
const dimsHeadBytes = 8 << 10

// imageDims recovers an image's pixel dimensions from its leading bytes, or 0,0
// when ext isn't a raster format handled here or the header can't be parsed. png,
// jpeg and gif go through the stdlib decoders; tga, psd and bmp have no stdlib
// support (and tga has no magic number to sniff), so their fixed-offset headers are
// read directly.
func imageDims(head []byte, ext string) (int, int) {
	switch ext {
	case "png", "jpg", "jpeg", "gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
		if err != nil {
			return 0, 0
		}
		return cfg.Width, cfg.Height
	case "tga":
		if len(head) < 18 {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint16(head[12:])), int(binary.LittleEndian.Uint16(head[14:]))
	case "psd":
		if len(head) < 26 || string(head[:4]) != "8BPS" {
			return 0, 0
		}
		return int(binary.BigEndian.Uint32(head[18:])), int(binary.BigEndian.Uint32(head[14:]))
	case "bmp":
		if len(head) < 26 || string(head[:2]) != "BM" {
			return 0, 0
		}
		// A top-down BMP stores a negative height; the sign is row order, not a dimension.
		return abs32(int32(binary.LittleEndian.Uint32(head[18:]))), abs32(int32(binary.LittleEndian.Uint32(head[22:])))
	}
	return 0, 0
}

func abs32(v int32) int {
	if v < 0 {
		return int(-v)
	}
	return int(v)
}

// isDimExt reports whether an extension is a raster format imageDims can measure.
func isDimExt(ext string) bool {
	switch ext {
	case "png", "jpg", "jpeg", "gif", "tga", "psd", "bmp":
		return true
	}
	return false
}

// readHead reads up to dimsHeadBytes from r, enough to recover an image's
// dimensions without pulling a whole file into memory.
func readHead(r io.Reader) []byte {
	head, _ := io.ReadAll(io.LimitReader(r, dimsHeadBytes))
	return head
}

// setImageDims fills in an asset's pixel dimensions when its extension is one that
// can be measured, reading only the head of the bytes open yields. A file that will
// not open leaves the dimensions at zero: they are a nicety on a card, and a library
// is full of textures nothing else needs to read.
//
// The bytes are reached through open rather than a path because the two callers hold
// different things — a loose file on disk, and an entry inside an already-open zip —
// and the decision about which extensions are worth a read belongs in one place.
func setImageDims(a *Asset, open func() (io.ReadCloser, error)) {
	if !isDimExt(a.Ext) {
		return
	}
	rc, err := open()
	if err != nil {
		return
	}
	defer rc.Close()
	a.Width, a.Height = imageDims(readHead(rc), a.Ext)
}
