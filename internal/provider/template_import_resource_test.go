package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// strVal wraps a string in types.StringValue for use in model literals.
func strVal(s string) types.String {
	return types.StringValue(s)
}

// TestBuildImportBody verifies that buildImportBody constructs the correct
// POST body from a plan model and returns the right idempotency key.
func TestBuildImportBody(t *testing.T) {
	t.Run("with explicit idempotency key", func(t *testing.T) {
		plan := templateImportModel{
			ClusterID:        strVal("cluster-uuid"),
			Name:             strVal("ubuntu-baseline"),
			SourceURL:        strVal("imports/ubuntu.ova"),
			SourceSHA256:     strVal("deadbeef"),
			SourceFormatHint: strVal("ova"),
			DatastoreID:      strVal("ds-uuid"),
			TargetKind:       strVal("template"),
			IdempotencyKey:   strVal("my-idem-key"),
		}
		body, idemKey, err := buildImportBody(plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idemKey != "my-idem-key" {
			t.Errorf("idempotency key: got %q, want %q", idemKey, "my-idem-key")
		}
		if body["cluster_id"] != "cluster-uuid" {
			t.Errorf("cluster_id: got %v, want cluster-uuid", body["cluster_id"])
		}
		src, ok := body["source"].(map[string]any)
		if !ok {
			t.Fatal("source is not a map")
		}
		if src["kind"] != "cos_object" {
			t.Errorf("source.kind: got %v, want cos_object", src["kind"])
		}
		if src["cos_object_key"] != "imports/ubuntu.ova" {
			t.Errorf("source.cos_object_key: got %v", src["cos_object_key"])
		}
		if src["sha256"] != "deadbeef" {
			t.Errorf("source.sha256: got %v", src["sha256"])
		}
		if src["format_hint"] != "ova" {
			t.Errorf("source.format_hint: got %v", src["format_hint"])
		}
		tgt, ok := body["target"].(map[string]any)
		if !ok {
			t.Fatal("target is not a map")
		}
		if tgt["kind"] != "template" {
			t.Errorf("target.kind: got %v, want template", tgt["kind"])
		}
		if tgt["name"] != "ubuntu-baseline" {
			t.Errorf("target.name: got %v", tgt["name"])
		}
		if tgt["datastore_id"] != "ds-uuid" {
			t.Errorf("target.datastore_id: got %v", tgt["datastore_id"])
		}
	})

	t.Run("without explicit idempotency key sends empty string", func(t *testing.T) {
		plan := templateImportModel{
			ClusterID:   strVal("cluster-uuid"),
			Name:        strVal("my-vm"),
			SourceURL:   strVal("imports/win.vhdx"),
			DatastoreID: strVal("ds-uuid"),
			TargetKind:  strVal("vm"),
			NetworkID:   strVal("net-uuid"),
		}
		body, idemKey, err := buildImportBody(plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idemKey != "" {
			t.Errorf("idempotency key should be empty when not provided, got %q", idemKey)
		}
		tgt, ok := body["target"].(map[string]any)
		if !ok {
			t.Fatal("target is not a map")
		}
		if tgt["kind"] != "vm" {
			t.Errorf("target.kind: got %v, want vm", tgt["kind"])
		}
		if tgt["network_id"] != "net-uuid" {
			t.Errorf("target.network_id: got %v", tgt["network_id"])
		}
	})

	t.Run("null TargetKind defaults to template", func(t *testing.T) {
		plan := templateImportModel{
			ClusterID:   strVal("c"),
			Name:        strVal("n"),
			SourceURL:   strVal("s"),
			DatastoreID: strVal("d"),
			// TargetKind is zero-value (null)
		}
		body, _, err := buildImportBody(plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		tgt, ok := body["target"].(map[string]any)
		if !ok {
			t.Fatal("target is not a map")
		}
		if tgt["kind"] != "template" {
			t.Errorf("default target.kind: got %v, want template", tgt["kind"])
		}
	})

	t.Run("optional fields omitted when null", func(t *testing.T) {
		plan := templateImportModel{
			ClusterID:   strVal("c"),
			Name:        strVal("n"),
			SourceURL:   strVal("s"),
			DatastoreID: strVal("d"),
		}
		body, _, _ := buildImportBody(plan)
		src := body["source"].(map[string]any)
		if _, ok := src["sha256"]; ok {
			t.Error("sha256 should be omitted when not set")
		}
		if _, ok := src["format_hint"]; ok {
			t.Error("format_hint should be omitted when not set")
		}
		tgt := body["target"].(map[string]any)
		if _, ok := tgt["network_id"]; ok {
			t.Error("network_id should be omitted when not set")
		}
		if _, ok := tgt["project_id"]; ok {
			t.Error("project_id should be omitted when not set")
		}
	})
}

// TestImportAPIToState verifies that importAPIToState copies computed fields
// correctly from the API response into the model.
func TestImportAPIToState(t *testing.T) {
	t.Run("REGISTERED with template_id", func(t *testing.T) {
		rm, _ := json.Marshal(apiRegisterMeta{
			TemplateID:            "tpl-uuid",
			GuestToolingReadiness: "paravirt-ready",
		})
		api := &apiTemplateImport{
			ID:           "import-uuid",
			State:        "REGISTERED",
			RegisterMeta: rm,
		}
		var m templateImportModel
		importAPIToState(api, &m)
		if m.ID.ValueString() != "import-uuid" {
			t.Errorf("ID: got %q, want import-uuid", m.ID.ValueString())
		}
		if m.Status.ValueString() != "REGISTERED" {
			t.Errorf("Status: got %q, want REGISTERED", m.Status.ValueString())
		}
		if m.TemplateID.ValueString() != "tpl-uuid" {
			t.Errorf("TemplateID: got %q, want tpl-uuid", m.TemplateID.ValueString())
		}
		if m.GuestToolingReadiness.ValueString() != "paravirt-ready" {
			t.Errorf("GuestToolingReadiness: got %q, want paravirt-ready", m.GuestToolingReadiness.ValueString())
		}
	})

	t.Run("REGISTERED with vm_id", func(t *testing.T) {
		rm, _ := json.Marshal(apiRegisterMeta{
			VMID:                  "vm-uuid",
			GuestToolingReadiness: "paravirt-partial",
		})
		api := &apiTemplateImport{
			ID:           "import-uuid",
			State:        "REGISTERED",
			RegisterMeta: rm,
		}
		var m templateImportModel
		importAPIToState(api, &m)
		if m.VMID.ValueString() != "vm-uuid" {
			t.Errorf("VMID: got %q, want vm-uuid", m.VMID.ValueString())
		}
		if m.GuestToolingReadiness.ValueString() != "paravirt-partial" {
			t.Errorf("GuestToolingReadiness: got %q", m.GuestToolingReadiness.ValueString())
		}
	})

	t.Run("STAGED — no register_meta", func(t *testing.T) {
		api := &apiTemplateImport{
			ID:    "import-uuid",
			State: "STAGED",
		}
		var m templateImportModel
		importAPIToState(api, &m)
		if m.Status.ValueString() != "STAGED" {
			t.Errorf("Status: got %q, want STAGED", m.Status.ValueString())
		}
		// TemplateID / VMID should remain unset.
		if m.TemplateID.ValueString() != "" {
			t.Errorf("TemplateID should be empty for STAGED, got %q", m.TemplateID.ValueString())
		}
	})

	t.Run("all guest_tooling_readiness values", func(t *testing.T) {
		for _, readiness := range []string{"paravirt-ready", "paravirt-partial", "unconverted", "unknown"} {
			rm, _ := json.Marshal(apiRegisterMeta{GuestToolingReadiness: readiness})
			api := &apiTemplateImport{State: "REGISTERED", RegisterMeta: rm}
			var m templateImportModel
			importAPIToState(api, &m)
			if m.GuestToolingReadiness.ValueString() != readiness {
				t.Errorf("readiness %q: got %q", readiness, m.GuestToolingReadiness.ValueString())
			}
		}
	})
}

// TestTerminalStates verifies the terminalStates map contains the expected keys.
func TestTerminalStates(t *testing.T) {
	for _, s := range []string{"REGISTERED", "FAILED", "CANCELLED"} {
		if !terminalStates[s] {
			t.Errorf("%q should be a terminal state", s)
		}
	}
	for _, s := range []string{"STAGED", "PROBED", "CONVERTING", "CONVERTED"} {
		if terminalStates[s] {
			t.Errorf("%q should NOT be a terminal state", s)
		}
	}
}

// TestFetchAndPopulate_404 verifies that a 404 from the API is returned as an
// *apiError so the Read method can detect it and remove the resource from state.
func TestFetchAndPopulate_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	r := &templateImportResource{client: newVVMClient(srv.URL, "test-token")}
	var m templateImportModel
	_, err := r.fetchAndPopulate(context.Background(), "gone", &m)
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

// TestFetchAndPopulate_Success verifies the happy-path GET decode.
func TestFetchAndPopulate_Success(t *testing.T) {
	registerMeta, _ := json.Marshal(apiRegisterMeta{
		TemplateID:            "tpl-success",
		GuestToolingReadiness: "paravirt-ready",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiTemplateImport{
			ID:           "import-ok",
			State:        "REGISTERED",
			RegisterMeta: registerMeta,
		})
	}))
	t.Cleanup(srv.Close)

	r := &templateImportResource{client: newVVMClient(srv.URL, "test-token")}
	var m templateImportModel
	api, err := r.fetchAndPopulate(context.Background(), "import-ok", &m)
	if err != nil {
		t.Fatalf("fetchAndPopulate: %v", err)
	}
	if api.State != "REGISTERED" {
		t.Errorf("api.State: got %q, want REGISTERED", api.State)
	}
	if m.TemplateID.ValueString() != "tpl-success" {
		t.Errorf("TemplateID: got %q, want tpl-success", m.TemplateID.ValueString())
	}
}

// TestDelete_409IsAcceptable confirms the Delete method treats 409 (already
// terminal) as a non-error so destroy completes cleanly.
func TestDelete_409IsAcceptable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	// The vvmClient.delete returns a 409 as an *apiError. The resource Delete
	// method is what absorbs it; here we test client behaviour directly.
	c := newVVMClient(srv.URL, "test-token")
	err := c.delete(context.Background(), "/v1/templates/imports/done", "", "")
	if err == nil {
		t.Fatal("expected error for 409, got nil — the resource Delete method should catch this")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("expected *apiError, got %T", err)
	}
	if ae.Status != 409 {
		t.Errorf("expected status 409, got %d", ae.Status)
	}
}

// TestIdempotencyKeyHonoured verifies that the same plan produces the same
// idempotency key on repeated calls (deterministic, no random component).
func TestIdempotencyKeyHonoured(t *testing.T) {
	plan := templateImportModel{
		ClusterID:      strVal("c"),
		Name:           strVal("n"),
		SourceURL:      strVal("s"),
		DatastoreID:    strVal("d"),
		IdempotencyKey: strVal("stable-key"),
	}
	_, key1, _ := buildImportBody(plan)
	_, key2, _ := buildImportBody(plan)
	if key1 != key2 {
		t.Errorf("idempotency key not stable: %q vs %q", key1, key2)
	}
	if key1 != "stable-key" {
		t.Errorf("idempotency key: got %q, want stable-key", key1)
	}
}

// TestPollUntilTerminal_ContextCancellation checks that pollUntilTerminal
// respects context cancellation and returns an error.
func TestPollUntilTerminal_ContextCancellation(t *testing.T) {
	// Server always returns STAGED — never terminal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(apiTemplateImport{
			ID:    "import-stuck",
			State: "CONVERTING",
		})
	}))
	t.Cleanup(srv.Close)

	r := &templateImportResource{client: newVVMClient(srv.URL, "test-token")}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the first sleep in pollUntilTerminal fires the
	// context-done branch.
	cancel()

	var m templateImportModel
	m.ID = strVal("import-stuck")
	_, err := r.pollUntilTerminal(ctx, "import-stuck", &m)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}
