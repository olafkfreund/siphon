package job

import (
	"os"
	"testing"
)

// Each test pipeline starts its own egress proxy; use ephemeral ports so
// they never collide on the configured 127.77.0.1:3128.
func TestMain(m *testing.M) {
	egressListenOverride = "127.0.0.1:0"
	os.Exit(m.Run())
}
