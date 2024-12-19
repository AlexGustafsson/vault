# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: BUSL-1.1

locals {
  // Variables
  pki_mount                 = "pki" # secret
  pki_issuer_name           = "issuer"
  pki_common_name           = "common"
  pki_default_ttl           = "72h"
  pki_test_data_path_prefix = "smoke"
  pki_test_dir      = "tmp-test-results"

  // Output
  pki_output = {
    mount        = local.pki_mount
    common_name  = local.pki_common_name
    test_results = local.pki_test_dir
  }

}

output "pki" {
  value = local.pki_output
}

# Verify PKI Certificate
resource "enos_remote_exec" "pki_verify_certificates" {
  for_each = var.hosts

  environment = {
    MOUNT             = local.pki_mount
    VAULT_ADDR        = var.vault_addr
    VAULT_INSTALL_DIR = var.vault_install_dir
    VAULT_TOKEN       = var.vault_root_token
    COMMON_NAME       = local.pki_common_name
    ISSUER_NAME       = local.pki_issuer_name
    TTL               = local.pki_default_ttl
    TEST_DIR  = local.pki_test_dir
  }

  scripts = [abspath("${path.module}/../../scripts/pki-verify-certificates.sh")]

  transport = {
    ssh = {
      host = each.value.public_ip
    }
  }
}

