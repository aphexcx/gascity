package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const repoCacheLockName = ".packman-cache.lock"

// ErrRepoCacheBusy reports that another process holds a conflicting repo-cache
// lock. Only a non-blocking acquisition returns it; the rest wait instead.
var ErrRepoCacheBusy = errors.New("repo cache is busy")

// repoCacheLockOptions selects which repo-cache lock an acquisition takes and
// how it behaves under contention.
type repoCacheLockOptions struct {
	mode        int
	createRoot  bool
	nonBlocking bool
}

// WithRepoCacheReadLock runs fn while holding the shared repo-cache lock if
// the cache root exists. It does not create cache files or directories.
func WithRepoCacheReadLock(root string, fn func() error) error {
	return withRepoCacheLock(root, repoCacheLockOptions{mode: repoCacheLockShared}, fn)
}

// TryWithRepoCacheReadLock runs fn while holding the shared repo-cache lock,
// returning ErrRepoCacheBusy immediately instead of waiting when another
// process holds the lock exclusively.
//
// Use this only where a missed result is acceptable and a stall is not, such
// as command discovery or shell completion: a single cache write holds the
// exclusive lock for as long as its network clone takes, and every blocking
// reader on the machine queues behind it. Callers whose answer must be
// correct — installers, prune, bootstrap, skills materialization — keep using
// WithRepoCacheReadLock and wait.
func TryWithRepoCacheReadLock(root string, fn func() error) error {
	return withRepoCacheLock(root, repoCacheLockOptions{mode: repoCacheLockShared, nonBlocking: true}, fn)
}

// WithRepoCacheReadLockContext waits for a shared cache lock within the caller's
// deadline. The callback must also honor ctx while it holds the lock.
func WithRepoCacheReadLockContext(ctx context.Context, root string, fn func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := TryWithRepoCacheReadLock(root, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		})
		if !errors.Is(err, ErrRepoCacheBusy) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// WithRepoCacheWriteLock runs fn while holding the exclusive repo-cache lock,
// waiting for whoever holds it. Callers whose answer must be correct —
// installers, prune, bootstrap — want this.
func WithRepoCacheWriteLock(root string, fn func() (string, error)) (string, error) {
	return withRepoCacheWriteLock(root, false, fn)
}

// withRepoCacheWriteLock runs fn under the exclusive repo-cache lock. With
// nonBlocking it returns ErrRepoCacheBusy immediately rather than waiting for
// a writer that may be doing a network clone: cache repair reached from an
// advisory load is better skipped than waited on, and the next authoritative
// command repairs it.
func withRepoCacheWriteLock(root string, nonBlocking bool, fn func() (string, error)) (string, error) {
	var result string
	err := withRepoCacheLock(root, repoCacheLockOptions{
		mode:        repoCacheLockExclusive,
		createRoot:  true,
		nonBlocking: nonBlocking,
	}, func() error {
		var fnErr error
		result, fnErr = fn()
		return fnErr
	})
	return result, err
}

func withRepoCacheReadLockForPath(path string, nonBlocking bool, fn func() error) error {
	root, ok := repoCacheRootForPath(path)
	if !ok {
		return fn()
	}
	if nonBlocking {
		return TryWithRepoCacheReadLock(root, fn)
	}
	return WithRepoCacheReadLock(root, fn)
}

// IsRepoCachePath reports whether path visits a known repository cache root.
// It compares directory file identity at every step of symlink resolution;
// lexical containment also counts, so spelling alone cannot rule a path out.
// An empty path denotes the working directory, like other relative paths.
// Missing paths use their existing ancestors and visited steps. The root itself
// is included. Other unfinished resolutions count as cached, so callers load
// configuration conservatively.
func IsRepoCachePath(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	walk := &repoCachePathFS{}
	for _, root := range repoCacheRootCandidates() {
		if pathWithinDir(abs, root) {
			return true
		}
		info, err := os.Stat(root)
		if err != nil && !os.IsNotExist(err) {
			return true
		}
		if info != nil && info.IsDir() {
			walk.roots = append(walk.roots, info)
		}
	}
	// ResolveSymlinks starts below the volume root; visit that ancestor too.
	if _, err := walk.Lstat(filepath.VolumeName(abs) + string(os.PathSeparator)); err != nil {
		return true
	}
	_, err = fsys.ResolveSymlinks(walk, abs)
	return walk.cached || (err != nil && !walk.missing)
}

type repoCachePathFS struct {
	fsys.OSFS
	roots   []os.FileInfo
	cached  bool
	missing bool
}

// Lstat observes the resolver's physical ancestors without following links early.
func (f *repoCachePathFS) Lstat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		f.missing = true
		// Stop at the first missing component, before any later parent segments.
		return nil, errors.New("cache path component does not exist")
	}
	for _, root := range f.roots {
		f.cached = f.cached || os.SameFile(info, root)
	}
	return info, err
}

func repoCacheRootForPath(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, root := range repoCacheRootCandidates() {
		if pathWithinDir(abs, root) {
			return root, true
		}
	}
	return "", false
}

func repoCacheRootCandidates() []string {
	var roots []string
	add := func(root string) {
		if root == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = root
		}
		for _, existing := range roots {
			if existing == abs {
				return
			}
		}
		roots = append(roots, abs)
	}
	if gcHome := ImplicitGCHome(); gcHome != "" {
		add(filepath.Join(gcHome, "cache", "repos"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".gc", "cache", "repos"))
	}
	return roots
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
