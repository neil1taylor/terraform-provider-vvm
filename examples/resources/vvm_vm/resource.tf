resource "vvm_vm" "app" {
  cluster_id          = var.cluster_id
  datastore_id        = vvm_datastore.app.id
  network_id          = vvm_network.app.id
  name                = "app-vm"
  vcpu_request        = 2
  memory_request_mib  = 4096
  golden_image        = var.golden_image
  boot_disk_gib       = 20
  boot_subnet_id      = var.boot_subnet_id
  ssh_authorized_keys = [var.ssh_public_key]
}
