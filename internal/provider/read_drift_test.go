package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Read is what makes `terraform plan` truthful (vvm-iac-spec.md §2.1), and it is
// the one method whose two interesting outcomes cannot be seen without driving
// it through the framework's own state: a resource that has GONE must be removed
// from state, and a resource that CHANGED must come back changed.
//
// 🚨 A 404 READ AS AN ERROR AND A 404 READ AS "GONE" LOOK IDENTICAL FROM THE
// OUTSIDE until a plan runs. The first makes every later plan fail on a resource
// nobody can get rid of; the second is the whole requirement.

func schemaOf(t *testing.T, ctx context.Context, res resource.Resource) fwschema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	res.Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func stateOf(t *testing.T, ctx context.Context, s fwschema.Schema, model any) tfsdk.State {
	t.Helper()
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := st.Set(ctx, model); d.HasError() {
		t.Fatalf("building the prior state: %v", d)
	}
	return st
}

func TestRead_AVMDeletedOutOfBandIsRemovedFromState(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not_found","message":"vm not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	res := &vmResource{client: newVVMClient(srv.URL, "tok")}
	s := schemaOf(t, ctx, res)
	prior := stateOf(t, ctx, s, vmModel{
		ID: strVal("vm-1"), Name: strVal("web-0"), ClusterID: strVal("cluster-1"),
		DatastoreID: strVal("ds-1"), NetworkID: strVal("net-1"),
		VcpuRequest: types.Int64Value(2), MemoryRequestMib: types.Int64Value(2048),
		GoldenImage: strVal("ubuntu-2404"), BootDiskGiB: types.Int64Value(20),
		BootSubnetID: strVal("sub-1"), ActualState: strVal("Running"),
		DesiredState: strVal("Running"), UpdatedAt: strVal("2026-10-10T10:00:00Z"),
		JobID: strVal(""), Encrypted: types.BoolValue(false),
		CurrentHostID: types.StringNull(), GuestIPv4: strVal("192.168.50.11"),
		SSHAuthorizedKey: types.ListNull(types.StringType),
	})
	resp := &resource.ReadResponse{State: prior}
	res.Read(ctx, resource.ReadRequest{State: prior}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("a VM deleted outside Terraform was reported as an error: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("a VM that is gone is still in state; the next plan will not offer to recreate it")
	}
}

func TestRead_AStoppedVMComesBackStopped(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vm-1","desired_state":"Stopped","actual_state":"Stopped",` +
			`"updated_at":"2026-10-10T11:00:00Z","current_host_id":"host-7",` +
			`"desired_guest_ipv4":"192.168.50.11","encrypted":false}`))
	}))
	t.Cleanup(srv.Close)

	res := &vmResource{client: newVVMClient(srv.URL, "tok")}
	s := schemaOf(t, ctx, res)
	prior := stateOf(t, ctx, s, vmModel{
		ID: strVal("vm-1"), Name: strVal("web-0"), ClusterID: strVal("cluster-1"),
		DatastoreID: strVal("ds-1"), NetworkID: strVal("net-1"),
		VcpuRequest: types.Int64Value(2), MemoryRequestMib: types.Int64Value(2048),
		GoldenImage: strVal("ubuntu-2404"), BootDiskGiB: types.Int64Value(20),
		BootSubnetID: strVal("sub-1"), ActualState: strVal("Running"),
		DesiredState: strVal("Running"), UpdatedAt: strVal("2026-10-10T10:00:00Z"),
		JobID: strVal(""), Encrypted: types.BoolValue(false),
		CurrentHostID: types.StringNull(), GuestIPv4: strVal("192.168.50.11"),
		SSHAuthorizedKey: types.ListNull(types.StringType),
	})
	resp := &resource.ReadResponse{State: prior}
	res.Read(ctx, resource.ReadRequest{State: prior}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("a VM that is still there was removed from state")
	}

	var got vmModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ActualState.ValueString() != "Stopped" {
		t.Errorf("actual_state came back %q; a VM stopped out of band shows no drift", got.ActualState.ValueString())
	}
	if got.CurrentHostID.ValueString() != "host-7" {
		t.Errorf("current_host_id came back %q; a VM migrated out of band shows no drift", got.CurrentHostID.ValueString())
	}
	// The declared fields are what the operator asked for and must survive a
	// refresh: a Read that blanked them would make every plan want to replace.
	if got.Name.ValueString() != "web-0" || got.GoldenImage.ValueString() != "ubuntu-2404" {
		t.Errorf("a refresh changed the declared fields: name %q, golden_image %q",
			got.Name.ValueString(), got.GoldenImage.ValueString())
	}
}

func TestRead_ANetworkDeletedOutOfBandIsRemovedFromState(t *testing.T) {
	ctx := context.Background()
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	}))
	t.Cleanup(gone.Close)

	res := &networkResource{client: newVVMClient(gone.URL, "tok")}
	s := schemaOf(t, ctx, res)
	model := networkModel{
		ID: strVal("net-1"), ClusterID: strVal("cluster-1"), Name: strVal("guests"),
		VlanID: types.Int64Value(101), VpcSubnetID: strVal("sub-1"), Cidr: strVal("192.168.50.0/24"),
		IpamRangeStart: strVal("192.168.50.10"), IpamRangeEnd: strVal("192.168.50.200"),
		ActualState: strVal("Active"), DesiredState: strVal("Active"),
		UpdatedAt: strVal("2026-10-10T10:00:00Z"), JobID: strVal(""),
	}
	prior := stateOf(t, ctx, s, model)
	resp := &resource.ReadResponse{State: prior}
	res.Read(ctx, resource.ReadRequest{State: prior}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("a network that is gone is still in state")
	}
}

// TestRead_ADatabaseFaultIsNotReadAsGone — the control for the three tests
// above. A 500 must leave the resource in state: removing it would make the
// next apply build a second network while the first still holds the VLAN.
func TestRead_ADatabaseFaultIsNotReadAsGone(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"internal","message":"query failed"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	res := &networkResource{client: newVVMClient(srv.URL, "tok")}
	s := schemaOf(t, ctx, res)
	prior := stateOf(t, ctx, s, networkModel{
		ID: strVal("net-1"), ClusterID: strVal("cluster-1"), Name: strVal("guests"),
		VlanID: types.Int64Value(101), VpcSubnetID: strVal("sub-1"), Cidr: strVal("192.168.50.0/24"),
		IpamRangeStart: strVal("192.168.50.10"), IpamRangeEnd: strVal("192.168.50.200"),
		ActualState: strVal("Active"), DesiredState: strVal("Active"),
		UpdatedAt: strVal("2026-10-10T10:00:00Z"), JobID: strVal(""),
	})
	resp := &resource.ReadResponse{State: prior}
	res.Read(ctx, resource.ReadRequest{State: prior}, resp)
	if !resp.Diagnostics.HasError() {
		t.Error("a 500 from the control plane was not reported")
	}
	if resp.State.Raw.IsNull() {
		t.Error("a network was removed from state because the control plane's database was unreadable")
	}
}

func TestRead_ADatastoreGrownOutOfBandComesBackGrown(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","desired_state":"Active","actual_state":"Active",` +
			`"updated_at":"2026-10-10T11:00:00Z","capacity_gb":1000}`))
	}))
	t.Cleanup(srv.Close)

	res := &datastoreResource{client: newVVMClient(srv.URL, "tok")}
	s := schemaOf(t, ctx, res)
	prior := stateOf(t, ctx, s, datastoreModel{
		ID: strVal("ds-1"), ClusterID: strVal("cluster-1"), Name: strVal("bulk"),
		MountPath: strVal("/mnt/vvm/bulk"), SizeGiB: types.Int64Value(500),
		ActualState: strVal("Active"), DesiredState: strVal("Active"),
		UpdatedAt: strVal("2026-10-10T10:00:00Z"), JobID: strVal(""),
		CapacityGb: types.Int64Value(500),
	})
	resp := &resource.ReadResponse{State: prior}
	res.Read(ctx, resource.ReadRequest{State: prior}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	var got datastoreModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.CapacityGb.ValueInt64() != 1000 {
		t.Errorf("capacity_gb came back %d; a share grown outside Terraform shows no drift",
			got.CapacityGb.ValueInt64())
	}
	if got.SizeGiB.ValueInt64() != 500 {
		t.Errorf("size_gib, which the operator declared, came back as %d", got.SizeGiB.ValueInt64())
	}
}
