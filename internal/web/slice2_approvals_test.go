package web

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/olafkfreund/siphon/internal/store"
)

// pendingAgentJob is a waiting job of rule "disk-full" (source disk, http) running an agent.
func (e *env) pendingAgentJob(expires time.Duration) int64 {
	tx, _ := e.st.DB.Begin()
	id, err := store.InsertJob(tx, store.Job{Rule: "disk-full", State: "pending_approval",
		ActionJSON: `{"action":{"Agent":"fixer"},"agents":{"fixer":{"Kind":"claude"}}}`}, e.now)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := store.CreateApproval(tx, id, make([]byte, 32), e.now.Add(expires)); err != nil {
		e.t.Fatal(err)
	}
	tx.Commit()
	return id
}

func TestApprovalsCard(t *testing.T) {
	e := newEnv(t, nil)
	id := strconv.Itoa(int(e.pendingAgentJob(10 * time.Minute)))
	b := e.page("/approvals")
	for _, want := range []string{
		"item-card is-wait", `data-kind="agent"`, "#" + id, "flow flow-2", `<code class="node-target" title="disk">disk</code>`,
		"fixer", "claude", `stamp err`, "expires in 10 min",
		`data-confirm="Deny job ` + id + `? It will be cancelled."`,
		`aria-label="Deny job ` + id + ` (disk-full)"`, `aria-label="Approve job ` + id + ` (disk-full)"`,
		`action="/approvals/` + id + `/approve"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(b, "filter-bar") || strings.ContainsAny(b, "✋⟳◷✓✕⊘") || strings.Contains(b, "need-card") {
		t.Error("one approval: no filter bar, no emoji, no dead class")
	}
	// a second approval, far from expiry, brings the filter bar and a plain stamp
	e.pendingAgentJob(5 * time.Hour)
	b = e.page("/approvals")
	if !strings.Contains(b, `data-filter-scope="approvals"`) || !strings.Contains(b, "stamp wait") {
		t.Error("two approvals: want the filter bar and a wait stamp")
	}
}

func TestApprovalsViewerSeesNoForms(t *testing.T) {
	e := newEnv(t, nil)
	e.pendingAgentJob(time.Hour)
	c, _ := e.asRole(roleViewer, "v")
	b := e.do("GET", "/approvals", nil, func(r *http.Request) { r.AddCookie(c) }).Body.String()
	if regexp.MustCompile(`<form[^>]*approvals/`).MatchString(b) || !strings.Contains(b, "An operator decides.") {
		t.Errorf("viewer: forms or no note")
	}
}
