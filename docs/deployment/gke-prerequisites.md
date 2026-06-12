# GKE prerequisites

`deployments/terraform/gcp` provisions all of this
([README](../../deployments/terraform/gcp/README.md)); manual equivalents:

1. **Artifact Registry** repo `tsm` (Docker); push both images.
2. **GKE** with: Workload Identity (`--workload-pool=<project>.svc.id.goog`),
   Gateway API (`--gateway-api=standard`), Dataplane V2
   (`--enable-dataplane-v2`, NetworkPolicy enforcement).
3. **Cloud SQL PostgreSQL 16** (SSL-only), db `terraform_state_manager`, user
   `tsm`. The chart connects by IP/DNS with `sslMode=require`; for the
   auth-proxy-sidecar pattern see the comment in `values-gke.yaml`.
4. **Secret Manager** secrets: `tsm-jwt-secret`, `tsm-encryption-key`
   (**escrow it**), `tsm-database-password`.
5. **GSA** with `roles/secretmanager.secretAccessor` on those secrets and a
   `roles/iam.workloadIdentityUser` binding for
   `serviceAccount:<project>.svc.id.goog[terraform-state-manager/tsm-terraform-state-manager]`.
6. Cluster add-ons: Secrets Store CSI Driver + the GCP provider
   (`secrets-store-csi-driver-provider-gcp`), cert-manager with
   `--enable-gateway-api`.
