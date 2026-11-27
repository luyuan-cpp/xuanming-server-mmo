# 登录排队:验证与开闸交接(2026-11-26,交 Gemini)

> 范围:登录排队这条线(login 排队、本仓网关前置限流、Unity / robot 持票轮询)。代码已经写完并入库,本单只剩运行验证,以及验证通过后的开闸。
> 执行方:Gemini(AGENTS §10.1),只做生成、编译、测试、冒烟。发现要改代码(含测试代码)时保留首错原文停下,交回 Claude 改:负责人 2026-11-26 定「逻辑代码由 Claude 改,Gemini 只负责编译」。第 4 节的开闸也由 Claude 做。

## 1. 现状

| 项 | 状态 |
|---|---|
| 服务端代码 | 写完。`b6a08148c`(入队按账号幂等、队列上限、每拍上限、重连占位、只接本区)、`f067d01fc`(会话带 `gate_zone_id`、`/api/refresh-token` 限流)、2026-11-26 三处(K8s 镜像两个上限键、告警 `LoginQueueFull`、快速通道一个账号只占一个在途名额,已入库);2026-11-26 晚:票据绑定账号、排队开着时不接匿名分配(login、gate、robot,未提交,见 `docs/PROGRESS.md` 同名条目) |
| 客户端代码 | 写完(Unity 持票轮询与收尾、robot 状态机),见 `docs/PROGRESS.md` 2026-11-19~20 四条。Unity 这次不用改代码(早已先登录再分配),只要重新生成协议 |
| 协议 | 2026-11-26 晚改了 `proto/common/base/message.proto`(`GateTokenPayload` 字段 7 `account_digest`)与 `session.proto`(`SessionDetails` 字段 11 `ticket_account_digest`),**未生成** |
| 编译 | login 生产代码在 2026-11-23 的 Docker 构建里编过;之后 `assigngatelogic.go`、`queue.go`、`gatetoken.go`、`loginlogic.go`、`gate_migration.go` 与 C++ gate 都又改过,没有新记录。**测试代码从没被编译过** |
| 测试 | 排队 Go 单测、网关用例、真实 Redis 冒烟都没有运行记录(2026-11-08 那份已过期) |
| 开关 | 所有环境关着:`go/login/etc/login.yaml` 的 `Queue.Enabled: false`,`tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared = $false` |

## 2. 按顺序执行

命令、通过标准、失败时保留什么,都写在 `docs/PROGRESS.md` 的条目里,这里只给顺序和入口。旧条目标题写的「待 Codex 验证」一律由 Gemini 执行。

| 步 | 做什么 | 入口(`docs/PROGRESS.md` 条目标题) |
|---|---|---|
| P | 生成协议(必须最先做):仓库根 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`;同级 `mmorpg-client` 依次 `pwsh -File tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`;`robot` 目录 `go mod vendor`(把新生成的 `go/proto` 带进 `robot/vendor`,别的会话没生成完的协议也会一起进来,见 [WebSocket 交接](./minigame-websocket-handoff-20261120.md) §3.4) | 本单第 3 节「协议」 |
| 0 | 语法与格式:`gofmt -l -e` 扫第 3 节列的 Go 文件。有语法错误就停下,交回 Claude | 本单第 3 节 |
| 1 | `go/login`、`go/player_locator`、`robot` 的 build、vet、单测 | 2026-11-20「登录排队遗留项收口」第 2 步;同日 Gemini「登录排队 / 开服限流与 Unity 客户端全链路标准收口」第 2 步;2026-11-26「登录排队:票据绑定账号」 |
| 1b | C++:**整解**重编(`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`),再跑 `routing_identity_test`(至少 `SessionDetailsBuilder.*`)与 gate 的测试工程。`SessionInfo` 多了一个成员,布局变了:所有包含 `session_info_comp.h` 的库和工程都要重编,只编测试工程会链到旧布局的库 | 2026-11-26「登录排队:票据绑定账号」 |
| 2 | 门槛 ① ②:25 条点名用例、`-count=10` 稳定性 | 2026-11-20「登录排队清合入门槛的 Codex 验证单」第一部分。要在第 1 步之后跑:其中 7 条被改过账号字面量 |
| 3 | 本仓网关:`AssignGateServiceAuthTest`、`AssignGateRateLimiterTest`、`RefreshTokenControllerTest` 等 | 「遗留项收口」第 3 步;Gemini 条目第 3 步 |
| 4 | 契约测试:`gate_pick_alerts_contract.tests.ps1`、`k8s_deploy_contract.tests.ps1` | 2026-11-26「登录排队复核与两处补齐」 |
| 5 | 变异取红 21 项(A~U),在 `%TEMP%` 的副本上做 | 「遗留项收口」第 4 步(A~K);2026-11-26「登录排队:快速通道一个账号只占一个在途名额」(L);2026-11-26「登录排队:票据绑定账号」(M~R);2026-11-26「gate:一张票只开一条连接、验签后限时登录」(S~U) |
| 6 | Unity 编译与 EditMode | Gemini 条目第 5 步 |
| 7 | 门槛 ③:真实 Redis 冒烟两轮(7.2 单实例、8.10.2 Cluster),各 11 个核对步骤加 4 个补充项 | 验证单第二部分;「遗留项收口」第 5 步 |

结果另起一条写进 `docs/PROGRESS.md`(不改旧条目),证据原件放 `build/verification/login-queue-gate-<UTC>/`。第 7 步的命令原文与输出摘要必须写进去,这是门槛 ③ 的要求。

## 3. 最可能红在哪

下面这些文件里有从没被编译过的代码,第 0、1 步先看它们:

- `go/login/internal/logic/pkg/loginqueue/queue_test.go` 文末 9 个用例(`TestQueueIDForAccount_*`、`TestEnqueue_*` 6 条、`TestHoldReconnectSlot_*`、`TestTryReserveFastPathSlot_OneSlotPerAccount`)。
- `go/login/internal/logic/pkg/loginqueue/dispatcher_test.go` 文末 `TestDispatchOnce_AdmitsAtMostTheTickLimit`。
- `go/login/internal/logic/loginpregate/assigngate_reconnect_test.go`。
- `java/gateway_node/src/test/java/com/game/gateway/service/AssignGateServiceAuthTest.java` 的两条 `queueFull*`。
- `tools/scripts/tests/gate_pick_alerts_contract.tests.ps1` 的「login.queue-length 组…」用例。
- 2026-11-26 晚「票据绑定账号」这一批(先做第 P 步,否则 `GetAccountDigest` / `GetTicketAccountDigest` / `set_ticket_account_digest` 都不存在):
  - `go/login/internal/logic/pkg/loginqueue/`:`gatetoken.go`(`SignGateToken`、`PickAndSignGateToken`、`SignMigrationGateToken` 都多一个 `account` 参数,新增 `TicketAccountDigest`、`TicketIssuedTo`);`queue.go`(`SignAdmitFn` 多一个 `account` 参数,`lookupScript` 的返回值从 4 个元素改成 5 个);`gatedrain_migrate_test.go` 的 `TestSignGateToken_BindsTheAccountOnly`(由 `TestSignGateToken_StaysUnbound` 改写);`queue_test.go` 的 `TestPoll_AdmitSlotWithoutMeta_GivesNoTicket`(新)与 `TestPoll_RetakeInsideWindow_ReturnsSameSlot`(加断言);`admit_recheck_test.go` 的 `TestPoll_ConsumeRepicksGateThatWentStaleAfterAdmit`(加断言)。
  - `go/login/internal/logic/loginpregate/assigngate_auth_test.go` 的 `TestAssignGateAuthEmptyAccountIsAnError`、`TestAnonymousAssignAllowedOnlyWithLegacyOnAndQueueOff`(新)。
  - `go/login/internal/logic/clientplayerlogin/gate_ticket_account_test.go`(新文件,复用同包 `legacy_gate_killswitch_login_test.go` 的环境)。
  - `robot/main.go`(`loginAndEnterWithAuth`、`loginAndEnterViaHttpGateway` 多一个参数,新增 `httpLoginTokens`)、`robot/authored_navmesh_e2e_test.go`。
  - `cpp/tests/routing_identity_test/session_details_builder_test.cpp` 的 `CopiesGateAndSessionIdentityFields`(加断言),文末新增 `GateTicketHolder.*` 4 条(一张票同一时刻只开一条连接)。
  - 2026-11-26 深夜 gate 再补两道(做法见设计稿 §5「一张票只开一条连接、验签后限时登录」):`cpp/libs/services/gate/session/manager/session_manager.h`(`ClaimTicket` / `ReleaseTicket`)、`session_info_comp.h`(`ticketKey`)、`client_session_handler.cpp`(验签占位、断开时摘除);`cpp/libs/engine/core/network/client_liveness.h`(登录检查点 `kLogin`、`ExpectLoginBy`,`Sweep` 多一个回调参数)、`client_heartbeat.{h,cpp}`(`kLoginDeadlineAfterVerify` = 60s、`login_timeouts` 计数与汇总字段);`cpp/nodes/gate/tests/client_liveness_test.cpp` 新增 3 条 `ClientLivenessSweeper.*Login*`,夹具的 `Sweep` 跟着多传一个回调;`tools/scripts/stress_summarize.ps1` 读 `login_timeouts`。

判读提示:

- 既有用例里凡是在循环里入队的,账号都已改成空串(匿名)。同一个账号现在只会得到一个条目;哪条用例的队列长度比预期少,先看它是不是用同一个账号入了多次。
- 入队从 MULTI 改成了一段脚本(`enqueueScript`)。按命令名拦截 `evalsha` / `eval` 的测试钩子会连入队一起拦到;放行脚本靠第一个键以 `admit:` 开头来认,入队脚本的第一个键是 `queue:{q:<z>}`。
- `TryReserveFastPathSlot` 多了第四个参数(账号,2026-11-26):带账号时占位成员是账号的条目号,一个账号只占一枚。既有用例都传空串,行为与以前相同。
- 验证单第二部分第 3 步写的「快速通道的成员以 `fast:` 开头」只对匿名分配成立。robot 带账号登录时,快速通道的占位成员是 `a-` 开头的条目号,同样约 20 秒后消失;按这个判。
- `gofmt -l` 列出上面这些文件时停下,把输出交回 Claude 改,Gemini 不改源码(2026-11-26 晚核对:`queue_test.go`、`dispatcher_test.go`、`assigngate_reconnect_test.go` 当时 `gofmt -l` 无输出)。列出别的文件只记录。
- 红了不要改断言迁就。保留失败用例的完整 `-v` 输出,停下报告。
- 第 7 步冒烟(2026-11-26 晚起):robot 配置必须 `use_http_login: true`,排队开着时 login 不接匿名分配。冒烟期间 `login_assign_gate_auth_total{result="rejected_anonymous"}` 应为 0,login 日志不应出现 `Login rejected: the gate ticket of this connection was issued to another account`;出现就是 robot 配置或代码问题,保留原文交回 Claude。robot 的各冒烟场景(`resolveGateAddrLocal`)是匿名分配,只在排队关着时跑。
- 变异 M~R(在副本上做,做法同 A~L,改完跑点名用例,确认红在下面写的地方,再还原):
  - M:`gatetoken.go` 的 `signGateToken` 删掉 `AccountDigest: TicketAccountDigest(account),` → `TestSignGateToken_BindsTheAccountOnly` 红(摘要长度),`TestPoll_ConsumeRepicksGateThatWentStaleAfterAdmit` 红(bob 也能用)。
  - N:`clientplayerlogin/loginlogic.go` 删掉 `TicketIssuedTo` 那个 `if` → `TestGateLoginRejectsATicketIssuedToAnotherAccount` 红(别人的票绑上了会话)。
  - O:`lookupScript` 取走分支里删掉「meta 不是 hash 就回 1」那三行,并把 `HSET` 改回只在 meta 是 hash 时执行 → `TestPoll_AdmitSlotWithoutMeta_GivesNoTicket` 红(signFn 被调用)。
  - P:`anonymousAssignAllowed` 改成 `return legacyGateLoginEnabled` → `TestAnonymousAssignAllowedOnlyWithLegacyOnAndQueueOff` 红。
  - Q:`authenticateAssignGate` 删掉空账号判断 → `TestAssignGateAuthEmptyAccountIsAnError` 红。
  - R:C++ `session_details_builder.h` 删掉 `set_ticket_account_digest` 那个 `if` → `SessionDetailsBuilder.CopiesGateAndSessionIdentityFields` 红。
  - S:`session_manager.h` 的 `ClaimTicket` 里「持有者还活着就回 false」改成回 true → `GateTicketHolder.SecondLiveConnectionWithTheSameTicketIsRejected` 红。
  - T:`client_liveness.h` 的 `Sweep` 里 `kLogin` 分支的 `onLoginTimeout(...)` 删掉(只留计数)→ `ClientLivenessSweeper.VerifiedButNeverLoggedInIsClosedAtTheLoginDeadline` 红。
  - U:`kLogin` 分支的 `if (view->loggedIn)` 改成 `if (false)` → `ClientLivenessSweeper.LoggingInBeforeTheDeadlineVoidsIt` 红。
- 心跳冒烟(heartbeat-idle-smoke)照常跑:H3 的裸连接只验签、不登录、静默,仍应在 29.5–35 秒内被空闲超时关掉(登录期限 60 秒,晚于它);日志汇总行多了 `login_timeouts`,这一项在冒烟里应为 0。

## 4. 开闸(改常量)

**前提**:第 2、5、7 步全部通过并已写进 `docs/PROGRESS.md`,负责人确认。缺一项都不改。

**由 Claude 执行**,Gemini 不做这一节:它改的是部署脚本和契约用例,属于代码改动。负责人确认后在 Claude 会话里说一声即可,下面是到时候照做的备忘。

同一个提交里做四件事:

1. `tools/scripts/k8s_deploy.ps1`:`$script:LoginQueueMergeGateCleared = $true`,并在它上方的注释里写上那条结果条目的标题。只能写字面量(契约测试按 AST 核对)。
2. `tools/scripts/tests/k8s_deploy_contract.tests.ps1`:用例 19、20 现在断的是「门槛未清」时的产物,换成下面两段(按 2026-11-26 的文件写的草稿;执行时辅助函数名以当时的文件为准)。用例 21、22 不动。同时改掉文件里 `# 登录排队合入门槛(用例 19–22…` 那段注释中「未清」的说法。

   ```powershell
   Test-Case '合入门槛已清(不带测试接缝):staging zone-up 的 -LoginQueue auto 解析为开,login 写 Queue 段与排队密钥的变量名,带削峰指纹' {
       # 用例 19。显式置空接缝变量:本例证明的是常量本身已经放开,不靠接缝。
       $env2 = @{} + $ProdEnv
       $env2['MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED'] = ''
       $run = Invoke-DeployDryRun -Arguments ($BaseArgs + @('-ReleaseProfile', 'staging', '-SkipPreflight', '-NodeImage', $PeakNodeImage)) -Env $env2
       Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "门槛已清时 staging 按档位开排队。输出: $($run.Output)"
       Assert-Match -Text $run.Output -Pattern 'PeakShaving: gateway_rate_limit=on\(-GatewayRateLimit auto\) login_queue=on\(-LoginQueue auto\) profile=staging ' -Because '计划行:限流与排队都按档位开'
       Assert-NotMatch -Text $run.Output -Pattern '\[PeakShaving\]LoginQueueGate=closed' -Because '门槛已清,不再打门槛警告'
       $loginFlat = ConvertTo-FlatManifest -Block (Select-ManifestByName -Output $run.Output -Name 'go-svc-login-config')
       Assert-Equal -Expected 'true' -Actual (Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.Queue.Enabled') -Because '排队开'
       Assert-Equal -Expected 'MMORPG_LOGIN_QUEUE_TOKEN_SECRET' -Actual (Get-FlatValue -Flat $loginFlat -KeyPath 'data.login.yaml.Secrets.QueueToken.Env') -Because 'ConfigMap 里只写排队密钥的变量名'
       $loginWorkload = @(Get-ManifestBlocks -Output $run.Output | Where-Object {
           $_ -cmatch '(?m)^kind: StatefulSet\s*$' -and $_ -cmatch '(?m)^  name: login\s*$'
       }) | Select-Object -First 1
       Assert-True -Condition ($null -ne $loginWorkload) -Because 'DryRun 输出里应当有 login 的工作负载'
       Assert-Match -Text $loginWorkload -Pattern 'peak-shaving-hash' -Because '排队开 = login 的 Pod 模板带削峰指纹'
   }

   Test-Case '合入门槛已清(不带测试接缝):prod -LoginQueue on 放行' {
       # 用例 20。
       $env2 = @{} + $ProdEnv
       $env2['MMORPG_TEST_LOGIN_QUEUE_GATE_CLEARED'] = ''
       $run = Invoke-DeployDryRun -Arguments ($ProdArgs + @('-LoginQueue', 'on')) -Env $env2
       Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "门槛已清时 -LoginQueue on 放行。输出: $($run.Output)"
       Assert-Match -Text $run.Output -Pattern 'login_queue=on\(-LoginQueue on\)' -Because '计划行写明排队按显式参数开'
       Assert-NotMatch -Text $run.Output -Pattern '合入门槛未清' -Because '不再拒绝'
   }
   ```

3. 把写着「当前 `$false`」「仍关闭」的说明改成现状:`docs/design/open-server-rate-limit-design.md` §11.1、`deploy/k8s/README.md`「登录排队的合入门槛」、`go/login/etc/login.yaml` 上线清单的「门槛」一段、`docs/ops/login-queue-stress-runbook.md` 的两处门槛说明、`docs/handoff/launch-readiness-20261013/spec_G4b.md` §7。
4. 重跑整套 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`,退出码 0。

改完常量之后,staging / prod 的下一次部署会按档位打开排队。`go/login/etc/login.yaml` 的 `Queue.Enabled` 保持 `false`(本机缺省不排队),不用改。

## 5. 不在本单范围

- 压测(L-14):`MaxAdmitPerTick`、区桶阈值、`handoffGrace` 都已按依据定值,压测只用来复核。压测流程按 AGENTS §6。
- 独立 Java 版:只登记待做,见 Java 仓 `PARITY.md` 2026-11-20「登录排队与开服限流标准收口」一节第 1~4 条。
- 网关 `AssignGateService.checkZoneAdmission` 注释里的「PREVIEW 区白名单暂不消费」:属于区服目录的预览区功能,不是排队。
