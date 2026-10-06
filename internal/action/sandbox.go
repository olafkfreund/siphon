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
}

func SandboxArgv(argv []string, timeout time.Duration, creds map[string]string) []string {
	seconds := int64((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	result := []string{"systemd-run", "--wait", "--pipe", "--collect", "--quiet",
		"--property=DynamicUser=yes", "--property=ProtectSystem=strict", "--property=ProtectHome=yes",
		"--property=PrivateTmp=yes", "--property=NoNewPrivileges=yes",
		"--property=IPAddressDeny=169.254.0.0/16", fmt.Sprintf("--property=RuntimeMaxSec=%ds", seconds)}
	keys := make([]string, 0, len(creds))
	for name := range creds {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		result = append(result, "--property=LoadCredential="+name+":"+creds[name])
	}
	return append(append(result, "--"), argv...)
}
