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
