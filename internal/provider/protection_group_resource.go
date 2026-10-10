package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure protectionGroupResource satisfies the resource.Resource interface.
var _ resource.Resource = &protectionGroupResource{}
var _ resource.ResourceWithImportState = &protectionGroupResource{}

// protectionGroupResource manages a VVM protection_group via the REST API.
//
// Spec §8.4 example:
//
//	resource "vvm_protection_group" "tier1_vms" {
//	  name               = "tier1-vms"
//	  policy_id          = vvm_backup_policy.weekly_full_daily_inc.id
//	  consistency_model  = "crash"
//	  selector_kind      = "explicit"
//	  selector_value     = jsonencode({ vm_ids = [ibm_is_instance.app1.id] })
//	}
type protectionGroupResource struct {
	client *vvmClient
}

// protectionGroupModel is the Terraform state model for a vvm_protection_group.
// SelectorValue is stored as a JSON string (users typically use jsonencode()).
// SkewPolicyOverride is optional; when null the policy-level skew_policy applies.
type protectionGroupModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	PolicyID           types.String `tfsdk:"policy_id"`
	ConsistencyModel   types.String `tfsdk:"consistency_model"`
	SelectorKind       types.String `tfsdk:"selector_kind"`
	SelectorValue      types.String `tfsdk:"selector_value"`
	SkewPolicyOverride types.String `tfsdk:"skew_policy_override"`
	UpdatedAt          types.String `tfsdk:"updated_at"`
}

// apiProtectionGroup is the wire shape returned by the VVM API.
type apiProtectionGroup struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	PolicyID           string          `json:"policy_id"`
	ConsistencyModel   string          `json:"consistency_model"`
	SelectorKind       string          `json:"selector_kind"`
	SelectorValue      json.RawMessage `json:"selector_value"`
	SkewPolicyOverride *string         `json:"skew_policy_override,omitempty"`
	UpdatedAt          string          `json:"updated_at"`
}

func NewProtectionGroupResource() resource.Resource {
	return &protectionGroupResource{}
}

func (r *protectionGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_protection_group"
}

func (r *protectionGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a VVM protection group (`/v1/protection-groups`). " +
			"A protection group binds a set of VMs to a backup policy and declares the " +
			"consistency model and selector that controls which VMs are included.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Unique identifier of the protection group (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable name for the protection group. Unique within the VVM instance.",
			},
			"policy_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "UUID of the `vvm_backup_policy` that governs this group.",
			},
			"consistency_model": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Consistency model for backups in this group. " +
					"One of `crash` (crash-consistent) or `app` (application-consistent via pre/post hooks).",
			},
			"selector_kind": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Kind of VM selector. One of `explicit`, `tag`, or `project`. " +
					"Controls the shape of `selector_value`.",
			},
			"selector_value": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON payload for the selector. Shape depends on `selector_kind`:\n\n" +
					"- `explicit`: `{\"vm_ids\": [\"<uuid>\", ...]}`\n" +
					"- `tag`:      `{\"tag\": \"<string>\"}`\n" +
					"- `project`:  `{\"project_id\": \"<uuid>\"}`\n\n" +
					"Use `jsonencode()` in HCL to build the value.",
			},
			"skew_policy_override": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Per-group override for skew handling. Null inherits from the " +
					"policy `skew_policy`. One of `flag-and-keep` or `strict-fail`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC3339Nano timestamp of the last update; used as the ETag / If-Match token.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *protectionGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*vvmClient)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *vvmClient, got %T", req.ProviderData),
		)
		return
	}
	r.client = client
}

func (r *protectionGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan protectionGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildGroupBody(plan)
	if err != nil {
		resp.Diagnostics.AddError("Error building request body", err.Error())
		return
	}

	raw, err := r.client.post(ctx, "/v1/protection-groups", body, idempotencyKey("create-protection-group", plan.Name.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Error creating protection group", err.Error())
		return
	}

	var api apiProtectionGroup
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing protection group response", jsonErr.Error())
		return
	}
	groupAPIToState(&api, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *protectionGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state protectionGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	raw, err := r.client.get(ctx, "/v1/protection-groups/"+state.ID.ValueString())
	if err != nil {
		if ae, ok := err.(*apiError); ok && ae.Status == 404 {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error reading protection group", err.Error())
		return
	}

	var api apiProtectionGroup
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing protection group", jsonErr.Error())
		return
	}
	groupAPIToState(&api, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *protectionGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state protectionGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildGroupBody(plan)
	if err != nil {
		resp.Diagnostics.AddError("Error building request body", err.Error())
		return
	}

	raw, err := r.client.put(ctx,
		"/v1/protection-groups/"+state.ID.ValueString(),
		body,
		state.UpdatedAt.ValueString(),
		idempotencyKey("update-protection-group", state.ID.ValueString()),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error updating protection group", err.Error())
		return
	}

	var api apiProtectionGroup
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing protection group update response", jsonErr.Error())
		return
	}
	groupAPIToState(&api, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *protectionGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state protectionGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.delete(ctx,
		"/v1/protection-groups/"+state.ID.ValueString(),
		state.UpdatedAt.ValueString(),
		idempotencyKey("delete-protection-group", state.ID.ValueString()),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting protection group", err.Error())
	}
}

func (r *protectionGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw, err := r.client.get(ctx, "/v1/protection-groups/"+req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Error importing protection group", err.Error())
		return
	}
	var api apiProtectionGroup
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing protection group", jsonErr.Error())
		return
	}
	var state protectionGroupModel
	groupAPIToState(&api, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// buildGroupBody constructs the JSON request body from the plan model.
// SelectorValue is a JSON string in the plan; we embed it as raw JSON in the body.
func buildGroupBody(plan protectionGroupModel) (map[string]any, error) {
	// Validate that SelectorValue is valid JSON before sending.
	sv := plan.SelectorValue.ValueString()
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(sv), &raw); err != nil {
		return nil, fmt.Errorf("selector_value must be valid JSON: %w", err)
	}

	body := map[string]any{
		"name":              plan.Name.ValueString(),
		"policy_id":         plan.PolicyID.ValueString(),
		"consistency_model": plan.ConsistencyModel.ValueString(),
		"selector_kind":     plan.SelectorKind.ValueString(),
		"selector_value":    raw,
	}
	if !plan.SkewPolicyOverride.IsNull() && !plan.SkewPolicyOverride.IsUnknown() {
		v := plan.SkewPolicyOverride.ValueString()
		body["skew_policy_override"] = v
	}
	return body, nil
}

// groupAPIToState copies API fields into the Terraform state model.
func groupAPIToState(api *apiProtectionGroup, m *protectionGroupModel) {
	m.ID = types.StringValue(api.ID)
	m.Name = types.StringValue(api.Name)
	m.PolicyID = types.StringValue(api.PolicyID)
	m.ConsistencyModel = types.StringValue(api.ConsistencyModel)
	m.SelectorKind = types.StringValue(api.SelectorKind)
	// Normalise selector_value to compact JSON for consistent state diffs.
	if len(api.SelectorValue) > 0 {
		compact, err := json.Marshal(json.RawMessage(api.SelectorValue))
		if err == nil {
			m.SelectorValue = types.StringValue(string(compact))
		} else {
			m.SelectorValue = types.StringValue(string(api.SelectorValue))
		}
	}
	if api.SkewPolicyOverride != nil {
		m.SkewPolicyOverride = types.StringValue(*api.SkewPolicyOverride)
	} else {
		m.SkewPolicyOverride = types.StringNull()
	}
	m.UpdatedAt = types.StringValue(api.UpdatedAt)
}
