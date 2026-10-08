package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
)

// Message is one notification. It carries metadata only: never an event
// payload, a prompt, job output or a token.
type Message struct {
	Event  string // approval | reminder | failed | source | source_ok | test
	Title  string
	Body   string
	Rule   string
	Source string
	Job    int64
	At     time.Time
}

var tags = map[string]string{"approval": "raised_hand", "reminder": "alarm_clock", "failed": "x", "source": "rotating_light", "source_ok": "white_check_mark", "test": "bell"}

// link is where the message points: the portal when server.public_url is set,
// otherwise (url == "") a CLI hint.
func (m Message) link(cfg *config.Config) (url, hint string) {
	base := strings.TrimRight(cfg.Server.PublicURL, "/")
	switch {
	case m.Job != 0 && base != "":
		return base + "/jobs/" + strconv.FormatInt(m.Job, 10), ""
	case m.Job != 0 && (m.Event == "approval" || m.Event == "reminder"):
		return "", "siphon approve " + strconv.FormatInt(m.Job, 10)
	case m.Job != 0:
		return "", "siphon get jobs"
	case m.Source != "" && base != "":
		return base + "/sources", ""
	case m.Source != "":
		return "", "siphon get sources"
	}
	return "", ""
}

// request builds the channel type's HTTP request for m.
func request(ctx context.Context, cfg *config.Config, ch *config.Notify, m Message) (*http.Request, error) {
	link, hint := m.link(cfg)
	var body []byte
	hdr := http.Header{}
	switch ch.Type {
	case "ntfy":
		text := m.Body
		if hint != "" {
			text += "\n" + hint
		}
		body = []byte(text)
		hdr.Set("Title", m.Title)
		hdr.Set("Priority", map[bool]string{true: "high", false: "default"}[m.Event == "approval" || m.Event == "reminder"])
		hdr.Set("Tags", tags[m.Event])
		if link != "" {
			hdr.Set("Click", link)
		}
	case "slack":
		text := "*" + m.Title + "*\n" + m.Body
		if link != "" {
			text += " <" + link + "|Open in Siphon>"
		} else if hint != "" {
			text += " `" + hint + "`"
		}
		body, _ = json.Marshal(map[string]string{"text": text})
		hdr.Set("Content-Type", "application/json")
	default: // webhook
		body, _ = json.Marshal(map[string]any{"event": m.Event, "rule": m.Rule, "job": m.Job, "source": m.Source,
			"title": m.Title, "message": m.Body, "url": link, "at": m.At.UTC().Format(time.RFC3339)})
		hdr.Set("Content-Type", "application/json")
	}
	if ch.Token.Value != "" && ch.Type != "slack" {
		hdr.Set("Authorization", "Bearer "+ch.Token.Value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL.Value, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	return req, nil
}
