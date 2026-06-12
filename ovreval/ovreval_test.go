package ovreval_test

import (
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
	"github.com/ArnaudGuiovanna/ouvrier/ovreval"
	"github.com/ArnaudGuiovanna/ouvrier/ovrtest"
)

type triage struct {
	Priority string `json:"priority"`
	Summary  string `json:"summary"`
	Score    int    `json:"score"`
}

func newSuite(t *testing.T) *ovreval.Suite {
	t.Helper()
	provider := ovrtest.NewProvider(
		ovrtest.Text(`{"priority":"high","summary":"cannot log in","score":9}`),
		ovrtest.Text(`{"priority":"low","summary":"typo report","score":1}`),
	)
	return ovreval.New(
		ovr.NewRunner(ovr.WithProvider(provider)),
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Output[triage](),
		),
		ovr.Reply(ovr.JSON[triage]()),
	)
}

func TestSuitePassingCases(t *testing.T) {
	report := newSuite(t).RunT(t,
		ovreval.Case{
			Name: "high priority",
			Path: "/tickets/1",
			Body: `{"subject":"down"}`,
			Assert: []ovreval.Assertion{
				ovreval.WantStatus(200),
				ovreval.OutputField("priority", "high"),
				ovreval.OutputField("score", 9),
				ovreval.OutputContains("cannot log in"),
			},
		},
		ovreval.Case{
			Name: "low priority",
			Path: "/tickets/2",
			Body: `{"subject":"typo"}`,
			Assert: []ovreval.Assertion{
				ovreval.WantStatus(200),
				ovreval.OutputField("priority", "low"),
			},
		},
	)
	if report.PassRate() != 1.0 {
		t.Fatalf("PassRate = %v, want 1.0 (%d/%d)", report.PassRate(), report.Passed(), len(report.Results))
	}
}

func TestSuiteDetectsRegression(t *testing.T) {
	// Run the suite directly (not via RunT) so a failing assertion is data, not
	// a test failure, and we can assert the harness reports it.
	report, err := newSuite(t).Run(
		ovreval.Case{
			Name: "wrong expectation",
			Path: "/tickets/1",
			Body: `{}`,
			Assert: []ovreval.Assertion{
				ovreval.OutputField("priority", "low"), // model returns "high"
			},
		},
	)
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if report.Failed() != 1 {
		t.Fatalf("Failed = %d, want 1", report.Failed())
	}
	if report.PassRate() != 0.0 {
		t.Fatalf("PassRate = %v, want 0.0", report.PassRate())
	}
	if len(report.Results[0].Errors) == 0 {
		t.Fatal("expected a recorded assertion error for the failing case")
	}
}
