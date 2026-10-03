package config

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestSingleStoreDemandExcludesShippedBeforeLimit(t *testing.T) {
	for _, source := range []string{"routed", "migration", "ephemeral", "anchor"} {
		t.Run(source, func(t *testing.T) {
			rows := make([]map[string]any, 0, 47)
			for i := 0; i < 47; i++ {
				meta := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
				if source == "migration" {
					meta = map[string]string{beadmeta.RunTargetMetadataKey: "worker", beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
				}
				if i < 25 {
					meta[beadmeta.WorkOutcomeMetadataKey] = beadmeta.WorkOutcomeShipped
				}
				rows = append(rows, map[string]any{"id": fmt.Sprintf("work-%02d", i), "status": "open", "assignee": "", "metadata": meta})
			}
			payload, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"DEMAND_SOURCE": source, "DEMAND_ROWS": string(payload), "GC_SESSION_ORIGIN": "ephemeral"}
			if source == "anchor" {
				env["GC_SESSION_NAME"] = "worker-session"
			}
			a := Agent{Name: "worker"}
			out := runEffectiveWorkQuery(t, a, env, shippedDemandFakeBD)
			ids := workQueryOutputIDOrder(t, out)
			wantCount := 20
			if source == "migration" || source == "ephemeral" {
				wantCount = 1
			}
			if len(ids) != wantCount || ids[0] != "work-25" || ids[len(ids)-1] != fmt.Sprintf("work-%02d", 24+wantCount) {
				t.Fatalf("candidate order = %v, want %d pending rows starting at work-25", ids, wantCount)
			}
			if source != "anchor" {
				count := runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), env, shippedDemandFakeBD)
				if count != "22\n" {
					t.Errorf("demand count = %q, want 22", count)
				}
			}
			rows[0]["metadata"].(map[string]string)[beadmeta.WorkOutcomeMetadataKey] = ""
			payload, err = json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			env["DEMAND_ROWS"] = string(payload)
			out = runEffectiveWorkQuery(t, a, env, shippedDemandFakeBD)
			if ids = workQueryOutputIDOrder(t, out); len(ids) == 0 || ids[0] != "work-00" {
				t.Errorf("cleared outcome candidates = %v, want work-00 first", ids)
			}
		})
	}
}

func TestSingleStoreShippedInProgressAssignmentRemainsVisible(t *testing.T) {
	out := runEffectiveWorkQuery(t, Agent{Name: "worker"}, map[string]string{
		"GC_SESSION_NAME": "worker-session", "GC_SESSION_ORIGIN": "ephemeral", "DEMAND_SOURCE": "recovery",
		"DEMAND_ROWS": `[{"id":"owned","status":"in_progress","assignee":"worker-session","metadata":{"gc.work_outcome":"shipped"}}]`,
	}, shippedDemandFakeBD)
	ids := workQueryOutputIDOrder(t, out)
	if len(ids) != 1 || ids[0] != "owned" {
		t.Fatalf("in-progress candidates = %v, want owned", ids)
	}
}

func TestSingleStoreDemandPreservesReaderFailure(t *testing.T) {
	result := runGeneratedQueryWithBD(t, (&Agent{Name: "worker"}).EffectivePoolDemandQuery(), nil, fakeBDEmpty,
		"#!/bin/sh\nprintf 'demand backend unavailable\\n' >&2\nexit 23\n")
	if result.exit != 23 || result.stderr != "demand backend unavailable\n" {
		t.Fatalf("reader failure = %+v, want exit 23 and original stderr", result)
	}
}

func TestSingleStoreMalformedDemandRemainsVisible(t *testing.T) {
	const malformedBD = "#!/bin/sh\ncase \"$1\" in ready) printf 'invalid-json' ;; *) printf '[]' ;; esac\n"
	a := Agent{Name: "worker"}
	out := runEffectiveWorkQuery(t, a, nil, malformedBD)
	if out != "invalid-json" {
		t.Errorf("work query = %q, want malformed reader payload for hook rejection", out)
	}
	result := runGeneratedQueryWithBD(t, a.EffectivePoolDemandQuery(), nil, fakeBDEmpty, malformedBD)
	if result.exit == 0 || result.stdout != "" || result.stderr == "" {
		t.Errorf("malformed demand count = %+v, want visible failure without a count", result)
	}
}

const shippedDemandFakeBD = `#!/bin/sh
set -eu
command="$1"
shift
args="$*"
limit=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --limit) shift; limit="$1" ;;
    --limit=*) limit="${1#--limit=}" ;;
  esac
  shift
done
emit=0
case "$command:$DEMAND_SOURCE" in
  ready:routed)
    case "$args" in *"--metadata-field gc.routed_to=worker"*) emit=1 ;; esac ;;
  ready:migration)
    case "$args" in *"--metadata-field gc.run_target=worker"*) emit=1 ;; esac ;;
  query:ephemeral)
    case "$args" in *"status=open"*) emit=1 ;; esac ;;
  ready:anchor)
    case "$args" in *"--metadata-field gc.root_bead_id=workflow-root"*) emit=1 ;; esac ;;
  list:anchor)
    printf '[{"id":"workflow-root","status":"in_progress","assignee":"worker-session","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"worker"}}]'
    exit 0 ;;
  list:recovery) emit=1 ;;
  show:*) printf '[{"dependencies":[]}]'; exit 0 ;;
esac
if [ "$emit" -eq 1 ]; then
  printf '%s' "$DEMAND_ROWS" | jq --argjson limit "$limit" 'if $limit > 0 then .[:$limit] else . end'
else
  printf '[]'
fi
`
