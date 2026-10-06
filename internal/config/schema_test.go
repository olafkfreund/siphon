package config

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"
	"time"
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

func TestSchemaDurationPattern(t *testing.T) {
	s, err := schemaFor(reflect.TypeFor[Duration]())
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := regexp.Compile(s["pattern"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"0", "+0", "-0", "1ns", "1us", "1µs", "1μs", "1ms", "1s", "1m", "1h",
		"1.5h", ".5h", "1.h", "2h45m", "-1.5h", "+2h45m",
		"", "+", ".", ".h", "1", "1.5", "1e3s", "1h-2m", "1d", " 1s", "1s ",
	} {
		_, parseErr := time.ParseDuration(value)
		if got, want := pattern.MatchString(value), parseErr == nil; got != want {
			t.Errorf("duration %q: schema matches %t, ParseDuration accepts %t", value, got, want)
		}
	}
}
