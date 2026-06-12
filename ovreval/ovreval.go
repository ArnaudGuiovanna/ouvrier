// Package ovreval is a lightweight evaluation and regression harness for
// Ouvrier workers. It runs a golden dataset of cases through a worker in
// process — using the same compiled pipeline that Run serves — and checks the
// typed output against assertions, producing a report you can gate CI on.
//
// Because it builds on ovr.Handler, an eval suite needs no network listener and
// no real model when paired with a scripted provider from the ovrtest package;
// point it at a live provider (ovr.WithProvider, or environment credentials via
// ovr.NewRunner defaults) to evaluate against a real model.
//
//	suite := ovreval.New(ovr.NewRunner(ovr.WithProvider(p)),
//		ovr.From("POST /tickets/{id}"),
//		ovr.Pipe("Triage.", ovr.Model("anthropic/claude-sonnet-4-6"), ovr.Output[Triage]()),
//		ovr.Reply(ovr.JSON[Triage]()),
//	)
//	report, err := suite.Run(
//		ovreval.Case{Name: "high priority", Path: "/tickets/1", Body: `{"subject":"down"}`,
//			Assert: []ovreval.Assertion{
//				ovreval.WantStatus(200),
//				ovreval.OutputField("priority", "high"),
//			}},
//	)
//	if report.PassRate() < 1.0 { /* fail CI */ }
package ovreval

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// Case is one evaluation input plus the assertions its result must satisfy.
type Case struct {
	Name   string
	Method string // defaults to POST when empty
	Path   string // request path, e.g. "/tickets/42"
	Body   string // raw request body
	Header map[string]string
	Assert []Assertion
}

// Result is what a Case produced: the HTTP status, the raw response body, and
// Output — the worker's typed result, unwrapped from the {status, output}
// reply envelope when present.
type Result struct {
	Status int
	Body   string
	Output string
}

// Assertion checks one property of a Result and returns a descriptive error on
// failure.
type Assertion func(Result) error

// WantStatus asserts the HTTP status code.
func WantStatus(code int) Assertion {
	return func(r Result) error {
		if r.Status != code {
			return fmt.Errorf("status = %d, want %d (body: %s)", r.Status, code, r.Body)
		}
		return nil
	}
}

// OutputContains asserts the output contains substr.
func OutputContains(substr string) Assertion {
	return func(r Result) error {
		if !strings.Contains(r.Output, substr) {
			return fmt.Errorf("output %q does not contain %q", r.Output, substr)
		}
		return nil
	}
}

// OutputField asserts that a top-level field of the JSON output equals want.
// Comparison is done on the JSON-normalized values, so numeric literals compare
// regardless of int/float spelling.
func OutputField(field string, want any) Assertion {
	return func(r Result) error {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.Output), &obj); err != nil {
			return fmt.Errorf("output is not a JSON object (%v): %s", err, r.Output)
		}
		raw, ok := obj[field]
		if !ok {
			return fmt.Errorf("output has no field %q: %s", field, r.Output)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			return fmt.Errorf("want value for %q is not JSON-encodable: %v", field, err)
		}
		gotNorm, err := normalizeJSON(raw)
		if err != nil {
			return fmt.Errorf("field %q value is invalid JSON: %v", field, err)
		}
		wantNorm, err := normalizeJSON(wantJSON)
		if err != nil {
			return fmt.Errorf("want value for %q normalizes badly: %v", field, err)
		}
		if gotNorm != wantNorm {
			return fmt.Errorf("field %q = %s, want %s", field, gotNorm, wantNorm)
		}
		return nil
	}
}

// Custom wraps an arbitrary check, for assertions the built-ins do not cover.
func Custom(fn func(Result) error) Assertion { return fn }

func normalizeJSON(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Suite binds a Runner configuration to a worker declaration so a dataset can be
// evaluated against it repeatedly.
type Suite struct {
	runner *ovr.Runner
	nodes  []ovr.Node
}

// New creates a Suite. Pass the Runner (typically built with ovr.WithProvider)
// and the same nodes you would pass to Run.
func New(runner *ovr.Runner, nodes ...ovr.Node) *Suite {
	if runner == nil {
		runner = ovr.NewRunner()
	}
	return &Suite{runner: runner, nodes: nodes}
}

// CaseResult is the outcome of evaluating one Case.
type CaseResult struct {
	Name   string
	Pass   bool
	Result Result
	Errors []error
	RunErr error // transport-level failure (handler build, request) rather than an assertion miss
}

// Report aggregates the outcomes of a Run.
type Report struct {
	Results []CaseResult
}

// Passed returns the number of cases that satisfied every assertion.
func (r Report) Passed() int {
	n := 0
	for _, c := range r.Results {
		if c.Pass {
			n++
		}
	}
	return n
}

// Failed returns the number of cases that did not pass.
func (r Report) Failed() int { return len(r.Results) - r.Passed() }

// PassRate returns the fraction of passing cases in [0,1]; an empty report
// rates 1.0 (vacuously passing).
func (r Report) PassRate() float64 {
	if len(r.Results) == 0 {
		return 1.0
	}
	return float64(r.Passed()) / float64(len(r.Results))
}

// Run evaluates every case and returns a Report. It returns an error only for a
// setup failure (the worker declaration does not compile); per-case transport
// and assertion failures are recorded in the Report.
func (s *Suite) Run(cases ...Case) (Report, error) {
	handler, err := s.runner.Handler(s.nodes...)
	if err != nil {
		return Report{}, fmt.Errorf("build worker handler: %w", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	report := Report{Results: make([]CaseResult, 0, len(cases))}
	for _, c := range cases {
		report.Results = append(report.Results, s.runCase(srv.URL, c))
	}
	return report, nil
}

// RunT runs the suite and reports each failing case via t.Errorf, so an eval
// dataset reads like a table test. It returns the Report for further assertions
// (e.g. require a minimum pass rate).
func (s *Suite) RunT(t *testing.T, cases ...Case) Report {
	t.Helper()
	report, err := s.Run(cases...)
	if err != nil {
		t.Fatalf("ovreval: %v", err)
		return report
	}
	for _, c := range report.Results {
		if c.Pass {
			continue
		}
		if c.RunErr != nil {
			t.Errorf("case %q: %v", c.Name, c.RunErr)
			continue
		}
		for _, e := range c.Errors {
			t.Errorf("case %q: %v", c.Name, e)
		}
	}
	return report
}

func (s *Suite) runCase(baseURL string, c Case) CaseResult {
	method := strings.TrimSpace(c.Method)
	if method == "" {
		method = http.MethodPost
	}
	result := CaseResult{Name: c.Name}

	req, err := http.NewRequest(method, baseURL+c.Path, strings.NewReader(c.Body))
	if err != nil {
		result.RunErr = err
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.Header {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.RunErr = err
		return result
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	res := Result{Status: resp.StatusCode, Body: string(body), Output: unwrapOutput(body)}
	result.Result = res

	pass := true
	for _, assert := range c.Assert {
		if assert == nil {
			continue
		}
		if err := assert(res); err != nil {
			result.Errors = append(result.Errors, err)
			pass = false
		}
	}
	result.Pass = pass
	return result
}

// unwrapOutput pulls the worker's typed result out of the {status, output}
// reply envelope. When the body is not that envelope (e.g. an SSE or raw
// terminal), it returns the body unchanged so assertions still have something
// to match.
func unwrapOutput(body []byte) string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err == nil {
		if raw, ok := probe["output"]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				return s
			}
			return string(raw)
		}
	}
	return string(body)
}
