package session

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

type presentationFailStore struct {
	beads.Store
	failID string
	err    error
}

func (s *presentationFailStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID {
		return s.err
	}
	return s.Store.Update(id, opts)
}

// Every closed origin that releases an explicit name must leave its successor
// alone, including when an attempted historical edit fails to persist.
func TestClosedSessionOperationsPreserveSuccessorRuntime(t *testing.T) {
	for _, origin := range []struct {
		name string
		meta map[string]string
	}{
		{"manual", map[string]string{"session_origin": "manual"}},
		{"pool-ephemeral", map[string]string{"session_origin": "ephemeral", "pool_managed": "true"}},
		{"configured-named", map[string]string{"session_origin": "named", "configured_named_identity": "rig/sky", "configured_named_session": "true"}},
		{"configured-named-without-flag", map[string]string{"session_origin": "named", "configured_named_identity": "rig/sky"}},
	} {
		for _, operation := range []string{"set-alias", "clear-alias", "failed-write", "title-only", "shutdown"} {
			t.Run(origin.name+"/"+operation, func(t *testing.T) {
				mem := beads.NewMemStore()
				failure := errors.New("presentation write denied")
				store := &presentationFailStore{Store: mem, err: failure}
				sp := runtime.NewFake()
				mgr := NewManagerWithOptions(store, sp)
				meta := maps.Clone(origin.meta)
				meta["session_name"] = "sky"
				meta["session_name_explicit"] = "true"
				meta["state"] = "active"
				meta["alias"] = "old-alias"
				nextAlias := "retired"
				if operation == "clear-alias" {
					nextAlias = ""
				}
				if meta["configured_named_session"] == "true" {
					// Config-managed aliases remain immutable. Reapplying their
					// current value still used to synchronize the wrong runtime.
					meta["alias"] = nextAlias
				}
				// Retained metadata on a closed record is a supported historical
				// shape, including named records closed outside Manager.Close.
				old, err := mem.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Title: "old title", Metadata: meta})
				if err != nil {
					t.Fatal(err)
				}
				if err := mem.Close(old.ID); err != nil {
					t.Fatal(err)
				}
				successor, err := mgr.CreateSession(context.Background(), CreateOptions{
					ExplicitName: "sky", Alias: "sky-live", Template: "helper", Title: "successor",
					Command: "claude", Provider: "claude", WorkDir: t.TempDir(),
					ExtraMeta: map[string]string{"session_origin": "manual"},
				})
				if err != nil {
					t.Fatalf("creating successor to released name: %v", err)
				}
				// Fake.Start records launch env but does not expose it as metadata.
				wantMeta := map[string]string{"GC_SESSION_ID": successor.ID, "GC_ALIAS": "sky-live", "GC_AGENT": "sky-live", "BEADS_ACTOR": "sky-live"}
				for key, value := range wantMeta {
					if err := sp.SetMeta("sky", key, value); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "failed-write" {
					store.failID = old.ID
				}
				firstCall := len(sp.SnapshotCalls())
				title := "historical title"
				switch operation {
				case "shutdown":
					err = mgr.SuspendForShutdown(old.ID)
				case "title-only":
					err = mgr.UpdatePresentation(old.ID, &title, nil)
				default:
					err = mgr.UpdatePresentation(old.ID, &title, &nextAlias)
				}
				if operation == "failed-write" {
					if !errors.Is(err, failure) {
						t.Fatalf("edit error = %v, want injected write failure", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				for _, call := range sp.SnapshotCalls()[firstCall:] {
					if call.Name != "sky" {
						continue
					}
					switch call.Method {
					case "SetMeta", "RemoveMeta", "Stop":
						t.Errorf("closed predecessor touched successor: %+v", call)
					case "GetMeta", "IsRunning":
						if operation != "shutdown" {
							t.Errorf("historical presentation edit consulted runtime: %+v", call)
						}
					}
				}
				if !sp.IsRunning("sky") {
					t.Error("successor runtime stopped")
				}
				for key, want := range wantMeta {
					if got, err := sp.GetMeta("sky", key); err != nil || got != want {
						t.Errorf("successor %s = %q, %v; want %q", key, got, err, want)
					}
				}
				after, err := mem.Get(old.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantTitle, wantAlias := "old title", meta["alias"]
				if operation != "failed-write" && operation != "shutdown" {
					wantTitle = title
					if operation != "title-only" {
						wantAlias = nextAlias
					}
				}
				if after.Status != "closed" || after.Title != wantTitle || after.Metadata["alias"] != wantAlias {
					t.Errorf("historical record = %+v; want closed, title %q, alias %q", after, wantTitle, wantAlias)
				}
			})
		}
	}
}

type identityReadErrorProvider struct {
	*runtime.Fake
	err error
}

func (p identityReadErrorProvider) GetMeta(name, key string) (string, error) {
	if key == "GC_SESSION_ID" {
		return "", p.err
	}
	return p.Fake.GetMeta(name, key)
}

func TestRuntimeIdentityFencePreservesLegacyOwnership(t *testing.T) {
	for _, operation := range []string{"alias", "shutdown"} {
		for _, owner := range []string{"same", "unowned", "successor", "unreadable"} {
			t.Run(operation+"/"+owner, func(t *testing.T) {
				store := beads.NewMemStore()
				b, err := store.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{"session_name": "sky", "state": "active"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Close(b.ID); err != nil {
					t.Fatal(err)
				}
				sp := runtime.NewFake()
				if err := sp.Start(context.Background(), "sky", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				id := map[string]string{"same": b.ID, "successor": "successor-id"}[owner]
				if err := sp.SetMeta("sky", "GC_SESSION_ID", id); err != nil {
					t.Fatal(err)
				}
				var provider runtime.Provider = sp
				failure := errors.New("runtime identity unavailable")
				if owner == "unreadable" {
					provider = identityReadErrorProvider{Fake: sp, err: failure}
				}
				firstCall := len(sp.SnapshotCalls())
				if operation == "alias" {
					err = SyncRuntimeAlias(provider, Info{ID: b.ID, SessionName: "sky", Alias: "new-alias"})
				} else {
					err = NewManagerWithOptions(store, provider).SuspendForShutdown(b.ID)
				}
				if owner == "unreadable" {
					if !errors.Is(err, failure) {
						t.Fatalf("error = %v, want identity read failure", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				allowed := owner == "same" || owner == "unowned"
				if !allowed {
					for _, call := range sp.SnapshotCalls()[firstCall:] {
						if call.Method == "SetMeta" || call.Method == "RemoveMeta" || call.Method == "Stop" {
							t.Errorf("unowned operation mutated runtime: %+v", call)
						}
					}
				}
				if operation == "shutdown" {
					if sp.IsRunning("sky") == allowed {
						t.Errorf("running = %v, allowed teardown = %v", sp.IsRunning("sky"), allowed)
					}
				} else {
					want := ""
					if allowed {
						want = "new-alias"
					}
					for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
						if got, err := sp.GetMeta("sky", key); err != nil || got != want {
							t.Errorf("%s = %q, %v; want %q", key, got, err, want)
						}
					}
				}
			})
		}
	}
}

func TestSuspendForShutdownClosedRuntimeAbsent(t *testing.T) {
	failure := errors.New("runtime identity unavailable")
	for _, tc := range []struct {
		name    string
		readErr error
		wantErr bool
	}{
		{"typed absence", runtime.ErrSessionNotFound, false},
		{"legacy session absence", errors.New("session not found"), false},
		{"legacy server absence", errors.New("no tmux server running"), false},
		{"mixed backend failure", errors.Join(runtime.ErrSessionNotFound, failure), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{"session_name": "sky"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
			sp := runtime.NewFake()
			provider := identityReadErrorProvider{Fake: sp, err: tc.readErr}
			err = NewManagerWithOptions(store, provider).SuspendForShutdown(b.ID)
			if tc.wantErr {
				if !errors.Is(err, failure) {
					t.Fatalf("error = %v, want backend failure", err)
				}
			} else if err != nil {
				t.Fatalf("absent runtime shutdown = %v, want nil", err)
			}
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "SetMeta" || call.Method == "RemoveMeta" || call.Method == "Stop" {
					t.Errorf("identity read failure caused runtime mutation: %+v", call)
				}
			}
		})
	}
}
