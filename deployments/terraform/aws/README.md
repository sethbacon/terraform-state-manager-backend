# EKS landing zone (Terraform)

EKS (OIDC/IRSA enabled) + ECR + RDS PostgreSQL + Secrets Manager secrets
(`tsm/jwt-secret`, `tsm/encryption-key`, `tsm/database-password`) + the IRSA
role the chart's ServiceAccount assumes for ASCP secret reads. Outputs map
onto `deployments/helm/values-eks.yaml`.

Bring your own VPC (pass `vpc_id` + `private_subnet_ids`).

```bash
terraform init
terraform apply \
  -var vpc_id=<existing-vpc> \
  -var 'private_subnet_ids=["subnet-aaa","subnet-bbb"]' \
  -var db_password="$(openssl rand -base64 24)" \
  -var jwt_secret="$(openssl rand -hex 32)" \
  -var encryption_key="$(openssl rand -hex 32)"
```

After apply, install AWS Load Balancer Controller (Gateway API support),
Secrets Store CSI Driver + ASCP, and cert-manager per
[docs/deployment/eks-deployment.md](../../../docs/deployment/eks-deployment.md).

**Required inputs:** `vpc_id`, `private_subnet_ids`, and the sensitive
`db_password` / `jwt_secret` / `encryption_key`. Notable defaults:
`kubernetes_version=1.30`, `node_instance_type=m6i.xlarge`, `node_count=3`,
`db_instance_class=db.m6g.large`, `region=us-east-1`.

**Outputs** (each maps onto `values-eks.yaml` — see its description):
`ecr_backend_url`, `ecr_frontend_url`, `rds_endpoint`, `irsa_role_arn`,
`cluster_name`, `kubeconfig_command`.

> **Escrow `encryption_key`** — losing it orphans stored credentials.
