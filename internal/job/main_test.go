package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Each test pipeline starts its own egress proxy; use ephemeral ports so
// they never collide on the configured 127.77.0.1:3128.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "mcp-bridge" { // a bridge that dies at once (see TestAgentBridgeFailureIsReported)
		if b, err := os.ReadFile(os.Args[2]); err == nil { // show the bridge's env, as a real bridge's stderr might
			var spec struct {
				SecretsFile string `json:"secrets_file"`
			}
			json.Unmarshal(b, &spec)
			sec, _ := os.ReadFile(spec.SecretsFile)
			os.WriteFile(bridgeEnvDump(os.Getppid()), sec, 0o600) // the test reads it back
		}
		os.Exit(3)
	}
	egressListenOverride = "127.0.0.1:0"
	os.Exit(m.Run())
}

// bridgeEnvDump is where the dying test bridge leaves its env for pid's tests.
func bridgeEnvDump(pid int) string {
	return filepath.Join(os.TempDir(), "siphon-job-test-bridge-"+strconv.Itoa(pid))
}
