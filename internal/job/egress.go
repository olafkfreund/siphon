package job

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/action"
	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/egress"
)

// needsEgress reports whether any agent or rule runs with an allowlist, so
// the proxy must be up before any job runs.
func (p *Pipeline) needsEgress() bool {
	for name := range p.Cfg.Agents {
		if _, on := p.Cfg.AgentEgress(p.Cfg.Agents[name]); on {
			return true
		}
	}
	for _, r := range p.Cfg.Rules {
		if _, on := p.Cfg.RuleEgress(r); on {
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
// stops agentgw; egressFor calls it lazily for pipelines used directly.
func (p *Pipeline) startEgress(ctx context.Context) error {
	egressMu.Lock()
	defer egressMu.Unlock()
	if p.egress != nil || !p.needsEgress() {
		return nil
	}
	listen := p.Cfg.Server.Egress.Listen
	if egressListenOverride != "" {
		listen = egressListenOverride
	}
	px := egress.New(listen, nil)
	errc := make(chan error, 1)
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
	p.egress = px
	return nil
}

// egressFor registers a run's allowlist with the proxy. env is nil when
// egress is off (the run uses the open template). finish releases the
// registration and returns a summary of blocked hosts for the job output.
func (p *Pipeline) egressFor(jobID int64, allow []config.HostPort, on bool) (env *action.EgressEnv, finish func() string, err error) {
	if !on {
		return nil, func() string { return "" }, nil
	}
	// Start on first use (no-op once running); if it can't start, fail
	// closed: never fall back to the open template.
	if err := p.startEgress(context.Background()); err != nil {
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
		entries[i] = egress.Entry{Host: hp.Host, Port: hp.Port, AllowPrivate: hp.AllowPrivate}
	}
	url, blocked, release := px.Register(entries)
	return &action.EgressEnv{ProxyURL: url}, func() string {
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
func (p *Pipeline) ruleEgress(rule string) ([]config.HostPort, bool) {
	for _, r := range p.Cfg.Rules {
		if r.Name == rule {
			return p.Cfg.RuleEgress(r)
		}
	}
	// Rule renamed or removed since the job was queued: no rule opt-in, but
	// server.egress.cmd_default still applies.
	return p.Cfg.RuleEgress(config.Rule{})
}
