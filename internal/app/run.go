package app

import (
	"context"
	"fmt"
	"log/slog"

	"arednlink-hf/internal/control"
	"arednlink-hf/internal/core"
	"arednlink-hf/internal/radio"
	"arednlink-hf/internal/resourceguard"
	"arednlink-hf/internal/status"
)

// Run owns the local control server, resource guard and configured adapters.
func Run(ctx context.Context, cfg Config, logger *slog.Logger, configPath ...string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	path := ""
	if len(configPath) > 0 {
		path = configPath[0]
	}
	backend, err := NewConfigEditor(cfg, path)
	if err != nil {
		return err
	}
	defer func() {
		if err := backend.Close(); err != nil {
			logger.Warn("close configuration directory")
		}
	}()
	for _, key := range cfg.DeprecatedKeys {
		logger.Warn("deprecated configuration key ignored", "key", key)
	}
	engineConfig, err := cfg.coreConfig()
	if err != nil {
		return err
	}
	engine, err := core.New(engineConfig, logger)
	if err != nil {
		return fmt.Errorf("configure peer engine: %w", err)
	}
	guard, err := resourceguard.New(cfg.ResourceGuard)
	if err != nil {
		return err
	}
	return runServices(ctx, cfg, engine, backend, guard, logger)
}

// radioResult reports which configured adapter stopped and why.
type radioResult struct {
	index int
	err   error
}

// radioSetup holds the per-adapter status and beacon settings, in configured order.
type radioSetup struct {
	adapters []RadioConfig
	beacons  []radio.BeaconConfig
	states   []*status.RadioStatus
	// aggregate is the status the control server reports; with a single adapter it is also that adapter's state.
	aggregate *status.RadioStatus
	multiple  bool
}

func (r radioSetup) stateFor(index int) *status.RadioStatus {
	if r.multiple {
		return r.states[index]
	}
	return r.aggregate
}

func newRadioSetup(cfg Config, guard *resourceguard.Monitor) (radioSetup, error) {
	setup := radioSetup{adapters: cfg.Radios, multiple: len(cfg.Radios) > 0}
	if len(setup.adapters) == 0 && cfg.Radio.Kind != "none" {
		setup.adapters = []RadioConfig{cfg.Radio}
	}
	setup.aggregate = status.NewRadioStatus(cfg.Radio.Kind, cfg.Radio.Beacon.Enabled)
	if setup.multiple {
		setup.aggregate = status.NewRadioStatus("multiple")
	}
	for _, adapter := range setup.adapters {
		beacon, err := adapter.beaconConfig()
		if err != nil {
			return radioSetup{}, err
		}
		setup.beacons = append(setup.beacons, beacon)
		setup.states = append(setup.states, status.NewRadioStatus(adapter.Kind, adapter.Beacon.Enabled))
	}
	if setup.multiple {
		setup.aggregate.SetAdapters(setup.states...)
	}
	setup.aggregate.SetResourceGuard(guard)
	return setup, nil
}

// serviceGroup owns the goroutines started by runServices and joins them.
type serviceGroup struct {
	cancel    context.CancelFunc
	control   chan error
	guard     chan error
	probe     chan error
	radioDone chan radioResult
	active    []bool
}

func startServices(runCtx context.Context, cancel context.CancelFunc, setup radioSetup, socket string, engine *core.Engine, editor control.ConfigBackend, guard *resourceguard.Monitor, logger *slog.Logger) *serviceGroup {
	// radioDone holds one slot per adapter so a stopping adapter never blocks on send.
	g := &serviceGroup{cancel: cancel, control: make(chan error, 1), guard: make(chan error, 1), probe: make(chan error, 1),
		radioDone: make(chan radioResult, len(setup.adapters)), active: make([]bool, len(setup.adapters))}
	go func() { g.control <- control.Run(runCtx, socket, engine, setup.aggregate, editor, logger) }()
	go func() { g.guard <- guard.Run(runCtx, logger) }()
	go func() { g.probe <- engine.Run(runCtx) }()
	for index, adapter := range setup.adapters {
		g.active[index] = true
		go func(index int, adapter RadioConfig, beacon radio.BeaconConfig, adapterState *status.RadioStatus) {
			g.radioDone <- radioResult{index, runRadio(runCtx, adapter, beacon, engine, adapterState, logger)}
		}(index, adapter, setup.beacons[index], setup.stateFor(index))
	}
	return g
}

func (g *serviceGroup) joinRadios() {
	for _, running := range g.active {
		if running {
			<-g.radioDone
		}
	}
}

// stop cancels every service and joins the radios and the probe worker.
func (g *serviceGroup) stop() error {
	g.cancel()
	g.joinRadios()
	return <-g.probe
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func runServices(ctx context.Context, cfg Config, engine *core.Engine, editor control.ConfigBackend, guard *resourceguard.Monitor, logger *slog.Logger) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	setup, err := newRadioSetup(cfg, guard)
	if err != nil {
		return err
	}
	g := startServices(runCtx, cancel, setup, cfg.ControlSocket, engine, editor, guard, logger)
	for {
		select {
		case err := <-g.control:
			probeErr := g.stop()
			return firstError(<-g.guard, err, probeErr)
		case err := <-g.guard:
			probeErr := g.stop()
			return firstError(err, <-g.control, probeErr)
		case result := <-g.radioDone:
			g.active[result.index] = false
			if runCtx.Err() == nil {
				setup.stateFor(result.index).Set("failed")
				logger.Error("radio adapter stopped", "kind", setup.adapters[result.index].Kind, "error", result.err)
			}
		}
	}
}

func runRadio(ctx context.Context, cfg RadioConfig, beacon radio.BeaconConfig, engine *core.Engine, state *status.RadioStatus, logger *slog.Logger) error {
	return radio.Run(ctx, radio.Config{Kind: cfg.Kind, Endpoint: cfg.Endpoint, Callsign: cfg.Callsign,
		Destination: cfg.Destination, AllowRemote: cfg.AllowRemote, OnState: state.Set, OnReceive: state.ObserveReceive,
		Beacon: beacon, OnTransmit: state.SetTransmit, OnTransmitRequest: state.SetTransmitRequest,
		OnTransmitRefused: state.SetTransmitRefused},
		func(ctx context.Context, message radio.Message) error {
			return engine.Observe(ctx, message.Source, message.Transport, message.Payload)
		}, logger)
}
