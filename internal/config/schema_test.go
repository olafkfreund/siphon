package config

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestSchemaUpToDate(t *testing.T) {
	got, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	const path = "../../schema/agentgw.schema.json"
	if os.Getenv("UPDATE_SCHEMA") == "1" {
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("schema is stale; run: go run ./cmd/agentgw schema > schema/agentgw.schema.json")
	}
}

func TestSchemaRuleStructure(t *testing.T) {
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	rule := s["properties"].(map[string]any)["rules"].(map[string]any)["items"].(map[string]any)
	props := rule["properties"].(map[string]any)
	if _, ok := props["when"]; !ok {
		t.Fatal("Rule.when missing")
	}
	if _, ok := props["on"]; !ok {
		t.Fatal("Rule.on missing")
	}
	if rule["additionalProperties"] != false {
		t.Fatal("Rule must reject unknown fields")
	}
}
