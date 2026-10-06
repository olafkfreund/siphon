package action

import (
	"fmt"
	"sort"
	"time"
)

type SandboxOptions struct {
	Mode        string
	Timeout     time.Duration
	Credentials map[string]string
	Unit        string
}

func SandboxArgv(argv []string, o SandboxOptions) []string {
	seconds := int64((o.Timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	result := []string{"systemd-run", "--wait", "--pipe", "--collect", "--quiet", "--setenv=HOME=/tmp",
		"--property=DynamicUser=yes", "--property=ProtectSystem=strict", "--property=ProtectHome=yes",
		"--property=PrivateTmp=yes", "--property=NoNewPrivileges=yes",
		"--property=IPAddressDeny=169.254.0.0/16", fmt.Sprintf("--property=RuntimeMaxSec=%ds", seconds)}
	if o.Unit != "" {
		result = append(result, "--unit="+o.Unit)
	}
	keys := make([]string, 0, len(o.Credentials))
	for name := range o.Credentials {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		result = append(result, "--property=LoadCredential="+name+":"+o.Credentials[name])
	}
	return append(append(result, "--"), argv...)
}
