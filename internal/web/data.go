package web

import (
	"database/sql"
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
}

type ruleView struct {
	Name       string     `json:"name"`
	Source     string     `json:"source"`
	On         string     `json:"on"`
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
	for name, src := range s.Cfg.Sources {
		v := sourceView{Name: name, Type: src.Type}
		if src.Type != "webhook" {
			v.Poll = time.Duration(src.Poll).String()
		}
		if st, ok := states[name]; ok {
			v.LastPollAt, v.LastError = st.LastPollAt, st.LastError
		}
		switch {
		case v.LastError != "":
			v.Health = "error"
		case v.LastPollAt == nil:
			v.Health = "idle"
		case src.Type != "webhook" && s.Now().Sub(*v.LastPollAt) > 2*time.Duration(src.Poll)+time.Minute:
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
	for _, r := range s.Cfg.Rules {
		on := r.On
		if on == "" {
			on = "edge"
		}
		v := ruleView{Name: r.Name, Source: r.Source, On: on, Action: actionSummary(r.Action), Enabled: !off[r.Name], Overridden: off[r.Name]}
		if t, ok := fired[r.Name]; ok {
			v.LastFired = &t
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *server) hasRule(name string) bool {
	for _, r := range s.Cfg.Rules {
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
		"rules": "Rules", "sources": "Sources", "audit": "Audit"}
	active = map[string]string{"dashboard": "dash", "jobs": "jobs", "job": "jobs", "approvals": "approvals",
		"rules": "rules", "sources": "sources", "audit": "audit"}
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
	v := &dashView{Dashboard: d, AgentCap: s.Cfg.Limits.AgentRunsPerDay, Running: d.Counts["running"], Failed: d.Counts["failed"]}
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
