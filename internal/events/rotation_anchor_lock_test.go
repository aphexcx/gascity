package events

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotationAnchorWriteExcludesFollowerAppends pins the gf-4c9 codex
// round-3 finding on row 6: the owner's rotation anchor was written without
// the active log's flock while a following recorder's append takes it, so a
// follower that reopened the fresh log between the owner's rename and its
// anchor could read the same empty file, allocate the same sequence number
// and append — and sequence-based readers would discard one of the two as a
// duplicate. The anchor is now published under the same flock: a follower
// that tries to append inside that window is refused by the bounded lock
// wait, its next append lands after the anchor, and the active log carries
// strictly increasing sequence numbers.
func TestRotationAnchorWriteExcludesFollowerAppends(t *testing.T) {
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

	for i := 1; i <= 10; i++ {
		owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-" + string(rune('0'+i))})
	}

	var insideWindow error
	owner.beforeAnchorWrite = func() {
		// The owner holds r.mu and the new active log's flock here. The
		// follower reopens the new log (the rename already happened) and
		// must NOT get to append until the anchor is published.
		insideWindow = follower.RecordAck(Event{Type: WorkerOperation, Actor: "follower", Subject: "inside-anchor-window"})
	}
	res, err := owner.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("ForceRotate did not rotate: %+v", res)
	}
	<-res.Done

	if insideWindow == nil {
		t.Fatalf("the follower appended inside the anchor window; it must be excluded by the active log's flock")
	}
	if !strings.Contains(insideWindow.Error(), "waiting on flock") {
		t.Fatalf("follower refusal = %v, want the bounded flock wait", insideWindow)
	}

	if err := follower.RecordAck(Event{Type: WorkerOperation, Actor: "follower", Subject: "after-anchor"}); err != nil {
		t.Fatalf("follower append after the anchor: %v", err)
	}
	// ReadFiltered stitches the archive and the active log: ten owner events,
	// the anchor, then the follower's one event, with no sequence reused.
	all, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(all) != 12 {
		t.Fatalf("got %d events, want 12 (ten owner events, the anchor, the follower's one): %+v", len(all), all)
	}
	for i := range all {
		if all[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d (no reuse, no gap)", i, all[i].Seq, i+1)
		}
	}
	if all[10].Type != EventsRotated {
		t.Fatalf("seq 11 = %s, want the rotation anchor", all[10].Type)
	}
	if all[11].Subject != "after-anchor" || all[11].Actor != "follower" {
		t.Fatalf("seq 12 = %s/%q, want the follower's after-anchor event", all[11].Actor, all[11].Subject)
	}
}
