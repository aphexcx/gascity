package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

type reloadOutcome string

const (
	reloadOutcomeApplied  reloadOutcome = "applied"
	reloadOutcomeNoChange reloadOutcome = "no_change"
	reloadOutcomeAccepted reloadOutcome = "accepted"
	reloadOutcomeFailed   reloadOutcome = "failed"
	reloadOutcomeBusy     reloadOutcome = "busy"
	reloadOutcomeTimeout  reloadOutcome = "timeout"
	reloadOutcomePreview  reloadOutcome = "preview"
	reloadOutcomeProgress reloadOutcome = "progress"
)

type reloadSource string

const (
	reloadSourceWatch  reloadSource = "watch"
	reloadSourceManual reloadSource = "manual"
)

var (
	// controllerReloadAcceptTimeout is how long a reload request waits for
	// the controller's main goroutine to drain it from reloadReqCh. The
	// main goroutine is blocked while a reconcile tick runs, and ticks can
	// take 30s–90s+ under bead-store churn (see issue #1560). 5s was
	// dramatically too short and produced "controller is busy" rejections
	// for many minutes at a time. 60s gives the controller enough headroom
	// to finish a tick before the reload is rejected, while still bounding
	// the wait for genuinely deadlocked controllers.
	controllerReloadAcceptTimeout = 60 * time.Second
	sendReloadControlRequestHook  = sendReloadControlRequest
	reloadUnavailableMessageHook  = reloadUnavailableMessage
	supervisorAPIBaseURLHook      = supervisorAPIBaseURL
)

type reloadControlRequest struct {
	Wait       bool                     `json:"wait"`
	Timeout    string                   `json:"timeout,omitempty"`
	DryRun     bool                     `json:"dry_run,omitempty"`
	Progress   bool                     `json:"progress,omitempty"`
	onProgress func(reloadControlReply) `json:"-"`
	// Soft requests that the reload tick accept any detected per-session
	// config drift instead of draining the drifted sessions. The
	// controller updates each open session's started_config_hash to the
	// hash that the freshly reloaded config produces for that session,
	// so the immediately-following reconcile tick sees no drift and
	// doesn't fire config-drift drains. Sessions whose template no
	// longer maps to a configured agent are NOT updated; normal
	// orphan/suspended drain handles them on the next tick.
	Soft bool `json:"soft,omitempty"`
}

type reloadControlReply struct {
	Outcome  reloadOutcome `json:"outcome,omitempty"`
	Message  string        `json:"message,omitempty"`
	Revision string        `json:"revision,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	// AcceptedDriftCount is set only for soft reload requests.
	AcceptedDriftCount    *int             `json:"accepted_drift_count,omitempty"`
	AcceptedDriftSessions []string         `json:"accepted_drift_sessions,omitempty"`
	Changes               []reloadChange   `json:"changes"`
	TargetRevision        string           `json:"target_revision,omitempty"`
	LastApplied           *reloadApplyInfo `json:"last_applied,omitempty"`
	Error                 string           `json:"error,omitempty"`
}

type reloadRequest struct {
	wait       bool
	timeout    time.Duration
	soft       bool
	dryRun     bool
	progressCh chan reloadControlReply
	acceptedCh chan reloadControlReply
	doneCh     chan reloadControlReply
	// started is stamped by handleReloadRequest when the request becomes
	// the active reload. The controller uses it to detect a wedged tick
	// that never cleared activeReload, so a fresh request can force-clear
	// the stuck slot instead of being rejected as busy indefinitely.
	started time.Time
}

func newReloadCmd(stdout, stderr io.Writer) *cobra.Command {
	var async bool
	var soft bool
	var dryRun bool
	var jsonOut bool
	var timeoutValue string
	cmd := &cobra.Command{
		Use:   "reload [path|name]",
		Short: "Reload the current city's config without restarting the city/controller",
		Long: `Force the current city controller to re-read effective config and
process one reload tick without restarting the city/controller.

Reload may fetch configured remote packs before recomputing effective
config. By default, per-session restarts may still happen if normal
config drift rules require them.

With --soft, the controller accepts any detected per-session config
drift instead of draining the drifted sessions: each open session's
recorded config hash is updated to the hash the freshly reloaded
config produces for it, the matching hash breakdown is refreshed, and
any already queued config-drift drain for that session is canceled. The
immediately-following reconcile tick sees no drift and no config-drift
drains fire. Useful when editing a running city's .gc/settings.json
without disrupting in-flight work. Sessions whose template no longer
maps to a configured agent are NOT updated; normal orphan/suspended
drain handles them on the next tick.

The reload plan names service replacements and existing sessions with
changed or unmapped templates. With --dry-run, load and validate config
and print that plan without applying it or running a reconciliation tick.
Config loading may populate the pack cache. Runtime activity and work
guards still control when planned session drains happen.

The default completion timeout is 5m. Progress is printed every 30s while
waiting. If a caller's outer timeout ends the wait early, supervisor.log's
"Config reloaded" line and revision are the proof of config application.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			timeoutChanged := cmd.Flags().Changed("timeout")
			if cmdReload(args, async, soft, dryRun, jsonOut, timeoutValue, timeoutChanged, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&async, "async", false, "Return after the controller accepts the reload request")
	cmd.Flags().BoolVar(&soft, "soft", false, "Accept config drift on open sessions instead of draining them")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview config changes without applying them or running a tick")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	cmd.Flags().StringVar(&timeoutValue, "timeout", "5m", "How long to wait for reload completion")
	return cmd
}

func cmdReload(args []string, async bool, soft bool, dryRun bool, jsonOut bool, timeoutValue string, timeoutChanged bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc reload: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if async && timeoutChanged {
		fmt.Fprintln(stderr, "gc reload: --async and --timeout cannot be used together") //nolint:errcheck // best-effort stderr
		return 1
	}
	if async && dryRun {
		fmt.Fprintln(stderr, "gc reload: --async and --dry-run cannot be used together") //nolint:errcheck // best-effort stderr
		return 1
	}

	planPrinted := false
	printedWarnings := make(map[string]bool)
	printWarning := func(warning string) {
		if !printedWarnings[warning] {
			fmt.Fprintf(stderr, "gc reload: warning: %s\n", warning) //nolint:errcheck // best-effort diagnostic
			printedWarnings[warning] = true
		}
	}
	req := reloadControlRequest{Wait: !async, Soft: soft, DryRun: dryRun, Progress: !async}
	req.onProgress = func(progress reloadControlReply) {
		for _, warning := range progress.Warnings {
			printWarning(warning)
		}
		if progress.Changes != nil {
			if !jsonOut {
				writeReloadChanges(stdout, progress.Changes)
				planPrinted = true
			}
		} else if progress.Message != "" {
			fmt.Fprintln(stderr, progress.Message) //nolint:errcheck // best-effort progress
		}
	}
	if !async {
		timeout, err := time.ParseDuration(timeoutValue)
		if err != nil {
			fmt.Fprintf(stderr, "gc reload: invalid --timeout %q: %v\n", timeoutValue, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if timeout <= 0 {
			fmt.Fprintln(stderr, "gc reload: --timeout must be greater than 0") //nolint:errcheck // best-effort stderr
			return 1
		}
		req.Timeout = timeout.String()
	}

	reply, err := sendReloadControlRequestHook(cityPath, req)
	if err != nil {
		if isControllerUnavailableError(err) {
			if msg := reloadUnavailableMessageHook(cityPath); msg != "" {
				fmt.Fprintf(stderr, "gc reload: %s: %v\n", msg, err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		fmt.Fprintf(stderr, "gc reload: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		ok := reply.Outcome == reloadOutcomeAccepted || reply.Outcome == reloadOutcomeApplied || reply.Outcome == reloadOutcomeNoChange || reply.Outcome == reloadOutcomePreview
		if !ok {
			return writeReloadFailureJSON(stdout, stderr, reply)
		}
		code := writeCLIJSONLineOrExit(stdout, stderr, "gc reload", lifecycleActionJSON{
			SchemaVersion: "1", OK: ok, Command: "reload", Action: "reload",
			Message: reloadReplyMessage(reply), CityPath: cityPath,
			Async: lifecycleBoolPtr(async), Soft: lifecycleBoolPtr(soft), DryRun: lifecycleBoolPtr(dryRun),
			Outcome: string(reply.Outcome), Revision: reply.Revision, Changes: reply.Changes,
			TargetRevision: reply.TargetRevision, LastApplied: reply.LastApplied,
			AcceptedDriftCount: reply.AcceptedDriftCount, AcceptedDriftSessions: reply.AcceptedDriftSessions,
			Warnings: reply.Warnings,
		})
		if code != 0 || !ok {
			return 1
		}
		return 0
	}
	switch reply.Outcome {
	case reloadOutcomeAccepted, reloadOutcomeApplied, reloadOutcomeNoChange, reloadOutcomePreview:
		message := reloadReplyMessage(reply)
		if message != "" {
			fmt.Fprintln(stdout, message) //nolint:errcheck // best-effort stdout
		}
		if !planPrinted {
			writeReloadChanges(stdout, reply.Changes)
		}
		if soft && reply.AcceptedDriftCount != nil {
			fmt.Fprintln(stdout, softReloadAcceptanceMessage(*reply.AcceptedDriftCount, reply.AcceptedDriftSessions)) //nolint:errcheck // best-effort stdout
		}
		for _, warning := range reply.Warnings {
			printWarning(warning)
		}
		return 0
	case reloadOutcomeFailed:
		if reply.Revision != "" {
			fmt.Fprintf(stderr, "Current config rev %s\n", shortRev(reply.Revision)) //nolint:errcheck // best-effort stderr
		}
		for _, warning := range reply.Warnings {
			printWarning(warning)
		}
		switch {
		case strings.TrimSpace(reply.Error) != "":
			fmt.Fprintln(stderr, strings.TrimSpace(reply.Error)) //nolint:errcheck // best-effort stderr
		case strings.TrimSpace(reply.Message) != "":
			fmt.Fprintln(stderr, strings.TrimSpace(reply.Message)) //nolint:errcheck // best-effort stderr
		default:
			fmt.Fprintln(stderr, "gc reload: reload failed") //nolint:errcheck // best-effort stderr
		}
		return 1
	case reloadOutcomeBusy, reloadOutcomeTimeout:
		if strings.TrimSpace(reply.Message) != "" {
			fmt.Fprintln(stderr, reloadReplyMessage(reply)) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc reload: %s\n", reply.Outcome) //nolint:errcheck // best-effort stderr
		}
		return 1
	default:
		fmt.Fprintf(stderr, "gc reload: unexpected controller outcome %q\n", reply.Outcome) //nolint:errcheck // best-effort stderr
		return 1
	}
}

func isControllerUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errControllerUnavailable) || errors.Is(err, errControllerUnresponsive)
}

func sendReloadControlRequest(cityPath string, req reloadControlRequest) (reloadControlReply, error) {
	if req.DryRun {
		identity := controllerIdentityHook(cityPath)
		if identity.PID > 0 && identity.ProtocolVersion < controllerProtocolVersion {
			return reloadControlReply{}, errors.New("the running controller predates reload previews; restart it on this build")
		}
	}
	command, err := marshalReloadControlCommand(req)
	if err != nil {
		return reloadControlReply{}, err
	}
	readTimeout, err := reloadControlReadTimeout(req)
	if err != nil {
		return reloadControlReply{}, err
	}
	var reply reloadControlReply
	err = readControllerCommandLines(cityPath, command, 2*time.Second, 5*time.Second, readTimeout, func(resp []byte) (bool, error) {
		var frame reloadControlReply
		if err := json.Unmarshal(resp, &frame); err != nil {
			return false, fmt.Errorf("parsing response: %w", err)
		}
		if frame.Outcome == reloadOutcomeProgress {
			if req.onProgress != nil {
				req.onProgress(frame)
			}
			return false, nil
		}
		reply = frame
		return true, nil
	})
	return reply, err
}

func marshalReloadControlCommand(req reloadControlRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}
	prefix := "reload:"
	if req.DryRun {
		// Older controllers ignore unknown JSON fields. A separate verb keeps
		// an unsupported preview from silently becoming a real reload.
		prefix = "reload-preview:"
	}
	return prefix + string(data), nil
}

func reloadControlReadTimeout(req reloadControlRequest) (time.Duration, error) {
	readTimeout := 2*controllerReloadAcceptTimeout + 10*time.Second
	if req.Wait && req.Timeout != "" {
		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return 0, fmt.Errorf("parsing request timeout: %w", err)
		}
		readTimeout += timeout
	}
	return readTimeout, nil
}

func reloadUnavailableMessage(cityPath string) string {
	info, ok := supervisorCityInfo(cityPath)
	if !ok {
		return ""
	}
	switch {
	case info.Running:
		return "controller is running but not responding"
	case info.Status == "init_failed" && strings.TrimSpace(info.Error) != "":
		return fmt.Sprintf("city failed to start under supervisor: %s", strings.TrimSpace(info.Error))
	case info.Status == "init_failed":
		return "city failed to start under supervisor"
	case strings.TrimSpace(info.Status) != "":
		return fmt.Sprintf("city is still starting under supervisor (%s)", controllerSupervisorStatusText(info.Status))
	default:
		return "city controller is not running"
	}
}

func supervisorCityInfo(cityPath string) (api.CityInfo, bool) {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered || supervisorAliveHook() == 0 {
		return api.CityInfo{}, false
	}
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return api.CityInfo{}, false
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return api.CityInfo{}, false
	}
	for _, city := range cities {
		if samePath(city.Path, entry.Path) {
			return city, true
		}
	}
	return api.CityInfo{}, false
}
