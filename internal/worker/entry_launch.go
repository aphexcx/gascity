package worker

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type entryLaunchKind uint8

const (
	entryLaunchRuntime entryLaunchKind = iota
	entryLaunchMessage
	entryLaunchInterrupt
	entryLaunchLiveOnly
)

type entryLaunch struct {
	command         string
	hints           runtime.Config
	controllerOwned bool
}

// launchForCall prepares launch inputs before the manager takes the session
// lock. A controller-owned result retains that decision across the boundary.
func (h *SessionHandle) launchForCall(id string, kind entryLaunchKind) (entryLaunch, error) {
	if h.buildSessionLaunch != nil && kind != entryLaunchLiveOnly {
		info, pr, err := sessionRecordViaManager(h.manager, id)
		if err != nil {
			return entryLaunch{}, err
		}
		mayStart := kind == entryLaunchInterrupt
		if !mayStart && h.launchProvider != nil && !h.launchProvider.IsRunning(info.SessionName) {
			mayStart = kind == entryLaunchRuntime || h.controllerRunning == nil || !h.controllerRunning()
			if kind == entryLaunchRuntime {
				waiting, err := h.manager.ControllerRestartPending(id)
				if err != nil {
					return entryLaunch{}, err
				}
				if waiting {
					return entryLaunch{controllerOwned: true}, nil
				}
			}
		}
		if mayStart {
			resolved, err := h.buildSessionLaunch(info, strings.TrimSpace(pr.Metadata["real_world_app_session_kind"]), cloneStringMap(pr.Metadata))
			if err != nil {
				return entryLaunch{}, err
			}
			if resolved != nil {
				launch := normalizeResolvedRuntimeInput(*resolved)
				info = sessionpkg.ResumeInfoForTransport(info, info.Transport, pr.Metadata)
				info.Command, info.Provider = launch.Command, launch.Provider
				if launch.Resume.ResumeFlag != "" {
					info.ResumeFlag = launch.Resume.ResumeFlag
				}
				if launch.Resume.ResumeStyle != "" {
					info.ResumeStyle = launch.Resume.ResumeStyle
				}
				if launch.Resume.ResumeCommand != "" {
					info.ResumeCommand = launch.Resume.ResumeCommand
				}
				command := sessionpkg.BuildResumeCommand(info)
				if firstProviderSessionStart(info.State, pr.Metadata) && launch.Resume.SessionIDFlag != "" && strings.TrimSpace(info.SessionKey) != "" {
					command = launch.Command + " " + launch.Resume.SessionIDFlag + " " + info.SessionKey
				}
				hints := launch.Hints
				hints.Command = launch.Command
				hints.Env = mergeStringMaps(hints.Env, launch.SessionEnv)
				return entryLaunch{command: command, hints: hints}, nil
			}
		}
	}
	command, err := h.startCommand(id)
	return entryLaunch{command: command, hints: h.runtimeHints()}, err
}
