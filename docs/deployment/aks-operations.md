# AKS operations

## Upgrades (app)

```bash
docker build -t $ACR.azurecr.io/terraform-state-manager-backend:v1.1.0 backend/ && docker push …
helm upgrade tsm ./deployments/helm -n terraform-state-manager \
  -f my-values-aks.yaml \
  --set backend.image.tag=v1.1.0 --set frontend.image.tag=v1.1.0
```

Database migrations run on pod start under advisory locks; rolling API
replicas are safe. The worker uses `Recreate`, so there is a short
(seconds-long) window with no schedule firing during upgrades — schedules due
in that window fire on the next 60s tick. For cautious upgrades run
`terraform-state-manager migrate up` from a one-off pod first, then roll
images. See [upgrade-guide.md](../upgrade-guide.md).

## Scaling

- API: `--set backend.replicaCount=N` or the HPA
  (`autoscaling.minReplicas/maxReplicas`). Database pool is per replica
  (`TSM_DATABASE_MAX_CONNECTIONS`, default 25) — size Postgres
  `max_connections` ≥ replicas × 25 + worker + headroom.
- Worker: **never scale past 1**. The chart pins it; don't override.
- Frontend: `--set frontend.replicaCount=N` — stateless.

## Certificates

cert-manager renews automatically (~30 days before expiry). Check:

```bash
kubectl -n terraform-state-manager get certificate,certificaterequest
kubectl -n terraform-state-manager describe certificate tsm-terraform-state-manager-tls
```

Stuck HTTP-01 challenges are almost always DNS not pointing at the AGfC
frontend FQDN, or the Gateway not Programmed (check ALB controller logs in
`azure-alb-system`).

## Secret rotation

Key Vault values sync into the k8s Secret when pods (re)start mounting the
CSI volume; the add-on's rotation poll also refreshes them (~2m), but env
vars are read at process start — **restart the deployments after rotating**:

```bash
kubectl -n terraform-state-manager rollout restart deploy
```

Caveats for `TSM_ENCRYPTION_KEY` in [secrets-rotation.md](../secrets-rotation.md)
(rotating it requires re-entering stored credentials).

## Health and diagnostics

```bash
kubectl -n terraform-state-manager get pods -o wide
kubectl -n terraform-state-manager logs deploy/tsm-terraform-state-manager-worker | tail
kubectl -n terraform-state-manager describe gateway
# Readiness flapping = DB connectivity; /health is DB-independent:
kubectl -n terraform-state-manager exec deploy/tsm-terraform-state-manager-backend -- \
  wget -qO- http://localhost:8080/ready
```

Worker silently down ⇒ schedules + periodic sync stop while the API stays
green. The `TSMTargetDown` alert covers it; manually:
`kubectl get deploy tsm-terraform-state-manager-worker`.

More in [troubleshooting.md](../troubleshooting.md).
