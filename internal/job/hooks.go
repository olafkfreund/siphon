package job

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// listenDebounce is the minimum gap between hint-triggered ticks of one source.
var listenDebounce = 5 * time.Second // var so tests can shorten it

// Webhooks returns the /hook/{source} lookup: a verified handler built from the
// current config on each call, so sources added by Apply work at once. It
// returns nil for an unknown or non-webhook source.
func (p *Pipeline) Webhooks() func(source string) http.Handler {
	return func(name string) http.Handler {
		cfg := p.Config()
		s := cfg.Sources[name]
		if s == nil || s.Type != "webhook" {
			return nil
		}
		lim := p.hookLimiters(name)
		var google *source.GoogleVerifier
		audience := s.Audience
		if s.Signature == "google-oidc" {
			google = p.googleVerifier()
			if audience == "" {
				audience = strings.TrimRight(cfg.Server.PublicURL, "/") + "/hook/" + name
			}
		}
		return source.NewWebhook(source.WebhookOptions{
			Name: name, Secret: s.Secret.Value, Signature: s.Signature, SigHeader: s.SigHeader, TokenHeader: s.TokenHeader,
			TimestampHeader: s.TimestampHdr, TokenField: s.TokenField, ReplyText: s.ReplyText, SigPrefix: s.SigPrefix, TimestampSep: s.TimestampSep, IDHeader: strings.TrimPrefix(s.ID, "header."), Format: s.Format, AllowQueryToken: s.AllowQueryToken,
			Audience: audience, ServiceAccount: s.ServiceAccount, Google: google,
			MaxBody: int64(cfg.Limits.HTTPMaxBody), Now: p.Now, PreLimit: lim.pre, Limit: lim.post,
			OnReject: p.rejectThrottle(name).note,
			OnVerificationToken: func(tok string) {
				p.notionMu.Lock()
				defer p.notionMu.Unlock()
				if p.notionPending == nil {
					p.notionPending = map[string]pendingToken{}
				}
				p.notionPending[name] = pendingToken{tok, p.Now()}
			},
		}, p.deliver)
	}
}

// googleVerifier returns the daemon's one Google token verifier, made on first use.
func (p *Pipeline) googleVerifier() *source.GoogleVerifier {
	p.googleOnce.Do(func() {
		p.google = source.NewGoogleVerifier(source.HTTPClient(false, 10*time.Second, 1<<20), p.Now)
	})
	return p.google
}

// rejectEvery is the least gap between database writes of one source's refused
// deliveries; a flood of bad requests costs one write per gap, not one each.
var rejectEvery = 10 * time.Second // var so tests can shorten it

// rejects keeps a source's latest refusal and writes it at most once per rejectEvery.
type rejects struct {
	p       *Pipeline
	name    string
	mu      sync.Mutex
	last    time.Time // of the last write
	status  int
	reason  string
	at      time.Time // when the latest refusal happened, not when it is written
	pending bool      // a newer refusal than the last write, flush timer armed
}

func (p *Pipeline) rejectThrottle(name string) *rejects {
	r, _ := p.hookRejects.LoadOrStore(name, &rejects{p: p, name: name})
	return r.(*rejects)
}

func (r *rejects) note(status int, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.p.Now()
	r.status, r.reason, r.at = status, reason, now
	if wait := rejectEvery - now.Sub(r.last); wait > 0 {
		if !r.pending {
			r.pending = true
			time.AfterFunc(wait, r.flush)
		}
		return
	}
	r.write()
}

func (r *rejects) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending {
		r.write()
	}
}

// write stores the latest refusal; r.mu is held.
func (r *rejects) write() {
	r.pending, r.last = false, r.p.Now()
	if err := store.SetSourceReject(r.p.Store.DB, r.name, r.at, r.status, r.reason); err != nil {
		slog.Warn("store webhook rejection", "source", r.name, "err", err)
	}
}

type hookLimit struct{ pre, post *source.Limiter }

// hookLimiters returns the source's rate limiters. They live in the Pipeline,
// not the per-request handler, so a burst is counted across requests and Apply.
// ponytail: entries of deleted sources stay (two small structs each).
func (p *Pipeline) hookLimiters(name string) *hookLimit {
	l, _ := p.hookLimits.LoadOrStore(name, &hookLimit{pre: source.NewLimiter(100, 50), post: source.NewLimiter(20, 10)})
	return l.(*hookLimit)
}

// deliver records the replay key and enqueues in one transaction, so a
// crash can neither lose an accepted webhook nor accept it twice.
func (p *Pipeline) deliver(ctx context.Context, ev source.Event, key string) (bool, error) {
	cfg := p.Config()
	_, ids, dup, ruleErr, err := p.handleEvent(ctx, cfg, rule.Event{Source: ev.Source, Headers: ev.Headers, Data: ev.Data},
		false, "hook:"+ev.Source, key)
	if !dup { // a replay must not overwrite the newer last event
		if serr := store.SetSourceEvent(p.Store.DB, ev.Source, ev.Data, ev.Headers); serr != nil {
			slog.Warn("store last event", "source", ev.Source, "err", serr)
		}
	}
	if ruleErr != nil {
		// Committed anyway: accept (202) so the sender doesn't retry into a 409.
		slog.Warn("webhook rules", "source", ev.Source, "err", pollErrText(cfg, ruleErr))
	}
	if err != nil {
		slog.Warn("webhook", "source", ev.Source, "err", err)
	}
	if len(ids) > 0 {
		p.nudgeWorkers()
	}
	return dup, err
}

// listen turns MCP resource-updated notifications into early ticks. It
// gives up quietly if the server can't subscribe; polling keeps working.
func (p *Pipeline) listen(ctx context.Context, cfg *config.Config, name string, hint chan<- struct{}) {
	s := cfg.Sources[name]
	if s.Type != "mcp" || s.Read == nil || s.Read.Resource == "" {
		return
	}
	for ctx.Err() == nil {
		m := source.MCP{Options: p.mcpOptions(cfg, name)}
		if p.MCPTransport != nil {
			m.Transport = p.MCPTransport(name)
		}
		err := m.Listen(ctx, func() {
			select {
			case hint <- struct{}{}:
			default:
			}
		})
		if errors.Is(err, source.ErrListenUnsupported) {
			slog.Info("listen unsupported, polling only", "source", name)
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listen dropped; reconnecting after poll interval", "source", name, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(s.Poll)):
		}
	}
}

// pendingToken is a Notion verification token awaiting an operator's accept.
type pendingToken struct {
	Token string
	At    time.Time
}

// NotionPending is the latest captured token of a source and when it came.
func (p *Pipeline) NotionPending(source string) (string, time.Time, bool) {
	p.notionMu.Lock()
	defer p.notionMu.Unlock()
	t, ok := p.notionPending[source]
	return t.Token, t.At, ok
}

// TakeNotionPending returns the pending token and clears it.
func (p *Pipeline) TakeNotionPending(source string) (string, bool) {
	p.notionMu.Lock()
	defer p.notionMu.Unlock()
	t, ok := p.notionPending[source]
	delete(p.notionPending, source)
	return t.Token, ok
}
