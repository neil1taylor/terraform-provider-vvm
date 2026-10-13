package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A provisioned datastore has no id when the control plane accepts it: the 202
// answers {"status":"Provisioning","job_id":...} and the datastore row is
// inserted by the job (postDatastoreProvision). So the id has to be read from
// the job's result, never from the 202.
//
// 🚨 A create that reads the id from the 202 asks for GET /v1/datastores/ with
// nothing after the slash, and `terraform apply` fails with the share already
// being made and nothing in state to find it by.

type provisionServer struct {
	mu        sync.Mutex
	paths     []string
	jobState  string
	jobResult string
	jobError  string
}

func (p *provisionServer) handler(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.Method+" "+r.URL.Path)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/datastores":
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"Provisioning","job_id":"job-1"}`))
	case r.URL.Path == "/v1/jobs/job-1":
		errField := `null`
		if p.jobError != "" {
			errField = `"` + p.jobError + `"`
		}
		_, _ = w.Write([]byte(`{"id":"job-1","kind":"datastore.provision","state":"` + p.jobState +
			`","result":` + p.jobResult + `,"error_message":` + errField + `,"attempts":1}`))
	case r.URL.Path == "/v1/datastores/ds-9":
		_, _ = w.Write([]byte(`{"id":"ds-9","desired_state":"Active","actual_state":"Active",` +
			`"updated_at":"2026-10-11T11:00:00Z","capacity_gb":10}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
	}
}

func createProvisioned(t *testing.T, p *provisionServer) (*resource.CreateResponse, []string) {
	t.Helper()
	old := asyncPollInterval
	asyncPollInterval = time.Millisecond
	t.Cleanup(func() { asyncPollInterval = old })

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	t.Cleanup(srv.Close)

	res := &datastoreResource{client: newVVMClient(srv.URL, "tok")}
	s := schemaOf(t, ctx, res)
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, datastoreModel{
		ID: types.StringUnknown(), DesiredState: types.StringUnknown(), ActualState: types.StringUnknown(),
		UpdatedAt: types.StringUnknown(), JobID: types.StringUnknown(), CapacityGb: types.Int64Unknown(),
		ClusterID: strVal("cluster-1"), Name: strVal("g5"), MountPath: strVal("/mnt/vvm/g5"),
		SizeGiB: types.Int64Value(10), Zone: strVal("eu-gb-1"),
		FileShareID: types.StringNull(), StorageClass: types.StringNull(),
		EncryptionKpKeyID: types.StringNull(), IOPS: types.Int64Null(),
		CreateTimeout: strVal("1m"), DeleteTimeout: strVal("1m"),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	res.Create(ctx, resource.CreateRequest{Plan: plan}, resp)
	p.mu.Lock()
	defer p.mu.Unlock()
	return resp, append([]string(nil), p.paths...)
}

func TestCreate_AProvisionedDatastoreTakesItsIdFromTheJobResult(t *testing.T) {
	p := &provisionServer{jobState: "succeeded", jobResult: `{"datastore_id":"ds-9"}`}
	resp, paths := createProvisioned(t, p)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create failed: %v (requests: %v)", resp.Diagnostics, paths)
	}
	var got datastoreModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.ID.ValueString() != "ds-9" {
		t.Errorf("state id = %q, want the datastore_id the job reported, ds-9", got.ID.ValueString())
	}
	if got.ActualState.ValueString() != "Active" {
		t.Errorf("actual_state = %q, want Active", got.ActualState.ValueString())
	}
}

func TestCreate_AProvisionJobThatFailsSaysWhyAndNeverAsksForARowWithNoId(t *testing.T) {
	p := &provisionServer{jobState: "failed", jobResult: `null`, jobError: "no capacity in eu-gb-1"}
	resp, paths := createProvisioned(t, p)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("a failed provision job was reported as success (requests: %v)", paths)
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "no capacity in eu-gb-1") {
		t.Errorf("the job's own error is missing from: %v", resp.Diagnostics)
	}
	for _, path := range paths {
		if path == "GET /v1/datastores/" || path == "GET /v1/datastores" {
			t.Errorf("asked for a datastore with no id: %s", path)
		}
	}
}
