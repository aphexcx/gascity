package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ArchiveCacheOptions selects the event types kept in a bounded, disposable
// archive cache. Dir must be dedicated to this caller. MaxBytes caps its total
// published entries; a full cache bypasses caching without losing history.
// AfterSeq filters delivery, never the cached summary. The active log is uncached.
type ArchiveCacheOptions struct {
	Dir      string
	Types    []string
	MaxBytes int64
	AfterSeq uint64
	Progress func(ArchiveCacheProgress)
}

// ArchiveCacheProgress describes the current retained-history read. Index is
// the one-based archive being visited, or Archives after the archive pass.
type ArchiveCacheProgress struct {
	AfterSeq              uint64
	Archives, Index       int
	Hits, Misses, Skipped int
	Phase                 string
}

type archiveCacheKey struct {
	Version  int
	Basename string
	Size     int64
	LastSeq  uint64
	ModTime  int64
	Types    []string
}

type archiveCacheEntry struct {
	Key    archiveCacheKey
	Events []archiveCacheRecord
	Digest string
}

// The cache is a summary for history consumers, not a copy of event payloads.
type archiveCacheRecord struct {
	Seq     uint64    `json:"seq"`
	Type    string    `json:"type"`
	Ts      time.Time `json:"ts"`
	Subject string    `json:"subject,omitempty"`
	Message string    `json:"message,omitempty"`
}

func summarizeArchiveEvent(e Event) archiveCacheRecord {
	return archiveCacheRecord{Seq: e.Seq, Type: e.Type, Ts: e.Ts, Subject: e.Subject, Message: e.Message}
}

func (r archiveCacheRecord) event() Event {
	return Event{Seq: r.Seq, Type: r.Type, Ts: r.Ts, Subject: r.Subject, Message: r.Message}
}

var (
	errArchiveCacheFull    = errors.New("archive cache byte budget exhausted")
	errArchiveCacheCorrupt = errors.New("corrupt archive cache")
)

// WalkCachedContext visits selected event types in retained sequence order.
// Delivered events contain only Seq, Type, Ts, Subject and Message, on both
// cache hits and misses and from the active log. Use WalkContext for full events.
// Complete immutable-archive summaries are reused only when their source key
// matches. Missing, stale or corrupt entries are rebuilt; I/O errors fail
// explicitly. Completed entries survive cancellation of a later archive.
func WalkCachedContext(ctx context.Context, path string, opts ArchiveCacheOptions, visit func(Event) error) error {
	return walkCachedContext(ctx, path, opts, visit, walkEventFile)
}

func walkCachedContext(ctx context.Context, path string, opts ArchiveCacheOptions, visit func(Event) error, read func(context.Context, string, bool, func(Event) error) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if opts.Dir == "" || opts.MaxBytes <= 0 || len(opts.Types) == 0 {
		return fmt.Errorf("archive cache requires a directory, positive byte budget and event types")
	}
	types := slices.Clone(opts.Types)
	slices.Sort(types)
	types = slices.Compact(types)
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("listing event archives: %w", err)
	}
	p := ArchiveCacheProgress{AfterSeq: opts.AfterSeq, Archives: len(archives)}
	var cacheFull, rebuiltCorrupt bool
	progress := func(phase string) {
		p.Phase = phase
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}
	deliver := func(e Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if matchesFilter(e, Filter{AfterSeq: opts.AfterSeq}) && slices.Contains(types, e.Type) {
			return visit(summarizeArchiveEvent(e).event())
		}
		return nil
	}
	if len(archives) > 0 {
		progress("waiting for cache")
		if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
			return fmt.Errorf("creating archive cache: %w", err)
		}
		// Lock the directory inode, not a status file. The kernel releases this
		// lock on exit; concurrent readers cannot race the total-size budget or
		// remove a cache writer's temporary file. The source journal is unlocked.
		lock, err := lockArchiveCache(ctx, opts.Dir)
		if err != nil {
			return err
		}
		defer lock.Close() //nolint:errcheck // closing releases the advisory lock
		used, err := prepareArchiveCache(opts.Dir, archives)
		if err != nil {
			return err
		}
		cacheFull = used > opts.MaxBytes
		for i, info := range archives {
			if err := ctx.Err(); err != nil {
				return err
			}
			p.Index = i + 1
			if !archiveOverlapsFilter(info, Filter{AfterSeq: opts.AfterSeq}) {
				p.Skipped++
				continue
			}
			source := filepath.Join(filepath.Dir(path), info.Basename)
			st, err := os.Stat(source)
			if err != nil {
				return fmt.Errorf("stat archive %q: %w", info.Basename, err)
			}
			key := archiveCacheKey{Version: 2, Basename: info.Basename, Size: st.Size(), LastSeq: info.LastSeq, ModTime: st.ModTime().UnixNano(), Types: types}
			cachePath := filepath.Join(opts.Dir, info.Basename+".json")
			var records []Event
			var oldSize int64
			var hit bool
			if !cacheFull {
				records, oldSize, hit, err = readArchiveCache(cachePath, key, opts.MaxBytes)
				if errors.Is(err, errArchiveCacheCorrupt) {
					if removeErr := os.Remove(cachePath); removeErr != nil {
						return fmt.Errorf("removing corrupt archive cache %q: %w", cachePath, removeErr)
					}
					log.Printf("events: %v; removed entry and rebuilding", err)
					used -= oldSize
					oldSize = 0
					rebuiltCorrupt = true
				} else if err != nil {
					return err
				}
			}
			if hit {
				p.Hits++
				progress("cache hit")
			} else {
				p.Misses++
				switch {
				case cacheFull:
					progress("cache full")
				case rebuiltCorrupt:
					progress("rebuilt corrupt entry")
				default:
					progress(fmt.Sprintf("cold fill %d of %d", i+1, len(archives)))
				}
				records = []Event{}
				err := read(ctx, source, true, func(e Event) error {
					if slices.Contains(types, e.Type) {
						records = append(records, summarizeArchiveEvent(e).event())
					}
					return ctx.Err()
				})
				if err != nil {
					return fmt.Errorf("reading archive %q: %w", info.Basename, err)
				}
				end, err := os.Stat(source)
				if err != nil {
					return fmt.Errorf("restat archive %q: %w", info.Basename, err)
				}
				if !os.SameFile(st, end) || st.Size() != end.Size() || !st.ModTime().Equal(end.ModTime()) {
					return fmt.Errorf("archive %q changed during cache fill", info.Basename)
				}
				if !cacheFull {
					size, err := writeArchiveCache(ctx, cachePath, key, records, opts.MaxBytes-used+oldSize)
					switch {
					case errors.Is(err, errArchiveCacheFull):
						cacheFull = true
						progress("cache full")
					case err != nil:
						return err
					default:
						used += size - oldSize
					}
				}
			}
			for _, e := range records {
				if err := deliver(e); err != nil {
					return err
				}
			}
		}
	}
	progress("active journal")
	if err := read(ctx, path, false, deliver); err != nil {
		return err
	}
	switch {
	case p.Archives == 0:
		progress("no archives")
	case cacheFull:
		progress("cache full")
	case rebuiltCorrupt:
		progress("rebuilt corrupt entry")
	case p.Hits > 0 && p.Misses == 0:
		progress("warm cache")
	case p.Misses > 0:
		progress("cold fill complete")
	default:
		progress("no matching archives")
	}
	return ctx.Err()
}

func lockArchiveCache(ctx context.Context, dir string) (*os.File, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("opening archive cache: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("locking archive cache: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func prepareArchiveCache(dir string, archives []archiveInfo) (int64, error) {
	keep := make(map[string]bool, len(archives))
	for _, info := range archives {
		keep[info.Basename+".json"] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("listing archive cache: %w", err)
	}
	var used int64
	for _, entry := range entries {
		_, parseErr := parseArchiveBasename(strings.TrimSuffix(entry.Name(), ".json"))
		owned := strings.HasSuffix(entry.Name(), ".json") && parseErr == nil
		if strings.HasPrefix(entry.Name(), ".archive-cache-") || (owned && !keep[entry.Name()]) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return 0, fmt.Errorf("pruning archive cache: %w", err)
			}
		} else if owned {
			st, err := entry.Info()
			if err != nil {
				return 0, fmt.Errorf("stat archive cache: %w", err)
			}
			if !st.Mode().IsRegular() {
				return 0, fmt.Errorf("archive cache %q is not a regular file", entry.Name())
			}
			used += st.Size()
		}
	}
	return used, nil
}

func readArchiveCache(path string, key archiveCacheKey, maxBytes int64) ([]Event, int64, bool, error) {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("stat archive cache: %w", err)
	}
	if st.Size() > maxBytes {
		return nil, st.Size(), false, errArchiveCacheFull
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, st.Size(), false, fmt.Errorf("reading archive cache: %w", err)
	}
	var entry archiveCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, st.Size(), false, fmt.Errorf("%w: decoding %q: %w", errArchiveCacheCorrupt, path, err)
	}
	if entry.Key.Version == 0 || entry.Events == nil || entry.Digest == "" {
		return nil, st.Size(), false, fmt.Errorf("%w: incomplete entry %q", errArchiveCacheCorrupt, path)
	}
	if entry.Key.Version != key.Version || entry.Key.Basename != key.Basename || entry.Key.Size != key.Size || entry.Key.LastSeq != key.LastSeq || entry.Key.ModTime != key.ModTime || !slices.Equal(entry.Key.Types, key.Types) {
		return nil, st.Size(), false, nil
	}
	records := make([]Event, len(entry.Events))
	for i, record := range entry.Events {
		records[i] = record.event()
	}
	digest, err := archiveCacheDigest(records)
	if err != nil {
		return nil, st.Size(), false, err
	}
	if entry.Digest != digest {
		return nil, st.Size(), false, fmt.Errorf("%w: checksum mismatch %q", errArchiveCacheCorrupt, path)
	}
	return records, st.Size(), true, nil
}

func archiveCacheDigest(records []Event) (string, error) {
	data, err := json.Marshal(archiveCacheRecords(records))
	if err != nil {
		return "", fmt.Errorf("encoding archive cache records: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func archiveCacheRecords(records []Event) []archiveCacheRecord {
	summary := make([]archiveCacheRecord, len(records))
	for i, e := range records {
		summary[i] = summarizeArchiveEvent(e)
	}
	return summary
}

func writeArchiveCache(ctx context.Context, path string, key archiveCacheKey, records []Event, available int64) (int64, error) {
	digest, err := archiveCacheDigest(records)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(archiveCacheEntry{Key: key, Events: archiveCacheRecords(records), Digest: digest})
	if err != nil {
		return 0, fmt.Errorf("encoding archive cache: %w", err)
	}
	if int64(len(data)) > available {
		return 0, fmt.Errorf("%w while caching %q", errArchiveCacheFull, key.Basename)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-cache-")
	if err != nil {
		return 0, fmt.Errorf("creating archive cache entry: %w", err)
	}
	defer os.Remove(f.Name()) //nolint:errcheck // renamed on success; best-effort cleanup on error
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("writing archive cache: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("syncing archive cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("closing archive cache: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return 0, fmt.Errorf("publishing archive cache: %w", err)
	}
	return int64(len(data)), nil
}
