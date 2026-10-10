# terraform-provider-vvm

Terraform provider for the IBM Cloud VPC Virtualization Manager (VVM). It
manages `vvm_network`, `vvm_datastore`, `vvm_vm`, `vvm_backup_policy`,
`vvm_protection_group` and related resources by calling the VVM REST API.

```hcl
terraform {
  required_providers {
    vvm = {
      source  = "neil1taylor/vvm"
      version = "~> 0.1"
    }
  }
}

provider "vvm" {
  api_url   = var.vvm_api_url   # or VVM_API_URL
  api_token = var.vvm_api_token # or VVM_API_TOKEN; mark the variable sensitive
}
```

Build locally with `go build -o terraform-provider-vvm .`.

The provider reaches the VVM API at `api_url`. Under IBM Cloud Schematics the
runner must be able to reach that address.

License: MIT.
