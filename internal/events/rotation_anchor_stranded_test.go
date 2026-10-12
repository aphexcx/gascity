package events

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestRotationArchivesTheRenamedSegmentWhenTheAnchorLockTimesOut pins the
// gf-4c9 codex round-4 finding on row 6: once the owner has renamed the old
// active log, a failure to publish the anchor (another writer holding the new
// log's flock past the bounded wait — or, before round 3 made the lock exist,
// a failed anchor write) must not strand the renamed segment as a rotating-*
// file that plain readers never see and no later rotation retries. The
// segment's compression starts before the anchor is attempted, so the
// rotation reports its error AND the archive still lands.
func TestRotationArchivesTheRenamedSegmentWhenTheAnchorLockTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup

	for i := 1; i <= 10; i++ {
		owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-" + string(rune('0'+i))})
	}

	// Another writer takes the NEW active log's flock the moment it exists
	// and holds it past the owner's bounded wait.
	var holder *os.File
	owner.beforeAnchorLock = func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Errorf("holder open: %v", err)
			return
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Errorf("holder flock: %v", err)
			return
		}
		holder = f
	}
	_, err = owner.ForceRotate()
	if holder != nil {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		_ = holder.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "waiting on flock") {
		t.Fatalf("ForceRotate error = %v, want the anchor lock's bounded-wait timeout", err)
	}
	owner.WaitForRotations()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var archives, rotating []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "events.jsonl.archive-") && strings.HasSuffix(e.Name(), ".gz"):
			archives = append(archives, e.Name())
		case strings.Contains(e.Name(), ".rotating-"):
			rotating = append(rotating, e.Name())
		}
	}
	if len(rotating) != 0 {
		t.Fatalf("the renamed segment is stranded as %v; it must be archived even though the anchor failed", rotating)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want exactly one for the ten rotated events", archives)
	}

	all, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("got %d events, want the ten rotated ones readable from the archive: %+v", len(all), all)
	}
	for i := range all {
		if all[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, all[i].Seq, i+1)
		}
	}

	// The owner keeps writing to the new active log with the sequence
	// continued past the archived ten.
	if err := owner.RecordAck(Event{Type: WorkerOperation, Actor: "owner", Subject: "after-failed-anchor"}); err != nil {
		t.Fatalf("owner append after the failed anchor: %v", err)
	}
	all, err = ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(all) != 11 || all[10].Seq != 11 || all[10].Subject != "after-failed-anchor" {
		t.Fatalf("after the failed anchor: got %d events, last = seq %d %q; want 11 with the owner's event at seq 11", len(all), all[len(all)-1].Seq, all[len(all)-1].Subject)
	}
}
