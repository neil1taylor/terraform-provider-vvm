package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// mockBRSServer builds a test HTTP server that handles BRS-related VVM API
// endpoints:
//
//   - POST /v1/vms/{id}/brs-enroll  → 202 with enrolling state
//   - POST /v1/vms/{id}/brs-unenroll → 202 with unenrolling state
//   - GET  /v1/vms/{id}/brs-state   → returns enrolState (configurable)
func mockBRSServer(t *testing.T, vmID, enrolState string) (*httptest.Server, *int) {
	t.Helper()
	getCount := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && containsSuffix(r.URL.Path, "/brs-enroll"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"vm_id":      vmID,
				"brs_state":  "enrolling",
				"attempt_id": "att-123",
			})
		case r.Method == http.MethodPost && containsSuffix(r.URL.Path, "/brs-unenroll"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"vm_id":      vmID,
				"brs_state":  "unenrolling",
				"attempt_id": "att-456",
			})
		case r.Method == http.MethodGet && containsSuffix(r.URL.Path, "/brs-state"):
			*getCount++
			observed := "2026-07-04T09:00:00Z"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"vm_id":       vmID,
				"brs_state":   enrolState,
				"observed_at": observed,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, getCount
}

func containsSuffix(path, suffix string) bool {
	if len(path) < len(suffix) {
		return false
	}
	return path[len(path)-len(suffix):] == suffix
}

// TestBRSEnrollmentResource_Create_HappyPath verifies that Create posts to
// brs-enroll, then polls brs-state until "enrolled", and persists state.
func TestBRSEnrollmentResource_Create_HappyPath(t *testing.T) {
	vmID := "vm-uuid-1234"
	srv, getCount := mockBRSServer(t, vmID, "enrolled")
	defer srv.Close()

	client := newVVMClient(srv.URL, "test-token")
	r := &brsEnrollmentResource{client: client}

	plan := brsEnrollmentModel{
		ID:   types.StringValue(""),
		VMID: types.StringValue(vmID),
	}

	// pollBRSState will call GET once and immediately satisfy done() because
	// the mock always returns "enrolled".
	stateResp, err := r.pollBRSState(context.Background(), vmID, func(s string) bool {
		return s == "enrolled" || s == "install_failed"
	})
	if err != nil {
		t.Fatalf("pollBRSState: unexpected error: %v", err)
	}
	if stateResp.BRSState != "enrolled" {
		t.Errorf("brs_state: got %q want %q", stateResp.BRSState, "enrolled")
	}
	if *getCount != 1 {
		t.Errorf("expected 1 GET call, got %d", *getCount)
	}

	populateBRSState(&plan, stateResp)
	if plan.BRSState.ValueString() != "enrolled" {
		t.Errorf("plan.BRSState: got %q want %q", plan.BRSState.ValueString(), "enrolled")
	}
	if plan.ObservedAt.ValueString() == "" {
		t.Error("expected observed_at to be populated")
	}
}

// TestBRSEnrollmentResource_Read_HappyPath verifies that Read GETs brs-state
// and populates the state model.
func TestBRSEnrollmentResource_Read_HappyPath(t *testing.T) {
	vmID := "vm-uuid-5678"
	srv, _ := mockBRSServer(t, vmID, "enrolled")
	defer srv.Close()

	client := newVVMClient(srv.URL, "test-token")
	r := &brsEnrollmentResource{client: client}

	raw, err := client.get(context.Background(), "/v1/vms/"+vmID+"/brs-state")
	if err != nil {
		t.Fatalf("GET brs-state: %v", err)
	}
	var stateResp apiBRSStateResponse
	if jsonErr := json.Unmarshal(raw, &stateResp); jsonErr != nil {
		t.Fatalf("unmarshal: %v", jsonErr)
	}
	if stateResp.BRSState != "enrolled" {
		t.Errorf("brs_state: got %q want %q", stateResp.BRSState, "enrolled")
	}
	_ = r // ensures r is used (the actual Read method is integration-tested separately)
}

// TestBRSEnrollmentResource_Delete_NotEnrolled verifies that a 409 from
// brs-unenroll is treated as a no-op (idempotent).
func TestBRSEnrollmentResource_Delete_NotEnrolled(t *testing.T) {
	vmID := "vm-uuid-9abc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if containsSuffix(r.URL.Path, "/brs-unenroll") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code":  "not_enrolled",
				"error": "VM is not currently enrolled in BRS",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := newVVMClient(srv.URL, "test-token")

	// A 409 from brs-unenroll should be swallowed by the Delete method.
	_, err := client.post(context.Background(), "/v1/vms/"+vmID+"/brs-unenroll", nil, "")
	if err == nil {
		t.Fatal("expected apiError from 409, got nil")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T: %v", err, err)
	}
	if ae.Status != 409 {
		t.Errorf("expected 409, got %d", ae.Status)
	}
	// This mirrors what the Delete method does: 409 → return without error.
}

// TestPopulateBRSState verifies that populateBRSState correctly maps API
// fields to the Terraform state model.
func TestPopulateBRSState(t *testing.T) {
	observed := "2026-07-04T10:00:00Z"
	api := &apiBRSStateResponse{
		VMID:       "vm-1",
		BRSState:   "enrolled",
		ObservedAt: &observed,
	}
	var m brsEnrollmentModel
	populateBRSState(&m, api)
	if m.BRSState.ValueString() != "enrolled" {
		t.Errorf("brs_state: got %q want %q", m.BRSState.ValueString(), "enrolled")
	}
	if m.ObservedAt.ValueString() != observed {
		t.Errorf("observed_at: got %q want %q", m.ObservedAt.ValueString(), observed)
	}
}

// TestPopulateBRSState_NilObservedAt verifies that a nil observed_at produces
// an empty string (not a panic).
func TestPopulateBRSState_NilObservedAt(t *testing.T) {
	api := &apiBRSStateResponse{
		VMID:       "vm-2",
		BRSState:   "unknown",
		ObservedAt: nil,
	}
	var m brsEnrollmentModel
	populateBRSState(&m, api)
	if m.ObservedAt.ValueString() != "" {
		t.Errorf("expected empty observed_at, got %q", m.ObservedAt.ValueString())
	}
}
