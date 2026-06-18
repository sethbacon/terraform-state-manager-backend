# GKE landing zone (Terraform)

GKE (Workload Identity, Gateway API, Dataplane V2 for NetworkPolicy) +
Artifact Registry + Cloud SQL PostgreSQL + Secret Manager secrets
(`tsm-jwt-secret`, `tsm-encryption-key`, `tsm-database-password`) + the GSA
the chart's KSA impersonates. Outputs map onto
`deployments/helm/values-gke.yaml`.

```bash
terraform init
terraform apply \
  -var project_id=<gcp-project-id> \
  -var db_password="$(openssl rand -base64 24)" \
  -var jwt_secret="$(openssl rand -hex 32)" \
  -var encryption_key="$(openssl rand -hex 32)"
```

After apply, install the Secrets Store CSI Driver + GCP provider and
cert-manager per [docs/deployment/gke-deployment.md](../../../docs/deployment/gke-deployment.md).

**Required inputs:** `project_id`, and the sensitive `db_password` /
`jwt_secret` / `encryption_key`. Notable defaults: `region=us-central1`,
`node_machine_type=e2-standard-4`, `node_count=3`, `db_tier=db-custom-2-8192`.

**Outputs** (each maps onto `values-gke.yaml` — see its description):
`artifact_registry_prefix`, `cloud_sql_ip`, `gsa_email`, `cluster_name`,
`kubeconfig_command`.

> **Escrow `encryption_key`** — losing it orphans stored credentials.
