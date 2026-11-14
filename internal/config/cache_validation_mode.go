package config

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// CacheValidationMode selects how a locked remote checkout is validated.
type CacheValidationMode uint8

const (
	// CacheValidationFull checks both the locked commit and worktree contents.
	CacheValidationFull CacheValidationMode = iota
	// CacheValidationQuick checks the locked commit without scanning the worktree.
	CacheValidationQuick
)

func quickCacheValidation(modes []CacheValidationMode) bool {
	return len(modes) == 1 && modes[0] == CacheValidationQuick
}

// ValidatePresentRemoteCaches fully checks installed entries in packs.lock
// without composing the city. Import inspection and repair must also work for
// incomplete configurations and locks whose checkouts have not been installed.
func ValidatePresentRemoteCaches(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "packs.lock"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading packs.lock: %w", err)
	}
	var lock remoteImportLockfile
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return fmt.Errorf("parsing packs.lock: %w", err)
	}
	cacheRoot, err := GlobalRepoCacheRoot()
	if err != nil {
		return err
	}
	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		commit := lock.Packs[source].Commit
		if commit == "" {
			continue
		}
		cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, commit))
		if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
			continue
		}
		if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit, false); err != nil {
			return err
		}
	}
	return nil
}

// readCacheHEAD handles ordinary loose and packed Git refs without starting
// Git. Unsupported metadata is left to rev-parse, including reftable storage,
// linked worktrees, missing refs and malformed or cyclic symbolic refs.
func readCacheHEAD(cacheDir string) (string, bool) {
	gitDir := filepath.Join(cacheDir, ".git")
	ref := "HEAD"
	for range 8 {
		data, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref)))
		if err != nil {
			if ref == "HEAD" || !os.IsNotExist(err) {
				return "", false
			}
			return readPackedCacheRef(gitDir, ref)
		}
		value := strings.TrimSpace(string(data))
		if validCacheObjectID(value) {
			return value, true
		}
		var ok bool
		ref, ok = strings.CutPrefix(value, "ref: ")
		if !ok || !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\\\x00\r\n\t ") || filepath.ToSlash(filepath.Clean(ref)) != ref || strings.Contains(ref, "..") {
			return "", false
		}
	}
	return "", false
}

func readPackedCacheRef(gitDir, ref string) (string, bool) {
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck // read-only metadata
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == ref && validCacheObjectID(fields[0]) {
			return fields[0], true
		}
	}
	return "", false
}

func validCacheObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
