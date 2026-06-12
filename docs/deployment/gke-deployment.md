# GKE deployment

After [gke-prerequisites.md](gke-prerequisites.md):

1. Copy `deployments/helm/values-gke.yaml`; replace `<PROJECT_ID>`,
   `<REGION>`, `<GSA_NAME>`, `<CLOUD_SQL_IP_OR_DNS>`, `<HOSTNAME>`,
   `<OPS_EMAIL>`, image tags. Note `gatewayAPI.createGatewayClass=false` —
   GKE pre-provisions `gke-l7-global-external-managed`.
2. ```bash
   helm upgrade --install tsm ./deployments/helm \
     --namespace terraform-state-manager --create-namespace \
     -f my-values-gke.yaml
   ```
3. The Gateway allocates a global external IP
   (`kubectl -n terraform-state-manager get gateway`); point DNS at it.
4. Verify per [aks-new-cluster.md](aks-new-cluster.md#4-verify).

Kustomize alternative: `deployments/kubernetes/overlays/gke`.
