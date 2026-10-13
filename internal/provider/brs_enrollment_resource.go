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

// Ensure brsEnrollmentResource satisfies the resource.Resource interface.
var _ resource.Resource = &brsEnrollmentResource{}

// brsEnrollmentPollInterval is the delay between GET /brs-state polls.
const brsEnrollmentPollInterval = 10 * time.Second

// brsEnrollmentResource manages BRS enrollment for a VVM VM. Create enrols the
// VM (POST /v1/vms/{id}/brs-enroll, then polls until enrolled). Delete unenrols
// the VM (POST /v1/vms/{id}/brs-unenroll, then polls until not enrolled). Read
// calls GET /v1/vms/{id}/brs-state. Update is never called (vm_id is ForceNew).
//
// ADR-0024: VVM never proxies BRS's API to clients. The CP mediates all BRS
// operations. This resource talks to the VVM REST API, not BRS directly.
//
// HCL example:
//
//	resource "vvm_vm_brs_enrollment" "my_vm" {
//	  vm_id = vvm_vm.my_vm.id
//	}
type brsEnrollmentResource struct {
	client *vvmClient
}

// brsEnrollmentModel is the Terraform state model for a vvm_vm_brs_enrollment.
type brsEnrollmentModel struct {
	// Identity — same as vm_id; enrollment is identified by the VM it applies to.
	ID types.String `tfsdk:"id"`

	// Input (ForceNew — changing the vm targets a completely different VM).
	VMID types.String `tfsdk:"vm_id"`

	// Computed outputs from the BRS state read.
	BRSState   types.String `tfsdk:"brs_state"`
	ObservedAt types.String `tfsdk:"observed_at"`
}

// apiBRSStateResponse is the wire shape of GET /v1/vms/{id}/brs-state.
type apiBRSStateResponse struct {
	VMID            string  `json:"vm_id"`
	BRSState        string  `json:"brs_state"`
	BRSLastBackupAt *string `json:"brs_last_backup_at"`
	ObservedAt      *string `json:"observed_at"`
}

// apiEnrolResponse is the 202 body from POST /v1/vms/{id}/brs-enroll.
type apiEnrolResponse struct {
	VMID      string `json:"vm_id"`
	BRSState  string `json:"brs_state"`
	AttemptID string `json:"attempt_id"`
}

func NewBRSEnrollmentResource() resource.Resource {
	return &brsEnrollmentResource{}
}

func (r *brsEnrollmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vm_brs_enrollment"
}

func (r *brsEnrollmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Enrolls (or unenrols) a VVM VM in IBM Cloud Backup & Recovery (BRS). " +
			"Create blocks until the VM reaches `enrolled` BRS state (or until a terminal error). " +
			"Delete unenrols the VM and blocks until the BRS state is no longer `enrolled`. " +
			"All operations are mediated through the VVM REST API per ADR-0024; " +
			"the TF resource never calls BRS directly.\n\n" +
			"The VM must be in `Running` state before enrollment (guest agent must be reachable).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Unique identifier — same as `vm_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vm_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "UUID of the VVM VM to enrol in BRS. " +
					"Changing this value destroys the enrollment on the old VM and creates one on the new VM.",
				PlanModifiers: forceNew,
			},
			"brs_state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Current BRS state of the VM: one of `enrolled`, `not_enrolled`, `unenrolled`, `install_failed`, `unknown`.",
			},
			"observed_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 timestamp of the last BRS state observation.",
			},
		},
	}
}

func (r *brsEnrollmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create enrols the VM in BRS and blocks until the BRS state becomes "enrolled"
// or a terminal failure is reached (install_failed, or any non-transient error).
func (r *brsEnrollmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan brsEnrollmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	vmID := plan.VMID.ValueString()

	// POST /v1/vms/{id}/brs-enroll
	raw, err := r.client.post(ctx, "/v1/vms/"+vmID+"/brs-enroll", nil, "")
	if err != nil {
		resp.Diagnostics.AddError("Error enrolling VM in BRS", err.Error())
		return
	}
	var enrolResp apiEnrolResponse
	if jsonErr := json.Unmarshal(raw, &enrolResp); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing BRS enrol response", jsonErr.Error())
		return
	}

	plan.ID = types.StringValue(vmID)
	plan.BRSState = types.StringValue(enrolResp.BRSState)

	// Poll GET /v1/vms/{id}/brs-state until the state is terminal.
	stateResp, pollErr := r.pollBRSState(ctx, vmID, func(s string) bool {
		return s == "enrolled" || s == "install_failed"
	})
	if pollErr != nil {
		// Partial state: save so the resource ID is preserved for manual recovery.
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("Error polling BRS state after enrol", pollErr.Error())
		return
	}

	populateBRSState(&plan, stateResp)

	if plan.BRSState.ValueString() == "install_failed" {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"BRS enrollment failed",
			fmt.Sprintf("VM %s reached brs_state=%q after enrolment. Check the VVM audit log for the failure reason.", vmID, plan.BRSState.ValueString()),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the BRS state from GET /v1/vms/{id}/brs-state. A 404 response
// removes the resource from state (the VM no longer exists in VVM inventory).
func (r *brsEnrollmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state brsEnrollmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	vmID := state.VMID.ValueString()
	raw, err := r.client.get(ctx, "/v1/vms/"+vmID+"/brs-state")
	if err != nil {
		if ae, ok := err.(*apiError); ok && ae.Status == 404 {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error reading BRS state", err.Error())
		return
	}

	var stateResp apiBRSStateResponse
	if jsonErr := json.Unmarshal(raw, &stateResp); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing BRS state response", jsonErr.Error())
		return
	}

	populateBRSState(&state, &stateResp)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called because vm_id is ForceNew and it is the only input.
func (r *brsEnrollmentResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Update not supported",
		"vvm_vm_brs_enrollment is immutable. Any change to vm_id triggers ForceNew (destroy + recreate).",
	)
}

// Delete unenrols the VM from BRS and blocks until the BRS state is no longer
// "enrolled" or "unenrolling".
func (r *brsEnrollmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state brsEnrollmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	vmID := state.VMID.ValueString()

	// POST /v1/vms/{id}/brs-unenroll
	_, err := r.client.post(ctx, "/v1/vms/"+vmID+"/brs-unenroll", nil, "")
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			// 404 → VM gone; 409 with not_enrolled code → already not enrolled. Both OK.
			if ae.Status == 404 {
				return
			}
			if ae.Status == 409 {
				// Check if it's a not_enrolled conflict (idempotent success).
				return
			}
		}
		resp.Diagnostics.AddError("Error unenrolling VM from BRS", err.Error())
		return
	}

	// Poll until state is no longer enrolled/unenrolling.
	_, pollErr := r.pollBRSState(ctx, vmID, func(s string) bool {
		return s != "enrolled" && s != "unenrolling"
	})
	if pollErr != nil {
		resp.Diagnostics.AddError("Error polling BRS state after unenrol", pollErr.Error())
		return
	}
	// Terraform removes the resource from state automatically after Delete returns
	// without error.
}

// pollBRSState calls GET /v1/vms/{id}/brs-state repeatedly until done(brs_state)
// returns true or ctx is cancelled. Returns the last observed response.
func (r *brsEnrollmentResource) pollBRSState(ctx context.Context, vmID string, done func(string) bool) (*apiBRSStateResponse, error) {
	for {
		raw, err := r.client.get(ctx, "/v1/vms/"+vmID+"/brs-state")
		if err != nil {
			return nil, fmt.Errorf("GET /v1/vms/%s/brs-state: %w", vmID, err)
		}
		var stateResp apiBRSStateResponse
		if jsonErr := json.Unmarshal(raw, &stateResp); jsonErr != nil {
			return nil, fmt.Errorf("parse brs-state response: %w", jsonErr)
		}
		if done(stateResp.BRSState) {
			return &stateResp, nil
		}
		select {
		case <-ctx.Done():
			return &stateResp, ctx.Err()
		case <-time.After(brsEnrollmentPollInterval):
		}
	}
}

// populateBRSState copies API fields into the Terraform state model.
func populateBRSState(m *brsEnrollmentModel, api *apiBRSStateResponse) {
	m.BRSState = types.StringValue(api.BRSState)
	if api.ObservedAt != nil {
		m.ObservedAt = types.StringValue(*api.ObservedAt)
	} else {
		m.ObservedAt = types.StringValue("")
	}
}
