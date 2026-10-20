package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWalkContextMatchesRetainedHistory(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	want, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	if err := WalkContext(context.Background(), path, func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walk=%v want=%v", got, want)
	}
}

func TestWalkContextCancelsWithinArchiveAndActiveFile(t *testing.T) {
	for _, archive := range []bool{false, true} {
		name := "active"
		if archive {
			name = "archive"
		}
		t.Run(name, func(t *testing.T) {
			var path string
			if archive {
				path = filepath.Join(seedArchives(t), "events.jsonl")
			} else {
				path = filepath.Join(t.TempDir(), "events.jsonl")
				if err := os.WriteFile(path, []byte("{\"seq\":1}\n{\"seq\":2}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := WalkContext(ctx, path, func(Event) error { calls++; cancel(); return nil })
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestWalkContextReturnsVisitorAndReadErrors(t *testing.T) {
	path := filepath.Join(seedArchives(t), "events.jsonl")
	boom := errors.New("visitor failed")
	if err := WalkContext(context.Background(), path, func(Event) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("error=%v", err)
	}
	if err := WalkContext(context.Background(), t.TempDir(), func(Event) error { return nil }); err == nil {
		t.Fatal("directory read must fail")
	}
	if err := WalkContext(context.Background(), filepath.Join(t.TempDir(), "missing"), func(Event) error { t.Fatal("unexpected event"); return nil }); err != nil {
		t.Fatal(err)
	}
}
