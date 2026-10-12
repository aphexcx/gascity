package events

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// TestFollowRotationRecorderWritesToTheActiveLogAfterTheOwnerRotates is the
// gf-4c9 row-6 regression (gf-9x2 codex gate Major at cmd/gc/providers.go:1053):
// a second, non-rotating recorder on the same events.jsonl — the CLI worker
// factory's, cached for the process lifetime — kept its handle on the file
// the rotating owner renamed away, so every event it recorded after a
// rotation landed in the archived file and disappeared from the readers.
// With WithFollowRotation the follower reopens the fresh active log before
// its next append; its events are read back from the active file with
// sequence numbers past the rotation anchor.
func TestFollowRotationRecorderWritesToTheActiveLogAfterTheOwnerRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	owner, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	follower, err := NewFileRecorder(path, &stderr, WithMaxSize(0), WithoutStartupSweep(), WithFollowRotation())
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close() //nolint:errcheck // test cleanup

	owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "before-rotation"})
	follower.Record(Event{Type: WorkerOperation, Actor: "follower", Subject: "follower-before-rotation"})

	res, err := owner.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("expected a rotation, got %+v", res)
	}
	select {
	case <-res.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation compression did not finish within 2s")
	}

	follower.Record(Event{Type: WorkerOperation, Actor: "follower", Subject: "follower-after-rotation"})

	active, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered(active): %v", err)
	}
	var found *Event
	for i := range active {
		if active[i].Subject == "follower-after-rotation" {
			found = &active[i]
		}
	}
	if found == nil {
		subjects := make([]string, 0, len(active))
		for _, e := range active {
			subjects = append(subjects, e.Subject)
		}
		t.Fatalf("the follower's post-rotation event is not in the active log %v (stderr %q); it was appended to the rotated file", subjects, stderr.String())
	}
	if found.Seq <= res.AnchorSeq {
		t.Fatalf("post-rotation event seq = %d, want > the anchor seq %d", found.Seq, res.AnchorSeq)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

// TestFollowRotationIsInertWhilePathIsUnchanged pins the cost: with no
// rotation between appends the follower keeps its handle (same inode) and
// never reopens, so the follow check is one stat per append and nothing else.
func TestFollowRotationIsInertWhilePathIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	follower, err := NewFileRecorder(path, &stderr, WithMaxSize(0), WithoutStartupSweep(), WithFollowRotation())
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close() //nolint:errcheck // test cleanup

	before := follower.file
	follower.Record(Event{Type: WorkerOperation, Actor: "follower", Subject: "one"})
	follower.Record(Event{Type: WorkerOperation, Actor: "follower", Subject: "two"})
	if follower.file != before {
		t.Fatal("the follower reopened its handle although the path never changed")
	}
	evts, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 || evts[0].Seq != 1 || evts[1].Seq != 2 {
		t.Fatalf("events = %+v, want seq 1 and 2 in the active log", evts)
	}
}
