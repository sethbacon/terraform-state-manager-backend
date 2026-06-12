# ============================================================
# GKE landing zone for the Terraform State Manager Helm chart
# ============================================================
# GKE (Workload Identity + Gateway API), Artifact Registry, Cloud SQL
# PostgreSQL, Secret Manager secrets (tsm-jwt-secret, tsm-encryption-key,
# tsm-database-password), and the GSA the chart's KSA impersonates for
# Secret Manager CSI reads. Outputs map onto deployments/helm/values-gke.yaml.
# Cluster add-ons (Secrets Store CSI + GCP provider, cert-manager) install per
# docs/deployment/gke-deployment.md.

terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

# ── Artifact Registry ─────────────────────────────────────────────────────────
resource "google_artifact_registry_repository" "main" {
  repository_id = "tsm"
  location      = var.region
  format        = "DOCKER"
}

# ── GKE ────────────────────────────────────────────────────────────────────────
resource "google_container_cluster" "main" {
  name     = "${var.prefix}-gke"
  location = var.region

  initial_node_count       = 1
  remove_default_node_pool = true

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  gateway_api_config {
    channel = "CHANNEL_STANDARD"
  }

  # Required for the chart's NetworkPolicies.
  datapath_provider = "ADVANCED_DATAPATH"

  deletion_protection = true
}

resource "google_container_node_pool" "main" {
  name       = "default"
  cluster    = google_container_cluster.main.id
  node_count = var.node_count

  node_config {
    machine_type = var.node_machine_type
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }
}

# ── Cloud SQL PostgreSQL ──────────────────────────────────────────────────────
resource "google_sql_database_instance" "main" {
  name             = "${var.prefix}-pg"
  database_version = "POSTGRES_16"
  region           = var.region

  settings {
    tier = var.db_tier
    backup_configuration {
      enabled = true
    }
    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
    }
  }

  deletion_protection = true
}

resource "google_sql_database" "main" {
  name     = "terraform_state_manager"
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "tsm" {
  name     = "tsm"
  instance = google_sql_database_instance.main.name
  password = var.db_password
}

# ── Secret Manager (names match the chart's CSI objects) ─────────────────────
resource "google_secret_manager_secret" "jwt" {
  secret_id = "tsm-jwt-secret"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "jwt" {
  secret      = google_secret_manager_secret.jwt.id
  secret_data = var.jwt_secret
}

resource "google_secret_manager_secret" "encryption" {
  secret_id = "tsm-encryption-key"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "encryption" {
  secret      = google_secret_manager_secret.encryption.id
  secret_data = var.encryption_key
}

resource "google_secret_manager_secret" "db" {
  secret_id = "tsm-database-password"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db" {
  secret      = google_secret_manager_secret.db.id
  secret_data = var.db_password
}

# ── App GSA + Workload Identity binding + secret access ──────────────────────
resource "google_service_account" "tsm" {
  account_id   = "${var.prefix}-app"
  display_name = "Terraform State Manager"
}

resource "google_secret_manager_secret_iam_member" "access" {
  for_each = {
    jwt        = google_secret_manager_secret.jwt.id
    encryption = google_secret_manager_secret.encryption.id
    db         = google_secret_manager_secret.db.id
  }
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.tsm.email}"
}

resource "google_service_account_iam_member" "wi_binding" {
  service_account_id = google_service_account.tsm.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.tsm_namespace}/${var.tsm_service_account}]"
}
