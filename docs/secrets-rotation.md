# Secrets rotation

| Secret | Rotation procedure | Impact |
|---|---|---|
| `TSM_JWT_SECRET` | Set new value (Key Vault → restart pods) | All sessions invalidated; users just log in again. API keys are bcrypt-hashed in the DB and are **not** affected |
| `TSM_DATABASE_PASSWORD` | Rotate on the server, update secret, restart | Brief reconnect blips |
| OIDC client secret | New secret in the IdP, update `oidc-client-secret`, restart | New logins only |
| LDAP bind password | Same pattern | |
| API keys (`tsm_…`) | Self-service rotate in `/admin/apikeys` (0–72h grace overlap) | Update the consumer during the grace window |
| TLS certs | cert-manager auto-renews (chart); certbot renew (binary) | None |
| `TSM_ENCRYPTION_KEY` | **Special** — see [Rotating `TSM_ENCRYPTION_KEY`](#rotating-tsm_encryption_key) below. Set `TSM_ENCRYPTION_KEY_PREVIOUS` to the outgoing key and **nothing needs re-entering**: notification-channel targets convert with `rekey-targets`, and every other credential is read through the previous key until it is next saved | Covered, as long as the previous key stays set |
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

`bind-targets` re-encrypts as a **side effect**, and only once. On its first run
after upgrading, every target is still unbound, so converting it also lands it on
the current key. After that every target is bound, so `bind-targets` skips them
all and re-encrypts **nothing** — which is why finishing a key rotation needs
`rekey-targets` below, not another `bind-targets` run.

## Rotating `TSM_ENCRYPTION_KEY`

Two different things are encrypted at rest, and a rotation treats them
differently. Read the [coverage table](#what-rekey-targets-verify-does-not-certify)
before planning the window.

```bash
# Re-encrypt every notification-channel target under the current key.
kubectl exec deploy/terraform-state-manager -- /app/terraform-state-manager rekey-targets

# Report what still needs the PREVIOUS key, writing nothing. Exits non-zero
# while any row does.
kubectl exec deploy/terraform-state-manager -- /app/terraform-state-manager rekey-targets verify
```

**The runbook.**

1. Generate the new key (`openssl rand -hex 32`).
2. Set `TSM_ENCRYPTION_KEY` to the **new** key and `TSM_ENCRYPTION_KEY_PREVIOUS`
   to the **old** one, in the same secret update.
3. `kubectl rollout restart deploy -n terraform-state-manager`. Notification
   delivery keeps working throughout: targets still sealed under the old key open
   through the previous-key fallback.
4. Re-enter, by hand, every credential in the
   [manual re-entry list](#what-rekey-targets-verify-does-not-certify). These are
   unreadable from step 3 onward — the fallback does not apply to them.
5. Run `rekey-targets`. Re-run it if it reports any `failed`; it is safe to
   re-run and safe to interrupt.
6. Run `rekey-targets verify`. **A zero exit is the gate.** While it exits
   non-zero, `TSM_ENCRYPTION_KEY_PREVIOUS` must stay in place.
7. Only once step 6 exits zero: delete `TSM_ENCRYPTION_KEY_PREVIOUS` and restart
   again. The old key can then be destroyed.

**Why `rekey-targets` exists at all.** `bind-targets` decides a row needs no work
by opening it under its own context, and that open falls back to the previous
key. A target that is already bound but still sealed under the old key therefore
counts as done and is skipped forever, so the previous key can never be dropped
(#364). `rekey-targets` asks the other question — *is this row readable WITHOUT
the previous key?* — using a cipher built from `TSM_ENCRYPTION_KEY` alone, which
is the only thing that can tell the two keys apart.

It also subsumes binding: a target that is both unbound and on the old key
converges to bound-and-current in one pass, so `bind-targets` is not a
prerequisite.

**What it will not do.** A target it cannot open at all — corrupt, or bound to a
*different* channel's row — is reported as `failed`, left exactly as it is, and
holds `verify` shut. It is never re-sealed into the row it was found in: that
would mint the binding an attacker who moved the value could not forge, and use
a routine rotation as the cover. Such a row must be investigated and its target
re-entered; "one row could not be read" is not evidence that the previous key can
go.

### What `rekey-targets verify` does not certify

A zero exit means: **no notification-channel target requires
`TSM_ENCRYPTION_KEY_PREVIOUS` any more.** That is exactly the set of data the
previous key protects — nothing else in this service ever reads it.

**It says nothing about the columns sealed by `internal/crypto`**, and that is a
narrower statement than it used to be. Until #368 that package read only
`TSM_ENCRYPTION_KEY`, so those values stopped decrypting at the restart in step 3
and every one had to be re-entered by hand. It now retries
`TSM_ENCRYPTION_KEY_PREVIOUS` on a failed open, so they keep working across a
rotation — but nothing re-encrypts them, so each stays on the OLD key until it
is next saved.

The practical consequence, and the reason this section changed rather than being
deleted: **a zero exit from `rekey-targets verify` does not authorise deleting
`TSM_ENCRYPTION_KEY_PREVIOUS`.** That gate covers
`notification_channels.encrypted_target` and nothing else. Every column in the
table below is still sealed under whichever key was current when it was last
written, and deleting the previous key makes those unreadable — permanently, and
with no command in this service reporting that it was about to happen.

| Encrypted at rest | Cipher | Rotation behaviour |
|---|---|---|
| `notification_channels.encrypted_target` | shared identity `TokenCipher`, bound per row, dual-key | **Covered.** Keeps working across the rotation; `rekey-targets` completes it |
| `state_sources.encrypted_credentials` | `internal/crypto`, no AAD, dual-key on read | **Keeps working** while `TSM_ENCRYPTION_KEY_PREVIOUS` is set. Not re-encrypted: converts only when the source is next saved |
| `ci_sources.encrypted_token` / `encrypted_client_secret` / `encrypted_app_private_key` | `internal/crypto` | As above |
| drift `pipeline_connections.encrypted_token` | `internal/crypto` | As above |
| `oidc_configs.client_secret_encrypted` | `internal/crypto` | As above |
| `system_settings` → SMTP password | `internal/crypto`, base64 in `smtp.password_sealed` | As above — **but** a password saved before the encoding fix was corrupted when it was written and must be re-entered regardless of any key |

### Purpose binding (#277)

Secrets written by this release onward are bound to **what they are for**, so a
ciphertext copied from one encrypted column into another no longer decrypts. The
purpose is stamped into the ciphertext, so a reader dispatches on what it finds
rather than on what it expected.

Secrets are bound from the release carrying this note onward. Reading a bound
value shipped one release earlier, so an upgrade across the pair is safe in
either order of replica restart.

**Forward only. Nothing is re-encrypted, ever.** A row converts when its secret
is next saved and not otherwise, and unbound values are read for as long as they
exist — there is no sweep, no backfill and no cutover. That is deliberate: a
sweep that derived a purpose wrongly would re-seal rows under the wrong binding,
pass its own round-trip check, and report success permanently
([terraform-registry-backend#878][reg878] is that failure, found in the sibling
app before it shipped).

So **coverage is a function of how often an administrator edits things.** On a
deployment where nobody touches a source for a year, this is closed in the
tracker and unmoved in the database. That is the honest position, and it is
preferred to the alternative, which risks making credentials unreadable.

Nothing to configure, and no operator action. It does not interact with key
rotation: a bound value is read through `TSM_ENCRYPTION_KEY_PREVIOUS` exactly as
an unbound one is.

[reg878]: https://github.com/sethbacon/terraform-registry-backend/issues/878

### Audit log retention (#373)

`audit_logs` can be bounded by an application sweep. It is **disabled by
default**, and stays disabled on upgrade: an enabled default would delete audit
history on the first boot, before any operator had chosen a period.

```yaml
audit_retention:
  enabled: true
  retention_days: 365   # no default; enabling without one is a config error
```

**Entries covered by an active legal hold are never deleted.** Place holds
through `/api/v1/admin/legal-holds`; placing and releasing are themselves
audited, because changing what may be deleted is a privileged act.

The sweep **refuses to start** if the legal-hold table is not readable on the
connection it sweeps — a deployment whose holds cannot be honoured gets no sweep
at all. An unbounded table is a problem; an unbounded table plus silent evidence
loss is a worse one.

In a coupled suite both apps write one shared `identity.audit_logs`, so two
independently-configured sweeps over one table means **the shorter retention
wins**. Decide which app owns audit retention before enabling it in either.

### Deliberately not encrypted

| Column | Why |
| --- | --- |
| `drift_runs.callback_token` | A single-use nonce, not a stored credential. Consumed by an atomic compare-and-clear (`UPDATE … SET callback_token='' WHERE id=$1 AND callback_token=$2`), which is what makes the CI callback one-shot — a replay finds it already cleared. **Encrypting it would break that**: AES-GCM re-nonces per seal, so the equality could never match. |
| `health_runs.callback_token` | As above, same mechanism, same reason. |

Neither is affected by a key rotation, because neither is sealed.

That table is enforced, not decorative:
`internal/maintenance/rekey_coverage_test.go` walks the source tree and fails the
build if an AAD-bound column is neither swept nor explicitly declared unswept
with a reason, or if a new `internal/crypto.Encrypt` call site appears that this
list does not mention — checked in both directions, so a stale entry fails too.
`internal/maintenance/plaintext_secret_columns_test.go` does the third: every
credential-named column in the schema must be either sealed or declared
plaintext with a reason, so a new token column stored in the clear is a
decision rather than an omission (#511). A
gate that quietly stopped covering a column would keep reporting success right up
until an operator deleted the key it was still guarding.
