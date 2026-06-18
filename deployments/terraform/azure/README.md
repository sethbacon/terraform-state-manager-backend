# AKS landing zone (Terraform)

Provisions every Azure resource the Helm chart's `values-aks.yaml` expects:
resource group, ACR, AKS (OIDC issuer + Workload Identity + Key Vault CSI
add-on, Azure CNI with NetworkPolicy), PostgreSQL Flexible Server + database,
Key Vault with `jwt-secret` / `encryption-key` / `database-password`,
Application Gateway for Containers + frontend, the app's user-assigned
identity (Key Vault Secrets User + federated credential bound to the chart's
ServiceAccount), and the ALB-controller identity.

```bash
terraform init
terraform apply \
  -var acr_name=<globally-unique> \
  -var key_vault_name=<globally-unique> \
  -var postgres_admin_password="$(openssl rand -base64 24)" \
  -var jwt_secret="$(openssl rand -hex 32)" \
  -var encryption_key="$(openssl rand -hex 32)"
```

`acr_name` and `key_vault_name` must each be **globally unique** (across all of
Azure). The ACR name is **alphanumeric only** (no hyphens), and the Key Vault
name must be **3–24 characters**; a first apply commonly fails on ACR name
validation if these are not met.

Then continue with [docs/deployment/aks-new-cluster.md](../../../docs/deployment/aks-new-cluster.md)
from the "Install cluster components" step — the outputs of this module map
1:1 onto the `values-aks.yaml` placeholders (each output's description names
its target).

> **Escrow `encryption_key`.** Losing it orphans every credential the app has
> stored (state sources, CI tokens). See docs/disaster-recovery.md.

Production hardening left to you: VNet-integrated PostgreSQL (this module
uses the allow-azure-services firewall rule), private cluster, and Azure
Monitor/Defender settings.
