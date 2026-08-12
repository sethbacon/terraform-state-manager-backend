# Secrets rotation

| Secret | Rotation procedure | Impact |
|---|---|---|
| `TSM_JWT_SECRET` | Set new value (Key Vault → restart pods) | All sessions invalidated; users just log in again. API keys are bcrypt-hashed in the DB and are **not** affected |
| `TSM_DATABASE_PASSWORD` | Rotate on the server, update secret, restart | Brief reconnect blips |
| OIDC client secret | New secret in the IdP, update `oidc-client-secret`, restart | New logins only |
| LDAP bind password | Same pattern | |
| API keys (`tsm_…`) | Self-service rotate in `/admin/apikeys` (0–72h grace overlap) | Update the consumer during the grace window |
| TLS certs | cert-manager auto-renews (chart); certbot renew (binary) | None |
| `TSM_ENCRYPTION_KEY` | **Special** — see [disaster-recovery.md](disaster-recovery.md): new key, restart, re-enter all stored credentials | Manual credential re-entry |
| `TSM_SUITE_SERVICE_TOKEN` | **Lockstep** — set the SAME new value on **both** this app and the sibling registry (its matching suite service token), then restart both. Rated *High* sensitivity by the [threat model](threat-model.md); see [ADR-001](adr/001-suite-coupling-shared-identity.md) for the coordination hazard | Cross-app reads (`/consumers`, `/audit/ingest`) return 401 until both sides carry the matching token; a leaked-but-unrotated token stays valid on whichever side was not updated |

Kubernetes mechanics: rotating a Key Vault/Secrets Manager value updates the
synced k8s Secret, but env vars are read at process start —
`kubectl rollout restart deploy -n terraform-state-manager` after every
rotation.

## One-time: bind stored secrets to their rows

Not a rotation — a **one-time migration per deployment**. Skipping it breaks
nothing, but the protection does not reach your existing rows until you run it.

A stored notification-channel target used to carry no indication of *which*
channel it belonged to, so anyone able to write to the database could copy one
channel's encrypted target into another and AES-GCM would accept it. Targets are
now bound to their own row, and a moved value fails to decrypt. Delivery
tolerates both forms, so this is safe to run at any time — and safe to defer.

```bash
# Convert every target that is not yet bound.
kubectl exec deploy/terraform-state-manager -- /app/terraform-state-manager bind-targets

# Report what remains, writing nothing.
kubectl exec deploy/terraform-state-manager -- /app/terraform-state-manager bind-targets verify
```

Needs `TSM_ENCRYPTION_KEY` (and `TSM_ENCRYPTION_KEY_PREVIOUS`, if set) — the same
values the server runs with. The re-encryption happens in the application because
AES-GCM needs the key; no SQL migration can do it.

Safe to re-run and safe to interrupt: an already-bound row is skipped rather than
re-encrypted, so an interrupted run resumes by running it again. `verify` writes
nothing.

A row reported `failed` could not be decrypted at all (wrong key, or corruption)
— a pre-existing problem this command did not cause and cannot repair. It is
reported and stepped over so the rest still convert; that channel's target has to
be re-entered.

`verify` exits **non-zero** while any row is still unbound, so it can gate a
runbook step rather than relying on someone reading output. Until it reports
zero, the service must keep accepting unbound targets — which is the weakness
being retired — so a future version that requires bound values is only safe to
adopt once `verify` passes.
