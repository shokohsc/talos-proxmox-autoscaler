provider "proxmox" {
  endpoint = var.proxmox_api_url
  username = var.proxmox_api_token_id
  password = var.proxmox_api_token_secret
  insecure = var.proxmox_insecure
}

module "workers" {
  source   = "./modules/proxmox-vm"
  count    = var.worker_count

  proxmox_api_url          = var.proxmox_api_url
  proxmox_api_token_id     = var.proxmox_api_token_id
  proxmox_api_token_secret = var.proxmox_api_token_secret
  proxmox_node             = var.proxmox_node
  vm_name                  = "${var.cluster_name}-worker-${count.index}"
  vm_id                    = var.vm_id_start + count.index
  cpu_cores                = var.worker_cpu_cores
  memory_mb                = var.worker_memory_mb
  disk_size_gb             = var.worker_disk_gb
  network_bridge           = var.network_bridge
  storage_pool             = var.storage_pool
  mac_address              = var.worker_mac_addresses != null ? var.worker_mac_addresses[count.index] : null
  serial                   = var.worker_serial
}
