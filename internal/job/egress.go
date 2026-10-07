package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/egress"
)

// needsEgress reports whether any agent or rule runs with an allowlist, so
// the proxy must be up before any job runs.
func needsEgress(cfg *config.Config) bool {
	for name := range cfg.Agents {
		if _, on := cfg.AgentEgress(cfg.Agents[name]); on {
			return true
		}
	}
	for _, r := range cfg.Rules {
		if _, on := cfg.RuleEgress(r); on {
			return true
		}
	}
	return false
}

// egressListenOverride lets tests use an ephemeral port instead of the
// configured 127.77.0.1:3128 (several pipelines in one test binary).
var egressListenOverride string

// egressMu guards lazy proxy start across workers.
var egressMu sync.Mutex

// startEgress starts the egress proxy when needed and waits until it is
// listening. serve/run-once call it up front so a proxy that can't start
// stops siphon; egressFor calls it lazily for pipelines used directly.
func (p *Pipeline) startEgress(ctx context.Context, cfg *config.Config) error {
	egressMu.Lock()
	defer egressMu.Unlock()
	if p.egress != nil || !needsEgress(cfg) {
		return nil
	}
	listen := cfg.Server.Egress.Listen
	if egressListenOverride != "" {
		listen = egressListenOverride
	}
	px := egress.New(listen, nil)
	errc := make(chan error, 2)
	go func() { errc <- px.Start(ctx) }()
	deadline := time.After(5 * time.Second)
	for px.Addr() == "" {
		select {
		case err := <-errc:
			return fmt.Errorf("egress proxy on %s: %w", listen, err)
		case <-deadline:
			return errors.New("egress proxy did not start listening")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if sock := cfg.Server.Egress.Socket; sock != "" {
		os.Remove(sock) // a stale socket must not pass for a live one below
		go func() { errc <- px.ServeUnix(ctx, sock) }()
		for {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			select {
			case err := <-errc:
				return fmt.Errorf("egress proxy on %s: %w", sock, err)
			case <-deadline:
				return errors.New("egress proxy socket did not appear")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	p.egress = px
	return nil
}

// egressFor registers a run's allowlist with the proxy. env is nil when
// egress is off (the run uses the open template). finish releases the
// registration and returns a summary of blocked hosts for the job output.
func (p *Pipeline) egressFor(cfg *config.Config, jobID int64, allow []config.HostPort, on bool) (env *action.EgressEnv, finish func() string, err error) {
	if !on {
		return nil, func() string { return "" }, nil
	}
	// Start on first use (no-op once running); if it can't start, fail
	// closed: never fall back to the open template.
	if err := p.startEgress(context.Background(), cfg); err != nil {
		return nil, nil, fmt.Errorf("egress is enabled for this run but the egress proxy is not running: %v", err)
	}
	egressMu.Lock()
	px := p.egress
	egressMu.Unlock()
	if px == nil {
		return nil, nil, errors.New("egress is enabled for this run but the egress proxy is not running")
	}
	entries := make([]egress.Entry, len(allow))
	for i, hp := range allow {
		entries[i] = egress.Entry{Host: hp.Host, Port: hp.Port, AllowPrivate: hp.AllowPrivate, NoLinkLocal: hp.NoLinkLocal}
	}
	url, blocked, release := px.Register(entries)
	return &action.EgressEnv{ProxyURL: url, Socket: cfg.Server.Egress.Socket}, func() string {
		b := blocked()
		release()
		if len(b) == 0 {
			return ""
		}
		hosts := make([]string, 0, len(b))
		for h := range b {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		var lines []string
		for _, h := range hosts {
			lines = append(lines, fmt.Sprintf("egress: blocked %s (%d)", h, b[h]))
			p.audit("egress_blocked", jobID, h)
		}
		return "\n" + strings.Join(lines, "\n")
	}, nil
}

// ruleEgress is the cmd-action allowlist of the job's rule.
func ruleEgress(cfg *config.Config, rule string) ([]config.HostPort, bool) {
	for _, r := range cfg.Rules {
		if r.Name == rule {
			return cfg.RuleEgress(r)
		}
	}
	// Rule renamed or removed since the job was queued: no rule opt-in, but
	// server.egress.cmd_default still applies.
	return cfg.RuleEgress(config.Rule{})
}
