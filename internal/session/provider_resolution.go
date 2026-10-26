package session

import "strings"

// ProviderResolutionIdentity returns the canonical template and provider to
// resolve. A provider-backed session has no agent template; a missing provider
// falls back to its persisted template name.
func ProviderResolutionIdentity(template, provider string, metadata map[string]string) (string, string) {
	template, provider = strings.TrimSpace(template), strings.TrimSpace(provider)
	if !UseAgentTemplateForProviderResolution(metadata["real_world_app_session_kind"], metadata, provider, "", true) {
		if provider == "" {
			provider = template
		}
		template = ""
	}
	return template, provider
}

// ProviderResolutionIdentity is the Info projection of the persisted identity
// rule, including manual sessions and explicit provider/agent session kinds.
func (i Info) ProviderResolutionIdentity() (string, string) {
	metadata := map[string]string{
		"real_world_app_session_kind": i.ProviderSessionKind,
		"session_origin":              i.SessionOrigin,
		"agent_name":                  i.AgentName,
	}
	if i.ConfiguredNamedSession {
		metadata[NamedSessionMetadataKey] = "true"
	}
	return ProviderResolutionIdentity(i.Template, i.Provider, metadata)
}

// UseAgentTemplateForProviderResolution reports whether a session should
// resolve provider options through its agent template instead of treating the
// persisted Template field as a raw provider name. The provider-name arguments
// are accepted for call-site symmetry but do not disqualify non-manual legacy
// sessions when the agent template still exists.
func UseAgentTemplateForProviderResolution(sessionKind string, metadata map[string]string, _, _ string, templateFound bool) bool {
	sessionKind = strings.TrimSpace(sessionKind)
	switch sessionKind {
	case "provider":
		return false
	case "agent":
		return true
	}
	if metadata == nil {
		return true
	}
	if strings.TrimSpace(metadata["agent_name"]) != "" ||
		strings.TrimSpace(metadata[NamedSessionMetadataKey]) == "true" {
		return true
	}
	if strings.TrimSpace(metadata["session_origin"]) == "manual" {
		return false
	}
	return templateFound
}
