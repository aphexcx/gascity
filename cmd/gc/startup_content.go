package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const startupContentReceiptEnv = "GC_STARTUP_CONTENT_RECEIPT"

// lockedSource captures only inputs that contribute to this session's
// fingerprint. Prompt and overlay sources have separate delivery contracts.
func (c *fingerprintContent) lockedSource(path string, scripts bool) (sessionpkg.StartupContentSource, bool) {
	if c == nil {
		return sessionpkg.StartupContentSource{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.identity(path)
	for root := range c.roots {
		if pathWithin(root, source) {
			input := sessionpkg.StartupContentSource{Path: source, Import: root, Scripts: scripts}
			key := fmt.Sprintf("%s\x00delivery:%t", source, scripts)
			hash := c.hashes[key]
			if hash == "" {
				hash = startupSourceHash(input)
				c.hashes[key] = hash
			}
			input.Hash = hash
			return input, hash != ""
		}
	}
	return sessionpkg.StartupContentSource{}, false
}

func changedStartupSources(sources []sessionpkg.StartupContentSource) []sessionpkg.StartupContentSource {
	var changed []sessionpkg.StartupContentSource
	for _, source := range sources {
		if startupSourceHash(source) != source.Hash {
			changed = append(changed, source)
		}
	}
	return changed
}

func startupSourceHash(source sessionpkg.StartupContentSource) string {
	if source.Scripts {
		return runtime.HashPathContentExcluding(source.Path, isOperationalScript)
	}
	return runtime.HashPathContent(source.Path)
}

// prepareStartupContent persists the first delivery observation and the
// per-attempt receipt before PreStart. Receipts contain command results, never
// process liveness; the session bead owns all lasting repair state.
func prepareStartupContent(item *preparedStart, cityPath string, store *sessionpkg.Store) (retErr error) {
	if store == nil || item.candidate.info.ID == "" {
		return nil
	}
	info, err := store.Get(item.candidate.info.ID)
	if err != nil {
		return fmt.Errorf("reading startup content before launch: %w", err)
	}
	// Async commit fences compare the original attempt with current state.
	// Refresh only delivery facts, retaining its command and ownership snapshot.
	item.candidate.info.StartupContent = info.StartupContent
	item.startupContentPrevious = info.StartupContent
	content := sessionpkg.StartupContent{Sources: changedStartupSources(item.candidate.tp.StartupContentSources)}
	defer func() {
		if retErr != nil && content.Receipt != "" {
			if err := os.Remove(content.Receipt); err != nil && !os.IsNotExist(err) {
				retErr = errors.Join(retErr, fmt.Errorf("removing failed startup receipt: %w", err))
			}
		}
	}()
	for _, command := range item.cfg.PreStart {
		if !strings.Contains(command, " internal materialize-skills ") {
			continue
		}
		dir := filepath.Join(cityPath, ".gc", "runtime", "startup-content")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating startup receipt directory: %w", err)
		}
		file, err := os.CreateTemp(dir, "receipt-*.json")
		if err != nil {
			return fmt.Errorf("creating startup receipt: %w", err)
		}
		content.Receipt = file.Name()
		_, writeErr := file.WriteString("{}\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf("initializing startup receipt: %w", errors.Join(writeErr, closeErr))
		}
		item.cfg.Env = maps.Clone(item.cfg.Env)
		if item.cfg.Env == nil {
			item.cfg.Env = make(map[string]string)
		}
		item.cfg.Env[startupContentReceiptEnv] = content.Receipt
		break
	}
	if len(content.Sources) == 0 && content.Receipt == "" && item.candidate.info.StartupContent == "" {
		return nil
	}
	content.Baseline = startupContentBaseline(item.fingerprintConfig)
	content.Pending = true
	info, err = store.RecordStartupContent(item.candidate.info, content)
	if err != nil {
		return fmt.Errorf("recording startup content: %w", err)
	}
	item.candidate.info.StartupContent = info.StartupContent
	item.startupContentPrepared = true
	return nil
}

func restoreReusedStartupContent(item *preparedStart, store *sessionpkg.Store) error {
	if item.candidate.info.ID == "" {
		return nil
	}
	current, err := store.Get(item.candidate.info.ID)
	if err != nil {
		return fmt.Errorf("reading reused startup content: %w", err)
	}
	if !item.startupContentPrepared {
		item.candidate.info.StartupContent = current.StartupContent
		return nil
	}
	content, err := sessionpkg.DecodeStartupContent(item.candidate.info.StartupContent)
	if err != nil {
		return err
	}
	if current.StartupContent == item.candidate.info.StartupContent {
		current, err = store.UpdateMetadataInfo(current, sessionpkg.MetadataPatch{sessionpkg.StartupContentMetadataKey: item.startupContentPrevious})
		if err != nil {
			return fmt.Errorf("restoring reused startup content: %w", err)
		}
	}
	item.candidate.info.StartupContent = current.StartupContent
	if content.Receipt != "" {
		if err := os.Remove(content.Receipt); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing unused startup receipt: %w", err)
		}
	}
	return nil
}

func reportSkippedSkillMaterialization(cause error) error {
	path := os.Getenv(startupContentReceiptEnv)
	if path == "" {
		return nil // standalone invocations have no controller start to repair
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return fmt.Errorf("recording skipped skill materialization: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // temporary result channel
	writeErr := json.NewEncoder(file).Encode(sessionpkg.StartupContent{Skipped: true, Reason: cause.Error()})
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return fmt.Errorf("writing skipped skill materialization: %w", errors.Join(writeErr, closeErr))
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publishing skipped skill materialization: %w", err)
	}
	return nil
}

func collectStartupContent(info sessionpkg.Info, sources []sessionpkg.StartupContentSource) (sessionpkg.StartupContent, error) {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	if err != nil {
		return content, err
	}
	if content.Receipt != "" {
		data, err := os.ReadFile(content.Receipt)
		if err != nil {
			return content, fmt.Errorf("reading startup receipt: %w", err)
		}
		var receipt sessionpkg.StartupContent
		if err := json.Unmarshal(data, &receipt); err != nil {
			return content, fmt.Errorf("decoding startup receipt: %w", err)
		}
		content.Skipped = content.Skipped || receipt.Skipped
		content.Reason = receipt.Reason
	}
	for _, source := range changedStartupSources(sources) {
		if !slices.Contains(content.Sources, source) {
			content.Sources = append(content.Sources, source)
		}
	}
	content.Receipt = ""
	content.Pending = false
	return content, nil
}

func applyStartupContentFingerprint(cfg *runtime.Config) {
	cfg.FingerprintExtra = maps.Clone(cfg.FingerprintExtra)
	if cfg.FingerprintExtra == nil {
		cfg.FingerprintExtra = make(map[string]string)
	}
	cfg.FingerprintExtra["startup-content"] = "delivery-repair"
}

func startupContentBaseline(cfg runtime.Config) sessionpkg.StartupContentBaseline {
	applyStartupContentFingerprint(&cfg)
	breakdown, _ := json.Marshal(runtime.CoreFingerprintBreakdown(cfg))
	return sessionpkg.StartupContentBaseline{
		Core: runtime.CoreFingerprint(cfg), Provision: runtime.ProvisionFingerprint(cfg),
		Launch: runtime.LaunchFingerprint(cfg), Breakdown: string(breakdown),
	}
}

// startupContentCheck shares one lazy advisory load across a reconcile or
// preview pass, including failed loads. A later pass retries a busy cache.
type startupContentCheck struct {
	checked bool
	ready   bool
}

// Keep only the marked session's baseline while its delivered sources remain
// dirty. Recovery removes this extra input and uses ordinary config drift,
// including the existing attached, active, pending-interaction and work guards.
func (check *startupContentCheck) retainFingerprint(cfg *runtime.Config, info sessionpkg.Info) {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	if err != nil || content.Pending {
		return // incomplete starts are recovered through their commit path
	}
	if len(changedStartupSources(content.Sources)) > 0 {
		applyStartupContentFingerprint(cfg)
		return
	}
	if content.Skipped {
		if !check.checked {
			_, err := loadCityConfigAdvisory(cfg.Env["GC_CITY_PATH"])
			check.checked, check.ready = true, err == nil
		}
		if !check.ready {
			applyStartupContentFingerprint(cfg)
		}
	}
}

func finishStartupContent(prepared *preparedStart, metadata sessionpkg.MetadataPatch, observeDelivery bool) (sessionpkg.StartupContent, error) {
	var sources []sessionpkg.StartupContentSource
	if observeDelivery {
		sources = prepared.candidate.tp.StartupContentSources
	}
	// A recovery is not a start: it can consume saved delivery facts, but
	// today's filesystem cannot establish what an older runtime received.
	content, err := collectStartupContent(prepared.candidate.info, sources)
	if err != nil {
		return content, err
	}
	if content.Skipped {
		// A fresh PreStart process rejected what a warm validator may have
		// accepted. Forget that success once, when consuming the skip receipt.
		config.ResetRemoteCacheValidationCache()
	}
	if len(content.Sources) > 0 || content.Skipped {
		if content.Baseline.Core == "" {
			content.Baseline = startupContentBaseline(prepared.fingerprintConfig)
		}
		prepared.coreHash = content.Baseline.Core
		prepared.provisionHash = content.Baseline.Provision
		prepared.launchHash = content.Baseline.Launch
	}
	maps.Copy(metadata, sessionpkg.StartupContentBaselinePatch(content))
	return content, nil
}

func recoverStartupContent(info sessionpkg.Info, store *sessionpkg.Store, stderr io.Writer) (sessionpkg.Info, error) {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	if err != nil || !content.Pending {
		return info, err
	}
	content, err = collectStartupContent(info, nil)
	if err != nil {
		return info, err
	}
	if content.Skipped {
		config.ResetRemoteCacheValidationCache()
	}
	updated, err := store.CommitStartupContent(info, content)
	if err != nil {
		return info, err
	}
	logStartupContent(info.SessionName, content, stderr)
	removeStartupReceipt(info, stderr)
	return updated, nil
}

func removeStartupReceipt(info sessionpkg.Info, stderr io.Writer) {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	if err == nil && content.Receipt != "" {
		if err := os.Remove(content.Receipt); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "startup receipt cleanup for %s: %v\n", info.ID, err) //nolint:errcheck
		}
	}
}

func logStartupContent(name string, content sessionpkg.StartupContent, stderr io.Writer) {
	if len(content.Sources) == 0 && !content.Skipped {
		return
	}
	var imports []string
	for _, source := range content.Sources {
		if !slices.Contains(imports, source.Import) {
			imports = append(imports, source.Import)
		}
	}
	fmt.Fprintf(stderr, "startup-content: session %q marked for repair; imports=%v skill-materialization-skipped=%t reason=%s\n", name, imports, content.Skipped, content.Reason) //nolint:errcheck // health diagnostic
}
