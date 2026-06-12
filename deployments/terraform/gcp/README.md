# GKE landing zone (Terraform)

GKE (Workload Identity, Gateway API, Dataplane V2 for NetworkPolicy) +
Artifact Registry + Cloud SQL PostgreSQL + Secret Manager secrets
(`tsm-jwt-secret`, `tsm-encryption-key`, `tsm-database-password`) + the GSA
the chart's KSA impersonates. Outputs map onto
`deployments/helm/values-gke.yaml`.

After apply, install the Secrets Store CSI Driver + GCP provider and
cert-manager per [docs/deployment/gke-deployment.md](../../../docs/deployment/gke-deployment.md).

> **Escrow `encryption_key`** — losing it orphans stored credentials.
