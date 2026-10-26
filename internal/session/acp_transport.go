package session

import (
	"fmt"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func storedACPKeys(metadata map[string]string) []string {
	var keys []string
	for _, key := range []string{MCPIdentityMetadataKey, MCPServersSnapshotMetadataKey} {
		if strings.TrimSpace(metadata[key]) != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// ResolveMCPTransport applies the provider gate to legacy MCP evidence. The
// boolean reports presence of that evidence, even when it selects no ACP route.
func ResolveMCPTransport(configuredTransport, identity, snapshot string) (string, bool) {
	if strings.TrimSpace(identity) == "" && strings.TrimSpace(snapshot) == "" {
		return "", false
	}
	if strings.TrimSpace(configuredTransport) == "acp" {
		return "acp", true
	}
	return "", true
}

// ResumeInfoForTransport discards a legacy ACP launch shape when the resolved
// transport is no longer ACP. Callers rebuild command and resume hints from the
// current provider before handing them to the session start boundary.
func ResumeInfoForTransport(info Info, transport string, metadata map[string]string) Info {
	if transport != "acp" && len(storedACPKeys(metadata)) > 0 {
		info.Command = ""
		info.ResumeCommand = ""
		info.ResumeFlag = ""
		info.ResumeStyle = ""
	}
	return info
}

// liveACPTransport is runtime evidence independent of persisted MCP keys. A
// config reload does not change the transport of a process already running.
func (m *Manager) liveACPTransport(name string) bool {
	detector, ok := m.sp.(transportDetector)
	return ok && detector.DetectTransport(name) == "acp"
}

func resolvedStartProvider(b beads.Bead, cfg runtime.Config) string {
	for _, name := range []string{cfg.ProviderOverlayName, cfg.ProviderName, b.Metadata["provider"]} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return "unknown"
}

// clearIncompatibleACPMetadata runs under the session operation lock before a
// start. The persisted clear makes a config-change migration idempotent and
// records it through the normal bead history, including when startup fails.
func (m *Manager) clearIncompatibleACPMetadata(id string, b *beads.Bead, name string, cfg runtime.Config) error {
	if m.transportResolver == nil || len(storedACPKeys(b.Metadata)) == 0 ||
		transportFromMetadata(*b) == "acp" ||
		m.resolveConfiguredTransport(ProviderResolutionIdentity(b.Metadata["template"], b.Metadata["provider"], b.Metadata)) == "acp" ||
		m.liveACPTransport(name) {
		return nil
	}
	patch := map[string]string{
		"command": "", "resume_command": "", "resume_flag": "", "resume_style": "",
	}
	if m.resumeConfigResolver != nil {
		command, resume, err := m.resumeConfigResolver(m.infoFromBead(*b), b.Metadata)
		if err != nil {
			return fmt.Errorf("resolving non-ACP resume settings: %w", err)
		}
		patch["command"] = command
		patch["resume_command"] = resume.ResumeCommand
		patch["resume_flag"] = resume.ResumeFlag
		patch["resume_style"] = resume.ResumeStyle
		patch["session_id_flag"] = resume.SessionIDFlag
	}
	ClearStoredMCPMetadata(patch)
	if err := m.store.SetMetadataBatch(id, patch); err != nil {
		return fmt.Errorf("clearing incompatible ACP keys for provider %q: %w", resolvedStartProvider(*b, cfg), err)
	}
	for key, value := range patch {
		b.Metadata[key] = value
	}
	if router, ok := m.sp.(acpRouteRegistrar); ok {
		router.Unroute(name)
	}
	log.Printf("session %s: ignoring ACP keys on a non-ACP provider %q", id, resolvedStartProvider(*b, cfg))
	return nil
}

func sessionStartError(operation string, b beads.Bead, cfg runtime.Config, transport string, err error) error {
	if transport != "acp" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	keys := storedACPKeys(b.Metadata)
	if transportFromMetadata(b) == "acp" {
		keys = []string{"transport/provider"}
	} else if len(keys) == 0 {
		keys = []string{"configured/live transport"}
	}
	return fmt.Errorf("%s (resolved provider %q, ACP selected by %s): %w", operation, resolvedStartProvider(b, cfg), strings.Join(keys, ", "), err)
}
