package operate

import "testing"

func TestSourceSecretAssignmentDoesNotConsumeFollowingLine(t *testing.T) {
	placeholder := "ANTHROPIC_API_KEY=\nOUVRIER_ENV=dev\n"
	if sourceContainsCredential(placeholder, Redactor{}) {
		t.Fatal("empty .env.example placeholder was joined to the following configuration line")
	}
	if !sourceContainsCredential("ANTHROPIC_API_KEY=real-test-credential-value\n", Redactor{}) {
		t.Fatal("non-empty credential assignment was not detected")
	}
}
