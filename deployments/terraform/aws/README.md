# EKS landing zone (Terraform)

EKS (OIDC/IRSA enabled) + ECR + RDS PostgreSQL + Secrets Manager secrets
(`tsm/jwt-secret`, `tsm/encryption-key`, `tsm/database-password`) + the IRSA
role the chart's ServiceAccount assumes for ASCP secret reads. Outputs map
onto `deployments/helm/values-eks.yaml`.

Bring your own VPC (pass `vpc_id` + `private_subnet_ids`). After apply,
install AWS Load Balancer Controller (Gateway API support), Secrets Store CSI
Driver + ASCP, and cert-manager per
[docs/deployment/eks-deployment.md](../../../docs/deployment/eks-deployment.md).

> **Escrow `encryption_key`** — losing it orphans stored credentials.
