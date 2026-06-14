package operate

import "testing"

func TestToolGovernanceClassification(t *testing.T) {
	r := NewToolRegistry()
	cases := map[string]Governance{
		"list_workers":     GovReadOnly,
		"read_worker_file": GovReadOnly,
		"audit_worker":     GovReadOnly,
		"review_worker":    GovReadOnly,
		"diff_worker":      GovReadOnly,
		"scaffold_worker":  GovSideEffecting,
		"patch_worker":     GovSideEffecting,
		"fix_worker":       GovSideEffecting,
		"build_worker":     GovSideEffecting,
		"transfer_worker":  GovRequiresApproval,
	}
	for name, want := range cases {
		tool, ok := r.Tool(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if tool.Governance != want {
			t.Errorf("tool %q governance = %v, want %v", name, tool.Governance, want)
		}
	}
}

func TestGovernanceNeedsApproval(t *testing.T) {
	if GovReadOnly.NeedsApproval(PostureManual) {
		t.Error("read-only must never need approval")
	}
	if !GovSideEffecting.NeedsApproval(PostureManual) {
		t.Error("side-effecting needs approval under manual posture")
	}
	if GovSideEffecting.NeedsApproval(PostureAutoSafe) {
		t.Error("side-effecting auto-passes under auto-safe posture")
	}
	if !GovRequiresApproval.NeedsApproval(PostureAutoSafe) {
		t.Error("requires-approval must always need approval, even auto-safe")
	}
}
