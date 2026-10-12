package events

import (
	"testing"
)

// TestFollowRotationAppendIsNotLostToARotationBetweenCheckAndAppend pins
// the gf-4c9 round-2 gate finding at recorder.go:304 (finding 1b): the
// follower's inode check ran before it took the active log's flock, and the
// owner's rotation took no lock on the old log, so a rotation landing
// between the check and the append — rename, compress, unlink — left the
// follower appending to an unlinked descriptor: RecordAck returned nil and
// no reader could ever recover the event. The follower now proves the handle
// under the flock and the owner renames under the same flock, so the append
// lands in the active log, after the anchor.
func TestFollowRotationAppendIsNotLostToARotationBetweenCheckAndAppend(t *testing.T) {
	owner, follower, path, stderr := newOwnerAndFollower(t)
	for i := 1; i <= 3; i++ {
		owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-" + string(rune('0'+i))})
	}

	var rotation RotationResult
	follower.beforeAppendLock = func() {
		// The follower has checked its handle (same file) and is about to
		// lock and append. The owner rotates and finishes compressing: the
		// file the follower holds is renamed, gzipped and unlinked.
		res, err := owner.ForceRotate()
		if err != nil {
			t.Errorf("ForceRotate inside the follower's window: %v", err)
			return
		}
		waitRotation(t, res)
		rotation = res
	}
	if err := follower.RecordAck(Event{Type: WorkerOperation, Actor: "follower", Subject: "acked-across-rotation"}); err != nil {
		t.Fatalf("RecordAck across the rotation: %v", err)
	}
	follower.beforeAppendLock = nil
	if !rotation.Rotated {
		t.Fatal("the owner did not rotate inside the follower's window")
	}

	all := readAllStitched(t, path)
	var found *Event
	for i := range all {
		if all[i].Subject == "acked-across-rotation" {
			found = &all[i]
		}
	}
	if found == nil {
		subjects := make([]string, 0, len(all))
		for _, e := range all {
			subjects = append(subjects, e.Subject)
		}
		t.Fatalf("the acknowledged append is unrecoverable: it is in neither the archive nor the active log %v (stderr %q)", subjects, stderr.String())
	}
	if found.Seq <= rotation.AnchorSeq {
		t.Fatalf("acked event seq = %d, want > the anchor seq %d", found.Seq, rotation.AnchorSeq)
	}
	for i := range all {
		if all[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d: %+v", i, all[i].Seq, i+1, all)
		}
	}
}

// TestFollowRotationBatchIsNotLostToARotationBetweenCheckAndAppend is the
// AppendBatch half of finding 1b: the same window, the same loss, the same
// proof under the flock.
func TestFollowRotationBatchIsNotLostToARotationBetweenCheckAndAppend(t *testing.T) {
	owner, follower, path, stderr := newOwnerAndFollower(t)
	for i := 1; i <= 3; i++ {
		owner.Record(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-" + string(rune('0'+i))})
	}

	var rotation RotationResult
	follower.beforeAppendLock = func() {
		res, err := owner.ForceRotate()
		if err != nil {
			t.Errorf("ForceRotate inside the follower's window: %v", err)
			return
		}
		waitRotation(t, res)
		rotation = res
	}
	batch := []Event{
		{Type: WorkerOperation, Actor: "follower", Subject: "batch-1"},
		{Type: WorkerOperation, Actor: "follower", Subject: "batch-2"},
	}
	if err := follower.AppendBatch(batch); err != nil {
		t.Fatalf("AppendBatch across the rotation: %v", err)
	}
	follower.beforeAppendLock = nil
	if !rotation.Rotated {
		t.Fatal("the owner did not rotate inside the follower's window")
	}

	all := readAllStitched(t, path)
	got := map[string]uint64{}
	for _, e := range all {
		got[e.Subject] = e.Seq
	}
	for _, subject := range []string{"batch-1", "batch-2"} {
		seq, ok := got[subject]
		if !ok {
			t.Fatalf("the acknowledged batch event %q is unrecoverable (stderr %q): %+v", subject, stderr.String(), all)
		}
		if seq <= rotation.AnchorSeq {
			t.Fatalf("%q seq = %d, want > the anchor seq %d", subject, seq, rotation.AnchorSeq)
		}
	}
	for i := range all {
		if all[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d: %+v", i, all[i].Seq, i+1, all)
		}
	}
}
