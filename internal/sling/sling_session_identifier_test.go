package sling

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

type fuzzySessionOwnerStore struct {
	beads.Store
	identifier string
	ownerID    string
}

func (s fuzzySessionOwnerStore) Get(id string) (beads.Bead, error) {
	if id == s.identifier {
		return s.Store.Get(s.ownerID)
	}
	return s.Store.Get(id)
}

func TestDoSling_PoolSessionIdentifierOwner(t *testing.T) {
	lookupErr := errors.New("session alias index unavailable")
	for _, tc := range []struct {
		name, identifier, alias, runtime, template string
		closed, ambiguous, fuzzy, generated        bool
		wantOwn                                    bool
		wantErr                                    error
	}{
		{name: "current alias", identifier: "named-seat", alias: "named-seat", wantOwn: true},
		{name: "runtime name", identifier: "runtime-seat", runtime: "runtime-seat", wantOwn: true},
		{name: "foreign alias", identifier: "named-seat", alias: "named-seat", template: "myrig/other"},
		{name: "foreign runtime", identifier: "runtime-seat", runtime: "runtime-seat", template: "myrig/other"},
		{name: "closed alias", identifier: "named-seat", alias: "named-seat", closed: true},
		{name: "closed runtime", identifier: "runtime-seat", runtime: "runtime-seat", closed: true},
		{name: "historical alias", identifier: "old-seat", alias: "new-seat"},
		{name: "deleted alias", identifier: "old-seat"},
		{name: "ambiguous alias", identifier: "named-seat", alias: "named-seat", ambiguous: true, wantErr: session.ErrAmbiguous},
		{name: "ambiguous runtime", identifier: "runtime-seat", runtime: "runtime-seat", ambiguous: true, wantErr: session.ErrAmbiguous},
		{name: "alias lookup error", identifier: "named-seat", alias: "named-seat", wantErr: lookupErr},
		{name: "fuzzy ID", identifier: "gc-owner", fuzzy: true},
		{name: "generated runtime fallback", fuzzy: true, generated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, deps, store, work := reassignTestSetup(t, tc.identifier)
			template := tc.template
			if template == "" {
				template = opts.Target.QualifiedName()
			}
			owner, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"template": template, "state": "active", "alias": tc.alias, "session_name": tc.runtime, "alias_history": `["old-seat"]`,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.closed {
				if err := store.Close(owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.ambiguous {
				if _, err := store.Create(beads.Bead{Type: owner.Type, Metadata: owner.Metadata}); err != nil {
					t.Fatal(err)
				}
			}
			if errors.Is(tc.wantErr, lookupErr) {
				deps.Store = listErrStore{Store: store, err: lookupErr}
			}
			identifier := tc.identifier
			if tc.generated {
				identifier = "s-" + owner.ID
			}
			if tc.fuzzy {
				deps.Store = fuzzySessionOwnerStore{Store: store, identifier: identifier, ownerID: owner.ID}
			}
			status := "in_progress"
			if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &identifier, Metadata: map[string]string{"gc.routed_to": opts.Target.QualifiedName()}}); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			opts.Nudge, opts.NoConvoy = true, true
			router := &fakeBeadRouter{}
			deps.Router = router
			result, err := DoSling(opts, deps, store)
			if tc.wantOwn {
				if err != nil || !result.Idempotent || result.NudgeAgent == nil {
					t.Errorf("valid owner sling=%+v, %v; want idempotent nudge", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), identifier) || result.NudgeAgent != nil {
				t.Errorf("invalid owner sling=%+v, %v; want named refusal", result, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error=%v, want %v", err, tc.wantErr)
			}
			after, getErr := store.Get(work.ID)
			if getErr != nil || !reflect.DeepEqual(before, after) || len(router.routed) != 0 {
				t.Errorf("claim changed: before=%+v after=%+v err=%v routes=%v", before, after, getErr, router.routed)
			}
		})
	}
}
