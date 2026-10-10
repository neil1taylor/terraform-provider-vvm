# A Schematics workspace template: a guest network, a datastore, a VM and a
# backup policy. Point a workspace at this folder and give it the variables
# below; mark api_token secure.

terraform {
  required_providers {
    vvm = { source = "neil1taylor/vvm" }
  }
}

variable "api_url" { type = string }
variable "api_token" {
  type      = string
  sensitive = true
}
variable "cluster_id" { type = string }
variable "name_prefix" { type = string }
variable "zone" { type = string }
variable "vlan_id" { type = number }
variable "vpc_subnet_id" { type = string }
variable "cidr" { type = string }
variable "ipam_start" { type = string }
variable "ipam_end" { type = string }
variable "mount_path" { type = string }
variable "size_gib" {
  type    = number
  default = 10
}
variable "golden_image" { type = string }
variable "boot_subnet_id" { type = string }
variable "ssh_public_key" { type = string }
variable "cos_bucket" { type = string }
variable "cos_region" { type = string }

provider "vvm" {
  api_url   = var.api_url
  api_token = var.api_token
}

resource "vvm_network" "this" {
  cluster_id       = var.cluster_id
  name             = "${var.name_prefix}-net"
  vlan_id          = var.vlan_id
  vpc_subnet_id    = var.vpc_subnet_id
  cidr             = var.cidr
  ipam_range_start = var.ipam_start
  ipam_range_end   = var.ipam_end
}

resource "vvm_datastore" "this" {
  cluster_id = var.cluster_id
  name       = "${var.name_prefix}-ds"
  mount_path = var.mount_path
  size_gib   = var.size_gib
  zone       = var.zone
}

resource "vvm_vm" "this" {
  cluster_id          = var.cluster_id
  datastore_id        = vvm_datastore.this.id
  network_id          = vvm_network.this.id
  name                = "${var.name_prefix}-vm"
  vcpu_request        = 1
  memory_request_mib  = 2048
  golden_image        = var.golden_image
  boot_disk_gib       = 20
  boot_subnet_id      = var.boot_subnet_id
  ssh_authorized_keys = [var.ssh_public_key]
}

resource "vvm_backup_policy" "this" {
  name                        = "${var.name_prefix}-policy"
  schedule_cron               = "0 4 29 2 *"
  retain_days                 = 1
  synthetic_full_cadence_days = 7
  skew_policy                 = "flag-and-keep"
  cos_bucket                  = var.cos_bucket
  cos_region                  = var.cos_region
}
