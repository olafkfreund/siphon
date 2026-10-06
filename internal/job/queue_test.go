package job

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func newPipeline(t *testing.T, workers int) *Pipeline {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Pipeline{
		Cfg:   &config.Config{Server: config.Server{Workers: workers, Sandbox: "none"}},
		Store: st, Now: time.Now,
	}
}

func insertJobs(t *testing.T, p *Pipeline, n int, state, payload string) {
	tx, _ := p.Store.DB.Begin()
	for i := 0; i < n; i++ {
		if _, err := store.InsertJob(tx, store.Job{Rule: "r", ActionJSON: payload, State: state}, p.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestEightWorkersEachJobOnce(t *testing.T) {
	p := newPipeline(t, 8)
	const n = 200
	insertJobs(t, p, n, "queued", "{}")
	var mu sync.Mutex
	runs := map[int64]int{}
	p.runFn = func(_ context.Context, j store.QueuedJob) (string, int, string) {
		mu.Lock()
		runs[j.ID]++
		mu.Unlock()
		return "done", 0, ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx) }()

	deadline := time.Now().Add(20 * time.Second)
	for {
		var d int
		p.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='done'`).Scan(&d)
		if d == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d done", d, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != n {
		t.Fatalf("%d distinct jobs ran, want %d", len(runs), n)
	}
	for id, c := range runs {
		if c != 1 {
			t.Fatalf("job %d ran %d times", id, c)
		}
	}
}

func TestRunOnceRequeuesInterruptedJobs(t *testing.T) {
	p := newPipeline(t, 1)
	insertJobs(t, p, 1, "running", `{"action":{"cmd":["true"]},"env":{}}`)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st string
	var at int
	p.Store.DB.QueryRow(`SELECT state, attempt FROM jobs`).Scan(&st, &at)
	if st != "done" || at != 1 {
		t.Fatalf("%s attempt=%d", st, at)
	}
}

func TestServeRequeuesAtStartupAndFailsAfterThree(t *testing.T) {
	p := newPipeline(t, 1)
	insertJobs(t, p, 1, "running", "{}")
	p.Store.DB.Exec(`UPDATE jobs SET attempt=2`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	p.Store.DB.QueryRow(`SELECT state FROM jobs`).Scan(&st)
	if st != "failed" {
		t.Fatal(st)
	}
}
