package job

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/servicenow"
	"github.com/olafkfreund/siphon/internal/source"
)

var sysIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// serviceNowHTTP builds the guarded client; tests swap it for a plain one.
var serviceNowHTTP = func(allowPrivate bool) *http.Client {
	return source.HTTPClient(allowPrivate, 30*time.Second, 4<<20)
}

// runServiceNow makes one ServiceNow call in the daemon. exit is 0 on success
// and -1 on failure; out is the text for the job, stdout the JSON for later steps.
func runServiceNow(ctx context.Context, cfg *config.Config, a *config.ServiceNowAction, env map[string]any) (exit int, out string, stdout []byte) {
	res, table, err := doServiceNow(ctx, cfg, a, env)
	if err != nil {
		return -1, string(action.Mask([]byte(err.Error()), cfg.Secrets())), nil
	}
	ref := str(res["number"])
	if ref == "" {
		ref = str(res["sys_id"])
	}
	if a.Op == "get" {
		stdout, _ = json.Marshal(res)
	} else {
		stdout, _ = json.Marshal(map[string]string{"op": a.Op, "table": table, "sys_id": str(res["sys_id"]), "number": str(res["number"])})
	}
	return 0, fmt.Sprintf("servicenow %s %s %s", a.Op, table, ref), action.Mask(stdout, cfg.Secrets())
}

func doServiceNow(ctx context.Context, cfg *config.Config, a *config.ServiceNowAction, env map[string]any) (map[string]any, string, error) {
	table := a.Table
	if table == "" || a.Op == "create_change" {
		table = "incident"
		if a.Op == "create_change" {
			table = "change_request"
		}
	}
	sysID, err := action.RenderPrompt(a.SysID, env)
	if err != nil {
		return nil, table, fmt.Errorf("servicenow: sys_id: %w", err)
	}
	if a.SysID != "" && !sysIDRe.MatchString(sysID) {
		return nil, table, fmt.Errorf("servicenow: sys_id %q is not 32 hex characters", sysID)
	}
	f := make(map[string]string, len(a.Fields))
	for k, v := range a.Fields {
		if f[k], err = action.RenderPrompt(v, env); err != nil {
			return nil, table, fmt.Errorf("servicenow: field %s: %w", k, err)
		}
	}
	src := cfg.Sources[a.Connection]
	if src == nil {
		return nil, table, fmt.Errorf("servicenow: unknown connection %q", a.Connection)
	}
	c, err := servicenow.ClientFromEnv(func(k string) string { return src.Env[k].Value },
		serviceNowHTTP(cfg.ServiceEndpoint(src.Env["SERVICENOW_INSTANCE_URL"].Value)))
	if err != nil {
		return nil, table, err
	}
	body := make(map[string]any, len(f)+1)
	for k, v := range f {
		body[k] = v
	}
	var res map[string]any
	switch a.Op {
	case "get":
		var fields []string
		if l := f["fields"]; l != "" {
			fields = strings.Split(l, ",")
		}
		res, err = c.Get(ctx, table, sysID, fields)
	case "create":
		res, err = c.Create(ctx, table, body)
	case "update":
		res, err = c.Patch(ctx, table, sysID, body)
	case "add_work_note":
		res, err = c.Patch(ctx, table, sysID, map[string]any{"work_notes": f["text"]})
	case "add_comment":
		res, err = c.Patch(ctx, table, sysID, map[string]any{"comments": f["text"]})
	case "resolve":
		res, err = c.Patch(ctx, table, sysID, map[string]any{"state": "6", "close_code": f["close_code"], "close_notes": f["close_notes"]})
	case "create_change":
		kind := f["type"]
		delete(body, "type") // the kind is in the URL
		res, err = c.CreateChange(ctx, kind, body)
	default:
		err = fmt.Errorf("servicenow: unknown op %q", a.Op)
	}
	return res, table, err
}

// str reads a record field: a string, or the {"value": …} object the change API returns.
func str(v any) string {
	if m, ok := v.(map[string]any); ok {
		v = m["value"]
	}
	s, _ := v.(string)
	return s
}
