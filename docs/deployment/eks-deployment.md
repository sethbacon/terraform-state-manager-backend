# EKS deployment

After [eks-prerequisites.md](eks-prerequisites.md):

1. Copy `deployments/helm/values-eks.yaml`, replace `<AWS_ACCOUNT_ID>`,
   `<REGION>`, `<TSM_IRSA_ROLE>`, `<RDS_ENDPOINT>`, `<HOSTNAME>`,
   `<OPS_EMAIL>`, image tags.
2. ```bash
   helm upgrade --install tsm ./deployments/helm \
     --namespace terraform-state-manager --create-namespace \
     -f my-values-eks.yaml
   ```
3. Point DNS at the ALB the Gateway provisions
   (`kubectl -n terraform-state-manager get gateway -o wide`).
4. Verify exactly as in [aks-new-cluster.md](aks-new-cluster.md#4-verify)
   (same probes, same staging→prod issuer switch).

Kustomize alternative: `deployments/kubernetes/overlays/eks` (same
placeholders; `kubectl apply -k`).

EKS notes: the IRSA annotation rides `serviceAccount.annotations` (Azure
workloadIdentity stays disabled); pod security contexts are compatible with
both Amazon Linux node AMIs and Bottlerocket; NetworkPolicy requires the VPC
CNI's network policy support (or Calico) — disable `networkPolicy.enabled`
if you run neither.
