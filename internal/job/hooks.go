package job

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/source"
	"github.com/olafkfreund/siphon/internal/store"
)

// listenDebounce is the minimum gap between hint-triggered ticks of one source.
var listenDebounce = 5 * time.Second // var so tests can shorten it

// Webhooks returns one verified handler per webhook source, for /hook/{source}.
func (p *Pipeline) Webhooks() map[string]http.Handler {
	hooks := map[string]http.Handler{}
	for name, s := range p.Cfg.Sources {
		if s.Type != "webhook" {
			continue
		}
		hooks[name] = source.NewWebhook(source.WebhookOptions{
			Name: name, Secret: s.Secret.Value, Signature: s.Signature, SigHeader: s.SigHeader,
			TimestampHeader: s.TimestampHdr, IDHeader: strings.TrimPrefix(s.ID, "header."),
			MaxBody: int64(p.Cfg.Limits.HTTPMaxBody), Now: p.Now,
		}, p.deliver)
	}
	return hooks
}

// deliver records the replay key and enqueues in one transaction, so a
// crash can neither lose an accepted webhook nor accept it twice.
func (p *Pipeline) deliver(ctx context.Context, ev source.Event, key string) (bool, error) {
	if serr := store.SetSourceEvent(p.Store.DB, ev.Source, ev.Data); serr != nil {
		slog.Warn("store last event", "source", ev.Source, "err", serr)
	}
	_, ids, dup, ruleErr, err := p.handleEvent(ctx, rule.Event{Source: ev.Source, Headers: ev.Headers, Data: ev.Data},
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
func (p *Pipeline) listen(ctx context.Context, name string, hint chan<- struct{}) {
	s := p.Cfg.Sources[name]
	if s.Type != "mcp" || s.Read == nil || s.Read.Resource == "" {
		return
	}
	for ctx.Err() == nil {
		m := source.MCP{Options: p.mcpOptions(name)}
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
