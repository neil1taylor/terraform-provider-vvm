package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure backupPolicyResource satisfies the resource.Resource interface.
var _ resource.Resource = &backupPolicyResource{}
var _ resource.ResourceWithImportState = &backupPolicyResource{}

// backupPolicyResource manages a VVM backup_policy via the REST API.
//
// Spec §8.4 example:
//
//	resource "vvm_backup_policy" "weekly_full_daily_inc" {
//	  name                        = "weekly_full_daily_incremental"
//	  schedule_cron               = "0 2 * * *"
//	  retain_days                 = 35
//	  synthetic_full_cadence_hours = 168
//	  skew_policy                 = "flag-and-keep"
//	  cos_bucket                  = ibm_cos_bucket.backup.bucket_name
//	  cos_region                  = "eu-gb"
//	}
type backupPolicyResource struct {
	client *vvmClient
}

// backupPolicyModel mirrors the API schema for POST/PUT /v1/backup-policies.
// UpdatedAt is used as the ETag / If-Match token on PUT + DELETE.
type backupPolicyModel struct {
	ID                        types.String `tfsdk:"id"`
	Name                      types.String `tfsdk:"name"`
	ScheduleCron              types.String `tfsdk:"schedule_cron"`
	RetainDays                types.Int64  `tfsdk:"retain_days"`
	SyntheticFullCadenceHours types.Int64  `tfsdk:"synthetic_full_cadence_hours"`
	SyntheticFullCadenceDays  types.Int64  `tfsdk:"synthetic_full_cadence_days"`
	SkewPolicy                types.String `tfsdk:"skew_policy"`
	COSBucket                 types.String `tfsdk:"cos_bucket"`
	COSRegion                 types.String `tfsdk:"cos_region"`
	UpdatedAt                 types.String `tfsdk:"updated_at"`
}

// apiBackupPolicy is the wire shape returned by the VVM API.
type apiBackupPolicy struct {
	ID                        string `json:"id"`
	Name                      string `json:"name"`
	ScheduleCron              string `json:"schedule_cron"`
	RetainDays                int64  `json:"retain_days"`
	SyntheticFullCadenceHours int64  `json:"synthetic_full_cadence_hours"`
	// SyntheticFullCadenceDays is absent when the hours are not a whole number
	// of days, so it is a pointer: absent and 0 are different.
	SyntheticFullCadenceDays *int64 `json:"synthetic_full_cadence_days"`
	SkewPolicy               string `json:"skew_policy"`
	COSBucket                string `json:"cos_bucket"`
	COSRegion                string `json:"cos_region"`
	UpdatedAt                string `json:"updated_at"`
}

func NewBackupPolicyResource() resource.Resource {
	return &backupPolicyResource{}
}

func (r *backupPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_backup_policy"
}

func (r *backupPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a VVM backup policy (`/v1/backup-policies`). " +
			"A backup policy defines the schedule, retention, COS target, and skew " +
			"behaviour for one or more protection groups.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Unique identifier of the backup policy (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable name for the backup policy. Unique within the VVM instance.",
			},
			"schedule_cron": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Cron expression controlling when the backup scheduler fires this policy (UTC).",
			},
			"retain_days": schema.Int64Attribute{
				Required: true,
				// ⚠️ This used to read "Number of days to retain recovery sets
				// before soft-deleting them", which describes a pruning
				// schedule. It is a WORM lock. Say so here, because this
				// string is what a Terraform user reads before choosing a
				// number.
				MarkdownDescription: "How long every object this policy writes stays **immutable**, in days. " +
					"This is not a pruning schedule: the value is written onto each object as an " +
					"object-lock retain-until date against a COMPLIANCE-mode bucket, so those objects " +
					"**cannot be deleted by anyone** — including with account root credentials, and " +
					"including by destroying this policy, the protection group or the VM — until the " +
					"date passes. COMPLIANCE mode has no early release, so the retention **cannot be " +
					"shortened or cancelled afterwards**; lowering this value affects only objects " +
					"written after the change. Budget accordingly: the storage is billable for the " +
					"whole period. Seven-year regulatory retention (SEC 17a-4, SOX) is `2555`.",
			},
			"synthetic_full_cadence_hours": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How old a chain's base may get, in hours, before the next scheduled " +
					"run makes a synthetic full. At least `1`. If neither this nor " +
					"`synthetic_full_cadence_days` is set, the API uses `168` (7 days).",
			},
			"synthetic_full_cadence_days": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The same cadence in whole days, kept for configurations written " +
					"before hours: taken as that many times 24 hours. If both are set they must agree. " +
					"Read back as null when the cadence is not a whole number of days.",
			},
			"skew_policy": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What the scheduler does when group-coordinated freeze skew exceeds " +
					"the SLO. One of `flag-and-keep` or `strict-fail`.",
			},
			"cos_bucket": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "IBM Cloud Object Storage bucket name where recovery objects are written.",
			},
			"cos_region": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "IBM Cloud region of the COS bucket (e.g. `eu-gb`).",
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC3339Nano timestamp of the last update; used as the ETag / If-Match token.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
	// Suppress "retain_days changed" noise for the plan modifier.
	_ = int64planmodifier.UseStateForUnknown()
}

func (r *backupPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *backupPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan backupPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildBackupPolicyBody(plan)

	raw, err := r.client.post(ctx, "/v1/backup-policies", body, idempotencyKey("create-backup-policy", plan.Name.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Error creating backup policy", err.Error())
		return
	}

	var api apiBackupPolicy
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing backup policy response", jsonErr.Error())
		return
	}
	apiToState(&api, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state backupPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	raw, err := r.client.get(ctx, "/v1/backup-policies/"+state.ID.ValueString())
	if err != nil {
		if ae, ok := err.(*apiError); ok && ae.Status == 404 {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Error reading backup policy", err.Error())
		return
	}

	var api apiBackupPolicy
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing backup policy", jsonErr.Error())
		return
	}
	apiToState(&api, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *backupPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state backupPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := buildBackupPolicyBody(plan)

	raw, err := r.client.put(ctx,
		"/v1/backup-policies/"+state.ID.ValueString(),
		body,
		state.UpdatedAt.ValueString(),
		idempotencyKey("update-backup-policy", state.ID.ValueString()),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error updating backup policy", err.Error())
		return
	}

	var api apiBackupPolicy
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing backup policy update response", jsonErr.Error())
		return
	}
	apiToState(&api, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state backupPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.delete(ctx,
		"/v1/backup-policies/"+state.ID.ValueString(),
		state.UpdatedAt.ValueString(),
		idempotencyKey("delete-backup-policy", state.ID.ValueString()),
	)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting backup policy", err.Error())
	}
}

func (r *backupPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw, err := r.client.get(ctx, "/v1/backup-policies/"+req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Error importing backup policy", err.Error())
		return
	}
	var api apiBackupPolicy
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing backup policy", jsonErr.Error())
		return
	}
	var state backupPolicyModel
	apiToState(&api, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// buildBackupPolicyBody is the POST/PUT body for plan. Of the two cadence
// fields it sends only those the configuration sets: a computed one not in the
// configuration is unknown in the plan, and sending its old value alongside a
// changed other field would be two fields that disagree, which the API
// refuses. Neither set lets the API apply its default.
func buildBackupPolicyBody(plan backupPolicyModel) map[string]any {
	body := map[string]any{
		"name":          plan.Name.ValueString(),
		"schedule_cron": plan.ScheduleCron.ValueString(),
		"retain_days":   plan.RetainDays.ValueInt64(),
		"skew_policy":   plan.SkewPolicy.ValueString(),
		"cos_bucket":    plan.COSBucket.ValueString(),
		"cos_region":    plan.COSRegion.ValueString(),
	}
	if v := plan.SyntheticFullCadenceHours; !v.IsNull() && !v.IsUnknown() {
		body["synthetic_full_cadence_hours"] = v.ValueInt64()
	}
	if v := plan.SyntheticFullCadenceDays; !v.IsNull() && !v.IsUnknown() {
		body["synthetic_full_cadence_days"] = v.ValueInt64()
	}
	return body
}

// apiToState copies API fields into the Terraform state model.
func apiToState(api *apiBackupPolicy, m *backupPolicyModel) {
	m.ID = types.StringValue(api.ID)
	m.Name = types.StringValue(api.Name)
	m.ScheduleCron = types.StringValue(api.ScheduleCron)
	m.RetainDays = types.Int64Value(api.RetainDays)
	m.SyntheticFullCadenceHours = types.Int64Value(api.SyntheticFullCadenceHours)
	m.SyntheticFullCadenceDays = types.Int64PointerValue(api.SyntheticFullCadenceDays)
	m.SkewPolicy = types.StringValue(api.SkewPolicy)
	m.COSBucket = types.StringValue(api.COSBucket)
	m.COSRegion = types.StringValue(api.COSRegion)
	m.UpdatedAt = types.StringValue(api.UpdatedAt)
}

// idempotencyKey derives a stable Idempotency-Key for CRUD operations.
// The key is truncated to 32 hex characters to stay within the VVM header limit.
func idempotencyKey(action, id string) string {
	// Simple deterministic key: action + id; no crypto needed here —
	// TF plan cycles guarantee a single attempt per apply.
	raw := action + ":" + id
	if len(raw) > 128 {
		raw = raw[:128]
	}
	// Replace characters outside [A-Za-z0-9_-] with underscores.
	out := make([]byte, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			out[i] = c
		} else {
			out[i] = '_'
		}
	}
	return string(out)
}
