#!/usr/bin/env bash
# Deploy Terraform State Manager to ECS Fargate via CloudFormation.
# Prereqs: aws CLI; VPC with public+private subnets; RDS PostgreSQL; ACM cert;
# Secrets Manager secret (JSON) with TSM_DATABASE_PASSWORD, TSM_JWT_SECRET,
# TSM_ENCRYPTION_KEY; images in ECR. See docs/deployment/aws-ecs.md.
set -euo pipefail

STACK_NAME="${STACK_NAME:-terraform-state-manager}"

aws cloudformation deploy \
  --stack-name "$STACK_NAME" \
  --template-file "$(dirname "$0")/cloudformation.yaml" \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
    VpcId="${VPC_ID:?}" \
    PrivateSubnetIds="${PRIVATE_SUBNET_IDS:?}" \
    PublicSubnetIds="${PUBLIC_SUBNET_IDS:?}" \
    BackendImage="${BACKEND_IMAGE:?}" \
    FrontendImage="${FRONTEND_IMAGE:?}" \
    DatabaseHost="${DATABASE_HOST:?}" \
    DatabaseSecretArn="${DATABASE_SECRET_ARN:?}" \
    PublicUrl="${PUBLIC_URL:?}" \
    CertificateArn="${CERTIFICATE_ARN:?}" \
  "$@"

aws cloudformation describe-stacks --stack-name "$STACK_NAME" \
  --query 'Stacks[0].Outputs' --output table
