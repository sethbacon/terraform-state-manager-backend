# EKS prerequisites

`deployments/terraform/aws` provisions all of this
([README](../../deployments/terraform/aws/README.md)); manual equivalents:

1. **ECR** repos `terraform-state-manager-backend` / `-frontend`; build & push
   (`aws ecr get-login-password | docker login …`).
2. **EKS** ≥1.29 with the OIDC provider associated (IRSA):
   `eksctl utils associate-iam-oidc-provider --cluster <c> --approve`.
3. **RDS PostgreSQL 16** reachable from the cluster SG, db
   `terraform_state_manager`, user `tsm`.
4. **Secrets Manager** secrets (names match the chart's ASCP objects):
   `tsm/jwt-secret`, `tsm/encryption-key` (both `openssl rand -hex 32`;
   **escrow the encryption key**), `tsm/database-password`.
5. **IRSA role** with `secretsmanager:GetSecretValue` on those three ARNs,
   trust policy bound to
   `system:serviceaccount:terraform-state-manager:tsm-terraform-state-manager`.
6. Cluster add-ons:
   - **Gateway API CRDs** (`gateway.networking.k8s.io/v1`) — the chart renders
     `Gateway`/`HTTPRoute` objects that depend on them; not present on a fresh
     cluster unless the AWS Load Balancer Controller install provides them:
     `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.1.0/standard-install.yaml`
   - AWS Load Balancer Controller **with Gateway API support** (`aws-alb-external`)
   - Secrets Store CSI Driver + AWS provider (ASCP):
     `helm install csi-secrets-store secrets-store-csi-driver/secrets-store-csi-driver -n kube-system`
     and the `aws-secrets-manager/secrets-store-csi-driver-provider-aws` chart
   - cert-manager with `--enable-gateway-api`
7. ACM is NOT used by the chart path (cert-manager issues via Let's Encrypt);
   if you prefer ACM, terminate at the ALB and set `gatewayAPI` TLS
   accordingly.
