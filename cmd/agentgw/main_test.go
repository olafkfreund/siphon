package main

import "testing"

// Plan step 2 verify: validate fails on bad.yaml and succeeds on a good config.
func TestValidate(t *testing.T) {
	if err := validate([]string{"-config", "../../internal/config/testdata/bad.yaml"}); err == nil {
		t.Fatal("validate bad.yaml: want error")
	}
	t.Setenv("AGW_TOKEN", "t")
	t.Setenv("AGW_FACTORY", "f")
	t.Setenv("AGW_GH", "g")
	if err := validate([]string{"-config", "../../internal/config/testdata/full.yaml"}); err != nil {
		t.Fatalf("validate full.yaml: %v", err)
	}
}
