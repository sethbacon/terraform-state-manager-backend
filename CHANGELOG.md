# Changelog

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
