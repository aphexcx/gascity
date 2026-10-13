package events

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
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
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("rotation compression did not finish within %s", testutil.GoroutineRaceTimeout)
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

// TestFollowRotationReopenContinuesSequencePastRenamedLog reproduces the
// interleaving the lane's codex round named: the owner has renamed the
// active log away but not yet written the rotation anchor, so the follower
// reopens an EMPTY active log. Its next append must continue past the
// renamed file's last sequence (11 after ten owner events), never reuse one
// that sequence-based watchers would discard.
func TestFollowRotationReopenContinuesSequencePastRenamedLog(t *testing.T) {
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
	// The owner's rotation, frozen after its rename and before its anchor.
	if err := os.Rename(path, filepath.Join(dir, formatRotatingBasename(time.Now().UTC(), 1, 10))); err != nil {
		t.Fatal(err)
	}

	follower.Record(Event{Type: WorkerOperation, Actor: "follower", Subject: "mid-rotation"})

	active, err := ReadFiltered(path, Filter{})
	if err != nil {
		t.Fatalf("ReadFiltered(active): %v", err)
	}
	if len(active) != 1 || active[0].Subject != "mid-rotation" {
		t.Fatalf("active log = %+v, want the follower's one event", active)
	}
	if active[0].Seq != 11 {
		t.Fatalf("mid-rotation event seq = %d, want 11 (past the renamed log's last sequence)", active[0].Seq)
	}
}

func TestFollowRotationInitialAttachmentContinuesPastRenamedLog(t *testing.T) {
	for _, activeExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("active-exists=%v", activeExists), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			writeJSONLEvents(t, path, 1, 10)
			if err := os.Rename(path, filepath.Join(dir, formatRotatingBasename(time.Now().UTC(), 1, 10))); err != nil {
				t.Fatal(err)
			}
			if activeExists {
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Attach only after rename, before either the archive or anchor exists.
			follower, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep(), WithFollowRotation())
			if err != nil {
				t.Fatal(err)
			}
			defer follower.Close() //nolint:errcheck // test cleanup
			if err := follower.RecordAck(Event{Type: WorkerOperation, Subject: "new-follower"}); err != nil {
				t.Fatal(err)
			}
			active, err := ReadFiltered(path, Filter{AfterSeq: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 1 || active[0].Seq != 11 {
				t.Fatalf("watcher after 10 sees %+v, want new follower event 11", active)
			}
		})
	}
}
