package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure actionInvocationDataSource satisfies the datasource interfaces.
var _ datasource.DataSource = &actionInvocationDataSource{}
var _ datasource.DataSourceWithConfigure = &actionInvocationDataSource{}

// actionInvocationDataSource reads the result of a VVM action by calling
// GET /v1/action-invocations/{id}.
//
// HCL example:
//
//	data "vvm_action_invocation" "quarterly_dr" {
//	  id = "act-90c5c2"
//	}
//
//	output "dr_probe_ok" {
//	  value = data.vvm_action_invocation.quarterly_dr.summary.probe_ok
//	}
type actionInvocationDataSource struct {
	client *vvmClient
}

// actionInvocationModel is the Terraform state model for a vvm_action_invocation
// data source.
type actionInvocationModel struct {
	ID                      types.String `tfsdk:"id"`
	Status                  types.String `tfsdk:"status"`
	StartedAt               types.String `tfsdk:"started_at"`
	FinishedAt              types.String `tfsdk:"finished_at"`
	Actor                   types.String `tfsdk:"actor"`
	Summary                 types.Map    `tfsdk:"summary"`
	AffectedVMIDs           types.List   `tfsdk:"affected_vm_ids"`
	AffectedHostIDs         types.List   `tfsdk:"affected_host_ids"`
	ActivityTrackerEventIDs types.List   `tfsdk:"activity_tracker_event_ids"`
}

// apiActionInvocation is the wire shape of GET /v1/action-invocations/{id}.
type apiActionInvocation struct {
	ID                      string            `json:"id"`
	Status                  string            `json:"status"`
	StartedAt               string            `json:"started_at"`
	FinishedAt              *string           `json:"finished_at,omitempty"`
	Actor                   string            `json:"actor"`
	Summary                 map[string]string `json:"summary"`
	AffectedVMIDs           []string          `json:"affected_vm_ids"`
	AffectedHostIDs         []string          `json:"affected_host_ids"`
	ActivityTrackerEventIDs []string          `json:"activity_tracker_event_ids"`
}

func NewActionInvocationDataSource() datasource.DataSource {
	return &actionInvocationDataSource{}
}

func (d *actionInvocationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_action_invocation"
}

func (d *actionInvocationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the result of a VVM action invocation by ID. " +
			"The action must have been triggered elsewhere (e.g. via the VVM CLI or " +
			"another Terraform resource). This data source is read-only.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The action invocation ID (Idempotency-Key / request_id).",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Current status of the action: `pending`, `running`, `succeeded`, or `failed`.",
			},
			"started_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 timestamp when the action started.",
			},
			"finished_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 timestamp when the action finished. Empty if still in progress.",
			},
			"actor": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identity of the principal that triggered the action.",
			},
			"summary": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Key-value pairs summarising the action outcome (e.g. `probe_ok`).",
			},
			"affected_vm_ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "UUIDs of VMs that were affected by the action.",
			},
			"affected_host_ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "UUIDs of hosts that were affected by the action.",
			},
			"activity_tracker_event_ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "IBM Cloud Activity Tracker event IDs emitted during the action.",
			},
		},
	}
}

func (d *actionInvocationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
	d.client = client
}

// Read calls GET /v1/action-invocations/{id} and maps the response to state.
func (d *actionInvocationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data actionInvocationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := data.ID.ValueString()
	_, err := d.fetchActionInvocation(ctx, id, &data)
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			resp.Diagnostics.AddError(
				fmt.Sprintf("Action invocation %q not found (HTTP %d)", id, ae.Status),
				ae.Error(),
			)
			return
		}
		resp.Diagnostics.AddError("Error reading action invocation", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// fetchActionInvocation calls GET /v1/action-invocations/{id}, parses the
// JSON response, and maps computed fields into m. Exported for test access.
func (d *actionInvocationDataSource) fetchActionInvocation(ctx context.Context, id string, m *actionInvocationModel) (*apiActionInvocation, error) {
	raw, err := d.client.get(ctx, "/v1/action-invocations/"+id)
	if err != nil {
		return nil, err
	}
	var api apiActionInvocation
	if jsonErr := json.Unmarshal(raw, &api); jsonErr != nil {
		return nil, fmt.Errorf("parse action invocation response: %w", jsonErr)
	}
	apiToActionInvocationModel(&api, m)
	return &api, nil
}

// apiToActionInvocationModel maps an apiActionInvocation wire response into
// the Terraform state model m.
func apiToActionInvocationModel(api *apiActionInvocation, m *actionInvocationModel) {
	m.ID = types.StringValue(api.ID)
	m.Status = types.StringValue(api.Status)
	m.StartedAt = types.StringValue(api.StartedAt)
	if api.FinishedAt != nil {
		m.FinishedAt = types.StringValue(*api.FinishedAt)
	} else {
		m.FinishedAt = types.StringValue("")
	}
	m.Actor = types.StringValue(api.Actor)

	summaryVals := make(map[string]attr.Value, len(api.Summary))
	for k, v := range api.Summary {
		summaryVals[k] = types.StringValue(v)
	}
	m.Summary = types.MapValueMust(types.StringType, summaryVals)

	vmIDVals := make([]attr.Value, len(api.AffectedVMIDs))
	for i, v := range api.AffectedVMIDs {
		vmIDVals[i] = types.StringValue(v)
	}
	m.AffectedVMIDs = types.ListValueMust(types.StringType, vmIDVals)

	hostIDVals := make([]attr.Value, len(api.AffectedHostIDs))
	for i, v := range api.AffectedHostIDs {
		hostIDVals[i] = types.StringValue(v)
	}
	m.AffectedHostIDs = types.ListValueMust(types.StringType, hostIDVals)

	atEventIDVals := make([]attr.Value, len(api.ActivityTrackerEventIDs))
	for i, v := range api.ActivityTrackerEventIDs {
		atEventIDVals[i] = types.StringValue(v)
	}
	m.ActivityTrackerEventIDs = types.ListValueMust(types.StringType, atEventIDVals)
}
