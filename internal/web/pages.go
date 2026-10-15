package web

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/action"
	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/cred"
	"github.com/olafkfreund/siphon/internal/store"
)

// jobView is the job detail page: the stored job plus what it runs, why, and
// what the sandbox refused.
type jobView struct {
	store.JobDetail
	Kind      string               `json:"kind"`   // agent | cmd | unit | routine
	Target    string               `json:"target"` // and its name
	Argv      []string             `json:"argv"`   // cmd
	Agent     *config.Agent        `json:"-"`
	AgentName string               `json:"agent,omitempty"`
	Prompt    string               `json:"prompt"` // rendered agent prompt (or the render error)
	CanReach  []config.HostPort    `json:"can_reach"`
	Event     string               `json:"event"` // triggering event, redacted, indented JSON
	Blocked   []blockedHost        `json:"blocked"`
	Steps     []stepView           `json:"steps"`
	Duration  string               `json:"duration"`
	Summary   string               `json:"summary"`       // failed jobs: the last meaningful output line
	Connect   string               `json:"connect"`       // a login the job needed but which isn't connected
	Notes     []store.Notification `json:"notifications"` // notifications about this job

	// the rule's lineage, from the live config
	Source     string `json:"source"`
	SourceType string `json:"source_type"`
	When       string `json:"when"`
	On         string `json:"on"`
	Provider   string `json:"provider"`
}

var notImported = regexp.MustCompile(`credential (\S+) is not imported`)

type blockedHost struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

type stepView struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	State  string `json:"state"`
}

var blockedLine = regexp.MustCompile(`(?m)^egress: blocked (\S+) \((\d+)\)$`)

// payload mirrors the fields of job.Payload the portal reads (web must not
// import job).
type payload struct {
	Action config.Action           `json:"action"`
	Env    map[string]any          `json:"env"`
	Steps  []config.Step           `json:"steps"`
	Agents map[string]config.Agent `json:"agents"`
}

func (s *server) jobDetail(id int64) (*jobView, bool, error) {
	j, ok, err := s.job(id)
	if !ok || err != nil {
		return nil, ok, err
	}
	v := &jobView{JobDetail: j}
	raw, resume, err := store.JobPayload(s.Store.DB, id)
	if err != nil {
		return nil, false, err
	}
	var p payload
	_ = json.Unmarshal([]byte(raw), &p) // an unreadable payload just shows less
	switch a := p.Action; {
	case a.Agent != "":
		v.Kind, v.Target = "agent", a.Agent
		if ag, ok := p.Agents[a.Agent]; ok {
			v.Agent = &ag
		} else {
			v.Agent = s.Config().Agents[a.Agent]
		}
		v.AgentName = a.Agent
		if v.Agent != nil {
			if pr, err := action.RenderPrompt(v.Agent.Prompt, p.Env); err == nil {
				v.Prompt = pr
			} else {
				v.Prompt = "(cannot render: " + err.Error() + ")"
			}
			v.CanReach, _ = s.Config().AgentEgress(v.Agent)
		}
	case a.Routine != "":
		v.Kind, v.Target = "routine", a.Routine
	case a.ServiceNow != nil:
		v.Kind, v.Target = "servicenow", a.ServiceNow.Op+" "+snTable(a.ServiceNow)
		v.Argv, _ = snArgv(a.ServiceNow, p.Env) // an unrenderable value just shows less
	case a.Unit != "":
		v.Kind, v.Target = "unit", a.Unit
	case len(a.Cmd) > 0:
		v.Kind, v.Target = "cmd", a.Cmd[0]
		if argv, err := action.Render(a.Cmd, p.Env); err == nil {
			v.Argv = argv
		} else {
			v.Argv = a.Cmd
		}
	}
	cfg := s.Config() // one config for the whole lookup
	for _, r := range cfg.Rules {
		if r.Name != j.Rule {
			continue
		}
		v.Source, v.When, v.On = r.Source, r.When, r.On
		if v.On == "" {
			v.On = "edge"
		}
		if src := cfg.Sources[r.Source]; src != nil {
			v.SourceType = src.Type
		} else if r.Source == "agent-result" {
			v.SourceType = "agent results"
		}
		break
	}
	if v.Kind == "agent" {
		v.Provider = agentProvider(v.Agent, cfg.Credentials) // v.Agent is the job's snapshot when it has one
	}
	if ev, ok := p.Env["event"]; ok {
		if b, err := json.MarshalIndent(store.RedactKeys(ev), "", "  "); err == nil {
			v.Event = string(b)
		}
	}
	if j.State == "failed" {
		lines := strings.Split(strings.TrimSpace(j.Output), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "egress: blocked") {
				v.Summary = l
				if m := notImported.FindStringSubmatch(l); m != nil {
					v.Connect = m[1]
				}
				break
			}
		}
	}
	for _, m := range blockedLine.FindAllStringSubmatch(j.Output, -1) {
		n, _ := strconv.Atoi(m[2])
		v.Blocked = append(v.Blocked, blockedHost{m[1], n})
	}
	for i, st := range p.Steps {
		sv := stepView{ID: st.ID, State: "queued"}
		switch {
		case st.Agent != "":
			sv.Kind, sv.Target = "agent", st.Agent
		case st.Unit != "":
			sv.Kind, sv.Target = "unit", st.Unit
		case st.ServiceNow != nil:
			sv.Kind, sv.Target = "servicenow", st.ServiceNow.Op+" "+snTable(st.ServiceNow)
		case len(st.Cmd) > 0:
			sv.Kind, sv.Target = "cmd", strings.Join(st.Cmd, " ")
		}
		switch {
		case i < resume || j.State == "done":
			sv.State = "done"
		case i == resume && j.State == "running":
			sv.State = "running"
		case i == resume && j.State == "failed":
			sv.State = "failed"
		case i == resume && j.State == "pending_approval":
			sv.State = "pending_approval"
		}
		v.Steps = append(v.Steps, sv)
	}
	if j.StartedAt != nil {
		end := s.Now()
		if j.FinishedAt != nil {
			end = *j.FinishedAt
		}
		v.Duration = end.Sub(*j.StartedAt).Round(100 * time.Millisecond).String()
	}
	if v.Notes, err = store.ListNotifications(s.Store.DB, store.NotificationFilter{Job: id, Limit: 20}); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// loginView is one credential's metadata; secrets never leave the store.
type loginView struct {
	Name, Provider, Type, Expiry, ExpiryClass, Written string
	Key                                                string   // claude | codex | agy
	Status                                             string   // connected | expiring | expired | missing | apikey
	Agents                                             []string // agents whose credential this is
}

// loginForm is the "Add / connect a login" form state.
type loginForm struct {
	Name, Provider, Kind, Err, Notice string
}

func (s *server) logins() []loginView {
	st := cred.StoreFor(s.Config())
	out := []loginView{}
	users := s.credentialUsers()
	for name, c := range s.Config().Credentials {
		if c.Provider == "ollama" || c.Provider == "openai" || c.Provider == "aws" {
			continue // listed under Models, or managed on Services
		}
		v := loginView{Name: name, Provider: providerName[c.Provider], Key: c.Provider, Type: "Subscription", Expiry: "not connected", ExpiryClass: "cancelled", Written: "—", Status: "missing", Agents: users[name]}
		if c.APIKey.Ref != "" {
			v.Type, v.Expiry, v.Status = "API key", "no expiry", "apikey"
		} else if exp, wrote, err := st.Info(name); err == nil {
			v.Status = "connected"
			v.Written = ago(s.Now(), wrote)
			switch left := exp.Sub(s.Now()); {
			case exp.IsZero():
				v.Expiry, v.ExpiryClass = "unknown", "queued"
			case left <= 0:
				v.Expiry, v.ExpiryClass, v.Status = "expired", "failed", "expired"
			case left < 24*time.Hour:
				v.Expiry, v.ExpiryClass, v.Status = "in "+left.Round(time.Minute).String(), "pending_approval", "expiring"
			default:
				v.Expiry, v.ExpiryClass = "in "+strconv.Itoa(int(left.Hours()/24))+" d", "done"
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// credentialUsers maps a credential to the sorted names of the agents that use it.
func (s *server) credentialUsers() map[string][]string {
	cfg := s.Config()
	out := map[string][]string{}
	for _, n := range sortedKeysOf(cfg.Agents) {
		if c := cfg.Agents[n].Credential; c != "" {
			out[c] = append(out[c], n)
		}
	}
	return out
}

var providerName = map[string]string{"claude": "Claude", "codex": "Codex", "agy": "Antigravity"}

// egressView lists each effective allowlist and the hosts refused recently.
type egressView struct {
	Lists   []egressList    `json:"lists"`
	Blocked []egressBlocked `json:"blocked"`
}

type egressList struct {
	Owner string            `json:"owner"` // agent | rule
	Name  string            `json:"name"`
	On    bool              `json:"on"`
	Hosts []config.HostPort `json:"hosts"`
}

type egressBlocked struct {
	Host  string    `json:"host"`
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	JobID int64     `json:"job_id"`
}

func (s *server) egress() (*egressView, error) {
	v := &egressView{}
	names := make([]string, 0, len(s.Config().Agents))
	for n := range s.Config().Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		hosts, on := s.Config().AgentEgress(s.Config().Agents[n])
		v.Lists = append(v.Lists, egressList{"agent", n, on, hosts})
	}
	for _, r := range s.Config().Rules {
		if hosts, on := s.Config().RuleEgress(r); on {
			v.Lists = append(v.Lists, egressList{"rule", r.Name, true, hosts})
		}
	}
	rows, err := store.ListAudit(s.Store.DB, 500)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for _, a := range rows {
		if a.Event != "egress_blocked" {
			continue
		}
		i, ok := seen[a.Detail]
		if !ok {
			i = len(v.Blocked)
			seen[a.Detail] = i
			b := egressBlocked{Host: a.Detail, Last: a.At}
			if a.JobID != nil {
				b.JobID = *a.JobID
			}
			v.Blocked = append(v.Blocked, b)
		}
		v.Blocked[i].Count++
	}
	return v, nil
}

// runs lists jobs with what they run. Agent runs from before snapshots, or
// whose snapshot lacks a kind, take the provider from the live config.
func (s *server) runs(state string, limit int) ([]store.JobAction, error) {
	js, err := store.ListJobActions(s.Store.DB, state, limit)
	for i := range js {
		switch {
		case js[i].Kind != "agent":
		case js[i].Provider == "": // a run from before snapshots
			js[i].Provider = providerOf(s.Config(), js[i].Target)
		case js[i].Provider == "model": // the snapshot's credential names the real provider
			js[i].Provider = modelProvider("model", js[i].Credential, s.Config().Credentials)
		}
	}
	return js, err
}
