package action

import "time"

// SandboxOptions selects how an action runs. Mode "systemd" (default) runs it
// in an siphon-action template unit (template.go); "none" runs it directly.
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

// Socket, when set, is the proxy's unix socket: in systemd mode the unit gets
// no route to the proxy's TCP address, so exec-job forwards 127.0.0.1:3128 to it.
type EgressEnv struct{ ProxyURL, Socket string }
