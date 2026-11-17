package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
)

const (
	remoteCacheValidationFile = "gc-validation-v1.json"
	remoteCacheValidationTTL  = time.Minute
)

var remoteCacheReloadAt atomic.Int64

type remoteCacheValidationRecord struct {
	Version     int       `json:"version"`
	Commit      string    `json:"commit"`
	Fingerprint string    `json:"fingerprint"`
	CheckedAt   time.Time `json:"checked_at"`
}

func remoteCacheValidationPath(cacheDir string) string {
	return filepath.Join(cacheDir, ".git", remoteCacheValidationFile)
}

func remoteCacheValidationFileFingerprint(cacheDir string) string {
	if info, err := os.Stat(remoteCacheValidationPath(cacheDir)); err == nil {
		return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return ""
}

func remoteCacheValidationFresh(checkedAt, now time.Time) bool {
	age := now.Sub(checkedAt)
	return age >= 0 && age < remoteCacheValidationTTL
}

// loadRemoteCacheValidation treats a missing, expired or malformed receipt as
// a cache miss. Receipts are an optimization, never a substitute for a failed
// validation. Synthetic bundled caches have no .git directory and keep their
// existing in-process memo.
func loadRemoteCacheValidation(cacheDir, commit, fingerprint string, now time.Time) (remoteCacheValidationEntry, bool) {
	if head, ok := readCacheHEAD(cacheDir); !ok || !gitutil.SameCommit(head, commit) {
		return remoteCacheValidationEntry{}, false
	}
	signature := remoteCacheValidationFileFingerprint(cacheDir)
	if signature == "" {
		return remoteCacheValidationEntry{}, false
	}
	data, err := os.ReadFile(remoteCacheValidationPath(cacheDir))
	if err != nil {
		return remoteCacheValidationEntry{}, false
	}
	var record remoteCacheValidationRecord
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Commit != commit ||
		record.CheckedAt.UnixNano() <= remoteCacheReloadAt.Load() ||
		record.Fingerprint != fingerprint || !remoteCacheValidationFresh(record.CheckedAt, now) ||
		signature != remoteCacheValidationFileFingerprint(cacheDir) || remoteCacheFingerprint(cacheDir) != fingerprint {
		return remoteCacheValidationEntry{}, false
	}
	return remoteCacheValidationEntry{
		fingerprint: fingerprint,
		checkedAt:   record.CheckedAt,
		receipt:     signature,
	}, true
}

// rememberRemoteCacheValidation runs before releasing the validation read lock.
// Otherwise an installer could invalidate the old result and then have a reader
// publish it again after the install. A manual concurrent edit is not memoized.
func rememberRemoteCacheValidation(key, cacheDir, commit, before string, checkedAt time.Time) {
	if remoteCacheFingerprint(cacheDir) != before {
		return
	}
	entry := remoteCacheValidationEntry{fingerprint: before, checkedAt: checkedAt}
	if info, err := os.Stat(filepath.Join(cacheDir, ".git")); err == nil && info.IsDir() {
		data, err := json.Marshal(remoteCacheValidationRecord{
			Version: 1, Commit: commit, Fingerprint: before, CheckedAt: checkedAt,
		})
		if err == nil {
			err = fsys.WriteFileAtomic(fsys.OSFS{}, remoteCacheValidationPath(cacheDir), data, 0o600)
		}
		if err != nil {
			// Read-only caches still work, but must validate on every load. Do
			// not memoize without a receipt: deletion on install must also
			// invalidate other processes' in-memory successes.
			log.Printf("remembering remote cache validation at %s: %v", cacheDir, err)
			return
		}
		entry.receipt = remoteCacheValidationFileFingerprint(cacheDir)
	}
	remoteCacheValidationCache.Store(key, entry)
}

// InvalidateRemoteCacheValidation removes a successful check before a managed
// checkout is installed or repaired. The caller must hold the repo-cache write
// lock. Removing the receipt invalidates other processes' in-memory entries too.
func InvalidateRemoteCacheValidation(cacheDir string) error {
	info, err := os.Stat(filepath.Join(cacheDir, ".git"))
	if gitutil.MissingCheckoutMarker(info, err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking remote cache before invalidation: %w", err)
	}
	if err := os.Remove(remoteCacheValidationPath(cacheDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("invalidating remote cache validation: %w", err)
	}
	return nil
}
