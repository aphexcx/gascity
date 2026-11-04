package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newStopCmd(stdout, stderr io.Writer) *cobra.Command {
	var wallClockTimeout time.Duration
	var force bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stop [path|name]",
		Short: "Stop all agent sessions in the city",
		Long: `Stop all agent sessions in the city with graceful shutdown.

Sends interrupt signals to running agents, waits for the configured
shutdown timeout, then force-kills any remaining sessions. Also stops
the Dolt server and cleans up orphan sessions. If a controller is
running, delegates shutdown to it.

If the city is registered with the machine-wide supervisor, stop also
unregisters it (equivalent to a following "gc unregister") — the city
will not be found by name or auto-started again until it is re-registered
with "gc register". Use "gc unregister" directly to remove a registration
without stopping sessions.

Use --timeout=DURATION to cap the wall-clock time gc stop will spend
before giving up; the default budgets configured session interrupt and
stop waves, the configured shutdown grace wait, and a second orphan
cleanup pass. Use --force to skip the interrupt grace period and go
straight to kill.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, jsonOut) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wallClockTimeout, "timeout", 0, "wall-clock cap for the stop sequence (0 = derive from city config)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the interrupt grace period and force-kill all sessions immediately")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

var sessionProviderForStopCity = newSessionProviderForCity

// cmdStop stops the city by terminating all configured agent sessions.
// If a path is given, operates there; otherwise uses cwd.
//
// wallClockTimeout caps how long cmdStop will wait for the shutdown
// sequence; if 0, a default derived from cfg.Daemon.ShutdownTimeoutDuration
// is used. force=true skips the interrupt grace period (gracefulStopAll
// runs with timeout=0, going straight to kill).
func cmdStop(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool) int {
	return cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, false)
}

type stopCommandOutcome struct {
	code         int
	cityPath     string
	unregistered bool
}

func cmdStopJSON(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool, jsonOut bool) int {
	var outcome stopCommandOutcome
	if wallClockTimeout > 0 {
		unregisterTx := newSupervisorUnregisterTransaction()
		outcome = runStopWithWallClockCap(wallClockTimeout, stderr, unregisterTx, func() stopCommandOutcome {
			return cmdStopJSONSequence(args, stdout, stderr, force, jsonOut, true, unregisterTx)
		})
	} else {
		// The uncapped path holds the same pending-unregister transaction as
		// the capped one: a stop that fails after removing the registration —
		// a managed provider that refuses to shut down, an invalid config the
		// body cannot recover — must hand the entry back rather than leave a
		// live city unregistered. This mirrors the capped arm's accept/rollback
		// exactly; the deferred success message is part of the same contract.
		unregisterTx := newSupervisorUnregisterTransaction()
		outcome = cmdStopJSONSequence(args, stdout, stderr, force, jsonOut, false, unregisterTx)
		if outcome.code == 0 {
			unregisterTx.commit()
		} else {
			writeSupervisorUnregisterRollback(stderr, "gc stop", "stop failed after unregistering city", unregisterTx.rollback())
		}
	}
	if outcome.code != 0 {
		return outcome.code
	}
	if jsonOut {
		return writeCityStopSuccess(stdout, stderr, outcome.cityPath, force, outcome.unregistered)
	}
	fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	return 0
}

func cmdStopJSONSequence(args []string, stdout, stderr io.Writer, force bool, jsonOut bool, wallClockCapApplied bool, unregisterTx *supervisorUnregisterTransaction) stopCommandOutcome {
	cityPath, err := resolveStopCityPath(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return stopCommandOutcome{code: 1}
	}

	stopStdout := stdout
	if jsonOut {
		stopStdout = io.Discard
	}

	unregisteredFromSupervisor := false
	if handled, code := unregisterCityFromSupervisorWithForce(cityPath, stopStdout, stderr, "gc stop", force, unregisterTx); handled {
		if code != 0 {
			return stopCommandOutcome{code: code, cityPath: cityPath}
		}
		unregisteredFromSupervisor = true
		if supervisorAliveHook() != 0 {
			if !stopCityManagedBeadsProviderAfterSuccessfulStop(cityPath, stderr) {
				return stopCommandOutcome{code: 1, cityPath: cityPath}
			}
			warnInvalidConfigAfterSuccessfulStop(cityPath, stderr)
			return stopCommandOutcome{cityPath: cityPath, unregistered: unregisteredFromSupervisor}
		}
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if handled, code := stopManagedRuntimeWithoutConfig(cityPath, err, stopStdout, stderr, force); handled {
			return stopCommandOutcome{code: code, cityPath: cityPath, unregistered: unregisteredFromSupervisor}
		}
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return stopCommandOutcome{code: 1, cityPath: cityPath}
	}

	stopLoadedCity := func() stopCommandOutcome {
		return stopCommandOutcome{
			code:         cmdStopBodyWithoutSuccess(cityPath, cfg, force, stopStdout, stderr),
			cityPath:     cityPath,
			unregistered: unregisteredFromSupervisor,
		}
	}
	if wallClockCapApplied {
		return stopLoadedCity()
	}
	return runStopWithWallClockCap(defaultStopWallClockTimeout(cfg), stderr, nil, stopLoadedCity)
}

func runStopWithWallClockCap(wallClockCap time.Duration, stderr io.Writer, unregisterTx *supervisorUnregisterTransaction, stop func() stopCommandOutcome) stopCommandOutcome {
	doneCh := make(chan stopCommandOutcome, 1)
	bodyDone := make(chan struct{})
	go func() {
		defer close(bodyDone)
		doneCh <- stop()
	}()
	if h := stopBodyLifecycleHook; h != nil {
		h(bodyDone)
	}
	timer := time.NewTimer(wallClockCap)
	defer timer.Stop()

	select {
	case out := <-doneCh:
		if unregisterTx != nil {
			if out.code == 0 {
				unregisterTx.commit()
			} else {
				writeSupervisorUnregisterRollback(stderr, "gc stop", "stop failed after unregistering city", unregisterTx.rollback())
			}
		}
		return out
	case <-timer.C:
		var rollback supervisorUnregisterRollback
		if unregisterTx != nil {
			rollback = unregisterTx.rollback()
		}
		fmt.Fprintf(stderr, "gc stop: timed out after %s; some sessions may not have stopped — retry with --force if stop is wedged, or raise --timeout for large stop sets\n", wallClockCap) //nolint:errcheck // best-effort stderr
		writeSupervisorUnregisterRollback(stderr, "gc stop", "wall-clock timeout", rollback)
		return stopCommandOutcome{code: 1}
	}
}

// stopBodyLifecycleHook receives the bounded stop worker's done channel.
// Tests with providers or supervisor waits that block past the wall-clock
// cap register this hook so they can wait for the worker to finish,
// preventing the leaked goroutine from racing on package-level stop hooks
// against a later test.
var stopBodyLifecycleHook func(<-chan struct{})

func writeCityStopSuccess(stdout, stderr io.Writer, cityPath string, force, unregistered bool) int {
	return writeLifecycleActionJSONOrExit(stdout, stderr, "gc stop", lifecycleActionJSON{
		Command:      "stop",
		Action:       "stop",
		Message:      "City stopped.",
		CityPath:     cityPath,
		Force:        lifecycleBoolPtr(force),
		Unregistered: lifecycleBoolPtr(unregistered),
	})
}

func resolveStopCityPath(args []string) (string, error) {
	if len(args) == 0 {
		return resolveCommandCity(nil)
	}
	// A name-shaped positional may be a registered city name or a local rig
	// directory; route it through the shared name resolver so a slashless rig
	// dir still resolves to its owning city without reopening the bare-name
	// walk-up footgun. Path-shaped args keep the exact stop path resolver.
	if classifyCityRef(args[0]) == cityRefName {
		ctx, err := resolveCityNameContext(args[0], func(name string) (resolvedContext, error) {
			cp, perr := stopCityPathFromArg(name)
			return resolvedContext{CityPath: cp}, perr
		})
		if err != nil {
			return "", err
		}
		return ctx.CityPath, nil
	}
	return stopCityPathFromArg(args[0])
}

// stopCityPathFromArg resolves a path-shaped stop argument (or a local city) to
// a city path, trying an exact city path, then a rig path, then an upward city
// walk — the original path-only behavior, now invoked as resolveCityRef's path
// resolver.
func stopCityPathFromArg(ref string) (string, error) {
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", err
	}
	if cityPath, err := validateCityPath(abs); err == nil {
		return cityPath, nil
	}
	ctx, ok, rigErr := resolveRigPathToContext(abs)
	if rigErr == nil && ok {
		return ctx.CityPath, nil
	}
	cityPath, findErr := findCity(abs)
	if findErr == nil {
		return cityPath, nil
	}
	if rigErr != nil {
		return "", rigErr
	}
	return "", findErr
}

// defaultStopWallClockTimeout returns the wall-clock cap used by cmdStop
// when --timeout is not set. Each pass budgets three sequential phases:
// interrupt provider dispatch, the configured post-interrupt grace wait, and
// bounded force-stop waves. A second pass covers orphan cleanup. Unknown extra
// live pool sessions or orphans can still require an explicit --timeout from
// the operator.
func defaultStopWallClockTimeout(cfg *config.City) time.Duration {
	base := 5 * time.Second
	if cfg != nil {
		if d := cfg.Daemon.ShutdownTimeoutDuration(); d > 0 {
			base = d
		}
	}
	targets := estimatedConfiguredStopTargets(cfg)
	interruptWaves := ceilDiv(targets, defaultMaxParallelInterrupts)
	stopWaves := ceilDiv(targets, defaultMaxParallelStopsPerWave)
	onePass := time.Duration(interruptWaves)*interruptPerTargetTimeout(cfg) +
		base +
		time.Duration(stopWaves)*stopPerTargetTimeoutDefault
	return 2*onePass + stopPerTargetTimeoutDefault
}

func estimatedConfiguredStopTargets(cfg *config.City) int {
	if cfg == nil || len(cfg.Agents) == 0 {
		return 1
	}
	total := 0
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if len(agent.NamepoolNames) > 0 {
			total += len(agent.NamepoolNames)
			continue
		}
		if maxSessions := agent.EffectiveMaxActiveSessions(); maxSessions != nil {
			switch {
			case *maxSessions == 0:
				continue
			case *maxSessions > 0:
				total += *maxSessions
				continue
			}
		}
		if minSessions := agent.EffectiveMinActiveSessions(); minSessions > 1 {
			total += minSessions
			continue
		}
		total++
	}
	if total < 1 {
		return 1
	}
	return total
}

func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	if d <= 0 {
		return n
	}
	return (n + d - 1) / d
}

func cmdStopBody(cityPath string, cfg *config.City, force bool, stdout, stderr io.Writer) int { //nolint:unparam // compatibility wrapper preserves the production-shaped force seam for direct tests
	code := cmdStopBodyWithoutSuccess(cityPath, cfg, force, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	}
	return code
}

// cmdStopBodyWithoutSuccess performs the stop flow without emitting the final
// success record. The command writes that record only after the bounded worker
// returns, so a timed-out worker cannot report a late success.
func cmdStopBodyWithoutSuccess(cityPath string, cfg *config.City, force bool, stdout, stderr io.Writer) int {
	cityName := loadedCityName(cfg, cityPath)

	// If a controller is running, ask it to shut down (it stops agents).
	stopResult := tryStopControllerWithForce(cityPath, stdout, force)
	switch stopResult.outcome {
	case controllerStopAcknowledged:
		if err := waitForStandaloneControllerStop(cityPath, cfg.Daemon.ShutdownTimeoutDuration()+15*time.Second); err != nil {
			fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Controller handled the shutdown — still stop the bead store, and
		// only after waiting for the controller to be gone: a live reader
		// restarts a provider-owned proxy the moment it is retired.
		if err := shutdownBeadsProviderForStop(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return 0
	case controllerStopDefinitePreEntryUnavailable:
		// No stop request entered a controller, so direct cleanup may proceed.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	default:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, _ := openCityStoreAt(cityPath)
	// Every store consumer in this stop flow is session-class (sleep-reason marks,
	// session-name lookups, session-runtime stop, orphan cleanup), so route the
	// whole flow through the session coordination-class store for relocation-safety.
	sessStore := cliSessionStore(store, cfg, cityPath)
	markCityStopSessionSleepReason(sessionFrontDoor(sessStore), stderr)

	sp, err := sessionProviderForStopCity(cfg, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sessionNames, desired, manualTargets, desiredErr := resolveStopSessionNames(sessStore, cfg, cityPath, cityName, sp, stderr)
	recorder := events.Discard
	if fr, err := newFileEventsRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), cfg.Events, stderr); err == nil {
		recorder = fr
	}

	graceTimeout := cfg.Daemon.ShutdownTimeoutDuration()
	if force {
		// gracefulStopAll treats timeout=0 as "skip interrupt pass, kill immediately".
		graceTimeout = 0
	}

	// Keep one dependency graph and grace window while retaining the backend
	// that proved each manual session's identity, even after routing changes.
	stopProvider := providerForManualStopTargets(sp, manualTargets)
	code := doStopWithoutSuccess(sessionNames, stopProvider, cfg, sessStore, graceTimeout, recorder, stdout, stderr)
	if desiredErr != nil {
		code = 1
	}

	// Clean up orphan sessions (sessions with the city prefix that are
	// not in the current config).
	orphansStopped := stopOrphans(cityPath, sp, desired, desiredErr, cfg, sessionFrontDoor(sessStore), graceTimeout, recorder, stdout, stderr)
	if !orphansStopped {
		code = 1
	}
	if err := verifyStopSurvivors(cityPath, cityName, sp, manualTargets); err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		code = 1
	}

	teardownEmptyServerForStop(cityName, sp, stderr)

	// Stop the bead store's backing service LAST, and only here. The order
	// this function runs in is load-bearing for a provider-owned proxied city:
	//
	//   controller (agents drain with it) -> sessions -> orphan sessions ->
	//   runtime server teardown -> bd dolt stop per provider-owned scope
	//
	// bd restarts a proxied scope's proxy and Dolt child on ANY read (beads
	// cmd/bd/main.go:1758 — BEADS_DOLT_AUTO_START does not reach that path),
	// so a single surviving reader after this point resurrects the processes
	// this call just retired. Retiring first and stopping readers afterwards
	// would leave the city up with a live proxy every time.
	//
	// The call is re-runnable: `bd dolt stop` is idempotent on rc.2 (exit 0,
	// stopped/verified true, with or without a live proxy), so a second
	// `gc stop` finds nothing to do and still exits 0.
	if err := shutdownBeadsProviderForStop(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal warning.
	}

	return code
}

// verifyStopSurvivors prevents skipped, filtered, or incomplete sweeps from
// reporting success while a city-owned session survives on an eligible backend.
func verifyStopSurvivors(cityPath, cityName string, sp runtime.Provider, retained []runtime.BackendListResult) error {
	retainedNames := make(map[string]bool)
	for _, target := range retained {
		for _, name := range target.Names {
			retainedNames[name] = true
		}
	}
	deadline := time.Now().Add(stopPerTargetTimeoutDefault)
	for {
		survivors, err := listStopSurvivors(cityPath, cityName, sp, retainedNames)
		if !survivors || !time.Now().Before(deadline) {
			return err
		}
		// Stop may acknowledge teardown before the runtime disappears. Use the
		// existing per-target bound even when --force skips the interrupt grace.
		time.Sleep(min(500*time.Millisecond, time.Until(deadline)))
	}
}

func listStopSurvivors(cityPath, cityName string, sp runtime.Provider, retained map[string]bool) (bool, error) {
	var result error
	var survivors bool
	for _, leaf := range stopCleanupProviders(sp) {
		scoped, ok := leaf.(interface{ IsCityScoped(string) bool })
		if ok && !scoped.IsCityScoped(cityName) {
			continue
		}
		backends := listSessionBackendsWithAbsenceCheck(leaf)
		if _, err := runtime.MergeBackendListResults(backends...); err != nil {
			result = errors.Join(result, fmt.Errorf("verifying surviving sessions: %w", err))
			continue
		}
		for _, backend := range backends {
			names := slices.Clone(backend.Names)
			if _, ok := backend.Provider.(interface {
				SessionOwnership(string) (string, string, error)
			}); ok {
				// Native ownership probes see artifacts (including Pending pods)
				// until deletion completes, even after liveness/listing omit them.
				for name := range retained {
					if !slices.Contains(names, name) {
						names = append(names, name)
					}
				}
			}
			for _, name := range names {
				ownerCity, _, err := stopSessionOwnership(backend.Provider, name)
				if runtime.IsSessionNotFound(err) {
					continue
				}
				switch {
				case err != nil:
					result = errors.Join(result, fmt.Errorf("verifying surviving session %q: %w", name, err))
				case ownerCity != "" && filepath.Clean(ownerCity) == filepath.Clean(cityPath):
					survivors = true
					result = errors.Join(result, fmt.Errorf("city-owned session %q remains after shutdown", name))
					if diagnostic, ok := backend.Provider.(interface{ ShutdownDiagnostic(string) error }); ok {
						result = errors.Join(result, diagnostic.ShutdownDiagnostic(name))
					}
				case retained[name]:
					result = errors.Join(result, fmt.Errorf("cannot confirm deletion of session %q: ownership changed or is missing", name))
				}
			}
		}
	}
	return survivors, result
}

// teardownEmptyServerForStop keeps the final server kill from undoing an
// orphan refusal or killing a foreign session preserved by the classifier.
func teardownEmptyServerForStop(cityName string, sp runtime.Provider, stderr io.Writer) {
	for _, backend := range stopCleanupProviders(sp) {
		teardownEmptyBackendForStop(cityName, backend, stderr)
	}
}

// stopCleanupProviders exposes independent backends before any runtime I/O,
// so an unknown remote scope cannot authorize cleanup or hide a scoped server.
func stopCleanupProviders(sp runtime.Provider) []runtime.Provider {
	if composite, ok := sp.(interface{ BackendProviders() []runtime.Provider }); ok {
		var leaves []runtime.Provider
		for _, backend := range composite.BackendProviders() {
			leaves = append(leaves, stopCleanupProviders(backend)...)
		}
		return leaves
	}
	return []runtime.Provider{sp}
}

func teardownEmptyBackendForStop(cityName string, sp runtime.Provider, stderr io.Writer) {
	if _, ok := sp.(runtime.ServerLifecycleProvider); !ok {
		return
	}
	scoped, ok := sp.(interface{ IsCityScoped(string) bool })
	if !ok || !scoped.IsCityScoped(cityName) {
		fmt.Fprintln(stderr, "gc stop: server teardown skipped: runtime cannot confirm a city-scoped socket") //nolint:errcheck // best-effort stderr
		return
	}
	backends := runtime.ListBackendSessions("runtime", sp, "")
	names, err := runtime.MergeBackendListResults(backends...)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: server teardown skipped: %s\n", strings.ReplaceAll(err.Error(), "\n", "; ")) //nolint:errcheck // best-effort stderr
		return
	}
	if len(names) > 0 {
		fmt.Fprintf(stderr, "gc stop: server teardown skipped: %d session(s) remain\n", len(names)) //nolint:errcheck // best-effort stderr
		return
	}
	for _, backend := range backends {
		teardownServerForStop(backend.Provider, stderr, "gc stop")
	}
}

// teardownServerForStop terminates a provider's shared server after every
// session has been stopped. logPrefix identifies the caller in the error
// line: the standalone path passes "gc stop", the supervisor-managed path
// passes its own "<logPrefix>: city '<name>'" so a teardown warning reads
// like every other managed-shutdown error.
func teardownServerForStop(sp runtime.Provider, stderr io.Writer, logPrefix string) {
	lifecycle, ok := sp.(runtime.ServerLifecycleProvider)
	if !ok {
		return
	}
	if err := lifecycle.TeardownServer(); err != nil {
		fmt.Fprintf(stderr, "%s: teardown server: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
	}
}

func markCityStopSessionSleepReason(sessFront *session.Store, stderr io.Writer) {
	if !sessFront.Backed() {
		return
	}
	// The label-only, closed-excluded, IsSessionBeadOrRepairable-UNfiltered Info
	// lister is byte-identical to the former ListByLabel("gc:session") + closed-skip
	// sweep: it keeps damaged gc:session-labeled beads with a non-"session" type (which
	// the narrowing Store.List would drop) and reads each row's classifier through the
	// typed twin (sessionMetadataStateInfo) + the Info.SleepReason mirror.
	sessions, err := sessFront.ListLabeledSessionInfosUnfiltered()
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: marking sessions: %v\n", err) //nolint:errcheck // best-effort warning
		return
	}
	for _, info := range sessions {
		if sessionMetadataStateInfo(info) != "active" {
			continue
		}
		if strings.TrimSpace(info.SleepReason) != "" {
			continue
		}
		if err := sessFront.SetMarker(info.ID, "sleep_reason", string(session.SleepReasonCityStop)); err != nil {
			fmt.Fprintf(stderr, "gc stop: marking session %s: %v\n", info.ID, err) //nolint:errcheck // best-effort warning
		}
	}
}

func stopCityManagedBeadsProviderAfterSuccessfulStop(cityPath string, stderr io.Writer) bool {
	_, err := stopCityManagedBeadsProvider(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

// stopCityManagedBeadsProvider retires the city's bead-store backend on the
// stop paths that do not run the full stop body (supervisor-unregistered, and
// a city whose config will not load).
//
// A provider-owned scope is not gated on a managed Dolt port. bd owns the
// process for those scopes and publishes no GC-managed port, so the port probe
// — which is the right question for the legacy managed-Dolt lifecycle — would
// answer "nothing to stop" for every proxied city and leave its proxy and Dolt
// child resident.
func stopCityManagedBeadsProvider(cityPath string) (bool, error) {
	if rawBeadsProvider(cityPath) != "bd" {
		return false, nil
	}
	providerOwned, err := cityHasProviderOwnedScope(cityPath)
	if err != nil {
		return false, err
	}
	if !providerOwned && currentResolvableManagedDoltPort(cityPath) == "" {
		return false, nil
	}
	return true, shutdownBeadsProviderForStop(cityPath)
}

var shutdownBeadsProviderForStop = shutdownBeadsProvider

func stopManagedRuntimeWithoutConfig(cityPath string, cfgErr error, stdout, stderr io.Writer, force bool) (bool, int) {
	controllerStopped, controllerErr := stopStandaloneControllerWithoutConfig(cityPath, stdout, force)
	if controllerErr != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", controllerErr) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	stopped, stopErr := stopCityManagedBeadsProvider(cityPath)
	if stopErr != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", stopErr) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	if !controllerStopped && !stopped {
		return false, 0
	}
	warnInvalidConfigStopSuccess(cfgErr, stderr)
	return true, 0
}

func stopStandaloneControllerWithoutConfig(cityPath string, stdout io.Writer, force bool) (bool, error) {
	stopResult := tryStopControllerWithForce(cityPath, stdout, force)
	switch stopResult.outcome {
	case controllerStopAcknowledged:
		if err := waitForStandaloneControllerStop(cityPath, supervisorCityStopTimeout(cityPath)); err != nil {
			return true, err
		}
		return true, nil
	case controllerStopDefinitePreEntryUnavailable:
		// No stop request entered a controller, so the lock probe may proceed.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		return true, stopResult.failClosedError()
	default:
		return true, stopResult.failClosedError()
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("probing standalone controller runtime dir: %w", err)
	}
	if err := waitForStandaloneControllerStop(cityPath, 0); err != nil {
		return false, err
	}
	return false, nil
}

func warnInvalidConfigAfterSuccessfulStop(cityPath string, stderr io.Writer) {
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err != nil {
		warnInvalidConfigStopSuccess(err, stderr)
	}
}

func warnInvalidConfigStopSuccess(err error, stderr io.Writer) {
	if err == nil {
		return
	}
	fmt.Fprintf(stderr, "gc stop: stopped city despite invalid config: %v\n", err) //nolint:errcheck // best-effort stderr
}

// resolveStopSessionNames preserves the configured stop's best-effort targets
// while retaining lookup failures that make orphan cleanup unsafe.
func resolveStopSessionNames(store beads.Store, cfg *config.City, cityPath, cityName string, sp runtime.Provider, stderr io.Writer) ([]string, map[string]bool, []runtime.BackendListResult, error) {
	st := cfg.Workspace.SessionTemplate
	var names []string
	desired := make(map[string]bool, len(cfg.Agents))
	var resolveErr error
	var manualTargets []runtime.BackendListResult
	if store == nil {
		resolveErr = errors.New("session store unavailable")
	}
	for _, a := range cfg.Agents {
		if !a.SupportsInstanceExpansion() {
			sn, err := lookupSessionNameOrLegacyChecked(store, cityName, a.QualifiedName(), st)
			resolveErr = errors.Join(resolveErr, err)
			names = append(names, sn)
			desired[sn] = true
		} else {
			refs, err := resolvePoolSessionRefsChecked(store, cfg, a.Name, a.Dir, scaleParamsFor(&a), &a, cityName, st, sp, stderr)
			resolveErr = errors.Join(resolveErr, err)
			for _, ref := range refs {
				names = append(names, ref.sessionName)
				desired[ref.sessionName] = true
			}
		}
	}
	if store != nil {
		var err error
		manualTargets, err = resolveManualStopSessionTargets(store, cfg, cityPath, sp)
		resolveErr = errors.Join(resolveErr, err)
		for _, target := range manualTargets {
			for _, sn := range target.Names {
				if !desired[sn] {
					names = append(names, sn)
					desired[sn] = true
				}
			}
		}
	}
	return names, desired, manualTargets, resolveErr
}

// stopOrphans stops this city's classified orphans after configured agents.
// A failed safety check refuses the whole sweep and fails the command.
func stopOrphans(cityPath string, sp runtime.Provider, desired map[string]bool, desiredErr error, cfg *config.City, sessFront *session.Store,
	timeout time.Duration, rec events.Recorder, stdout, stderr io.Writer,
) bool {
	if desiredErr != nil {
		fmt.Fprintf(stderr, "gc stop: orphan sweep skipped: partial desired session set: %s\n", strings.ReplaceAll(desiredErr.Error(), "\n", "; ")) //nolint:errcheck // best-effort stderr
		return false
	}
	cityName := loadedCityName(cfg, cityPath)
	var backends []runtime.BackendListResult
	var ownedBackends []runtime.BackendListResult
	ownershipResolved := true
	for _, leaf := range stopCleanupProviders(sp) {
		scoped, ok := leaf.(interface{ IsCityScoped(string) bool })
		if !ok {
			if _, owned := leaf.(interface {
				SessionOwnership(string) (string, string, error)
			}); !owned {
				fmt.Fprintf(stderr, "gc stop: orphan sweep skipped: runtime cannot confirm a city-scoped socket or host ownership for backend %T\n", leaf) //nolint:errcheck // best-effort stderr
				continue
			}
			candidates, unknown, err := ownedOrphanStopTargets(leaf, cityPath, desired)
			if err != nil {
				fmt.Fprintf(stderr, "gc stop: orphan sweep skipped: %s\n", strings.ReplaceAll(err.Error(), "\n", "; ")) //nolint:errcheck // best-effort stderr
				return false
			}
			ownedBackends = append(ownedBackends, candidates...)
			if len(unknown) > 0 {
				fmt.Fprintf(stderr, "gc stop: %d session(s) on %T carry no ownership record (started before this gc or by another city): %s; inspect with gc doctor; after verifying ownership, use gc session kill ID-OR-ALIAS for persisted sessions or the runtime provider for orphans\n", len(unknown), leaf, strings.Join(unknown, ", ")) //nolint:errcheck // best-effort stderr
				ownershipResolved = false
			}
			continue
		}
		if len(desired) == 0 {
			// Scoped classification needs a configured name-shape baseline.
			fmt.Fprintln(stderr, "gc stop: orphan sweep skipped: empty desired session set") //nolint:errcheck // best-effort stderr
			continue
		}
		if !scoped.IsCityScoped(cityName) {
			fmt.Fprintf(stderr, "gc stop: orphan sweep skipped: runtime cannot confirm a city-scoped socket for backend %T\n", leaf) //nolint:errcheck // best-effort stderr
			continue
		}
		if len(cfg.Agents) == 0 {
			continue // persisted manual targets do not provide a name-shape baseline
		}
		check := doctor.NewOrphanSessionsCheck(cfg, cityName, cfg.Workspace.SessionTemplate, stopOrphanListingProvider{leaf})
		candidates, err := check.CleanupCandidates(&doctor.CheckContext{CityPath: cityPath}, desired)
		if err != nil {
			fmt.Fprintf(stderr, "gc stop: orphan sweep skipped: %s\n", strings.ReplaceAll(err.Error(), "\n", "; ")) //nolint:errcheck // best-effort stderr
			return false
		}
		backends = append(backends, candidates...)
	}
	var targets []stopTarget
	var scopedTargets []runtime.BackendListResult
	for _, backend := range backends {
		for _, target := range stopTargetsForNames(backend.Names, cfg, sessFront.Store().Store, stderr) {
			provider := backend.Provider
			if target.sessionID == "" {
				ownerCity, id, err := stopSessionOwnership(backend.Provider, target.name)
				if runtime.IsSessionNotFound(err) {
					continue
				}
				if err == nil && ownerCity != "" && (cityPath == "" || filepath.Clean(ownerCity) != filepath.Clean(cityPath)) {
					err = fmt.Errorf("runtime ownership changed to another city")
				}
				if err == nil && strings.TrimSpace(id) != "" {
					_, err = sessFront.Store().Get(strings.TrimSpace(id))
					if stopSessionRecordAbsent(err) {
						target.runtimeOnly = true
						provider = &verifiedManualStopBackend{Provider: backend.Provider, verify: func() error {
							return verifyScopedOrphanStopOwnership(backend.Provider, target.name, ownerCity, id)
						}}
						err = nil
					}
				}
				if err != nil {
					fmt.Fprintf(stderr, "gc stop: orphan sweep refused: resolving runtime %q session record: %v\n", target.name, err) //nolint:errcheck // best-effort stderr
					return false
				}
			}
			targets = append(targets, target)
			scopedTargets = append(scopedTargets, runtime.BackendListResult{Label: backend.Label, Provider: provider, Names: []string{target.name}})
		}
	}
	for _, backend := range ownedBackends {
		// Persisted targets were handled above. An orphan's ownership ID can
		// refer to a deleted bead; use the runtime worker with its verified
		// owner instead of trying to reconstruct that missing session.
		for _, target := range stopTargetsForNames(backend.Names, cfg, nil, stderr) {
			target.runtimeOnly = true
			targets = append(targets, target)
		}
	}
	seen := make(map[string]bool, len(targets))
	unique := targets[:0]
	for _, target := range targets {
		if runtimeOnly, exists := seen[target.name]; exists {
			if runtimeOnly || target.runtimeOnly {
				fmt.Fprintf(stderr, "gc stop: orphan sweep refused: multiple backends claim runtime %q\n", target.name) //nolint:errcheck // best-effort stderr
				return false
			}
			continue // scoped copies share a lifecycle target and retain every owner
		}
		seen[target.name] = target.runtimeOnly
		target.order = len(unique)
		unique = append(unique, target)
	}
	provider := providerForManualStopTargets(sp, append(scopedTargets, ownedBackends...))
	stopped := gracefulStopTargets(unique, provider, timeout, rec, cfg, sessFront.Store(), stdout, stderr, nil)
	return stopped && ownershipResolved
}

// stopSessionRecordAbsent accepts only a single wrapped not-found sentinel.
// Collisions and joined errors cannot prove that runtime-only teardown is safe.
func stopSessionRecordAbsent(err error) bool {
	if errors.Is(err, beads.ErrIDCollision) {
		return false
	}
	for err != nil {
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		next := errors.Unwrap(err)
		if next == nil {
			return errors.Is(err, beads.ErrNotFound)
		}
		err = next
	}
	return false
}

// stopOrphanListingProvider confirms absence independently on each leaf while
// retaining the doctor's socket-scope check and original owning providers.
type stopOrphanListingProvider struct{ runtime.Provider }

// IsCityScoped retains the original leaf's socket-scope policy.
func (p stopOrphanListingProvider) IsCityScoped(city string) bool {
	scoped, ok := p.Provider.(interface{ IsCityScoped(string) bool })
	return ok && scoped.IsCityScoped(city)
}

// ListRunningBackends confirms absent servers independently for each leaf.
func (p stopOrphanListingProvider) ListRunningBackends(string) []runtime.BackendListResult {
	return listSessionBackendsWithAbsenceCheck(p.Provider)
}

// tryStopController connects to the controller socket and sends "stop".
// Returns true if a controller acknowledged the shutdown. If no controller
// is running (socket doesn't exist or connection refused), returns false.
func tryStopController(cityPath string, stdout io.Writer) bool {
	return tryStopControllerWithForce(cityPath, stdout, false).outcome == controllerStopAcknowledged
}

func tryStopControllerWithForce(cityPath string, stdout io.Writer, force bool) controllerStopResult {
	result := sendControllerStop(cityPath, force)
	if result.outcome == controllerStopAcknowledged {
		fmt.Fprintln(stdout, "Controller stopping...") //nolint:errcheck // best-effort stdout
	}
	return result
}

func waitForStandaloneControllerStop(cityPath string, timeout time.Duration) error {
	return waitForControllerStop(cityPath, timeout)
}

func waitForSupervisorControllerStop(cityPath string, timeout time.Duration) error {
	return waitForControllerStop(cityPath, timeout)
}

func waitForControllerStop(cityPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		pid := controllerAlive(cityPath)
		lock, err := acquireControllerLock(cityPath)
		switch {
		case err == nil && pid == 0:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
			return nil
		case err == nil:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
		case !errors.Is(err, errControllerAlreadyRunning):
			return fmt.Errorf("probing controller: %w", err)
		}
		if time.Now().After(deadline) {
			if pid != 0 {
				identity := probeControllerIdentity(cityPath)
				if identity.PID == 0 {
					identity.PID = pid
				}
				return controllerStopTimeoutError(identity, false)
			}
			return controllerStopTimeoutError(controllerIdentityReply{}, true)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func controllerStopTimeoutError(identity controllerIdentityReply, waitingForLock bool) error {
	authority := "controller"
	switch identity.HostingMode {
	case controllerHostingStandalone:
		authority = "standalone controller"
	case controllerHostingSupervisor:
		authority = "supervisor-hosted controller"
	}
	if identity.PID != 0 {
		return fmt.Errorf("timed out waiting for %s (PID %d) to stop", authority, identity.PID)
	}
	if waitingForLock {
		return fmt.Errorf("timed out waiting for controller to release its lock")
	}
	return fmt.Errorf("timed out waiting for %s to stop", authority)
}

// doStop is the pure logic for "gc stop". Filters to running sessions and
// performs graceful shutdown (interrupt → wait → kill). Accepts session names,
// provider, timeout, and recorder for testability.
func doStop(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration, //nolint:unparam // compatibility wrapper preserves the production-shaped store seam for direct tests
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	code := doStopWithoutSuccess(sessionNames, sp, cfg, store, timeout, rec, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	}
	return code
}

func doStopWithoutSuccess(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration,
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	code := 0
	visible := map[string]bool{}
	if sp != nil {
		names, err := runtime.MergeBackendListResults(listSessionBackendsWithAbsenceCheck(sp)...)
		if err != nil {
			code = 1
		}
		partialList := runtime.IsPartialListError(err)
		if err != nil && !partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
			names = nil
		}
		if partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				visible[name] = true
			}
		}
	}
	// Discovery verified these artifacts even if their provider does not count
	// them as running. Retain them for teardown and its ownership recheck.
	if manual, ok := sp.(*manualStopProvider); ok {
		for name := range manual.owners {
			visible[name] = true
		}
	}
	var running []string
	for _, sn := range sessionNames {
		sn = strings.TrimSpace(sn)
		if sn == "" {
			continue
		}
		if alive, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, sn); err == nil && alive {
			running = append(running, sn)
			continue
		}
		if visible[sn] {
			running = append(running, sn)
		}
	}
	if !gracefulStopAll(running, sp, timeout, rec, cfg, beads.SessionStore{Store: store}, stdout, stderr) {
		code = 1
	}
	if manual, ok := sp.(*manualStopProvider); ok && manual.shutdownError() != nil {
		return 1
	}
	return code
}
