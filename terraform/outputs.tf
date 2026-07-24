output "worker_ips" {
  description = "IP addresses of worker nodes"
  value       = [for vm in module.workers : vm.ip]
}

output "worker_vmids" {
  description = "Proxmox VM IDs of worker nodes"
  value       = [for vm in module.workers : vm.vmid]
}
