package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Locked caches identify immutable commits. Keep their fingerprint inputs for
// the lifetime of that path; a relock selects a different path. The city key
// keeps mutable local sources' last successful reads separate between cities.
var fingerprintContents = struct {
	sync.Mutex
	cities map[string]*fingerprintContent
}{cities: make(map[string]*fingerprintContent)}

type fingerprintContent struct {
	mu      sync.Mutex
	config  *config.City
	roots   map[string]bool
	hashes  map[string]string
	files   map[string][]byte
	dirs    map[string]fingerprintDir
	stats   map[string]fingerprintStat
	aliases map[string]string
}

type fingerprintStat struct {
	info os.FileInfo
	err  error
}

type fingerprintDir struct {
	entries []os.DirEntry
	err     error
}

func fingerprintContentForConfig(cityPath string, cfg *config.City, stderr io.Writer) *fingerprintContent {
	fingerprintContents.Lock()
	c := fingerprintContents.cities[cityPath]
	if c == nil {
		c = &fingerprintContent{roots: make(map[string]bool), hashes: make(map[string]string), files: make(map[string][]byte), dirs: make(map[string]fingerprintDir)}
		fingerprintContents.cities[cityPath] = c
	}
	fingerprintContents.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config == cfg {
		return c
	}
	c.config = cfg
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "fingerprint sources: reading packs.lock: %v (retaining known sources)\n", err) //nolint:errcheck // best-effort diagnostic
		}
		return c
	}
	dirs := append([]string(nil), cfg.PackDirs...)
	for _, rigDirs := range cfg.RigPackDirs {
		dirs = append(dirs, rigDirs...)
	}
	for source, pack := range lock.Packs {
		root, err := packman.RepoCachePath(source, pack.Commit)
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "fingerprint sources: resolving locked cache: %v\n", err) //nolint:errcheck // best-effort diagnostic
			}
			continue
		}
		for _, dir := range dirs {
			if pathWithin(root, dir) {
				c.rememberRoot(root)
				break
			}
		}
	}
	return c
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c *fingerprintContent) rememberRoot(root string) {
	c.roots[filepath.Clean(root)] = true
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		c.roots[resolved] = true
	}
}

// identity is called with mu held. A relocked symlink selects a new identity;
// an unavailable link retains its last resolved identity for hash fallback.
func (c *fingerprintContent) identity(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if c.aliases == nil {
			c.aliases = make(map[string]string)
		}
		c.aliases[path] = resolved
		return resolved
	}
	if previous, ok := c.aliases[path]; ok {
		return previous
	}
	return path
}

// locked is called with mu held, after resolving source identity.
func (c *fingerprintContent) locked(path string) bool {
	for root := range c.roots {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func (c *fingerprintContent) hash(path, variant string, compute func() string) string {
	if c == nil {
		return compute()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.identity(path)
	key := source + "\x00" + variant
	previous := c.hashes[key]
	if previous != "" && c.locked(source) {
		return previous
	}
	value := compute()
	if value == "" {
		return previous
	}
	c.hashes[key] = value
	return value
}

func (c *fingerprintContent) hashPath(path string) string {
	return c.hash(path, "content", func() string { return runtime.HashPathContent(path) })
}

func (c *fingerprintContent) agentCatalog(path string) (materialize.AgentCatalog, error) {
	var fs fsys.FS = fsys.OSFS{}
	if c != nil {
		fs = fingerprintFS{FS: fs, content: c}
	}
	return materialize.LoadAgentCatalogFS(fs, path)
}

// fingerprintFS freezes prompt source bytes and fragment membership under
// locked imports. Templates still render against each session's current context.
type fingerprintFS struct {
	fsys.FS
	content *fingerprintContent
}

func (f fingerprintFS) Stat(path string) (os.FileInfo, error) {
	c := f.content
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.identity(path)
	if !c.locked(source) {
		return f.FS.Stat(path)
	}
	if previous, known := c.stats[source]; known {
		return previous.info, previous.err
	}
	info, err := f.FS.Stat(path)
	if err == nil || os.IsNotExist(err) {
		if c.stats == nil {
			c.stats = make(map[string]fingerprintStat)
		}
		c.stats[source] = fingerprintStat{info, err}
	}
	return info, err
}

func (f fingerprintFS) ReadFile(path string) ([]byte, error) {
	c := f.content
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.identity(path)
	previous, known := c.files[source]
	if known && c.locked(source) {
		return append([]byte(nil), previous...), nil
	}
	data, err := f.FS.ReadFile(path)
	if err != nil {
		if known {
			return append([]byte(nil), previous...), nil
		}
		return nil, err
	}
	c.files[source] = append([]byte(nil), data...)
	return data, nil
}

func (f fingerprintFS) ReadDir(path string) ([]os.DirEntry, error) {
	c := f.content
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.identity(path)
	if c.locked(source) {
		if previous, known := c.dirs[source]; known {
			return append([]os.DirEntry(nil), previous.entries...), previous.err
		}
		entries, err := f.FS.ReadDir(path)
		if err == nil || os.IsNotExist(err) {
			c.dirs[source] = fingerprintDir{append([]os.DirEntry(nil), entries...), err}
		}
		return entries, err
	}
	return f.FS.ReadDir(path)
}
