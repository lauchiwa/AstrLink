package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin/adapters"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

const (
	forkCheckinSettingsFile = "fork-checkin.json"
	// forkCheckinStopTarget is the measured shutdown budget. Missing it is
	// logged and then waited out: shared keys are never cleared under a
	// worker that is still running.
	forkCheckinStopTarget = 10 * time.Second
)

type forkCheckinExtension struct {
	facade      *forkcheckin.Facade
	handler     http.Handler
	cancelStart context.CancelFunc
	started     chan struct{}
	logf        func(string, ...any)
}

// newForkCheckinExtension reads only the private switch file. Tables, the
// scheduler and site clients are created by start, and only when the stored
// switch is on; a missing or damaged file leaves the extension off.
func newForkCheckinExtension(store *sqlite.Store, dataDirectory string, logf func(string, ...any)) (*forkCheckinExtension, error) {
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: filepath.Join(dataDirectory, forkCheckinSettingsFile),
		Factory:      store.ForkCheckinModuleFactory(newForkCheckinAdapter),
	})
	if err != nil {
		return nil, err
	}
	return &forkCheckinExtension{facade: facade, handler: forkcheckin.NewAPIHandler(facade), logf: logf}, nil
}

func newForkCheckinAdapter(factory *forkcheckin.TransportFactory) (forkcheckin.SiteAdapter, error) {
	read, err := adapters.NewNewAPIRead(factory, adapters.NewAPILegacy)
	if err != nil {
		return nil, err
	}
	return adapters.NewNewAPISubmit(read)
}

// start initializes in the background so a slow extension never delays the
// Core; a failure is visible in the extension status, not fatal.
func (extension *forkCheckinExtension) start(ctx context.Context) {
	startCtx, cancel := context.WithCancel(ctx)
	extension.cancelStart = cancel
	extension.started = make(chan struct{})
	go func() {
		defer close(extension.started)
		extension.facade.Start(startCtx)
	}()
}

// stop must run before the shared Store closes.
func (extension *forkCheckinExtension) stop() {
	if extension.cancelStart != nil {
		extension.cancelStart()
		<-extension.started
	}
	ctx, cancel := context.WithTimeout(context.Background(), forkCheckinStopTarget)
	err := extension.facade.Stop(ctx)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) {
		extension.logf("check-in extension missed its %s stop target; waiting for its workers before closing storage", forkCheckinStopTarget)
		err = extension.facade.Stop(context.Background())
	}
	if err != nil {
		extension.logf("stop check-in extension: %v", err)
	}
}
