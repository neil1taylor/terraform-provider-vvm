package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure vvmProvider satisfies the provider.Provider interface.
var _ provider.Provider = &vvmProvider{}
var _ provider.ProviderWithFunctions = &vvmProvider{}

// vvmProvider is the root provider implementation for the VVM Terraform
// provider. It reads VVM_API_URL + VVM_API_TOKEN (or IBMCLOUD_API_KEY) from
// the environment (or from the `api_url` / `api_token` attributes in the
// provider block) and wires a vvmClient into every resource/data source.
type vvmProvider struct {
	version string
}

// vvmProviderModel maps the provider schema attributes to Go fields.
type vvmProviderModel struct {
	APIURL   types.String `tfsdk:"api_url"`
	APIToken types.String `tfsdk:"api_token"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &vvmProvider{version: version}
	}
}

func (p *vvmProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "vvm"
	resp.Version = p.version
}

func (p *vvmProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Terraform provider for the IBM Cloud VPC Virtualization Manager (VVM). " +
			"Manages VMs, guest networks, datastores, backup policies and protection groups via the " +
			"VVM REST API.",
		Attributes: map[string]schema.Attribute{
			"api_url": schema.StringAttribute{
				MarkdownDescription: "VVM API base URL (e.g. `https://vvm-api.example.ibm.com`). " +
					"Falls back to the `VVM_API_URL` environment variable.",
				Optional: true,
			},
			"api_token": schema.StringAttribute{
				MarkdownDescription: "VVM API bearer token. Falls back to `VVM_API_TOKEN`. " +
					"Set `IBMCLOUD_API_KEY` for automatic IAM token exchange (not yet implemented " +
					"in the provider; use an explicit token for now).",
				Optional:  true,
				Sensitive: true,
			},
		},
	}
}

func (p *vvmProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg vvmProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiURL := cfg.APIURL.ValueString()
	if apiURL == "" {
		apiURL = os.Getenv("VVM_API_URL")
	}
	if apiURL == "" {
		resp.Diagnostics.AddError(
			"Missing VVM API URL",
			"Set the `api_url` provider attribute or the `VVM_API_URL` environment variable.",
		)
		return
	}

	apiToken := cfg.APIToken.ValueString()
	if apiToken == "" {
		apiToken = os.Getenv("VVM_API_TOKEN")
	}
	if apiToken == "" {
		resp.Diagnostics.AddError(
			"Missing VVM API token",
			"Set the `api_token` provider attribute or the `VVM_API_TOKEN` environment variable.",
		)
		return
	}

	client := newVVMClient(apiURL, apiToken)
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *vvmProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewNetworkResource,
		NewDatastoreResource,
		NewVMResource,
		NewBackupPolicyResource,
		NewProtectionGroupResource,
		NewTemplateImportResource,
		NewBRSEnrollmentResource,
	}
}

func (p *vvmProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewActionInvocationDataSource,
	}
}

func (p *vvmProvider) Functions(_ context.Context) []func() function.Function {
	return nil
}
