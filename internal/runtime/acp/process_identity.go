package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

type providerOps struct {
	processIdentity func(int) (string, error)
	signal          func(int, syscall.Signal) error
	beforeCleanup   func()
}

func (p *Provider) validateStopOwnership(name string) error {
	return runtime.WithProviderStateDirLock(p.dir, func() error {
		owner, err := p.readProcessOwnership(name)
		if err != nil {
			return err
		}
		if owner != nil && (owner.CityPath == "") != (owner.SessionID == "") {
			return fmt.Errorf("preserving session %q: incomplete ownership", name)
		}
		return nil
	})
}

// UnmarshalJSON accepts legacy ownership without a process identity, but
// requires an explicit start token once a PID or process name is present.
func (o *sessionOwnership) UnmarshalJSON(data []byte) error {
	var wire struct {
		CityPath  string  `json:"city_path"`
		SessionID string  `json:"session_id"`
		Name      string  `json:"name"`
		PID       int     `json:"pid"`
		Start     *string `json:"start"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if (wire.PID != 0 || wire.Name != "") && wire.Start == nil {
		return errors.New("missing recorded process start token")
	}
	*o = sessionOwnership{CityPath: wire.CityPath, SessionID: wire.SessionID, Name: wire.Name, PID: wire.PID}
	if wire.Start != nil {
		o.Start = *wire.Start
	}
	return nil
}

func (p *Provider) readProcessOwnership(name string) (*sessionOwnership, error) {
	data, err := p.GetMeta(name, sessionOwnershipMetaKey)
	if err != nil || data == "" {
		return nil, err
	}
	var owner sessionOwnership
	if err := json.Unmarshal([]byte(data), &owner); err != nil {
		return nil, fmt.Errorf("decoding ACP %q ownership: %w", name, err)
	}
	if err := validateProcessOwnership(name, owner); err != nil {
		return nil, err
	}
	return &owner, nil
}

func validateProcessOwnership(name string, owner sessionOwnership) error {
	if owner.PID == 0 && owner.Start == "" && owner.Name == "" {
		return nil
	}
	if owner.Name != name || owner.PID <= 1 {
		return fmt.Errorf("invalid ACP %q process identity", name)
	}
	return nil
}

func (p *Provider) processRunning(name string) (bool, error) {
	owner, err := p.readProcessOwnership(name)
	if err != nil || owner == nil || owner.PID == 0 {
		return false, err
	}
	return proctable.ProcessGroupIdentityMatches(owner.PID, owner.Start, p.ops.processIdentity, p.ops.signal)
}

func (p *Provider) publishProcessOwnership(name string, env map[string]string, pid int) error {
	start, err := p.ops.processIdentity(pid)
	if err != nil && !errors.Is(err, proctable.ErrProcessGone) {
		return err
	}
	if err == nil && start == "" {
		return fmt.Errorf("empty process identity for PID %d", pid)
	}
	data, err := json.Marshal(sessionOwnership{CityPath: env["GC_CITY_PATH"], SessionID: env["GC_SESSION_ID"], Name: name, PID: pid, Start: start})
	if err != nil {
		return err
	}
	return p.setMetaLocked(name, sessionOwnershipMetaKey, string(data))
}

func (p *Provider) listRecordedProcesses(prefix string, seen map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(p.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".meta."+metaFileKey(sessionOwnershipMetaKey)) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.dir, entry.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		var owner sessionOwnership
		if err := json.Unmarshal(data, &owner); err != nil {
			return nil, fmt.Errorf("decoding ACP process identity: %w", err)
		}
		if owner.PID == 0 && owner.Start == "" && owner.Name == "" {
			continue
		}
		if owner.Name == "" || owner.PID <= 1 || entry.Name() != filepath.Base(p.metaPath(owner.Name, sessionOwnershipMetaKey)) {
			return nil, fmt.Errorf("invalid ACP process identity in %s", entry.Name())
		}
		if !strings.HasPrefix(owner.Name, prefix) || seen[owner.Name] {
			continue
		}
		live, err := proctable.ProcessGroupIdentityMatches(owner.PID, owner.Start, p.ops.processIdentity, p.ops.signal)
		if err != nil {
			return nil, err
		}
		if live {
			names = append(names, owner.Name)
		}
	}
	return names, nil
}

func (p *Provider) stopProcessGeneration(name string, owner *sessionOwnership) error {
	if owner == nil || owner.PID == 0 {
		return nil
	}
	if err := proctable.TerminateProcessGroup(owner.PID, owner.Start, p.ops.processIdentity, p.ops.signal); err != nil {
		return fmt.Errorf("stopping ACP session %q: %w", name, err)
	}
	return p.cleanupProcessGeneration(name, owner)
}

func (p *Provider) cleanupProcessGeneration(name string, owner *sessionOwnership) error {
	if owner == nil {
		return nil
	}
	return runtime.WithProviderStateDirLock(p.dir, func() error {
		current, err := p.readProcessOwnership(name)
		if err != nil || current == nil {
			return err
		}
		if *current != *owner {
			return fmt.Errorf("preserving replacement ACP session %q during cleanup", name)
		}
		if owner.PID != 0 {
			if live, err := proctable.ProcessGroupIdentityMatches(owner.PID, owner.Start, p.ops.processIdentity, p.ops.signal); err != nil {
				return err
			} else if live {
				return fmt.Errorf("preserving session %q: process group remains", name)
			}
		}
		if p.ops.beforeCleanup != nil {
			p.ops.beforeCleanup()
		}
		p.cleanupMeta(name)
		for _, path := range []string{p.sockPath(name), p.sockNamePath(name)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}

// ShutdownDiagnostic describes a legacy record whose child cannot be identified.
func (p *Provider) ShutdownDiagnostic(name string) error {
	if err := p.validateStopOwnership(name); err != nil {
		return err
	}
	owner, err := p.readProcessOwnership(name)
	if err != nil {
		return err
	}
	if owner != nil && owner.PID == 0 && !p.socketAlive(name) {
		return fmt.Errorf("session %q has no recorded process identity; restart this session under the current gc binary to record its child identity", name)
	}
	return nil
}
