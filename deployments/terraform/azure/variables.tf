variable "location" {
  description = "Azure region"
  type        = string
  default     = "eastus2"
}

variable "resource_group_name" {
  description = "Resource group to create"
  type        = string
  default     = "rg-terraform-state-manager"
}

variable "prefix" {
  description = "Name prefix for resources (lowercase alphanumeric)"
  type        = string
  default     = "tsm"
}

variable "acr_name" {
  description = "Globally-unique ACR name (alphanumeric only)"
  type        = string
}

variable "key_vault_name" {
  description = "Globally-unique Key Vault name"
  type        = string
}

variable "kubernetes_version" {
  description = "AKS Kubernetes version (null = current default)"
  type        = string
  default     = null
}

variable "node_vm_size" {
  description = "AKS node pool VM size"
  type        = string
  default     = "Standard_D4s_v5"
}

variable "node_count" {
  description = "AKS node count"
  type        = number
  default     = 3
}

variable "postgres_sku" {
  description = "PostgreSQL Flexible Server SKU"
  type        = string
  default     = "GP_Standard_D2ds_v5"
}

variable "postgres_admin_password" {
  description = "PostgreSQL admin password (also stored in Key Vault as database-password)"
  type        = string
  sensitive   = true
}

variable "jwt_secret" {
  description = "TSM JWT signing secret (openssl rand -hex 32); stored in Key Vault as jwt-secret"
  type        = string
  sensitive   = true
}

variable "encryption_key" {
  description = "TSM encryption key, 64 hex chars (openssl rand -hex 32); stored in Key Vault as encryption-key. ESCROW THIS — losing it orphans stored credentials."
  type        = string
  sensitive   = true
}

variable "tsm_namespace" {
  description = "Kubernetes namespace the chart deploys into (federated credential subject)"
  type        = string
  default     = "terraform-state-manager"
}

variable "tsm_service_account" {
  description = "ServiceAccount name the chart creates (federated credential subject). Default matches helm release 'tsm' + chart fullname."
  type        = string
  default     = "tsm-terraform-state-manager"
}
