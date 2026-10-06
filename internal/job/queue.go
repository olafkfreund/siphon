package job

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

// idlePoll is how long an idle worker waits before checking the queue again.
const idlePoll = time.Second

// Requeue is the startup recovery for jobs a previous process left `running`.
func (p *Pipeline) Requeue() error {
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
	p.nudge = make(chan struct{}, 64) // wakes idle workers when jobs are enqueued
	nudge := p.nudge
	var wg sync.WaitGroup
	spawn := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	for _, name := range sortedSources(p.Cfg) {
		if s := p.Cfg.Sources[name]; s.Type != "webhook" {
			hint := make(chan struct{}, 1)
			spawn(func() { p.listen(ctx, name, hint) })
			spawn(func() { p.pollLoop(ctx, name, time.Duration(s.Poll), hint) })
		}
	}
	for i := 0; i < p.Cfg.Server.Workers; i++ {
		spawn(func() { p.worker(ctx, nudge) })
	}
	spawn(func() { p.retention(ctx) })
	spawn(func() { p.expiryLoop(ctx) })

	wg.Wait()
	return nil
}

func (p *Pipeline) pollLoop(ctx context.Context, name string, every time.Duration, hint <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := p.tickAndNudge(ctx, name)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-hint:
			// ponytail: hints within listenDebounce of the last tick are dropped;
			// the next ticker poll still catches the change.
			if time.Since(last) < listenDebounce {
				continue
			}
		}
		last = p.tickAndNudge(ctx, name)
	}
}

func (p *Pipeline) tickAndNudge(ctx context.Context, name string) time.Time {
	start := time.Now()
	ids, err := p.Tick(ctx, name)
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
