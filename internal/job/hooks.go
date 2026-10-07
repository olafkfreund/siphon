package job

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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
		return source.NewWebhook(source.WebhookOptions{
			Name: name, Secret: s.Secret.Value, Signature: s.Signature, SigHeader: s.SigHeader, TokenHeader: s.TokenHeader,
			TimestampHeader: s.TimestampHdr, IDHeader: strings.TrimPrefix(s.ID, "header."),
			MaxBody: int64(cfg.Limits.HTTPMaxBody), Now: p.Now, PreLimit: lim.pre, Limit: lim.post,
			OnReject: func(status int, reason string) {
				if err := store.SetSourceReject(p.Store.DB, name, p.Now(), status, reason); err != nil {
					slog.Warn("store webhook rejection", "source", name, "err", err)
				}
			},
		}, p.deliver)
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
	if serr := store.SetSourceEvent(p.Store.DB, ev.Source, ev.Data); serr != nil {
		slog.Warn("store last event", "source", ev.Source, "err", serr)
	}
	_, ids, dup, ruleErr, err := p.handleEvent(ctx, p.Config(), rule.Event{Source: ev.Source, Headers: ev.Headers, Data: ev.Data},
		false, "hook:"+ev.Source, key)
	if ruleErr != nil {
		// Committed anyway: accept (202) so the sender doesn't retry into a 409.
		slog.Warn("webhook rules", "source", ev.Source, "err", ruleErr)
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
		m := source.MCP{Options: mcpOptions(cfg, name)}
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
