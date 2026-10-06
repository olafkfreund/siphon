package action

import "time"

// SandboxOptions selects how an action runs. Mode "systemd" (default) runs it
// in an agentgw-action template unit (template.go); "none" runs it directly.
type SandboxOptions struct {
	Mode      string
	Timeout   time.Duration
	Dir       string            // systemd: per-run directories live here (<state>/actions)
	Env       map[string]string // extra child env (secrets stay off argv)
	Egress    *EgressEnv
	Files     map[string][]byte // systemd: private files, at FilePath(name) inside the unit
	Writeback []string
	home      string
	stderr    *[]byte
}

type EgressEnv struct{ ProxyURL string }
