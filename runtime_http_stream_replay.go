package ovr

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type adminStreamReplayRequest struct {
	URI string `json:"uri"`
}

type adminStreamReplayResponse struct {
	Status   string `json:"status"`
	URI      string `json:"uri,omitempty"`
	DLQ      string `json:"dlq,omitempty"`
	Replayed int    `json:"replayed"`
}

// serveAdminStreamReplay drains the dead-letter queue for a configured stream
// plan and reprocesses each message through the pipeline, returning the number
// of messages replayed. It sits behind the same admin auth as every other
// /admin/* route. The optional request body selects which stream plan to replay
// by source URI; when omitted the single stream plan with a DLQ target is used.
func (rt httpRuntime) serveAdminStreamReplay(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}

	var request adminStreamReplayRequest
	if req.Body != nil {
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			writeJSONStatus(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}

	plan, ok := rt.adminStreamReplayPlan(strings.TrimSpace(request.URI))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	replayed, err := rt.ReplayStreamDLQ(req.Context(), plan)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "replay_failed")
		return
	}

	response := adminStreamReplayResponse{
		Status:   "ok",
		URI:      streamDisplayURI(plan.Trigger.URI),
		Replayed: replayed,
	}
	if target := strings.TrimSpace(plan.Trigger.DLQTarget); target != "" {
		response.DLQ = streamDisplayURI(target)
	}
	writeJSON(w, http.StatusOK, response)
}

// adminStreamReplayPlan selects the stream plan to replay. With a non-empty uri
// it matches the source stream URI exactly; otherwise it returns the single
// stream plan that has a DLQ target configured.
func (rt httpRuntime) adminStreamReplayPlan(uri string) (runtimeplan.Plan, bool) {
	var match runtimeplan.Plan
	matches := 0
	for _, route := range rt.adminPlans {
		if route.plan.Trigger.Kind != runtimeplan.TriggerStream {
			continue
		}
		if strings.TrimSpace(route.plan.Trigger.DLQTarget) == "" {
			continue
		}
		if uri != "" && route.plan.Trigger.URI != uri {
			continue
		}
		match = route.plan
		matches++
	}
	return match, matches == 1
}
