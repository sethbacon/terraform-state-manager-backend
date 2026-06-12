output "artifact_registry_prefix" {
  description = "values-gke.yaml: image repository prefix (<REGION>-docker.pkg.dev/<PROJECT_ID>/tsm)"
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.main.repository_id}"
}

output "cloud_sql_ip" {
  description = "values-gke.yaml: externalDatabase.host"
  value       = google_sql_database_instance.main.public_ip_address
}

output "gsa_email" {
  description = "values-gke.yaml: serviceAccount.annotations iam.gke.io/gcp-service-account"
  value       = google_service_account.tsm.email
}

output "cluster_name" {
  value = google_container_cluster.main.name
}

output "kubeconfig_command" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.main.name} --region ${var.region} --project ${var.project_id}"
}
