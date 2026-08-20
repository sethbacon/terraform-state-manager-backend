# Changelog

## [3.8.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.7.0...v3.8.0) (2026-08-20)


### Features

* **db:** give the domain roots an organization, nullable and unread ([#393](https://github.com/sethbacon/terraform-state-manager-backend/issues/393) phase 1) ([#414](https://github.com/sethbacon/terraform-state-manager-backend/issues/414)) ([bd24d55](https://github.com/sethbacon/terraform-state-manager-backend/commit/bd24d558f61a5a35d42e6086c99df326a52b2b70))

## [3.7.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.6.0...v3.7.0) (2026-08-19)


### ⚠ BREAKING CHANGES

* **auth:** authority behind requireAuth is now bounded by the credential presenting the request rather than by its owner's role rows. Three requests that previously succeeded are refused: POST /auth/refresh from an API key or mTLS client certificate now answers 403 (rotate the key at POST /apikeys/{id}/rotate instead of exchanging it for a session); the /admin/organizations/{id} routes now require the presenting credential to itself carry organizations:write or admin, which no API key can (both are outside assignableKeyScopes), so organization management is an interactive-session action; and POST /apikeys/{id}/rotate now refuses to rotate a key whose scopes the caller does not hold, which also refuses an owner whose own authority was reduced since the key was minted. See docs/upgrade-guide.md.

### Bug Fixes

* **auth:** bind authority ceilings to the presenting credential, not the owner ([#406](https://github.com/sethbacon/terraform-state-manager-backend/issues/406)) ([0ccf318](https://github.com/sethbacon/terraform-state-manager-backend/commit/0ccf31880aecb571938854946c97226d270d5917))


### Dependencies

* finish the move off lib/pq to jackc/pgx and note why it remains indirect ([#411](https://github.com/sethbacon/terraform-state-manager-backend/issues/411)) ([7ac47e7](https://github.com/sethbacon/terraform-state-manager-backend/commit/7ac47e79b5aa6613a5261f4cf386ecbf99d53dc2))

## [3.6.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.5.0...v3.6.0) (2026-08-15)


### ⚠ BREAKING CHANGES

* **identity:** authorization now reads this application's own role tables (TSM_AUTHZ_ROLE_SOURCE=app, the default). Deployments with suite.role_seed_owner set to the sibling have been authorizing against the sibling's definition of every role name and will authorize against this build's from the first boot. Run `server authz-drift` before upgrading and require a zero exit; set TSM_AUTHZ_ROLE_SOURCE=identity and restart to roll back.

### Features

* **drift:** store per-run completeness so a run can answer "was this check complete?" ([#388](https://github.com/sethbacon/terraform-state-manager-backend/issues/388)) ([cbbcf28](https://github.com/sethbacon/terraform-state-manager-backend/commit/cbbcf2828efc4c267acce9e325b18cdff24fe324))
* **identity:** authorize from TSM's own role tables, gated on proven equivalence ([#389](https://github.com/sethbacon/terraform-state-manager-backend/issues/389)) ([3572d76](https://github.com/sethbacon/terraform-state-manager-backend/commit/3572d76e4f867a5a948a602a73deff00e4f21694))
* **identity:** give TSM a platform-admin carrier and an audit outbox ([#385](https://github.com/sethbacon/terraform-state-manager-backend/issues/385)) ([83bf1aa](https://github.com/sethbacon/terraform-state-manager-backend/commit/83bf1aa8570e8229e887c42ee33947041915bc10))
* **identity:** give TSM its own role tables, backfilled and dual-written ([#387](https://github.com/sethbacon/terraform-state-manager-backend/issues/387)) ([a1fe840](https://github.com/sethbacon/terraform-state-manager-backend/commit/a1fe8400f9f94fd9ee1b94e8a7e1db2d9dddd97a))


### Bug Fixes

* **auth:** report carrier-granted admin on /auth/me and resolve platform-admin identities ([#396](https://github.com/sethbacon/terraform-state-manager-backend/issues/396)) ([47b0447](https://github.com/sethbacon/terraform-state-manager-backend/commit/47b0447c80ee83a8442374c18c415f202b76ce9f))
* **security:** take go1.26.6 for seven reachable stdlib advisories ([#390](https://github.com/sethbacon/terraform-state-manager-backend/issues/390)) ([4122b42](https://github.com/sethbacon/terraform-state-manager-backend/commit/4122b42f6001f73b16e3ab03578f4fb8328bf147)), closes [#334](https://github.com/sethbacon/terraform-state-manager-backend/issues/334)

## [3.5.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.4.0...v3.5.0) (2026-08-14)


### Bug Fixes

* **drift:** persist the contract's completeness markers instead of dropping them ([#382](https://github.com/sethbacon/terraform-state-manager-backend/issues/382)) ([ced4fd2](https://github.com/sethbacon/terraform-state-manager-backend/commit/ced4fd24fc4588c3a151d6a40f4cde89e986153f))

## [3.4.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.3.0...v3.4.0) (2026-08-14)


### Features

* **drift:** bound the dispatched summary and carry the contract's new markers ([#379](https://github.com/sethbacon/terraform-state-manager-backend/issues/379)) ([9c181e8](https://github.com/sethbacon/terraform-state-manager-backend/commit/9c181e83c8d29a85685dbeff8dcb9b8d1ab4e067))


### Bug Fixes

* **drift:** run the contract's conformance corpus, and reconcile the jq summarizer ([#378](https://github.com/sethbacon/terraform-state-manager-backend/issues/378)) ([3fc5541](https://github.com/sethbacon/terraform-state-manager-backend/commit/3fc5541d86ab99dcee55f25ee3a9eb90fbccf509))

## [3.3.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.2.0...v3.3.0) (2026-08-13)


### Bug Fixes

* **security:** mask on either sensitivity mirror, project dispatched module_calls ([#374](https://github.com/sethbacon/terraform-state-manager-backend/issues/374)) ([e2dfb63](https://github.com/sethbacon/terraform-state-manager-backend/commit/e2dfb6392ba313f74acf056ed8425f5d8b33a72c))
* **security:** project the dispatched module lockfile, not just module_calls ([#377](https://github.com/sethbacon/terraform-state-manager-backend/issues/377)) ([095eaca](https://github.com/sethbacon/terraform-state-manager-backend/commit/095eacacc59311831f55a4e63faedd920ffdcbe5)), closes [#376](https://github.com/sethbacon/terraform-state-manager-backend/issues/376)

## [3.2.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.1.0...v3.2.0) (2026-08-12)


### Features

* **maintenance:** add bind-targets backfill with a verify gate ([#360](https://github.com/sethbacon/terraform-state-manager-backend/issues/360)) ([9dee6ac](https://github.com/sethbacon/terraform-state-manager-backend/commit/9dee6ac9b2ea6e98bf1f517c16e59335d6ec1482))
* **maintenance:** add rekey-targets so a key rotation can be completed ([#366](https://github.com/sethbacon/terraform-state-manager-backend/issues/366)) ([e46d21d](https://github.com/sethbacon/terraform-state-manager-backend/commit/e46d21deafa2bcc986f920ff16fee578d6c7a601)), closes [#364](https://github.com/sethbacon/terraform-state-manager-backend/issues/364)
* **notifications:** bind the encrypted channel target to its channel row ([#359](https://github.com/sethbacon/terraform-state-manager-backend/issues/359)) ([8dfe34d](https://github.com/sethbacon/terraform-state-manager-backend/commit/8dfe34d31dc852fb795eaeba09cdbe2a07b17de0)), closes [#153](https://github.com/sethbacon/terraform-state-manager-backend/issues/153)


### Bug Fixes

* **admin:** scope a target user's memberships to the caller's organizations ([#357](https://github.com/sethbacon/terraform-state-manager-backend/issues/357))5 ([e295718](https://github.com/sethbacon/terraform-state-manager-backend/commit/e295718c127fa4fb84514211f1b4721631851df2))
* **ci:** check out the two ADO extension repos the replay gate requires ([#356](https://github.com/sethbacon/terraform-state-manager-backend/issues/356)) ([da6bf20](https://github.com/sethbacon/terraform-state-manager-backend/commit/da6bf20cb6c79f3ace32beeb91597a90cd949cf9))
* **ci:** point the suite-ui checkout at its new owner ([#363](https://github.com/sethbacon/terraform-state-manager-backend/issues/363)) ([d9ae8c2](https://github.com/sethbacon/terraform-state-manager-backend/commit/d9ae8c2973e540c8e1628b2405781fb43ebcaab1))
* **ci:** repair the empty `with:` blocks that broke five workflows at startup ([#353](https://github.com/sethbacon/terraform-state-manager-backend/issues/353)) ([7d4be14](https://github.com/sethbacon/terraform-state-manager-backend/commit/7d4be14365ba2d008eec13d723a4905c235a3014))
* **ci:** spend the replay credential on the one private checkout only ([#365](https://github.com/sethbacon/terraform-state-manager-backend/issues/365)) ([d0f1c98](https://github.com/sethbacon/terraform-state-manager-backend/commit/d0f1c982659ef2fee22fd78bc94082ce3fc4569e))
* **db:** return the migration helpers' pooled connections instead of leaking them ([#355](https://github.com/sethbacon/terraform-state-manager-backend/issues/355)) ([3b817cb](https://github.com/sethbacon/terraform-state-manager-backend/commit/3b817cb21d9476806421eb5b54841dabe847f876))


### Documentation

* document the one-time bind-targets migration for operators ([#362](https://github.com/sethbacon/terraform-state-manager-backend/issues/362)) ([fbd3311](https://github.com/sethbacon/terraform-state-manager-backend/commit/fbd3311cdf9e87492f69230de09103b0c7ad3bef)), closes [#153](https://github.com/sethbacon/terraform-state-manager-backend/issues/153)

## [3.1.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v3.0.0...v3.1.0) (2026-08-08)


### ⚠ BREAKING CHANGES

* **identity:** three further operator-visible changes ship in 3.1.0, each written up in docs/upgrade-guide.md. (1) Read-then-mutate races on users, organizations, memberships, API keys and OIDC configs now return 404 where they previously returned a success status for a write that changed nothing; repeat DELETEs keep their existing success codes. (2) SMTP configuration carries an explicit TLS mode whose zero value requires TLS, and the mailer refuses credentials over plaintext to a non-local relay; a PUT to the SMTP config endpoint that omits use_tls no longer disables TLS -- an omitted field now leaves the current setting alone. (3) An authority reduction sweeps every API key the principal holds rather than only those stamped with the affected organization, because every key in this application carries the default organization regardless of its owner, so the organization-filtered sweep left the credentials it was meant to retire working.
* **identity:** Outbound requests to the IdP and to the sibling suite app now go through the shared egress guard, whose default policy denies loopback, RFC1918 and link-local addresses. A deployment whose IdP is internal must set TSM_SECURITY_EGRESS_ALLOWLIST or authentication fails at startup with `egress to "<host>" blocked`. Note this key REPLACES the connectors' built-in default rather than widening it, so any value must re-state the private ranges: TSM_SECURITY_EGRESS_ALLOWLIST=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7,<idp-host> Omitting them stops every internal state backend resolving. DEV_MODE does not cover this: the scheme rule and the destination rule are separate controls.

### Bug Fixes

* **identity:** announce the operator-visible changes already on main ([#346](https://github.com/sethbacon/terraform-state-manager-backend/issues/346)) ([9914756](https://github.com/sethbacon/terraform-state-manager-backend/commit/99147562596761de18be7fbae86b5a31ccb98060))
* **identity:** carry the three notices dropped from the 3.1.0 changelog ([#348](https://github.com/sethbacon/terraform-state-manager-backend/issues/348)) ([15e83f1](https://github.com/sethbacon/terraform-state-manager-backend/commit/15e83f1bae3e891fcee80ca80abaed126e0ec477))

## [3.0.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.7.1...v3.0.0) (2026-08-05)


### ⚠ BREAKING CHANGES

* **auth:** Authority-reduction events now end the affected user's live sessions. Removing or re-roling an organization member, deleting an organization, deleting or erasing a user, and every SCIM deactivation path move that user's revocation watermark, so their existing sessions are rejected on the next request and they must log in again. Refresh sits behind requireAuth, so this is a re-login rather than a token refresh. API keys are revoked irreversibly where they over-ask against retained authority, and offboarding revokes all of them; secrets are shown once, so affected keys must be recreated. SCIM PUT no longer deprovisions unless "active": false is explicit. User delete and erase now answer 500 without performing the destructive step if the credential sweep could not complete. See docs/upgrade-guide.md.
* local and kubernetes state sources now require TSM_STATESOURCE_LOCAL_ROOTS / TSM_STATESOURCE_KUBECONFIG_ROOTS. Existing installs that use them will refuse to construct those sources until the roots are configured. Defaulting to the previous behaviour would preserve the defect for every install that never reads the release notes, so this fails closed and says so at the point of failure -- the rejection names both the path and the configured roots. Helm users with localStates.enabled=true are handled by the chart automatically; see docs/upgrade-guide.md.

### Bug Fixes

* **auth:** announce that authority changes now end live sessions ([56827e2](https://github.com/sethbacon/terraform-state-manager-backend/commit/56827e229c3f26f7d40296cab89aa8499f2f5ecd))
* **auth:** invalidate every credential family when authority is reduced ([#330](https://github.com/sethbacon/terraform-state-manager-backend/issues/330)) ([#338](https://github.com/sethbacon/terraform-state-manager-backend/issues/338)) ([75750a7](https://github.com/sethbacon/terraform-state-manager-backend/commit/75750a7b73a68af71d1cae69184db208c2e85883))
* confine server-local source paths to configured roots ([#337](https://github.com/sethbacon/terraform-state-manager-backend/issues/337)) ([518c771](https://github.com/sethbacon/terraform-state-manager-backend/commit/518c7713d6c566d2882658e7e79e5664bdc0684d))
* scope audit log reads to the caller's organizations ([#332](https://github.com/sethbacon/terraform-state-manager-backend/issues/332)) ([eb55c6f](https://github.com/sethbacon/terraform-state-manager-backend/commit/eb55c6f6aa3079610f0cc1c0c8be272dcf56f11f))

## [2.7.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.7.0...v2.7.1) (2026-07-31)


### Bug Fixes

* add CSRF-protected POST /auth/logout alongside the GET route ([#328](https://github.com/sethbacon/terraform-state-manager-backend/issues/328)) ([03221db](https://github.com/sethbacon/terraform-state-manager-backend/commit/03221db753e036e43f5c6d79b95fac061d77de55))
* bound GET /sources so it cannot serialize the whole table ([#326](https://github.com/sethbacon/terraform-state-manager-backend/issues/326)) ([8928898](https://github.com/sethbacon/terraform-state-manager-backend/commit/892889848db4e8e90cb90a5925528dc00e6d7979))
* bound state_backups with an age-capped retention sweep ([#325](https://github.com/sethbacon/terraform-state-manager-backend/issues/325)) ([249aa36](https://github.com/sethbacon/terraform-state-manager-backend/commit/249aa36845a43a9429d6a43dcb0cf97fd033e0d1))
* remove the forgeable GET /auth/logout route ([#329](https://github.com/sethbacon/terraform-state-manager-backend/issues/329)) ([50bb4e4](https://github.com/sethbacon/terraform-state-manager-backend/commit/50bb4e41f1a499c49a6b82967fde9adb46890ce1))

## [2.7.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.6.0...v2.7.0) (2026-07-28)


### Features

* **admin:** org-narrow the admin audit-log view and tag org events ([#298](https://github.com/sethbacon/terraform-state-manager-backend/issues/298)) ([#319](https://github.com/sethbacon/terraform-state-manager-backend/issues/319)) ([d30b6c5](https://github.com/sethbacon/terraform-state-manager-backend/commit/d30b6c5aa1bea6396bbe9532194b8376e9493660))
* CSRF-safe POST /reconcile + state-edit diff endpoint (FE [#214](https://github.com/sethbacon/terraform-state-manager-backend/issues/214), [#215](https://github.com/sethbacon/terraform-state-manager-backend/issues/215)) ([#324](https://github.com/sethbacon/terraform-state-manager-backend/issues/324)) ([dc88dfe](https://github.com/sethbacon/terraform-state-manager-backend/commit/dc88dfec2c17e4ed5caf6c36fb6b66fa0a68c7a3))
* **edit:** mark forced state writes in the audit trail and edit ledger ([#280](https://github.com/sethbacon/terraform-state-manager-backend/issues/280)) ([#317](https://github.com/sethbacon/terraform-state-manager-backend/issues/317)) ([46864a0](https://github.com/sethbacon/terraform-state-manager-backend/commit/46864a0c6221c527c3d2c139a4e0921de418ee21))
* **statesource:** SSRF egress residuals - config allow-list, git dial-pin, consul token strip ([#302](https://github.com/sethbacon/terraform-state-manager-backend/issues/302)) ([#318](https://github.com/sethbacon/terraform-state-manager-backend/issues/318)) ([28d6579](https://github.com/sethbacon/terraform-state-manager-backend/commit/28d6579da7035305600bb0646423c328e2c52fd8))
* **telemetry:** return a stop function from StartDBStatsCollector ([#289](https://github.com/sethbacon/terraform-state-manager-backend/issues/289)) ([#316](https://github.com/sethbacon/terraform-state-manager-backend/issues/316)) ([ff68ffe](https://github.com/sethbacon/terraform-state-manager-backend/commit/ff68ffe8ed2a6f10dd17a36b82353fab7cc4c826))


### Bug Fixes

* **admin:** narrow admin user/API-key lists to the caller's orgs; correct org_owner state claim ([#182](https://github.com/sethbacon/terraform-state-manager-backend/issues/182), [#253](https://github.com/sethbacon/terraform-state-manager-backend/issues/253)) ([#299](https://github.com/sethbacon/terraform-state-manager-backend/issues/299)) ([1ea62ab](https://github.com/sethbacon/terraform-state-manager-backend/commit/1ea62ab06afefd0927a9ede4754b124544ffdec9))
* **api:** cap audit-ingest body, log edit-history write failures, stop leaking raw connector errors ([#284](https://github.com/sethbacon/terraform-state-manager-backend/issues/284), [#285](https://github.com/sethbacon/terraform-state-manager-backend/issues/285), [#286](https://github.com/sethbacon/terraform-state-manager-backend/issues/286)) ([#312](https://github.com/sethbacon/terraform-state-manager-backend/issues/312)) ([8e79b7c](https://github.com/sethbacon/terraform-state-manager-backend/commit/8e79b7c0a029cbffb102a0af92f762d95e1f2e3c))
* **auth:** cap API-key scopes by the owner's live privileges; bar admin-scoped keys ([#223](https://github.com/sethbacon/terraform-state-manager-backend/issues/223), [#252](https://github.com/sethbacon/terraform-state-manager-backend/issues/252)) ([#297](https://github.com/sethbacon/terraform-state-manager-backend/issues/297)) ([6d279ad](https://github.com/sethbacon/terraform-state-manager-backend/commit/6d279ad6f93e722acf0aa6f5077c0d57b00cde71))
* **auth:** enforce JWT secret strength, rate-limit LDAP login, gate LDAP insecure TLS ([#249](https://github.com/sethbacon/terraform-state-manager-backend/issues/249), [#250](https://github.com/sethbacon/terraform-state-manager-backend/issues/250), [#251](https://github.com/sethbacon/terraform-state-manager-backend/issues/251)) ([#300](https://github.com/sethbacon/terraform-state-manager-backend/issues/300)) ([5b9aa36](https://github.com/sethbacon/terraform-state-manager-backend/commit/5b9aa36d632d39c5d0f8fa0e6d43db435318965a))
* **resilience:** bound state requests with a timeout; retry idempotent pipeline GETs ([#263](https://github.com/sethbacon/terraform-state-manager-backend/issues/263), [#264](https://github.com/sethbacon/terraform-state-manager-backend/issues/264)) ([#304](https://github.com/sethbacon/terraform-state-manager-backend/issues/304)) ([1d8e82a](https://github.com/sethbacon/terraform-state-manager-backend/commit/1d8e82aacd15a5514535d3b3e3396bcdd2fa2fd1))
* **state:** cap connector state reads and gzip output; paginate the backups list ([#254](https://github.com/sethbacon/terraform-state-manager-backend/issues/254), [#255](https://github.com/sethbacon/terraform-state-manager-backend/issues/255), [#262](https://github.com/sethbacon/terraform-state-manager-backend/issues/262)) ([#301](https://github.com/sethbacon/terraform-state-manager-backend/issues/301)) ([19a9332](https://github.com/sethbacon/terraform-state-manager-backend/commit/19a933203d6900a57d2965cac940ec9350d4e8e0))
* **state:** lock transfer/migrate plane to prevent silent lost-update ([#247](https://github.com/sethbacon/terraform-state-manager-backend/issues/247)) ([#294](https://github.com/sethbacon/terraform-state-manager-backend/issues/294)) ([7806546](https://github.com/sethbacon/terraform-state-manager-backend/commit/7806546155998a3224abdf1884109b75b90c1db0))
* **state:** route state-source connectors through an SSRF egress guard ([#256](https://github.com/sethbacon/terraform-state-manager-backend/issues/256)) ([#303](https://github.com/sethbacon/terraform-state-manager-backend/issues/303)) ([4ac2694](https://github.com/sethbacon/terraform-state-manager-backend/commit/4ac26942c88026b33912345e0227927a0206c859))


### Documentation

* add suite-token rotation, correct Trivy gate wording, fix CODEOWNERS rules ([#273](https://github.com/sethbacon/terraform-state-manager-backend/issues/273), [#291](https://github.com/sethbacon/terraform-state-manager-backend/issues/291), [#278](https://github.com/sethbacon/terraform-state-manager-backend/issues/278)) ([#311](https://github.com/sethbacon/terraform-state-manager-backend/issues/311)) ([c734c78](https://github.com/sethbacon/terraform-state-manager-backend/commit/c734c781bb6624bd430547a3858f9059b297a9ee))
* **state:** document that pre-write state backups are persisted, unencrypted, unbounded ([#248](https://github.com/sethbacon/terraform-state-manager-backend/issues/248), [#272](https://github.com/sethbacon/terraform-state-manager-backend/issues/272)) ([#296](https://github.com/sethbacon/terraform-state-manager-backend/issues/296)) ([e5f7c92](https://github.com/sethbacon/terraform-state-manager-backend/commit/e5f7c92bfdb1b282a24dd5df52e472d80009b099))


### Refactor

* **api:** extract leader-elected background workers from NewRouter into a testable constructor ([#265](https://github.com/sethbacon/terraform-state-manager-backend/issues/265)) ([#310](https://github.com/sethbacon/terraform-state-manager-backend/issues/310)) ([b56613c](https://github.com/sethbacon/terraform-state-manager-backend/commit/b56613c8176dbcd38db4c943dd57b34ad0c890c4))
* **repos:** standardize on errors.Is(err, sql.ErrNoRows) across the repository layer ([#287](https://github.com/sethbacon/terraform-state-manager-backend/issues/287)) ([#314](https://github.com/sethbacon/terraform-state-manager-backend/issues/314)) ([1b12e97](https://github.com/sethbacon/terraform-state-manager-backend/commit/1b12e97de7bcd835bb4a00b8787140d166ef8d5a))
* **statesource:** extract shared httpDo core for the HTTP connectors ([#266](https://github.com/sethbacon/terraform-state-manager-backend/issues/266)) ([#315](https://github.com/sethbacon/terraform-state-manager-backend/issues/315)) ([ee5e205](https://github.com/sethbacon/terraform-state-manager-backend/commit/ee5e205cad9c8d357a368bd184f2fdb553610b2c))

## [2.6.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.5.0...v2.6.0) (2026-07-23)


### Features

* whitelabel theming — persisted branding served at /ui/theme ([#242](https://github.com/sethbacon/terraform-state-manager-backend/issues/242)) ([a99ea84](https://github.com/sethbacon/terraform-state-manager-backend/commit/a99ea84b7d9e8aaf5e08161631ffde1ce707def8))


### Bug Fixes

* adopt org_owner/org_provisioner scopes and grant org_owner instead of admin on org creation ([#246](https://github.com/sethbacon/terraform-state-manager-backend/issues/246)) ([003d043](https://github.com/sethbacon/terraform-state-manager-backend/commit/003d043db0d5d223a7f41b20fd5e2b001f89afe2))
* durable cross-replica login state; cap the in-memory store ([#241](https://github.com/sethbacon/terraform-state-manager-backend/issues/241)) ([70cac3f](https://github.com/sethbacon/terraform-state-manager-backend/commit/70cac3f8f3665eace7fbbf10697ab392a040936e))

## [2.5.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.4.2...v2.5.0) (2026-07-23)


### Features

* **api:** surface the cause on 5xx responses in the access log ([#230](https://github.com/sethbacon/terraform-state-manager-backend/issues/230)) ([ae553ed](https://github.com/sethbacon/terraform-state-manager-backend/commit/ae553edb84fe7881ef9a365327399b32d1b0ac4b))
* **config:** validate configuration at boot (fail-fast) ([#233](https://github.com/sethbacon/terraform-state-manager-backend/issues/233)) ([57d2a9a](https://github.com/sethbacon/terraform-state-manager-backend/commit/57d2a9a1846f6cf6b529525bcb86225551867937)), closes [#218](https://github.com/sethbacon/terraform-state-manager-backend/issues/218)
* **sources:** add POST /sources/test for unsaved source configurations ([#227](https://github.com/sethbacon/terraform-state-manager-backend/issues/227)) ([1a3681d](https://github.com/sethbacon/terraform-state-manager-backend/commit/1a3681d4b25381c5be9fffcc4817f65ad00bbd42))


### Bug Fixes

* **admin:** revoke a user's API keys on delete and GDPR erasure ([#234](https://github.com/sethbacon/terraform-state-manager-backend/issues/234)) ([7c81c9d](https://github.com/sethbacon/terraform-state-manager-backend/commit/7c81c9da1dec6dca8798e80a73c25e03eec27245)), closes [#223](https://github.com/sethbacon/terraform-state-manager-backend/issues/223)
* claim schedules atomically before dispatch (no duplicate CI runs) ([#237](https://github.com/sethbacon/terraform-state-manager-backend/issues/237)) ([c15dd6d](https://github.com/sethbacon/terraform-state-manager-backend/commit/c15dd6d54bc295815e7b251e3d336f5680c9ebe9))
* consul connector implements Locker via session lock on &lt;key&gt;/.lock ([#240](https://github.com/sethbacon/terraform-state-manager-backend/issues/240)) ([ce18265](https://github.com/sethbacon/terraform-state-manager-backend/commit/ce1826564235ac3ea0ea10b6fd24c242431efef5))
* harden connector credential handling ([#235](https://github.com/sethbacon/terraform-state-manager-backend/issues/235)) ([e20d3c9](https://github.com/sethbacon/terraform-state-manager-backend/commit/e20d3c9914f01792b92c8ccb24e17615712cd586))
* reap stale state locks by heartbeat, not acquisition age ([#238](https://github.com/sethbacon/terraform-state-manager-backend/issues/238)) ([c8a5a45](https://github.com/sethbacon/terraform-state-manager-backend/commit/c8a5a45d4ddfa37aa20f6245e372aa85b85bf877))


### Performance

* cache dashboard aggregates, de-correlate SyncStatuses, index history prune ([#236](https://github.com/sethbacon/terraform-state-manager-backend/issues/236)) ([ff5e671](https://github.com/sethbacon/terraform-state-manager-backend/commit/ff5e67142f4c78b5baf8b5d7004a093adae4e286))
* **dashboard:** push the exact-match version drill-down into SQL ([#232](https://github.com/sethbacon/terraform-state-manager-backend/issues/232)) ([a27a4e8](https://github.com/sethbacon/terraform-state-manager-backend/commit/a27a4e84007bac5c1eb4a768fa1cba2c9ebb74df))
* multi-replica-safe workers — leader election, parallel sync, liveness telemetry ([#239](https://github.com/sethbacon/terraform-state-manager-backend/issues/239)) ([e487598](https://github.com/sethbacon/terraform-state-manager-backend/commit/e487598300534e1566b4aa8ac4580607169e96d3))
* **reports:** push report filters into SQL instead of scanning the whole store ([#228](https://github.com/sethbacon/terraform-state-manager-backend/issues/228)) ([63bfb44](https://github.com/sethbacon/terraform-state-manager-backend/commit/63bfb445bb23e909059dd568dfe749997d50dd1e))

## [2.4.2](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.4.1...v2.4.2) (2026-07-20)


### Bug Fixes

* **api:** auto-add the creator as the new organization's first admin member ([#212](https://github.com/sethbacon/terraform-state-manager-backend/issues/212)) ([73e4e65](https://github.com/sethbacon/terraform-state-manager-backend/commit/73e4e651829e88e84467d2dd5199202b5439ffbf))

## [2.4.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.4.0...v2.4.1) (2026-07-19)


### Bug Fixes

* **db:** drop column defaults before type change in migration 000023 ([#210](https://github.com/sethbacon/terraform-state-manager-backend/issues/210)) ([de876c9](https://github.com/sethbacon/terraform-state-manager-backend/commit/de876c9a3dd294e8dcb25a6b8134c1dc82cd70a2))

## [2.4.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.3.0...v2.4.0) (2026-07-18)


### Features

* **notifications:** add GET/PUT /notifications/api-key-expiry endpoint ([#208](https://github.com/sethbacon/terraform-state-manager-backend/issues/208)) ([5e6bea1](https://github.com/sethbacon/terraform-state-manager-backend/commit/5e6bea12bb9a212b924a00dbb0c564b9552e377f))

## [2.3.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.2.1...v2.3.0) (2026-07-18)


### Features

* **notify:** adopt shared identity/notify, identity/crypto, identity/httpsafe ([#206](https://github.com/sethbacon/terraform-state-manager-backend/issues/206)) ([72c92b4](https://github.com/sethbacon/terraform-state-manager-backend/commit/72c92b4dcda3223f98463dde5e69aeceaa42fd15))

## [2.2.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.2.0...v2.2.1) (2026-07-17)


### Bug Fixes

* **helm:** apply backend.extraInitContainers to the dedicated worker too ([#204](https://github.com/sethbacon/terraform-state-manager-backend/issues/204)) ([a2e25e6](https://github.com/sethbacon/terraform-state-manager-backend/commit/a2e25e6cace905c31ecbd106f0775085eb23e5ea))

## [2.2.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.1.1...v2.2.0) (2026-07-17)


### Features

* **helm:** support extra init containers on the backend pod ([#203](https://github.com/sethbacon/terraform-state-manager-backend/issues/203)) ([c25f200](https://github.com/sethbacon/terraform-state-manager-backend/commit/c25f200cb4325131eb3dad4d42f63123e91a7e98))


### Bug Fixes

* **docker:** bump Go toolchain to 1.26.5 (CVE-2026-39822) ([#201](https://github.com/sethbacon/terraform-state-manager-backend/issues/201)) ([fa399f4](https://github.com/sethbacon/terraform-state-manager-backend/commit/fa399f4da76f4c6f22e322258f48bbe1899059bb))

## [2.1.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.1.0...v2.1.1) (2026-07-17)


### Bug Fixes

* **config:** document missing notifications.smtp section in config.example.yaml ([#199](https://github.com/sethbacon/terraform-state-manager-backend/issues/199)) ([016ef9b](https://github.com/sethbacon/terraform-state-manager-backend/commit/016ef9be6d0d8f650f1bc9bab992746f9e0d0072))

## [2.1.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.0.1...v2.1.0) (2026-07-16)


### Features

* **notifications:** DB-persisted SMTP relay config and TLS control ([#195](https://github.com/sethbacon/terraform-state-manager-backend/issues/195)) ([1f31a96](https://github.com/sethbacon/terraform-state-manager-backend/commit/1f31a96659add7f8cb69a1f39f087b541db72e8a))


### Bug Fixes

* **ci:** make Trivy scans blocking and scan the published image digest ([#191](https://github.com/sethbacon/terraform-state-manager-backend/issues/191)) ([aeb756f](https://github.com/sethbacon/terraform-state-manager-backend/commit/aeb756ffbf7376ed6f0f4f2540e420b19e0da023))
* require shared-org admin for cross-org user management ([#54](https://github.com/sethbacon/terraform-state-manager-backend/issues/54)) ([#194](https://github.com/sethbacon/terraform-state-manager-backend/issues/194)) ([12ee2df](https://github.com/sethbacon/terraform-state-manager-backend/commit/12ee2dfa78eb4544bfb906671f608f3588a0db56))

## [2.0.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v2.0.0...v2.0.1) (2026-07-13)


### Bug Fixes

* **driftingest:** reject a malformed host instead of an empty RegistryHost ([#189](https://github.com/sethbacon/terraform-state-manager-backend/issues/189)) ([d77cc33](https://github.com/sethbacon/terraform-state-manager-backend/commit/d77cc337c027889b3c25d12973b90f906afa62fd))

## [2.0.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.17.0...v2.0.0) (2026-07-13)


### ⚠ BREAKING CHANGES

* **auth:** adopt terraform-suite-identity v0.17.0 ([#186](https://github.com/sethbacon/terraform-state-manager-backend/issues/186))

### Features

* **auth:** adopt terraform-suite-identity v0.17.0 ([#186](https://github.com/sethbacon/terraform-state-manager-backend/issues/186)) ([47dc27e](https://github.com/sethbacon/terraform-state-manager-backend/commit/47dc27ec9228650b0a962f4cbd014005d97ee29a))


### Bug Fixes

* **admin:** require per-organization membership on /admin/organizations/:id* routes ([#183](https://github.com/sethbacon/terraform-state-manager-backend/issues/183)) ([7a22f3b](https://github.com/sethbacon/terraform-state-manager-backend/commit/7a22f3b3a6e0bdc6b865f20bfefd38a99c97f38a))
* **auth:** bind OIDC login with a nonce and PKCE verifier ([#185](https://github.com/sethbacon/terraform-state-manager-backend/issues/185)) ([0f61102](https://github.com/sethbacon/terraform-state-manager-backend/commit/0f6110245c648a1fd930bf92e2d9a384f53b228e))
* **auth:** call SetAudience to close the JWT audience gap in [#178](https://github.com/sethbacon/terraform-state-manager-backend/issues/178) ([#188](https://github.com/sethbacon/terraform-state-manager-backend/issues/188)) ([7f3aea7](https://github.com/sethbacon/terraform-state-manager-backend/commit/7f3aea7efa392973bab956377359ce727652618d))
* **auth:** enforce OIDC RequireHTTPS in prod + JWT issuer pin ([#184](https://github.com/sethbacon/terraform-state-manager-backend/issues/184)) ([0ec9a04](https://github.com/sethbacon/terraform-state-manager-backend/commit/0ec9a04cdf3a40b6e2855001fc4851b716e95859))

## [1.17.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.16.1...v1.17.0) (2026-07-10)


### Features

* audit auth events and add server-side audit-log export ([#167](https://github.com/sethbacon/terraform-state-manager-backend/issues/167)) ([eca1b05](https://github.com/sethbacon/terraform-state-manager-backend/commit/eca1b05b526007b124099eef68439ad81964b9eb))
* backup content viewer and restore-preview diff endpoints ([#168](https://github.com/sethbacon/terraform-state-manager-backend/issues/168)) ([c6ac9fc](https://github.com/sethbacon/terraform-state-manager-backend/commit/c6ac9fcdf9c5ce7d5800265bbace53366e868369))
* list active state locks ([#166](https://github.com/sethbacon/terraform-state-manager-backend/issues/166)) ([5ceff02](https://github.com/sethbacon/terraform-state-manager-backend/commit/5ceff02c0c4e4ff8da92f100e0e2b55559d664ca))
* request access logging with request IDs and identity-DB readiness check ([#165](https://github.com/sethbacon/terraform-state-manager-backend/issues/165)) ([9e89e66](https://github.com/sethbacon/terraform-state-manager-backend/commit/9e89e66653f034e2ba3d46c06ed8f0a084057c0f))
* server-side pagination and filtering for drift runs and records ([#170](https://github.com/sethbacon/terraform-state-manager-backend/issues/170)) ([27f7984](https://github.com/sethbacon/terraform-state-manager-backend/commit/27f7984e8a71e67a21986d9b24346bfcfcbbd937))


### Bug Fixes

* fix Windows-incompatible git path handling and flaky transfer test ([#169](https://github.com/sethbacon/terraform-state-manager-backend/issues/169)) ([58285be](https://github.com/sethbacon/terraform-state-manager-backend/commit/58285be7ee0ad5bd2150ac8fdeb4c51b5b132e5e))

## [1.16.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.16.0...v1.16.1) (2026-07-01)


### Bug Fixes

* **stateops:** honor for_each/count instance index in state rm/mv ([#163](https://github.com/sethbacon/terraform-state-manager-backend/issues/163)) ([403b669](https://github.com/sethbacon/terraform-state-manager-backend/commit/403b6697042b543b6e0dfa6bfb6c3095dfbc2643)), closes [#162](https://github.com/sethbacon/terraform-state-manager-backend/issues/162)

## [1.16.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.15.1...v1.16.0) (2026-06-26)


### Features

* **reports:** scope the Reports refresh to the selected source(s) ([d4ff1a4](https://github.com/sethbacon/terraform-state-manager-backend/commit/d4ff1a440dd27247a2ead084dfb6eeb4878488bf))
* **reports:** scope the Reports refresh to the selected source(s) ([593c711](https://github.com/sethbacon/terraform-state-manager-backend/commit/593c711901154f10a6665fb59b68189d5676abc5))


### Bug Fixes

* **analyzer:** aggregate aliased providers in pre-v4 (0.11.x) state ([04ccfb4](https://github.com/sethbacon/terraform-state-manager-backend/commit/04ccfb4c4e22c0edf964c39e70042ad3e0b0d2fb))
* correct resource counts for pre-v4 (0.11.x) state on Reports & Dashboard ([604bf7d](https://github.com/sethbacon/terraform-state-manager-backend/commit/604bf7dca160d85a916cf568dd79c1577b472dd1))
* **statesync:** re-analyze stored states when the analyzer logic changes ([c17478b](https://github.com/sethbacon/terraform-state-manager-backend/commit/c17478b84ba4fea0c304f5f0de2e263b1a858b05))


### Documentation

* **swagger:** regenerate swagger.json for the reports refresh annotation ([a76618d](https://github.com/sethbacon/terraform-state-manager-backend/commit/a76618d6fcb246ea0dfd49311dd5813c22007d04))

## [1.15.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.15.0...v1.15.1) (2026-06-24)


### Bug Fixes

* **analyzer:** count resources in pre-v4 (Terraform 0.11) state files ([#157](https://github.com/sethbacon/terraform-state-manager-backend/issues/157)) ([aa7f97e](https://github.com/sethbacon/terraform-state-manager-backend/commit/aa7f97e8b7b8858f26afbfdca832fdeb1c2841cf))

## [1.15.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.14.1...v1.15.0) (2026-06-24)


### Features

* **api:** add admin-only delete state operation ([#154](https://github.com/sethbacon/terraform-state-manager-backend/issues/154)) ([b89fbc7](https://github.com/sethbacon/terraform-state-manager-backend/commit/b89fbc72cf12dc2298a9b24517c06fb5335e7b73)), closes [#153](https://github.com/sethbacon/terraform-state-manager-backend/issues/153)

## [1.14.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.14.0...v1.14.1) (2026-06-22)


### Documentation

* **deployment:** add in-cluster PostgreSQL (CloudNativePG) option for AKS ([#151](https://github.com/sethbacon/terraform-state-manager-backend/issues/151)) ([a1a547c](https://github.com/sethbacon/terraform-state-manager-backend/commit/a1a547c899fa9c193e5c738b10e46481a6357e2c))

## [1.14.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.13.0...v1.14.0) (2026-06-22)


### Features

* paginate version-lab runs list ([#149](https://github.com/sethbacon/terraform-state-manager-backend/issues/149)) ([8d8a3aa](https://github.com/sethbacon/terraform-state-manager-backend/commit/8d8a3aacfe9f0ab0b4c6cf4779836b8a9305e16d))
* report version-lab failures before callback ([#148](https://github.com/sethbacon/terraform-state-manager-backend/issues/148)) ([18eea8a](https://github.com/sethbacon/terraform-state-manager-backend/commit/18eea8a5ddb2cec71f06f180edc45713063a893b))

## [1.13.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.12.0...v1.13.0) (2026-06-22)


### Features

* **drift:** reconcile dispatched runs stuck without a callback ([#145](https://github.com/sethbacon/terraform-state-manager-backend/issues/145)) ([d1bd132](https://github.com/sethbacon/terraform-state-manager-backend/commit/d1bd132c1cf7ce47b70561c9b3f7adf0a70c8c8b))
* **health:** reconcile dispatched version-lab runs stuck without a callback ([#147](https://github.com/sethbacon/terraform-state-manager-backend/issues/147)) ([1302f4e](https://github.com/sethbacon/terraform-state-manager-backend/commit/1302f4ef563e7c3c747029ab6e9743051c3fd9f9))

## [1.12.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.11.0...v1.12.0) (2026-06-20)


### Features

* add suite-based drift + version-lab CI templates (profile=suite) ([#143](https://github.com/sethbacon/terraform-state-manager-backend/issues/143)) ([a186127](https://github.com/sethbacon/terraform-state-manager-backend/commit/a1861270ac0140f6ad17ac54bbae9946d088f640))

## [1.11.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.10.0...v1.11.0) (2026-06-20)


### Features

* **drift:** reconcile ingest summarizer with dispatch contract (read-skip, attrs, masking) ([#140](https://github.com/sethbacon/terraform-state-manager-backend/issues/140)) ([6c73ffe](https://github.com/sethbacon/terraform-state-manager-backend/commit/6c73ffe8ae312b444478156211dd8498c11ce2d5))
* **notifications:** add Microsoft Teams and email channel types ([#142](https://github.com/sethbacon/terraform-state-manager-backend/issues/142)) ([f5888e3](https://github.com/sethbacon/terraform-state-manager-backend/commit/f5888e34024d36594df9013c8d6051e9ff0a3b61))

## [1.10.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.9.0...v1.10.0) (2026-06-19)


### Features

* **drift:** add pipeline connection update endpoint ([#138](https://github.com/sethbacon/terraform-state-manager-backend/issues/138)) ([4f76886](https://github.com/sethbacon/terraform-state-manager-backend/commit/4f76886a3e73f1540c9ec33f05b433684de9634c))

## [1.9.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.8.0...v1.9.0) (2026-06-18)


### Features

* operator-managed CI workflow template registry ([#137](https://github.com/sethbacon/terraform-state-manager-backend/issues/137)) ([a1f87b2](https://github.com/sethbacon/terraform-state-manager-backend/commit/a1f87b29b8a0bb0bb0d12677cd8f43a43a989751))
* **reports:** cross-fleet state query and multi-format export ([#135](https://github.com/sethbacon/terraform-state-manager-backend/issues/135)) ([735d48f](https://github.com/sethbacon/terraform-state-manager-backend/commit/735d48f29e8584af095587ffeaa38cbd64bb6765))

## [1.8.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.7.0...v1.8.0) (2026-06-18)


### Features

* **dashboard:** add states-by-version endpoint ([#133](https://github.com/sethbacon/terraform-state-manager-backend/issues/133)) ([28249bc](https://github.com/sethbacon/terraform-state-manager-backend/commit/28249bc2e8f8f62565dcc623900bca4ff10e8c41))

## [1.7.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.6.0...v1.7.0) (2026-06-18)


### Features

* **drift:** add GitHub App auth for GitHub CI sources ([#127](https://github.com/sethbacon/terraform-state-manager-backend/issues/127)) ([f060ead](https://github.com/sethbacon/terraform-state-manager-backend/commit/f060ead860e6b9052c4933dd196b487f48ef0422))
* **drift:** Entra app-registration auth for Azure DevOps CI sources ([#126](https://github.com/sethbacon/terraform-state-manager-backend/issues/126)) ([3d506df](https://github.com/sethbacon/terraform-state-manager-backend/commit/3d506dfd3d8088000d7bc1b802f1403d6c145cf5))


### Documentation

* add governance, architecture, ADRs, and suite-coupling/SSO guides ([#129](https://github.com/sethbacon/terraform-state-manager-backend/issues/129)) ([9507173](https://github.com/sethbacon/terraform-state-manager-backend/commit/9507173f2818c8a19fd13bd8edfca5defe865a5c))
* document configuration, suite coupling, and deployment caveats ([#132](https://github.com/sethbacon/terraform-state-manager-backend/issues/132)) ([318558b](https://github.com/sethbacon/terraform-state-manager-backend/commit/318558bef7e8fca70b812d8ccf014a652e4af01f))
* **drift:** align GitHub App runbook table formatting ([#131](https://github.com/sethbacon/terraform-state-manager-backend/issues/131)) ([0a62b75](https://github.com/sethbacon/terraform-state-manager-backend/commit/0a62b750112ccab86b83fb069e56f2f9be73c2ba))
* **plans:** add drift CI app-auth plans (Entra app for ADO, GitHub App) ([#125](https://github.com/sethbacon/terraform-state-manager-backend/issues/125)) ([f94439e](https://github.com/sethbacon/terraform-state-manager-backend/commit/f94439ebac8b725ae9e2fd28fa6387749a6a781a))

## [1.6.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.5.0...v1.6.0) (2026-06-16)


### Features

* **drift:** capture module provenance from dispatched CI runs (P2-5) ([#123](https://github.com/sethbacon/terraform-state-manager-backend/issues/123)) ([441e4fc](https://github.com/sethbacon/terraform-state-manager-backend/commit/441e4fcd334cef3653bd1a0612c87d94ff57359f))
* **suite:** module freshness proxy vs the sibling registry (P2-5) ([#122](https://github.com/sethbacon/terraform-state-manager-backend/issues/122)) ([48fb14a](https://github.com/sethbacon/terraform-state-manager-backend/commit/48fb14a7ee75b1914c2b5b3d03d3e743d6b0c31a))

## [1.5.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.4.0...v1.5.0) (2026-06-16)


### Features

* **drift:** ingest module lockfiles to record resolved module versions (P2-5) ([#118](https://github.com/sethbacon/terraform-state-manager-backend/issues/118)) ([ce0ad97](https://github.com/sethbacon/terraform-state-manager-backend/commit/ce0ad97f558b45133f8106b837b40923077dd505))
* **suite:** add cross-app audit federation ingest endpoint (P2-7) ([#117](https://github.com/sethbacon/terraform-state-manager-backend/issues/117)) ([0ef5aa6](https://github.com/sethbacon/terraform-state-manager-backend/commit/0ef5aa60f01d6b747b172e38c794348e5df32e19))


### Bug Fixes

* **helm:** make the frontend CSP nonce reach emotion (configmap parity) ([#120](https://github.com/sethbacon/terraform-state-manager-backend/issues/120)) ([514aec9](https://github.com/sethbacon/terraform-state-manager-backend/commit/514aec9bb5513e60f8057fd79607b45ef5446a4d))

## [1.4.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.3.0...v1.4.0) (2026-06-16)


### Features

* **chart:** wire suite coupling config + setup-wizard hint ([#114](https://github.com/sethbacon/terraform-state-manager-backend/issues/114)) ([45f265b](https://github.com/sethbacon/terraform-state-manager-backend/commit/45f265b84e207a16b84319db6b3a16ead5d8539e))
* **helm:** add setup.token value for a pre-seeded wizard token ([#116](https://github.com/sethbacon/terraform-state-manager-backend/issues/116)) ([7567e7e](https://github.com/sethbacon/terraform-state-manager-backend/commit/7567e7e3e21a322accce94051dec08308ecf054f))
* **setup:** first-run setup-wizard foundation (token gate + status) ([#109](https://github.com/sethbacon/terraform-state-manager-backend/issues/109)) ([b1cd98c](https://github.com/sethbacon/terraform-state-manager-backend/commit/b1cd98c09bd1ab12961920d78cc4a15e146b44e5))
* **setup:** owner, sources, and complete handlers (wizard backend done) ([#113](https://github.com/sethbacon/terraform-state-manager-backend/issues/113)) ([016a34c](https://github.com/sethbacon/terraform-state-manager-backend/commit/016a34c74b2a052c49bb09e4d26f65c99eb87ff6))
* **setup:** runtime OIDC config for the wizard (DB-backed + boot-reload) ([#112](https://github.com/sethbacon/terraform-state-manager-backend/issues/112)) ([31ec623](https://github.com/sethbacon/terraform-state-manager-backend/commit/31ec623c4bac775852414f80151ae27100e47f97))
* **setup:** support an operator-provided setup token (SETUP_TOKEN) ([#115](https://github.com/sethbacon/terraform-state-manager-backend/issues/115)) ([0c15f16](https://github.com/sethbacon/terraform-state-manager-backend/commit/0c15f1673975542d42381b0f3a8b05b8537460de))


### Refactor

* **auth:** make the OIDC provider runtime-swappable (atomic pointer) ([#111](https://github.com/sethbacon/terraform-state-manager-backend/issues/111)) ([99cbbeb](https://github.com/sethbacon/terraform-state-manager-backend/commit/99cbbebe812ae92f26bb1abe9c50b132e08ebdef))

## [1.3.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.2.0...v1.3.0) (2026-06-15)


### Features

* **api:** add module-provenance query endpoints ([#102](https://github.com/sethbacon/terraform-state-manager-backend/issues/102)) ([1309a3d](https://github.com/sethbacon/terraform-state-manager-backend/commit/1309a3d5b6273bcd523ad7e10c867e7712613922))
* **drift:** capture registry-module provenance from ingested plans ([#100](https://github.com/sethbacon/terraform-state-manager-backend/issues/100)) ([0a7cb08](https://github.com/sethbacon/terraform-state-manager-backend/commit/0a7cb082f38793259ae7c1f385b077c4ceb1e956))
* **suite:** canonical-host generated column + host-alias matching ([#106](https://github.com/sethbacon/terraform-state-manager-backend/issues/106)) ([d65fa2c](https://github.com/sethbacon/terraform-state-manager-backend/commit/d65fa2c89eeb89a883f0fff4fb5636b30d18bf39))
* **suite:** gate /consumers on a shared suite service token ([#103](https://github.com/sethbacon/terraform-state-manager-backend/issues/103)) ([8c47727](https://github.com/sethbacon/terraform-state-manager-backend/commit/8c477270fcb90a5a5127c59b4d938cd2c01ae4cb))


### Bug Fixes

* **suite:** canonicalize registry host on capture and consumers read ([#105](https://github.com/sethbacon/terraform-state-manager-backend/issues/105)) ([95acaa2](https://github.com/sethbacon/terraform-state-manager-backend/commit/95acaa26a8e8be8a7c01c69f7a2f60ebb7d93eb2))


### Refactor

* **suite:** adopt shared suite.CanonicalHost (drop local copy) ([#107](https://github.com/sethbacon/terraform-state-manager-backend/issues/107)) ([9074fa1](https://github.com/sethbacon/terraform-state-manager-backend/commit/9074fa17626a3bde1316f13c397348157ef9dd49))

## [1.2.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.1.0...v1.2.0) (2026-06-14)


### Features

* **identity:** support a separable identity database DSN ([#97](https://github.com/sethbacon/terraform-state-manager-backend/issues/97)) ([4f45441](https://github.com/sethbacon/terraform-state-manager-backend/commit/4f45441ee6cab3eb6080a1544f3d3c43860c2a91))
* **suite:** advertise and forward the identity shared-store signal ([#99](https://github.com/sethbacon/terraform-state-manager-backend/issues/99)) ([b7d0d72](https://github.com/sethbacon/terraform-state-manager-backend/commit/b7d0d72e9e7dc3c57562024c0daf321e006148b5))


### Bug Fixes

* **identity:** gate system-role seeding on suite.role_seed_owner ([#96](https://github.com/sethbacon/terraform-state-manager-backend/issues/96)) ([d91d911](https://github.com/sethbacon/terraform-state-manager-backend/commit/d91d911f418ebfa18ed142b5cac7eb99abb57027))

## [1.1.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.0.1...v1.1.0) (2026-06-14)


### Features

* **suite:** add runtime discovery contract (Phase 0) ([#88](https://github.com/sethbacon/terraform-state-manager-backend/issues/88)) ([2612c97](https://github.com/sethbacon/terraform-state-manager-backend/commit/2612c97de9fd00e5b4b48ad4f139b9b75ea433d8))


### Bug Fixes

* **helm:** activate CSP nonce in frontend nginx configmap ([#95](https://github.com/sethbacon/terraform-state-manager-backend/issues/95)) ([3793c03](https://github.com/sethbacon/terraform-state-manager-backend/commit/3793c035ce978502f489da8b015df6cd286cdbbe))
* **helm:** allow disabling OIDC require_verified_email via values ([#94](https://github.com/sethbacon/terraform-state-manager-backend/issues/94)) ([d7ffeda](https://github.com/sethbacon/terraform-state-manager-backend/commit/d7ffedade44de5d443a4be08081157524efb4953))
* **server:** restrict trusted proxies to prevent X-Forwarded-For spoofing ([#90](https://github.com/sethbacon/terraform-state-manager-backend/issues/90)) ([1defd69](https://github.com/sethbacon/terraform-state-manager-backend/commit/1defd696beb18508f00a36969238925ebf4b254a))


### Security

* enforce verified email and block cross-provider account rebind ([#91](https://github.com/sethbacon/terraform-state-manager-backend/issues/91)) ([c06d4fd](https://github.com/sethbacon/terraform-state-manager-backend/commit/c06d4fd50112ba9be828b2f5759518dc06b6e8dc))
* harden response headers with full HSTS/CSP/CORP/COOP/COEP ([#92](https://github.com/sethbacon/terraform-state-manager-backend/issues/92)) ([dc73597](https://github.com/sethbacon/terraform-state-manager-backend/commit/dc73597adf3eed3876a5ef25988e2628fc912c15))

## [1.0.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v1.0.0...v1.0.1) (2026-06-12)


### Documentation

* add Apache-2.0 LICENSE, NOTICE attributions, and license/disclaimer sections ([#86](https://github.com/sethbacon/terraform-state-manager-backend/issues/86)) ([03e14a9](https://github.com/sethbacon/terraform-state-manager-backend/commit/03e14a94ec6734c4fa465841e965febb8042b412))

## [1.0.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.10.1...v1.0.0) (2026-06-12)


### chore

* release 1.0.0 ([2383c4a](https://github.com/sethbacon/terraform-state-manager-backend/commit/2383c4aea454e1cd589efb2599e70e646a53ce32))

## [0.10.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.10.0...v0.10.1) (2026-06-12)


### Bug Fixes

* **deps:** patch grpc 1.79.3 (critical GHSA-p77j-4mvh-x3m3) and otel 1.44.0 advisories ([bd6e6b1](https://github.com/sethbacon/terraform-state-manager-backend/commit/bd6e6b1753ea685a781a62073612a009a0267830))

## [0.10.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.9.0...v0.10.0) (2026-06-12)


### Features

* alerting — notification channels + drift-event hook (ogtsm reconcile) ([45d350c](https://github.com/sethbacon/terraform-state-manager-backend/commit/45d350c04f5d1e567483f8bc14b1c36ec630d7d5))
* Consul, PostgreSQL, Kubernetes, and HTTP-backend state connectors (ogtsm reconcile) ([68eca94](https://github.com/sethbacon/terraform-state-manager-backend/commit/68eca946e5bfc86c9085481aeb72d89e6acacc11))
* cron scheduler — recurring drift runs (ogtsm reconcile) ([159b2cc](https://github.com/sethbacon/terraform-state-manager-backend/commit/159b2cce3c31e6112d4ed8292eea325e2e6a558b))
* dashboard overview aggregation endpoint (Phase C) ([60b3ffd](https://github.com/sethbacon/terraform-state-manager-backend/commit/60b3ffd5533a2aeeae0b025654ee8a993c85b9a8))
* identity-management admin read endpoints (Phase D) ([9ff0c31](https://github.com/sethbacon/terraform-state-manager-backend/commit/9ff0c310d14cf4197133eafe980b8979f38d934c))
* LDAP / Active Directory search-bind auth (security-hardened port) ([f514934](https://github.com/sethbacon/terraform-state-manager-backend/commit/f514934c9856c90b9b95f131157ae99d835dab24))
* mTLS client-certificate auth (security-hardened port) ([2888565](https://github.com/sethbacon/terraform-state-manager-backend/commit/2888565fa575c233da72ce75a53468962bca485e))
* OIDC group-to-role mapping (registry parity) ([f81e6cf](https://github.com/sethbacon/terraform-state-manager-backend/commit/f81e6cfea6298478ac2800bd712e361dee665da0))
* persistent state-analysis store with incremental sync ([a3eb45c](https://github.com/sethbacon/terraform-state-manager-backend/commit/a3eb45c601fae1d20e249f6f7e808afa013b593b))
* publish OpenAPI/Swagger spec from handler annotations (Phase B) ([f4274a5](https://github.com/sethbacon/terraform-state-manager-backend/commit/f4274a51cf6bd59511c25c52a686791662d90193))
* SAML 2.0 SSO auth (security-hardened port) ([4c202ac](https://github.com/sethbacon/terraform-state-manager-backend/commit/4c202acc70d7e1c37208c5230c27a1d3f5f632a0))
* SCIM 2.0 user/group provisioning (registry parity) ([7822c57](https://github.com/sethbacon/terraform-state-manager-backend/commit/7822c579a453bdb5a5f5375fbc166134e26c3363))
* **sources:** edit and test-connection endpoints ([2c2fedc](https://github.com/sethbacon/terraform-state-manager-backend/commit/2c2fedc84884eeb89158741c282548236439e09e))
* **sources:** overlay store sizes onto listings without byte sizes ([7a17c36](https://github.com/sethbacon/terraform-state-manager-backend/commit/7a17c362ea716f52944a803ee34ddf7da6a15f65))
* state outputs endpoint + version-token sync markers ([973c64c](https://github.com/sethbacon/terraform-state-manager-backend/commit/973c64c96b1320ed8ee037ea21a44006f9ca8f35))
* **statesource:** write support for hcp and git; create-on-write for k8s and nested local keys ([87d9465](https://github.com/sethbacon/terraform-state-manager-backend/commit/87d94653ba57656152b33f4f79a042a63cf46d1b))
* surface SSO providers + read-only SSO admin endpoint ([b33a3f1](https://github.com/sethbacon/terraform-state-manager-backend/commit/b33a3f1b83b8695d6261084651e006a5c0ed8a69))


### Bug Fixes

* **deps:** bump Go to 1.26.4 to resolve stdlib advisories ([cfaf23b](https://github.com/sethbacon/terraform-state-manager-backend/commit/cfaf23baf7c6dd1f5f94b8bbca2b8efb31205d99))
* **http-backend:** clamp absent Content-Length to zero size ([fbf193a](https://github.com/sethbacon/terraform-state-manager-backend/commit/fbf193ad359560b643acf6f3cf1c642310a70b36))
* **security:** deprovision IdP-mapped roles on group loss + first-login-only default ([19879a1](https://github.com/sethbacon/terraform-state-manager-backend/commit/19879a15391f6d26d4ff0440bdf7dd3af336fd5d))


### Documentation

* API keys UI moved to /admin/apikeys (Identity group) ([1365799](https://github.com/sethbacon/terraform-state-manager-backend/commit/1365799fc0db8bd3f2b946b8abe04587b44920f8))
