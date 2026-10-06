package action

import "time"

// SandboxOptions selects how an action runs. Mode "systemd" (default) runs it
// in the agentgw-action@ template unit (template.go); "none" runs it directly.
type SandboxOptions struct {
	Mode      string
	Timeout   time.Duration
	Dir       string            // systemd: per-run directories live here (<state>/actions)
	Env       map[string]string // systemd: extra child env (secrets stay off argv)
	Files     map[string][]byte // systemd: private files, at FilePath(name) inside the unit
	Writeback []string
}
