package session

import (
	"encoding/json"
	"fmt"
)

// StartupContentMetadataKey holds the delivery repair record for one start.
const StartupContentMetadataKey = "startup_content"

// StartupContentSource identifies accepted bytes exposed by a locked import.
type StartupContentSource struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	Import  string `json:"import"`
	Scripts bool   `json:"scripts,omitempty"`
}

// StartupContent records a start that needs repair when accepted bytes return.
// Receipt is the transient PreStart result channel, retained until committed.
type StartupContent struct {
	Sources  []StartupContentSource `json:"sources,omitempty"`
	Skipped  bool                   `json:"skipped,omitempty"`
	Reason   string                 `json:"reason,omitempty"`
	Receipt  string                 `json:"receipt,omitempty"`
	Pending  bool                   `json:"pending,omitempty"`
	Baseline StartupContentBaseline `json:"baseline,omitempty"`
}

// StartupContentBaseline preserves the start's hashes across a supervisor crash.
// It contains no environment values or rendered prompts.
type StartupContentBaseline struct {
	Core      string `json:"core"`
	Provision string `json:"provision"`
	Launch    string `json:"launch"`
	Breakdown string `json:"breakdown"`
}

// DecodeStartupContent reads a persisted startup delivery record.
func DecodeStartupContent(value string) (StartupContent, error) {
	var result StartupContent
	if value != "" {
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return result, fmt.Errorf("decoding startup content: %w", err)
		}
	}
	return result, nil
}

// StartupContentPatch replaces the previous incarnation's delivery record.
func StartupContentPatch(content StartupContent) MetadataPatch {
	value := ""
	if len(content.Sources) > 0 || content.Skipped || content.Receipt != "" || content.Pending {
		// This closed type contains only strings and booleans; Marshal cannot fail.
		data, _ := json.Marshal(content)
		value = string(data)
	}
	return MetadataPatch{StartupContentMetadataKey: value}
}

// RecordStartupContent persists delivery facts before a runtime starts, so a
// supervisor crash cannot lose the pending PreStart result channel.
func (s *Store) RecordStartupContent(info Info, content StartupContent) (Info, error) {
	return s.UpdateMetadataInfo(info, StartupContentPatch(content))
}

// StartupContentBaselinePatch commits a delivery record with the hashes of the
// start that produced it. A clean delivery only clears the transient record.
func StartupContentBaselinePatch(content StartupContent) MetadataPatch {
	patch := StartupContentPatch(content)
	if (len(content.Sources) > 0 || content.Skipped) && content.Baseline.Core != "" {
		patch["started_config_hash"] = content.Baseline.Core
		patch["started_provision_hash"] = content.Baseline.Provision
		patch["started_launch_hash"] = content.Baseline.Launch
		patch["core_hash_breakdown"] = content.Baseline.Breakdown
	}
	return patch
}

// CommitStartupContent recovers delivery facts after spawn but before the
// ordinary start commit. The session's lifecycle state is left intact.
func (s *Store) CommitStartupContent(info Info, content StartupContent) (Info, error) {
	return s.UpdateMetadataInfo(info, StartupContentBaselinePatch(content))
}
