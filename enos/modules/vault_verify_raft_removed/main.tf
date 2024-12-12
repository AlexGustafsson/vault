# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: BUSL-1.1

terraform {
  required_providers {
    enos = {
      source = "registry.terraform.io/hashicorp-forge/enos"
    }
  }
}

variable "hosts" {
  type = map(object({
    ipv6       = string
    private_ip = string
    public_ip  = string
  }))
  description = "The vault cluster instances that were removed"
}

variable "retry_interval" {
  type        = number
  description = "How many seconds to wait between each retry"
  default     = 2
}

variable "timeout" {
  type        = number
  description = "The max number of seconds to wait before timing out"
  default     = 60
}

variable "listener_port" {
  type        = number 
  description = "The port of the vault address" 
}
variable "vault_leader_addr" {
  type        = string
  description = "The cluster leader API address"
}
variable "vault_root_token" {
  type        = string
  description = "The vault root token"
}
variable "vault_seal_type" {
  type        = string
  description = "The Vault seal type"
}

variable "add_back_nodes" {
  type = bool 
  description = "whether to add the nodes back"
}

variable "vault_unseal_keys" {}

resource "enos_remote_exec" "verify_raft_peer_removed" {
  for_each = var.hosts

  environment = {
    RETRY_INTERVAL  = var.retry_interval
    TIMEOUT_SECONDS = var.timeout
    VAULT_ADDR      = "${each.value.public_ip}:${var.listener_port}"
    VAULT_TOKEN     = var.vault_root_token
  }

  scripts = [abspath("${path.module}/scripts/verify_raft_removed_peer.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

resource "enos_remote_exec" "verify_unseal_fails" {
  for_each = {
    for idx, host in var.hosts : idx => host
    if var.vault_seal_type == "shamir"
  }

  environment = {
    VAULT_ADDR      = var.vault_addr
    VAULT_TOKEN     = var.vault_root_token
    UNSEAL_KEYS       = join(",", var.vault_unseal_keys)
  }

  scripts = [abspath("${path.module}/scripts/verify_unseal_fails.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

resource "enos_remote_exec" "verify_rejoin_fails" {
  for_each = var.hosts

  environment = {
    VAULT_ADDR      = var.vault_addr
    VAULT_TOKEN     = var.vault_root_token
    RETRY_INTERVAL  = var.retry_interval
    TIMEOUT_SECONDS = var.timeout
    JOIN_EXIT_CODE   = 1
    VAULT_LEADER_ADDR = var.vault_leader_addr
  }

  scripts = [abspath("${path.module}/scripts/try_rejoin.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}
resource "enos_remote_exec" "restart" {
  depends_on = [enos_remote_exec.verify_rejoin_fails, enos_remote_exec.verify_raft_peer_removed]
  for_each = var.hosts 

  inline = ["sudo systemctl restart vault"]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

resource "enos_remote_exec" "verify_removed_after_restart" {
  depends_on = [enos_remote_exec.restart]
  for_each = var.hosts

  environment = {
    RETRY_INTERVAL  = var.retry_interval
    TIMEOUT_SECONDS = var.timeout
    VAULT_ADDR      = var.vault_addr
    VAULT_TOKEN     = var.vault_root_token
  }

  scripts = [abspath("${path.module}/scripts/verify_raft_removed_peer.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

module "stop" {
  depends_on = [enos_remote_exec.verify_removed_after_restart]
  source = "../stop_vault"
  count = var.add_back_nodes ? 1 : 0 

  hosts         = var.hosts 
}

resource "enos_remote_exec" "delete_data" {
  depends_on = [module.stop]
  for_each = {
    for idx, host in var.hosts : idx => host
    if var.add_back_nodes
  }

  scripts = [abspath("${path.module}/scripts/delete_data.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }

}
resource "enos_remote_exec" "start" {
  depends_on = [enos_remote_exec.delete_data]
  for_each = {
    for idx, host in var.hosts : idx => host
    if var.add_back_nodes
  }
  inline   = ["sudo systemctl start vault; sleep 5"]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

resource "enos_remote_exec" "verify_rejoin_succeeds" {
  for_each = {
    for idx, host in var.hosts : idx => host
    if var.add_back_nodes
  }

  environment = {
    VAULT_ADDR      = var.vault_addr
    VAULT_TOKEN     = var.vault_root_token
    RETRY_INTERVAL  = var.retry_interval
    TIMEOUT_SECONDS = var.timeout
    JOIN_EXIT_CODE   = 0
    VAULT_LEADER_ADDR = var.vault_leader_addr
  }

  scripts = [abspath("${path.module}/scripts/try_rejoin.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}
