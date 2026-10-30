package hooks

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

type codexTrustRecord struct {
	Key, Hash string
}

type codexHook struct {
	Key                    string  `json:"key"`
	CurrentHash            string  `json:"currentHash"`
	SourcePath             string  `json:"sourcePath"`
	EventName              string  `json:"eventName"`
	HandlerType            string  `json:"handlerType"`
	Command                string  `json:"command"`
	Matcher                *string `json:"matcher"`
	Async                  bool    `json:"async"`
	Enabled                bool    `json:"enabled"`
	TimeoutSec             uint64  `json:"timeoutSec"`
	StatusMessage          *string `json:"statusMessage"`
	AdditionalContextLimit *uint64 `json:"additionalContextLimit"`
	TrustStatus            string  `json:"trustStatus"`
}

type codexHooksList struct {
	Data []struct {
		Cwd    string      `json:"cwd"`
		Hooks  []codexHook `json:"hooks"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"data"`
}

// TrustCodexManagedHooks records trust for exact bundled Gas City hooks before
// the first turn. The configured launcher reports its own CODEX_HOME and hook
// hashes; neither is inferred from the supervisor's environment. Existing
// decisions and unrelated configuration are preserved. No model turn runs.
func TrustCodexManagedHooks(ctx context.Context, launcher, cityDir, workDir string) (int, error) {
	path := filepath.Join(workDir, ".codex", "hooks.json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("reading Codex hooks: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	home, listed, err := inspectCodexHooks(ctx, launcher, workDir)
	if err != nil {
		return 0, err
	}
	records, err := managedCodexTrustRecords(listed, cityDir, workDir)
	if err != nil || len(records) == 0 {
		return 0, err
	}
	if !filepath.IsAbs(home) {
		return 0, fmt.Errorf("codex initialize returned a non-absolute codexHome")
	}
	configPath := filepath.Join(home, "config.toml")
	// Preserve symlinked user configuration by updating the actual target.
	if target, err := filepath.EvalSymlinks(configPath); err == nil {
		configPath = target
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("resolving Codex config: %w", err)
	}
	// Lock the existing directory inode, not a status/lock file. Atomic rename
	// replaces config.toml's inode, so locking the config file would not serialize
	// two Gas City starts. No network or Codex process runs while holding this lock.
	dir, err := os.Open(filepath.Dir(configPath))
	if err != nil {
		return 0, fmt.Errorf("opening Codex config directory: %w", err)
	}
	defer dir.Close() //nolint:errcheck // closes the directory lock
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return 0, fmt.Errorf("locking Codex config directory: %w", err)
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN) //nolint:errcheck // close also releases it
	existing, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("reading Codex config: %w", err)
	}
	updated, count, err := appendCodexTrust(existing, records)
	if err != nil || count == 0 {
		return count, err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(configPath); err == nil {
		mode = info.Mode().Perm()
	}
	// Refuse a concurrent editor save rather than overwrite its changes.
	latest, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("rechecking Codex config: %w", err)
	}
	if !bytes.Equal(latest, existing) {
		return 0, fmt.Errorf("codex config changed while adding hook trust; retry startup")
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, configPath, updated, mode); err != nil {
		return 0, fmt.Errorf("recording Codex hook trust: %w", err)
	}
	return count, nil
}

func appendCodexTrust(existing []byte, records []codexTrustRecord) ([]byte, int, error) {
	var cfg struct {
		Hooks struct {
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
				Enabled     *bool  `toml:"enabled"`
			}
		}
	}
	if _, err := toml.Decode(string(existing), &cfg); err != nil {
		return existing, 0, fmt.Errorf("parsing Codex hook trust: %w", err)
	}
	var suffix strings.Builder
	count := 0
	seen := make(map[string]bool)
	for _, record := range records {
		if seen[record.Key] {
			continue
		}
		seen[record.Key] = true
		hash, err := hex.DecodeString(strings.TrimPrefix(record.Hash, "sha256:"))
		if record.Key == "" || !strings.HasPrefix(record.Hash, "sha256:") || err != nil || len(hash) != 32 {
			return existing, 0, fmt.Errorf("codex returned an invalid hook trust record")
		}
		if state, ok := cfg.Hooks.State[record.Key]; ok {
			if state.Enabled != nil && !*state.Enabled {
				return existing, 0, fmt.Errorf("codex hook %q was explicitly disabled; preserving user decision", record.Key)
			}
			if state.TrustedHash == record.Hash {
				continue
			}
			return existing, 0, fmt.Errorf("codex hook %q has an existing trust decision; review its changed definition with /hooks", record.Key)
		}
		fmt.Fprintf(&suffix, "\n[hooks.state.%s]\ntrusted_hash = %s\n", strconv.Quote(record.Key), strconv.Quote(record.Hash))
		count++
	}
	if count == 0 {
		return existing, 0, nil
	}
	updated := append(bytes.Clone(existing), suffix.String()...)
	if _, err := toml.Decode(string(updated), &cfg); err != nil {
		return existing, 0, fmt.Errorf("validating appended Codex hook trust: %w", err)
	}
	return updated, count, nil
}

func managedCodexTrustRecords(listed codexHooksList, cityDir, workDir string) ([]codexTrustRecord, error) {
	raw, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		return nil, err
	}
	normalized, _, err := normalizeCodexHookCommands(raw, cityDir)
	if err != nil {
		return nil, err
	}
	// Accept the actual bundled overlay and the installer's normalized form.
	// Do not infer ownership from a command prefix: custom hooks remain subject
	// to the user's review, including commands that append shell instructions.
	allowed := make(map[string]bool)
	eventNames := map[string]string{"SessionStart": "sessionStart", "PreCompact": "preCompact", "UserPromptSubmit": "userPromptSubmit"}
	for _, data := range [][]byte{raw, normalized} {
		var doc struct {
			Hooks map[string][]struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, err
		}
		for event, entries := range doc.Hooks {
			for _, entry := range entries {
				for _, hook := range entry.Hooks {
					allowed[eventNames[event]+"\x00"+entry.Matcher+"\x00"+hook.Command] = true
				}
			}
		}
	}
	var records []codexTrustRecord
	found := false
	for _, result := range listed.Data {
		if filepath.Clean(result.Cwd) != filepath.Clean(workDir) {
			continue
		}
		found = true
		if len(result.Errors) != 0 {
			return nil, fmt.Errorf("codex could not load hooks for %q: %s", workDir, result.Errors[0].Message)
		}
		for _, hook := range result.Hooks {
			matcher := ""
			if hook.Matcher != nil {
				matcher = *hook.Matcher
			}
			if filepath.Clean(hook.SourcePath) != filepath.Join(workDir, ".codex", "hooks.json") ||
				hook.HandlerType != "command" || !hook.Enabled || hook.Async || hook.TimeoutSec != 600 ||
				hook.StatusMessage != nil || hook.AdditionalContextLimit != nil ||
				!allowed[hook.EventName+"\x00"+matcher+"\x00"+hook.Command] {
				continue
			}
			records = append(records, codexTrustRecord{Key: hook.Key, Hash: hook.CurrentHash})
		}
	}
	if !found {
		return nil, fmt.Errorf("codex hooks/list omitted working directory %q", workDir)
	}
	return records, nil
}

func inspectCodexHooks(ctx context.Context, launcher, workDir string) (home string, listed codexHooksList, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("inspecting Codex launcher %q: %w", launcher, resultErr)
		}
	}()
	command, err := codexInspectionCommand(launcher)
	if err != nil {
		return "", listed, err
	}
	// Reuse the runtime shell launch path (subprocess.Start and tmux pre_start),
	// inheriting the pre-start environment and evaluating the configured shell
	// expressions in the session workdir, including account selection.
	cmd := runtime.ShellCommand(ctx, command)
	cmd.Dir = workDir
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", listed, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", listed, err
	}
	if err := cmd.Start(); err != nil {
		return "", listed, fmt.Errorf("starting Codex hook inspection: %w", err)
	}
	defer func() {
		_ = in.Close()
		_ = cmd.Wait()
	}()
	enc, dec := json.NewEncoder(in), json.NewDecoder(out)
	params := struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}{}
	params.ClientInfo.Name, params.ClientInfo.Version = "gascity-hook-trust", "1"
	params.Capabilities.ExperimentalAPI = true
	var init struct {
		CodexHome string `json:"codexHome"`
	}
	if err := codexHookRPC(enc, dec, 1, "initialize", params, &init); err != nil {
		return "", listed, err
	}
	if err := enc.Encode(struct {
		Method string `json:"method"`
	}{"initialized"}); err != nil {
		return "", listed, err
	}
	if err := codexHookRPC(enc, dec, 2, "hooks/list", struct {
		Cwds []string `json:"cwds"`
	}{[]string{workDir}}, &listed); err != nil {
		return "", listed, err
	}
	return init.CodexHome, listed, nil
}

// Codex 0.153.3 rejects --profile and other TUI options on app-server.
// Hook keys/hashes are definition-based; inspect through the same launcher
// and CODEX_HOME, retaining config overrides but omitting runtime-only flags.
func codexInspectionCommand(launcher string) (string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(launcher), "launcher")
	if err != nil {
		return "", fmt.Errorf("parsing launcher: %w", err)
	}
	if len(file.Stmts) == 0 {
		return "", fmt.Errorf("codex hook inspection requires an executable launcher")
	}
	// The runtime appends provider arguments to the terminal command. Keep
	// shell setup, assignments, quoting, expansions, redirects and operators
	// intact; only adapt that terminal invocation to the app-server protocol.
	stmt := file.Stmts[len(file.Stmts)-1]
	for {
		if stmt.Background || stmt.Negated {
			return "", fmt.Errorf("codex inspection requires a foreground terminal command")
		}
		binary, ok := stmt.Cmd.(*syntax.BinaryCmd)
		if !ok {
			break
		}
		stmt = binary.Y
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", fmt.Errorf("codex inspection requires a terminal executable command")
	}
	args := call.Args[:1:1]
	for i := 1; i < len(call.Args); i++ {
		parts, complete := codexStaticWordPrefix(call.Args[i].Parts)
		fields, err := expand.Fields(nil, &syntax.Word{Parts: parts})
		if err != nil {
			return "", fmt.Errorf("reading launcher option: %w", err)
		}
		word := ""
		if len(fields) == 1 {
			word = fields[0]
		}
		// An expansion after a known --option= prefix is its value. An
		// expansion within the option name cannot be classified statically.
		if !complete && !strings.Contains(word, "=") {
			word = ""
		}
		switch word {
		case "-p", "--profile", "-m", "--model", "-a", "--ask-for-approval", "-s", "--sandbox", "--add-dir", "-i", "--image":
			if i+1 == len(call.Args) {
				return "", fmt.Errorf("codex launcher option %s has no value", word)
			}
			i++
			continue
		case "--dangerously-bypass-approvals-and-sandbox", "--full-auto", "--no-alt-screen", "--search":
			continue
		case "-c", "--config", "--enable", "--disable":
			if i+1 == len(call.Args) {
				return "", fmt.Errorf("codex launcher option %s has no value", word)
			}
			args = append(args, call.Args[i], call.Args[i+1])
			i++
			continue
		}
		name, _, hasValue := strings.Cut(word, "=")
		if hasValue && (name == "--profile" || name == "--model" || name == "--ask-for-approval" || name == "--sandbox" || name == "--add-dir" || name == "--image") {
			continue
		}
		args = append(args, call.Args[i])
	}
	args = append(args, &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: "app-server"}}})
	call.Args = args
	var command strings.Builder
	if err := syntax.NewPrinter().Print(&command, file); err != nil {
		return "", fmt.Errorf("rendering inspection launcher: %w", err)
	}
	return command.String(), nil
}

// codexStaticWordPrefix retains only literal syntax for option recognition.
// Expansion nodes are never evaluated here; the original word remains intact
// for retained arguments and is evaluated only by the runtime shell.
func codexStaticWordPrefix(parts []syntax.WordPart) ([]syntax.WordPart, bool) {
	var prefix []syntax.WordPart
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
			prefix = append(prefix, part)
		case *syntax.DblQuoted:
			inner, complete := codexStaticWordPrefix(p.Parts)
			quoted := *p
			quoted.Parts = inner
			prefix = append(prefix, &quoted)
			if !complete {
				return prefix, false
			}
		default:
			return prefix, false
		}
	}
	return prefix, true
}

func codexHookRPC(enc *json.Encoder, dec *json.Decoder, id int, method string, params, result any) error {
	if err := enc.Encode(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params}); err != nil {
		return fmt.Errorf("codex %s: %w", method, err)
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := dec.Decode(&reply); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("codex %s ended before responding", method)
			}
			return fmt.Errorf("reading Codex %s: %w", method, err)
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("codex %s: %s", method, reply.Error.Message)
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("decoding Codex %s: %w", method, err)
		}
		return nil
	}
}
