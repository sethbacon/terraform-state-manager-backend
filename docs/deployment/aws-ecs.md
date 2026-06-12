# AWS ECS (Fargate)

`deployments/aws-ecs` — CloudFormation stack: cluster (Service Connect
namespace `tsm`), backend service (scalable, behind an ALB), worker service
(`DesiredCount: 1`, `MaximumPercent: 100` so deploys never run two workers),
frontend service, HTTPS listener with path rules
(`/api/* /scim/* /health /ready /swagger*` → backend, default → frontend).

**Service Connect** publishes the alias `backend:8080`, which is exactly the
upstream baked into the frontend image's nginx — no image changes.

```bash
# Prereqs: VPC (public+private subnets), RDS PostgreSQL, ACM cert for your
# hostname, ECR images, and ONE Secrets Manager secret with JSON keys
# TSM_DATABASE_PASSWORD, TSM_JWT_SECRET, TSM_ENCRYPTION_KEY:
aws secretsmanager create-secret --name tsm/app --secret-string \
  "{\"TSM_DATABASE_PASSWORD\":\"…\",\"TSM_JWT_SECRET\":\"$(openssl rand -hex 32)\",\"TSM_ENCRYPTION_KEY\":\"$(openssl rand -hex 32)\"}"

cd deployments/aws-ecs
VPC_ID=… PRIVATE_SUBNET_IDS=subnet-a,subnet-b PUBLIC_SUBNET_IDS=subnet-c,subnet-d \
BACKEND_IMAGE=… FRONTEND_IMAGE=… DATABASE_HOST=… \
DATABASE_SECRET_ARN=… PUBLIC_URL=https://tsm.example.com CERTIFICATE_ARN=… \
./deploy.sh
```

Point DNS at the `LoadBalancerDNS` output. Scale the API with
`aws ecs update-service --service backend --desired-count N`; never scale
`worker`.
