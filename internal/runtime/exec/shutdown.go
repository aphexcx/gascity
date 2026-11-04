package exec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/runtime"
)

type shutdownProvider struct {
	*Provider
	name         string
	cityPath     string
	sessionID    string
	metadataFree bool
	unsupported  bool
}

// BindForShutdown binds a configured script runtime by name. Scripts that keep
// no ownership metadata retain their name-based shutdown contract; partial,
// foreign, or changed metadata is rejected before every shutdown mutation.
func (p *Provider) BindForShutdown(name, cityPath, sessionID string) (runtime.Provider, error) {
	city, id, unsupported, err := p.shutdownOwnership(name)
	if err != nil {
		return nil, fmt.Errorf("reading exec runtime %q shutdown ownership: %w", name, err)
	}
	bound := &shutdownProvider{Provider: p, name: name, cityPath: cityPath, sessionID: sessionID, metadataFree: city == "" && id == "", unsupported: unsupported}
	if err := bound.verifyOwnership(name, city, id); err != nil {
		return nil, err
	}
	return bound, nil
}

func (p *Provider) shutdownOwnership(name string) (string, string, bool, error) {
	city, cityErr := p.runStrictWithContext(context.Background(), p.timeout, nil, "get-meta", name, "GC_CITY_PATH")
	id, idErr := p.runStrictWithContext(context.Background(), p.timeout, nil, "get-meta", name, "GC_SESSION_ID")
	// get-meta is optional. Only two unsupported reads establish that mode;
	// mixed responses and ordinary failures remain errors.
	if errors.Is(cityErr, errUnsupportedOperation) && errors.Is(idErr, errUnsupportedOperation) {
		return "", "", true, nil
	}
	return city, id, false, errors.Join(cityErr, idErr)
}

func (p *shutdownProvider) verifyOwnership(name, city, id string) error {
	if name != p.name || p.cityPath == "" {
		return fmt.Errorf("preserving exec runtime %q: invalid shutdown binding", name)
	}
	if p.metadataFree && city == "" && id == "" {
		return nil
	}
	if p.metadataFree || city == "" || id == "" || filepath.Clean(city) != filepath.Clean(p.cityPath) || id != p.sessionID {
		return fmt.Errorf("preserving exec runtime %q: shutdown ownership changed or is incomplete", name)
	}
	return nil
}

func (p *shutdownProvider) verify(name string) error {
	city, id, unsupported, err := p.shutdownOwnership(name)
	if err != nil {
		return fmt.Errorf("rechecking exec runtime %q shutdown ownership: %w", name, err)
	}
	if unsupported != p.unsupported {
		return fmt.Errorf("preserving exec runtime %q: shutdown metadata support changed", name)
	}
	return p.verifyOwnership(name, city, id)
}

// Stop verifies the binding before invoking the script's stop operation.
func (p *shutdownProvider) Stop(name string) error {
	if err := p.verify(name); err != nil {
		return err
	}
	return p.Provider.Stop(name)
}

// Interrupt verifies the binding before invoking the script's interrupt operation.
func (p *shutdownProvider) Interrupt(name string) error {
	if err := p.verify(name); err != nil {
		return err
	}
	return p.Provider.Interrupt(name)
}

// SendKeys verifies the binding before sending cancellation keys.
func (p *shutdownProvider) SendKeys(name string, keys ...string) error {
	if err := p.verify(name); err != nil {
		return err
	}
	return p.Provider.SendKeys(name, keys...)
}
