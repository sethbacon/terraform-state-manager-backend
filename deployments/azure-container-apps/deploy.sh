#!/usr/bin/env bash
# Deploy Terraform State Manager to Azure Container Apps.
# Prereqs: az CLI logged in; resource group + Azure Database for PostgreSQL
# Flexible Server provisioned; images pushed to ACR; parameters.json filled in
# (or pass -p overrides). See docs/deployment/azure-container-apps.md.
set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:?set RESOURCE_GROUP}"

az deployment group create \
  --resource-group "$RESOURCE_GROUP" \
  --template-file "$(dirname "$0")/main.bicep" \
  --parameters "@$(dirname "$0")/parameters.json" \
  "$@"

echo
echo "Frontend URL:"
az deployment group show -g "$RESOURCE_GROUP" -n main \
  --query properties.outputs.frontendUrl.value -o tsv
