output "vmid" {
  description = "Proxmox VM ID"
  value       = proxmox_virtual_environment_vm.vm.id
}

output "ip" {
  description = "VM IP address"
  value       = var.ip_address
}

output "status" {
  description = "VM status"
  value       = proxmox_virtual_environment_vm.vm.status
}
