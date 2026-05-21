package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	runtimeplan "ouvrier/internal/runtime"
)

type compositionRun struct {
	pipeline runtimeplan.Pipeline
	input    string
}

type compositionOutcome struct {
	result planRunResult
	err    error
}

func (rt httpRuntime) runParallelStepResult(ctx context.Context, step runtimeplan.Step, input string, scope planRunScope) (planRunResult, error) {
	runs := make([]compositionRun, 0, len(step.Branches))
	for _, branch := range step.Branches {
		runs = append(runs, compositionRun{pipeline: branch, input: input})
	}
	outcomes := rt.runComposition(ctx, runs, defaultCompositionConcurrency(len(runs)), step.PartialOK, scope)
	output, err := encodeCompositionOutput(outcomes, step.PartialOK)
	result := compositionResult(outcomes, output)
	if err != nil {
		return result, err
	}
	if !step.PartialOK {
		if err := firstCompositionError(outcomes); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (rt httpRuntime) runMapStepResult(ctx context.Context, step runtimeplan.Step, input string, scope planRunScope) (planRunResult, error) {
	items, err := mapInputItems(input)
	if err != nil {
		return planRunResult{Output: input}, err
	}
	runs := make([]compositionRun, 0, len(items))
	for _, item := range items {
		runs = append(runs, compositionRun{pipeline: step.MapPipeline, input: string(item)})
	}
	outcomes := rt.runComposition(ctx, runs, step.Concurrency, step.PartialOK, scope)
	output, err := encodeCompositionOutput(outcomes, step.PartialOK)
	result := compositionResult(outcomes, output)
	if err != nil {
		return result, err
	}
	if !step.PartialOK {
		if err := firstCompositionError(outcomes); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (rt httpRuntime) runComposition(ctx context.Context, runs []compositionRun, concurrency int, partialOK bool, scope planRunScope) []compositionOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(runs) && len(runs) > 0 {
		concurrency = len(runs)
	}
	runCtx := ctx
	cancel := func() {}
	if !partialOK {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	outcomes := make([]compositionOutcome, len(runs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	wg.Add(len(runs))
	for i, run := range runs {
		i, run := i, run
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				outcomes[i].err = runCtx.Err()
				return
			}
			result, err := rt.runStepsResult(runCtx, run.pipeline.Steps, run.input, scope)
			outcomes[i] = compositionOutcome{result: result, err: err}
			if err != nil && !partialOK {
				cancel()
			}
		}()
	}
	wg.Wait()
	return outcomes
}

func defaultCompositionConcurrency(items int) int {
	if items <= 0 {
		return 1
	}
	concurrency := runtime.NumCPU()
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > items {
		return items
	}
	return concurrency
}

func mapInputItems(input string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		return nil, fmt.Errorf("Map input must be a JSON array: %w", err)
	}
	if items == nil {
		return nil, errors.New("Map input must be a JSON array")
	}
	return items, nil
}

func encodeCompositionOutput(outcomes []compositionOutcome, partialOK bool) (string, error) {
	items := make([]any, len(outcomes))
	for i, outcome := range outcomes {
		if partialOK {
			items[i] = partialOutcomeValue(outcome)
			continue
		}
		items[i] = outputJSONValue(outcome.result.Output)
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func partialOutcomeValue(outcome compositionOutcome) map[string]any {
	if outcome.err != nil {
		return map[string]any{
			"ok":    false,
			"error": outcome.err.Error(),
		}
	}
	return map[string]any{
		"ok":     true,
		"output": outputJSONValue(outcome.result.Output),
	}
}

func outputJSONValue(output string) any {
	if json.Valid([]byte(output)) {
		var decoded any
		if err := json.Unmarshal([]byte(output), &decoded); err == nil {
			return decoded
		}
	}
	return output
}

func firstCompositionError(outcomes []compositionOutcome) error {
	for _, outcome := range outcomes {
		if outcome.err != nil {
			return outcome.err
		}
	}
	return nil
}

func compositionResult(outcomes []compositionOutcome, output string) planRunResult {
	result := planRunResult{Output: output}
	for _, outcome := range outcomes {
		if outcome.result.HasSession {
			result.Session = outcome.result.Session
			result.HasSession = true
		}
	}
	return result
}
