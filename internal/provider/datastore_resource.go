package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = &datastoreResource{}

// A provisioned datastore is a VPC File Share made, a mount target made, and
// then a mount on every Ready host before it reads Active.
const (
	datastoreCreateTimeout = "45m"
	datastoreDeleteTimeout = "30m"
)

// datastoreResource manages a VVM datastore (`/v1/datastores`).
type datastoreResource struct {
	client *vvmClient
}

type datastoreModel struct {
	ID           types.String `tfsdk:"id"`
	DesiredState types.String `tfsdk:"desired_state"`
	ActualState  types.String `tfsdk:"actual_state"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
	JobID        types.String `tfsdk:"job_id"`

	ClusterID         types.String `tfsdk:"cluster_id"`
	Name              types.String `tfsdk:"name"`
	MountPath         types.String `tfsdk:"mount_path"`
	FileShareID       types.String `tfsdk:"file_share_id"`
	SizeGiB           types.Int64  `tfsdk:"size_gib"`
	Zone              types.String `tfsdk:"zone"`
	StorageClass      types.String `tfsdk:"storage_class"`
	EncryptionKpKeyID types.String `tfsdk:"encryption_kp_key_id"`
	IOPS              types.Int64  `tfsdk:"iops"`

	CapacityGb types.Int64 `tfsdk:"capacity_gb"`

	CreateTimeout types.String `tfsdk:"create_timeout"`
	DeleteTimeout types.String `tfsdk:"delete_timeout"`
}

// datastoreRow is a datastore read, with the one field beyond the shared
// lifecycle set that this resource reports.
type datastoreRow struct {
	asyncRow
	CapacityGb int64 `json:"capacity_gb"`
}

func NewDatastoreResource() resource.Resource { return &datastoreResource{} }

func (r *datastoreResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_datastore"
}

func (r *datastoreResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A VVM datastore (`/v1/datastores`): the shared storage a cluster's VMs keep " +
			"their disks on.\n\n" +
			"Two modes, and exactly one of them:\n\n" +
			"  * **provision** — set `size_gib`. VVM makes the VPC File Share and its mount target.\n" +
			"  * **adopt** — set `file_share_id`. You already have a share; VVM mounts it.\n\n" +
			"`terraform apply` returns when the control plane reports the datastore `Active`, which means a " +
			"host has actually mounted it — not merely that the share exists. Every attribute is immutable: " +
			"a change destroys and recreates.",
		Attributes: mergeAttributes(
			lifecycleAttributes("datastore"),
			timeoutAttributes(datastoreCreateTimeout, datastoreDeleteTimeout),
			map[string]schema.Attribute{
				"cluster_id": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "UUID of the cluster this datastore belongs to.",
					PlanModifiers:       forceNew(),
				},
				"name": schema.StringAttribute{
					Required:            true,
					MarkdownDescription: "Name of the datastore. Required when provisioning.",
					PlanModifiers:       forceNew(),
				},
				"mount_path": schema.StringAttribute{
					Required: true,
					MarkdownDescription: "Absolute path each host mounts the share at, e.g. `/mnt/vvm/bulk`. " +
						"Required in both modes, and the control plane refuses one that is not absolute.",
					PlanModifiers: forceNew(),
				},
				"file_share_id": schema.StringAttribute{
					Optional: true,
					MarkdownDescription: "**Adopt mode.** Id of an existing VPC File Share. " +
						"Mutually exclusive with `size_gib`.",
					PlanModifiers: forceNew(),
				},
				"size_gib": schema.Int64Attribute{
					Optional: true,
					MarkdownDescription: "**Provision mode.** Size of the share VVM creates, in GiB. " +
						"Mutually exclusive with `file_share_id`. Grow it afterwards through the API; " +
						"a change here destroys and recreates, which would lose the data on it.",
					PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"zone": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "VPC zone the share is created in, e.g. `eu-gb-1`.",
					PlanModifiers:       forceNew(),
				},
				"storage_class": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "Share profile VVM provisions with.",
					PlanModifiers:       forceNew(),
				},
				"encryption_kp_key_id": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "Key Protect key the share is encrypted with. Omit for the platform default.",
					PlanModifiers:       forceNew(),
				},
				"iops": schema.Int64Attribute{
					Optional: true,
					MarkdownDescription: "IOPS to provision. Omit to take the platform default, which is " +
						"derived from the size. The permitted range depends on both the size and the profile.",
					PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				},
				"capacity_gb": schema.Int64Attribute{
					Computed: true,
					MarkdownDescription: "The capacity the control plane has MEASURED, in GB. " +
						"A reading, not an instruction: it tracks the share as it really is, so a grow " +
						"made outside Terraform shows up here on the next plan.",
				},
			},
		),
	}
}

func (r *datastoreResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req, resp)
}

// buildDatastoreBody is the POST /v1/datastores body.
//
// The mode is checked here rather than sent for the control plane to refuse:
// the two fields are mutually exclusive, and `terraform plan` is where an
// operator can still fix it without having made anything.
func buildDatastoreBody(plan datastoreModel) (map[string]any, error) {
	share := optionalString(plan.FileShareID)
	size := plan.SizeGiB
	_, provisioning := optionalInt64(size)
	switch {
	case share != "" && provisioning:
		return nil, errors.New("file_share_id and size_gib are mutually exclusive: " +
			"set file_share_id to adopt a share you already have, or size_gib to have VVM provision one")
	case share == "" && !provisioning:
		return nil, errors.New("one of file_share_id (adopt an existing share) or size_gib " +
			"(provision a new one) is required")
	}

	body := map[string]any{
		"cluster_id": plan.ClusterID.ValueString(),
		"name":       plan.Name.ValueString(),
		"mount_path": plan.MountPath.ValueString(),
	}
	if share != "" {
		body["file_share_id"] = share
	}
	if sizeGiB, set := optionalInt64(size); set {
		body["size_gib"] = sizeGiB
	}
	if z := optionalString(plan.Zone); z != "" {
		body["zone"] = z
	}
	if sc := optionalString(plan.StorageClass); sc != "" {
		body["storage_class"] = sc
	}
	if k := optionalString(plan.EncryptionKpKeyID); k != "" {
		body["encryption_kp_key_id"] = k
	}
	if iops, set := optionalInt64(plan.IOPS); set {
		body["iops"] = iops
	}
	return body, nil
}

func (r *datastoreResource) waitFor(id, jobID string) stateWait {
	return stateWait{
		client: r.client,
		path:   "/v1/datastores/" + id,
		jobID:  jobID,
		kind:   "datastore",
		target: "Active",
		failed: []string{"Failed"},
	}
}

func (r *datastoreResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan datastoreModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(plan.CreateTimeout), datastoreCreateTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid create_timeout", err.Error())
		return
	}
	body, err := buildDatastoreBody(plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid datastore configuration", err.Error())
		return
	}

	key, err := idempotencyKeyForBody("vvm_datastore", body)
	if err != nil {
		resp.Diagnostics.AddError("Error creating datastore", err.Error())
		return
	}
	raw, err := r.client.post(ctx, "/v1/datastores", body, key)
	if err != nil {
		resp.Diagnostics.AddError("Error creating datastore", err.Error())
		return
	}
	var ack writeAck
	if jsonErr := json.Unmarshal(raw, &ack); jsonErr != nil {
		resp.Diagnostics.AddError("Error parsing the datastore create response", jsonErr.Error())
		return
	}
	if ack.ID == "" {
		// Provisioning: the datastore row does not exist until the job has made
		// the share, so the 202 names a job and no id (postDatastoreProvision).
		id, idErr := r.idFromProvisionJob(ctx, ack.JobID, timeout)
		if idErr != nil {
			resp.Diagnostics.AddError("Error provisioning datastore", idErr.Error())
			return
		}
		ack.ID = id
	}
	plan.ID = types.StringValue(ack.ID)
	plan.JobID = types.StringValue(ack.JobID)
	plan.CapacityGb = types.Int64Value(0)

	// The id is saved whatever the wait says: a provisioned share that is still
	// mounting is real, costs money, and must not be forgotten. The wait's own
	// last answer is what the state is built from — asking again would be a
	// second call for the row it has just described.
	_, last, waitErr := r.waitFor(ack.ID, ack.JobID).wait(ctx, timeout)
	r.applyRaw(last, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if waitErr != nil {
		resp.Diagnostics.AddError("Datastore did not reach Active", waitErr.Error())
	}
}

// idFromProvisionJob waits for the datastore.provision job and reads the new
// datastore's id out of its result.
func (r *datastoreResource) idFromProvisionJob(ctx context.Context, jobID string, timeout time.Duration) (string, error) {
	if jobID == "" {
		return "", errors.New("the control plane accepted the datastore but named neither a datastore nor a job")
	}
	result, err := awaitJob(ctx, r.client, jobID, timeout)
	if err != nil {
		return "", err
	}
	var res struct {
		DatastoreID string `json:"datastore_id"`
	}
	if jsonErr := json.Unmarshal(result, &res); jsonErr != nil || res.DatastoreID == "" {
		return "", fmt.Errorf("job %s succeeded but its result names no datastore_id: %s", jobID, string(result))
	}
	return res.DatastoreID, nil
}

func (r *datastoreResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state datastoreModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, raw, err := readRow(ctx, r.client, "/v1/datastores/"+state.ID.ValueString())
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error reading datastore", err.Error())
		return
	}
	r.applyRaw(raw, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *datastoreResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Update not supported",
		"Every attribute of vvm_datastore is immutable, so a change destroys and recreates. "+
			"Reaching this is a provider bug.")
}

func (r *datastoreResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state datastoreModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := parseTimeout(optionalString(state.DeleteTimeout), datastoreDeleteTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Invalid delete_timeout", err.Error())
		return
	}
	if err := r.waitFor(state.ID.ValueString(), optionalString(state.JobID)).
		deleteAndWait(ctx, timeout); err != nil {
		resp.Diagnostics.AddError("Error deleting datastore", err.Error())
	}
}

// applyRaw decodes one datastore answer onto the model. Shared by Create and
// Read so there is one place the observed fields are read out of a body, and so
// Create does not ask the control plane again for the row the wait just read.
//
// An answer it cannot decode is a warning, not an error: the datastore exists
// either way, and losing its id is the worse outcome.
func (r *datastoreResource) applyRaw(raw []byte, m *datastoreModel, diags *diag.Diagnostics) {
	m.CapacityGb = knownOrZero(m.CapacityGb)
	if len(raw) == 0 {
		return
	}
	var row datastoreRow
	if err := json.Unmarshal(raw, &row); err != nil {
		diags.AddWarning("Could not read the datastore back",
			fmt.Sprintf("The control plane's answer for datastore %s could not be parsed: %s. "+
				"Its observed state in Terraform may be behind the control plane's.", m.ID.ValueString(), err))
		return
	}
	datastoreRowToState(row, m)
}

func datastoreRowToState(row datastoreRow, m *datastoreModel) {
	applyLifecycle(row.asyncRow, &m.ID, &m.DesiredState, &m.ActualState, &m.UpdatedAt, &m.JobID)
	m.CapacityGb = types.Int64Value(row.CapacityGb)
}
