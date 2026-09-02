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
	"time"
)

// StaleTempAge is how old a temp file an interrupted write abandoned must be before
// the next write clears it. Anything younger could belong to another process writing
// the same file right now, and the point of the sweep is to leave one less thing
// behind rather than to race one.
const StaleTempAge = 24 * time.Hour

// Atomic writes what encode produces to path through a temp file in path's own
// directory, renamed into place. A reader therefore never sees a half-written file,
// and a failure at any point leaves the previous contents untouched. The temp file
// is removed on every failure path. tmpPattern is an os.CreateTemp pattern, and it has
// to contain a "*": sweepStaleTemps reuses it as a filepath.Glob pattern, and without
// one the glob matches the literal name while CreateTemp appends a random suffix, so
// nothing an interrupted write abandons is ever swept and the failure is invisible.
//
// The bytes are fsynced before the rename, because rename atomicity alone only
// survives a crashing process, not a crashing machine: the rename can reach the
// journal while the data blocks have not, and the file comes back zero-length. A
// TOML store truncated that way still parses, so the loss reads as "your tags are
// gone" with nothing reporting an error.
func Atomic(path, tmpPattern string, encode func(io.Writer) error) error {
	path = resolveLinks(path)
	// Swept here rather than by the caller, which knows the path it asked for but not
	// the one a symlink resolved it to — and the temp is created in the resolved
	// directory, so that is the only place an abandoned one can be.
	sweepStaleTemps(filepath.Dir(path), tmpPattern)
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

// maxLinkHops bounds the symlink chain resolveLinks will follow, so a cycle ends in a
// write to the last path seen rather than looping.
const maxLinkHops = 16

// resolveLinks rewrites path to the file a rename must actually land on. os.Rename
// replaces the path it is given rather than what that path points through, so renaming
// onto a symlink swaps the link for a regular file and leaves the real file holding its
// old contents — with nothing to report, because the write succeeded. The tag store is
// meant to be committed and shared, so being linked into a synced folder or between two
// checkouts is an ordinary setup for it, and losing that link silently is the worst
// available outcome for the one file quarry writes into a user's tree.
//
// Resolving the parent separately matters twice over: it covers a destination reached
// through a linked directory, and it keeps the temp file on the same filesystem as the
// real destination, without which the rename fails outright.
//
// This does not preserve a hard link. Both names there are the file, with no target to
// resolve, and keeping one would mean writing in place — which is the guarantee this
// package exists to provide instead.
func resolveLinks(path string) string {
	// Followed by hand rather than with EvalSymlinks, which refuses a link whose target
	// does not exist: a store linked into place and not yet saved is exactly that.
	for i := 0; i < maxLinkHops; i++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, err := os.Readlink(path)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	dir, name := filepath.Split(path)
	if dir == "" {
		return path
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(real, name)
	}
	return path
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
//
// Also unlike Atomic, the bytes are not fsynced, so this survives a crashing process
// but not a crashing machine: dst can come back short. Callers writing thousands of
// files at a time cannot afford a commit each, so a caller that needs the stronger
// guarantee has to establish it — by syncing, or by being able to tell a short file
// from a real one afterwards.
//
// perm applies only when dst is created. An existing dst keeps its own mode, so a
// caller for whom the mode matters must remove dst first or chmod it after.
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

// sweepStaleTemps removes temp files an interrupted write abandoned. Destinations
// like the tag store sit in a user's project directory, often under source control,
// where a leftover is one more thing to notice and explain; failures are ignored
// because this is tidying, not part of the write.
func sweepStaleTemps(dir, tmpPattern string) {
	matches, err := filepath.Glob(filepath.Join(dir, tmpPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() && time.Since(fi.ModTime()) > StaleTempAge {
			os.Remove(m)
		}
	}
}
