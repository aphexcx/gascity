package proctable

import "path/filepath"

// ManagedDoltScopeWatchdogVerb is the argv[1] re-exec marker of gc's managed
// Dolt scope watchdog (cmd/gc/dolt_scope_watchdog.go).
const ManagedDoltScopeWatchdogVerb = "__gc-managed-dolt-scope-watchdog"

// BDProxyChildVerb is the argv[1] verb of bd's database proxy supervisor
// (proxyendpoint.ChildVerb; duplicated here so this package stays free of the
// beads dependency tree, and pinned equal by a test).
const BDProxyChildVerb = "db-proxy-child"

// IsCityInfrastructureArgv reports whether argv is a long-lived city
// infrastructure process that can inherit an agent session's environment but
// is never that session's runtime: a Dolt sql-server, gc's managed Dolt scope
// watchdog, or bd's db-proxy-child. Terminating one as a session orphan takes
// the city's store down with it, including servers launched by older scripts
// that inherited the caller's session identity.
//
// The watchdog/proxy markers are re-execs of an operator-named binary. A
// sql-server must be the dolt executable itself, not text in an agent's prompt.
func IsCityInfrastructureArgv(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	switch argv[1] {
	case ManagedDoltScopeWatchdogVerb, BDProxyChildVerb:
		return true
	case "sql-server":
		return filepath.Base(argv[0]) == "dolt"
	}
	return false
}

// IsCityInfrastructureRoot reads pid's argv and reports whether it is city
// infrastructure per IsCityInfrastructureArgv.
//
// It is a kill-path fence, NOT part of the scanner: the scanner still reports
// such a process as a root (so its children are not promoted to roots in its
// place), and the callers that would terminate a scanned root — the orphan
// sweep and a session's pre-start orphan kill — consult this first and leave
// it alone. An unreadable argv reports false, which keeps those callers'
// pre-fence behavior. Under `go test` without an injected procfs root it
// reports false rather than read the host's live process table.
func IsCityInfrastructureRoot(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := liveScanGuard(); err != nil {
		return false
	}
	argv, err := rootArgv(pid)
	if err != nil {
		return false
	}
	return IsCityInfrastructureArgv(argv)
}

// RootStartIdentity returns pid's start-time token as the scanner would read
// it, or "" when it cannot be read. Callers use it to tell a recycled pid from
// the same process, e.g. to report a fenced root once rather than every sweep.
func RootStartIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	if err := liveScanGuard(); err != nil {
		return ""
	}
	return rootStartIdentity(pid)
}
