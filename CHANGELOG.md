<!-- markdownlint-disable MD024 -->

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
