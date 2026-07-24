variable "proxmox_api_url" {
  description = "Proxmox API endpoint URL"
  type        = string
}

variable "proxmox_api_token_id" {
  description = "Proxmox API token ID"
  type        = string
  sensitive   = true
}

variable "proxmox_api_token_secret" {
  description = "Proxmox API token secret"
  type        = string
  sensitive   = true
}

variable "proxmox_insecure" {
  description = "Skip TLS verification for Proxmox API"
  type        = bool
  default     = false
}

variable "proxmox_node" {
  description = "Proxmox node name"
  type        = string
}

variable "cluster_name" {
  description = "Talos cluster name"
  type        = string
}

variable "worker_count" {
  description = "Number of worker nodes"
  type        = number
  default     = 1
}

variable "worker_cpu_cores" {
  description = "CPU cores per worker"
  type        = number
  default     = 4
}

variable "worker_memory_mb" {
  description = "Memory in MB per worker"
  type        = number
  default     = 8192
}

variable "worker_disk_gb" {
  description = "Disk size in GB per worker"
  type        = number
  default     = 100
}

variable "worker_mac_addresses" {
  description = "MAC addresses for workers (optional, Proxmox assigns if null)"
  type        = list(string)
  default     = null
}

variable "worker_serial" {
  description = "SMBIOS serial number for workers (optional)"
  type        = string
  default     = null
}

variable "vm_id_start" {
  description = "Starting Proxmox VM ID for workers"
  type        = number
  default     = 2000
}

variable "network_bridge" {
  description = "Proxmox network bridge"
  type        = string
  default     = "vmbr0"
}

variable "storage_pool" {
  description = "Proxmox storage pool"
  type        = string
  default     = "local-lvm"
}
