package subprocess

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

const processIdentityMetaKey = "gc_process_identity"

type processIdentity struct {
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

// UnmarshalJSON distinguishes a missing start token from the explicit empty
// token recorded when the child has already exited during Start.
func (r *processIdentity) UnmarshalJSON(data []byte) error {
	var wire struct {
		Name  string  `json:"name"`
		PID   int     `json:"pid"`
		Start *string `json:"start"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Start == nil {
		return errors.New("missing recorded process start token")
	}
	*r = processIdentity{Name: wire.Name, PID: wire.PID, Start: *wire.Start}
	return nil
}

func (p *Provider) readProcessIdentity(name string) (*processIdentity, error) {
	data, err := os.ReadFile(p.metaPath(name, processIdentityMetaKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading subprocess %q process identity: %w", name, err)
	}
	var record processIdentity
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decoding subprocess %q process identity: %w", name, err)
	}
	if record.Name != name || record.PID <= 1 {
		return nil, fmt.Errorf("invalid subprocess %q process identity", name)
	}
	return &record, nil
}

func (p *Provider) processRunning(name string) (bool, error) {
	record, err := p.readProcessIdentity(name)
	if err != nil || record == nil {
		return false, err
	}
	return proctable.ProcessIdentityMatches(record.PID, record.Start, p.ops.processIdentity)
}

func (p *Provider) persistProcessIdentity(name string, pid int) error {
	start, err := p.ops.processIdentity(pid)
	if err != nil && !errors.Is(err, proctable.ErrProcessGone) {
		return err
	}
	if err == nil && start == "" {
		return fmt.Errorf("empty process identity for PID %d", pid)
	}
	data, err := json.Marshal(processIdentity{Name: name, PID: pid, Start: start})
	if err != nil {
		return err
	}
	return p.SetMeta(name, processIdentityMetaKey, string(data))
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
		if !strings.HasSuffix(entry.Name(), ".meta."+metaFileKey(processIdentityMetaKey)) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.dir, entry.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var record processIdentity
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decoding subprocess process identity: %w", err)
		}
		if record.Name == "" || record.PID <= 1 || entry.Name() != filepath.Base(p.metaPath(record.Name, processIdentityMetaKey)) {
			return nil, fmt.Errorf("invalid subprocess process identity in %s", entry.Name())
		}
		if !strings.HasPrefix(record.Name, prefix) || seen[record.Name] {
			continue
		}
		live, err := proctable.ProcessIdentityMatches(record.PID, record.Start, p.ops.processIdentity)
		if err != nil {
			return nil, err
		}
		if live {
			names = append(names, record.Name)
		}
	}
	return names, nil
}

func (p *Provider) stopRecordedProcess(name string) error {
	record, err := p.readProcessIdentity(name)
	if err != nil || record == nil {
		return err
	}
	if err := proctable.TerminateProcessGroup(record.PID, record.Start, p.ops.processIdentity, p.ops.signal); err != nil {
		return fmt.Errorf("stopping subprocess %q: %w", name, err)
	}
	current, err := p.readProcessIdentity(name)
	if err != nil {
		return err
	}
	if current != nil && *current == *record {
		p.clearSessionMeta(name)
	}
	return nil
}

// ShutdownDiagnostic describes a legacy record whose child cannot be identified.
func (p *Provider) ShutdownDiagnostic(name string) error {
	record, err := p.readProcessIdentity(name)
	if err != nil {
		return err
	}
	if record == nil && !p.socketAlive(name) {
		return fmt.Errorf("session %q has no recorded process identity; restart this session under the current gc binary to record its child identity", name)
	}
	return nil
}

func (p *Provider) recordedOwnership(name string) error {
	if sc := p.procs[name]; sc != nil && sc.alive() {
		return nil
	}
	record, recordErr := p.readProcessIdentity(name)
	if err := p.sendSocketCommand(name, "ping", 500*time.Millisecond); err == nil {
		return nil
	} else if !isUnavailableSocketError(err) {
		return err
	}
	if recordErr != nil || record == nil {
		return recordErr
	}
	live, err := proctable.ProcessIdentityMatches(record.PID, record.Start, p.ops.processIdentity)
	if err != nil {
		return err
	}
	if !live {
		return runtime.ErrSessionNotFound
	}
	return nil
}
