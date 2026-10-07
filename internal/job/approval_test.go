package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/rule"
	"github.com/olafkfreund/siphon/internal/store"
)

// pendingJob fires an approve:true rule and returns the job id and the link.
func pendingJob(t *testing.T, p *Pipeline) (int64, string) {
	t.Helper()
	p.Config().Rules = []config.Rule{{Name: "r", Source: "s", When: "true", On: "each", ID: "event.id", Approve: true,
		Action: config.Action{Cmd: []string{"true"}}}}
	_, ids, err := p.HandleEvent(context.Background(), rule.Event{Source: "s", Data: map[string]any{"id": time.Now().UnixNano()}}, false)
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	return ids[0], ""
}

func state(t *testing.T, p *Pipeline, id int64) string {
	var s string
	if err := p.Store.DB.QueryRow(`SELECT state FROM jobs WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func auditCount(p *Pipeline, event string, id int64) (n int) {
	p.Store.DB.QueryRow(`SELECT COUNT(*) FROM audit WHERE event=? AND job_id=?`, event, id).Scan(&n)
	return
}

func TestApproveDenySecondDecision(t *testing.T) {
	p := newPipeline(t, 1)
	a, _ := pendingJob(t, p)
	if state(t, p, a) != "pending_approval" || auditCount(p, "approval_requested", a) != 1 {
		t.Fatal("expected pending job with audit")
	}
	if err := p.Decide(a, true, "olaf"); err != nil {
		t.Fatal(err)
	}
	if state(t, p, a) != "queued" || auditCount(p, "approve", a) != 1 {
		t.Fatal("approve→queued + audit")
	}
	if err := p.Decide(a, false, "olaf"); err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Fatalf("second decision: %v", err)
	}

	b, _ := pendingJob(t, p)
	if err := p.Decide(b, false, "olaf"); err != nil {
		t.Fatal(err)
	}
	if state(t, p, b) != "cancelled" || auditCount(p, "deny", b) != 1 {
		t.Fatal("deny→cancelled + audit")
	}
	var by string
	p.Store.DB.QueryRow(`SELECT decided_by FROM approvals WHERE job_id=?`, b).Scan(&by)
	if by != "olaf" {
		t.Fatal(by)
	}
	if err := p.Decide(999, true, "x"); err == nil {
		t.Fatal("unknown job must error")
	}
}

func TestExpiry(t *testing.T) {
	p := newPipeline(t, 1)
	now := time.Unix(1_800_000_000, 0)
	p.Now = func() time.Time { return now }
	id, _ := pendingJob(t, p)

	now = now.Add(approvalTTL - time.Minute)
	if err := p.ExpireApprovals(); err != nil || state(t, p, id) != "pending_approval" {
		t.Fatal("not yet expired", err)
	}
	now = now.Add(2 * time.Minute)
	if err := p.Decide(id, true, "late"); err == nil {
		t.Fatal("decision after expiry must fail")
	}
	if err := p.ExpireApprovals(); err != nil {
		t.Fatal(err)
	}
	if state(t, p, id) != "failed" || auditCount(p, "approval_expired", id) != 1 {
		t.Fatal("expiry→failed + audit")
	}
	var dec *string
	p.Store.DB.QueryRow(`SELECT decision FROM approvals WHERE job_id=?`, id).Scan(&dec)
	if dec != nil {
		t.Fatal("decision must stay NULL")
	}
}

func TestDecideWithToken(t *testing.T) {
	p := newPipeline(t, 1)
	p.Config().Rules = []config.Rule{{Name: "r", Source: "s", When: "true", Approve: true, Action: config.Action{Cmd: []string{"true"}}}}
	// Capture the link by running newApproval directly, as enqueue does.
	tx, _ := p.Store.DB.Begin()
	jid, _ := store.InsertJob(tx, store.Job{Rule: "r", ActionJSON: "{}", State: "pending_approval"}, p.Now())
	link, err := p.newApproval(tx, jid, p.Now())
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	parts := strings.Split(link, "/") // "", "a", id, token
	token := parts[3]
	if len(token) != 64 {
		t.Fatalf("token: %q", link)
	}

	if err := p.DecideWithToken(jid, "wrong", true, "web"); err != store.ErrBadToken {
		t.Fatalf("wrong token: %v", err)
	}
	if err := p.DecideWithToken(jid, "", true, "web"); err != store.ErrBadToken {
		t.Fatalf("empty token: %v", err)
	}
	if state(t, p, jid) != "pending_approval" || auditCount(p, "approval_bad_token", jid) != 1 {
		t.Fatal("bad token must not decide, must audit")
	}
	var detail string
	p.Store.DB.QueryRow(`SELECT detail FROM audit WHERE event='approval_requested'`).Scan(&detail)
	if strings.Contains(detail, token) {
		t.Fatal("token leaked into audit")
	}
	var stored []byte
	p.Store.DB.QueryRow(`SELECT token_hash FROM approvals WHERE job_id=?`, jid).Scan(&stored)
	if len(stored) != 32 || strings.Contains(string(stored), token) {
		t.Fatal("only the sha256 may be stored")
	}
	if err := p.DecideWithToken(jid, token, true, "web"); err != nil || state(t, p, jid) != "queued" {
		t.Fatal("right token should approve", err)
	}
	if err := p.DecideWithToken(jid, token, false, "web"); err == nil {
		t.Fatal("link is one-shot")
	}
}

func TestRunOnceExpiresApprovals(t *testing.T) {
	p := newPipeline(t, 1)
	now := time.Unix(1_800_000_000, 0)
	p.Now = func() time.Time { return now }
	id, _ := pendingJob(t, p)
	now = now.Add(approvalTTL + time.Second)
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state(t, p, id) != "failed" {
		t.Fatal(state(t, p, id))
	}
}
