variable "region" {
  type    = string
  default = "us-east-1"
}

variable "prefix" {
  type    = string
  default = "tsm"
}

variable "vpc_id" {
  description = "Existing VPC for EKS and RDS"
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnets (EKS nodes + RDS)"
  type        = list(string)
}

variable "kubernetes_version" {
  type    = string
  default = "1.30"
}

variable "node_instance_type" {
  type    = string
  default = "m6i.xlarge"
}

variable "node_count" {
  type    = number
  default = 3
}

variable "db_instance_class" {
  type    = string
  default = "db.m6g.large"
}

variable "db_password" {
  description = "RDS master password (also stored in Secrets Manager)"
  type        = string
  sensitive   = true
}

variable "jwt_secret" {
  description = "openssl rand -hex 32; stored as tsm/jwt-secret"
  type        = string
  sensitive   = true
}

variable "encryption_key" {
  description = "openssl rand -hex 32; stored as tsm/encryption-key. ESCROW THIS."
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
