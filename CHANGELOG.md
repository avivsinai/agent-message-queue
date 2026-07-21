# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Release Please generates new entries from conventional squash commits on
`main`; richer or multi-entry notes can be added through commit overrides or by
editing the release PR.

## [0.46.0](https://github.com/avivsinai/agent-message-queue/compare/v0.45.0...v0.46.0) (2026-07-21)


### Features

* /spec slash command for collaborative spec workflow ([f5d7ce0](https://github.com/avivsinai/agent-message-queue/commit/f5d7ce0a68807b949b59e7923c72d787496defbc))
* add --session flag and shell-setup command for native co-op aliases ([7be3b3a](https://github.com/avivsinai/agent-message-queue/commit/7be3b3a1fa9d0f4593f11160623301f35557c8ab))
* add /amq-spec slash command for collaborative spec workflow ([a677b56](https://github.com/avivsinai/agent-message-queue/commit/a677b56b100888cf18dd3b3c9bf94c5df126cb17))
* add amq coop spec collaborative specification workflow ([f86d0c0](https://github.com/avivsinai/agent-message-queue/commit/f86d0c0bf2160837a0bf8b007a569f44d7fef144))
* add amq env command for streamlined session setup ([#8](https://github.com/avivsinai/agent-message-queue/issues/8)) ([ff191cd](https://github.com/avivsinai/agent-message-queue/commit/ff191cdc852122f6fdd4dda1879f93795d699418))
* add amq upgrade command and update notifier ([8fc0d5f](https://github.com/avivsinai/agent-message-queue/commit/8fc0d5f66488c90dd64ea0f3d1203a2a52ba4fc0))
* add amq upgrade command and update notifier ([171dd83](https://github.com/avivsinai/agent-message-queue/commit/171dd837fb9552ffcab33ce7fb8326767549a2dc))
* add amq wake command for TIOCSTI-based agent notifications ([c53a614](https://github.com/avivsinai/agent-message-queue/commit/c53a614199a0ae3140852aa0cb0982adc151bd49))
* add amq wake command with TIOCSTI terminal injection ([763afb6](https://github.com/avivsinai/agent-message-queue/commit/763afb6ed9cafe482e02f49edd7e630e1bd2f944))
* add co-op setup script, stop hook, and autonomous operation docs ([51c1a58](https://github.com/avivsinai/agent-message-queue/commit/51c1a58450d4a98be3ba46188662ad6772da0605))
* add Codex interface metadata to plugin manifest ([4db7856](https://github.com/avivsinai/agent-message-queue/commit/4db785607292bd029cd62ae862290438e26dfd92))
* add collaborative spec workflow (amq coop spec) ([76ad42a](https://github.com/avivsinai/agent-message-queue/commit/76ad42a67faeeb1157594536965f577a12e2c69f))
* add coop start, doctor commands and install.sh --skill ([458205f](https://github.com/avivsinai/agent-message-queue/commit/458205f1531ea9c2a5cbc333f225986ccc0dba93))
* add dead letter queue for failed messages ([a95892b](https://github.com/avivsinai/agent-message-queue/commit/a95892bc650c14895e0a38082853c6b15efdf04e))
* add evidence, failed, and blocked states to swarm tasks ([#36](https://github.com/avivsinai/agent-message-queue/issues/36)) ([b848cdd](https://github.com/avivsinai/agent-message-queue/commit/b848cdd66dee71c47e83e4b843187e4e6d9d7d79))
* add initiator protocol and wake interrupts ([#18](https://github.com/avivsinai/agent-message-queue/issues/18)) ([bb57a8d](https://github.com/avivsinai/agent-message-queue/commit/bb57a8d8f33dcdfddfebcf1c1259f5830afa1d5f))
* add message filtering flags to list command ([a20b2de](https://github.com/avivsinai/agent-message-queue/commit/a20b2dea16a295b629b43de366f43452eb520618))
* add shell completion command (bash, zsh, fish) ([8887e2c](https://github.com/avivsinai/agent-message-queue/commit/8887e2c8697f79bf4b37404442f2bc77872a3864))
* add shell completions (bash/zsh/fish) ([2c91272](https://github.com/avivsinai/agent-message-queue/commit/2c91272fdb6b88d37dcf0e23c5abd51d538ba792))
* agent retains coop awareness after /clear ([#84](https://github.com/avivsinai/agent-message-queue/issues/84)) ([a3061e4](https://github.com/avivsinai/agent-message-queue/commit/a3061e43adc9ee0f444349e842c40cc1810f6a4b))
* **ci:** tag-based skill publishing with release-skills script ([d001c2a](https://github.com/avivsinai/agent-message-queue/commit/d001c2a5a91d52a2c0d5c0eb6bdff0d9e49e42db))
* CLI-guided spec workflow with NEXT STEP output ([64c9869](https://github.com/avivsinai/agent-message-queue/commit/64c98692b6d15cacb007594977dd8a39ec65d5d3))
* **cli:** add Grok coop support ([#240](https://github.com/avivsinai/agent-message-queue/issues/240)) ([fb024f2](https://github.com/avivsinai/agent-message-queue/commit/fb024f2c5aec3fe6dad4d53779125af21d1faa59)), closes [#218](https://github.com/avivsinai/agent-message-queue/issues/218)
* **cli:** tri-state tree identity, physical pins, and authority de-laundering ([#252](https://github.com/avivsinai/agent-message-queue/issues/252)) ([6ef0e6d](https://github.com/avivsinai/agent-message-queue/commit/6ef0e6df1177ccf6009a04bf9359f723ea9ea0d3))
* **coop:** add --no-gitignore flag to coop init ([#173](https://github.com/avivsinai/agent-message-queue/issues/173)) ([4337154](https://github.com/avivsinai/agent-message-queue/commit/4337154a2d99219f8c48589a694329dce9e28796)), closes [#172](https://github.com/avivsinai/agent-message-queue/issues/172)
* **coop:** add `coop exec` command, remove shell/start, .amqrc in defaultRoot ([04419e9](https://github.com/avivsinai/agent-message-queue/commit/04419e940f41c6b8a638602e45ae30a67f3bf3eb))
* **coop:** add amq coop command for simplified onboarding ([1723d45](https://github.com/avivsinai/agent-message-queue/commit/1723d4582c1676646127ce527087b60bc8d54690))
* **coop:** add coop exec, remove shell/start, .amqrc in defaultRoot ([66f0bfa](https://github.com/avivsinai/agent-message-queue/commit/66f0bfa481e3d51c780871d8bde6d7b69463fdc6))
* **coop:** introduce phased parallel work model ([#15](https://github.com/avivsinai/agent-message-queue/issues/15)) ([2d832fd](https://github.com/avivsinai/agent-message-queue/commit/2d832fdde9e612c56379d05e42cd48915b99ea59))
* cross-orchestrator integrations (Symphony, Kanban, doctor --ops) ([#47](https://github.com/avivsinai/agent-message-queue/issues/47)) ([9cf4ac2](https://github.com/avivsinai/agent-message-queue/commit/9cf4ac21136c94bcb7cc879fd4abaaca51d43157))
* cross-project messaging via peers in .amqrc ([#44](https://github.com/avivsinai/agent-message-queue/issues/44)) ([891e1b1](https://github.com/avivsinai/agent-message-queue/commit/891e1b165e387d852bdfae09be555da5ec509606))
* cross-session messaging (--session flag) ([#40](https://github.com/avivsinai/agent-message-queue/issues/40)) ([536b92b](https://github.com/avivsinai/agent-message-queue/commit/536b92b6135941ecd66bf21e8222898599ce4f90))
* default coop exec to --session collab ([3a1be72](https://github.com/avivsinai/agent-message-queue/commit/3a1be7202d839870c33051ca2e047874a8ef271d))
* **doctor:** detect mailbox divergence across worktrees ([#228](https://github.com/avivsinai/agent-message-queue/issues/228)) ([db99e78](https://github.com/avivsinai/agent-message-queue/commit/db99e78f7fccd414dccc5199fde8ac3685ae9a58)), closes [#207](https://github.com/avivsinai/agent-message-queue/issues/207)
* **doctor:** report extension manifests ([#110](https://github.com/avivsinai/agent-message-queue/issues/110)) ([6b8c74b](https://github.com/avivsinai/agent-message-queue/commit/6b8c74bc15bb147f8296e49374d95c3070244fb9))
* embed shell aliases natively, base+session layout ([#27](https://github.com/avivsinai/agent-message-queue/issues/27)) ([5030372](https://github.com/avivsinai/agent-message-queue/commit/5030372962bdfca6eb8d2fbea7de1e29c9b8ffff))
* **env:** add --session-name flag and session_name to --json output ([6e2a308](https://github.com/avivsinai/agent-message-queue/commit/6e2a30809fcdfe4867a526483cdf4b7fbca902aa))
* **env:** add json v1 contract ([#109](https://github.com/avivsinai/agent-message-queue/issues/109)) ([f42fd16](https://github.com/avivsinai/agent-message-queue/commit/f42fd165ee6d59704a531c198d1d634cbadec06b))
* expose project/peers in env --json, optimize skill descriptions ([b005f3a](https://github.com/avivsinai/agent-message-queue/commit/b005f3aeb888a026a3256b54c2ab47f7fb11f910))
* **fsq:** fd-confined message delivery via os.Root capability ([#257](https://github.com/avivsinai/agent-message-queue/issues/257)) ([059a076](https://github.com/avivsinai/agent-message-queue/commit/059a0763b66b8b9f848a10fc46352376fa43ecf0))
* implement co-op mode for inter-agent collaboration ([b92a5ba](https://github.com/avivsinai/agent-message-queue/commit/b92a5baf6325fc3e94c585183fb2a0e29f88fc77))
* include session name in AMQ notifications ([#67](https://github.com/avivsinai/agent-message-queue/issues/67)) ([b332937](https://github.com/avivsinai/agent-message-queue/commit/b332937102ea06f7fcacb8059f29105202ed347e))
* OSS hardening - pinned Actions, govulncheck, Dependabot, checksum verification ([4d3e4de](https://github.com/avivsinai/agent-message-queue/commit/4d3e4de384639c61dd13ca3c99575f662f504bb4))
* OSS release preparation - docs, security, and CLI improvements ([0652461](https://github.com/avivsinai/agent-message-queue/commit/0652461bd5d6df258fba5c5ad5a9dbaefa33a337))
* **presence:** distinguish notifier liveness from activity ([#230](https://github.com/avivsinai/agent-message-queue/issues/230)) ([a8d9b8f](https://github.com/avivsinai/agent-message-queue/commit/a8d9b8fd5a9cace05252fe5882e2a092cb0d15f8)), closes [#206](https://github.com/avivsinai/agent-message-queue/issues/206)
* replace ack with receipt ledger for delivery confirmation ([#72](https://github.com/avivsinai/agent-message-queue/issues/72)) ([5279444](https://github.com/avivsinai/agent-message-queue/commit/52794441255c6bfe8222c8d0c61185bd27e77cf9))
* require explicit AM_ROOT for message-routing commands ([10243dc](https://github.com/avivsinai/agent-message-queue/commit/10243dcc4e6a706c4676a73d46c94ed99e95dd0b))
* **route:** explain send routing ([#111](https://github.com/avivsinai/agent-message-queue/issues/111)) ([0f2ce46](https://github.com/avivsinai/agent-message-queue/commit/0f2ce46a1ba6afdf4d8f5238f33aa3ed4461d14f))
* **send:** support from-session routing ([#112](https://github.com/avivsinai/agent-message-queue/issues/112)) ([af7bae2](https://github.com/avivsinai/agent-message-queue/commit/af7bae21776992836c284ffd2c9e473400d4c0a0))
* session management + transport improvements ([2388301](https://github.com/avivsinai/agent-message-queue/commit/23883014be11c835c628b433692822d8384eb3c4))
* session-aware routing in skill, wake presence heartbeat, statusline docs ([694e693](https://github.com/avivsinai/agent-message-queue/commit/694e693373f069b4ca2c39e86e3cd8bd072fb85a))
* session-aware routing, wake presence heartbeat, statusline docs ([d085af5](https://github.com/avivsinai/agent-message-queue/commit/d085af5154e4582110ce90769f9af340cdec9782))
* **swarm:** Agent Teams integration + full codebase review fixes ([#19](https://github.com/avivsinai/agent-message-queue/issues/19)) ([186e05a](https://github.com/avivsinai/agent-message-queue/commit/186e05ac5041d5076a46a5f7b83ba98f35a18b17))
* **wake:** add --inject-via for external injection commands ([#99](https://github.com/avivsinai/agent-message-queue/issues/99)) ([5e8b82e](https://github.com/avivsinai/agent-message-queue/commit/5e8b82e4e7ac43eb26b14d1e49570faf482d889b))
* **wake:** add amq wake retire for identity-safe inject-via stop ([#256](https://github.com/avivsinai/agent-message-queue/issues/256)) ([a57dba9](https://github.com/avivsinai/agent-message-queue/commit/a57dba96f44ef05794275ea5abfc28f5d1c78ebc))
* **wake:** add zero-input notification mode ([#221](https://github.com/avivsinai/agent-message-queue/issues/221)) ([3d3376f](https://github.com/avivsinai/agent-message-queue/commit/3d3376faa603829580418bec044a79064e36af81))
* **wake:** Darwin cooperative unix-socket shutdown for inject-via wakes ([#254](https://github.com/avivsinai/agent-message-queue/issues/254)) ([6d66a20](https://github.com/avivsinai/agent-message-queue/commit/6d66a205236ef205e6a8dc76117ee736fc021ba3))
* **wake:** Linux pidfd wake termination capability ([#251](https://github.com/avivsinai/agent-message-queue/issues/251)) ([e528d76](https://github.com/avivsinai/agent-message-queue/commit/e528d766e375c084df832fa1164209633f5c7e3f))
* **wake:** refuse live raw Darwin wake orphans instead of bare-PID kill ([#253](https://github.com/avivsinai/agent-message-queue/issues/253)) ([77ac2e5](https://github.com/avivsinai/agent-message-queue/commit/77ac2e51d995f9dfef998bdf969431fbd5858c1d))
* **wake:** serialize wake lifecycle under permanent guard with generation-bound readiness ([#250](https://github.com/avivsinai/agent-message-queue/issues/250)) ([ca4cdb4](https://github.com/avivsinai/agent-message-queue/commit/ca4cdb420dac98c499563061f2bdffdd50739acc))
* wire command registry into CLI dispatcher and help system ([cc39682](https://github.com/avivsinai/agent-message-queue/commit/cc3968237d5436e852452dd28805baa5687b5f6e))
* wire command registry into help system ([ea891ba](https://github.com/avivsinai/agent-message-queue/commit/ea891baeb2e2fb69c723966dcc11f9ed0821ea7d))


### Bug Fixes

* add --inject-mode flag to wake for Claude Code compatibility ([05fb76d](https://github.com/avivsinai/agent-message-queue/commit/05fb76dea8dec8c4f8547cb9e670e22fb1665a58))
* add .amqrc to gitignore during coop init ([8ab26ed](https://github.com/avivsinai/agent-message-queue/commit/8ab26edd63490231c0535c19b5cd4a6e2be54a76))
* add AM_SESSION env var and enforce session-scoped communication ([1174728](https://github.com/avivsinai/agent-message-queue/commit/117472856e889df9b09ef37b506a5978c4a69976))
* add mandatory read gate for workflow reference files ([757ce4f](https://github.com/avivsinai/agent-message-queue/commit/757ce4fd5fe2aa8186e78bc2b6314413d1806167))
* add version string injection via ldflags and module info ([b13b9c8](https://github.com/avivsinai/agent-message-queue/commit/b13b9c8b8f2c2726596167e0922fad703da4a637))
* address code review feedback from Codex ([9dfe0e5](https://github.com/avivsinai/agent-message-queue/commit/9dfe0e5086faf2ba9267e056b083f9d5cb2185a7))
* address code review findings for co-op mode ([7230d44](https://github.com/avivsinai/agent-message-queue/commit/7230d44b395124f106590920d77743c4a02e0cb3))
* address Codex code review findings for co-op mode ([7bea9eb](https://github.com/avivsinai/agent-message-queue/commit/7bea9ebc2e8775c97353b44fc25425d7a002ca00))
* address codex review — topic path traversal, start race, pycache ([8274264](https://github.com/avivsinai/agent-message-queue/commit/82742641c1d8b8a6ad688f03dd8d6765a921ea15))
* address Codex review feedback on install script ([c0b1fdc](https://github.com/avivsinai/agent-message-queue/commit/c0b1fdcc7db05859ad3efdc61382541c25257021))
* address PR [#27](https://github.com/avivsinai/agent-message-queue/issues/27) review findings ([1da468d](https://github.com/avivsinai/agent-message-queue/commit/1da468d93f405405e5bafee1f929b34daad47d0b))
* address remaining code review findings ([54d9405](https://github.com/avivsinai/agent-message-queue/commit/54d9405530b6d960256be8c9f9b0b8594c9fb09a))
* address remaining PR [#29](https://github.com/avivsinai/agent-message-queue/issues/29) review issues ([f941c67](https://github.com/avivsinai/agent-message-queue/commit/f941c67e46511bbe700a196aa5f9ee8d8bc9e592))
* address review findings for base+session layout ([16cfc9b](https://github.com/avivsinai/agent-message-queue/commit/16cfc9bceec0a233323e5c12c2811959bfba7911))
* amq wake examples require --me flag ([e5623e9](https://github.com/avivsinai/agent-message-queue/commit/e5623e942ae8c2e8e0448c3db4a4bc9a373c9dee))
* apply Codex review feedback - ASCII arrows, clarify watch vs monitor ([dd49140](https://github.com/avivsinai/agent-message-queue/commit/dd49140d90f7c826f36309167eafb9eb221d0e87))
* auto-create .gitignore with agent-mail directory ([3ef482a](https://github.com/avivsinai/agent-message-queue/commit/3ef482a4f570f779ce29919aa3ac3c2c26657dfa))
* auto-update presence on send, drain, and reply ([#45](https://github.com/avivsinai/agent-message-queue/issues/45)) ([654cd57](https://github.com/avivsinai/agent-message-queue/commit/654cd57be85a75fd151795ea219dbc1b79fd5d25))
* bump Go to 1.25.8 for govulncheck (GO-2026-4602, GO-2026-4601) ([93ed84b](https://github.com/avivsinai/agent-message-queue/commit/93ed84b8abf49f3ce5378af5e8760d6026ccf9c8))
* canonicalize wake test temp dirs ([#194](https://github.com/avivsinai/agent-message-queue/issues/194)) ([534d4cd](https://github.com/avivsinai/agent-message-queue/commit/534d4cdaa270203adb667ba4d46ec9fa38495bb4))
* check tty.Close error for linter ([568cc18](https://github.com/avivsinai/agent-message-queue/commit/568cc18b4714a0ec43291d8018d725276f24ba2f))
* **ci:** add HOMEBREW_TAP_GITHUB_TOKEN for goreleaser brew push ([b74fef5](https://github.com/avivsinai/agent-message-queue/commit/b74fef5e5ae1f783bd92625de6b319a0e29a4645))
* **ci:** address Codex review — dispatch input, version validation, var collision ([3eac7cd](https://github.com/avivsinai/agent-message-queue/commit/3eac7cd431c4563cfb805d56c33ce687e19505b6))
* **ci:** consolidate skill publishing into release workflow ([#59](https://github.com/avivsinai/agent-message-queue/issues/59)) ([8638b15](https://github.com/avivsinai/agent-message-queue/commit/8638b15cecba900e8d810dcf488c6daed44b6067))
* **ci:** don't retry publish after alias failure (package already uploaded) ([0b97d4b](https://github.com/avivsinai/agent-message-queue/commit/0b97d4b9774388637ed59b4f9e0979e0d7421d49))
* **ci:** handle skild alias conflicts gracefully in publish workflow ([0464b0a](https://github.com/avivsinai/agent-message-queue/commit/0464b0a4187c24386d6a673f571b8cb74fdbe525))
* **ci:** prevent release race condition with concurrency control ([0b4c10b](https://github.com/avivsinai/agent-message-queue/commit/0b4c10b4f6ea0690c045971f0bfdd9bbca49311d))
* claim inbox messages before draining ([#174](https://github.com/avivsinai/agent-message-queue/issues/174)) ([2a1c89e](https://github.com/avivsinai/agent-message-queue/commit/2a1c89eba8b58bae672e2793d4b63b8d3b4f553e))
* **cli:** advertise claude-code swarm type in help and sync CLI docs ([#243](https://github.com/avivsinai/agent-message-queue/issues/243)) ([927d825](https://github.com/avivsinai/agent-message-queue/commit/927d825094eb0aeb73b5b35532f26abc557fac8d))
* **cli:** clarify Grok skill discovery docs and coop next-steps hint ([#242](https://github.com/avivsinai/agent-message-queue/issues/242)) ([07eee13](https://github.com/avivsinai/agent-message-queue/commit/07eee133cb7e290c673180586fdde7a4a7d90818))
* **cli:** close cross-tree guard bypass in root classification ([#231](https://github.com/avivsinai/agent-message-queue/issues/231)) ([774f568](https://github.com/avivsinai/agent-message-queue/commit/774f568efc656a3c57ed7c4c48ee5022f4e415d9))
* **cli:** fail closed on empty --body, treat - as stdin ([#143](https://github.com/avivsinai/agent-message-queue/issues/143)) ([6517153](https://github.com/avivsinai/agent-message-queue/commit/6517153b92f22bf16d5247d3b85538e627d926b9))
* **cli:** refuse unqualified cross-tree --root sends ([#144](https://github.com/avivsinai/agent-message-queue/issues/144)) ([#146](https://github.com/avivsinai/agent-message-queue/issues/146)) ([173cefe](https://github.com/avivsinai/agent-message-queue/commit/173cefe9926a90bd7bc8f974fe7e6a620d08b3c0))
* completion command help handling and extra args validation ([13568bf](https://github.com/avivsinai/agent-message-queue/commit/13568bf5778304bab0728df591b3dcb2126dc99e))
* coop exec reuses only usable existing wake ([#153](https://github.com/avivsinai/agent-message-queue/issues/153)) ([82ea981](https://github.com/avivsinai/agent-message-queue/commit/82ea981f4245f9ccbc4470f58da61436a31669f4))
* **coop:** don't overwrite .amqrc when --root is explicit ([e61dc4d](https://github.com/avivsinai/agent-message-queue/commit/e61dc4d407b3f1b7388826ee15a8fd623a48f0cb))
* **coop:** don't overwrite .amqrc when --root is explicit ([16c1710](https://github.com/avivsinai/agent-message-queue/commit/16c171016e9a0931f80c006f18c7b6fafd8c2c79))
* correct AM_ROOT guidance in skill docs ([3b40a9e](https://github.com/avivsinai/agent-message-queue/commit/3b40a9e3952a43a6f786ba349315f0a8115f9ada))
* correct AM_ROOT guidance in skill docs for outside-coop-exec usage ([1d6f226](https://github.com/avivsinai/agent-message-queue/commit/1d6f2269579424e285ad64902a33687b54673a33))
* correct NEXT STEP output after draft→review phase advance ([7f5ef24](https://github.com/avivsinai/agent-message-queue/commit/7f5ef24d851740613225731416f1e5c8ef30416d))
* cross-compile wake repairability gate ([#157](https://github.com/avivsinai/agent-message-queue/issues/157)) ([daada34](https://github.com/avivsinai/agent-message-queue/commit/daada347d1695df74dbc010cf12b5301bd322c66))
* cross-project sender identity (from_project field) ([#48](https://github.com/avivsinai/agent-message-queue/issues/48)) ([59792c6](https://github.com/avivsinai/agent-message-queue/commit/59792c679cd88e8e65483f46ccce1854c5db0de8))
* deduplicate Environment Rules in SKILL.md ([9162237](https://github.com/avivsinai/agent-message-queue/commit/9162237d14c23c6923a44fd9e2715cede9a25d05))
* deduplicate Environment Rules section in SKILL.md ([0ec4528](https://github.com/avivsinai/agent-message-queue/commit/0ec452803cd82ffc8fa5943511df1bf774caf196))
* defer wake injection during terminal activity ([#97](https://github.com/avivsinai/agent-message-queue/issues/97)) ([0262568](https://github.com/avivsinai/agent-message-queue/commit/0262568072b454ebb0ca64a0a57e206d866f43d5))
* **delivery:** report indeterminate-durability commits + capability-relative reads ([#261](https://github.com/avivsinai/agent-message-queue/issues/261)) ([63cc42f](https://github.com/avivsinai/agent-message-queue/commit/63cc42f2652c709aae33c75c286d82407fd74b6c))
* **deps:** add 7-day cooldown to Dependabot version updates ([#63](https://github.com/avivsinai/agent-message-queue/issues/63)) ([7b07905](https://github.com/avivsinai/agent-message-queue/commit/7b07905dd93e49add3158a4f4a7e23d070cd685e))
* detect short atomic writes ([#184](https://github.com/avivsinai/agent-message-queue/issues/184)) ([03d3be1](https://github.com/avivsinai/agent-message-queue/commit/03d3be1f786f44b0b6b31403069882b29dacbbaf))
* **docs:** correct skill installation instructions ([211d153](https://github.com/avivsinai/agent-message-queue/commit/211d153bc8e705f42d1ef3455622583779eab609))
* enforce send-first-research-second in spec skill ([7a9c1eb](https://github.com/avivsinai/agent-message-queue/commit/7a9c1eb2b894ba1d3d6d9a3db3af2e3a389d09ff))
* expire owner-dead inject-via wakes ([#177](https://github.com/avivsinai/agent-message-queue/issues/177)) ([640ee0b](https://github.com/avivsinai/agent-message-queue/commit/640ee0b8eb84db9f2f7e8a16db74192716146461))
* gofmt common.go after merge ([b2561eb](https://github.com/avivsinai/agent-message-queue/commit/b2561ebbbb2a2231d95e8cff4161cadf26e187fb))
* harden 0.33 routing contracts ([#114](https://github.com/avivsinai/agent-message-queue/issues/114)) ([38b415c](https://github.com/avivsinai/agent-message-queue/commit/38b415c267580108a84beecd54b2dd9f23a1ab6d))
* harden dlq retries and moves ([#175](https://github.com/avivsinai/agent-message-queue/issues/175)) ([bbcc57d](https://github.com/avivsinai/agent-message-queue/commit/bbcc57d5eec9231212a2832ea0ef02e9ab468e4c))
* harden stale wake lock cleanup ([58db568](https://github.com/avivsinai/agent-message-queue/commit/58db5681f8f0c35d5ad502553ce9eafbcb4190f3))
* harden wake metadata writes ([#187](https://github.com/avivsinai/agent-message-queue/issues/187)) ([a6addcb](https://github.com/avivsinai/agent-message-queue/commit/a6addcb839ade5da6a083523fc3a0e447c446302))
* hold wake submit CR past TUI paste-burst window and restore rescue CR ([#214](https://github.com/avivsinai/agent-message-queue/issues/214)) ([46060ea](https://github.com/avivsinai/agent-message-queue/commit/46060ea70f84e3ed6fee1c92f50069f88b973e2a))
* honor no-gitignore in coop exec auto-init ([#192](https://github.com/avivsinai/agent-message-queue/issues/192)) ([8878ef6](https://github.com/avivsinai/agent-message-queue/commit/8878ef6eb0e9d3a7b141e1d01e53c9e7126f9704))
* **hook:** cap HOOK_LOG growth on session start ([#91](https://github.com/avivsinai/agent-message-queue/issues/91)) ([6368086](https://github.com/avivsinai/agent-message-queue/commit/6368086bfb7bd18cbbb3dc58f0cd8d3a896909af))
* ignore generated skill workspace artifacts ([dd8feb7](https://github.com/avivsinai/agent-message-queue/commit/dd8feb7258ebc38251f74a8efd2abd7b05e3ee03))
* improve error handling in hasMessageFiles and add answer to kind help ([c292f62](https://github.com/avivsinai/agent-message-queue/commit/c292f62547885094c85926809af39a19c2e60b6d))
* improve install script robustness ([f3076d9](https://github.com/avivsinai/agent-message-queue/commit/f3076d979f1d9ac6acc6e8aaa0f719249b531588))
* improve wake lock TTY detection and kill orphaned processes ([7826a77](https://github.com/avivsinai/agent-message-queue/commit/7826a77289bdb86c8ddd4165407c95f0dbe34202))
* install to user-local paths instead of /usr/local/bin ([ae53a3a](https://github.com/avivsinai/agent-message-queue/commit/ae53a3a0550b60bfa3dd50e50495d26fc21d702b))
* isolate golangci-lint cache per checkout ([#205](https://github.com/avivsinai/agent-message-queue/issues/205)) ([921cde2](https://github.com/avivsinai/agent-message-queue/commit/921cde261a85901f1ce64efd2ecfe1787ed871da))
* isolate smoke git sandboxes from hook env ([#196](https://github.com/avivsinai/agent-message-queue/issues/196)) ([6050962](https://github.com/avivsinai/agent-message-queue/commit/60509629acfcc16419a9774cb139f2029daa78f5))
* let --root override AM_ROOT for cross-session routing ([#60](https://github.com/avivsinai/agent-message-queue/issues/60)) ([8b157f7](https://github.com/avivsinai/agent-message-queue/commit/8b157f78c0f36ee402247e76fd81e998cf136a4b))
* make --session pure sugar, revert --root semantics ([c10671a](https://github.com/avivsinai/agent-message-queue/commit/c10671a511c6d213dc8df5251e07e453125157a1))
* make amq-cli skill self-contained by inlining watcher ([c0a59f7](https://github.com/avivsinai/agent-message-queue/commit/c0a59f79cef79ce2bae77461fc4608d5e2bdd20a))
* normalize help system correctness and exit semantics ([36d6cd2](https://github.com/avivsinai/agent-message-queue/commit/36d6cd21013182d22bfc64d8626ff804b5fe1231))
* normalize help system correctness and exit semantics ([072f006](https://github.com/avivsinai/agent-message-queue/commit/072f0068807bc4f1d2993dad2e8c133d5d96cff3))
* pin session roots to absolute paths (+ reply --wait-for, who base-root header) ([#204](https://github.com/avivsinai/agent-message-queue/issues/204)) ([dbaec17](https://github.com/avivsinai/agent-message-queue/commit/dbaec17d8edd68f9779d28c1b0e896c1f270d2a1))
* preserve committed partial deliveries ([#176](https://github.com/avivsinai/agent-message-queue/issues/176)) ([7abc895](https://github.com/avivsinai/agent-message-queue/commit/7abc895a5e977070b3c1364a742b8f54aac6eb9f))
* prevent partner agent from implementing during spec review ([f271664](https://github.com/avivsinai/agent-message-queue/commit/f271664624fb56bcaf3dd4677717262702a6256c))
* prevent wake suspension and duplicate processes ([#2](https://github.com/avivsinai/agent-message-queue/issues/2)) ([e881803](https://github.com/avivsinai/agent-message-queue/commit/e881803627c70fd4054a3e2aaf80f240f96a7385))
* reject unexpected positional args in send and reply ([#64](https://github.com/avivsinai/agent-message-queue/issues/64)) ([6f186b8](https://github.com/avivsinai/agent-message-queue/commit/6f186b84cffd727a99a25e7c6aff6162ac33c3cb))
* reject unsafe queue artifact reads ([#186](https://github.com/avivsinai/agent-message-queue/issues/186)) ([8fc9e7e](https://github.com/avivsinai/agent-message-queue/commit/8fc9e7e70fc9b21abbf582591e28ef98b5b9087b))
* **release:** handle already-published retry path ([f4413a3](https://github.com/avivsinai/agent-message-queue/commit/f4413a3356e450691693a6625952b75081e22afb))
* **release:** honor explicit build version ([318d52b](https://github.com/avivsinai/agent-message-queue/commit/318d52bb7b90bb465a672183af0dbabc3a02a763))
* **release:** maintain changelog compare links ([#116](https://github.com/avivsinai/agent-message-queue/issues/116)) ([4836aa8](https://github.com/avivsinai/agent-message-queue/commit/4836aa891fe96de2fd8011f17a30d8cfbac8b5bf))
* **release:** pin goreleaser toolchain ([ba5bd7a](https://github.com/avivsinai/agent-message-queue/commit/ba5bd7aa9ee180bb23f83e0d108e63f92e902520))
* **release:** preserve release-notes arg in CI ([3f3e01b](https://github.com/avivsinai/agent-message-queue/commit/3f3e01b3cb09c24e7f8b3810ff11c4b769355dfa))
* remove invalid 'category' key from plugin manifest ([cbe5722](https://github.com/avivsinai/agent-message-queue/commit/cbe572202c4858cce9769c9f9e737273093d038f))
* remove me from .amqrc (root-only config) ([#10](https://github.com/avivsinai/agent-message-queue/issues/10)) ([b335bc4](https://github.com/avivsinai/agent-message-queue/commit/b335bc474516c0a37dd2913b1649ef9adbda4960))
* remove unused writeFlagDefaults (lint) ([39ebe84](https://github.com/avivsinai/agent-message-queue/commit/39ebe840b0169e2f0b7bc92544e4e74f36c02d20))
* rename spec skill to amq-spec to avoid naming collision ([d81e49d](https://github.com/avivsinai/agent-message-queue/commit/d81e49d9af44752bb379fcc655729faed10030db))
* require wake readiness for coop exec ([#123](https://github.com/avivsinai/agent-message-queue/issues/123)) ([e4cb881](https://github.com/avivsinai/agent-message-queue/commit/e4cb881c75f0d01e62f767d1a65bf5644e4d16d0))
* resolve AM_ROOT from amq env instead of hardcoding in skill docs ([68c3e74](https://github.com/avivsinai/agent-message-queue/commit/68c3e74fdc91b51956af2e841190f38ffa40af1f))
* resolve root from parent dirs to prevent split mailboxes ([59b8e80](https://github.com/avivsinai/agent-message-queue/commit/59b8e8043e63a0fa8863fc766b5cd987fa66da98))
* resolve wake inject-via symlinks before validation ([#200](https://github.com/avivsinai/agent-message-queue/issues/200)) ([e0f92e6](https://github.com/avivsinai/agent-message-queue/commit/e0f92e6c39f404e6ee27b4fd110e631389a60cf9))
* restore video URL in README ([49adb1a](https://github.com/avivsinai/agent-message-queue/commit/49adb1a0ddf4a6711784b5f73f3d8e2dd1007b67))
* self-heal stale wake locks via verified ownership ([#151](https://github.com/avivsinai/agent-message-queue/issues/151)) ([3f64862](https://github.com/avivsinai/agent-message-queue/commit/3f6486273c9d9bcc0e7823998710f60866d083be))
* **session:** refuse cross-session mailbox access and surface sibling backlogs ([#224](https://github.com/avivsinai/agent-message-queue/issues/224)) ([#225](https://github.com/avivsinai/agent-message-queue/issues/225)) ([57a296e](https://github.com/avivsinai/agent-message-queue/commit/57a296e0ecd77bf8b73a5c065f2a151260ef0a06))
* show both Claude and Codex examples in SKILL.md ([d6d3a71](https://github.com/avivsinai/agent-message-queue/commit/d6d3a71942fc5eafed4ef27f94a748852959bb1b))
* skill publish workflow handles existing versions gracefully ([7a167da](https://github.com/avivsinai/agent-message-queue/commit/7a167daae1e7dc69ca5688029dcbe004b17f8d9b))
* **skill:** statusline snippet backward-compat with pre-0.27 amq ([706ce5f](https://github.com/avivsinai/agent-message-queue/commit/706ce5fafdb3f53a195934efbc3f7993183aea68))
* skip changelog gate on Dependabot PR author, not only event actor ([#219](https://github.com/avivsinai/agent-message-queue/issues/219)) ([fc87a86](https://github.com/avivsinai/agent-message-queue/commit/fc87a861e92b9df041cd10158657c424a87139cd))
* split wake.go for Windows cross-compile ([bd84e13](https://github.com/avivsinai/agent-message-queue/commit/bd84e1313dadee064c127245c78c204c7766f8cc))
* suppress duplicate update-check in coop exec wake subprocess ([c5d8816](https://github.com/avivsinai/agent-message-queue/commit/c5d8816a8cdaeb5fde57e6922dee462c31dbc0b5))
* tighten post-merge spec workflow follow-ups ([6228c42](https://github.com/avivsinai/agent-message-queue/commit/6228c42334646de071351d2d6b618063b8deb972))
* tighten post-merge spec workflow follow-ups ([409a1c8](https://github.com/avivsinai/agent-message-queue/commit/409a1c834c127c70ed47979c88624c1e48732944))
* update comment to be more general ([86a7c3f](https://github.com/avivsinai/agent-message-queue/commit/86a7c3fb5c3d5d61f6559813f9330eaeef98eb5e))
* update stop hook format and add robustness improvements ([ede3e73](https://github.com/avivsinai/agent-message-queue/commit/ede3e73ca9c941c6b3d1d9449b341d937d634214))
* **upgrade:** remove API headers from asset downloads ([59dd450](https://github.com/avivsinai/agent-message-queue/commit/59dd4500ca5215c90f5838497405d128e47336a7))
* use amq env instead of hardcoded AM_ROOT exports ([#12](https://github.com/avivsinai/agent-message-queue/issues/12)) ([d73d0b8](https://github.com/avivsinai/agent-message-queue/commit/d73d0b8904632d22514ffd2941aaf7dd6a1c998a))
* use atomic replace on windows ([#188](https://github.com/avivsinai/agent-message-queue/issues/188)) ([ce93f30](https://github.com/avivsinai/agent-message-queue/commit/ce93f30a40b6fe041c215c20558d24f2ed784ace))
* use bracketed paste for TUI-compatible wake notifications ([0b074a4](https://github.com/avivsinai/agent-message-queue/commit/0b074a4cb5919523d20d407e27f07197e4dc90ea))
* use clickable thumbnail for demo video (mobile compatibility) ([d92cb67](https://github.com/avivsinai/agent-message-queue/commit/d92cb67d548e911a87fb77015e37d60ad3eba9eb))
* use inline STOP-READ-THEN-ACT gate for spec workflow ([c25495c](https://github.com/avivsinai/agent-message-queue/commit/c25495c7741c84e1f124a12c9a66ea4ed19cd17d))
* use raw inject mode for both Claude Code and Codex ([9e28239](https://github.com/avivsinai/agent-message-queue/commit/9e28239bd9eba7e3b156f28dd7c2401c2fc9d37c))
* use unix.Getsid for cross-platform session ID check ([b8ee0cd](https://github.com/avivsinai/agent-message-queue/commit/b8ee0cdb0ad83cfcfb05a9611a9371a57971cadf))
* validate queue message filenames ([#185](https://github.com/avivsinai/agent-message-queue/issues/185)) ([dd3a136](https://github.com/avivsinai/agent-message-queue/commit/dd3a1363efd53f38372772d8ed8e2ac48131491e))
* wait for wake raw input drain before enter ([#208](https://github.com/avivsinai/agent-message-queue/issues/208)) ([0259f92](https://github.com/avivsinai/agent-message-queue/commit/0259f92bf54c2c32c3e58aa8870c0f5feece4bca))
* **wake:** adopt tri-state identity classification for wake locks ([#247](https://github.com/avivsinai/agent-message-queue/issues/247)) ([c28bb2a](https://github.com/avivsinai/agent-message-queue/commit/c28bb2ab220f86250b79d7a531907cc05ffb74ad))
* **wake:** allow multiple wake processes per TTY ([#11](https://github.com/avivsinai/agent-message-queue/issues/11)) ([d9f62c2](https://github.com/avivsinai/agent-message-queue/commit/d9f62c28c7b000bb0940b45f039420527467d1cb))
* **wake:** close cooperative-stop duplicate-injection window and harden Darwin lifecycle ([#260](https://github.com/avivsinai/agent-message-queue/issues/260)) ([8d6e6f4](https://github.com/avivsinai/agent-message-queue/commit/8d6e6f46a646228f18e4507bf3db95aaad6c34e1))
* **wake:** enforce exact wake-mode compatibility ([#246](https://github.com/avivsinai/agent-message-queue/issues/246)) ([8153b8a](https://github.com/avivsinai/agent-message-queue/commit/8153b8af39eccfedd36994058bd1ba2523bceb5f))
* **wake:** harden Darwin boot identity and zombie detection ([#236](https://github.com/avivsinai/agent-message-queue/issues/236)) ([ae776b8](https://github.com/avivsinai/agent-message-queue/commit/ae776b8210292b92b10658221455176a3c46cf4e))
* **wake:** require exact injector identity for inject-via wake reuse ([#248](https://github.com/avivsinai/agent-message-queue/issues/248)) ([4871088](https://github.com/avivsinai/agent-message-queue/commit/4871088a944d7f2f2cac0ee13b8c3838b1ef01a5))


### Dependencies

* bump golang.org/x/term from 0.44.0 to 0.45.0 ([#239](https://github.com/avivsinai/agent-message-queue/issues/239)) ([9ff9c1d](https://github.com/avivsinai/agent-message-queue/commit/9ff9c1daa4e84109399102fb81d320c26b87e9fc))

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
