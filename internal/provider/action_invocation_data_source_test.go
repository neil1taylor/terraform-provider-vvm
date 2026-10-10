package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestActionInvocationDataSource_Read_HappyPath verifies that fetchActionInvocation
// correctly maps a succeeded API response into the state model.
func TestActionInvocationDataSource_Read_HappyPath(t *testing.T) {
	finishedAt := "2026-07-04T10:05:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiActionInvocation{
			ID:                      "act-90c5c2",
			Status:                  "succeeded",
			StartedAt:               "2026-07-04T10:00:00Z",
			FinishedAt:              &finishedAt,
			Actor:                   "user@example.com",
			Summary:                 map[string]string{"probe_ok": "true"},
			AffectedVMIDs:           []string{"vm-uuid-1", "vm-uuid-2"},
			AffectedHostIDs:         []string{},
			ActivityTrackerEventIDs: []string{"at-event-1"},
		})
	}))
	t.Cleanup(srv.Close)

	d := &actionInvocationDataSource{client: newVVMClient(srv.URL, "test-token")}
	var m actionInvocationModel
	m.ID = types.StringValue("act-90c5c2")

	api, err := d.fetchActionInvocation(context.Background(), "act-90c5c2", &m)
	if err != nil {
		t.Fatalf("fetchActionInvocation: unexpected error: %v", err)
	}
	if api.Status != "succeeded" {
		t.Errorf("api.Status: got %q, want %q", api.Status, "succeeded")
	}

	// Identity
	if m.ID.ValueString() != "act-90c5c2" {
		t.Errorf("ID: got %q, want %q", m.ID.ValueString(), "act-90c5c2")
	}
	// Scalar computed fields
	if m.Status.ValueString() != "succeeded" {
		t.Errorf("Status: got %q, want %q", m.Status.ValueString(), "succeeded")
	}
	if m.StartedAt.ValueString() != "2026-07-04T10:00:00Z" {
		t.Errorf("StartedAt: got %q, want %q", m.StartedAt.ValueString(), "2026-07-04T10:00:00Z")
	}
	if m.FinishedAt.ValueString() != finishedAt {
		t.Errorf("FinishedAt: got %q, want %q", m.FinishedAt.ValueString(), finishedAt)
	}
	if m.Actor.ValueString() != "user@example.com" {
		t.Errorf("Actor: got %q, want %q", m.Actor.ValueString(), "user@example.com")
	}

	// Summary map
	if m.Summary.IsNull() || m.Summary.IsUnknown() {
		t.Fatal("Summary should not be null/unknown")
	}
	summaryElems := m.Summary.Elements()
	if len(summaryElems) != 1 {
		t.Errorf("Summary len: got %d, want 1", len(summaryElems))
	}
	if v, ok := summaryElems["probe_ok"]; !ok {
		t.Error("Summary missing key probe_ok")
	} else if v.String() != `"true"` {
		t.Errorf("Summary[probe_ok]: got %s, want \"true\"", v.String())
	}

	// AffectedVMIDs list
	vmElems := m.AffectedVMIDs.Elements()
	if len(vmElems) != 2 {
		t.Errorf("AffectedVMIDs len: got %d, want 2", len(vmElems))
	}

	// AffectedHostIDs list — empty
	if len(m.AffectedHostIDs.Elements()) != 0 {
		t.Errorf("AffectedHostIDs len: got %d, want 0", len(m.AffectedHostIDs.Elements()))
	}

	// ActivityTrackerEventIDs list
	atElems := m.ActivityTrackerEventIDs.Elements()
	if len(atElems) != 1 {
		t.Errorf("ActivityTrackerEventIDs len: got %d, want 1", len(atElems))
	}
}

// TestActionInvocationDataSource_Read_NotFound verifies that a 404 from the API
// is returned as an *apiError so the Read method can surface a diagnostic error.
func TestActionInvocationDataSource_Read_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_found","message":"action invocation not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	d := &actionInvocationDataSource{client: newVVMClient(srv.URL, "test-token")}
	var m actionInvocationModel
	m.ID = types.StringValue("act-missing")

	_, err := d.fetchActionInvocation(context.Background(), "act-missing", &m)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T: %v", err, err)
	}
	if ae.Status != 404 {
		t.Errorf("expected status 404, got %d", ae.Status)
	}
}

// TestActionInvocationDataSource_Read_Pending verifies that an in-progress
// invocation (no finished_at) maps FinishedAt to an empty string.
func TestActionInvocationDataSource_Read_Pending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// finished_at is absent — pending invocation
		_ = json.NewEncoder(w).Encode(apiActionInvocation{
			ID:                      "act-pending",
			Status:                  "running",
			StartedAt:               "2026-07-04T11:00:00Z",
			FinishedAt:              nil,
			Actor:                   "",
			Summary:                 map[string]string{},
			AffectedVMIDs:           []string{},
			AffectedHostIDs:         []string{},
			ActivityTrackerEventIDs: []string{},
		})
	}))
	t.Cleanup(srv.Close)

	d := &actionInvocationDataSource{client: newVVMClient(srv.URL, "test-token")}
	var m actionInvocationModel
	m.ID = types.StringValue("act-pending")

	_, err := d.fetchActionInvocation(context.Background(), "act-pending", &m)
	if err != nil {
		t.Fatalf("fetchActionInvocation: unexpected error: %v", err)
	}
	if m.Status.ValueString() != "running" {
		t.Errorf("Status: got %q, want %q", m.Status.ValueString(), "running")
	}
	if m.FinishedAt.ValueString() != "" {
		t.Errorf("FinishedAt: got %q, want empty string for pending invocation", m.FinishedAt.ValueString())
	}
	if m.Summary.IsNull() || m.Summary.IsUnknown() {
		t.Fatal("Summary should not be null/unknown even when empty")
	}
	if len(m.Summary.Elements()) != 0 {
		t.Errorf("Summary len: got %d, want 0", len(m.Summary.Elements()))
	}
}

// TestApiToActionInvocationModel verifies the mapping helper in isolation.
func TestApiToActionInvocationModel(t *testing.T) {
	finishedAt := "2026-07-04T12:00:00Z"
	api := &apiActionInvocation{
		ID:                      "act-test",
		Status:                  "failed",
		StartedAt:               "2026-07-04T11:55:00Z",
		FinishedAt:              &finishedAt,
		Actor:                   "svc-account",
		Summary:                 map[string]string{"reason": "timeout"},
		AffectedVMIDs:           []string{"vm-a", "vm-b"},
		AffectedHostIDs:         []string{"host-x"},
		ActivityTrackerEventIDs: []string{},
	}
	var m actionInvocationModel
	apiToActionInvocationModel(api, &m)

	if m.ID.ValueString() != "act-test" {
		t.Errorf("ID: got %q, want %q", m.ID.ValueString(), "act-test")
	}
	if m.Status.ValueString() != "failed" {
		t.Errorf("Status: got %q, want %q", m.Status.ValueString(), "failed")
	}
	if m.FinishedAt.ValueString() != finishedAt {
		t.Errorf("FinishedAt: got %q, want %q", m.FinishedAt.ValueString(), finishedAt)
	}
	if m.Actor.ValueString() != "svc-account" {
		t.Errorf("Actor: got %q, want %q", m.Actor.ValueString(), "svc-account")
	}

	if len(m.AffectedVMIDs.Elements()) != 2 {
		t.Errorf("AffectedVMIDs len: got %d, want 2", len(m.AffectedVMIDs.Elements()))
	}
	if len(m.AffectedHostIDs.Elements()) != 1 {
		t.Errorf("AffectedHostIDs len: got %d, want 1", len(m.AffectedHostIDs.Elements()))
	}
	if len(m.ActivityTrackerEventIDs.Elements()) != 0 {
		t.Errorf("ActivityTrackerEventIDs len: got %d, want 0", len(m.ActivityTrackerEventIDs.Elements()))
	}
}
