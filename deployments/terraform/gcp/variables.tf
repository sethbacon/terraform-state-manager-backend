variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "prefix" {
  type    = string
  default = "tsm"
}

variable "node_machine_type" {
  type    = string
  default = "e2-standard-4"
}

variable "node_count" {
  type    = number
  default = 3
}

variable "db_tier" {
  type    = string
  default = "db-custom-2-8192"
}

variable "db_password" {
  description = "Cloud SQL password (also stored as tsm-database-password)"
  type        = string
  sensitive   = true
}

variable "jwt_secret" {
  description = "openssl rand -hex 32; stored as tsm-jwt-secret"
  type        = string
  sensitive   = true
}

variable "encryption_key" {
  description = "openssl rand -hex 32; stored as tsm-encryption-key. ESCROW THIS."
  type        = string
  sensitive   = true
}

variable "tsm_namespace" {
  type    = string
  default = "terraform-state-manager"
}

variable "tsm_service_account" {
  type    = string
  default = "tsm-terraform-state-manager"
}
