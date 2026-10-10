package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// clientFrom is the provider's client, as handed to a resource at Configure.
// nil when the provider has not been configured yet, which the framework does
// on purpose during validation.
func clientFrom(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *vvmClient {
	if req.ProviderData == nil {
		return nil
	}
	c, ok := req.ProviderData.(*vvmClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *vvmClient, got %T", req.ProviderData))
		return nil
	}
	return c
}

// The attributes every asynchronous VVM resource carries, in one place. Three
// resources declaring the same five attributes three times is three chances for
// one of them to mean something slightly different.

// forceNew marks an attribute the API cannot change in place: a change to it
// destroys and recreates.
func forceNew() []planmodifier.String {
	return []planmodifier.String{stringplanmodifier.RequiresReplace()}
}

// keepFromState holds a computed value across a plan that has nothing to say
// about it.
func keepFromState() []planmodifier.String {
	return []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
}

// lifecycleAttributes are the observed-state attributes.
//
// 🔑 Read REFRESHES THESE, AND THAT IS WHAT MAKES `terraform plan` TRUTHFUL. If
// a VM was stopped or migrated out of band, actual_state is where the plan shows
// it (vvm-iac-spec.md §2.1). They are computed, never declared: desired_state is
// the operator's wish expressed through the resource's own attributes, and
// actual_state is the control plane's reconciled reading of reality.
func lifecycleAttributes(kind string) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "UUID the control plane minted for this " + kind + ".",
			PlanModifiers:       keepFromState(),
		},
		"desired_state": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The state the control plane is working towards.",
		},
		"actual_state": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The state the control plane has OBSERVED, reconciled against reality. " +
				"`terraform plan` shows a change here when the " + kind + " was changed outside Terraform.",
		},
		"updated_at": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "When the row last changed, as the API reported it. " +
				"Used as the `If-Match` precondition on a delete; it is re-read immediately " +
				"before the delete is sent, because the row's timestamp moves on its own.",
		},
		"job_id": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The asynchronous job the create emitted. Named in the error when a " +
				"create or a delete runs out of time, so the real job state is what the operator sees.",
		},
	}
}

// timeoutAttributes are how long an apply and a destroy may wait. Per resource,
// because provisioning a VM (scheduling, network reconcile, boot) and making a
// network are not the same wait (vvm-iac-spec.md §2.1).
func timeoutAttributes(createDefault, deleteDefault string) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"create_timeout": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "How long `terraform apply` waits for this resource to reach its target state, " +
				"as a Go duration (e.g. `45m`). Default `" + createDefault + "`. " +
				"On timeout the error carries the state the control plane last reported and the state of the job behind it.",
		},
		"delete_timeout": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "How long `terraform destroy` waits for this resource to be gone, " +
				"as a Go duration. Default `" + deleteDefault + "`.",
		},
	}
}

// mergeAttributes builds one schema's attribute map from the shared sets and the
// resource's own.
func mergeAttributes(sets ...map[string]schema.Attribute) map[string]schema.Attribute {
	out := map[string]schema.Attribute{}
	for _, set := range sets {
		for name, attr := range set {
			out[name] = attr
		}
	}
	return out
}

// optionalString is the value of an attribute the operator may have left out,
// or "" when they did.
func optionalString(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}

// optionalInt64 is the value of a number the operator may have left out, and
// whether they set one. Zero is not a value any of these fields can take, so an
// unset number and a zero are the same thing to the control plane.
func optionalInt64(v types.Int64) (int64, bool) {
	if v.IsNull() || v.IsUnknown() || v.ValueInt64() == 0 {
		return 0, false
	}
	return v.ValueInt64(), true
}

// knownOrEmpty settles a computed string that nothing has filled in.
//
// 🔑 Terraform FAILS an apply whose plan promised a value and whose state came
// back unknown ("value is unknown after apply"). Every computed attribute has
// to end up known, and a resource whose create answered nothing for one of them
// says so with an empty string.
func knownOrEmpty(v types.String) types.String {
	if v.IsNull() || v.IsUnknown() {
		return types.StringValue("")
	}
	return v
}

// knownOrZero settles a computed number nothing has filled in, for the same
// reason knownOrEmpty settles a string.
func knownOrZero(v types.Int64) types.Int64 {
	if v.IsNull() || v.IsUnknown() {
		return types.Int64Value(0)
	}
	return v
}

// applyLifecycle copies the observed lifecycle fields off a row. The three
// resources differ in what else they report; this part they share.
func applyLifecycle(row asyncRow, id, desired, actual, updatedAt, jobID *types.String) {
	if row.ID != "" {
		*id = types.StringValue(row.ID)
	}
	*desired = types.StringValue(row.DesiredState)
	*actual = types.StringValue(row.ActualState)
	*updatedAt = types.StringValue(row.UpdatedAt)
	*jobID = knownOrEmpty(*jobID)
}
