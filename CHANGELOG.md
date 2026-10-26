# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Release Please generates new entries from conventional squash commits on
`main`; richer or multi-entry notes can be added through commit overrides or by
editing the release PR.

## [0.113.6](https://github.com/avivsinai/agent-message-queue/compare/v0.113.5...v0.113.6) (2026-10-26)


### Bug Fixes

* **coop:** preserve relay job control ([#1261](https://github.com/avivsinai/agent-message-queue/issues/1261)) ([926227a](https://github.com/avivsinai/agent-message-queue/commit/926227abbee029a27348ade4e9e125ecee1b6b35))

## [0.113.5](https://github.com/avivsinai/agent-message-queue/compare/v0.113.4...v0.113.5) (2026-10-26)


### Bug Fixes

* **wake:** verify notification loop health before reporting valid ([#1258](https://github.com/avivsinai/agent-message-queue/issues/1258)) ([475681d](https://github.com/avivsinai/agent-message-queue/commit/475681d237c38ea4194992e6ddb31fb6b4f74200))

## [0.113.4](https://github.com/avivsinai/agent-message-queue/compare/v0.113.3...v0.113.4) (2026-10-26)


### Features

* **remote:** carry the Buzz event id through the pi bridge ([#1254](https://github.com/avivsinai/agent-message-queue/issues/1254)) ([c15e866](https://github.com/avivsinai/agent-message-queue/commit/c15e86612b24d56d0f3640c29946766a6e0dc48b))
* **wake:** write darwin restart stages under ~/.amq/state ([#1255](https://github.com/avivsinai/agent-message-queue/issues/1255)) ([d51720b](https://github.com/avivsinai/agent-message-queue/commit/d51720b5741c3f9342b3a5a40ecba89f9c540503))


### Bug Fixes

* **usage:** keep plan limits fresh and recover collection ([#1253](https://github.com/avivsinai/agent-message-queue/issues/1253)) ([0738093](https://github.com/avivsinai/agent-message-queue/commit/0738093000f333619e52fef129d43c49bdfe07b9))
* **wake:** hold dialogs safely and bind startup release to its launch ([#1256](https://github.com/avivsinai/agent-message-queue/issues/1256)) ([c10d97c](https://github.com/avivsinai/agent-message-queue/commit/c10d97c43b6b73f794e5487d9e44f3ad548093ab))

## [0.113.3](https://github.com/avivsinai/agent-message-queue/compare/v0.113.2...v0.113.3) (2026-10-26)


### Bug Fixes

* **board:** close the panel with a second board command ([#1249](https://github.com/avivsinai/agent-message-queue/issues/1249)) ([a25d802](https://github.com/avivsinai/agent-message-queue/commit/a25d802658bebfad7d9be913785632fe34095076))
* **remote:** preserve busy cancellation and bind startup checks ([#1250](https://github.com/avivsinai/agent-message-queue/issues/1250)) ([4d2c37f](https://github.com/avivsinai/agent-message-queue/commit/4d2c37f2562046d62c91e04b36b074e714acccf9))
* **wake:** keep terminal notices out of harness composers ([#1251](https://github.com/avivsinai/agent-message-queue/issues/1251)) ([8d561e9](https://github.com/avivsinai/agent-message-queue/commit/8d561e9c62a2ab76ea61a0e4124881899d8f1f40))

## [0.113.2](https://github.com/avivsinai/agent-message-queue/compare/v0.113.1...v0.113.2) (2026-10-26)


### Bug Fixes

* **coop:** ring Codex seats at once unless a startup screen can show ([#1246](https://github.com/avivsinai/agent-message-queue/issues/1246)) ([676c44c](https://github.com/avivsinai/agent-message-queue/commit/676c44c42458e211dc1495aa501b30d875f78719))
* **wake:** show a notice at once when a dialog holds the doorbell ([#1247](https://github.com/avivsinai/agent-message-queue/issues/1247)) ([849d3d8](https://github.com/avivsinai/agent-message-queue/commit/849d3d8c4fae68ab6780e776a0486d38df604f1e))

## [0.113.1](https://github.com/avivsinai/agent-message-queue/compare/v0.113.0...v0.113.1) (2026-10-25)


### Bug Fixes

* **coop:** detect signed-in Codex daemon seats ([1c0cfca](https://github.com/avivsinai/agent-message-queue/commit/1c0cfca7e2116c362d10adb25bd1c039d9445a17))
* **coop:** require terminals before Codex startup ([#1243](https://github.com/avivsinai/agent-message-queue/issues/1243)) ([41119a1](https://github.com/avivsinai/agent-message-queue/commit/41119a100b2c36e2c7e20ad08414b3053140dfb0))
* **usage:** read symlinked homes and keep holds per home ([#1245](https://github.com/avivsinai/agent-message-queue/issues/1245)) ([df7f85b](https://github.com/avivsinai/agent-message-queue/commit/df7f85bf94d6ce67a0521921204bddbcd312d003))

## [0.113.0](https://github.com/avivsinai/agent-message-queue/compare/v0.112.0...v0.113.0) (2026-10-25)


### Features

* **board:** show Codex plan limits from app-server ([#1235](https://github.com/avivsinai/agent-message-queue/issues/1235)) ([7142efb](https://github.com/avivsinai/agent-message-queue/commit/7142efb8d1a7c3857f2e3d55774e882e5d3b0c68))


### Bug Fixes

* **board:** bound one-shot GitHub refreshes ([#1239](https://github.com/avivsinai/agent-message-queue/issues/1239)) ([de3c9e7](https://github.com/avivsinai/agent-message-queue/commit/de3c9e72fd557d3ba0dc80542eed7d63355f89f4))
* **coop:** warn when PATH hides a newer harness ([#1237](https://github.com/avivsinai/agent-message-queue/issues/1237)) ([200c696](https://github.com/avivsinai/agent-message-queue/commit/200c696ece366fdce1a1bf56e89fc34e505d87b6))
* **wake:** answer Cursor hooks with a neutral JSON object ([#1240](https://github.com/avivsinai/agent-message-queue/issues/1240)) ([e2c7320](https://github.com/avivsinai/agent-message-queue/commit/e2c7320aa3c184bcdd6cfd9a2c56d90d6f0409d3))
* **wake:** submit the Cursor doorbell after its paste settles ([#1236](https://github.com/avivsinai/agent-message-queue/issues/1236)) ([c1aab50](https://github.com/avivsinai/agent-message-queue/commit/c1aab508871285766fc9383d8b2984ca69fca789))

## [0.112.0](https://github.com/avivsinai/agent-message-queue/compare/v0.111.0...v0.112.0) (2026-10-25)


### Features

* **wake:** hold the doorbell while Cursor shows an approval prompt ([#1233](https://github.com/avivsinai/agent-message-queue/issues/1233)) ([cf991bd](https://github.com/avivsinai/agent-message-queue/commit/cf991bd3c1dc49d8c9d09a598385f572c8e8219c))


### Bug Fixes

* **board:** separate recent activity from live seats ([#1231](https://github.com/avivsinai/agent-message-queue/issues/1231)) ([e8ae799](https://github.com/avivsinai/agent-message-queue/commit/e8ae799834fbe37102d941177f196990d2ca23fb))
* **coop:** hold the doorbell through every Codex startup prompt ([#1232](https://github.com/avivsinai/agent-message-queue/issues/1232)) ([2855817](https://github.com/avivsinai/agent-message-queue/commit/2855817adc3014dd15caf243dbb6d4522c570630))
* **doctor:** show multiple Codex executables on PATH ([#1229](https://github.com/avivsinai/agent-message-queue/issues/1229)) ([72c37b3](https://github.com/avivsinai/agent-message-queue/commit/72c37b3e59745b93b1a32db988694822f7649a61))
* **remote:** register mailbox carriers for strict replies ([#1230](https://github.com/avivsinai/agent-message-queue/issues/1230)) ([1325454](https://github.com/avivsinai/agent-message-queue/commit/1325454cf8c4455ae5ff6ae126ac5e60eb2482ee))

## [0.111.0](https://github.com/avivsinai/agent-message-queue/compare/v0.110.0...v0.111.0) (2026-10-25)


### Features

* **remote:** deliver strict-share requests to AMQ mailboxes ([#1226](https://github.com/avivsinai/agent-message-queue/issues/1226)) ([2a779fe](https://github.com/avivsinai/agent-message-queue/commit/2a779fe2820eca674c174613dc0185671cdf9d4f))


### Bug Fixes

* **board:** count usage records once and say windows only when observed ([#1227](https://github.com/avivsinai/agent-message-queue/issues/1227)) ([1cd5c0d](https://github.com/avivsinai/agent-message-queue/commit/1cd5c0d25a620847e6a889da9095eb257ac0d8bf))

## [0.110.0](https://github.com/avivsinai/agent-message-queue/compare/v0.109.0...v0.110.0) (2026-10-25)


### Features

* **launch:** admit Codex workspace-write sandbox pins as provider arguments ([#1221](https://github.com/avivsinai/agent-message-queue/issues/1221)) ([b7e1058](https://github.com/avivsinai/agent-message-queue/commit/b7e1058d3ee2d126cc76b4c80b51695f9d62f3dd))
* **release:** create release PRs and tags with the release App ([#1224](https://github.com/avivsinai/agent-message-queue/issues/1224)) ([e5fb6b9](https://github.com/avivsinai/agent-message-queue/commit/e5fb6b9a05aa99a6b7976b3d107706148389c346))


### Bug Fixes

* **board:** page cells fit their longest value; document the live acceptance path ([#1222](https://github.com/avivsinai/agent-message-queue/issues/1222)) ([f5becb8](https://github.com/avivsinai/agent-message-queue/commit/f5becb8077d2f46eb5e19231fc3b296983a66e8c))
* **release:** request only the installed App permissions ([#1225](https://github.com/avivsinai/agent-message-queue/issues/1225)) ([79434c2](https://github.com/avivsinai/agent-message-queue/commit/79434c2b9487ee75c94c3fe41ad886b323272971))

## [0.109.0](https://github.com/avivsinai/agent-message-queue/compare/v0.108.0...v0.109.0) (2026-10-25)


### Features

* **board:** /board panel for Claude Code as an opt-in plugin ([#1216](https://github.com/avivsinai/agent-message-queue/issues/1216)) ([d8f37f4](https://github.com/avivsinai/agent-message-queue/commit/d8f37f4fcdfd2b0d5cb3afc38a48835a0fe5d46d))
* **board:** amq-board serve local live view ([#1215](https://github.com/avivsinai/agent-message-queue/issues/1215)) ([fd8e514](https://github.com/avivsinai/agent-message-queue/commit/fd8e514319a7ec9dcbdf6ba5f8563ca2b5bcb541))
* **board:** static page for amq-board ([#1209](https://github.com/avivsinai/agent-message-queue/issues/1209)) ([34f6f79](https://github.com/avivsinai/agent-message-queue/commit/34f6f79da153e2d4201e3d7685402aae80c7e712))


### Bug Fixes

* **board:** an open PR you authored waits on its reviewers, not on you ([#1218](https://github.com/avivsinai/agent-message-queue/issues/1218)) ([94cc0c9](https://github.com/avivsinai/agent-message-queue/commit/94cc0c92836142d7dbf88aa410ee5a728ddf1e9b))
* **board:** one uncountable mailbox is a note, not a degraded AMQ source ([#1217](https://github.com/avivsinai/agent-message-queue/issues/1217)) ([e5cbbcb](https://github.com/avivsinai/agent-message-queue/commit/e5cbbcb9a6255c1fc58fedf9934e9905978e56a2))
* **board:** read recent usage first so listing cannot starve the budget ([#1219](https://github.com/avivsinai/agent-message-queue/issues/1219)) ([1a05251](https://github.com/avivsinai/agent-message-queue/commit/1a0525119dffe66db201a1fe951107e0824cbbc9))
* **init:** create an explicit relative --root in the current directory ([#1213](https://github.com/avivsinai/agent-message-queue/issues/1213)) ([f02ccd6](https://github.com/avivsinai/agent-message-queue/commit/f02ccd6dbaed516b336c04c84773ea1612fd7df1))
* **owner-sign:** forbid core dumps before reading the owner key ([#1220](https://github.com/avivsinai/agent-message-queue/issues/1220)) ([b343868](https://github.com/avivsinai/agent-message-queue/commit/b343868f7f7dde2c93f76f1ab346a85eea742778))

## [0.108.0](https://github.com/avivsinai/agent-message-queue/compare/v0.107.0...v0.108.0) (2026-10-25)


### Features

* **board:** fleet board core with AMQ and beads sources ([#1172](https://github.com/avivsinai/agent-message-queue/issues/1172)) ([fdd2579](https://github.com/avivsinai/agent-message-queue/commit/fdd25792b13af93aed663b740edd7160be0ad9fd))
* **board:** GitHub source, board config, and external providers ([#1200](https://github.com/avivsinai/agent-message-queue/issues/1200)) ([e37f1be](https://github.com/avivsinai/agent-message-queue/commit/e37f1be4ed7e6cb079b1e62c36563bf9ccec6214))
* **board:** show Claude permission waits on their agents ([#1207](https://github.com/avivsinai/agent-message-queue/issues/1207)) ([8ec4dcf](https://github.com/avivsinai/agent-message-queue/commit/8ec4dcf6cf13bdacee999e42d56c074fa520ffca))
* **board:** show Codex goals for the current project ([#1202](https://github.com/avivsinai/agent-message-queue/issues/1202)) ([2defc0d](https://github.com/avivsinai/agent-message-queue/commit/2defc0dcc27aa073a644c84853f1ab346d446bfe))
* **board:** show usage per worker and limits per account ([#1203](https://github.com/avivsinai/agent-message-queue/issues/1203)) ([80d1841](https://github.com/avivsinai/agent-message-queue/commit/80d18411082b0ca348b4c543d932336e49a909b5))
* **cli:** classify agent registration from rosters and identity records ([#1195](https://github.com/avivsinai/agent-message-queue/issues/1195)) ([c3ca5db](https://github.com/avivsinai/agent-message-queue/commit/c3ca5db0c3edf6e3679a8de11c7fad47761f2422))
* **cli:** list open review requests, questions and todos per thread ([#1166](https://github.com/avivsinai/agent-message-queue/issues/1166)) ([fb36ddb](https://github.com/avivsinai/agent-message-queue/commit/fb36ddb23f1c71b0779115e1dbd46e917bed0064))
* **cli:** record and list which harness session belongs to each agent ([#1192](https://github.com/avivsinai/agent-message-queue/issues/1192)) ([1bbdac3](https://github.com/avivsinai/agent-message-queue/commit/1bbdac3108e453e6cba580ff651491124fc8a92d))
* **cli:** show every agent with its registration in who, presence and doctor ([#1196](https://github.com/avivsinai/agent-message-queue/issues/1196)) ([70410ed](https://github.com/avivsinai/agent-message-queue/commit/70410ed5b2b0d1a38b3878266b61ac55f55aa909))
* **coop:** one harness table, and generic wake behavior for unknown programs ([#1197](https://github.com/avivsinai/agent-message-queue/issues/1197)) ([59d17dc](https://github.com/avivsinai/agent-message-queue/commit/59d17dc210e72eaa55b63aec667b27e5d157bf9e))
* **coop:** record which harness runs under a handle ([#1201](https://github.com/avivsinai/agent-message-queue/issues/1201)) ([fed4fc9](https://github.com/avivsinai/agent-message-queue/commit/fed4fc96f38973e96224ba40174c69c4dfb8d39d))
* **launchapi:** let an embedder name the AMQ executable for managed panes ([#1186](https://github.com/avivsinai/agent-message-queue/issues/1186)) ([aaec267](https://github.com/avivsinai/agent-message-queue/commit/aaec267664a6df55cf149e296ca439b802b7ca6d))
* **launch:** start and resume pi seats with amq launch ([#1188](https://github.com/avivsinai/agent-message-queue/issues/1188)) ([575934f](https://github.com/avivsinai/agent-message-queue/commit/575934fb0617a40204c00df49e4b54473c247008))
* **pi:** hold the wake doorbell while a pi extension prompt is open ([#1199](https://github.com/avivsinai/agent-message-queue/issues/1199)) ([e5b79b4](https://github.com/avivsinai/agent-message-queue/commit/e5b79b4796163c2c1b6d9a1badd1dd7842503ba4))
* **setup:** name an agent and its harness separately ([#1205](https://github.com/avivsinai/agent-message-queue/issues/1205)) ([38b2a98](https://github.com/avivsinai/agent-message-queue/commit/38b2a987a6d1ba804a7d2f94d4d7c0054191e466))


### Bug Fixes

* **board:** one GitHub refresh at a time across board runs ([#1208](https://github.com/avivsinai/agent-message-queue/issues/1208)) ([b054486](https://github.com/avivsinai/agent-message-queue/commit/b054486700424deebba89e9860a8dd0f3ec65699))
* **coop:** keep the doorbell until Codex accepts input after its trust dialog ([#1204](https://github.com/avivsinai/agent-message-queue/issues/1204)) ([ea93c9a](https://github.com/avivsinai/agent-message-queue/commit/ea93c9a3647c69b737607a3f59a9234f7b078c19))
* **coop:** remove legacy hook scripts and notify only on new Codex mail ([#1198](https://github.com/avivsinai/agent-message-queue/issues/1198)) ([2ce39dc](https://github.com/avivsinai/agent-message-queue/commit/2ce39dc95e414784884858c84ece6a77d08d3b6a))
* **wake:** survive concurrent self-upgrades on macOS ([#1193](https://github.com/avivsinai/agent-message-queue/issues/1193)) ([46577ec](https://github.com/avivsinai/agent-message-queue/commit/46577ec98fd7aa61de89d6e3f6019d94542cd0df))

## [0.107.0](https://github.com/avivsinai/agent-message-queue/compare/v0.106.2...v0.107.0) (2026-10-23)


### Features

* **acp:** allow a Claude tool call with the owner's ✅ in Desktop ([#1179](https://github.com/avivsinai/agent-message-queue/issues/1179)) ([a843c19](https://github.com/avivsinai/agent-message-queue/commit/a843c1985950dfdbfed345712320c7ad358773b1))
* **wake:** hold the doorbell while Codex shows an approval prompt ([#1181](https://github.com/avivsinai/agent-message-queue/issues/1181)) ([c62069d](https://github.com/avivsinai/agent-message-queue/commit/c62069d58ea9bc6adc18542f09480fb7b213076f))


### Bug Fixes

* **coop:** choose the wake typing mode from the harness, not the handle ([#1187](https://github.com/avivsinai/agent-message-queue/issues/1187)) ([d3a5531](https://github.com/avivsinai/agent-message-queue/commit/d3a5531f48eb3a2448034508f2ffce502d45f148))
* **coop:** give every Codex seat its AMQ identity in shell commands ([#1176](https://github.com/avivsinai/agent-message-queue/issues/1176)) ([3983ded](https://github.com/avivsinai/agent-message-queue/commit/3983ded112d544cf65f7c523a65ce00f733317ae))
* **coop:** hold the doorbell while agy asks for permission or sign-in ([#1190](https://github.com/avivsinai/agent-message-queue/issues/1190)) ([f388b92](https://github.com/avivsinai/agent-message-queue/commit/f388b92da92400967467cab0f44c5f3afab57b42))
* **coop:** hold the doorbell while Claude or Codex asks to trust the folder ([#1180](https://github.com/avivsinai/agent-message-queue/issues/1180)) ([370d17c](https://github.com/avivsinai/agent-message-queue/commit/370d17ce89b09b8f2015f0574bf7ad4df8fda4c7))

## [0.106.2](https://github.com/avivsinai/agent-message-queue/compare/v0.106.1...v0.106.2) (2026-10-23)


### Bug Fixes

* **launch:** advertise every argument form that validation admits ([#1175](https://github.com/avivsinai/agent-message-queue/issues/1175)) ([38ded9e](https://github.com/avivsinai/agent-message-queue/commit/38ded9e04e5cabdda2f93b6bfc3ecc520fe94f03))


### Dependencies

* bump goreleaser/goreleaser-action from 7.2.3 to 7.3.0 ([#1183](https://github.com/avivsinai/agent-message-queue/issues/1183)) ([332c159](https://github.com/avivsinai/agent-message-queue/commit/332c159ba08ebaf112b69de6e8b0044531986b25))

## [0.106.1](https://github.com/avivsinai/agent-message-queue/compare/v0.106.0...v0.106.1) (2026-10-22)


### Bug Fixes

* **launch:** capture Codex seats after Apply and record the real backend ([#1174](https://github.com/avivsinai/agent-message-queue/issues/1174)) ([8e492b1](https://github.com/avivsinai/agent-message-queue/commit/8e492b112e96725c2ff0fe5e61284153bf22888a))

## [0.106.0](https://github.com/avivsinai/agent-message-queue/compare/v0.105.1...v0.106.0) (2026-10-22)


### Features

* **acp:** label mailbox-binding prompts with their Buzz DM event id ([#1168](https://github.com/avivsinai/agent-message-queue/issues/1168)) ([cdba235](https://github.com/avivsinai/agent-message-queue/commit/cdba235bb69e5590108d768c641000149e2cec8c))
* **remote:** allow a Claude tool call from the Buzz Desktop DM ([#1173](https://github.com/avivsinai/agent-message-queue/issues/1173)) ([a93c2fe](https://github.com/avivsinai/agent-message-queue/commit/a93c2fea47724a8b1c41760ece9b40211fad8b10))


### Bug Fixes

* **relay:** give AMQ-signed Nostr events the id NIP-01 clients compute ([#1170](https://github.com/avivsinai/agent-message-queue/issues/1170)) ([171bcb0](https://github.com/avivsinai/agent-message-queue/commit/171bcb0c2bb04a3f9f4cc1ea72626226231670d9))

## [0.105.1](https://github.com/avivsinai/agent-message-queue/compare/v0.105.0...v0.105.1) (2026-10-22)


### Bug Fixes

* **acp:** report only new agy tool denials per turn ([#1162](https://github.com/avivsinai/agent-message-queue/issues/1162)) ([e63cd8f](https://github.com/avivsinai/agent-message-queue/commit/e63cd8fc62fd30aa81b357009cdc8d9c643f5596))
* **coop:** ring the wake doorbell for agy again ([#1164](https://github.com/avivsinai/agent-message-queue/issues/1164)) ([066d068](https://github.com/avivsinai/agent-message-queue/commit/066d0681cf8f02d6fde264edc68075ed6ac48cd8))
* **hooks:** under a live wake, block the stop only for baseline mail ([#1152](https://github.com/avivsinai/agent-message-queue/issues/1152)) ([790736e](https://github.com/avivsinai/agent-message-queue/commit/790736ebccc742d740423ab4a6a4c80853e4718d))
* **presence:** keep the wake's notifier fields when presence changes ([#1161](https://github.com/avivsinai/agent-message-queue/issues/1161)) ([5890cff](https://github.com/avivsinai/agent-message-queue/commit/5890cffcb39fe2826ff46b70a06df727c6caeedf))

## [0.105.0](https://github.com/avivsinai/agent-message-queue/compare/v0.104.0...v0.105.0) (2026-10-22)


### Features

* **acp:** drive agy as a headless AMQ worker over stream-json ([#1143](https://github.com/avivsinai/agent-message-queue/issues/1143)) ([6fb0a91](https://github.com/avivsinai/agent-message-queue/commit/6fb0a9127d36c655760780a3ea5bdc8f5947c17c))
* **launch:** start an ACP host from amq launch ([#1142](https://github.com/avivsinai/agent-message-queue/issues/1142)) ([ad025a2](https://github.com/avivsinai/agent-message-queue/commit/ad025a2e2b7bddc1a9ced60914956df47f9b1b91))


### Bug Fixes

* **acp:** deliver to the pinned queue and thread, and never fake or drop an answer ([#1149](https://github.com/avivsinai/agent-message-queue/issues/1149)) ([8f442ef](https://github.com/avivsinai/agent-message-queue/commit/8f442ef07cba92589574a10ed73acedefb5182ce))
* **acp:** make the first host turn's journal durable before dispatch ([#1151](https://github.com/avivsinai/agent-message-queue/issues/1151)) ([4757a13](https://github.com/avivsinai/agent-message-queue/commit/4757a135683b89bd05c05fe60adc33824168580b))
* **coop:** do not type the wake doorbell into agy ([#1139](https://github.com/avivsinai/agent-message-queue/issues/1139)) ([2d91e0a](https://github.com/avivsinai/agent-message-queue/commit/2d91e0a339089a2fb5ffd0bcb88519854908e32e))
* **lock:** hold the file lock on Windows and share one lock implementation ([#1140](https://github.com/avivsinai/agent-message-queue/issues/1140)) ([5d1a964](https://github.com/avivsinai/agent-message-queue/commit/5d1a96480a98d6f88ec9fb903e7bb4d518858619))
* **remote:** admit a Codex request only on its own evidence and before its deadline ([#1146](https://github.com/avivsinai/agent-message-queue/issues/1146)) ([6413101](https://github.com/avivsinai/agent-message-queue/commit/64131012264109d95966f9f76ba64931f2a0bcc9))
* **remote:** doctor reports damaged request records and one bad Buzz reply never blocks the rest ([#1148](https://github.com/avivsinai/agent-message-queue/issues/1148)) ([18b71a5](https://github.com/avivsinai/agent-message-queue/commit/18b71a5e43bec1d2239b31d29971dbe54b46ed51))
* **remote:** keep link revocation, detach and live reload consistent ([#1145](https://github.com/avivsinai/agent-message-queue/issues/1145)) ([b62968f](https://github.com/avivsinai/agent-message-queue/commit/b62968f9b04810eacd8be481dc5c8d6c972b2c47))
* **remote:** release pi and ACP-host results when core acknowledges them ([#1144](https://github.com/avivsinai/agent-message-queue/issues/1144)) ([609a755](https://github.com/avivsinai/agent-message-queue/commit/609a7558ba6a18cf985695048b7d1fb0f0d7a2be))
* **remote:** run a Buzz request only in the native session it was sent to ([#1150](https://github.com/avivsinai/agent-message-queue/issues/1150)) ([574b594](https://github.com/avivsinai/agent-message-queue/commit/574b59426237060bcd66696b31595b9d59874677))
* **remote:** sender status and retry follow core execution ([#1147](https://github.com/avivsinai/agent-message-queue/issues/1147)) ([7dbaab2](https://github.com/avivsinai/agent-message-queue/commit/7dbaab233e44249ff2c2f6ef2b25b343394b7bc3))

## [0.104.0](https://github.com/avivsinai/agent-message-queue/compare/v0.103.0...v0.104.0) (2026-10-22)


### Features

* **acp:** cancel a running ACP turn from Buzz ([#1135](https://github.com/avivsinai/agent-message-queue/issues/1135)) ([bb95b5f](https://github.com/avivsinai/agent-message-queue/commit/bb95b5f2f802b76230cac1bcaf701f3924fd08c3))
* **acp:** drive an ACP harness from Buzz through amq-remote ([#1128](https://github.com/avivsinai/agent-message-queue/issues/1128)) ([68dfd5e](https://github.com/avivsinai/agent-message-queue/commit/68dfd5ef2b8a26af7329a1075b9637c0ec47b454))
* **acp:** resume the host session and probe a harness with --check ([#1117](https://github.com/avivsinai/agent-message-queue/issues/1117)) ([531bbc4](https://github.com/avivsinai/agent-message-queue/commit/531bbc45fcfb177693f967d653fb1d4690b92f43))
* **cli:** check the agy skill and add an agy shell alias ([#1138](https://github.com/avivsinai/agent-message-queue/issues/1138)) ([27820a7](https://github.com/avivsinai/agent-message-queue/commit/27820a79f41476ea7c62900e87f48619cde7deaa))


### Bug Fixes

* **fsq:** read a regular file renamed over the path after its lstat ([#1124](https://github.com/avivsinai/agent-message-queue/issues/1124)) ([7a4cf23](https://github.com/avivsinai/agent-message-queue/commit/7a4cf23090d0f4cd76f4f6d50594f3f4de20970c))
* **launch:** accept codex-cli at or above the verified capture floor ([#1108](https://github.com/avivsinai/agent-message-queue/issues/1108)) ([04faebf](https://github.com/avivsinai/agent-message-queue/commit/04faebf3ac4ae43412fdb3f996f92ea2f789edce))
* **launch:** Codex approval values that Codex accepts, per-seat containment refusal ([#1125](https://github.com/avivsinai/agent-message-queue/issues/1125)) ([3575701](https://github.com/avivsinai/agent-message-queue/commit/3575701fe6ac7893cb746338ef4c29d5495e2c28))
* **launch:** record Codex capture evidence from the seat ticket ([#1137](https://github.com/avivsinai/agent-message-queue/issues/1137)) ([c0c6e7f](https://github.com/avivsinai/agent-message-queue/commit/c0c6e7fd54fbc25ca2c9d138efbdde5abaa4704f))
* **upgrade:** upgrade amq-remote and amq-owner-sign with --all ([#1115](https://github.com/avivsinai/agent-message-queue/issues/1115)) ([f3684b4](https://github.com/avivsinai/agent-message-queue/commit/f3684b4338bd5160e7b99553a4cb43c9405910ed))

## [0.103.0](https://github.com/avivsinai/agent-message-queue/compare/v0.102.0...v0.103.0) (2026-10-21)


### Features

* **release:** Developer ID signing and notarization for macOS binaries ([#1101](https://github.com/avivsinai/agent-message-queue/issues/1101)) ([44e46b1](https://github.com/avivsinai/agent-message-queue/commit/44e46b17bc8d23e1fc7ed02aa8d9a67879c86ea6))


### Bug Fixes

* **build:** use Go 1.26.10 for the crypto/x509 fix ([#1111](https://github.com/avivsinai/agent-message-queue/issues/1111)) ([4205f08](https://github.com/avivsinai/agent-message-queue/commit/4205f0836a2fd244d5dd723b8041a8f400ef6aae))
* leave panicked approvals to the terminal and remove dead code ([#1110](https://github.com/avivsinai/agent-message-queue/issues/1110)) ([75e85ad](https://github.com/avivsinai/agent-message-queue/commit/75e85ad0abbd9f7af5ebd2859d77de2706749c11))
* remove internal names and keep testing out of the amq binary ([#1103](https://github.com/avivsinai/agent-message-queue/issues/1103)) ([141807c](https://github.com/avivsinai/agent-message-queue/commit/141807c2443888ea68af910ad3251033545b0abb))

## [0.102.0](https://github.com/avivsinai/agent-message-queue/compare/v0.101.2...v0.102.0) (2026-10-20)


### Features

* **acp:** mirror native turn activity into Desktop ([#1099](https://github.com/avivsinai/agent-message-queue/issues/1099)) ([8a71b75](https://github.com/avivsinai/agent-message-queue/commit/8a71b75bf91b8c339f796c80569c30be53e44401))
* **acp:** run an ACP harness as an AMQ participant ([#1102](https://github.com/avivsinai/agent-message-queue/issues/1102)) ([61d2643](https://github.com/avivsinai/agent-message-queue/commit/61d2643f34d5ae783d2094d4d2008735f63be141))
* **remote:** answer approvals of terminal-started Codex turns from Buzz ([#1100](https://github.com/avivsinai/agent-message-queue/issues/1100)) ([53a940c](https://github.com/avivsinai/agent-message-queue/commit/53a940cba2e09e6110e62633f7f103d2520ac212))


### Bug Fixes

* **remote:** warn that Claude auto mode defeats the approval pin ([#1097](https://github.com/avivsinai/agent-message-queue/issues/1097)) ([2b65b86](https://github.com/avivsinai/agent-message-queue/commit/2b65b868cf67f540dea4eb892468e5fc7233a28e))

## [0.101.2](https://github.com/avivsinai/agent-message-queue/compare/v0.101.1...v0.101.2) (2026-10-20)


### Bug Fixes

* **acp:** run Desktop-created agents with any parallelism ([#1094](https://github.com/avivsinai/agent-message-queue/issues/1094)) ([b9d86cb](https://github.com/avivsinai/agent-message-queue/commit/b9d86cb1577263930f69a303b304ea3ff01be677))
* **acp:** stop persisting sessions that have no channel id ([#1096](https://github.com/avivsinai/agent-message-queue/issues/1096)) ([f4fb086](https://github.com/avivsinai/agent-message-queue/commit/f4fb08615af2c8844962dfe1c928bdf379fc889a))

## [0.101.1](https://github.com/avivsinai/agent-message-queue/compare/v0.101.0...v0.101.1) (2026-10-19)


### Bug Fixes

* **acp:** make the owner an admin of the helper's control channel ([#1091](https://github.com/avivsinai/agent-message-queue/issues/1091)) ([db602bf](https://github.com/avivsinai/agent-message-queue/commit/db602bfc2553844be4285953230bb7e98fa0afc4))

## [0.101.0](https://github.com/avivsinai/agent-message-queue/compare/v0.100.0...v0.101.0) (2026-10-19)


### Features

* **keepalive:** move ~/.amq-keepalive to ~/.amq/keepalive with doctor --migrate-home ([#1089](https://github.com/avivsinai/agent-message-queue/issues/1089)) ([e0862ad](https://github.com/avivsinai/agent-message-queue/commit/e0862ad23249f0ed3b5685a333cfffefe098acd0))

## [0.100.0](https://github.com/avivsinai/agent-message-queue/compare/v0.99.0...v0.100.0) (2026-10-19)


### Features

* **acp:** reload turn timeout and poll settings each turn ([#1086](https://github.com/avivsinai/agent-message-queue/issues/1086)) ([847c211](https://github.com/avivsinai/agent-message-queue/commit/847c211fdc082217f997728cb70f3b70c0974895))
* **cli:** move launch trust to ~/.amq/state with doctor --migrate-home ([#1088](https://github.com/avivsinai/agent-message-queue/issues/1088)) ([55e5dfc](https://github.com/avivsinai/agent-message-queue/commit/55e5dfcab3be099005567658b8a40edb6d26e466))
* **wake:** accept wake restart stages under ~/.amq/state ([dd378a1](https://github.com/avivsinai/agent-message-queue/commit/dd378a127b147914ee981c6dd7cb4d15ba133844))
* **wake:** hot-reload the self-upgrade kill switch ([#1087](https://github.com/avivsinai/agent-message-queue/issues/1087)) ([e927c6e](https://github.com/avivsinai/agent-message-queue/commit/e927c6ee1c7d06f492b837a16df9610e586b896d))


### Bug Fixes

* **who:** find sessions of a custom base beside ~/.claude/agents ([#1084](https://github.com/avivsinai/agent-message-queue/issues/1084)) ([6529815](https://github.com/avivsinai/agent-message-queue/commit/65298151bedcbc7e7b057f574eb4db0004fbce8a))
* **who:** preserve known bases during session discovery ([#1080](https://github.com/avivsinai/agent-message-queue/issues/1080)) ([9b259d4](https://github.com/avivsinai/agent-message-queue/commit/9b259d4bc229806e2418bb2de724eb79d2fc4dcb))

## [0.99.0](https://github.com/avivsinai/agent-message-queue/compare/v0.98.0...v0.99.0) (2026-10-19)


### Features

* **cli:** remove one empty agent mailbox with cleanup --mailbox ([#1081](https://github.com/avivsinai/agent-message-queue/issues/1081)) ([a1d0862](https://github.com/avivsinai/agent-message-queue/commit/a1d08622277b67d94247d5815c631a7706c52035))

## [0.98.0](https://github.com/avivsinai/agent-message-queue/compare/v0.97.0...v0.98.0) (2026-10-18)


### Features

* **acp:** helper creates session agents through Desktop drafts ([#1076](https://github.com/avivsinai/agent-message-queue/issues/1076)) ([3ea7efe](https://github.com/avivsinai/agent-message-queue/commit/3ea7efe16bc7c4cadd377b60a2dc8db35ce76967))
* **acp:** pin by system prompt and archive with the session ([#1069](https://github.com/avivsinai/agent-message-queue/issues/1069)) ([6023d62](https://github.com/avivsinai/agent-message-queue/commit/6023d620ad63be0cc0a22ac2fac3d2eb947a4f83))
* **acp:** set up the helper agent with one command ([#1077](https://github.com/avivsinai/agent-message-queue/issues/1077)) ([ccfc799](https://github.com/avivsinai/agent-message-queue/commit/ccfc79987361f6009095da74f084961eb0207052))
* **remote:** connect from one command ([#1078](https://github.com/avivsinai/agent-message-queue/issues/1078)) ([f609122](https://github.com/avivsinai/agent-message-queue/commit/f609122d6542a523bf8beeb52a9516511516185e))
* **remote:** record session liveness and prune ended sessions ([c76bc33](https://github.com/avivsinai/agent-message-queue/commit/c76bc3377b9965c6323b2e9e51b1b60d211527d0))
* **remote:** restart the endpoint at login with service install ([c76bc33](https://github.com/avivsinai/agent-message-queue/commit/c76bc3377b9965c6323b2e9e51b1b60d211527d0))


### Bug Fixes

* **acp:** keep per-event state for each Buzz agent ([#1071](https://github.com/avivsinai/agent-message-queue/issues/1071)) ([0b6c2a1](https://github.com/avivsinai/agent-message-queue/commit/0b6c2a158ff09b45df8fc51750975ae4f7d38f88))
* **remote:** keep Codex sessions on one name and prune ended threads ([c76bc33](https://github.com/avivsinai/agent-message-queue/commit/c76bc3377b9965c6323b2e9e51b1b60d211527d0))
* **remote:** keep links in the machine root and find them from any session ([#1065](https://github.com/avivsinai/agent-message-queue/issues/1065)) ([ab81822](https://github.com/avivsinai/agent-message-queue/commit/ab81822b3a606b817b54cfd00114174c9a115136))
* **remote:** name bindings by session identity ([#1067](https://github.com/avivsinai/agent-message-queue/issues/1067)) ([970e366](https://github.com/avivsinai/agent-message-queue/commit/970e366d303730408a24ba920bbb44c6dd6f5a21))
* **remote:** refuse a Claude attach without the Stop hook ([c76bc33](https://github.com/avivsinai/agent-message-queue/commit/c76bc3377b9965c6323b2e9e51b1b60d211527d0))


### Dependencies

* bump actions/setup-node from 7.0.0 to 7.1.0 ([#1074](https://github.com/avivsinai/agent-message-queue/issues/1074)) ([8a6b782](https://github.com/avivsinai/agent-message-queue/commit/8a6b7827befbc7d90cf1117b831c635e36610be7))
* bump golang.org/x/crypto from 0.52.0 to 0.58.0 ([#1073](https://github.com/avivsinai/agent-message-queue/issues/1073)) ([37681f0](https://github.com/avivsinai/agent-message-queue/commit/37681f04638cab7774ef8cdcc15c9db7d3a54de0))
* bump golang.org/x/sys from 0.48.0 to 0.49.0 ([#1072](https://github.com/avivsinai/agent-message-queue/issues/1072)) ([489243b](https://github.com/avivsinai/agent-message-queue/commit/489243b0de89dc0727a3d67ba0ca0bc16ff53640))

## [0.97.0](https://github.com/avivsinai/agent-message-queue/compare/v0.96.1...v0.97.0) (2026-10-15)


### Features

* **bridge:** add grokbot and slack wake ring adapters ([#1057](https://github.com/avivsinai/agent-message-queue/issues/1057)) ([4d46fe9](https://github.com/avivsinai/agent-message-queue/commit/4d46fe9c43934c7aa8665cea6e0cf6c249ae0678))
* **bridge:** add up status and discard ([#1062](https://github.com/avivsinai/agent-message-queue/issues/1062)) ([18ef5fc](https://github.com/avivsinai/agent-message-queue/commit/18ef5fcda0bd79a3c7bfd60530d4a1fa0c8d6800))
* **bridge:** file v3 transfers through intent and published ([#1059](https://github.com/avivsinai/agent-message-queue/issues/1059)) ([8bcd4cc](https://github.com/avivsinai/agent-message-queue/commit/8bcd4cc3d836bd282ae0e8f3a9a3536421bc2027))
* **bridge:** freeze v3 transfer record beside Envelope v2 ([#1055](https://github.com/avivsinai/agent-message-queue/issues/1055)) ([0ac4efc](https://github.com/avivsinai/agent-message-queue/commit/0ac4efc1164f014bc3fb189839afa0ddea975125))
* **bridge:** pair peers only after a fingerprint match ([#1061](https://github.com/avivsinai/agent-message-queue/issues/1061)) ([ed09249](https://github.com/avivsinai/agent-message-queue/commit/ed09249c7afa08305ce8a4654c8fad8041e2aa4b))
* **bridge:** ship amq-bridge on Homebrew and the plugin ([#1063](https://github.com/avivsinai/agent-message-queue/issues/1063)) ([f459445](https://github.com/avivsinai/agent-message-queue/commit/f4594458484fbbb0fca723648bfcb1ab8cd8a5e5))
* **bridge:** sync one Noise round over a byte pipe ([#1060](https://github.com/avivsinai/agent-message-queue/issues/1060)) ([6fa7faf](https://github.com/avivsinai/agent-message-queue/commit/6fa7fafca875b52447e3de7389767fcddc2ef6dc))
* **bridge:** write peers file and durable tx outbox ([#1056](https://github.com/avivsinai/agent-message-queue/issues/1056)) ([8ec24f6](https://github.com/avivsinai/agent-message-queue/commit/8ec24f64aa6d781d29ce4937ee14665186fc1aba))
* **keepalive:** keep the keepalive home under ~/.amq/keepalive on a fresh install ([#1052](https://github.com/avivsinai/agent-message-queue/issues/1052)) ([f7be6bf](https://github.com/avivsinai/agent-message-queue/commit/f7be6bf843892eb52d76e9df164c5f0e4a252350))
* **launch:** keep launch trust state under ~/.amq/state on a fresh machine ([#1049](https://github.com/avivsinai/agent-message-queue/issues/1049)) ([7f2c20e](https://github.com/avivsinai/agent-message-queue/commit/7f2c20ed84c6bfa2da6074bf0d8565de4deb8318))
* **wake:** hold the doorbell while a Claude Code dialog is open ([#1058](https://github.com/avivsinai/agent-message-queue/issues/1058)) ([d375416](https://github.com/avivsinai/agent-message-queue/commit/d375416ea3e5f53da90233b4c75891205a3c1acd))


### Bug Fixes

* **remote:** reconcile relay shares from manifest.json in a running serve ([#1048](https://github.com/avivsinai/agent-message-queue/issues/1048)) ([fc23af6](https://github.com/avivsinai/agent-message-queue/commit/fc23af6ebc0f9c71e99df4e9ff84b323c7dedf79))

## [0.96.1](https://github.com/avivsinai/agent-message-queue/compare/v0.96.0...v0.96.1) (2026-10-11)


### Bug Fixes

* **fsq:** fall back to a guarded plain rename when RENAME_NOREPLACE is unsupported ([#1042](https://github.com/avivsinai/agent-message-queue/issues/1042)) ([e9e2cec](https://github.com/avivsinai/agent-message-queue/commit/e9e2ceccbffe9871f3511674bdb9629c57b82888))
* **remote:** install-stop-hook replaces a stop hook on another binary ([#1044](https://github.com/avivsinai/agent-message-queue/issues/1044)) ([51ecd7f](https://github.com/avivsinai/agent-message-queue/commit/51ecd7fe1e2659b9c0c18b5c5687d986c57d2095))

## [0.96.0](https://github.com/avivsinai/agent-message-queue/compare/v0.95.0...v0.96.0) (2026-10-11)


### Features

* **remote:** a linked server sees and touches only what it created and what you shared ([#1027](https://github.com/avivsinai/agent-message-queue/issues/1027)) ([dbac923](https://github.com/avivsinai/agent-message-queue/commit/dbac92304ae620f98f8eb8546375367cdadd7b04))
* **remote:** attach --self finds the pi (Amit) chat it runs in ([#1035](https://github.com/avivsinai/agent-message-queue/issues/1035)) ([a877ac1](https://github.com/avivsinai/agent-message-queue/commit/a877ac17b55407bec0d939ced1deac9be49625ab))
* **remote:** call a linked server's tools from this machine ([#1028](https://github.com/avivsinai/agent-message-queue/issues/1028)) ([286e5b3](https://github.com/avivsinai/agent-message-queue/commit/286e5b346e213a61e37e1aba3cd76d5c47dab513))
* **remote:** confirm a consent-local task on this machine with Touch ID ([#1031](https://github.com/avivsinai/agent-message-queue/issues/1031)) ([e5f61ce](https://github.com/avivsinai/agent-message-queue/commit/e5f61ce087c11467bb751a959a3581e250bf3bb5))
* **remote:** link this machine to a server over amq.remote.link/1 ([#1026](https://github.com/avivsinai/agent-message-queue/issues/1026)) ([e3d607a](https://github.com/avivsinai/agent-message-queue/commit/e3d607a5de70a2ed1540e6a904eb128cf0538093))
* **remote:** list live pi chats with their session id on discover ([e1831bc](https://github.com/avivsinai/agent-message-queue/commit/e1831bc4aa9b978d57ddb59e4f1bcdb33bdc6267))
* **remote:** pin the amq.remote.link/1 contract with golden frames and JCS ([#1025](https://github.com/avivsinai/agent-message-queue/issues/1025)) ([6408615](https://github.com/avivsinai/agent-message-queue/commit/64086153b12356fd9b70c12d648918cda245b141))
* **remote:** run a task only from bytes you signed, on the session you signed for ([#1030](https://github.com/avivsinai/agent-message-queue/issues/1030)) ([b2dd3b5](https://github.com/avivsinai/agent-message-queue/commit/b2dd3b5d8759c8b6ac5369396084bcb394c53d41))
* **remote:** show Claude approvals to sources that cannot answer them ([e1831bc](https://github.com/avivsinai/agent-message-queue/commit/e1831bc4aa9b978d57ddb59e4f1bcdb33bdc6267))
* **remote:** tell the pi bridge which source sent a task ([e1831bc](https://github.com/avivsinai/agent-message-queue/commit/e1831bc4aa9b978d57ddb59e4f1bcdb33bdc6267))


### Bug Fixes

* **remote:** a confirmed local task is not held again on a resend ([#1040](https://github.com/avivsinai/agent-message-queue/issues/1040)) ([02937fd](https://github.com/avivsinai/agent-message-queue/commit/02937fd6159f08195aa2eb959d3001ac0c1a38b4))
* **remote:** keep the store quota true after a busy retry and a failed seed ([#1032](https://github.com/avivsinai/agent-message-queue/issues/1032)) ([66b470e](https://github.com/avivsinai/agent-message-queue/commit/66b470e3e7f58147e14e40b889b8cacaadea0ca9))
* **remote:** link shares survive a session change ([#1041](https://github.com/avivsinai/agent-message-queue/issues/1041)) ([38f4bd3](https://github.com/avivsinai/agent-message-queue/commit/38f4bd32cecf6fe55712109b053b889998c1dc06))


### Upgrade notes

* **Restart `amq-remote up` after upgrading.** An endpoint started by the old version keeps running it. While it
  runs, `amq-remote link add` and `attach --link` refuse with "restart amq-remote up first".
* **Buzz `/status` reads only its own share.** A Buzz DM now reads, waits on and cancels only requests from its
  own share. Your terminal still reads every request.
* **Mailbox command keys must be `[a-z0-9_]`.** A command sent through the AMQ mailbox with any other key (for
  example `Op` or `REQUEST_ID`) is refused as `invalid`. AMQ's own clients already send lowercase keys.
* **An unreadable request file stops the endpoint.** A file in the request store that cannot be read
  (permissions, a symlink, an oversized file) makes the endpoint refuse to start, and the error names it. A
  corrupt but readable file is still skipped.
* **`attach --self` refuses an ambiguous session.** When two sessions report the same process, it refuses
  instead of picking one.
* **`AMQ_REMOTE_BINDING` needs a directory you own.** Its directory, or the nearest one that exists above it,
  must have no symlink on its path, be owned by you and not be group- or world-writable. Missing directories
  below it are created (0700).
* **`up --discover` prints `native_session_id=`** after a pi entry.
* **Downgrading:** run `amq-remote link remove NAME` before you go back to an older version. An older
  amq-remote does not know links, and it drops the manifest's `links` block if it rewrites the manifest.

## [0.95.0](https://github.com/avivsinai/agent-message-queue/compare/v0.94.0...v0.95.0) (2026-10-10)


### Features

* **wake:** set wake settings for the whole machine in ~/.amq/wake.settings ([#1036](https://github.com/avivsinai/agent-message-queue/issues/1036)) ([e2e8e77](https://github.com/avivsinai/agent-message-queue/commit/e2e8e77cae23ec71d448937ac4badd129198d8cb))

## [0.94.0](https://github.com/avivsinai/agent-message-queue/compare/v0.93.2...v0.94.0) (2026-10-09)


### Features

* **wake:** change wake settings live with amq wake config ([#1017](https://github.com/avivsinai/agent-message-queue/issues/1017)) ([3abfd0e](https://github.com/avivsinai/agent-message-queue/commit/3abfd0e7d5bedfe7132e01d5bf3e7f15d6f572a6)), closes [#1014](https://github.com/avivsinai/agent-message-queue/issues/1014)


### Bug Fixes

* **wake:** keep seeding argv settings and allow a wake startup grace ([#1019](https://github.com/avivsinai/agent-message-queue/issues/1019)) ([d84df51](https://github.com/avivsinai/agent-message-queue/commit/d84df515318015d02d096e35e3a706278abc9ad6)), closes [#1014](https://github.com/avivsinai/agent-message-queue/issues/1014)

## [0.93.2](https://github.com/avivsinai/agent-message-queue/compare/v0.93.1...v0.93.2) (2026-10-09)


### Bug Fixes

* **remote:** a bare native refusal says why in words ([#1015](https://github.com/avivsinai/agent-message-queue/issues/1015)) ([70b91da](https://github.com/avivsinai/agent-message-queue/commit/70b91da2e503e31c7bbe46c6671b78925a894d2c))

## [0.93.1](https://github.com/avivsinai/agent-message-queue/compare/v0.93.0...v0.93.1) (2026-10-09)


### Bug Fixes

* **remote:** a second same-option answer keeps the first one's intent ([#1007](https://github.com/avivsinai/agent-message-queue/issues/1007)) ([4afd0a9](https://github.com/avivsinai/agent-message-queue/commit/4afd0a904ef44e6a0188d74233d5315bf629cb31))

## [0.93.0](https://github.com/avivsinai/agent-message-queue/compare/v0.92.0...v0.93.0) (2026-10-09)


### Features

* **wake:** hold the doorbell by message priority ([#1008](https://github.com/avivsinai/agent-message-queue/issues/1008)) ([0407203](https://github.com/avivsinai/agent-message-queue/commit/0407203e0f999c0012c49b0eb29307d1c4db9d5a))


### Bug Fixes

* **build:** use Go 1.26.9 security toolchain ([#1010](https://github.com/avivsinai/agent-message-queue/issues/1010)) ([0f117b1](https://github.com/avivsinai/agent-message-queue/commit/0f117b19883f56e8f61894d868ff8b744ff5d3f3))

## [0.92.0](https://github.com/avivsinai/agent-message-queue/compare/v0.91.0...v0.92.0) (2026-10-08)


### Features

* **remote:** a yes typed in a Claude approval's thread allows it ([#1004](https://github.com/avivsinai/agent-message-queue/issues/1004)) ([68d7f2a](https://github.com/avivsinai/agent-message-queue/commit/68d7f2a4f79fc23c02c7ba136f53cf183d3d615b))


### Bug Fixes

* **remote:** one verdict per Claude allow proof, and the DM hears a refusal ([#1000](https://github.com/avivsinai/agent-message-queue/issues/1000)) ([5a1135a](https://github.com/avivsinai/agent-message-queue/commit/5a1135a0c7f12a0c1f0719944e7e0032dc1600e8))

## [0.91.0](https://github.com/avivsinai/agent-message-queue/compare/v0.90.0...v0.91.0) (2026-10-08)


### Features

* **cli:** resume a Codex --last or named thread under coop exec ([#1001](https://github.com/avivsinai/agent-message-queue/issues/1001)) ([089aeb8](https://github.com/avivsinai/agent-message-queue/commit/089aeb822baade6aeb514360a34df8956e95596b))


### Bug Fixes

* **remote:** an interrupted Claude turn cancels its request ([#1003](https://github.com/avivsinai/agent-message-queue/issues/1003)) ([6a85f22](https://github.com/avivsinai/agent-message-queue/commit/6a85f2283112b89cc2aa2ec308b83568fdb3709b))
* **remote:** attach names a Codex daemon socket the sandbox blocks ([#1002](https://github.com/avivsinai/agent-message-queue/issues/1002)) ([89c902c](https://github.com/avivsinai/agent-message-queue/commit/89c902c4ee5136b475caa909d87aebea192e4343))
* **remote:** Codex approvals hide secrets and approve only what shows ([#996](https://github.com/avivsinai/agent-message-queue/issues/996)) ([27150b5](https://github.com/avivsinai/agent-message-queue/commit/27150b54f7e9934ea9f7e3f7754a522645eee648))

## [0.90.0](https://github.com/avivsinai/agent-message-queue/compare/v0.89.2...v0.90.0) (2026-10-08)


### Features

* **remote:** deny a Claude tool call from the Buzz Desktop DM ([#995](https://github.com/avivsinai/agent-message-queue/issues/995)) ([76302ee](https://github.com/avivsinai/agent-message-queue/commit/76302eeaf5ac98d8ee722e35b84229e60cf74a41))

## [0.89.2](https://github.com/avivsinai/agent-message-queue/compare/v0.89.1...v0.89.2) (2026-10-07)


### Bug Fixes

* **coop:** amq inside a Codex 0.160 session knows its AMQ identity ([#993](https://github.com/avivsinai/agent-message-queue/issues/993)) ([33f151d](https://github.com/avivsinai/agent-message-queue/commit/33f151d92f389a7e2f9672d859eb756c561505cb))
* **coop:** name a Codex session on a busy daemon ([#994](https://github.com/avivsinai/agent-message-queue/issues/994)) ([385555b](https://github.com/avivsinai/agent-message-queue/commit/385555b739d81615e10762810826da9d0bf1d7b0))
* **remote:** tell Codex users the $amq-remote invocation ([2ae7753](https://github.com/avivsinai/agent-message-queue/commit/2ae7753fb07c575e6aa662ff55eac31fff3ee792))

## [0.89.1](https://github.com/avivsinai/agent-message-queue/compare/v0.89.0...v0.89.1) (2026-10-07)


### Bug Fixes

* **acp:** a non-message inbox entry is no proof of a Buzz delivery ([#990](https://github.com/avivsinai/agent-message-queue/issues/990)) ([404397d](https://github.com/avivsinai/agent-message-queue/commit/404397df6b37e1c15a6165e0813b5d8e5e142db3))
* **acp:** make the cockpit event journal a claim before delivery ([#976](https://github.com/avivsinai/agent-message-queue/issues/976)) ([dc5a0d3](https://github.com/avivsinai/agent-message-queue/commit/dc5a0d3e2049c0c6d4886c80d1583e34fa87b6b9))
* **remote:** ask the current attachment when retiring owed outcomes ([8511631](https://github.com/avivsinai/agent-message-queue/commit/8511631ba45da30ecc2a09a070eda43ba0b6ea58))
* **remote:** bound the Claude hook's allow check and serialize answer writes ([#986](https://github.com/avivsinai/agent-message-queue/issues/986)) ([35fc50b](https://github.com/avivsinai/agent-message-queue/commit/35fc50baaed65db42da77f7dc9cdd8e4e9816a70))
* **remote:** Codex permission and input requests show in the DM ([#985](https://github.com/avivsinai/agent-message-queue/issues/985)) ([427b00e](https://github.com/avivsinai/agent-message-queue/commit/427b00e652665921344ec0b4b5c7177813b7b8b6))
* **remote:** retire an unsettleable owed outcome at the horizon ([8511631](https://github.com/avivsinai/agent-message-queue/commit/8511631ba45da30ecc2a09a070eda43ba0b6ea58))

## [0.89.0](https://github.com/avivsinai/agent-message-queue/compare/v0.88.0...v0.89.0) (2026-10-07)


### Features

* **remote:** doctor names Codex's automatic approvals reviewer ([#982](https://github.com/avivsinai/agent-message-queue/issues/982)) ([b24b69d](https://github.com/avivsinai/agent-message-queue/commit/b24b69d476670490b0294b0ee2f12acd31103d8b))
* **remote:** pi targets publish live activity to the Buzz session panel ([#978](https://github.com/avivsinai/agent-message-queue/issues/978)) ([32a78c3](https://github.com/avivsinai/agent-message-queue/commit/32a78c32ac62fc87525a17d72548c689fc4f9fec))


### Bug Fixes

* **remote:** a Codex thread with no turn yet says to send one prompt first ([#983](https://github.com/avivsinai/agent-message-queue/issues/983)) ([2c26a10](https://github.com/avivsinai/agent-message-queue/commit/2c26a1096044f87daaf624f4cf192d9a3305d8bc))
* **remote:** a stored DM row whose relay OK was lost no longer stalls its request ([#984](https://github.com/avivsinai/agent-message-queue/issues/984)) ([ef1934a](https://github.com/avivsinai/agent-message-queue/commit/ef1934ad902d22649fe56b5a6fa990ba127038f4))
* **remote:** claude hook installers refuse --help instead of running ([#981](https://github.com/avivsinai/agent-message-queue/issues/981)) ([c15fd0a](https://github.com/avivsinai/agent-message-queue/commit/c15fd0a3790233bd677f0d6d15e40c9ee6dea8b9))
* **remote:** render adapter refusal text inert in the DM ([#972](https://github.com/avivsinai/agent-message-queue/issues/972)) ([0829b92](https://github.com/avivsinai/agent-message-queue/commit/0829b921c3a2ee538f4dfaa347002455fd997d6f))

## [0.88.0](https://github.com/avivsinai/agent-message-queue/compare/v0.87.0...v0.88.0) (2026-10-07)


### Features

* **acp:** follow-up Buzz DMs steer an open mailbox turn ([#965](https://github.com/avivsinai/agent-message-queue/issues/965)) ([1412b89](https://github.com/avivsinai/agent-message-queue/commit/1412b89fd895891c2eee5ef72d33678c2440a0b4))
* **remote:** answer a Buzz DM approval by typing yes or no ([#970](https://github.com/avivsinai/agent-message-queue/issues/970)) ([b95a84b](https://github.com/avivsinai/agent-message-queue/commit/b95a84bac4ca5c310347be881df2db8fa20675e7))


### Bug Fixes

* **acp:** a Buzz steer never recreates a removed mailbox ([#974](https://github.com/avivsinai/agent-message-queue/issues/974)) ([4098eb3](https://github.com/avivsinai/agent-message-queue/commit/4098eb3783b0be7446840141896e6299b1e1742b))
* **acp:** a mailbox DM that fails before delivery says not delivered ([#964](https://github.com/avivsinai/agent-message-queue/issues/964)) ([22312f6](https://github.com/avivsinai/agent-message-queue/commit/22312f6592afe95b56bb797ce2efac11156d3dc0))
* **coop:** name a Codex 0.160 session through the managed daemon ([#950](https://github.com/avivsinai/agent-message-queue/issues/950)) ([3cddfc7](https://github.com/avivsinai/agent-message-queue/commit/3cddfc71089a1843cfc3ee687f2607dff1d875bb))
* **coop:** name Codex 0.160 sessions launched with options ([#975](https://github.com/avivsinai/agent-message-queue/issues/975)) ([8a9ff75](https://github.com/avivsinai/agent-message-queue/commit/8a9ff7513b58ffd52cc11cae0b304f6c1616243f))
* **remote:** amq-acp ensures the buzz sender mailbox ([#951](https://github.com/avivsinai/agent-message-queue/issues/951)) ([27b4792](https://github.com/avivsinai/agent-message-queue/commit/27b4792ffca22403413125048af611fabe1caa14))
* **remote:** Buzz thread replies keep their root; refused rows stop blocking ([#971](https://github.com/avivsinai/agent-message-queue/issues/971)) ([3a71c12](https://github.com/avivsinai/agent-message-queue/commit/3a71c1261aadb7a33f93604567ba47e197bff863))
* **remote:** Codex 0.160 approvals reach the DM ([#969](https://github.com/avivsinai/agent-message-queue/issues/969)) ([d44c3ab](https://github.com/avivsinai/agent-message-queue/commit/d44c3abf166b7d1e1bd7d2b7294a853a374435fe))
* **remote:** the pi reference bridge publishes its session id ([#977](https://github.com/avivsinai/agent-message-queue/issues/977)) ([f290adb](https://github.com/avivsinai/agent-message-queue/commit/f290adbcc64f6041779a6ac68853ab7e10cf20c2))

## [0.87.0](https://github.com/avivsinai/agent-message-queue/compare/v0.86.1...v0.87.0) (2026-10-06)


### Features

* **remote:** allow a Claude tool call from Buzz with the owner's signed reaction ([#936](https://github.com/avivsinai/agent-message-queue/issues/936)) ([2f2420d](https://github.com/avivsinai/agent-message-queue/commit/2f2420d4b8abb24b836b876656c46caa471e0f36))


### Bug Fixes

* **acp:** a late Buzz DM reply still reaches the DM once ([5f691c1](https://github.com/avivsinai/agent-message-queue/commit/5f691c13a0eaef586f44cccdc68864bdd045a6c2))
* **acp:** a missing amq-remote model never falls back to another binding ([#954](https://github.com/avivsinai/agent-message-queue/issues/954)) ([007d14f](https://github.com/avivsinai/agent-message-queue/commit/007d14f86d3f2382b6a54037e2fe71fa9d7c5f9c))
* **acp:** a redelivered Buzz event posts its reply once ([#959](https://github.com/avivsinai/agent-message-queue/issues/959)) ([6fa6174](https://github.com/avivsinai/agent-message-queue/commit/6fa6174700384922cdfa5b74c6d2a0b03d8d36fa))
* **acp:** an open Buzz DM turn polls only new replies ([5f691c1](https://github.com/avivsinai/agent-message-queue/commit/5f691c13a0eaef586f44cccdc68864bdd045a6c2))
* **acp:** give the owner's Buzz key only to a verified Buzz.app CLI or the explicit override ([#962](https://github.com/avivsinai/agent-message-queue/issues/962)) ([665a8a8](https://github.com/avivsinai/agent-message-queue/commit/665a8a842820ada75452ac2d8bf001628c7e2f33))
* **acp:** prefer the Buzz.app CLI over PATH for the owner key ([#956](https://github.com/avivsinai/agent-message-queue/issues/956)) ([ba96347](https://github.com/avivsinai/agent-message-queue/commit/ba9634780272189dbec7d069a10cc26bc1753624))
* **coop:** start an unknown CLI quietly when naming is only the default ([#949](https://github.com/avivsinai/agent-message-queue/issues/949)) ([0018a7d](https://github.com/avivsinai/agent-message-queue/commit/0018a7de343e2feeeea0ec04381fa0bca496e0f1))
* **remote:** attach and detach honour --json ([#958](https://github.com/avivsinai/agent-message-queue/issues/958)) ([afa1de5](https://github.com/avivsinai/agent-message-queue/commit/afa1de575b99e8dd3deca63a2f446a313ebc9e4b))
* **remote:** attach lists buzz in the root roster ([#955](https://github.com/avivsinai/agent-message-queue/issues/955)) ([4fdd058](https://github.com/avivsinai/agent-message-queue/commit/4fdd0583fb446027111f0345a964b324d29d4e69))
* **remote:** attach refuses a taken binding name; detach needs a scope ([#953](https://github.com/avivsinai/agent-message-queue/issues/953)) ([d25b791](https://github.com/avivsinai/agent-message-queue/commit/d25b791b5978f7a4615585d9db2101c21dac1b6a))
* **remote:** bind the Buzz DM and presence by flag after enrollment ([#947](https://github.com/avivsinai/agent-message-queue/issues/947)) ([fd8c323](https://github.com/avivsinai/agent-message-queue/commit/fd8c323fe279e50970018eeff05432d4a3ada9c6))
* **remote:** Buzz setup steps need no Desktop restart ([#948](https://github.com/avivsinai/agent-message-queue/issues/948)) ([5c90f57](https://github.com/avivsinai/agent-message-queue/commit/5c90f579058e1576642f40c7d277992cf768b12f))
* **remote:** discover the codex 0.160 symlinked control socket ([#945](https://github.com/avivsinai/agent-message-queue/issues/945)) ([6d5928d](https://github.com/avivsinai/agent-message-queue/commit/6d5928d5ab6f1c7354698a7ce7b0459f8ca12486))
* **remote:** doctor names a stale amq-acp after an upgrade ([#933](https://github.com/avivsinai/agent-message-queue/issues/933)) ([9c5b824](https://github.com/avivsinai/agent-message-queue/commit/9c5b824264a228be632ad61861953f13fef9e631))
* **remote:** refuse a request record held by a case-variant key ([#943](https://github.com/avivsinai/agent-message-queue/issues/943)) ([43845f5](https://github.com/avivsinai/agent-message-queue/commit/43845f560629161e8c6f70fe1d98b53699de6b1d))
* **remote:** Stop hook allows a session bound under bindings/ ([#952](https://github.com/avivsinai/agent-message-queue/issues/952)) ([184b05c](https://github.com/avivsinai/agent-message-queue/commit/184b05c12844e19d66bcc077d345468e34214fd2))
* **remote:** the Buzz DM row names the target, not the request ref ([#946](https://github.com/avivsinai/agent-message-queue/issues/946)) ([c20f00c](https://github.com/avivsinai/agent-message-queue/commit/c20f00c184d8274a35b12b4cbad16c29df9c627d))
* **remote:** the owner sees a failed result write as uncertain ([#941](https://github.com/avivsinai/agent-message-queue/issues/941)) ([eaf3c5f](https://github.com/avivsinai/agent-message-queue/commit/eaf3c5f4adf387029ffd15b239c5851c186cc3e6))

## [0.86.1](https://github.com/avivsinai/agent-message-queue/compare/v0.86.0...v0.86.1) (2026-10-04)


### Bug Fixes

* **remote:** only the owner's Buzz share cancels its request ([#934](https://github.com/avivsinai/agent-message-queue/issues/934)) ([8608a69](https://github.com/avivsinai/agent-message-queue/commit/8608a69998a25388cc50586e3bdb196a34ac2b7d))
* **remote:** the spool drainer replays only a valid local submit ([#935](https://github.com/avivsinai/agent-message-queue/issues/935)) ([917dbfa](https://github.com/avivsinai/agent-message-queue/commit/917dbfadfabd31a4319ab25f157c3aa3dbc4d555))

## [0.86.0](https://github.com/avivsinai/agent-message-queue/compare/v0.85.0...v0.86.0) (2026-10-04)


### Features

* **remote:** block a Claude tool call from the Buzz DM ([#932](https://github.com/avivsinai/agent-message-queue/issues/932)) ([d49be10](https://github.com/avivsinai/agent-message-queue/commit/d49be105cdd0bd5da59b1ec9bbe5b779c139fb92))
* **remote:** pi bridge revision 4 approves a tool call from Buzz ([#926](https://github.com/avivsinai/agent-message-queue/issues/926)) ([517dc3d](https://github.com/avivsinai/agent-message-queue/commit/517dc3df025c879d59b8e8a33715e6bbfea78980))


### Bug Fixes

* **remote:** only the owner's Buzz share may answer an approval ([#930](https://github.com/avivsinai/agent-message-queue/issues/930)) ([f42f46f](https://github.com/avivsinai/agent-message-queue/commit/f42f46fbedef2c7dba58b53126c57ae7c96230d9))

## [0.85.0](https://github.com/avivsinai/agent-message-queue/compare/v0.84.0...v0.85.0) (2026-09-30)


### Features

* **remote:** name the pi build's own remedy when its bridge is too old ([#927](https://github.com/avivsinai/agent-message-queue/issues/927)) ([1c6f855](https://github.com/avivsinai/agent-message-queue/commit/1c6f8551202bdc14120ef7ffcea52f7e20181674))


### Bug Fixes

* **remote:** fence pi bridge revisions and close request-ownership gaps ([#923](https://github.com/avivsinai/agent-message-queue/issues/923)) ([9e83412](https://github.com/avivsinai/agent-message-queue/commit/9e83412d8711de44ec17085f57364b5d09f71b99))

## [0.84.0](https://github.com/avivsinai/agent-message-queue/compare/v0.83.0...v0.84.0) (2026-09-29)


### Features

* **remote:** approve Codex tool calls from the Buzz DM ([#919](https://github.com/avivsinai/agent-message-queue/issues/919)) ([86c4a3c](https://github.com/avivsinai/agent-message-queue/commit/86c4a3c3921240691ddb18080a21835840d8ceb0))


### Bug Fixes

* **remote:** correlate, pin, persist and busy-refuse pi bridge requests ([#920](https://github.com/avivsinai/agent-message-queue/issues/920)) ([a3b5728](https://github.com/avivsinai/agent-message-queue/commit/a3b57287defb8e714cbf6fdec1949bc78c960c7f))

## [0.83.0](https://github.com/avivsinai/agent-message-queue/compare/v0.82.0...v0.83.0) (2026-09-29)


### Features

* **wake:** let an explicit take retire a wake another surface started (--takeover) ([#916](https://github.com/avivsinai/agent-message-queue/issues/916)) ([bd5806c](https://github.com/avivsinai/agent-message-queue/commit/bd5806c849f802d7f395d11cea4a893da9eba68b))

## [0.82.0](https://github.com/avivsinai/agent-message-queue/compare/v0.81.2...v0.82.0) (2026-09-28)


### Features

* **remote:** one owner step enrolls a strict relay share ([#910](https://github.com/avivsinai/agent-message-queue/issues/910)) ([18f6f1c](https://github.com/avivsinai/agent-message-queue/commit/18f6f1cbbd309ca1f2ece2825c6dae8763847583))
* **remote:** ship the pi bridge extension and its public protocol ([#914](https://github.com/avivsinai/agent-message-queue/issues/914)) ([4209f4f](https://github.com/avivsinai/agent-message-queue/commit/4209f4ffd30f5c2153c6b9d4b18aae6b4971efd0))
* **remote:** stream Codex turns live into the Buzz activity view ([#913](https://github.com/avivsinai/agent-message-queue/issues/913)) ([8afa583](https://github.com/avivsinai/agent-message-queue/commit/8afa583d056f567c0b2bf8d87eb95a41d03f7ebc))
* **remote:** support pi as the harness kind; remove kind amit ([#912](https://github.com/avivsinai/agent-message-queue/issues/912)) ([36065c0](https://github.com/avivsinai/agent-message-queue/commit/36065c0472cd99780aa8e360643d3249b3885764))

## [0.81.2](https://github.com/avivsinai/agent-message-queue/compare/v0.81.1...v0.81.2) (2026-09-28)


### Bug Fixes

* **cli:** resolve bridged messages by header ID ([#849](https://github.com/avivsinai/agent-message-queue/issues/849)) ([43e9574](https://github.com/avivsinai/agent-message-queue/commit/43e95743fefd80b79aa8c3704ac9b8c80b62c838))
* **remote:** drop a dead amit pin after /new, reload or fork ([#902](https://github.com/avivsinai/agent-message-queue/issues/902)) ([0c83d8a](https://github.com/avivsinai/agent-message-queue/commit/0c83d8a78ac10deff2a0a39cb91c1b780105a50f))
* **remote:** keep admission evidence when reconcile races submit ([#909](https://github.com/avivsinai/agent-message-queue/issues/909)) ([a197209](https://github.com/avivsinai/agent-message-queue/commit/a197209ecc7d7e3f5343aea04a2f05e1d56487c0))
* **scripts:** stop the keepalive contract probe from leaking its wake ([#906](https://github.com/avivsinai/agent-message-queue/issues/906)) ([4d9665e](https://github.com/avivsinai/agent-message-queue/commit/4d9665ef0fa666ebeed1df0e6c108b27be9f8431))


### Dependencies

* bump github.com/btcsuite/btcd/btcec/v2 from 2.3.4 to 2.5.0 ([#899](https://github.com/avivsinai/agent-message-queue/issues/899)) ([2ef9667](https://github.com/avivsinai/agent-message-queue/commit/2ef96676ece5fb8105f97d21fa08434eb902af82))

## [0.81.1](https://github.com/avivsinai/agent-message-queue/compare/v0.81.0...v0.81.1) (2026-09-27)


### Bug Fixes

* **release:** allow the amq archive to hold fewer binaries on Windows ([#900](https://github.com/avivsinai/agent-message-queue/issues/900)) ([573df18](https://github.com/avivsinai/agent-message-queue/commit/573df1879dc5cc04052bedbfe949a26c377ef92e))
* **release:** re-release a tag's source with the dispatched GoReleaser config ([#903](https://github.com/avivsinai/agent-message-queue/issues/903)) ([c761892](https://github.com/avivsinai/agent-message-queue/commit/c76189212615d9d9165587448ad84da72f083088))
* **remote:** first amit submit after a restart completes ([#901](https://github.com/avivsinai/agent-message-queue/issues/901)) ([baf53e4](https://github.com/avivsinai/agent-message-queue/commit/baf53e47c9271c713912da3dfa4aa10f291641d8))

## [0.81.0](https://github.com/avivsinai/agent-message-queue/compare/v0.80.1...v0.81.0) (2026-09-24)


### Features

* **acp:** install a Buzz harness from the pinned shell ([#877](https://github.com/avivsinai/agent-message-queue/issues/877)) ([5e41e11](https://github.com/avivsinai/agent-message-queue/commit/5e41e11df356fc440bb79de8caf066a5c6fef481))
* **acp:** live ACP v2 cockpit turns over AMQ threads ([#854](https://github.com/avivsinai/agent-message-queue/issues/854)) ([4b929d6](https://github.com/avivsinai/agent-message-queue/commit/4b929d655d2da41ad180ccd3b925a3e501e6bbf2))
* **acp:** remote mode submits Buzz Desktop prompts to one amq-remote target ([#876](https://github.com/avivsinai/agent-message-queue/issues/876)) ([c533d37](https://github.com/avivsinai/agent-message-queue/commit/c533d3741f292f59772cb455f7459dc244447fc8))
* **acp:** steer and cancel live cockpit turns; replies correlate by refs ([#860](https://github.com/avivsinai/agent-message-queue/issues/860)) ([36b92ce](https://github.com/avivsinai/agent-message-queue/commit/36b92ce042259bff2b4aca7698b91c222264854b))
* **acp:** write the AMQ Remote Buzz import in one setup ([#884](https://github.com/avivsinai/agent-message-queue/issues/884)) ([0b80746](https://github.com/avivsinai/agent-message-queue/commit/0b80746e1c61debeca992d8e4a51a3f53461d140))
* **amq-bridge:** trust add provisions peer trust from identity public output (ug3) ([#845](https://github.com/avivsinai/agent-message-queue/issues/845)) ([3785459](https://github.com/avivsinai/agent-message-queue/commit/378545925a1367dae41893fdf4514f22fb2ab3da))
* **remote:** /amq-remote defaults to the AMQ mailbox carrier ([#895](https://github.com/avivsinai/agent-message-queue/issues/895)) ([1a301d0](https://github.com/avivsinai/agent-message-queue/commit/1a301d0d8d14bd85bd25298cbda993360da308a6))
* **remote/claude:** adapter skeleton — registry-backed inspect, honest refusal projection (611.12 PR1) ([#852](https://github.com/avivsinai/agent-message-queue/issues/852)) ([1fe7c8e](https://github.com/avivsinai/agent-message-queue/commit/1fe7c8ea463294b9f20b881f9a6f7a0157b674ec))
* **remote/claude:** submit over the pinned socket wire with a transcript evidence ladder and Stop-hook bridge ([#855](https://github.com/avivsinai/agent-message-queue/issues/855)) ([b86b3c2](https://github.com/avivsinai/agent-message-queue/commit/b86b3c2c91c0a06c9ee4354c499578c41865c145))
* **remote:** attach --self binds the Buzz agent to the invoking session ([#885](https://github.com/avivsinai/agent-message-queue/issues/885)) ([c8d376c](https://github.com/avivsinai/agent-message-queue/commit/c8d376cfb717cbef664dabd198296946de1622fa))
* **remote:** Buzz presence: owned profile, status and policy check (611.17) ([#867](https://github.com/avivsinai/agent-message-queue/issues/867)) ([91dcd05](https://github.com/avivsinai/agent-message-queue/commit/91dcd059285d9a1eba102a55fd813cfe55e1e707))
* **remote:** discover running Claude and Codex sessions for the manifest; serve prints attached targets ([#858](https://github.com/avivsinai/agent-message-queue/issues/858)) ([196ab2c](https://github.com/avivsinai/agent-message-queue/commit/196ab2c8e79ac2016b98ed0dbbd60509313fce1a))
* **remote:** doctor names each failing boundary with a remedy (611.18) ([#869](https://github.com/avivsinai/agent-message-queue/issues/869)) ([954ba7d](https://github.com/avivsinai/agent-message-queue/commit/954ba7df2a6277f9b56315773d2a23f5eae2fdd3))
* **remote:** enroll one bundle and write the relay block ([#878](https://github.com/avivsinai/agent-message-queue/issues/878)) ([92cfb37](https://github.com/avivsinai/agent-message-queue/commit/92cfb37828e5931deebdf26bd87ddcda73ce4226))
* **remote:** NIP-OA body identity — per-kind bodykey enrollment, share command, doctor ([#846](https://github.com/avivsinai/agent-message-queue/issues/846)) ([57a19ad](https://github.com/avivsinai/agent-message-queue/commit/57a19ad295d2c00c74a4c820a0fb688943c7f1fa))
* **remote:** one Buzz agent per session, selected by the ACP model ([#897](https://github.com/avivsinai/agent-message-queue/issues/897)) ([1e3876d](https://github.com/avivsinai/agent-message-queue/commit/1e3876d102da5944e2758a11beac2380bb38b9c7))
* **remote:** owner Buzz DM commands with one editable result row (611.16) ([#866](https://github.com/avivsinai/agent-message-queue/issues/866)) ([85a571f](https://github.com/avivsinai/agent-message-queue/commit/85a571f16084161ab8a1454bd4c6a6494fda9ae2))
* **remote:** project Claude transcript lines into kind 24200 frames ([#868](https://github.com/avivsinai/agent-message-queue/issues/868)) ([a2f5e32](https://github.com/avivsinai/agent-message-queue/commit/a2f5e32055d646961f6e77bcdcc3c687691cb308))
* **remote:** project Codex activity into kind 24200 frames ([#865](https://github.com/avivsinai/agent-message-queue/issues/865)) ([2262535](https://github.com/avivsinai/agent-message-queue/commit/22625356e881ad6a9a76e99c0a8636f831b364de))
* **remote:** relay AUTH for enrolled shares (611.15 slice 1) ([#863](https://github.com/avivsinai/agent-message-queue/issues/863)) ([7a559c9](https://github.com/avivsinai/agent-message-queue/commit/7a559c96cb3daaaadc832daf3d3da5254e1b3389))
* **remote:** serve exports Claude activity for pinned shares (611.17) ([#872](https://github.com/avivsinai/agent-message-queue/issues/872)) ([aa8abe1](https://github.com/avivsinai/agent-message-queue/commit/aa8abe17c5183c97966f2ab1c491288300521023))
* **remote:** serve exports Codex activity for pinned shares (611.17) ([#871](https://github.com/avivsinai/agent-message-queue/issues/871)) ([b14492e](https://github.com/avivsinai/agent-message-queue/commit/b14492e002fcd70f926391d10d47b1d9baba92dc))
* **skills:** /amq-remote connects this session to Buzz in one command ([#887](https://github.com/avivsinai/agent-message-queue/issues/887)) ([761da23](https://github.com/avivsinai/agent-message-queue/commit/761da23d78636df4e7faab83ff10e9e119aed842))


### Bug Fixes

* **acp:** advertise the one destination as the session model ([#883](https://github.com/avivsinai/agent-message-queue/issues/883)) ([5652b9b](https://github.com/avivsinai/agent-message-queue/commit/5652b9bc8a03d97e366b14b6c08f3947255a4e87))
* **acp:** post owner-facing answers into the Buzz DM ([#896](https://github.com/avivsinai/agent-message-queue/issues/896)) ([06b372a](https://github.com/avivsinai/agent-message-queue/commit/06b372a7abfa4b6a9f58925c65a805c354461d13))
* **acp:** queue a busy DM until the session can run it ([#889](https://github.com/avivsinai/agent-message-queue/issues/889)) ([e949eb8](https://github.com/avivsinai/agent-message-queue/commit/e949eb8c5162ef8576bc2ab8839bd0ded0720295))
* **amit:** a foreign-stream refusal on a terminal run lifts once the seam is clean v1 (e3b) ([#839](https://github.com/avivsinai/agent-message-queue/issues/839)) ([761bba4](https://github.com/avivsinai/agent-message-queue/commit/761bba4d11bfea03061c2476956000fe3208128a))
* **amq-remote:** describe only the exits and busy modes v1 has ([#862](https://github.com/avivsinai/agent-message-queue/issues/862)) ([814be90](https://github.com/avivsinai/agent-message-queue/commit/814be903350a9935294ba91e996a7dd4e214d468))
* **amq-remote:** install carrier warn handler + reply router before Reconcile (w4x) ([#836](https://github.com/avivsinai/agent-message-queue/issues/836)) ([fe4f1f2](https://github.com/avivsinai/agent-message-queue/commit/fe4f1f2434b90614bd6a7b5ed6eecca427430d4f))
* **cli:** stop the Codex notify test from spawning a shell ([#873](https://github.com/avivsinai/agent-message-queue/issues/873)) ([0468f74](https://github.com/avivsinai/agent-message-queue/commit/0468f74edbdededb27543835578889ed658c7122))
* **doctor:** name the confirmed relaunch for a wake lock another machine wrote ([#874](https://github.com/avivsinai/agent-message-queue/issues/874)) ([4483165](https://github.com/avivsinai/agent-message-queue/commit/4483165f0253d6669798849bf38b3dfdfe84006f))
* **fsq:** implement Windows directory sync via FlushFileBuffers (u35) ([#847](https://github.com/avivsinai/agent-message-queue/issues/847)) ([644a70f](https://github.com/avivsinai/agent-message-queue/commit/644a70fa802a572b40f0aa0cdb259c224d0f5121))
* **hooks:** keep an existing pre-push hook ([#882](https://github.com/avivsinai/agent-message-queue/issues/882)) ([30f9f75](https://github.com/avivsinai/agent-message-queue/commit/30f9f758363e98f716f68db2acc11333a7ac0bd3))
* **remote/claude:** recover in-flight requests after an endpoint restart; accept meta-flagged deliveries (v2.1.280) ([#859](https://github.com/avivsinai/agent-message-queue/issues/859)) ([b55391f](https://github.com/avivsinai/agent-message-queue/commit/b55391f0b383ef467232beb4d2be138f3a46a021))
* **remote/codex:** synchronize attachment client publication (611.22.20 BK5) ([#842](https://github.com/avivsinai/agent-message-queue/issues/842)) ([920ee64](https://github.com/avivsinai/agent-message-queue/commit/920ee64b6ce5758a6d2db27c276544ce4cd66969))
* **remote:** bound request-store reads to regular files ([#888](https://github.com/avivsinai/agent-message-queue/issues/888)) ([d28a554](https://github.com/avivsinai/agent-message-queue/commit/d28a5544b09b864cc8f8fbe4f2ed742733bc4bf0))
* **remote:** doctor names a missing endpoint route ([#880](https://github.com/avivsinai/agent-message-queue/issues/880)) ([3546be0](https://github.com/avivsinai/agent-message-queue/commit/3546be0dfd247debf0f125532ccc6f3e51c46fa2))
* **remote:** fence store close against in-flight writers ([#886](https://github.com/avivsinai/agent-message-queue/issues/886)) ([f7ba30f](https://github.com/avivsinai/agent-message-queue/commit/f7ba30f2761f0f181a896ea93806095a1b1ae8de))
* **remote:** keep the adapter refusal reason on the snapshot ([#857](https://github.com/avivsinai/agent-message-queue/issues/857)) ([c27d62a](https://github.com/avivsinai/agent-message-queue/commit/c27d62a4baa5933a7b91e1a3b061d8f1551c75d5))
* **remote:** limit the Claude Stop hook to bound sessions ([#894](https://github.com/avivsinai/agent-message-queue/issues/894)) ([6ba3162](https://github.com/avivsinai/agent-message-queue/commit/6ba3162ba3628566fc192ff962c8ce5190d561e7))
* **sender:** serialize Spool transactions across processes with a stable flock (asd) ([#838](https://github.com/avivsinai/agent-message-queue/issues/838)) ([f49593c](https://github.com/avivsinai/agent-message-queue/commit/f49593c7c260990fcd861d3a44ba086f746cda35))
* **tests:** isolate the test home, temp dirs and walks so no test reads or writes the operator's real HOME (988) ([#853](https://github.com/avivsinai/agent-message-queue/issues/853)) ([00f1768](https://github.com/avivsinai/agent-message-queue/commit/00f1768efd681679ef212ff623cbf26bcdea596a))


### Dependencies

* bump golang.org/x/crypto from 0.39.0 to 0.52.0 ([#875](https://github.com/avivsinai/agent-message-queue/issues/875)) ([f627bec](https://github.com/avivsinai/agent-message-queue/commit/f627bec3d3218534b767bf4791e6482ef2949194))

## [0.80.1](https://github.com/avivsinai/agent-message-queue/compare/v0.80.0...v0.80.1) (2026-09-20)


### Bug Fixes

* **amq-remote:** up residue — one refusal shape for one-up-per-root, owner in the refusal, secure existing registry dir (611.13.3) ([#832](https://github.com/avivsinai/agent-message-queue/issues/832)) ([eab8533](https://github.com/avivsinai/agent-message-queue/commit/eab853388169556b09778a21bab2feb82a273b7b))

## [0.80.0](https://github.com/avivsinai/agent-message-queue/compare/v0.79.0...v0.80.0) (2026-09-20)


### Features

* **remote:** .13 up command — lifetime ownership, flag forwarding, wake decoupling, doctor companions (611.13 PR2/3) ([#815](https://github.com/avivsinai/agent-message-queue/issues/815)) ([3245400](https://github.com/avivsinai/agent-message-queue/commit/3245400343cca4b9de30172b1e033eb0b5315878))
* **remote:** amit factory — extension-only adapter over the amq-bridge event log (611.13 PR3/3) ([#816](https://github.com/avivsinai/agent-message-queue/issues/816)) ([93b451a](https://github.com/avivsinai/agent-message-queue/commit/93b451a106251f9ba798a0e827f416fa22d5970c))
* **remote:** manifest + registry + serve + codex/fake/claude factories (611.13 PR1/3) ([#809](https://github.com/avivsinai/agent-message-queue/issues/809)) ([03f3a4c](https://github.com/avivsinai/agent-message-queue/commit/03f3a4cd8c08f59a13db2565b04f4d8fcd6c7a61))


### Bug Fixes

* **amq-remote:** bound up shutdown, stop respawn loops, reclaim phantom companions (611.13.2) ([#817](https://github.com/avivsinai/agent-message-queue/issues/817)) ([e53c701](https://github.com/avivsinai/agent-message-queue/commit/e53c70163ba1f21244fd3a70856f19d519fbbe32))
* **bridge:** transfer-ledger apply state machine + round-4 review fixes (611.14) ([#827](https://github.com/avivsinai/agent-message-queue/issues/827)) ([a97788a](https://github.com/avivsinai/agent-message-queue/commit/a97788a8ff7cdbd4913ff287e4ba72b78ee9c342))
* **cli:** a status message defaults to low priority, as the message-kinds table promises ([#820](https://github.com/avivsinai/agent-message-queue/issues/820)) ([e4f3b87](https://github.com/avivsinai/agent-message-queue/commit/e4f3b87e934bdd07225ab1d2a378fd517e82661e))
* **config:** EnsureAgent round-trips unmodelled config.json keys (611.22.55) ([#821](https://github.com/avivsinai/agent-message-queue/issues/821)) ([26a818b](https://github.com/avivsinai/agent-message-queue/commit/26a818b0fd66afb2ad5dc45d44a3697b7aa735d0))
* **config:** preserve unknown keys in setup and launch ([#823](https://github.com/avivsinai/agent-message-queue/issues/823)) ([357b1a0](https://github.com/avivsinai/agent-message-queue/commit/357b1a009fa150c22dce00f7e1afa67adcf17e57))
* **remote:** serve and doctor audit residue (611.13.4) ([#824](https://github.com/avivsinai/agent-message-queue/issues/824)) ([929f363](https://github.com/avivsinai/agent-message-queue/commit/929f363f933153833a92633fc199127fc6325527))
* **schemas:** remote-session submit evidence enum names only Go's classes (611.22.56) ([#822](https://github.com/avivsinai/agent-message-queue/issues/822)) ([f8b1f0f](https://github.com/avivsinai/agent-message-queue/commit/f8b1f0fe498bc15cf339b41a323daca52138f54d))


### Dependencies

* bump golang.org/x/term from 0.45.0 to 0.46.0 ([#808](https://github.com/avivsinai/agent-message-queue/issues/808)) ([59bc740](https://github.com/avivsinai/agent-message-queue/commit/59bc740fd4eb735e6b501b1e3dbf95499a6ecf43))

## [0.79.0](https://github.com/avivsinai/agent-message-queue/compare/v0.78.9...v0.79.0) (2026-09-18)


### Features

* **remote:** durable outgoing sender spool + .7 CLI integration ([#802](https://github.com/avivsinai/agent-message-queue/issues/802)) ([cc13d31](https://github.com/avivsinai/agent-message-queue/commit/cc13d3151ee5dffa2012d884ae1fbf054702d91c))
* **remote:** optional MinEvidence submit-input floor (Wave A.4) ([#803](https://github.com/avivsinai/agent-message-queue/issues/803)) ([38f8fda](https://github.com/avivsinai/agent-message-queue/commit/38f8fdaefc5d1f7035feef0f34fba7b2600c8db5))
* **remote:** print achieved evidence class on submit ([#803](https://github.com/avivsinai/agent-message-queue/issues/803) follow-up) ([#807](https://github.com/avivsinai/agent-message-queue/issues/807)) ([2ae43c9](https://github.com/avivsinai/agent-message-queue/commit/2ae43c99f236668e176d78817c4b936ee537e381))
* **remote:** register mailbox handle in config.json on serve startup (.10) ([#804](https://github.com/avivsinai/agent-message-queue/issues/804)) ([fe17f43](https://github.com/avivsinai/agent-message-queue/commit/fe17f439f1f9f5b5275d13906b1ab7cec527c88b))


### Bug Fixes

* **core:** per-key drain obligations + marker-only retry retirement (611.22.48) ([#806](https://github.com/avivsinai/agent-message-queue/issues/806)) ([ecf4c78](https://github.com/avivsinai/agent-message-queue/commit/ecf4c782153d50240ffc9c704fe4e38d11e2e68b))
* **remote:** bounded retention + aggregate quota (BK4 611.22.19) ([#800](https://github.com/avivsinai/agent-message-queue/issues/800)) ([83108cd](https://github.com/avivsinai/agent-message-queue/commit/83108cdc24d044f36707dc829c1b8c6fb68c4c56))

## [0.78.9](https://github.com/avivsinai/agent-message-queue/compare/v0.78.8...v0.78.9) (2026-09-16)


### Bug Fixes

* **611.23:** pre-bind sun_path length check with typed error ([#792](https://github.com/avivsinai/agent-message-queue/issues/792)) ([c20cac3](https://github.com/avivsinai/agent-message-queue/commit/c20cac399b20fd3f3b92883afe042875e7f605c8))
* **7ra:** scrub parent environment before spawning wake child process ([#790](https://github.com/avivsinai/agent-message-queue/issues/790)) ([92efbb3](https://github.com/avivsinai/agent-message-queue/commit/92efbb39f78a0dfb38843a5da93a6cdac97f88ce))
* **fdf:** widen inject timeout from 1s to 5s in wall-clock-sensitive wake tests ([#794](https://github.com/avivsinai/agent-message-queue/issues/794)) ([2095868](https://github.com/avivsinai/agent-message-queue/commit/20958682e8c4977cae71af70a46876dc0d5e7aca))
* **pgn:** guide user to recovery when wake cleanup hits changed-stage refusal ([#797](https://github.com/avivsinai/agent-message-queue/issues/797)) ([0351e24](https://github.com/avivsinai/agent-message-queue/commit/0351e24c1a3efd564b50965f87229f7b007649ca))
* **t64:** persist degraded notifier status on attention-only fallback ([#796](https://github.com/avivsinai/agent-message-queue/issues/796)) ([c65d3df](https://github.com/avivsinai/agent-message-queue/commit/c65d3df8e395bd9d9ee51fd9738494e828c90489))

## [0.78.8](https://github.com/avivsinai/agent-message-queue/compare/v0.78.7...v0.78.8) (2026-09-16)


### Bug Fixes

* **65s:** bind Darwin control listener under lifecycle guard + revalidate ([#787](https://github.com/avivsinai/agent-message-queue/issues/787)) ([a6e0c25](https://github.com/avivsinai/agent-message-queue/commit/a6e0c25e53b488818e05483a1c62700d84fda30f))
* **byc:** revalidate canonical wake namespace before authoritative reads + blind-unlinks ([#788](https://github.com/avivsinai/agent-message-queue/issues/788)) ([57eeb40](https://github.com/avivsinai/agent-message-queue/commit/57eeb401c9cd7855dd4335278626668f2d213dfc))

## [0.78.7](https://github.com/avivsinai/agent-message-queue/compare/v0.78.6...v0.78.7) (2026-09-16)


### Bug Fixes

* **amqio:** curtail full cur resweep on persistent read failures (611.22.46) ([#783](https://github.com/avivsinai/agent-message-queue/issues/783)) ([92b711c](https://github.com/avivsinai/agent-message-queue/commit/92b711cda8edadf919188bcec16d1628f24679d8))
* **core:** gate Reconcile/Tick on lifecycle state (611.22.47) ([#782](https://github.com/avivsinai/agent-message-queue/issues/782)) ([86ee4b9](https://github.com/avivsinai/agent-message-queue/commit/86ee4b99f17e61f4a8d439c2e3315556310b0fcb))
* **core:** move carrier.Publish outside e.mu (611.22.48) ([#784](https://github.com/avivsinai/agent-message-queue/issues/784)) ([2a5e583](https://github.com/avivsinai/agent-message-queue/commit/2a5e58363df20b87925e9765d5be8dee94236703))

## [0.78.6](https://github.com/avivsinai/agent-message-queue/compare/v0.78.5...v0.78.6) (2026-09-16)


### Bug Fixes

* **cli:** classify non-ENOENT session Lstat as ErrPeerRootUnreachable (611.22.42) ([#780](https://github.com/avivsinai/agent-message-queue/issues/780)) ([8766aee](https://github.com/avivsinai/agent-message-queue/commit/8766aeefcaf19a57219228847dac67f6c24edc0d))

## [0.78.5](https://github.com/avivsinai/agent-message-queue/compare/v0.78.4...v0.78.5) (2026-09-16)


### Bug Fixes

* **amqio:** gate cross-session reply on ValidateExistingMailboxLayout (611.22.43) ([#777](https://github.com/avivsinai/agent-message-queue/issues/777)) ([db7c7dd](https://github.com/avivsinai/agent-message-queue/commit/db7c7dd244e8775cfc50bad5224da779c6959092))
* **amqio:** validate reply-ledger id in read+write (bead 611.22.44) ([#778](https://github.com/avivsinai/agent-message-queue/issues/778)) ([bc49e24](https://github.com/avivsinai/agent-message-queue/commit/bc49e24efa8b66897cca47123c22f677d288554f))

## [0.78.4](https://github.com/avivsinai/agent-message-queue/compare/v0.78.3...v0.78.4) (2026-09-16)


### Bug Fixes

* **remote/core:** compare history evidence against the record's stable digest, not the full memo ([#774](https://github.com/avivsinai/agent-message-queue/issues/774)) ([5757bf2](https://github.com/avivsinai/agent-message-queue/commit/5757bf2c7bb54f24ff479845b50e6c5dcc2407e4))

## [0.78.3](https://github.com/avivsinai/agent-message-queue/compare/v0.78.2...v0.78.3) (2026-09-15)


### Bug Fixes

* **remote/codex:** check terminal before the lostStateGen guard in the turn/start continuation (e5q) ([#769](https://github.com/avivsinai/agent-message-queue/issues/769)) ([1a4c831](https://github.com/avivsinai/agent-message-queue/commit/1a4c831380c5d83c88a67be83cf5ddad87f995e8))
* **remote/codex:** testDispatchGate becomes atomic.Pointer with a single write path (jb7) ([#770](https://github.com/avivsinai/agent-message-queue/issues/770)) ([37d2a08](https://github.com/avivsinai/agent-message-queue/commit/37d2a086d1c7bc6a7fcfbdd7b5f4eacbd84d156d))

## [0.78.2](https://github.com/avivsinai/agent-message-queue/compare/v0.78.1...v0.78.2) (2026-09-15)


### Bug Fixes

* **cli:** register amit as a coop-named argv harness ([#762](https://github.com/avivsinai/agent-message-queue/issues/762)) ([491e02a](https://github.com/avivsinai/agent-message-queue/commit/491e02a0417e804414745b37f9ee4f671312f108))
* **remote/codex:** lost-state generation guard on the turn/start restoration path (611.22.39-r2) ([#764](https://github.com/avivsinai/agent-message-queue/issues/764)) ([661a9fb](https://github.com/avivsinai/agent-message-queue/commit/661a9fbcc156a833c959f35345beee79f7547518))
* **remote/core:** durable ack-confirmed flag so ack replay converges across restarts (611.22.34) ([#767](https://github.com/avivsinai/agent-message-queue/issues/767)) ([879914e](https://github.com/avivsinai/agent-message-queue/commit/879914ee942e881b886c76d81b6af92e64d4468e))
* **remote/requests:** CompactOne marks the tombstone published so Reconcile does not republish it (611.22.41) ([#766](https://github.com/avivsinai/agent-message-queue/issues/766)) ([054dd62](https://github.com/avivsinai/agent-message-queue/commit/054dd62b4670dcc5635e50bc4d2dc0d049b6e677))

## [0.78.1](https://github.com/avivsinai/agent-message-queue/compare/v0.78.0...v0.78.1) (2026-09-15)


### Bug Fixes

* **codex/ws:** bound the close-frame reply and Client.Respond write (611.22.40) ([#759](https://github.com/avivsinai/agent-message-queue/issues/759)) ([f0e98ad](https://github.com/avivsinai/agent-message-queue/commit/f0e98ad702a360d5e8670a7224c3f578c3c4f0ed))
* **codex:** drop stale activeTurn on notLoaded/systemError status (611.22.39) ([#758](https://github.com/avivsinai/agent-message-queue/issues/758)) ([7e8e0e4](https://github.com/avivsinai/agent-message-queue/commit/7e8e0e44b726615abd6c20b00bcd46268757ab26))
* **codex:** Inspect picks PendingInteraction deterministically — oldest first (yl0) ([#760](https://github.com/avivsinai/agent-message-queue/issues/760)) ([144a76d](https://github.com/avivsinai/agent-message-queue/commit/144a76ddd5b83160b13b9bc4cc63ca13f943b8a9))
* **fsq:** sync every mailbox level a first-contact delivery creates ([#742](https://github.com/avivsinai/agent-message-queue/issues/742)) ([2087569](https://github.com/avivsinai/agent-message-queue/commit/2087569e982cbd95507e34c3956d3c8af6027ec9))
* **remote/amqio:** a visible-but-unsynced publish is not a durable publish ([#754](https://github.com/avivsinai/agent-message-queue/issues/754)) ([9d968cd](https://github.com/avivsinai/agent-message-queue/commit/9d968cdfdef5f33f17343bfe1edb20446181f0da))
* **remote/amqio:** answer a cross-project caller in ITS root, not ours ([#743](https://github.com/avivsinai/agent-message-queue/issues/743)) ([5befa1e](https://github.com/avivsinai/agent-message-queue/commit/5befa1eb47651a3d8f1008310a251b8b29645884))
* **remote/amqio:** make the creator host injective ([#747](https://github.com/avivsinai/agent-message-queue/issues/747)) ([a6641e0](https://github.com/avivsinai/agent-message-queue/commit/a6641e081a64369350c016ca9863718bf484b576))
* **remote/amqio:** reconcile claimed-but-unreceipted commands in cur ([#752](https://github.com/avivsinai/agent-message-queue/issues/752)) ([2ab67bb](https://github.com/avivsinai/agent-message-queue/commit/2ab67bb87efff2a90543b56e9265bf542420df45))
* **remote/codex:** install handlers before the read pump starts ([#751](https://github.com/avivsinai/agent-message-queue/issues/751)) ([eb0efa9](https://github.com/avivsinai/agent-message-queue/commit/eb0efa93eb606f61efa98ab5146cda2f216f84de))
* **remote/codex:** release acked evidence; never treat absence as refusal ([#744](https://github.com/avivsinai/agent-message-queue/issues/744)) ([d6c19be](https://github.com/avivsinai/agent-message-queue/commit/d6c19be7b45b527cc868460aeac1cae3a74664d3))
* **remote/core:** B14e — bounded compaction with per-record re-read + settlement gate ([#753](https://github.com/avivsinai/agent-message-queue/issues/753)) ([d57b0e0](https://github.com/avivsinai/agent-message-queue/commit/d57b0e04ba53762fcc599e06fdf5d0733e96e34d))
* **remote/core:** endpoint lifecycle accepting/draining/closed; D1 gate at admitDeferred ([#755](https://github.com/avivsinai/agent-message-queue/issues/755)) ([5b7f200](https://github.com/avivsinai/agent-message-queue/commit/5b7f20037b4ee06411a4e1f4ccbd6e2e18710b60))
* **remote/core:** per-runtime reservation fail-closed + raced-cancel confirmation (B14c) ([#739](https://github.com/avivsinai/agent-message-queue/issues/739)) ([250c642](https://github.com/avivsinai/agent-message-queue/commit/250c642b1ca6a6753d89b914fb673d7dfaad4a4b))
* **remote/ipc:** bound every I/O boundary; a ctx error is not a failure ([#746](https://github.com/avivsinai/agent-message-queue/issues/746)) ([3961080](https://github.com/avivsinai/agent-message-queue/commit/39610809d2155f6b9fdd7f09115d75b08771410e))
* **remote/protocol:** the digest doc contradicted the digest ([#748](https://github.com/avivsinai/agent-message-queue/issues/748)) ([85d124a](https://github.com/avivsinai/agent-message-queue/commit/85d124af3fd6ea2780d92049f2c63a32ff87794a))


### Dependencies

* bump golang.org/x/sys from 0.47.0 to 0.48.0 ([#740](https://github.com/avivsinai/agent-message-queue/issues/740)) ([418d3a7](https://github.com/avivsinai/agent-message-queue/commit/418d3a759faf24ac14f1427b948d260320f89646))

## [0.78.0](https://github.com/avivsinai/agent-message-queue/compare/v0.77.3...v0.78.0) (2026-09-12)


### Features

* **remote:** request core, endpoint, fake runtime, AMQ carrier and amq-remote CLI ([f7a1a46](https://github.com/avivsinai/agent-message-queue/commit/f7a1a462f0a8b17253a273611c2935e0de091a9f))


### Bug Fixes

* **remote/amqio:** a store refusal is not a durable answer ([#745](https://github.com/avivsinai/agent-message-queue/issues/745)) ([2097dcf](https://github.com/avivsinai/agent-message-queue/commit/2097dcf765bef867587b00d683c3a8e502e8b597))
* **remote/codex:** bound writer + non-lossy reply-required + safe teardown (B14b) ([f530140](https://github.com/avivsinai/agent-message-queue/commit/f530140a30d7f99ff6372d2c0b60c62ac6b01f5f))
* **remote:** disable busy=queue and deliver=steer in v1 (B05/D1) ([da9d568](https://github.com/avivsinai/agent-message-queue/commit/da9d568f04c834ebfbe4a37a6e84cb03c3c88747))
* **remote:** interaction respond validates before persisting, refusal clears intent (B07) ([fec0b51](https://github.com/avivsinai/agent-message-queue/commit/fec0b51646a0743c90fa18e05c33f0f268ac9e2d))
* **remote:** native acks outside e.mu + explicit unlock discipline (B14a) ([f1156a9](https://github.com/avivsinai/agent-message-queue/commit/f1156a9ed658469a006ea8c750d8e7ba506008dc))
* **remote:** reconcile replays native ack of unacked terminal evidence (B06) ([049c18b](https://github.com/avivsinai/agent-message-queue/commit/049c18bd9df0afa1da1c0bb1e7dcaf99272d6c0d))

## [0.77.3](https://github.com/avivsinai/agent-message-queue/compare/v0.77.2...v0.77.3) (2026-09-05)


### Bug Fixes

* **cli:** tell agents a live wake makes watch and monitor redundant ([#718](https://github.com/avivsinai/agent-message-queue/issues/718)) ([2de1bae](https://github.com/avivsinai/agent-message-queue/commit/2de1baeaa4310c38483cf5cc089207ec4bacb173))
* **skills:** support Grok Bot local AMQ and wake-first waits ([#722](https://github.com/avivsinai/agent-message-queue/issues/722)) ([5b7ee45](https://github.com/avivsinai/agent-message-queue/commit/5b7ee45b6ee62ce6c7c7ba8e99220d7c20182bf8))

## [0.77.2](https://github.com/avivsinai/agent-message-queue/compare/v0.77.1...v0.77.2) (2026-09-05)


### Bug Fixes

* **coop:** recover stale reboot state and bind Codex naming to process ([#719](https://github.com/avivsinai/agent-message-queue/issues/719)) ([f247e37](https://github.com/avivsinai/agent-message-queue/commit/f247e3752c2cd5f9a1c9b5fa00976a765c242f2f))

## [0.77.1](https://github.com/avivsinai/agent-message-queue/compare/v0.77.0...v0.77.1) (2026-09-03)


### Bug Fixes

* **test:** isolate the test cwd from live queues and stop ledger appends recreating mailboxes ([#712](https://github.com/avivsinai/agent-message-queue/issues/712)) ([324ceb5](https://github.com/avivsinai/agent-message-queue/commit/324ceb52b4e422b56a901a08ace15f27aacf101c))
* **trace:** preserve contradictory ledger history through compaction ([#713](https://github.com/avivsinai/agent-message-queue/issues/713)) ([d4abe78](https://github.com/avivsinai/agent-message-queue/commit/d4abe78e3f2689b8a7171fb760069926732316c3))
* **wake:** re-arm deferred cohorts and type rotation-only ledger errors ([#710](https://github.com/avivsinai/agent-message-queue/issues/710)) ([a2b30f3](https://github.com/avivsinai/agent-message-queue/commit/a2b30f3a87348e5ee4aba88572b4347d2eadec5e))
* **wake:** retire a wake whose lock vanished during its graceful exit ([#715](https://github.com/avivsinai/agent-message-queue/issues/715)) ([92afe29](https://github.com/avivsinai/agent-message-queue/commit/92afe2997fb569cfdea14a05978536cc6a0fb925))

## [0.77.0](https://github.com/avivsinai/agent-message-queue/compare/v0.76.0...v0.77.0) (2026-09-02)


### Features

* **wake:** add durable notification-attempt ledger ([#700](https://github.com/avivsinai/agent-message-queue/issues/700)) ([3601cb6](https://github.com/avivsinai/agent-message-queue/commit/3601cb6881ca49c388ef41008f75634eb295e7f0))
* **wake:** add inject-via provider acceptance protocol ([#704](https://github.com/avivsinai/agent-message-queue/issues/704)) ([7d7490b](https://github.com/avivsinai/agent-message-queue/commit/7d7490b71648e82f56a75f7006c87550c89390eb))


### Bug Fixes

* **trace:** distinguish absent ledger from missing record ([#705](https://github.com/avivsinai/agent-message-queue/issues/705)) ([7a27195](https://github.com/avivsinai/agent-message-queue/commit/7a2719522cf927fb3b22106dcb5a788512cf0c8d))
* **wake:** bind the inbox parent directory identity through repair ([#698](https://github.com/avivsinai/agent-message-queue/issues/698)) ([7f96d4c](https://github.com/avivsinai/agent-message-queue/commit/7f96d4ce438ed40299d1ecd763451738845eed72))
* **wake:** close release-gate injector and ledger findings ([#706](https://github.com/avivsinai/agent-message-queue/issues/706)) ([c8837fb](https://github.com/avivsinai/agent-message-queue/commit/c8837fbc998b64216707a5e8cb525f56d4889a80))
* **wake:** protect a published successor during cleanup and stop ([#695](https://github.com/avivsinai/agent-message-queue/issues/695)) ([8605c49](https://github.com/avivsinai/agent-message-queue/commit/8605c49ed318072760afb209d98034858e307842))
* **wake:** record deferred interrupts and restore ledger write-failure evidence ([#709](https://github.com/avivsinai/agent-message-queue/issues/709)) ([2674059](https://github.com/avivsinai/agent-message-queue/commit/2674059388b0f285229b33e3ffcf4168ec3f95cf))

## [0.76.0](https://github.com/avivsinai/agent-message-queue/compare/v0.75.0...v0.76.0) (2026-08-31)


### Features

* **keepalive:** support native Windows agent adapters ([#697](https://github.com/avivsinai/agent-message-queue/issues/697)) ([51eabc9](https://github.com/avivsinai/agent-message-queue/commit/51eabc9600fe89d85546f823fadd8bb0fff2cff8))

## [0.75.0](https://github.com/avivsinai/agent-message-queue/compare/v0.74.1...v0.75.0) (2026-08-30)


### Features

* **bridge:** envelope v2 length-prefix and payload_b64 ([fe3ea1e](https://github.com/avivsinai/agent-message-queue/commit/fe3ea1e265794678d396ef2a296934b1037ddd92))


### Bug Fixes

* **bridge:** derive hop-probe ids and allow slash in thread_id ([fe3ea1e](https://github.com/avivsinai/agent-message-queue/commit/fe3ea1e265794678d396ef2a296934b1037ddd92))

## [0.74.1](https://github.com/avivsinai/agent-message-queue/compare/v0.74.0...v0.74.1) (2026-08-29)


### Bug Fixes

* **doctor:** find the codex skill where npx skills add installs it ([#688](https://github.com/avivsinai/agent-message-queue/issues/688)) ([7d10de5](https://github.com/avivsinai/agent-message-queue/commit/7d10de5c910dfe84a1e2a5169358b60b75209e7c))
* **launch:** retry tmux new-session once after server exit race ([#692](https://github.com/avivsinai/agent-message-queue/issues/692)) ([a51d19f](https://github.com/avivsinai/agent-message-queue/commit/a51d19ff62892fb135267c6e775a41d9bbf377eb))
* **wake:** self-upgrade after the running image path is unlinked ([#691](https://github.com/avivsinai/agent-message-queue/issues/691)) ([9314771](https://github.com/avivsinai/agent-message-queue/commit/9314771d07b46c94a467bd0c82a1379fde4de188))

## [0.74.0](https://github.com/avivsinai/agent-message-queue/compare/v0.73.0...v0.74.0) (2026-08-28)


### Features

* **keepalive:** re-exec a strictly newer supervisor image in place ([#672](https://github.com/avivsinai/agent-message-queue/issues/672)) ([dc15276](https://github.com/avivsinai/agent-message-queue/commit/dc15276d62bdb9077c82639c511a44817a7f1b7e))
* **selfupgrade:** crash-loop guard and darwin stage identity checks ([#677](https://github.com/avivsinai/agent-message-queue/issues/677)) ([3e7deca](https://github.com/avivsinai/agent-message-queue/commit/3e7deca27ca52df883a4382897662476c3981bd1))
* **upgrade:** package-manager-aware amq upgrade with companion --all ([#674](https://github.com/avivsinai/agent-message-queue/issues/674)) ([7eaec7f](https://github.com/avivsinai/agent-message-queue/commit/7eaec7f4e4d1b58fcf84c6f60c29f09fa9903869))


### Bug Fixes

* **bridge:** prefer GNU stat for enqueue config mode ([#686](https://github.com/avivsinai/agent-message-queue/issues/686)) ([71a2379](https://github.com/avivsinai/agent-message-queue/commit/71a2379719a4b91c345cb6a326a8232b4d766f9a))
* **cli:** assert physical canonical roots in env test ([#678](https://github.com/avivsinai/agent-message-queue/issues/678)) ([ecccfee](https://github.com/avivsinai/agent-message-queue/commit/ecccfee71f208381ac7fc49540e6db5883b44add))
* **cli:** isolate nested worktree root discovery ([#676](https://github.com/avivsinai/agent-message-queue/issues/676)) ([c7e73ae](https://github.com/avivsinai/agent-message-queue/commit/c7e73aed9239f81895f366a401bd0d6108332cab))
* **selfupgrade:** read candidate versions from build info ([#675](https://github.com/avivsinai/agent-message-queue/issues/675)) ([79cb14c](https://github.com/avivsinai/agent-message-queue/commit/79cb14c759df7262b199050149e4f4880d374cba))
* **update:** reject unpublished sentinel latest versions ([#685](https://github.com/avivsinai/agent-message-queue/issues/685)) ([cf0ada9](https://github.com/avivsinai/agent-message-queue/commit/cf0ada91809a54b101f7b36f19e37b4fec73b74a))
* **wake:** retry restart readiness when the record changes while opening ([#683](https://github.com/avivsinai/agent-message-queue/issues/683)) ([fc85bea](https://github.com/avivsinai/agent-message-queue/commit/fc85beaf9783510235d559769cb486074727810e))

## [0.73.0](https://github.com/avivsinai/agent-message-queue/compare/v0.72.0...v0.73.0) (2026-08-27)


### Features

* **launch:** name managed Claude sessions from the plan ([#668](https://github.com/avivsinai/agent-message-queue/issues/668)) ([c83e997](https://github.com/avivsinai/agent-message-queue/commit/c83e997e54add35ac6c666db08a5526fa0ce0193))


### Bug Fixes

* **coop:** unify the harness naming table with the launch provider set ([#666](https://github.com/avivsinai/agent-message-queue/issues/666)) ([a76c0e9](https://github.com/avivsinai/agent-message-queue/commit/a76c0e9aabd798867ea496ec09d463ccf08f6320))

## [0.72.0](https://github.com/avivsinai/agent-message-queue/compare/v0.71.0...v0.72.0) (2026-08-26)


### Features

* **coop:** name harness sessions &lt;session&gt;/&lt;handle&gt; by default ([#664](https://github.com/avivsinai/agent-message-queue/issues/664)) ([b809591](https://github.com/avivsinai/agent-message-queue/commit/b8095914b3e2603e1a79a47986657c40c2884cef))
* **launch:** honor project-root authority and adopt squad argv grammar ([#663](https://github.com/avivsinai/agent-message-queue/issues/663)) ([7421b8e](https://github.com/avivsinai/agent-message-queue/commit/7421b8ec77eeeed99746450786315a1e83f2cc0e))


### Bug Fixes

* **coop:** refuse --named under a managed launch ([#659](https://github.com/avivsinai/agent-message-queue/issues/659)) ([4280e7f](https://github.com/avivsinai/agent-message-queue/commit/4280e7f2d3932a1dafa028e93d89c207e9ccadda))
* **launch:** classify an exited tmux server as a missing target ([#665](https://github.com/avivsinai/agent-message-queue/issues/665)) ([8e24caa](https://github.com/avivsinai/agent-message-queue/commit/8e24caa59f78ae154f265952290b71849c36e6b1))
* **wake:** tolerate ctime-only staging races and supersede refused restart records ([#654](https://github.com/avivsinai/agent-message-queue/issues/654)) ([e7b5144](https://github.com/avivsinai/agent-message-queue/commit/e7b5144fc613abe2bb06acbd5fb5e4fb2642d713))

## [0.71.0](https://github.com/avivsinai/agent-message-queue/compare/v0.70.0...v0.71.0) (2026-08-26)


### Features

* **keepalive:** add claude-print submitted seat for existing Claude sessions ([#653](https://github.com/avivsinai/agent-message-queue/issues/653)) ([b1c7455](https://github.com/avivsinai/agent-message-queue/commit/b1c745560e1cc64ecc011b2f3692161e99936061))
* **keepalive:** add codex-queue submitted seat for live Codex threads ([#651](https://github.com/avivsinai/agent-message-queue/issues/651)) ([7a7fbc6](https://github.com/avivsinai/agent-message-queue/commit/7a7fbc6ce9b7218b59dffa117c27590e2f13569b))


### Bug Fixes

* **coop:** detect named inject TTY via /dev/tty ([#649](https://github.com/avivsinai/agent-message-queue/issues/649)) ([a32c500](https://github.com/avivsinai/agent-message-queue/commit/a32c5006b9c528912babc124c3d496cc0515f69c))
* **keepalive:** isolate hookinstall tests from HOME and self-heal stale hooks ([#652](https://github.com/avivsinai/agent-message-queue/issues/652)) ([5340ad0](https://github.com/avivsinai/agent-message-queue/commit/5340ad018a20c1d390c8276c061d33b06726657b))

## [0.70.0](https://github.com/avivsinai/agent-message-queue/compare/v0.69.0...v0.70.0) (2026-08-25)


### Features

* **wake:** add gated GUI app seat scaffolds ([#644](https://github.com/avivsinai/agent-message-queue/issues/644)) ([30d0180](https://github.com/avivsinai/agent-message-queue/commit/30d018035656902810ddb9c1897ee3ef23e4022b))
* **wake:** ship gated GUI deep-link prefill seats ([#647](https://github.com/avivsinai/agent-message-queue/issues/647)) ([108211c](https://github.com/avivsinai/agent-message-queue/commit/108211ce2dcc72207711f53fcfe1373bf17d4616))

## [0.69.0](https://github.com/avivsinai/agent-message-queue/compare/v0.68.0...v0.69.0) (2026-08-24)


### Features

* **coop:** add opt-in --named flag for CLI session naming ([#642](https://github.com/avivsinai/agent-message-queue/issues/642)) ([d860af6](https://github.com/avivsinai/agent-message-queue/commit/d860af6bc5a894bea915388f82cdf29ab7d93e8d))

## [0.68.0](https://github.com/avivsinai/agent-message-queue/compare/v0.67.2...v0.68.0) (2026-08-22)


### Features

* **acp:** add Buzz remote-task safety to amq-acp ([#637](https://github.com/avivsinai/agent-message-queue/issues/637)) ([cb8b011](https://github.com/avivsinai/agent-message-queue/commit/cb8b0113bc1ab37aaf5a0c7e8c57370d6228aa27))

## [0.67.2](https://github.com/avivsinai/agent-message-queue/compare/v0.67.1...v0.67.2) (2026-08-22)


### Bug Fixes

* **wake:** self-upgrade after Homebrew Cellar unlink ([#634](https://github.com/avivsinai/agent-message-queue/issues/634)) ([8094f26](https://github.com/avivsinai/agent-message-queue/commit/8094f26cd35241e58e0a8e3fe79c41428c8ee6f7))

## [0.67.1](https://github.com/avivsinai/agent-message-queue/compare/v0.67.0...v0.67.1) (2026-08-22)


### Bug Fixes

* **acp:** accept v1 baseline resource_link in session/prompt ([#631](https://github.com/avivsinai/agent-message-queue/issues/631)) ([2d78fb6](https://github.com/avivsinai/agent-message-queue/commit/2d78fb667986cef6a09c640a9fc8c8c7e201d968))

## [0.67.0](https://github.com/avivsinai/agent-message-queue/compare/v0.66.1...v0.67.0) (2026-08-22)


### Features

* **acp:** publish amq-acp asset and Buzz BYOH harness ([#629](https://github.com/avivsinai/agent-message-queue/issues/629)) ([4ffbaed](https://github.com/avivsinai/agent-message-queue/commit/4ffbaed70714f9e3c53350cd3294a2090d0915a3))

## [0.66.1](https://github.com/avivsinai/agent-message-queue/compare/v0.66.0...v0.66.1) (2026-08-21)


### Bug Fixes

* **bridge:** keep opaque thread id on Bot hop replies ([#626](https://github.com/avivsinai/agent-message-queue/issues/626)) ([81e8266](https://github.com/avivsinai/agent-message-queue/commit/81e82663556673359d3de7ef64acab41b21d3bd4))

## [0.66.0](https://github.com/avivsinai/agent-message-queue/compare/v0.65.3...v0.66.0) (2026-08-21)


### Features

* **acp:** add truthful ACP v1 stdio companion ([adbf598](https://github.com/avivsinai/agent-message-queue/commit/adbf5985709e431ce6ed0804608e3854ed8b468c))
* **bridge:** add amq-bridge companion courier with Ed25519 host auth ([adbf598](https://github.com/avivsinai/agent-message-queue/commit/adbf5985709e431ce6ed0804608e3854ed8b468c))
* **bridge:** apply Bot-dropped envelopes into local Maildir ([#624](https://github.com/avivsinai/agent-message-queue/issues/624)) ([a1a2767](https://github.com/avivsinai/agent-message-queue/commit/a1a2767d08abfea368f7d526fd5d818419efd906))
* **cli:** treat Grok Build as a first-class skill peer ([#623](https://github.com/avivsinai/agent-message-queue/issues/623)) ([d3707ba](https://github.com/avivsinai/agent-message-queue/commit/d3707baa458f068fc8e93b15d5b6b17e3ec6ed12))

## [0.65.3](https://github.com/avivsinai/agent-message-queue/compare/v0.65.2...v0.65.3) (2026-08-19)


### Bug Fixes

* **cli:** bootstrap absent coop roots fail-closed with exclusive nofollow creation ([#613](https://github.com/avivsinai/agent-message-queue/issues/613)) ([6b98e87](https://github.com/avivsinai/agent-message-queue/commit/6b98e877fa97c832d95bd2b11de5132262e65138))
* **fsq:** make windows publication and dlq moves no-replace ([#614](https://github.com/avivsinai/agent-message-queue/issues/614)) ([d0516d6](https://github.com/avivsinai/agent-message-queue/commit/d0516d618385ea2e18225e3335bfd207bde4539c))
* **fsq:** no-replace publication and claim on unix with unique tmp attempts and DLQ byte-compare recovery ([#604](https://github.com/avivsinai/agent-message-queue/issues/604)) ([872f395](https://github.com/avivsinai/agent-message-queue/commit/872f395dcc2e79fcc8429860cbd6ab8bd189bf0c))
* **fsq:** validate mailbox entrypoints, keep destinations on rename failure, fail closed on DLQ id RNG error ([#588](https://github.com/avivsinai/agent-message-queue/issues/588)) ([a168ba2](https://github.com/avivsinai/agent-message-queue/commit/a168ba2eb69da20440295854662d698af8db327f))
* **keepalive:** fail closed on cmux liveness and persist tty ownership across supervisor restarts ([#594](https://github.com/avivsinai/agent-message-queue/issues/594)) ([e228470](https://github.com/avivsinai/agent-message-queue/commit/e2284709ce6237ee18ca49288decb59ad90fe58e))
* **keepalive:** fail closed on hook config rewrites and launchd plist identity ([#609](https://github.com/avivsinai/agent-message-queue/issues/609)) ([0ba702e](https://github.com/avivsinai/agent-message-queue/commit/0ba702e54f610e0a8052762550821f7c82d3f4fe))
* **keepalive:** give the gc serialization test fixture a 0700 registry dir ([#615](https://github.com/avivsinai/agent-message-queue/issues/615)) ([f44fef8](https://github.com/avivsinai/agent-message-queue/commit/f44fef8fce1b8ad5a6d5e84fc90af5df800a4d71))
* **keepalive:** harden registry identity, locks, and canonicalization ([#611](https://github.com/avivsinai/agent-message-queue/issues/611)) ([f275472](https://github.com/avivsinai/agent-message-queue/commit/f2754722bca5dadce47b1b904a39914767a81ee9))
* **keepalive:** open file adapter targets nofollow/nonblock and refuse non-regular files ([#590](https://github.com/avivsinai/agent-message-queue/issues/590)) ([f464b3a](https://github.com/avivsinai/agent-message-queue/commit/f464b3aa126309239d02a0362e28edf54c291241))
* **keepalive:** run gc under the registration lock and parse ready markers strictly ([#612](https://github.com/avivsinai/agent-message-queue/issues/612)) ([da3aeee](https://github.com/avivsinai/agent-message-queue/commit/da3aeee311dbec42850321117c55e9ba597d2ef4))
* **keepalive:** use a private exclusive timeout marker and process-group termination in the session hook ([#589](https://github.com/avivsinai/agent-message-queue/issues/589)) ([715e364](https://github.com/avivsinai/agent-message-queue/commit/715e3640c97683980349df0cdab8f2503fbeaf27))
* **launchapi:** advertise only platform-callable compatibility features ([#593](https://github.com/avivsinai/agent-message-queue/issues/593)) ([5f29ec3](https://github.com/avivsinai/agent-message-queue/commit/5f29ec3127ddff6a381e904c88a6b7875c94df2b))
* **launchapi:** reject duplicate JSON keys at every depth with a bounded typed strict decoder ([#595](https://github.com/avivsinai/agent-message-queue/issues/595)) ([da2720d](https://github.com/avivsinai/agent-message-queue/commit/da2720dc98b6dfbac65133f7a820970dcbd5515f))
* **launch:** bind apply authorization and reconcile to the authorized target and identities ([#596](https://github.com/avivsinai/agent-message-queue/issues/596)) ([94e2112](https://github.com/avivsinai/agent-message-queue/commit/94e2112c43934a790316545d019464f95d058d6a))
* **launch:** check raw and resolved paths for amq executable containment ([#605](https://github.com/avivsinai/agent-message-queue/issues/605)) ([5363654](https://github.com/avivsinai/agent-message-queue/commit/53636540eb323a24a4b576d07d359ff0567ef414))
* **launch:** close only exact acknowledged backend resources and reconstruct full reclaim identity ([#606](https://github.com/avivsinai/agent-message-queue/issues/606)) ([8a4b2e1](https://github.com/avivsinai/agent-message-queue/commit/8a4b2e1f4127a7f4b3815e3c2a7b4c9aeb41b017))
* **launch:** contain wrappers and the amq launcher to non-project paths and validate before capabilities ([#592](https://github.com/avivsinai/agent-message-queue/issues/592)) ([a028344](https://github.com/avivsinai/agent-message-queue/commit/a0283441d1aed3cd6e1c554e046f5e66cb51f54b))
* **launch:** journal managed tmux joins and always clean tickets on definite pre-create failure ([#600](https://github.com/avivsinai/agent-message-queue/issues/600)) ([3fab628](https://github.com/avivsinai/agent-message-queue/commit/3fab628714cfcb6dbf817fd5b509966ac952fede))
* **launch:** keep mint conversations pending until provider start is acknowledged ([#602](https://github.com/avivsinai/agent-message-queue/issues/602)) ([2a187c8](https://github.com/avivsinai/agent-message-queue/commit/2a187c848fae5da6dff1e91e3f0c120797508ae9))
* **launch:** make execution policy trust-bearing and reject unsafe initial input ([#587](https://github.com/avivsinai/agent-message-queue/issues/587)) ([40fa964](https://github.com/avivsinai/agent-message-queue/commit/40fa964abf34fa68af6278bc1abbdcd8474ef64d))
* **launch:** make lease release retryable and notify/revert lease-bounded ([#585](https://github.com/avivsinai/agent-message-queue/issues/585)) ([71bef84](https://github.com/avivsinai/agent-message-queue/commit/71bef84ac8708a020978d0809847fb7cb02b8bf1))
* **launch:** persist exact ghostty candidates and refuse changed recovery markers ([#601](https://github.com/avivsinai/agent-message-queue/issues/601)) ([13f91cf](https://github.com/avivsinai/agent-message-queue/commit/13f91cf1de01fca8c94fa7792ee7eb09cd143a46))
* **launch:** refuse undecided apply, map unsupported create, and require exact journal identities ([#591](https://github.com/avivsinai/agent-message-queue/issues/591)) ([1efb5e8](https://github.com/avivsinai/agent-message-queue/commit/1efb5e8e163c0798229fec2eaacc94ba362fd372))
* **launch:** report committed/uncertain mutation dispositions for apply and lifecycle ([#598](https://github.com/avivsinai/agent-message-queue/issues/598)) ([d20ac7f](https://github.com/avivsinai/agent-message-queue/commit/d20ac7fe90934681640dcfbf659e18127b7e5430))
* **launch:** require reprepare for schema-1 apply with runnable participants ([#603](https://github.com/avivsinai/agent-message-queue/issues/603)) ([3a56076](https://github.com/avivsinai/agent-message-queue/commit/3a56076f71c620134b06a20c1418f549eaf153f3))
* **launch:** return a coherent snapshot when the lifecycle binding changes during inspect ([#597](https://github.com/avivsinai/agent-message-queue/issues/597)) ([5d316aa](https://github.com/avivsinai/agent-message-queue/commit/5d316aaeb34c4cbb0d2867e9ce50e31d8a40a3f6))
* **setup:** default the Cursor command to agent with cursor-agent as legacy alias ([#599](https://github.com/avivsinai/agent-message-queue/issues/599)) ([8f4144c](https://github.com/avivsinai/agent-message-queue/commit/8f4144ccbb45e50e75db02a21a2f0b18e53ab86d))
* **wake:** generation CAS for retire and fail-closed darwin signaling ([#616](https://github.com/avivsinai/agent-message-queue/issues/616)) ([10ee1ec](https://github.com/avivsinai/agent-message-queue/commit/10ee1eca7c60761c56c85fb9995645b09b954f5b))
* **wake:** report uncertain terminal progress after text lands instead of replaying ([#607](https://github.com/avivsinai/agent-message-queue/issues/607)) ([7ae3acf](https://github.com/avivsinai/agent-message-queue/commit/7ae3acf8209c35da770de6daba24a69424e569ac))
* **wake:** run restart preflight outside the lifecycle guard with CAS republication ([#610](https://github.com/avivsinai/agent-message-queue/issues/610)) ([47ebb27](https://github.com/avivsinai/agent-message-queue/commit/47ebb27781a1b736e48c39e66e54025e4a7acccf))

## [0.65.2](https://github.com/avivsinai/agent-message-queue/compare/v0.65.1...v0.65.2) (2026-08-18)


### Bug Fixes

* **keepalive:** bound session-start watchdog by a sleep seam, not wall clock ([#582](https://github.com/avivsinai/agent-message-queue/issues/582)) ([38595d0](https://github.com/avivsinai/agent-message-queue/commit/38595d0f3ae7bb2a77753b64965186a21f07e02f))
* **keepalive:** make reattach timing deterministic via seams ([#584](https://github.com/avivsinai/agent-message-queue/issues/584)) ([c68a135](https://github.com/avivsinai/agent-message-queue/commit/c68a135d7e71a62098d1b8ad429a269a84fde666))

## [0.65.1](https://github.com/avivsinai/agent-message-queue/compare/v0.65.0...v0.65.1) (2026-08-18)


### Bug Fixes

* **launch:** refuse --apply with --prepare before requiring --plan ([#580](https://github.com/avivsinai/agent-message-queue/issues/580)) ([2e419cb](https://github.com/avivsinai/agent-message-queue/commit/2e419cb09f4bfa9842cd1bfd66eee0b4ba1ddb2a))

## [0.65.0](https://github.com/avivsinai/agent-message-queue/compare/v0.64.1...v0.65.0) (2026-08-18)


### Features

* **wake:** classify a stale wake whose binary directory is gone ([#575](https://github.com/avivsinai/agent-message-queue/issues/575)) ([64408e4](https://github.com/avivsinai/agent-message-queue/commit/64408e4d55188029225cab16e2ec2590347bb2ed))
* **wake:** stage darwin restart images under the machine-local state dir ([#577](https://github.com/avivsinai/agent-message-queue/issues/577)) ([2d599e8](https://github.com/avivsinai/agent-message-queue/commit/2d599e8fb3c09adb74a5e98d56c4f8b4095cde8b))


### Bug Fixes

* **launch:** bind wrapper identity into tickets and harden strict decode ([#578](https://github.com/avivsinai/agent-message-queue/issues/578)) ([fe23bc4](https://github.com/avivsinai/agent-message-queue/commit/fe23bc4254f83cf37927a62df28744ee1b8dc1f9))

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
