# Deployment overview

Terraform State Manager ships as two containers (backend API + frontend SPA)
plus a PostgreSQL database. Every production topology also runs **exactly one
worker replica** (the backend image with `TSM_WORKERS_ENABLED=true`) for the
schedule runner and the state-sync loop — see
[deployment/README.md](deployment/README.md#worker-topology) for why.

| Method | Best for | Artifacts | Guide |
|---|---|---|---|
| **Helm on AKS** (primary) | Production on Azure | `deployments/helm` + `values-aks.yaml` | [aks-new-cluster.md](deployment/aks-new-cluster.md) / [aks-existing-cluster.md](deployment/aks-existing-cluster.md) |
| Helm on EKS | Production on AWS | `deployments/helm` + `values-eks.yaml` | [eks-deployment.md](deployment/eks-deployment.md) |
| Helm on GKE | Production on GCP | `deployments/helm` + `values-gke.yaml` | [gke-deployment.md](deployment/gke-deployment.md) |
| Kustomize | GitOps-flavored k8s installs | `deployments/kubernetes` (base + 5 overlays) | [deployment/README.md](deployment/README.md) |
| Azure Container Apps | Serverless Azure, no cluster | `deployments/azure-container-apps` (Bicep) | [azure-container-apps.md](deployment/azure-container-apps.md) |
| AWS ECS Fargate | Serverless AWS | `deployments/aws-ecs` (CloudFormation) | [aws-ecs.md](deployment/aws-ecs.md) |
| Google Cloud Run | Serverless GCP | `deployments/google-cloud-run` | [google-cloud-run.md](deployment/google-cloud-run.md) |
| Docker Compose (prod) | Single host, small teams | `deployments/docker-compose.prod.yml` | [docker-compose-production.md](deployment/docker-compose-production.md) |
| systemd binary | VMs without containers | `deployments/binary` | [binary-install.md](deployment/binary-install.md) |
| Terraform IaC | Provisioning the cloud resources above | `deployments/terraform/{azure,aws,gcp}` | per-module READMEs |

Cross-cutting references:

- [configuration.md](configuration.md) — every `TSM_*` environment variable
- [initial-setup.md](initial-setup.md) — secrets generation, first login, Entra ID OIDC
- [observability.md](observability.md) — metrics, alerts, dashboards
- [disaster-recovery.md](disaster-recovery.md) — backups, restore, **encryption-key custody**
- [secrets-rotation.md](secrets-rotation.md)
- [upgrade-guide.md](upgrade-guide.md)
- [troubleshooting.md](troubleshooting.md)

The development stack (Keycloak, seeded states, DEV_MODE) lives in the
frontend repo at `terraform-state-manager-frontend/deployments/` and is NOT a
production artifact.
