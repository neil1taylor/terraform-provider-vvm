package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &vmResource{}

// A VM create is scheduling, a disk from the golden image, a network reconcile
// and a boot. The delete waits longer than it looks: the control plane holds a
// delete for five minutes after a migration of that VM settles.
const (
	vmCreateTimeout = "45m"
	vmDeleteTimeout = "30m"
)

// vmResource manages a VVM VM (`/v1/vms`).
type vmResource struct {
	client *vvmClient
}

type vmModel struct {
	ID           types.String `tfsdk:"id"`
	DesiredState types.String `tfsdk:"desired_state"`
	ActualState  types.String `tfsdk:"actual_state"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
	JobID        types.String `tfsdk:"job_id"`

	ClusterID        types.String `tfsdk:"cluster_id"`
	DatastoreID      types.String `tfsdk:"datastore_id"`
	NetworkID        types.String `tfsdk:"network_id"`
	Name             types.String `tfsdk:"name"`
	VcpuRequest      types.Int64  `tfsdk:"vcpu_request"`
	MemoryRequestMib types.Int64  `tfsdk:"memory_request_mib"`
	GoldenImage      types.String `tfsdk:"golden_image"`
	BootDiskGiB      types.Int64  `tfsdk:"boot_disk_gib"`
	BootSubnetID     types.String `tfsdk:"boot_subnet_id"`
	DrTier           types.String `tfsdk:"dr_tier"`
	Encrypted        types.Bool   `tfsdk:"encrypted"`
	Hostname         types.String `tfsdk:"hostname"`
	SSHAuthorizedKey types.List   `tfsdk:"ssh_authorized_keys"`
	UserData         types.String `tfsdk:"user_data"`

	CurrentHostID types.String `tfsdk:"current_host_id"`
	GuestIPv4     types.String `tfsdk:"guest_ipv4"`

	CreateTimeout types.String `tfsdk:"create_timeout"`
	DeleteTimeout types.String `tfsdk:"delete_timeout"`
}

// vmRow is a VM read, with the fields beyond the shared lifecycle set that this
// resource reports.
type vmRow struct {
	asyncRow
	CurrentHostID    *string `json:"current_host_id"`
	DesiredGuestIPv4 *string `json:"desired_guest_ipv4"`
	Encrypted        *bool   `json:"encrypted"`
}

func NewVMResource() resource.Resource { return &vmResource{} }

func (r *vmResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vm"
}

func (r *vmResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A VVM virtual machine (`/v1/vms`).\n\n" +
			"`terraform apply` returns when the control plane reports the VM `Running` — scheduled onto a " +
			"host, its disk made from the golden image, its network reconciled and booted. It does not " +
			"return when the API merely accepted the create. On timeout the error carries the state the " +
			"control plane last reported and the state of the scheduling job, so a VM that could not be " +
			"placed says so.\n\n" +
			"Every attribute is immutable: a change destroys and recreates. Power and resize are day-2 " +
			"operations, done through the API or the Ansible collection, not by editing this resource.\n\n" +
			"⚠️ There is no DHCP. On a VSI cluster the control plane assigns the guest's address at create " +
			"and it is reported in `guest_ipv4`; nothing else will tell you what it is.",
		Attributes: mergeAttributes(
			lifecycleAttributes("VM"),
			timeoutAttributes(vmCreateTimeout, vmDeleteTimeout),
			map[string]schema.Attribute{
				"cluster_id": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "UUID of the cluster the VM runs on.",
					PlanModifiers:       forceNew(),
				},
				"datastore_id": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "UUID of the datastore the VM's disks live on — reference a " +
						"`vvm_datastore`. It must belong to `cluster_id`.",
					PlanModifiers: forceNew(),
				},
				"network_id": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "UUID of the guest network the VM attaches to — reference a " +
						"`vvm_network`. It must belong to `cluster_id`, and a network that is being " +
						"deleted is refused.",
					PlanModifiers: forceNew(),
				},
				"name": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "Name of the VM.",
					PlanModifiers:       forceNew(),
				},
				"vcpu_request": schema.Int64Attribute{
					Required:            true,
					MarkdownDescription: "vCPUs the scheduler must find room for.",
					PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"memory_request_mib": schema.Int64Attribute{
					Required:            true,
					MarkdownDescription: "Memory the scheduler must find room for, in MiB.",
					PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"golden_image": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Stem of the golden image on the datastore, WITHOUT an extension: " +
						"the host resolves `ubuntu-2404` to `<images>/ubuntu-2404.qcow2`. " +
						"A value carrying `.qcow2`, a path separator or a leading dot is refused.",
					PlanModifiers: forceNew(),
				},
				"boot_disk_gib": schema.Int64Attribute{
					Required:            true,
					MarkdownDescription: "Size of the boot disk, in GiB.",
					PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"boot_subnet_id": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "Id of the VPC subnet the VM's NIC attaches to.",
					PlanModifiers:       forceNew(),
				},
				"dr_tier": schema.StringAttribute{
					Optional: true,
					MarkdownDescription: "DR tier. ⛔ Only `none` is accepted today — VVM replicates no VM to " +
						"another region, so the control plane refuses any other tier rather than record " +
						"protection that does not exist.",
					PlanModifiers: forceNew(),
				},
				"encrypted": schema.BoolAttribute{
					Optional: true,
					Computed: true,
					Default:  booldefault.StaticBool(false),
					MarkdownDescription: "Encrypt the boot disk under the project's Key Protect root key. " +
						"Refused when the control plane has no Key Protect instance configured, rather " +
						"than accepted and silently stored as a plain disk.",
					PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				},
				"hostname": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "cloud-init hostname for the guest.",
					PlanModifiers:       forceNew(),
				},
				"ssh_authorized_keys": schema.ListAttribute{
					Optional:    true,
					ElementType: types.StringType,
					MarkdownDescription: "Public keys cloud-init installs in the guest. " +
						"Keys that would reach no user who can log in are refused at create.",
					PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
				},
				"user_data": schema.StringAttribute{
					Optional: true,
					// 🚨 SENSITIVE, BECAUSE TERRAFORM PRINTS EVERYTHING ELSE.
					// cloud-init user-data is where a password hash, a join
					// token or a registry credential goes, and a plan is read
					// aloud in reviews, kept in CI logs and, under Schematics,
					// stored with the workspace.
					Sensitive:           true,
					MarkdownDescription: "cloud-init user-data, handed to the guest verbatim, e.g. `file(\"cloud-init/web.yaml\")`.",
					PlanModifiers:       forceNew(),
				},
				"current_host_id": schema.StringAttribute{
					Computed: true,
					MarkdownDescription: "The host the VM is OBSERVED on. Null until the scheduler places it. " +
						"A VM migrated out of band shows up here on the next plan.",
				},
				"guest_ipv4": schema.StringAttribute{
					Computed: true,
					MarkdownDescription: "The guest's address, assigned by the control plane on a VSI cluster. " +
						"Empty on bare metal, where the guest takes its address from the network itself.",
				},
			},
		),
	}
}

func (r *vmResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req, resp)
}

// buildVMBody is the POST /v1/vms body.
//
// The three cloud-init attributes are flat in HCL and nested here, because
// that is the shape the API takes. An absent cloud_init is left out
// altogether: an empty block is a different request.
func buildVMBody(ctx context.Context, plan vmModel, diags *diag.Diagnostics) map[string]any {
	body := map[string]any{
		"cluster_id":         plan.ClusterID.ValueString(),
		"datastore_id":       plan.DatastoreID.ValueString(),
		"network_id":         plan.NetworkID.ValueString(),
		"name":               plan.Name.ValueString(),
		"vcpu_request":       plan.VcpuRequest.ValueInt64(),
		"memory_request_mib": plan.MemoryRequestMib.ValueInt64(),
		"golden_image":       plan.GoldenImage.ValueString(),
		"boot_disk_gib":      plan.BootDiskGiB.ValueInt64(),
		"boot_subnet_id":     plan.BootSubnetID.ValueString(),
	}
	if tier := optionalString(plan.DrTier); tier != "" {
		body["dr_tier"] = tier
	}
	// Sent only when it is true. `encrypted: false` and no field at all mean the
	// same thing to the control plane, and the smaller body is the honest one.
	if !plan.Encrypted.IsNull() && !plan.Encrypted.IsUnknown() && plan.Encrypted.ValueBool() {
		body["encrypted"] = true
	}

	cloudInit := map[string]any{}
	if h := optionalString(plan.Hostname); h != "" {
		cloudInit["hostname"] = h
	}
	if u := optionalString(plan.UserData); u != "" {
		cloudInit["user_data"] = u
	}
	if !plan.SSHAuthorizedKey.IsNull() && !plan.SSHAuthorizedKey.IsUnknown() {
		var keys []string
		diags.Append(plan.SSHAuthorizedKey.ElementsAs(ctx, &keys, false)...)
		if len(keys) > 0 {
			cloudInit["ssh_authorized_keys"] = keys
		}
	}
	if len(cloudInit) > 0 {
		body["cloud_init"] = cloudInit
	}
	return body
}

func (r *vmResource) waitFor(id, jobID string) stateWait {
	return stateWait{
		client: r.client,
		path:   "/v1/vms/" + id,
		jobID:  jobID,
		kind:   "VM",
		target: "Running",
		// ⛔ 'Failed' is NOT in this list, deliberately. A VM whose host went
		// away reads Failed and HA restarts it elsewhere, so Failed is a state
		// it can leave. ScheduleFailed is not: nothing retries placement.
		failed: []string{"ScheduleFailed"},
	}
}

func (r *vmResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vmModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(plan.CreateTimeout), vmCreateTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid create_timeout", err.Error())
		return
	}
	body := buildVMBody(ctx, plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := idempotencyKeyForBody("vvm_vm", body)
	if err != nil {
		resp.Diagnostics.AddError("Error creating VM", err.Error())
		return
	}
	raw, err := r.client.post(ctx, "/v1/vms", body, key)
	if err != nil {
		resp.Diagnostics.AddError("Error creating VM", err.Error())
		return
	}
	var ack writeAck
	if jsonErr := json.Unmarshal(raw, &ack); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing the VM create response", jsonErr.Error())
		return
	}
	plan.ID = types.StringValue(ack.ID)
	plan.JobID = types.StringValue(ack.JobID)
	// Taken from the create answer because nothing else reports it: there is no
	// DHCP lease and no DNS record to look the address up in afterwards.
	plan.GuestIPv4 = types.StringValue(ack.GuestIPv4)
	plan.CurrentHostID = types.StringNull()

	// 🚨 THE ID IS SAVED EVEN WHEN THE WAIT FAILS. A VM the control plane is
	// building that Terraform has forgotten is a guest nothing will ever delete:
	// it holds its name, its address and a host's capacity for good.
	_, last, waitErr := r.waitFor(ack.ID, ack.JobID).wait(ctx, timeout)
	r.applyRaw(last, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if waitErr != nil {
		resp.Diagnostics.AddError("VM did not reach Running", waitErr.Error())
	}
}

func (r *vmResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state vmModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, raw, err := readRow(ctx, r.client, "/v1/vms/"+state.ID.ValueString())
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error reading VM", err.Error())
		return
	}
	r.applyRaw(raw, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *vmResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Update not supported",
		"Every attribute of vvm_vm is immutable, so a change destroys and recreates. "+
			"Reaching this is a provider bug.")
}

func (r *vmResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vmModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(state.DeleteTimeout), vmDeleteTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid delete_timeout", err.Error())
		return
	}
	// A VM delete reaches the host that holds it, and the row goes only when
	// that host confirms the domain is gone. Waiting is the whole point:
	// a destroy that returned on the 202 would hand the next apply a name and an
	// address a running guest still has.
	if err := r.waitFor(state.ID.ValueString(), optionalString(state.JobID)).
		deleteAndWait(ctx, timeout); err != nil {
		resp.Diagnostics.AddError("Error deleting VM", err.Error())
	}
}

// applyRaw decodes one VM answer onto the model. Shared by Create and Read so
// the observed fields are read out of a body in one place, and so Create works
// from the answer the wait already had rather than asking again.
//
// An answer it cannot decode is a warning, not an error: the VM exists either
// way and losing its id is the worse outcome.
func (r *vmResource) applyRaw(raw []byte, m *vmModel, diags *diag.Diagnostics) {
	m.GuestIPv4 = knownOrEmpty(m.GuestIPv4)
	if len(raw) == 0 {
		return
	}
	var row vmRow
	if err := json.Unmarshal(raw, &row); err != nil {
		diags.AddWarning("Could not read the VM back",
			fmt.Sprintf("The control plane's answer for VM %s could not be parsed: %s. "+
				"Its observed state in Terraform may be behind the control plane's.", m.ID.ValueString(), err))
		return
	}
	vmRowToState(row, m)
}

func vmRowToState(row vmRow, m *vmModel) {
	applyLifecycle(row.asyncRow, &m.ID, &m.DesiredState, &m.ActualState, &m.UpdatedAt, &m.JobID)
	if row.CurrentHostID != nil && *row.CurrentHostID != "" {
		m.CurrentHostID = types.StringValue(*row.CurrentHostID)
	} else {
		m.CurrentHostID = types.StringNull()
	}
	// ⚠️ An absent address does NOT overwrite the one the create reported. On a
	// bare-metal cluster there is no assigned address at all, and a read that
	// does not carry one is not the control plane saying the guest lost it.
	if row.DesiredGuestIPv4 != nil && *row.DesiredGuestIPv4 != "" {
		m.GuestIPv4 = types.StringValue(*row.DesiredGuestIPv4)
	} else if m.GuestIPv4.IsNull() || m.GuestIPv4.IsUnknown() {
		m.GuestIPv4 = types.StringValue("")
	}
	if row.Encrypted != nil {
		m.Encrypted = types.BoolValue(*row.Encrypted)
	}
}
