package config

import "testing"

// #135 A6: an unknown agent needs approval, like StepNeedsApproval.
func TestNeedsApprovalUnknownAgent(t *testing.T) {
	if !(&Config{}).NeedsApproval(Rule{Action: Action{Agent: "x"}}) {
		t.Fatal("unknown agent should need approval")
	}
}
