# 第三方登录与实名防沉迷：剩余代码与全部验证交接（2026-11-26，交 Gemini）

本单也是「全球账号库与第三方登录」会话派生的四条线的总入口：§4 给出四条线合在一起、一遍跑完的顺序。

## 0. 范围与现状

- 负责人 2026-11-26 把这四条线的剩余工作全部交给 Gemini。
- 11-27 额度重置后，原来的 Claude 会话又在跑，并各自更新了自己的交接单。所以：
  - 动手前先看各交接单的最新状态，以及 `docs/PROGRESS.md` 文末的新条目。
  - 同一项只留一个写者。
  - 本单 §3.2 的两项已去问实名会话由谁来做。11-26 实名会话的答复：设计稿正文它已改完；它复核时另发现一处缺陷；细节写在 [real-name-anti-addiction-handoff-20261126.md](real-name-anti-addiction-handoff-20261126.md)。11-26 晚实名会话换回 Fable 5.2 后，把缺陷修复与沙箱 mock 也写完了：本单 §3.2 没有待写项，实名这条线只剩验证，不要再写一遍。
  - 改代码按 AGENTS §10.3 用 Gemini 4 Neon Extra；11-27 时这几个 Claude 会话都跑在 Opus 5.5 上，不满足 §10.3。
- 四条线与各自的交接单：

| 线 | 细节在哪 | 本单管什么 |
|---|---|---|
| 第三方登录：微信 / 抖音小游戏登录、分配 gate 凭 access token 鉴权、Apple / Google 身份令牌 | 本单 §2 | 全部 |
| 实名认证与未成年人防沉迷：含 K8s 子会话、go/payment 限额闸、11-21 Gemini 的账本分片 | 本单 §3 | 全部 |
| 账号注销与客户端 Apple / Google 登录（含剧本审核流水保留期） | [account-deletion-handoff-20261120.md](account-deletion-handoff-20261120.md) | 只排进 §4 的顺序 |
| 小游戏 WebSocket 入口 | [minigame-websocket-handoff-20261120.md](minigame-websocket-handoff-20261120.md) | 只排进 §4 的顺序 |

- 不在本单：真钱支付（[payment-integration-handoff-20261120.md](payment-integration-handoff-20261120.md)），组队跟随（[team-follow-afk-hosting-handoff-20261120.md](team-follow-afk-hosting-handoff-20261120.md)），以及 Java 独立仓（只在它的 `PARITY.md` 里登记了待做）。
- **代码状态**：§2、§3 的服务端代码都在 HEAD，已被 11-19 至 11-26 的批量提交收进去。11-26 核对过：`go/login`、`go/shared/redisslot`、`proto/login`、gate 的 `minor_play_deadline.*` 都没有未提交的改动。客户端以各交接单为准。
- **验证状态**：四条线在当前代码上**都没有验证过**。11-20 以来没有任何 `go/login` 的编译记录；11-21 Gemini 那一条写的也是「未编译运行」。

## 1. 规矩

原文以 `AGENTS.md` 与 `docs/rules/` 为准，这里只摘本单用到的：

- **模型档位（AGENTS §10.3）**
  - 写代码、改文档、分析原因：用 `Gemini 4 Neon Extra`。
  - 生成、编译、跑测试、冒烟：用 `Carbon Medium`。
  - 运行中发现要改代码时，先保留第一条错误的原文，切回 Neon Extra 修根因，再回 Carbon Medium 继续跑。
- 只在 main 工作区做，不开分支或 worktree，不 `git push`、不 `git tag`。负责人明确要求时才提交，而且只提交自己的改动。
  - 工作区里常有几百个别的会话未提交的文件：不要 `git add -A`，不要 `git reset`，不要 `git checkout --`。
- 不手改 `generated/` 与各语言的 proto 输出树。日志文案用英文；文档、注释、提交说明用中文。
- C++ 的约束：
  - 类成员不放裸指针。
  - 节点代码不新起线程。
  - MSBuild 一律 `/m:1`。
  - `run_cpp_tests.ps1 -Build` 不会重编库，所以先整解。
  - 新增的 `.cpp` 只登记进 `.vcxproj` / `.filters`。
- 失败时不注释断言、不放宽阈值、不跳测试、不连续自动重试。报错如果落在别的会话正在改的文件里，先用 `git diff` 分清归属：不是本单的，原样记录、不修。
- 游戏没上线，数据可以全部清掉。冒烟前按 `docs/rules/stress-testing.md` 清场：Redis、MySQL / TiDB、etcd、Kafka offset 都清。
- 结果在 `docs/PROGRESS.md` **文末追加**：标题写「Gemini：… 验证」，列命令、通过数和失败原文摘要，旧条目不改。日志放 `build/verification/<运行名>/`。
- Java 独立仓不写代码。本仓的 `java/gateway_node` 照常验证。

## 2. 第三方登录（只剩验证，没有要写的代码）

### 2.1 内容

- **小游戏登录**
  - `auth_type` 是 `wechat_minigame` / `douyin_minigame`，走 code2Session。代码在 `go/login/internal/logic/pkg/auth/minigame_providers.go`。
  - K8s 有六个部署参数，两把 AppSecret 走工作负载密钥目录。
- **分配 gate 凭 access token 鉴权**
  - 协议：`AssignGateRequest` 删掉字段 3，新增字段 5 `access_token`。
  - login：`go/login/internal/logic/loginpregate/assigngate_auth.go`，令牌被拒回 status=4，网关映射成 HTTP 401。
  - 网关：网关的冷却键改为令牌摘要。
  - robot：用令牌分配 gate，401 时改匿名重试一次。
  - Unity：`LoginCredential`、`BuildAssignGateRequest`。
- **Apple / Google 身份令牌**
  - `go/login/internal/logic/pkg/auth/idtoken.go`：RS256 + JWKS 缓存。
  - 沙箱 mock：`go/login/cmd/sandbox_mock/idtoken.go`。
  - K8s 有四个参数，没有密钥。
  - 之后账号注销线把令牌改成一次性消费，并在 mock 里加了 `apple_revoke.go`、`passport.go`。这两处以账号注销交接为准。
- **设计与手册**
  - [第三方登录设计](../design/third-party-login-end-to-end-design.md) §二、§十一、§十二。
  - [登录排队](../design/login-queue-2026-05.md) §5 末尾的「2026-11-12 更正」。
  - [沙盒手册](../ops/wechat-qq-sandbox-runbook.md) §G、§H、§I。
- **原始验证条目**：11-20 PROGRESS 瘦身时已移出，原文用 `git show ccf2840b2:docs/PROGRESS.md` 查，标题是：
  - 「2026-11-12 Claude:微信 / 抖音小游戏登录」
  - 「2026-11-12 Claude:小游戏登录接进 K8s 部署脚本」
  - 「2026-11-13 Claude:分配 gate 凭 access token 鉴权」
  - 「2026-11-13 Claude:Apple / Google 登录」

  下面 §2.2 是按现在的代码合并后的版本，以它为准。

### 2.2 验证（在 §4 第 2 步的生成之后跑）

1. **go/login**（工作目录 `go/login`）
   - 先跑 `go build ./...`、`go vet ./...`。
   - 再整包跑：`go test ./internal/logic/pkg/auth/ ./internal/logic/pkg/token/ ./internal/logic/loginpregate/ ./internal/config/ ./internal/svc/ ./cmd/sandbox_mock/ -count=1 -v`。不要只用 11-13 写的 `-run` 过滤：之后账号注销线改过同包的用例。
   - 逐个点名核对这些用例：`TestIdToken*`、`TestRSAPublicKeyFromJWK*`、`TestResolveAuthProvidersIdToken*`、`TestIdTokenProvidersAgainstSandboxMock`、`TestAssignGateAuth*`、`TestValidateAccess*`，以及名字含 `MiniGame` 的。
   - 并发：`go test ./internal/logic/pkg/auth/ -run IdToken -race -count=3`。
   - 最后 `go test ./... -count=1`。
2. **robot**（工作目录 `robot`）
   - `go build ./...`、`go vet .`。
   - `go test -run 'AssignGateWithQueue_UnauthenticatedReturnsTypedError|ReusableAccessToken' -count=1 -v .`。
   - `vendor/` 缺 `golang.org/x/net/websocket` 时，按 WebSocket 交接先 `go mod vendor`，或者加 `-mod=readonly`。
3. **本仓网关**（仓库根）
   - 先跑定向用例：`mvn -B -f java/pom.xml -pl gateway_node -am test "-Dtest=AssignGateControllerTest,AssignGateServiceZoneAdmissionTest,AssignGateServiceAuthTest,AssignGateRequestProtoTest" -Dsurefire.failIfNoSpecifiedTests=false`。
   - 再全量：`mvn -B -f java/pom.xml -pl gateway_node -am test`。
4. **部署契约**
   - `pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`。
   - `k8s_deploy_contract.tests.ps1`、`k8s_client_entry_contract.tests.ps1`。
   - 名字里带「Apple / Google」「小游戏」的用例要逐个看到通过。
5. **客户端**（客户端仓根）
   - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`，期望 0 error。
   - EditMode 跑 `MmorpgClient.Tests.EditMode.Net`。其中 `LoginCredentialTests` 是 7 个方法，参数化展开后 11 个用例。
6. **冒烟**
   - 起 login、网关、`sandbox_mock`，按沙盒手册 §G.3、§H.3、§I.2 的 curl 跑，期望值写在命令注释里。
   - robot 跑一轮登录进场，`login_assign_gate_auth_total{result="authenticated"}` 应递增。
   - 关掉 `LegacyGateLoginEnabled` 之后，不带令牌的 `/api/assign-gate` 应回 401。

## 3. 实名认证与未成年人防沉迷

### 3.1 现状

- 服务端、网关、客户端、K8s 参数、go/payment 的未成年人限额闸，都在 11-19 至 11-20 写完。
- 11-20 实名会话的最后一批在它撞额度前**没写进 PROGRESS**。这一批是：
  - 防沉迷会话账本 `go/login/internal/logic/pkg/nppa/ledger.go`；
  - gate 的到点断开 `cpp/nodes/gate/handler/rpc/minor_play_deadline.{h,cpp}`，单测在 `cpp/nodes/gate/tests/minor_play_deadline_test.cpp`，登记在 `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`；
  - login 的接线与单测。
- 11-21 Gemini 在这一批之上把账本拆成了 16 片，见 PROGRESS 11-21「Gemini（Gemini 4 Neon Extra）：全仓代码完成度终审与四项实缺口补齐」第 2 项。
- 以上全部没有编译，没有运行。
- 负责人要亲自办的手续不归 Gemini，见设计稿 §9：版号、国家系统企业端注册（bizId / appId / secretKey）、渠道登记、出口 IP。

### 3.2 还要写的（Neon Extra）

细节以 [real-name-anti-addiction-handoff-20261126.md](real-name-anti-addiction-handoff-20261126.md)（下称「实名细节单」）为准，这里只列项。下面第 2 项原来的写法保留作对照，已全部吸收进实名细节单 §2.2，两处有出入时按实名细节单做。

1. **改正设计稿正文：已做。** 11-26 实名会话已按现在的代码重写 §5.3–§5.5 与 §8。原来这里写的「会话收尾时 GETDEL 失败、丢一条下线上报是定案」已过时：现在没有 GETDEL 了，End 失败时由账本清扫补下线。旧键名 `login_anti_addiction` 的残留仍要 grep 一遍。
1a. **缺陷修复：已做**（11-26 晚，实名会话）。原来截止时刻一到，清扫就删掉账本条目，login 的兜底踢线随之失效。改动与回归用例见实名细节单 §2.1。
2. **国家系统沙箱 mock：已写**（11-26 晚，实名会话）。下面是原来的要求，留作对照；实现与它的两处出入见实名细节单 §2.2。
   - 位置：在 `go/login/cmd/sandbox_mock` 加三个接口，路径与 `nppa.DefaultCheckURL`、`DefaultQueryURL`、`DefaultLoginoutURL` 相同。报文格式以 `go/login/internal/logic/pkg/nppa/client.go` 为准：报文体用 secretKey 做 AES-128-GCM 再 Base64，请求头签名见 `sign`。
   - 校验：核请求头签名并解开报文；签名错或解不开时，回规范里的错误码。secretKey 只用开发值。
   - 实名结果：
     - 按身份证号确定性派生，同一个号码永远得到同一个 pi。
     - 另留几个固定号码，分别得到「认证中」「认证失败」「未成年」。年龄由号码里的生日算，口径同 `go/shared/antiaddiction`。这样端到端能走到拒绝与到点断开。
   - 上下线上报：收下并逐条回结果。另留一个固定的 ai，让整批失败，用来走重试路径。
   - 单测：照 `idtoken_test.go` 的写法，用真的 `nppa.Client` 打 mock 的 `newMux()`，覆盖成功、认证中、失败、签名错四种。
   - 配置：
     - linux 登录栈 `deploy/login-stack.linux/login.yaml` 加 `AntiAddiction` 段，测试地址 `AntiAddiction.Nppa.CheckUrl` / `QueryUrl` / `LoginoutUrl` 指向 `http://sandbox-mock:18090`。字段见 `go/login/internal/config/anti_addiction.go`。
     - secretKey 只走环境变量。
   - 手册：沙盒手册加一节 §J，写清端到端的步骤。
3. **PROGRESS**：在文末追加一条，列出改动的文件与验证命令。

### 3.3 验证

实名细节单 §3 另列了逐条用例名与冒烟步骤（含真 Redis 7.2 / 8.x 各一轮），两边都要跑。PROGRESS 里按标题 grep 下列条目，每条末尾都有验证步骤：

- 11-19「实名认证与未成年人防沉迷落码」：含清库，`account_directory` 多了一列。
- 11-19「实名与防沉迷接进 K8s 部署脚本」。
- 11-19「未成年人充值限额闸接入 go/payment」，以及「未成年人充值限额闸收尾」。
- 11-19「实名与防沉迷 K8s 接入的三处收尾」。
- 11-20「实名与防沉迷收掉「以后再做」的四处」。
- 11-20「部署替运维维护 domain-service-secrets 的内部调用密钥」。
- 11-21 Gemini「全仓代码完成度终审与四项实缺口补齐」第 2 项。

合起来至少要跑：

1. **Go**：下面每个模块都跑 build、vet、定向用例，最后全量。
   - `go/shared`：`antiaddiction`、`redisslot`。
   - `go/data_service`：`-run RealName`；有一次性 TiDB 时加跑 `TestTiDBAccountRealNameIsWrittenOnce`。
   - `go/login`：`nppa`、`realname`、`loginadmin`、`clientplayerlogin`、`svc`、`config`。
   - `go/payment`：`./internal/core/ -run 'Minor|Gate'`。
2. **C++**：整解之后跑 `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test`，点名核对 `minor_play_deadline` 与 gate 登录拦截的用例。
3. **网关**
   - 定向：`LoginRpcClientRealNameCodecTest`、`LoginServiceRealNameTest`、`LoginServiceTest`、`LoginRpcClientParseTest`。
   - 再全量。
4. **部署契约**
   - `k8s_deploy_contract.tests.ps1`、`k8s_client_entry_contract.tests.ps1`、`k8s_login_auth_provider_guard.tests.ps1`：管实名参数与防误关。
   - domain-service-secrets 那一条点名的契约测试。
5. **客户端**：编译检查，再跑 EditMode 里实名窗的用例。
6. **冒烟**（§3.2 的 mock 写完以后跑），要看到这五种结果：
   - 成年人实名后能进游戏；
   - 未成年人在可玩时段外进不去；
   - 未成年人在时段内进去，到点被 gate 断开；
   - mock 收到了上线与下线上报；
   - payment 按未成年人限额拒单。

## 4. 四条线一遍跑完的顺序

0. **先写完代码（已全部完成，2026-11-26）**：本单 §3.2 已由实名会话（Fable 5.2）写完；剧本审核流水搬库冻结不计失败已由 Claude（Fable 5.2）写完；账号注销交接 §3 第 8 步（Google 按钮文案改官方「使用 Google 账号登录」+ 放宽文本框、核实并替换四张官方品牌原件及来源说明与单测）已由 Gemini 4 Neon Extra 写完于客户端仓工作区；WebSocket 交接无待写代码。现在可直接切 `Carbon Medium` 从第 1 步开始执行（第 9 步客户端测试前需先关掉 Unity 重烘 `QdaoServerSelect.prefab`）。
1. **清场**：按 `docs/rules/stress-testing.md` 清 MySQL / TiDB、Redis（`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`）、etcd、Kafka offset。账号目录与注销加了列和表，旧库一定要清。
2. **生成**（仓库根）
   - `dev.bat gen`：导表，给实名与注销新加的 tip 码发号。
   - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`：用默认配置，连客户端仓一起写，这几条线都有客户端改动。
   - 不用再跑 `go/build.ps1`（11-21 随 goctl 退役删除）：login 转发层是手写的，实名与注销新加的方法（`SubmitRealName`、`ResetAccountRealName`、`RequestAccountDeletion` 等）已在 `go/login/internal/server/`，由 `go/login` 的 `TestGoctlServerWrappersImplementEveryRPC` 核对。
   - 客户端仓 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`（AGENTS §4）。
3. **静态检查**
   - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`、`python tools/scripts/tests/test_cpp_thread_sources.py`、`python tools/scripts/check_log_text_english.py`。
   - 改过的 Go 文件跑 `gofmt -l`，改过的 ps1 用 `Parser::ParseFile` 查语法。
4. **Go**：每个模块按 build → vet → 定向用例 → 全量的顺序。
   - 先跑 `go/shared` → `go/data_service` → `go/login` → `go/payment` → `go/match`。
   - 再跑账号注销与 WebSocket 交接里列的模块：`go/guild`、`go/mail`、`go/rank`、`go/friend`、`go/trade`、`go/player_inbox`、`go/scene_manager`、`go/db`。
   - 最后跑 `robot`。
5. **C++**
   - 先整解：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。
   - 再按各单的 `-Filter` 跑 `run_cpp_tests.ps1 -Build`：`routing_identity_test`、`client_endpoint_test`、`cross_zone_test` 等。
6. **本仓 Java**：`gateway_node` 先跑 §2.2、§3.3 与注销交接列的定向用例，再全量。
7. **部署契约**：本单与两份交接单点名的 `tools/scripts/tests/*.tests.ps1` 全部跑。
8. **真库用例**：有一次性 TiDB DSN 时跑，各条目都写了是哪几条。
9. **客户端**：先编译检查，再跑 EditMode：Net 组，加各单点名的用例。
10. **冒烟**：本单 §2.2 第 6 步、§3.3 第 6 步，以及注销与 WebSocket 交接的冒烟。
11. **记录**：每条线追加一条 PROGRESS「Gemini：… 验证」。

## 5. 已经定案，别重开

- **Apple / Google**
  - 只认一个 ClientId。
  - 不做挑战式 nonce，改为身份令牌一次性消费（11-19 定，见账号注销交接）。
- **分配 gate**
  - 不带令牌的匿名分配，只在 `LegacyGateLoginEnabled` 开着时允许。
  - 令牌被拒回 401，令牌存储故障回 500。
- **实名**：会话收尾时 GETDEL 失败、丢一条下线上报可以接受。
- **小游戏**：转小游戏包上线前再做，走 Unity 7。
- **Java 独立仓**：只登记待做，不写代码。
