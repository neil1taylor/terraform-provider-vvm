package provider

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &networkResource{}

// networkCreateTimeout / networkDeleteTimeout — a guest network is OVN and VPC
// work on each host, not a boot, so it settles in minutes.
const (
	networkCreateTimeout = "20m"
	networkDeleteTimeout = "20m"
)

// networkResource manages a VVM guest network (`/v1/networks`).
type networkResource struct {
	client *vvmClient
}

type networkModel struct {
	ID           types.String `tfsdk:"id"`
	DesiredState types.String `tfsdk:"desired_state"`
	ActualState  types.String `tfsdk:"actual_state"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
	JobID        types.String `tfsdk:"job_id"`

	ClusterID          types.String `tfsdk:"cluster_id"`
	Name               types.String `tfsdk:"name"`
	VlanID             types.Int64  `tfsdk:"vlan_id"`
	VpcSubnetID        types.String `tfsdk:"vpc_subnet_id"`
	Cidr               types.String `tfsdk:"cidr"`
	IpamRangeStart     types.String `tfsdk:"ipam_range_start"`
	IpamRangeEnd       types.String `tfsdk:"ipam_range_end"`
	OvnLogicalSwitchID types.String `tfsdk:"ovn_logical_switch_id"`

	CreateTimeout types.String `tfsdk:"create_timeout"`
	DeleteTimeout types.String `tfsdk:"delete_timeout"`
}

func NewNetworkResource() resource.Resource { return &networkResource{} }

func (r *networkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_network"
}

func (r *networkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A VVM guest network (`/v1/networks`): a VLAN on a cluster, the subnet guests " +
			"take their addresses from, and the IPAM range VVM hands out of it.\n\n" +
			"`terraform apply` returns when the control plane reports the network `Active` — the point at " +
			"which a VM may be put on it. Every attribute is immutable: a change destroys and recreates.\n\n" +
			"⛔ A guest CIDR that contains a host's own address, or the address a file share is mounted from, " +
			"is refused by the control plane (it would take the host off the network). Pick a range the " +
			"cluster does not already use.",
		Attributes: mergeAttributes(
			lifecycleAttributes("network"),
			timeoutAttributes(networkCreateTimeout, networkDeleteTimeout),
			map[string]schema.Attribute{
				"cluster_id": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "UUID of the cluster this network belongs to.",
					PlanModifiers:       forceNew(),
				},
				"name": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "Name of the network.",
					PlanModifiers:       forceNew(),
				},
				"vlan_id": schema.Int64Attribute{
					Required: true,
					MarkdownDescription: "VLAN id. Unique within the cluster: a second network with the same " +
						"`vlan_id` on the same cluster is refused.",
					PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"vpc_subnet_id": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Id of the IBM Cloud VPC subnet this network sits on — reference an " +
						"`ibm_is_subnet` built by the IBM provider rather than managing it here.",
					PlanModifiers: forceNew(),
				},
				"cidr": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "The guest subnet in CIDR form, e.g. `192.168.50.0/24`.",
					PlanModifiers:       forceNew(),
				},
				"ipam_range_start": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "First address VVM may assign to a guest. Must be inside `cidr`.",
					PlanModifiers:       forceNew(),
				},
				"ipam_range_end": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "Last address VVM may assign to a guest. Must be inside `cidr`.",
					PlanModifiers:       forceNew(),
				},
				"ovn_logical_switch_id": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "Existing OVN logical switch to adopt. Omit to let VVM make one.",
					PlanModifiers:       forceNew(),
				},
			},
		),
	}
}

func (r *networkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req, resp)
}

// buildNetworkBody is the POST /v1/networks body.
//
// An omitted optional field is LEFT OUT rather than sent empty: an empty
// ovn_logical_switch_id is not the same request as no ovn_logical_switch_id.
func buildNetworkBody(plan networkModel) map[string]any {
	body := map[string]any{
		"cluster_id":       plan.ClusterID.ValueString(),
		"name":             plan.Name.ValueString(),
		"vlan_id":          plan.VlanID.ValueInt64(),
		"vpc_subnet_id":    plan.VpcSubnetID.ValueString(),
		"cidr":             plan.Cidr.ValueString(),
		"ipam_range_start": plan.IpamRangeStart.ValueString(),
		"ipam_range_end":   plan.IpamRangeEnd.ValueString(),
	}
	if sw := optionalString(plan.OvnLogicalSwitchID); sw != "" {
		body["ovn_logical_switch_id"] = sw
	}
	return body
}

func (r *networkResource) waitFor(id, jobID string) stateWait {
	return stateWait{
		client: r.client,
		path:   "/v1/networks/" + id,
		jobID:  jobID,
		kind:   "network",
		target: "Active",
		failed: []string{"Failed"},
	}
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(plan.CreateTimeout), networkCreateTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid create_timeout", err.Error())
		return
	}

	body := buildNetworkBody(plan)
	key, err := idempotencyKeyForBody("vvm_network", body)
	if err != nil {
		resp.Diagnostics.AddError("Error creating network", err.Error())
		return
	}
	raw, err := r.client.post(ctx, "/v1/networks", body, key)
	if err != nil {
		resp.Diagnostics.AddError("Error creating network", err.Error())
		return
	}
	var ack writeAck
	if jsonErr := json.Unmarshal(raw, &ack); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing the network create response", jsonErr.Error())
		return
	}
	plan.ID = types.StringValue(ack.ID)
	plan.JobID = types.StringValue(ack.JobID)

	// 🚨 THE ID IS SAVED EVEN WHEN THE WAIT FAILS. Without it Terraform forgets a
	// network the control plane is building, and the next apply makes a second
	// one while the first keeps its VLAN — which the cluster then refuses.
	row, _, waitErr := r.waitFor(ack.ID, ack.JobID).wait(ctx, timeout)
	networkRowToState(row, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if waitErr != nil {
		resp.Diagnostics.AddError("Network did not reach Active", waitErr.Error())
	}
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	row, _, err := readRow(ctx, r.client, "/v1/networks/"+state.ID.ValueString())
	if isNotFound(err) {
		// Gone outside Terraform. Removing it from state is what makes the next
		// plan say so instead of failing on every operation.
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error reading network", err.Error())
		return
	}
	networkRowToState(row, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *networkResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Update not supported",
		"Every attribute of vvm_network is immutable, so a change destroys and recreates. "+
			"Reaching this is a provider bug.")
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(state.DeleteTimeout), networkDeleteTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid delete_timeout", err.Error())
		return
	}
	if err := r.waitFor(state.ID.ValueString(), optionalString(state.JobID)).
		deleteAndWait(ctx, timeout); err != nil {
		resp.Diagnostics.AddError("Error deleting network", err.Error())
	}
}

// networkRowToState copies the observed fields onto the model. The declared
// fields are left alone: they are what the operator asked for, and the control
// plane stores them as given.
func networkRowToState(row asyncRow, m *networkModel) {
	applyLifecycle(row, &m.ID, &m.DesiredState, &m.ActualState, &m.UpdatedAt, &m.JobID)
}
