package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) workerFactory(store beads.Store) (*worker.Factory, error) {
	cfg := s.state.Config()
	var resolveTransport func(template, provider string) string
	if cfg != nil {
		resolveTransport = func(template, provider string) string {
			return configuredSessionTransport(cfg, template, provider)
		}
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              s.state.SessionProvider(),
		CityPath:              s.state.CityPath(),
		SearchPaths:           s.sessionLogPaths(),
		Recorder:              s.state.EventProvider(),
		UsageSink:             s.state.UsageSink(),
		ResolveTransport:      resolveTransport,
		ResolveSessionRuntime: s.resolveWorkerSessionRuntimeWithMetadata,
		ControllerRunning:     s.controllerRunning,
		PokeController:        func() error { s.state.Poke(); return nil },
		WithdrawWaitNudges: func(ids []string) error {
			return withdrawQueuedWaitNudges(s.state.NudgesBeadStore(), s.state.CityPath(), ids)
		},
		Pricing:      cfg.PricingRegistry(),
		ActivityMemo: s.activityMemo,
	})
}

func (s *Server) controllerRunning() bool {
	state, ok := s.state.(interface{ ControllerRunning() bool })
	return ok && state.ControllerRunning()
}

func (s *Server) workerSessionCatalog(store beads.Store) (*worker.SessionCatalog, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}
