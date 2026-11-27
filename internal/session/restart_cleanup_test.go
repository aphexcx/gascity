package session

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestControllerMessageWithdrawsCanceledWaitNudges(t *testing.T) {
	store, sp := beads.NewMemStore(), runtime.NewFake()
	cityPath := t.TempDir()
	pokes, withdrawals := 0, 0
	m := NewManagerWithOptions(store, sp, WithCityPath(cityPath),
		WithRestartCoordinator(func() bool { return true }, func() error { pokes++; return nil }),
		WithWaitNudgeWithdrawal(func(ids []string) error {
			withdrawals++
			if len(ids) != 1 || ids[0] != "wait-nudge" {
				t.Errorf("withdrawals=%v", ids)
			}
			return errors.New("injected withdrawal failure")
		}))
	info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: cityPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(waitBeadFixture("wait-1", "open", info.ID, map[string]string{"state": "ready", "nudge_id": "wait-nudge"})); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	delivery, err := m.Send(t.Context(), info.ID, "wake", "launch", runtime.Config{})
	if err != nil || !delivery.Queued || pokes != 1 || withdrawals != 1 {
		t.Fatalf("delivery=%+v err=%v pokes=%d withdrawals=%d", delivery, err, pokes, withdrawals)
	}
	if !strings.Contains(output.String(), "injected withdrawal failure") {
		t.Fatal("withdrawal error was not logged")
	}
	queue, err := nudgequeue.LoadState(cityPath)
	if err != nil || len(queue.Pending) != 1 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
}
