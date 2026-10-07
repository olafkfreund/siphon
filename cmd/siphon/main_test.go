package main

import (
	"testing"

	"github.com/olafkfreund/siphon/internal/config"
)

// Plan step 2 verify: validate fails on bad.yaml and succeeds on a good config.
func TestValidate(t *testing.T) {
	if err := validate([]string{"-config", "../../internal/config/testdata/bad.yaml"}); err == nil {
		t.Fatal("validate bad.yaml: want error")
	}
	t.Setenv("AGW_TOKEN", "t-0123456789abcdef0123456789abcdef")
	t.Setenv("AGW_FACTORY", "f")
	t.Setenv("AGW_GH", "g")
	if err := validate([]string{"-config", "../../internal/config/testdata/full.yaml"}); err != nil {
		t.Fatalf("validate full.yaml: %v", err)
	}
}

// Plan step 1: SIPHON_LISTEN overrides server.listen.
func TestListenEnvOverride(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Listen: ":8080"}}
	applyListenEnv(cfg)
	if cfg.Server.Listen != ":8080" {
		t.Fatal(cfg.Server.Listen)
	}
	t.Setenv("SIPHON_LISTEN", "0.0.0.0:9")
	applyListenEnv(cfg)
	if cfg.Server.Listen != "0.0.0.0:9" {
		t.Fatal(cfg.Server.Listen)
	}
}
