# ============================================================
# AKS landing zone for the Terraform State Manager Helm chart
# ============================================================
# Provisions: resource group, ACR, AKS (OIDC issuer + Workload Identity +
# Key Vault Secrets Store CSI add-on), Azure Database for PostgreSQL Flexible
# Server, Key Vault with the three app secrets, Application Gateway for
# Containers (AGfC) + ALB-controller identity, and the app's user-assigned
# managed identity with a federated credential bound to the chart's
# ServiceAccount.
#
# Outputs map 1:1 onto deployments/helm/values-aks.yaml placeholders.
# See docs/deployment/aks-new-cluster.md for the end-to-end walkthrough
# (this module covers Azure resources; cert-manager + ALB controller install
# and the helm release itself stay imperative).

terraform {
  required_version = ">= 1.6"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

provider "azurerm" {
  features {}
}

data "azurerm_client_config" "current" {}

resource "azurerm_resource_group" "main" {
  name     = var.resource_group_name
  location = var.location
}

# ── Container registry ────────────────────────────────────────────────────────
resource "azurerm_container_registry" "main" {
  name                = var.acr_name
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  sku                 = "Standard"
  admin_enabled       = false
}

# ── AKS ────────────────────────────────────────────────────────────────────────
resource "azurerm_kubernetes_cluster" "main" {
  name                = "${var.prefix}-aks"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  dns_prefix          = var.prefix
  kubernetes_version  = var.kubernetes_version

  default_node_pool {
    name                 = "default"
    vm_size              = var.node_vm_size
    node_count           = var.node_count
    auto_scaling_enabled = false
  }

  identity {
    type = "SystemAssigned"
  }

  # Required by Workload Identity and the chart's Key Vault CSI integration.
  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  key_vault_secrets_provider {
    secret_rotation_enabled = true
  }

  network_profile {
    network_plugin = "azure"
    network_policy = "azure" # the chart's NetworkPolicies need an enforcing CNI
  }
}

resource "azurerm_role_assignment" "aks_acr_pull" {
  scope                = azurerm_container_registry.main.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.main.kubelet_identity[0].object_id
}

# ── PostgreSQL Flexible Server ────────────────────────────────────────────────
resource "azurerm_postgresql_flexible_server" "main" {
  name                          = "${var.prefix}-pg"
  resource_group_name           = azurerm_resource_group.main.name
  location                      = azurerm_resource_group.main.location
  version                       = "16"
  sku_name                      = var.postgres_sku
  storage_mb                    = 65536
  administrator_login           = "tsm"
  administrator_password        = var.postgres_admin_password
  backup_retention_days         = 14
  geo_redundant_backup_enabled  = false
  public_network_access_enabled = true # tighten to VNet integration for production
}

resource "azurerm_postgresql_flexible_server_database" "main" {
  name      = "terraform_state_manager"
  server_id = azurerm_postgresql_flexible_server.main.id
}

# Allow Azure-internal services (incl. AKS egress) — replace with VNet rules
# or private endpoints for a locked-down install.
resource "azurerm_postgresql_flexible_server_firewall_rule" "allow_azure" {
  name             = "allow-azure-services"
  server_id        = azurerm_postgresql_flexible_server.main.id
  start_ip_address = "0.0.0.0"
  end_ip_address   = "0.0.0.0"
}

# ── Key Vault + app secrets (names match the chart's SecretProviderClass) ─────
resource "azurerm_key_vault" "main" {
  name                      = var.key_vault_name
  resource_group_name       = azurerm_resource_group.main.name
  location                  = azurerm_resource_group.main.location
  tenant_id                 = data.azurerm_client_config.current.tenant_id
  sku_name                  = "standard"
  rbac_authorization_enabled = true
  purge_protection_enabled  = true
}

resource "azurerm_role_assignment" "deployer_kv_admin" {
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Administrator"
  principal_id         = data.azurerm_client_config.current.object_id
}

resource "azurerm_key_vault_secret" "jwt_secret" {
  name         = "jwt-secret"
  value        = var.jwt_secret
  key_vault_id = azurerm_key_vault.main.id
  depends_on   = [azurerm_role_assignment.deployer_kv_admin]
}

resource "azurerm_key_vault_secret" "encryption_key" {
  name         = "encryption-key"
  value        = var.encryption_key
  key_vault_id = azurerm_key_vault.main.id
  depends_on   = [azurerm_role_assignment.deployer_kv_admin]
}

resource "azurerm_key_vault_secret" "database_password" {
  name         = "database-password"
  value        = var.postgres_admin_password
  key_vault_id = azurerm_key_vault.main.id
  depends_on   = [azurerm_role_assignment.deployer_kv_admin]
}

# ── App identity: Workload Identity + Key Vault read ──────────────────────────
resource "azurerm_user_assigned_identity" "tsm" {
  name                = "${var.prefix}-app-identity"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
}

resource "azurerm_role_assignment" "tsm_kv_secrets" {
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.tsm.principal_id
}

resource "azurerm_federated_identity_credential" "tsm" {
  name                = "${var.prefix}-chart-sa"
  resource_group_name = azurerm_resource_group.main.name
  parent_id           = azurerm_user_assigned_identity.tsm.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = azurerm_kubernetes_cluster.main.oidc_issuer_url
  subject             = "system:serviceaccount:${var.tsm_namespace}:${var.tsm_service_account}"
}

# ── Application Gateway for Containers + ALB controller identity ─────────────
resource "azurerm_application_load_balancer" "main" {
  name                = "${var.prefix}-agfc"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
}

resource "azurerm_application_load_balancer_frontend" "main" {
  name                         = "${var.prefix}-frontend"
  application_load_balancer_id = azurerm_application_load_balancer.main.id
}

resource "azurerm_user_assigned_identity" "alb_controller" {
  name                = "${var.prefix}-alb-controller"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
}

resource "azurerm_role_assignment" "alb_controller_agfc" {
  scope                = azurerm_application_load_balancer.main.id
  role_definition_name = "AppGw for Containers Configuration Manager"
  principal_id         = azurerm_user_assigned_identity.alb_controller.principal_id
}

resource "azurerm_federated_identity_credential" "alb_controller" {
  name                = "${var.prefix}-alb-controller-sa"
  resource_group_name = azurerm_resource_group.main.name
  parent_id           = azurerm_user_assigned_identity.alb_controller.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = azurerm_kubernetes_cluster.main.oidc_issuer_url
  subject             = "system:serviceaccount:azure-alb-system:alb-controller-sa"
}
