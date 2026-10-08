// Package notify tells a person when a job needs approval, fails, or a source
// breaks. It scans the database on a timer, records each message once in the
// outbox, and delivers it to the configured channels. It creates no event,
// job or rule state, so a notification can never cause another one.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

const (
	scanEvery      = 15 * time.Second
	reminderWithin = 4 * time.Hour
	sourceAfter    = 15 * time.Minute
	hourlyCap      = 30
	batch          = 50
)

// backoff is the wait before each retry; after the last one the row fails.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

type Notifier struct {
	Store  *store.Store
	Config func() *config.Config
	Now    func() time.Time
	// Client returns the client for a channel's url; tests inject it.
	Client func(cfg *config.Config, rawURL string) (*http.Client, func(), error)
}

func New(st *store.Store, cfg func() *config.Config, now func() time.Time) *Notifier {
	return &Notifier{Store: st, Config: cfg, Now: now, Client: GuardedClient}
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Run scans and delivers every 15 s until ctx is done.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(scanEvery)
	defer t.Stop()
	for {
		if err := n.Scan(ctx); err != nil {
			slog.Error("notify scan", "err", err)
		}
		if err := n.Deliver(ctx); err != nil {
			slog.Error("notify deliver", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func dur(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func kindOf(k string) string {
	if k == "" {
		return "action"
	}
	return k
}

// Scan records the messages each channel is owed. Nothing older than a
// channel's baseline is ever recorded.
func (n *Notifier) Scan(ctx context.Context) error {
	cfg, db, now := n.Config(), n.Store.DB, n.now()
	names, err := store.ChannelNames(db)
	if err != nil {
		return err
	}
	for _, name := range names { // a deleted channel loses its baseline, so re-adding starts fresh
		if cfg.Notify[name] == nil {
			if err := store.DropChannel(db, name); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Notify)) {
		ch := cfg.Notify[name]
		if ch == nil {
			continue
		}
		if err := n.scanChannel(cfg, name, ch, now); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) scanChannel(cfg *config.Config, name string, ch *config.Notify, now time.Time) error {
	db := n.Store.DB
	since, err := store.ChannelSince(db, name, now)
	if err != nil {
		return err
	}
	want := func(ev string) bool { return len(ch.Events) == 0 || slices.Contains(ch.Events, ev) }
	add := func(m Message, key string) error {
		_, err := store.EnqueueNotification(db, store.Notification{Channel: name, Event: m.Event, Key: key, JobID: m.Job,
			Source: m.Source, Title: m.Title, Body: m.Body}, now)
		return err
	}
	if want("approval") || want("reminder") {
		aps, err := store.PendingApprovalsFor(db, since)
		if err != nil {
			return err
		}
		for _, a := range aps {
			key := fmt.Sprintf("job:%d:%d", a.ID, a.ResumeStep)
			left := a.ExpiresAt.Sub(now)
			if want("approval") {
				if err := add(Message{Event: "approval", Title: "Approval needed", Rule: a.Rule, Job: a.ID,
					Body: fmt.Sprintf("Rule %s: job %d (%s) is waiting for approval. Expires in %s.", a.Rule, a.ID, kindOf(a.Kind), dur(left))}, key); err != nil {
					return err
				}
			}
			if want("reminder") && left <= reminderWithin {
				if err := add(Message{Event: "reminder", Title: "Approval reminder", Rule: a.Rule, Job: a.ID,
					Body: fmt.Sprintf("Rule %s: job %d (%s) still waits for approval; %s left.", a.Rule, a.ID, kindOf(a.Kind), dur(left))}, key); err != nil {
					return err
				}
			}
		}
	}
	if want("failed") {
		js, err := store.FailedJobsSince(db, since)
		if err != nil {
			return err
		}
		for _, j := range js {
			if err := add(Message{Event: "failed", Title: "Job failed", Rule: j.Rule, Job: j.ID,
				Body: fmt.Sprintf("Rule %s: job %d (%s) failed.", j.Rule, j.ID, kindOf(j.Kind))}, "job:"+strconv.FormatInt(j.ID, 10)); err != nil {
				return err
			}
		}
	}
	if want("source") {
		fs, err := store.FailingSources(db)
		if err != nil {
			return err
		}
		failing := map[string]bool{}
		for _, f := range fs {
			key := fmt.Sprintf("src:%s:%d", f.Name, f.Since.UnixMilli())
			failing[key] = true
			if now.Sub(f.Since) >= sourceAfter && !f.Since.Before(since) {
				if err := add(Message{Event: "source", Title: "Source failing", Source: f.Name,
					Body: fmt.Sprintf("Source %s has been failing for %s.", f.Name, dur(now.Sub(f.Since)))}, key); err != nil {
					return err
				}
			}
		}
		eps, err := store.SentSourceEpisodes(db, name)
		if err != nil {
			return err
		}
		for _, e := range eps {
			if !failing[e.Key] { // healthy again (or failing anew under a new key)
				if err := add(Message{Event: "source_ok", Title: "Source recovered", Source: e.Source,
					Body: fmt.Sprintf("Source %s is polling again.", e.Source)}, e.Key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Deliver sends the due rows, at most 50 a call.
func (n *Notifier) Deliver(ctx context.Context) error {
	cfg, db, now := n.Config(), n.Store.DB, n.now()
	due, err := store.DueNotifications(db, now, batch)
	if err != nil {
		return err
	}
	for _, row := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ch := cfg.Notify[row.Channel]
		if ch == nil {
			if err := store.MarkFailed(db, row.ID, now, "channel no longer exists"); err != nil {
				return err
			}
			continue
		}
		if sent, err := store.SentInLastHour(db, row.Channel, now); err != nil {
			return err
		} else if sent >= hourlyCap {
			if err := store.MarkSuppressed(db, row.ID); err != nil {
				return err
			}
			continue
		}
		m := Message{Event: row.Event, Title: row.Title, Body: row.Body, Source: row.Source, Job: row.JobID, At: row.CreatedAt}
		if row.JobID != 0 {
			db.QueryRow(`SELECT rule FROM jobs WHERE id=?`, row.JobID).Scan(&m.Rule)
		}
		// ponytail: the count is taken before the send, so a failed send loses it from the suffix.
		if k, err := store.TakeSuppressed(db, row.Channel); err != nil {
			return err
		} else if k > 0 {
			m.Body += fmt.Sprintf(" (%d more suppressed)", k)
		}
		status, serr := n.post(ctx, cfg, ch, m)
		switch {
		case serr == nil:
			err = store.MarkSent(db, row.ID, now)
		case retryable(status) && row.Attempts < len(backoff):
			err = store.MarkRetry(db, row.ID, now.Add(backoff[row.Attempts]), serr.Error())
		default:
			err = store.MarkFailed(db, row.ID, now, serr.Error())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// retryable: no answer, a server error, or 408/429. Other 4xx and redirects are final.
func retryable(status int) bool {
	return status == 0 || status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// post sends m and returns the HTTP status (0 if none) and an error whose
// text is masked against the url and token.
func (n *Notifier) post(ctx context.Context, cfg *config.Config, ch *config.Notify, m Message) (int, error) {
	secrets := []string{ch.URL.Value, ch.Token.Value}
	if u, err := url.Parse(ch.URL.Value); err == nil && len(u.Path) > 1 {
		secrets = append(secrets, u.Path, strings.Trim(u.Path, "/")) // the topic or hook id is the credential
	}
	mask := func(s string) error { return errors.New(string(action.Mask([]byte(s), secrets))) }
	client, closeFn, err := n.Client(cfg, ch.URL.Value)
	if err != nil {
		return 0, mask(err.Error())
	}
	if closeFn != nil {
		defer closeFn()
	}
	req, err := request(ctx, cfg, ch, m)
	if err != nil {
		return 0, mask("bad request")
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error repeats the url
		}
		return 0, mask("send failed: " + err.Error())
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse)) // never kept: it may echo the token
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("the channel answered HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// Send delivers m to a channel now, bypassing the outbox (for `notify test`).
// It records only an audit row, and returns the HTTP status and a masked error.
func (n *Notifier) Send(ctx context.Context, name string, m Message, actor string) (int, error) {
	cfg := n.Config()
	ch := cfg.Notify[name]
	if ch == nil {
		return 0, fmt.Errorf("no notification channel %q", name)
	}
	if m.At.IsZero() {
		m.At = n.now()
	}
	status, err := n.post(ctx, cfg, ch, m)
	detail := name + ": ok"
	if err != nil {
		detail = name + ": " + err.Error()
	}
	if tx, terr := n.Store.DB.Begin(); terr == nil {
		if store.Audit(tx, n.now(), actor, "notify_test", 0, detail) == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	return status, err
}

// TestMessage is what `notify test` sends.
func TestMessage() Message {
	return Message{Event: "test", Title: "Siphon test", Body: "This is a test message from Siphon."}
}
