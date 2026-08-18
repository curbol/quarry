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
	return nil
}

// Stream copies src into dst, creating or truncating it with perm. The final Close
// is checked rather than deferred, for the same reason Atomic checks its own: a
// flush failure would leave a silently truncated file that later reads treat as the
// real thing.
func Stream(dst string, src io.Reader, perm os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
