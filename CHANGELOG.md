# Changelog

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
