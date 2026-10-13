package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/federation"
)

// TestHookCommandClaimCrossCityFenceUsesFederationIdentity drives the real
// `gc hook --claim` command (cmdHookWithOptions) against a fake bd, so the
// production wiring of hookClaimOptions.FederationIdentity is what is under
// test — not a value injected into doHookClaim. The identity is city.toml
// [federation] identity = "jadegate", while the workspace is named
// "test-city" and the city directory is a t.TempDir leaf: a fence keyed off the
// workspace name or the directory would refuse the own case (wrong identity) or
// claim the foreign case. The last row pins the opt-in: with no [federation]
// table the city is not federated and a foreign-labeled bead is claimed.
//
// Two foreign rows, because the fence has two layers that meet the production
// wiring differently since gf-xid finding 1h: the GENERATED work query filters
// a foreign-owned row out of every tier before the hook ever sees it (the
// identity reaches the generated jq; the session drains no_work with no claim
// write and no hook-level refusal line, because nothing was refused by the
// hook), while a custom work_query is returned verbatim, so its foreign row
// reaches the hook and the hook's own fence refuses and LOGS it with the
// [federation] identity.
func TestHookCommandClaimCrossCityFenceUsesFederationIdentity(t *testing.T) {
	const (
		thisIdentity = "jadegate"
		foreign      = "citadel"
	)
	ownerLabel := func(identity string) string {
		label, _ := federation.OwnerLabel(identity)
		return label
	}
	cases := []struct {
		name       string
		beadID     string
		labels     []string
		federated  bool
		wantClaim  bool
		workQuery  string // a custom work_query: returned verbatim, so its rows reach the hook unfiltered
		wantRefuse bool   // the hook's own refusal line is expected on stderr
	}{
		{name: "foreign owner filtered by the generated read", beadID: "hw-foreign", labels: []string{ownerLabel(foreign)}, federated: true, wantClaim: false},
		{
			name: "foreign owner refused by the hook behind a custom work_query", beadID: "hw-foreign", labels: []string{ownerLabel(foreign)}, federated: true, wantClaim: false,
			workQuery: "bd ready --json --metadata-field gc.routed_to=worker", wantRefuse: true,
		},
		{name: "own identity claimed", beadID: "jg-own", labels: []string{ownerLabel(thisIdentity)}, federated: true, wantClaim: true},
		{name: "handoff to this identity claimed", beadID: "hw-handed", labels: []string{ownerLabel(foreign), federation.HandoffLabel(thisIdentity)}, federated: true, wantClaim: true},
		{name: "legacy unlabeled claimed", beadID: "hw-legacy", labels: nil, federated: true, wantClaim: true},
		{name: "not federated: foreign owner claimed, fence off", beadID: "hw-foreign", labels: []string{ownerLabel(foreign)}, federated: false, wantClaim: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			cityDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
			if tc.workQuery != "" {
				cityToml += fmt.Sprintf("work_query = %q\n", tc.workQuery)
			}
			if tc.federated {
				cityToml += fmt.Sprintf("\n[federation]\nidentity = %q\n", thisIdentity)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}

			labels, err := json.Marshal(tc.labels)
			if err != nil {
				t.Fatal(err)
			}
			if tc.labels == nil {
				labels = []byte("[]")
			}
			row := func(status, assignee string) string {
				return fmt.Sprintf(`{"id":%q,"status":%q,"assignee":%q,"issue_type":"task","labels":%s,"metadata":{%q:"worker"}}`,
					tc.beadID, status, assignee, labels, beadmeta.RoutedToMetadataKey)
			}
			fakeBin := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "bd.log")
			script := fmt.Sprintf(`#!/bin/sh
printf 'args=%%s\n' "$*" >> %q
case "$*" in
  *"update %s --claim --json"*)
    printf '[%s]' ;;
  *"show --json %s"*)
    printf '[%s]' ;;
  *"query --json ephemeral=true AND status=open --limit 0"*)
    printf '[]' ;;
  *"gc.routed_to=worker"*)
    printf '[%s]' ;;
  *)
    printf '[]' ;;
esac
`, logPath, tc.beadID, row("in_progress", "worker-1"), tc.beadID, row("in_progress", "worker-1"), row("open", ""))
			if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_ALIAS", "worker-1")
			t.Setenv("GC_SESSION_ID", "session-id-1")
			t.Setenv("GC_SESSION_NAME", "test-city--worker-1")
			t.Setenv("GC_SESSION_ORIGIN", "ephemeral")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			logData, _ := os.ReadFile(logPath)
			claimWrite := fmt.Sprintf("args=update %s --claim --json", tc.beadID)

			if tc.wantClaim {
				if code != 0 || result.Action != "work" || result.BeadID != tc.beadID {
					t.Fatalf("labels %q federated=%v: code=%d result=%+v, want the bead claimed as work\nstderr: %s", tc.labels, tc.federated, code, result, stderr.String())
				}
				if !strings.Contains(string(logData), claimWrite) {
					t.Fatalf("labels %q: the claim never reached bd; log:\n%s", tc.labels, logData)
				}
				if strings.Contains(stderr.String(), "cross-city-fence refused") {
					t.Fatalf("labels %q: a claimable bead logged a refusal:\n%s", tc.labels, stderr.String())
				}
				return
			}
			// Without --drain-ack a drain is exit 1 with the structured no_work record.
			if code != 1 || result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
				t.Fatalf("labels %q: code=%d result=%+v, want a no_work drain\nstderr: %s", tc.labels, code, result, stderr.String())
			}
			if strings.Contains(string(logData), claimWrite) {
				t.Fatalf("REGRESSION jg-66rdw8: bd received a claim write for foreign bead %s; log:\n%s", tc.beadID, logData)
			}
			if !tc.wantRefuse {
				// The generated read filtered the row before the hook (finding
				// 1h): nothing reached the hook's fence, so nothing was refused
				// there — the read ran, though, which the fake bd's log shows.
				if strings.Contains(stderr.String(), "cross-city-fence refused") {
					t.Fatalf("the generated read must filter the foreign row before the hook, not hand it to the hook's fence:\n%s", stderr.String())
				}
				if !strings.Contains(string(logData), "gc.routed_to=worker") {
					t.Fatalf("the routed read never ran; log:\n%s", logData)
				}
				return
			}
			for _, want := range []string{
				"cross-city-fence refused",
				"bead=" + tc.beadID,
				"owner=" + foreign,
				"this_identity=" + thisIdentity, // the [federation] identity, not the workspace name or the directory
				"missing=" + federation.HandoffLabel(thisIdentity),
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
				}
			}
		})
	}
}
