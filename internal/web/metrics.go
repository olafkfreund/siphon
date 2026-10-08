package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/olafkfreund/siphon/internal/store"
)

// metricLabel escapes a label value for the Prometheus text format.
var metricLabel = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// metrics serves GET /metrics: gauges only, series for names in the running config,
// never a job id, error text, URL or event data.
func (s *server) metrics(w http.ResponseWriter, _ *http.Request) {
	cfg := s.Config()
	m, err := store.GetMetrics(s.Store.DB, s.Now())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	var b strings.Builder
	fam := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	line := func(name string, v any, kv ...string) {
		b.WriteString(name)
		for i := 0; i < len(kv); i += 2 {
			sep := ","
			if i == 0 {
				sep = "{"
			}
			fmt.Fprintf(&b, `%s%s="%s"`, sep, kv[i], metricLabel.Replace(kv[i+1]))
		}
		if len(kv) > 0 {
			b.WriteString("}")
		}
		fmt.Fprintf(&b, " %v\n", v)
	}
	keys := func(n int, at func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = at(i)
		}
		sort.Strings(out)
		return out
	}

	fam("siphon_build_info", "Build information; the value is always 1.")
	line("siphon_build_info", 1, "version", s.Version)

	fam("siphon_jobs", "Jobs by state.")
	states := make([]string, 0, len(m.Jobs))
	for st := range m.Jobs {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		line("siphon_jobs", m.Jobs[st], "state", st)
	}
	fam("siphon_jobs_finished_last_hour", "Jobs that finished in the last hour, by final state.")
	for _, st := range []string{"cancelled", "done", "failed"} {
		line("siphon_jobs_finished_last_hour", m.Finished[st], "state", st)
	}
	fam("siphon_queue_oldest_seconds", "Age of the oldest queued job, 0 if none.")
	line("siphon_queue_oldest_seconds", m.QueueOldest)
	fam("siphon_approvals_pending", "Jobs waiting for approval.")
	line("siphon_approvals_pending", m.Approvals)
	fam("siphon_approval_oldest_seconds", "Age of the oldest job waiting for approval, 0 if none.")
	line("siphon_approval_oldest_seconds", m.ApprovalOldest)
	fam("siphon_agent_runs_today", "Agent jobs created in the daily-cap window (the last 24h).")
	line("siphon_agent_runs_today", m.AgentRuns)
	fam("siphon_agent_runs_daily_limit", "The configured limits.agent_runs_per_day.")
	line("siphon_agent_runs_daily_limit", cfg.Limits.AgentRunsPerDay)

	var polled []string // sources that are polled, in the running config
	for name, src := range cfg.Sources {
		if src != nil && src.Polled() {
			polled = append(polled, name)
		}
	}
	sort.Strings(polled)
	fam("siphon_source_failing", "1 if the source's polls are failing.")
	for _, n := range polled {
		v := 0
		if m.SourceFailing[n] {
			v = 1
		}
		line("siphon_source_failing", v, "source", n)
	}
	fam("siphon_source_last_poll_timestamp_seconds", "Unix time of the source's last poll.")
	for _, n := range polled {
		if t, ok := m.SourcePoll[n]; ok {
			line("siphon_source_last_poll_timestamp_seconds", t, "source", n)
		}
	}
	fam("siphon_rule_error", "1 if the rule has a recorded evaluation error.")
	rules := keys(len(cfg.Rules), func(i int) string { return cfg.Rules[i].Name })
	for _, n := range rules {
		v := 0
		if m.RuleError[n] {
			v = 1
		}
		line("siphon_rule_error", v, "rule", n)
	}
	fam("siphon_notifications", "Notifications by channel and state.")
	var nk [][2]string
	for k := range m.Notifications {
		if cfg.Notify[k[0]] != nil {
			nk = append(nk, k)
		}
	}
	sort.Slice(nk, func(i, j int) bool {
		if nk[i][0] != nk[j][0] {
			return nk[i][0] < nk[j][0]
		}
		return nk[i][1] < nk[j][1]
	})
	for _, k := range nk {
		line("siphon_notifications", m.Notifications[k], "channel", k[0], "state", k[1])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}
