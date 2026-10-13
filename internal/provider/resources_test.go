package provider

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestSchemasMatchTheirModels — an attribute in the schema with no `tfsdk` field
// on the model, or a field with no attribute, fails EVERY operation on that
// resource at runtime with a value-conversion error. Nothing catches it at
// compile time, and the provider still builds and still loads.
func TestSchemasMatchTheirModels(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		res   resource.Resource
		model any
	}{
		{"vvm_network", NewNetworkResource(), networkModel{}},
		{"vvm_datastore", NewDatastoreResource(), datastoreModel{}},
		{"vvm_vm", NewVMResource(), vmModel{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &resource.SchemaResponse{}
			c.res.Schema(ctx, resource.SchemaRequest{}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("the schema reported errors: %v", resp.Diagnostics)
			}
			if len(resp.Schema.Attributes) == 0 {
				t.Fatal("the schema declares no attributes")
			}

			onModel := map[string]bool{}
			rt := reflect.TypeOf(c.model)
			for i := 0; i < rt.NumField(); i++ {
				tag := rt.Field(i).Tag.Get("tfsdk")
				if tag == "" {
					t.Errorf("model field %s has no tfsdk tag", rt.Field(i).Name)
					continue
				}
				onModel[tag] = true
			}
			for name := range resp.Schema.Attributes {
				if !onModel[name] {
					t.Errorf("schema attribute %q has no field on the model", name)
				}
			}
			for name := range onModel {
				if _, ok := resp.Schema.Attributes[name]; !ok {
					t.Errorf("model field %q has no attribute in the schema", name)
				}
			}
			if resp.Schema.MarkdownDescription == "" {
				t.Error("the resource has no description, so `terraform providers schema` documents nothing")
			}
		})
	}
}

// TestResourceTypeNames — the names in the issue and the spec. A typo here is a
// resource nobody's HCL can name.
func TestResourceTypeNames(t *testing.T) {
	ctx := context.Background()
	want := map[string]resource.Resource{
		"vvm_network":   NewNetworkResource(),
		"vvm_datastore": NewDatastoreResource(),
		"vvm_vm":        NewVMResource(),
	}
	for name, res := range want {
		resp := &resource.MetadataResponse{}
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "vvm"}, resp)
		if resp.TypeName != name {
			t.Errorf("got type name %q, want %q", resp.TypeName, name)
		}
	}
}

// TestProviderRegistersTheThreeResources — a resource that builds, has a schema
// and is not in this list does not exist as far as Terraform is concerned.
func TestProviderRegistersTheThreeResources(t *testing.T) {
	ctx := context.Background()
	got := map[string]bool{}
	for _, mk := range New("test")().Resources(ctx) {
		resp := &resource.MetadataResponse{}
		mk().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "vvm"}, resp)
		got[resp.TypeName] = true
	}
	for _, name := range []string{"vvm_vm", "vvm_network", "vvm_datastore"} {
		if !got[name] {
			t.Errorf("the provider does not register %s; a config naming it fails at init", name)
		}
	}
	// The control: the resources that were already there are still registered,
	// so adding three has not replaced four.
	for _, name := range []string{"vvm_backup_policy", "vvm_protection_group", "vvm_template_import", "vvm_vm_brs_enrollment"} {
		if !got[name] {
			t.Errorf("%s is no longer registered", name)
		}
	}
}

// TestSecretBearingAttributesAreSensitive — Terraform prints every
// non-sensitive value: in the plan a reviewer reads, in the CI log that plan is
// kept in, and in the Schematics workspace that stores it. cloud-init user-data
// is where a password hash, a join token or a registry credential goes.
//
// ⚠️ The control matters as much as the claim. Marking everything sensitive
// would make a plan unreadable and would pass this test just as well, so the
// fields that must stay visible are asserted too.
func TestSecretBearingAttributesAreSensitive(t *testing.T) {
	ctx := context.Background()
	resp := &resource.SchemaResponse{}
	NewVMResource().Schema(ctx, resource.SchemaRequest{}, resp)

	for _, name := range []string{"user_data"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("%s is not in the schema", name)
		}
		if !attr.IsSensitive() {
			t.Errorf("%s is not sensitive, so Terraform prints its value in every plan and log", name)
		}
	}
	// A public key is public, an address is not a secret, and a plan that hides
	// which image or host a VM is on cannot be reviewed.
	for _, name := range []string{"name", "golden_image", "ssh_authorized_keys", "guest_ipv4", "actual_state"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("%s is not in the schema", name)
		}
		if attr.IsSensitive() {
			t.Errorf("%s is marked sensitive; it carries no secret and hiding it makes a plan unreadable", name)
		}
	}
}

func TestBuildNetworkBody(t *testing.T) {
	plan := networkModel{
		ClusterID:      strVal("cluster-1"),
		Name:           strVal("guests"),
		VlanID:         types.Int64Value(101),
		VpcSubnetID:    strVal("sub-1"),
		Cidr:           strVal("10.242.0.0/24"),
		IpamRangeStart: strVal("10.242.0.10"),
		IpamRangeEnd:   strVal("10.242.0.200"),
	}
	body := buildNetworkBody(plan)
	for key, want := range map[string]any{
		"cluster_id":       "cluster-1",
		"name":             "guests",
		"vlan_id":          int64(101),
		"vpc_subnet_id":    "sub-1",
		"cidr":             "10.242.0.0/24",
		"ipam_range_start": "10.242.0.10",
		"ipam_range_end":   "10.242.0.200",
	} {
		if body[key] != want {
			t.Errorf("%s: got %v (%T), want %v", key, body[key], body[key], want)
		}
	}
	if _, present := body["ovn_logical_switch_id"]; present {
		t.Error("an OVN switch the operator did not name was sent anyway; " +
			"an empty switch id is not the same request as no switch id")
	}
	// The control: when they DO name one it is sent, so the absence above is the
	// omission and not a field the builder never writes.
	plan.OvnLogicalSwitchID = strVal("sw-1")
	if buildNetworkBody(plan)["ovn_logical_switch_id"] != "sw-1" {
		t.Error("a named OVN switch was not sent")
	}
}

func TestBuildDatastoreBody_OneModeOnly(t *testing.T) {
	base := datastoreModel{
		ClusterID: strVal("cluster-1"),
		Name:      strVal("bulk"),
		MountPath: strVal("/mnt/vvm/bulk"),
	}

	t.Run("adopt", func(t *testing.T) {
		plan := base
		plan.FileShareID = strVal("share-1")
		body, err := buildDatastoreBody(plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body["file_share_id"] != "share-1" {
			t.Errorf("file_share_id: got %v", body["file_share_id"])
		}
		if _, present := body["size_gib"]; present {
			t.Error("adopt mode sent size_gib, which the control plane refuses alongside file_share_id")
		}
		if body["mount_path"] != "/mnt/vvm/bulk" {
			t.Errorf("mount_path: got %v", body["mount_path"])
		}
	})

	t.Run("provision", func(t *testing.T) {
		plan := base
		plan.SizeGiB = types.Int64Value(500)
		body, err := buildDatastoreBody(plan)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body["size_gib"] != int64(500) {
			t.Errorf("size_gib: got %v (%T)", body["size_gib"], body["size_gib"])
		}
		if _, present := body["file_share_id"]; present {
			t.Error("provision mode sent file_share_id")
		}
	})

	t.Run("both is refused before anything is made", func(t *testing.T) {
		plan := base
		plan.FileShareID = strVal("share-1")
		plan.SizeGiB = types.Int64Value(500)
		_, err := buildDatastoreBody(plan)
		if err == nil {
			t.Fatal("a body naming both a share to adopt and a size to provision was accepted")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("the message does not say which of the two to drop: %v", err)
		}
	})

	t.Run("neither is refused", func(t *testing.T) {
		if _, err := buildDatastoreBody(base); err == nil {
			t.Fatal("a datastore with no share and no size was accepted; it names no storage at all")
		}
	})

	t.Run("optional fields are omitted when unset", func(t *testing.T) {
		plan := base
		plan.SizeGiB = types.Int64Value(500)
		body, _ := buildDatastoreBody(plan)
		for _, key := range []string{"zone", "storage_class", "encryption_kp_key_id", "iops"} {
			if _, present := body[key]; present {
				t.Errorf("%s was sent although the operator did not set it", key)
			}
		}
		plan.Zone = strVal("eu-gb-1")
		plan.StorageClass = strVal("dp2")
		plan.EncryptionKpKeyID = strVal("key-1")
		plan.IOPS = types.Int64Value(3000)
		body, _ = buildDatastoreBody(plan)
		for key, want := range map[string]any{
			"zone": "eu-gb-1", "storage_class": "dp2", "encryption_kp_key_id": "key-1", "iops": int64(3000),
		} {
			if body[key] != want {
				t.Errorf("%s: got %v, want %v", key, body[key], want)
			}
		}
	})
}

func TestBuildVMBody(t *testing.T) {
	ctx := context.Background()
	base := vmModel{
		ClusterID:        strVal("cluster-1"),
		DatastoreID:      strVal("ds-1"),
		NetworkID:        strVal("net-1"),
		Name:             strVal("web-0"),
		VcpuRequest:      types.Int64Value(4),
		MemoryRequestMib: types.Int64Value(8192),
		GoldenImage:      strVal("ubuntu-2404"),
		BootDiskGiB:      types.Int64Value(20),
		BootSubnetID:     strVal("sub-1"),
	}

	t.Run("the fields the scheduler needs", func(t *testing.T) {
		var diags diag.Diagnostics
		body := buildVMBody(ctx, base, &diags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		for key, want := range map[string]any{
			"cluster_id": "cluster-1", "datastore_id": "ds-1", "network_id": "net-1",
			"name": "web-0", "vcpu_request": int64(4), "memory_request_mib": int64(8192),
			"golden_image": "ubuntu-2404", "boot_disk_gib": int64(20), "boot_subnet_id": "sub-1",
		} {
			if body[key] != want {
				t.Errorf("%s: got %v (%T), want %v", key, body[key], body[key], want)
			}
		}
	})

	t.Run("no cloud_init block when no cloud-init field is set", func(t *testing.T) {
		var diags diag.Diagnostics
		body := buildVMBody(ctx, base, &diags)
		if _, present := body["cloud_init"]; present {
			t.Error("an empty cloud_init block was sent; it is a different request from none")
		}
		if _, present := body["encrypted"]; present {
			t.Error("encrypted was sent for a VM that did not ask for it")
		}
		if _, present := body["dr_tier"]; present {
			t.Error("dr_tier was sent for a VM that did not ask for one")
		}
	})

	t.Run("cloud-init is nested the way the API takes it", func(t *testing.T) {
		plan := base
		plan.Hostname = strVal("web-0")
		plan.UserData = strVal("#cloud-config\n")
		keys, d := types.ListValueFrom(ctx, types.StringType, []string{"ssh-ed25519 AAAA", "ssh-ed25519 BBBB"})
		if d.HasError() {
			t.Fatalf("building the key list: %v", d)
		}
		plan.SSHAuthorizedKey = keys

		var diags diag.Diagnostics
		body := buildVMBody(ctx, plan, &diags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		ci, ok := body["cloud_init"].(map[string]any)
		if !ok {
			t.Fatalf("cloud_init is %T, not a nested object", body["cloud_init"])
		}
		if ci["hostname"] != "web-0" {
			t.Errorf("cloud_init.hostname: got %v", ci["hostname"])
		}
		if ci["user_data"] != "#cloud-config\n" {
			t.Errorf("cloud_init.user_data: got %v", ci["user_data"])
		}
		got, ok := ci["ssh_authorized_keys"].([]string)
		if !ok {
			t.Fatalf("cloud_init.ssh_authorized_keys is %T, not a list of strings", ci["ssh_authorized_keys"])
		}
		if !reflect.DeepEqual(got, []string{"ssh-ed25519 AAAA", "ssh-ed25519 BBBB"}) {
			t.Errorf("the keys sent were %v", got)
		}
		// ⛔ Flat in HCL, nested on the wire — so the flat names must NOT also
		// appear at the top level, where the control plane would ignore them and
		// the guest would come up with no keys and no user-data.
		for _, key := range []string{"hostname", "user_data", "ssh_authorized_keys"} {
			if _, present := body[key]; present {
				t.Errorf("%s was sent at the top level, where the API does not read it", key)
			}
		}
	})

	t.Run("encrypted is sent only when it is true", func(t *testing.T) {
		plan := base
		plan.Encrypted = types.BoolValue(false)
		var diags diag.Diagnostics
		if _, present := buildVMBody(ctx, plan, &diags)["encrypted"]; present {
			t.Error("encrypted:false was sent; it says nothing the omission does not")
		}
		plan.Encrypted = types.BoolValue(true)
		if buildVMBody(ctx, plan, &diags)["encrypted"] != true {
			t.Error("a VM that asked for an encrypted disk did not say so")
		}
	})

	t.Run("dr_tier is passed through so the control plane can refuse it", func(t *testing.T) {
		plan := base
		plan.DrTier = strVal("none")
		var diags diag.Diagnostics
		if buildVMBody(ctx, plan, &diags)["dr_tier"] != "none" {
			t.Error("dr_tier was not sent")
		}
	})
}

// TestVMWaitTargets — which states a create gives up on. Getting this wrong in
// either direction is costly: give up on Failed and an apply fails on a VM that
// HA is about to restart elsewhere; wait on ScheduleFailed and the apply sits
// for its whole budget on a VM nothing will ever place again.
func TestVMWaitTargets(t *testing.T) {
	w := (&vmResource{}).waitFor("vm-1", "job-1")
	if w.target != "Running" {
		t.Errorf("a VM create waits for %q, want Running", w.target)
	}
	if w.path != "/v1/vms/vm-1" {
		t.Errorf("the VM is read at %q", w.path)
	}
	has := func(s string) bool {
		for _, f := range w.failed {
			if f == s {
				return true
			}
		}
		return false
	}
	if !has("ScheduleFailed") {
		t.Error("a VM no host could take is waited on until the budget runs out, " +
			"although nothing retries placement")
	}
	if has("Failed") {
		t.Error("Failed is treated as terminal, but HA restarts a Failed VM on another host, " +
			"so the apply would fail on a VM that is coming back")
	}
}

func TestNetworkAndDatastoreWaitTargets(t *testing.T) {
	n := (&networkResource{}).waitFor("net-1", "")
	if n.target != "Active" || n.path != "/v1/networks/net-1" {
		t.Errorf("network wait: target %q at %q", n.target, n.path)
	}
	d := (&datastoreResource{}).waitFor("ds-1", "")
	if d.target != "Active" || d.path != "/v1/datastores/ds-1" {
		t.Errorf("datastore wait: target %q at %q", d.target, d.path)
	}
}

// TestVMRowToState_AnAbsentAddressDoesNotEraseTheOneWeWereGiven — on a VSI
// cluster the create answer is the only place the guest's address appears: there
// is no DHCP lease and no DNS record. A read that does not carry one must not be
// read as the guest having lost it.
func TestVMRowToState_AnAbsentAddressDoesNotEraseTheOneWeWereGiven(t *testing.T) {
	m := vmModel{GuestIPv4: strVal("10.242.0.11")}
	vmRowToState(vmRow{asyncRow: asyncRow{ID: "vm-1", ActualState: "Running"}}, &m)
	if m.GuestIPv4.ValueString() != "10.242.0.11" {
		t.Errorf("guest_ipv4 became %q after a read that carried none", m.GuestIPv4.ValueString())
	}
	// The control: a read that DOES carry one updates it, so the line above is
	// not a field that is simply never written.
	addr := "10.242.0.12"
	vmRowToState(vmRow{asyncRow: asyncRow{ID: "vm-1"}, DesiredGuestIPv4: &addr}, &m)
	if m.GuestIPv4.ValueString() != addr {
		t.Errorf("guest_ipv4 is %q after a read reporting %q", m.GuestIPv4.ValueString(), addr)
	}
}

// TestVMRowToState_ObservedFieldsAreRefreshed — these are what make `plan` show
// drift. A row-to-state that left them alone would report a VM as Running for
// ever after its first apply.
func TestVMRowToState_ObservedFieldsAreRefreshed(t *testing.T) {
	host := "host-7"
	enc := true
	m := vmModel{ActualState: strVal("Running"), CurrentHostID: strVal("host-1")}
	vmRowToState(vmRow{
		asyncRow:      asyncRow{ID: "vm-1", DesiredState: "Running", ActualState: "Stopped", UpdatedAt: "2026-10-10T10:00:00Z"},
		CurrentHostID: &host,
		Encrypted:     &enc,
	}, &m)
	if m.ActualState.ValueString() != "Stopped" {
		t.Errorf("actual_state is %q after a read saying Stopped; a plan would show no drift", m.ActualState.ValueString())
	}
	if m.CurrentHostID.ValueString() != host {
		t.Errorf("current_host_id is %q after a read saying %q", m.CurrentHostID.ValueString(), host)
	}
	if m.UpdatedAt.ValueString() != "2026-10-10T10:00:00Z" {
		t.Errorf("updated_at is %q", m.UpdatedAt.ValueString())
	}
	if !m.Encrypted.ValueBool() {
		t.Error("encrypted was not read back")
	}

	// A VM with no placement reads as null rather than the host it used to be
	// on: "" would be a host id nothing can look up.
	vmRowToState(vmRow{asyncRow: asyncRow{ID: "vm-1", ActualState: "Scheduling"}}, &m)
	if !m.CurrentHostID.IsNull() {
		t.Errorf("current_host_id is %q for an unplaced VM, want null", m.CurrentHostID.ValueString())
	}
}

func TestDatastoreRowToState_CapacityIsTheMeasuredOne(t *testing.T) {
	m := datastoreModel{CapacityGb: types.Int64Value(100)}
	datastoreRowToState(datastoreRow{
		asyncRow:   asyncRow{ID: "ds-1", ActualState: "Active", UpdatedAt: "2026-10-10T10:00:00Z"},
		CapacityGb: 500,
	}, &m)
	if m.CapacityGb.ValueInt64() != 500 {
		t.Errorf("capacity_gb is %d after a read reporting 500; a share grown outside Terraform "+
			"would never show up", m.CapacityGb.ValueInt64())
	}
	if m.ActualState.ValueString() != "Active" {
		t.Errorf("actual_state is %q", m.ActualState.ValueString())
	}
}

func TestNetworkRowToState(t *testing.T) {
	// ⚠️ EVERY COMPUTED ATTRIBUTE STARTS UNKNOWN, because that is what a plan
	// hands Create: "(known after apply)". The model's zero value is NULL, not
	// unknown, so a test built from it could never exercise the case this
	// guards — Terraform fails an apply whose state comes back unknown
	// ("Provider produced inconsistent result ... value is unknown").
	m := networkModel{
		ID: types.StringUnknown(), DesiredState: types.StringUnknown(),
		ActualState: types.StringUnknown(), UpdatedAt: types.StringUnknown(),
		JobID: types.StringUnknown(),
	}
	networkRowToState(asyncRow{ID: "net-1", DesiredState: "Active", ActualState: "Provisioning",
		UpdatedAt: "2026-10-10T10:00:00Z"}, &m)
	if m.ID.ValueString() != "net-1" || m.ActualState.ValueString() != "Provisioning" {
		t.Errorf("got id %q state %q", m.ID.ValueString(), m.ActualState.ValueString())
	}
	for name, v := range map[string]types.String{
		"desired_state": m.DesiredState, "actual_state": m.ActualState,
		"updated_at": m.UpdatedAt, "job_id": m.JobID, "id": m.ID,
	} {
		if v.IsUnknown() {
			t.Errorf("%s is still unknown after a read; Terraform fails the apply on it", name)
		}
	}
}

// TestTimeoutDefaultsAreDurations — a default that does not parse turns every
// apply of that resource into an "Invalid create_timeout" error.
func TestTimeoutDefaultsAreDurations(t *testing.T) {
	for name, given := range map[string]string{
		"networkCreateTimeout":   networkCreateTimeout,
		"networkDeleteTimeout":   networkDeleteTimeout,
		"datastoreCreateTimeout": datastoreCreateTimeout,
		"datastoreDeleteTimeout": datastoreDeleteTimeout,
		"vmCreateTimeout":        vmCreateTimeout,
		"vmDeleteTimeout":        vmDeleteTimeout,
	} {
		if _, err := parseTimeout("", given); err != nil {
			t.Errorf("%s (%q) is not a usable duration: %v", name, given, err)
		}
	}
	// The VM's delete has to outlast the five minutes the control plane holds a
	// delete after a migration settles (#728), or every destroy of a VM that has
	// ever been moved fails.
	d, err := parseTimeout("", vmDeleteTimeout)
	if err != nil || d.Minutes() < 6 {
		t.Errorf("vmDeleteTimeout is %v; the product holds a delete for five minutes after a move", d)
	}
}

// TestEveryResourceRefusesUpdate — all attributes are ForceNew, so Update is
// unreachable. It must say so rather than silently do nothing, which would
// report a change applied that never left Terraform.
func TestEveryResourceRefusesUpdate(t *testing.T) {
	ctx := context.Background()
	for _, res := range []resource.Resource{NewNetworkResource(), NewDatastoreResource(), NewVMResource()} {
		resp := &resource.UpdateResponse{}
		res.Update(ctx, resource.UpdateRequest{}, resp)
		if !resp.Diagnostics.HasError() {
			resp2 := &resource.MetadataResponse{}
			res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "vvm"}, resp2)
			t.Errorf("%s accepted an Update without reporting anything", resp2.TypeName)
		}
	}
}

// TestLifecycleAttributesAreComputedOnly — desired_state and actual_state are
// readings. Declaring one would let an operator write a state into Terraform
// that the control plane never agreed to.
func TestLifecycleAttributesAreComputedOnly(t *testing.T) {
	attrs := lifecycleAttributes("thing")
	names := make([]string, 0, len(attrs))
	for name, attr := range attrs {
		names = append(names, name)
		if attr.IsRequired() || attr.IsOptional() {
			t.Errorf("%s is settable; it is a reading, not an instruction", name)
		}
		if !attr.IsComputed() {
			t.Errorf("%s is not computed", name)
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "actual_state,desired_state,id,job_id,updated_at" {
		t.Errorf("the shared lifecycle attributes are %q", got)
	}
}
