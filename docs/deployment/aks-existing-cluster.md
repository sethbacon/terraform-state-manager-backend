# AKS deployment — existing cluster

Deploying into a cluster you already run. Differences from the
[new-cluster path](aks-new-cluster.md):

## Cluster feature checklist

```bash
az aks show -n <cluster> -g <rg> --query \
  "{oidc:oidcIssuerProfile.enabled, wi:securityProfile.workloadIdentity.enabled, kv:addonProfiles.azureKeyvaultSecretsProvider.enabled, np:networkProfile.networkPolicy}"
```

Enable anything missing:

```bash
az aks update -n <cluster> -g <rg> --enable-oidc-issuer --enable-workload-identity
az aks enable-addons -n <cluster> -g <rg> --addons azure-keyvault-secrets-provider
```

- **NetworkPolicy**: if the cluster runs kubenet (no enforcement), set
  `networkPolicy.enabled=false` in your values — the resources would be
  silently ignored and mislead audits.
- **ALB Controller / cert-manager**: skip installs if already present; just
  confirm cert-manager runs with `--enable-gateway-api`.
- **Existing ingress stack instead of AGfC?** Set `gatewayAPI.enabled=false`,
  `ingress.enabled=true`, `ingress.className=<your-class>` and bring your own
  TLS secret. The chart's nginx-Ingress path routes everything to the
  frontend, whose nginx proxies API paths onward.
- **Shared cluster hygiene**: the chart creates namespace-scoped resources
  except the GatewayClass and the two ClusterIssuers (cluster-scoped, only
  when `gatewayAPI.enabled=true`). If another team owns Let's Encrypt
  issuers, point `gatewayAPI.certManagerIssuer` at theirs — the chart's
  issuers are create-if-rendered, so coordinate names.

## Identity wiring

The federated credential subject must match YOUR release name/namespace:
`system:serviceaccount:<namespace>:<release>-terraform-state-manager`
(or set `serviceAccount.name` explicitly and use that). Re-run the
`az identity federated-credential create` step from the prerequisites with
the right subject.

Then continue from step 2 of [aks-new-cluster.md](aks-new-cluster.md).
