# 在线原身份只认 gate 签名：Codex 执行清单（2026-11-05）

本批由 Claude 改代码，**未编译、未运行任何测试**。请 Codex 按下列顺序执行并回填结果（建议写同目录 `codex-results.md`）。设计见 [gate 在线身份](../../design/gate-online-identity.md)。

## 改了什么

项目未上线，删除在线域「请求没带原账号就去 login Redis 读 login_session」的兼容回退：

- 共享：新增 `go/shared/roleidentity/session.go`（`SessionAccount` / `CaptureSession`，只认 `callerauth` 验签原件）与测试；新增测试辅助 `go/shared/callerauth/callerauthtest`；删除整个 `go/shared/loginsession` 包与 `go/shared/playerredis/login_reader.go`（`LoginReaderConfig`）；`go/shared/callerauth/session.go` 给 `GateInterceptor` 补验签结果指标 `caller_auth_session_verify_total{result}` 与 nonce 溢出回调（计数 `caller_auth_nonce_overflow_total` + ERROR 日志）。
- 六个在线服务 match / chat / friend / guild / trade / mail：身份入口改用 `roleidentity.CaptureSession` / `SessionAccount`；删除 `GlobalLoginSessions`、`LoginRedis`（trade 为 `Rmb.*`）配置、校验、Redis 客户端与关闭路径；yaml 删同名段；测试改为真签名身份（`callerauthtest.Incoming`）与未签名负例（`callerauthtest.Unverified`）。
- 部署脚本：`tools/scripts/k8s_deploy.ps1`、`go_services.ps1`、`lib/sql_boot_catalog_local.ps1`、`prepare_docker_debug.py` 不再为这六个服务生成、校验或计账 login Redis；Redis 连接总账去掉这六个服务的登录域池；对应脚本测试同步。login 自己的 `LoginRedis` 不变。
- login：删除内部验签的 dev/test 宽松档（`InternalAuthConf.ForceEnforce`、`config.EnforceInternalAuth`、`login.go` 的宽松日志分支、`pkg/callerauth/interceptor.go` 的告警放行分支、`etc/login.yaml` 的 `ForceEnforce` 行），任何模式验签失败都 Unauthenticated；`entergamelogic.go` 修正一处过时注释。C++ 无改动。
- 另一会话（服务器代码缺口补完）同期修复 K8s `-ClientEntryMode external` 下 gate StatefulSet 缺 `MMORPG_INTERNAL_AUTH_SECRET`、Pod 一直 CrashLoop 的回归，一并验证：`tools/scripts/lib/k8s_client_entry.ps1`（契约加密钥 env / Secret 名 / key，env 渲染支持 secretKeyRef，StatefulSet 带上该项）、`k8s_deploy.ps1`（New-NodeDeploymentYaml 的 gate env 改取契约名字、渲染结果不变；Apply-Zone 在两种入口形态之前都先建 `gate-identity-auth`，`-SkipGoSvc` 时也建）、`tests/k8s_client_entry_contract.tests.ps1`（新增两条用例）。
- 372caca25 给在线 Go 服务加 `gate-identity-auth` 后留下的两处：`tests/k8s_migrate_gate.tests.ps1` 里 mail / trade 资产关 / guild 资产关三条「不建 Secret」断言改为排除这把必需密钥并单独断言它恰好建一次；`tools/scripts/deploy-staging.sh` 启动前强制检查已 export 该密钥（≥32 字节、无首尾空白），`docs/ops/linux-staging-stress-runbook.md` 同步。

逐文件清单见文末「文件」。

## 1. 静态检查（仓库根）

```powershell
rg -n 'shared/loginsession|LoginReaderConfig|loginsession\.Account|WithLoginSessions|rmbLoginRedis' go tools deploy
rg -n 'GlobalLoginSessions' go tools deploy
```

通过标准：两条都无输出（第二条若命中，只允许出现在 docs 的历史记录里，不允许在 go/、tools/、deploy/）。`rg -n 'LoginRedis' go` 只允许出现在 `go/login/` 与 tip 生成码（`kLoginRedisError`）里。

## 2. Go 构建、vet 与测试（串行，每个模块单独执行）

```powershell
foreach ($m in 'shared','match','chat','friend','guild','trade','mail','rank','login') {
    Push-Location "go/$m"
    go build ./... ; go vet ./... ; go test -count=1 ./...
    Pop-Location
}
```

- `shared`：重点 `./callerauth/...`、`./roleidentity/...`、`./playerredis/...`、`./domainhost/...`。
- `rank` 与 `login` 本批没改业务代码，但依赖 `shared/callerauth`（新增 go-zero metric 注册），必须编过并跑全量测试，确认没有 Prometheus 重名注册 panic。
- 通过标准：全部 build / vet 无输出，test 全绿；跳过数与上一轮（审核包 `Go测试与静态检查.json`）对比，新增跳过要说明。失败保留完整输出，不要自动重试或改断言。

再构建服务程序：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build -GoServices match,chat,friend,guild,trade,mail,rank,login
```

## 3. 脚本合同测试（仓库根，逐个执行）

```powershell
foreach ($t in 'k8s_deploy_contract','redis_player_cluster_contract','lobby_authority_render','go_services_ports',
               'sql_boot_catalog_local','docker_debug_sql_boot','go_services_local_mysql','chat_authority_deploy',
               'gate_identity_environment','k8s_client_entry_contract','k8s_migrate_gate','k8s_redis_split_contract',
               'redis_maxclients_contract') {
    pwsh -File "tools/scripts/tests/$t.tests.ps1"
}
bash -n tools/scripts/deploy-staging.sh   # 有 shellcheck 时再跑 shellcheck tools/scripts/deploy-staging.sh
```

`deploy-staging.sh` 另做两次手工检查（不需要起服务）：不设 `MMORPG_INTERNAL_AUTH_SECRET` 执行 `start`，应在「checking docker infra」之前报错退出；设一个 31 字节的值，同样报错。

通过标准：全部通过。`lobby_authority_render` 有一条既有失败（AssetOpsOn=false 夹具与 guild.yaml 的 AssetOp.Enabled=true，审核包已记录），若仍是这一条且原因相同，记为既有失败；出现任何新失败都要贴用例名与输出。

## 4. 本机真连接冒烟

前置：按既有启动方式拉起整套（保留 `player-store-r2-infra` Compose 项目选择）；`MMORPG_INTERNAL_AUTH_SECRET` 由本机启动器从 login.yaml 开发值注入。服务配置里已没有 LoginRedis，启动日志不应再出现这六个服务连接 login Redis 的记录。

1. 机器人新账号：Login → CreatePlayer → EnterGame → 等 NotifyEnterScene → PVE 一局（直连回合消息 ≥1、Scene 持久结算终局）→ 聊天 / 好友 / 公会 / 邮件各一个客户端请求成功。
2. 进场后删除本连接的 `login_session:{<gate_uuid>:<incarnation>}`，再发上述在线请求：仍成功（在线域不读它）。
3. LeaveGame 后不重新 Login，直接发在线请求：被拒（gate 已清原账号）。
4. 重启 gate（换 PID/UUID）：旧连接未登录直接 EnterGame 被拒；重新 Login 同账号、EnterGame 原角色后在线请求成功。可复用审核包 `复核用测试/gate_restart_acceptance_test.go` 与 `复核用脚本/run_gate_runtime_acceptance.ps1`。
5. 抓 match、chat、friend、guild、trade、mail、rank 的 `/metrics`：`caller_auth_session_verify_total{result!="ok"}` 为 0；`caller_auth_nonce_overflow_total` 为 0。

通过标准：1、2、4 成功，3 被拒，5 计数为 0。回执写明账号、gate 前后 UUID、各步结果；不把本机功能冒烟写成压测或生产灰度结论。

## 5. 不需要做

- 不需要协议生成：本批没改 proto。
- 不需要 C++ 编译：本批没改 C++。
- 客户端不需要改动或重编译。
- Java 版本批不做，已在 Java 仓 `PARITY.md` 记 TODO。

## 文件

共享与 login（Claude）：

- 新增 `go/shared/roleidentity/session.go`、`session_test.go`，`go/shared/callerauth/callerauthtest/callerauthtest.go`
- 删除 `go/shared/loginsession/{reader.go,reader_test.go,authenticated_account_test.go}`、`go/shared/playerredis/login_reader.go`；`go/shared/playerredis/playerredis_test.go` 删 `TestLoginReaderConfig`
- `go/shared/callerauth/session.go`（指标与溢出回调）、`callerauth.go`（包注释）
- `go/login/login.go`、`internal/config/config.go`、`internal/config/secrets.go`、`internal/logic/pkg/callerauth/interceptor.go`、`metrics.go`、`etc/login.yaml`、`internal/logic/clientplayerlogin/entergamelogic.go`（注释）
- `tools/scripts/tests/k8s_migrate_gate.tests.ps1`、`tools/scripts/deploy-staging.sh`、`docs/ops/linux-staging-stress-runbook.md`

chat / friend / guild（子任务 B）：

- chat：`chat.go`、`etc/chat.yaml`、`internal/config/config.go`、`internal/config/sensitive_words_test.go`、`internal/session/identity.go`、`identity_test.go`、`internal/svc/authority.go`、`internal/svc/servicecontext.go`、`internal/constants/constants.go`（注释）、`original_actor_chain_test.go`
- friend：`friend.go`、`friend_test.go`、`go.mod`（只改注释）、`etc/friend.yaml`、`internal/config/config.go`、`config_test.go`、`internal/session/identity.go`、`identity_test.go`、`internal/svc/servicecontext.go`；删除 `internal/svc/login_session_test.go`
- guild：`guild.go`、`etc/guild.yaml`、`internal/config/config.go`、`config_test.go`、`internal/session/identity.go`、`identity_test.go`
- 注意：chat 已不直接 import miniredis 与 go-redis，`go/chat/go.mod` 未改；build/test 不受影响，按仓库口径需要 tidy 时由 Codex 执行 `go mod tidy` 并回报 go.mod / go.sum 差异。

match / trade / mail（子任务 A，trade 的错误归类由 Claude 复核后改）：

- match：`original_role_interceptor.go`（改为不依赖 ServiceContext 的普通拦截器）、`original_role_interceptor_test.go`、`match_service.go`（只改装配一行）、`internal/config/config.go`、`config_test.go`、`internal/svc/servicecontext.go`、`etc/match_service.yaml`、`internal/authority/rank_mail_chain_integration_test.go`；删除 `internal/svc/login_session.go`
- trade：`internal/svc/role_trade.go`（身份核不过回 `commerce.ErrInvalid` = kInvalidParameter，不再落成故障 tip）、`internal/svc/role_trade_test.go`、`internal/svc/servicecontext.go`、`internal/config/config.go`、`config_test.go`、`etc/trade.yaml`
- mail：`mail.go`、`internal/server/server.go`、`session_identity_test.go`、`rank_mail_chain_fixture_test.go`、`flow_integration_test.go`、`etc/mail.yaml`
- 注意 1：`TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` 用预编的 mail 夹具程序（环境变量 `RANK_MAIL_CHAIN_MAIL_EXE`），必须用本批源码重编；旧程序没有验签器，ListMails 会回 kInvalidParameter。父子进程共用 `callerauthtest.Key{}`。
- 注意 2：中文键的 map 与表驱动结构是手工对齐的，请对全部改动文件跑 `gofmt -l`，有输出就 `gofmt -w` 后再测（只是格式）。

部署脚本与文档（子任务 C，报告的连接总账旧值→新值见下）：

- `tools/scripts/k8s_deploy.ps1`：`Get-RedisPoolMirror`、`Get-RedisPlayerPoolMirror`、`Get-RedisConnectionLedgerInputs`、`Get-RedisPlayerConnectionLedgerInputs`、`$script:PlayerRedisClientServices` 去掉六个服务的登录域池（chat 两份账都不再出现）；`New-PlayerRedisConfigYaml` 删 `-StandaloneAddr/-StandaloneDb/-StandalonePasswordEnv/-StandalonePassword`（`LoginRedis` 段只给 login）；guild / chat / match / friend / mail / trade 的 ConfigMap 不再渲染 `GlobalLoginSessions` / `LoginRedis`，trade 的 Rmb 段改为整段镜像（删 `$rmbHits -ne 2` 检查）；chat 形状校验只删这一项条件。
- `tools/scripts/go_services.ps1`（删 `Get-LocalAccountDirectorySources`，chat / match 本机设置只核 SQL 字段）、`tools/scripts/lib/sql_boot_catalog_local.ps1`（`New-LocalSqlBootSettings` 删必填 `-LoginConfigPath`）、`tools/scripts/lib/sql_boot_catalog_docker.ps1`（调用处同步）、`tools/scripts/prepare_docker_debug.py`（删只为 match `LoginRedis.Addrs` 的单行序列 dumper）。
- 测试：`tools/scripts/tests/` 下 `k8s_deploy_contract`、`redis_player_cluster_contract`、`lobby_authority_render`、`chat_authority_deploy`、`go_services_ports`、`go_services_local_mysql`、`sql_boot_catalog_local`、`docker_debug_sql_boot`。
- 文档：`deploy/k8s/README.md`、`tools/scripts/README.md`。
- 清单（Claude，11-06）：`deploy/k8s/manifests/go-svc/chat.yaml` 删 `LOGIN_REDIS_PASSWORD`、`match.yaml` 删 `MMORPG_LOGIN_REDIS_PASSWORD`（都是 optional Secret 引用，已无人读取，没有测试依赖其存在）。
- Redis 连接总账（规划档 Z=4、R=2、所有 Go 服务 2 副本）：控制域全局 557→469、稳态 2269→2181、滚动余量 984→896、峰值 3303→3127；玩家域每主节点全局 312→224、稳态 1288→1200、滚动余量 554→466、峰值 1892→1716。差额即六个服务每副本的登录域池（match / friend / guild / trade / mail 各 8，chat 4）× 2 副本。合同测试的期望值按此重算。
- 本地另需：`python -I -m py_compile tools/scripts/prepare_docker_debug.py`；相邻回归再跑 `logical_db_deploy_contract`、`k8s_gate_drain`、`go_services_stop`。
