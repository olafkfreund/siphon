package web

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

func itoa(i int) string { return strconv.Itoa(i) }

type sourceView struct {
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Poll       string     `json:"poll,omitempty"`
	LastPollAt *time.Time `json:"last_poll_at"`
	LastError  string     `json:"last_error"`
	Health     string     `json:"health"` // ok | stale | error | idle

	// schedule sources only
	At        string     `json:"at,omitempty"`
	Timezone  string     `json:"timezone,omitempty"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
}

// fillSchedule sets a schedule source's next and last run. The next moment
// follows the last one handled (source_state), as the scheduler computes it.
func (s *server) fillSchedule(v *sourceView, src *config.Source) {
	v.At = src.At
	sched, err := config.ParseSchedule(src.At)
	loc, lerr := src.Location()
	if err != nil || lerr != nil {
		return
	}
	v.Timezone = loc.String()
	base := s.Now()
	if v.LastPollAt != nil {
		base = *v.LastPollAt
	}
	if n := sched.Next(base.In(loc)); !n.IsZero() {
		v.NextRunAt = &n
	}
	if raw, _ := store.SourceEvent(s.Store.DB, v.Name); raw != "" {
		var ev struct {
			ScheduledAt string `json:"scheduled_at"`
		}
		if json.Unmarshal([]byte(raw), &ev) == nil {
			if t, err := time.Parse(time.RFC3339, ev.ScheduledAt); err == nil {
				v.LastRunAt = &t
			}
		}
	}
}

type ruleView struct {
	Name       string     `json:"name"`
	Source     string     `json:"source"`
	SourceType string     `json:"source_type"`
	When       string     `json:"when"`
	On         string     `json:"on"`
	Cooldown   string     `json:"cooldown,omitempty"`
	Kind       string     `json:"kind"`
	Target     string     `json:"target"`
	Action     string     `json:"action"`
	Enabled    bool       `json:"enabled"`
	Overridden bool       `json:"overridden"`
	LastFired  *time.Time `json:"last_fired"`
}

func (s *server) sources() ([]sourceView, error) {
	states, err := store.SourceStates(s.Store.DB)
	if err != nil {
		return nil, err
	}
	out := []sourceView{}
	for name, src := range s.Config().Sources {
		v := sourceView{Name: name, Type: src.Type}
		if src.Polled() {
			v.Poll = time.Duration(src.Poll).String()
		}
		if st, ok := states[name]; ok {
			v.LastPollAt, v.LastError = st.LastPollAt, st.LastError
		}
		if src.Type == "schedule" {
			s.fillSchedule(&v, src)
		}
		switch {
		case v.LastError != "":
			v.Health = "error"
		case v.LastPollAt == nil:
			v.Health = "idle"
		case src.Polled() && s.Now().Sub(*v.LastPollAt) > 2*time.Duration(src.Poll)+time.Minute:
			v.Health = "stale"
		default:
			v.Health = "ok"
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func actionSummary(a config.Action) string {
	switch {
	case len(a.Cmd) > 0:
		return "cmd: " + strings.Join(a.Cmd, " ")
	case a.Unit != "":
		return "unit: " + a.Unit
	case a.Agent != "":
		return "agent: " + a.Agent
	case a.Routine != "":
		return "routine: " + a.Routine
	}
	return ""
}

func (s *server) rules() ([]ruleView, error) {
	off, err := store.RuleOverrides(s.Store.DB)
	if err != nil {
		return nil, err
	}
	fired, err := store.RuleLastFiredAll(s.Store.DB)
	if err != nil {
		return nil, err
	}
	out := []ruleView{}
	for _, r := range s.Config().Rules {
		on := r.On
		if on == "" {
			on = "edge"
		}
		v := ruleView{Name: r.Name, Source: r.Source, When: r.When, On: on, Action: actionSummary(r.Action), Enabled: !off[r.Name], Overridden: off[r.Name]}
		if src := s.Config().Sources[r.Source]; src != nil {
			v.SourceType = src.Type
		} else if r.Source == "agent-result" {
			v.SourceType = "agent results"
		}
		if r.Cooldown > 0 {
			v.Cooldown = time.Duration(r.Cooldown).String()
		}
		v.Kind, v.Target, _ = strings.Cut(v.Action, ": ")
		if t, ok := fired[r.Name]; ok {
			v.LastFired = &t
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *server) hasRule(name string) bool {
	for _, r := range s.Config().Rules {
		if r.Name == name {
			return true
		}
	}
	return false
}

func (s *server) job(id int64) (store.JobDetail, bool, error) {
	j, err := store.GetJob(s.Store.DB, id)
	if err == sql.ErrNoRows {
		return j, false, nil
	}
	return j, err == nil, err
}

// Page titles and which nav entry each page lights up.
var (
	titles = map[string]string{"dashboard": "Dashboard", "jobs": "Jobs", "job": "Job", "approvals": "Approvals",
		"rules": "Rules", "sources": "Sources", "audit": "Audit", "logins": "Connections", "egress": "Egress", "services": "Services", "servicedone": "Services", "serviceconnect": "Services",
		"cfglist": "Config", "cfgedit": "Edit", "history": "History", "historyitem": "Revision",
		"help": "Help & Docs", "helppage": "Help & Docs", "helptemplates": "Templates"}
	active = map[string]string{"dashboard": "dash", "jobs": "jobs", "job": "jobs", "approvals": "approvals",
		"rules": "rules", "sources": "sources", "audit": "audit", "logins": "logins", "egress": "egress", "services": "services", "servicedone": "services", "serviceconnect": "services",
		"cfglist": "rules", "cfgedit": "rules", "history": "history", "historyitem": "history",
		"help": "help", "helppage": "help", "helptemplates": "help"}
	// Config kinds as people read them, and the nav entry each lights up.
	kindTitle = map[string]string{"rules": "Rules", "sources": "Sources", "agents": "Agents", "routines": "Routines", "credentials": "Connections", "notify": "Notifications"}
	kindOne   = map[string]string{"rules": "rule", "sources": "source", "agents": "agent", "routines": "routine", "credentials": "connection", "notify": "notification channel"}
	kindNav   = map[string]string{"rules": "rules", "sources": "sources", "agents": "agents", "routines": "routines", "credentials": "logins", "notify": "notify"}
	provLabel = map[string]string{"file": "from siphon.yaml", "portal": "added in the portal", "override": "overrides siphon.yaml", "deleted": "deleted in the portal"}
	// Pages that refresh themselves every 5 s (job detail decides by state).
	polls = map[string]bool{"dashboard": true, "jobs": true, "approvals": true, "sources": true, "audit": true}
)

type dashView struct {
	store.Dashboard
	Greeting                                    string
	Total, Running, Failed, AgentCap, SourcesOK int
	SourcesErr                                  int
	SuccessRate, AgentBar                       string
}

func (s *server) dashboard(srcs []sourceView) (*dashView, error) {
	now := s.Now()
	d, err := store.GetDashboard(s.Store.DB, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	v := &dashView{Dashboard: d, AgentCap: s.Config().Limits.AgentRunsPerDay, Running: d.Counts["running"], Failed: d.Counts["failed"]}
	for _, n := range d.Counts {
		v.Total += n
	}
	switch h := now.Local().Hour(); {
	case h < 5 || h >= 18:
		v.Greeting = "Good evening"
	case h < 12:
		v.Greeting = "Good morning"
	default:
		v.Greeting = "Good afternoon"
	}
	v.SuccessRate = "—"
	if fin := d.Counts["done"] + d.Counts["failed"]; fin > 0 {
		v.SuccessRate = strconv.FormatFloat(100*float64(d.Counts["done"])/float64(fin), 'f', 1, 64) + "%"
	}
	pct := 0
	if v.AgentCap > 0 {
		pct = min(100, 100*d.AgentRuns/v.AgentCap)
	}
	if pct > 0 && pct < 5 {
		pct = 5 // a few runs still show on the bar
	}
	v.AgentBar = "w" + strconv.Itoa(pct/5*5) // width classes in steps of 5 (no inline styles: CSP)
	for _, src := range srcs {
		switch src.Health {
		case "ok":
			v.SourcesOK++
		case "error":
			v.SourcesErr++
		}
	}
	return v, nil
}

// ago renders a past time as "38 s ago", "6 min ago", "3 h ago", or a date.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return t.Local().Format("2 Jan 15:04")
}
