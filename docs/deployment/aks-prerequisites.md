# AKS prerequisites

Everything the AKS deployment (Helm `values-aks.yaml` or the
`overlays/aks` kustomization) expects to exist. The Terraform module
`deployments/terraform/azure` provisions all of it — use it
([README](../../deployments/terraform/azure/README.md)) or create the pieces
manually as below.

## Tooling

`az` (≥2.60), `kubectl`, `helm` (≥3.14), `docker`. Log in: `az login`.

## 1. Resource group + ACR

```bash
export RG=rg-terraform-state-manager LOCATION=eastus2 ACR=<globally-unique>
az group create -n $RG -l $LOCATION
az acr create -n $ACR -g $RG --sku Standard
```

Build and push the images:

```bash
az acr login -n $ACR
docker build -t $ACR.azurecr.io/terraform-state-manager-backend:v1.0.0 backend/
docker build -t $ACR.azurecr.io/terraform-state-manager-frontend:v1.0.0 \
  ../terraform-state-manager-frontend/frontend/
docker push $ACR.azurecr.io/terraform-state-manager-backend:v1.0.0
docker push $ACR.azurecr.io/terraform-state-manager-frontend:v1.0.0
```

## 2. AKS cluster (required features)

The cluster needs: **OIDC issuer**, **Workload Identity**, the **Key Vault
Secrets Store CSI add-on**, and an enforcing CNI (the chart ships
NetworkPolicies — kubenet ignores them).

```bash
az aks create -n tsm-aks -g $RG \
  --enable-oidc-issuer --enable-workload-identity \
  --enable-addons azure-keyvault-secrets-provider \
  --network-plugin azure --network-policy azure \
  --node-count 3 --node-vm-size Standard_D4s_v5 \
  --attach-acr $ACR
az aks get-credentials -n tsm-aks -g $RG
export OIDC_ISSUER=$(az aks show -n tsm-aks -g $RG --query oidcIssuerProfile.issuerUrl -o tsv)
```

## 3. Azure Database for PostgreSQL Flexible Server

```bash
az postgres flexible-server create -n tsm-pg -g $RG -l $LOCATION \
  --version 16 --tier GeneralPurpose --sku-name Standard_D2ds_v5 \
  --storage-size 64 --admin-user tsm --admin-password "<strong-password>"
az postgres flexible-server db create -s tsm-pg -g $RG -d terraform_state_manager
# Networking: allow the AKS egress (or use VNet integration / private endpoint)
```

### Alternative: in-cluster PostgreSQL with CloudNativePG

The chart never bundles a database — it connects to whatever `externalDatabase.host`
points at, so the database can live **outside or inside** the cluster. Managed Flexible
Server (above) is the recommended, lowest-ops option. If you instead need PostgreSQL to
**run inside AKS** (no managed service, in-cluster data residency, or cost control), run it
with the [CloudNativePG](https://cloudnative-pg.io/) operator and point the chart at it.

```bash
# 1. Install the operator (mirror the image into your ACR for locked-down clusters)
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm upgrade --install cnpg cnpg/cloudnative-pg \
  --namespace cnpg-system --create-namespace

# 2. Create a cluster (single instance shown; raise `instances` + add backups for production)
kubectl apply -f - <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: terraform-state-manager-pg
  namespace: terraform-state-manager
spec:
  instances: 1
  storage:
    size: 64Gi
    storageClass: managed-csi-premium   # block volume — do NOT use Azure Files for PGDATA
  bootstrap:
    initdb:
      database: terraform_state_manager
      owner: tsm
EOF
```

Then point the chart at the CloudNativePG read-write Service instead of a Flexible Server FQDN:

```yaml
externalDatabase:
  host: "terraform-state-manager-pg-rw.terraform-state-manager.svc.cluster.local"
  port: 5432
  name: "terraform_state_manager"
  user: "tsm"
  sslMode: "require"
  existingSecret: "<db-secret>"   # wire in CloudNativePG's generated role password (its "-app" Secret)
```

**You take on what the managed service handled:** backups (configure CloudNativePG's
Barman backup to Azure Blob for point-in-time recovery), minor/patch updates (bump the
cluster `imageName`; the operator does a rolling update), and major-version upgrades
(deliberate — never automatic). Always use a managed-disk (block) `storageClass`, never
Azure Files, for `PGDATA`.

## 4. Key Vault + the three app secrets

Secret NAMES are what the chart's SecretProviderClass expects.

```bash
export KV=<globally-unique>
# --enable-purge-protection true matches the Terraform module (recommended for
# production: protects the escrowed encryption-key from accidental/early deletion).
az keyvault create -n $KV -g $RG --enable-rbac-authorization true --enable-purge-protection true
az role assignment create --assignee $(az ad signed-in-user show --query id -o tsv) \
  --role "Key Vault Administrator" --scope $(az keyvault show -n $KV --query id -o tsv)

az keyvault secret set --vault-name $KV -n jwt-secret        --value "$(openssl rand -hex 32)"
az keyvault secret set --vault-name $KV -n encryption-key    --value "$(openssl rand -hex 32)"
az keyvault secret set --vault-name $KV -n database-password --value "<strong-password>"
# OIDC only:
# az keyvault secret set --vault-name $KV -n oidc-client-secret --value "<app-reg-secret>"
```

> **Escrow `encryption-key`.** Losing it orphans every stored source/CI
> credential ([disaster-recovery.md](../disaster-recovery.md)).

## 5. Managed identity + federated credential

The chart's pods read Key Vault through a user-assigned managed identity
bound to the chart's ServiceAccount (`tsm-terraform-state-manager` in
namespace `terraform-state-manager` for a release named `tsm`).

```bash
az identity create -n tsm-app-identity -g $RG
export APP_CLIENT_ID=$(az identity show -n tsm-app-identity -g $RG --query clientId -o tsv)
az role assignment create --assignee $APP_CLIENT_ID --role "Key Vault Secrets User" \
  --scope $(az keyvault show -n $KV --query id -o tsv)
az identity federated-credential create -n tsm-chart-sa \
  --identity-name tsm-app-identity -g $RG \
  --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:terraform-state-manager:tsm-terraform-state-manager" \
  --audiences api://AzureADTokenExchange
```

## 6. Application Gateway for Containers (AGfC)

```bash
az network alb create -n tsm-agfc -g $RG
az network alb frontend create -n tsm-frontend -g $RG --alb-name tsm-agfc
export ALB_ID=$(az network alb show -n tsm-agfc -g $RG --query id -o tsv)
export AGFC_FQDN=$(az network alb frontend show -n tsm-frontend -g $RG --alb-name tsm-agfc \
  --query properties.fqdn -o tsv)
```

Create a DNS CNAME for your hostname → `$AGFC_FQDN`.

## 7. Entra ID app registration (OIDC login)

See [initial-setup.md](../initial-setup.md#entra-id-oidc) — you'll need the
tenant ID, the app's client ID, a client secret (→ Key Vault
`oidc-client-secret`), redirect URI
`https://<hostname>/api/v1/auth/callback`, and a `groups` claim.

> **Entra/Azure AD:** set `auth.oidc.requireVerifiedEmail=false` (chart default
> is `true`). Entra omits the `email_verified` claim, so leaving the default on
> makes the **first login fail with `email_not_verified`**. Referenced again at
> [aks-new-cluster.md](aks-new-cluster.md) §5 (first login).

Continue with [aks-new-cluster.md](aks-new-cluster.md) (cluster components +
helm install) or [aks-existing-cluster.md](aks-existing-cluster.md).
