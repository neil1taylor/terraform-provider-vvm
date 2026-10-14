# Provision mode: VVM makes the VPC File Share and mounts it on every host.
resource "vvm_datastore" "app" {
  cluster_id = var.cluster_id
  name       = "app-ds"
  mount_path = "/mnt/vvm/app"
  size_gib   = 100
  zone       = "eu-gb-1"
}
