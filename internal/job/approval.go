package job

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

const approvalTTL = 24 * time.Hour

// newApproval inserts the approvals row for a pending job and returns the
// one-shot link path. The token exists only in that return value (and the log
// line the caller writes); the DB keeps its SHA-256, and the audit row the masked path.
func (p *Pipeline) newApproval(tx *sql.Tx, jobID int64, now time.Time) (link string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	if err := store.CreateApproval(tx, jobID, sum[:], now.Add(approvalTTL)); err != nil {
		return "", err
	}
	if err := store.Audit(tx, now, "system", "approval_requested", jobID, fmt.Sprintf("/a/%d/***", jobID)); err != nil {
		return "", err
	}
	return fmt.Sprintf("/a/%d/%s", jobID, token), nil
}

// Decide is the operator path (CLI on the host): no token needed. id is the job id.
func (p *Pipeline) Decide(jobID int64, approve bool, by string) error {
	return store.DecideApproval(p.Store.DB, jobID, approve, by, "", p.Now())
}

// DecideWithToken is the one-shot link path.
func (p *Pipeline) DecideWithToken(jobID int64, token string, approve bool, by string) error {
	if token == "" {
		return store.ErrBadToken
	}
	return store.DecideApproval(p.Store.DB, jobID, approve, by, token, p.Now())
}

// ExpireApprovals fails pending jobs whose approval ran out.
func (p *Pipeline) ExpireApprovals() error {
	n, err := store.ExpireApprovals(p.Store.DB, p.Now())
	if n > 0 {
		slog.Info("approvals expired", "jobs", n)
	}
	return err
}
