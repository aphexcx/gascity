package events

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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

func TestWalkCachedContextRejectsCorruptAndOversizedCache(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(opts.Dir, files[0].Name()), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WalkCachedContext(context.Background(), path, opts, visit); err == nil {
		t.Fatal("corrupt cache must not produce success")
	}
	opts.Dir = t.TempDir()
	opts.MaxBytes = 1
	if err := WalkCachedContext(context.Background(), path, opts, visit); err == nil || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("cache capacity error=%v", err)
	}
	files, err = os.ReadDir(opts.Dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("over-budget cache files=%v error=%v", files, err)
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
	if err := WalkCachedContext(context.Background(), path, opts, visit); err == nil {
		t.Fatal("aggregate cache budget ignored")
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
