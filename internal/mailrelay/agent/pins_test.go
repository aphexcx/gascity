package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A pin removed while the agent runs is no longer believed at the next
// batch: mail signed by that peer is passed over as not-a-peer.
func TestRemovedPinIsNotBelievedAtTheNextBatch(t *testing.T) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	city := newCityAPI(t, "alpha", "reader")
	run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the first ask", func() bool { return len(rel.recorder.reads()) > 0 })
	if err := os.Remove(filepath.Join(alpha.settings.PeersDir, "hub.pub")); err != nil {
		t.Fatal(err)
	}
	rel.add(1, seal(t, hub, alpha, "after-unpin", time.Now(), letter("reader")))
	rel.recorder.await(t, "the place to pass the mail", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
	run.stop(t)
	if n := len(city.inbox("reader")); n != 0 {
		t.Fatalf("filed %d mails from a peer whose pin was removed", n)
	}
	if drops := alpha.log.lines("drop "); len(drops) != 1 || countDrops(alpha.log, "after-unpin", "not-a-peer") != 1 {
		t.Fatalf("drop lines %q, want one naming not-a-peer", drops)
	}
}

// A pin replaced by another key while the agent runs: mail signed with the
// old key is not filed, mail signed with the new key is.
func TestReplacedPinBelievesOnlyTheNewKey(t *testing.T) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	city := newCityAPI(t, "alpha", "reader")
	run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the first ask", func() bool { return len(rel.recorder.reads()) > 0 })
	rotated := newCityFixture(t, "hub")
	alpha.pinKey("hub", rotated.public())
	old := letter("reader")
	old.Subject = "signed with the old key"
	rel.add(1, seal(t, hub, alpha, "old-key", time.Now(), old))
	rel.add(2, seal(t, rotated, alpha, "new-key", time.Now(), letter("reader")))
	rel.recorder.await(t, "the place to pass both", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
	run.stop(t)
	inbox := city.inbox("reader")
	if len(inbox) != 1 || inbox[0].Subject != subjectSentinel {
		t.Fatalf("filed %d mails after the pin was replaced: %+v", len(inbox), inbox)
	}
	if countDrops(alpha.log, "old-key", "bad-signature") != 1 {
		t.Fatalf("drop lines %q", alpha.log.lines("drop "))
	}
}

// When the peers folder cannot be read before a batch, nothing in the batch
// is filed from pins the agent can no longer vouch for, and nothing is
// passed over either: the batch stays at the relay and is asked again, and
// it is filed once the folder reads again.
func TestUnreadablePeersFolderHoldsTheBatch(t *testing.T) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	city := newCityAPI(t, "alpha", "reader")
	run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the first ask", func() bool { return len(rel.recorder.reads()) > 0 })
	pin := filepath.Join(alpha.settings.PeersDir, "hub.pub")
	if err := os.Chmod(pin, 0o664); err != nil {
		t.Fatal(err)
	}
	rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
	rel.recorder.await(t, "the batch to be held twice, or filed", func() bool {
		return len(alpha.log.lines("peers-failed ")) >= 2 || len(city.inbox("reader")) > 0
	})
	if n := len(city.inbox("reader")); n != 0 {
		t.Fatalf("filed %d mails while the peers folder could not be read", n)
	}
	if drops := alpha.log.lines("drop "); len(drops) != 0 {
		t.Fatalf("passed over while the peers folder could not be read: %q", drops)
	}
	if slices.ContainsFunc(rel.recorder.reads(), func(n int64) bool { return n != 0 }) {
		t.Fatalf("the place moved while the peers folder could not be read: %v", rel.recorder.reads())
	}
	if err := os.Chmod(pin, 0o600); err != nil {
		t.Fatal(err)
	}
	rel.recorder.await(t, "the place to pass the mail", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
	run.stop(t)
	if n := len(city.inbox("reader")); n != 1 {
		t.Fatalf("filed %d mails once the folder read again", n)
	}
}
