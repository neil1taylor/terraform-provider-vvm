terraform {
  required_providers {
    vvm = {
      source = "neil1taylor/vvm"
    }
  }
}

provider "vvm" {
  # Or set VVM_API_URL and VVM_API_TOKEN in the environment.
  api_url   = var.vvm_api_url
  api_token = var.vvm_api_token
}

variable "vvm_api_url" { type = string }

variable "vvm_api_token" {
  type      = string
  sensitive = true
}
