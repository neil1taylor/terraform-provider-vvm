resource "vvm_network" "app" {
  cluster_id       = var.cluster_id
  name             = "app-net"
  vlan_id          = 105
  vpc_subnet_id    = var.vpc_subnet_id
  cidr             = "10.249.0.0/24"
  ipam_range_start = "10.249.0.10"
  ipam_range_end   = "10.249.0.50"
}
