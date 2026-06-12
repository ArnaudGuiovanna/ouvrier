package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPSuspendsGatedToolWhenApprovalStoreConfigured(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	call := provider.ToolCall{ID: "call_1", Name: "wire_payment"}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need approval", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"resumed"}`, StopReason: provider.StopEndTurn},
		},
	}
	called := make(chan struct{}, 1)
	store := state.NewMemoryStore()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /payments"),
		Pipe("settle",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("wire_payment", func(ctx context.Context) error {
				select {
				case called <- struct{}{}:
				default:
				}
				return nil
			}, RequiresApproval()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{"amount":100}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusAccepted)
	}
	select {
	case <-called:
		t.Fatal("gated tool body executed before approval")
	default:
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		ExecID     string `json:"exec_id"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "pending_approval" || body.ApprovalID == "" {
		t.Fatalf("body = %+v, want pending_approval with approval id", body)
	}

	pending, err := store.PendingApprovals(context.Background())
	if err != nil {
		t.Fatalf("PendingApprovals returned error: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(pending))
	}
	if pending[0].ToolName != "wire_payment" || pending[0].ExecID == "" {
		t.Fatalf("pending approval = %+v, want gated tool context", pending[0])
	}
	if pending[0].ID != body.ApprovalID {
		t.Fatalf("persisted approval id = %q, want %q", pending[0].ID, body.ApprovalID)
	}

	approvalReq := httptest.NewRequest(http.MethodPost, "/admin/approvals/"+body.ApprovalID, strings.NewReader(`{"decision":"approve","decided_by":"ops"}`))
	approvalRec := httptest.NewRecorder()
	handler.ServeHTTP(approvalRec, approvalReq)
	if approvalRec.Code != http.StatusOK {
		t.Fatalf("approval status = %d body=%s, want %d", approvalRec.Code, approvalRec.Body.String(), http.StatusOK)
	}
	var approvalBody struct {
		Status  string `json:"status"`
		Resumed bool   `json:"resumed"`
	}
	decodeAdminJSON(t, approvalRec, &approvalBody)
	if approvalBody.Status != "ok" || !approvalBody.Resumed {
		t.Fatalf("approval body = %+v, want resumed approval", approvalBody)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		approvals, _ := store.PendingApprovals(context.Background())
		events, _ := store.Events(context.Background(), body.ExecID)
		execution, ok, _ := store.Execution(context.Background(), body.ExecID)
		t.Fatalf("approved gated tool was not executed; execution ok=%v value=%+v approvals=%+v events=%+v", ok, execution, approvals, events)
	}
	deadline := time.Now().Add(time.Second)
	for {
		execution, ok, err := store.Execution(context.Background(), body.ExecID)
		if err != nil {
			t.Fatalf("Execution returned error: %v", err)
		}
		if ok && execution.Status == state.ExecutionCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution = %+v ok=%v, want completed", execution, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func seedApproval(t *testing.T, store state.Store, id, tool string) {
	t.Helper()
	if err := store.SaveApproval(context.Background(), state.PendingApproval{
		ID:       id,
		ExecID:   "exec-" + id,
		ToolName: tool,
		Status:   state.ApprovalPending,
	}); err != nil {
		t.Fatalf("SaveApproval returned error: %v", err)
	}
}

func TestHTTPAdminApprovalsRequiresAuth(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "")
	store := state.NewMemoryStore()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{stateStore: store, adminToken: "secret"})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/approvals", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad-token status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHTTPAdminApprovalsListsPending(t *testing.T) {
	store := state.NewMemoryStore()
	seedApproval(t, store, "a1", "wire_payment")
	seedApproval(t, store, "a2", "delete_user")
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/approvals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status    string `json:"status"`
		Approvals []struct {
			ID       string `json:"id"`
			ToolName string `json:"tool_name"`
			Status   string `json:"status"`
		} `json:"approvals"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Approvals) != 2 {
		t.Fatalf("body = %+v, want 2 pending approvals", body)
	}
	if body.Approvals[0].ID != "a1" || body.Approvals[1].ID != "a2" {
		t.Fatalf("approval order = %s,%s want a1,a2", body.Approvals[0].ID, body.Approvals[1].ID)
	}
}

func TestHTTPAdminApprovalApproveResolves(t *testing.T) {
	store := state.NewMemoryStore()
	seedApproval(t, store, "a1", "wire_payment")
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/a1", strings.NewReader(`{"decision":"approve","decided_by":"ops"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	got, ok, err := store.Approval(context.Background(), "a1")
	if err != nil || !ok {
		t.Fatalf("Approval ok=%v err=%v", ok, err)
	}
	if got.Status != state.ApprovalApproved || got.DecidedBy != "ops" {
		t.Fatalf("resolved approval = %+v, want approved by ops", got)
	}
}

func TestHTTPAdminApprovalDenyResolves(t *testing.T) {
	store := state.NewMemoryStore()
	seedApproval(t, store, "a1", "wire_payment")
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/a1", strings.NewReader(`{"decision":"deny"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	got, _, _ := store.Approval(context.Background(), "a1")
	if got.Status != state.ApprovalDenied {
		t.Fatalf("status = %q, want denied", got.Status)
	}
}

func TestHTTPAdminApprovalRejectsInvalidDecision(t *testing.T) {
	store := state.NewMemoryStore()
	seedApproval(t, store, "a1", "wire_payment")
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/a1", strings.NewReader(`{"decision":"maybe"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHTTPAdminApprovalMissingReturnsNotFound(t *testing.T) {
	store := state.NewMemoryStore()
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/absent", strings.NewReader(`{"decision":"approve"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHTTPAdminApprovalAlreadyDecidedConflicts(t *testing.T) {
	store := state.NewMemoryStore()
	seedApproval(t, store, "a1", "wire_payment")
	if _, err := store.ResolveApproval(context.Background(), "a1", state.ApprovalApproved, "ops"); err != nil {
		t.Fatalf("ResolveApproval returned error: %v", err)
	}
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/a1", strings.NewReader(`{"decision":"deny"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}
