# Secrets rotation

| Secret | Rotation procedure | Impact |
|---|---|---|
| `TSM_JWT_SECRET` | Set new value (Key Vault → restart pods) | All sessions invalidated; users just log in again. API keys are bcrypt-hashed in the DB and are **not** affected |
| `TSM_DATABASE_PASSWORD` | Rotate on the server, update secret, restart | Brief reconnect blips |
| OIDC client secret | New secret in the IdP, update `oidc-client-secret`, restart | New logins only |
| LDAP bind password | Same pattern | |
| API keys (`tsm_…`) | Self-service rotate in `/apikeys` (0–72h grace overlap) | Update the consumer during the grace window |
| TLS certs | cert-manager auto-renews (chart); certbot renew (binary) | None |
| `TSM_ENCRYPTION_KEY` | **Special** — see [disaster-recovery.md](disaster-recovery.md): new key, restart, re-enter all stored credentials | Manual credential re-entry |

Kubernetes mechanics: rotating a Key Vault/Secrets Manager value updates the
synced k8s Secret, but env vars are read at process start —
`kubectl rollout restart deploy -n terraform-state-manager` after every
rotation.
