package ovr

import (
	"context"
	"net/http"
	"strings"
	"time"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

// /admin/runs is the operator surface over the durable-run journal (#40):
// GET lists journal rows (status=orphaned filters to interrupted runs no live
// lease protects), POST /admin/runs/{execID}/recover forces the replay of a
// run the automatic loop refused (the EventReplayIndeterminateTool path).

// adminRunResponse reports one durable-run journal row plus its recovery
// posture. A run is orphaned when its journal exists, its execution has not
// completed, and no unexpired lease (a heartbeating run or an in-flight
// recovery) protects it.
type adminRunResponse struct {
	ExecID          string `json:"exec_id"`
	PlanKey         string `json:"plan_key"`
	TriggerKind     string `json:"trigger_kind,omitempty"`
	CreatedAt       string `json:"created_at"`
	ExecutionStatus string `json:"execution_status,omitempty"`
	Orphaned        bool   `json:"orphaned"`
	LeaseHolder     string `json:"lease_holder,omitempty"`
	LeaseExpiresAt  string `json:"lease_expires_at,omitempty"`
	Checkpoints     int    `json:"checkpoints"`
	OpenIntents     int    `json:"open_intents"`
}

type adminRunsResponse struct {
	Status string             `json:"status"`
	Runs   []adminRunResponse `json:"runs"`
}

func (rt httpRuntime) serveAdminRuns(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	statusFilter := strings.TrimSpace(req.URL.Query().Get("status"))
	if statusFilter != "" && statusFilter != "orphaned" {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_status")
		return
	}
	if rt.stateStore == nil {
		writeJSON(w, http.StatusOK, adminRunsResponse{Status: "ok", Runs: []adminRunResponse{}})
		return
	}
	journals, err := rt.stateStore.RunJournals(req.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	liveLeases, err := rt.durableRunLeases(req.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}

	response := adminRunsResponse{Status: "ok", Runs: []adminRunResponse{}}
	for _, journal := range journals {
		row := adminRunResponse{
			ExecID:      journal.ExecID,
			PlanKey:     journal.PlanKey,
			TriggerKind: journal.TriggerKind,
			CreatedAt:   journal.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		execution, ok, err := rt.stateStore.Execution(req.Context(), journal.ExecID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		if ok {
			row.ExecutionStatus = string(execution.Status)
		}
		if lease, held := liveLeases[durableRunLeaseName(journal.ExecID)]; held {
			row.LeaseHolder = lease.Holder
			row.LeaseExpiresAt = lease.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		row.Orphaned = row.LeaseHolder == "" && (!ok || execution.Status != state.ExecutionCompleted)
		if statusFilter == "orphaned" && !row.Orphaned {
			continue
		}
		checkpoints, err := rt.stateStore.RunCheckpoints(req.Context(), journal.ExecID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		row.Checkpoints = len(checkpoints)
		intents, err := rt.stateStore.ToolIntents(req.Context(), journal.ExecID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		for _, intent := range intents {
			if intent.CompletedAt.IsZero() {
				row.OpenIntents++
			}
		}
		response.Runs = append(response.Runs, row)
	}
	writeJSON(w, http.StatusOK, response)
}

// durableRunLeases returns the unexpired run:* leases keyed by lease name; an
// empty map when the store has no lease capability.
func (rt httpRuntime) durableRunLeases(ctx context.Context) (map[string]state.Lease, error) {
	live := map[string]state.Lease{}
	leaseStore, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		return live, nil
	}
	leases, err := leaseStore.Leases(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, lease := range leases {
		if strings.HasPrefix(lease.Name, "run:") && lease.ExpiresAt.After(now) {
			live[lease.Name] = lease
		}
	}
	return live, nil
}

func (rt httpRuntime) serveAdminRunRecover(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	if rt.stateStore == nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_missing")
		return
	}
	if rt.durableRuns == nil {
		writeJSONStatus(w, http.StatusConflict, "durable_runs_disabled")
		return
	}
	leases, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		writeJSONStatus(w, http.StatusConflict, "lease_store_unavailable")
		return
	}
	execID := strings.TrimSpace(req.PathValue("execID"))
	if execID == "" {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	journal, ok, err := rt.stateStore.RunJournal(req.Context(), execID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	execution, ok, err := rt.stateStore.Execution(req.Context(), execID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	// The operator-forced replay still runs under a claimed run lease: a live
	// run (or an in-flight automatic recovery) holding the lease wins.
	lease, acquired, err := leases.AcquireLease(req.Context(), durableRunLeaseName(execID), cronReplicaID(), rt.durableRunLeaseTTLForRuntime())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	if !acquired {
		writeJSONStatus(w, http.StatusConflict, "run_active")
		return
	}

	plans := rt.adminRecoverablePlans()
	if !rt.startAsync(func(ctx context.Context) {
		rt.recoverClaimedDurableRun(ctx, leases, plans, journal, execution, lease, true)
	}) {
		releaseCtx, cancel := context.WithTimeout(context.Background(), cronLeaseReleaseTimeout)
		defer cancel()
		_ = leases.ReleaseLease(releaseCtx, durableRunLeaseName(execID), cronReplicaID(), lease.Fence)
		writeJSONStatus(w, http.StatusServiceUnavailable, "shutting_down")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "recovering",
		"exec_id": execID,
	})
}

// adminRecoverablePlans gathers every compiled plan this runtime serves;
// adminPlans carries the full set on every handler construction path, with
// adminRoutes as the fallback for HTTP-only slices.
func (rt httpRuntime) adminRecoverablePlans() []runtimeplan.Plan {
	if len(rt.adminPlans) > 0 {
		plans := make([]runtimeplan.Plan, 0, len(rt.adminPlans))
		for _, route := range rt.adminPlans {
			plans = append(plans, route.plan)
		}
		return plans
	}
	plans := make([]runtimeplan.Plan, 0, len(rt.adminRoutes))
	for _, route := range rt.adminRoutes {
		plans = append(plans, route.plan)
	}
	return plans
}
