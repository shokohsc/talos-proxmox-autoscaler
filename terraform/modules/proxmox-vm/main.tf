resource "proxmox_virtual_environment_vm" "vm" {
  node_name = var.proxmox_node
  vm_id     = var.vm_id
  name      = var.vm_name

  agent {
    enabled = true
  }

  bios = "seabios"

  cpu {
    cores = var.cpu_cores
    type  = "host"
  }

  memory {
    dedicated = var.memory_mb
  }

  disk {
    datastore_id = var.storage_pool
    size         = var.disk_size_gb
    interface    = "scsi0"
  }

  network_devices {
    bridge   = var.network_bridge
    model    = "virtio"
    macaddr  = var.mac_address != null ? replace(var.mac_address, ":", "") : null
    firewall = false
  }

  operating_system {
    type = "l26"
  }

  boot {
    order = ["scsi0", "net0"]
  }

  dynamic "smbios" {
    for_each = var.serial != null ? [1] : []
    content {
      serial = var.serial
    }
  }

  started     = true
  description = "Talos worker: ${var.vm_name}"

  lifecycle {
    ignore_changes = [
      disk,
      network_devices,
    ]
  }
}

resource "time_sleep" "wait_for_vm" {
  depends_on = [proxmox_virtual_environment_vm.vm]

  create_duration = "60s"
}
