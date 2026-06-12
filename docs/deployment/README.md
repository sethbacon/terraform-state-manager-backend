# Kubernetes deployment

Two equivalent packagings:

- **Helm** (`deployments/helm`) — recommended; one chart deploys backend,
  worker, and frontend with per-cloud values files (`values-aks.yaml`,
  `values-eks.yaml`, `values-gke.yaml`).
- **Kustomize** (`deployments/kubernetes`) — `base/` plus overlays `dev`,
  `production`, `aks`, `eks`, `gke` for teams standardizing on
  `kubectl apply -k` / GitOps.

## Worker topology

The backend runs two periodic loops: the **schedule runner** (fires due drift
schedules every 60s) and the **state-sync reconciler** (keeps the analysis
store current every 5m). The schedule runner has **no cross-replica claim** —
if two replicas run it, due schedules dispatch twice. Therefore:

- API replicas run with `TSM_WORKERS_ENABLED=false` and can scale/HPA freely.
- Exactly **one** worker replica runs with `TSM_WORKERS_ENABLED=true`
  (`Recreate` strategy so rollouts never overlap two workers).
- On-demand syncs (post-edit refresh, source-create backfill) work on every
  replica regardless — only the periodic loops are gated.

Both the chart (`workers.dedicated`, default `true`) and the kustomize base
encode this. Single-replica installs may run everything in one pod
(`workers.dedicated=false` / env default `true`) — never combine that with
more than one backend replica.

## Ingress architecture

Two options, mutually exclusive:

1. **Gateway API** (recommended; what the aks/eks/gke profiles use):
   `Gateway` + `HTTPRoute`s send `/api/*`, `/scim/*`, `/health`, `/ready`,
   `/swagger*` straight to the backend Service and everything else to the
   frontend; cert-manager solves ACME HTTP-01 through the Gateway.
   - AKS: Application Gateway for Containers (`azure-alb-external`)
   - EKS: AWS Load Balancer Controller (`aws-alb-external`)
   - GKE: built-in Gateway controller (`gke-l7-global-external-managed`)
2. **Legacy nginx Ingress**: a single catch-all rule to the frontend, whose
   nginx proxies API paths to the backend.

Either way, the **frontend's nginx config is supplied by the
chart/kustomize** (templated to the in-cluster backend Service DNS): the
config baked into the image targets the docker-compose service name and is
overridden by a mounted ConfigMap.

## Secrets

Never put real secrets in values files or the kustomize placeholder Secret.
Use the cloud secret store + Secrets Store CSI Driver (Key Vault / Secrets
Manager / Secret Manager — wired in the cloud profiles) or a pre-created
Kubernetes Secret referenced by `security.existingSecret`. Required keys:
`TSM_JWT_SECRET`, `TSM_ENCRYPTION_KEY`, `TSM_DATABASE_PASSWORD` (and
`TSM_AUTH_OIDC_CLIENT_SECRET` when OIDC is enabled).

## CI callback reachability

Drift and Version Lab runs end with the CI job POSTing results to
`TSM_SERVER_CALLBACK_URL` (`server.callbackUrl`). GitHub-hosted and
ADO Microsoft-hosted runners need that endpoint to be **publicly reachable**;
self-hosted runners need a network path to it. The drift wizard's callback
preflight warns when it looks unreachable.
