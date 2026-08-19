// Package safewrite writes files without ever leaving a partial one behind. Both
// helpers exist because a truncated write here is not a lost byte but a lost
// artifact: a half-written tag store is the user's only hand-made data, and a
// half-written binary or cache entry is indistinguishable from a complete one on the
// next read.
package safewrite

import (
	"io"
	"os"
	"path/filepath"
)

// Atomic writes what encode produces to path through a temp file in path's own
// directory, renamed into place. A reader therefore never sees a half-written file,
// and a failure at any point leaves the previous contents untouched. The temp file
// is removed on every failure path. tmpPattern is an os.CreateTemp pattern.
//
// The bytes are fsynced before the rename, because rename atomicity alone only
// survives a crashing process, not a crashing machine: the rename can reach the
// journal while the data blocks have not, and the file comes back zero-length. A
// TOML store truncated that way still parses, so the loss reads as "your tags are
// gone" with nothing reporting an error.
func Atomic(path, tmpPattern string, encode func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := encode(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	// The temp file is created 0600, and the rename carries that onto the target. A
	// store the user (or a checkout) left group-readable must not silently become
	// owner-only on the first edit.
	if fi, err := os.Stat(path); err == nil {
		if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
			return fail(err)
		}
	} else if err := tmp.Chmod(0o644); err != nil {
		return fail(err)
	}
	// Checked, not deferred: a flush failure would otherwise be renamed into place as
	// if it were the whole file.
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// The rename itself needs the same durability the bytes just got: fsyncing the
	// file guarantees the data is on disk, not that the directory entry pointing at it
	// is. Without this a crash right after a successful save can come back to the
	// previous contents — the edit reported as written is simply gone.
	//
	// A directory that cannot be opened or synced is not worth failing a write that
	// already landed, so the error is dropped rather than returned: the file is in
	// place either way, and reporting failure here would send a caller into recovery
	// over a save that succeeded.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Stream copies src into dst, creating or truncating it with perm, and removes dst
// if the copy does not complete. The final Close is checked rather than deferred,
// for the same reason Atomic checks its own: a flush failure would leave a silently
// truncated file that later reads treat as the real thing.
//
// Unlike Atomic this writes in place, so an interrupted Stream takes the previous
// contents of dst with it. Use it where dst is a fresh path — a temp dir about to be
// renamed, a download staged beside its target — and Atomic where dst is a file
// something else may already be reading.
func Stream(dst string, src io.Reader, perm os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}
