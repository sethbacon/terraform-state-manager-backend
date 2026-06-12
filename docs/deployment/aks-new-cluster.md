# AKS deployment — new cluster, end to end

Assumes [aks-prerequisites.md](aks-prerequisites.md) is done (or
`deployments/terraform/azure` applied — its outputs name the placeholders
they fill).

## 1. Install cluster components

**ALB Controller** (drives AGfC via Gateway API):

```bash
az identity create -n tsm-alb-controller -g $RG
export ALB_CLIENT_ID=$(az identity show -n tsm-alb-controller -g $RG --query clientId -o tsv)
az role assignment create --assignee $ALB_CLIENT_ID \
  --role "AppGw for Containers Configuration Manager" --scope $ALB_ID
az identity federated-credential create -n alb-controller-sa \
  --identity-name tsm-alb-controller -g $RG \
  --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:azure-alb-system:alb-controller-sa" \
  --audiences api://AzureADTokenExchange

helm install alb-controller oci://mcr.microsoft.com/application-lb/charts/alb-controller \
  --namespace azure-alb-system --create-namespace \
  --set albController.namespace=azure-alb-system \
  --set albController.podIdentity.clientID=$ALB_CLIENT_ID
```

**cert-manager** with Gateway API support:

```bash
helm repo add jetstack https://charts.jetstack.io && helm repo update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --set "extraArgs={--enable-gateway-api}"
```

## 2. Fill in the values file

Copy `deployments/helm/values-aks.yaml` and replace every `<PLACEHOLDER>`
(ACR, hostname, Postgres FQDN, Key Vault name, tenant ID, managed-identity
client ID, AGfC resource ID, ops email; Entra OIDC issuer/client when using
SSO).

## 3. Install

```bash
helm upgrade --install tsm ./deployments/helm \
  --namespace terraform-state-manager --create-namespace \
  -f my-values-aks.yaml
```

What comes up: `tsm-terraform-state-manager-backend` (3 replicas, HPA),
`…-worker` (exactly 1), `…-frontend` (2), Gateway + HTTPRoutes, cert-manager
issuers + Certificate, SecretProviderClass syncing Key Vault →
`tsm-terraform-state-manager` Secret.

## 4. Verify

```bash
kubectl -n terraform-state-manager get pods
kubectl -n terraform-state-manager get gateway,httproute,certificate
kubectl -n terraform-state-manager port-forward svc/tsm-terraform-state-manager-backend 8080:8080 &
curl -s localhost:8080/health && curl -s localhost:8080/ready
```

The Certificate starts with `letsencrypt-staging` (untrusted). Once it's
`Ready=True` and DNS resolves through AGfC, switch to production issuance:

```bash
helm upgrade tsm ./deployments/helm -n terraform-state-manager \
  -f my-values-aks.yaml --set gatewayAPI.certManagerIssuer=letsencrypt-prod
kubectl -n terraform-state-manager delete secret terraform-state-manager-tls  # force re-issue
```

## 5. First login + smoke

Follow [initial-setup.md](../initial-setup.md) (Entra login → admin role via
group mapping or default role; add a state source; run the e2e checklist:
browse a state, dispatch or ingest a drift run, create an API key and call
`GET /api/v1/sources` with it).

Day-2 (upgrades, scaling, cert renewals, troubleshooting):
[aks-operations.md](aks-operations.md).
