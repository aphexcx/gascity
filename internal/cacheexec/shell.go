package cacheexec

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// ValidateCachedShellCommand validates cached files executed by a shell launcher.
// Expansion is read-only: it never evaluates command/process substitutions.
// Dynamic launchers whose executable cannot be resolved statically retain FULL
// validation of the resolved imports, rather than silently weakening the guard.
func ValidateCachedShellCommand(ctx context.Context, command, dir string, env []string) error {
	return validateCachedShellCommand(ctx, command, dir, env, 0)
}

func validateCachedShellCommand(ctx context.Context, command, dir string, env []string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= 32 {
		return validateActiveRemoteCaches(ctx, dir, env)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "launcher")
	if err != nil {
		return validateActiveRemoteCaches(ctx, dir, env)
	}
	if env == nil {
		env = os.Environ()
	}
	var validationErr error
	unresolved := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if validationErr != nil {
			return false
		}
		// A syntax walk is not shell execution: conditional/subshell writes,
		// loop variables and declarations cannot update a single environment
		// soundly. Keep state-changing launchers on the conservative FULL path.
		switch node := node.(type) {
		case *syntax.Assign, *syntax.ForClause, *syntax.DeclClause, *syntax.FuncDecl,
			*syntax.ArithmCmd, *syntax.ArithmExp, *syntax.LetClause:
			unresolved = true
		case *syntax.ParamExp:
			if node.Exp != nil && (node.Exp.Op == syntax.AssignUnset || node.Exp.Op == syntax.AssignUnsetOrNull) {
				unresolved = true
			}
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		callEnv := env
		if len(call.Args) == 0 {
			return true
		}
		expansion := &expand.Config{
			Env:     expand.ListEnviron(callEnv...),
			NoUnset: true,
			// The parser requires this callback for process substitutions.
			// Walk their commands below without executing or inventing output.
			ProcSubst: func(*syntax.ProcSubst) (string, error) {
				return "", errors.New("process substitution requires execution")
			},
			// Unexpanded globs can hide symlinks into a cached checkout.
			// Retain FULL for these dynamic launchers without enumerating files.
			ReadDir2: func(string) ([]fs.DirEntry, error) {
				return nil, errors.New("filename glob requires runtime expansion")
			},
		}
		// Arguments of readers and shell output builtins are data. Still walk
		// their substitutions: those contain separate executable call nodes.
		if command, err := expand.Literal(expansion, call.Args[0]); err == nil && readsShellArguments(shellCommandName(command, dir, callEnv)) {
			validationErr = validateCachedShellPath(ctx, command, dir, callEnv)
			return true
		}
		args, err := expand.Fields(expansion, call.Args...)
		if err != nil {
			unresolved = true
			return true
		}
		validationErr = validateCachedShellArgs(ctx, args, dir, callEnv, depth)
		return true
	})
	if validationErr != nil {
		return validationErr
	}
	if unresolved {
		return validateActiveRemoteCaches(ctx, dir, env)
	}
	return nil
}

func validateCachedShellArgs(ctx context.Context, args []string, dir string, env []string, depth int) error {
	if len(args) == 0 {
		return nil
	}
	if err := validateCachedShellPath(ctx, args[0], dir, env); err != nil {
		return err
	}
	name := shellCommandName(args[0], dir, env)
	switch name {
	case "exec", "command", "env":
		for i := 1; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-") {
				return validateActiveRemoteCaches(ctx, dir, env)
			}
			if key, _, ok := strings.Cut(args[i], "="); ok && name == "env" && key != "" {
				env = append(env, args[i])
				continue
			}
			return validateCachedShellArgs(ctx, args[i:], dir, env, depth)
		}
	case "eval":
		return validateCachedShellCommand(ctx, strings.Join(args[1:], " "), dir, env, depth+1)
	case "sh", "bash", "dash", "zsh", "ksh":
		for i := 1; i < len(args); i++ {
			arg := args[i]
			if arg == "--" {
				if i+1 < len(args) {
					return validateCachedScriptPath(ctx, args[i+1], dir)
				}
				break
			}
			if arg == "-o" || arg == "+o" || arg == "-O" || arg == "+O" {
				i++ // the next word is an option name, not a script
				continue
			}
			if strings.HasPrefix(arg, "--") || arg == "-" {
				return validateActiveRemoteCaches(ctx, dir, env)
			}
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+") {
				if strings.ContainsAny(arg[1:], "oOs") {
					return validateActiveRemoteCaches(ctx, dir, env)
				}
				if strings.HasPrefix(arg, "-") && strings.ContainsRune(arg[1:], 'c') {
					if i+1 < len(args) {
						return validateCachedShellCommand(ctx, args[i+1], dir, env, depth+1)
					}
					break
				}
				continue
			}
			return validateCachedScriptPath(ctx, arg, dir)
		}
		return validateActiveRemoteCaches(ctx, dir, env)
	case "python", "python3", "node", "ruby", "perl", "lua":
		for _, arg := range args[1:] {
			if arg == "--" || arg == "-u" || arg == "-B" {
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return validateActiveRemoteCaches(ctx, dir, env)
			}
			return validateCachedScriptPath(ctx, arg, dir)
		}
		return validateActiveRemoteCaches(ctx, dir, env)
	case ".", "source":
		if len(args) > 1 {
			if err := validateCachedShellPath(ctx, args[1], dir, env); err != nil {
				return err
			}
		}
		return validateActiveRemoteCaches(ctx, dir, env)
	case "cd", "pushd", "popd", "read", "unset", "set", "shift", "getopts", "trap":
		return validateActiveRemoteCaches(ctx, dir, env)
	case "cat", "cp", "echo", "printf", "true", "false", "test", "[", ":":
		// These operations consume file contents without executing them.
		return nil
	default:
		// An arbitrary interpreter may execute a file supplied as an argument.
		// Validate only arguments belonging to resolved caches; ordinary option
		// strings and paths outside the cache retain their existing behavior.
		for _, arg := range args[1:] {
			if strings.ContainsAny(arg, `/\`) {
				path := arg
				if !filepath.IsAbs(path) {
					path = filepath.Join(dir, path)
				}
				if _, err := os.Stat(path); err != nil {
					continue // arguments need not be file names (for example, prompts)
				}
				if err := validateCachedShellPath(ctx, arg, dir, env); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func readsShellArguments(command string) bool {
	switch command {
	case "cat", "cp", "echo", "printf", "true", "false", "test", "[", ":":
		return true
	}
	return false
}

func validateCachedScriptPath(ctx context.Context, path, dir string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	// Interpreters open relative scripts in their working directory; they do
	// not search PATH as the shell does for an executable command.
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return ValidateCachedExecutable(ctx, abs)
}

// Interpreter behavior follows the resolved executable, including local aliases.
// Keep shell builtin names when there is no executable on PATH.
func shellCommandName(path, dir string, env []string) string {
	name := filepath.Base(path)
	// A known invocation also selects interpreter behavior for multicall
	// binaries and versioned targets (for example sh -> busybox).
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "python", "python3", "node", "ruby", "perl", "lua":
		return name
	}
	if resolved := resolveShellPath(path, dir, env); resolved != "" {
		if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
			resolvedName := filepath.Base(canonical)
			// Distribution aliases may select a GNU-prefixed reader (cp ->
			// gnucp) or a multicall applet. Other targets still determine the
			// behavior, so a reader-named alias to a shell cannot hide code.
			if readsShellArguments(name) && (resolvedName == "gnu"+name || resolvedName == "coreutils" || resolvedName == "busybox") {
				return name
			}
			return resolvedName
		}
	}
	return name
}

func validateCachedShellPath(ctx context.Context, path, dir string, env []string) error {
	if resolved := resolveShellPath(path, dir, env); resolved != "" {
		return ValidateCachedExecutable(ctx, resolved)
	}
	return nil
}

func resolveShellPath(path, dir string, env []string) string {
	if strings.ContainsAny(path, `/\`) {
		if !filepath.IsAbs(path) && dir != "" {
			path = filepath.Join(dir, path)
		}
		return path
	}
	search := os.Getenv("PATH")
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "PATH="); ok {
			search = value
		}
	}
	for _, prefix := range filepath.SplitList(search) {
		if !filepath.IsAbs(prefix) && dir != "" {
			prefix = filepath.Join(dir, prefix)
		}
		candidate := filepath.Join(prefix, path)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
