package job

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// idlePoll is how long an idle worker waits before checking the queue again.
const idlePoll = time.Second

// Requeue is the startup recovery for jobs a previous process left `running`.
// stopOrphans is a seam for tests.
var stopOrphans = action.StopOrphans

func (p *Pipeline) Requeue() error {
	action.CleanBridgeSecrets(filepath.Dir(p.Config().Server.DB))
	if p.Config().Server.Sandbox != "none" {
		// A crashed siphon leaves its siphon-action@ units running (they live
		// outside our cgroup); stop them before their jobs are requeued, so a
		// step never runs twice at once.
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := stopOrphans(sctx)
		cancel()
		if err != nil {
			// Refuse to start: requeueing now could run a step twice at once.
			// systemd restarts siphon and retries.
			return fmt.Errorf("stopping orphaned siphon-action@ units failed (check systemd/polkit), refusing to start: %w", err)
		}
	}
	rq, f, err := store.RequeueRunning(p.Store.DB, p.Now())
	if rq+f > 0 {
		slog.Info("recovered interrupted jobs", "requeued", rq, "failed", f)
	}
	return err
}

// Serve is the daemon loop: startup requeue, one ticker per polled source,
// a worker pool and daily retention. It returns after ctx is cancelled and
// every goroutine has stopped. Jobs interrupted by cancellation stay `running`
// for the next startup's Requeue.
func (p *Pipeline) Serve(ctx context.Context) error {
	if err := p.Requeue(); err != nil {
		return err
	}
	if err := p.startEgress(ctx, p.Config()); err != nil {
		return err
	}
	if p.nudge == nil { // literal Pipelines in tests; serve uses New
		p.nudge = make(chan struct{}, 64)
	}
	nudge := p.nudge
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	p.applyMu.Lock()
	p.serveCtx = ctx
	p.stopPollers = p.startPollers(ctx, p.Config())
	p.applyMu.Unlock()
	for i := 0; i < p.Config().Server.Workers; i++ {
		spawn(func() { p.worker(ctx, nudge) })
	}
	spawn(func() { p.retention(ctx) })
	spawn(func() { p.expiryLoop(ctx) })

	wg.Wait()
	p.applyMu.Lock()
	p.stopPollers() // ctx is cancelled: this just waits for the pollers to exit
	p.stopPollers = nil
	p.applyMu.Unlock()
	return nil
}

// startPollers runs one listener and one poll loop per polled source of cfg
// (a "generation"); stop cancels them and waits for them to exit.
func (p *Pipeline) startPollers(ctx context.Context, cfg *config.Config) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, name := range sortedSources(cfg) {
		if s := cfg.Sources[name]; s.Polled() {
			hint := make(chan struct{}, 1)
			wg.Add(2)
			go func() { defer wg.Done(); p.listen(ctx, cfg, name, hint) }()
			go func() { defer wg.Done(); p.pollLoop(ctx, cfg, name, time.Duration(s.Poll), hint) }()
		}
	}
	return func() { cancel(); wg.Wait() }
}

func (p *Pipeline) pollLoop(ctx context.Context, cfg *config.Config, name string, every time.Duration, hint <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := p.tickAndNudge(ctx, cfg, name)
	var trailing <-chan time.Time // armed when a hint lands inside the debounce window
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-trailing:
		case <-hint:
			// Trailing-edge debounce: a hint inside the window is deferred to the
			// window's end, never dropped, so a change seen mid-tick is re-read.
			if wait := listenDebounce - p.Now().Sub(last); wait > 0 {
				if trailing == nil {
					trailing = time.After(wait)
				}
				continue
			}
		}
		trailing = nil
		last = p.tickAndNudge(ctx, cfg, name)
	}
}

func (p *Pipeline) tickAndNudge(ctx context.Context, cfg *config.Config, name string) time.Time {
	start := p.Now()
	ids, err := p.tick(ctx, cfg, name)
	if err != nil && ctx.Err() == nil {
		slog.Warn("poll failed", "source", name, "err", err)
	}
	for range ids {
		p.nudgeWorkers()
	}
	return start
}

func (p *Pipeline) worker(ctx context.Context, nudge <-chan struct{}) {
	for ctx.Err() == nil {
		ran, err := p.runOne(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("worker", "err", err)
		}
		if ran && err == nil {
			continue
		}
		t := time.NewTimer(idlePoll)
		select {
		case <-ctx.Done():
		case <-nudge:
		case <-t.C:
		}
		t.Stop()
	}
}

func (p *Pipeline) retention(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if err := store.Cleanup(p.Store.DB, p.Now()); err != nil {
			slog.Error("retention", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Pipeline) expiryLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := p.ExpireApprovals(); err != nil {
			slog.Error("approval sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
