package events

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// newOwnerAndFollower opens a rotating owner and a WithFollowRotation
// follower on the same events.jsonl, the two recorders the controller and
// the CLI worker factory keep on one log.
func newOwnerAndFollower(t *testing.T) (owner, follower *FileRecorder, path string, stderr *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "events.jsonl")
	stderr = &bytes.Buffer{}
	var err error
	owner, err = NewFileRecorder(path, stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() }) //nolint:errcheck // test cleanup
	follower, err = NewFileRecorder(path, stderr, WithMaxSize(0), WithoutStartupSweep(), WithFollowRotation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { follower.Close() }) //nolint:errcheck // test cleanup
	return owner, follower, path, stderr
}

func waitRotation(t *testing.T, res RotationResult) {
	t.Helper()
	select {
	case <-res.Done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("rotation compression did not finish within %s", testutil.GoroutineRaceTimeout)
	}
}

// readAllStitched reads the archives and the active log, waiting for the
// in-flight compression first so the stitched view is complete.
func readAllStitched(t *testing.T, path string) []Event {
	t.Helper()
	all, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	return all
}

// TestRotationAnchorContinuesPastAFollowersSequence pins the gf-4c9 round-2
// gate finding at recorder.go:724 (finding 1a): the following recorder
// advances the log without the rotating owner's cached sequence seeing it,
// so when the owner wrote sequence 1, the follower 2 to 4, and the owner
// rotated, the anchor was written as sequence 2 and every later owner event
// overlapped the archived sequences — a watcher already at 4 discarded them
// all. The rotation now advances the owner's sequence to the renamed log's
// last sequence before writing the anchor.
func TestRotationAnchorContinuesPastAFollowersSequence(t *testing.T) {
	owner, follower, path, stderr := newOwnerAndFollower(t)

	owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-1"})
	for i := 2; i <= 4; i++ {
		if err := follower.RecordAck(Event{Type: WorkerOperation, Actor: "follower", Subject: "follower-" + string(rune('0'+i))}); err != nil {
			t.Fatalf("follower append %d: %v", i, err)
		}
	}

	res, err := owner.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("ForceRotate did not rotate: %+v", res)
	}
	if res.FirstSeq != 1 || res.LastSeq != 4 {
		t.Fatalf("rotated window = %d..%d, want 1..4 (the follower's three events are in the renamed log)", res.FirstSeq, res.LastSeq)
	}
	if res.AnchorSeq != 5 {
		t.Fatalf("anchor seq = %d, want 5: the anchor reused a sequence the archive already holds", res.AnchorSeq)
	}
	waitRotation(t, res)

	owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-after"})

	all := readAllStitched(t, path)
	if len(all) != 6 {
		t.Fatalf("got %d events, want 6 (owner, three follower, the anchor, owner-after): %+v", len(all), all)
	}
	for i := range all {
		if all[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d (no reuse, no gap): %+v", i, all[i].Seq, i+1, all)
		}
	}
	if all[4].Type != EventsRotated || all[5].Subject != "owner-after" {
		t.Fatalf("seq 5/6 = %s/%q, want the anchor then the owner's next event", all[4].Type, all[5].Subject)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}
