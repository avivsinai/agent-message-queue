# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Release Please generates new entries from conventional squash commits on
`main`; richer or multi-entry notes can be added through commit overrides or by
editing the release PR.

## [0.64.1](https://github.com/avivsinai/agent-message-queue/compare/v0.64.0...v0.64.1) (2026-08-18)


### Bug Fixes

* **wake:** reclaim a darwin restart stage whose parent directory is gone ([#572](https://github.com/avivsinai/agent-message-queue/issues/572)) ([acd67b9](https://github.com/avivsinai/agent-message-queue/commit/acd67b982a6343e192ebe550ab943217bbf551ef))

## [0.64.0](https://github.com/avivsinai/agent-message-queue/compare/v0.63.3...v0.64.0) (2026-08-18)


### Features

* **launchapi:** add base_root authority with planned base creation ([#564](https://github.com/avivsinai/agent-message-queue/issues/564)) ([fbe05b1](https://github.com/avivsinai/agent-message-queue/commit/fbe05b1e9d12ec2bd4965bff442e64b18371e3ca))
* **launchapi:** add initial_input and tool-policy grammar ([#562](https://github.com/avivsinai/agent-message-queue/issues/562)) ([6cd6353](https://github.com/avivsinai/agent-message-queue/commit/6cd6353d8432e8fcc1c9cf72bf2722fce928c5f3))
* **launchapi:** add on_live keep|refuse with per-member dispositions ([#565](https://github.com/avivsinai/agent-message-queue/issues/565)) ([2f91cf2](https://github.com/avivsinai/agent-message-queue/commit/2f91cf2987b13c248aa9c31120d794365e979818))
* **launchapi:** add typed placement with negotiation and preview ([55287d4](https://github.com/avivsinai/agent-message-queue/commit/55287d45b217de1c46cd8611ed420fdd9afcbeab))
* **launchapi:** add typed wrapper pre-exec with trust-bound argv ([#568](https://github.com/avivsinai/agent-message-queue/issues/568)) ([a6b8b8f](https://github.com/avivsinai/agent-message-queue/commit/a6b8b8f15ae70ee953c16e2380615ac7d5d8c989))
* **launchapi:** bind and persist caller context in subject and evidence ([#566](https://github.com/avivsinai/agent-message-queue/issues/566)) ([0ef3371](https://github.com/avivsinai/agent-message-queue/commit/0ef3371ad0ad44b2d8c4ea49dedca51750937292))
* **launchapi:** bind provider executable identity into prepare subject ([#567](https://github.com/avivsinai/agent-message-queue/issues/567)) ([a466fd5](https://github.com/avivsinai/agent-message-queue/commit/a466fd5fd567ef2d0c73e59d58abc9f3ef810859))
* **launchapi:** record subject_schema and advertise contract 0.61.1 ([#559](https://github.com/avivsinai/agent-message-queue/issues/559)) ([0b00d9f](https://github.com/avivsinai/agent-message-queue/commit/0b00d9f29567b67b93123564ff4f8357a1ce953c))
* **launch:** bind placement to owned resources with marker-auth close ([#561](https://github.com/avivsinai/agent-message-queue/issues/561)) ([b532f1c](https://github.com/avivsinai/agent-message-queue/commit/b532f1c7f698d4a79139ad1ba1f74bd1aa681bd7))


### Bug Fixes

* **cli:** reject trailing JSON on launch --placement ([55287d4](https://github.com/avivsinai/agent-message-queue/commit/55287d45b217de1c46cd8611ed420fdd9afcbeab))

## [0.63.3](https://github.com/avivsinai/agent-message-queue/compare/v0.63.2...v0.63.3) (2026-08-18)


### Bug Fixes

* **wake:** inject stop preparer for fixture tests ([#551](https://github.com/avivsinai/agent-message-queue/issues/551)) ([13bba2a](https://github.com/avivsinai/agent-message-queue/commit/13bba2adcf01e42b480670828ae866fa6f4642ed))

## [0.63.2](https://github.com/avivsinai/agent-message-queue/compare/v0.63.1...v0.63.2) (2026-08-17)


### Bug Fixes

* **symphony:** select managed fragment bounds once for detect and inject ([f20cdcb](https://github.com/avivsinai/agent-message-queue/commit/f20cdcb1bb991f97cb0e1334e93f12a57eaccb98))

## [0.63.1](https://github.com/avivsinai/agent-message-queue/compare/v0.63.0...v0.63.1) (2026-08-17)


### Bug Fixes

* **dlq:** verify purge content under the envelope lock ([#545](https://github.com/avivsinai/agent-message-queue/issues/545)) ([e499e67](https://github.com/avivsinai/agent-message-queue/commit/e499e67b053d5a6fcf8344bd988afaccbeda4f1f))
* **init:** provision reserved user mailbox so fresh queue passes doctor ([#539](https://github.com/avivsinai/agent-message-queue/issues/539)) ([776a13d](https://github.com/avivsinai/agent-message-queue/commit/776a13dd6134e39198635a48c986aa5810f5f739))
* **wake:** confirm SIGKILL with a 3s window, not 100ms SIGTERM grace ([#540](https://github.com/avivsinai/agent-message-queue/issues/540)) ([d77d8b1](https://github.com/avivsinai/agent-message-queue/commit/d77d8b1cd152000fbdbc166ecfc33df451b8d622))

## [0.63.0](https://github.com/avivsinai/agent-message-queue/compare/v0.62.0...v0.63.0) (2026-08-17)


### Features

* **launch:** add cursor-agent capture adapter ([#526](https://github.com/avivsinai/agent-message-queue/issues/526)) ([3289365](https://github.com/avivsinai/agent-message-queue/commit/3289365eeec80eff6061021d9179158b0e000757))
* **launch:** add managed cmux backend ([b505360](https://github.com/avivsinai/agent-message-queue/commit/b5053606afa600cdab50e2f0f8ecea32305d4716))
* **launch:** add managed Ghostty backend ([083e43b](https://github.com/avivsinai/agent-message-queue/commit/083e43b87b224508307d85fd5482d7c87977ab2e))
* **launch:** capture Codex identity through an adapter-owned notify hook ([#529](https://github.com/avivsinai/agent-message-queue/issues/529)) ([532c414](https://github.com/avivsinai/agent-message-queue/commit/532c4149d6dfd8dc18e63615c65e6529ea720d89))
* **launch:** no_action aggregate and --require-agent; document symlink trust boundary ([#524](https://github.com/avivsinai/agent-message-queue/issues/524)) ([be5ae39](https://github.com/avivsinai/agent-message-queue/commit/be5ae3924243992c1998cf4ff2ef18b1ad8eda03))


### Bug Fixes

* **build:** embed the same bare semver as GoReleaser ([d837886](https://github.com/avivsinai/agent-message-queue/commit/d8378869ac13cc11b75d7f46320d4cb3ededde56))
* **keepalive:** identify cmux surfaces by UUID when tty is absent ([056cc56](https://github.com/avivsinai/agent-message-queue/commit/056cc56f799775252925d1ea687be20601eaf575))
* **launch:** close Ghostty create orphans and pin DefaultBackends ([083e43b](https://github.com/avivsinai/agent-message-queue/commit/083e43b87b224508307d85fd5482d7c87977ab2e))
* **launch:** parse cmux OK workspace ack and keep operator focus ([b505360](https://github.com/avivsinai/agent-message-queue/commit/b5053606afa600cdab50e2f0f8ecea32305d4716))
* **launch:** pin cmux protocol 2 and orphan-close on create timeout ([b505360](https://github.com/avivsinai/agent-message-queue/commit/b5053606afa600cdab50e2f0f8ecea32305d4716))
* **launch:** send cmux input with workspace and restore selection ([b505360](https://github.com/avivsinai/agent-message-queue/commit/b5053606afa600cdab50e2f0f8ecea32305d4716))
* **release:** look up the exact release tag instead of scanning the releases list ([#535](https://github.com/avivsinai/agent-message-queue/issues/535)) ([0317640](https://github.com/avivsinai/agent-message-queue/commit/03176403d3b967c4ad194e167075c5b076841c74))

## [0.62.0](https://github.com/avivsinai/agent-message-queue/compare/v0.61.0...v0.62.0) (2026-08-17)


### Features

* **launchapi:** Apply with atomic roster provisioning and no-replace session publication ([#520](https://github.com/avivsinai/agent-message-queue/issues/520)) ([a9bbbeb](https://github.com/avivsinai/agent-message-queue/commit/a9bbbeb675a2d2fe6199ab628a771227c198ac00))
* **launchapi:** public launch intent contract with fail-closed codecs ([#517](https://github.com/avivsinai/agent-message-queue/issues/517)) ([87050d6](https://github.com/avivsinai/agent-message-queue/commit/87050d6e3de017136a3418512e8cdab22cd24f3c))
* **launchapi:** public lifecycle API and immutable capture evidence ([#522](https://github.com/avivsinai/agent-message-queue/issues/522)) ([0867c13](https://github.com/avivsinai/agent-message-queue/commit/0867c13f3014d748cb5248ffd08587d82368f903))
* **launchapi:** zero-write Prepare with typed required actions ([#519](https://github.com/avivsinai/agent-message-queue/issues/519)) ([d8fb934](https://github.com/avivsinai/agent-message-queue/commit/d8fb934f8e43c5e8bf48ebf21bbb3150729b642e))
* **launch:** exact-root coop exec emission with typed execution options ([#521](https://github.com/avivsinai/agent-message-queue/issues/521)) ([b17e8a7](https://github.com/avivsinai/agent-message-queue/commit/b17e8a7b2bd9b8ef6a56dee2f1e9018e220551a8))
* **launch:** public CLI plan workflow, golden parity, and end-to-end matrix ([#523](https://github.com/avivsinai/agent-message-queue/issues/523)) ([fc89b1f](https://github.com/avivsinai/agent-message-queue/commit/fc89b1f819cd2fd5c81370ec0b48f505d1f223ac))


### Dependencies

* bump actions/attest from 4.2.1 to 4.2.2 ([#505](https://github.com/avivsinai/agent-message-queue/issues/505)) ([38aad4a](https://github.com/avivsinai/agent-message-queue/commit/38aad4ab5f391233708754c2084f92356939f1f3))

## [0.61.0](https://github.com/avivsinai/agent-message-queue/compare/v0.60.5...v0.61.0) (2026-08-14)


### Features

* **cli:** add amq launch and session resume reconciliation engine ([#502](https://github.com/avivsinai/agent-message-queue/issues/502)) ([2df983a](https://github.com/avivsinai/agent-message-queue/commit/2df983a6e19bfb1e2acff8382bdcb67865ebcfa2)), closes [#480](https://github.com/avivsinai/agent-message-queue/issues/480)
* **cli:** add amq setup preview-then-commit porcelain ([#500](https://github.com/avivsinai/agent-message-queue/issues/500)) ([fb723fe](https://github.com/avivsinai/agent-message-queue/commit/fb723fe5d73552d93d511c575e39832b2adb88a9))
* **cli:** add exit code 6 (action_required) and launch aggregate precedence ([#493](https://github.com/avivsinai/agent-message-queue/issues/493)) ([ca96060](https://github.com/avivsinai/agent-message-queue/commit/ca960605a3a1b430cbf4dd6f7f17b6f1a987192b))
* **cli:** add session create and session list commands ([#496](https://github.com/avivsinai/agent-message-queue/issues/496)) ([cb96416](https://github.com/avivsinai/agent-message-queue/commit/cb964164104c0ff58de201939d541d32adee86c1))
* **cli:** add stateless setup preview with digest-gated apply ([#513](https://github.com/avivsinai/agent-message-queue/issues/513)) ([01fd066](https://github.com/avivsinai/agent-message-queue/commit/01fd0667a4ba1148994516f4950a2df18d1505f2))
* **coop:** honor declared default_session and deprecate exec creation paths ([#501](https://github.com/avivsinai/agent-message-queue/issues/501)) ([2cb22fa](https://github.com/avivsinai/agent-message-queue/commit/2cb22fa895a9ab48bcb9a0a86e38a56db713e3ee))
* **launch:** add backend contract, commands plan_only backend, and conformance skeleton ([#498](https://github.com/avivsinai/agent-message-queue/issues/498)) ([5a5073a](https://github.com/avivsinai/agent-message-queue/commit/5a5073ad82f49c5932e78dd18885bf588f672427))
* **launch:** add harness adapter contract and tier-1 identity adapters ([#497](https://github.com/avivsinai/agent-message-queue/issues/497)) ([84d0fa2](https://github.com/avivsinai/agent-message-queue/commit/84d0fa28f3a315f55b39dfc58ebad96f6396401f))
* **launch:** add managed tmux backend with journal-backed recovery ([#514](https://github.com/avivsinai/agent-message-queue/issues/514)) ([5fd0750](https://github.com/avivsinai/agent-message-queue/commit/5fd0750193d18c3b809408a288d3659e91df5af6))
* **launch:** add managed-create recovery journal and execution acknowledgement ([#510](https://github.com/avivsinai/agent-message-queue/issues/510)) ([26f6b6c](https://github.com/avivsinai/agent-message-queue/commit/26f6b6cfe113ee3b46beb495320fb379507151fc)), closes [#480](https://github.com/avivsinai/agent-message-queue/issues/480)
* **launch:** add session launch lease with handle locks and lease-gated binding writes ([#499](https://github.com/avivsinai/agent-message-queue/issues/499)) ([64c232f](https://github.com/avivsinai/agent-message-queue/commit/64c232ff64d3a79eab5bc102e02014bb5aebc255))
* **launch:** add versioned launch plan, binding, and trust contracts ([#495](https://github.com/avivsinai/agent-message-queue/issues/495)) ([1b9c0f3](https://github.com/avivsinai/agent-message-queue/commit/1b9c0f35c7b4df23fd4a79af6a312e80a5d0bb12))


### Bug Fixes

* **launch:** bind execution trust digest to session and root identity ([#511](https://github.com/avivsinai/agent-message-queue/issues/511)) ([7159908](https://github.com/avivsinai/agent-message-queue/commit/71599084ecc5daeebc9ebda821c6119485514070))
* **launch:** gate capture-mode planning on declared capabilities ([#512](https://github.com/avivsinai/agent-message-queue/issues/512)) ([51d9218](https://github.com/avivsinai/agent-message-queue/commit/51d921895e11e9ec5ec1d562cdcb3d73eb02ac64))
* **launch:** gate conversation continuity on execution evidence ([#508](https://github.com/avivsinai/agent-message-queue/issues/508)) ([6bf355e](https://github.com/avivsinai/agent-message-queue/commit/6bf355e788417e743c5a31a6ebbebe4e05533b5b))

## [0.60.5](https://github.com/avivsinai/agent-message-queue/compare/v0.60.4...v0.60.5) (2026-08-13)


### Bug Fixes

* **coop:** refuse provisioning into non-queue roots and resolve the binary first ([#491](https://github.com/avivsinai/agent-message-queue/issues/491)) ([2b69010](https://github.com/avivsinai/agent-message-queue/commit/2b69010efcd7311f353818b4ab027d5bb837261d))

## [0.60.4](https://github.com/avivsinai/agent-message-queue/compare/v0.60.3...v0.60.4) (2026-08-12)


### Bug Fixes

* **fsq:** reconcile windows claim residue when the source name is already gone ([#490](https://github.com/avivsinai/agent-message-queue/issues/490)) ([037794a](https://github.com/avivsinai/agent-message-queue/commit/037794a69c8ec44cc318a53568b9cf49d21f02fc))
* **wake:** verify lock machine identity instead of drifting hostname ([#488](https://github.com/avivsinai/agent-message-queue/issues/488)) ([449b49e](https://github.com/avivsinai/agent-message-queue/commit/449b49ed4270673db5131dd3fe67ed05e3b32bdf))

## [0.60.3](https://github.com/avivsinai/agent-message-queue/compare/v0.60.2...v0.60.3) (2026-08-11)


### Bug Fixes

* **fsq:** make concurrent drain claims exclusive on Windows ([#486](https://github.com/avivsinai/agent-message-queue/issues/486)) ([60a819f](https://github.com/avivsinai/agent-message-queue/commit/60a819f1045be0a079d1598c281438ff00550769))

## [0.60.2](https://github.com/avivsinai/agent-message-queue/compare/v0.60.1...v0.60.2) (2026-08-11)


### Bug Fixes

* **ci:** isolate git env in fixture tests and pre-push hook ([#482](https://github.com/avivsinai/agent-message-queue/issues/482)) ([9c973f6](https://github.com/avivsinai/agent-message-queue/commit/9c973f6b240d15bb25e9a769bf79e010c87c5837))

## [0.60.1](https://github.com/avivsinai/agent-message-queue/compare/v0.60.0...v0.60.1) (2026-08-10)


### Bug Fixes

* **wake:** reuse detached external injectors safely ([#478](https://github.com/avivsinai/agent-message-queue/issues/478)) ([9a343d5](https://github.com/avivsinai/agent-message-queue/commit/9a343d5c0817c99bcc3ad421c095c9acdea2e720))

## [0.60.0](https://github.com/avivsinai/agent-message-queue/compare/v0.59.1...v0.60.0) (2026-08-07)


### Features

* **wake:** retain bounded refused self-upgrade history ([4290592](https://github.com/avivsinai/agent-message-queue/commit/4290592cbbdb761d28b38b6e7938968d4776089b))


### Bug Fixes

* **wake:** preserve pending restart after notify race ([4290592](https://github.com/avivsinai/agent-message-queue/commit/4290592cbbdb761d28b38b6e7938968d4776089b))

## [0.59.1](https://github.com/avivsinai/agent-message-queue/compare/v0.59.0...v0.59.1) (2026-08-07)


### Bug Fixes

* **keepalive:** isolate degraded cmux ownership ([#473](https://github.com/avivsinai/agent-message-queue/issues/473)) ([38933f8](https://github.com/avivsinai/agent-message-queue/commit/38933f87661e5e2a5c07076b11fa50dc69828cb7))
* **wake:** recover stalled self-upgrades ([#474](https://github.com/avivsinai/agent-message-queue/issues/474)) ([6f894a7](https://github.com/avivsinai/agent-message-queue/commit/6f894a7e39c01c8d065bc31e13eacdb5817098f1))

## [0.59.0](https://github.com/avivsinai/agent-message-queue/compare/v0.58.0...v0.59.0) (2026-08-07)


### Features

* **wake:** automatically adopt newer installed images ([#471](https://github.com/avivsinai/agent-message-queue/issues/471)) ([441bc0d](https://github.com/avivsinai/agent-message-queue/commit/441bc0d13c1e9eea3e022a4df2b484187fc85315))

## [0.58.0](https://github.com/avivsinai/agent-message-queue/compare/v0.57.3...v0.58.0) (2026-08-07)


### Features

* **keepalive:** refresh stale wake images ([#460](https://github.com/avivsinai/agent-message-queue/issues/460)) ([2db7c55](https://github.com/avivsinai/agent-message-queue/commit/2db7c554592b30b11c0b930c9d9a80fc706a73ac))

## [0.57.3](https://github.com/avivsinai/agent-message-queue/compare/v0.57.2...v0.57.3) (2026-08-07)


### Bug Fixes

* **wake:** harden restart control plane ([#468](https://github.com/avivsinai/agent-message-queue/issues/468)) ([73cbbe9](https://github.com/avivsinai/agent-message-queue/commit/73cbbe9428db509e8d54cd282a2356e34d09ed80))

## [0.57.2](https://github.com/avivsinai/agent-message-queue/compare/v0.57.1...v0.57.2) (2026-08-07)


### Bug Fixes

* **test:** restore portable wake helper ([#466](https://github.com/avivsinai/agent-message-queue/issues/466)) ([e6e47aa](https://github.com/avivsinai/agent-message-queue/commit/e6e47aa74cde6409313071c1a7386cd1f0780cb5))

## [0.57.1](https://github.com/avivsinai/agent-message-queue/compare/v0.57.0...v0.57.1) (2026-08-07)


### Bug Fixes

* **release:** enforce Skild alias ownership ([#463](https://github.com/avivsinai/agent-message-queue/issues/463)) ([8352211](https://github.com/avivsinai/agent-message-queue/commit/83522110a53b50acd88e4301ec9c4b76c49cba3f))

## [0.57.0](https://github.com/avivsinai/agent-message-queue/compare/v0.56.1...v0.57.0) (2026-08-07)


### Features

* **wake:** add agent-safe self restart ([#461](https://github.com/avivsinai/agent-message-queue/issues/461)) ([61ef16e](https://github.com/avivsinai/agent-message-queue/commit/61ef16ed477012f9cc1c7ec1a27bd56f3fcd53bc))

## [0.56.1](https://github.com/avivsinai/agent-message-queue/compare/v0.56.0...v0.56.1) (2026-08-07)


### Bug Fixes

* **wake:** skip Codex LF prelude for submit-only reminders ([#458](https://github.com/avivsinai/agent-message-queue/issues/458)) ([f28a298](https://github.com/avivsinai/agent-message-queue/commit/f28a298965e5018f6d968668cee0b44a4dcf91bf))

## [0.56.0](https://github.com/avivsinai/agent-message-queue/compare/v0.55.0...v0.56.0) (2026-08-07)


### Features

* **wake:** derive Darwin mapped image identity ([#454](https://github.com/avivsinai/agent-message-queue/issues/454)) ([bd23d70](https://github.com/avivsinai/agent-message-queue/commit/bd23d70288618d31987e7173068951d93232ba65))


### Dependencies

* bump actions/attest from 4.2.0 to 4.2.1 ([#455](https://github.com/avivsinai/agent-message-queue/issues/455)) ([edcede7](https://github.com/avivsinai/agent-message-queue/commit/edcede77ed9ebea215251c3caec5cce6a65c6495))

## [0.55.0](https://github.com/avivsinai/agent-message-queue/compare/v0.54.0...v0.55.0) (2026-08-06)


### Features

* **keepalive:** add integrated wake supervisor ([#294](https://github.com/avivsinai/agent-message-queue/issues/294)) ([2b5b6fc](https://github.com/avivsinai/agent-message-queue/commit/2b5b6fc91a848e29c3ff18b703818ccb134d5c1c))

## [0.54.0](https://github.com/avivsinai/agent-message-queue/compare/v0.53.0...v0.54.0) (2026-08-06)


### Features

* **wake:** acknowledge injected doorbells with --retry-until injected ([#424](https://github.com/avivsinai/agent-message-queue/issues/424)) ([96a872b](https://github.com/avivsinai/agent-message-queue/commit/96a872b5c6ad593112c6123c0180574f3326ed12))

## [0.53.0](https://github.com/avivsinai/agent-message-queue/compare/v0.52.6...v0.53.0) (2026-08-06)


### Features

* **wake:** enforce fail-closed bound-state reads ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** gate bound-lock mutations and document rollback ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** publish state-bound locks for new claims ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))


### Bug Fixes

* **doctor:** fail closed on root-wide wake quarantine scan errors ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** apply submit-only reminder nudge to all injecting wakes, not just owner-bound ([#449](https://github.com/avivsinai/agent-message-queue/issues/449)) ([a179217](https://github.com/avivsinai/agent-message-queue/commit/a1792171690aaf8480cf4e21943acb0c65df6c68))
* **wake:** close P2b review contract gaps ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** name a runnable orphan-target remedy ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** quarantine orphan lifecycle artifacts ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** reconcile bound prepared projections ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** reject ambiguous lock JSON ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** retire exact claims durably ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** retry bound state observation failures ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))
* **wake:** surface inconclusive doctor target reads ([82fc2ae](https://github.com/avivsinai/agent-message-queue/commit/82fc2ae57552eebcbebe6b935d896b3f37df4395))

## [0.52.6](https://github.com/avivsinai/agent-message-queue/compare/v0.52.5...v0.52.6) (2026-08-06)


### Bug Fixes

* **ci:** capture output before grep -q in smoke test to avoid SIGPIPE flake ([#442](https://github.com/avivsinai/agent-message-queue/issues/442)) ([cd8777e](https://github.com/avivsinai/agent-message-queue/commit/cd8777edfc44b9fc8f84faf33b15a18a2c843c2c))
* **test:** make reload-transport FD-leak check immune to concurrent descriptor churn ([#447](https://github.com/avivsinai/agent-message-queue/issues/447)) ([eaccc52](https://github.com/avivsinai/agent-message-queue/commit/eaccc52907acc923312b7c2409e27e095dc85725)), closes [#444](https://github.com/avivsinai/agent-message-queue/issues/444)
* **wake:** nudge submit instead of re-typing reminder payloads ([#445](https://github.com/avivsinai/agent-message-queue/issues/445)) ([bf3eeda](https://github.com/avivsinai/agent-message-queue/commit/bf3eedaaa546c61564aae05dc84bbb1ee8a5b484))

## [0.52.5](https://github.com/avivsinai/agent-message-queue/compare/v0.52.4...v0.52.5) (2026-08-05)


### Bug Fixes

* **wake:** guard readiness publication on the capability path ([#420](https://github.com/avivsinai/agent-message-queue/issues/420)) ([8295405](https://github.com/avivsinai/agent-message-queue/commit/8295405751a64b2677add01aa77607b3f43e1234))
* **wake:** retry the retained-inbox kqueue wait on EINTR ([#422](https://github.com/avivsinai/agent-message-queue/issues/422)) ([c32199f](https://github.com/avivsinai/agent-message-queue/commit/c32199f59c33851d25c742e9a466ec0941577c62)), closes [#421](https://github.com/avivsinai/agent-message-queue/issues/421)

## [0.52.4](https://github.com/avivsinai/agent-message-queue/compare/v0.52.3...v0.52.4) (2026-08-05)


### Bug Fixes

* **doctor:** report darwin wakes running deleted images ([#438](https://github.com/avivsinai/agent-message-queue/issues/438)) ([#439](https://github.com/avivsinai/agent-message-queue/issues/439)) ([6fdab50](https://github.com/avivsinai/agent-message-queue/commit/6fdab5093e88816de530e3540eef5ca668a83a26))

## [0.52.3](https://github.com/avivsinai/agent-message-queue/compare/v0.52.2...v0.52.3) (2026-08-05)


### Bug Fixes

* **upgrade:** guard Homebrew-owned paths by prefix ownership ([#435](https://github.com/avivsinai/agent-message-queue/issues/435)) ([1f5169c](https://github.com/avivsinai/agent-message-queue/commit/1f5169cd34c87eeceb9db89fa54f758e358c4385))

## [0.52.2](https://github.com/avivsinai/agent-message-queue/compare/v0.52.1...v0.52.2) (2026-08-05)


### Bug Fixes

* **doctor:** correlate unread backlog with proven notifier absence ([#430](https://github.com/avivsinai/agent-message-queue/issues/430)) ([085ddeb](https://github.com/avivsinai/agent-message-queue/commit/085ddebed0457f0911ca71d8b0af2d5332230ed3))
* **wake:** bound doorbell reminders per undrained cohort ([#428](https://github.com/avivsinai/agent-message-queue/issues/428)) ([2cefc16](https://github.com/avivsinai/agent-message-queue/commit/2cefc1662dd46364465f52dc354bb5072c93bed8))

## [0.52.1](https://github.com/avivsinai/agent-message-queue/compare/v0.52.0...v0.52.1) (2026-08-04)


### Bug Fixes

* **coop:** bootstrap AMQ queue at Git worktree top when unconfigured ([#429](https://github.com/avivsinai/agent-message-queue/issues/429)) ([2f3b2a5](https://github.com/avivsinai/agent-message-queue/commit/2f3b2a5d7821f3b0f5953dbc5268d946627a2658))

## [0.52.0](https://github.com/avivsinai/agent-message-queue/compare/v0.51.1...v0.52.0) (2026-08-02)


### Features

* **wake:** add inert .wake.state document primitives ([#415](https://github.com/avivsinai/agent-message-queue/issues/415)) ([fbc574a](https://github.com/avivsinai/agent-message-queue/commit/fbc574a8d2b26b2526dfae5d9c5c87408007ac39))
* **wake:** decompose resume authority and add reload endpoint ([#405](https://github.com/avivsinai/agent-message-queue/issues/405)) ([e4b6c17](https://github.com/avivsinai/agent-message-queue/commit/e4b6c1742b4c587abf9c00d2eb3b3d4cf1c346ce))
* **wake:** dual-write state from legacy mutations ([#416](https://github.com/avivsinai/agent-message-queue/issues/416)) ([1d16b1e](https://github.com/avivsinai/agent-message-queue/commit/1d16b1e82c34451997ab100ad84d6d0e883c706a))
* **wake:** prefer state document on validated dual-read ([#417](https://github.com/avivsinai/agent-message-queue/issues/417)) ([acd9e35](https://github.com/avivsinai/agent-message-queue/commit/acd9e35511f0d5f13c9ed68349929bfcf488cecf))
* **wake:** route session guards through decision table ([#412](https://github.com/avivsinai/agent-message-queue/issues/412)) ([6619399](https://github.com/avivsinai/agent-message-queue/commit/661939938923e7be8d807b02ea183bbac775bdf1))

The new `.wake.state` document ships in three P2a compatibility stages: inert
storage primitives, legacy-first shadow writes, and validated dual-read that
silently falls back to the legacy files whenever the document does not match
the live legacy publication. Legacy files remain authoritative through P2a;
these stages do not activate self-resume, change the legacy lock ABI, or add
migration machinery. The Linux reload transport is an unadvertised,
refusal-only seam in this release; it adds no reload command, candidate custody,
self-exec, or ready capability. Session routing now shares one explicit guard
table through the `internal/sessionguard` package; the extraction preserves the
existing routing outcomes and command-line contract.

### Documentation

* **wake:** specify the authoritative `.wake.state` lifecycle, commit protocol, invariants, and crash matrix ([#407](https://github.com/avivsinai/agent-message-queue/issues/407), [#414](https://github.com/avivsinai/agent-message-queue/issues/414)). These documents define forward contracts; they do not by themselves activate new runtime behavior.


### Tests

* **wake:** add a crash-contract regression net across publication boundaries ([#409](https://github.com/avivsinai/agent-message-queue/issues/409)). This is test-only coverage of the existing fail-closed publication contract.
* **wake:** make detached-child cleanup unconditional, including `setsid` process groups. This is test-harness hygiene; production behavior is unchanged.
* **wake:** pin schema-1 check output byte-for-byte and protect the P0 legacy lock keys, diagnostic fields and enums, exit status, and read-only behavior ([#411](https://github.com/avivsinai/agent-message-queue/issues/411)). This golden suite protects the existing ABI; it does not introduce an ABI change.

## [0.51.1](https://github.com/avivsinai/agent-message-queue/compare/v0.51.0...v0.51.1) (2026-08-01)


### Bug Fixes

* **wake:** retry torn lock reads during acquisition ([#403](https://github.com/avivsinai/agent-message-queue/issues/403)) ([48901d1](https://github.com/avivsinai/agent-message-queue/commit/48901d1e58aacf2d60c1d6c759077da353065714))

## [0.51.0](https://github.com/avivsinai/agent-message-queue/compare/v0.50.1...v0.51.0) (2026-08-01)


### Features

* **wake:** add self-resume protocol foundation ([#389](https://github.com/avivsinai/agent-message-queue/issues/389)) ([595241c](https://github.com/avivsinai/agent-message-queue/commit/595241c9447c786f8161551e968d41df96a12640))
* **wake:** add versioned check schema ([#391](https://github.com/avivsinai/agent-message-queue/issues/391)) ([3024aec](https://github.com/avivsinai/agent-message-queue/commit/3024aec6c7ae48bbf78859ede14da248a5b471e6))
* **wake:** classify reload advertisement ([#392](https://github.com/avivsinai/agent-message-queue/issues/392)) ([6f3b3d4](https://github.com/avivsinai/agent-message-queue/commit/6f3b3d40a4fecbb288b61cc67717beb532852d54))

Schema v2 had not shipped before this release, so its `reload` field was
additive to a pre-release contract; schema 1 and default JSON bytes remain
unchanged. `reload: advertised` is structural metadata only: it means a live,
identity-confirmed wake has a structurally valid resume advertisement, not that
the wake is reloadable. No wake started by this release advertises it yet:
Darwin requires launch-bound image evidence (Wave 3), and Linux requires the
Wave 2 control endpoint. The field ships as forward schema only. This release
adds no reload command, transport, quiescence claim, or execution permission.
It also does not ship a general agent-safe wake restart; that remains tracked
in [#356](https://github.com/avivsinai/agent-message-queue/issues/356).


### Bug Fixes

* **routing:** honor verified sessionless root pin ([#397](https://github.com/avivsinai/agent-message-queue/issues/397)) ([9331546](https://github.com/avivsinai/agent-message-queue/commit/9331546896600929411f82cd2e486cb5839854e7)), closes [#350](https://github.com/avivsinai/agent-message-queue/issues/350)
* **wake:** bind resume advertisements to trusted agent ([#400](https://github.com/avivsinai/agent-message-queue/issues/400)) ([1578501](https://github.com/avivsinai/agent-message-queue/commit/1578501f43a5698c33c670fed0dc7c78fef268f8))
* **wake:** bind resume claims to exact evidence ([#399](https://github.com/avivsinai/agent-message-queue/issues/399)) ([3da2f32](https://github.com/avivsinai/agent-message-queue/commit/3da2f32d3c5fe1d65a4a9233470f102024fea97d))
* **wake:** corroborate Darwin process image ([#395](https://github.com/avivsinai/agent-message-queue/issues/395)) ([3dd1987](https://github.com/avivsinai/agent-message-queue/commit/3dd1987eaa76a8d008b5f13a35b34c4e7b1172d1))
* **wake:** preserve attention retry decay ([#393](https://github.com/avivsinai/agent-message-queue/issues/393)) ([f8a8d81](https://github.com/avivsinai/agent-message-queue/commit/f8a8d81b83293b1945c6b6377cb650cd2722868c))

[#377](https://github.com/avivsinai/agent-message-queue/issues/377) is fully
closed: generation-exclusivity fencing landed in
[#383](https://github.com/avivsinai/agent-message-queue/pull/383) and
[#385](https://github.com/avivsinai/agent-message-queue/pull/385); cadence
accumulation and current-cohort rendering at the retained retry deadline landed
in [#393](https://github.com/avivsinai/agent-message-queue/pull/393). For the
#393 cadence change, durable inbox completion and terminal-input delivery
semantics are unchanged.


### Tests

* **coop:** pin auto-init fixture isolation ([#396](https://github.com/avivsinai/agent-message-queue/pull/396)) ([2c388d5](https://github.com/avivsinai/agent-message-queue/commit/2c388d5c6aabeab416e8c55750410b1bea1049c3)), closes [#376](https://github.com/avivsinai/agent-message-queue/issues/376). This is hostile-context regression coverage for containment already fixed by [#346](https://github.com/avivsinai/agent-message-queue/pull/346), not a new production routing change.
* **send:** make strict replacement race deterministic ([#394](https://github.com/avivsinai/agent-message-queue/pull/394)) ([6cba0a1](https://github.com/avivsinai/agent-message-queue/commit/6cba0a1b013b3d8f79a84f146deff7c8005cfe74)), closes [#381](https://github.com/avivsinai/agent-message-queue/issues/381). This replaces a FIFO/timeout race in the test harness with a deterministic seam; the production send path is unchanged.

## [0.50.1](https://github.com/avivsinai/agent-message-queue/compare/v0.50.0...v0.50.1) (2026-07-31)


### Bug Fixes

* **send:** refuse routed physical self-delivery ([#386](https://github.com/avivsinai/agent-message-queue/issues/386)) ([9454eb1](https://github.com/avivsinai/agent-message-queue/commit/9454eb1a094264c2552ea1981687abee51a4c53a))
* **wake:** fence cohort attention by generation ([#385](https://github.com/avivsinai/agent-message-queue/issues/385)) ([47a8b73](https://github.com/avivsinai/agent-message-queue/commit/47a8b731457ecad6ca259dad80f699c21e1bf913))
* **wake:** harden check evidence and restart advice ([#388](https://github.com/avivsinai/agent-message-queue/issues/388)) ([2f20e94](https://github.com/avivsinai/agent-message-queue/commit/2f20e94b17bd5ed3b2dafe1debf9dd673e8df1df))
* **wake:** reject incoherent repair observations ([#387](https://github.com/avivsinai/agent-message-queue/issues/387)) ([744c681](https://github.com/avivsinai/agent-message-queue/commit/744c681b676319c142a08e41280b6cad5b08d23e))
* **wake:** silence superseded attention ([#383](https://github.com/avivsinai/agent-message-queue/issues/383)) ([2e9883c](https://github.com/avivsinai/agent-message-queue/commit/2e9883c30d9438c34ed62d76bcccc197ecfffc9e))

#383 and #385 close the generation-exclusivity portion of known issue #377: a superseded wake generation can no longer emit peer or operator attention, and inconclusive evidence never silences a healthy wake.
The cadence-accumulation portion of #377 (attention re-announce ladder while an agent is not draining) remains open.

## [0.50.0](https://github.com/avivsinai/agent-message-queue/compare/v0.49.13...v0.50.0) (2026-07-31)


### Features

* **wake:** expose a read-only restart capability probe ([#379](https://github.com/avivsinai/agent-message-queue/issues/379)) ([2d1ef6b](https://github.com/avivsinai/agent-message-queue/commit/2d1ef6b5bb5a57d8196ba12414cd64f5f6a4f23e))

`amq wake check` reports whether a wake can be safely restarted as
`agent_safe`, `operator_only`, or `unavailable`, with an exact next action. It
diagnoses restart capability; it does not restart a wake. Existing wakes remain
`operator_only`: they cannot be replaced in place and still require their
owning terminal or supervisor. Agent-safe restart remains tracked in
[#356](https://github.com/avivsinai/agent-message-queue/issues/356).


### Bug Fixes

* **send:** refuse ambiguous self-delivery ([#378](https://github.com/avivsinai/agent-message-queue/issues/378)) ([9f5e1fc](https://github.com/avivsinai/agent-message-queue/commit/9f5e1fc77970411df6194559b88065cda97e1b28)), closes [#357](https://github.com/avivsinai/agent-message-queue/issues/357)

### Known Issues

* **wake attention exclusivity ([#377](https://github.com/avivsinai/agent-message-queue/issues/377)):**
  terminal-input delivery remains count-independent and bounded, and controlled
  testing confirmed that pending messages remain intact across attention
  re-announcements: no message is dropped, discarded, or suppressed. The
  separate attention channel still re-announces on its 30-second-to-15-minute
  cadence and pulls its deadline forward on new arrivals, so repeated notices
  can accumulate while an agent is not draining. Independently, a superseded
  wake generation is correctly barred from terminal input but can still emit
  attention. These are open notification cadence and generation-exclusivity
  defects, not delivery-loss defects.


### Dependencies

* bump actions/checkout from 7.0.0 to 7.0.1 ([#371](https://github.com/avivsinai/agent-message-queue/issues/371)) ([16edd6c](https://github.com/avivsinai/agent-message-queue/commit/16edd6cca9212b78a55ed20e8ce441b8ede42d5e))

## [0.49.13](https://github.com/avivsinai/agent-message-queue/compare/v0.49.12...v0.49.13) (2026-07-31)


### Bug Fixes

* **wake:** keep admitted delivery self-healing ([#372](https://github.com/avivsinai/agent-message-queue/issues/372)) ([037aa60](https://github.com/avivsinai/agent-message-queue/commit/037aa60ed32ec58cef109e98f54461eff09c6a99))
* **wake:** keep inconclusive ownership retryable ([#372](https://github.com/avivsinai/agent-message-queue/issues/372)) ([037aa60](https://github.com/avivsinai/agent-message-queue/commit/037aa60ed32ec58cef109e98f54461eff09c6a99))
* **wake:** preserve live renamed wake locks ([#372](https://github.com/avivsinai/agent-message-queue/issues/372)) ([037aa60](https://github.com/avivsinai/agent-message-queue/commit/037aa60ed32ec58cef109e98f54461eff09c6a99))

## [0.49.12](https://github.com/avivsinai/agent-message-queue/compare/v0.49.11...v0.49.12) (2026-07-30)


### Bug Fixes

* **ci:** require justification for removed wake tests ([70fe085](https://github.com/avivsinai/agent-message-queue/commit/70fe085872530ea799c4e33bf079a8a3d703dc18))
* **coop:** refuse live wake conflicts immediately ([70fe085](https://github.com/avivsinai/agent-message-queue/commit/70fe085872530ea799c4e33bf079a8a3d703dc18))
* **doctor:** report foreign live wake ownership ([70fe085](https://github.com/avivsinai/agent-message-queue/commit/70fe085872530ea799c4e33bf079a8a3d703dc18))
* **wake:** keep transient delivery failures retryable ([70fe085](https://github.com/avivsinai/agent-message-queue/commit/70fe085872530ea799c4e33bf079a8a3d703dc18))
* **wake:** preserve authoritative recovery ([#369](https://github.com/avivsinai/agent-message-queue/issues/369)) ([0acad72](https://github.com/avivsinai/agent-message-queue/commit/0acad72bb8fd4ee9a44ad94163b5070d38ae7dac))
* **wake:** restore rate-limited attention retries ([#366](https://github.com/avivsinai/agent-message-queue/issues/366)) ([637f11e](https://github.com/avivsinai/agent-message-queue/commit/637f11e893f474ce9aaf55d5903649ea3d5ce2c3))

## [0.49.11](https://github.com/avivsinai/agent-message-queue/compare/v0.49.10...v0.49.11) (2026-07-30)


### Bug Fixes

* **wake:** preserve retry delivery guarantees ([27923be](https://github.com/avivsinai/agent-message-queue/commit/27923be925733b4e7d57fdc82a2ca57b1a716fc5))
* **wake:** suppress repeated retry attention ([61149dc](https://github.com/avivsinai/agent-message-queue/commit/61149dc5ba4d78b598dacd617d9862b1ad86b61a))
* **wake:** surface owner lifecycle failures ([#358](https://github.com/avivsinai/agent-message-queue/issues/358)) ([5c8a971](https://github.com/avivsinai/agent-message-queue/commit/5c8a971f15ca9fb44784cdac1360ced0c5785269))

## [0.49.10](https://github.com/avivsinai/agent-message-queue/compare/v0.49.9...v0.49.10) (2026-07-29)


### Bug Fixes

* **cli:** reject flag-shaped handles while preserving legacy read-only inspection ([1be7cd8](https://github.com/avivsinai/agent-message-queue/commit/1be7cd80b4b3df8ce9108e488429eb0abb67d961))
* **upgrade:** report stale wakes across the active AMQ base tree after upgrade ([1be7cd8](https://github.com/avivsinai/agent-message-queue/commit/1be7cd80b4b3df8ce9108e488429eb0abb67d961))
* **wake:** preserve usable legacy raw wakes during orphan checks ([1be7cd8](https://github.com/avivsinai/agent-message-queue/commit/1be7cd80b4b3df8ce9108e488429eb0abb67d961))
* **wake:** re-announce pending doorbells without stacking input turns ([1be7cd8](https://github.com/avivsinai/agent-message-queue/commit/1be7cd80b4b3df8ce9108e488429eb0abb67d961))

## [0.49.9](https://github.com/avivsinai/agent-message-queue/compare/v0.49.8...v0.49.9) (2026-07-28)


### Bug Fixes

* **wake:** coalesce pending co-op doorbells ([#348](https://github.com/avivsinai/agent-message-queue/issues/348)) ([1ed2b3b](https://github.com/avivsinai/agent-message-queue/commit/1ed2b3b26e194dccfd5a6ef5f40f2a631724298a))

## [0.49.8](https://github.com/avivsinai/agent-message-queue/compare/v0.49.7...v0.49.8) (2026-07-28)


### Bug Fixes

* **routing:** unify root authority and fail closed on ambiguous Git contexts ([5bf4247](https://github.com/avivsinai/agent-message-queue/commit/5bf4247ae2d19a25a393d02116f62bdad8b8e39c))
* **dlq:** serialize retries and preserve terminal audit state ([5bf4247](https://github.com/avivsinai/agent-message-queue/commit/5bf4247ae2d19a25a393d02116f62bdad8b8e39c))
* **federation:** refuse symlinked source roots ([5bf4247](https://github.com/avivsinai/agent-message-queue/commit/5bf4247ae2d19a25a393d02116f62bdad8b8e39c))
* **watch:** refuse replaced mailbox roots ([5bf4247](https://github.com/avivsinai/agent-message-queue/commit/5bf4247ae2d19a25a393d02116f62bdad8b8e39c))

## [0.49.7](https://github.com/avivsinai/agent-message-queue/compare/v0.49.6...v0.49.7) (2026-07-27)


### Bug Fixes

* **doctor:** expose structured base backlog hints ([#341](https://github.com/avivsinai/agent-message-queue/issues/341)) ([6484837](https://github.com/avivsinai/agent-message-queue/commit/648483747811d4714761bbb9a44729b39f8d72d0))
* **doctor:** warn on stale wake binaries ([#342](https://github.com/avivsinai/agent-message-queue/issues/342)) ([0a9c2d6](https://github.com/avivsinai/agent-message-queue/commit/0a9c2d67c71d08739a52fb5fc943ccc9ed4593eb))
* **release:** reconcile published labels idempotently ([#344](https://github.com/avivsinai/agent-message-queue/issues/344)) ([5c0c4e7](https://github.com/avivsinai/agent-message-queue/commit/5c0c4e702ead5dfe37be63f471f1722ccfb7d0b2))
* **send:** complete destination mailbox before delivery ([#339](https://github.com/avivsinai/agent-message-queue/issues/339)) ([f7be8b7](https://github.com/avivsinai/agent-message-queue/commit/f7be8b7191ab61c3dbed83107e29b032dca9fc73))
* **send:** make mailbox recovery self-healing ([fab4c76](https://github.com/avivsinai/agent-message-queue/commit/fab4c76af2734610c157fe8996a6c7d8ed037000))
* **upgrade:** refuse Homebrew-managed binaries ([#340](https://github.com/avivsinai/agent-message-queue/issues/340)) ([d1b04db](https://github.com/avivsinai/agent-message-queue/commit/d1b04db8a01d15fbb32bf29e8f6fa75fd5821be1))

## [0.49.6](https://github.com/avivsinai/agent-message-queue/compare/v0.49.5...v0.49.6) (2026-07-27)


### Bug Fixes

* **coop:** provision the full roster where `coop exec` reads while preserving base compatibility mailboxes ([#332](https://github.com/avivsinai/agent-message-queue/issues/332)) ([6236c7a](https://github.com/avivsinai/agent-message-queue/commit/6236c7ae1a87f6ef9507df91f3636049d6482059))
* **coop:** refuse session symlinks instead of redirecting mailbox writes through them ([#332](https://github.com/avivsinai/agent-message-queue/issues/332)) ([6236c7a](https://github.com/avivsinai/agent-message-queue/commit/6236c7ae1a87f6ef9507df91f3636049d6482059))
* **coop:** report base-root backlog from session consumer paths and `doctor --ops` ([#332](https://github.com/avivsinai/agent-message-queue/issues/332)) ([6236c7a](https://github.com/avivsinai/agent-message-queue/commit/6236c7ae1a87f6ef9507df91f3636049d6482059))

## [0.49.5](https://github.com/avivsinai/agent-message-queue/compare/v0.49.4...v0.49.5) (2026-07-27)


### Bug Fixes

* **coop:** preserve configured agents on init reruns ([#330](https://github.com/avivsinai/agent-message-queue/issues/330)) ([3424c7b](https://github.com/avivsinai/agent-message-queue/commit/3424c7b5f8262443345c8faaecc335e62f8d518d))

## [0.49.4](https://github.com/avivsinai/agent-message-queue/compare/v0.49.3...v0.49.4) (2026-07-27)


### Bug Fixes

* **wake:** recover partial takeovers with duplicate warning ([#328](https://github.com/avivsinai/agent-message-queue/issues/328)) ([7684772](https://github.com/avivsinai/agent-message-queue/commit/76847726b82e9668bf5a487b5d8f9e33ba1f5c7e))

## [0.49.3](https://github.com/avivsinai/agent-message-queue/compare/v0.49.2...v0.49.3) (2026-07-27)


### Bug Fixes

* **wake:** resolve Darwin wake TTYs so same-terminal replacement works ([#326](https://github.com/avivsinai/agent-message-queue/issues/326)) ([996e329](https://github.com/avivsinai/agent-message-queue/commit/996e3297bba7b2e43f0be1da244cff080a1bade6))
* **wake:** recover live raw coop takeovers ([#326](https://github.com/avivsinai/agent-message-queue/issues/326)) ([996e329](https://github.com/avivsinai/agent-message-queue/commit/996e3297bba7b2e43f0be1da244cff080a1bade6))
* **wake:** enable guarded live-wake recovery on Linux ([#326](https://github.com/avivsinai/agent-message-queue/issues/326)) ([996e329](https://github.com/avivsinai/agent-message-queue/commit/996e3297bba7b2e43f0be1da244cff080a1bade6))

## [0.49.2](https://github.com/avivsinai/agent-message-queue/compare/v0.49.1...v0.49.2) (2026-07-27)


### Bug Fixes

* deliver wake attention in Ghostty ([#324](https://github.com/avivsinai/agent-message-queue/issues/324)) ([82c28ba](https://github.com/avivsinai/agent-message-queue/commit/82c28ba83d56cc39bd3b6893b262db389e769cb4))
* safely reclaim blocking coop wake locks ([#322](https://github.com/avivsinai/agent-message-queue/issues/322)) ([e5f98c6](https://github.com/avivsinai/agent-message-queue/commit/e5f98c6631e660b5ea68dcb77dda5b4e33e2cf00))

## [0.49.1](https://github.com/avivsinai/agent-message-queue/compare/v0.49.0...v0.49.1) (2026-07-27)


### Bug Fixes

* **hooks:** guard Stop blocks by message content ([#318](https://github.com/avivsinai/agent-message-queue/issues/318)) ([bc18f97](https://github.com/avivsinai/agent-message-queue/commit/bc18f97113d3df2091cd9c7aa2d6d2d915266b6b))
* **hooks:** recover incomplete session context ([#319](https://github.com/avivsinai/agent-message-queue/issues/319)) ([2b02729](https://github.com/avivsinai/agent-message-queue/commit/2b027298a21b21609f6e8ec3d8c4d3b810913b20))
* **release:** reconcile labels after attestation failure ([#308](https://github.com/avivsinai/agent-message-queue/issues/308)) ([3fc28a7](https://github.com/avivsinai/agent-message-queue/commit/3fc28a7ed2553d1b0c4ba5217c6cbecd618e36db))
* **wake:** correct interrupt and repair guidance ([#313](https://github.com/avivsinai/agent-message-queue/issues/313)) ([ce3a6f8](https://github.com/avivsinai/agent-message-queue/commit/ce3a6f8249f00a29432956ada05bf779f4a25da1))
* **wake:** defer injection after max hold ([#314](https://github.com/avivsinai/agent-message-queue/issues/314)) ([d3f92db](https://github.com/avivsinai/agent-message-queue/commit/d3f92db49e720c31c4ce2ff5df828a50e8ec66c1))
* **wake:** make periodic capability checks honest ([#320](https://github.com/avivsinai/agent-message-queue/issues/320)) ([4b9bc8d](https://github.com/avivsinai/agent-message-queue/commit/4b9bc8d1caf7d73b2872242485d3aa60fcfcd4ee))
* **wake:** require consecutive quiet samples ([#315](https://github.com/avivsinai/agent-message-queue/issues/315)) ([a68d9b2](https://github.com/avivsinai/agent-message-queue/commit/a68d9b241c8909eccf39afd8a0e8f523641e873a))
* **wake:** retry held notifications after pgrp handoff ([#310](https://github.com/avivsinai/agent-message-queue/issues/310)) ([f015d8b](https://github.com/avivsinai/agent-message-queue/commit/f015d8bfc9c5cf56d3aefbb3d4ff7c6700bcc9a2))
* **wake:** use fixed standalone doorbell ([#317](https://github.com/avivsinai/agent-message-queue/issues/317)) ([3641d5e](https://github.com/avivsinai/agent-message-queue/commit/3641d5e82cf852ed1861982bbf3d59c86f08abda))

## [0.49.0](https://github.com/avivsinai/agent-message-queue/compare/v0.48.0...v0.49.0) (2026-07-26)


### Features

* **trace:** join message lifecycle evidence ([#307](https://github.com/avivsinai/agent-message-queue/issues/307)) ([e9cbd34](https://github.com/avivsinai/agent-message-queue/commit/e9cbd348b4af0cf03fc69a34195e4020fd9dea89))


### Bug Fixes

* **doctor:** add discovered mailbox remedies ([#305](https://github.com/avivsinai/agent-message-queue/issues/305)) ([4c4057d](https://github.com/avivsinai/agent-message-queue/commit/4c4057db305fbc6deb8660001947514e6febfc9f))

## [0.48.0](https://github.com/avivsinai/agent-message-queue/compare/v0.47.3...v0.48.0) (2026-07-26)


### Features

* **wake:** probe TIOCSTI capability and degrade to a non-input notifier ([#302](https://github.com/avivsinai/agent-message-queue/issues/302)) ([ac89fa5](https://github.com/avivsinai/agent-message-queue/commit/ac89fa566985eb5b5d5817ddce8b172fb68d1934))


### Bug Fixes

* **doctor:** report and repair malformed mailbox layout ([#304](https://github.com/avivsinai/agent-message-queue/issues/304)) ([9a9ec0f](https://github.com/avivsinai/agent-message-queue/commit/9a9ec0facaf15e1cae98b63d80da52c92f8c623b))

## [0.47.3](https://github.com/avivsinai/agent-message-queue/compare/v0.47.2...v0.47.3) (2026-07-26)


### Bug Fixes

* **cli:** reject stray leaf arguments ([#293](https://github.com/avivsinai/agent-message-queue/issues/293)) ([9bd6c1a](https://github.com/avivsinai/agent-message-queue/commit/9bd6c1a95cab0f1633aeaf2acfedde662b12ab24))
* **install:** verify release checksums fail closed ([#291](https://github.com/avivsinai/agent-message-queue/issues/291)) ([321881c](https://github.com/avivsinai/agent-message-queue/commit/321881c81cf1c1c540f4556737369dd1b8304c56))
* **wake:** admit wake when an unverified generic lock cannot be proven ([#301](https://github.com/avivsinai/agent-message-queue/issues/301)) ([d177948](https://github.com/avivsinai/agent-message-queue/commit/d1779485f982c73f257ce6db2372e242ef909fcb))
* **wake:** close wake terminal descriptors on exec ([#300](https://github.com/avivsinai/agent-message-queue/issues/300)) ([e4db6ea](https://github.com/avivsinai/agent-message-queue/commit/e4db6ea11669aa9174e0e6398c56926fb3ed957e))
* **wake:** make interrupt injection opt-in ([#299](https://github.com/avivsinai/agent-message-queue/issues/299)) ([bf11814](https://github.com/avivsinai/agent-message-queue/commit/bf11814b004d57ed619a6a6df6134cfe0d121a3a))

## [0.47.2](https://github.com/avivsinai/agent-message-queue/compare/v0.47.1...v0.47.2) (2026-07-26)


### Bug Fixes

* **wake:** keep terminal authority stable during tty activity ([0a62b1a](https://github.com/avivsinai/agent-message-queue/commit/0a62b1a5126e5dfa51bd80adbc212920d4295c0a))

## [0.47.1](https://github.com/avivsinai/agent-message-queue/compare/v0.47.0...v0.47.1) (2026-07-24)


### Bug Fixes

* **wake:** harden coop owner lifecycle ([#288](https://github.com/avivsinai/agent-message-queue/issues/288)) ([9f0d7b8](https://github.com/avivsinai/agent-message-queue/commit/9f0d7b88126f5067ac065050cbc22836c34420db))
* **wake:** make repair continuity fail closed ([327f474](https://github.com/avivsinai/agent-message-queue/commit/327f47469ef7008c7d3c0d8d133d2a3778d9ae86))


### Dependencies

* bump actions/attest from 4.1.1 to 4.2.0 ([#274](https://github.com/avivsinai/agent-message-queue/issues/274)) ([d1332d4](https://github.com/avivsinai/agent-message-queue/commit/d1332d41e97458c96f9b6842761c4e6c980bd264))
* bump actions/setup-go from 6.5.0 to 7.0.0 ([#275](https://github.com/avivsinai/agent-message-queue/issues/275)) ([7309635](https://github.com/avivsinai/agent-message-queue/commit/7309635cf802dd79b129a7593149990563c01685))
* bump actions/setup-node from 6.4.0 to 7.0.0 ([#276](https://github.com/avivsinai/agent-message-queue/issues/276)) ([29bd9a6](https://github.com/avivsinai/agent-message-queue/commit/29bd9a624dce1ea80a29ad5f0beae1329fa6a31e))

## [0.47.0](https://github.com/avivsinai/agent-message-queue/compare/v0.46.0...v0.47.0) (2026-07-24)


### Features

* **wake:** add identity-safe owner-bound lifecycle ([#277](https://github.com/avivsinai/agent-message-queue/issues/277)) ([84ea950](https://github.com/avivsinai/agent-message-queue/commit/84ea9508faa673d70870bd004b73e2bb19c5c5b0))


### Bug Fixes

* **ci:** skip release-please on release-commit pushes to avoid tag race ([#271](https://github.com/avivsinai/agent-message-queue/issues/271)) ([e37067a](https://github.com/avivsinai/agent-message-queue/commit/e37067a91b4447c3ed99bf647b71e7ec9dbf3824))

## [0.46.0](https://github.com/avivsinai/agent-message-queue/compare/v0.45.0...v0.46.0) (2026-07-22)


### Features

* **wake:** add --baseline-existing so a starting agent isn't injected with backlog ([#267](https://github.com/avivsinai/agent-message-queue/issues/267)) ([ee0fc46](https://github.com/avivsinai/agent-message-queue/commit/ee0fc460234f8c71a94fd2213a9d4bb2d55ece94))

## [0.45.0](https://github.com/avivsinai/agent-message-queue/compare/v0.44.0...v0.45.0) (2026-07-21)


### Features

* **fsq:** fd-confined message delivery via os.Root capability ([#257](https://github.com/avivsinai/agent-message-queue/issues/257)) ([059a076](https://github.com/avivsinai/agent-message-queue/commit/059a0763b66b8b9f848a10fc46352376fa43ecf0))
* **wake:** add amq wake retire for identity-safe inject-via stop ([#256](https://github.com/avivsinai/agent-message-queue/issues/256)) ([a57dba9](https://github.com/avivsinai/agent-message-queue/commit/a57dba96f44ef05794275ea5abfc28f5d1c78ebc))
* **wake:** Darwin cooperative unix-socket shutdown for inject-via wakes ([#254](https://github.com/avivsinai/agent-message-queue/issues/254)) ([6d66a20](https://github.com/avivsinai/agent-message-queue/commit/6d66a205236ef205e6a8dc76117ee736fc021ba3))


### Bug Fixes

* **delivery:** report indeterminate-durability commits + capability-relative reads ([#261](https://github.com/avivsinai/agent-message-queue/issues/261)) ([63cc42f](https://github.com/avivsinai/agent-message-queue/commit/63cc42f2652c709aae33c75c286d82407fd74b6c))
* **wake:** close cooperative-stop duplicate-injection window and harden Darwin lifecycle ([#260](https://github.com/avivsinai/agent-message-queue/issues/260)) ([8d6e6f4](https://github.com/avivsinai/agent-message-queue/commit/8d6e6f46a646228f18e4507bf3db95aaad6c34e1))

## [0.44.0](https://github.com/avivsinai/agent-message-queue/compare/v0.43.1...v0.44.0) (2026-07-21)


### Features

* **cli:** add Grok coop support ([#240](https://github.com/avivsinai/agent-message-queue/issues/240)) ([fb024f2](https://github.com/avivsinai/agent-message-queue/commit/fb024f2c5aec3fe6dad4d53779125af21d1faa59)), closes [#218](https://github.com/avivsinai/agent-message-queue/issues/218)
* **cli:** tri-state tree identity, physical pins, and authority de-laundering ([#252](https://github.com/avivsinai/agent-message-queue/issues/252)) ([6ef0e6d](https://github.com/avivsinai/agent-message-queue/commit/6ef0e6df1177ccf6009a04bf9359f723ea9ea0d3))
* **wake:** Linux pidfd wake termination capability ([#251](https://github.com/avivsinai/agent-message-queue/issues/251)) ([e528d76](https://github.com/avivsinai/agent-message-queue/commit/e528d766e375c084df832fa1164209633f5c7e3f))
* **wake:** refuse live raw Darwin wake orphans instead of bare-PID kill ([#253](https://github.com/avivsinai/agent-message-queue/issues/253)) ([77ac2e5](https://github.com/avivsinai/agent-message-queue/commit/77ac2e51d995f9dfef998bdf969431fbd5858c1d))
* **wake:** serialize wake lifecycle under permanent guard with generation-bound readiness ([#250](https://github.com/avivsinai/agent-message-queue/issues/250)) ([ca4cdb4](https://github.com/avivsinai/agent-message-queue/commit/ca4cdb420dac98c499563061f2bdffdd50739acc))


### Bug Fixes

* **cli:** advertise claude-code swarm type in help and sync CLI docs ([#243](https://github.com/avivsinai/agent-message-queue/issues/243)) ([927d825](https://github.com/avivsinai/agent-message-queue/commit/927d825094eb0aeb73b5b35532f26abc557fac8d))
* **cli:** clarify Grok skill discovery docs and coop next-steps hint ([#242](https://github.com/avivsinai/agent-message-queue/issues/242)) ([07eee13](https://github.com/avivsinai/agent-message-queue/commit/07eee133cb7e290c673180586fdde7a4a7d90818))
* **wake:** adopt tri-state identity classification for wake locks ([#247](https://github.com/avivsinai/agent-message-queue/issues/247)) ([c28bb2a](https://github.com/avivsinai/agent-message-queue/commit/c28bb2ab220f86250b79d7a531907cc05ffb74ad))
* **wake:** enforce exact wake-mode compatibility ([#246](https://github.com/avivsinai/agent-message-queue/issues/246)) ([8153b8a](https://github.com/avivsinai/agent-message-queue/commit/8153b8af39eccfedd36994058bd1ba2523bceb5f))
* **wake:** require exact injector identity for inject-via wake reuse ([#248](https://github.com/avivsinai/agent-message-queue/issues/248)) ([4871088](https://github.com/avivsinai/agent-message-queue/commit/4871088a944d7f2f2cac0ee13b8c3838b1ef01a5))


### Dependencies

* bump golang.org/x/term from 0.44.0 to 0.45.0 ([#239](https://github.com/avivsinai/agent-message-queue/issues/239)) ([9ff9c1d](https://github.com/avivsinai/agent-message-queue/commit/9ff9c1daa4e84109399102fb81d320c26b87e9fc))

## [0.43.1](https://github.com/avivsinai/agent-message-queue/compare/v0.43.0...v0.43.1) (2026-07-14)


### Bug Fixes

* **cli:** close cross-tree guard bypass in root classification ([#231](https://github.com/avivsinai/agent-message-queue/issues/231)) ([774f568](https://github.com/avivsinai/agent-message-queue/commit/774f568efc656a3c57ed7c4c48ee5022f4e415d9))
* **wake:** harden Darwin boot identity and zombie detection ([#236](https://github.com/avivsinai/agent-message-queue/issues/236)) ([ae776b8](https://github.com/avivsinai/agent-message-queue/commit/ae776b8210292b92b10658221455176a3c46cf4e))

## [0.43.0](https://github.com/avivsinai/agent-message-queue/compare/v0.42.1...v0.43.0) (2026-07-11)


### Features

* **doctor:** detect mailbox divergence across worktrees ([#228](https://github.com/avivsinai/agent-message-queue/issues/228)) ([db99e78](https://github.com/avivsinai/agent-message-queue/commit/db99e78f7fccd414dccc5199fde8ac3685ae9a58)), closes [#207](https://github.com/avivsinai/agent-message-queue/issues/207)
* **presence:** distinguish notifier liveness from activity ([#230](https://github.com/avivsinai/agent-message-queue/issues/230)) ([a8d9b8f](https://github.com/avivsinai/agent-message-queue/commit/a8d9b8fd5a9cace05252fe5882e2a092cb0d15f8)), closes [#206](https://github.com/avivsinai/agent-message-queue/issues/206)

## [0.42.1](https://github.com/avivsinai/agent-message-queue/compare/v0.42.0...v0.42.1) (2026-07-11)


### Bug Fixes

* **session:** refuse cross-session mailbox access and surface sibling backlogs ([#224](https://github.com/avivsinai/agent-message-queue/issues/224)) ([#225](https://github.com/avivsinai/agent-message-queue/issues/225)) ([57a296e](https://github.com/avivsinai/agent-message-queue/commit/57a296e0ecd77bf8b73a5c065f2a151260ef0a06))

## [0.42.0](https://github.com/avivsinai/agent-message-queue/compare/v0.41.1...v0.42.0) (2026-07-11)


### Features

* **wake:** add zero-input notification mode ([#221](https://github.com/avivsinai/agent-message-queue/issues/221)) ([3d3376f](https://github.com/avivsinai/agent-message-queue/commit/3d3376faa603829580418bec044a79064e36af81))

  `amq wake --inject-mode none` now provides an AMQ-enforced zero-input mode
  for permission-prompt workflows: normal notices go to wake stderr, urgent
  interrupts emit one bell plus the stderr notice instead of Ctrl+C, and the
  mode needs neither TIOCSTI nor a controlling TTY. It fails closed when
  combined with `--inject-via`, `--inject-arg`, or `--inject-cmd`; `coop exec`
  exposes the mode through `--wake-inject-mode` and refuses to satisfy an
  explicit `none` request by reusing a wake whose zero-input mode cannot be
  proven. Documentation now warns that every input-injecting mode can activate
  a focused permission/approval dialog and that input deferral cannot detect
  modal state (closes [#216](https://github.com/avivsinai/agent-message-queue/issues/216)).


### Bug Fixes

* skip changelog gate on Dependabot PR author, not only event actor ([#219](https://github.com/avivsinai/agent-message-queue/issues/219)) ([fc87a86](https://github.com/avivsinai/agent-message-queue/commit/fc87a861e92b9df041cd10158657c424a87139cd))

  CI changelog gate no longer fails Dependabot PRs after a maintainer updates
  the branch: a manual `gh pr update-branch` makes the maintainer the
  synchronize-event actor, so the actor-based skip stopped applying. The gate
  now also skips on the PR author (`pull_request.user.login`), GitHub's
  documented Dependabot-detection pattern, which stays `dependabot[bot]`
  regardless of who updates the branch.

## [0.41.1] - 2026-07-10
### Fixed

- `amq wake` raw injection submits again in fast-reading TUIs (codex-tui, busy
  Claude Code): the v0.41.0 drain-wait completes within microseconds when the
  TUI is actively reading, so the submit CR landed inside the TUI's paste-burst
  window and was inserted as a pasted newline instead of pressing Enter. The
  injector now holds the submit key for a 150ms settle delay after the text
  drains (clearing codex-tui's 120ms Enter-suppress window) and restores the
  second rescue submit on the same spacing (a no-op when the first already
  submitted), skipping the rescue only when the first is provably still queued.
- `amq wake` injects a single LF prelude before the submit CR for codex
  targets: in the reproduced Ghostty wake path with codex-tui's kitty keyboard
  enhancement active, a bare `\r` did not submit at any tested delay; the LF
  routes through codex-tui's Ctrl-J binding, which flushes and clears
  paste-burst state before the CR lands (the prelude newline is trimmed from
  the submitted payload). Raw-mode injection deliberately stays single-byte —
  TIOCSTI delivers one byte per ioctl, so a multi-byte escape sequence (such as
  the kitty CSI-u Enter) can be split by reader scheduling into a lone ESC,
  which a TUI parses as the Escape key and cancels an active turn. Claude Code
  targets keep the plain `\r` submit with no prelude.

### Security

- Bumped the Go toolchain to 1.25.12 to pick up the GO-2026-5856 fix
  (Encrypted Client Hello privacy leak in crypto/tls).


## [0.41.0] - 2026-07-08
### Added

- `amq reply --wait-for <stage> --wait-timeout <duration>` blocks on the
  recipient's delivery receipt, mirroring `amq send --wait-for`, so reply
  delivery can be confirmed instead of assumed.
- `amq who` text output now prints a `Base root:` header naming the tree it is
  reading, so per-root `active`/`stale` presence is not mistaken for global
  liveness when the same session name exists in another root (JSON output is
  unchanged).

### Fixed

- `amq wake` raw TIOCSTI injection now waits for the terminal input queue to
  drain after writing notification text, then injects a single carriage return,
  preventing Ghostty/Claude Code from intermittently receiving text and Enter
  in one paste-shaped stdin chunk (#208).
- `make lint` now uses a checkout-local golangci-lint cache, preventing stale
  analyzer results from deleted git worktrees from leaking into future lint runs
  and failing pre-push checks (#199).
- `amq coop exec` now pins the session root to an absolute path before
  starting wake and exporting `AM_ROOT`/`AM_BASE_ROOT`. A relative root
  (e.g. from the `.agent-mail` default) re-resolved against every future cwd
  of the agent process, silently splitting one session name into
  per-directory mailbox trees across git worktrees — peers on the "same"
  session could not see each other and sends queued where nobody was reading.
- `amq env` shell output (plain and `--export`) now emits absolute
  `AM_ROOT`/`AM_BASE_ROOT` values for the same reason: these exports exist to
  pin a terminal to one mailbox, and a relative export re-splits per cwd
  (JSON output is unchanged).


## [0.40.0] - 2026-07-05
### Added

- `amq coop init --no-gitignore` now leaves `.gitignore` unchanged, for users
  who manage ignore rules globally or manually (#173, closes #172).
- `amq coop exec --no-gitignore` passes the gitignore opt-out through auto-init,
  so `coop exec` can start a session without modifying `.gitignore` (#192,
  closes #179).
- Contributor workflow: pull requests now require a `CHANGELOG.md` Unreleased
  entry unless skipped by Dependabot, release branches, or the `no-changelog`,
  `docs`, or `chore` labels (#182).

### Changed

- `amq wake --help` no longer lists internal readiness-coordination flags;
  managed wake startup flows can still pass them (#189).
- Bump `github.com/coder/websocket` from 1.8.14 to 1.8.15 (#169)
- Bump `actions/checkout` from 6.0.3 to 7.0.0 (#170)

### Fixed

- Hardened message, DLQ-envelope, and receipt parsing by rejecting queue files
  that are themselves symlinks or non-regular files before reading them (#186).
- Inbox and DLQ operations now reject malformed or non-canonical `.md` queue
  filenames at queue boundaries (#185).
- Wake metadata and readiness writes now refuse symlinked destination files and
  install atomically with fsync (#187).
- Wake identity mismatches now stay `unverified` unless AMQ can prove the
  recorded PID is not an `amq wake` process (#187).
- `amq wake --inject-via` now accepts symlinked executables, such as
  Homebrew-installed injectors, by resolving them before validation; validation,
  persistence, and execution all use the resolved physical path (closes #197).
- On Windows, atomic file writes now replace existing files atomically instead
  of temporarily deleting the destination during rename retries (#188).
- Atomic queue-file writes now fail with `io.ErrShortWrite` if the filesystem
  reports success after writing only part of the file (#184).
- Contributor workflow: pre-push smoke tests no longer write synthetic release
  refs into the caller repository when run from a git hook (#196, closes #195).
- Test-only: wake tests now create their sandboxes under the physical
  repository path, so symlink-spelled checkouts no longer fail loop/inject-via
  assertions (#194, closes #193).
- Test-only: made inject-via wake notification tests deterministic by replacing
  shell-redirection capture with a helper process (#183).

### Compatibility

The stricter queue validation above applies to queue files themselves, not to
how their directories are reached. A message, receipt, DLQ envelope, or wake
metadata file that is itself a symlink (or any non-regular file) is now
rejected; queue roots reached through symlinked parent directories — for
example a symlinked checkout or home layout — are unaffected. Files created by
the AMQ CLI are always regular files, so only hand-placed symlinks inside queue
directories are impacted.


## [0.39.0] - 2026-06-30
### Fixed

- `amq drain` and drain-mode `amq monitor` now claim inbox messages before
  parsing them, preventing duplicate consumption under concurrent drains.
- DLQ moves and retries now claim or update queue state before redelivery, and
  reject tampered original filenames before restoring messages.
- Multi-recipient delivery now preserves already committed inbox messages when
  a later recipient fails, and `amq send --project` now rejects multiple
  recipients instead of applying undefined cross-project partial semantics.
- AMQ-owned `--inject-via` wake processes started by `coop exec` or
  `wake repair` now exit when their recorded owner process is gone or no
  longer matches, preventing stale terminal injectors from blocking session
  reopen recovery.


## [0.38.0] - 2026-06-22
### Added

- `amq env --export` now prints eval-safe shell exports for opt-in terminal
  pinning, including `AM_BASE_ROOT` only when the resolved root is a session
  root (#149).

### Fixed

- `amq who`, `amq presence list`, and `amq doctor --ops` now present the
  reserved `user` mailbox as a human operator gate instead of a stale agent
  process (#139).
- The human operator handle `user` is now reserved for configured projects, and
  `amq coop init` seeds `claude,codex,user` by default so strict operator gates
  no longer require custom coop setup (#139).
- Release publishing now detects release commits inside normally merged release
  PRs while ordinary feature merge commits no-op before tag, artifact, or skill
  publishing jobs (#163).


## [0.37.1] - 2026-06-22

### Fixed

- Hardened general stale `.wake.lock` cleanup paths so `amq wake` acquisition
  and `doctor --ops --fix-wake-locks` refuse to remove locks whose live wake PID
  only appears stale because boot-id or process-start metadata was tampered or
  mismatched (#156).
- `amq send --root <path>` now shows the root basename as the session label for
  root-only local sends that are not routed by project, session, or
  from-session flags (#150).

## [0.37.0] - 2026-06-22
### Added

- Document operator-gate conventions in the `amq-cli` and `amq-spec` skills,
  covering structural human handoffs, initialized human handles, and spec
  approval gates (#136).
- Report and optionally fix identity-verified stale `.wake.lock` files from
  `amq doctor --ops`, including roots whose config is missing or corrupt (#151).
- Add `amq wake repair` plus `coop exec --wake-inject-via` support so managed
  launchers can restart a dead external-injector wake for a still-running agent
  session from a digest-bound, private saved target (#154).

### Fixed

- `amq coop exec --require-wake` can reuse an existing usable wake process, while
  still failing closed when the existing wake cannot safely inject (#153).


## [0.36.0] - 2026-06-13
### Changed

- `amq send` now refuses an explicit `--root` that targets a different base tree
  than the caller's active session (`AM_ROOT`/`AM_BASE_ROOT`) when no routing
  dimension (`--project`/`--session`/`--from-session`) is given. A direct `--root`
  is root selection, not federation routing: such a message carried no
  sender-origin metadata, so the recipient could not reply and a naive reply
  looped back into their own tree. Replyable cross-tree messaging must use
  `--project`/`--session`. Bare-root sends with no session env set are
  unaffected (#144).
- `amq send` no longer stamps `reply_to` on ordinary same-session sends; it is
  stamped only for actual cross-session/cross-project routes. The stray
  same-session `reply_to` is what made a direct cross-root send look replyable
  while looping into the replier's own tree (#144).


## [0.35.0] - 2026-06-13
### Added

- `amq send` and `amq reply` accept `--allow-empty` to deliver an intentionally
  blank body (for example when the subject carries the full message) (#143).

### Changed

- `amq send` and `amq reply` now treat `--body -` (and `--body @-` or an omitted
  `--body`) as stdin per the standard CLI convention, and **fail closed** when the
  resolved body is empty or whitespace-only instead of silently delivering a
  blank message. Previously `--body -` shipped a literal hyphen, so a dropped or
  mistyped body could reach the recipient blank with no warning. Pass
  `--allow-empty` to send a blank body deliberately (#143).
- Bumped the Go toolchain directive to 1.25.11 so CI and release checks pick up
  the standard-library fixes for GO-2026-5039 (`net/textproto`) and GO-2026-5037
  (`crypto/x509`) that `govulncheck` now flags.


## [0.34.1] - 2026-05-11
### Added

- `amq coop exec --require-wake` now refuses to launch the agent command unless
  the background wake process starts and confirms it acquired the wake lock,
  giving managed launchers a safe mode for wake health enforcement (#120).

### Changed

- Bumped the Go toolchain directive to 1.25.10 so CI and release checks use the
  standard-library vulnerability fixes required by `govulncheck`.


## [0.34.0] - 2026-04-28
### Added

- `amq wake` now supports an explicit external injection transport via `--inject-via <executable>`, repeatable `--inject-arg <arg>`, and bounded `--inject-timeout` (default `5s`), letting orchestrators and no-controlling-TTY environments receive wake notifications without TIOCSTI. AMQ appends the sanitized notification payload as the final argv element and does not run the command through a shell. `--bell` is honored on the inject-via path, and a one-time fallback warning is emitted before writing to stderr when the external injector fails (#99, closes #98).

### Fixed

- Release tooling preserves CHANGELOG compare links when preparing release PRs (#116).



## [0.33.0] - 2026-04-28
### Added

- `amq env --json` now emits the documented v1 machine-readable contract with `schema_version`, `amq_version`, `base_root`, `in_session`, `root_source`, always-present string fields, and `{}` for unconfigured `peers` (#101).
- Reserved extension metadata namespaces under `<AM_ROOT>/extensions/<layer>/` and `<AM_ROOT>/agents/<handle>/extensions/<layer>/`; `amq doctor --json` now reports passive root extension manifests and malformed extension metadata diagnostics without executing extension code (#102).
- `amq route explain --json` now reports canonical route resolution with routability, structured `argv`, display command, source/delivery roots, project, and session metadata for same-session, cross-session, and cross-project sends (#103).
- `amq send --from-session <source-session>` supports setup-terminal cross-session sends from a base root, writing the sender outbox in the source session and stamping `reply_to` for replies back to that session (#104).

### Fixed

- Explicit `--root`/`--from-root` project lookups no longer fall back to the current working directory's `.amqrc`, and global `~/.amqrc` no longer infers project identity from the home directory basename.
- `amq env --json` now emits `.amqrc` peer paths as resolved absolute paths so consumers do not need to reimplement AMQ's peer path resolution.
- Extension layer names now reject `..` substrings, and `amq doctor --json` only reads passive extension manifests that are regular files below the size cap.


## [0.32.2] - 2026-04-27
### Added

- `amq wake` now has an enabled-by-default, best-effort input-activity deferral gate before non-interrupt TIOCSTI injection. The gate only runs after a wake notification is pending, samples the controlling terminal for unread input and recent reads, and is bounded by `--input-poll-interval`, `--input-quiet-for`, and `--input-max-hold`. This does not prove the foreground app's prompt buffer is empty; a paused in-progress prompt can still be injected into and submitted. Atime sampling uses stdin when it is a TTY (the `/dev/tty` alias inode does not track underlying ttys reads on macOS, and a freshly opened `/dev/tty` fd is not always in the tty's open-file list on Linux); Linux tty atime is maintained at ~8s granularity, so `--input-quiet-for` values shorter than that are advisory.


## [0.32.1] - 2026-04-13
### Fixed

- `scripts/claude-session-start.sh` now rotates oversized `$HOOK_LOG` files opportunistically at hook start, keeping stderr logging bounded without affecting hook output or exit behavior.


## [0.32.0] - 2026-04-13
### Added

- `scripts/claude-session-start.sh` phase 2: SessionStart hook now re-injects coop context (session, project, peers, unread count) as `additionalContext` after `/clear` or context compaction, restoring the awareness Claude Code loses when its context is reset (#84, fixes #71). Composes existing CLI primitives — no new Go surface.
- Smoke test coverage for the SessionStart hook: phase 1 env-file write, phase 2 JSON shape, `/clear` recovery, quoted-root path round-trip, and `/clear` with env-file-only `AM_ME=<non-default>`.

### Fixed

- SessionStart hook is now safe under stock macOS `/bin/bash` 3.2 + `set -u`; replaced empty-array expansion (`"${ROOT_FLAGS[@]}"`) with explicit rooted/non-rooted command branches.
- SessionStart hook correctly round-trips POSIX single-quote-escaped roots (e.g. paths containing `'`); replaced fragile `sed` decoding with isolated `/bin/sh` eval of the matched `export` line.
- SessionStart hook `/clear` recovery now reloads `AM_ME` from the env file symmetrically to `AM_ROOT`, so phase 2 targets the correct handle when only the env file carries identity.


## [0.31.3] - 2026-04-12
### Changed

- Added token-efficiency guidance to the `amq-cli` skill: send file paths instead of inlining large file contents, and run multi-round AMQ review loops in background workers or subagents so intermediate rounds stay out of the main context.


## [0.31.2] - 2026-04-10
### Changed

- Doc sweep to align CLAUDE.md, README.md, skills, and CLI help text with the receipt ledger model — no more stale ack references in agent-facing or user-facing docs.
- Removed the unused `Header.AckRequired` field and `ack_required` JSON tag from the message format. Outgoing messages no longer carry the dead `"ack_required": false` field in their frontmatter.
- Dropped dead `--ack=false` branches from drain test helpers; simplified signatures to match the current drain API.


## [0.31.1] - 2026-04-10
### Changed

- `amq read` now applies the same strict header validator as `drain` and `monitor`, so messages with malformed headers are moved to DLQ and get a `dlq` receipt instead of staying in `inbox/new`.
- Simplified `receipt.WaitFor()` by collapsing the redundant `agent` parameter; callers now pass only the consumer that owns the receipt namespace.


## [0.31.0] - 2026-04-09
### Added

- Added delivery receipts with `drained` and `dlq` stages, plus the new `amq receipts list` and `amq receipts wait` commands for querying receipt history and waiting on receipt arrival.
- Added `amq send --wait-for <stage>` so senders can block for delivery confirmation on single-recipient handoffs.
- Added `receipt.WaitFor()` for targeted receipt polling by message id, consumer, and stage.

### Changed

- Replaced the old ack model with a receipt ledger stored under agent `receipts/` directories.
- Simplified receipt emission to consumer-local writes, with send-side waits reading from the actual delivery root instead of relying on mirrored receipt files.
- Bumped the Go toolchain to 1.25.9.

### Removed

- Removed the `amq ack` command, `--ack` flags, `ack_required` header field, and `acks/` directories from the active protocol and docs.

### Fixed

- Validated `header.ID` in `amq read` before emitting receipts, closing a path-manipulation risk on malformed message headers.

## [0.30.1] - 2026-04-05
### Added

- Regression tests for session name detection: 2 Go monitor tests (JSON session field) and 5 Python tests covering all resolution paths (`AM_BASE_ROOT`, `.amqrc`, `.agent-mail`, sibling sessions, non-session roots).
- Python session-name tests integrated into `smoke-test.sh` for CI coverage.


## [0.30.0] - 2026-04-05
### Added

- Notifications from `wake`, `monitor`, and hook scripts now include the session name (e.g., `AMQ [stream3]: message from codex - ...`) so agents can identify which session a message belongs to in multi-session setups.
- `monitor` JSON output includes a new `session` field when inside a session context.
- Python hook scripts (`codex-amq-notify.py`, `claude-amq-user-prompt-submit.py`) mirror the full Go `classifyRoot` logic including `AM_BASE_ROOT` and `.amqrc` resolution for session detection.


## [0.29.1] - 2026-04-05
### Fixed

- `amq send` and `amq reply` no longer silently drop positional arguments; they now return a usage error (exit 2) suggesting `--body`.


## [0.29.0] - 2026-04-04
### Fixed

- `--root` flag now overrides `AM_ROOT` when explicitly provided, fixing cross-session and cross-project sends from within active coop sessions.
- `classifyRoot()` no longer blindly trusts stale `AM_BASE_ROOT`; validates the supplied root is actually under the base before using it.
- Consolidated skill publishing into `release.yml` so it runs directly after the release job instead of relying on a tag-triggered workflow (tags pushed with `GITHUB_TOKEN` do not trigger other workflows).

### Changed

- `classifyRoot()` recognizes the default `.agent-mail` directory convention, enabling session detection in projects without `.amqrc`.
- Removed `guardRootOverride()` and dead `validate()` call sites across all command handlers (-140 lines).
- `send` and `reply` emit a `note:` to stderr when `--root` overrides `AM_ROOT` for visibility.
- `configuredBaseRoot()` now logs `.amqrc` parse/permission errors to stderr instead of swallowing them silently.

- SHA-pinned all remaining GitHub Actions across every workflow.
- Added concurrency groups and timeouts to all workflows.
- Scoped `release.yml` permissions per job instead of top-level `contents: write`.
- Reduced `publish-skill.yml` to a manual `workflow_dispatch` fallback (no longer triggered by tag push).
- Added `skip-skill-publish` dispatch input to `release.yml` for manual reruns.
- Updated `release.sh` PR body to reflect the consolidated release flow.


## [0.28.8] - 2026-04-02
### Fixed

- Passed the temp release-notes path directly to GoReleaser so GitHub Actions preserves the `--release-notes` argument during publishing.

### Fixed

- Wrote generated GitHub release notes to the runner temp directory so GoReleaser can publish without dirtying the checked-out tree.


## [0.28.7] - 2026-04-02
### Fixed

- Let release verification honor an explicit `VERSION` override so CI checks the tagged binary instead of a `git describe` snapshot.


## [0.28.6] - 2026-04-02
### Changed

- Switched releases to the shared PR-based `scripts/release.sh` flow, with `CHANGELOG.md` supplying the GitHub release notes and CI creating the version tag only after the merged release commit verifies.

### Fixed

- Removed deprecated release shims so there is exactly one supported release entrypoint.


## [0.28.5] - 2026-04-01

### Fixed

- Pinned the GitHub release workflow to the GoReleaser v2 series instead of floating `latest`, so upstream releases cannot silently change the AMQ release pipeline.
- Keyed manual release reruns to the requested tag in the workflow concurrency group, so rerunning an older release no longer shares the default-branch concurrency slot.

## [0.28.4] - 2026-04-01

### Fixed

- Treated `Version already exists` as success when a skill publish reruns after retrying without an alias, preventing false-negative publish failures after a successful publish without alias fallback.

## [0.28.3] - 2026-04-01

### Changed

- Aligned the shared release helper and GitHub workflows so manual tag reruns, metadata verification, marketplace notification, and skill publishing all follow the same release path.

## [0.28.2] - 2026-04-01

### Fixed

- Ad-hoc signed macOS release binaries with the stable identifier `io.github.avivsinai.amq` so Keychain approvals survive Homebrew upgrades.

### Changed

- Moved the release workflow onto `macos-latest` so signed darwin artifacts are produced in CI before Homebrew updates.

## [0.28.1] - 2026-04-01

### Fixed

- Avoided retrying skill publishes after an alias failure when the package version had already been uploaded successfully.
- Hardened release metadata validation so skill and plugin manifest versions must match the release tag before publishing.

### Changed

- Added a default-branch marketplace dispatch workflow so plugin updates are announced after merges to `main`.
- Documented the marketplace dispatch behavior and generalized release helper usage from fixed examples to `X.Y.Z`.

## [0.28.0] - 2026-03-30

### Added

- Tag-based skill publishing aligned with versioned releases.
- Tab-title statusline guidance in the AMQ skill documentation.

### Fixed

- Addressed release workflow issues around dispatch input handling, version validation, and variable name collisions.

## [0.27.0] - 2026-03-30

### Added

- `amq env --session-name` flag: prints current session name for statusline embedding (empty + exit 0 when not in a session)
- `session_name` field in `amq env --json` output
- Session-aware routing instructions in amq-cli skill: Claude now discovers sessions via `amq who --json` before cross-session sends
- Statusline snippet documentation in SKILL.md for showing AMQ session in Claude Code status bar
- Wake presence heartbeat: `amq wake` touches presence on startup and every 30s, so `amq who` reports agents as active while working

### Changed

- Moved `classifyRoot` from `send.go` to `common.go` (shared by send, reply, who, env)
- Added `resolveSessionName` helper combining `classifyRoot` + `sessionName`

## [0.26.0] - 2026-03-29

### Added

- Embedded the AMQ design philosophy in the project docs.

### Changed

- Added the main-branch documentation policy and removed frozen implementation
  specs from the docs tree.
- Aligned plugin manifests and metadata for the 0.26.0 release.

## [0.25.1] - 2026-03-28

### Changed

- Bumped the Codex plugin manifest version to 0.25.1.

## [0.25.0] - 2026-03-28

### Added

- Added cross-orchestrator integration surfaces for Symphony, Kanban, and
  `doctor --ops` (#47).
- Added Codex interface metadata to the plugin manifest.

### Fixed

- Corrected cross-project sender identity by preserving `from_project` (#48).

### Changed

- Renamed the spec skill to `amq-spec` to avoid naming collisions.
- Eliminated duplicated skill packaging.

## [0.24.1] - 2026-03-22

### Fixed

- `amq who` always showed agents as "stale" because presence was only updated by explicit `amq presence set` calls
- Presence `LastSeen` is now auto-updated (best-effort) on `send`, `drain`, and `reply`
- `presence.Touch` only creates a default record on missing file — corrupt presence files are no longer silently overwritten

### Added

- `presence.Touch(root, handle)` function for lightweight presence refresh

## [0.24.0] - 2026-03-19

### Added

- Cross-project messaging: send messages between agents in different projects on the same machine
- `.amqrc` extended with `project` (self-identity) and `peers` (name→path registry) fields
- `--project` flag for `amq send` to target a peer project's inbox
- Inline `agent@project:session` addressing syntax for terser cross-project sends
- `reply_project` header field for automatic cross-project reply routing
- `DeliverToExistingInbox`: atomic Maildir delivery that never creates directories in peer projects
- `findAmqrcForRoot`: root-aware `.amqrc` lookup (works when cwd differs from project dir)
- Decision threads protocol: decentralized cross-project decisions using existing AMQ primitives
- Skill docs: Cross-Project Routing and Decision Threads sections (v1.7.0)
- New reference doc: `references/cross-project.md`

### Changed

- `findAmqrcForRoot` prioritizes root-based lookup over cwd when root is provided
- Session detection uses `.amqrc` base root comparison as fallback when `classifyRoot` fails
- `--json` output omits misleading `source_session` when sender is at base root

## [0.23.0] - 2026-03-18

### Added

- Shell completions: `amq completion bash|zsh|fish` generates tab-completion scripts
- Routed help: `amq help <command> [subcommand]` dispatches to command-specific help
- Command registry: centralized command metadata drives help, routing, and completions

### Fixed

- `amq init` and `amq cleanup` now exit with code 2 (not 1) on missing required flags
- Flag parse errors (`amq send --bogus`) now exit with code 2
- Unknown command/subcommand errors include help hints consistently
- `amq completion --help` shows usage instead of erroring
- `amq env --help` no longer shows duplicate "Usage:" header

### Changed

- Top-level and subcommand group help auto-generated from registry (single source of truth)
- Presence help enriched to match dlq/swarm/coop format
- Empty "Options:" section suppressed when command has no flags

## [0.22.0] - 2026-03-18

### Added

- Cross-session messaging via `amq send --session <name>` with reply routing between sessions
- `amq who` command to list sessions and agents with active/stale presence status

### Changed

- Cross-session peer-to-peer threads are now session-qualified to avoid collisions
- `coop exec` now sets `AM_BASE_ROOT` for cross-session resolution

### Fixed

- Tightened cross-session validation around `reply_to`, session context detection, and foreign inbox checks

## [0.21.0] - 2026-03-11

### Added

- `amq swarm fail` and `amq swarm block` for richer task lifecycle tracking
- `amq swarm complete --evidence <json|@file>` for attaching structured proof-of-work

### Changed

- Swarm tasks now support `failed` and `blocked` statuses in listings and bridge events

### Fixed

- Reclaiming failed or blocked tasks now clears stale failure, block, and evidence metadata

## [0.20.0] - 2026-03-11

### Added

- `/amq-spec` slash command for the collaborative specification workflow

### Changed

- Moved the spec workflow out of the core CLI and into the `amq-spec` skill
- Replaced spec-specific core message kinds with generic kinds plus labels

### Fixed

- Tightened spec workflow follow-ups and corrected `NEXT STEP` phase guidance
- Enforced send-first-research-second and prevented partner implementation during spec review
- Bumped Go to 1.25.8 for `govulncheck` advisories `GO-2026-4602` and `GO-2026-4601`

## [0.19.0] - 2026-03-05

### Added

- `amq coop spec` collaborative specification workflow with guided `NEXT STEP` output

### Changed

- `coop exec` now defaults to `--session collab` when neither `--session` nor `--root` is provided
- Message-routing commands now require an explicit AMQ root context instead of inferring one implicitly

### Fixed

- Suppressed duplicate update checks in the `coop exec` wake subprocess
- Corrected `AM_ROOT` guidance in the AMQ skill for usage outside `coop exec`

## [0.18.0] - 2026-02-24

### Added

- `amq shell-setup` command: outputs shell aliases for quick co-op session management
- `--session` flag on `coop exec` and `env`: pure sugar for `--root <base>/<session>`

### Changed

- `--root` is now literal — no implicit session subdirectory appended
- `.amqrc` format simplified: `{"root": "..."}` (removed `default_session`)
- `coop init` no longer prompts for shell alias installation (use `eval "$(amq shell-setup)"` instead)

### Removed

- `--install` flag from `shell-setup` (use `eval "$(amq shell-setup)"` in your rc file)
- `default_session` field from `.amqrc` format
- Interactive prompts from `coop init`

## [0.17.1] - 2026-02-11

### Fixed

- Don't overwrite `.amqrc` when `--root` is explicitly provided in `coop exec`

## [0.17.0] - 2026-02-10

### Added

- `coop exec` command for running agents inside a cooperative session

### Removed

- `coop shell` and `coop start` commands (replaced by `coop exec`)

### Changed

- `.amqrc` is now written to `defaultRoot` instead of CWD

## [0.16.0] - 2026-02-08

### Added

- Agent Teams (swarm) integration with full codebase review fixes
- Homebrew tap auto-update via goreleaser

### Fixed

- CI release race condition with concurrency control
- CI: add `HOMEBREW_TAP_GITHUB_TOKEN` for goreleaser brew push

### Changed

- Bump `actions/setup-node` from 4 to 6
- Bump `actions/checkout` from 4 to 6

## [0.15.0] - 2026-02-04

### Added

- Initiator protocol and wake interrupts for agent coordination

## [0.14.1] - 2026-02-01

### Fixed

- Add `.amqrc` to `.gitignore` during `coop init`
- Skill publish workflow handles existing versions gracefully

### Changed

- Bump `golang.org/x/term` from 0.38.0 to 0.39.0
- Bump `actions/checkout` from 6.0.1 to 6.0.2
- Bump `actions/setup-go` from 6.1.0 to 6.2.0
- Bump `golang.org/x/sys` from 0.39.0 to 0.40.0

## [0.14.0] - 2026-01-28

### Changed

- `coop start` no longer execs agent; auto-starts wake instead

## [0.13.1] - 2026-01-26

### Fixed

- Auto-create `.gitignore` with `agent-mail` directory entry

[0.41.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.41.0...v0.41.1
[0.41.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.37.1...v0.38.0
[0.37.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.35.0...v0.36.0
[0.35.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.34.1...v0.35.0
[0.34.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.34.0...v0.34.1
[0.34.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.33.0...v0.34.0
[0.33.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.2...v0.33.0
[0.32.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.1...v0.32.2
[0.32.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.0...v0.32.1
[0.32.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.3...v0.32.0
[0.31.3]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.2...v0.31.3
[0.31.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.1...v0.31.2
[0.31.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.0...v0.31.1
[0.31.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.30.1...v0.31.0
[0.30.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.30.0...v0.30.1
[0.30.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.29.1...v0.30.0
[0.29.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.29.0...v0.29.1
[0.29.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.8...v0.29.0
[0.28.8]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.7...v0.28.8
[0.28.7]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.6...v0.28.7
[0.28.6]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.5...v0.28.6
[0.28.5]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.4...v0.28.5
[0.28.4]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.3...v0.28.4
[0.28.3]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.2...v0.28.3
[0.28.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.1...v0.28.2
[0.28.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.0...v0.28.1
[0.28.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.27.0...v0.28.0
[0.27.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.26.0...v0.27.0
[0.26.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.25.1...v0.26.0
[0.25.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.25.0...v0.25.1
[0.25.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.24.1...v0.25.0
[0.24.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.24.0...v0.24.1
[0.24.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.1...v0.18.0
[0.17.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.1...v0.15.0
[0.14.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.0...v0.13.1
