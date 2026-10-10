package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure templateImportResource satisfies the resource.Resource interface.
var _ resource.Resource = &templateImportResource{}
var _ resource.ResourceWithImportState = &templateImportResource{}

// pollInterval is the delay between GET polls when waiting for a terminal state.
// Kept short for unit tests (tests swap the transport); real applies benefit from
// a longer interval to reduce API load, but the import can take 30–60 min so we
// don't want to sleep excessively.
const pollInterval = 10 * time.Second

// terminalStates are the import states after which polling should stop.
var terminalStates = map[string]bool{
	"REGISTERED": true,
	"FAILED":     true,
	"CANCELLED":  true,
}

// templateImportResource manages a VVM template import via the REST API.
//
// Spec §8.3 example:
//
//	resource "vvm_template_import" "ubuntu_2204" {
//	  cluster_id              = vvm_cluster.prod.id
//	  name                    = "ubuntu-22.04-baseline"
//	  source_url              = "imports/ubuntu-22.04.ova"
//	  datastore_id            = vvm_datastore.bulk.id
//	  target_kind             = "template"
//	}
type templateImportResource struct {
	client *vvmClient
}

// templateImportModel is the Terraform state model for a vvm_template_import.
// All user-provided fields are ForceNew: imports are immutable once started.
type templateImportModel struct {
	// Identity
	ID types.String `tfsdk:"id"`

	// Input (ForceNew — any change destroys and recreates)
	ClusterID        types.String `tfsdk:"cluster_id"`
	Name             types.String `tfsdk:"name"`
	SourceURL        types.String `tfsdk:"source_url"`
	SourceSHA256     types.String `tfsdk:"source_sha256"`
	SourceFormatHint types.String `tfsdk:"source_format_hint"`
	DatastoreID      types.String `tfsdk:"datastore_id"`
	TargetKind       types.String `tfsdk:"target_kind"`
	NetworkID        types.String `tfsdk:"network_id"`
	ProjectID        types.String `tfsdk:"project_id"`
	IdempotencyKey   types.String `tfsdk:"idempotency_key"`

	// Computed
	Status                types.String `tfsdk:"status"`
	TemplateID            types.String `tfsdk:"template_id"`
	VMID                  types.String `tfsdk:"vm_id"`
	GuestToolingReadiness types.String `tfsdk:"guest_tooling_readiness"`
}

// apiTemplateImport is the wire shape returned by GET /v1/templates/imports/{id}.
type apiTemplateImport struct {
	ID           string          `json:"id"`
	ClusterID    string          `json:"cluster_id"`
	State        string          `json:"state"`
	StateReason  string          `json:"state_reason,omitempty"`
	Source       json.RawMessage `json:"source"`
	Target       json.RawMessage `json:"target"`
	RegisterMeta json.RawMessage `json:"register_meta,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

// apiRegisterMeta is the shape of the register_meta field set on REGISTERED.
type apiRegisterMeta struct {
	TemplateID            string `json:"template_id,omitempty"`
	VMID                  string `json:"vm_id,omitempty"`
	GuestToolingReadiness string `json:"guest_tooling_readiness,omitempty"`
}

// postTemplateImportResponse is the 201/200 body shape.
type postTemplateImportResponse struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	IdempotencyKey string `json:"idempotency_key"`
}

func NewTemplateImportResource() resource.Resource {
	return &templateImportResource{}
}

func (r *templateImportResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_template_import"
}

func (r *templateImportResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a VVM template import (`/v1/templates/imports`). " +
			"A template import converts a source disk image (OVA, VMDK, VHDX, qcow2) from " +
			"IBM Cloud Object Storage into a VVM template or one-shot VM. " +
			"All input fields are immutable: any change triggers a ForceNew (destroy + recreate). " +
			"Create blocks until the import reaches a terminal state (REGISTERED or FAILED).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Unique identifier of the template import job (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cluster_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "UUID of the VVM cluster that will process the import.",
				PlanModifiers:       forceNew,
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name for the resulting template or VM. " +
					"Part of the idempotency key: changing `name` triggers a ForceNew.",
				PlanModifiers: forceNew,
			},
			"source_url": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "IBM Cloud Object Storage object key of the source image (e.g. `imports/ubuntu-22.04.ova`). The object must be readable by the VVM control plane.",
				PlanModifiers:       forceNew,
			},
			"source_sha256": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Hex-encoded SHA-256 of the source image. " +
					"Used as part of the derived idempotency key so a second `terraform apply` " +
					"with the same inputs is a no-op.",
				PlanModifiers: forceNew,
			},
			"source_format_hint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Format hint for the source image: `ova`, `ovf`, `vmdk`, `vhdx`, `qcow2`, or `raw`. When omitted the control plane probes the format.",
				PlanModifiers:       forceNew,
			},
			"datastore_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "UUID of the VVM datastore where the converted disk will be stored.",
				PlanModifiers:       forceNew,
			},
			"target_kind": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Target kind: `template` (default) registers a reusable template; " +
					"`vm` provisions a one-shot VM immediately via Materialize.",
				PlanModifiers: forceNew,
			},
			"network_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "UUID of the VVM network. Required when `target_kind = \"vm\"`; ignored for `template`.",
				PlanModifiers:       forceNew,
			},
			"project_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "UUID of the VVM project the template or VM will belong to.",
				PlanModifiers:       forceNew,
			},
			"idempotency_key": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Idempotency key sent in the `Idempotency-Key` header. " +
					"When not provided the control plane derives one from " +
					"`sha256(source_sha256 + name + cluster_id)`. " +
					"The server-acknowledged key is stored in state; a second `terraform apply` " +
					"with the same inputs finds the existing import via this key and returns immediately.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},

			// Computed outputs
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Final state of the import: one of `REGISTERED`, `FAILED`, or `CANCELLED`.",
			},
			"template_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "UUID of the registered template. Populated when `status = REGISTERED` and `target_kind = template`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vm_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "UUID of the provisioned VM. Populated when `status = REGISTERED` and `target_kind = vm`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"guest_tooling_readiness": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Guest tooling readiness of the resulting template: one of `paravirt-ready`, `paravirt-partial`, `unconverted`, or `unknown`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *templateImportResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *templateImportResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan templateImportModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, idemKey, err := buildImportBody(plan)
	if err != nil {
		resp.Diagnostics.AddError("Error building request body", err.Error())
		return
	}

	// POST /v1/templates/imports
	raw, err := r.client.post(ctx, "/v1/templates/imports", body, idemKey)
	if err != nil {
		resp.Diagnostics.AddError("Error creating template import", err.Error())
		return
	}

	var postResp postTemplateImportResponse
	if jsonErr := json.Unmarshal(raw, &postResp); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing template import response", jsonErr.Error())
		return
	}

	plan.ID = types.StringValue(postResp.ID)
	plan.IdempotencyKey = types.StringValue(postResp.IdempotencyKey)
	plan.Status = types.StringValue(postResp.State)

	// If the server returned a 200 (idempotency replay) and the import is already
	// terminal, skip polling and go straight to the read-from-state path.
	if terminalStates[postResp.State] {
		importRow, readErr := r.fetchAndPopulate(ctx, postResp.ID, &plan)
		if readErr != nil {
			resp.Diagnostics.AddError("Error reading terminal import", readErr.Error())
			return
		}
		if importRow.State == "FAILED" {
			resp.Diagnostics.AddError(
				"Template import failed",
				fmt.Sprintf("state=%s error_code=%s reason=%s", importRow.State, importRow.ErrorCode, importRow.StateReason),
			)
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// Poll until terminal.
	importRow, pollErr := r.pollUntilTerminal(ctx, postResp.ID, &plan)
	if pollErr != nil {
		// Save partial state so the import ID is preserved (allows `terraform import` recovery).
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("Error polling template import", pollErr.Error())
		return
	}

	if importRow.State == "FAILED" {
		// Save state so operators can inspect or taint; surface the structured error.
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Template import failed",
			fmt.Sprintf("state=%s error_code=%s reason=%s", importRow.State, importRow.ErrorCode, importRow.StateReason),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *templateImportResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state templateImportModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, err := r.fetchAndPopulate(ctx, state.ID.ValueString(), &state)
	if err != nil {
		if ae, ok := err.(*apiError); ok && ae.Status == 404 {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error reading template import", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update should never be called because all attributes are ForceNew.
// It is included to satisfy the resource.Resource interface; any call indicates
// a provider bug.
func (r *templateImportResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Update not supported",
		"vvm_template_import is immutable. All attribute changes trigger ForceNew.",
	)
}

func (r *templateImportResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state templateImportModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Best-effort cancel. The API returns 202 if the import is cancellable,
	// 409 if it has already reached a terminal state (REGISTERED / FAILED /
	// CANCELLED). Both outcomes are fine for a Terraform destroy — we just want
	// to signal the control plane to stop work if still running.
	err := r.client.delete(ctx, "/v1/templates/imports/"+state.ID.ValueString(), "", "")
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			// 404 → already gone; 409 → already terminal. Both are acceptable.
			if ae.Status == 404 || ae.Status == 409 {
				return
			}
		}
		resp.Diagnostics.AddError("Error cancelling template import", err.Error())
	}
}

func (r *templateImportResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw, err := r.client.get(ctx, "/v1/templates/imports/"+req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Error importing template import", err.Error())
		return
	}
	var api apiTemplateImport
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing template import", jsonErr.Error())
		return
	}
	var state templateImportModel
	importAPIToState(&api, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// buildImportBody constructs the POST /v1/templates/imports request body and
// returns the idempotency key that was used (caller stores it in state).
func buildImportBody(plan templateImportModel) (map[string]any, string, error) {
	targetKind := "template"
	if !plan.TargetKind.IsNull() && !plan.TargetKind.IsUnknown() && plan.TargetKind.ValueString() != "" {
		targetKind = plan.TargetKind.ValueString()
	}

	source := map[string]any{
		"kind":           "cos_object",
		"cos_object_key": plan.SourceURL.ValueString(),
	}
	if !plan.SourceSHA256.IsNull() && !plan.SourceSHA256.IsUnknown() {
		source["sha256"] = plan.SourceSHA256.ValueString()
	}
	if !plan.SourceFormatHint.IsNull() && !plan.SourceFormatHint.IsUnknown() {
		source["format_hint"] = plan.SourceFormatHint.ValueString()
	}

	target := map[string]any{
		"kind":         targetKind,
		"name":         plan.Name.ValueString(),
		"datastore_id": plan.DatastoreID.ValueString(),
	}
	if !plan.NetworkID.IsNull() && !plan.NetworkID.IsUnknown() && plan.NetworkID.ValueString() != "" {
		target["network_id"] = plan.NetworkID.ValueString()
	}
	if !plan.ProjectID.IsNull() && !plan.ProjectID.IsUnknown() && plan.ProjectID.ValueString() != "" {
		target["project_id"] = plan.ProjectID.ValueString()
	}

	body := map[string]any{
		"cluster_id": plan.ClusterID.ValueString(),
		"source":     source,
		"target":     target,
	}

	// Determine the idempotency key to use.
	idemKey := ""
	if !plan.IdempotencyKey.IsNull() && !plan.IdempotencyKey.IsUnknown() && plan.IdempotencyKey.ValueString() != "" {
		idemKey = plan.IdempotencyKey.ValueString()
	}

	return body, idemKey, nil
}

// pollUntilTerminal loops GET /v1/templates/imports/{id} until the import
// reaches a terminal state (REGISTERED, FAILED, or CANCELLED). It updates the
// plan model's Status field on each iteration so the final state is always
// reflected in what gets stored.
func (r *templateImportResource) pollUntilTerminal(ctx context.Context, id string, model *templateImportModel) (*apiTemplateImport, error) {
	for {
		api, err := r.fetchAndPopulate(ctx, id, model)
		if err != nil {
			return nil, err
		}
		if terminalStates[api.State] {
			return api, nil
		}
		// Respect context cancellation (e.g. Terraform timeout).
		select {
		case <-ctx.Done():
			return api, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// fetchAndPopulate GETs the import by ID, decodes the response, and writes all
// fields back into model. Returns the decoded apiTemplateImport for callers that
// need to inspect State / ErrorCode.
func (r *templateImportResource) fetchAndPopulate(ctx context.Context, id string, model *templateImportModel) (*apiTemplateImport, error) {
	raw, err := r.client.get(ctx, "/v1/templates/imports/"+id)
	if err != nil {
		return nil, err
	}
	var api apiTemplateImport
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		return nil, fmt.Errorf("parse template import: %w", jsonErr)
	}
	importAPIToState(&api, model)
	return &api, nil
}

// importAPIToState copies API fields into the Terraform state model.
// Input fields (cluster_id, name, source_url, etc.) are NOT overwritten from
// the API response — the API returns them as opaque jsonb and we don't want to
// drift from what the operator declared in HCL. Only computed outputs are
// updated here.
func importAPIToState(api *apiTemplateImport, m *templateImportModel) {
	m.ID = types.StringValue(api.ID)
	m.Status = types.StringValue(api.State)

	// Parse register_meta to extract template_id / vm_id / guest_tooling_readiness.
	if len(api.RegisterMeta) > 0 {
		var rm apiRegisterMeta
		if err := json.Unmarshal(api.RegisterMeta, &rm); err == nil {
			if rm.TemplateID != "" {
				m.TemplateID = types.StringValue(rm.TemplateID)
			}
			if rm.VMID != "" {
				m.VMID = types.StringValue(rm.VMID)
			}
			if rm.GuestToolingReadiness != "" {
				m.GuestToolingReadiness = types.StringValue(rm.GuestToolingReadiness)
			}
		}
	}
}
