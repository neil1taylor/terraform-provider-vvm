// Command terraform-provider-vvm is the entry point for the VVM Terraform
// provider. It registers the provider with the Terraform plugin framework
// and starts the gRPC server that Terraform connects to.
//
// Usage (manual install during development):
//
//	go build -o terraform-provider-vvm .
//	mv terraform-provider-vvm ~/.terraform.d/plugins/registry.terraform.io/neil1taylor/vvm/0.1.0/$(go env GOOS)_$(go env GOARCH)/
//
// Terraform configuration:
//
//	terraform {
//	  required_providers {
//	    vvm = {
//	      source  = "neil1taylor/vvm"
//	      version = "~> 0.1"
//	    }
//	  }
//	}
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/neil1taylor/terraform-provider-vvm/internal/provider"
)

// version is set at build time via -ldflags "-X main.version=<semver>".
// It defaults to "dev" for local builds.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run in debug mode with additional logging")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/neil1taylor/vvm",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err)
	}
}
