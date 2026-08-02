package operate

import (
	"reflect"
	"testing"
)

func TestFallbackPlannerKeepsQuestionsReadOnly(t *testing.T) {
	runtime := &AgentRuntime{}
	plan := runtime.planPrompt("What does this worker do?", &Workspace{Dir: t.TempDir()})
	if got, want := plannedToolNames(plan), []string{"read_ouvrier_api", "list_worker_files", "read_worker_file"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("question tools = %#v, want %#v", got, want)
	}
	for _, tool := range plan.Tools {
		if isMutationTool(tool.Name) {
			t.Fatalf("question planned mutation tool %q", tool.Name)
		}
	}
}

func TestFallbackPlannerValidatesEveryMutation(t *testing.T) {
	runtime := &AgentRuntime{}
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "edit",
			text: "Add a read-only ticket lookup tool",
			want: []string{"read_ouvrier_api", "patch_worker", "audit_worker", "build_worker"},
		},
		{
			name: "fix review findings",
			text: "Fix the review findings",
			want: []string{"fix_worker", "audit_worker", "build_worker"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := runtime.planPrompt(test.text, &Workspace{Dir: t.TempDir()})
			if got := plannedToolNames(plan); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tools = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFallbackCreateAlwaysEndsWithAuditAndBuild(t *testing.T) {
	runtime := &AgentRuntime{}
	plan := runtime.planPrompt("Create a worker ticket-worker that receives POST /tickets", nil)
	got := plannedToolNames(plan)
	if len(got) < 2 || !reflect.DeepEqual(got[len(got)-2:], []string{"audit_worker", "build_worker"}) {
		t.Fatalf("create tools = %#v, want audit/build completion evidence", got)
	}
}

func plannedToolNames(plan promptPlan) []string {
	names := make([]string, len(plan.Tools))
	for i, tool := range plan.Tools {
		names[i] = tool.Name
	}
	return names
}
