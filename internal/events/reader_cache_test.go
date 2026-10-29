package events

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestWalkCachedContextReusesArchivesAndReadsActive(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
	opens := 0
	read := func(ctx context.Context, path string, compressed bool, visit func(Event) error) error {
		if compressed {
			opens++
		}
		return walkEventFile(ctx, path, compressed, visit)
	}
	walk := func() []Event {
		t.Helper()
		var got []Event
		if err := walkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }, read); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want, err := ReadFiltered(path, Filter{Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Actor = ""
	}
	if got := walk(); !reflect.DeepEqual(got, want) || opens != 3 {
		t.Fatalf("cold opens=%d got=%v want=%v", opens, got, want)
	}
	opens = 0
	writeJSONLEvents(t, path, 100)
	got := walk()
	if opens != 0 || len(got) != len(want)+1 || got[len(got)-1].Seq != 100 {
		t.Fatalf("warm opens=%d events=%v", opens, got)
	}
}

func TestWalkCachedContextSkipsExcludedArchivesBeforeOpening(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20, AfterSeq: archives[0].LastSeq}
	opens := 0
	var got []Event
	err = walkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }, func(ctx context.Context, p string, compressed bool, visit func(Event) error) error {
		if compressed {
			if filepath.Base(p) == archives[0].Basename {
				t.Fatal("excluded archive opened")
			}
			opens++
		}
		return walkEventFile(ctx, p, compressed, visit)
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := ReadFiltered(path, Filter{Type: BeadCreated, AfterSeq: opts.AfterSeq})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Actor = ""
	}
	if opens != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("opens=%d got=%v want=%v", opens, got, want)
	}
}

func TestWalkCachedContextDoesNotPublishPartialArchives(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "read error", true: "cancel"}[cancelRead], func(t *testing.T) {
			path := filepath.Join(seedArchives(t), "events.jsonl")
			opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opens := 0
			boom := errors.New("source failed")
			err := walkCachedContext(ctx, path, opts, func(Event) error { return nil }, func(ctx context.Context, p string, compressed bool, visit func(Event) error) error {
				if compressed {
					opens++
				}
				if opens == 2 {
					if err := visit(Event{Seq: 4, Type: BeadCreated}); err != nil {
						return err
					}
					if cancelRead {
						cancel()
						return ctx.Err()
					}
					return boom
				}
				return walkEventFile(ctx, p, compressed, visit)
			})
			wantErr := boom
			if cancelRead {
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v", err)
			}
			files, err := os.ReadDir(opts.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 {
				t.Fatalf("published partial archive or temp file: %v", files)
			}
			opens = 0
			err = walkCachedContext(context.Background(), path, opts, func(Event) error { return nil }, func(ctx context.Context, p string, compressed bool, visit func(Event) error) error {
				if compressed {
					opens++
				}
				return walkEventFile(ctx, p, compressed, visit)
			})
			if err != nil || opens != 2 {
				t.Fatalf("resume opens=%d error=%v", opens, err)
			}
		})
	}
}

func TestWalkCachedContextRebuildsCorruptCache(t *testing.T) {
	for _, damage := range []string{"incomplete", "truncated", "checksum"} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(seedArchives(t), "events.jsonl")
			opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
			var want []Event
			if err := WalkCachedContext(context.Background(), path, opts, func(e Event) error { want = append(want, e); return nil }); err != nil {
				t.Fatal(err)
			}
			files, err := os.ReadDir(opts.Dir)
			if err != nil {
				t.Fatal(err)
			}
			cachePath := filepath.Join(opts.Dir, files[0].Name())
			data, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "incomplete":
				data = []byte("{}")
			case "truncated":
				data = data[:len(data)/2]
			case "checksum":
				data = bytes.Replace(data, []byte("r0-0"), []byte("bad!"), 1)
			}
			if err := os.WriteFile(cachePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			oldOutput := log.Writer()
			log.SetOutput(&diagnostic)
			t.Cleanup(func() { log.SetOutput(oldOutput) })
			var progress ArchiveCacheProgress
			opts.Progress = func(p ArchiveCacheProgress) { progress = p }
			var got []Event
			opens := 0
			err = walkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }, func(ctx context.Context, source string, compressed bool, visit func(Event) error) error {
				if compressed {
					opens++
					if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
						t.Fatalf("corrupt entry not removed before rebuild: %v", err)
					}
				}
				return walkEventFile(ctx, source, compressed, visit)
			})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("error=%v got=%v want=%v", err, got, want)
			}
			if opens != 1 || progress.Hits != 2 || progress.Misses != 1 || progress.Phase != "rebuilt corrupt entry" {
				t.Fatalf("opens=%d progress=%+v", opens, progress)
			}
			if strings.Count(diagnostic.String(), cachePath) != 1 {
				t.Fatalf("diagnostic=%q", diagnostic.String())
			}
			if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if progress.Hits != 3 || progress.Misses != 0 || progress.Phase != "warm cache" {
				t.Fatalf("rebuilt entry not reusable: %+v", progress)
			}
		})
	}
}

func TestWalkCachedContextFullCacheDeliversHistory(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary exceeds budget", true: "existing cache exceeds budget"}[populated], func(t *testing.T) {
			path := filepath.Join(seedArchives(t), "events.jsonl")
			opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
			var want []Event
			if err := WalkContext(context.Background(), path, func(e Event) error {
				if e.Type == BeadCreated {
					e.Actor = ""
					want = append(want, e)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if populated {
				if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			opts.MaxBytes = 1
			var progress ArchiveCacheProgress
			opts.Progress = func(p ArchiveCacheProgress) { progress = p }
			var got []Event
			if err := WalkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) || progress.Hits != 0 || progress.Misses != 3 || progress.Phase != "cache full" {
				t.Fatalf("got=%v want=%v progress=%+v", got, want, progress)
			}
			files, err := os.ReadDir(opts.Dir)
			if err != nil {
				t.Fatal(err)
			}
			wantFiles := 0
			if populated {
				wantFiles = 3
			}
			if len(files) != wantFiles {
				t.Fatalf("published over budget: %v", files)
			}
		})
	}
}

func TestWalkCachedContextNoArchivesPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeJSONLEvents(t, path, 1)
	var progress ArchiveCacheProgress
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1, Progress: func(p ArchiveCacheProgress) { progress = p }}
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if progress.Phase != "no archives" {
		t.Fatalf("progress=%+v", progress)
	}
}

func TestWalkCachedContextInvalidatesSourceAndTypeSelection(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
	opens := 0
	walk := func() []Event {
		t.Helper()
		opens = 0
		var got []Event
		err := walkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }, func(ctx context.Context, p string, compressed bool, visit func(Event) error) error {
			if compressed {
				opens++
			}
			return walkEventFile(ctx, p, compressed, visit)
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	walk()
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(path), archives[0].Basename)
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	changed := Event{Seq: 1, Type: BeadCreated, Subject: "changed source"}
	if err := json.NewEncoder(gz).Encode(changed); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := walk(); opens != 1 || got[0].Subject != changed.Subject {
		t.Fatalf("opens=%d got=%v", opens, got)
	}
	st, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, st.ModTime(), st.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	walk()
	if opens != 1 {
		t.Fatalf("mtime-only change opened %d archives", opens)
	}
	opts.Types = []string{EventsRotated}
	got := walk()
	want, err := ReadFiltered(path, Filter{Type: EventsRotated})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Actor = ""
		want[i].Payload = nil
	}
	if opens != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("type change opens=%d got=%v want=%v", opens, got, want)
	}
}

func TestArchiveCacheKeyAndChecksum(t *testing.T) {
	key := archiveCacheKey{Version: 1, Basename: "archive", Size: 99, LastSeq: 5, ModTime: 123, Types: []string{OrderFailed}}
	path := filepath.Join(t.TempDir(), "entry.json")
	if _, err := writeArchiveCache(context.Background(), path, key, []Event{{Seq: 5, Type: OrderFailed, Message: "failed"}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "basename", "size", "last seq", "mtime", "types"} {
		t.Run(field, func(t *testing.T) {
			changed := key
			switch field {
			case "version":
				changed.Version++
			case "basename":
				changed.Basename += "new"
			case "size":
				changed.Size++
			case "last seq":
				changed.LastSeq++
			case "mtime":
				changed.ModTime++
			case "types":
				changed.Types = []string{OrderCompleted}
			}
			if _, _, hit, err := readArchiveCache(path, changed, 1<<20); err != nil || hit {
				t.Fatalf("hit=%v error=%v", hit, err)
			}
		})
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Still valid JSON: a checksum must catch semantic corruption too.
	data = []byte(strings.Replace(string(data), `"message":"failed"`, `"message":"passed"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readArchiveCache(path, key, 1<<20); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error=%v", err)
	}
}

func TestWalkCachedContextDoesNotCacheTheCursor(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20, AfterSeq: 1}
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	opts.AfterSeq = 0
	var got []Event
	if err := WalkCachedContext(context.Background(), path, opts, func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want, err := ReadFiltered(path, Filter{Type: BeadCreated})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		want[i].Actor = ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached cursor lost same-archive history: %v want %v", got, want)
	}
}

func TestWalkCachedContextPrunesRetiredEntriesAndBoundsTotalBytes(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
	visit := func(Event) error { return nil }
	if err := WalkCachedContext(context.Background(), path, opts, visit); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var largest int64
	for _, file := range files {
		st, err := file.Info()
		if err != nil {
			t.Fatal(err)
		}
		largest = max(largest, st.Size())
	}
	opts.MaxBytes = largest + 1
	var progress ArchiveCacheProgress
	opts.Progress = func(p ArchiveCacheProgress) { progress = p }
	if err := WalkCachedContext(context.Background(), path, opts, visit); err != nil || progress.Phase != "cache full" || progress.Misses != 3 || progress.Hits != 0 {
		t.Fatalf("aggregate budget fallback: progress=%+v error=%v", progress, err)
	}
	opts.MaxBytes = 1 << 20
	retired := files[0].Name()
	if err := os.Remove(filepath.Join(filepath.Dir(path), strings.TrimSuffix(retired, ".json"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, ".archive-cache-interrupted"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WalkCachedContext(context.Background(), path, opts, visit); err != nil {
		t.Fatal(err)
	}
	files, err = os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("retired/partial entries retained: %v", files)
	}
}

func TestArchiveCacheLockHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	lock, err := lockArchiveCache(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := lockArchiveCache(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestWalkCachedContextRebuildsVersionOneAsSummary(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	opts := ArchiveCacheOptions{Dir: t.TempDir(), Types: []string{BeadCreated}, MaxBytes: 1 << 20}
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(opts.Dir, files[0].Name())
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct a valid pre-change entry with all Event fields and its digest.
	var old struct {
		Key    archiveCacheKey
		Events []Event
		Digest string
	}
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	old.Key.Version = 1
	old.Events[0].Payload = json.RawMessage(`{"unused":"large payload"}`)
	old.Events[0].Actor = "unused actor"
	old.Events[0].RunID = "unused run"
	recordBytes, err := json.Marshal(old.Events)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(recordBytes)
	old.Digest = hex.EncodeToString(digest[:])
	data, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var progress ArchiveCacheProgress
	opts.Progress = func(p ArchiveCacheProgress) { progress = p }
	if err := WalkCachedContext(context.Background(), path, opts, func(e Event) error {
		if e.Payload != nil || e.Actor != "" || e.RunID != "" {
			t.Errorf("not summarized: %+v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if progress.Misses != 1 || progress.Hits != 2 {
		t.Fatalf("version not rebuilt: %+v", progress)
	}
	data, err = os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"payload"`, `"actor"`, `"run_id"`} {
		if bytes.Contains(data, []byte(field)) {
			t.Errorf("entry retains %s", field)
		}
	}
	var rebuilt archiveCacheEntry
	if err := json.Unmarshal(data, &rebuilt); err != nil {
		t.Fatal(err)
	}
	if rebuilt.Key.Version <= 1 {
		t.Fatalf("version=%d", rebuilt.Key.Version)
	}
}

func TestWalkCachedContextReturnsCacheIOAndVisitorErrors(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := ArchiveCacheOptions{Dir: blocked, Types: []string{BeadCreated}, MaxBytes: 1 << 20}
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { t.Fatal("visitor called on cache failure"); return nil }); err == nil {
		t.Fatal("unwritable cache produced success")
	}
	opts.Dir = t.TempDir()
	boom := errors.New("visitor failed")
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("error=%v", err)
	}
	if err := WalkCachedContext(context.Background(), path, opts, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	err := walkCachedContext(ctx, path, opts, func(Event) error { seen++; cancel(); return nil }, func(context.Context, string, bool, func(Event) error) error {
		t.Fatal("warm canceled walk opened source")
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("seen=%d error=%v", seen, err)
	}
}
