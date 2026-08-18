package assetindex

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// makeGLB builds a minimal binary glTF holding just a JSON chunk, enough to
// exercise the animation-name reader without a real mesh or buffer.
func makeGLB(t *testing.T, doc any) []byte {
	t.Helper()
	jb, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for len(jb)%4 != 0 {
		jb = append(jb, ' ')
	}
	total := 12 + 8 + len(jb)
	buf := make([]byte, 12+8)
	binary.LittleEndian.PutUint32(buf[0:], glbMagic)
	binary.LittleEndian.PutUint32(buf[4:], 2)
	binary.LittleEndian.PutUint32(buf[8:], uint32(total))
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(jb)))
	binary.LittleEndian.PutUint32(buf[16:], glbChunkJSON)
	return append(buf, jb...)
}

func writeGLB(t *testing.T, path string, animNames ...string) {
	t.Helper()
	anims := make([]map[string]any, len(animNames))
	for i, n := range animNames {
		anims[i] = map[string]any{"name": n}
	}
	doc := map[string]any{"asset": map[string]any{"version": "2.0"}, "animations": anims}
	if err := os.WriteFile(path, makeGLB(t, doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGLBAnimationNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.glb")
	writeGLB(t, p, "Walk", "Run", "Idle")
	names, err := glbAnimationNames(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "Walk" || names[1] != "Run" || names[2] != "Idle" {
		t.Errorf("names = %v, want [Walk Run Idle]", names)
	}
}

func TestGLBAnimationNamesNone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.glb")
	writeGLB(t, p)
	names, err := glbAnimationNames(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want empty", names)
	}
}

func TestGLBAnimationNamesNotGLB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notglb.bin")
	os.WriteFile(p, []byte("this is not a glb file at all"), 0o644)
	if _, err := glbAnimationNames(p); err == nil {
		t.Error("expected an error for a non-GLB file")
	}
}

// A GLB's JSON length is read from the file itself, so a truncated download or a
// corrupt header can declare far more than is there. Both have to come back as an
// error: looseAssets treats any error as "not a clip library" and keeps the file
// whole, whereas a panic or a huge allocation here would take the whole scan with it.
func TestGLBAnimationNamesRejectsBadChunkLength(t *testing.T) {
	writeWithChunkLen := func(t *testing.T, name string, chunkLen uint32, truncate bool) string {
		t.Helper()
		b := makeGLB(t, map[string]any{"animations": []map[string]any{{"name": "Walk"}, {"name": "Run"}}})
		binary.LittleEndian.PutUint32(b[12:], chunkLen)
		if truncate {
			b = b[:len(b)-4]
		}
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name     string
		chunkLen uint32
		truncate bool
	}{
		{"truncated file", 4096, true},
		{"length past the end", 1 << 20, false},
		{"length over the cap", maxGLBJSON + 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := writeWithChunkLen(t, "bad.glb", tc.chunkLen, tc.truncate)
			if _, err := glbAnimationNames(p); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}
