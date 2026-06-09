<!-- markdownlint-disable MD024 -->

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.8.0...v0.9.0) (2026-06-09)


### Features

* **analysis:** flag provider pin drift against the public registry ([#70](https://github.com/sethbacon/terraform-state-manager-backend/issues/70)) ([cd6abbd](https://github.com/sethbacon/terraform-state-manager-backend/commit/cd6abbde76b4b3daa5a45cd6fcc9e8e5566e3969))
* **drift:** Azure read-only state-vs-environment drift engine ([#74](https://github.com/sethbacon/terraform-state-manager-backend/issues/74)) ([e3f90cb](https://github.com/sethbacon/terraform-state-manager-backend/commit/e3f90cb79208a1312c2f5d425dabf3c9d668499c))
* **drift:** outbound ADO plan-trigger with WIF federated-token provider ([#75](https://github.com/sethbacon/terraform-state-manager-backend/issues/75)) ([ccf9e51](https://github.com/sethbacon/terraform-state-manager-backend/commit/ccf9e515bd60f04a69b1e3614f549fd9f5bb3652))
* **drift:** schedule + expose env-drift and outbound-trigger capabilities ([#77](https://github.com/sethbacon/terraform-state-manager-backend/issues/77)) ([37e2c2b](https://github.com/sethbacon/terraform-state-manager-backend/commit/37e2c2b4ff1ee8c7fca177648e95fede97f37a1f))
* **migration:** ADO repo-migration execute path (REST, idempotent, resumable) ([#71](https://github.com/sethbacon/terraform-state-manager-backend/issues/71)) ([cc7b930](https://github.com/sethbacon/terraform-state-manager-backend/commit/cc7b9305bfcfa7e1153e94fc7c01202d890ac069))
* **migration:** git-history push for ADO repo migration (go-git) ([#76](https://github.com/sethbacon/terraform-state-manager-backend/issues/76)) ([5f7b2fa](https://github.com/sethbacon/terraform-state-manager-backend/commit/5f7b2fa73b5c45a9aecd8622dcfc2c837d87be90))
* **sources:** link state sources to ADO repos/pipelines ([#78](https://github.com/sethbacon/terraform-state-manager-backend/issues/78)) ([386397f](https://github.com/sethbacon/terraform-state-manager-backend/commit/386397f70ff9b51a2224724aebb01dde7a8f5feb))

## [0.8.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.7.0...v0.8.0) (2026-06-08)


### Features

* **compliance:** expose engine_type via the policy API + engines list ([#67](https://github.com/sethbacon/terraform-state-manager-backend/issues/67)) ([8d6e39a](https://github.com/sethbacon/terraform-state-manager-backend/commit/8d6e39a72a09fe3f75da9f61c7bc82f9c84600d4)), closes [#66](https://github.com/sethbacon/terraform-state-manager-backend/issues/66)


### Bug Fixes

* **auth:** redirect to OIDC end-session on logout instead of returning JSON ([#69](https://github.com/sethbacon/terraform-state-manager-backend/issues/69)) ([4c1ae16](https://github.com/sethbacon/terraform-state-manager-backend/commit/4c1ae1674c34185b1e32b300c865e08e4ac832cc))

## [0.7.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.6.0...v0.7.0) (2026-06-08)


### Features

* **analysis:** extract required_version + lockfile pins and flag TF version drift ([#62](https://github.com/sethbacon/terraform-state-manager-backend/issues/62)) ([430c49f](https://github.com/sethbacon/terraform-state-manager-backend/commit/430c49ffaace4d0b6a67b23de47a9c6821f29072))
* **compliance:** add embedded OPA/Rego engine with cross-engine parity ([#59](https://github.com/sethbacon/terraform-state-manager-backend/issues/59)) ([6eecd1d](https://github.com/sethbacon/terraform-state-manager-backend/commit/6eecd1d91935f74bb3b7b9ba2d93fcac402e42ae)), closes [#57](https://github.com/sethbacon/terraform-state-manager-backend/issues/57)
* **config:** add ui_theme/whitelabel config endpoint for FE parity ([#55](https://github.com/sethbacon/terraform-state-manager-backend/issues/55)) ([ef8926c](https://github.com/sethbacon/terraform-state-manager-backend/commit/ef8926c4b57bc9dfa32c74524cd056efdc88e53f)), closes [#54](https://github.com/sethbacon/terraform-state-manager-backend/issues/54)
* **drift:** ingest ADO plan-JSON via OIDC-authenticated webhook into code-drift events ([#60](https://github.com/sethbacon/terraform-state-manager-backend/issues/60)) ([2679622](https://github.com/sethbacon/terraform-state-manager-backend/commit/26796229f680100b1577a62a2fe86ecb951e03ed)), closes [#58](https://github.com/sethbacon/terraform-state-manager-backend/issues/58)
* **extensibility:** capability contract + version-no-op-test capability ([#65](https://github.com/sethbacon/terraform-state-manager-backend/issues/65)) ([a84e875](https://github.com/sethbacon/terraform-state-manager-backend/commit/a84e87512c5b2c9f521419d6e0162d207514f91d)), closes [#64](https://github.com/sethbacon/terraform-state-manager-backend/issues/64)

## [0.6.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.5.1...v0.6.0) (2026-06-07)


### Features

* **backup,migration:** checksum-verified restore round-trip and post-transfer migration verification ([#52](https://github.com/sethbacon/terraform-state-manager-backend/issues/52)) ([a6d2f3c](https://github.com/sethbacon/terraform-state-manager-backend/commit/a6d2f3c0306f8d4a677464137e5ce135f5891dac)), closes [#48](https://github.com/sethbacon/terraform-state-manager-backend/issues/48)
* **clients:** add read-only Azure DevOps REST client + dry-run migration plan ([#51](https://github.com/sethbacon/terraform-state-manager-backend/issues/51)) ([fb6b723](https://github.com/sethbacon/terraform-state-manager-backend/commit/fb6b7238e804e18530b50a21ea8c2636fa7c830a)), closes [#47](https://github.com/sethbacon/terraform-state-manager-backend/issues/47)


### Bug Fixes

* **migration:** honor per-job storage config in migration factory ([#49](https://github.com/sethbacon/terraform-state-manager-backend/issues/49)) ([d778862](https://github.com/sethbacon/terraform-state-manager-backend/commit/d77886253d806184b463f66f3a27c44e3b9741fc)), closes [#45](https://github.com/sethbacon/terraform-state-manager-backend/issues/45)


### Refactor

* **compliance:** extract PolicyEngine interface + CustomRulesEngine behind Checker ([#50](https://github.com/sethbacon/terraform-state-manager-backend/issues/50)) ([56c6c03](https://github.com/sethbacon/terraform-state-manager-backend/commit/56c6c0321cac620cd4ae07487bef018d81aac3ee))

## [0.5.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.5.0...v0.5.1) (2026-06-07)


### Bug Fixes

* **deps:** bump terraform-suite-identity to v0.12.0 ([#43](https://github.com/sethbacon/terraform-state-manager-backend/issues/43)) ([8aeafba](https://github.com/sethbacon/terraform-state-manager-backend/commit/8aeafbade8f683e78560660c89b7c2d0dfe6a8e8))

## [0.5.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.4.0...v0.5.0) (2026-06-07)


### Features

* **identity:** unify identity + auth on the shared canonical model ([#39](https://github.com/sethbacon/terraform-state-manager-backend/issues/39)) ([a0b74d7](https://github.com/sethbacon/terraform-state-manager-backend/commit/a0b74d79169391307a98070f5ab2193be2e2162b))

## [0.4.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.3.3...v0.4.0) (2026-06-05)


### Features

* **auth:** add reversible identity-schema cutover flag ([#36](https://github.com/sethbacon/terraform-state-manager-backend/issues/36)) ([601f779](https://github.com/sethbacon/terraform-state-manager-backend/commit/601f7791156ecf5aa292b7541345688f211aaba9))

## [0.3.3](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.3.2...v0.3.3) (2026-06-05)


### Refactor

* **db:** consume identity models + store from the module ([#34](https://github.com/sethbacon/terraform-state-manager-backend/issues/34)) ([02cebf5](https://github.com/sethbacon/terraform-state-manager-backend/commit/02cebf5ac9e3e9b2bb0f4d5e280f9e89061ea77b))

## [0.3.2](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.3.1...v0.3.2) (2026-06-05)


### Refactor

* **auth:** delegate OIDC provider to the identity module ([#32](https://github.com/sethbacon/terraform-state-manager-backend/issues/32)) ([0cf6eec](https://github.com/sethbacon/terraform-state-manager-backend/commit/0cf6eec7104b9d8dc8d0eaf28b658cfba9ded2a8))

## [0.3.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.3.0...v0.3.1) (2026-06-04)


### Refactor

* **auth:** delegate API key gen/validation to the identity module ([#31](https://github.com/sethbacon/terraform-state-manager-backend/issues/31)) ([7706e52](https://github.com/sethbacon/terraform-state-manager-backend/commit/7706e52e1406cb652a8d687a031520cf4fb09195))
* **auth:** delegate JWT to the identity TokenManager ([#28](https://github.com/sethbacon/terraform-state-manager-backend/issues/28)) ([b60fab7](https://github.com/sethbacon/terraform-state-manager-backend/commit/b60fab7209a45faad7a74c679eef2d11260531c8))

## [0.3.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.2.1...v0.3.0) (2026-06-04)


### Features

* **auth:** delegate scope-checking to identity module v0.3.0 ([#26](https://github.com/sethbacon/terraform-state-manager-backend/issues/26)) ([18b8a7e](https://github.com/sethbacon/terraform-state-manager-backend/commit/18b8a7ebf603a2e50628af4385139543b4ce0a1d))

## [0.2.1](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.2.0...v0.2.1) (2026-06-04)


### Bug Fixes

* **auth:** skip env-managed OIDC secret instead of logging a decrypt error ([#24](https://github.com/sethbacon/terraform-state-manager-backend/issues/24)) ([dbc1804](https://github.com/sethbacon/terraform-state-manager-backend/commit/dbc180486359cd716f1f3597f7e41f06e0ffce20))

## [0.2.0](https://github.com/sethbacon/terraform-state-manager-backend/compare/v0.1.0...v0.2.0) (2026-06-04)


### Features

* add shared identity schema migrations ([#20](https://github.com/sethbacon/terraform-state-manager-backend/issues/20)) ([04b990d](https://github.com/sethbacon/terraform-state-manager-backend/commit/04b990def4cfcba8607bf26e7f1b4fe6a9d5e392)), closes [#19](https://github.com/sethbacon/terraform-state-manager-backend/issues/19)


### Bug Fixes

* serve valid YAML from the /swagger.yaml endpoint ([#16](https://github.com/sethbacon/terraform-state-manager-backend/issues/16)) ([2406655](https://github.com/sethbacon/terraform-state-manager-backend/commit/240665565bf4816427a226b93e5909c54809a79e)), closes [#15](https://github.com/sethbacon/terraform-state-manager-backend/issues/15)


### Documentation

* add architecture decision records and correct OpenAPI doc ([#12](https://github.com/sethbacon/terraform-state-manager-backend/issues/12)) ([e40addd](https://github.com/sethbacon/terraform-state-manager-backend/commit/e40addde893fbd2eba895268fdf412a483e5c46a)), closes [#11](https://github.com/sethbacon/terraform-state-manager-backend/issues/11)


### Refactor

* consume identity from terraform-suite-identity module ([#22](https://github.com/sethbacon/terraform-state-manager-backend/issues/22)) ([c03fd22](https://github.com/sethbacon/terraform-state-manager-backend/commit/c03fd2275d8bb93ce62d47e8a200498110299c6c))

## [Unreleased]

---

## [0.1.0] - 2026-03-04

- Initial commit
