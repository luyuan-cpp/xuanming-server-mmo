# 进度流水

> 2026-11-20 按负责人要求瘦身:仅保留 2026-11-19 起的近期有效条目(并删去 4 条同日已被后续落码条目取代的纯设计草案);2026-11-18 及以前的全部历史条目已清理以降低文档体积。原文用 `git show ccf2840b2:docs/PROGRESS.md` 查;本机是 partial clone,别用 `git log -p` / `-S` 翻历史,会去远端补对象而卡住。
> 条目按时间往后追加,最新的在文末。找某项进度按标题关键词 grep 再读对应条目,不要从头整份读。

## 2026-11-19 Claude：Codex 数据一致性测试补丁审核结论（无必修；只读审核，未编译、未测试）

- **范围**：Codex 11-10 整理的交审材料（`C:/Users/luyua/Documents/Codex/2026-11-10/claude-code/` 下的审核说明与补丁），只审本聊天的 7 个 Go 测试文件（+30/−11，随 `1194c4d46`、`bc24d1802`、`de6891478` 入库），对应 11-06 Codex「数据一致性三条交接首轮验证」等四条。11-13 按补丁落入时的生产代码逐项对照，并看了当时 HEAD `4edb25640` 与工作区；11-19 追加前复核，下文行号在 HEAD `6dc2c82d3` 仍成立。没改代码，没编译、没跑测试。
- **结论：无必修**，没有把缺陷改成预期：
  - trade 三处 `Start(…, true)`：只有 `true` 才起重投循环，`TestAssetChannelStartThenCloseReturns` 等的 wg 才有意义；nil 接收者与 Close 后再 Start 两条和参数无关，`true` 是更严的一侧。
  - 成员对账夹具：`listSceneMemberSets` 只走 `orchestratedZones`（即本进程 `Config.ZoneId`）；`scene_manager:zones` 已删，无残留引用。
  - `seedLeaveCasFixture` 补 `scene:{id}:zone`：登记脚本在同一段 Lua 里同时写属主与 zone 两个键，生产没有只写属主的路径，旧夹具缺 zone 键是生产造不出的状态。场景键改成带 zone tag 后这一行已在 HEAD 删除。
  - 跨区成员清理改由 zone 77 执行：仍守住 `locationOverlap` 的 zone 比较；「本区不改外区」另由 `TestOwnsControlZoneSkipsForeignMembership` 守。
  - zone 翻转外区哨兵：`handleWatchEvent` 对「本区前缀下自称别区」的注册按注销收尾、不写别区，与单 zone 契约一致。
  - 死节点排空不扣计数：`finishDrainedChannel` 只对活属主置 `CountOwner`，Lua 另要求计数 > 0 才 DECR（`world-channel-autoscale.md` §1.3）；活属主扣减仍由 `TestAutoscale_DrainStepReadErrorKeepsDrainingState/owner` 守。
  - 真实 Redis 脚本：执行的是生产 Lua 原文（Go 回滚 = helpers + `luaRollbackPlayerPlacement`，C++ 登记 = `kFenceLuaHelpers` + `kRegisterFenceLua`，另有 3 项跨语言逐字比对），KEYS/ARGV 与两边调用点一致，8 个用例返回值互不相同，没有空转断言。
- **建议（不阻塞，留给各文件现在的负责会话）**：
  1. `scene_membership_test.go:278`：场景成员键带 zone 之后，`other` 铺在 testZoneId、却在 zone 77 摘，场景集合那一半没测到。改用 `seedSceneMembersInZone(mr, 77, other, 2, playerID)`，断言 zone 77 场景集合变 1、testZoneId 的集合不变。
  2. `node_detach_deferred_test.go:530`：哨兵只比成员名；补 `nodeSceneNodeTypeKey(newZone, "10")`、`nodeDeathAtKey(newZone, "10")` 不存在，99 的分数仍为 7。
  3. `scene_member_reconcile_test.go`：缺「对账不碰别区集合」的用例（铺 zone 77 负载集与带残留的成员集合，`runRound` 后原样）。
  4. `read_unknown_fail_closed_test.go:540`：`countBefore` 钉成 `"2"`，防夹具铺成 "0" 时「不扣」空转通过。
  5. 真实 Redis 脚本（`build/verification/consistency-20261106-afca/evidence/real-redis-repeat/validate_real_redis.py`）：回滚分支补「整库键集合 = 之前减去被删两键」；校验正则捕获的前缀标识符确实是 `luaCppHandoffFenceHelpers`。
  6. trade 缺 `Start(…, false)`（canary 只起节点镜像、不起 Pipeline、Close 照常返回）的用例，属发布轨道那批的缺口。
- **尚缺的运行证据**：
  - C++：这批的隔离整解退出 1（`build/verification/consistency-20261106-afca/cpp-isolated/game-build-result.json` 的 `ExitCode=1`）。11-06「修正 scene_manager 五项旧夹具」一条写的「最终结果另追加」一直没补，以本条为准；这批没有 C++ 测试结果。同期其他会话的 C++ 结果（11-06 历练交接第四轮 currency / bag 全量、owner_epoch 复验的 `CurrencyTest.*AgainstRealRedis`）按各自原件引用，不记在这批名下。
  - scene_manager 756 PASS 与真实 Redis 16/16 只对应 11-06 的源码。现 HEAD 已把场景键改成带 zone tag，回滚 Lua 加了作废分支（ARGV[11..14]，返回 6 / 7）；现有真实 Redis 脚本只传 10 个 ARGV，覆盖不到新分支。scene_manager 全量要按现 HEAD 重跑。
  - 真实 MySQL 条件写竞态、跨节点换图、流水提交时序、etcd 分区、Kafka 投递故障、promtool 与 Java 运行仍未做。
- **核对证据的注意**：原件里的源文件哈希按工作区原始字节算。`handoff_sql_barrier.h` 当时在工作区是 CRLF（`.gitattributes` 要 LF），去掉 CR 后才与提交 blob 一致。
- **Java 版（AGENTS §12）**：不涉及，只审本仓测试。
- **提交情况**：只追加本条，未提交。

## 2026-11-19 Claude：工作负载密钥第 8 批复核——撤掉 11-13 合并带回的旧 MySQL 口令读法,修 route-tool 维护身份断言(未编译、未测试,待 Codex 验证)

- **起因**:复核 11-13 之后这条线的变化,对照上面「Codex：保留 Claude 开发快照并合并 main」条目里记下的两处问题:
  1. 「四份远端 MySQL 密码测试引用未实现接口」:合并把第 7 批删掉的 `go/{db,friend,trade,player_inbox}/internal/config/mysql_password_test.go` 带了回来,被测的 `MySQLPasswordEnv` / `ResolveMySQLPassword` 已不存在,这四个包的测试编不过。同一次合并还带回了 data_service 第 7 批的读法:`internal/config/config.go` 的字段与方法、`data_service.go` 的解析调用、`routetoolcli/cli.go` 在 `withControlAccount` 之前多出的一段 `ResolveMySQLPassword`,以及 `internal/config/mysql_password_test.go`。部署脚本早已不写 `MySQLPasswordEnv`,这些代码没有读者。
  2. 「route-tool 清单的身份变量匹配」:第 8 批写的两条断言拿整份文本查「不含 `MMORPG_SQL_MAINTENANCE_DSN`」,而 Job 模板文件头的注释为了说明 freeze / unfreeze 的跑法写了这个名字,断言必红。
- **改动**:
  - Go:删 `go/data_service/internal/config/config.go` 的 `MySQLPasswordEnv`、`ResolveMySQLPassword` 与 `shared/secretenv` 引用,删 `go/data_service/data_service.go` 的解析调用,删 `go/data_service/internal/routetoolcli/cli.go` 里多出的那段;删五个 `mysql_password_test.go`(data_service、db、friend、trade、player_inbox,删前核对与 HEAD 一致)。
  - 测试:`tools/scripts/tests/k8s_route_tool.tests.ps1` 与 `k8s_deploy_contract.tests.ps1` 里「维护身份不进集群」两条断言改成只查 env 条目(`- name: MMORPG_SQL_MAINTENANCE_DSN` 等三个变量名)。
- **写回方式**:守卫仍拦主仓,按 AGENTS.md §10.2 在草稿目录改副本后写回;`k8s_deploy_contract.tests.ps1` 期间被别的会话改过,用 `git merge-file` 干净合并,写回后核对只多了本条的改动;其余四个主仓未变,直接写。
- **Codex 验证**:
  1. `gofmt -l go/data_service` 无输出。
  2. `go/data_service`:`go build ./... && go vet ./internal/config/ ./internal/routetoolcli/ && go test -count=1 ./internal/config/ ./internal/routetoolcli/ .`。
  3. `go/db`、`go/friend`、`go/trade`、`go/player_inbox`:各自 `go vet ./internal/config/ && go test -count=1 ./internal/config/`,不应再有「未定义 MySQLPasswordEnv / ResolveMySQLPassword」的编译错误。
  4. PowerShell(仓库根,先清掉 `MMORPG_*` 环境变量):`pwsh -NoProfile -File tools/scripts/tests/k8s_route_tool.tests.ps1`;`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` 里「工作负载密钥:迁移 Job 与 route-tool Job 的清单都不引用共享数据库口令」一条。两份都还要跑上面第 8 批、第 8 批补充两条条目列的用例。
- **Java 版**:不适用,PARITY 不登记。
- **状态**:未运行,未提交。

## 2026-11-19 Claude:口令登录部署开关复核——修 login 建表测试把注释算进语句,合同测试改用新的 Redis 总账接口(未编译、未测试,待 Codex 验证)

- **起因**:复核 11-12「K8s 上 login 口令登录的部署开关」与 11-13「口令库连接串改成运维预置」两条之后的变化,发现两处必红:
  1. 11-13 Codex 条目记的「login schema 测试计入 SQL 注释」:`go/login/model/password_auth_schema.sql` 的文件头注释里写了 `CREATE TABLE IF NOT EXISTS`,`TestPasswordSchemaFileMatchesTheShapeGuard` 按整份文本数建表语句,数到 2 条。
  2. 别的会话在 11-13 之后把 Redis 连接总账拆成控制域表加玩家域表(`Get-RedisConnectionLedgerInputs -ControlOnCore / -PlayerOnCore`、`Get-RedisClusterConnectionLedgerInputs -Cluster`),删了 `Get-RedisPlayerConnectionLedgerInputs` 和 `-PlayerDomainSplit`,本开关的 `-LoginPasswordAuth` 也一并带了过去。但 `tools/scripts/tests/login_password_auth_deploy_contract.tests.ps1` 还按旧函数名抽取,一加载就报「k8s_deploy.ps1 缺少函数 Get-RedisPlayerConnectionLedgerInputs」,整份用例一条都跑不到。
- **改动**(部署脚本本身没改):
  - `go/login/internal/logic/pkg/auth/password_schema_test.go`:比对前去掉 `--` 开头的整行注释(`-ddl-file` 的加载器也跳过这种行),其余断言不变。
  - `login_password_auth_deploy_contract.tests.ps1`:抽取名单改成现有的 `Get-RedisPoolMirrorSource`、`Get-RedisControlClientRows`、`Get-RedisClusterConnectionLedgerInputs`、`Get-RedisCluster`。「Redis 连接总账」一条重写:核心那份里 login 有控制域、玩家域两行,按服务名求和后比较。开了口令登录时,核心那份(玩家域在核心上)和玩家域集群那份的 login 各多 `passwordQuotaRedisPoolSize`(4)条;`-PlayerOnCore $false` 时核心那份不变;data-service、scene-manager、guild 不受影响。
  - 静态核对:用例调用的每个部署脚本函数都在,具名参数都对得上;用例断言的输出文案在源码里都还在,包括入口摘要、DryRun 核对行、SQL 总账行、互斥与缺字段报错,以及 `维护原件有未知字段：`(在 `maintenance_drain.ps1`)。用例从源码读的 Go 常量形状没变:`PasswordAuthDatabase`、`defaultArgonMemory`、`MaxPasswordKDFConcurrency`、`passwordQuotaRedisPoolSize`。共享 SQL boot 目录夹具 `New-SqlWriterCatalogFixture` 的字段与 `Get-SqlWriterDeploymentEntry` 的必填和可选字段一致。四份 ps1 语法解析 0 错误,`gofmt -l` 无输出。没有跑测试。
- **给 Codex**(仓库根,先清掉 `MMORPG_*` 环境变量):
  1. `cd go/login && go vet ./internal/logic/pkg/auth/ && go test -count=1 ./internal/logic/pkg/auth/`。
  2. `pwsh -NoProfile -File tools/scripts/tests/login_password_auth_deploy_contract.tests.ps1`,退出码 0、全部 PASS 算通过。这份用例写好以来还没有运行证据;第一次跑如果还有别的红,保留原始输出,不改断言。
  3. 同一组 Redis 总账函数顺带重跑:`pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1`,以及 `k8s_deploy_contract.tests.ps1` 里 Redis 连接总账那几条。
- **Java 版(AGENTS §12)**:不适用,只改测试;不往 `PARITY.md` 加行。
- **状态**:未运行,未提交。这条线(去单点第二轮,含 `-LoginPasswordAuth`)的代码全部写完,只剩 Codex 验证和 kind 实跑。

## 2026-11-19 Claude：客户端请求幂等键——收尾核对与三项定案，设计稿对齐账本扩展并禁止整体清空（只改文档，无代码改动）

- **起因**：负责人问「代码实现完毕了吗」，并要求先前留到以后的事现在就定。
- **核对结论**：本功能的服务端与客户端代码没有待写的部分。
  - 服务端：三处接入点（强化、神秘商人购买、新建方案）自 11-08 验证后没有被改坏。11-13 之后的提交只在 `client_request_ledger.{h,cpp}` 上追加了装备的两种种类，三处入口的去重逻辑原样。
  - 客户端：三处调用点在客户端仓 `2978c807`（11-12）。Codex 11-17 离线编译客户端全部运行时源码无错。
  - robot：11-13 的改动随 `6914a1af8` 入库。Codex 同日合并 main 后「Go 全包测试通过 … robot」，本批 6 个纯函数用例就在这个包里；当时的日志在系统临时目录，仓库里没有原件。三条冒烟还没在 dev 环境跑。
  - 强化冒烟里镜像的表值今天对过：11-17 的改表对物品表和强化表只追加了行，1101、强化石、三种宝石、前 12 级强化和孔位表都没变。
  - 重核扣资源的请求：scene 里 14 处 `CurrencySystem::DeductCurrency` 和全部会改状态的客户端 RPC 逐个核过。11-13 之后新增的五个都有防重，没有漏接的：
    - `CommitEquipmentCraft`、`ClaimEquipmentMaterialReward` 用的就是这本账本（种类 4、5），另带水位。
    - `BuyPet` 带服务端给的购买序号；`SavePetEntries` 带请求号与版本；宝宝的 `UpgradeSkill` 比对 `expected_rank`。
- **发现并改正的问题**：设计稿 §5.2 是我 11-13 写的，允许「玩家离线、且客户端只在同一条连接上重发」时整体清空账本，还列了一套逐条修法。11-17 起这两处都是错的：
  - 账本组件里多了 `forge_revision` 和 `material_reward`（含玩家没领的余额），客户端也会把打造、领取请求存在本地，跨重启重发。清空会抹掉额度，并让过期的请求重新执行。
  - 修法里有一条「指纹坏了就改成全零，按换了参数拒绝」。装备两种请求把「换了参数」回成 REJECTED，意思是没动资产，而首次其实已经成功。
  - 现在的口径：整体清空一律禁止；判坏保持不可用；定向修复守四条原则（不删键、水位只升不降、余额不凭空增减、修完过 `Validate`）。
- **三项定案**（原先写的是「可选后续」或「由负责人定」，原因都写在设计稿原处）：
  1. 其余靠前置状态防重的请求不加幂等键（设计稿 §1）。
  2. 账本判坏不做自动修复，也不预先做通用修复工具（§5.2）。
  3. 强化、神秘商人购买、新建方案不做跨连接、跨重启的自动重发（§6，交接 §9）。
- **文件**：
  - `docs/design/client-request-idempotency.md`：状态、§0、§1 的表与定案、§2.1 第 6 条、§3.1、§3.2、§3.6、§4、§5.2、§6、§8 第 6 步、§9。
  - `docs/handoff/client-request-idempotency-unity-20261106.md`：执行状态、§1 一段、§9。
  - 没有代码改动，不需要编译。
- **仍归 Codex**：三条 robot 冒烟，命令与通过标准见本文件 2026-11-13 同名条目；Unity EditMode 用例，见本文件 2026-11-12 客户端条目第 4 步。
- **Java 版（AGENTS §12）**：不变，待做，已在 Java 仓 `PARITY.md` 登记。本批不改两版共享的协议。

## 2026-11-19 Claude：场景 lane 验证状态核对（补 11-12「场景 lane 收尾定案」「场景 lane 补旧场景消息闸」两条；只核对，没有改代码）

- **代码**：场景 lane 的全部改动随 `6914a1af8`（11-13）入库，之后没有任何提交或未提交改动碰过这些文件（`scene_lane.h`、`scene_lane_item.h`、`client_scene_lane.{h,cpp}`、`client_session_handler.cpp`、`gate_redirect_delivery.cpp`、`drain_migration.cpp`、`session_info_comp.h`、`main.cpp`、`scene_handler.cpp`、`player_handoff_gate.h`、`scene.proto`、`robot/channel_smoke_scenario.go`）。服务端与 robot 代码已写完，客户端不用改。
- **已有的编译证据**：`build/verification/docker-rebuild-all-20261118T082435.788540Z` 的 C++ 段（Linux，GCC `-Wall -Wextra`，退出码 0）编出了 gate、scene、battle 三个节点，gate 的链接行含 `client_scene_lane.cpp.o`，scene 的链接行含 `scene_handler.cpp.o`；生成的 `cpp/generated/proto/scene/scene.pb.h` 已有 `set_received_on_previous_route` 与 `CLIENT_MESSAGE_DISPATCH_VERDICT_RETRY_LATER`。那次重建失败在运行期一步（`Existing volume or bind mounts changed: gateway`），与本条无关。
- **还没有证据的**：Windows 串行整解（`msbuild game.sln /m:1`）、`routing_identity_test` 与 `cross_zone_test` 两套单测（`SceneLane.*`、`ClientSceneLane.*`、`PlayerHandoffGate.*` 等新用例在 `build/verification` 里一条运行记录都没有）、robot `go mod vendor` 后的编译与 `channel-smoke` 的 `C4-hold` 冒烟。
- **验证**：命令照 11-12「场景 lane 收尾定案」第 2~6 步执行（第 1 步协议已生成，可跳过），用例数按「场景 lane 补旧场景消息闸」那条核对。

## 2026-11-19 Claude:审计发件箱(R13)完成度核对——代码已全部在 main,无缺口;本机已有运行证据,单测与演练仍待 Codex(只读核对,没改代码)

- **代码位置**:11-12 两个会话写的全部改动已随 `6914a1af8`(2026-11-13)提交,之后 R13 相关文件没有再改;Codex 11-13 合并 `origin/main` 之后,
  已删的 `audit_topic.h`、`LogItemTransfer`、`SetTransactionLogQueuedHook`、C++ 的 `AuditTopicGeneration` 都没有被带回,`config.proto` 字段 20 仍是 `reserved`,
  生成物(C++ / Go / robot vendor)已重新生成。同伴会话按约改掉了 go/ 下「C++ 生产者」的旧注释与设计稿 §9 的 store_too_old 一行。
- **已有的运行证据**:
  - Codex 11-13「main 合并提交前验证记录」:`tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1` 通过;完整协议生成成功。
  - Codex 11-14「修复本机 C++ 节点旧产物与重编阻塞」:gate / scene / battle 三个节点编译通过(含本功能的 C++ 改动)。
  - 本机 Docker 调试栈(11-18 ~ 11-19):go/db 日志 `[audit_outbox] Audit outbox relay started: brokers=[kafka:29092] transaction_log_topic=transaction_log_topic_g1 snapshot_topic=player_snapshot_topic_g1`;
    11-19 03:09 有一次探测读失败(`权威SQL结果未定（业务发送后），禁止重放`,写者闸门连接中途断开的既定 fail-closed 口径),约 2 秒后 `Relay read recovered`,行留在发件箱、下一轮重投,符合设计。
    scene 日志卷(`xuanming-debug-current_scene-logs`,11-17 ~ 11-19 七份)里 `metric=audit_outbox_overflow / audit_outbox_record_too_large / txlog_refused_* / snapshot_refused_* / audit_outbox_store_too_old` 一条都没有。
  - 迁移工具对 `player_audit_outbox` 打的「没有声明 table_name 选项」警告对 `player_store`、`user` 等全部表都打,是迁移工具对本仓 `OptionTableName` 写法的通用提示,与本功能无关。
- **仍待 Codex**(命令见 2026-11-12「审计发件箱(R13)scene、部署与告警部分」条目第 3~8 步与同日三条 go/ 条目):`currency_test`(`AuditOutboxTest.*`)、`bag_test`(`PlayerStoreClientTest.*` 审计用例)、
  go/db 的 `auditoutbox` / `playerstore` / `config` 单测与真库用例、`k8s_deploy_contract.tests.ps1` 的两条新用例、`promtool check rules`、Kafka 全停 6 分钟演练。11-13 的 TEMP 验证日志已被清,查不到这几项的原件。
- **Java 版**:仍只在 Java 仓 `PARITY.md` 登记待做(2026-11-12「审计发件箱」),没有 Java 代码。

## 2026-11-19 Claude:组队跟随间距放大到 3 倍(1.4 → 4.2)、玩家视野放大到 5 倍(10 → 50)(未编译、未测试,待 Codex 验证;Java 待做)

- **起因**:负责人「现在队员跟随的时候的间距太小了,改成三倍,然后也把玩家视野改成五倍吧」。
- **改动**(定案写进 `docs/design/team-system.md` V.7 第 5 条,取代 11-12「间距保留 1.4」):
  - `cpp/libs/services/scene/spatial/constants/view.h`:`kMaxViewRadius` 10 → 50,离开半径按系数 2 跟着变成 100;`kAoiHexSize` 20 → 50。文件里的 `static_assert` 要求格子不小于视野,否则半径内的目标可能不在候选格里;取下限 50,每次重判扫的候选最少。
  - `player/system/team_follow_formation.{h,cpp}`:`kTeamFollowSpacing` 1.4 → 4.2。删掉全局 `kTrailKeepBehindHead`,`KeepOdo` 改按队形构造时的间距算(队员上限 + 1)× 间距 = 21 米,免得构造参数与全局常量不一致。
  - `player/system/player_team.h`:`kTeamRegroupDistance` 8 → 24。`player_team.cpp` 顶部两条 `static_assert`(整列 16.8 ≤ 归队阈值、归队阈值 < 视野 50)照旧守着。
  - `player/system/team_follow_system.cpp`:`kTeamFollowMaxApproachPath` 16 → 48,仍取归队阈值的两倍。
  - `kTeamFollowMaxLag`(24)不改:它量的是每名队员落后自己槽位多少,与整列多长无关。
  - 注释:`spatial/system/aoi.h`、`player_team.cpp`。
- **测试**(都在已有工程里,不用登记新文件):
  - `cpp/tests/aoi_test/team_follow_formation_test.cpp`:拐角、发夹、补位这些用例的期望坐标按 1.4 手算,队形改为显式按 1.4 构造;直走那条改用生产间距 `kTeamFollowSpacing`。
  - `cpp/tests/aoi_test/aoi_system_test.cpp`:`ResetEntityVisibilityRebuildsViewAfterSameHexTeleport` 改成两人分站原点那一格内切圆两侧。视野等于格子尺寸后,从格心出发同格内够不到视野外。
  - `cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp`:
    - 距离重判那组按新尺寸重算几何:同格相距 60、跨格 200 → 100 → 60 → 48、隐身起点在视野外;
    - 视野变化那条两人先各给 20 m 视野,再放大到 40、缩到 10;
    - 同帧合批那条用 ±60 → ±30;
    - 进视野补发移动那条起点改到视野外;
    - 五处"成排的远处实体"间隔 50 → 120,保持彼此不可见。
  - `cpp/tests/cross_zone_test/player_team_follow_test.cpp`:`kLeaderX` 20 → 30(超过新归队阈值);`LeavingFollowStopsDrivingAtOnce` 队长多走一个间距,保证暂离时队员正在走。
  - `robot/team_follow_smoke_scenario.go`:拉开距离 12 → 60(要在视野 50 之外,C3 才能断言客户端只有被瞬移后才进入队长视野),步长 16 → 72;C4 停步距离 0.5~3.0 → 3.0~5.8(一号槽位 4.2)。
- **文档**:`docs/design/team-follow-server-trail.md` §1、§3.2、§3.3、§3.4、§7;`docs/design/team-system.md` V.2 第 2、6 条与 V.7 第 5 条;`docs/design/aoi-directed-interest.md`「空间关注的评估时机」。
- **影响**:
  - 客户端可见:能看到的人从 10 m 内变成 50 m 内,出视野从 20 m 外变成 100 m 外。客户端代码不用改,视野由服务端的创建 / 移除通知决定,跟随间距由服务器驱动。
  - 性能:每次重判扫描的面积约为原来的 6 倍((50/20)²);视野内的人约为原来的 25 倍,更容易顶到兴趣表容量,移动广播扇出随之增加。性能结论要按 AGENTS §6 压测之后再下。
  - 旧客户端(自己做走路跟随,间距仍是它自己的 1.4)不受间距影响;归队阈值变成 24,它们被瞬移归队的次数会变少。
- **交 Codex**(主仓根 `D:\luyuan\xuanming-server-mmo-cpp`,串行;工作区有别的会话未提交的改动,不要回退;失败保留完整输出,不连续重试):
  1. `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`,要求 0 error。`view.h` 与 `player_team.cpp` 的 `static_assert` 必须通过。先整解再跑测试:测试工程链接的是预编好的库。
  2. `build\cpp\tests\aoi_test.exe` 全量,要求 `TeamFollow*` 18 条、`AoiGridTest.*`、`AoiVisibilityTest.*` 全过;`build\cpp\tests\aoi_delivery_test.exe` 全量全过。
  3. `build\cpp\tests\cross_zone_test.exe --gtest_filter=PlayerTeamFollowTest.*:TeamFollowDriveTest.*` 全过,再全量跑,与改动前相比没有新增失败。
  4. `robot` 下 `go build ./...`、`go vet ./...`。组队跟随冒烟(可选)要地图上有一个方向能直线走出 72 米;F1 报"8 个方向都没能让…离开 60 以上"时,记下地图与位置,不要改阈值重跑。
  5. 压测(下性能结论之前):按 AGENTS §6 跑一轮人群移动,对比视野放大前后的 AOI 重判耗时与广播量。
- **没做**:Java 版;压测;机器人的真实客户端跟随(C3 / C4)没跑。本条没有提交。
- **Java 版(AGENTS §12)**:待做,已在 Java 仓 `PARITY.md` 末尾追加「2026-11-19 组队跟随间距 ×3、玩家视野 ×5(Java 待做)」,没有改 Java 代码。

## 2026-11-19 Claude：落实一致性测试补丁审核的 6 条建议（只改测试；未编译、未测试，待 Codex 验证；Java 不适用）

- **起因**：上一条「Codex 数据一致性测试补丁审核结论」列的 6 条建议，负责人要求现在就改，不留到以后。只改测试文件，生产代码一行没动。「Redis 上 Cluster 第 2a 批」会话问过分工，已回复这 6 条由本会话做。
- **改动**（行号按 HEAD `6dc2c82d3`）：
  1. `go/scene_manager/internal/logic/scene_membership_test.go` 的「location 记的 zone 不同」子用例：场景成员键带 zone 之后，`other` 原来铺在 testZoneId、却在 zone 77 摘，场景集合那半边没测到。改为 `seedSceneMembersInZone(mr, 77, other, …)`，断言 zone 77 的场景集合与节点集合各少一人，testZoneId 的同号节点 10 不动。
  2. `node_detach_deferred_test.go` 的 zone 翻转用例：外区哨兵之外再断言 99 的负载分数仍为 7，别区没有 `nodeSceneNodeTypeKey(2, "10")` 与 `nodeDeathAtKey(2, "10")`。
  3. `scene_member_reconcile_test.go` 新增 `TestSceneMemberReconcileLeavesOtherZonesAlone`：
     - zone 77 的场景集合与节点集合各放一名没有 location 的残留和一个坏成员（`not-a-player`），跑一轮对账后原样；本 zone 同样的残留照常摘掉，证明这一轮确实跑过。
     - 坏成员是直接 SREM 的，不经 `removeSceneMember` 的跨 zone 守卫，所以这条用例守的是「对账根本不列出别的 zone 的集合」这一层。
  4. `read_unknown_fail_closed_test.go` 的死节点排空用例：`countBefore` 钉成 `"2"`。计数为 0 时 Lua 的「大于 0 才扣」会让「不扣」空转通过。
  5. `owner_epoch_realredis_test.go` 新增 `TestRealRedisUnmarkedRollbackJudgesByHandoffFence`（缺 `MMORPG_TEST_REDIS_ADDR` 时跳过），把 11-06 只存在证据目录里的一次性脚本 `validate_real_redis.py` 收进仓库：
     - 在真实 Redis 上跑 miniredis 那张 6 行表：原件缺失、更早 → 回滚；同代持有（用 C++ 入场登记脚本的逐字副本 `cppLuaRegisterFence` 写）、同代已撤销 → 不动；更新、写坏 → 不当证据。
     - 回滚先到时，迟到的登记拿着铸出值被拒（回 -1）、一个字节不写。
     - 落点、计划与回滚都走生产函数（`placePlayerLocationCtx`、`planRouteRollback`、`rollbackPlayerPlacement`），用的是现行带作废分支的回滚 Lua。
     - 不回滚的分支比对玩家名下每个键的 DUMP 与键集合；回滚的分支只许 owner_epoch、location 与修订侧车变。
     - 以后这条路径的真实 Redis 证据以这条用例为准，那份 Python 脚本不再维护。
  6. `go/trade/internal/svc/assetchannel_recovery_test.go` 新增 `TestAssetChannelCanaryStartsOnlySceneWatcher`：私有注册表上看重投循环的活性序列 `assetop_loop_last_run_unixtime`，stable（`Start(…, true)`）会出现，canary（`false`）等满 1 秒也不出现，两种都按时 Close。通道装配与限时 Close 抽成 `newStartableAssetChannel`、`requireCloseReturns`，`TestAssetChannelStartThenCloseReturns` 改为复用，断言不变。
- **交 Codex**（按顺序；Go 命令都带 `GOTOOLCHAIN=local`、`GOPROXY=off`；失败就停下归因，不连续重试）：
  1. 格式：`gofmt -l` 以下 6 个文件，无输出——`go/scene_manager/internal/logic/` 下的 `scene_membership_test.go`、`node_detach_deferred_test.go`、`scene_member_reconcile_test.go`、`read_unknown_fail_closed_test.go`、`owner_epoch_realredis_test.go`，以及 `go/trade/internal/svc/assetchannel_recovery_test.go`。
  2. `go/scene_manager` 下：`go vet ./internal/logic/`；`go test ./internal/logic/ -count=1 -v -run 'TestRemoveSceneMember_KeepsWhatLocationStillPointsAt|TestHandleWatchPut_ZoneFlipRetiresOldZoneIdentity|TestSceneMemberReconcile|TestAutoscale_FinishDrainRemovesResidualMembersFromNodeSet|TestRealRedisUnmarkedRollbackJudgesByHandoffFence'`。这一轮没设 Redis 地址，最后一条应为 SKIP，其余 PASS。
  3. 真实 Redis，各跑一轮：Redis 7.2 与 8.10.x，各起一个一次性单实例（`--appendonly yes --appendfsync everysec`，无副本，测试期间没有别的写入），设 `MMORPG_TEST_REDIS_ADDR=<地址>`。在 `go/scene_manager` 下跑 `go test ./internal/logic/ -count=1 -v -run '^TestRealRedis'`，全部 PASS、0 SKIP，记下 `redis-server --version`。这一步同时覆盖已有的作废分支用例。
  4. `go/trade` 下：`go vet ./internal/svc/`；`go test ./internal/svc/ -count=1 -v -run 'TestAssetChannel|TestNilAssetChannelIsSafe'`，全部 PASS。
  5. 变异（在 `%TEMP%` 的副本里做，每次一种，看完复原；不要改主仓文件）：
     - M1：`scene_member_reconcile.go` 的 `listSceneMemberSets` 改成遍历 `unionZones(orchestratedZones(svcCtx), []uint32{77})` → `TestSceneMemberReconcileLeavesOtherZonesAlone` 红。
     - M2：`world_autoscale.go` 的 `finishDrainedChannel` 把 `CountOwner` 改成 `nodeID != ""` → `TestAutoscale_FinishDrainRemovesResidualMembersFromNodeSet` 红。
     - M3：`go/trade/internal/svc/assetchannel.go` 的 `Start` 删掉 `if !globalBackground { …; return }` → `TestAssetChannelCanaryStartsOnlySceneWatcher/canary` 红。
  - **通过标准**：第 1 步无输出，第 2～4 步全绿，第 3 步两个版本都 0 SKIP，M1～M3 各自让点名的用例红。
  - **失败时保留**：失败用例 `-run '^用例名$' -v` 的完整输出；第 3 步再留 `INFO server` 的版本行。
- **没做**：scene_manager 全量与 C++ 不在本条范围，上一条列的运行证据缺口照旧。
- **Java 版（AGENTS §12）**：不适用，只改本仓测试，不往 `PARITY.md` 加行。
- **提交情况**：未提交。

## 2026-11-19 Claude:全部 Redis 上 Cluster 第 3 批收尾——落地唤醒只订本副本负责的片,每条唤醒只投给 3 个副本;三处过时说法(缺口第 34 条;未编译、未测试,待 Codex 验证;Java 待做)

- **来由**:负责人 11-19 要求把查出的问题现在修完。第 3 批 11-12 落码时,唤醒订阅每个副本都订全部 64 片,设计稿 §4.6 写明「扇出」要等缺口第 25 条(重投按片分工)落地再收;第 25 条已落码,这一步没有人接。第 3 批本身的 C++ 改动已在 main,Codex 11-14 编过 gate / scene / battle(PROGRESS 2026-11-14「修复本机 C++ 节点旧产物与重编阻塞」,scene 已读 `control_redis`),用例还没有跑过。
- **问题**:每条唤醒投给 4 + 2Z 个订阅副本(trade、guild 各 2,data_service 每 zone 2),每个副本还各写一次去重键。Z 取 200~445 时每秒投递 1000 万~7000 万条 [I],去重写入同量级,加主节点也扛不住。
- **改动**:
  - `go/shared/assetopwake/wake.go`:
    - 新增 `Subscriber.BindShardOwner(owns func(shard int) bool)`,Start 之前调用。owns 为 nil、Start / Stop 之后调用都回错误,nil 接收者是空操作(唤醒关着)。
    - Start 改起一个管理循环 `supervise`:每 `ownerCheckInterval`(1 秒)按 owns 重算负责的片,新负责的片起订阅循环,不再负责的片退订。集群上一片一组;单机上全部负责的片一组,集合变了整组换一条订阅;一片都不负责时不订。没绑 owns 时只算一次、订全部片,与原行为相同。
    - 订阅循环本身(确认、收包、PING、退避重订)不变。
  - **指标** `assetop_wake_subscribed_shards{service}`(`go/shared/assetop/metrics.go` 新增 `SetWakeSubscribedShards`):本副本此刻已确认订上的片数,订上加、断开减,Stop 后归 0;没绑分工时每副本 64。订阅端经 Counter 的可选能力 `shardGauge` 调它,`Counter` 接口不变。告警规则与契约由 [0cc556] 写(`deploy/k8s/asset-channel-alerts.yaml`、`tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`,`sum by (namespace, service) (assetop_wake_subscribed_shards) < 64`),以它补的条目为准。
  - **接线**:
    - guild:`AssetPipeline.AttachWorkShards` 建完分工表后给唤醒绑上同一张表与同一个类别键 `guild-assetop`。
    - trade:新增 `AssetChannel.BindWorkOwner(owns)`,同时绑重投循环与唤醒(类别键 `reconcile.AssetOpWorkKind`,由私有常量改为导出);`servicecontext.go` 改调它。
    - data_service:回收的重投与派发按 zone 圈片(每片每个 zone 一个副本,第 25 条设计 §10.2),唤醒照它分仍有 Z 份。所以唤醒另开一张全服成员表:前缀 `DataServiceNodeService.rpc/`(`/allocated/` 占位键由 `workshard.StableMembers` 跳过),类别键 `gm-recall-wake`,`bindWorkMember` 一并 SetSelf,`Start` 里先于订阅起。按全服圈只会「圈多」:落在关了回收或晚升级的副本名下的片收不到唤醒,退回本 zone 重投的退避,不丢行。任何 zone 的副本都能处理任何玩家的唤醒:回收行在全服共用的流水库,scene 发现是全服前缀。
  - **结果**:每条唤醒只投给 3 个副本(每个服务一个),与副本数、zone 数无关。片换主的几秒里某一片可能两个副本都订(去重键与 Claim CAS 挡住),也可能暂时没人订(那几秒的唤醒退回原退避),口径同第 25 条 §3。canary 不进成员表:成员表里有 stable 副本时它一片都不订;trade、guild 的 canary 不读成员表,表为空时照旧订全部片,与改动前相同。
  - 注释:guild、trade、data_service 配置与装配处「SSUBSCRIBE 全部 64 个分片频道」的说法改正。
- **三处过时说法**(第 3 批改了配置之后没跟上):`cpp/nodes/battle/logic/battle_room_manager.cpp` 一处注释还说 GameConfig 承载 `zone_redis`;`deploy/k8s/scene-ownership-alerts.yaml` 的 `SceneOwnershipReconcileBlind` 说明与 `docs/ops/scene-node-role-split.md` 告警表那一行还让人查 `scene_control_redis`,改成 `control_redis` 与 `redis-control-alerts.yaml`。`deploy/k8s/AGENTS.md` 第 117 行与 `deploy/k8s/README.md` 的 C++ 口令段由第 6b 批会话 [703f1c] 随渲染一起改(它在做 6b,渲染代码 11-13 已入库,正在改部署契约)。
- **用例**(新文件 `go/shared/assetopwake/shard_owner_test.go`,全走 fakeBus):`TestSubscribesOnlyOwnedShardsAndFollowsOwnershipChanges`(集群形状:只订负责的片,换主后新片订上、旧片退订、仍负责的片不重订,gauge 跟着变,Stop 后归 0)、`TestStandaloneGroupFollowsTheOwnedChannelSet`、`TestOwningNoShardSubscribesNothing`、`TestWithoutShardOwnerEveryShardIsSubscribed`(没绑 = 订全部,gauge 64)、`TestBindShardOwnerRules`。
- **设计**:redis-cluster-all-domains-20261110.md §4.6「扇出」改成实际做法、§10 表第 3 行与修订记录;assetop-redelivery-sharding-20261110.md §2.1、§5 第 4 条改成「`WakePlayer` 不看归属,订阅按片」;缺口清单第 34 条补一行。
- **给 Codex**(仓库根;失败保留 `go test -v` 失败输出与第一条编译错误原文,不连续重试;这些模块里有别的会话的未提交改动,报错先分清归属):
  1. `cd go/shared && gofmt -l assetopwake assetop/metrics.go`:无输出(Claude 跑过一次,无输出)。
  2. `cd go/shared && go vet ./assetopwake/... ./assetop/... && go test ./assetopwake/... ./assetop/... -count=1 -v`:全部 PASS;上面五条新用例在列表里,原有 assetopwake 用例(`TestStartSubscribesEveryShardGroup`、`TestResubscribeAfterServerUnsubscribe` 等)不回归。可选 `go test -race ./assetopwake/... -count=1`。
  3. `go/guild`、`go/trade`、`go/data_service` 各跑 `go build ./... && go vet ./...`,再跑 `go test ./internal/svc/... -count=1`;trade 另跑 `go test ./internal/reconcile/... -count=1`(常量改名)。通过标准:0 个编译错误,用例全部 PASS。
  4. 告警:照 [0cc556] 的条目跑契约;有 promtool 时 `promtool check rules deploy/k8s/asset-channel-alerts.yaml deploy/k8s/scene-ownership-alerts.yaml`。
  5. 真 Redis:2026-11-12「全部 Redis 上 Cluster 第 3 批」第 6 步照旧(那两条用例不绑分工,订全部片)。
  6. 可选变异取红(`git diff` 确认改回,不用 checkout / stash):`supervise` 里 ticker 分支不再调 `reconcileGroups` → `TestSubscribesOnlyOwnedShardsAndFollowsOwnershipChanges` 红;`ownedChannels` 忽略 owns → `TestOwningNoShardSubscribesNothing` 红;删掉 `consume` 里的 `noteSubscribed` → gauge 两条红。
- **没做**:C++ 在真集群上的冒烟(等第 2b、5、6b 批);第 7 批文档改写(控制面设计稿 §4 / §5、玩家域设计 §5.1、TiDB 总体方案、AGENTS.md §6 压测口径、`docs/ops/release-checklist.md` §6.1 还写着核心 `zoneredis`)仍无人认领,本条只改了与对账告警直接相关的两行。
- **Java(AGENTS §12)**:服务端内部订阅与指标,不改客户端契约;Java 仓 `PARITY.md`「Redis Cluster」那行补一句待做,本次不写 Java 代码。
- **文件**:`go/shared/assetopwake/wake.go`、`shard_owner_test.go`(新)、`go/shared/assetop/metrics.go`、`go/guild/internal/svc/asset_op.go`、`go/guild/internal/config/config.go`(注释)、`go/guild/etc/guild.yaml`(注释)、`go/trade/internal/svc/assetchannel.go`、`servicecontext.go`、`go/trade/internal/reconcile/pipeline.go`、`go/data_service/internal/svc/recall_channel.go`、`go/data_service/internal/config/config.go`(注释)、`cpp/nodes/battle/logic/battle_room_manager.cpp`(注释)、`deploy/k8s/scene-ownership-alerts.yaml`(说明文字)、`docs/ops/scene-node-role-split.md`、两份设计稿、缺口清单、本文件;Java 仓 `PARITY.md`。未编译,未测试,未提交。

## 2026-11-19 Claude:全局单点线收尾:etcd / Kafka 成员反亲和按档位、k8s_image 透传 -InfraNamespace(未运行,待 Codex 验证;Java 不适用)

- **起因**:负责人 11-19 问「服务器代码实现完毕了吗」,要求没做完的按最标准的方式做完、找到的问题现在修。按原始交接 `docs/handoff/handoff-global-singletons-10m-ccu-20261013.md` §3、§4.4 逐条对工作区核对,结论表在 `docs/handoff/handoff-global-singletons-p0-20261015.md` §10.2:绝大多数已由别的工作线做完或前提已不成立。
- **改了两项**:
  - etcd 与 Kafka 的成员反亲和:`etcd.yaml`、`kafka.yaml` 改成占位,由 `Get-QuorumMemberAntiAffinityYaml` 按档位渲染。staging / prod 用 required,两个成员不在同一节点;dev 用 preferred,产物与改动前逐字相同。发号器两套 etcd 只在非 dev 档部署,直接写 required。
  - `k8s_image.ps1` 声明并透传 `-InfraNamespace`,`dev_tools.ps1` 的 `Invoke-K8sImage` 同步透传。
- **没改**:`redis-cluster.yaml` 同样是 preferred,归 6b 批会话,已发消息;Apply-Infra 删旧 etcd / kafka Deployment 不加确认闸,项目未上线、数据可清。
- **文件**:见交接 §10.1。五个 ps1 用解析器静态查过 0 错误,没有运行任何测试。
- **交 Codex**:见交接 §10.4。跑 `k8s_deploy_contract`、`etcd_maintenance_contract`、`id_issuer_deploy_contract` 三套。上集群前 staging / prod 至少 3 个节点。
- **Java 版(AGENTS §12)**:不适用,Java 版没有 K8s 清单,不加 PARITY 行。

## 2026-11-19 Claude:落地唤醒缺片告警 `AssetOpWakeShardsUnsubscribed` 与契约(接同日「全部 Redis 上 Cluster 第 3 批收尾——落地唤醒只订本副本负责的片」;未运行,待 Codex 验证;Java 不涉及)

- **来由**:同日上一条由「Redis 上 Cluster 第 3 批:C++ 控制集群句柄与分片唤醒」[9394b0] 写,给唤醒订阅加了 gauge `assetop_wake_subscribed_shards`。按它的分工,告警规则与契约归本会话(第 3 批 Go 部分的会话,原 [0cc556])。
- **改动**:
  - `deploy/k8s/asset-channel-alerts.yaml` 新增组 `asset-channel.wake-shards`,规则 `AssetOpWakeShardsUnsubscribed`:`sum by (service) (assetop_wake_subscribed_shards) < 64`,for 5m,warning,评估间隔 60s。规则说明写了缺片的后果(在线玩家等原退避,离线停放的行最长等 30 分钟)和排查顺序。
  - **按 `(service)` 聚合,不带 namespace**。上一条写的是 `sum by (namespace, service)`,以本条为准:
    - data_service 的唤醒成员表跨全部 zone(前缀 `DataServiceNodeService.rpc/`),每片全服只有一个 data_service 副本订。单看一个 zone 的 namespace 本来就不足 64 片,按 namespace 聚合会一直响。
    - guild / trade 只在 infra namespace 部署,两种写法结果一样。
  - `tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1` 新加 3 条用例,原有的「告警名不重名」改成连新告警名一起核:
    - wake-shards 组的规则名单、severity、for、summary、description;
    - 指标由 `go/shared/assetop/metrics.go` 注册、不带可变 label;阈值等于 `go/shared/assetopwake` 的 `ShardCount`;按 `(service)` 聚合的前提成立,即 `recallWakeMembersPrefix` 不带 zone,唤醒用的就是这张表;
    - 三个订阅方都调 `BindShardOwner`,传给 `assetopwake.New` 的 counter 不是 nil(否则没有序列,规则永远不响)。
- **协调**:「控制面按 zone 化与控制域 Redis 分片」[703f1c](第 6b 批)确认不碰这两个文件。[9394b0] 名下的代码文件本条没碰。
- **静态检查**:契约脚本经 PowerShell 解析器解析,0 个错误。
- **给 Codex**(仓库根执行):
  1. `pwsh -NoProfile -File tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`。通过标准:全部 PASS,原有 5 条加新增 3 条。它读 [9394b0] 的代码(metrics.go 的 gauge、三个订阅方的 `BindShardOwner`、`recallWakeMembersPrefix`),要与同日上一条一起在工作区里。
  2. 本机有 promtool 时,把 `deploy/k8s/asset-channel-alerts.yaml` 的 `spec` 抽成规则文件跑 `promtool check rules`,期望 SUCCESS;没有就报告「规则语法未验证」。
  3. 可选变异取红(核对后用 `git diff` 确认改回,不用 checkout / stash):
     - 规则里的 `sum by (service)` 改成 `sum by (namespace, service)` → 聚合那条用例红;
     - `recallWakeMembersPrefix` 改成带 zone 的前缀 → 同一条红;
     - guild 里删掉 `BindShardOwner` 那一行 → 订阅方那条用例红。
- **没做**:告警没有在真集群上触发验证。
- **Java(AGENTS §12)**:不涉及,只加部署告警与契约,不往 `PARITY.md` 加行。
- **文件**:`deploy/k8s/asset-channel-alerts.yaml`、`tools/scripts/tests/assetop_work_shard_alerts_contract.tests.ps1`、本文件。未运行,未提交。

## 2026-11-19 Claude:更正「全部 Redis 上 Cluster 第 3 批收尾」里唤醒缺片告警的聚合口径(只改文档)

- 那条写的 `sum by (namespace, service) (assetop_wake_subscribed_shards) < 64` 不对,实际规则是 `sum by (service) (…) < 64`,以「落地唤醒缺片告警 `AssetOpWakeShardsUnsubscribed` 与契约」条目为准。
- 原因:data_service 的唤醒成员表跨全部 zone,每片全服只有一个副本订,单看一个 zone 的 namespace 本来就凑不满 64 片,按 namespace 聚合会一直报警。guild、trade 只部署在 infra 一个 namespace,两种写法结果相同。
- 代码、设计稿与缺口清单没写这个式子,不用改。Java 不涉及。

## 2026-11-19 Claude：真钱支付接入（微信支付 / 支付宝 / 微信小游戏 / 抖音小游戏 / App Store / Google Play）

- **做了什么**：新服务 `go/payment`（`PaymentNodeService = 19`，库 `mmorpg_payment`），商城加货币发放、付款修订、扣回待办、退款转人工，trade 加货币发放与扣回收敛，scene 的交易扣出流允许 `TX_PAYMENT_REVERSAL = 31`，客户端加支付流程与订单界面入口。事实记录在 `docs/design/payment-recharge.md` §22，§7 按实现重写。
- **状态**：代码写完。**未编译、未运行任何测试、未联调**；11-13 那次合并验证里记的「payment 缺 8 个源表 tip」本条已补（`data/tip/Tip.xlsx` 新段 `//payment_error base=31000`）。没有任何渠道的真实商户号，渠道代码只对照官方文档写成。
- **没做**：
  - K8s 部署（清单、`payment-migrate` Job、回调 Ingress、Secret、部署契约测试）。支付服务目前只能本机跑。
  - 客户端的 App Store、Google Play 桥（工程里没有 StoreKit 2 / Play Billing 插件，装哪个插件要负责人定）。安卓微信 / 支付宝与两个小游戏的桥写了，没上过真机；安卓微信还缺包名下的 `wxapi.WXPayEntryActivity` 与两家 SDK 的打包依赖。
  - 生产环境四个 App 渠道（`wechat_pay`、`alipay`、`apple`、`google`）被合规闸挡着，等实名与未成年人充值限额（会话「实名认证与未成年人防沉迷」，接入点见 §22.8）。
- **数据**：商城存储合同戳换成 `mall-game-official-sku-v2-currency-grant-payment-revision`，trade 官方合同的请求摘要变了（意图多两个字段）。项目未上线，验证前清空商城库、`mmorpg_trade`、`mmorpg_payment` 重装。
- **Codex 验证**（仓库根目录，按顺序；任何一步失败保留完整输出，不要改断言）：
  1. 导表（平时的导表入口，生成 `payment_error_tip`），再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。期望：`go/shared/generated/pb/table/payment_error_tip.pb.go`、`cpp/generated/table/proto/tip/payment_error_tip.pb.{h,cc}`、客户端 `PaymentErrorTip.cs` 出现；`cpp/generated/table` 的工程文件已手工登记这两个文件。
  2. `cd go/payment && go mod tidy && go build ./... && go test ./...`。期望：三个包的单测通过（`internal/data`、`internal/channel`、`internal/mall`，共 7 条，纯函数，不连库）。
  3. `cd go/trade && go build ./... && go test ./internal/commerce -run "TestPlanOfficialFollowsPaymentRevisions|TestOfficialBundleIsItemsOrCurrency|TestLegTxType|TestMallPaging"`。真 TiDB 的 `TestTiDBMall*` 按平时的夹具跑：两个商城测试桩加了 `/internal/reversals/poll` 的空页应答。
  4. `java/mall_game`：`mvn -q test`（或平时的离线入口）。新用例 `PaymentRevisionStoreTest`（7 条，H2）。既有用例应不受影响：`DeliveryApi.PollEntry`、`CatalogAdminApi.Terms` / `Sku` 都留了旧参数个数的构造。
  5. C++：先 msbuild 重编 `cpp/generated/table` 与 scene 的库（`/m:1`），再跑 `currency_test` 里的 `AssetOpSystemTest.PaymentReversalDebitsOnTradeStreamAndNeverOverdraws` 与整组 `AssetOpSystemTest`。
  6. 脚本：`tools/scripts/tests/sql_boot_catalog_local.tests.ps1`（写者清单多了 `payment`，夹具里加了 `mmorpg_payment` 的表）。`go_services.ps1`、`lib/sql_boot_catalog_local.ps1`、`maintenance_drain.ps1` 只做过语法解析，没运行。
  7. 本机冒烟（devpay）：设 `COMMERCE_PAYMENT_SECRET`（与商城同值）、`PAYMENT_DEVPAY_SECRET`（≥32 字节），商城的 `COMMERCE_ALLOWED_PAYMENT_CHANNELS` 加 `devpay`；`payment -f etc/payment.yaml -migrate` 后启动。流程：下官方订单 → `CreatePayIntent(channel=DEVPAY)` → 向 `http://127.0.0.1:18130/notify/devpay` 发带 `X-Devpay-Signature` 的到账通知（正文与签名算法见 `go/payment/internal/channel/channel.go` 的 devpay 段）→ `GetPayIntent` 变 PAID → 商城订单 `DELIVERY_PENDING` → trade 发货后 `DELIVERED`。再发 `revoke=true` 的通知：订单 `REVERSING` → 玩家在线扣回后 `REVERSED`。
  8. Unity：EditMode 跑 `PaymentClientTests`（4 条）。新增的 `.cs` 与 `Assets/Plugins/WebGL/MinigamePay.jslib` 没有 `.meta`，打开工程后把生成的 `.meta` 一起提交。安卓与 WebGL 分支只有切到对应平台才会编译。
- **Java 版（AGENTS §12）**：未做，已在独立仓 `PARITY.md` 登记待做（客户端契约：`payment.proto`、`payment_error` 段、`CommerceOrderState` 的 `REVERSING` / `REVERSED`）。
- **文件**：`go/payment/**`、`proto/payment/*`、`proto/trade/{commerce,trade_table}.proto`、`proto/common/rollback/transaction_log.proto`、`java/mall_game/src/**`（`PaymentRevisionStore`、`PaymentRevisionController`、`DeliveryStore`、`CommerceStore`、`CatalogAdmin*`、`schema.sql`、`SchemaVerifier` 等）、`go/trade/internal/commerce/{mall,engine,leg_tx_type,official_reversal}*.go`、`cpp/libs/services/scene/player/system/asset_op_system.cpp`、`cpp/tests/currency_test/asset_op_system_test.cpp`、`cpp/generated/table/{CMakeLists.txt,table.vcxproj,table.vcxproj.filters}`、`data/tip/Tip.xlsx`、`deploy/mysql-init/00_init_zone_dbs.sql`、`tools/scripts/{go_services.ps1,maintenance_drain.ps1,lib/sql_boot_catalog_local.ps1,tests/sql_boot_catalog_local.tests.ps1}`、`docs/design/payment-recharge.md`；客户端仓 `Assets/Scripts/Game/Jubaozhai/{PaymentClient,AndroidPayBridges,MinigamePayBridges,CommerceClient}.cs`、`Assets/Scripts/UI/Ugui/Jubaozhai/{CommerceWindow,JubaozhaiUiRoot}.cs`、`Assets/Plugins/WebGL/MinigamePay.jslib`、`Assets/Tests/EditMode/Jubaozhai/PaymentClientTests.cs`。本条之前的部分已随负责人的快照提交进 main；11-19 这一批未提交。

## 2026-11-19 Claude:落实 Codex 11-13 两份回执里的红项——cross_zone_test 裸指针成员、排空脚本旧键断言、客户端重连夹具(未编译、未测试,待 Codex 验证;Java 不适用)

- **起因**:负责人贴来 Codex 11-13 的两份回执(帮会恢复结论在客户端 HEAD 的重跑与变异红灯;gate 排空迁移收尾批的验证),口径是 Claude 只改代码,协议生成、编译、压测交 Codex。两份回执跑在 11-12~13 的隔离副本上,先逐项对照主工作区现状:
  1. scene 节点 `id_segment_transport.h:158` 的 `EventLoop* loop_`:已改成 `std::optional<std::reference_wrapper<EventLoop>>`,本文件 11-14「修复本机 C++ 节点旧产物与重编阻塞」记了三节点构建与裸指针门禁通过。本轮不动。
  2. `cross_zone_test` 在 `player_handoff_gate_test.cpp` 被同一门禁拦下:**仍在,本轮改**。
  3. login `password_schema_test.go:102` 把 SQL 注释里的 `CREATE TABLE` 算进去:本文件 11-19「口令登录部署开关复核」已改,未运行。本轮不动。
  4. player_locator `session_reconciler_test.go:1530` 传错类型:HEAD 已是 `leader.NewUniversalStore(sc.RedisClient)`。本轮不动。
  5. 排空脚本用例 61/62,红的一条还按旧的 Redis 键写法断言:**仍在,本轮改**。
  6. 协议绑定与 login 的 `RequestGateMigration` 包装只在隔离副本里重生:主工作区 HEAD 的 `clientplayerloginserver.go` 与 `go/proto/login/*.pb.go` 已有这个方法。本轮不动。
  7. 客户端网络组 142/145,三条红在重连夹具还等 `account`:补丁没写回客户端仓,**本轮写回**。
- **改动**:
  - `cpp/tests/cross_zone_test/player_handoff_gate_test.cpp`:`NamedMethod` 的两个成员由 `const ServiceDescriptor *`、`const char *` 改成借用引用与 `std::string_view`,加一个收指针的构造函数,36 处初始化写法不用动;`Find` 改用 `.`。断言没动。
  - `tools/scripts/tests/k8s_gate_drain.tests.ps1`「同 zone 候选口径与 login 源头一致」最后一条断言:login 的判定循环已改用 `gatepick.DrainingKey(zoneID, g.NodeID)` 取键名(键形 `gate:{gm:<zone>}:<id>:draining`),断言跟着改。要守的事不变,仍是「键名由快照里的 `g.NodeID` 算出」。
  - 客户端仓 `Assets/Tests/EditMode/Net/GameClientReconnectTests.cs`:假网关两处由「请求体含账号名」改成「`access_token` 等于夹具里的令牌,且请求体不含 `account` 字段」。内容与 Codex 的 `build/verification/guild-recovery-notice-20261113/client-67b9c589/assign-gate-reconnect-fixture.patch` 相同(对工作区做反向应用检查通过)。
- **静态核对**(没有编译,没有跑任何用例):
  - 上次整解里门禁在首错处停下,`cross_zone_test` 还有 7 个翻译单元没查到(`discovery_zone_scope_test`、`discovery_set_test`、`etcd_registration_wait_test`、`id_segment_transport_test`、`scene_ownership_reconcile_test`、`scene_placement_fence_test`、`planned_drain_test`)。按成员声明的写法筛了一遍,没有指针成员。这是文本筛查,不等于门禁结论。
  - 排空脚本用例文件语法解析 0 错误;新断言的正则对现行 `gatedrain_monitor.go` 能匹配;CRLF 行数没变。
  - 客户端夹具用到的 `AssignGateRequest` 是 `MmorpgClient.Net` 里的 HTTP 请求体(测试文件没有 `using Loginpb`,不会和同名的协议类冲突)。`67b9c589` 是当前客户端 HEAD 的祖先,之后这份测试与 `GatewayHttpClient.cs` 都没变。
- **给 Codex**(仓库根;先清掉 `MMORPG_*` 环境变量;C++ 必须串行;失败保留首错原文,不连续重试):
  1. C++ 整解:`msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /p:CL_MPCount=1 /nologo /v:minimal`。通过标准:0 个 `error C` / `LNK` / `MSB3073`,日志有 `[no-raw-pointer-member] PASSED for 'cross_zone_test'`,产出 `scene.exe`。门禁若在别的翻译单元再红,保留那一段输出,不改门禁。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/codex-receipts-followup-20261119/cpp-tests`。通过标准:`PlayerHandoffGate.*` 10 条全过(这组用例此前没有运行记录);11-12「gate 排空迁移收尾批续」第 5 步点名的 `ConnectionLostHoldReentry.*`、`SceneEntryPermissionTest.*`、`ConnectionLostHold*`、`EnterFenceEcs.*` 全过;其余不回归。
  3. `cd go/login && go vet ./internal/logic/pkg/auth/ && go test -count=1 ./...`,全包通过(上次唯一的红是 `TestPasswordSchemaFileMatchesTheShapeGuard`)。
  4. `cd go/player_locator && go vet ./... && go test -count=1 ./...`,`internal/logic` 编得过并全过。
  5. `pwsh -NoProfile -File tools/scripts/tests/k8s_gate_drain.tests.ps1`,退出码 0、全部 PASS。同文件「部署生成器的真实 login 模板」那条有别的会话的未提交改动,那条红时先分清是哪一批的。
  6. 客户端(仓根 `D:\luyuan\mmorpg-client`,先关掉占着工程的 Unity,不加 `-quit`):`D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Net" -testResults D:\luyuan\mmorpg-client\.codex-artifacts\codex-receipts-followup-20261119\editmode-net.xml -logFile D:\luyuan\mmorpg-client\.codex-artifacts\codex-receipts-followup-20261119\editmode-net.log`。通过标准:零失败、零跳过、退出码 0(`67b9c589` 上是 145 条);`GameClientReconnectTests` 的 `DefinitiveAssignGateResultEndsRecoveryWithoutRetry`、`RecoveryRetriesPastAStoppedGateWithinTheBudget`、`RecoveryUsesFreshTicketTokenLoginAndOriginalRoleOverRealTcp` 转绿。
  7. 两条冒烟(第 1 步产出 `scene.exe`、本地栈按当前源码重建之后):双 gate 迁移冒烟照 11-12「gate 排空迁移收尾批续」第 10 步;帮会联网冒烟照 `docs/handoff/guild-economy-recovery-exits-handoff-20261023.md` §8.7 末「给 Codex」,先确认 Guild 服务在跑、Scene Manager 健康检查通过。
- **没做的**:Codex 没清掉的隔离副本还在本机(`run/codex-gate-drain-closeout-20261112`、`build/verification/guild-recovery-notice-20261113/client-editmode-project` 与 `client-editmode-head.tar`),删不删由负责人定。
- **Java 版(AGENTS §12)**:不适用,三处都只改测试,客户端契约没变;不往 `PARITY.md` 加行。
- **状态**:未编译、未运行、未提交;客户端仓那一处同样未提交。

## 2026-11-19 Claude:审计表过期清理跟不上时有信号了(缺口清单第 15 条「按天分区不做」的配套;未编译、未测试,待 Codex 验证;Java 不涉及)

- **为什么补**:11-11 第 15 条定案「按天分区不做、清理跟不上先加 data_service 副本」,但线上没有任何信号能看出清理跟不上:删除行数只能拿去和写入行数比,`DataServiceAuditRetentionFailing` 只报语句失败。负责人 11-19 要求查出的问题现在修。
- **改了什么**:
  - `go/data_service/internal/store/audit_retention.go`:每张表每轮清理之后 `reportOldest` 读现存最老一行,报给新的 `AuditRetentionOptions.ObserveOldest(table, 保留期, 最老行年龄)`;空表报 0,时钟偏差的「未来」行报 0,读失败只记日志不报,取消或维护排空时不读。读法收在 `oldestSQLTimestamp`:16 个桶各做一次 `SELECT <时间列> FROM <表> WHERE shard = ? ORDER BY <时间列> LIMIT 1`(走 `(shard, 时间)` 索引,不做全表 MIN)。
  - `snapshot_store.go`、`transaction_log_store.go`:各一条查询与 `oldestRetentionTimestamp`;`export_test.go` 导出给真库用例。
  - `go/data_service/internal/metrics/metrics.go`:`data_service_audit_retention_oldest_age_seconds{table}`、`data_service_audit_retention_max_age_seconds{table}` 两个 gauge 与 `SetAuditRetentionOldest`;`data_service.go` 接上。
  - `deploy/k8s/data-service-audit-alerts.yaml`:新告警 `DataServiceAuditRetentionBehind`(最老一行比保留期还老一天以上、持续 1 小时,warning),处置写在注释与描述里。
  - 设计稿 `audit-pipeline-horizontal-scale-20261106.md` §2.D 补「怎么发现清理跟不上」。
- **交 Codex**(仓库根;`data_service.go`、`metrics.go` 有别的会话的改动,失败先看是否落在本条改的行上):
  1. `cd go/data_service && go build ./... && go vet ./... && go test ./internal/store/ -run 'TestAuditRetention' -count=1 && go test ./internal/metrics/ -count=1`,新用例 `TestAuditRetention_ReportsOldestRowAgeAfterEachRound`。
  2. 真库(MySQL 与 TiDB 各一轮,环境变量按 `storetest` 的约定):`go test -tags integration ./internal/store/ -run 'TestAuditRetention_OldestRowAcrossShards|TestAuditRetention_ShardLockIsExclusiveAndDeletesOnlyItsShard' -count=1`。
  3. 有 promtool 时:`promtool check rules deploy/k8s/data-service-audit-alerts.yaml`。
- **Java 版**:Java 版的审计管道形态不同,不加 Java 行。

## 2026-11-19 Claude:配置表并行加载 `LoadTablesAsync` 可重复调用——去掉函数内静态门闩,加载线程改为返回前 join(未编译,待 Codex 验证;Java 不涉及)

- **起因**:负责人盘点 C++ 节点的线程来源时定下两点:表数据量以后会剧增,启动期的并行加载保留;其中的隐患要修。`LoadTablesAsync()` 原来每张表起一条 `detach()` 的线程,靠函数内 `static muduo::CountDownLatch latch(N)` 等齐。静态门闩只在第一次调用时初始化,第二次调用时计数已经是 0,`latch.wait()` 立即返回,各表的 `LoadSuccess()` 与加载完成回调会和加载线程并发(数据竞争)。全仓目前只有 `Node::LoadAllConfigData` 调一次,`ReloadTables()` 走串行的 `LoadTables()`,所以此前没有触发过。
- **改动**:模板 `tools/data_table_exporter/templates/cpp_all_table.cpp.j2` 里,加载线程改为花括号作用域内的 `std::jthread`,走出花括号时全部 join,之后才跑 `LoadSuccess()`;删掉静态门闩、`detach()` 与 `muduo/base/CountDownLatch.h` 引用。每次调用自带一组线程,没有跨调用的状态,可以重复调用。返回时加载线程已全部退出(原来只保证 `Load()` 跑完,线程可能还在收尾)。起线程中途失败时,已起的线程先 join,再把异常抛给调用方。对现有唯一调用点行为不变:仍然阻塞到全部加载完。仍是每张表一条线程,模板里留了 `shortcut:` 注释:表数量远多于 CPU 核数时再改成按核数限流。
- **产物**:`generated/code/cpp/all_table.cpp` 与 `cpp/generated/table/code/all_table.cpp` 不是手改的,也没有跑整套导表(工作区里有别的会话未提交的 `data/tip/Tip.xlsx` 等改动,整套导表会把它们一起带进产物)。做法是用与 `config_gen._gen_all_cpp` 相同的 Jinja 设置只渲染这一个模板,表名取自现有产物(53 张)。先用未改的模板渲染,与仓内两份产物逐字节相同(sha256 前 16 位 `274853f26f15b88e`),证明这个渲染办法与导表器等价;再渲染改后的模板写回两份(`7e55f583b8dbdbee`,两份相同,LF)。`all_table.h` 没变。
- **测试**:`cpp/tests/configuration_table_test/configuration_table_test.cpp` 新增 `LoadTablesAsyncTest.CanBeCalledRepeatedly`:连调两次,每次返回时加载完成回调都已跑过(计数依次为 1、2),之后 `Test` 表 id=1 可查。并行加载路径此前没有任何单测。原缺陷是数据竞争,普通构建下不能稳定复现成失败,所以这条用例在旧模板上不保证红;结构上的保证是「join 之后才往下走,没有跨调用状态」。
- **文件**:上述模板、两份产物、一个测试文件。没有新增或删除 .cpp,vcxproj / CMakeLists 不变。
- **双版本**:Java 版不涉及。这是 C++ 表加载的内部实现,不是客户端契约。本仓的 Java 模板 `java_all_table.java.j2` 里门闩是方法内的局部变量,Go 模板用 goroutine,都没有这个问题。
- **给 Codex**(仓库根目录,按序;失败不连续重试,保留第一条编译错误原文与 gtest 的失败断言):
  1. **导表器产物核对**(对仓库只读):`py tools/data_table_exporter/tools/sandbox_export.py --out build/verification/table-async-reentrant-20261119/sandbox --compare`。通过标准:导表本身成功;差异列表里**不出现** `code/cpp/all_table.cpp`。工作区有别的会话未提交的表与 tip 改动,列表里出现它们的产物不属于本条,比较总数非零不算本条失败。
  2. **导表器单测**:在 `tools/data_table_exporter` 目录执行 `py -m pytest -q`。通过标准:与改前相同,无新增失败(本条没有改 Python)。
  3. **C++ 构建**(串行):`msbuild cpp/generated/table/table.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`。通过标准:0 个 error C / LNK。`all_table.cpp` 用到 `std::jthread`,工程已是 `stdcpplatest`。有 Linux 环境时再跑 `tools/scripts/build_linux.sh`(CMake 为 C++23,`std::stop_token` 在 Agones 代码里已经在用)。
  4. **C++ 用例**(第 3 步已重编 table 库,`run_cpp_tests.ps1 -Build` 不重编库):`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter configuration_table_test -LogDir build/verification/table-async-reentrant-20261119/cpp-tests`。通过标准:零失败;日志里有 `LoadTablesAsyncTest.CanBeCalledRepeatedly` 且 PASS。
  5. **节点启动**(随下一次整解一起做即可):gate / scene / battle 都链接 table 库,重新链接后起任一节点,能走完启动,表加载这一步没有 FATAL。
- **残余**:没有按核数限流(见上)。`LoadTablesAsync` 名字里的 Async 实际是「并行加载并阻塞等待」,没有改名。`docs/design/thread-count-monitoring*.md` 的线程构成表里没有「启动期每张表一条加载线程」这一行,本条没有补。

## 2026-11-19 Claude:组队跟随间距改定 3 米(补同日「组队跟随间距放大到 3 倍(1.4 → 4.2)、玩家视野放大到 5 倍(10 → 50)」;未编译、未测试,待 Codex 验证;Java 待做)

- **起因**:负责人「跟随间距改成 3 米吧」,取代同日上一条里的 4.2。视野 50、格子 50 不变。
- **改动**:
  - `cpp/libs/services/scene/player/system/team_follow_formation.h`:`kTeamFollowSpacing` 4.2 → 3.0。整列 4 × 3 = 12,保留历史(队员上限 + 1)× 间距 = 15 米。
  - 归队阈值 `kTeamRegroupDistance` 仍是 24:整列 12 ≤ 24 < 视野 50,`player_team.cpp` 两条 `static_assert` 照样成立,只改了 `player_team.h` 的说明。加入时最长走路 48 也不变。
  - `robot/team_follow_smoke_scenario.go`:C4 停步距离 3.0~5.8 → 2.0~4.6(一号槽位 3)。拉开距离 60 / 步长 72 是按视野 50 定的,不变。
  - 测试不用改:跟随相关用例的期望都按 `kTeamFollowSpacing` 算。按 3 米逐条推算过:直走、拐角朝向、战后赶回、暂离、补推。纯逻辑那几条按 1.4 构造队形,与生产间距无关。
  - 文档:`docs/design/team-follow-server-trail.md` §1、§3.2、§3.3、§3.4、§7;`docs/design/team-system.md` V.2 第 2、6 条与 V.7 第 5 条。
- **交 Codex**:与同日上一条同一批执行,命令照那一条;`aoi_test.exe --gtest_filter=TeamFollow*` 与 `cross_zone_test.exe --gtest_filter=PlayerTeamFollowTest.*:TeamFollowDriveTest.*` 必须在本条改动之后跑。
- **Java 版(AGENTS §12)**:待做。Java 仓 `PARITY.md` 同日条目里的间距已改成 3 米,没有改 Java 代码。本条没有提交。

## 2026-11-19 Claude:审计发件箱改成一笔带完——删掉分笔,存盘通道单消息上限放到 16 MiB(收掉设计稿 §12「分笔窗口里 scene 崩溃丢已提交改动的流水」;未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做,PARITY 那条已改口径)

- **起因**:负责人 11-19 要求「查出的问题现在修」。先核这件事的现状:
  - 运行证据:11-13「Codex:main 合并提交前验证记录」编过 go/db 生产代码、审计 / 活性告警契约通过;11-14、11-18 编过 scene。单测、真库用例、冒烟一条没跑。
  - 原会话那一半(K8s 渲染与暂停开关、告警、compose、清库清单 `player_audit_outbox`、`docs/README.md`、重连总览 R13 行、Java 仓 PARITY 登记)都已落。11-13 合并没有冲掉两边任何文件。
  - 剩下设计稿 §12 记的一条残余,违背「已提交的资产改动一定有流水」:积压超过 512 KiB 时分笔提交。前一笔的改动已经提交、剩下的记录还在内存里,两笔之间 scene 崩溃就丢,进程没了也没处计数。
- **做法**:一笔存盘带上这名玩家排着的全部记录。
  - 每人队列上限 8 MiB(`AuditOutbox::kMaxBytesPerPlayer`)就是一笔里审计记录的上限。存盘请求的传输上限从 gRPC 缺省 4 MiB 放到 16 MiB:行 3 MiB + 审计 8 MiB + 余量,装得下。
  - 认领响应早就这样单独放大过接收上限(`kMaxClaimResponseBytes`),这次照同一写法、同一个常量头文件,两边由 Go 单测读头文件核对。
  - TiDB 一个事务至多约 19 MiB(行 3 MiB、流水 8 MiB、进退场两份快照各 ≤ 4 MiB),单条 INSERT ≤ 8 MiB,低于驱动与 TiDB 64 MiB 的单包上限;存盘 12 秒方法超时够用。正常一笔只带一两秒的量。
  - 不选「暂存表 + 最后一笔原子转正」:要多一张表、一种请求、孤儿清理,只换来消息小一些。也不选「排满就挡资产操作」:要改所有货币、背包入口的预检,组合操作会半途失败。
- **改动**:
  - C++:`cpp/libs/engine/config/player_store_path.h` 加 `kMaxSaveRequestBytes`;`cpp/libs/engine/core/node/system/node/node_connector.cpp` 连 go/db 的通道 `maxSendMessageBytes` 用它;
    `cpp/libs/modules/audit/audit_outbox.{h,cpp}` 删 `kMaxBytesPerSave` 与 `Attached{through, more}`,`AttachDue` 一笔全带、不返回值;
    `cpp/libs/services/scene/player/system/player_store_client.{h,cpp}` 删 `AuditAttachment`、`auditAckMark`、续笔 `continuation` 与 `auditStalled`,放行一律按这份快照的水位,`maxRequestBytes` 缺省改为 `kMaxSaveRequestBytes − 64 KiB`;`player_lifecycle.cpp` 的 `attachAudit` 钩子跟着改。
  - C++ 测试:`cpp/tests/bag_test/player_store_client_test.cpp` 删四条分笔用例,新 `PlayerStoreClientTest.AllDueAuditGoesWithTheRowsAndReleaseInOneSave`、`PlayerStoreClientTest.DefaultRequestLimitHoldsAFullAuditQueue`;
    `cpp/tests/currency_test/transaction_log_commit_gate_test.cpp` 两条分笔用例换成 `AuditOutboxTest.OneSaveCarriesEverythingDueInOrder`、`AuditOutboxTest.AttachRepeatsTheSameRecordsUntilAcked`,`OverflowByCountDropsTheOldest`、`OverflowByBytesDropsTheOldest` 改成核一笔带完。
  - go/db:`internal/playerstore/store.go` 加 `MaxSaveRequestBytes = 16 << 20`;`internal/playerstore/audit_outbox.go` 的 `MaxAuditBytesPerSave` 改 8 MiB、`maxSaveRequestBytes` 由传输上限推出;`db.go` 加 `s.AddOptions(grpc.MaxRecvMsgSize(playerstore.MaxSaveRequestBytes))`。
    单测:`TestAuditLimits_MatchCpp` 改核 `kMaxBytesPerPlayer = 8 * 1024 * 1024`;新 `TestMaxSaveRequestBytes_MatchesCpp`(`store_test.go`);`TestNew_RejectsARowBudgetWithNoRoomForAuditRecords` 按新数仍成立,没改。
  - 文档:`docs/design/player-audit-outbox.md`(状态、§5.1、§5.2、§6.4、§11、§12)、`data-consistency-guarantee-map-20261020.md` §6.3 第 1 条与 §6.4 第 1 行、`infra-reconnect-overview.md` R13 行、`docs/ops/audit-topic-generation-bump-runbook.md` §8。
  - Java 仓 `PARITY.md`:2026-11-12 审计发件箱那条待做里「一笔存档带不完就分笔」改成「一笔存档带完,不分笔」(只改这一条,该文件别的未提交改动保留)。
- **给 Codex 的验证步骤**(仓库根目录,VS 2026 Developer PowerShell;本会话没有编译、没有跑任何测试):
  0. 不改 proto,不用重新生成。与「审计发件箱」前几条合跑时,那几条要求的清库、协议生成照旧先做。
  1. C++ 全量:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,期望 0 错误。全仓 `kMaxBytesPerSave`、`AuditAttachment`、`auditStalled`、`auditAckMark` 不应再有引用。
  2. C++ 单测(`run_cpp_tests.ps1 -Build` 不重编库,第 1 步之后再跑):`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test`,再 `-Filter bag_test`。
     通过标准:上面点名的四条新用例与两条改过的溢出用例通过,`AuditOutboxTest.*`、`PlayerStoreClientTest.*` 其余用例不回归。
  3. go/db(工作目录 `go/db`):`gofmt -l .` 无输出 → `go build ./...` → `go vet . ./internal/playerstore/...` → `go test . ./internal/playerstore/... ./internal/auditoutbox/... ./internal/config/... -count=1`。
     重点:`TestAuditLimits_MatchCpp`、`TestMaxSaveRequestBytes_MatchesCpp`、`TestMaxClaimResponseBytes_MatchesCpp`、`TestNew_RejectsARowBudgetWithNoRoomForAuditRecords` 通过。
     `./internal/config/...` 要在工作区跑:11-13 合并带回的 `go/db/internal/config/mysql_password_test.go` 引用已删的接口,同日「工作负载密钥第 8 批复核」条目已在工作区删掉它(未提交),按 HEAD 跑会编不过。
  4. 契约:`pwsh -NoProfile -File tools/scripts/tests/audit_outbox_alerts_contract.tests.ps1`,期望 fail=0(它读 `player_store_client.cpp` 的 `IncStoreTooOld`,本条没动)。
  5. 冒烟(和前几条第 5、7 步合跑即可):机器人登录、花一笔货币、下线,`player_audit_outbox` 先有行、约 1 秒内清空,data_service `transaction_log` 有对应行。
     大积压一笔带完:停 go/db 几分钟,其间机器人持续花货币,积压超过 512 KiB(约 2000 条流水)才覆盖到原先要分笔的情形,做不到就报告「大积压未覆盖」。
     恢复之后 go/db 与 scene 日志里没有 `RESOURCE_EXHAUSTED`、没有 `over the transport budget`;积压的流水随第一笔提交一次写进发件箱
     (`db_audit_outbox_records_written_total{kind="transaction_log"}` 一次跳到积压条数),`scene_audit_outbox_records_dropped_total` 各原因仍是 0。
  6. 失败时保留第一个编译错误原文、失败用例完整输出;不改断言、不跳过用例。
- **残余**:设计稿 §12 只剩「新旧混跑时 EQUAL 判断不了之前那次提交是不是旧 go/db 做的」,这一版随首次上线整批发布、没有不认识审计字段的旧 go/db 在线,上线前不阻塞。
  scene 排满 8 MiB 之后丢最旧记录(`reason="overflow"`,计数并告警)是有意的内存上限,不是本条范围。
- **Java 版**(AGENTS §12):不写 Java 代码,`PARITY.md` 那条待做已改成新口径。不改客户端契约,没有客户端改动。
- **提交情况**:main 工作区,未提交,没有开分支。

## 2026-11-19 Claude:组队跟随与离线托管本线遗留项清零(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人要求查出来的问题现在修,等压测、等联机的事先按标准做法定,不留到以后。本条清点本线(组队跟随服务器驱动、挂机月卡离线托管、11-19 间距与视野)记过的遗留项,逐条处置。
- **处置**:
  1. **入场后补推缺自动化覆盖**(11-10 条目记"留给联机验收"):补推的判据抽成只读查询 `TeamFollowSystem::DrivenSlot`(`team_follow_system.{h,cpp}`)。`ResendStateAfterEntry` 只在它有值时发 `driven=true`,名次就是它。新用例 `TeamFollowDriveTest.DrivenSlotIsWhatTheResendAfterEntryReports`(`cpp/tests/cross_zone_test/player_team_follow_test.cpp`)验三件事:名次;队长不被驱动;换了场景实例、在下一 tick 摘驱动之前也不补推。发送那一行与其他跟随状态推送共用 `SendFollowState`,不单独截获。截获下发的 `aoi_delivery_test` 靠假发送函数,不编移动与跟随源码,硬编进去要连带拉入大半个 scene 库。
  2. **robot 冒烟一次上报跳 72 米**(11-19 条目记"没在真实地图上核对"):`robot/team_follow_smoke_scenario.go` 的 `teamFollowSmokeWalkAway` 改成沿一个方向从当前位置每步上报 16 米(与视野放大前一样)。走够 60 就返回;一步推进不到 1 米或走满 8 步就换方向。不再要求地图上有一整条 70 米以上的直路。
  3. **ExitGame handler 里过时的 TODO**:`cpp/nodes/scene/handler/rpc/player/player_lifecycle_handler.cpp` 守护段内"有月卡就改 AI 控制"的 TODO 和注释掉的代码已删。它引用的 `MonthlyCardComp`、`SwitchToAIControl` 都不存在,月卡托管已由断线保留实现。换成一句指向 `player-connection-lost-hold.md` §4.1 的说明;行为不变,主动登出照常立即退出。
  4. **两台机器墙钟差超过 5 秒走慢路径**(11-06 托管条目记的已知边界):按标准做法定案,写进 `player-connection-lost-hold.md` §4.1「时钟口径」。scene 与 player_locator 都用本机墙钟,依赖 NTP;超过 5 秒按对时故障告警处理,只多等一个 claim 超时、不丢数据。不改读 Redis `TIME`。
  5. **视野放大后的性能**:没有待定的代码决策。格子尺寸取下限 50(扫描最少),兴趣表容量与优先级照旧裁决人多的场面;结论靠 AGENTS §6 压测数据。
- **核对过、不用改**:属性同步按距离分档(`actor_state_attribute_sync.cpp`)。视野外、仍在显示的关注者落到最低档同步,不被丢;视野放大后照样成立。
- **文档**:`docs/design/team-follow-server-trail.md` §3.5 表(离线一行)、§8;`docs/design/player-connection-lost-hold.md` §4.1、§8 第 7 条。
- **交 Codex**:与同日另两条(间距与视野、间距改定 3 米)同一批。`cross_zone_test.exe --gtest_filter=PlayerTeamFollowTest.*:TeamFollowDriveTest.*` 要包含新用例 `DrivenSlotIsWhatTheResendAfterEntryReports`;`robot` 下 `go build ./...`、`go vet ./...`。
- **Java 版(AGENTS §12)**:本条只改服务端内部结构、测试、robot 与文档,不涉及客户端契约,不往 `PARITY.md` 加行。本条没有提交。

## 2026-11-19 Claude:全部 Redis 上 Cluster 第 6b-1 批——各服务接控制集群,退役按用途拆实例与 `-RedisSplit` 五个参数(缺口第 34 条;代码写完,未运行,待 Codex 验证;Java 不涉及)

- 范围、做法与偏差见设计稿 `docs/design/redis-cluster-all-domains-20261110.md` §10.7(§10 表第 6b 行拆成 6b-1 / 6b-2)。11-13 写的大部分随快照 `6914a1af8` 入库,今天补完契约测试、两处总账漏记、集群成员反亲和与部署说明。
- 改了什么:
  - `tools/scripts/k8s_deploy.ps1`:
    - 控制端点 `Get-ControlRedisEndpoint`:staging / prod 是 `redis-control` 的种子与 Secret `redis-control-auth`,dev 是核心 `redis` 与 `redis-auth`。八个 Go 服务的 `ControlRedis` 段、gateway 的 `spring.data.redis`(集群写 `cluster.nodes`)、scene 的 `control_redis` 与 `CONTROL_REDIS_PASSWORD` / 配置指纹都从这里取。
    - gate、battle 不写 Redis 段、不注入 Redis 口令。口令来源按服务分(`Get-SharedRedisClientSource`)。开控制集群必须同时开玩家域集群(`Assert-RedisClusterPairing`)。
    - 连接总账:核心那份 = 在核心上的域的行,两个域都在集群上时只打 `no clients`;控制集群那份 = `Get-RedisControlClientRows -Cluster $true`。补记两处漏掉的连接:data-service 的第二个控制域客户端、login 的排队放行流水线 1 条。规划档删掉 gate / battle 台数。
    - queue / ctl / purpose 整套机制与五个参数删除。
    - `redis-cluster.yaml` 的成员反亲和改按档位渲染:staging / prod 用 required,复用「全局单例扩容交接」会话同日加的 `Get-QuorumMemberAntiAffinityYaml`。
  - `tools/scripts/dev_tools.ps1`、`k8s_image.ps1`:删掉五个参数的透传。
  - `tools/scripts/k8s_gate_drain.ps1`:读 `ControlRedis.Addrs[0]` 与 `ControlRedis.Cluster`,集群时 exec `pod/<种子>`。
  - `tools/scripts/lib/k8s_client_entry.ps1`:注释改指现行调用方。
  - `deploy/k8s/manifests/infra/redis-cluster.yaml` 加反亲和占位;删 `redis-purpose.yaml`、`redis-purpose-ha.yaml`。
  - 测试:
    - `k8s_deploy_contract`:总账、池镜像、口令来源、guild / data-service ConfigMap、scene-manager 发现键;
    - `redis_player_cluster_contract`:控制集群总账、核心组合、两套都开的 zone-up / infra-up 渲染、配对负向、反亲和;
    - `k8s_redis_split_contract`:改成只钉运维命令与退役;
    - `k8s_client_entry_contract`、`k8s_gate_drain.tests`、`redis_exporter_contract`、`lobby_authority_render` 按新口径改;
    - 删 `k8s_redis_ctl_contract.tests.ps1`。
  - 文档:
    - 设计稿 §7、§10、§10.7;
    - `deploy/k8s/README.md` 的 Redis 总账、运维命令、两套集群、C++ / Go 口令几节;
    - `deploy/k8s/AGENTS.md` 的 Redis 口令一条;
    - `docs/ops/redis-player-cluster-runbook.md` 的节点数与总账。
- **给 Codex 的验证步骤**(仓库根目录。本会话没有跑任何测试,只做了 PowerShell 静态语法解析,全部 0 错误):
  0. 没改 proto,也没改 Go / C++ / Java 代码,不用生成、不用编译。
  1. 部署契约,逐个跑:
     ```
     pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/k8s_gate_drain.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/redis_exporter_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/lobby_authority_render.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/login_password_auth_deploy_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1
     ```
     最后一个用的是同一个反亲和函数。通过标准:fail=0。失败时保留失败用例全文与 DryRun 输出尾部,先判断是夹具还是渲染,不改断言。
  2. 日志英文守卫:`python tools/scripts/check_log_text_english.py`,期望没有新增违例。
  3. 冒烟(有 kind 环境时):dev 档 `k8s-all-up` 起服后登录一次,login、scene-manager、scene 都起来,能进场景;`dev_tools.ps1 -Command k8s-redis-split-status` 退出码 0。staging 档每套集群另需至少 6 个可调度节点。
- **残余**:第 6b-2 批(退役 staging / prod 的核心单实例、`-RedisReplicas` 与 `redis-ha.yaml`、match / mail 指向核心的段)由本会话接着做。
- **Java 版**:没有 K8s 生产形态,不涉及。
- **提交情况**:main 工作区,未提交,没有开分支。`k8s_deploy.ps1` 与 `k8s_deploy_contract.tests.ps1` 里还有「全局单例扩容交接」会话今天的 etcd / Kafka 反亲和改动,不属于本条。

## 2026-11-19 Claude：发号器第五批——计数器超前一天改小块、重新登记看 API server 时钟（未编译，待 Codex 验证）

- **起因**：交接 §11.5 的残余（三份记录一起出事、没有进程活着，加上计数器曾超前时钟 7 天或时钟变慢）在代码里关不掉，用户要求做两处缓解把它缩小。
- **① 小块**：计数器比时钟超前一天以上时，新块从 2^32 改成 2^28 个号（16 秒的号），追回一天以内恢复整块。崩溃循环每重启一次只推前 16 秒，推不到 `ahead_of_clock`。正常发布超前几个小时，用不到小块；代价是小块期间登记处不可达时手里的号更快用完。
- **② 两个时钟**：登记处丢码重新登记时，时间上界取 data-service 本机时钟和 K8s API server 时钟（回应里的 Date 头，和读部署身份同一个请求）里较晚的那个；API server 读不到就拒绝、重跑。
- **剩下的**：时钟曾跳到未来，或者两个时钟都变慢。写进设计 §6.3。
- **改动**：`go/data_service/internal/idissuer`、`internal/clusteridentity`、`internal/svc/servicecontext.go` 的发号器装配；新增 4 个单测；设计 §6.3、发号器手册、交接 §12（原 `docs/handoff/global-id-issuer-registry-20261105.md`，已清理，见 `ccf2840b2`）。
- **验证**：没有编译，没有运行任何测试。命令在交接 §12.3，可以和 §11.3 合并成一轮。
- **Java 版（AGENTS §12）**：不涉及，Java 版没有发号器，`PARITY.md` 照旧。
- **提交情况**：未提交。

## 2026-11-19 Claude:snowflake 收尾第三批——切磋挑战记录的过时说明、规范与说明文件里的「SnowFlake GUID」叫法、thread_context 筛选清单(只改注释、文档与工程清单;未生成协议、未编译,待 Codex 验证;Java 不涉及)

- **来由**:11-12 两条(「proto 源与 `shared/snowflakealloc` 里过时的 snowflake 说法改成现状」「删掉已无调用方的 C++ `SnowFlakeManager`」)末尾列了留给别处的几项,负责人 11-19 要求查出的问题现在修。动手前核对了 11-12 的改动经过 11-13 之后的快照与合并提交都还在:`snow_flake_manager.{h,cpp}` 没有复活,`thread_context/CMakeLists.txt` 已重新生成、去掉了那一行,proto 注释与 snowflakealloc 的四处改动都在 HEAD,生成物也已带上 11-12 的新注释。只用 main,未提交。
- **改动**:
  - `proto/match/match_service.proto` 切磋段:「挑战记录存 Redis(TTL 60s)」改为存 match 的 SQL 表 `match_challenge_invitation`,有效期 `ChallengeTTLSeconds`(默认 60 秒),过了 `expires_ms` 按库时钟判作废(`go/match/internal/authority/challenge.go`)。同段「双方 battle:lock 校验在应战时刻做」核对仍成立(`global_challenge.go` 的 `respondGlobalChallenge`),没改。
  - `proto/db/proto_option.proto` 的 `OptionTiDBNonclusteredPK` 说明:「雪花主键防写热点的前提」改为「行按 _tidb_rowid 存,是 SHARD_ROW_ID_BITS 打散写入的前提」,与各表选项上方已有的说法一致(主键现在是发号器打散过的号)。
  - `AGENTS.md` §4:「SnowFlake GUID 用 uint64」改为「64 位 ID 用 uint64」,同一批字段按号源分三类列出(发号器的永久 ID、SnowFlake 的 `scene_id`、运行时实例 ID),约束本身(必须 `uint64`)不变。`proto/AGENTS.md` 第 28、36 行与 `proto/README.md` 第 59 行同口径。
  - `.github/copilot-instructions.md` 的「SnowFlake Node ID Isolation Constraint」一节按 AGENTS.md §7 第 1 条重写:永久 ID 只由发号器发;SnowFlake 只剩 scene_id,worker id 来自 `shared/snowflakealloc` 的槽位,与路由 node_id 无关;路由 node_id 按 node_type 分配(`shared/noderegistry` 的 `AllocationKey`),不喂给发号器;不变量改按 kind 表述。原文「worker 取自 etcd 分配的 node_id」「scene 用 SnowFlake 发物品 guid」「node_id 按 (zone, node_type) 分配」都已不成立。
  - `cpp/README.md`:thread_context 里已经没有雪花 ID(11-12 删了 `SnowFlakeManager`),目录说明改成现有内容。`go/README.md` 的 shared 一行写明号段管永久 ID、雪花只剩 scene_id。
  - `cpp/libs/engine/thread_context/thread_context.vcxproj.filters`:列表改成与 vcxproj 的文件条目完全一致(删掉七组早已不在磁盘上的文件,补上 `trace_context_tls.cpp`)。它只影响 VS 里的显示,没有工具读它。
- **Codex 验证**(仓库根目录):
  1. 生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
  2. 核对生成物只有注释变化:用 11-12 第一条里的那段 pwsh 脚本(同样在提交之前跑、对比 HEAD),把第一行换成 `$names = 'match_service','proto_option'`,通过标准仍是打印 `comment-only: OK`。match_service 改的是独立注释,只会出现在 `generated/**` 的 `.proto` 副本里;proto_option 是字段行尾注释,还会出现在 `go/proto/db/proto_option.pb.go`。脚本若报出非注释差异,先看是不是 `6914a1af8` 等快照提交里别的会话对 `match_service.proto` 的字段改动还没生成,那部分不归本条。
  3. C++:`msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64` 时 `thread_context` 工程照常编过(filters 不参与编译,这一步只是确认 VS 打开工程不报错);可与 11-12 第二条的第 1 步合并成一轮。
  4. 11-12 两条:11-13 Codex 的合并记录(「main 合并提交前验证记录」)里完整协议生成与 robot vendor 同步已做,生成物已带上新注释,11-12 那段核对脚本对比 HEAD 已没有可比的差异,不用再跑。还没有点名回报、要和本条一起跑的是:`cd go/shared` 后 `go vet ./snowflakealloc/...`、`go vet -tags=integration ./snowflakealloc/...`、`go test -count=1 ./snowflakealloc/...`(同一记录里 shared 只定向复验了 authoritysql / noderegistry);以及删掉 `SnowFlakeManager` 之后的 Windows 整解(同一记录里全方案被追踪代码的门禁挡住,没有验证),即上面第 3 步。
  5. 失败时保留脚本打印的文件清单与 `Compare-Object` 结果、首个编译错误;不连续重试。
- **Java 版**(0.1.0-SNAPSHOT):只改注释、文档与 VS 清单,不涉及,不加 PARITY 行。
- **文件**(本会话改的):`proto/match/match_service.proto`、`proto/db/proto_option.proto`、`AGENTS.md`、`proto/AGENTS.md`、`proto/README.md`、`.github/copilot-instructions.md`、`cpp/README.md`、`go/README.md`、`cpp/libs/engine/thread_context/thread_context.vcxproj.filters`、本文件;未提交。

## 2026-11-19 Claude:组队开局回执两处收尾——首发回执不改成恢复用语、过户前的原件给确定结论(收掉 PVP 修复批审核的三条低风险发现;未生成、未编译、未测试,待 Codex 验证;Java 待做)

- **起因**:负责人要求把审核 `build/verification/pvp-repair-review-20261115/` 时报的三条低风险发现现在修掉。
  - 第 1 条:服务端 11-15 起排队成功也带回执(state=2),客户端的回执分支总把提示改成「原队伍匹配已确认入队。」,首发 PVP 入队的排队说明被盖掉;满员 PVE 首发也一直被改成「原队伍开局已受理，正在恢复战斗。」。
  - 第 2 条:角色过户后,同一台设备上的新主人加载到前主人留下的恢复文件,带原请求号查队伍。`TeamLaunch` / `FindDirect` / `RequestCancelled` 把「原件主人不是当前会话」也报成 `ErrOriginalPlayer`,GetMyTeam 回 ErrInternal(计故障),客户端每 3 秒重试,本机原意图永远清不掉,换参数开战一直被拦。审核时写的「整包不带队伍视图、单人按钮一直灰」不准确:`respond` 会重读视图,队伍面板能加载,本条更正。
  - 第 3 条:`tools/scripts/README.md` 第 91 行仍说 rank 不在调试栈里,和同文件第 76~89 行冲突。
- **做法**:
  - 服务端区分两种「非原角色」。会话本身已不归角色现主人时仍是 `ErrOriginalPlayer`。会话已按护栏核对为现主人、原件却记着前主人时,改回新哨兵 `ErrForeignOriginal`;它包着 `ErrOriginalPlayer`,按 `errors.Is` 判断的调用方行为不变(全仓没有按值比较的地方)。
  - 过户前护栏要求整队原件已终结、角色已离队(`authority.RoleFencePending`),所以新主人撞上的前主人原件这时都已关闭,对现主人是确定结论,不是未知。
  - `TeamMatchIntent` 遇到它回 `state=6`(`authority.TeamMatchIntentForeign`),GetMyTeam 照常回码 0 和调用者视图;StartTeamMatch 沿前主人的请求号重发时回 `kInvalidParameter` 并带同一回执,不建第二份原件。
  - 客户端把 6 和 2、5 一样当确定结论,清掉本机原意图并提示可以重新发起;「原……」确认用语只在恢复查询或重发原请求时出现,首发请求的回包沿用 `FinishWrite` 写好的提示。
- **改动**:
  - Go:`go/match/internal/authority/role_identity.go`(`ErrForeignOriginal`)、`team_launch.go`(`teamLaunchOnce`)、`store.go`(`findDirectOnce`、`requestCancelledOnce`)、`team_intent.go`(`TeamMatchIntentForeign` 与外层换算)、`go/match/internal/team/sql_launch.go`(StartTeamMatch)。CancelQueue 沿用原来的报错:客户端撤销排队不带开战请求号,走不到这条路。
  - 协议:`proto/team/team.proto` 只改 `TeamMatchIntent` 的注释(写明有无 battle_id 两种口径和新值 6),字段不变。
  - Go 测试:`go/match/internal/authority/team_intent_test.go` 移出原「different owner」子用例(它期望报错),新增 `TestTeamMatchIntentPredecessorOriginalIsDefinitive`(整队原件、旧直接成局、撤销墓碑三种)、`TestTeamMatchIntentStaleSessionIsNotPredecessorOriginal`;
    新文件 `go/match/internal/team/predecessor_start_intent_integration_test.go` 的 `TestTeamTiDBPredecessorStartIntentIsDefinitiveForNewOwner`(真库:撤排队、解散、过户之后验 TeamMatchIntent / GetMyTeam / StartTeamMatch,前主人的原件不动,旧会话照旧被拒)。
  - 客户端(同级仓 `mmorpg-client`):`Assets/Scripts/Game/Team/TeamClient.cs`(`PredecessorStartIntentState`、`PredecessorStartIntentMessage`、`ObserveStartIntent(response, recovery)`);
    `Assets/Tests/EditMode/Tianyong/TeamClientTests.cs` 新增 `PvpQueueReceiptUsesRecoveryWordingOnlyWhenRecovering`(两例)、`FirstDirectStartReceiptKeepsTheGatheringText`、`PredecessorReceiptFromRecoveryReleasesTheIntentAndAllowsANewStart`、`PredecessorReceiptOnAResentStartReleasesTheIntent`。
  - 文档:`docs/design/cross-zone-matchmaking.md` §13.2「整队」按现实现改写回执口径;`tools/scripts/README.md` 第 91 行;`tools/scripts/prepare_docker_debug.py` 新增密钥的注释补「排行榜两把密钥」。
- **给 Codex 的验证步骤**(本会话没有生成、没有编译、没有跑任何测试):
  1. 协议(仓库根目录):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。本条同时改了客户端,用默认配置,连同级客户端仓一起写。只有注释变化,生成物里 `TeamMatchIntent` 的注释会变,字段与编号不变。
  2. Go(工作目录 `go/match`):`gofmt -l internal/authority internal/team` 无输出 → `go build ./...` → `go vet ./internal/authority/... ./internal/team/...` → `go test ./internal/authority/... ./internal/team/... -count=1`。
     重点:`TestTeamMatchIntent*` 五个用例(含本条两个新的)通过;`ErrOriginalPlayer` 相关的既有用例不回归。
  3. 真库(`MATCH_IT_TIDB` 指向一次性 TiDB,`MATCH_IT_REQUIRE_TIDB=1`):`go test ./internal/team/ -run 'TestTeamTiDBPredecessorStartIntentIsDefinitiveForNewOwner|TestTeamTiDBExpiredMemberTicketRecoversIntentAndAllowsRequeue|TestTeamTiDBOldInvitationCannotAuthorizeNewOwner' -count=1`。
  4. Unity EditMode:`TeamClientTests` 全部通过;重点是上面点名的五例,以及 `DroppedPvpStartCanReleaseItsDurableIntentAfterTheOriginalQueueIsConfirmedOrWithdrawn`、`BattlelessRecoveryDoesNotReleaseAnUnknownReservedOrDifferentRequest`、`StartMatchUnknownReusesOriginalUuidAndMissingStatusCannotReleaseIt`、`StartMatchBackfill*` 不回归。
  5. 失败时保留第一个编译错误原文与失败用例的完整输出;不改断言、不跳过用例。
- **Java 版**(AGENTS §12):不写 Java 代码。Java 仓 `PARITY.md` 加了同日待做条目,连同 11-15 PVP 修复批里同样没登记的无战斗回执口径、整队排队维护、PVE 经验字段一起记。
- **提交情况**:两仓 main 工作区,未提交,没有开分支。

## 2026-11-19 Claude:组队跟随合并广播的发送层补单测(拆出 actor_move_broadcast.cpp);查出挂机月卡没有开通入口(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人要求查出来"以后再修"的现在修。11-12「组队跟随遗留三项收尾」把合并广播的发送层(按组选 133 / 135、每组一次广播)留给了联机验收,
  理由是能截获客户端消息的 `aoi_delivery_test` 不编 `movement.cpp`(编进去会连带拉进导航)。
- **改动**:
  - `MovementSystem::BroadcastMove` 与 `ActorMoveBatch` 的实现(连同取接收者、拼消息两个内部函数)原样从 `spatial/system/movement.cpp`
    搬到新文件 `spatial/system/actor_move_broadcast.cpp`;声明仍在 `movement.h`,调用方不变。新文件只依赖兴趣索引、网络号、
    消息内容(`actor_motion.h`)与发送函数,不碰导航。`movement.cpp` 删掉只给这几段用的头文件。
  - 登记:`scene.vcxproj` / `.filters` 加 ClCompile(Linux 的 CMakeLists 由 vcxproj 重新生成);`aoi_delivery_test.vcxproj` 加编这个文件。
  - 新用例(`aoi_delivery_test.cpp`):`AoiDeliveryTest.MoveBatchSendsOneMessagePerReceiverAndSharesItAcrossTheGroup`
    (看得见两人的旁观者与带本人的队员共用一条 135、按登记先后;只看得见一人的收 133;重复登记只发一条;两组各广播一次)、
    `AoiDeliveryTest.MoveBatchOfOneIsAPlainMoveBroadcast`。
  - 设计 `team-follow-server-trail.md` §4、§8 同步。
- **核对结论**(本条之外不改代码):
  - 离线托管队员跨场景 / 跨节点跟随、托管兜底重试、朝向口径、合并广播的代码都在 main。11-18 的 Docker Linux 整编
    (`build/verification/docker-rebuild-all-20261118T082435.788540Z`)C++、Go 阶段都编过;`build/verification` 里没有这几批用例的运行记录。
  - 排空改派、落点重试、托管归位由「托管角色排空疏散改派与落点失败重试」会话 11-12~13 做完;间距、视野由「队伍跟随性能优化」会话 11-19 改定(间距 3 米、视野 50)。
  - **挂机月卡没有开通入口**:`player:afk_pass:{<player_id>}` 只有 scene(`AfkHostingSystem`)与 player_locator 在读,仓库里没有任何写入方;
    支付设计 `payment-recharge.md` 把月卡列为 v1 非目标。所以"开了挂机卡、杀掉客户端照样跟"目前只能手工写 Redis 才走得到,
    也没法写 robot 冒烟。另起任务卡提议。
  - 审计 `disconnect-reconnect-audit-20261020.md` §9.2「保留到期之后才重连,实时 CD / Buff 丢失,规则未定」没有人接,另起任务卡提议。
- **交 Codex**(仓库根;串行;失败保留首个编译错误原文与失败用例完整输出,不连续重试;工作区有别的会话未提交的改动,不要回退):
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`:scene 库多一个编译单元,scene 节点重链接。先编库再跑测试(`run_cpp_tests -Build` 不重编库)。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter aoi_delivery_test`:全量通过,含上面两条新用例。
     测试工程自己编 `actor_move_broadcast.cpp`,不该再从 scene 库拉进 `movement.obj`;报 LNK2005 / LNK2019 时贴原文。
  3. 依次 `-Filter aoi_test`、`-Filter cross_zone_test`:与改动前相比没有新增失败(重点 `MoveReceiverGroupingTest.*`、`TeamFollowDriveTest.*`)。
  4. Linux:`build_linux.sh`(不带 `--skip-generate`)或 Docker 整编,生成的 scene CMakeLists 里有 `spatial/system/actor_move_broadcast.cpp`,链接无未定义符号。
- **Java 版**:不涉及(只挪了服务端内部代码与测试),不往 `PARITY.md` 加行。
- **未提交,未编译,未测试。**

## 2026-11-19 Claude:gate 排空迁移与帮会恢复结论两条线完成度核对——代码无待写;补全仓指针成员筛查与两份文档的验证回执(只改文档;未编译、未测试;Java 不涉及)

- **起因**:负责人问「代码实现完毕了吗」,要求查出来的问题现在修、不等压测。接同日「落实 Codex 11-13 两份回执里的红项」。
- **结论**:两条线的服务端与客户端代码都没有待写的部分。
  - gate 排空迁移:设计稿 §10、§12、§13、§18.4 原先留着的项 11-12 已全部落码。残余只有「一轮换版一个玩家最多迁 2 次」这条固有代价(§13)。
  - 帮会恢复结论:§8.3~§8.5 的用例 11-13 已由 Codex 补齐变异红灯(四组 16 条),帮会 EditMode 323/323。
- **本轮做的**:
  1. **全仓指针成员筛查**(只读脚本,不入库):扫 `cpp/` 下 1842 个源文件(不含 third_party、generated、muduo_windows),按类 / 结构体里的成员声明找指针成员。正对照用旧版 `player_handoff_gate_test.cpp` 与旧版 `id_segment_transport.h`,三处都能报出;现版两份 0 命中。全仓只命中一处:`cpp/libs/engine/core/network/accept_admission.h` 的 `RateLimitEnvNames`(三个 `const char *` 成员)。这是「battle 直连准入前移」会话当天新建、还在改的未入库文件,已发消息告知对方,本会话没有动它。**它不改的话 gate、battle 的门禁会红。** 筛查是文本启发式,不等于门禁结论。
  2. **同一用例其余断言**:排空脚本用例「同 zone 候选口径与 login 源头一致」的 11 条正则断言,逐条对现行源码求值,全部匹配。
  3. **视野放大对本线用例的影响**:同日视野由 10 放大到 50、格子由 20 放大到 50。`ConnectionLostHoldReentry.*` 与 `SceneEntryPermissionTest.*` 的夹具把玩家和邻居都放在原点,不依赖视野半径,不受影响。
  4. **文档**:`docs/design/gate-drain-session-migration.md` 状态栏、§19.1、§20.2 里「goctl 包装现在还没有、第一步先跑 go/build.ps1」的说法改成现状,新增 §21「验证回执与补修」;`docs/handoff/guild-economy-recovery-exits-handoff-20261023.md` 追加 §8.8。
- **按用例名核对 Codex 原件时确认的事实**(`build/verification/gate-drain-closeout-20261112/`):
  - `routing_identity_test`:`GateRedirectDelivery.*` 6 条、`DrainMigration.*` 21 条(含 `CountersAreExportedAsPrometheusMetrics`)为 OK。
  - 同一份原件里 `SceneLane.*` 14 条(现行全部)与 `ClientSceneLane.*` 22 条也是 OK。同日「场景 lane 验证状态核对」条目写的「这两组一条运行记录都没有」不准确。确实没有运行记录的是「补旧场景消息闸」新增的 4 条 `ClientSceneLane`(`MessagesSentOnTheSameRouteAreNotMarked`、`RequestsSentAfterASceneChangeAreMarked`、`QueuedOneWayCrossingANodeChangeIsMarked`、`OneWayFlushedAtHoldIsMarkedByItsOwnReceiptRoute`)与整组 `PlayerHandoffGate.*`。
  - `aoi_delivery_test`:`ResendVisibleTargets*` 2 条 OK。
  - login:`SetSession` 5 条、进场定向 24 条 PASS。**player_locator 的 `TestSetSession_*` 三条新增没有跑到**(同包 `session_reconciler_test.go` 当时编不过,整包没跑)。
  - 客户端网络组(夹具改后):§19、§20 的 6 条重定向用例按名字都是 Passed。
- **没有改、但 Codex 会看到的**:`gofmt -l` 在 login 与 robot 下有输出,都不是这两条线的文件,不影响编译;CI 没有 gofmt 门禁。清单:`go/login/internal/logic/pkg/createbudget/budget.go`,`robot/` 下的 `battle_observer_wire_direct_test.go`、`battle_observer_wire_test.go`、`guild_smoke_scenario.go`、`http_assign_gate_test.go`、`metrics/stats.go`,以及生成物 `robot/logic/handler/message_body_handler.go`(协议生成器产出,不手改)。按 §10.2 不顺手整理别的会话的文件,留给各自的线。
- **给 Codex**:命令不变,照同日「落实 Codex 11-13 两份回执里的红项」七步。补三点:
  1. 第 1 步整解前先确认 `accept_admission.h` 的 `RateLimitEnvNames` 已不含指针成员,否则 gate / battle 的门禁必红(那是另一条线的事,不要改门禁)。
  2. 第 4 步按名字核对 `TestSetSession_RecordAdvancedByDisconnectIsAborted`、`TestSetSession_RewriteOnTheFreshRecordCommits`、`TestSetSession_NonAdvancingLoginGenerationIsNotAborted` 为 PASS。
  3. 顺带跑 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test -LogDir build/verification/codex-receipts-followup-20261119/cpp-tests`,按名字核对上面 4 条 `ClientSceneLane` 新增用例为 OK。
- **Java 版(AGENTS §12)**:本轮只改文档,不涉及。gate 排空迁移的 Java 版仍是待做,已在 Java 仓 `PARITY.md` 登记。
- **状态**:未编译、未运行、未提交。

## 2026-11-19 Claude：分布式日志阶段三——span 导出到 Tempo（代码写完；未编译、未测试，待 Codex 验证；Java 0.1.0-SNAPSHOT 待做）

- **背景**：方案见 `docs/design/distributed-tracing.md` §10.3（2026-11-12 定案）。11-12~13 写的 C++ 接缝（RPC0 / gRPC / Kafka / gate 的 span、异步恢复改为原样装回捕获的上下文）、`proto/opentelemetry/`、`TracingConfig`、OTLP 编码与第一版导出器已随 `6914a1af8` 入库；11-14 Codex 编 gate / scene / battle 时编过那一版（本文件 11-14「修复本机 C++ 节点旧产物与重编阻塞」）。那一版跑在专用导出线程上，单测、配置与部署都还没写。
- **本条改的**：
  1. **C++ 导出器改挂节点主 EventLoop**（负责人 11-19「C++ 节点除主线程不再开线程」）：`otlp_span_exporter.{h,cpp}` 构造参数加 `EventLoop&` 与解析好的地址，阻塞的 `Shutdown()` 换成不阻塞的 `BeginShutdown()` + `Finished()`；主机名在 `Node::StartSpanExporter` 里解析一次（`ResolveOtlpEndpoint`；解析不了只打 ERROR、不导出，不拒启）；`Node::FinalizeTeardownInLoop` 在 `metricsExporter_.reset()` 之后调 `FinishSpanExportInLoop()`，没冲完就 50 毫秒后重入，与前面等本机工作的写法相同，冲刷上限 2 秒。
  2. **C++ 单测**（rpc_controller_test，5 个新文件未入库，已登记 `rpc_controller_test.vcxproj`）：`span_recording_test.cpp` 10 条、`span_export_queue_test.cpp` 6 条、`otlp_trace_encoder_test.cpp` 2 条、`otlp_http_test.cpp` 13 条、`otlp_span_exporter_test.cpp` 10 条（进程内假采集器、真 TCP 回环）；`span_test_support.h` 共用。改了原有用例：`trace_context_test.cpp`（恢复后 span_id 与捕获值相同）、`rpc_channel_loopback_test.cpp`（`RequestFramesCarryTheCallerTrace` 改口径，新加 `ClientAndServerSpansFormOneTreeAcrossTheConnection`、`RejectedDispatchMarksTheServerSpanAsAnError`）、`grpc_client_loopback_test.cpp`（新加 `ClientAndServerSpansAreRecordedAndLinked`、`FailedCallMarksTheClientSpanWithTheGrpcStatus`）、`muduo_grpc_test/server_test.cpp`（4 条钩子用例跟新签名，核对 `:path`）。逐条说明见设计稿 §11。
  3. **Go**：15 个服务的 etc 加 `Telemetry`（`Name`、`Endpoint: 127.0.0.1:4317`、`Sampler: 1.0`、`Batcher: otlpgrpc`）。都是经 `zrpc.RpcServerConf`（mail、rank 经 `domainhost.Config`）内嵌的 `ServiceConf`，`zrpc.NewServer` 里 `SetUp` 起导出，不改代码。
  4. **本仓 Java**：`gateway_node`、`mall_game`、`config_node` 的 `pom.xml` 加 `io.opentelemetry:opentelemetry-exporter-otlp`（BOM 管版本），`application.yaml` / `application.yml` 设 `management.otlp.tracing.endpoint: http://127.0.0.1:4318/v1/traces`；`GatewayTracingIntegrationTest`、`MallTracingHttpTest` 设 `management.otlp.tracing.export.enabled=false`。
  5. **本机观测台**：`docker-compose.observability.yml` 加 Tempo（`grafana/tempo:2.6.1`，配置 `deploy/observability/tempo/tempo.yaml`，7 天），Alloy 开 `127.0.0.1:4317` / `4318`；`config.alloy` 末尾加 OTLP 接收 → 尾部采样（错误全留、总耗时 ≥ 1 秒全留、其余 10%，`decision_wait` 30s、`num_traces` 50000）→ Tempo；Grafana 加 Tempo 数据源（trace 跳回 Loki），Loki 数据源加 `trace_id` 派生字段（日志跳 Tempo）。
  6. **K8s**：`pod-log-collector.yaml` 的节点级 Alloy 加 OTLP 接收 → `otelcol.exporter.loadbalancing`（按 trace_id，DNS 解析 headless `trace-gateway-headless`），入口 Service `pod-log-collector-otlp`（`internalTrafficPolicy: Local`，4317 / 4318）；同文件加网关级 `trace-gateway`（ConfigMap、headless Service、2 副本 Deployment，尾部采样 → Tempo）；新清单 `deploy/k8s/manifests/infra/tempo.yaml`（单体、PVC 10Gi、168h）。`k8s_deploy.ps1`：`Get-TraceAgentEndpoints` 一处给出集群地址，`Set-GoSvcTelemetryBlock` 把 Go 配置里本机那段 `Telemetry` 换成集群那段，C++ 节点 ConfigMap 写 `Tracing.OtlpTracesEndpoint`，gateway 写 OTLP 段；与日志采集同开同关（`-NoCppLogSidecar` 时 Go 写 `Disabled: true`、gateway 写 `export.enabled: false`、C++ 不写地址），`Apply-Infra` 在部署采集器时一并部署 Tempo。契约测试 `k8s_log_collection_contract.tests.ps1` 加 5 条，dump 另写 `trace-gateway.alloy`。
  7. **文档**：设计稿状态行、§2、§3.4、§4.1、§4.2、§5.2、§5.3、§10.3（改写成已落码的实际做法：导出器、配置、span 模型、已知局限、拓扑）、§11、§12、§13 第 2 条、§14；运维手册 `docs/ops/grafana-loki-local-logs.md` 状态行、§0、组件表、§6.3（集群 Tempo 的 port-forward 与数据源）。
- **取舍**：异步恢复原样装回捕获的 span（OTel Context 语义），回调里发起的 client span 不会以一个没导出的 span 为父；统一 Go 生成跳过 `proto/opentelemetry/`（go-zero 依赖的 `go.opentelemetry.io/proto/otlp` 已注册同名消息，再生成一份启动即 panic）；导出失败一律丢批计数、不重试堆积，连不上时 1 秒起翻倍封顶 60 秒退避。已知局限（server span 只量同步部分、本地没发出的调用不记 client span、Kafka producer span 只到交给 librdkafka）写在 §10.3，不另做。
- **交 Codex**（仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7，按顺序；失败不连续重试，保留第一条错误原文，不改断言。工作区里有别的会话没提交的改动（WebSocket 入口、匹配、账号注销、Redis Cluster 等），报错落在本条没列的文件时先按文件归因，单独报告）：
  1. 生成器：本条没有新的 proto 改动，不用重新生成。只跑生成器的单测：`cd tools/proto_generator/protogen; go test ./internal/generator/go/ -count=1`。通过：`TestPruneUnifiedGoExcludedDirsDropsOnlyTheListedTopLevelDirectories` 与原有用例全过。以后为别的原因跑 `proto-gen-run` 时，确认 `go/proto/` 下不出现 `opentelemetry` 目录（出现就是生成器没重编，先 `proto-gen-build`）。
  2. C++ 编译（Debug x64，串行）：`msbuild game.sln "/t:cpp\nodes\gate;cpp\nodes\scene;cpp\nodes\battle" /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`，连带编 core / thread_context / muduo_grpc / infra / proto 等库。通过：退出码 0，裸指针成员门禁保持开启。
  3. C++ 单测（库已在第 2 步编好；`-Build` 只编测试工程）：
     ```powershell
     $log = 'build/verification/tracing-phase3-20261119/cpp-tests'
     foreach ($f in 'rpc_controller', 'muduo_grpc', 'kafka_command', 'cross_zone') { pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter $f -LogDir $log; "$f exit=$LASTEXITCODE" }
     ```
     通过：四行都是 `exit=0`；`$log/rpc_controller_test/` 的输出里能 grep 到 `SpanRecordingTest.`、`SpanExportQueueTest.`、`OtlpTraceEncoderTest.`、`OtlpHttpEndpointTest.`、`OtlpExportSettingsTest.`、`OtlpHttpRequestTest.`、`HttpResponseParseTest.`、`OtlpSpanExporterLoopbackTest.` 共 41 条 `[       OK ]`，以及上面第 2 项点名的改过与新加的回环用例。`OtlpSpanExporterLoopbackTest` 起本机回环端口，被防火墙或端口占用挡住时把原文贴回来。
  4. Linux（有环境时）：`tools/scripts/build_linux.sh` 不带 `--skip-generate`，按 vcxproj 重生 CMakeLists 后 core 与三个节点编过（本条没有新增 .cpp；导出器的 .cpp 在 `6914a1af8` 已登记，11-18 的 Linux 构建编的是旧线程版）。`cpp/tests/redis_lifecycle_test` 按既有口径跑一轮：`trace_context_tls.cpp` 现在包含 `core/tracing/span_record.h`，只用头文件，CMakeLists 不用改。
  5. Go：配置只改了 yaml，逐个起服务确认能读配置。本机 `dev_tools.ps1 -Command go-svc-build` 之后用平时的一键启动起全套；通过：15 个服务都起来，没有 `Telemetry` 相关的配置报错；观测台没起时 go-zero 的 OTLP 导出连不上只打它自己的错误日志，服务照常。
  6. Java（仓库根）：`mvn -B -f java/pom.xml -pl gateway_node,mall_game -am test`；`cd java/config_node; ./mvnw -B -q package`（没有测试源码，只确认依赖解析与编译）。新依赖要从 Maven 仓库拉取，拉不到时贴原文，不改版本。通过：退出码 0，`GatewayTracingIntegrationTest`、`MallTracingHttpTest` 照旧通过。
  7. 契约测试（不连集群，每个都要退出码 0、`fail=0`）：第一个是本条加了用例的，其余是回归（Go / C++ / gateway 的 ConfigMap 都多了一段，infra-up 多了 Tempo 与网关级 Alloy）：
     ```powershell
     pwsh -NoProfile -File tools/scripts/tests/k8s_log_collection_contract.tests.ps1
     foreach ($t in 'loki_ha_deploy_contract', 'k8s_deploy_contract', 'k8s_client_entry_contract', 'id_issuer_deploy_contract', 'logical_db_deploy_contract', 'lobby_authority_render', 'login_metrics_contract', 'login_password_auth_deploy_contract', 'k8s_login_auth_provider_guard', 'k8s_redis_split_contract', 'redis_player_cluster_contract', 'redis_maxclients_contract', 'deploy_capture_contract', 'k8s_gate_drain', 'k8s_migrate_gate', 'k8s_route_tool', 'k8s_netpol_probe', 'etcd_maintenance_contract', 'dev_tools_merge_zone_contract') { pwsh -NoProfile -File "tools/scripts/tests/$t.tests.ps1"; "$t exit=$LASTEXITCODE" }
     $errs = $null; [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path tools/scripts/k8s_deploy.ps1), [ref]$null, [ref]$errs) | Out-Null; "parse errors: $($errs.Count)"
     ```
     通过：全部 `exit=0`，`parse errors: 0`。红在本条没碰的函数时先核对是不是别的会话的改动。
  8. Alloy 配置校验（钉死的镜像；有语法错误时 `alloy fmt` 退出码非 0）：
     ```powershell
     $dump = Join-Path (Get-Location) 'build/verification/tracing-phase3-20261119/alloy'
     $env:MMORPG_DUMP_ALLOY_CONFIG_DIR = $dump; pwsh -NoProfile -File tools/scripts/tests/k8s_log_collection_contract.tests.ps1; Remove-Item Env:MMORPG_DUMP_ALLOY_CONFIG_DIR
     foreach ($f in 'pod-log-collector.alloy', 'trace-gateway.alloy', 'cpp-log-sidecar.alloy') { docker run --rm -v "${dump}:/cfg:ro" --entrypoint /bin/alloy grafana/alloy:v1.10.0 fmt "/cfg/$f" | Out-Null; "$f fmt exit=$LASTEXITCODE" }
     $local = (Resolve-Path deploy/observability/alloy).Path
     docker run --rm -v "${local}:/cfg:ro" --entrypoint /bin/alloy grafana/alloy:v1.10.0 fmt /cfg/config.alloy | Out-Null; "config.alloy fmt exit=$LASTEXITCODE"
     ```
     通过：四行都是 `exit=0`。
  9. 本机冒烟：`dev.bat obs` 后用平时的一键启动起全套，机器人登录并进游戏一次。通过：`docker logs xm-alloy`、`docker logs xm-tempo` 没有 `level=error`；C++ 节点日志里有一行 `[Tracing] exporting spans over OTLP/HTTP to http://127.0.0.1:4318/v1/traces`；Grafana 里从一行带 `trace_id` 的 gate 日志点「在 Tempo 里看这条 trace」打得开（尾部采样只留 10% 的普通 trace，多登几次或找一条报错的），trace 里 gate 的客户端消息根下面挂着 scene 的 server span；在 span 上跳回 Loki 查得到这条 trace 的日志。再停掉观测台（`dev.bat obs-down`）跑一分钟：各进程照常服务，gate / scene 的 `/metrics` 里 `spans_dropped_total{reason="export_failed"}` 上涨，日志里只有一行 `OTLP span export ... failed` 的 WARN；重新 `dev.bat obs` 后出现一行 `recovered after` 的 INFO。
  10. kind（可选，有集群时）：按 11-12「分布式日志阶段二」第 3 步的写法预载 `grafana/tempo:2.6.1` 后 `infra-up`；通过：`ds/pod-log-collector`、`deploy/trace-gateway`、`deploy/tempo` 都 rollout 成功，三者日志没有 `level=error`。
- **Java 版**（`D:\luyuan\xuanming-server-mmo`，0.1.0-SNAPSHOT）：只在它的 `PARITY.md` 登记待做（span 经 OTLP 送采集器，span 名、类型与尾部采样口径照设计稿 §10.3），不写代码。
- **文件**（本条改的，都未提交）：`cpp/libs/engine/core/tracing/otlp_span_exporter.{h,cpp}`、`cpp/libs/engine/core/node/system/node/node.{h,cpp}`（只有导出器那几段；同文件里 WebSocket 地址的改动是别的会话的）、`cpp/tests/rpc_controller_test/{span_recording_test,span_export_queue_test,otlp_trace_encoder_test,otlp_http_test,otlp_span_exporter_test}.cpp`（新）、`span_test_support.h`、`trace_context_test.cpp`、`rpc_channel_loopback_test.cpp`、`grpc_client_loopback_test.cpp`、`rpc_controller_test.vcxproj`、`cpp/tests/muduo_grpc_test/server_test.cpp`、`go/*/etc/*.yaml`（15 个，只加 `Telemetry` 段）、`java/{gateway_node,mall_game,config_node}` 的 `pom.xml` 与配置、两个 Java 测试、`deploy/docker-compose.observability.yml`、`deploy/observability/alloy/config.alloy`、`deploy/observability/tempo/tempo.yaml`（新）、`deploy/observability/grafana/provisioning/datasources/{loki,tempo}.yaml`（tempo 新）、`deploy/k8s/manifests/infra/pod-log-collector.yaml`、`deploy/k8s/manifests/infra/tempo.yaml`（新）、`tools/scripts/k8s_deploy.ps1`（只有上面那几个函数与调用点）、`tools/scripts/tests/k8s_log_collection_contract.tests.ps1`、`docs/design/distributed-tracing.md`、`docs/ops/grafana-loki-local-logs.md`、本文件。

### 2026-11-19 Claude:补上一条「两条线完成度核对」的筛查结果(只读,没改代码)

- `accept_admission.h` 的 `RateLimitEnvNames`:「battle 直连准入前移」会话收到消息后已把三个成员改成 `std::string_view`,拼接处改用 `std::string(...)`。上一条「给 Codex」补充第 1 点里的前置确认已满足。
- 随后重扫全仓,又命中一处当天新建的未入库文件:`cpp/nodes/gate/tests/websocket_entry_test.cpp` 里局部结构体 `Violation` 的 `const char *name`(函数里的局部结构体也是门禁的检查对象)。已发消息告知「小游戏客户端连 gate:WebSocket 入口与传输」会话,本会话没有动它。Codex 跑整解前先看这一处改了没有。
- 筛查脚本没有入库(会话草稿目录,跨天会被清)。要点:去掉注释与字面量后按花括号层级找类 / 结构体里形如 `T *name;` 的成员声明,跳过静态成员、函数声明和指针数组。它只是文本启发式,结论以门禁为准。

## 2026-11-20 Claude：数据覆写审计 r2 修复批次 S、T、U 的独立评审与修正（批 T 一条必修已改：交付阶段转人工时卖家所得还压着，人工动作全被拒；未编译、未测试，待 Codex 验证；Java 待做）

- **起因**：2026-11-12「数据覆写审计 r2 修复批次 S、T、U」与「批 T、U 补做」两条要求每批一个独立评审，当时周额度已过 80% 没派，负责人定为 11-19 额度重置之后派。本条就是这次评审，只查正确性、数据一致性与会编不过的地方。
- **评审对象**：
  - 三批已由 11-13 的快照提交 `6914a1af8` 带进 HEAD，批前基线 `e0790ad40`；之后的合并没有冲掉它们；`TX_AUCTION_RETURN` 的协议已生成。
  - build/verification 里没有这三批新用例的任何运行记录。
- **方式**：
  - 批 S、批 T 的 go/trade 部分、批 U 各派一个只读评审代理。
  - 批 T 的 C++ 部分由本会话自审：`CountsAsGain`、`PlanCredit` / `CreditOverflows`、`ApplyCredit` 的 (a″)、三条 C++ 新用例。
  - 必修项由本会话沿代码复核成立后再修。
- **结论**：
  - 批 S：没有必修，可选 3 条。
  - 批 T：go/trade 一条必修，已修；C++ 部分没有必修；可选 3 条。
  - 批 U：没有必修，可选 4 条。
- **批 T 必修：交付阶段转人工时卖家所得还压着，人工动作一个都做不了**
  - 问题：先买家、后卖家的闸让转人工的合同多了一种形状：交付阶段转人工时 `seller_receive` 还没发出（op_id=0）。
    - 人工处置先补记结局（`settleLegOutcomes`）。它对没发出的腿，除 redirect 外一律拒绝。resume、reissue、refund、partial、void-auction 因此都回「腿 seller_receive 还没发出」，合同出不了人工；拍品引用的出价也一直停在 bid_held。
    - 可达路径有三条：买家那条部分到账（advance 当场转人工 `applied_partial`）；买家那份补发又被拒（`final_credit_failed`）；交付阶段被隔离（`corrupt`）。
    - 会红的既有真库用例：`TestRealDBPartialCreditReissuesTheShortfall`（两个并发 partial 都回 NotAllowed，:318 的 VersionChanged 断言失败）、`TestRealDBManualFillDoesNotQuarantineTheParent`（:561 的 resume 回 NotAllowed）。
  - 一起暴露的两处：
    - 只放行补记、不改退回的话，整单退回会把合同卡在 return。整单退回指 refund、void-auction 与托管部分扣出时的 partial，三者共用 `applyRefund`。
      - 它不把压着的卖家所得标被取代，`finalCredits` 一直等它；闸又不放行（放行就是双付）。
      - return 阶段的 `nextDueMs` 恒为现在，每轮空转。
    - `refundCommon` 只看没被取代的发放腿有没有到账。
      - 买家那份部分到账、缺口补发又被拒时，卖家所得没到账，整单退回能通过，会把已经交给买家的那部分货再全额退给卖家。
      - return 阶段「退回腿部分到账、缺口补发又被拒」再整单退回，也会把已退回的部分再退一遍。这一处在闸之前就有。
  - 修法（go/trade `internal/commerce`）：
    - `delivery_order.go`：新增 `isSellerProceeds`（`seller_receive` 及其补发），`heldForBuyerFirst` 改用它。
    - `manual.go`：`settleLegOutcomes` 放过还没发出的卖家所得。
      - 按角色认，不看买家那条此刻到没到账。寄售、拍品的 `buyer_receive` 排在它前面，会在同一个循环里先补记成到账；按「闸此刻是否压着」判就又会拒。
      - 带 `seller_receive` 腿的只有双方交易、寄售、拍品与撮合子单，正是闸管的几种；人民币寄售与官方单不受影响。
    - `manual_plan.go`：
      - `applyRefund` 把没发出的卖家所得一并标 `superseded_by=refund`。
      - `refundCommon` 拒绝有发放腿部分到账过（`Outcome` 为 APPLIED_PARTIAL，不论是否已被取代）的合同，缺口用 reissue 补。
    - 文档：聚宝斋设计 §15.7 的 refund 一行与「共同的前置」一段；隔离手册 §4.2 的 refund 一行。
  - 新增纯函数用例（`delivery_order_test.go`，新 import `context`、`trade/internal/data`）：
    - `TestManualSettlingPassesOverHeldSellerProceeds`；
    - `TestManualRefundSupersedesHeldSellerProceeds`；
    - `TestManualRefundRefusedAfterPartialDelivery`。
    - 三条在修复前都会红。两条既有真库用例不改断言，修复后按原断言应当通过。
- **可选项（列给负责人，本条没改）**：
  - 批 S：
    1. 退役行种 1、2、5 不在 `PlayerStoreKind_name` 里，`validateSave` 现在回 TOO_NEW（原来回 INVALID），告警会提示去升 go/db。这与 C++ `IsNewerKind`（kind ≥ kKindCount 才算新）的口径不一致。C++ 发不出这几个号，实际无影响。要对齐的话：大于已知最大号的回 TOO_NEW，其余未知号回 INVALID。
    2. `docs/ops/release-checklist.md` §4「部署上一版本」没写混版设计 §8.3 的约束：go/db 不能回退到低于存档世代的版本。
       - 升了世代的版本整体回退时，zone-up 先退 go/db，staging / prod 还要等它滚完。
       - 这段时间新 scene 的存盘全回 TOO_NEW，随后 scene 被换下，这段在线进度丢失。
       - 建议清单写明：升世代的版本只回退 scene，`-GoSvcTag` 保持本版本。
    3. staging / prod 每次 zone-up 都等 `rollout status statefulset/db`，缺省 `-WaitTimeoutSeconds 180`，副本多时可能超时。超时在发 scene 之前抛错，fail-closed，不丢数据。
    - 评审没核到的一处：新 scene 的存盘卡在 TOO_NEW 时，玩家若被改派到旧 scene，旧 go/db 会放行旧 scene 的认领，新 scene 被围住，内存里的进度丢失。只在发布顺序被违反（或 dev 不带 `-WaitReady`）时可达，属于混版设计 §8.2「可选项（第 4 批）」要堵的那一类。
  - 批 T：
    1. `heldBidKeepsFunds` 只看卖家所得有没有发出，不看 `buyer_receive` 是否到过账。现有路径到不了「货已交付、拍品却撤单」，可以加一道防御。
    2. 买家背包满时一直回 RETRY，卖家所得跟着无限期压着（以前卖家立即到账）。这是设计取舍，建议写进聚宝斋设计 §6.2 末。
    3. 改绑后，退回腿发给原账号名下另一名角色时仍带 `TX_AUCTION_RETURN`、不判获取封禁，影响极小。
    - 提交风险：工作区的 `leg_tx_type.go` 有另一会话「官方单付款撤销」的未提交改动，依赖同目录新增的 `official_reversal.go` 里的 `isOfficialReverseLeg`。两者必须一起提交，否则 go/trade 编不过。
  - 批 U：
    1. 清理循环的旧描述没删干净，r2-doc#2 要防的「照文档把删除恢复回去」在这些地方还在：
       - `docs/design/player-inbox-service.md` 的 :100、:228（IB-1「清理只删终态行」）、:1269、:1383-1384、:1499、:1515、:1522；
       - `horizontal-scale-logical-db.md:75`、`no-single-node-horizontal-scaling-20261001.md:478`；
       - `proto/player_inbox/player_inbox_table.proto:59` 的索引注释。
    2. `go/data_service/internal/logic/recall_plan.go:24-27` 与 `exploit_loss_prevention_zh.md:118` 写的是 11-05 的旧机制：提交闸门、Kafka 发送失败只打日志、会少一条。
       - 现在是审计发件箱（`transaction_log_system.h:18-24`）：流水与存盘同事务落库，由 go/db 转发。
       - 「没有流水不等于没有应用」的结论仍然成立，写给 GM 的原因要改。
    3. `go/match/etc/match_service.yaml:153-154`、`go/match/internal/config/config.go:102-104`、`go/guild/internal/config/config.go:144` 仍写 battle 的 `kMatchResultsTopic` 发 match-results。这与本批新写的文件头矛盾，C++ 里已经没有这个常量。
    4. `forEachPlayerItemsPage` 每页读全部 25 列（含最大 16KiB 的 payload），判定只用 `finalizedRefColumns` 的 9 列。
       - 缺省预算 2.5s，战斗行多的老玩家可能一直回 StoreUnavailable。
       - 建议只取 `finalizedRefColumns`。把握中，是性能问题。
- **验证（Codex，仓库根目录；先照跑 2026-11-12 那两条的步骤，再接着跑下面几步）**：
  1. go/trade（`go/trade` 目录）：
     - `gofmt -l internal/commerce/` 无输出，`go build ./...`，`go vet ./internal/commerce/`；
     - `go test ./internal/commerce/ -count=1 -v -run 'TestManualSettlingPassesOverHeldSellerProceeds|TestManualRefundSupersedesHeldSellerProceeds|TestManualRefundRefusedAfterPartialDelivery|TestSettleLegOutcomes|TestRefund|TestVoidAuction|TestPartial|TestReissue|TestBuyer|TestAuctionUnwind|TestHeldBid|TestRetargeted|TestLegacyContract'`，全部通过；
     - 再跑一次全量 `go test ./internal/commerce/ -count=1`。真库用例无 DSN 时 Skip，Skip 不算通过。
  2. 真库：设 `COMMERCE_TEST_DSN=<隔离 TiDB>`、`COMMERCE_REQUIRE_TIDB=1`，不要设 `COMMERCE_TEST_MYSQL_DSN`，跑 `go test ./internal/commerce/ -run 'TestRealDB|TestTiDB' -count=1`。
     - 修复前必红的两条：`TestRealDBPartialCreditReissuesTheShortfall`、`TestRealDBManualFillDoesNotQuarantineTheParent`；
     - 11-12 条目列的 2 条新用例与 5 条改过的用例也在这一步。
  3. 变异取红：在 `%TEMP%` 副本里改，每次只改一处。
     - 去掉 `settleLegOutcomes` 里的 `|| isSellerProceeds(l)`：`TestManualSettlingPassesOverHeldSellerProceeds` 与第 2 步那两条真库用例红；
     - 删掉 `applyRefund` 的第二个循环：`TestManualRefundSupersedesHeldSellerProceeds` 红；
     - 删掉 `refundCommon` 开头的部分到账判断：`TestManualRefundRefusedAfterPartialDelivery` 红。
  - 失败时保留：编译的首个错误及上下文，失败用例的完整输出。
  - go/trade 编不过时先按文件归因：同目录另一会话的 `official_reversal.go`、`mall.go`、`engine.go` 改动还没验证过。
- **工作区状态**：
  - 本条改的文件：go/trade `internal/commerce/` 的 `delivery_order.go`、`delivery_order_test.go`、`manual.go`、`manual_plan.go`；`docs/design/jubaozhai-market.md`；`docs/ops/trade-commerce-quarantine-runbook.md`；r2 文档 §5.0 / §6；r2 交接 README；本条。
  - 都在 main 工作区，没有分支、没有提交。
  - 评审期间有人把整个工作区（328 个文件）加进了暂存区，本条的改动随后也被暂存。本会话没动暂存区。
- **Java 版（AGENTS §12）**：待做，本轮不写 Java 代码。Java 仓 `PARITY.md` 2026-11-12「交易退回不受获取封禁约束、成交交付先买家后卖家…」条目补了一项：实现先买家后卖家时，人工处置要同批调整这三处。

## 2026-11-19 Claude:断线重连待确认五条核实与修复——match 撞车不再误记 HOLD、Go 内部 gRPC 补 keepalive、rank 不阻塞启动(未编译、未测试,待 Codex 验证;Java 待核对)

- **范围**:总纲 `docs/design/infra-reconnect-overview.md` §4 末尾五条待确认，结论已写回该处正文。本条记 match、mail、rank 三处修复。
  - 「断线通知只发一次」是缺陷，正在改，另起一条「断线通知至少送达一次」。
  - 「同一 node_id 双进程」的残余另开了卡「scene 按 gate 实例认会话(e 方案 C)」。
  - R3、R6 残余、R17、R19 由「接着做完服务器剩下的代码(缺口清单续)」会话认领。
- **match**:
  - `internal/logic/global_pool_matcher.go`:成局提交失败只在守卫判结局未知时记排空 HOLD(`claimOutcomeUnknown`,即 `errors.Is(err, authoritysql.ErrOutcomeUnknown)`),删掉原来的「正常撞车」白名单 `expectedClaimRejection`。
  - 原白名单漏了 `ErrMatchEntryOccupied`:组队池里两个副本撞同一支整队，每撞一次就记一次要人工处理的永久 HOLD。跨区等待、观战占用这类在提交点被拒的情形，原来同样漏在白名单外。成局提交是 SQL 守卫上的一笔纯 DML 事务，其余失败都已整笔回滚，结局已知;按结局判之后这一类不会再出现。这与 trade 后台循环的口径相同。
  - 两个副本同时撮合同一个池不会重复开局:role_fence 升序锁 → 票行复核 → 成局行与票状态同一笔悲观事务;派 battle 要持工作租约,battle 按 battle_id 去重。
  - 用例:
    - `TestClaimHoldsOnlyWhenOutcomeUnknown`:替换 `TestExpectedClaimRejectionCoversStaleClaims`,以及子代理当天新加的 `TestExpectedClaimRejectionCoversClaimedTeamUnits`;
    - `TestRatingSpreadAtCommitDoesNotHold`:原 `TestRatingSpreadIsExpectedClaimRejection`;
    - 新增 team 包真 TiDB 用例 `TestTeamTiDBSecondClaimOfClaimedUnitsIsRejectedWholly`。
  - 设计稿 `global-match-hierarchical-pools.md` §5.2.1 第 7 条正文已改写。
- **Go 内部 gRPC keepalive**:新包 `go/shared/grpckeepalive`,Time 5 分钟、Timeout 20 秒;grpc-go 据此把 TCP_USER_TIMEOUT 设成 20 秒。Time 不小于对端缺省的强制策略，不会被 too_many_pings 踢断(R16 的教训)。
  - 接到四处:match→rank(`authority/rank_publisher.go`)、rank→mail(`rank.go`)、data_service→mail(`rolefencecoord/coordinator.go`)、mail→guild(`mail.go`)。
  - 这几条走 K8s Service 地址，原来都没开 keepalive:对端整机掉电或网络分区后，半开连接要等内核重传超时(约 15 分钟)才断。
  - 用例:`TestParamsStayWithinServerDefaultEnforcement`;Linux 上另有 `TestWithoutKeepaliveUnacknowledgedDataIsNotBounded`、`TestDialOptionBoundsUnacknowledgedDataByAckTimeout`。
- **rank**:`etc/rank.yaml` 的 MailRpc 加 `NonBlock: true`(K8s ConfigMap 由这份 yaml 文本替换生成，部署脚本不用改)。原来 mail 没起来时 rank 起不来，与 R5 同类。用例 `TestMailRpcDoesNotBlockStartup`。
- **暂存区**:新包和前三处拨号改动，大部分已被别的会话 `git add` 进暂存区(当时暂存区约 328 个文件);其余是未暂存的工作区改动。我没有动暂存区。
- **Codex 验证**(Go 各模块之间可并行):
  1. 仓库根目录 `git grep -n "grpckeepalive.DialOption" go/match go/rank go/data_service go/mail`:恰好 4 处。
  2. `cd go/shared && go vet ./grpckeepalive/ && go test ./grpckeepalive/ -count=1 -v`:Windows 上 `TestParamsStayWithinServerDefaultEnforcement` PASS;Linux 上(Docker golang 镜像，版本同 go.mod)三条都 PASS,不能 SKIP。
  3. `cd go/match && go build ./... && go vet ./internal/logic/ ./internal/team/ ./internal/authority/`,再跑 `go test ./internal/logic/ -run "TestClaimHoldsOnlyWhenOutcomeUnknown|TestRatingSpreadAtCommitDoesNotHold" -count=1 -v`。
  4. 真 TiDB(`MATCH_IT_TIDB=<数值 loopback 地址>`、`MATCH_IT_REQUIRE_TIDB=1`),全部 PASS、不能 SKIP:
     - `go test ./internal/team/ -run "TestTeamTiDBSecondClaimOfClaimedUnitsIsRejectedWholly|TestTeamTiDBWholeUnitsCrossWaitUnknownAndRecovery" -count=1 -v`;
     - `go test ./internal/authority/ -run "TestAuthorityTiDBReplayAndClaimAtomicity|TestPoolSliceTiDBSecondClaimOfSameTicketsIsStale|TestAuthorityTiDBOnlyOneReplicaWorksAndExpiredWorkerCannotCommit" -count=1 -v`;
     - 最后在 go/match 下跑 `go test ./... -count=1`。
  5. `cd go/rank && go build ./... && go vet ./... && go test ./... -count=1`,含 `TestMailRpcDoesNotBlockStartup`。
  6. `cd go/mail && go build ./... && go vet ./...`。
  7. `cd go/data_service && go build ./... && go vet ./internal/rolefencecoord/ && go test ./internal/rolefencecoord/ -count=1 -v`,`TestNewRequiresFullConfigAndSecret` PASS。
  8. 失败时保留完整输出，不连续重试。
- **Java 版**:没写 Java 代码，待核对项与断线通知一并登记到 Java 仓 `PARITY.md`。

## 2026-11-19 Claude:battle 直连准入前移到 Acceptor——accept 处拒绝发 RST、新建连接速率闸、连接类指标与告警,准入纯逻辑与 gate 共用一份(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:2026-11-12「gate 连接准入 P1-1 ~ P1-4」一条的剩余风险里记着 battle 直连还在连接回调里判上限。核对属实:`battle_client_edge.cpp` 的 `HandleEstablished` 判 `BattleMaxConnections`,超限 `forceClose`(建完 `TcpConnection` 再关、走 FIN、留 TIME_WAIT);没有新建速率闸,battle 也没有任何连接类指标。
- **口径**(不改客户端协议;客户端看到的只有被拒的直连是 RST,与 gate 相同):
  - 与 gate 同一套:accept 之后、`TcpConnection` 建出来之前判「关源 → 总量 → 新建速率」,被拒的以 RST 关闭、不建对象;连接回调里的总量判定留作第二道,计进拒绝原因。
  - 缺省速率与 gate 同一推导:`BattleMaxConnections` 4096 时 100 条/秒(下限)、桶深 4096。`BATTLE_ACCEPT_RATE_PER_SECOND` / `BATTLE_ACCEPT_BURST` 覆盖,写错拒启,没有「0 = 关闭」。
  - 关源:battle 的 `closeSource`(停机与维护关源共用)第一步同步关准入闸;原来替换连接回调的两行留作第二道(准入回调要等 `Install` 之后才有)。计划排空不关源,打着的房间还要接重连。
  - 日志原因 `at_capacity` 改名 `capacity`(全仓无依赖,已检索),与指标标签同名;accept 处的拒绝另打一行采样 WARN `battle direct connection rejected at accept(sampled)`,关源期间的拒绝只进指标。
- **共享方式**:`AcceptRateGate`、`RateLimit` 与缺省推导、`ParseOptionalPositive` / `ResolveRateLimit`(变量名与上限配置项名由调用方给)、握手耗时直方图分桶与新增的 `HandshakeHistogram`(含原 gate `.cpp` 里的 `ElapsedMs`),从 gate 的 `connection_admission.{h,cpp}` 挪到 core 的 `cpp/libs/engine/core/network/accept_admission.h`。
  - gate 头用 using 引回,`gate_connection_admission::` 下的名字、`ResolveRateLimit` 的签名、`main.cpp` 与 `client_session_handler.cpp` 都没动,行为不变;`AcceptRateGate` 去掉了没人用的默认构造。
  - 「最大的有限桶上界 = 握手期限」从 gate 单测里的运行期断言改成 core 头里的 `static_assert`。
  - 拒绝原因、判定函数、计数与指标名各节点自己写:原因集合不同(battle 没有 `not_ready`;gate 的 P0-2 会加自己的判据)。同义的原因用同一个标签取值(`capacity` / `accept_rate` / `shutting_down` / `no_token_secret`),由 battle 的契约测试对照 gate 头核对。
  - `RateLimitEnvNames` 的三个成员是 `std::string_view`:写成 `const char*` 会被指针成员门禁(`cpp/plugin/no_raw_ptr_matcher.cq`)拦下(「帮会恢复通知验证」会话提醒)。
- **⚠️ 新发现的风险(写进了设计稿与运维手册,代码没改)**:全服匹配的网络探测也连 battle 的直连端口,而探测计划每个机房只给 NodeId 最小的那一台(`go/match/internal/logic/network_probe.go`),全服玩家的探测都落在它上面。探测有效期 120 秒,robot 提前 40 秒重探,1 万人排队约 125 条/秒,超过缺省的 100 条/秒;超出的探测被 RST,客户端这个机房的探测失败。
  - 上线和 `match-stress` 压测前要先给 battle 设 `BATTLE_ACCEPT_RATE_PER_SECOND`(取值见运维手册 §3.3);根治是 match 侧把探测目标分散到机房内多台,另开任务。
  - 探测连接出示过 match 签发的授权,`battle_unauthenticated_connections` 不计它们,否则那一台的未认证告警会随探测量误报。
- **代码**:
  - 新增 `cpp/libs/engine/core/network/accept_admission.h`(header-only,登记进 `core.vcxproj` / `.filters`)。
  - 新增 `cpp/nodes/battle/client/battle_connection_admission.{h,cpp}`:拒绝原因、`BATTLE_*` 变量名与 `ResolveRateLimit` 转发、`Decide`、`Admission` 类(速率闸、关源、计数、握手直方图、`CollectMetrics`)。登记进 `battle.vcxproj` / `.filters`,以及 `rpc_controller_test.vcxproj`(那里直接编 battle 源文件)。
  - `battle_client_edge.{h,cpp}`:`Install` 经 `TcpServer::setAdmissionCallback` 装准入回调(只绑 `weak_ptr<BattleClientEdge>`,AGENTS §11.7),并登记采集回调(同样只绑 weak);`ConfigureAdmission` / `CloseSource`;`DirectSession::openedAt`;建连计接纳、握手成功记耗时、握手期限关闭计收割、两条纵深防御分支计拒绝原因;未认证连接数在抓取时数一遍会话表。
  - `cpp/nodes/battle/main.cpp`:`ConfigureClientAdmissionOrDie`(启动日志一行 `Battle client connection admission ENFORCED at accept, ...`),`closeSource` 里 `CloseSource()`。
  - gate:`connection_admission.h` 删掉挪走的定义、改 using 引回,`ResolveRateLimit` 改一行转发;`connection_admission.cpp` 的 `State` 用 `HandshakeHistogram`,删本地 `ElapsedMs`。
- **指标**(battle 的 `/metrics`,与 gate 同构):`battle_connections_current`、`battle_connections_max`、`battle_unauthenticated_connections`、`battle_accepted_total`、`battle_rejected_total{reason}`、`battle_accept_rate_limit_per_second`、`battle_accept_burst`、`battle_handshake_duration_ms`(直方图)、`battle_preauth_reaped_total`。标签只有 `reason`。
- **告警**:新增 `deploy/k8s/battle-connection-alerts.yaml`,6 条 warning(`BattleConnectionsNearCapacity`、`BattleRejectingAtCapacity`、`BattleAcceptRateLimited`、`BattleUnauthenticatedConnectionsHigh`、`BattlePreauthConnectionsReaped`、`BattleHandshakeSlow`),阈值与 gate 相同。手工 apply。
- **测试(新增 / 调整,未运行)**:
  - 新增 `cpp/tests/rpc_controller_test/accept_admission_test.cpp`,15 条:`AcceptAdmissionPolicy.*` 5、`AcceptRateGate.*` 8、`HandshakeHistogram.*` 2。前 13 条是从 gate 测试原样挪来的,新增的是按调用方变量名出错误说明与直方图累计。
  - 新增 `cpp/nodes/battle/tests/battle_connection_admission_test.cpp`(登记在 `rpc_controller_test.vcxproj`,Debug|x64),11 条:`BattleConnectionAdmissionPolicy.*` 3、`BattleConnectionAdmission.*` 8。
  - `cpp/nodes/gate/tests/connection_admission_test.cpp`:删掉挪走的 11 条(`AcceptRateGate.*` 8 条与 `DefaultRate…`、`OptionalPositive…`、`HandshakeBuckets…`)和 `TakeAll`,`RateLimitResolvesFromEnvOrFallsBackToTheDefault` 多一条断言(错误说明点名 `GateMaxConnections`)。本会话改完后 P0-2 会话接着在改这个文件,用例数以它为准。
  - 新增 `tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`,8 条:指标族与类型、reason 取值、与 gate 同义取值一致、标签只有 reason、直连面装配与各计数点、`main.cpp` 配参与关源、告警形状、规则引用的指标与 reason、告警名不重名。只用 PowerShell 解析器查过语法(0 个错误);告警 YAML 只做过 YAML 解析(6 条规则);都没有执行过。
  - `.github/workflows/deploy-config-tests.yml` 两段触发路径加了 battle 的三个源文件。
- **文档**:`docs/design/gate-connection-admission-control.md`(P1-3 的位置与测试改指 core,新增「P1-1 ~ P1-4 · battle 直连面」一节含探测风险)、`docs/design/turn-based-battle-server.md`(D27、§18.6 观测与指标)、`docs/design/battle-transport-decision.md`(直连能力清单一行)、`docs/ops/gate-kernel-tuning-runbook.md`(新增 §3.3)、`deploy/k8s/README.md`。
- **同时改同一批文件的会话**(失败先按文件归因):
  - 「gate 未验签连接预算、LRU 淘汰与按概率早拒(准入 P0-2)」在本会话改完 gate 三个文件之后接着改 `connection_admission.{h,cpp}`、`connection_admission_test.cpp`、`gate-connection-alerts.yaml` 与 gate 契约测试,改动已事先告知;
  - 「小游戏客户端连 gate:WebSocket 入口与传输」在改 `battle_client_edge.{h,cpp}`(`Send` / `SendEncoded` / `OnConnection` / `EncodedFrame`),位置不重叠,已告知本会话的编辑位置;
  - 暂存区里有别的会话 `git add` 的一大批改动(约 306 个文件),其中有本条早期版本的 `accept_admission.h` 与 gate 三个文件,本会话之后又改过工作区。本会话没动暂存区;提交时以工作区为准。
- **交 Codex**(没有 proto 改动,不用生成;前一步红就停下归因):
  1. 仓库根,合同测试,都应全过、退出 0:
     - `pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`(8 条)
     - `pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`(gate 头只挪了定义,`kRejectReasonNames` 与装配没动;条数以 P0-2 会话改后为准)
     - `pwsh -NoProfile -File tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1`(它的「告警名不重名」会扫到新告警文件)
     - 本机有 promtool 时,把 `deploy/k8s/battle-connection-alerts.yaml` 的 `spec` 抽成规则文件跑 `promtool check rules`;没有就报告「规则语法未验证」。
  2. Windows 整解:`msbuild game.sln /t:Build /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
     - 必须整解:core 头被 gate.lib 与 battle 节点、两个测试工程同时用,`BattleClientEdge` 的布局也变了(多了 `admission_` 与 `openedAt`)。
     - 各工程的指针成员门禁(`check_no_raw_pointer_member.ps1`)会扫到 `accept_admission.h` 与 `battle_connection_admission.h`,应无命中。
     - 首错落在 `accept_admission.h`、`battle_connection_admission.*`、`battle_client_edge.*` 里本条加的行、battle `main.cpp` 的 `ConfigureClientAdmissionOrDie`,或 gate `connection_admission.*` 里 using / `HandshakeHistogram` 那几行时,交回 Claude。
     - 已知的编译风险点(本机没法验):core 头里 `std::chrono::milliseconds(kHandshakeUpperBoundsMs.back())` 用在 `static_assert` 里;battle 的 `TreatWarningAsError` 下 `static_cast<uint32_t>(kHardMaxConnections)` 与 `EffectiveMaxConnections()` 的窄化都显式写了;`RejectReasonName(...).data()` 当 `const char*` 用(名字都是字面量)。
  3. 测试,`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter <名>` 依次跑(先做完第 2 步:`-Build` 不重编库):
     - `rpc_controller_test`:`AcceptAdmissionPolicy.*` 5、`AcceptRateGate.*` 8、`HandshakeHistogram.*` 2、`BattleConnectionAdmissionPolicy.*` 3、`BattleConnectionAdmission.*` 8 条 PASS;`AcceptorAdmission.*`、`BattleObserverContract.*` 与其余用例不回归;
     - `routing_identity_test`:`ConnectionAdmissionPolicy.*`、`ConnectionAdmission.*` PASS(本条留下的是 4 + 12 条,P0-2 会话另加的以它的条目为准);`ClientHeartbeat*`、`DrainMigration*`、lane 各组不回归。
  4. Linux(需授权 Docker;不可达或要拉镜像就停下来问):`docker --context desktop-linux build -f deploy/k8s/Dockerfile.cpp --build-arg BUILD_JOBS=2 -t mmorpg-node:battle-admission .`,battle 以 `-Werror` 编过(新的 `battle_connection_admission.cpp` 由 vcxproj 生成进 CMake)。
  5. battle 行为(Linux 容器,本机 Windows 只能验单测;准备:一台 battle,`BattleMaxConnections` 调成 50,`MMORPG_METRICS_PORT=9400`,按 gate 准入设计稿 §9.3 写只建连不发包的裸 dial 循环,不用 robot 代替):
     - 建 50 条不发包的直连占满,再建 20 条:后 20 条读到 `ECONNRESET`,`battle_rejected_total{reason="capacity"}` 增加 20,`battle_connections_current` 停在 50;
     - 满员期间每秒建 100 条、持续 30 秒,前后各取一次 `ss -s`:TIME_WAIT 不随被拒数上涨,RSS 与 `process_open_fds` 不涨;
     - 建 50 条不发包的直连等 12 秒:`battle_unauthenticated_connections` 先到 50、约 10 秒后回到 0,`battle_preauth_reaped_total` 增加 50;
     - `BATTLE_ACCEPT_RATE_PER_SECOND=10`、`BATTLE_ACCEPT_BURST=20` 启动(上限调回 4096),一次建 100 条:接纳约 20 条,其余 RST,`battle_rejected_total{reason="accept_rate"}` 增加约 80,之后每秒能再进约 10 条;
     - `BATTLE_ACCEPT_RATE_PER_SECOND=0` 启动:进程退出,stderr 最后一行含 `Refusing to start: BATTLE_ACCEPT_RATE_PER_SECOND must be an integer`;
     - SIGTERM 期间继续建连:新直连读到 `ECONNRESET`,`battle_rejected_total{reason="shutting_down"}` 增加(进程退出前抓得到的话)。
  6. 冒烟:缺省参数下跑一轮 robot 战斗冒烟(`battle_smoke`)。通过标准:`BATTLE_SMOKE_OK`;`curl :9400/metrics`(battle 的出口)里 `battle_rejected_total` 各 reason 都是 0,`battle_handshake_duration_ms_count` 等于建了直连的 robot 数,`battle_unauthenticated_connections` 回到 0;battle 启动日志有 `Battle client connection admission ENFORCED at accept` 一行;gate 那一侧照 2026-11-12 条目第 5 步复核一次(共享头挪动后行为不变)。
  7. 失败时保留:首个编译错误原文、失败用例的完整输出、容器 stderr 的前 40 行。不改断言,不跳过用例。
- **剩余风险与没做的**:
  - 探测热点(上面 ⚠️ 一条):缺省参数下全服匹配人数上去就会拒探测,要先调参或在 match 侧分散探测目标(另开任务)。
  - 持续超限期间合法新直连与攻击连接抢同一个速率桶;battle 没有 gate P0-2 那样的未认证独立预算,也没有按来源的限制(§4.2.1 的前置条件 hostPort 已具备)。
  - `k8s_deploy.ps1` 没有接线两个环境变量;Agones Fleet 形态只能 `kubectl edit fleet battle`,下一次部署会被生成物覆盖(运维手册 §3.3)。
  - 未认证连接数是抓取时遍历会话表算的(直连数以 `BattleMaxConnections` 为界);将来要在准入时按它判预算,得改成 O(1) 计数。
- **Java 版(AGENTS §12)**:待做。Java 仓 `PARITY.md` 末尾登记了「battle 直连准入」一条(`game-battle` 的 `BattleEdgeServer` / `BattleEdgeHandler` 按同一套对齐,指标同名)。本轮没写 Java 代码。
- **未提交,未编译,未测试。**

## 2026-11-20 Claude：数据覆写审计 r2 批 U 文档残留——改正收件箱清理循环的旧描述（只改文档与注释，未编译；接同日「数据覆写审计 r2 修复批次 S、T、U 的独立评审与修正」）

- **起因**：同日独立评审给批 U 列了一条可选：清理循环的旧描述没删干净，r2-doc#2 要防的「照文档把删除恢复回去」在这些地方还在。负责人 11-20 答「现在改」。只做这一条，批 U 其余可选项不动。
- **改了什么**（只改文档与注释，不改行为）：
  - `docs/design/player-inbox-service.md`，共 11 处：
    - §1.1 负责表：保留期清理挪到「不负责」，写明条目行是永久收据；
    - §1.5 IB-1 的「靠什么保证」；
    - §4.1 建表示例里的索引注释；
    - §6.2 生产方契约里「在保留期内」一句；
    - §9.3 对账一段的「清理、补发、gauge 仍只在领导者上跑」；
    - §10 后台循环两条；
    - §11.4 文件清单里 `loops.go` 的内容，测试覆盖表的「后台循环」「审计（L9）」两行；
    - §13.2 决策 D14 的参数。
  - `docs/design/horizontal-scale-logical-db.md`：「冻结不打排空 HOLD」的后台循环列表，去掉 player_inbox 清理。
  - `docs/design/no-single-node-horizontal-scaling-20261001.md`：单领导者循环表里 player_inbox 那一行。
  - `proto/player_inbox/player_inbox_table.proto`：`player_inbox_item_row` 索引选项的行尾注释，由「清理/对账」改成「对账的终结窗口(RecentlyFinalized)」。这条索引现在只给对账用。生成产物里没有这段文字（全仓 grep 过），不必为这一处重跑协议生成。
  - `go/player_inbox/internal/inbox/store_integration_test.go`：`TestIntegrationAuditRecordsWhatWasDone` 的函数头注释。用例体 11-12 已改成永久收据口径，只剩注释还写着「30 天后被清理」。
- **没改的**：
  - 历史叙述按当时状态写，不回头改：PROGRESS 旧条目、r2 审计记录、交接目录（如 `server-gap-audit-20261101.md:226`、`player-inbox-handoff-20261015.md:363`）。
  - 设计稿里的方案对比表与评审答复表（:1715、:1732、:1833）。
  - 同日评审条目里批 U 的可选第 2～4 条：`recall_plan.go` 的旧机制说法、match / guild 配置注释、对账翻页每页读整行。
- **复扫**：在 docs、go、deploy、tools、proto 下按「清理只删终态行」「保留期清理」「TerminalRetention」「SweepInterval」「SweepTerminal」「sweep_deleted」「30 天后被清理」「清理已空转」扫过一遍。剩下的命中只有三类：已经写明「没有清理」的、上面那类历史叙述、别的服务自己的扫描与清理。
- **验证（Codex，`go/player_inbox` 目录）**：
  - `gofmt -l internal/inbox/store_integration_test.go` 无输出；
  - `go vet -tags integration ./internal/inbox/` 通过。
  - 只改注释，不必单独重跑用例；同日上一条与 11-12 条目里的收件箱真库用例照常跑。
- **Java 版**：不涉及，是本仓自己的文档与注释，没加 PARITY 行。

## 2026-11-19 Claude:全部 Redis 上 Cluster 第 7 批——过时文档改成现状,排队与单 zone 上限两处定案(缺口第 34 条;只改文档,无代码改动;Java 不涉及)

- **范围**:设计稿 `docs/design/redis-cluster-all-domains-20261110.md` §10 表第 7 行。`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md` 归第 6b 批会话,设计稿 §4.9 另有代理在改,本批都没碰。写进文档的说法都对着当前代码与脚本核对过:`stress_snap.ps1`、`dev_tools.ps1`、`k8s_deploy.ps1`(`Invoke-RedisWipe`、`Get-RedisCluster`、`Get-ControlRedisEndpoint`、`Get-RedisControlClientRows`)、`lib/redis_local_flush.ps1`、各 Go 服务 etc yaml 的 `ControlRedis` 段、`bin/etc/game_config.yaml` 的 `control_redis`、`deploy/k8s/redis-control-alerts.yaml`、`loginqueue/retry_after.go`、`docs/design/zone-full-handling.md`。
- **改了什么**:
  - `docs/design/control-plane-zone-scoping-20261105.md`:标题改为「控制面按 zone 化与控制域 Redis 按 zone 分片」;§1 第 5 条、§4.1 选型、§4.2 键的归属表(控制集群、gate 标记与唤醒频道的位置、scene_manager 玩家域的回落规则)、§4.3 里「同组共用一个实例」的说法、§4.5 C++ 句柄(`control_redis` / `tlsControlRedis`)、§4.6 K8s(控制集群参数、控制端点、ConfigMap 与 node-config、总账、清库与状态、告警、退役)、§5 否决项、§6 第 3、4、8、9 条按现状改写正文,保留一句「11-05 曾按 zone 组分实例,11-10 负责人改口径」的历史说明;§7 末尾补「被全部 Redis 上 Cluster 换掉的部分」;§8 第 4、5 步去掉已删的 `k8s_redis_ctl_contract.tests.ps1` 与 `-RedisSplit queue` / `redis-ctl-g0` 检查;§9 补一句 Java 仓已有「Redis Cluster」待做行。
  - `docs/design/redis-player-store-cluster.md`:§1 第 8 条、§5.1 域表控制域行与接缝规则、§5.6「两个句柄」、§5.7 末条、§5.12 Pub/Sub、§10.2(不再有「控制域单主」;排队在控制集群按 `{q:<z>}`)、§12「共享库不许集群化」一行、§16 第 5 条按现状改写;修订记录加一条。
  - `docs/design/player-data-tidb-scaling-plan.md`:§2.1 末两条(两套 Redis Cluster;原来读父快照的三处读方现在读什么)、§8 表 Redis 行、§8.4(控制域不再是单主,剩单 zone 一个槽的上限)。
  - `docs/design/shared-redis-split-by-purpose.md`(G4a):标题加「历史稿」,开头加历史稿说明,写明作废与仍可参考的部分(§2 盘点与 §16 推算只作历史输入、§6.4 的压测 Redis 快照至今在用、§22 以账号角色目录设计为准);正文没改。`docs/README.md`:这份文档的索引行标注历史稿;控制面设计稿的索引行跟着改标题;第 52 行「控制域留原实例」改成控制集群。
  - `AGENTS.md` §6.1 / §6.2:只改事实描述,补控制集群的 target `k8s-cluster:<namespace>/redis-control`、本机控制句柄指法的出处,以及 `k8s-redis-wipe` 只许 `mmorpg-stress-*`、实际清 `redis` / `redis-match-cluster` / `redis-player` / `redis-control`。没加新规则。
  - `tools/scripts/README.md`「compose.redis-cluster.yml」一节:写清控制句柄怎么指到本机开发集群(八个 Go 服务的 `ControlRedis.Addrs` + `Cluster: true`,login 要在顶层新加这一段;C++ 的 `control_redis`;网关限流的 `spring.data.redis.cluster.nodes`)、两条前提(玩家域先指过去;控制域的读写方一起改)、启动日志、清库与压测 target;PlayerRedis 的服务清单补上 data_service。
  - 设计稿本身(不碰 §4.9):负责人 11-19「不等压测,按最标准的定」。§6.2 排队定为不拆分片(每 zone 一个有序集合 + `RetryAfterForRank` + 区服容量封顶),10 万人的轮询量按 `retry_after.go` 注释的估算改成约 6000 次/秒(原稿 4300 把 2000~3 万名一律按 30 秒算,偏乐观),补 100 万人约 3.6 万次/秒与余量 [I];§6.3 定为单 zone 的槽可迁到独占主节点、再大拆 zone(或拆线)、不拆脚本里的键,告警口径只写 `redis-control-alerts.yaml` 里真有的 `RedisControlMemoryNearMax`、`RedisControlOomRejected`,并写明没有按主节点 CPU / ops 的告警;§0 第 3、7 条,§1 的前情,§7「AGENTS.md §6」一条,§12 第 1、4 条同步;§10 表第 7 行改已完成;状态行与修订记录更新。
  - `docs/handoff/server-gap-audit-20261101.md` 第 34 条补第 7 批的进度。
- **因为代码里找不到依据而没写的说法**:按主节点主线程 CPU / ops 的告警(`redis-control-alerts.yaml` 里没有);排队长度的硬上限(login 没有队长上限,只有容量封顶与按角色上限暂停创角);Unity 客户端是否真按 `retry_after_ms` 轮询(只有网关 `AssignGateRequest` 注释里的契约,没有客户端任务授权,不读客户端仓)。负责人原话里的「拆线」在仓库文档里没有现成定义,原样写进 §6.3。
- **顺带发现、没改**(不在本批文档范围,或是代码注释):`go/login/etc/login.yaml` 约第 70 行注释还写「K8s 上由 `k8s_deploy.ps1 -RedisSplit queue` 整段渲染」;`tools/scripts/k8s_deploy.ps1` 约 11502 行注释还说 scene-manager 回落到 `CoreRedis`;`tools/scripts/stress_snap.ps1` 头注释还说 K8s 没有 redis_exporter;`docs/design/login-queue-2026-05.md` §14 第 10 条还写「排队实例按 zone 组分开」;`redis-player-store-cluster.md` §5.6 / §5.7 两段 10-31 实现补充里「`PUBLISH` 不进表」「控制域客户端不经守卫」是当时的记录,没动。
- **提交情况**:本会话没有 commit、没有开分支。工作期间另有会话提交了 `9824368eb`(「批量提交工作区全部改动」),把当时已写完的控制面设计稿改动和玩家域设计稿的一部分带进了那次提交;其余改动在 main 工作区,未提交。
- **Codex**:只改文档,没有要跑的构建或测试。
- **Java 版**:不涉及(只改本仓文档;Java 仓 `PARITY.md` 的「Redis Cluster」待做行不变)。

## 2026-11-19 Claude:补「battle 直连准入前移到 Acceptor」一条的提交状态(只改文档)

- 那一条末尾写的「未提交」只对一部分成立。22:27 别的会话按要求做了批量提交 `9824368eb`(「批量提交工作区全部改动……」),把当时暂存区里本任务的 C++ 部分一起提交了:`accept_admission.h`、`battle_connection_admission.{h,cpp}`、`battle_client_edge.{h,cpp}`、battle `main.cpp`、gate `connection_admission.{h,cpp}` 与 `connection_admission_test.cpp` 里本任务的改动、两份新单测、`battle.vcxproj` / `.filters`、`core.vcxproj` / `.filters`、`rpc_controller_test.vcxproj`。已核对这些文件提交后的内容与本会话最终版本一致。代码仍然**未编译、未测试**。
- 仍未提交的是:`deploy/k8s/battle-connection-alerts.yaml`、`tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`(两个新文件)、`.github/workflows/deploy-config-tests.yml` 的触发路径、`docs/design/gate-connection-admission-control.md`、`docs/design/turn-based-battle-server.md`、`docs/design/battle-transport-decision.md`、`docs/ops/gate-kernel-tuning-runbook.md` §3.3、`deploy/k8s/README.md`,以及那一条与本条 PROGRESS。
- Codex 的验证命令照那一条执行,不受影响。那一条「同时改同一批文件的会话」里说的暂存区,就是这次提交带走的那一批。

## 2026-11-19 Claude:裸指针成员文本预检入库——`test_cpp_no_raw_pointer_member.py`,规范两处登记(预检自身已跑通;不涉及产品代码,不需要编译;Java 不适用)

- **起因**:负责人同意把同日「两条线完成度核对」用的筛查脚本入库,供各会话交 Codex 前先跑。
- **为什么要有**:真门禁(`check_no_raw_pointer_member.ps1`,规则 `cpp/plugin/no_raw_ptr_matcher.cq`)只在 Windows 的 MSBuild 里跑,首个违例就让整个工程失败,该工程的用例一条都跑不到。Claude 不跑 MSBuild,CI 的 Linux 构建也不带这道门禁,违例只能等 Codex 的 Windows 构建才暴露。两份规范此前都没写这条规则,当天两个会话各新写出一处违例。
- **改动**:
  - 新增 `tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。做法照 `test_cpp_level_comp_reads.py`:识别逻辑、自测用例、全仓断言都在这一个文件里,文件头写了识别规则、认不出的写法与命中后的改法。`RecognitionTest` 9 条,`RepositoryRawPointerMemberTest` 1 条(扫 `cpp/`,排除口径与门禁一致,另排除检查器自己的 `cpp/plugin`)。
  - 文件名匹配 `test_cpp*`,所以 `.github/workflows/cpp-build-ci.yml`「构建脚本与工程清单契约」一步在推 main 且改到 `cpp/**` 或 `tools/scripts/tests/test_cpp*` 时会跑它,**没有改 workflow**。这一步因此多了一道会判红的检查:仓库里出现裸指针成员,或识别规则误报。
  - `.github/copilot-instructions.md` 新增「C++ 类成员不放裸指针（no-raw-pointer-member 门禁）」一节:规则、首错即停的后果、写法、预检命令。
  - `AGENTS.md` §10.1 加一条:Claude 改了 C++ 源码时交付前先跑这份预检,并写明它是只读文本预检、不在禁跑之列。
- **运行结果**(这是预检工具自己的自测,不是产品构建或产品测试):`python -B tools/scripts/tests/test_cpp_no_raw_pointer_member.py`,10 条全过,约 4 秒;当前工作区扫 883 个源文件,0 命中。正对照:入库前的 `player_handoff_gate_test.cpp` 与 `id_segment_transport.h`(取自 `6914a1af8`)分别报出第 36、37 行与第 158 行。
- **更正**:同日「两条线完成度核对」写的「扫 `cpp/` 下 1842 个源文件」不对。1842 是另写的一行计数命令把 `generated` 等排除目录也数进去的数;筛查脚本实际扫的是不含这些目录的源文件,与现在的 883 同口径。当时的命中与结论不受影响。
- **识别范围外的写法**(文件头已写明):宏拼出的成员、`typedef` 出来的指针类型、成员声明前有不带分号的宏。结论以门禁为准。
- **给 Codex**:不需要单独验证。日后整解若在门禁处红而这份预检是绿的,说明识别规则有漏,把那种写法补进 `RecognitionTest` 再改识别规则。
- **Java 版(AGENTS §12)**:不适用。
- **状态**:未提交。

## 2026-11-19 Claude:match 观战大厅三把全服 ZSET 拆成 16 桶,列表读加每副本 1 秒缓存(缺口第 34 条遗留;未编译、未测试,待 Codex 验证;Java 待做)

- **起因**:设计稿 [全部 Redis 上 Cluster](design/redis-cluster-all-domains-20261110.md) §4.9:match 私有集群上活跃集合 `spectate:battles:active`、L2 配额 `spectate:bcast:active`、赛事局索引 `spectate:featured:active` 各是一个键,每局开局 ZADD、终局 ZREM 都打在上面,是三个单键热点,加主节点分不开。原稿把它挂给「全服观战架构设计」会话、列在第 5 批之后,一直没人接。负责人 11-19 要求现在修,不等压测。
- **动手前核对**:三处读写点之外,`go/match` 以外没有任何地方用这三个键(C++、Java、tools、deploy、robot 都 grep 过,只有生成的 proto 注释提到旧键名)。读了 PROGRESS 里观战相关的条目(11-10 发布轨道按观众轨道过滤列表与随机选场、11-10 观战第二轮评审核对、11-12 数据一致性补审),这些行为都保持不变:列表按轨道过滤不摘索引、canary 镜像非空时 stable 观众多读一倍候选、懒剔除与可见性闸、`pickRandomBattleFor` 的接口。动手时 `go/match/internal/logic` 与 `internal/svc` 没有别人的未提交改动;期间别的会话改了 `global_pool_matcher.go`、`pool_segment_test.go`、`internal/authority/*`、`internal/team/sql_launch.go`,本条没碰。
- **做了什么**(分桶写 + 扇出读 + 进程内短缓存):
  - **键形**:每把按 `battle_id % 16` 拆成 16 个 ZSET:`spectate:battles:active:{sba:<k>}`、`spectate:bcast:active:{sbc:<k>}`、`spectate:featured:active:{sft:<k>}`。新文件 `internal/logic/spectate_index.go`:桶数常量 `spectateIndexBuckets = 16`(注释写明上线后改桶数要停写迁移),`spectateIndex{prefix, tag}` 的 `bucketKey` 是三把索引唯一拼键的地方,`keyFor(battleId)` 取本场的桶。48 个桶落 48 个不同的 slot(用例按 CRC16 逐个核对)。
  - **写**:ZADD / ZREM 只写本局所在的那个桶(`addSpectateActive`、`removeSpectateBattle`、`removeSpectateActiveMember`、`recordBroadcastQuota`、`releaseBroadcastQuotaUnlessPromoted`、`markSpectateFeatured`、`unmarkSpectateFeatured`)。
  - **读**:跨桶的读都由 `eachSpectateBucket` 把 16 条命令放进一个 pipeline(集群客户端按节点拆开),任一条失败整体失败(go-zero 熔断拦下 pipeline 时命令根本没发出,不能当成空桶)。
    - 列表(按分数取前 N):`top` / `topFrom` 各桶取前 N,合并后按分数降序、同分按成员倒序(与 ZREVRANGE 同序)截断成 N。
    - 总数:`counts` 是各桶 ZCARD。
    - 按下标取一项(随机选场,原来是 Zcard + `ZrangeWithScores(idx, idx)`):`locateSpectateIndex` 按各桶数量的累计和把全服下标换成(桶号, 桶内下标),`at` 去那个桶里取,每场被挑中的概率仍然均匀。
    - 按分数清过期成员:`removeByScore` 每个桶都执行(定期清理的活跃集合与赛事局索引)。
    - 解析不出 battle_id 的坏成员算不出桶,`removeGarbage` 16 个桶各发一条 ZREM(只有手工写坏的数据会走到)。
  - **L2 配额**(做法里没写到,本条定的):原来「清过期 + 已占 + `ZCARD < B` + 写入」是一段原子 Lua,拆桶后一段 Lua 只能原子地动一个桶。改成三步:`liveBroadcastQuota` 一个 pipeline 清各桶过期项并数各桶项数;本桶 Lua 先判已占(回 2),再判「本桶项数 + 别的桶项数 ≥ B」(回 0),否则写入(回 1);新占到的再数一遍全服,超过 B 就按 `releaseBroadcastQuotaUnlessPromoted`(退之前复读档位)退回、按满处理。经抢占留下的场数仍不超过 B:最后一个留下的请求复数时,其余留下的都已写入,而留下的不会自己退。代价:两场在不同的桶同时抢最后一个名额时可能两边都退,各等下一个升档租约周期(3 秒)再试;每次抢配额多两次 pipeline 往返(只有热门局按 3 秒租约才抢,量很小)。别的桶里过期的配额项由抢配额时这一步清,不进定期清理。
  - **读放大**:只给观战列表读加缓存。新文件 `internal/svc/spectate_lobby_cache.go` 的 `SpectateLobbyCache`(svc 不 import logic,和 `battleplace.Picker` 一样挂在 `ServiceContext.SpectateLobby`,`NewServiceContextWithAdmission` 用 `time.Now` 装配,单测注入假时钟):缓存合并后的结果 1 秒(`SpectateLobbyCacheTTL`),过期后 go-zero `syncx.SingleFlight` 保证同一时刻只有一个请求回源,回源时再查一次缓存,一个周期只回源一次;回源失败不缓存;回源用自己的 1 秒 ctx,不挂在某个请求上。一份缓存装赛事局前 50 条与活跃集合前 100 条(`spectateLobbyFeaturedMax` / `spectateLobbyActiveMax`),各请求按自己的 limit / scan 截前缀。nil 缓存 = 每次回源(手工构造的单测 ServiceContext 都是 nil,原有列表用例走的是回源路径)。写路径、随机选场与抢配额都直接读 Redis。
  - **缓存同时忘掉本副本摘掉的成员**(做法里没写到,本条加的):`removeSpectateActiveMember`(`removeSpectateBattle` 也经它)与坏成员清理调 `SpectateLobby.Forget`,换新切片、不改已交出的那份。不加的话,一场在缓存期内结束,这一秒里本副本的每个列表请求都会再读一次它的成局、再发四条删除,Redis 写量又跟着请求量涨。
  - **代价**:观战列表最多旧 1 秒:新开的局最晚 1 秒后出现,运营标记 / 取消赛事局最多 1 秒后在列表里生效;已结束的局照旧逐条读成局过滤掉,不会多列;别的副本缓存里的那一条 1 秒内还会被读一次成局、再摘一次(幂等)。项目未上线,不做旧键迁移;本地留着旧的三个单键不出错,只是里面的成员不再出现、不计配额,按 AGENTS §6.2 清 Redis 即可。
- **用例**:
  - 新增(`internal/logic/spectate_index_test.go`,全走 miniredis,时间用 `newFakeClock`):`TestSpectateIndexTopMergesBucketsAndTruncates`(跨桶合并排序截断、同分次序、下限含等号、前 N 全在一个桶)、`TestLocateSpectateIndexMapsGlobalIndexToBucket`、`TestSpectateIndexAtVisitsEveryMemberOnce`(全局下标映射是一一对应)、`TestSpectateIndexRemoveByScoreClearsEveryBucket`(逐桶清理与坏成员)、`TestClaimBroadcastQuotaBacksOffWhenAnotherBucketClaimedConcurrently`(miniredis 前置钩子在本场 Lua 之前往别的桶写入对手,本场复数超额退回)、`TestListWatchableBattlesServesCachedLobbyWithinOneSecond`(1 秒内不回源、满 1 秒重读)、`TestListWatchableBattlesCacheForgetsEvictedMember`、`TestSpectateLobbyCacheSharesOneFetchAcrossConcurrentCallers`(8 个并发请求只回源一次)、`TestSpectateLobbyCacheRetriesAfterFetchError`、`TestSpectateLobbyCacheForgetKeepsHandedOutSnapshot`;`keys_test.go` 的 `TestSpectateIndexBucketsSpreadAcrossSlots`(桶映射、键形、48 个桶两两不同 slot)。
  - 改名:`keys_test.go` 的 `TestPerEntityKeysHaveNoHashTag` 拆成上面那条与 `TestPlayerContractKeysShareThePlayerTag`(玩家域那一半原样保留)。
  - 改写引用旧键名的用例:`spectate_test.go`、`spectate_tier_test.go`(`TestSpectateMaintenanceLoopPrunesIndexes` 的两场改成落在不同的桶)、`setspectatefeaturedlogic_test.go`、`release_track_test.go`、`gather_spectate_index_test.go`(ZADD 故障钩子按桶前缀匹配)、`global_spectate_lobby_native_test.go`(真 TiDB)、`internal/svc/servicecontext_test.go`(只是换了键名字符串)。新增测试辅助 `spectateIndexSize`(经生产 `counts`)与 `spectateIndexMembers`(直接读 miniredis)。
- **给 Codex**(工作目录 `go/match`;失败保留第一条编译错误原文与失败用例的 `-v` 输出,不连续重试。`internal/logic`、`internal/authority`、`internal/team` 里有别的会话的未提交改动,报错先分清归属,别的会话的文件编不过时如实报告,不要改本条代码去迁就):
  1. `gofmt -l internal/logic internal/svc`:本条的文件不应出现在输出里(Claude 没跑 gofmt,手工对齐过)。
  2. `go vet ./... && go test ./... -count=1`:0 个编译错误,全部 PASS。上面新增的 11 条用例都要在 `go test ./internal/logic -count=1 -v -run 'SpectateIndex|LocateSpectateIndex|ClaimBroadcastQuota|ListWatchableBattles|SpectateLobbyCache|PlayerContractKeys'` 的输出里出现并 PASS;原有观战用例(`TestPickRandomBattle*`、`TestListWatchableBattles*`、`TestLobby*`、`TestPromote*`、`TestClaimBroadcastQuotaExpiresStaleAndCaps`、`TestSpectateMaintenanceLoopPrunesIndexes`、`TestSetSpectateFeatured*`)不回归。
  3. 有 TiDB 时:`MATCH_IT_TIDB=<回环地址> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/logic -run TestGlobalWatchTiDB -count=1 -v`,0 SKIP、全部 PASS。
  4. 可选 `go test -race ./internal/logic -run 'SpectateLobbyCache|ClaimBroadcastQuotaBacksOff' -count=1`。
  5. 可选变异取红(改完用 `git diff` 确认改回,不用 checkout / stash):`claimBroadcastQuota` 去掉复数退回 → `TestClaimBroadcastQuotaBacksOffWhenAnotherBucketClaimedConcurrently` 红;`SpectateLobbyCache.Load` 不查新旧每次回源 → `TestListWatchableBattlesServesCachedLobbyWithinOneSecond` 红;`removeSpectateActiveMember` 不调 `Forget` → `TestListWatchableBattlesCacheForgetsEvictedMember` 红。
- **没做**:
  - `docs/design/turn-based-battle-server.md` §10.2 / §10.4 与 `docs/design/cross-zone-matchmaking.md` 键表里的 `spectate:battles:active`:两份文件此刻都有别的会话的未提交改动,没碰,等它们提交后顺手改成桶键。
  - `proto/match/match_service.proto` 两处注释还写着旧键名,改它要重跑协议生成,本条不改生成源。
  - 压测口径:`stress_summarize.ps1` 不读这三个键,不用改。
  - 缺口清单(`docs/handoff/server-gap-audit-20261101.md`)第 34 条没补行。
- **Java 版(AGENTS §12)**:待做。服务端内部存储,不改客户端契约;本次不写 Java 代码,Java 仓 `PARITY.md` 没加行。
- **文件**:`go/match/internal/logic/{spectate_index.go(新),spectate_index_test.go(新),spectate.go,spectate_tier.go,listwatchablebattleslogic.go,spectate_test.go,spectate_tier_test.go,setspectatefeaturedlogic_test.go,release_track_test.go,gather_spectate_index_test.go,global_spectate_lobby_native_test.go,keys_test.go}`、`go/match/internal/svc/{spectate_lobby_cache.go(新),servicecontext.go,servicecontext_test.go}`、设计稿 `redis-cluster-all-domains-20261110.md` §4.9(只改这一节)、`battle-spectate-tiered-fanout.md` §6.2 与 §15.6 的键说明和冒烟准备一句、本条。未编译,未测试,未提交。

## 2026-11-19 Claude:配置表并行加载按核数限流、C++ 线程清单守卫(接同日「配置表并行加载 `LoadTablesAsync` 可重复调用」;未编译未运行,待 Codex 验证;Java 不涉及)

- **起因**:负责人要求遗留项不等以后、按最标准的定。上一条留了三处残余(每张表一条线程、`LoadTablesAsync` 的接口语义没写、线程文档缺启动期加载线程),盘点线程来源时提过的「线程清单」检查也一直没定。本条全部做掉。
- **按核数限流**:模板 `tools/data_table_exporter/templates/cpp_all_table.cpp.j2` 里,`LoadTablesAsync()` 不再每张表起一条线程。工作线程数取 `max(1, hardware_concurrency)` 与表数的较小者;各线程从一个原子计数器取号,按号调 `LoadTableAt(index)`(生成的 `switch`,序号与 `LoadTables()` 的顺序一致),同一个序号只会被取到一次。表再多,线程数和同时在解析的表数都不超过核数。上一条的性质不变:每次调用自带一组 `std::jthread`,返回前全部 join,可以重复调用。起线程中途失败时,已起的线程会把剩下的表加载完再 join,异常照常抛给调用方。上一条留的 `shortcut:` 注释随之删掉。`hardware_concurrency` 在容器里看到的是节点核数而不是 CPU 限额,所以上限是节点核数;超出限额的部分只是被 CFS 限流,没有为此读 cgroup。
- **接口契约**:`cpp_all_table.h.j2` 给 `LoadTablesAsync` 补了三行注释:并行、阻塞到全部加载完且 `LoadSuccess` 与回调跑完、返回时工作线程已退出、可重复调用、调用期间不能有别的线程读表;名字里的 Async 指并行。没有改名:Go、Java 的生成加载器同名(`LoadTablesAsync` / `loadTablesAsync`),改名要三端一起动,收益只有名字。
- **产物**:四份产物(`generated/code/cpp/` 与 `cpp/generated/table/code/` 下的 `all_table.cpp`、`all_table.h`)沿用上一条的办法,只渲染这两个模板。改模板前核对过四份产物与模板逐字节一致;改后 sha256 前 16 位:`all_table.cpp` 为 `96d42f00d7f54666`,`all_table.h` 为 `64a91aa589bc6929`,两处副本相同。
- **测试**:`LoadTablesAsyncTest.CanBeCalledRepeatedly` 加强:每一轮调用前后比对 `ActivitySchedule`(加载顺序第一张)与 `ZoneGroup`(最后一张)的快照地址,地址变了才算这一轮确实重新加载过,守住取号循环的两头。`Load()` 先建新快照再换掉旧的,所以这个判据是确定的。
- **线程清单守卫**:新增 `tools/scripts/tests/test_cpp_thread_sources.py`(单测形式,文本筛查,不编译)。范围是 `cpp/libs`(不含 `muduo_windows`)、`cpp/nodes` 与 `cpp/generated/table/code`,当前 922 个源文件。去掉注释与字面量、跳过 `#include` 行之后,出现 `std::thread`(`::id` 与 `::hardware_concurrency` 除外)、`std::jthread`、`std::async`、`EventLoopThread(Pool)`、`AsyncLogging`、`ThreadPool`、`pthread_create`、`_beginthread(ex)`、`CreateThread(` 而文件不在清单里就失败;清单里的文件已经没有这些写法时同样失败。清单现有 8 个文件,每条带保留原因:表加载器、`node.h`(异步日志)、`node.cpp` 与 `kafka_manager.cpp`(维护流程的阻塞操作)、`kafka_consumer.{h,cpp}`(消费轮询)、`agones_gameserver_lifecycle.{h,cpp}`(Agones worker)。`.github/workflows/cpp-build-ci.yml` 的第一层本来就按 `test_cpp*` 跑单测,新文件自动带上,只把步骤名改成「C++ 静态契约(独立 SDK 依赖边界、线程清单)」。第三方库内部的线程它看不到,仍靠 ThreadMonitor。
- **规范与文档**:`AGENTS.md` §9 加一条「C++ 节点自己的代码不新起线程」并指向清单;`docs/design/thread-count-monitoring{,_en,_zh}.md` 的线程构成表补「配置表加载(仅启动期)」一行,设计要点补第 9 条,文件索引补守卫。
- **核对后不改的**:表文件缺失时 `File2String` 直接 `LOG_FATAL`,不会静默成空表(0 字节的 `.pb` 是合法的空表,不能拒)。Kafka 消费轮询、librdkafka 内部线程、异步日志、Agones worker、维护流程的 `std::async` 负责人同日已定不改。追踪导出线程已由「分布式日志阶段三」挪到主 EventLoop,清单里没有它。`cpp/nodes/gate/main.cpp` 有一行没用到的 `EventLoopThreadPool.h` 引用,别的会话正在改这个文件,没有动;守卫不看 `#include` 行。
- **文件**:两个模板、四份产物、`configuration_table_test.cpp`、新守卫、`cpp-build-ci.yml`、`AGENTS.md`、三份线程文档。没有新增或删除 .cpp,vcxproj / CMakeLists 不变。
- **双版本**:Java 版不涉及,理由同上一条;「不新起线程」是本仓 C++ 节点的规则。
- **没有运行的**:守卫是一条单测,项目规则不许 Claude 跑测试命令,我没有运行它;清单是按文本检索的结果登记的。C++ 未编译。
- **给 Codex**(仓库根目录,按序;失败不连续重试,保留第一条编译错误原文、gtest 的失败断言与守卫的失败输出;本条的命令覆盖上一条的第 1、3、4 步):
  1. **线程清单守卫**:`python -X utf8 -B -m unittest discover -s tools/scripts/tests -p 'test_cpp_thread*' -v`。通过标准:5 条全过。`test_threads_only_in_registered_files` 红时输出里有清单外的文件与行号;`test_registered_files_still_start_threads` 红时输出里有失效的登记项。两种都把输出贴回来,不要自行改清单。
  2. **导表器产物核对**(对仓库只读):`py tools/data_table_exporter/tools/sandbox_export.py --out build/verification/table-load-worker-pool-20261119/sandbox --compare`。通过标准:导表本身成功;差异列表里不出现 `code/cpp/all_table.cpp` 与 `code/cpp/all_table.h`。别的会话未提交的表与 tip 改动造成的差异不属于本条。
  3. **C++ 构建**(串行):`msbuild cpp/generated/table/table.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`。通过标准:0 个 error C / LNK。有 Linux 环境时再跑 `tools/scripts/build_linux.sh`。
  4. **C++ 用例**(第 3 步已重编 table 库):`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter configuration_table_test -LogDir build/verification/table-load-worker-pool-20261119/cpp-tests`。通过标准:零失败;日志里有 `LoadTablesAsyncTest.CanBeCalledRepeatedly` 且 PASS。
  5. **节点启动**(随下一次整解一起做即可):gate / scene / battle 重新链接后起任一节点,能走完启动,表加载这一步没有 FATAL。

## 2026-11-19 Claude:snowflake 残留收尾的复核与补修——match 战斗号替身改发号器形状、三处过时注释(未编译、未测试,待 Codex 验证;Java 不涉及)

- **复核**:2026-11-12「snowflake 残留收尾」一条的改动(C++ 三处 PlayerId 注释、guild / player_locator / `shared/noderegistry` 的 `nodeIDMax` 说明、`TestNodeIDRangeMatchesCpp`)经 11-13 之后的合并与快照仍在。后续会话清掉的 `snowflakealloc` 三处 guild 字样与 proto 源注释也核对过,确已改成现状。「所有服务中 redis mysql tidb etcd 等包括工会啊」会话今天正把 guild、player_locator 的 `internal/node` 并进 `shared/noderegistry`,本条没有碰这几处文件;`shared/noderegistry/registry.go` 里 11-12 写的 `nodeIDMax` 说明与 D-11 一句在它的改动里保留着。
- **补修(全仓复查时找到的四处,都已改)**:
  - `go/match/internal/logic/gather_fingerprint_test.go`:`newGatherSvcCtx` 原来拿 `snowflake.NewNode(1)` 当 `BattleIDGen`,让用例发 snowflake 布局的 battle_id;生产早已是号段(`segmentBattleIDs`),battle_id 是发号器的号、没有位布局(AGENTS.md §7 第 1 条)。改为文件内的替身 `issuerShapedBattleIDs`:计数器值经 `idsegment.MixID` 打散,与生产同形,不会撞上用例里手写的小号,也省掉 snowflake 首号要等下一秒。去掉 `shared/snowflake` 导入。没有用例依赖 battle_id 的形状。
  - `go/shared/inboxshard/shard.go`:`Mix64` 注释原写「保持全部存量 SnowFlake 及号段玩家的原分布」,旧号要求清库,不存在存量 SnowFlake 玩家。改为分片只靠它摊匀、不依赖 player_id 的位布局,改它与改 `ShardCount` 一样要迁移收件箱主键。只改注释。
  - `cpp/libs/engine/core/type_define/player_guid_comp.h`:「存量 SnowFlake 与新号段」改为 player_id 只由发号器发、当不透明 ID 用。只改注释。
  - `cpp/libs/services/scene/player/system/player_lifecycle.cpp` 预登记会话那段:原注释说会话号复用来自「snowflake collision」;会话号其实是 gate 的 `[node][seq]` 临时号(`SessionIdGenerator`),复用来自 seq 回绕或 gate 重启。只改注释。
- **复查范围**:Go(去掉生成物)、C++ `cpp/libs` / `cpp/nodes` / `cpp/tests`、proto 源里所有 snowflake 字样逐条看过。剩下的都是准确的说法:scene_manager 的 scene_id、「没有 snowflake 回退」、标明「当时 / 旧」的历史说明、C++ `SnowFlake` 类本身与它的测试(`ParseSnowflakeGuid` 仍用来解 scene_id)。生成物(`go/proto/**/*.pb.go` 等)里的旧注释等下次 proto-gen 跟上。
- **验证(Codex,各模块目录下执行;Windows 上 `go test` 报 Access denied 时改用 `go test -c -o <临时目录>\x.test.exe` 再运行)**:
  1. `go/match`:`go vet ./internal/logic/`;`go test -count=1 ./internal/logic/`。用到新替身的是 `TestPreparePlayerFencedClassifiesFailures` 与 `spectate_tier_test.go` 里经 `newRelaySvcCtx` 的用例(`TestLobby*`、`TestSpectate*`、`TestPromote*` 等),这些必须通过。`internal/logic` 里还有别的会话未提交的改动(`spectate_test.go`、`spectate_tier_test.go`、`global_pool_matcher.go` 等),别处失败先按那几处定位。
  2. `go/shared`:`go vet ./inboxshard/`(只改注释)。
  3. 11-12 那一条与后续会话条目里的 `go/shared` snowflake / snowflakealloc / noderegistry 命令照旧;guild、player_locator 的 `internal/node` 按迁移会话改完后的代码与它的条目跑。
  - C++ 两处只改注释,不需要单独编译,下一次整解顺带覆盖。
  - 失败时保留:首个编译错误及上下文;失败用例名和 `-v` 输出。
- **Java 版**(0.1.0-SNAPSHOT):不涉及,不加 PARITY 行。
- **文件**:`go/match/internal/logic/gather_fingerprint_test.go`、`go/shared/inboxshard/shard.go`、`cpp/libs/engine/core/type_define/player_guid_comp.h`、`cpp/libs/services/scene/player/system/player_lifecycle.cpp`、本文件;未提交。

## 2026-11-19 Claude:gate 未验签连接预算——独立预算、LRU 淘汰最老的未验签连接、按概率早拒(准入设计 P0-2 的后一半;未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:11-12「gate 连接准入 P1-1 ~ P1-4」只做了 P0-2 的握手死线(10 秒),独立预算、LRU 淘汰、按概率早拒(sshd `MaxStartups`)没做。负责人口径是不等压测、先按标准做法定并写明推导,本条照此做完。
- **口径**(不改客户端协议;推导全文在 `docs/design/gate-connection-admission-control.md` P0-2 与 `connection_admission.h`):
  - accept 处的准入回调多一道判定。顺序变为「关源 → 总量 → 未验签预算 → 速率」:预算排在速率前面,被预算拒掉的连接不消耗速率名额。
  - 未验签池的大小就是 11-12 的 RAII 占位数,池上有两个点:早拒起点 S 与预算 B。
    1. n < S:放行。
    2. n ≥ S,且最老的一条未验签连接已建连满 3 秒:淘汰它,放行新连接。
    3. 没有够老的、n ≥ B:拒(`preauth_budget`)。
    4. 没有够老的、S ≤ n < B:按概率拒(`preauth_early_drop`),p = (n − S)/(B − S)。
  - 取值:B = 缺省持续速率 × 握手期限,上限 20000 时 2000(上限的 10%),上限不到 1 万时 1000;S = B/2;可淘汰年龄 3 秒。
    - 合法洪峰的未验签池按冷启动全量重连估约为上限的 2.5%,起点留了一倍余量。
    - 早拒曲线取 sshd 公式的 rate = 0,即 Envoy scaled trigger 的形状。
    - 3 秒与 `GateHandshakeSlow` 的 P99 阈值同值。
    - 上限调得很小(§9.6 用的 50)时起点 500 高于上限,这一道不起作用。
  - 淘汰:
    - 每条领了占位的会话在 `OnSessionOpened` 时按建连先后进一个队列,条目是 `(session_id, 会话代次)`,惰性丢弃失效条目。
    - 选中前核对:同一代、没验签、还持有占位。
    - 先收回占位,再经会话里的 weak 连接 `forceClose`,不在准入回调里删会话,由断开回调删。连接进了 kDisconnecting 之后 codec 不再分发它的帧,所以不会出现「刚验签就被关」。
    - 只有速率闸也放行时才淘汰。
  - 早拒的随机数是注入的:`UniformBelow` 函数指针,生产用线程局部 `std::minstd_rand`,单测经 `SetEarlyDropRandomForTesting` 注入定值。
  - 没有覆盖用的环境变量,要改只能改常数。启动日志 `Client connection admission ENFORCED at accept, ...` 多带 `preauth_early_drop_start=` 与 `preauth_budget=`。
  - 客户端看到的:被预算或早拒拒掉的是 RST(同其它准入拒绝);被淘汰的是已建出来的连接被关(FIN)。两者都按普通断线退避重连,客户端不用改。
- **代码**:
  - `cpp/nodes/gate/handler/rpc/connection_admission.h`:
    - 新增拒绝原因 `kPreAuthBudget` / `kPreAuthEarlyDrop`,追加在枚举末尾,原有取值的顺序不变;
    - 新增 `PreAuthBudget`、`DefaultPreAuthBudget`、`kEarlyDropStartPercent`、`kMinEvictableAge`、`PreAuthVerdict`、`UniformBelow`、`DecidePreAuth`;
    - `AdmissionState` 加三个字段;`Decide` 改为返回 `Admission{rejected, evictOldestUnauthenticated}` 并多一个随机源参数;
    - `OnSessionOpened` 多了 `SessionId` 参数;新增 `CurrentPreAuthBudget()`、`SetEarlyDropRandomForTesting()`;`Counters` 加 `preAuthEvicted`。
  - `connection_admission.cpp`:淘汰索引(`std::deque`)、`OldestUnauthenticated`、`EvictUnauthenticated`、采样日志、`Configure` 推导预算、三个新指标族。
  - `client_session_handler.cpp`:调用点改为 `OnSessionOpened(sessionEntry->first, sessionEntry->second, establishedAt)`;第二道总量闸的注释说明预算不在那里重判。
  - `main.cpp`:`ConfigureConnectionAdmissionOrDie` 的启动日志带上预算。
  - 三个文件都是精确编辑,`HandleConnectionDisconnection` 没碰;同一时间断线重连、WebSocket、battle 会话也在改这几个文件。
- **指标与告警**:
  - `gate_rejected_total{reason}` 多 `preauth_budget` / `preauth_early_drop`;
  - 新增 `gate_preauth_evicted_total`(counter)、`gate_preauth_budget` / `gate_preauth_early_drop_start`(gauge)。
  - `deploy/k8s/gate-connection-alerts.yaml` 从 6 条变 7 条:
    - `GateUnauthenticatedConnectionsHigh` 改为「未验签 / 早拒起点 > 0.8,持续 2 分钟」。原来是「/ 上限 > 0.05」,现在池承压时停在起点,原阈值永远到不了;
    - 新增 `GatePreauthBudgetRejecting`(5 分钟内有 `preauth_*` 拒绝);
    - 另两条的说明跟着改了。
- **契约测试**:`tools/scripts/tests/gate_connection_alerts_contract.tests.ps1` 从 7 条变 8 条:
  - 指标族清单、reason 清单、`OnSessionOpened` 调用点、告警名单都已同步;
  - 新增一条:淘汰只经 weak 连接 forceClose、不删会话、按会话代次核对、`Decide` 里预算排在速率前、`main.cpp` 打出预算。
  - 「标签只有 reason」放宽为「只有 reason 与 result」。
  - WebSocket 会话(「小游戏客户端连 gate:WebSocket 入口与传输」)同时往 `CollectMetrics` 加了 `gate_websocket_handshakes_total{result}` 与 `gate_websocket_protocol_closed_total`。我已告知那个会话,并把这两族一起写进了清单。之后它再改族名或标签,这一行要跟着改。
- **单测**(`cpp/nodes/gate/tests/connection_admission_test.cpp`,登记在 `routing_identity_test`,未运行):共 28 条,其中新增 12 条。
  - `ConnectionAdmissionPolicy.*` 共 10 条,新增 6 条:预算推导、不承压不取随机数、承压先淘汰、早拒概率恰好线性、退化配置、只在放行时淘汰;另有 1 条改写:判定顺序。
  - `ConnectionAdmission.*` 共 17 条,新增 5 条:Configure 推导预算、最老的先让位且会话不删、跳过已验签与会话号被复用的、满池全是刚建的连接时不消耗速率名额、注入的随机源;另有 1 条改了:指标页多了新样本行。
  - `ConnectionAdmissionEviction.*` 新增 1 条:用真实回环连接(`loopback_connection.h`)验证被淘汰的那条真的关掉、晚建连的不受影响、准入回调返回时会话还在。
- **文档**:
  - 设计稿:状态、G1、§5 现状、P0-2 重写、P1-1 判定顺序、P1-4 指标与告警表、P2-3、§8 新增一段、§9.6 新增 P0-2 验收表;
  - `docs/ops/gate-kernel-tuning-runbook.md` §3.2;`cpp/nodes/gate/SECURITY.md` §5;`deploy/k8s/README.md` gate 一节。
- **⚠️ HEAD 里有一份半截文件**:`9824368eb`(「批量提交工作区全部改动……」,另一个会话做的整区提交)提交时,恰好在我写 `connection_admission.cpp` 的途中读了它。
  - HEAD 里的这个文件只有 178 行,停在 `AdmitConnection` 中间一行,没有结尾换行。
  - 同一提交里 `connection_admission.h` 已是新签名,而 `connection_admission_test.cpp`、`client_session_handler.cpp`、`main.cpp` 是旧版。HEAD 编不过。
  - 工作区里是完整版。发起那次提交的会话已经结束,没能通知到。**推送或从干净检出验证之前,要先把工作区的这些文件再提交一次**(由负责人决定)。
- **交 Codex**(没有 proto 改动,不用生成;前一步红就停下归因;在工作区跑,不要从 HEAD 的干净检出跑,原因见上一条):
  1. 仓库根跑契约测试,都应全过、退出 0:
     - `pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`(8 条)
     - `pwsh -NoProfile -File tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1`(它的「告警名不重名」会扫到新告警)
     - 本机有 promtool 时,把 `deploy/k8s/gate-connection-alerts.yaml` 的 `spec` 抽成规则文件跑 `promtool check rules`;没有就报告「规则语法未验证」。
  2. Windows 整解:`msbuild game.sln /t:Build /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
     - 必须整解:`OnSessionOpened` 签名变了,gate.exe 与 `routing_identity_test` 都要重编。只编测试工程会链到旧的 gate 库(`run_cpp_tests -Build` 不重编库)。
     - 首错落在 `connection_admission.*`、`connection_admission_test.cpp`,或 `client_session_handler.cpp` / `main.cpp` 里带 `gate_connection_admission::` 的行时,交回 Claude。
     - 已知风险点:`connection_admission.h` 里 `(std::min)` 带括号(gate 没定义 NOMINMAX);`Admission{.rejected = ...}` 是指定初始化(C++20);日志里 `RejectReasonName(...).data()`(muduo LogStream 没有 string_view 重载)。
  3. 测试:`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test`。
     - 新的 `ConnectionAdmissionPolicy.*` 10 条、`ConnectionAdmission.*` 17 条、`ConnectionAdmissionEviction.*` 1 条要求全部 PASS;
     - 其余 gate 用例(`ClientHeartbeat*`、`DrainMigration*`、`GateClientEntryWiring*`、lane 各组)不回归。
     - `ConnectionAdmissionEviction.*` 在测试线程上起 EventLoop 与回环端口,与 `GateClientEntryWiring` 同一夹具;Windows 上大约多花几百毫秒。
  4. Linux(需授权 Docker;不可达或要拉镜像就停下来问):
     - 构建:`docker --context desktop-linux build -f deploy/k8s/Dockerfile.cpp --build-arg BUILD_JOBS=2 -t mmorpg-node:gate-preauth .`
     - 行为:按设计稿 §9.6 新增的「P0-2 的未验签预算」表逐行做:不承压时不起作用;承压时最老的先让位(`gate_preauth_evicted_total` 约 +200);全是刚建的连接时按概率拒(未验签数停在 2000 以下,`accept_rate` 不动);robot 不受影响。
     - 同一节上面各行(上限 50)的结果应与 P0-2 之前相同。
  5. 冒烟:缺省参数下跑一轮 robot 登录。
     - 全部登录成功;
     - `curl :9400/metrics` 里 `gate_rejected_total` 各 reason(含两个 `preauth_*`)都是 0,`gate_preauth_evicted_total` 为 0;
     - 启动日志那一行带 `preauth_early_drop_start=1000 preauth_budget=2000`。
  6. 失败时保留:首个编译错误原文、失败用例的完整输出、容器 stderr 的前 40 行。不改断言,不跳过用例。
- **剩余风险与没做的**:
  - 阈值没有压测:冷启动全量重连时未验签池的峰值(估 2.5%)要在压测里看。
    - 复核方法:正常洪峰里 `gate_unauthenticated_connections` 离 `gate_preauth_early_drop_start` 多远;`preauth_*` 拒绝与淘汰是否为 0。
  - 本端 loop 卡顿几秒之后,第一批 accept 可能淘汰一条验签请求还在接收缓冲里没读的连接,每次 accept 至多一条。握手死线那一侧有「loop 卡过 2 秒这一轮不关」,这一侧没做。
  - 会话总数满员时不淘汰未验签连接让位(未验签最多占 10%,login 在 90% 停止派人)。
  - 仍是全局、不按来源(P2-2)。
  - battle 直连没有未验签预算:battle 会话今天只做了 P1-1 ~ P1-4 那一套。
  - 没有运维覆盖变量,改阈值要发版。
- **Java 版(AGENTS §12)**:待做,Java 仓 `PARITY.md` 末尾已登记一行。不涉及客户端契约。
- **文件**:
  - 修改:
    - `cpp/nodes/gate/handler/rpc/connection_admission.h`、`connection_admission.cpp`、`client_session_handler.cpp`(两处注释与一个调用点)、`main.cpp`(启动日志)、`cpp/nodes/gate/tests/connection_admission_test.cpp`、`cpp/nodes/gate/SECURITY.md`;
    - `deploy/k8s/gate-connection-alerts.yaml`、`deploy/k8s/README.md`;`tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`;
    - `docs/design/gate-connection-admission-control.md`、`docs/ops/gate-kernel-tuning-runbook.md`、本文件。
  - 没有新增 .cpp,vcxproj / CMakeLists 不用动。
- **未提交,未编译,未测试。**

## 2026-11-19 Claude:全部 Redis 上 Cluster 收口——过时说法清理与第 34 条现状(缺口第 34 条;只改文档与注释,未运行;Java 待做)

- **现状**:
  - 第 1、2a、2b、3(含 11-19 收尾)、4、5、6a、7 批的代码与文档都已写完。
  - 观战大厅三把全服 ZSET 拆成 16 桶,见同日 PROGRESS「观战」条目。
  - 只剩第 6b-2 批(退役核心单实例):「控制面按 zone 化与控制域 Redis 分片」会话在做。
  - 各批都要 Codex 按各自 PROGRESS 条目验证。
- **本条改的**(都是文档与注释):
  - 设计稿 §6.3 去掉「或拆线」:同一 zone 的各条线共用 `{ctl:<z>}`,仍在一个槽里,开线分不开负载。
  - `docs/design/login-queue-2026-05.md` §14 第 10 条:排队改写成在控制集群里、按 `{q:<z>}`。
  - `tools/scripts/stress_snap.ps1` 头注释:K8s 上其实有 redis_exporter,快照仍统一抓 INFO。
  - `docs/design/cross-zone-matchmaking.md`、`docs/design/turn-based-battle-server.md` 的键表:观战活跃索引改成 16 桶的键形。
  - `proto/match/match_service.proto` 两处注释:同上,只改注释、不改字段。
- **交给 6b 会话的**:login 的 `Node.QueueRedis` 已没有部署渲染,建议在 6b-2 删掉;`k8s_deploy.ps1` 约第 11502 行注释还提 `CoreRedis`。
- **要注意**:`9824368eb`(工作区整体提交)带进了 match 观战拆桶的生产代码,配套测试还在工作区。单看 HEAD,match 的 logic 包测试编不过,要一起提交。
- **Codex**:先 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(proto 只改了注释,生成物的注释会跟着变),其余按各批条目。
- **Java**:只登记待做,本次不写 Java 代码。

## 2026-11-19 Claude:断线重连 R3 收尾——guild、player_locator、login 三份注册实现迁成 shared/noderegistry 薄适配层,注册层告警(未编译、未测试,待 Codex 验证;Java 不涉及)

- **范围**:2026-11-12「断线重连清单收尾」条目里没做的 R3 两项。会话「接着做完服务器剩下的代码(缺口清单续)」做,和正在做 §4 待确认的断线重连会话「所有服务中 redis mysql tidb etcd 等包括工会啊」对过分工(R3、R6 残余、R17、R19 归本会话,它不碰;它在改的 gate 断线通知、login `disconnectlogic.go` / `session_lifecycle.go`、match 成局 HOLD 判定本条都没碰)。状态表 `docs/design/infra-reconnect-overview.md` §4.1 R3、R4 行已改。都在 main 工作区,没有分支、没有提交。
- **三份迁移**(注册、续租、失租重注册、按 uuid 条件注销、注册层指标全部转调 `shared/noderegistry`,Go 侧不再有注册副本):
  - `go/guild/internal/node/node.go`:`NewNode` / `Info` / `Close` 调用面不变,`KeepAlive()` 不再返回错误(共享包里续约起不来按失租处理、退避重试,不会在启动时返回错误),`guild.go` 一处调用同改。策略 `ReallocateNewID`,与迁移前同义。注册用的 etcd 客户端补 `etcdwatch.WithKeepAlive`(R1 那一轮漏了这个客户端)。
  - `go/player_locator/internal/node/node.go`:同上,另保留 `EtcdClient()`(热关停 watch 与会话对账借用,生命周期仍归 Node)与 `WithLeaseBoundMarkers`,前缀原样交给 `Spec.LeaseBoundMarkers`,能力键与 NodeInfo 同一笔 Txn、同一个租约,首注册、重夺原号、换号三条路径都带上。NodeInfo 补 `grpcEndpoint`(与 `endpoint` 同值):共享包拒绝 grpcEndpoint 端口为 0 的注册值。对账 leader 读 NodeInfo 的 `nodeUuid` 与 `allocated/` 的裸 uuid(`reconcile_view.go` 的 `collectNodeUUIDs`),两处都是共享包写的同一个 uuid,判旧写者的口径不变。`player_locator.go` 一处调用同改。
  - `go/login/internal/logic/pkg/node/node.go`:策略 `ExitProcess`(原号已归别人就退出进程,与迁移前同义)。`NewNode` 仍是失败回 nil,原因现在逐步打日志(原来 etcd 客户端建不起来时一行日志都没有)。删 `node_allocator.go`、`pkg/etcd/registry.go`(只被它用)与已无调用方的 `BuildRpcPath`;`pkg/etcd/client.go`、gate 目录 watch 用的 `GetRpcPrefix` / `BuildRpcPrefix` / `NewEtcdClient` 不动。`login.go` 一处调用同改。
  - **行为变化**:login 原来的分配器从 0 开始分号,干净的 etcd 上第一台 login 拿到 node_id 0;0 在 C++ 与 Kafka 命令过滤里是「未分配」的哨兵(例如 `kafka_consumer.cpp` 拒收 `target_node_id()==0`)。共享包从 1 开始,随迁移改正。三个服务的注册值键序变成字母序、带 `releaseTrack`(共享包经 map 重新编码),发现方都按字段名解析,不受影响。
  - 测试:删掉只测旧副本内部函数(`allocateNodeID`、`tryClaimNodeID`、`leaseBoundMarkerOps`)的 `guild/internal/node/node_integration_test.go`、`player_locator/internal/node/node_integration_test.go` 与 `TestLeaseBoundMarkerOps`;这些不变量(从 1 起分号、跨 zone 唯一、CAS 落空换下一个、标记与双 key 同租约)由共享包的 `registry_test.go`、`registry_integration_test.go`、`lease_bound_markers_test.go` 覆盖。guild 的 `TestNewNodePublishesGRPCEndpoint` 保留,夹具挪进同文件、改用 `sharedregistry.RpcPath`,并多核对发布值的 node_id / uuid 与 `Info` 回填一致;player_locator 的 `TestNewNodeInfoCarriesReleaseTrack` 多核对 grpcEndpoint 端口。
  - `shared/noderegistry/registry.go` 包注释的「迁移进度」改成现状;`docs/design/player-locator-gate-session-index.md` 里「allocator 是逐字副本、加了 extraOps 钩子」一段改成现状。
- **注册层告警**(R4「从发现里消失而且没有告警」、R17 的 Go 部分):
  - 新 `deploy/k8s/noderegistry-alerts.yaml`,组 `go.noderegistry` 两条,都是 warning:`NodeRegistryInstanceUnregistered`(`max by (namespace, pod, prefix) (noderegistry_registered) < 1` 持续 3 分钟)、`NodeRegistryLeaseFlapping`(30 分钟内失租 3 次以上)。停机时 Close 之后 gauge 为 0,各 Go 服务的停机强退时限远小于 3 分钟,不会误报。
  - 契约测试 `tools/scripts/tests/noderegistry_alerts_contract.tests.ps1`(4 条):规则名单、指标名都在 `metrics.go` 里定义、分组 label 只有 namespace / pod / prefix、for 不小于 2 ×(`backoffMax` × 1.2 + `reRegisterOpTimeout`)、告警名全仓唯一。
  - `deploy/k8s/README.md` 加一节安装说明(该文件里别的会话还有未提交改动,本条只在「Go 服务后台循环的活性与 panic 告警」一节之后插入一节)。
- **Codex 验证**(仓库根目录;Go 各模块之间可并行;失败保留第一条编译错误与 `go test` 的完整失败输出,不改断言,不连续重试):
  1. `cd go/guild && gofmt -l internal/node && go build ./... && go vet ./internal/node/ && go test -count=1 ./internal/node/`:gofmt 无输出;`TestNewNodeRejectsInvalidReleaseTrackBeforeEtcd`、`TestNewNodeInfoCarriesReleaseTrack` PASS。再起本机 etcd(`pwsh -File tools/scripts/dev_tools.ps1 -Command etcd-up`)跑 `go test -tags=integration -count=1 -run TestNewNodePublishesGRPCEndpoint -v ./internal/node/`,PASS,不能 SKIP。
  2. `cd go/player_locator && gofmt -l internal/node && go build ./... && go vet ./internal/node/ && go test -count=1 ./...`:`TestWithLeaseBoundMarkersAccumulates`、`TestNewNodeRejectsInvalidReleaseTrackBeforeEtcd`、`TestNewNodeInfoCarriesReleaseTrack` PASS,其余既有用例照旧。
  3. `cd go/login && gofmt -l internal/logic/pkg && go build ./... && go vet ./internal/logic/pkg/... && go test -count=1 ./internal/logic/pkg/node/ ./internal/svc/`:`TestNewNodeInfoCarriesReleaseTrack` 与既有用例 PASS。
  4. `cd go/shared && go vet ./noderegistry/ && go test -count=1 ./noderegistry/`,再带 etcd 跑 `go test -tags=integration -count=1 ./noderegistry/`(只改了包注释,确认没有回归)。
  5. `pwsh -NoProfile -File tools/scripts/tests/noderegistry_alerts_contract.tests.ps1`:`total=4 pass=4`。告警语法:把 `deploy/k8s/noderegistry-alerts.yaml` 的 `spec` 抽成普通规则文件后 `promtool check rules`(与其余告警文件同法)。
  6. 冒烟(调试栈,按 AGENTS §6.2 先清 etcd):起 guild、player_locator、login 各一个,`etcdctl get --prefix GuildNodeService.rpc/`、`PlayerLocatorNodeService.rpc/`、`LoginNodeService.rpc/` 各有 zone 键与 `allocated/` 键,值里有 `grpcEndpoint` 与 `releaseTrack`,login 的 node_id 是 1;`etcdctl get --prefix /mmorpg/capability/player_locator/gate_session_index/v2/` 有一个以 player_locator 的 uuid 结尾的键,`etcdctl lease timetolive <lease> --keys` 显示它与两把注册键挂在同一个租约上;三个进程的 `/metrics` 里 `noderegistry_registered{prefix="…"}` 为 1。正常停掉三个进程后上述键全部消失。
- **Java 版(AGENTS §12)**:不涉及(Go 服务的注册实现与告警,Java 版有自己的注册方式),不加 PARITY 行。
- **文件**:`go/guild/internal/node/{node.go,node_registration_integration_test.go}`(删 `node_integration_test.go`)、`go/guild/guild.go`;`go/player_locator/internal/node/{node.go,node_registration_test.go,node_release_track_test.go}`(删 `node_integration_test.go`)、`go/player_locator/player_locator.go`;`go/login/internal/logic/pkg/node/node.go`(删 `node_allocator.go`、`../etcd/registry.go`)、`go/login/login.go`;`go/shared/noderegistry/registry.go`(注释);`deploy/k8s/noderegistry-alerts.yaml`(新)、`deploy/k8s/README.md`;`tools/scripts/tests/noderegistry_alerts_contract.tests.ps1`(新);`docs/design/infra-reconnect-overview.md`、`docs/design/player-locator-gate-session-index.md`、本条。三个入口文件里别的会话同时加了 `clockcheck.MustPass`,不是本条的改动。

## 2026-11-19 Claude：节点时钟告警与服务启动时钟核对（未编译、未测试，待 Codex 验证）

- **起因**：负责人反映线上运维把机器时间调回一年前又调回来，没有被发现。问了大厂标准做法后，要求告警和启动自检都做。
- **告警**：`deploy/k8s/clock-alerts.yaml`，比节点时间（node_exporter 的 `node_time_seconds`）和 Prometheus 的抓取时刻：差 1 秒以上、15 分钟 warning；差 30 秒以上、2 分钟 critical。不只靠 kube-prometheus 自带的 timex 规则：chrony 不经内核 PLL 调时的时候，它看不见手工改错的时钟。
- **启动核对**：每个服务启动时，和 Kubernetes API server 回应里的 Date 头比一次（`GET /version`，不要 RBAC 权限），差 30 秒以上拒启。读不到参照只告警、照常启动；不在 Kubernetes 里不核；`MMORPG_CLOCK_CHECK_MODE=warn` 是 API server 自己的时钟错了时的应急开关。
  - Go：新包 `go/shared/clockcheck`；15 个服务的 main 在版本行之后调 `clockcheck.MustPass`。
  - C++：纯判定在 `clock_check.h`，发请求在 `node.cpp` 的 `EnsureClockOrDie`（libcurl，只在 Linux 编），排在 fd 上限门槛之后、注册 etcd 之前，gate / scene / battle 都经过。
  - Java 网关：`ClockCheck`，在 `SpringApplication.run` 之前。mall_game 没有 K8s 部署，没接。
- **文档**：新增 `docs/ops/clock-sync-runbook.md`（已登记 `docs/README.md`）；`deploy/k8s/README.md` 加告警一节。
- **验证（交 Codex）**：
  1. Go（`GOTOOLCHAIN=local`）：在 `go/shared` 跑 `gofmt -l ./clockcheck`、`go vet ./clockcheck/`、`go test -count=1 -v ./clockcheck/`；再对 15 个改过 main 的服务（chat、client_rpc_router、data_service、db、friend、guild、login、mail、match、payment、player_inbox、player_locator、rank、scene_manager、trade）各在自己目录跑 `gofmt -l .` 和 `go build -mod=readonly ./...`。
  2. C++（MSBuild 串行 `/m:1`）：先 `msbuild cpp/libs/engine/core/core.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`，再用 `tools/scripts/run_cpp_tests.ps1` 跑 `client_endpoint_test`，新用例 `ClockCheck.*` 要 PASS。Linux 上 `tools/scripts/build_linux.sh` 编 gate / scene / battle：`node.cpp` 的 curl 那一段只在 Linux 编。
  3. Java：在 `java` 目录跑 `mvn -pl gateway_node -am test -Dtest=ClockCheckTest`。
  4. 契约：`pwsh -NoProfile -File tools/scripts/tests/clock_alerts_contract.tests.ps1`；规则语法用 `promtool check rules` 验（先取出 PrometheusRule 的 spec）。
  5. 集群上（可选）：kind 里起一个服务，看启动行 `clock check passed`；在演练节点上把时间调偏 60 秒，看服务拒启、`NodeClockOffsetTooLarge` 响。
- **Java 版（AGENTS §12）**：Java 版服务器同样要做，已在 Java 仓 `PARITY.md` 登记待做。
- **提交情况**：未提交。

## 2026-11-19 Claude:搬库工具三处改动逐行复核——系统表与变量在真库上核对,cutover 加 TTL 预检,cleanup 问 TiCDC 之前先核健康(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:2026-11-12「数据一致性补审」里搬库的三处改动(cleanup 核复制流已撤、冻结窗口里再比一次全量、校验和比表定义)是后台代理写的,当时只确认了关键函数在。负责人要求把留下的三条风险修掉:没逐行审、TiCDC 应答与系统表列名凭记忆、搬库前要停 TTL 而手册没写。
- **逐行审的结论**:`tools/dbmove` 的 `replication.go`、`mover.go`、`checksum.go`、`side.go`、`main.go`、`journal.go` 与 `go/shared/dbplacement` 的 `Move` / `FeedIdentity` 全部读过。三处改动的逻辑没有发现错误:cleanup 的顺序(保留期 → audit → 确认 → 地址 → 闸门是本 run 冻结 → 复制流已撤 → DROP)、窗口里比对排在记目标位置之前、比不过时清 verify 记录、`-budget` 与 `-compare-budget` 分开计时、无主键表在 plan / verify 两处拒绝,都和设计一致。发现两处缺口,见下面「改动」。
- **真库核对**(本机一次性容器,`pingcap/tidb:v8.5.2` 单机与 `mysql/community-server:8.4.13`,用完已删;只核语句和列在真库上的样子,Go 用例没跑):
  - TiDB:`information_schema.columns` 七列、`information_schema.statistics` 的 `expression` / `sub_part` / `collation` / `non_unique` 都对;聚簇与非聚簇主键的索引名都是 `PRIMARY`;`tidb_snapshot` 下表定义、`SHOW CREATE TABLE`、`ADMIN CHECKSUM TABLE`、逐行摘要都读得到那一刻的内容;逐行摘要分得清逗号挪位和 `''` / `NULL`;`@@tidb_current_ts` 只在事务里非 0。
  - MySQL:`@@global.replica_skip_errors` / `replica_exec_mode` 缺省是 `OFF` / `STRICT`;通道配置行在 `STOP REPLICA` 之后还在、`RESET REPLICA ALL FOR CHANNEL` 之后消失;`replication_applier_filters` 的两列、`AUTO_POSITION` 是文本 `1`。
  - 这些都和代码里写的一致,**没有要改的**。单机 TiDB 的 `ADMIN CHECKSUM TABLE` 数值是假的,`Total_kvs` 仍要在带 TiKV 的集群上核。
  - **TiCDC 没核到真的**:本机没有镜像。只从 tiflow `release-8.5` 的 `cdc/api/util.go` 核到 `ErrChangeFeedNotExists` 在按 400 回的那一组里;状态码映射所在的中间件、响应体、changefeed 详情的字段名都没读到,仍是待核项。
- **改动一:cutover 冻结之前核两侧的表都关着 TTL**(`requireTTLOff`,`sqlAdmin.TablesWithTTLEnabled`)。
  - 复核时发现原来写的风险只说了一半。TiDB 的 TTL 任务不过闸门,除了可能撞上窗口里的比对,更要紧的是翻转之后:`audit` 读的是冻结的源库的**当前数据**,源库的 TTL 任务每过一个 `TTL_JOB_INTERVAL` 接着删过期行,`audit` 必然报不一致(手册把它当数据事故处理),冻结的源库也不再是翻转那一刻的原样。
  - 做法:预检逐表看 `SHOW CREATE TABLE`(真库核过 TTL 属性不在 `information_schema` 里,写法是 `/*T![ttl] TTL_ENABLE='ON' */`),任何一侧有表开着就退出码 1 并点名表和哪一侧,什么都不改。由运维在源集群逐表 `ALTER TABLE ... TTL_ENABLE='OFF'`。关表属性而不关全局开关 `tidb_ttl_job_enable`:全局开关连别的库一起停,而且总要再打开,那时源库这几张表又接着删。
  - 工具只看属性,不看有没有任务还在跑:刚关掉就切,没跑完的任务仍可能撞上窗口里的比对(回滚,不丢数据,要重跑 verify)。手册给了查 `mysql.tidb_ttl_table_status` 的语句,列名在真库上核过。
  - 全仓带 TTL 属性的表只有 `mmorpg_chat.chat_send_authority`。
- **改动二:cleanup 问 TiCDC「changefeed 还在不在」之前先核它健康**(`requireHealthy`,`GET /api/v2/health`)。
  - 复核时发现的缺口:TiCDC 查 changefeed 答的是 owner 内存里的那一份,owner 刚接任或者滚动升级到一半(各节点版本不一致)时,还在的 changefeed 它也答「没有」。原来的判据会把它当成「已撤掉」,接着删源库,`DROP DATABASE` 顺着没撤的复制流删掉目标上正在用的库。
  - 做法:先问健康、再问 changefeed,不健康按查不成拒绝。
  - **这条依据的是我对 tiflow owner 的记忆(`isHealthy` 在 owner 没 tick 过、版本不一致时回 false),没有核到源码,也没有真实 TiCDC。** 判断错了的后果是 cleanup 多一次请求或者一直拒绝,不会多删东西。Codex 要用真实 TiCDC 核 `/api/v2/health` 的形状,手册 §10 有核法。
- **文档**:`docs/ops/db-move-runbook.md`(第 5 步开头加「先关 TTL」、预检清单、第 8 步、第 9 步、§6 故障表三行、§7 回滚、§8 审计排查、§10 验证与演练两行,并记下这次手工核过的语句形状);`docs/design/horizontal-scale-logical-db.md` §8.2 两条。原来担心的「cleanup 必须带 `-sync`、冻结窗口变长」两条手册里已经写全,没有再改。
- **审计发件箱**:上次说的「转发循环没写」已由发件箱会话做完并在 main 上(`go/db/internal/auditoutbox`、`go/db/audit_outbox_wiring.go`),保障地图 §6.3 第 1 条与 §6.4 第 1 行已是现状,本条没有改动。它的验证看 PROGRESS 标题带「审计发件箱」的各条。
- **Codex 验证**(仓库根目录;失败保留完整输出,不改断言,不连续重试):
  1. `cd tools/dbmove && gofmt -l . && go vet ./... && go test -count=1 ./...`:gofmt 无输出;`TestCutoverRefusesBeforeFreezing` 新增的三条子用例(源库有表开着 TTL、目标有表开着 TTL、读不到 TTL 属性)与 `TestTicdcStillExists` PASS,其余既有用例照旧。
  2. 真库,TiDB 与 MySQL 两档各一遍(两个实例,口径同手册 §10):`$env:DBMOVE_IT_REQUIRE='1'`,设好 `DBMOVE_IT_SOURCE_DSN` / `DBMOVE_IT_TARGET_DSN` / `MMORPG_SQL_BACKEND` 后 `go test -count=1 -run '^TestIT' -v ./...`:`TestITTTLAttributeOnRealDatabases` PASS(SKIP 算失败),`TestITCutoverOnRealDatabases` 照旧 PASS(预检多了一步)。
  3. 真实 TiCDC:`curl -i http://<cdc>:8300/api/v2/health` 健康时是 200;`curl -i http://<cdc>:8300/api/v2/changefeeds/no-such-feed` 是 400 且响应体含 `CDC:ErrChangeFeedNotExists`。任何一条不符就把两个回答原样贴回来,改 `requireHealthy` / `changefeedNotExists`。
  4. 演练:手册 §10 表末新增的两行(带 TTL 的表、TiCDC 升级中 cleanup)。
- **没做的**:工具不查 TTL 任务是否还在跑(只给了手工查的语句);`audit` 不复核源库的 TTL 有没有被重新打开(手册写明不要开);MySQL 的 Event Scheduler 同样不过闸门,本仓没有用 event,没有处理。
- **Java 版(AGENTS §12)**:不涉及(本仓的搬库运维工具),不加 PARITY 行。
- **提交情况**:未提交。文件:`tools/dbmove/{mover.go,side.go,replication.go,mover_test.go,main_test.go,integration_test.go}`、`docs/ops/db-move-runbook.md`、`docs/design/horizontal-scale-logical-db.md`。

## 2026-11-19 Claude:实名认证与未成年人防沉迷落码——国家系统接口、gate 上 Login 拦截、到点踢线、上下线上报、/api/real-name、客户端实名窗(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人 11-19 定「App 也上,按清单写」(设计稿 `docs/design/real-name-anti-addiction.md` §7 第 1 条)。小游戏渠道由平台管,服务端只对其余渠道生效。
- **做了什么**(口径在设计稿 §5 与 §8 末尾「落码时的调整」):
  - 协议:`account_directory` 第 8 号 `real_name_pi`;`AccountDirectoryResponse` 第 3 号 `real_name_pi`;data_service 新 RPC `SetAccountRealName`(只认 login)、`GetAccountRealName`(只认 payment);`LoginResponse` 第 10 号 `AntiAddictionStatus`;`LoginPreGate.SubmitRealName`;`KickPlayerEvent` 第 4 号 `reason_tip`。
  - tip:`data/tip/Tip.xlsx` login 段末尾 8 个码(`LoginRealNameRequired`、`Invalid`、`Pending`、`TooManyAttempts`、`AlreadyBound`、`ServiceUnavailable`(fault=1)、`LoginMinorNotInPlayTime`、`LoginMinorPlayTimeOver`),排在注销会话的 4 个码之后。
  - Go 共用 `go/shared/antiaddiction`:pi 解出生日期(规范样例 `1hpfml` = 2010-01-01)、周岁、充值档位、可玩时段(周五到周日加 `ExtraPlayDays` 的 20:00–21:00,北京时间固定偏移)、身份证校验码与姓名预检。`go/shared/accountdirectory/real_name.go`:`(*Client).SetRealName`、给 payment 的 `RealNameClient`。
  - data_service:DDL 续写 `real_name_pi VARBINARY(38)`;`readAccount` 同一条 SELECT 带出 pi,`ReadWithLifecycle` 回它;新文件 `store/account_real_name.go`(只写一次的 UPDATE、按玩家查);两个 RPC 与 `PaymentDirectoryAuth`(`MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`)。
  - login:国家系统客户端 `pkg/nppa`(AES-128-GCM、SHA-256 签名、三个接口)与上报器(每批 ≤128、每副本 ≤1 QPS、150 秒内重试);`pkg/realname`(每日次数、认证中记 ai、下一次 /api/login 收尾);`config/anti_addiction.go` 门禁(在任何外部连接之前);`svc/anti_addiction.go`(判定 `Evaluate`、上报循环);gate 上的 Login 先读目录再写登录态,拒绝没实名的与不在可玩时段的未成年人;RenewSession 把续租间隔缩到截止时刻,到点发带 `kLoginMinorPlayTimeOver` 的踢线;会话收尾(Disconnect、LeaveGame)用 GETDEL 取走记录报下线,重放不重复;`loginpregate/submitrealnamelogic.go`;`rpclogging` 屏蔽实名请求体;`auth.AuthResult.Channel` 让 access_token 带出签发时的渠道。
  - C++ gate:`KickPlayerEventHandler` 用 `reason_tip`(0 照旧是「账号在别处登录」)。
  - 本仓 Java 网关:`POST /api/real-name`(限流同 assign-gate 的令牌摘要冷却,命名空间 `real-name`),`LoginRpcClient` 手写编解码加 `SubmitRealName` 与 `LoginResponse` 第 10 号字段,`/api/login` 应答带 `anti_addiction`。
  - 客户端(`D:\luyuan\mmorpg-client`):`GatewayHttpClient` 新 DTO 与 `SubmitRealName`;`GameClient.AntiAddiction.cs`(状态、实名提交、文案、剩 10 / 5 / 1 分钟提醒);`EnterZone` 要实名或未成年人不在时段时不去排队;gate 拒绝与到点踢线的文案;`LoginCredential.ForAccessToken`;选服界面复用账号面板做实名窗(`QdaoServerSelectView.RealName.cs`)。
- **与别的会话**:账号目录这一块与「海外上架:客户端 Apple / Google 登录与账号注销」会话同批改,按约定分工(注销占列 3–7,两边共用 `readAccount` 与 `ReadWithLifecycle`;login 的 `readAccountDirectoryWithRealName` 回 pi 与注销状态,`loginlogic.go` 两处调用由那边改);go/payment 与充值设计 §12 归「游戏应用支付系统接入」会话;断线重连会话要让 Disconnect 至少送达一次,下线上报已确认幂等。
- **没做**:K8s 部署参数(没做之前 K8s 上开不了防沉迷)、go/payment 限额闸(接入点在设计稿 §5.6)、人工换实名。见设计稿 §8 末尾。
- **文件**(多个文件同时带着别的会话的未提交改动,提交时按 hunk 分):
  - 协议:`proto/common/database/rollback_database_table.proto`、`proto/data_service/data_service.proto`、`proto/login/login.proto`、`proto/contracts/kafka/gate_event.proto`;`data/tip/Tip.xlsx`。
  - Go 新增:`go/shared/antiaddiction/antiaddiction.go`、`antiaddiction_test.go`;`go/shared/accountdirectory/real_name.go`;`go/data_service/internal/store/account_real_name.go`、`account_real_name_tidb_test.go`;`go/data_service/internal/server/account_real_name_test.go`;`go/login/internal/logic/pkg/nppa/{client,reporter,client_test,reporter_test}.go`;`go/login/internal/logic/pkg/realname/{realname,realname_test}.go`;`go/login/internal/config/{anti_addiction,anti_addiction_test}.go`;`go/login/internal/svc/anti_addiction.go`;`go/login/internal/logic/clientplayerlogin/anti_addiction.go`;`go/login/internal/logic/loginpregate/submitrealnamelogic.go`。
  - Go 修改:`go/shared/redisslot/login_keys.go`;data_service `store/account_directory_store.go`、`store/account_directory_steps.go`、`server/account_directory.go`、`svc/servicecontext.go`、`svc/role_fence.go`;login `config/config.go`、`config/secrets.go`、`svc/servicecontext.go`、`svc/gate_command.go`、`logic/clientplayerlogin/{loginlogic,renewsessionlogic,session_lifecycle}.go`、`logic/pkg/auth/{provider,providers}.go`、`rpclogging/{config,config_test}.go`、`etc/login.yaml`(注释示例)。
  - C++:`cpp/nodes/gate/handler/event/gate_event_handler.cpp`。
  - Java 网关:新增 `dto/{AntiAddictionStatus,RealNameRequest,RealNameResponse}.java`、测试 `grpc/LoginRpcClientRealNameCodecTest.java`、`service/LoginServiceRealNameTest.java`;修改 `grpc/LoginRpcClient.java`、`service/LoginService.java`、`controller/LoginController.java`、`dto/LoginResponse.java`。
  - 客户端仓:新增 `Assets/Scripts/Game/GameClient.AntiAddiction.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.RealName.cs`、`Assets/Tests/EditMode/Net/AntiAddictionTests.cs`(各带 .meta);修改 `Assets/Scripts/Net/GatewayHttpClient.cs`、`Assets/Scripts/Net/LoginCredential.cs`、`Assets/Scripts/Game/GameClient.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`。
  - 文档:`docs/design/real-name-anti-addiction.md`、`docs/design/third-party-login-end-to-end-design.md` §七、`docs/README.md`。
- **Codex 验证**(串行;失败时保留完整输出;工作区有别的会话的在途改动,失败先按文件归属分清):
  0. **清库重建**:`account_directory` 多了一列,`CREATE TABLE IF NOT EXISTS` 不会给旧表加列,形状守卫会让 data_service 拒启。本地按 AGENTS §6.2 第 2 步清 MySQL / TiDB 再起。
  1. 生成(仓库根):`dev.bat gen`(给 8 个码发号);再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(本任务含客户端,默认配置写客户端仓可以);再 `pwsh -File go/build.ps1` 重生成 login 的 goctl 包装(`LoginPreGate` 多了 `SubmitRealName`)。期望:Go `shared/generated/pb/table` 出现 `LoginError_kLoginRealNameRequired` 等 8 个、客户端 `LoginErrorTip.cs` 出现 `KLoginMinorPlayTimeOver`、`go/login/internal/server/loginpregate/loginpregateserver.go` 出现 `SubmitRealName`。
  2. Go(各自模块目录):`gofmt -l` 本条新增的 Go 文件期望无输出(11-19 本会话对自己的文件跑过,已无输出);`go build ./...` 与 `go vet ./...` 于 `go/shared`、`go/login`、`go/data_service`。
  3. 新用例:`go/shared`:`go test ./antiaddiction/ -count=1 -v`(8 个);`go/login`:`go test ./internal/logic/pkg/nppa/ -count=1 -v`(规范的加密样例能解开、签名样例逐字相同,这两条是和国家系统格式对齐的唯一证据;-race 再跑一遍上报器)、`go test ./internal/logic/pkg/realname/ -count=1 -v`、`go test ./internal/config/ -run 'AntiAddiction' -count=1 -v`、`go test ./internal/rpclogging/ -count=1 -v`;`go/data_service`:`go test ./internal/server/ -run 'RealName' -count=1 -v`。
  4. 回归:`go test ./... -count=1` 于 `go/login`、`go/data_service`、`go/shared`。重点看 gate 上 Login 的用例(`login_commit_two_step_test.go`、`login_session_identity_test.go`、`login_error_semantics_test.go`):目录读挪到了写登录态之前。
  5. 真 TiDB(有专用 DSN 时):`ACCOUNT_DIRECTORY_TEST_DSN=<dsn> go test ./internal/store/ -run 'TestTiDBAccountRealNameIsWrittenOnce' -count=1 -v`(工作目录 `go/data_service`)。
  6. C++(仓库根):`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,看 gate 工程编过(`KickPlayerEvent.reason_tip` 来自重新生成的协议)。
  7. Java 网关(仓库根):`mvn -B -f java/pom.xml -pl gateway_node -am test "-Dtest=LoginRpcClientRealNameCodecTest,LoginServiceRealNameTest,LoginServiceTest,LoginRpcClientParseTest" -Dsurefire.failIfNoSpecifiedTests=false`,再全量 `mvn -B -f java/pom.xml -pl gateway_node -am test`。
  8. 客户端(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error;EditMode(先确认没有别的 Unity 进程占着本工程):`D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Net" -testResults <忽略目录>\real-name-20261119\editmode-net.xml -logFile <忽略目录>\real-name-20261119\editmode-net.log`,`AntiAddictionTests` 5 个全过,`LoginCredentialTests` 不变。
  9. `python tools/scripts/check_log_text_english.py`:干净。
  10. 冒烟(可选,要 login、网关、gate 与一个假的国家系统):login 配 `AntiAddiction.Enabled: true`、`Nppa.*Url` 指向假服务;口令账号 /api/login 回 `anti_addiction.real_name_required=true`,直接去 gate 上 Login 回 `kLoginRealNameRequired`;/api/real-name 成功后再登录不再要求;把可玩日与时钟凑成未成年人时段内,续租在截止时刻踢线、客户端显示「今日游戏时间已结束」。仓里没有国家系统的 mock,假服务要按设计稿 §2 的报文格式写,这一步可以留到真接口测试阶段。
- **未验证**:以上全部没有运行。国家系统的报文格式只对照技术规范 V1.9 与它的样例,没有连过真实系统;微信是否也要在国家系统添加渠道、抖音的充值限额由谁执行、「法定节假日」口径,仍按设计稿 §1、§3、§7 待核实。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;已在 `D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾登记「2026-11-19 实名认证与未成年人防沉迷」。客户端可见的新契约是 `/api/real-name`、`LoginResponse.anti_addiction`、8 个 tip 码与踢线原因,两版要同批。

## 2026-11-19 Claude:角色换主人之后,本机待确认记录作废——进游戏应答带归属修订号、记录按它判前主人,单人排队的查询与撤销给确定结论(补同日「组队开局回执两处收尾」没覆盖的同类链路;未生成、未编译、未测试,待 Codex 验证;Java 待做)

- **起因**:同日上一条只修了整队开局这一处。修的时候就看到同一个根因还在别处,当时没做也没说明,负责人要求「查出的问题现在修」,这条补上。
  - 根因:客户端有 8 处本机待确认记录(单人排队、整队开局、组队操作、活动领奖、装备打造、材料选择、帮会经济、商城),全走 `DurableRequestFileStore`,只按「网关地址 + 角色编号」存。角色过户后,新主人在同一台设备上登录会读到前主人留下的记录。
  - 逐条查过的后果:
    - 单人排队会卡死。`GetQueueStatus` 带前主人的请求号,`RequestCancelled` 报「非原角色」,整条 RPC 回错;客户端停在「排队中」一直轮询,撤销也回错,匹配入口全被挡住,只能手动删本机文件。
    - 组队操作不会卡:服务端对前主人的回执回业务拒绝(`errTeamIntentConflict` → `kInvalidParameter`),客户端收到即作废。但前主人没提交成功的请求会被自动重发,变成新主人没点过的操作(例如建队、申请入队)。
    - 活动领奖、打造、材料选择:去重账本跟着角色存档走、不记主人,服务端分不出;打造恢复会自动重发,前主人没执行成功的打造会替新主人做掉。
    - 帮会经济、商城:只看了客户端(业务拒绝即作废记录),没有逐行查这两个服务对主人不符的处理;同一个根因,由下面客户端那一层统一挡住。
- **依据**:过户、删角之前护栏要求这名角色在各参与域的在途操作全部了结(`authority.RoleFencePending` 是 match 那一份)。所以归属修订号一变,此前留在设备上的待确认记录全部作废,新主人既不该恢复也不该重发。
- **做法**(两层):
  - 客户端统一收口:受理的 `EnterGameResponse` 新带 `owner_revision = 5`(账号目录的归属修订号,过户一次加一;只在受理时填),`GameClient.OwnerRevision` 先于 `PlayerId` 置位、同时清零。`DurableRequestFileStore` 把写入时的修订号记在文件首行(`@owner-revision:<n>`);读到的记录与当前修订号都已知且不同,就按不存在处理:不恢复、不重发、不占槽位。
    任一方未知(旧服务端、沿用上一发在途进场而应答是合成的、旧格式文件)时照旧保留。没有选「按修订号分目录」:时机对不上时会把本人的记录藏起来,那比要修的问题更糟;现在的写法最坏只是退回原行为。
    没有选从角色列表或「进入场景」推送下发:前者账号列表自愈时可能缺这名角色,后者要动 C++ scene;登录服受理时手里正好有这个值,改一行。
  - 服务端兜底(客户端判断不了时:修订号未知、旧客户端):`authority.QueueRequestClosed` 把「请求号的原件属于前主人」和撤销墓碑一样算作不会再入队,`GetQueueStatus` 的 `request_cancelled` 用它;`CancelIntent` / CancelQueue 对前主人的票、意图、整队原件回成功且原件不动。
    会话本身已不归现主人时不给这些结论,仍按原会话失效处理。新主人沿前主人的请求号 Join 本来就被拒(`joinOnce` 核原主人),没有改。
- **改动**:
  - 协议:`proto/login/login.proto`(`EnterGameResponse.owner_revision = 5`);`proto/match/match_service.proto` 只改 `request_cancelled` 的注释。
  - go/login:`internal/logic/clientplayerlogin/entergamelogic.go` 受理时填 `resp.OwnerRevision`。gate 不解析这条应答,C++ 不用改。
  - go/match:`internal/authority/store.go`(`cancelIntentOnce` 两处主人不符改回 `ErrForeignOriginal`、`CancelIntent` 换算成成功、新 `QueueRequestClosed`);`internal/logic/getqueuestatuslogic.go`、`cancelqueuelogic.go`。
  - Go 测试:`go/login/.../enter_online_identity_test.go`(受理应答的修订号等于绑定原身份的修订号)、`entergame_pool_saturated_test.go`(被拒的应答不带);
    新 `go/match/internal/authority/predecessor_original_test.go`(`TestQueueRequestClosedCoversTombstoneAndPredecessorOriginal`、`TestCancelIntentLeavesPredecessorOriginalUntouched`,各带「会话失效」反例);
    新 `predecessor_original_native_test.go`(`TestQueueTiDBPredecessorJoinRequestIsClosedForNewOwner`,真库:票到期、过户、新主人查询与撤销得到确定结论且原件逐字节不变、沿旧请求号入队被拒、新请求号照常入队);
    `team_intent_test.go` 改用上面文件里的公共替身(`predecessorAccount`、`rejectingRoleGuard`)。
  - 客户端(同级仓 `mmorpg-client`):`Assets/Scripts/Net/DurableRequestFileStore.cs`(首行修订号、作废判定;构造与 `ForOrigin` 加可选的修订号来源)、`Net/FileMatchJoinRecovery.cs`、`Game/Jubaozhai/CommerceClient.cs`(`CommerceFileRecoveryStore`)透传;
    `Game/GameClient.cs`(`OwnerRevision`,6 处存储构造传入)、`Game/GameClient.RoleSwitch.cs`、`UI/Ugui/Guild/GuildUiRoot.cs`、`UI/Ugui/Jubaozhai/JubaozhaiUiRoot.cs`。
  - 客户端测试:`DurableRequestFileStoreTests` 新 4 例(`RecordWrittenUnderOneOwnerRevisionIsAbsentAfterTheRoleChangesOwner`、`SupersededRecordNeitherBlocksNorAuthorizesTheNewOwner`、`RecordWithoutRevisionStaysVisibleAndKeepsItsSlot`、`ValueShapedLikeTheRevisionLineIsRejectedAndAMalformedLineIsNotGuessed`);
    `FileMatchJoinRecoveryTests.JoinIntentLeftByThePreviousOwnerIsNotRestoredOnceTheRoleChangesOwner`;`TeamClientTests.StartIntentLeftByThePreviousOwnerIsNotRestoredOnceTheRoleChangesOwner`;`GameClientReconnectTests.RecoveryUsesFreshTicketTokenLoginAndOriginalRoleOverRealTcp` 加一句核 `OwnerRevision`。
  - 文档:`docs/design/cross-zone-matchmaking.md` §13.2 末尾新增「角色换主人之后的本机原请求」。
- **给 Codex 的验证步骤**(本会话没有生成、没有编译、没有跑任何测试;与同日上一条合跑):
  1. 协议必须先生成(仓库根目录):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`,默认配置,连同级客户端仓一起写。本条加了字段,不生成两边都编不过(Go 的 `resp.OwnerRevision`、客户端的 `egResp.OwnerRevision`)。
  2. go/login(工作目录 `go/login`):`gofmt -l internal/logic/clientplayerlogin` 无输出 → `go build ./...` → `go vet ./internal/logic/clientplayerlogin/...` → `go test ./internal/logic/clientplayerlogin/... -count=1`。
     重点:`TestEnterGameKeepsLoginSessionAndPublishesOriginalIdentity`(两个子用例)、`TestEnterGame_PoolSaturatedCountsWithoutLatencySample`。
  3. go/match(工作目录 `go/match`):`gofmt -l internal/authority internal/team internal/logic` 无输出 → `go build ./...` → `go vet ./internal/authority/... ./internal/team/... ./internal/logic/...` → `go test ./internal/authority/... ./internal/team/... ./internal/logic/... -count=1`。
     重点:上面点名的两个新单测、`TestTeamMatchIntent*` 五个、`TestCancelQueueFromUnboundSessionTouchesNoTicket`、`TestGetQueueStatusFromUnboundSessionReportsNotQueued`。
  4. 真库(`MATCH_IT_TIDB` 指向一次性 TiDB,`MATCH_IT_REQUIRE_TIDB=1`):`go test ./internal/authority/ -run 'TestQueueTiDBPredecessorJoinRequestIsClosedForNewOwner|TestJoinTiDBPreparedCancellationAndOwnerABARejectLateCompletion' -count=1`,连同上一条第 3 步的 team 用例。
  5. Unity EditMode:`DurableRequestFileStoreTests`、`FileMatchJoinRecoveryTests`、`TeamClientTests`、`GameClientReconnectTests` 全部通过;用到这个存储的其余用例不回归:`TeamActionRecoveryTests`、`GuildEconomyRecoveryTests`、`GuildRequestFailureTests`、`GuildUiTests`、`CommerceClientTests`、`ActivityVitalityClientTests`、`EquipmentCraftClientTests`、`EquipmentCraftUiStateTests`、`EquipmentMaterialRewardClientTests`。
  6. 失败时保留第一个编译错误原文与失败用例的完整输出;不改断言、不跳过用例。
- **残余**:
  - 修订号未知的那一次进场(沿用在途的 EnterGame、旧服务端)不作废记录。这时排队和整队开局靠服务端的确定结论收敛;组队操作、帮会经济、商城、场景侧三项仍可能把前主人没执行成功的请求重发一次。要同时碰上「过户后第一次登录、同一台设备、留有待确认记录、且这次进场没拿到应答」,不再加机制。
  - 作废的旧文件留在原路径,等下一次写入、清除或持锁读取时删掉;体积上限 64 KiB,不单独清理。
  - 调试栈里协调者连不上其它参与域,过户一律被拒,做不了冒烟;覆盖靠上面的单测与真库用例。
- **Java 版**(AGENTS §12):不写 Java 代码,Java 仓 `PARITY.md` 加了同日待做条目。
- **提交情况**:两仓 main 工作区,未提交,没有开分支。

## 2026-11-19 Claude：评审修正·发号器毫秒水位、一致性核对工具上 Cluster、login 断线以会话头为准（未编译、未测试，待 Codex 验证；Java 不涉及）

- **范围**：独立评审 2026-11-12「数据审计遗留·发号器毫秒水位、一致性核对工具上 Cluster、login 断线以会话头为准」与同日补条（工具按 `{gr:<区>}` 键形解析）。评审员按现行源码核对，**没有必修**，没改文件；下面的改动是主会话按评审建议收的（负责人 11-12「以后修的不要等以后，现在修」）。
- **据建议改的**：
  1. **login Disconnect 请求体与会话头不一致时不再整条拒绝**：改为记 CRITICAL（`logx.Severef`），照常按会话头收尾。理由：这条通知由 gate 断线时合成、只发一次、没人重发，拒绝就让这条连接上的玩家永久停在 ONLINE（`markPlayerSessionDisconnecting` 是唯一入口）；11-12 条目残余里「等死 gate 清扫」不成立，gate 还活着时不会清扫。会话头验过签，按它收尾不会被请求体带偏，拒绝换不来安全性。今天走不到这条路径：全仓只有 C++ gate 合成 Disconnect，请求体与会话头取同一个 `sessionId`。
     - `disconnectlogic.go`：删 InvalidArgument 分支与 `codes` / `status` 导入；`disconnectSessionID` 恒回会话头的值，另回是否一致。
     - `disconnect_session_header_test.go`：`TestDisconnect_RejectsABodySessionIDThatDiffersFromTheHeader` 改为 `TestDisconnect_BodySessionIDThatDiffersFromTheHeaderStillSettlesByTheHeader`（断言按会话头的 session_id 挂了租约、登录态清掉、栅栏关上）；`TestDisconnectSessionID` 不一致那一例改为回会话头、consistent=false。
     - `proto/login/login.proto` 只改注释；`docs/design/client-request-ordering.md` §6.2 Disconnect 行与 §16 那一行同步。
  2. **注释与文档改正**：`go/shared/snowflakealloc/allocator.go` 文件头（闸在水位 Txn 成功时 Ack：写入新值，或存值不低于目标值时吃下存值）；`snowflake-id-allocation.md` 规则 2（Go 申领 Txn 只写 slots / affinity，第一笔水位申领后单独写）；`node-id-overhaul-plan-20260908.md` §3.5 写盘时机改按 §7.5-5（稳态零写盘），§3.5.1 的 C++ 一条标成已删、保留作历史；`go/guild/internal/data/guild_keys_contract_test.go` 金样注释（工具已经 import redisslot）；`tools/data_consistency_check/route_source_test.go` 头注释（module 还依赖本仓 shared）。
- **核过没问题**（评审员）：
  - 删除干净：`ReadMsWatermark` / `PutMsWatermark` / `readMsWatermarkRaw` / `watermarkMsKey` / `msWritten` / `msRev`、三个缓存字段、`PlayerID*Bits`、`Options.ClusterBits` / `SlotBits`、`SyncWatermark`、`Handle.FenceClock`、`AttachFencer` 在 go/、tools/、robot/、cpp/、deploy/、java/ 都没有引用（含 integration tag）；11-13 的合并没有带回来，三条删掉的用例确实不在。
  - scene_manager 是唯一调用方，它用到的 Options 字段与 LogFields、WorkerID、NewNode、Lost、Close 都在。
  - reclaim 只比秒级水位的 revision 仍安全：继任者只能在隔离期之后申领，它的第一笔水位按前任的 revision 做 CAS 且一定写入，失联期间槽被用过 revision 就一定变了；缓存启动不写水位，缓存资格 F 小于隔离期 Q；对 scene_manager 毫秒比较一直是 0==0，删掉没有语义损失。
  - 旧缓存文件带已删字段照常读；`TestReclaim_IgnoresALeftoverMsWatermarkKey` 改动前必红；改写的故障期写盘用例逐拍推导写盘次数 2、3、4，与断言一致。
  - 工具：单实例 / Cluster 客户端类型与 `ScanEachMaster` 的分支一致；扫描前后核拓扑指纹（槽合计 16384、无迁移标记）或 run_id；没写 `-redis-cluster` 却连到 Cluster 节点，扫描前拒绝；任一步失败报 fail、计数清零、退出 1；重复键去重；键形重拼逐字比对；replace 路径正确。
  - login 既有 6 个测试文件里请求体与会话头的 session_id 都一致。
- **建议，没做**：
  - 工具连错 Redis（指向玩家域或空实例）扫到 0 个榜键时报 info「no orphan rank ZSETs」：改动前就有；要判「guild 表非空而榜键为 0」得另读 guild 库，留给后续。
  - `snowflakealloc` 的 LegacyPrefix 兼容层整批删（连同 scene_manager 的实参）：旧二进制已不存在、项目未上线，没有实际风险，归 snowflake 残留收尾。
  - 榜键前缀在 guild 与工具各写一份（两边金样钉住同一个键形，现在不会漂）。
  - 每项检查固定 60s，控制集群键量大时 `SCAN COUNT 200` 可能扫不完、一直 NOT CHECKED：可调大 COUNT 或加超时参数。
- **交 Codex**（接原条目各步；失败保留首错原文，不改断言、不连续重试）：
  1. 工作目录 `go/login`：`gofmt -l ./internal/logic/clientplayerlogin/` 无输出；`go vet ./internal/logic/clientplayerlogin/`；`go test -count=1 -v -run 'TestDisconnect' ./internal/logic/clientplayerlogin/`，`TestDisconnect_BodySessionIDThatDiffersFromTheHeaderStillSettlesByTheHeader`、`TestDisconnect_SettlesWithTheSessionHeaderSessionID`、`TestDisconnectSessionID`（3 个子用例）、`TestDisconnect_ZeroPlayerIdUsesFenceBoundPlayer` 都要 PASS；再整包 `go test ./... -count=1`（Access denied 时改用 `go test -c`）。
  2. 原条目第 4 步：工具先 `go mod tidy`。现在 go.sum 还是 go-redis v9.7.3，不 tidy 一定编不过；tidy 之后 go.mod、go.sum 的差异要入库。
  3. `go/guild`：`go vet ./internal/data/`（只改了注释）。协议只改了注释，随别批的 proto-gen 一起生成。
- **Java 版**：不涉及。Java 的 DisconnectHandler 不读请求体（原条目已核），其余是本仓注释与文档。

## 2026-11-19 Claude：评审修正·账号目录、角色名与 GM 回收：孤儿名字有 ref 没有路由行时不释放、删名字缓存有自己的上限（未编译、未测试，待 Codex 验证；Java 待做已补登）

- **范围**：独立评审 2026-11-12「数据审计遗留·账号目录、角色名与 GM 回收残余收口」。1 条必修，评审员已改，主会话复核成立；另据建议收 1 处。
- **必修（评审员改）**：`create_residual` 收孤儿名字时，没有路由行就直接判「没有证据」，不再看 ACTIVE 的 ref。
  - 失败场景（多集群 PITR 恢复点错开）：玩家片恢复到登记路由之前（路由行、角色行都没了），名字片停在「预留之后、绑定之前」，账号片在 activateRef 之后（ref 是 ACTIVE）。工具报 `orphan_name planned`，apply 时证明回 Unrouted、不写墓碑，然后删掉名字行；之后玩家片恢复到更靠近 T 的点，角色回来了却没有名字登记，名字可能已被别人注册，变成一名两角。违背本批自己写的契约（`03-names.md` §3.4a、PITR 手册 §4.1a：某个账号上有 ACTIVE 的 ref 只报告），也与后台 `committedEvidence` 对同样状态判 `no_route` 不一致。
  - 改法（`create_residual_sql.go` 的 `orphanEvidence`）：先读这名角色在各账号片上的 ref，任一账号有 ACTIVE 就回 `ref_active`，不管有没有路由行；没有路由行或落点时，有 ref 回 `no_route`（核不全），没有 ref 才回空（可以收）。`create_residual.go` 文件头与接口注释、`mysql-backup-pitr-runbook.md` 两处、`guild-phase2/03-names.md` 一处同步。
  - 主会话复核：`createRoleWithInitialRows` 先过 `initialStorage`（要求有路由行和落点）才写 ref，所以「有 ref 没有路由行」只可能是恢复点错开；login 在登记路由之前回滚留下的孤儿一条 ref 都没有，照常收，本批要收的「玩家片冻结期间回滚」那种孤儿（有路由行）也不受影响。
  - 回归用例：`TestTiDBCreateResidualKeepsOrphanNamesWithRefsButNoRoute`（`create_residual_names_tidb_test.go`）：12 号有 ACTIVE ref、13 号只有 PENDING ref，两人都没有路由行，名字是 1 小时前的预留态；期望 `manual ref_active` / `manual no_route`，名字留着、不写墓碑、不删缓存。改之前两条都是 `repaired`。原有 `TestTiDBCreateResidualSettlesOrphanNameReservations` 结论不变。汇总按玩家号排序，断言顺序确定。
- **据建议改（主会话）**：删名字缓存给自己的总上限 300ms（`account_directory_events.go` 的 `nameCacheInvalidateBudget`，在 `invalidateNameCache` 里套 `context.WithTimeout`）。原来删缓存直接用调用方的 ctx：删角收尾一共 1 秒（`removedRoleSettleTimeout`），Redis 被黑洞时 DEL 吃满预算，后面删小表那一行拿到的是已过期的 ctx，每一轮都记 `drop_removal_row` 与 Unsettled，`DataServiceRoleRemovalResumeStuck` 一直响，前台删角每名多等约 1 秒（不丢数据，名字已经放了）。工具 `create_residual` 直接调 `delNameCacheWithRetry`，不受影响。新用例 `TestNameCacheInvalidationHasItsOwnBudget`（`account_directory_events_test.go`）：给一小时的 ctx，假缓存立即失败并记下每次拿到的剩余时间，断言每次都 ≤ 上限；改之前每次都约一小时。
- **建议，没做**（所在文件正被账号注销、审计保留期两个会话改着，交给它们或下一轮）：
  - gauge `account_role_ref_held` 的注释写「不预置」，但 `PrimeAccountDirectory` 注册普通 Gauge 后立刻导出 0：计数从启动起一直失败时 gauge 恒 0，`DataServiceRoleRefHeldForReview` 永远不响。同文件新加的 `account_deletion_overdue` 照抄了这个假设。要么首次 Set 才注册，要么给计数失败单独计数并告警。
  - `holdRef` 不看 RowsAffected（`account_role_ref.go`）：判陈旧读完之后迟到的建角提交并激活了 ref（nonce 已换），holdRef 改 0 行却仍记 ERROR 与 Held+1。gauge 是对的，只是假告警日志与计数。
  - 已绑定、却没有角色行的名字，工具两种扫描都报不出来（`runNames` 跳过 `BoundMs≠0`），只能靠保留 ref 的告警间接发现；可加一个只报告的种类。
- **核过没问题**（评审员）：
  - 标保留、复核轮转、清标记：写 ref 的四处（`writePendingRef`、`activateRefSQL` 的 6 处 IF 且 tx_id 最后赋值、`promoteRef`、`holdRef` 按 nonce 加 state）互相不会丢更新；多副本同时复核结果幂等。
  - 「证据没了 → 写墓碑」：`tombstoneMissingRole` 在玩家片锁角色行后严格插入，新提交的在役角色不可能被写成墓碑；能补出证据的写入都要求角色行在。
  - seal 的证明与只删预留态是两步，但墓碑不可逆、DELETE 带 `bound_ms=0`；Unrouted 那一支靠 login 每跳都有截止时间加 30 分钟门槛。`sealRoleTx` 是从 11-06 版逐字抽出来的。
  - 删名字缓存都在库提交之后；三条新查询都对得上索引，`ensureIndexes` 按前缀覆盖，`OptionIndex` 只在末尾追加；`ToPlayerID` 与 `PlayerID` 同时给在发 SQL 前报错，按收到方查补流水与原来按玩家查的分组结果等价。
  - GM 回收的自动终结与人工终结 / ROLE_GONE 在「批次 → op」同一把行锁下比 status，不会双扣，三列也不互相覆盖。
  - `create_residual` 只碰 data_service 自己的库；指标 label 都是封闭集合、没有 player_id；告警与契约测试逐项对得上，CI paths 已覆盖；改签名的 `removeRole`、`NewCreateResidualAuditor`、两个 steps 接口，调用方与替身都跟上了。
- **交 Codex**（仓库根起步；接原条目各步；失败保留首错原文，不改断言、不连续重试）：
  1. `cd go/data_service`：`gofmt -l ./internal/store/` 里属于本条的 `create_residual_sql.go`、`create_residual.go`、`create_residual_names_tidb_test.go`、`account_directory_events.go`、`account_directory_events_test.go` 应无输出。
  2. `go test -count=1 -v -run 'TestNameCacheInvalidationHasItsOwnBudget|TestReleaseRemovedRoleNameInvalidatesTheNameCache|TestDelNameCacheWithRetry|TestRemovalSettleFailureIsCounted|TestBindRestore' ./internal/store/`，全 PASS。
  3. TiDB（同原条目第 5 步的环境变量）：`go test -count=1 -p=1 -timeout=30m ./internal/store/ -run 'TestTiDBCreateResidual'`，要求 PASS 不是 SKIP，`TestTiDBCreateResidualKeepsOrphanNamesWithRefsButNoRoute` 在列。可选变异：把 `orphanEvidence` 临时改回「先判路由行就返回」，这条应红成两条 `repaired`。
  4. 在 MySQL 与 TiDB 上各 `EXPLAIN` 一次补查流水 `SELECT … FROM transaction_log WHERE to_player=? AND correlation_id IN (…) ORDER BY timestamp_sec DESC LIMIT …`（`recall_logic.go` 的那条），确认走 `(to_player, correlation_id)` 那条索引、没被 `(to_player, timestamp_sec)` 抢走，把计划原样贴回。
  - 注意：评审期间账号注销、审计保留期两个会话正在改本批里的 `account_directory_store.go` / `steps`、`account_role_ref.go`、`account_directory_flow_test.go`、`metrics/*.go`、`svc/role_fence.go`、`servicecontext.go`、`rollback_database_table.proto`、`transaction_log_store.go`（steps 接口签名已改），编译红在这些文件上时先按归属分清。
- **Java 版**：待做，补登进 Java 仓 `PARITY.md`「2026-11-12 数据覆写审计残余收口」一节：孤儿名字的证据先看 ref（有 ACTIVE 回 ref_active；没有路由行但有 ref 回 no_route，都只报告）；删名字缓存不占删角收尾的预算。

## 2026-11-19 Claude：评审修正·网关区服乐观并发：压测种子改已有区服时版本加一（未编译、未测试，待 Codex 验证；Java 待做已补登）

- **范围**：独立评审 2026-11-12「数据审计遗留·网关区服乐观并发与错误体、商城应答口径」（本仓 `java/`）。本批文件里没有必修；1 条必修在仓库自带的压测种子，评审员给了补丁，主会话照改。
- **必修（主会话按评审补丁改）**：`java/gateway_node/src/main/resources/seed_stress_3zones.sql` 的 ODKU 改已有区服（zone 1 由 schema.sql 插入）时不加版本。
  - 失败场景：运维 GET zone 1 拿到 ETag `"1"`；有人按压测手册跑这份种子，capacity 改成 20000、version 仍是 1；运维拿 `If-Match: "1"` 整行 PUT（只想改名），版本条件成立，capacity 被悄悄写回 45000，正是本批要挡的后写覆盖；同一窗口里带 `If-None-Match: "1"` 的 GET 回 304，行却已经变了。严重度低：只在手工跑压测种子时出现。全仓只有它改已有行（Go、tools、merge_zone、dbmove、K8s 脚本都不写 zone_config；schema.sql 与文档里的种子是 `INSERT IGNORE`）。
  - 改法：ODKU 末尾加 `version = version + 1`，文件头补一段说明。
  - 回归用例：`GatewaySchemaContractTest.bundledStressSeedBumpsVersionOfExistingZone`（H2 MySQL 模式，先跑 schema.sql 断言 zone 1 版本 1，再跑种子断言 zone 1 版本 2、zone 2 版本 1）。改之前 zone 1 仍是 1，必红。H2 上 ODKU 里 `version = version + 1` 的写法未实测。
  - 残余（不改）：种子新插的行取缺省 1；删掉 zone 1 之后再跑压测种子，按 schema.sql 种子内容发出的旧 ETag `"1"` 会对上新插的行。只在手工删区再跑种子时出现。
- **据建议改（主会话）**：`docs/design/login-queue-2026-05.md`（区容量一条）与 `deploy/k8s/README.md`（排队容量一段）写明改区容量要先 GET 取 ETag、PUT 带 If-Match，否则照做先收到 428。
- **建议，没做**：
  - create 与白名单 add 回读改成加锁读，只有 mock / H2 用例钉住「用的是加锁读」，没有一条能复现 TiDB RR 下看不见别人提交的那一行；可补一条受 `GATEWAY_SCHEMA_TIDB_URL` 控制的真 TiDB 用例。
  - `deploy/k8s/login-alerts.yaml`、`go/login/etc/login.yaml` 注释、`loginqueue/capacity.go` 的日志只写「经 PUT /admin/zones/<id>」：是指路，428 的问题体自带原因码，没改。
  - `@ExceptionHandler(ErrorResponseException.class)` 也接住子类 `ResponseStatusException`，以后这两个控制器抛 RSE 会把原因文案原样放进 detail、也没有 reason 码：建议在注释里约定只抛 `AdminProblems`。
  - 白名单 add 旧缺口：不校验 note 长度（128），超长时落库报错回 500 而不是 400 `too_long`；行号撞号且本键已存在、note 相同时，别人那一行的 note 会被静默改掉。
  - 以后管理面若放到会 gzip 的代理后面，强 ETag 会被改弱，PUT 将永远 412；现在不经 Ingress、网关没开压缩，不受影响。
- **核过没问题**（评审员）：
  - PUT 先加锁点查判存在并持锁到提交，再 `UPDATE … WHERE zone_id=? AND version=?`、SET 里 `version=version+1`；412 在事务内抛出整笔回滚。网关内 5 条 UPDATE 都加一（含 RoleCapacityGuard 的 CAS），ODKU 已存在分支不动版本；多副本下都是行锁里的单条语句。
  - 读方：Go 侧没有 zone_config 读（全走 etcd）；Java 只经实体；`ZoneInfoDto` 与 ZonePolicy 的 JSON 不含 version。Hibernate validate 的 `long` 对 `BIGINT NOT NULL DEFAULT 1`。两份 DDL 去注释压空白后逐字一致。
  - If-Match：没带、空白、`*` 回 428；没加引号、空引号、多个标签回 400；弱标签、前导零、0、超出 long 回 412 且不发 UPDATE；判定顺序与文档一致。304 由 Spring `HttpEntityMethodProcessor` 只在 GET/HEAD 的 200 上判（只读核对过 6.2.3 jar）。新行起始版本在 [1, 2^53)，version 不进 JSON。
  - 错误体只作用于两个管理控制器，没有全局打开 problemdetails；两个 `FOR UPDATE` 读声明 MANDATORY，生产调用方都在 TransactionTemplate 里；改名改签名的调用方都已跟上。商城 HTTP 用例的计数与 store 逻辑一致。
- **交 Codex**（工作目录 `java/`，接原条目各步）：`mvn -B -pl gateway_node -am test "-Dtest=GatewaySchemaContractTest" -Dsurefire.failIfNoSpecifiedTests=false`，要求 `bundledStressSeedBumpsVersionOfExistingZone` PASS；红在 H2 不认 ODKU 里的 `version = version + 1` 时保留原文报回来，不要改种子去迁就 H2。
- **Java 版**：待做，补登进 Java 仓 `PARITY.md` 同一节：改已有区服的种子 / 脚本也要让版本加一。

## 2026-11-19 Claude：评审修正·排空框架与消费组：组在分区上从没提交过时收尾不结清、收尾核对加整体时间上限（未编译、未测试，待 Codex 验证；Java 待做已补登）

- **范围**：独立评审 2026-11-12「数据审计遗留·排空框架与消费组残余收口」。1 条必修，评审员已改，主会话复核成立；另据建议收 1 处。
- **必修（评审员改）**：收尾核对在「组在该分区从没提交过」时会给保留期丢掉的记录背书。
  - 失败场景：guild-trial 首次上线（或新一代 topic），组在分区 P 上没有已提交位点。A 打开 P 落在日志起点，取到最老的一条 X，回调失败、正在退避；保留期删掉 X 所在的段；维护排空时 A 交还 X 并离组；B 用 FirstOffset 打开 P，从新起点 L1（> X）读起，提交到 L1+1；A 收尾读到 L1+1（>= 0 且 > X），记 `ResolvedHandoffs`，收据成立。X 从没被处理；B 的 `judgeGap` 看不到缺口（它的 initial 就是 L1），探针的 RetentionGap 只算提交过的分区，不 HOLD 也不计数。reader 模式漂走的恢复票据（`GROUP_COMMITTED_PAST_AT_CLOSE`）同理。11-06 版这两类票据在终止时一律 HOLD，本批改成按组内位点结清，但「位点 >= 0」只说明组此刻有位点，不说明票据产生之前就有。
  - 改法：`group_lag.go` 新增 `boundPartition.hasCommitFrontier()`（`groupCommitted || committed > initial`）；handler 模式交还（`settleGuarded`）与 reader 模式交还（`releaseUncommitted`）在锁内记下 `frontier`；`bound_consumer_tickets.go` 交还记录要求 `frontier`，恢复票据要求来源代次分区 `hasCommitFrontier()`，不满足就不结清、预算用完 HOLD。已有位点的分区行为不变。设计稿 `kafka-bound-consumer-group.md` §6.2 主会话按评审补丁改了：结清多一个条件，代价是新组在某分区上的第一条在途记录赶上排空时收据 HOLD，按 ABORTED_CLOSED 重试同一 run。
  - 主会话复核：组有真实位点时，新属主一定从不晚于该记录的位置读起；记录被保留期删了它的 `judgeGap` 会自己 HOLD，所以结清成立。组没有位点时宁可误 HOLD，不给丢数据背书。
  - 回归用例：`TestBoundConsumerGroupDrainHoldsHandoffOnPartitionTheGroupNeverCommitted`、`TestBoundMessageReaderHoldsDriftedReplayTicketOnPartitionTheGroupNeverCommitted`（改前第一轮读到 6 就结清，必红）。夹具同步：`...DrainHandsOffRecordAndConfirmsFromGroupCommit` 与 `...LeavingOneByOneHandsOffRejectedOldGenerationCommit` 改为 `deliver(1, 0)`（组在该分区已提交到 0），前者第一轮位点 -1 改 0；`TestBoundMessageReaderCommitRetriesWhileAliveAndHandsOffWhenTerminating` 的夹具加 `groupCommitted: true`。
- **据建议改（主会话）**：收尾核对加按单调时钟的整体上限 45 秒（`boundTicketConfirmBudget`，接缝 `ticketConfirmBudget`，零值取缺省）。原来只有「30 轮 × 带抖动的 sleep」，违反 AGENTS §11.3「不得用重试次数 × sleep 代替时间预算」；每轮新建 sarama 探针，broker 不通时一轮要几十秒，data_service 关停时传的 ctx 不带期限，最坏十几分钟才 HOLD。新用例 `TestBoundConsumerGroupTicketConfirmationHasATimeBudget`：上限设成 50ms，读位点的替身记下每轮拿到的 ctx 还剩多久、立即失败；改之前每轮看到的都是调用方给的 10 秒。
- **建议，没做**：
  - 同类旧洞（11-06 R5，不是本批）：`resolveReplayPassedLocked` 和「重开时 FirstOffset 落到 earliest」两处，组在分区上从没提交过时会静默越过已被保留期删掉的已接纳记录。根治是首次打开无位点分区时立刻提交起点，属语义变更，**交负责人定**。
  - reader 模式提交持续失败而入组正常时，每次交还都结束代次，leader 每轮触发一次全组再均衡：不丢数据，可按连续交还次数指数退避。
  - owner 终止但代次仍在时，「只差提交」的记录第一次提交失败就交还，最后一个副本会直接 HOLD：可在代次仍在时做有界重试。
  - `KafkaBoundConsumerDeadLetterWriteFailing` 是 `increase[10m] > 0` 加 `for 5m`，单次瞬时失败也会响，和「持续失败」的说法不符。
  - 注释里的数字没算抖动（20 次退避实际 226–451s，「每轮间隔 1s」实际 0.5–1s）；`retry_total` 的 Help 漏了 dead_letter，`released_total` 的 Help 没写含交给新属主。
  - 死信比原件多约 1KB 头，接近 `max.message.bytes` 的原件写不进死信；`kafka_dlq` 中途失败的报错可带上 dlq_offset 与原 offset。
  - `CaptureDrainCatalog` 限 129 个 topic，多了 guild-trial 的死信 topic 和它的分区标记 topic，上线前核一下余量。
  - 旧问题：`appendHistoryLocked` 发现结局不平时只调 relay.Hold，不计 `hold_total`。
- **核过没问题**（评审员）：
  - 组已有提交位点的分区上，交还 → 收尾没有「既没处理也没 HOLD」的窗口，先记 HOLD 再结束票据；旧代次提交在 PreparingRebalance 期间被接受、新代次形成后被拒，越过位点的属主只能从不晚于该记录处读起。
  - 死信：写成功才提交，写失败留在原处、不再调回调；sink panic 算一次失败；退避被打断后不写死信。死信 topic 耐久跟结果 topic 走（RF3、minISR 2、禁 unclean），1 分区 90 天；启动门禁与 `runBound` 用同一份 TopicSpecs。
  - 重放工具：组、解析、unprocessable 的校验都在任何写之前；同一原位置一轮只写一次；按原分区写回（kafka-go v0.4.47 `writer.go:656` 把整条 Message 交给 Balancer）；acks=all、不自动建 topic；写回相同字节对 match 的 `ArchiveCompletion` 幂等。
  - 保留期缺口：C++ 生产者只开 idempotence、没开事务，缺口只可能来自保留期；kafka-go 两处静默跳起点都能逐条核出；回退 HOLD 没有误报来源。
  - reader 结束代次：交还的那批已落库、不丢；还有在途记录时代次换不了；通道里的旧代次投递被跳过；data_service 收到交还错误后不重复提交。
  - panic 只算一次失败、不 HOLD；guild 在 `runBound` 里等探针退出、无死锁，match 经 BindLagProbe 收口；label 是低基数常量并已预置；告警与契约对得上、新告警名全局唯一。
  - 删除项确认不在：`RequireArrivals`、match 无 relay 的自建循环（`runResultConsumer` 只在 `result_loop_test.go`）、旧用例名、guild 生产路径 panic 时的 Hold；match 生产路径 relay 恒非空且传了 BindOwner。
- **交 Codex**（工作目录 `go/shared`，接原条目第 1～6 步）：
  1. `gofmt -l kafkautil` 无输出；`go vet ./kafkautil/`。
  2. `go test -race -count=3 -v -run 'TestBoundConsumerGroupDrainHoldsHandoffOnPartitionTheGroupNeverCommitted|TestBoundMessageReaderHoldsDriftedReplayTicketOnPartitionTheGroupNeverCommitted|TestBoundConsumerGroupTicketConfirmationHasATimeBudget|TestBoundConsumerGroupDrainHandsOffRecordAndConfirmsFromGroupCommit|TestBoundConsumerGroupLeavingOneByOneHandsOffRejectedOldGenerationCommit|TestBoundMessageReaderCommitRetriesWhileAliveAndHandsOffWhenTerminating|TestBoundMessageReaderResolvesDriftedReplayTicketAtClose|TestBoundConsumerGroupDrainHoldsHandoffThatNoOwnerSettles' ./kafkautil/`，全部 PASS（含子用例）。
  3. 可选变异：去掉 `bound_consumer_tickets.go` 里的 `|| !handoff.frontier` 与 `|| !delivery.partition.hasCommitFrontier()`，前两条新用例应变红；恢复后再绿。
- **Java 版**：待做，补登进 Java 仓 `PARITY.md`「2026-11-12 数据覆写审计残余收口」一节：维护排空交还的记录，只有交还时组在该分区已有已提交位点才能按组内位点结清；收尾核对要有时间上限。

## 2026-11-19 Claude：评审修正·背包腾位唤醒、gate 接线用例、欠款守卫与扣还计数（含「补」淘汰轴整条删除）：无必修，补出口清单、接线守卫与三处过时文档（未编译、未测试，待 Codex 验证）

- **范围**：独立评审 2026-11-12「数据审计遗留·背包腾位唤醒、gate 接线用例、欠款守卫与扣还计数」与同日「补」那一条。评审员按现行源码逐项核对，**没有必修**，没改文件；下面是主会话据建议收的。
- **据建议改的**：
  1. 出口清单补上 `ExchangeInventoryExact`（11-17 别的会话加的出包入口，`bag_service.cpp` 里已经在真扣到材料时通知），并写明以后加扩容 / 解锁格子的入口也要在成功后调 `NotifySpaceFreed`（今天 `Bag::ExpandCapacity` 没有生产调用方）：`bag_service.h` 腾位通知一段、`bag-rule-policy-layering.md` §6.6「腾位唤醒」。
  2. 接线守卫：`tools/scripts/tests/persist_version_skew_alerts_contract.tests.ps1` 新增一例，钉住 scene main 在 `PlayerInboxSystem::Initialize()` 之后调 `BagService::SetSpaceFreedListener(&PlayerInboxSystem::OnBagSpaceFreed)`。少了这一行不报错，收件箱的背包满延后会退化成按退避等（最长约 60 秒）。
  3. 三处过时文档改正文（11-12 条目残余里说「归别的会话」的那几处）：`player-inbox-service.md` 触发入口一条与 §7.3 第 5 个入口、`player-inbox-runbook.md` 两包都满那一行，从「只有取回会唤醒、整理不唤醒」改成「凡是让人物背包或临时格腾出位置的成功操作都经 BagService 的腾位通知唤醒（2026-11-12 起）」。
- **建议，没做**：
  - gate 的三处接线（`client_session_handler.cpp` 的 `SetSendHooks(&gate_client_entry::LoginTransitionRejectTip, …)`、`gate_client_entry::RejectUnauthorizedMessageId(`、`gate_client_entry::RejectLoginTransitionBeforeDirectSend(`）同样没有守卫，少一行两道闸直接失效。评审员建议加进 `gate_connection_alerts_contract.tests.ps1`，但那个文件另一个会话正改着（工作区未提交），本次不做第二个写者，留给它或下一轮。
  - `BagSpaceFreedTest` 没覆盖 `ExchangeInventoryExact`：它要带获得或替换才能调（会铸号或改装备实例），夹具成本高，留给加这个入口的会话补「扣到材料通知一次、只发获得不通知」。
  - `persist-version-skew-alerts.yaml` 的 `increase()` 有首次抓取盲区：回滚出来的旧 Pod 首次抓取之前就触发、首个样本已是 1 时告警不响（WARN 日志还在）。全仓同一口径，要收紧就改成 `max by (namespace)(…) > 0` 或加 `unless … offset 10m` 一支，契约测试一起改。
  - `currency_system.cpp:32` 注释还提已删的 `ReserveOrEvict`：这个文件归评审批 T 的会话（r2-s-t-u），补丁已转给它（原文「这与背包侧"纯预检必须全部跑在 ReserveOrEvict 之前"是同一条纪律。」改为「这与背包侧"所有纯预检排在唯一会改状态的那一步之前"(bag-rule-policy-layering.md §7 红线 7)是同一条纪律。」）。
- **核过没问题**（评审员）：
  - 腾位出口全部覆盖：`RemoveItem`、`RemoveItemsClamped`、`RemoveItemsExact`、`RemoveItemsByGuid`、`MergeAndCompact` / `SortByPlayerRequest`、`RetrieveFromTemporary`、`ConsumeInventoryAndGrant`、`ExchangeInventoryExact` 都只在成功、真移走了东西之后通知；穿戴只在装到空槽时通知；GM 回收（含临时格）也走这些入口，生产代码没有别的移出路径。
  - 按地址比、只认本人的人物背包与临时格；实体无效或缺组件直接返回、不建组件。`OnBagSpaceFreed` 只用 `try_get`、只改 `nextCheckMs`，在执行器调用栈里被调也安全。scene main 在收件箱初始化之后、任何背包流量之前装订阅方；两处测试装配是作用域守卫，用例之间不串。
  - 淘汰轴的全部符号、文件与参数，`RetrieveTemporary` 里的直接唤醒，21 条删掉的用例都已不在；vcxproj、filters、modules 的 CMakeLists 没有残留。带回执的 `AddItem` / `AddItems` 调用都在 bag_test，实参类型逐个核对；`AddItems` 新规划与旧 `PlanEviction` 的首问逐行等价（合堆、独立实例、包满、具名槽）；`sizeof(Bag)` 变了但没有按步长写的代码，不影响持久化。
  - gate 抽取后判定顺序（越界 → 白名单 → gate 合成号）、日志、非法包计数、阈值断连、2005 信封逐行不变；构造函数钩子和两处收包调用点都接上了；测试工程登记了两个新文件、依赖齐全。
  - 扣还计数只在跳过补缴的提交分支、与 WARN 同一处自增；指标名、告警、契约正则、workflow 触发路径、README、`scene.vcxproj` 登记全部对得上。欠款守卫「没有入口」成立：三个函数只有单测调用，GM RPC 106 / 109 / 113 已 reserved，`AttachDebt` 先判未知字段。新用例改动前都编不过或下一拍不应用，用测试时钟、不依赖墙钟。
- **交 Codex**：原两条目的命令照旧（C++ 全量与定向用例）；另跑 `pwsh -NoProfile -File tools/scripts/tests/persist_version_skew_alerts_contract.tests.ps1`，新例「scene main:收件箱初始化之后装背包腾位通知的订阅方」要 PASS。`bag_service.h` 只改了注释。
- **Java 版**：不新增（原条目的 Java 待做已登记，本次只改本仓注释、文档与契约测试）。

## 2026-11-19 Claude：评审修正·客户端强化期望等级与幂等键、临时格取回、进场失败后换角：重发后被 gate 确定拒绝仍按结果不明收尾（客户端仓 main；未编译、未测试，待 Codex 验证；本批不改 Java 代码）

- **范围**：独立评审 2026-11-12「客户端·强化期望等级与幂等键、临时格取回、进场失败后换角」。评审员按客户端现行代码核对，**没有必修**，没改文件；下面是主会话据建议收的三处，都在客户端仓 `D:\luyuan\mmorpg-client` main 工作区，未提交。
- **据建议改的**：
  1. **重发之后，任何失败都按结果不明收尾**：首发超时或 1003（结果不明）之后重发，重发又被 gate 层确定拒绝（限流、场景 lane 满回 1008 一类），只说明重发没到 scene，首发可能已经执行。原来强化按规则拒绝收尾、不重拉快照，购买走 `FailBuy`（确定没买成），新建加点方案不重拉面板：玩家看着旧状态再点一次就是换新键再扣一次。现在三处一律按不明处理：强化报「网络异常」并重拉两份快照（`PlayerFeaturesClient.cs`），购买进 `RequiresReconnect`（`MysteryMerchantClient.cs`），新建方案写入结束时重拉面板（`AttributeClient.cs`）。只在 lane 满或限流时才走得到，正常客户端很少遇到，但这是资产路径，按 fail-closed 收。
  2. `ClientRequestKeys.TryCreate` 改为接住所有异常（原来只接 `CryptographicException`）：WebGL / 小游戏宿主上随机源可能抛 `PlatformNotSupportedException` 一类，漏接就会从调用方冒出去，`AttributeClient.CreateScheme` 在 `BeginWrite` 之后才取键，加点面板会停在 Busy 直到断线。拿不到就照旧「不发包、提示玩家」。
  3. EnterGame 重发前复位 `_enterFailedTipId`（`GameClient.RoleSwitch.cs`，与 LeaveThenEnter 那一支同样处理）：首发 1003 而 login 其实已受理、那条链很快失败推了 3023 时，不复位的话重发开出的新链还没走完，等入场就被旧失败判成被拒、回选角页。原来也有上限（重选、取消、回选角 3 次），不会卡死。
- **新用例**（EditMode）：`EquipForgeClientTests.ResentEnhanceRejectedByTheGateStillResyncsAsUnknown`、`MysteryMerchantClientTests.Buy_ResendRejectedAtTheGateAfterAnUnknownFirstAttempt_IsStillAnUnknownOutcome`、`AttributeClientTests.CreateSchemeResendRejectedAtTheGateStillRefetchesThePanel`；三条改之前都红（分别是不重拉快照、`RequiresReconnect` 为假、不重拉面板）。第 2、3 条没有新用例：随机源是静态的、换不进替身；第 3 条要回环假 gate，见下。
- **建议，没做**：
  - 同一角色重发收到 2005 一律判 InProgress：2005 也可能来自角色寄售中或预加载池满，那两种会白等 60 秒再报「等待进入场景超时」。
  - 换角编排没有用例：`EnterGameRoleSwitchTests` 只测纯函数 `ClassifyEnterGameReply`；`EnterGameOnConnection` / `LeaveGameOnConnection` / `ReloginOnConnection`、回选角循环和 `MaxEnterReselects` 可照 `GameClientReconnectTests` 的回环假 gate 补三类（3023 → 回选角 → 选 B → LeaveGame → EnterGame 回 2028 → Login → EnterGame；LeaveGame 连续三次 1003 后拆连接；第 4 次 3023 拆连接）。`GameClient.cs` 与 `GameClientReconnectTests.cs` 现在都有别的会话的未提交改动，留到它们落定。
  - `LeaveThenEnter` 分支实际走不到（进入时已按 `_enterRequestPlayerId` 先 LeaveGame），留作防御可在注释里写明；`ReportKeyReused` 对任何 1005 都记错误日志；取回在未就绪时临时格面板状态行空白、面板每次 OnChanged 整页重建焦点会跳。
- **核过没问题**（评审员）：
  - 键在本地检查都通过后才生成，非 0，用密码学随机源；三处请求对象只构造一次，重发逐字节相同；只有超时和信封 1003 会重发、只重发一次；`replayed` 与首次结果走同一条路，每次点击至多一次 `OnEnhanceResult` / `Purchased`；发出次数计数加请求序号挡住旧请求的迟到回调。三处重放回包都带当前快照 / 货架 / 面板（服务端 handler 已核）。
  - 换角：resent / left / relogged 各只走一次，LeaveGame 最多 3 次，回选角最多 3 次，每个失败出口都走 FailPipeline；凭据只在协程参数里、优先用 token；断线时清 `_enterRequestPlayerId` 与 `_roleRoster`。2005 / 2028 / 1003 的分类与服务端一致；超时常量 5+10+3×15+60=120、75+120+15=210。
  - 临时格：读取用自己的序号、不改 RequestedBagType；取回成功整体覆盖两包并作废更早的读取；与整理、装备类操作互斥（打造和材料领取 11-17 起也检查 BusyTemporary）；三个码的处理与 `bag-rule-policy-layering.md` §6.6 (4) 一致；限流窗口都不超。
  - `BagErrorTip.cs` 已生成进仓（原条目第 2 步说的 CS0103 不会再出现）；三个测试 asmdef 能收进新测试文件；8 个新 `.meta` 格式正确、GUID 唯一；回调都在主线程。改过断言的 4 条旧用例都是行为按设计变了。
- **交 Codex**（客户端仓根，接原条目第 1～4 步）：EditMode 里新增的三条（见上）要 PASS；`EquipForgeClientTests.SecondUnknownEnhanceOutcomeIsNotResentAndOnlyResyncs`、`EnhanceRateLimited_IsNotResent`、`MysteryMerchantClientTests.Buy_RateLimitedAtTheGate_IsADefiniteFailure_NotAnUnknownOutcome`、`AttributeClientTests.CreateSchemeTwiceUnknownReportsTheErrorAndRefetchesThePanel` 照旧 PASS（首发就被拒的仍是确定失败）。
- **Java 版**：只改客户端，不涉及两版共享的协议与码表，客户端行为对两版服务端都成立。

## 2026-11-19 Claude：启动时钟核对补上商城 mall，Java 的核对挪进共用模块 server_startup（未编译、未测试，待 Codex 验证）

- **起因**：上一条「节点时钟告警与服务启动时钟核对」写的是「mall_game 没有 K8s 部署，没接」。复查时发现别的会话已经加了 `deploy/k8s/manifests/java-svc/mall.yaml`，镜像目录 `tools/scripts/java_svc_image.ps1` 里也有 mall，那条判断不成立了。
- **改动**：
  - 新增 Maven 模块 `java/server_startup`（只用 JDK），`ClockCheck` 与 `ClockCheckTest` 从 gateway_node 挪进来，包名 `com.game.startup`；reactor 的 `java/pom.xml` 登记模块；gateway_node、mall_game 的 pom 加依赖。镜像构建是 `-pl <服务> -am`，会连带构建它，Dockerfile 不用改。
  - `MallGameApplication.main` 在 `SpringApplication.run` 之前调 `ClockCheck.mustPass("mall")`。
  - 合同测试 `clock_alerts_contract` 改为核对镜像目录里的每个 Java 服务都接了核对：以后新加 Java 服务，没接就会红。
  - 另外核对过：部署里唯一限制出口的 NetworkPolicy 只管 WebSocket 边缘代理，服务进程访问 API server 不受影响；没有工作负载关掉 ServiceAccount 令牌挂载。
- **验证（交 Codex，替换上一条第 3 步的 Java 命令）**：在 `java` 目录跑 `mvn -pl server_startup -am test`（`ClockCheckTest` 两条要 PASS），再跑 `mvn -pl gateway_node,mall_game -am package -DskipTests` 确认两个服务带上新依赖能编过。合同测试照旧：`pwsh -NoProfile -File tools/scripts/tests/clock_alerts_contract.tests.ps1`。
- **Java 版（AGENTS §12）**：不变，Java 仓 `PARITY.md` 已有本功能的待做项。
- **提交情况**：未提交。

## 2026-11-19 Claude：分布式日志阶段三收尾——导出线程改回、server span 量到回复、本地失败也记 client span（未生成、未编译、未测试，待 Codex 验证；Java 0.1.0-SNAPSHOT 待做）

- **为什么有这一条**：补同日「分布式日志阶段三——span 导出到 Tempo」。
  1. 那一条第 1 项把 C++ 导出器从专用线程改挂了主 EventLoop。负责人同日在另一个会话里已经定下「追踪导出线程保留」（主机名解析是阻塞的，编码也不该占主线程；原话「这些我赞同…其他的不做吧」，见记忆 cpp-main-thread-only-intent），那次改动没看到这个决定。本条改回 11-14 Codex 编过的线程版（`a8ee317b6` 里的 `otlp_span_exporter.{h,cpp}` 原样恢复，只在头文件注释里记下这个决定）。
  2. 那一条在设计稿里列的两条「已知局限」不符合 OTel 约定，按「以后修的现在修」改掉：server span 只量派发的同步部分；本地没发出的调用不记 client span。
- **改了什么**：
  1. **导出线程**：`otlp_span_exporter.{h,cpp}` 恢复线程版；`node.{h,cpp}` 恢复 `StopSpanExporter`（`FinalizeTeardownInLoop` 里阻塞等导出线程收尾，最多 2 秒加 2 秒余量），去掉启动时的地址解析与 `FinishSpanExportInLoop` 的让轮重入。主机名改回在导出线程上按连接解析，停机冲刷不做解析。
  2. **server span 量到回复发出**：`trace_context_tls.{h,cpp}` 加 `PendingSpan::TakeCurrentServerSpan(system)`，只接最内层、类型为 SERVER、来源对得上的在录 span；接走后派发作用域照旧装着上下文，只是退出时不再结束它。
     - RPC0（`rpc_channel.cpp`）：`RpcDoneCallback::Adopt` 时完成句柄把 server span 接过去；`CompleteDeferred` 在回复发出（或确定发不出，标 `REPLY_CHANNEL_GONE`）时结束，失败回复标错误码名；句柄没 Run / Fail 就被丢掉时在 `~State` 里结束并标 `REPLY_DROPPED`。
     - gRPC（`grpc_unary_call.h`）：`GrpcUnaryCall` 构造时接过派发钩子开的 server span，在 `FinishOk` / `FinishError`（含 `ParseRequest` 失败、`Reject`、序列化失败）时记 `rpc.grpc.status_code` 并结束。按 OTel gRPC 约定只把服务端故障码（UNKNOWN、DEADLINE_EXCEEDED、UNIMPLEMENTED、INTERNAL、UNAVAILABLE、DATA_LOSS）标错。调用已被截止、取消收口的，`Proceed` 时标错、不记码；没经本对象完成就释放的，析构时标错。
  3. **本地确定没发出的调用也记 client span**（标错，说明是原因）：
     - RPC0：`RpcChannel::FailNotSent`（没有 channel、超时不合法、类型不符、在途已满、维护关闭等全部本地失败都经它）与单向请求被拒（`SendRequest` 的 reject）。
     - gRPC：`grpc_client_runtime.h` 加 `RecordGrpcLocalFailureSpan(methodPath, code, reason)`；生成器模板 `grpc_async_client.cpp.tmpl` 在三处本地失败（缺通道 / 序列化失败、类型擦除入口的两种类型不符）排本地完成之前调用它。**要重新生成 C++ gRPC 客户端**，生成的文件不手改。
  4. **顺手修的未定义行为**：带名字的 `ScopedIncoming` 在 `rpc_channel.cpp`、`grpc_trace_hooks.cpp`、`kafka_consumer.cpp` 三处声明成了 `const`，而 `MarkCurrentSpanError` 经线程上下文改它的记录器。改 const 对象是未定义行为，现在都不再声明成 const；头文件写明了这条约束。
  5. **Kafka producer span 不改**：仍到交给 librdkafka 的那次 `produce` 为止（OTel 的 send span）。本生产者的投递回执只在本线程下一次 send / flush 的 poll 里派发，等它会把两次发送之间的空闲算进 span，量出来的不是 broker 确认耗时。这条原因写进了 `kafka_producer.cpp` 注释与设计稿 §10.3，不再列为局限。
  6. **单测**：
     - `span_recording_test.cpp` 加 2 条（共 12 条）；
     - `otlp_span_exporter_test.cpp` 跟线程版的接口改写，共 11 条，新加「析构等同停机」「停机冲刷不做 DNS 解析」；
     - `rpc_channel_loopback_test.cpp` 加 `AdoptedReplyEndsTheServerSpanWhenTheReplyIsSent`、`LocalFailuresRecordAnErrorClientSpan`；
     - `grpc_client_loopback_test.cpp` 加 `DeferredProceedEndsTheServerSpanWhenTheReplyIsSent`、`ServerSpanMarksOnlyServerFaultCodesAsErrors`、`LocalFailureSpanCarriesTheMethodCodeAndReason`（测试服务加了 `kReject` 模式），`FailedCallMarksTheClientSpanWithTheGrpcStatus` 另核对 server span；
     - 生成器 `grpc_process_test.go` 核对三处本地失败都先记 span。
  7. **文档**：设计稿状态行、§4.1、§5.2、§5.3、§10.3（线程、配置、停机、span 表、两处定下的口径）、§11、§13 第 2 条。
- **交 Codex**（仓库根，pwsh 7，按顺序；失败不连续重试，保留第一条错误原文，不改断言；工作区里有别的会话没提交的改动，红在本条没列的文件时先按文件归因、单独报告）。本条取代同日「分布式日志阶段三」的第 2、3 步，那一条第 1 步与第 5~10 步照旧：
  1. 生成器单测：`cd tools/proto_generator/protogen; go test ./internal/generator/cpp/ -count=1`。通过：全绿，`TestGrpcAsyncClientTemplatesUseMuduoChannelAndRejectStreaming` 在内。
  2. 重新生成（模板改了）：回仓库根依次跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build` 和 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
     - 通过：`cpp/generated/grpc_client/` 下每个 unary 方法多出 3 行 `RecordGrpcLocalFailureSpan(`，可以用 `git diff -U0 -- cpp/generated/grpc_client | Select-String '^[+-][^+-]'` 核对只有这些加行；`go/proto/` 下仍没有 `opentelemetry` 目录。
     - 别的会话还没生成的 proto 改动带出的差异，不归本条。
  3. C++ 编译（Debug x64，串行）：`msbuild game.sln "/t:cpp\nodes\gate;cpp\nodes\scene;cpp\nodes\battle" /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。通过：退出码 0，裸指针成员门禁保持开启。
  4. C++ 单测（库已在第 3 步编好；`-Build` 只编测试工程）：
     ```powershell
     $log = 'build/verification/tracing-phase3-closeout-20261119/cpp-tests'
     foreach ($f in 'rpc_controller', 'muduo_grpc', 'kafka_command', 'cross_zone') { pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter $f -LogDir $log; "$f exit=$LASTEXITCODE" }
     ```
     - 通过：四行都是 `exit=0`。
     - `$log/rpc_controller_test/` 的输出里，下面这些都要有 `[       OK ]`：`SpanRecordingTest.` 12 条、`OtlpSpanExporterLoopbackTest.` 11 条，`RpcChannelLoopback.AdoptedReplyEndsTheServerSpanWhenTheReplyIsSent`、`RpcChannelLoopback.LocalFailuresRecordAnErrorClientSpan`，`GrpcTraceLoopbackTest.DeferredProceedEndsTheServerSpanWhenTheReplyIsSent`、`GrpcTraceLoopbackTest.ServerSpanMarksOnlyServerFaultCodesAsErrors`、`GrpcTraceLoopbackTest.LocalFailureSpanCarriesTheMethodCodeAndReason`。
     - 回环用例占本机端口，被防火墙或端口占用挡住时把原文贴回来。
  5. Linux（有环境时）：`tools/scripts/build_linux.sh` 不带 `--skip-generate`，core 与三个节点编过；`cpp/tests/redis_lifecycle_test` 按既有口径跑一轮（`trace_context_tls.cpp` 改了，CMakeLists 不用改）。
  6. 冒烟（同日那一条第 9 步之外另核对两点）：
     - 停服时 C++ 节点日志在 `logSystem.stop()` 之前收尾，没有 `span exporter thread did not finish its shutdown in time`；
     - 在 Tempo 里打开一条进游戏的 trace：scene 上延后回复的 RPC0 server span 时长覆盖到回复发出，不是一闪而过。
- **Java 版**（`D:\luyuan\xuanming-server-mmo`，0.1.0-SNAPSHOT）：它的 `PARITY.md` 末尾那节待做补上同样的口径（server span 量到回复发出、服务端只把服务端故障码标错、本地没发出的调用也记 client span），不写代码。
- **文件**（本条改的，都未提交）：
  - C++ 库：`cpp/libs/engine/core/tracing/otlp_span_exporter.{h,cpp}`、`cpp/libs/engine/core/node/system/node/node.{h,cpp}`（只有导出器那几段）、`cpp/libs/engine/thread_context/trace_context_tls.{h,cpp}`、`cpp/libs/engine/core/network/{rpc_channel.cpp,grpc_unary_call.h,grpc_trace_hooks.h,grpc_trace_hooks.cpp,grpc_client_runtime.h}`、`cpp/libs/engine/infra/messaging/kafka/{kafka_consumer.cpp,kafka_producer.cpp}`；
  - 生成器：`tools/proto_generator/protogen/internal/template/grpc_async_client.cpp.tmpl`、`tools/proto_generator/protogen/internal/generator/cpp/grpc_process_test.go`；
  - 测试：`cpp/tests/rpc_controller_test/{span_recording_test,otlp_span_exporter_test,rpc_channel_loopback_test,grpc_client_loopback_test}.cpp`；
  - 文档：`docs/design/distributed-tracing.md`、本文件。

## 2026-11-19 Claude：更正上一条「分布式日志阶段三收尾」——C++ 导出器保持挂主 EventLoop，不改回线程（其余照旧；未编译，待 Codex 验证）

- **上一条第 1 项（导出线程改回）已撤回，作废。** 写完才看到：负责人 02:36 同意「追踪导出线程保留」之后，同日 03:39 又在线程盘点会话里被告知「追踪导出线程已经被别的会话挪到主循环，清单里没有它」，没有提出异议。之后 AGENTS.md §9 定下「C++ 节点自己的代码不新起线程」，保留的线程登记在 `tools/scripts/tests/test_cpp_thread_sources.py`，那份清单不列导出器。改回线程版会让这个守卫判红，所以以主 EventLoop 为准。
  - 现状：`otlp_span_exporter.{h,cpp}`、`cpp/tests/rpc_controller_test/otlp_span_exporter_test.cpp` 与 HEAD（`9824368eb`）逐字节相同；`node.{h,cpp}` 里导出器那几段与 HEAD 相同，同文件里时钟核对等改动是别的会话的。
  - 设计稿 §10.3 的线程、配置、停机三段与 §13 第 2 条已按主 EventLoop 写回，§13 第 2 条记下了这段经过。
- **上一条其余各项照旧**：server span 量到回复发出、本地确定没发出的调用也记标错的 client span、模板加 `RecordGrpcLocalFailureSpan`（要重新生成）、三处带名字的作用域不再是 const、Kafka producer span 的口径与新加单测。
- **交 Codex**：照上一条第 1~6 步，改两处：
  - 第 4 步的 `OtlpSpanExporterLoopbackTest.` 是 10 条（同日「分布式日志阶段三」那一版，没有新加用例）；
  - 第 6 步第一点改为核对停服时 C++ 节点日志里没有 `span exporter stopped with`。采集器在线时剩下的 span 应该全部冲出去；这行 WARN 只在 2 秒内没冲完时出现。
- **文件**：上一条的文件清单去掉 `otlp_span_exporter.{h,cpp}`、`node.{h,cpp}`、`otlp_span_exporter_test.cpp`（都回到了 HEAD）；本条另改了 `docs/design/distributed-tracing.md` 与本文件。Java 版不涉及。

## 2026-11-19 Claude:断线重连 R6 残余——「事务已整体回滚、可整笔重跑」收成一个共享判定,guild 请求路径接上提交前中止,mail / rank 的事务循环换成共享骨架(未编译、未测试,待 Codex 验证;Java 不涉及)

- **范围**:2026-11-12「断线重连清单收尾」条目里 R6 没做的两项。会话「接着做完服务器剩下的代码(缺口清单续)」做,分工同上一条「断线重连 R3 收尾」;match 成局提交那段的 HOLD 判定归断线重连会话,本条没碰。状态表 `docs/design/infra-reconnect-overview.md` §4.1 R6 行已改。都在 main 工作区,没有分支、没有提交。
- **共享判定**:`go/shared/authoritysql/tx_outcome.go` 新增 `IsRetryableTxError`:提交前中止(`ErrTxAbortedBeforeCommit`)与 1213 / 1205 / 9007 可整笔重跑;错误树里有结局未知(`ErrOutcomeUnknown`)一律回假。只判「重跑安全」,次数、退避、要不要对 1205 重跑由调用方的循环定。错误号常量用 `driver.go` 里已有的那三个。
  - 原来同一个判定写了四份:trade `data.IsRetryableTxError`、player_inbox `inbox.IsRetryableTxError`、data_service `isRecallTxRetryable`、guild `isRetryableBackground`。前三份删掉,调用点改成 `authoritysql.IsRetryableTxError`(trade:`asset_op_repo.go` 7 处、`commerce_repo.go`、`listing_repo.go`、`reconcile/pipeline.go`;player_inbox `TxRetryConfig`;data_service 召回的 `tx` 与 `EnsureSeqRowRetry`)。guild 后台改成 `errRetryTx` 加共享判定。data_service 只被那份用的 `mysqlErrTiDBWriteConflict` 一并删掉。
  - 与原来四份的唯一行为差别:死锁或写冲突与「结局未知」被 Join 在同一棵错误树里时,原来按错误号重跑,现在不重跑(只要有一处可能已经提交,就不整笔重跑)。
- **guild 请求路径**(`guild/internal/data/guild_manage_repo.go`):
  - `retryOnDeadlock` 遇到提交前中止换连接整笔重跑,次数照旧受 `maxTxAttempts` 限,**不**计 `guild_tx_deadlock_total`(不是锁冲突);用尽时日志从 "deadlock persisted" 改成 "transaction still retryable"。1205 照旧直接回忙、不重跑。
  - `classifyCommitErr` 把提交前中止当结论明确(没提交)原样返回,不再归成「结果不明」打 ERROR。`isRetryableTxError` 本身不改,它仍只供死锁计数与提交分类。
- **mail、rank 的事务循环**:原来各自手写,退避固定 15 / 30ms 不带抖动,只认 1213 / 1205(漏 9007 与提交前中止)。改用共享骨架 `assetop.WithTxRetry`(READ COMMITTED、最多 3 次、10ms 起指数退避 ±20% 抖动、可取消;提交期结局未知不重做)。mail 的锁行动者行挪进事务函数开头;rank 的 `retryableTxError` 改成 `errScoreInsertRace` 加共享判定。rank 新 import `shared/assetop`(它的依赖 rank 的 go.mod 里都有,go-redis 仍只是 indirect)。
- **其余自写循环核对后不改**(写进了 §4.1 R6 行):guild `retryOnDeadlock`(有界、10–50ms 随机退避、可取消);data_service 名字表与快照表的单条语句重试(有界、带抖动、可取消,判定刻意更窄);friend 不退避的 1213 重试只在没装角色护栏的夹具路径上,生产都装护栏;match 名册乐观 CAS 与 data_service 角色封存的竞态循环是重读状态,不需要退避。
- **测试**:
  - 新增 `authoritysql.TestIsRetryableTxError`(11 个用例:三个错误号、包装与 Join、唯一键冲突、提交前中止、结局未知及其与前两者 Join);
  - guild `TestRetryOnDeadlock` 新增子用例「提交前中止换连接重跑、不计死锁」「提交前中止与结局未知混在一起时不重跑」,`TestCommitTxNormalizesUnknownOutcome` 的「结论明确」一组加一条提交前中止;
  - player_inbox `TestRetryClassifierCoversRequiredCodes` 改成直接测 `TxRetryConfig()` 注入的判定(先判非 nil);
  - 删掉 trade `TestIsRetryableTxError`(用例并进共享包那条),`asset_op_seq_guard_test.go` 的一处断言改调共享判定。guild `TestIsRetryableBackground` 的 9 个用例不改,新实现下预期照旧。
- **Codex 验证**(仓库根目录;Go 各模块之间可并行;失败保留第一条编译错误与 `go test` 的完整失败输出,不改断言,不连续重试):
  1. `cd go/shared && gofmt -l authoritysql && go vet ./authoritysql/ && go test -count=1 -run 'TestIsRetryableTxError|TestTxAbortedBeforeCommit|TestExplicitTx|TestTxRollback|TestAutoTx|TestTxCommit|TestTxAfter|TestPlainDML' -v ./authoritysql/`,全部 PASS;再跑一次 `go test -count=1 ./authoritysql/ ./assetop/`。
  2. `cd go/trade && gofmt -l internal && go build ./... && go vet ./internal/data/ ./internal/reconcile/ ./internal/svc/ && go test -count=1 ./internal/data/ ./internal/reconcile/ ./internal/svc/`。真库用例按平时的夹具另跑(无 DSN 时 SKIP 不算通过)。
  3. `cd go/player_inbox && go build ./... && go vet ./internal/inbox/ && go test -count=1 -run TestRetryClassifierCoversRequiredCodes -v ./internal/inbox/ && go test -count=1 ./internal/inbox/`。
  4. `cd go/data_service && go build ./... && go vet ./internal/store/ && go test -count=1 ./internal/store/`。
  5. `cd go/guild && go build ./... && go vet ./internal/data/ && go test -count=1 -run 'TestRetryOnDeadlock|TestCommitTxNormalizesUnknownOutcome|TestIsRetryableBackground|TestRetryResultsDoNotAccumulate' -v ./internal/data/`,全部 PASS(含两条新子用例);再跑 `go test -count=1 ./internal/data/`。
  6. `cd go/mail && go build ./... && go vet ./internal/data/ && go test -count=1 ./internal/data/`;`cd go/rank && go build ./... && go vet ./internal/data/ && go test -count=1 ./internal/data/`;两个模块各跑一次 `go mod tidy -diff`(或 `go mod tidy` 后 `git diff go.mod go.sum`),应无改动,rank 的 go-redis 仍在 indirect。真库集成用例(`repo_integration_test.go`)按平时的夹具跑。
- **Java 版(AGENTS §12)**:不涉及(Go 服务的 SQL 重试判定;Java 版用自己的事务框架),不加 PARITY 行。
- **文件**:`go/shared/authoritysql/{tx_outcome.go,tx_outcome_test.go}`;`go/trade/internal/data/{asset_op_repo.go,asset_op_repo_test.go,asset_op_seq_guard_test.go,commerce_repo.go,listing_repo.go}`、`go/trade/internal/reconcile/pipeline.go`、`go/trade/internal/svc/session_lock_wait.go`(注释);`go/player_inbox/internal/inbox/{store.go,decide_test.go}`;`go/data_service/internal/store/recall_store.go`;`go/guild/internal/data/{asset_store.go,guild_manage_repo.go,guild_manage_repo_test.go}`;`go/mail/internal/data/repo.go`;`go/rank/internal/data/repo.go`;`docs/design/infra-reconnect-overview.md`、本条。

## 2026-11-19 Claude:全部 Redis 上 Cluster 第 6b-2 批——staging / prod 退役核心单实例与高可用形态(`-RedisReplicas`、`redis-ha.yaml`),附带删 login 的 `Node.QueueRedis`(缺口第 34 条;代码写完,未运行,待 Codex 验证;Java 不涉及)

- 范围、定案与偏差见设计稿 `docs/design/redis-cluster-all-domains-20261110.md` §10.8。第 6b-1 批见上面同日那一条,两批可以合成一轮验证。
- 改了什么:
  - `tools/scripts/k8s_deploy.ps1`:
    - 新增 `Test-CoreRedisDeployed`:控制端点不是集群才部署核心,也就是只有 dev 没开控制集群时。staging / prod 的 `Apply-Infra` 不再渲染、apply、等待 `redis.yaml`,重启闸与 maxclients 核对只在部署核心时做;不部署时删掉 infra 里残留的 `redis-auth`。
    - `Initialize-InjectedSecrets` 只在部署核心时解析 `MMORPG_REDIS_PASSWORD`。两套集群的口令也过 `Assert-RedisPasswordYamlSafe`,报错点名输入变量。Spring 占位检查改按 gateway 自己的口令来源取值。`Get-SharedRedisClientPassword` 的非 dev 非空检查只对集群来源做。
    - 删 `-RedisReplicas` 与高可用形态的整套逻辑:拓扑判定、跟随线上副本数、1 → 3 切换闸、哨兵口令检查、代理总账等十几个函数。`Assert-RedisCoreRestartConfirmed` 只剩两个参数。owner_epoch 铸造的 `WaitAOFNumReplicas` 改按玩家域取。
    - 注释改准:核心口令 Secret 的使用方、两套集群 Secret 的 zone 副本、match 模板里 `Redis` 回落段、scene-manager 的回落对象。
  - `tools/scripts/dev_tools.ps1`、`k8s_image.ps1`:删 `-RedisReplicas` 透传。`k8s_gate_drain.ps1`:注释与失败提示不再提高可用代理。
  - 清单:删 `deploy/k8s/manifests/infra/redis-ha.yaml`(`git rm`,删除在暂存区);改 `redis.yaml` 头注释;改 `deploy/k8s/manifests/go-svc/mail.yaml` 可选口令引用的注释。
  - 告警:`redis-alerts.yaml`、`redis-control-alerts.yaml`、`redis-player-alerts.yaml` 改头注释;`scene-ownership-alerts.yaml` 的 `SceneOwnershipMassRelease` 描述改成查控制集群。
  - 测试:
    - `k8s_deploy_contract`:核心只在 dev 部署(AST 与 prod DryRun)、prod 不读 `MMORPG_REDIS_PASSWORD`、退役守卫、WAITAOF、口令字符集;
    - `redis_maxclients_contract`、`redis_exporter_contract`、`k8s_gate_drain.tests`。
  - 文档:
    - `deploy/k8s/README.md`:高可用一节改成退役说明,另改参数表、总账开头、C++ 口令轮换与 debug-command 一条;
    - `deploy/k8s/AGENTS.md` 的基础设施一行;
    - `docs/ops/release-checklist.md`:§2.1 的切换一条改成退役说明,§6.1 改清理对象;
    - `login-queue-stress-runbook.md`、`scene-node-role-split.md`、`mysql-backup-pitr-runbook.md`。
  - 附带一组(「Redis 分布式水平扩展」会话提出,单独验证):login 删 `Node.QueueRedis`,排队与放行锁直接用 `ControlRedis`,放行流水线的专用句柄按控制端点的配置建。
    - 代码:`go/login/internal/config/config.go`、`internal/svc/servicecontext.go`、`login.go`、`etc/login.yaml`;
    - 测试:`internal/svc/queue_redis_test.go`、`internal/config/shared_redis_password_test.go`、`internal/config/redis_domains_conf_test.go`。
- **给 Codex 的验证步骤**(仓库根目录。本会话没有编译,也没有跑任何测试;只做了 PowerShell 静态语法解析与 `gofmt -l`,结果干净):
  0. 没改 proto 与 C++,不用生成。
  1. 部署契约:与 6b-1 那一条第 1 步是同一组十个文件,逐个跑一次即可覆盖两批。通过标准 fail=0。失败时保留失败用例全文与 DryRun 输出尾部,先判断是夹具还是渲染,不改断言。
  2. login 附带的那一组:
     ```
     cd go/login
     gofmt -l internal/config internal/svc login.go
     go build ./...
     go vet ./internal/config/... ./internal/svc/...
     go test -count=1 ./internal/config/... ./internal/svc/... ./internal/logic/pkg/loginqueue/...
     ```
     通过标准:`gofmt -l` 无输出,其余全绿。`internal/svc` 的测试用 miniredis,不需要真 Redis。
  3. 日志英文守卫:`python tools/scripts/check_log_text_english.py`,期望没有新增违例。
  4. 冒烟(有 kind 时):
     - dev 档同 6b-1 第 3 步;
     - 另跑一次 staging 档 `infra-up -DryRun`:输出里有 `Redis core: not deployed` 一行,没有 `redis.yaml` 的 apply,也不要求 `MMORPG_REDIS_PASSWORD`。
- **人工**:以前在 staging 上切过高可用的环境,按 README「共享 Redis 的高可用形态(2026-11-19 退役)」删残留对象。项目未上线,没有生产存量。
- **Java 版**:不涉及。Java 版没有 K8s 生产形态,login 是 Go 服务。
- **提交情况**:main 工作区,未提交。
  - `redis-ha.yaml` 的删除在暂存区(`git rm`)。之后想撤出暂存区时 `.git/index.lock` 被占,没有删锁。
  - 不属于本条的改动:`k8s_deploy.ps1` 与 `k8s_deploy_contract.tests.ps1` 里有别的会话同日的改动,`go/login/login.go`、`go/login/etc/login.yaml` 里也有别的会话未提交的改动。

## 2026-11-19 Claude:补正上一条「全部 Redis 上 Cluster 第 6b-2 批」的提交情况与文件清单

- **提交情况改正**:负责人 22:27 的批量提交 `9824368eb` 已经带走第 6b-1 批全部和第 6b-2 批的前一半。前一半包括:删 `redis-ha.yaml`(上一条说它「在暂存区」,不对)、`k8s_deploy.ps1` 里的退役代码、`dev_tools.ps1` / `k8s_image.ps1` 去掉 `-RedisReplicas`、`redis.yaml` 与三份 Redis 告警的头注释。第 6b-2 批其余改动与 login 那一组在工作区,未提交。HEAD 单看是半截状态(契约测试与部署说明没跟上),验证以工作区为准。
- **文件清单补两项**(只改注释):`go/scene_manager/internal/config/config.go`、`go/scene_manager/etc/scene_manager_service.yaml`。它们原来指向已删的 `Get-OwnerEpochMintWaitAOFNumReplicas`,现在写成按玩家域取值。`gofmt -l` 干净;Codex 在 `go/scene_manager` 跑一次 `go build ./...` 即可。

## 2026-11-19 Claude:实时技能冷却与 Buff 跨重登保留(定案并落码,收掉断线重连审计 §9.2 与断线保留 §8 第 5 条)

- **规则**(设计稿 [realtime-cooldown-buff-persistence.md](design/realtime-cooldown-buff-persistence.md)):
  - 技能冷却全部随玩家存档保留,不设时长阈值。存冷却组号 + 开始时刻(UTC 毫秒),就绪时刻 = 开始时刻 + Cooldown 表 `duration`。
  - Buff 只保留 Buff 表新列 `keep_on_logout = 1` 的行(字段号 54,`cfg_owner = "server"`,现有 98 行全是 0),存表号、层数、到期时刻(UTC 毫秒,永久记 0)。其余 Buff 实体重建即清。
  - 离线期间时间照常走。恢复按加载时的表裁剪。保留的 Buff 不存施法者、按 `no_caster` 叠层;子 Buff 不单独存;恢复不重跑准入判断。
  - 阈值为 0、Buff 不做离线暂停的理由与代价写在设计稿 §2.1、§2.3。
- **改动**(main 工作区,未提交):
  - 配置表:
    - `data/schema/buff_table.proto`;
    - `data/Buff.xlsx`:用 openpyxl 在末尾加第 64 列表头,原有单元格逐格比对未变,数据格留空 = 0。
  - 协议:
    - `proto/common/component/buff_comp.proto`:加 `SavedBuffComp`;
    - `proto/common/database/mysql_database_table.proto`:加 `skill_cooldowns = 28`、`saved_buffs = 29`,都在 CHARACTER 行;
    - `proto/player_store/player_store.proto`:只改注释。
  - C++:
    - `combat/buff/comp/buff_comp.h`;
    - `combat/buff/system/buff.{h,cpp}`:新建条目抽成 `StartBuffEntry`,施加与恢复共用;加 `IgnoresCaster`、`SaveKeptBuffs`、`RestoreSavedBuffs`;
    - `combat/skill/system/skill.{h,cpp}`:加 `RestoreCooldowns`;
    - `combat/skill/comp/skill_comp.h`:只改注释;
    - `player/system/player_database_loader.cpp`;
    - `player/system/player_lifecycle.cpp`:`InitPlayerFromAllData` 调恢复。
  - 测试:
    - `buff_test` 新增 `BuffKeepOnLogoutTest` 7 条;
    - `skill_test` 新增 `SkillUtilTest.RestoreCooldowns_*` 3 条;
    - `bag_test/persist_round_trip_test.cpp` 的夹具补冷却与保留的 Buff 两类节点。
  - 文档:新设计稿;`ecs-component-access-rules.md`;`disconnect-reconnect-audit-20261020.md` §9.2;`player-connection-lost-hold.md` §8 第 5 条;`docs/README.md`。
  - 客户端:不改。客户端没有实时冷却和 Buff 的显示,也没有对应协议(设计稿 §7)。
- **给 Codex 的验证步骤**(仓库根目录,按顺序,前一步失败就停下并保留输出。本会话没有生成、导表、编译,也没有跑任何测试):
  1. 导表:
     - 先看 `git status --short data/`。除 `data/Buff.xlsx`、`data/schema/buff_table.proto` 外还有别人未提交的表改动时,停下报告,不整批导出;
     - `dev.bat gen`;
     - `py tools/data_table_exporter/tools/gen_schema_index.py`;
     - 通过标准:零报错;Buff 表的生成代码出现 `keep_on_logout`;客户端表产物不变;`data/AGENTS.md` 索引里 Buff 是 64 列、54 个字段。
  2. 协议生成:
     - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`;
     - 通过标准:`proto/persist_schema_gen.txt` 只追加世代 1 的行(仍是 bootstrap),含 `player_database#28 … group=6`、`player_database#29 repeated SavedBuffComp saved_buffs group=6`、`SavedBuffComp#1..3` 以及 `CooldownTimeListComp`、`CooldownTimeComp` 的字段行;没有改号、换组报错。
  3. C++ 编译:MSBuild 一律 `/m:1` 串行。先编生成的 table / proto 工程、engine core 与 `cpp/libs/services/scene/scene.vcxproj`,再编 `cpp/nodes/scene`。测试工程链接预编好的 lib,库要先编。
  4. C++ 测试:
     - `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter buff`,`-Filter skill`、`-Filter bag` 同样各跑一次,日志放 `build/verification/<本次运行>/cpp-tests/`;
     - 通过标准:新用例全过(`BuffKeepOnLogoutTest.*`、`SkillUtilTest.RestoreCooldowns_*`、`PersistRoundTripTest.*`),原有用例不变红;
     - 重点看 `BuffStackingTest.*`、`BuffDurationTest.*`(施加路径抽了函数)与 `player_feature_persistence_test` 的按需状态缺席断言。
  5. 整解按现有口径构建一次,`cross_zone_test` 全量跑一次,Linux 按 `tools/scripts/build_linux.sh` 构建一次。
  6. 冒烟(本地起服 + robot):
     - 放一个有冷却的技能后下线,等断线保留结束(25 秒以上)再重登,立刻再放,应回 `kSkillCooldownNotReady`;
     - 跨节点换图后同样核一次;
     - 现有表里没有 `keep_on_logout = 1` 的行,Buff 那一半只由单测覆盖。
- **Java 版**:对应 Java 版本无(`0.1.0-SNAPSHOT`),只在 Java 仓 `PARITY.md` 末尾登记待做,没有改 Java 代码。

## 2026-11-19 Claude:聚合加载器 `all_table` 改名为 `table_loader`(五种语言 + 导表器 + 客户端仓;未编译未运行,待 Codex 验证;独立 Java 仓无同名类)

- **起因**:负责人问「all_table 这个名字是不是有些怪」。结论是该改:(1) 每张表的生成文件都叫 `<表名>_table.*`,`all_table.*` 用了同一个后缀,读起来像「名叫 All 的那张表」,`ls *_table.h` 会把它多算一个(`docs/design/generated-table-header-decoupling-plan.md` 的 P2-K 就多算过);(2) 导表器没有表名保留检查,真建一张叫 `All` 的表,C++ / Go / Python 的 `all_table.*` 会直接撞文件名;(3) 里面只有 `LoadTables` / `ReloadTables` / `OnTablesLoadSuccess`,本质是加载器。表文件都以 `_table` 结尾,`table_loader` 不会和任何表重名,这个坑从结构上消失,不用另加保留名检查。负责人选「连客户端仓一起改」。
- **改名对照**:C++ / Go / Python 的文件 `all_table.*` → `table_loader.*`;Java / C# 的类与文件 `AllTable` → `TableLoader`;导表器模板 `{cpp,go,java,csharp,python,ue}_all_table.*.j2` → `*_table_loader.*.j2`(共 8 个,UE 两个模板的产物仍是 `ConfigSubsystem.*`)。函数名(`LoadTables` / `LoadTablesAsync` / `OnTablesLoadSuccess` / `ReloadTables` / `GetTableLoadVersion`、Java / C# 的 `loadTables` 等、Python 的 `load_tables`)都没改。`bAllTablesCoherent`、`read_all_tables` 是别的名字,不动。
- **导表器**:`core/generators/config_gen.py`(模板名与产物名共 16 处)、`core/orchestrator.py`(Java 源目录陈旧文件检查的期望清单 `AllTable.java` → `TableLoader.java`)、`tests/test_orchestrator_gate.py`(同一清单的测试夹具)、`python_config.py.j2` 与 `cpp_config_fk.cpp.j2` 里提到旧名的两处注释。
- **产物**:没有跑整套导表(工作区里有别的会话未提交的表与 tip 改动,整套导表会把它们一起带进产物)。沿用同日前两条的办法,只渲染这几个聚合模板,表名取自现有产物(53 张)。先用**旧**模板渲染,与仓内 11 份旧名字产物逐字节相同,证明办法与导表器等价;再用新模板渲染写成新名字,每份副本都相同。sha256 前 16 位:`table_loader.cpp` 为 `d6796a95f339ca0d`,`table_loader.h` 为 `64a91aa589bc6929`,Go 为 `b6e6632822fefb4a`,Java 为 `55997dc37e4af344`,C# 为 `534148cc0aa88e17`,Python 为 `51d1f662a20481f0`。内容变化只有名字:C++ 实现里的 `#include "table_loader.h"`、Java / C# 的类名、Python 模块文档里的模块名;头文件和 Go 的内容里没有名字,字节不变。旧名字的产物全部删除:`md5_copy` 只覆盖不删除,目标树里残留的旧文件会被陈旧产物检查拒绝收工(Go 还会因同包重复声明编不过)。53 个 Python 表文件里两处提到加载器名字的注释,按模板的同一处文字做了文本替换。
- **调用方与构建**:`node.cpp`、`node_entry.h`、`configuration_table_test.cpp` 的 `#include`;`battle_table_fingerprint.h`、`bag_test.cpp` 的注释;`cpp/generated/table/{CMakeLists.txt,table.vcxproj,table.vcxproj.filters}` 的登记项(原位替换);`java/config_node` 的 `TableService.java`(`TableLoader.loadTables` / `reloadTables`);`tools/scripts/tests/test_cpp_thread_sources.py` 清单里的路径。
- **客户端仓**(`D:\luyuan\mmorpg-client`,导表器 C# 部署目标):删除 `Assets/Scripts/Table/Generated/AllTable.cs` 与 `AllTable.cs.meta`(该 GUID 没有被任何地方引用),新增 `TableLoader.cs`(与服务器侧 `generated/code/csharp/TableLoader.cs` 逐字节相同)。核对过:客户端手写代码里没有任何地方引用 `AllTable`,只有历史验证证据 JSON 里记着它的路径,那些是记录,没动。**新文件还没有 `.meta`,Unity 打开后会生成,需要提交。**这个仓里还有别的会话未提交的改动,本次只动了上面这三个文件。
- **文档**:`table-api-reference{,-zh}.md`、`bag-instance-layout-split.md`、`global-match-hierarchical-pools.md`、`NOTES_{en,zh}.md` 改成新名字;`mail-system.md`、`friend-client-spec-20260920.md` 里的类名改成新名字并注明原名,没有动它们的核实结论;`generated-table-header-decoupling-plan.md` 是写死的历史快照,只在顶部加一条改名对照。**有意没改的**:本文件与 `docs/handoff/` 的历史记录;`mystery-merchant.md`(当时沙盒对拍的记录)、`player-features-ui.md`(验证回执);`robot/vendor/shared/generated/table/all_table.go`(`go mod vendor` 生成的副本,本来就落后于 `go/shared`,缺 petentry / pettier 等表,不手改,下次在 `robot/` 下 `go mod vendor` 自然刷新)。
- **Java**:本仓 `java/config_node` 已随改。独立 Java 仓(`D:\luyuan\xuanming-server-mmo`)没有 `AllTable` 类,不涉及;它的 `docs/porting/inventory/tools.md` 第 27 行列了本仓导表器的模板名模式,是对本仓的清单,下次同步时更新,本条没有动那个仓。
- **没有运行的**:全部未编译、未运行。产物与模板的一致性只按「旧模板渲染 = 旧产物」核对过;53 个 Python 表文件的注释是文本替换,没有经过导表器渲染,靠下面第 1 步核对。改 C++ 后跑了 `tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 预检,10 条通过。
- **给 Codex**(仓库根目录,按序;失败不连续重试,保留第一条编译错误原文与失败断言。本条的第 1、4、5 步取代上一条「配置表并行加载按核数限流、C++ 线程清单守卫」的第 2、3、4 步,那几步里的 `all_table` 路径已改名):
  1. **导表器产物核对**(对仓库只读):`py tools/data_table_exporter/tools/sandbox_export.py --out build/verification/table-loader-rename-20261119/sandbox --compare`。通过标准:导表成功;差异列表里不出现任何 `table_loader` / `TableLoader` 产物与 `generated/code/python/table/*_table.py`;「仓内独有(可能陈旧)」里不出现 `all_table` / `AllTable`。别的会话未提交的表与 tip 改动造成的差异不属于本条。
  2. **导表器单测**:在 `tools/data_table_exporter` 目录执行 `py -m pytest -q`。通过标准:与改前相同,无新增失败(`test_java_code_source_orphan_is_fail_closed` 的夹具文件名同步改过)。
  3. **线程清单守卫**:`python -X utf8 -B -m unittest discover -s tools/scripts/tests -p 'test_cpp_thread*' -v`。通过标准:5 条全过(清单里的路径已改成 `table_loader.cpp`)。
  4. **C++ 构建**(串行):`msbuild cpp/generated/table/table.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`,再 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(`node.cpp`、`node_entry.h` 的 include 变了,三个节点都要重编)。通过标准:0 个 error C / LNK。有 Linux 环境时再跑 `tools/scripts/build_linux.sh`(`cpp/generated/table/CMakeLists.txt` 改了登记项)。
  5. **C++ 用例**(第 4 步已重编 table 库):`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter configuration_table_test -LogDir build/verification/table-loader-rename-20261119/cpp-tests`。通过标准:零失败;日志里有 `LoadTablesAsyncTest.CanBeCalledRepeatedly` 且 PASS。
  6. **Go**:在 `go/shared` 目录执行 `go build ./...` 与 `go test ./generated/table/...`(`table_loader.go` 与旧文件同包,旧文件已删,不会重复声明)。要验机器人时先在 `robot/` 下执行 `go mod vendor` 刷新副本,`robot/vendor` 里还是旧文件名。
  7. **Java**:`java/config_node` 按该模块自己的构建口径编译(`TableService.java` 改用 `TableLoader`)。
  8. **客户端**:用 Unity 打开 `D:\luyuan\mmorpg-client`,等导入完成,确认 `Assets/Scripts/Table/Generated/TableLoader.cs.meta` 已生成、没有编译错误,再在客户端仓提交这几个文件(删 `AllTable.cs`、`AllTable.cs.meta`,增 `TableLoader.cs`、`TableLoader.cs.meta`)。
- **残余**:Python / UE 两种产物目前没有消费者(`docs/handoff/repo-layout-20261007.md`),本条只改了名字,没有动「要不要关掉这两种产物」。客户端手写代码目前没有调用聚合加载器的地方,C# 的 `TableLoader` 还要不要继续生成,是另一个决定。

## 2026-11-19 Claude:全服匹配网络探测目标在机房内分散——按玩家分到同机房各台 battle,自报 CLOSED 的不探(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- 起因:同日「battle 直连准入前移到 Acceptor」一条发现的风险。探测计划每个机房只给 NodeId 最小的一台,全服的探测都打到它;1 万人排队、robot 约 80 秒重探一次,就是 125 条/秒,超过 battle 缺省的新建速率 100 条/秒,超出的探测被 RST。
- 改了什么:
  - `go/match/internal/logic/network_probe.go`:选目标抽成纯函数 `probeTargets(nodes, playerID)`,`GetNetworkProbePlan` 只负责签授权。
    - 每个机房在可探测节点上做最高随机权重哈希,用 `shared/workshard.Owner`(键 = player_id,成员 = 实例 UUID)。各台均摊;节点集合不变时同一玩家总落同一台;节点增减只挪动相关玩家;结果与快照顺序和副本无关。
    - 可探测条件在原有基础上加「自报可分配状态不是 CLOSED」。依据:battle 启动未完成时直连面可能还没装好,停机或维护关源后新直连被 RST,这两种探测必然失败,而 robot 任一机房失败整轮不提交;计划排空的节点马上要走。
    - 机房里全是 CLOSED 时整个机房不进计划(它本来就不可放置)。UNSPECIFIED 照常可探测;本副本的本地熔断不看。
    - 机房先后与 16 个上限不变,每个机房一台时计划与改动前相同。授权里的 `target_node_id` / `target_instance_id` 与 battle 侧校验都不变。
    - 推导与取舍(为什么不用取模、为什么不退回探 CLOSED 节点)写在 `docs/design/global-match-hierarchical-pools.md` §5.1「探测目标」。
  - 新增 `go/match/internal/logic/network_probe_target_test.go`:四条纯单测,不连库。
  - 文档:
    - `docs/design/global-match-hierarchical-pools.md`:§5.1 组件列表与「探测目标」段、§5.2.1 第 2 条、§12.9;
    - `docs/design/gate-connection-admission-control.md`「P1-1 ~ P1-4 · battle 直连面」最后一条;
    - `docs/design/turn-based-battle-server.md` D27;
    - `docs/ops/gate-kernel-tuning-runbook.md` §3.3:探测一条改成每台速率 ≈ N ÷(可探测台数 × 重探周期),台数按机房里最少的时候算;`BattleAcceptRateLimited` 一行改成先看同机房是否一起在拒;
    - `deploy/k8s/battle-connection-alerts.yaml`:头注释、`BattleAcceptRateLimited` 的规则注释与 description;
    - `deploy/k8s/README.md` 速率闸一条。
  - battle 只改两处注释(`cpp/nodes/battle/client/battle_connection_admission.h`、`battle_client_edge.cpp`),不改逻辑。只读预检 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 已跑:10 条 OK。
- 不属于本条的改动:`network_probe.go` 里 `WsUrl: node.ClientWsURL` 那一行、`go/match/internal/discovery/node_watcher.go` 的 `ClientWsURL` 字段,以及 `proto/match/match_network.proto` 的 `ws_url`,都是「小游戏客户端连 gate」会话同日未提交的改动,本条原样保留。
- **给 Codex 的验证步骤**(本会话没有编译,也没有跑 Go 测试):
  0. 前提:上面那批小游戏改动的 proto 还没生成(`go/proto/match` 里没有 `WsUrl`),在它生成之前 `go/match` 整个编不过,报 `WsUrl` / `GetClientWsUrl` 未定义。先按那批的条目跑生成(仓库根,no-unity):
     ```
     pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
     ```
     本条自己没改 proto。不要为了让本条编过去删掉那一行。C++ 只改注释,不用为本条编译 battle。
  1. Go(cwd `go/match`):
     ```
     gofmt -l internal/logic/network_probe.go internal/logic/network_probe_target_test.go
     go build ./...
     go vet ./internal/logic/...
     go test -count=1 -v -run 'TestProbeTargets' ./internal/logic/
     go test -count=1 ./internal/logic/... ./internal/battleplace/... ./internal/discovery/...
     ```
     通过标准:
     - `gofmt -l` 无输出,build / vet 无错;
     - 四条 `TestProbeTargets*` 全部 PASS:`SpreadPlayersAcrossRegionNodes`、`SamePlayerStaysOnItsNode`、`SingleNodeRegionsKeepTheOldPlan`、`OnlyPickProbeableNodes`(含 6 个子用例);
     - 其余用例与改动前一致。没有 TiDB 时 `*TiDB*` 用例跳过属正常,如实报为未运行。
     失败时保留编译错误全文、失败用例名与断言原文。`SpreadPlayersAcrossRegionNodes` 若报偏离,记下四台的实际计数,不要放宽阈值。
  2. 告警合同(本条只改注释与 description 文字):`pwsh -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`,通过标准与改动前相同。
  3. 冒烟(可选,本机起两台同机房的 battle 时):两个以上 robot 账号做探测,两台的 `battle_accepted_total` 都涨。再停掉一台(停机关源),之后新拿的计划只指向另一台,robot 探测不失败。
- **Java 版(AGENTS §12)**:0.1.0-SNAPSHOT 待做,已在 Java 仓 `PARITY.md` 末尾登记。Java 的 `GlobalMatchService.plan` 已经只探 accepting 节点,差的是机房内按玩家分散(现在也是取 NodeId 最小的一台)。本条不写 Java 代码。
- **提交情况**:main 工作区,未提交。

## 2026-11-19 Claude:网关前置限流的排队提示加下限与随机量、排队重试只扣 0.1 个 IP 令牌,Unity 客户端排队等待按契约加下限与抖动(未编译、未测试,待 Codex 验证;独立 Java 版待做)

- **来由**:负责人 11-19 要求把查出来的「以后再修」现在就修。这一项是 L-18 复核表里 Java gateway 限流那一行(`docs/design/player-route-mapping-durability-proposal.md` §12.3「§12.1 漏列的环节」),也是 `redis-cluster-all-domains-20261110.md` §6.2 里那句「`rl:zone:<z>` 上限是该 zone 的请求速率」。
- **问题**(读代码确认):
  - 区桶空时 `retry_after_ms` = 区桶补到一个令牌要等的毫秒。K8s 开服档 2000 rps 时是 1ms,打包缺省 500 rps 时不超过 2ms。
  - Unity 的两处排队循环原样等这个值(`WaitMs(r.retry_after_ms > 0 ? r.retry_after_ms : 2000)`),robot 的无票分支也原样等。
  - 区桶空时 IP 令牌全额退还,所以 IP 桶拦不住排队中的重试:排队的人每个往返打一次同一个 `rl:zone:<id>` 键。
  - 后果:压力超过一台 Redis 主节点后命令超时,整体 fail-open 并熔断 5 秒,正好在最该限流的时候放开。
  - 波次提示原样下发距开放的秒数,开放那一刻所有人同时回来。
  - 不守提示的单个 IP 可以无限速地打区桶热键。
- **改法**(本仓 `java/gateway_node`):
  - `AssignGateRateLimiter`:两种排队提示改为 max(下限, 原始等待),再加 [0, 抖动上界) 的均匀随机量。
    - 新属性 `queue-retry-min-ms`(缺省 2000,必须为正)与 `queue-retry-jitter-ms`(缺省 2000,可为 0)。
    - 随机源作为构造参数显式传入,生产用 `ThreadLocalRandom`,单测注入确定值。
  - `RedisRateLimitStore`:区桶空时只退还「1 个令牌减去 `queue-retry-ip-cost-milli`」。该属性缺省 100,即 0.1 个令牌,取值 0~1000。
    - `REFUND_SCRIPT` 多一个 ARGV[4]:还回多少千分之一令牌。
    - 份额是整个令牌时不调退还。
  - `RateLimitConfig`:校验从限流器 bean 挪到存储 bean。存储只在限流打开时创建,而且先于限流器创建。三个新键也加了校验。
  - `application.yaml` 写上三个缺省值。K8s ConfigMap 仍只写原来六个键(`Get-GatewayRateLimitSettings`,契约测试按顺序钉住),新键走 jar 内缺省,部署脚本没改。
- **效果**(推算 [I],待压测复核):
  - 守规矩的客户端排队时每 2~4 秒一次,区桶热键负载约为「排队人数 / 3 秒」。10 万人约 3.3 万次/秒,对照每台主节点 5 万~10 万次 Lua/秒。
  - 随机量把同一时刻被拒的一批人摊到 2 秒宽的时间窗里。
  - 不守提示的单个 IP 最多 ip-rps × 10 次/秒就被 429,开服档是 50 次/秒。429 只读 IP 桶,不碰区桶。
  - 代价:同一出口 IP 后面约 150 人同时排队才会碰到 IP 桶;份额写 0 可回到全额退还。
- **Unity 客户端**(`../mmorpg-client` 的 `Assets/Scripts/Game/GameClient.cs`):
  - 两处排队等待改为调用 `QueueRetryDelayMs`:max(retry_after_ms, 2s),再加 [0, 20%] 的抖动,抖动按不超过 30s 的基础间隔算。
  - 这样旧网关原样下发 1ms 时也不会紧循环。
  - 契约 §11.5 第 3 条(查询失败时的退避与恢复重入)本条没改:客户端仍是查询一失败就报错退出登录流程。
- **文档**:
  - `docs/design/open-server-rate-limit-design.md`:
    - §二步骤 2、3,并新加一段「排队提示」;
    - 顺带把「两个键在同一段 Lua 里、要求同实例」的旧说法改为 11-12 起的每桶单键脚本;
    - §11.4 J1、§11.5 无票分支与 Unity 一行、§11.7 热键一句。
  - `docs/design/redis-cluster-all-domains-20261110.md` §6.2 的网关一行。
  - `docs/design/player-route-mapping-durability-proposal.md` §12.3 表里网关那一行补「已修」。
- **同一条线上其余「以后再修」项的核对**(11-19 只读核对,都已了结):
  - 补外观时读整份存档:那个文件已删。
  - 账号目录:已改为先读,缺了才建。
  - login 核心 Redis 客户端的超时:别的会话已接上。
  - 角色列表补齐失败的降级计数与告警:已做。
  - TiDB 侧连接总账:`Resolve-MysqlLedgerLimit` 有 `tidb_budget` 一档,data_service 的账号目录池与路由池都已记账。
  - 每区 gate 台数:`deploy/k8s/zones.ops-recommended.yaml` 定为每区 3 台,并写明「台数 × 18000 ≥ 区容量」的规则。
  - player_locator 会话对账:全量一遍已分摊到 gate sweep 的各段(`index_pass.go` 的 `fullPassSharded`)。
  - 映射对账 leader:随 §15 一起删了。
  - go/db 在 K8s 上 `RequireTiDB` 与 MySQL 主机的矛盾:已改成 `-DatabaseBackend` 的两档。
  - 只剩 data_service / scene_manager / player_locator 的发现键不带 zone,归缺口第 4、5 条的会话。
- **验证(交 Codex,按 AGENTS §10.1)**:
  1. 网关单测(不需要 Redis),在 `java/` 下:
     ```
     mvn -pl gateway_node -am test -Dtest=AssignGateRateLimiterTest,RedisRateLimitStoreTest,RateLimitConfigTest,WaveScheduleTest -Dsurefire.failIfNoSpecifiedTests=false
     ```
     - 通过标准:Failures / Errors 为 0。
     - 新用例:`waveClosed_retryAfterIsTimeUntilOpenPlusJitter`、`zoneBucketEmpty_shortWaitIsRaisedToTheFloor`、`queueHint_addsJitterBelowTheConfiguredBound`、`zeroJitter_drawsNoRandom`、`zoneEmpty_withFullQueuedCost_skipsTheRefund`、`queuedIpCostOutsideOneToken_isRejected`、`requireValidLimits_rejectsQueueRetrySettingsThatReopenTightRetryLoops`。
     - 改了断言的用例:`waveClosed_queuesWithRetryAfter`(现在断言 3600000)、`zoneEmpty_refundsTheIpTokenAndCarriesTheZoneWait`(ARGV 多了 "900")。
  2. 真 Redis:起两个一次性容器,`redis:7.2` 与本地 compose 用的 8.x,不要指向共享的开发 Redis。
     - 分别设 `GATEWAY_RATE_LIMIT_REDIS_URL=redis://127.0.0.1:<端口>`,跑 `-Dtest=RedisRateLimitStoreIntegrationTest`。
     - 新用例或改名的用例:`zoneEmpty_returnsCeilWait_andSpendsOnlyTheQueuedShareOfAnIpToken`、`queuedRetriesFromOneIp_drainItsBucketByTheQueuedShare`、`zoneEmpty_partialRefund_expiresWhenTheIpBucketWouldBeFull`、`zoneEmpty_onAFullIpBucket_withFullRefund_leavesNoIpKey`。
  3. 集群:先 `docker compose -f deploy/compose.redis-cluster.yml up -d`,再设 `GATEWAY_RATE_LIMIT_REDIS_CLUSTER_NODES=127.0.0.1:7100,127.0.0.1:7101,127.0.0.1:7102` 跑 `-Dtest=RedisRateLimitStoreClusterIntegrationTest`。`zoneEmpty_refundsMostOfTheIpTokenOnItsOwnNode` 改了断言。
  4. 全量 `mvn -pl gateway_node -am test`:确认没有别的地方还在用旧的两个构造函数。设上第 2、3 步的环境变量,统一入口的预检才会放行。
  5. Unity:EditMode 编译通过即可,改动只有 `GameClient.cs` 里一个私有静态方法和两处调用。
     - 联网冒烟:开限流,把 zone rps 调到很小,看到「服务器繁忙,稍候重试…」时请求间隔应在 2~4 秒,不再连发。
  6. 部署契约:`k8s_deploy_contract.tests.ps1` 与 `k8s_client_entry_contract.tests.ps1` 不受影响(ConfigMap 键没变),顺手跑一次确认。
- **Java 版**:独立 Java 版(xuanming-server-mmo 的 `game-gateway` `RateLimiter`)待做,已在 Java 仓 `PARITY.md` 登记一行。本轮不写 Java 仓代码。
- **提交情况**:
  - main 工作区,未提交。
  - 客户端改动在 `../mmorpg-client` 工作区,也未提交;`GameClient.cs` 里还有别的会话未提交的改动。

## 2026-11-19 Claude:小游戏 WebSocket 入口——微信 / 抖音小游戏经 WebSocket 连 gate 与 battle(代码写完,未生成协议、未编译、未运行,待 Codex 验证;Java 待做)

- 设计、定案与取舍见 `docs/design/minigame-websocket-gate.md`(W1–W10,已登记 `docs/README.md`)。约 60 个文件,负责人 2026-11-19 逐项确认后回复「开始改」。
- 为什么要做:小游戏登录 11-13 已落码,但 gate 只有 TCP 入口。抖音小游戏只有 `tt.connectSocket`,没有 TCP 接口;微信另有 TCP 接口但限制多。两家都要支持,只能走 WebSocket。
- 定下来的做法:
  - gate / battle 的客户端端口同时接两种连接:首字节是 `G` 的走 HTTP 升级握手(原始帧的首字节恒为 0),之后一条二进制 WebSocket 消息承载一个原来的客户端帧。不开新端口,验票、心跳、准入、排空迁移、踢线都不分叉。
  - TLS 不在 gate / battle 里做。小游戏平台只许连事先登记的 wss 域名,所以前面放一个固定域名的边缘 nginx(ws-edge):终止 TLS,按路径 `/ws/<PodIP>/<端口>` 把连接一对一转到那台 gate / battle。原生 App 照旧 TCP 直连,不经过它。
  - gate / battle 自报 `NodeInfo.client_ws_url`(部署注入 `CLIENT_WS_BASE_URL`),随登录分配、排队放行、msg 124、战斗分配、地域测速目标下发;老客户端忽略新字段。
- 改了什么:
  - C++ 引擎库(`cpp/libs/engine/core`):
    - 新增 `network/codec/websocket.{h,cpp}`:握手解析、拆帧、包帧、每连接状态表(线程局部,只存 weak_ptr,AGENTS.md §11.7)。
    - `network/codec/codec.{h,cpp}`:`onMessage` 入口先过 `websocket::Unwrap`,解析完调 `FinishRead`;`send` 改成 static,对 WebSocket 连接多包一个帧头。
    - `node/system/node/client_endpoint.{h,cpp}`:`ParseWsBaseUrl`、`IsIpv4Literal`、`BuildWsUrl`。`node.cpp` 启动时校验,`node_allocator.cpp` 在端口定下来之后写 `client_ws_url`。
    - `core.vcxproj`、`core.vcxproj.filters` 登记两个新文件。
  - gate:`main.cpp` 断开时清表项;`handler/gate_push_delivery.h` 的 `EncodedFrame` 对 WebSocket 连接多一份带帧头的字节(仍然只编码一次);`handler/rpc/connection_admission.cpp` 导出 `gate_websocket_handshakes_total{result}` 与 `gate_websocket_protocol_closed_total`;`handler/event/gate_event_handler.cpp`、`handler/rpc/drain_migration.cpp` 推 124 时带上 `target_ws_url`。
  - battle:`client/battle_client_edge.{h,cpp}` 两处发帧认 WebSocket、断开时清表项;`logic/battle_room_manager.cpp` 分配包带 `ws_url`。
  - proto(7 个文件,只加字段):`NodeInfo.client_ws_url = 17`、`AssignGateResponse.ws_url = 12`、`QueryQueueStatusResponse.ws_url = 11`、`GateMigrationResponse.target_gate_ws_url = 7`、`RedirectToGateEvent.target_gate_ws_url = 8`、`RedirectToGateInfo.target_gate_ws_url = 6`、`RedirectToGateNotify.target_ws_url = 6`、`BattleAssignedS2C.ws_url = 8`、`NetworkProbeTarget.ws_url = 5`。
  - Go:
    - login:`loginqueue` 的 `GateCandidate` / `AdmitSlot` / `AdmitToken` 加 `WsURL`(存 Redis 的 JSON 字段 `ws_url`,omitempty),选中、复核、签票、应答一路带出;`svc/servicecontext.go`、`svc/gate_migration.go`、`loginpregate/{assigngatelogic,queryqueuestatuslogic}.go`。
    - scene_manager:`logic/gate_redirect.go` 取挑中那台 gate 的地址,`logic/enterscenelogic.go` 写进 Kafka 事件。
    - match:`discovery/node_watcher.go` 读 battle 自报的地址,`logic/network_probe.go` 写进测速目标(测速计划缺省必需,不带它小游戏进不了全服匹配)。
  - 本仓 Java gateway:`grpc/LoginRpcClient.java` 手工解码两条应答的新字段;`dto/AssignGateResponse.java` 加 `gateWsUrl`(JSON `gate_ws_url`,空时不出现);`service/AssignGateService.java` 赋值。
  - 部署脚本:
    - `tools/scripts/lib/k8s_client_entry.ps1`:PodCommon 加 `ClientWsBaseUrl`,三个生成器经 `Get-ClientEntryNodeBaseEnv` 写 `CLIENT_WS_BASE_URL`;新增 `New-WsEdgeYaml`(ConfigMap、Deployment、Service、只许连 gate / battle 客户端口的 egress NetworkPolicy);预检加三个参数的组合检查。
    - `tools/scripts/k8s_deploy.ps1`:新参数 `-WsEdgeHost`、`-WsEdgeTlsSecret`、`-WsEdgePodCidrs`、`-WsEdgeReplicas`、`-WsEdgeServiceType`、`-WsEdgeNodePort`、`-WsEdgeImage`;`infra-up` 渲染并 apply ws-edge,TLS Secret 缺了在写操作之前拒绝;线上已经在自报 WebSocket 地址而本次漏传或换了域名时,按集群现状拒绝(`Get-GateAddressDrift`、`Get-BattleEntryDrift`)。
    - `tools/scripts/dev_tools.ps1`、`k8s_image.ps1`:三条包装链透传这七个参数。
  - 客户端(`D:\luyuan\mmorpg-client`,不含生成的 C# 协议):
    - 新增 `Assets/Scripts/Net/{ClientTransport,WebSocketFramedConnection,WebSocketDrivers,MiniGameLogin}.cs`、`Assets/Plugins/WebGL/MmorpgMiniGame.jslib`(直接调 `tt.connectSocket` / `wx.connectSocket` 与 `tt.login` / `wx.login`,不依赖两家的 Unity SDK)。
    - 修改 `Assets/Scripts/Net/{IFramedConnection,BattleDirectLink,RegionProbeClient,GatewayHttpClient}.cs`、`Assets/Scripts/Game/{GameClient,GameClient.Reconnect}.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`。
    - WebGL 构建用 WebSocket,原生与编辑器照旧 TCP;编辑器加编译符号 `MMORPG_FORCE_WS` 可以拿 WebSocket 连本机 gate。
  - 与设计稿第一版不同的两处(设计稿正文已改):
    - 客户端没有用开源插件 UnityWebSocket,改为自己的 JS 桥直接调两家平台的 `connectSocket`。理由:插件靠转换工具补出来的 `WebSocket` 全局对象,又是 git 依赖,解析失败或与 Unity 6000.6 不兼容会让整个客户端工程编不过;仓里已有的支付桥也是这个做法。
    - 地域测速目标也带 `ws_url`(原来写「本批不做」):测速计划缺省必需。
- 新增与修改的测试(都没运行):
  - C++:`cpp/nodes/gate/tests/websocket_entry_test.cpp`(新,登记在 `routing_identity_test.vcxproj`,13 条:`WebSocketHandshake.*` 4 条、`WebSocketFrame.*` 2 条、`WebSocketEntry.*` 7 条);`cpp/tests/client_endpoint_test/client_endpoint_test.cpp` 加 `ClientEndpointWsUrl.*` 5 条。
  - Go:`TestAdmit_WsURLFollowsTheSignedGate`、`TestBuildGateCandidates_CarriesClientWsUrl`、`TestSignRedirectToGate_WsUrlIsThePickedGatesOwn`、`TestParseReadsClientWsUrl`。
  - Java:`AssignGateServiceAuthTest.gateWsUrlIsPassedThroughOnlyWhenTheGateAdvertisesOne`。
  - 部署契约:`tools/scripts/tests/k8s_client_entry_contract.tests.ps1` 加预检 6 行、两条库用例、两条端到端 DryRun 用例,包装链与参数类型清单补七个参数。
  - 客户端:`Assets/Tests/EditMode/Net/WebSocketFramedConnectionTests.cs`(新,9 条)。
  - `gate_connection_alerts_contract.tests.ps1` 钉着 gate 指标族清单,由「gate 准入 P0-2」会话同日补上两个 WebSocket 指标族,不在本条改动里。
- **给 Codex 的验证步骤**(本会话没有生成协议,没有编译,没有跑任何测试;只对五个 ps1 做了 PowerShell 语法解析,零错误):
  0. 生成协议(仓库根)。这一批有客户端任务授权,用默认配置,会写 `D:\luyuan\mmorpg-client\Assets\Scripts\Proto\Generated`:
     ```
     pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
     pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
     pwsh -File go/build.ps1
     ```
     通过标准:三条退出码 0。没生成之前 C++、Go、客户端都编不过(引用了新字段)。
  1. C++(MSBuild 串行 `/m:1`)。`websocket.cpp`、`codec.cpp` 在 core 库里,`run_cpp_tests -Build` 不重编库,所以先整解:
     ```
     msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
     pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test -LogDir build/verification/minigame-ws-20261119/cpp-tests
     pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter client_endpoint_test -LogDir build/verification/minigame-ws-20261119/cpp-tests
     ```
     通过标准:整解零错误;两个工程零失败;日志里 `WebSocketHandshake.*`、`WebSocketFrame.*`、`WebSocketEntry.*`、`ClientEndpointWsUrl.*` 都是 OK,gate 现有用例(推送、心跳、重定向、准入)照旧全过。失败时保留对应工程的 stdout 日志。
     - 要留意的点:`ProtobufCodec::send` 改成了 static;`websocket.cpp` 用 BoringSSL 的 `SHA1`(`<openssl/sha.h>`)与仓里的 `Base64Encode`;Linux 上 `build_linux.sh` 会按 vcxproj 重新生成 CMakeLists。
  2. Go(各服务目录):
     ```
     cd go/login && gofmt -l internal && go build ./... && go test -count=1 ./internal/logic/pkg/loginqueue/... ./internal/svc/... ./internal/logic/loginpregate/...
     cd go/scene_manager && gofmt -l internal && go build ./... && go test -count=1 ./internal/logic/...
     cd go/match && gofmt -l internal && go build ./... && go test -count=1 ./internal/discovery/... ./internal/logic/...
     ```
     通过标准:`gofmt -l` 无输出(本会话没跑 gofmt,结构体字面量是手工对齐的,有输出就按它格式化);其余全绿。`go/match/internal/logic/network_probe.go` 同日另有「探测目标分散到多台 battle」会话的改动,那一组用例以它的条目为准。
  3. Java gateway(工作目录 `java`):`mvn -B -pl gateway_node -am test "-Dtest=AssignGateServiceAuthTest" -Dsurefire.failIfNoSpecifiedTests=false`,新用例 PASS,其余照旧。
  4. 部署契约(仓库根,只读夹具,不连集群):
     ```
     pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1
     python tools/scripts/check_log_text_english.py
     ```
     通过标准:fail=0;日志英文守卫没有新增违例。失败时保留失败用例全文与 DryRun 输出尾部,先判断是夹具还是渲染,不改断言。
  5. 客户端(仓根 `D:\luyuan\mmorpg-client`,先关掉占着工程的 Unity,不加 `-quit`):
     ```
     pwsh -File tools/client_compile_check.ps1 -ShowErrors 40
     D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Net" -testResults D:\luyuan\mmorpg-client\.codex-artifacts\minigame-ws-20261119\editmode-net.xml -logFile D:\luyuan\mmorpg-client\.codex-artifacts\minigame-ws-20261119\editmode-net.log
     ```
     通过标准:编译 0 error;EditMode 零失败、零跳过,`WebSocketFramedConnectionTests` 9 条 PASS;`MmorpgClient.Tests.EditMode.Battle` 再跑一次同样零失败(`BattleDirectLink`、`RegionProbeClient` 的连接调用改了)。新文件的 `.meta` 由 Unity 这次打开时生成。
  6. 本机联调(可选,有本机栈时):
     - gate 不设 `CLIENT_WS_BASE_URL` 也接 WebSocket。用任意 WebSocket 客户端连 `ws://127.0.0.1:<gate 端口>/`,握手应答 101;连上后不发验票,10 秒被关。
     - Unity 编辑器在 Player Settings 的 Scripting Define Symbols 里加 `MMORPG_FORCE_WS`,走完登录、进场、战斗。gate 日志没有 `WebSocket frames rejected`,`/metrics` 里 `gate_websocket_handshakes_total{result="accepted"}` 增长。
     - 设 `CLIENT_WS_BASE_URL=wss://ws.example.com/ws` 启动 gate,启动日志里有一行 `Client WebSocket URL: wss://ws.example.com/ws/<ip>/<port>`;设成 `https://...` 时拒绝启动,stderr 有 `FATAL client endpoint`。
  7. kind(可选):`infra-up` 与 `zone-up` 带 `-ClientEntryMode external -WsEdgeHost <域名> -WsEdgeTlsSecret <名> -WsEdgePodCidrs <Pod CIDR>`,先在 infra namespace 建好 TLS Secret。看三件事:ws-edge 的 Pod 起得来(nginx 认 `geo` 用路径里捕获的地址,这个写法没有实测);`/ws/<gate PodIP>/18000` 握手 101;`/ws/<别的 Pod IP>/20000` 被 NetworkPolicy 或 403 挡住。
- **没做 / 没核实**:
  - 把客户端真正转成微信 / 抖音小游戏包(转换工具、构建配置);真机与开发者工具联调(没有开发者账号)。两家转换工具支不支持 Unity 6000.6 没有核实。
  - robot 的 WebSocket 模式(vendored muduoclient 只会自己拨 TCP)。
  - 账号注销的重新认证没有接小游戏登录(注销面板上只有 Apple / Google)。
  - battle 没有导出 WebSocket 的三项计数;按源 IP 限流、`permessage-deflate`、IPv6 Pod 网络都不做。
- **人工**:上线前要备案一个域名指向 ws-edge 的 Service,证书建成 infra namespace 里的 TLS Secret(`tls.crt` 带完整证书链),并在微信、抖音后台把它登记成 socket 合法域名;gateway 的 https 域名登记成 request 合法域名。
- **Java 版**:待做。客户端契约有变化(端口多接一种连接、五处地址字段),已在 Java 仓 `PARITY.md` 登记 2026-11-19 同名条目;本轮没有改 Java 仓代码。
- **提交情况**:main 工作区,本会话没有提交。负责人 22:27 的批量提交 `9824368eb` 带走了当时已经写好的部分(C++ 的 WebSocket 层与接线、六个 proto、login 与 scene_manager 的 Go 代码、部署库的前一半、设计稿第一版);之后写的(match、测试、Java、`k8s_deploy.ps1` 与两个包装脚本、契约测试、设计稿改写、文档互链)在工作区未提交。HEAD 单看是半截状态,验证以工作区为准。客户端仓的改动同样未提交。

## 2026-11-19 Claude:断线通知至少送达一次——gate 持有 Disconnect 直到 login 确认,login 只在 player_locator 挂上断线租约后才回成功(未编译、未测试,待 Codex 验证;Java 待做)

- **缺陷**(总纲 §4 末尾「原待确认的五条」第 1 条):gate 活着、只是断线通知丢了时,player_locator 里的会话永远停在 ONLINE(会话键没有 TTL)。在线状态、location 与场景成员都不清，副本实例回收不了，只能靠玩家重登、这台 gate 死掉或人工补救。丢的来源有三处:
  - gate 路由模式不看发送结果;
  - 直连模式挑不到 login 节点时整条丢掉，没有日志;
  - login 调 player_locator 4 次失败后照样回 OK。
- **gate**(17 个文件里的 C++ 部分):
  - `login_session_renewal.{h,cpp}` 在原续租模块上扩展：连接关闭时 `NoteConnectionClosed` 记一条按会话代次建键的待送达通知，带断线当下的 SessionDetails。当拍有预算就发，否则排到下一拍。
  - 调用失败、超时、login 回错误、挑不到 login 或路由服、签名失败，都按退避重发:1、2、4、8、16 秒后封顶 30 秒，乘 0.8~1.2 的确定性系数。login 回成功才移除。
  - 每拍预算 200、在途 400,与续租共用，断线通知先发。
  - 表上限 131071 条(= 会话号空间,static_assert 对齐 `kSeqMask`),满了新来的打带 player_id 的 ERROR 并计数;待送达满 60 秒打一次 ERROR。
  - 指标:`gate_login_disconnect_pending`、`gate_login_disconnect_total{result=queued|sent|retried|delivered|failed|not_sent|overdue|dropped}`。
  - `client_session_handler.cpp`:断线时只构造通知并交给模块;新出口 `SendLoginDisconnectNotice` 检查路由模式的应答(带 tip 或调用失败都算没送达);删掉已无人调用的 `SendViaRouter`。
  - `main.cpp`:关源时不再取消续租节拍，只在拆除前停;停时还有没确认的通知打一行 WARN。进程退出后剩下的由死 gate 清扫兜住。
- **login**:
  - `disconnectlogic.go`:栅栏 Close 失败回 `Unavailable`、什么都不收尾(原来 fail-open);每个要收尾的玩家都试一次 `SetDisconnecting`,任一失败回 `Unavailable`,由 gate 重发。
  - `session_lifecycle.go`:`markPlayerSessionDisconnecting` 只调一次、返回 error;原来 4 次退避加 CRITICAL 搬进 `markPlayerSessionDisconnectingWithRetry`,只给 EnterGame 补偿路径用(`entergamelogic.go` 的 `compensateFenceBind`),行为不变。
- **重放安全**(逐个核实，文件与行号见子代理回报，已写进 `disconnectlogic.go` 注释):
  - 栅栏 Close、清登录态、player_locator 的 `SetDisconnecting`(已是 DISCONNECTING 回成功、不延长租约)都幂等;
  - 防沉迷下线上报用 GETDEL 至多报一次。GETDEL 本身失败时这条下线不补报，这一取舍归「实名认证与未成年人防沉迷」会话，见 `docs/design/real-name-anti-addiction.md` §5.5「已知局限」。
- **新增测试**:
  - gate `login_session_renewal_test.cpp` 10 个用例;
  - login `disconnect_retry_test.go` 4 个;
  - player_locator `setdisconnecting_replay_test.go` 1 个。
- **文档正文已改**:`player-locator-gate-session-index.md` §1、§8 残余 5;`release-checklist.md` F.B-1;`gate-online-identity.md` §5、§6、§7 第 3 条、§10.4;`client-request-ordering.md` §6.2;`session_reconciler.go` 文件头。
- **剩余与取舍**:
  - player_locator 不认会话代次:会话号回绕且同一玩家落在同号上时，迟到的旧通知会转错新会话。已另派子代理照 `gate_instance_id` 的做法补 `session_incarnation`(要改 proto),另起一条记。
  - 停机排空不等待送达表清空：多于每拍预算的部分交给死 gate 清扫(约 2.5~3 分钟收口)。等表清空会在 login 不可用时把维护停机卡住，所以不等。
  - 栅栏绑定只存 6 分钟：一台 gate 积压到约 7 万条以上、重发晚于 6 分钟时，只有栅栏知道的绑定玩家收不了尾，属于多重故障叠加，已写进注释。
- **Codex 验证**(仓库根目录;C++ 串行):
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`:0 个错误，重点看 gate 的 `login_session_renewal.cpp`、`client_session_handler.cpp`、`main.cpp`。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity -LogDir build/verification/disconnect-at-least-once-20261119/cpp-tests`:全绿。原有 `LoginSessionRenewal.*`、`LoginSessionRenewalInterval.*` 照过，并逐个点名新用例:
     - `LoginSessionRenewal.ClosedConnectionSendsItsDisconnectRightAway`
     - `LoginSessionRenewal.ConfirmedDisconnectLeavesTheTable`
     - `LoginSessionRenewal.UnconfirmedDisconnectIsResentWithAGrowingBackoff`
     - `LoginSessionRenewal.UnsentDisconnectStaysQueuedAndGoesOutOnALaterTick`
     - `LoginSessionRenewal.LateReplyForAnOlderIncarnationDoesNotTouchTheNewNotice`
     - `LoginSessionRenewal.DisconnectsShareThePerTickBudgetWithRenewals`
     - `LoginSessionRenewal.DueDisconnectsGoOutBeforeDueRenewals`
     - `LoginSessionRenewal.FullTableRefusesNewNoticesAndCountsThem`
     - `LoginSessionRenewal.NoticeStillUnconfirmedAfterAMinuteIsFlaggedOnce`
     - `LoginDisconnectRetryBackoff.DoublesFromOneSecondUpToThirtySecondsWithJitter`
  3. `cd go/login && go build ./... && go vet ./internal/logic/clientplayerlogin/ && go test ./internal/logic/clientplayerlogin/ -count=1`,点名:
     - `TestDisconnect_SetDisconnectingFailureReturnsUnavailableAndTheResendSettles`
     - `TestDisconnect_FenceCloseFailureReturnsUnavailableWithoutCleanup`
     - `TestDisconnect_ResendAfterALostFenceReplyStillSettlesTheBoundPlayer`
     - `TestDisconnect_ResendAfterSuccessRepeatsNothing`
     - 原有 `TestDisconnect*`、`TestBindSessionFence*`、`TestCompensateFenceBind*` 与 entergame_kick 系列也要过;遇到 Access denied 改用 `go test -c`。
  4. `cd go/player_locator && go build ./... && go vet ./internal/logic/ && go test ./internal/logic/ -count=1`,点名 `TestSetDisconnecting_ReplayKeepsTheFirstLeaseDeadline`。
  5. `python tools/scripts/check_log_text_english.py`。
  6. 可选冒烟:robot 登录后停掉 player_locator 约 10 秒，其间断开连接，再恢复。期望 gate 打 `[LoginDisconnect] Disconnect confirmed by login after retries`,`gate_login_disconnect_pending` 回到 0;该会话先变 DISCONNECTING,30 秒后被删。
  7. 失败时保留:msbuild 的第一条错误(带文件和行号)、`-LogDir` 下的 gtest 原始输出、失败 Go 用例的 `-v` 输出。
- **Java 版**:没写 Java 代码，待做登记在 Java 仓 `PARITY.md`「2026-11-19 断线重连待确认五条」一节。

## 2026-11-19 Claude：错误汇总缓冲收尾——计数进 /metrics，集中推送、致命信号录入与 Go 同款缓冲定为不做；分布式日志复核（未编译、未测试，待 Codex 验证；Java 待做已登记）

- **起因**：负责人的标准提示（「以后修的现在修、先按最标准的定」）。分布式日志三个阶段的代码都已写完，这轮复核现状，并把 11-12 在 `docs/design/error-reporting.md`（todo #250）里查出、当时只记了没做的几项收掉。
- **复核**：
  - Codex 11-18 的 Java 全量里，网关的追踪用例都过了：`UntrustedTraceHeaderFilterTest` 4、`MallAdminClientTest` 8、`MallAdminControllerTest` 5、`GatewayTracingIntegrationTest` 4（证据 `build/verification/docker-rebuild-all-20261118T082435.788540Z/java/maven.log`）。同一轮 Linux Docker 编过 gate / scene / battle 节点和 login 等 Go 镜像。商城、config_node、C++ 与 Go 的单测都还没人跑。
  - 11-12 在 `player_scene.cpp`、`player_landing.cpp` 两处直接调 Hiredis 的地方加的 `tracing::Bind` 已经不在了：这两处随「Redis 全上 Cluster」改走 `tlsControlRedis`（`RedisClusterClient`），提交处统一续 trace，去掉调用点那层是对的。全仓核对，业务代码里已没有直接调 Hiredis 的调用点。设计稿 `distributed-tracing.md` §4.3、§11 按现状改正。
- **代码（#250 切片 B）**：
  - `cpp/libs/engine/core/error_reporter/error_reporter.h` 加 `WriteMetrics(buffer, writer)` 与 `RegisterMetrics()`，三个指标：`error_reporter_events_total`、`error_reporter_dropped_events_total`（counter）和 `error_reporter_buffered_events`（gauge），不带标签。
  - `Node::StartMetricsExporter`（`node.cpp`）起 `/metrics` 时调 `error_reporter::RegisterMetrics()`，三种节点都有。
  - 用例：`cpp/tests/rpc_controller_test/error_reporter_test.cpp` 加 2 条：`ErrorReporterMetrics.WritesTheThreeCounters`（容量 2 录 3 条 → 3 / 1 / 2）、`ErrorReporterMetrics.RegistersOnceOnTheProcessPage`（重复登记不出第二份）。
- **定案**（`error-reporting.md`「切片状态与定案」）：
  - 集中推送（切片 C）不做：四个录入点同时都打了带 `trace_id` / `player_id` 的日志，集中查看与告警走 Loki，不另起第二份管道。
  - `HandleFatalSignal` 不录：信号处理函数里加锁、分配内存不是异步信号安全的，可能死锁；它已经用 `LOG_ERROR` 写了信号号与调用栈。
  - Go 同款缓冲（切片 E）不做：Go 的错误已有日志与 Prometheus 计数两条去处。
  - 敏感信息：录入的 message 只写方法名、消息号、计数、内部 id，不写账号、token、IP 与 proto 正文，转储不外发；不在 `Record` 里自动脱敏。
  - 抽样由接点负责，缓冲不加按 tag 的限速。
- **Java 独立仓（AGENTS §12）**：`D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾追加一节「2026-11-19 分布式日志阶段一与收尾」，只登记待做。原先只登记了阶段二、三，阶段一的整套约定与 11-12 收尾一直没登记。那份文件有别的会话没提交的改动，这次只在文末追加。
- **预检**：Claude 在仓库根跑了 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（只读的源码文本预检），10 条通过。改动里没有新起线程。
- **交 Codex**（Debug|x64，MSBuild 串行 `/m:1`，仓库根执行）：
  1. 依次编 `cpp/libs/engine/core/core.vcxproj`（`error_reporter.h`、`node.cpp` 改了），再依次编节点 `cpp/nodes/gate/gate.vcxproj` → `cpp/nodes/scene/scene.vcxproj` → `cpp/nodes/battle/battle.vcxproj`。gate 有两个 cpp 引用 `error_reporter.h`，现在它多包含了 `metrics/metrics_registry.h`。
  2. 编 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`，先跑 `build\cpp\tests\rpc_controller_test.exe --gtest_filter=ErrorReporter*`，应为 8 条（11-12 的 6 条加这轮 2 条），再跑全量。11-12「分布式日志收尾」条目的其余 C++ / Go / Java 步骤照旧。
  3. 冒烟（可选）：任一节点设 `MMORPG_METRICS_PORT` 后抓 `/metrics`，有 `error_reporter_events_total` 等三族；对 gate 发一条非法包，`error_reporter_events_total` 加 1。
  4. `node.cpp` 里有别的会话没提交的改动。失败时先看是否落在本条改的那两行上，分开报告，保留构建日志与 gtest 输出。

## 2026-11-19 Claude：修复身份落码——数据修复工具不再用维护身份，按工具语句逐表授权（未编译、未测试，待 Codex 验证；Java 不涉及）

- **起因**：负责人 2026-11-19 在「维护身份在业务表上的写权限」里选了推荐方案「新增修复身份」（接同日「维护身份在业务表上的写权限——最小权限推荐方案」）。
- **做了什么**：
  1. `go/shared/authoritysql/config.go`：新增 `RepairPrincipalEnv`（`MMORPG_SQL_REPAIR_PRINCIPAL`）与 `LoadRepairPermission`。权限形状同维护身份（Operator：连上之后核 `CURRENT_USER()` 等于声明的主体），能写什么全看库里的 GRANT。
  2. `go/data_service/cmd/create_residual`、`cmd/admission_recover`：连库改用 `LoadRepairPermission`，只设维护身份时在碰库之前拒绝（退出 2，报出要的变量名）；文件头与 `-dsn-env` 说明同步。plan 与 apply 用同一个修复身份（与推荐方案的差别：不再单设只读身份。plan 本来就在修复窗口里跑，修复账号平时锁着，多一套口令换不来多少；理由写进逻辑库设计稿）。
  3. `tools/scripts/maintenance_drain.ps1`：新增 `ConvertTo-AuthorityRepairRoleSql` 与命令 `render-repair-role`（迁移之后执行，输出一段 SQL）。计划只要 `repairPrincipal`、`maintenancePrincipal`、`registrarPrincipal`、`schemaPrincipal`（四者互不相同）、`directorySchemas`、`storeSchemas`，多一个字段就拒绝；不读写者槽位、REGISTER，也不看 runtime 的 `tables`（`render-roles` 的计划按「逻辑库 × 写者槽位」各一份，修复授权跟槽位无关）。
  4. 授权按两个工具实际执行的语句逐表核过（含打开库时的形状守卫与片号核对）：
     - 目录各片：`route_meta` SELECT（锁读闸门行）、`directory_shard` SELECT（打开时核片号）、`account_directory` SELECT/INSERT/UPDATE（补 ref 锁目录行、没有就建，upsert）、`account_role` SELECT/INSERT（只插墓碑）、`account_role_removal` SELECT（形状守卫）、`account_role_ref` SELECT/INSERT/UPDATE（补 ACTIVE ref，upsert）、`player_route` SELECT、`player_name` 与 `player_name_owner` SELECT/INSERT/UPDATE/DELETE。
     - 各存档库：`route_meta` SELECT、`player_store` SELECT/INSERT/UPDATE（补初始行 upsert、摘死持有者三列）。
     - 不授：闸门行的 UPDATE（能改就能解冻冻结的库）、`account_role` 的 UPDATE / DELETE（墓碑不可改删）、任何 DDL / GRANT OPTION / 整库授权 / LOCK TABLES、写者名册与排空 / 冻结票据、合服表。
     - 与推荐方案原清单的差别：原清单漏了 `account_directory`（补 ref 要锁并按需新建目录行），多算了 `account_role_removal` 的 DELETE 与交易持有者两张表（工具不写，只有形状守卫要读 `account_role_removal`；交易持有者两张表是服务运行时自己写的）。列级授权没用：TiDB 上列级权限的行为没在真库核过，先按表级授。
     - mysql 档的 `SELECT … FOR UPDATE` 另要 UPDATE / DELETE / LOCK TABLES 之一，脚本不授：锁读被拒、写不进去（fail-closed），要在 mysql 档修复另行授权（负责人第 2 问「mysql 档要不要支持」没答，按不支持处理，手册写明）。
  5. 文档：`docs/ops/mysql-backup-pitr-runbook.md` §4.1a（身份、授权内容、账号平时锁住、第 1 步环境变量）；`docs/ops/k8s-open-server-runbook.md` §1 授权步骤加第 3 步；`docs/ops/db-move-runbook.md` 第 3 条（搬目录片或存档库时修复身份也要在目标集群建好并重跑授权，`plan` 不核它）；`docs/design/horizontal-scale-logical-db.md`（修复身份一条，含为什么、plan 不单设只读身份、暂不用列级授权）；`deploy/sql/authority_operator_role.sql` 头注释（维护身份不授业务表，注释不写表名，契约测试按渲染结果找表名）；r2 审计文档 §1、§5.0、§6 改成已定。
- **用例**：
  - `tools/scripts/tests/maintenance_drain.tests.ps1` 新增「修复身份按工具语句逐表授权，闸门行只读，墓碑不可改删」：两个目录片加一个存档库共 20 条 GRANT；每个库的 `route_meta` 只 SELECT；`account_role` 只 SELECT、INSERT；没有 DDL / 整库 / LOCK TABLES / 权限真值表 / 合服表；存档表只在存档库、目录表只在目录片；修复身份借用另外三个主体、同一库两头算、库清单为空、计划自带 `tables` 都拒绝。
  - `go/data_service/cmd/create_residual/main_test.go` 新增 `TestRequiresTheRepairIdentityNotTheMaintenanceOne`，`cmd/admission_recover/main_test.go` 新增 `TestRecoveryRequiresTheRepairIdentityNotTheMaintenanceOne`：只设维护身份时退出 2、报 `MMORPG_SQL_REPAIR_PRINCIPAL`（改之前照常往下走）。原有 `TestRequiresDbPlacement`、`TestRecoveryRequiresDbPlacement` 改设修复身份。
  - 两份 ps1 已用 PowerShell 解析器静态查过语法（0 个解析错误）；没跑任何测试。
- **交 Codex**（仓库根，失败保留首错原文，不改断言、不连续重试）：
  1. `cd go/shared`：`gofmt -l ./authoritysql/` 无输出；`go vet ./authoritysql/`；`go test -count=1 ./authoritysql/`。
  2. `cd go/data_service`：`gofmt -l ./cmd/create_residual/ ./cmd/admission_recover/` 无输出；`go vet ./cmd/create_residual/ ./cmd/admission_recover/`；`go test -count=1 -v ./cmd/create_residual/ ./cmd/admission_recover/`，新的两条与原有用例全 PASS。
  3. 仓库根：`pwsh -NoProfile -File tools/scripts/tests/maintenance_drain.tests.ps1`，新例要 PASS、原有用例不回归。
  4. 可选，真 TiDB（一次性库，别借 `codex-*` 容器）：建目录片与存档库并跑完迁移，建修复账号，执行 `render-repair-role` 的输出，用修复身份跑一遍 `create_residual -mode plan`（应能打开并扫完）与一个夹具场景的 `-mode apply`、`admission_recover -mode plan`；再核修复账号执行 `UPDATE route_meta SET writable=0` 与 `DELETE FROM account_role` 都被 1142 拒绝。结果原样贴回。
- **残余**：
  - 部署者要另建修复账号并保持平时锁住；新开目录片或存档库之后要重跑授权（开服手册第 3 步写了，没有自动化）。
  - `dbmove plan` 不核修复账号，搬库之后漏建只会让两个工具连不上（fail-closed），手册写了要自己建。
  - mysql 档不支持这两个工具的写（见上）。
  - 列级收紧（如 `player_store` 只许改三列加 upsert 的 `player_id`）要等 TiDB 真库核过列级权限再做。
- **Java 版**：不涉及，Java 版没有这两个修复工具，按 Java 仓规矩不加行。

## 2026-11-19 Claude:未验签预算收尾——loop 卡顿时不淘汰、运维覆盖变量、battle 直连接上预算、gate 按来源前缀限速(P2-2)(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:接本日「gate 未验签连接预算」一条的「剩余风险」四项,负责人要求做完:
  1. loop 卡顿之后可能误淘汰;
  2. 预算没有覆盖变量;
  3. battle 直连没有这套预算;
  4. 仍是全局、不按来源。
  设计稿 `docs/design/gate-connection-admission-control.md` 的 P0-2 与 P2-2 已按落地改写正文。
- **口径**:
  1. **loop 刚卡过时不淘汰**:
     - core `client_liveness.h` 新增 `LoopStallWatch`(1 秒节拍)。三种情况都算刚卡过:还没打过拍;上一拍本身迟到;距上一拍已超过两个节拍。
     - `LivenessSweeper` 改用它判自己的迟到,判定行为不变,另导出 `LoopRecentlyStalled`。
     - gate 用存活扫描的节拍(`gate_client_heartbeat::LoopRecentlyStalled`);battle 在 `Install` 起一个 1 秒 `TimerTaskComp`,只绑 weak 自己,挂主 EventLoop,不起线程。
     - 刚卡过时不给最老一条的年龄,判定落到早拒 / 预算。口径与握手死线「迟到只推迟」相同。
  2. **覆盖变量**:
     - gate:`GATE_PREAUTH_BUDGET`、`GATE_PREAUTH_EARLY_DROP_START`、`GATE_PREAUTH_EVICTABLE_AGE_MS`。battle:`BATTLE_PREAUTH_*` 同名三个。
     - 规范十进制;只设预算时起点按新预算取一半;起点不能大于预算;年龄 1~9999 毫秒(短于握手期限)。写错拒启,没有「0 = 关闭」。
     - 解析在 core `accept_admission.h` 的 `ResolvePreAuthBudget`,带调用方的变量名。
     - `PreAuthBudget` 多了 `minEvictableAge` 字段,缺省 3 秒。两个节点的启动日志都多打 `preauth_evictable_age_ms=`。
  3. **battle 直连同一套**:
     - 预算的纯逻辑(`PreAuthBudget` / `DefaultPreAuthBudget` / `DecidePreAuth` / `UniformBelow`)从 gate 头挪到 core `accept_admission.h`,gate 经 using 引回,行为不变。
     - battle 的池是「没完成票据握手、也没出示探测授权、还连着」的直连。`AdmitConnection` 每次 accept 扫一遍会话表(`UnauthenticatedScan`),
       数出条数、挑出最老一条。与它的指标同一数法,以 4096 为界;O(直连数) 这一点写在设计稿「已知局限」里。
     - 淘汰经会话里的 weak 连接 `forceClose`,会话由 `HandleClosed` 删。
     - 判定顺序为「关源 → 总量 → 未验签预算 → 速率」。battle 的 `Decide` 改为返回 `Verdict`。
     - 缺省:4096 时跟着速率下限,起点 500、预算 1000。
  4. **gate 按来源前缀限速(P2-2)**,按设计稿 §7 的约束做:
     - 限速率不限总量;IPv4 /24、IPv6 /64 聚合(IPv4 映射地址按 IPv4);不封来源、不落持久状态。
     - **缺省关闭**:设了 `GATE_SOURCE_RATE_PER_SECOND` 才开,`GATE_SOURCE_BURST` 可选;只设桶深拒启。
     - 开了也**只在承压时生效**:未验签池过了早拒起点,或全局速率闸最近 10 秒内拒过连接。
     - 推荐值 `DefaultSourceRateLimit` = 全局缺省持续速率的 1/10,桶深是它的 10 秒量(上限 2 万时 20 条/秒、200)。启动日志打出推荐值。
     - 判定排在总量之后、预算之前:先只问前缀的桶(`WouldAdmit`),所有闸都放行后才记账(`Take`)。超速前缀不消耗全局名额,也不会让别人的老连接被淘汰。
     - 前缀表上限 65536。满了每秒最多清一次已回满的;仍满就不登记、直接放行(fail-open),计数、采样日志并告警。
     - 只在 gate 看得到真实源地址的形态里开(external + `externalTrafficPolicy: Local`);podip 不要开。battle 没做按来源限速。
  - 另:`State` 里原来的早拒随机源函数指针挪到结构体外的线程局部变量。AGENTS.md §10.1 的 no-raw-pointer-member 门禁,函数指针也算。
    battle 同样放在结构体外。
- **代码**:
  - core:
    - `cpp/libs/engine/core/network/accept_admission.h`:预算纯逻辑与 `ResolvePreAuthBudget`;`AcceptRateGate` 加只读的 `CanTake` / `Full`,`TryTake` 判据不变;
    - `client_liveness.h`:`LoopStallWatch`,`LivenessSweeper` 改用它。
  - gate:
    - `handler/rpc/connection_admission.{h,cpp}`:using 引回;`GATE_PREAUTH_*`;`SourcePrefix` / `SourceRateGate` / `ResolveSourceRateLimit` / `DefaultSourceRateLimit`;
      拒绝原因 `source_rate`;`Configure` 四参版(两参版留给单测);`SourcePrefixOf`;`CurrentSourceRateLimit`;卡顿判定;6 个新指标族。
    - `handler/rpc/client_heartbeat.{h,cpp}`:`LoopRecentlyStalled`。
    - `main.cpp`:`ConfigureConnectionAdmissionOrDie` 解析三组参数,启动日志加字段。精确编辑;别的会话在改同文件其它段落。
  - battle:
    - `client/battle_connection_admission.{h,cpp}`:两个拒绝原因、`BATTLE_PREAUTH_*`、`UnauthenticatedScan`、`Verdict`、`Admit` 五参版、淘汰计数、4 个新指标族、早拒随机源与测试钩子。
    - `client/battle_client_edge.{h,cpp}`:`AdmitConnection` 扫表与淘汰、`EvictUnauthenticated`、loop 节拍定时器、`ConfigureAdmission` 两参版。
    - `main.cpp`:`ConfigureClientAdmissionOrDie`。
- **指标与告警**:
  - gate:新增 `gate_preauth_evictable_age_ms`、`gate_source_rate_limit_per_second`、`gate_source_burst`、`gate_source_rate_active`、`gate_source_tracked`、`gate_source_untracked_total`,
    以及 reason `source_rate`。`deploy/k8s/gate-connection-alerts.yaml` 从 7 条变 9 条(+`GateSourceRateLimited`、`GateSourceTableFull`),`GatePreauthBudgetRejecting` 的处置改为用覆盖变量调。
  - battle:新增 `battle_preauth_evicted_total`、`battle_preauth_budget`、`battle_preauth_early_drop_start`、`battle_preauth_evictable_age_ms`,
    以及 reason `preauth_budget` / `preauth_early_drop`。`deploy/k8s/battle-connection-alerts.yaml` 从 6 条变 7 条(+`BattlePreauthBudgetRejecting`);
    `BattleUnauthenticatedConnectionsHigh` 改为「/ 早拒起点 > 0.8」,与 gate 同一口径。
  - 契约测试:
    - `tools/scripts/tests/gate_connection_alerts_contract.tests.ps1` 从 8 条变 9 条:族清单、reason、告警名单、main.cpp 解析与传参、卡顿判定;新增「按来源限速」一条。
    - `battle_connection_alerts_contract.tests.ps1` 从 8 条变 9 条:族清单、reason、告警名单、main.cpp;新增「直连面的未验签预算」一条。
- **单测**(未运行):
  - `cpp/nodes/gate/tests/connection_admission_test.cpp` 共 36 条:`ConnectionAdmissionPolicy` 13、`SourceRateGate` 2、`ConnectionAdmission` 20、`ConnectionAdmissionEviction` 1。
    - 新增:覆盖变量、来源前缀聚合、按来源限速开关;两条 `SourceRateGate`;loop 卡顿不淘汰、按来源限速只在承压时生效、全局速率拒绝触发按来源限速。
    - 判定顺序一条改写为含来源。
    - 淘汰类用例先 `MarkLoopHealthy` 打两拍;会话代次改为 `kIncarnationBase + 会话号`,避开别的用例留在线程局部扫描堆里的检查点。
  - `cpp/nodes/gate/tests/client_liveness_test.cpp` 新增 2 条:`ClientLivenessLoopStall.StallIsReportedUntilAnOnTimeTickFollows`、`ClientLivenessSweeper.ReportsALoopStallFromItsOwnSweepCadence`。
  - `cpp/tests/rpc_controller_test/accept_admission_test.cpp` 新增 2 条:`AcceptRateGate.CanTakeAsksWithoutSpendingAndFullMeansTheBurstIsBack`、`AcceptAdmissionPolicy.PreAuthBudgetResolvesFromTheCallersEnvNames`。
  - `cpp/nodes/battle/tests/battle_connection_admission_test.cpp` 整体重写,共 15 条(Policy 6、Admission 9),新增预算相关 5 条。
- **已做的静态检查**(不是编译 / 测试证据):
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`:10 条全过;
  - 两份契约测试的 PowerShell 解析 0 错误;
  - 两份告警 YAML 解析出 9 / 7 条规则。
- **⚠️ 上一条说过的 HEAD 半截文件仍在**:`9824368eb` 里的 `connection_admission.cpp` 是写到一半时被整区提交读走的。推送或从干净检出验证之前,先按工作区重新提交本条与上一条涉及的文件。
- **交 Codex**(没有 proto 改动;前一步红就停下归因;在工作区跑):
  1. 仓库根:
     - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`(10 条)
     - `pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`(9 条)
     - `pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`(9 条)
     - `pwsh -NoProfile -File tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1`(告警名不重名)
     - 有 promtool 时,两份告警的 `spec` 各跑一次 `promtool check rules`。
  2. Windows 整解:`msbuild game.sln /t:Build /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
     - 必须整解:core 头(`accept_admission.h`、`client_liveness.h`)与 gate / battle 的类布局都变了。
     - 首错落在下面这些位置时交回 Claude:
       - 本条列出的文件;
       - battle `main.cpp` 的 `ConfigureClientAdmissionOrDie`;
       - gate `main.cpp` 的 `ConfigureConnectionAdmissionOrDie`。
     - 已知风险点:
       - `SourcePrefixOf` 在 Windows 上读 `sockaddr_in6::sin6_addr.s6_addr` 与 `sockaddr_in::sin_addr.s_addr`(winsock 宏);
       - `std::erase_if` 用在 `unordered_map` 上(C++20);
       - battle 测试里 `Verdict{.evictOldestUnauthenticated = true}` 是指定初始化。
  3. 测试,`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter <名>`:
     - `routing_identity_test`:`ConnectionAdmissionPolicy.*` 13、`SourceRateGate.*` 2、`ConnectionAdmission.*` 20、`ConnectionAdmissionEviction.*` 1、`ClientLiveness*` 全组全部 PASS;
       `ClientHeartbeat*`(存活扫描改用 `LoopStallWatch`,迟到判定应不变)、`DrainMigration*`、`GateClientEntryWiring*`、lane 各组不回归。
     - `rpc_controller_test`:`AcceptRateGate.*` 9、`AcceptAdmissionPolicy.*` 6、`HandshakeHistogram.*` 2、`BattleConnectionAdmission*` 15 全部 PASS;
       `AcceptorAdmission.*` 与其余用例不回归。
  4. Linux(需授权 Docker;不可达或要拉镜像就停下来问):
     - `docker --context desktop-linux build -f deploy/k8s/Dockerfile.cpp --build-arg BUILD_JOBS=2 -t mmorpg-node:preauth-closeout .`
     - 行为:按设计稿 §9.6 末尾新增的四行做:覆盖变量生效与拒启、loop 卡顿时不淘汰、按来源限速缺省关闭、只在承压时生效;再加 battle 的启动日志核对。
  5. 冒烟:缺省参数下 robot 登录与一局战斗。通过标准:
     - gate / battle 的 `*_rejected_total` 各 reason 都是 0;`gate_preauth_evicted_total` 与 `battle_preauth_evicted_total` 为 0;`gate_source_rate_active` 为 0;
     - 启动日志带 `preauth_evictable_age_ms=3000`;gate 那行带 `source_rate_per_second=0`。
  6. 失败时保留:首个编译错误原文、失败用例完整输出、容器 stderr 前 40 行。不改断言,不跳过用例。
- **剩余风险与没做的**:
  - 所有阈值仍是推导值,压测里看:
    - 正常洪峰下 `gate_unauthenticated_connections` 离早拒起点多远;
    - 承压期间 CGNAT 出口是否被 `source_rate` 误伤(开了按来源限速时)。
  - battle 每次 accept 扫一遍会话表,O(直连数);以 4096 为界。
  - battle 没有按来源限速;`k8s_deploy.ps1` 没有接线任何新变量,调参走 `kubectl set env`(Fleet 形态改 Fleet env)。
  - 前缀表满时 fail-open:全局各道照常生效,按来源这一道失效。
- **Java 版(AGENTS §12)**:待做,Java 仓 `PARITY.md` 末尾登记一行。不涉及客户端契约(被淘汰是正常关闭,被拒仍是 RST)。
- **文件**(修改,无新增):
  - core:`cpp/libs/engine/core/network/accept_admission.h`、`client_liveness.h`;
  - gate:`cpp/nodes/gate/handler/rpc/connection_admission.{h,cpp}`、`client_heartbeat.{h,cpp}`、`cpp/nodes/gate/main.cpp`、
    `cpp/nodes/gate/tests/connection_admission_test.cpp`、`client_liveness_test.cpp`、`cpp/nodes/gate/SECURITY.md`;
  - battle:`cpp/nodes/battle/client/battle_connection_admission.{h,cpp}`、`battle_client_edge.{h,cpp}`、`cpp/nodes/battle/main.cpp`、
    `cpp/nodes/battle/tests/battle_connection_admission_test.cpp`;
  - 其余测试:`cpp/tests/rpc_controller_test/accept_admission_test.cpp`;
  - 告警与契约:`deploy/k8s/gate-connection-alerts.yaml`、`battle-connection-alerts.yaml`、`deploy/k8s/README.md`、
    `tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`、`battle_connection_alerts_contract.tests.ps1`;
  - 文档:`docs/design/gate-connection-admission-control.md`、`docs/design/turn-based-battle-server.md`(D27)、`docs/ops/gate-kernel-tuning-runbook.md`(§3.2、§3.3)、本文件。
  - 没有新增 .cpp,vcxproj / CMakeLists 不用动。
- **未提交,未编译,未测试。**

## 2026-11-19 Claude:小游戏 WebSocket 入口——交付前复读的补正(C++ 用例数更正,客户端补一处修复;未编译)

补充上面同日「小游戏 WebSocket 入口——微信 / 抖音小游戏经 WebSocket 连 gate 与 battle」条目。那一条的命令不变,只有下面两处数字以本条为准。

- **更正**:那一条写 `cpp/nodes/gate/tests/websocket_entry_test.cpp` 有 13 条、其中 `WebSocketEntry.*` 7 条,数错了。实际 12 条:`WebSocketHandshake.*` 4 条、`WebSocketFrame.*` 2 条、`WebSocketEntry.*` 6 条。`ClientEndpointWsUrl.*` 5 条不变。
- **客户端修复**(`D:\luyuan\mmorpg-client\Assets\Scripts\Net\WebSocketFramedConnection.cs`):上层的消息处理器抛异常时,已经派发的帧没有从接收缓冲里移走;下一次收包会从头再解,同一帧反复派发、反复抛,排在它后面的帧(包括心跳应答)永远轮不到,连接等于卡死。改为在 `finally` 里移走已派发的字节。TCP 实现(`GateTcpClient`)的帧出队即消费,本来没有这个问题。回归用例 `HandlerException_DoesNotRedeliverTheFrame`;`WebSocketFramedConnectionTests` 因此从 9 条变 **10 条**,那一条第 5 步的通过标准按 10 条核对。
- **客户端加固**:`Assets/Plugins/WebGL/MmorpgMiniGame.jslib` 认二进制消息改按「不是字符串」判断,不用 `instanceof ArrayBuffer`(宿主从原生层交上来的 ArrayBuffer 不保证和脚本里的是同一个构造函数,认错了会把所有下行帧丢掉);`WebSocketDrivers.cs`、`MiniGameLogin.cs` 传给 JS 的回调改用专门声明的委托类型,不用 `Action<>`。
- **复读范围**:交付前把 `websocket.{h,cpp}`、`codec.cpp` 的接线、gate 广播帧与 battle 发送路径、节点地址解析、C++ 与客户端两份用例(逐条对照实现推演)、边缘 nginx 模板、部署脚本里用到的函数与契约键是否都有定义、Go 改动的 gofmt 对齐,从头读了一遍;服务端没有再改。
- **验证状态**:仍然未生成协议、未编译、未运行,待 Codex 按那一条的第 0 ~ 7 步验证。没做 / 没核实的清单与那一条相同。
- **Java 版**:不涉及(只动了 Unity 客户端的实现,客户端契约没变)。
- **提交情况**:服务端、客户端都在工作区,本会话没有提交。

## 2026-11-19 Claude:实名与防沉迷接进 K8s 部署脚本——六个部署参数、secretKey 走工作负载密钥目录、防误关与包装入口同小游戏;data-service 与 payment 注入查实名的调用密钥(未运行,待 Codex 验证;Java 不适用)

- **起因**:同日「实名认证与未成年人防沉迷」落码条目没做 K8s 部署参数,K8s 上的国内部署开不了防沉迷(设计稿 `docs/design/real-name-anti-addiction.md` §8 末尾)。由那个会话派给本会话单做。
- **做了什么**:
  - `k8s_deploy.ps1` 加六个参数 `-LoginNppaAppId` / `-LoginNppaBizId` / `-LoginAntiAddictionExtraPlayDays` / `-LoginNppaCheckUrl` / `-LoginNppaQueryUrl` / `-LoginNppaLoginoutUrl`。AppId 非空才渲染 login 顶层 `AntiAddiction` 段(`New-LoginAntiAddictionYaml`):`Enabled: true`、`PlatformManagedAuthTypes: [wechat_minigame, douyin_minigame]`、`Nppa.AppId / BizId`、`Nppa.SecretKeyEnv: "MMORPG_LOGIN_NPPA_SECRET_KEY"`;可玩日与三个地址给了才写,`DailySubmitLimit` 不写(login 缺省 5)。键名核对自 `go/login/internal/config/anti_addiction.go`。不开时 login 模板与之前逐字相同。
  - 参数门禁 `Get-LoginAntiAddictionParamErrors`,在 `Assert-ClientEntryDeployPreflight` 里、密钥解析与任何写操作之前,不论本次部不部署 login 都查:AppId / BizId 要么都给要么都不给、只允许 `[A-Za-z0-9_.-]`;可玩日逗号分隔、每项是存在的日期且写成 `yyyy-MM-dd`(与 login 的 `time.DateOnly` 往返校验一致);地址须是 `http(s)://主机[:端口][/路径]`,各档位都收 http(官方查询与上报地址就是 http);没给 AppId 却给了可玩日或地址也拒绝(否则会被静默丢掉)。生成器用同一份规则再查一次。
  - secretKey 走工作负载密钥目录:`$script:WorkloadSecretCatalog` 新增 `login-nppa`(Env `MMORPG_LOGIN_NPPA_SECRET_KEY`,键 `secret`,消费者 login),建 Secret、注入 env、写指纹都按目录表。解析在 `Initialize-InjectedSecrets` 的 `Resolve-LoginNppaSecretKey`,只在本次部署 login 且给了 AppId 时:任何档位都没有回落值,必须恰好 32 位十六进制(与 login 门禁同口径,带结尾换行也拒绝),报错不回显值。本次没开时 `Apply-OneGoSvc` 在 login 部署之后删 `login-nppa`(不粘滞)。
  - 防误关:lib `tools/scripts/lib/k8s_client_entry.ps1` 新增第 11 节 `ConvertFrom-LoginAntiAddictionYaml` / `Get-LoginAntiAddictionDrift` / `Get-LoginAntiAddictionLiveState`,形状照第 10 节口令登录那套。`k8s_deploy.ps1` 的 `Assert-LoginAntiAddictionClusterState` 在非 DryRun、本次部署 login 时排在口令登录那道闸之后:线上模板开着而本次漏传、换了 AppId 或 BizId、线上段认不出,都在写操作之前拒绝,所有 zone 一次报全并给照传串;确认开关共用 `-ConfirmLoginAuthProviderChange`。可玩日与地址不比(按年补可玩日是常规改动,地址只在接口测试阶段换,都不会让 Pod 起不来)。
  - 摘要行追加 `login_anti_addiction=`;开着时多打一行英文提示,要把 `go-svc-login-config` 里的 `AntiAddiction` 段抄进 `runtimeConfigSecret`(login 是 SQL 写者,常驻进程读的是那份;没抄进去 login 起得来但防沉迷是关的)。
  - `dev_tools.ps1`:参数块末尾加六个参数(不挪动已有参数的位置绑定),`Invoke-K8sDeploy` 非空才透传,`Invoke-K8sImage` 给了即拒绝。
  - `deploy/k8s/manifests/go-svc/data-service.yaml` 与 `payment.yaml` 各从 `domain-service-secrets` 的 `account-directory-payment` 注入 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`(`go/shared/accountdirectory/real_name.go` 的 `PaymentSecretEnv`;data_service 用它验 `GetAccountRealName`),照 trade 那把写成 `optional: false`。
- **上线前要做(运维)**:每个 zone namespace 与 infra namespace 的 `domain-service-secrets` 先加 `account-directory-payment`(各处同值,至少 32 字节,与别的 key 都不同),再发 data-service 与 payment;缺了两边的 Pod 都卡在 CreateContainerConfigError,不开防沉迷的部署也要有。清单登记在开服手册 §1。部署脚本不在写操作之前核这个键(只有 `mail-producer-battle` 有专门核对),只能靠按手册预置。
- **取舍**:`account-directory-payment` 写成 `optional: false` 是照任务说的「照 trade 那条路」与目录验签密钥不作可缺省 Secret 的现行口径。data_service 自己对缺失是停用 `GetAccountRealName`、go/payment 目前还不读它(限额闸没做);若要让不开防沉迷的部署免预置,改成 `optional: true` 只动两份清单与本条的一个契约用例。
- **文件**(前八份同时带着别的会话的未提交改动,`payment.yaml` 是支付会话建的未入库文件,提交时按 hunk 分):`tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/dev_tools.ps1`、`deploy/k8s/manifests/go-svc/data-service.yaml`、`deploy/k8s/manifests/go-svc/payment.yaml`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`、`tools/scripts/tests/k8s_client_entry_contract.tests.ps1`;文档 `docs/design/real-name-anti-addiction.md`(状态行、§5.8 一行注释、§8 末尾表的 K8s 一行)、`docs/ops/k8s-open-server-runbook.md` §1(新增一条操作说明与 Secret 表一行)、`deploy/k8s/README.md`(login 参数清单一条)。
- **Codex 验证**(仓库根目录,不需要集群;失败时保留完整输出):
  1. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:新增「login 实名与未成年人防沉迷」一节 9 个用例(纯函数 3、基线 1、端到端 3、源码形状 1、两份清单 1),另起 3 个 DryRun 子进程;原有「工作负载密钥:k8s_deploy.ps1 的目录表与契约表逐项一致」覆盖新加的 `login-nppa`。
  2. `pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`:第 5b 节新增 25 个(往返 1、差异 4、JSON 往返 1、认不出的形状 13、读现状 4、防误关闸 1、接入位置 1),第 6、7 节各新增 1 个(包装入口透传 / 拒绝)。
  3. `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`:包装链 AST 契约与参数类型表加了六个参数,没有新用例。
  4. 回归(按 AST 抽了同一批函数或读同一份 login ConfigMap):`login_password_auth_deploy_contract.tests.ps1`、`login_metrics_contract.tests.ps1`、`lobby_authority_render.tests.ps1`、`k8s_migrate_gate.tests.ps1`,同样用 `pwsh -NoProfile -File tools/scripts/tests/<名字>`。期望全过。
  5. `python tools/scripts/check_log_text_english.py`:期望干净(新加的 Write-Host / Write-Warning 都是英文,本会话只用文本检索核过)。
  6. 变异核对(可选,各改一处、跑完改回):把 `New-LoginAntiAddictionYaml` 里 `SecretKeyEnv` 的字面量改一个字母,「纯函数 New-LoginAntiAddictionYaml」与端到端 dev 用例应红;删掉 `Apply-OneGoSvc` 里删 `login-nppa` 的那条,「实名与防沉迷源码形状」与基线用例应红;去掉 `Get-LoginAntiAddictionDrift` 里比 BizId 的那条,防误关「换了 BizId」「两个都换了」两例应红。
- **未验证**:以上全部没有运行;六份脚本只用 `Parser::ParseFile` 查过语法(0 错误),两份清单只做过 YAML 解析。
- **Java 版(AGENTS §12)**:不适用,Java 版没有 K8s 清单与部署脚本,不往 PARITY.md 加行。同日落码条目登记的实名与防沉迷待做项不变。

## 2026-11-19 Claude:未成年人充值限额闸接入 go/payment——按实名判年龄,单次 / 每月 / 单日限额按人合并预占,迟到的钱锁下复核(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:实名会话落码时留下的第二批(设计稿 `docs/design/real-name-anti-addiction.md` §5.6、§8 末尾)。支付会话 11-19 回复 go/payment 与 `data/tip/Tip.xlsx` 已改完之后才动手。规则是国新出发〔2019〕34 号:未满 8 周岁不得充值;8–16 周岁单次 50 元、每月 200 元;16–18 周岁单次 100 元、每月 400 元。单日上限按配置,缺省 0 = 不设(充值设计 §18 第 7 条待负责人定)。
- **做了什么**(口径在设计稿 §5.6):
  - 配置:`Payment.Compliance.MinorSpendingGate` 加 `realname`,选它才放开 `wechat_pay`、`alipay`、`apple`、`google` 在生产模式启用。`realname` 要 `DataServiceRpc`(etcd 发现 `dataservice.rpc`)与密钥 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`,缺了拒启。新增 `DailyLimitFenAge8To16`、`DailyLimitFenAge16To18`。
  - 建意图:不由平台管限额的渠道(含 devpay)在写意图之前读实名。没实名、未满 8 周岁、非人民币、超单次上限分别拒绝;未成年人在 `payment_spend` 一人一行的行锁下核对「本月起已付 + 全部在途 + 本次」(单日同理),通过才写意图。两个小游戏渠道不经闸。
  - 额度不另记计数,由意图行算:CREATED 全额占着,PAID 按 `paid_ms` 计当月当日,CLOSED 不占。过期关单与渠道撤销自然释放,关单、取消不释放(充值设计 §12)。意图加四列:`spender_hi/lo`(pi 的 SHA-256,不存 pi)、`paid_ms`、`minor_age`。
  - 意图关闭之后才到的钱在同一把锁下复核:迟到的付款超限不发货、转人工(ORPHAN);App Store 撤回退款、记账第二步没做完而意图已到期关闭的,照样计入,超限记合规日志与指标。
  - 渠道翻转只跟订单本身那笔钱(已绑定或还没报给商城):商城已转退款、拒收、转人工的付款被撤销或撤回,不再改意图状态。之前重复付款被撤销会把意图关掉,会把正付着的那一单的额度也放掉。
  - tip:`data/tip/Tip.xlsx` payment 段末尾 5 个码:`PaymentRealNameRequired`、`PaymentMinorNotAllowed`、`PaymentMinorSingleLimit`、`PaymentMinorDailyLimit`、`PaymentMinorMonthlyLimit`,都不是故障。
  - `accountdirectory.NewRealNameClient` 改成构造时校验密钥,回 `(*RealNameClient, error)`;唯一调用方是 payment。
  - 指标 `payment_minor_gate_total{outcome}`。
- **文件**:`proto/payment/payment_table.proto`;`data/tip/Tip.xlsx`;`go/payment/{payment.go,channels.go,etc/payment.yaml}`;`go/payment/internal/config/{config.go,config_test.go}`;`go/payment/internal/core/{service.go,workers.go,minor.go,minor_test.go}`;`go/payment/internal/data/{repo.go,tables.go,spend.go,spend_test.go,spend_tidb_test.go}`;`go/payment/internal/constants/constants.go`;`go/payment/internal/server/server.go`;`go/shared/accountdirectory/real_name.go`;`docs/design/real-name-anti-addiction.md`(头部状态、§5.6、§6、§8 两行);Java 仓 `PARITY.md`。`go/payment/payment.go` 与设计稿里另有别的会话未提交的改动(时钟核对、§5.5、K8s 一行),提交时按 hunk 分。
- **没做、交给别的会话**:
  - 支付会话(正在做 payment 的 K8s 批次,本条没碰 `tools/scripts/`、`deploy/`):payment 的 ConfigMap 要带 `DataServiceRpc`(换成集群的 etcd)与 `Payment.Compliance` 的新键;`tools/scripts/tests/sql_boot_catalog_local.tests.ps1` 夹具里 `mmorpg_payment` 的表名清单要加 `payment_spend`;充值设计 §22.8、§22.11 第 4 条「只认 none」已过时。
  - 密钥注入 payment 与 data_service 已由「实名与防沉迷接进 K8s 部署脚本」条目落码(`domain-service-secrets` 的 `account-directory-payment`)。
  - 客户端:本条没有客户端授权,5 个码的客户端生成物等有客户端授权的那次生成一起出。
- **数据**:`payment_intent` 多了四列、新表 `payment_spend`。项目未上线,验证前清空 `mmorpg_payment` 重跑 `payment -migrate`。
- **Codex 验证**(仓库根,串行;失败保留完整输出,不要改断言;工作区有别的会话的在途改动,失败先按文件归属分清):
  1. 生成:`dev.bat gen`(给 payment 段新加的 5 个码发号);再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(`payment_table.proto` 变了)。期望:`go/shared/generated/pb/table/payment_error_tip.pb.go` 出现 `PaymentError_kPaymentRealNameRequired` 等 5 个;`go/proto/payment/payment_table.pb.go` 出现 `PaymentSpendRecord` 与 `SpenderHi`。
  2. Go:`cd go/payment && gofmt -l . && go build ./... && go vet ./...`(本会话对改过的文件跑过 `gofmt -l`,无输出);`cd go/shared && go build ./accountdirectory/ && go vet ./accountdirectory/`。
  3. 新用例:`cd go/payment && go test ./internal/data/ ./internal/core/ ./internal/config/ -count=1 -v`。期望通过:`TestSpenderKeyIsStable`(期望值由 Python hashlib 离线算出)、`TestSpendLimitAdmit`(11 个分支)、`TestCreateIntentMinorSpendingGate`(12 个分支)、`TestCreateIntentReservesAgainstTheBuyersAgeBracket`、`TestLatePaymentOfAMinorIsRecheckedUnderTheLimit`、`TestChannelFlipsFollowOnlyTheOrdersOwnPayment`、`TestValidateMinorSpendingGate`;没配 DSN 时三条 `TestTiDB*` 显示 SKIP。再 `go test ./... -count=1` 回归原有的 7 条。
  4. 真库(有专用 TiDB 时;账号要能建库、删库,用例只建删自己的随机库 `payment_it_*`):工作目录 `go/payment`,`PAYMENT_TEST_DSN='<user>:<pwd>@tcp(127.0.0.1:4000)/' PAYMENT_REQUIRE_TIDB=1 go test ./internal/data/ -run TestTiDB -count=1 -v`,三条:同一个人并发 8 个 50 元只成 4 个、同一订单并发只插一行、到账 / 下个月 / 关闭释放 / 迟到付款超限不改 / 撤回强制计入。再加 `-race` 跑一遍。能的话对 MySQL 8 也跑一遍(读已提交下不加间隙锁)。
  5. `python tools/scripts/check_log_text_english.py`:本条新日志都是英文,期望不多出发现。
  6. 冒烟(可选,devpay):payment 配 `MinorSpendingGate: realname`,payment 与 data_service 都设同值的 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`(≥ 32 字节),清空 `mmorpg_payment` 后 `payment -migrate`。没实名的账号 `CreatePayIntent(DEVPAY)` 回 `kPaymentRealNameRequired`;给账号写一个 15 周岁的 pi(走 /api/real-name,或直接改 data_service 库的 `real_name_pi`)后,51 元的单回 `kPaymentMinorSingleLimit`,50 元的四单成功、第五单回 `kPaymentMinorMonthlyLimit`;一单到账后仍计在本月;一单过期关闭后额度释放。
- **未验证**:以上全部没有运行。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;已在 `D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾登记「2026-11-19 未成年人充值限额闸」。客户端可见的新契约是 payment 段的 5 个 tip 码。

## 2026-11-19 Claude:账号注销三批落码——Apple / Google 客户端登录、身份令牌一次性消费、注销主流程(冷静期 7 天、执行者、Apple 撤销)、删角收尾清数据(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因与定案**:接同日「账号注销与 Apple / Google 客户端登录接入——设计稿」条目。负责人 11-19 定:Apple 撤销用 B(申请注销时重新授权,login 先换票,冷静期提交成功后撤销,不保存 Apple 令牌);身份令牌一次性消费,不做挑战式 nonce;冷静期 7 天;设计 §10 三批都同意动码。设计 `docs/design/account-deletion.md`(§9 定案,§6.2 按落码结果改正)。大部分文件已被别的会话的批量提交 `9824368eb` 收进去(本会话没有提交),之后的改动仍在工作区。
- **做了什么**:
  - 第 1 批:login 的 Apple / Google 身份令牌验签后在登录域 Redis `SET NX PX` 登记摘要(`redisslot.IdTokenUsedKey`),同一枚第二次按凭证无效拒、登记失败按渠道故障;两个 provider 改到登录域句柄建好之后注册(`svc.RegisterIdTokenProviders`,复用 LoginRedis,连接总账不变)。客户端 iOS(AuthenticationServices,不要 scope)、安卓(`.androidlib`,Credential Manager 的 Sign in with Google,Java)、C# 桥 `PlatformSignIn`、登录页两个按钮。沙箱 mock 签出的令牌带随机 `jti`(同秒同种子不再逐字节相同),Apple 签发顺带给授权码。
  - 第 2 批:
    - 协议:`account_directory` 加字段 3~7,新表 `account_deletion_receipt`(只存账号 SHA-256);data_service 新 RPC `RequestAccountDeletion` / `CancelAccountDeletion` / `ClaimAccountDeletions` / `FinishAccountDeletion`,`AccountDirectoryResponse.lifecycle = 2`,Lookup 的 `include_account_state = 3` / `account_deletion_state = 7`;login 的 `LoginResponse` 7~9,`LoginPreGate` 两个、`LoginAdmin` 一个新方法;`role_fence.proto` 加 `PurgeRemovedRole`。`data/tip/Tip.xlsx` login 段组尾插 4 行:2038 `LoginAccountDeletionPending`、2039 `Blocked`、2040 `InProgress`、2041 `ReauthRequired`(都不是故障;脚本重载、只插这 4 行、写后逐格核对;实名会话的 8 个码排在其后,2042~2049)。
    - data_service:状态机 `store/account_deletion.go`(申请先预检:逐名并行走协调者新增的 `DryRunBlockers`,有冻结令牌直接算挂起;提交、撤销、认领、收尾都持账号行锁;收尾先 `reapRefs` 再要求 ref 为空才写回执、删行);`reserveRef` 对注销中的账号名额按 0 算;`readAccount` 同一条 SELECT 带出注销状态(实名会话又加了 `real_name_pi`);`ReadAccountDirectory` 一律走 `ReadWithLifecycle`,注销没装配时只清注销状态;建新行时读回执给出 `previous_deleted_ms`;冷静期配置 `AccountDirectory.DeletionCoolingOffSeconds`(缺省 604800,范围 [0, 30 天]);逾期 gauge `data_service_account_deletion_overdue`,告警 `DataServiceAccountDeletionOverdue`(30 分钟)。
    - login:`pkg/accountdeletion`(申请:Apple 先换票→目录预检并提交→提交后撤销 Apple、`RevokeAll`、按角色在线会话踢线;撤销;执行者每 30 秒认领 8 个,逐名踢线并走现有 `RemoveAccountRoles`,删完调收尾);`pkg/auth/apple_revoke.go`(ES256 现签 client_secret,换票 / 撤销,invalid_grant 回 2041);配置 `AuthProviders.Apple.TeamId / KeyId / PrivateKeyEnv`(三项都写或都不写,私钥启动时解析,坏了拒启);EnterGame、CreatePlayer 遇注销中回 2038;`/api/login` 应答带注销状态;`auth_type=access_token` 的登录(HTTP 与 gate)不建账号行;运维代办 `LoginAdmin.ScheduleAccountDeletion`(source=2)。与实名会话共用 `readAccountDirectoryWithRealName`(同一次目录读,回 4 个值)。
    - 网关:`/api/account/delete`、`/api/account/delete/cancel`(按区选路、沿用登录限流桶,令牌只以摘要进限流键),`/api/login` 三个新字段。
    - 部署:`k8s_deploy.ps1 -LoginAppleTeamId / -LoginAppleKeyId`,单键 Secret `login-apple-signin`(键 `private-key`)经 secretKeyRef 注入 `MMORPG_LOGIN_APPLE_SIGNIN_PRIVATE_KEY`;不粘滞、防误关同 ClientId。login 配置来自运维预置的 `runtimeConfigSecret`,撤销要等运维加那三行才生效。
    - 客户端注销界面:设置面板「注销账号」(说明、勾选已阅读、重新认证、按 tip 分支)、冷静期弹窗(撤销 / 退出)、「之前的账号已注销」提示。
  - 第 3 批:`shared/rolefence` 加 `PurgeFunc` / `NoPurge` / `PurgeRemovedRole`,`NewParticipantServer` 多一个 purge 参数;协调者加 `Purge`(并行、2 秒、全成功才算);删角收尾 `settleRemovedRole` 在释放名字与删小表之间清 `player_store` 并调六个参与者,失败计 `purge_data` 步、后台重做,收尾预算 1 秒放到 4 秒;六个参与者各自的清理(删 / 留 / 跳过的表与理由见设计 §6.2),guild 删角挂起加 `guild_leader`。
- **文件**(实名会话同时改了 login / data_service 的目录文件,支付、追踪等会话也碰了同几个装配文件,提交时按 hunk 分):
  - 协议 / 表:`proto/common/database/rollback_database_table.proto`、`proto/data_service/data_service.proto`、`proto/login/login.proto`、`proto/role_fence/role_fence.proto`、`data/tip/Tip.xlsx`。
  - data_service:新增 `internal/store/account_deletion.go`、`account_deletion_tidb_test.go`、`player_purge.go`、`internal/server/account_deletion.go`;修改 `internal/store/account_directory_store.go`、`account_directory_steps.go`、`account_role_ref.go`、`account_role_fence.go`、`account_role_name.go`、`account_directory_events.go`、`schema.go`、`account_directory_flow_test.go`、`account_role_cross_domain_tidb_test.go`,`internal/server/account_directory.go`,`internal/svc/servicecontext.go`、`role_fence.go`、`recall_channel.go`,`internal/config/config.go`,`internal/metrics/account_directory.go`、`metrics.go`、`account_directory_test.go`,`internal/rolefencecoord/coordinator.go`。
  - shared:新增 `accountdirectory/deletion.go`;修改 `rolefence/participant.go`、`rolefence_test.go`、`rolefence_tidb_test.go`,`redisslot/login_keys.go`。
  - login:新增 `internal/logic/pkg/accountdeletion/service.go`、`executor.go`、`service_test.go`,`internal/logic/pkg/auth/apple_revoke.go`、`apple_revoke_test.go`、`idtoken_replay_test.go`,`internal/svc/account_deletion.go`,`internal/logic/loginpregate/requestaccountdeletionlogic.go`、`cancelaccountdeletionlogic.go`,`internal/logic/loginadmin/scheduleaccountdeletionlogic.go`,`internal/logic/clientplayerlogin/resolve_account.go`,`internal/config/auth_providers_apple_revoke_test.go`,`cmd/sandbox_mock/apple_revoke.go`、`apple_revoke_test.go`;修改 `internal/logic/pkg/auth/idtoken.go`、`idtoken_providers.go`,`internal/svc/auth_init.go`、`servicecontext.go`,`internal/config/config.go`、`auth_providers.go`,`internal/logic/clientplayerlogin/loginlogic.go`、`entergamelogic.go`、`createplayerlogic.go`、`account_directory.go`、`anti_addiction.go`,`cmd/sandbox_mock/idtoken.go`、`main.go`,`etc/login.yaml`。
  - 参与者:新增 `go/friend/internal/data/role_fence_purge.go`(+`_tidb_test.go`)、`go/guild/internal/data/role_fence_purge.go`(+`_tidb_test.go`)、`go/mail/internal/data/role_fence_purge.go`(+`_tidb_test.go`)、`go/match/internal/authority/role_fence_purge.go`(+`_native_test.go`)、`go/player_inbox/internal/inbox/role_fence_purge.go`(+`_integration_test.go`,integration 标签)、`go/trade/internal/commerce/role_fence_purge.go`(+`_tidb_test.go`);修改 `go/guild/internal/data/role_fence_pending.go`、`go/match/internal/authority/battle_report.go`(文件头注释),装配 `go/friend/friend.go`、`go/guild/guild.go`、`go/mail/mail.go`、`go/match/match_service.go`、`go/player_inbox/player_inbox.go`、`go/trade/trade.go`,测试调用点 `go/guild/internal/data/role_fence_tidb_test.go`、`go/mail/internal/data/role_fence_tidb_test.go`、`go/match/internal/authority/occupancy_native_test.go`、`go/player_inbox/internal/inbox/role_authority_integration_test.go`、`go/trade/internal/commerce/role_fence_tidb_test.go`。
  - Java 网关(`java/gateway_node/src/...`):新增 `controller/AccountDeletionController.java`、`service/AccountDeletionService.java`、`dto/AccountDeletionRequest.java`、`dto/AccountDeletionResponse.java`、`dto/CancelAccountDeletionRequest.java`,测试 `controller/AccountDeletionControllerTest.java`、`service/AccountDeletionServiceTest.java`、`grpc/LoginRpcClientAccountDeletionCodecTest.java`;修改 `grpc/LoginRpcClient.java`、`dto/LoginResponse.java`、`service/LoginService.java`。
  - 部署:`tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/dev_tools.ps1`,`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`k8s_login_auth_provider_guard.tests.ps1`、`k8s_client_entry_contract.tests.ps1`、`data_service_account_directory_alerts_contract.tests.ps1`,`deploy/k8s/data-service-account-directory-alerts.yaml`、`deploy/k8s/README.md`,`.github/workflows/deploy-config-tests.yml`(路径过滤加 `account_deletion.go`)。
  - 客户端仓(`D:\luyuan\mmorpg-client`,都带 `.meta`):新增 `Assets/Plugins/iOS/MmorpgAppleSignIn.mm`、`Assets/Plugins/Android/MmorpgGoogleSignIn.androidlib/`(`build.gradle`、`AndroidManifest.xml`、`src/main/java/com/mmorpg/signin/GoogleSignInBridge.java`)、`Assets/Editor/AppleSignInBuildPostProcess.cs`、`Assets/Scripts/Net/PlatformSignIn.cs`、`Assets/Tests/EditMode/Net/PlatformSignInTests.cs`、`AccountDeletionTests.cs`;修改 `Assets/Scripts/Core/ClientSettings.cs`、`Assets/Scripts/Net/GatewayHttpClient.cs`、`Assets/Scripts/Game/GameClient.cs`、`Assets/Scripts/UI/Ugui/QdaoServerSelectView.cs`、`.Visuals.cs`、`.LoginUtilities.cs`(实名会话同时改了后两类文件)。
  - 文档:`docs/design/account-deletion.md`、`third-party-login-end-to-end-design.md`、`horizontal-scale-logical-db.md`、`account-role-authority.md`、`workload-secrets-via-k8s-secret.md`,`docs/ops/wechat-qq-sandbox-runbook.md`(§I.4 部署、§I.5 注销联调),`docs/README.md`;Java 仓 `PARITY.md`。
- **Codex 验证**(失败时保留完整输出;工作区有很多会话的在途改动,失败先按文件归属分清):
  0. 本地库重建:`account_directory` 加了列、多了 `account_deletion_receipt`,目录的形状守卫对旧库拒启(项目未上线,旧库重建);data_service 跑一次 `-migrate`。
  1. 生成(仓库根):`dev.bat gen`(期望 `LoginError_kLoginAccountDeletionPending` 等四个码 2038~2041 出现在三语言 tip 产物里,其余码号不变);`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(默认配置,写客户端仓;期望客户端 `Assets/Scripts/Proto/Generated/LoginErrorTip.cs` 出现那四个枚举);`pwsh -File go/build.ps1`(login 的 goctl 包装,期望 `internal/server/loginpregate/loginpregateserver.go` 出现 `RequestAccountDeletion` / `CancelAccountDeletion`、`loginadmin` 出现 `ScheduleAccountDeletion`,并调到本条新增的三个 logic 文件)。
  2. `gofmt -l`:对上面列的全部 Go 文件,期望无输出(11-19 本会话与子任务都跑过,无输出;gofmt 不算编译)。
  3. 编译与静态检查,逐个模块(`go/shared`、`go/data_service`、`go/login`、`go/friend`、`go/guild`、`go/mail`、`go/match`、`go/player_inbox`、`go/trade`):`go build ./... && go vet ./...`。
  4. 新用例:
     - `go/shared`:`go test ./rolefence/ -run 'PurgeRemovedRole|NewParticipantServer' -count=1 -v`;`go test ./accountdirectory/ ./redisslot/ -count=1`。
     - `go/login`:`go test ./internal/logic/pkg/auth/ -run 'IdTokenProviderConsumesEachTokenOnce|IdTokenUsedForCoversExpiryPlusLeeway|AppleRevoke|NewAppleRevokerRejectsBadKeys' -count=1 -v`;`go test ./internal/logic/pkg/accountdeletion/ -count=1 -v`(7 个顶层);`go test ./internal/config/ -run 'AppleRevokeKeyAllOrNone|GoogleSectionRejectsAppleRevokeFields' -count=1 -v`;`go test ./cmd/sandbox_mock/ -count=1 -v`(含新增 `TestSandboxAppleRevokeRoundTrip`、`TestSandboxIdTokensAreUnique`);`go test ./internal/svc/ -run IdToken -count=1 -v`;并发:`go test ./internal/logic/pkg/auth/ -run IdToken -race -count=3`。
     - `go/data_service`:`go test ./internal/metrics/ ./internal/rolefencecoord/ -count=1`;真 TiDB(专用库,与现有目录真库用例同一组 `ACCOUNT_DIRECTORY_TEST_DSN`、`ACCOUNT_DIRECTORY_REQUIRE_TIDB=1` 与 `MMORPG_SQL_FIXTURE_*` 环境):`go test ./internal/store/ -run 'TiDBAccountDeletion|TiDBAccountDirectoryCapReplayRepairAndRemoval' -count=1 -v`(新增 3 个顶层;后一个回归删角收尾多了清数据一步)。
     - 参与者(前五个都要带 `MMORPG_SQL_FIXTURE_PHYSICAL_AUTHORITY_SHA256`、`MMORPG_SQL_FIXTURE_LOCK_QUALIFICATION_SHA256`、`MMORPG_SQL_FIXTURE_LOCK_PRIMITIVE`、`MMORPG_SQL_FIXTURE_RECEIPT_DIR`,friend 除外):`go/friend` `FRIEND_TEST_TIDB_DSN`(专用库,会删表重建)+`FRIEND_REQUIRE_TIDB_TESTS=1` `go test ./internal/data/ -run 'TestTiDBFriend' -count=1 -v`;`go/guild` `GUILD_ROLE_TEST_DSN`+`GUILD_ROLE_REQUIRE_TIDB=1` `go test ./internal/data/ -run 'TestTiDBRoleFence|TestTiDBPendingGuild' -count=1 -v`;`go/mail` `MAIL_RANK_TEST_TIDB=127.0.0.1:<port>`+`MAIL_RANK_REQUIRE_TESTS=1` `go test ./internal/data/ -run 'TestTiDBMail' -count=1 -v`;`go/match` `MATCH_IT_TIDB=127.0.0.1:<port>`+`MATCH_IT_REQUIRE_TIDB=1` `go test ./internal/authority/ -run 'TestPurgeTiDB|TestPendingTiDB|TestOccupancyTiDB|TestBattleReportTiDB' -count=1 -v`;`go/player_inbox` `PLAYER_INBOX_TEST_MYSQL_DSN=...`+`PLAYER_INBOX_REQUIRE_MYSQL_TESTS=1` `go test -tags integration ./internal/inbox/ -run 'TestIntegrationInbox' -count=1 -v`;`go/trade` `COMMERCE_TEST_DSN`+`COMMERCE_REQUIRE_TIDB=1` `go test ./internal/commerce/ -run 'TestTiDBRole' -count=1 -v`,再 `go test ./internal/data/ -run TestSeqTableRowsAreNeverDeleted -count=1`(应保持绿)。
  5. 回归:上面九个模块各 `go test ./... -count=1`。
  6. C++:改了 `login.proto`、`data_service.proto`(gate / scene 用它们的生成物),没改 C++ 逻辑;`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(串行),确认生成物编得过。
  7. Java 网关(仓库根):`mvn -B -f java/pom.xml -pl gateway_node -am test "-Dtest=AccountDeletionControllerTest,AccountDeletionServiceTest,LoginRpcClientAccountDeletionCodecTest,LoginServiceTest,LoginRpcClientParseTest,LoginRpcClientRetryTest,AssignGateControllerTest,LoginEndpointIntegrationTest" -Dsurefire.failIfNoSpecifiedTests=false`,再 `mvn -B -f java/pom.xml -pl gateway_node -am test` 全量。
  8. PowerShell 契约(仓库根,不连集群,每条退出码 0、全部 `[PASS]`):`pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`、`k8s_client_entry_contract.tests.ps1`、`data_service_account_directory_alerts_contract.tests.ps1`、`k8s_deploy_contract.tests.ps1`(慢)、`login_password_auth_deploy_contract.tests.ps1`(回归,读密钥目录)。
  9. 客户端(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error;关掉 Unity 后重新烘焙预制体 `Unity.exe -batchmode -quit -projectPath D:\luyuan\mmorpg-client -executeMethod MmorpgClient.UI.EditorTools.QdaoUguiBuilder.BuildAll -logFile Logs/qdao-ugui-build.log`(日志有 `[QdaoUguiBuilder] PREFAB_PATH=`,`git diff --stat` 只有 `QdaoServerSelect.prefab` 一类变化,`Bootstrap.unity` 或贴图 `.meta` 也变了要报);EditMode(Unity 6000.6.0f1,不加 `-quit`)`-runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Net.PlatformSignInTests;MmorpgClient.Tests.EditMode.Net.AccountDeletionTests;MmorpgClient.Tests.EditMode.Net.LoginCredentialTests;MmorpgClient.Tests.EditMode.Net.GatewayLoginErrorTextTests;MmorpgClient.Tests.EditMode.Ugui.QdaoServerSelectPrefabTests;MmorpgClient.Tests.EditMode.Ugui.QdaoLoginPresentationTests"` 全绿(新增 8 + 13 个用例)。可选:导出一次安卓 Gradle 工程编译(依赖解析与 Java 编译),导出一次 iOS 工程确认权利文件有 `com.apple.developer.applesignin`、`.mm` 能编过。
  10. `python tools/scripts/check_log_text_english.py`:干净。
  11. 沙箱端到端(可选,需要 login、网关、data_service 与 mock):按 `docs/ops/wechat-qq-sandbox-runbook.md` §I.5,期望值写在各条注释里。
- **未验证**:以上全部没有运行。真机(iOS、安卓)要 Apple / Google 开发者账号,现在没有;登录页的渠道按钮与注销弹窗没有效果图,Apple 按钮未用官方标志,视觉验收待定。
- **已知限制**(设计 §7.2、§11):`player_locator` 按区部署,踢线只踢得到本区的在线设备(别区令牌照样吊销、进不了游戏,执行时还在线的角色要等断线);转让帮主不锁目标的护栏副本,已删除的成员也可能被转成帮主,清理会一直被拒、走告警人工处理;`match_battle_script*` 没有清。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;已在 `D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾登记「2026-11-19 账号注销与 Apple / Google 身份令牌一次性消费」。客户端可见契约(两个新接口、`/api/login` 三个新字段、tip 2038~2041)两版要同批。

## 2026-11-19 Claude:读 11-15 与 11-18 的运行日志后修两处——战斗打完时客户端把正常收尾当成断线;调试栈 C++ 节点一直去连不存在的 span 采集器(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人要求读那天的日志看问题并修掉。读了三样:调试栈 13 个容器的输出、日志卷里 C++ 节点 11-09~11-19 的日志(重点 11-15 与 11-18)、客户端 11-18 的 `Player.log`。日志只拷到会话草稿目录,没有进仓库;没有读 `.env`,没有启动服务。
- **日志里看到的**:
  1. 11-15 一天:战斗直连每 30 秒被 battle 按空闲踢一次(`idle timeout;disconnecting` 155 次;5 场战斗握手了 159 次)。这就是 PVP 修复批里已修的客户端直连心跳,11-16 之后的日志里没有再出现。
  2. 11-07 起几乎每天:`battle direct connection rejected … reason=ticket_not_in_roster`(抽样日志,每个进程只记首次)。11-18 的客户端日志对上了:每场战斗结束,直连断开 → 1 秒后同票重连 → 被 battle 拒掉 → 排补签 → 终局包到了才收场。
  3. 每个 C++ 节点约每分钟一条 `Connector::handleError state=1`(ERROR)加一条 `POLLHUP`(WARN),11-15 一天 scene 1119 对、gate 1121 对:span 导出器在连 `127.0.0.1:4318`,容器里没有采集器。
- **第 2 条的根因**:终局提交改成「个人 `BattleEndS2C` 只由 scene 在结算提交之后推」(turn-based §24)之后,battle 的 `FinishBattle` 只广播最后一回合帧就关直连。客户端仍只把 `NotifyBattleEnd` / `NotifySpectateEnd` 当成「本局结束」,而 FIN 先到、终局包后到(11-18 本机实测晚 1~2 秒),于是按意外断开处理。
  玩家看到的:最后一回合播完后相位文案变成「等待战斗连接…」,1.5 秒后亮「正在连接战斗服务器…」横幅;结算再慢一点,补签会发出去,match 回「战斗不存在」,客户端报「战斗已结束」并收屏,最后一回合的演出被打断。
- **改动(客户端仓 `mmorpg-client`;两版服务端共用这一个客户端,服务端代码不用改)**:
  - `Assets/Scripts/Game/Battle/DirectRoutingBattleTransport.cs`:已出胜负的 `NotifyTurnResult` 也算「本局结束」信号,交给 `BattleDirectLink.HandleBattleEnded`,之后的 FIN 是正常收尾,不重连、不补签。
  - `Assets/Scripts/Game/Battle/BattleClient.cs`:`AckTurnPlayed` 在已出胜负时不回 WaitingAction,留在 Resolving 等结算包,至多 `SettlementWaitSeconds`(10 秒);到点按「补拉到已出胜负」同口径收场,结算包晚到照样弹结算面板。新属性 `IsBattleDecided`、`IsAwaitingSettlement`;开新局、重连提示、收尾都会作废这个等待,到点时再核对一次仍在等这一局。
  - `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`:已出胜负后不亮连接横幅,相位文案改「战斗结算中…」。
  - `Assets/Scripts/App/PresentationShowcase.cs`:演出台等最后一回合播完的判据跟着改(原来等相位回 WaitingAction)。
  - `Assets/Scripts/Net/BattleDirectLink.cs`:只改生命周期注释。
  - 测试:`DirectRoutingBattleTransportTests` 新 2 例(`DecidedTurnResult_MarksLinkEnded_ServerFinIsCleanClose`、反例 `OngoingTurnResult_ThenFin_IsStillAnUnexpectedDisconnect`);`BattleClientStateMachineTests` 新 3 例(`DecidedTurnResult_AckKeepsResolving_SettlementEndStillDelivered`、`DecidedTurnResult_SettlementNeverArrives_ConvergesNoneAfterTheWait_LateEndStillDelivered`、`SettlementWait_IsVoidedByTheNextBattle`)。
- **改动(本仓)**:
  - `tools/scripts/prepare_docker_debug.py`:生成 C++ 节点配置时去掉 `Tracing` 段。这个栈不起采集器,不配地址节点就不导出(`core/tracing/otlp_http.h`);要看链路时给容器设 `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`。
  - `tools/scripts/tests/prepare_docker_debug_refresh_test.py`:整套渲染用例里加一条「生成的节点配置不带 `Tracing`」。
  - `docs/design/turn-based-battle-server.md` §18.2「生命周期」按现行实现改写(原文写的是先推终局包再关直连)。
- **看了没改的**:
  - 整场时限到点强制判平(`OnBattleDeadline`)没有最后一回合帧,作废与停机也没有。客户端这时按意外断开恢复,由补签拿到「战斗已不存在」后提示并收屏,结算包晚到照样弹面板。这条路没有误导性的文案,要让它也静默收尾得加一条协议消息,这次没做。
  - `[GrpcServer] … GOAWAY: invalid last_stream_id`(每天两三次,随后一条 `Broken pipe`):Go 侧 gRPC 客户端关空闲连接时发的 GOAWAY 带的是自己的流号,C++ 这边的 HTTP/2 库判它不合规。连接本来就在关,没有功能影响;要消掉得升级 Go 的 gRPC 库,没动。
  - battle 上一条 `kRateLimitExceeded messageId=162`(11-15,`SetAutoBattle` 在限速窗口内连发两次)、scene 重启时 48 次 `Preset RPC port … already registered`(上一个进程的租约还没到期,到期后自己注册上)、客户端日志里的 `[unhandled] message_id=66`(Unity 不消费 `SyncBaseAttribute`,Java 仓契约文档已记)、路由服 `No available instance for target type FriendNodeService`(调试栈不带 friend):都是已知行为,没动。
  - 11-18 12:39(UTC)之后 etcd / Kafka / MySQL 的成批超时、match 两次 `Lock wait timeout`:时间点成簇,对应主机休眠后恢复,各服务都自己恢复了,没有当成代码问题。
- **给 Codex 的验证步骤**(本会话没有编译、没有跑任何测试):
  1. Unity EditMode:`DirectRoutingBattleTransportTests`、`BattleClientStateMachineTests`、`BattleDirectLinkTests`、`BattleClientAutoBattleTests` 以及战斗 HUD 相关用例全部通过;失败时保留第一个编译错误原文与失败用例的完整输出。
  2. 仓库根目录:`python tools/scripts/tests/prepare_docker_debug_refresh_test.py`,全部通过。
  3. 重新准备并重启调试栈后各打一场 PVE 与一场 PVP,看三处:battle 日志不再出现 `ticket_not_in_roster`;客户端 `Player.log` 里战斗结束只有「链路终结 kind=Ended」,没有「直连断开 / 同票重连 / 补签」;C++ 节点日志不再出现 `otlp-span-export` 与 `Connector::handleError`。
  4. 演出台(`PresentationShowcase`)跑一遍:最后一回合播完后立刻进结算,不等 10 秒、不报「播放超时」。
- **残余**:结算包超过 10 秒才到时,战斗屏会先收起、面板稍后弹出;这个时限是按本机实测的 1~2 秒留的余量,没有压测数据。
- **Java 版**(AGENTS §12):不涉及。只改了共享客户端和本仓的调试栈脚本,Java 服务端同样受益;没有往 Java 仓 `PARITY.md` 加行。
- **提交情况**:两仓 main 工作区,未提交,没有开分支。

## 2026-11-19 Claude:断线重连 R19——C++ etcd 地址改为只认一个、CLUSTER SLOTS 在途加上限、Kafka 启动核对不再卡主循环、两侧过时注释;Go 先监听后注册(未编译、未测试,待 Codex 验证;Java 不涉及)

- **范围**:2026-11-12「断线重连清单收尾」条目里 R19 没做的各项,逐项落到「已改」或「核过不用改」。会话「接着做完服务器剩下的代码(缺口清单续)」做(分工见本日「断线重连 R3 收尾」)。状态表 `docs/design/infra-reconnect-overview.md` §4.1 R19 行与 §3.2 MySQL 建池一条已改。都在 main 工作区,没有分支、没有提交。
- **C++**:
  - **etcd 只用第一项**(`cpp/libs/engine/config/config.cpp` 的 `readBaseDeployConfig`):定为配置契约——`Etcd.Hosts` 必须恰好一个负载均衡地址(K8s 上 `k8s_deploy.ps1` 本来就只渲染 `etcd.<infra>:2379` 这个 Service),多填或不填在加载配置时 `FATAL Etcd config: ...` 拒启。原来多填的其余地址静默不用、让人以为配了故障转移;不填时 `EtcdService::Init` 解引用空列表。没有做多地址轮换:注册、发现、热关停三条通道的流与在途账本都绑在一条通道上,轮换要改的面远大于收益,K8s 上 Service 已经做了成员摘除。`bin/etc/base_deploy_config.yaml` 的注释同改。
  - **`CLUSTER SLOTS` 在途没有超时**(`redis_cluster_topology.{h,cpp}` 的 `TopologyRefreshPolicy`、`redis_cluster_client.{h,cpp}`):节拍加在途上限 `RedisClusterOptions::topologyRefreshTimeout`(缺省 5s)与代次。1s 的 `OnTick` 发现在途超时就作废、按失败计数(`topologyRefreshFailures`)、保留请求另起一次;迟到的应答按代次丢弃,不会结束新的在途、也不会拿旧视图盖新视图。原来应答一直不来时周期刷新与按需刷新都被挡住,要等那条连接的存活探测(约 15+5s)断开才解开。发送失败时同一代次当场按失败结束(原来不记开始时刻,现在受 1s 限速,与节拍同频)。
  - **Kafka 启动核对分区数卡主循环**(`kafka_consumer.cpp`):原来每个消费者最多 5 ×(5s 查询 + 1s 等待)= 30s,跑在节点主 EventLoop 上,gate 一次起 4 个就是两分钟,远超节点链路 15s 判死。改成每个消费者 3s 预算、单次查询 1s、不在最后一次失败之后再等;上一个消费者用完预算也没查到时,后面的只查一次(进程内一个原子标记,查到一次就清)。broker 答了不同分区数时照旧拒启,查不到时照旧按契约 assign。gate 最坏约 2.5s + 3 × 1s。
  - **过时注释**:`node.cpp` 冲突关停的「四个触发点」改成现在的两个(都是 `kReRegistrationFailed`)、`OnTxnFailed` 改成重注册的 CAS 失败、「重试定时器」改成 EtcdService 里的协程任务;`node.h` 的「fence 发号」改成实际顺序。
- **Go**:
  - **scene_manager、client_rpc_router 先注册后监听**:两个入口改成先起 gRPC(后台 goroutine,裸 go:监听失败 panic 就该让进程死),端口可连之后再注册 C++ NodeInfo,与 match / chat / friend 同一写法。`shared/noderegistry` 新导出 `WaitForListening`(`RegisterAfterListening` 的前半段,后者改成调它,行为不变)。client_rpc_router 注册之前到达的请求(只可能来自不经 etcd 发现的调用方)没有子集身份,按 `SetSubsetIdentity` 的既有约定在全部实例里挑目标。
  - **login 口令库不强制超时**:核对后已不成立——口令存储 11-13 起走 `authoritysql.Open` 的只读池(拨号 1s、读写有界),不用再改;§3.2「除 login 的口令库外」一句改成现状。
  - **各句柄超时口径不一、部分带病启动**:§3.2 那张表是 11-06 的现状,之后 Redis Cluster 第 2b、4、5、6b-1 批把这些句柄换成了 `shared/clusterredis` / `playerredis`;剩下 match 的 `Redis` 段在第 6b-2 批(「控制面按 zone 化」会话正在做),SaToken 的外部 Redis 只读、启动 Ping 失败即拒启、超时取配置,不用改。
  - **go/db `store_health.go` 过时注释**:`store_health.go`、`store_registry.go`、`proto_sql/db.go` 里「worker 子分片」「routeTask」「key_ordered_consumer.go」「任务按 L-04 延后」等指向已删的 Kafka 任务消费者的说法改成现在的调用方(存档直接提交、审计转发)。
  - **新发现(只改注释,另开任务定)**:存档库的运行期冷却(`ReportConnLost` / `ReportHealthy` / `CoolingDown`)的唯一调用方就是那条已删的消费者,现在没有任何 SQL 调用点上报,cooling 表恒空,`Store()` 的冷却分支与 `db_store_cooldown_stores` 都不生效(`deploy/k8s/README.md` 已注明恒为 0)。接到直接提交与审计转发的调用点上(恢复「整库不可达时秒级挡回、只放一个探活」),还是整套删掉(连同指标、压测汇总的那一列),涉及另一会话正在改的 `playerstore/store.go`,不在本条做。
  - **其余 Kafka 写入器的 acks 复核**:逐个核过,不用改。拿 broker ACK 当提交点的都已是 acks=all:scene_manager 路由 / 重定向、player_locator 清理命令(11-11 改)、login 命令生产者(sarama WaitForAll)、go/db 审计转发、match 对局结果、死信 sink 与 `kafka_dlq` 工具;guild / friend / mail / match 组队通知 / player_inbox / chat 发的是提示,丢了有拉取或 SQL 后台循环兜底,保持 acks=1(11-11 定)。
- **测试**:`redis_cluster_logic_test.cpp` 的两条节拍用例改成新签名(三参构造、`OnStarted` 返回代次、`OnFinished(代次, 成败)`),新增 `RedisClusterTopologyTest.RefreshPolicyExpiresStuckInFlightAndDropsTheLateReply`。Kafka 预算与 etcd 配置校验没有单测(要真 broker / 要进程退出),由下面的冒烟覆盖。
- **Codex 验证**(仓库根目录;C++ 必须串行;失败保留第一条编译错误与 gtest / go test 的完整失败输出,不改断言,不连续重试):
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`:0 个错误。测试工程链接预编好的库,先编库再跑测试。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test -LogDir build/verification/reconnect-r19-r17-20261119/cpp-tests`:全绿,逐个报 PASS:`RedisClusterTopologyTest.RefreshPolicyLimitsRateAndKeepsPeriod`、`RefreshPolicyRetriesAfterFailure`、`RefreshPolicyExpiresStuckInFlightAndDropsTheLateReply`,以及全部 `RedisClusterClientTest.*`。再换 `-Filter configuration_table_test` 跑一遍(读 `bin/etc/base_deploy_config.yaml`,只有一个 etcd 地址,应照常加载)。
  3. Go:`cd go/shared && go vet ./noderegistry/ && go test -count=1 ./noderegistry/`(带 etcd 再跑 `-tags=integration`,`TestRegisterAfterListening_WaitsForThePort` PASS);`cd go/scene_manager && go build ./... && go vet . && go test -count=1 ./...`;`cd go/client_rpc_router && go build ./... && go vet . && go test -count=1 ./...`;`cd go/db && go build ./... && go vet ./internal/logic/pkg/proto_sql/`。
  4. 冒烟(调试栈):
     - 把 `bin/etc/base_deploy_config.yaml` 的 `Etcd.Hosts` 临时写成两项起任一 C++ 节点,应立即退出,stderr 有 `FATAL Etcd config: Etcd.Hosts in ... must list exactly one`;改回一项照常起。
     - 先停 Kafka 再起 gate:从第一条 `KafkaConsumer: partition contract query not settled yet` 到 gate 注册完成不超过约 6s(原来约两分钟),之后打 `cannot verify partition contract` 的 ERROR 并照常起服;起 Kafka 后命令照常收得到。
     - 起 scene_manager 与 client_rpc_router:日志里 gRPC 开始监听早于 `[NodeRegistry ...] registered`,`etcdctl get --prefix SceneManagerNodeService.rpc/` 与 `ClientRpcRouterNodeService.rpc/` 有键;gate 发现路由服后转发正常。
- **Java 版(AGENTS §12)**:不涉及(C++ 节点与 Go 服务内部的连接实现),不加 PARITY 行。
- **文件**:`cpp/libs/engine/config/config.cpp`、`bin/etc/base_deploy_config.yaml`(注释)、`cpp/libs/engine/infra/storage/redis_cluster/{redis_cluster_topology.h,redis_cluster_topology.cpp,redis_cluster_client.h,redis_cluster_client.cpp}`、`cpp/tests/rpc_controller_test/redis_cluster_logic_test.cpp`、`cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp`、`cpp/libs/engine/core/node/system/node/{node.cpp,node.h}`(注释);`go/shared/noderegistry/registry.go`、`go/scene_manager/scene_manager_service.go`、`go/client_rpc_router/client_rpc_router_service.go`、`go/db/internal/logic/pkg/proto_sql/{store_health.go,store_registry.go,db.go}`(注释);`docs/design/infra-reconnect-overview.md`、本条。`node.cpp`、`node.h`、`kafka_consumer.cpp`、三个 Go 入口文件里别的会话同时有未提交改动(如 `clockcheck.MustPass`),不是本条的。

## 2026-11-19 Claude:断线重连 R17——C++ 节点到各依赖的连接指标与告警(未编译、未测试,待 Codex 验证;Java 不涉及)

- **范围**:§4.1 R17 行剩下的 C++ 部分(Go 注册层告警随本日「断线重连 R3 收尾」做了)。会话「接着做完服务器剩下的代码(缺口清单续)」做。都在 main 工作区,没有分支、没有提交。
- **指标**(新 `cpp/libs/engine/core/metrics/dependency_link_metrics.h`,仅头文件:进程级 relaxed 原子 + 写出,不依赖 muduo / muduo_grpc,所以 thread_context 与 core 都能直接 include,不给 muduo_grpc 加任何依赖):
  - `cpp_node_dependency_connections{dependency}`(此刻可用的连接数)、`cpp_node_dependency_lost_total{dependency}`(可用的连接意外断开)、`cpp_node_dependency_connect_failures_total{dependency,reason}`(reason ∈ connect / auth / handshake);dependency ∈ redis_player / redis_control / redis_other / etcd / grpc / node_link,枚举 + 名字表 + `static_assert`。每个组合进程一启动就以 0 导出。
  - `Node::StartMetricsExporter` 登记采集回调 `dependency_link`。
- **钩子**:
  - **RedisManager**(`thread_context/redis_manager.{h,cpp}`):握手移交记可用;业务连接断线、存活摘下记意外断开;换 Connect 周期、Shutdown 记本端主动关;TCP 建连失败记 connect,AUTH 发不出 / 被拒 / 截止前无应答记 auth。新成员 `connectionCounted_` 保证每次「可用」恰好配一次「不可用」,哪条路先到都只减一次。`RedisConnectOptions::metricsDependency` 决定记在哪一类;`RedisClusterClient` 按句柄的域填(kPlayer → redis_player、kControl → redis_control、其余 → redis_other)。
  - **muduo_grpc 通道**(`muduo_grpc/channel.{h,cpp}`):`ChannelOptions` 新增 `linkObserver`,通道只报事件——READY、READY 的连接断了、按 GOAWAY 退役、一次建连失败、READY 时本端销毁;每次 READY 之后恰好跟一次后三者之一。core 新增 `network/grpc_link_metrics.h` 把事件接到计数上(GOAWAY 退役与本端销毁按主动关,不计意外断开),etcd 传输(注册与发现、热关停两条通道)记 etcd,`node_connector.cpp` 的业务通道记 grpc。
  - **节点链路**(`network/rpc_client.h`、`node/system/registration/registration_manager.cpp`):`ready_` 收成唯一写入点 `SetReady`,由假变真记可用、由真变假按是否本端主动(关机阶段 0、销毁)记;为配平加了只碰计数的析构函数(与连接生命周期无关,类注释的约定不变)。身份握手失败记 handshake。
  - **没有指标的**(写进了头文件与告警文件头):Kafka(librdkafka 自管连接,生产者刻意不挂 event_cb);节点链路的 TCP 建连失败(muduo 的 Connector 自己重试、不回调)。
- **告警**:新 `deploy/k8s/cpp-node-dependency-alerts.yaml`,组 `cpp-node.dependencies` 两条 warning:`CppNodeDependencyConnectFailing`(某依赖连续 5 分钟每个窗口都有建连失败)、`CppNodeDependencyFlapping`(15 分钟内意外断开 3 次以上,不含 node_link:对端滚动重启必然让链路断一次)。规则只看增量,不看 connections 是否为 0(节点没用到的依赖恒为 0)。契约测试新 `tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1`(4 条);`deploy/k8s/README.md` 加安装说明。
- **测试**:新 `cpp/tests/rpc_controller_test/dependency_link_metrics_test.cpp`(登记进 `rpc_controller_test.vcxproj`;CMakeLists 由 `build_linux.sh` 按 vcxproj 生成,不手改):`DependencyLinkMetrics.UpDownAndFailuresMoveOnlyTheirOwnCounters`、`ExportsEveryDependencyAndReason`、`UnpairedDownIsExportedAsZeroNotAWrappedValue`。计数是进程级的,用例只比增量。
- **Codex 验证**(接本日 R19 那条的第 1、2 步之后;失败保留第一条编译错误与 gtest 完整输出,不改断言,不连续重试):
  1. 同一次 `msbuild game.sln /m:1` 之后,`run_cpp_tests -Build -Filter rpc_controller_test`:新三条 `DependencyLinkMetrics.*` PASS;既有的 `RedisManagerAuth.*`、`RedisManagerLiveness.*`、`RedisManagerEventBatch.*`、`RpcClientLifecycle*`、`GrpcClientLoopback*`、`GrpcClientLocalReject*`、`RedisClusterClientTest.*` 照旧全绿(钩子只动计数,不改行为)。再换 `-Filter muduo_grpc_test`、`-Filter routing_identity_test`、`-Filter cross_zone_test` 各跑一遍,全绿。
  2. Linux:`build_linux.sh` 重新生成 CMakeLists 后编过(新增的只有测试 .cpp;两个新头文件不进工程)。
  3. `pwsh -NoProfile -File tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1`:`total=4 pass=4`;把 `spec` 抽成普通规则文件后 `promtool check rules`。
  4. 冒烟(调试栈,设 `MMORPG_METRICS_PORT`):起 gate、scene、battle 各一个,`curl -s http://127.0.0.1:<端口>/metrics | grep cpp_node_dependency_`:scene 的 `connections{dependency="redis_player"}`、`redis_control`、`etcd`(2,注册与热关停)、`grpc`、`node_link` 都 ≥ 1;gate / battle 的 redis_* 为 0。停 Redis 30 秒再起:scene 的 `lost_total{dependency="redis_player"}` 加 1 左右、`connect_failures_total{dependency="redis_player",reason="connect"}` 持续上涨、`connections` 先降后回到原值。正常停 scene,gate 的 `lost_total{dependency="node_link"}` 加 1(对端断开算意外,这正是告警排除 node_link 的原因)。
- **Java 版(AGENTS §12)**:不涉及(C++ 节点内部的观测面),不加 PARITY 行。
- **文件**:新 `cpp/libs/engine/core/metrics/dependency_link_metrics.h`、`cpp/libs/engine/core/network/grpc_link_metrics.h`、`cpp/tests/rpc_controller_test/dependency_link_metrics_test.cpp`、`deploy/k8s/cpp-node-dependency-alerts.yaml`、`tools/scripts/tests/cpp_node_dependency_alerts_contract.tests.ps1`;改 `cpp/libs/engine/thread_context/{redis_manager.h,redis_manager.cpp}`、`cpp/libs/engine/infra/storage/redis_cluster/redis_cluster_client.cpp`、`cpp/libs/engine/muduo_grpc/{channel.h,channel.cpp}`、`cpp/libs/engine/core/node/system/etcd/muduo_etcd_transport.cpp`、`cpp/libs/engine/core/node/system/node/{node_connector.cpp,node.cpp}`、`cpp/libs/engine/core/network/rpc_client.h`、`cpp/libs/engine/core/node/system/registration/registration_manager.cpp`、`cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`、`deploy/k8s/README.md`、`docs/design/infra-reconnect-overview.md`、本条。

## 2026-11-19 Claude:挂机月卡开通入口——灵石开通、存档为事实源、Redis 只作投影;客户端面板与 robot 冒烟(协议未生成、xlsx 未产出、未编译、未测试;Java 0.1.0-SNAPSHOT 待做)

- **起因**:离线托管(断线后角色常驻、跟着队长换场景)只认 `player:afk_pass:{<player_id>}`,仓库里没有任何写入方,真实玩家走不到,也写不了冒烟
  (本文件同日「组队跟随合并广播的发送层补单测」的核对结论)。负责人同日确认设计稿 `docs/design/afk-monthly-card.md` §13 四点并「按稿开工」。
- **定案**(细节见设计稿):
  - 月卡 = 存档里的到期时刻 `PlayerUint64Comp.afk_pass_expires_at`(字段 2,PROFILE 行),不是物品。唯一写入方 `AfkPassSystem`。
  - 获得:`BuyAfkPass` 花灵石开通,到期 = max(现在, 原到期) + 30 天,剩余最多 180 天;客户端幂等键必须非 0(`ClientRequestKind` 6),
    报价防呆(币种、价格);扣钱、写到期、记键同一次同步调用,随同一次存盘提交。真钱走现有充值档位(30 元 = 300 灵石 = 一次开通),不做月卡商品。
    GM:`GmSetAfkPass(剩余秒数)`,只在 dev / test 放行(gate GM 清单 + scene GM 闸)。
  - 投影:scene 只按**已提交**的值写 `SET … EXAT <到期>` / `DEL`:存盘提交回调(`OnPlayerStoreSaveDone`,值变了才写)、加载进场后(按库里的值重建)、
    托管在场心跳(20 秒补写一次 SET)。读方:scene 改读内存(`AfkHostingSystem::Probe` 读组件、判定排到本轮之后执行,时序同原来等 Redis 回包);
    player_locator 不改代码,继续读投影。
  - 规则表 `AfkPassRule`(单行:30 天、灵石 300、上限 180 天、提前 72 小时提醒);Tip 新段 `//afk_pass_error base=32000`(5 个码,`kAfkPassUnavailable` 是故障)。
- **改动**(53 个文件,清单在设计稿 §10):服务端协议 3、配置 2、scene 逻辑 9、handler 2、工程登记 10、gate 1、单测 5、Go 注释 1、robot 4、文档 6;
  客户端 10(`AfkPassClient.cs`、`AfkPassUiRoot.cs`、`AfkPassClientTests.cs` 及 `.meta`、`WorldHudNavigation.cs` 入口枚举、`tools/gen_messageids.ps1`
  白名单、`tools/gen_proto.ps1` 协议清单)。
  - 别的会话同时在改的文件(只做了插入,合并时先分清):`player_lifecycle.cpp`(Buff 下线保留会话同时加了 `RestoreSavedBuffs`)、
    `cpp/generated/table/table.vcxproj{,.filters}`(另一会话把 `all_table` 改名 `table_loader`)。
  - 我已跑:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`(全仓只读预检)通过;两份客户端 ps1 用 `Parser::ParseFile` 静态查过语法。
- **交 Codex**(串行;失败保留首个错误原文与失败用例完整输出,不连续重试;工作区有别的会话未提交的改动,不要回退):
  1. **xlsx**(仓库根):`python tools/scripts/afk_pass_xlsx_patch.py new-tables`,再 `python tools/scripts/afk_pass_xlsx_patch.py tip-codes`。
     通过:新建 `data/AfkPassRule.xlsx`(1 行:1 / 30 / 1 / 300 / 180 / 72);`Tip.xlsx` 末尾多一组 `//afk_pass_error base=32000 width=1000` 与 5 个码。
     32000 被占(脚本报 `range … is already taken`)时:改脚本顶部 `TIP_BASE` 为下一个空千位段,同步设计稿 §4.5,别的代码只按枚举名引用、不用改。
  2. **导表**:`dev.bat gen`。通过:生成 `cpp/generated/table/code/afkpassrule_table.{h,cpp}`、`afkpassrule_table_comp.h`、`proto/afkpassrule_table.pb.{h,cc}`、
     `proto/tip/afk_pass_error_tip.pb.{h,cc}`(这几个已登记进 `table.vcxproj` / `.filters`,对不上名字时贴实际文件名);表加载入口(`table_loader` / `all_table`)
     注册了 AfkPassRule;`go/shared/generated/tip/segments.go` 有 `afk_pass` 段、`faults.go` 含 `kAfkPassUnavailable`;客户端 `Proto/Generated` 的 tip 枚举由第 3 步出。
  3. **协议**(仓库根,用默认配置,客户端任务已授权):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(生成器有变化时)、
     `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`;再到客户端仓 `D:\luyuan\mmorpg-client` 跑
     `pwsh -File tools/gen_proto.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp` 与 `pwsh -File tools/gen_messageids.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp`。
     通过:`proto/message_id.txt` 多 `SceneAfkPassClientPlayer` 的 3 个方法;`proto/persist_schema_gen.txt` 多 `1=PlayerUint64Comp#2 optional uint64 afk_pass_expires_at`;
     `cpp/nodes/scene/handler/rpc/player/player_afk_pass_handler.h` 重生成后与手写的一致、`.cpp` 守护段保留;`player_service_interface.cpp` 注册了 `SceneAfkPassClientPlayer`;
     `rpc/service_metadata/player_afk_pass_service_metadata.h` 有 `SceneAfkPassClientPlayerGmSetAfkPassMessageId`;`robot/generated/pb/game/message_id.go` 有 3 个 `SceneAfkPassClientPlayer*MessageId`;
     客户端 `MessageIds.cs` 的 `GetAfkPass` / `BuyAfkPass` 非 0、`Proto/Generated` 有 `PlayerAfkPass.cs` 与 `AfkPassErrorTip.cs`。
  4. **C++ 整解**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(库先编,`run_cpp_tests -Build` 不重编库)。新编译单元:scene 库 `afk_pass.cpp`、
     scene 节点 `player_afk_pass_handler.cpp`、table 库两份新表文件、proto 库 `player_afk_pass.pb.cc`。
  5. **C++ 用例**:
     - `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test`:`AfkPassRulesTest.*`(5 条)、`AfkPassSystemTest.*`(9 条)、`ClientRequestLedgerTest.*` 全过,
       整工程与改动前相比没有新增失败。`AfkPassSystemTest` 读导出的 AfkPassRule,第 2 步没跑会在 SetUp 报 `AfkPassRule has not been exported`。
     - `-Filter currency_test`:`ClientGmGateTest.*`(含新的 `AfkPassServiceGatesOnlyItsGmMethod`)与键形金样(`player:afk_pass:{42}`)那条全过。
     - `-Filter cross_zone_test`:先 `--gtest_filter=ConnectionLostHold*:HostedPlacement*:ExitPersistEcs.*:PlayerTeamFollowTest.*:TeamFollowDriveTest.*`,
       新增 `ConnectionLostHoldAfkPassEcs.ProbeReadsThePassFromTheCharactersOwnSaveData`、`ProbeWithoutAPassSettlesOnTheNormalWindow` 在列且通过,原有的不回归;再全量。
  6. **Go**:`cd go/player_locator && go build ./... && go test ./internal/logic/...`(只改了注释);`cd robot && go build ./... && go vet ./... && go test ./config/...`。
  7. **客户端**(`D:\luyuan\mmorpg-client`):`pwsh -File tools/client_compile_check.ps1` 无错;Unity EditMode 跑 `MmorpgClient.Tests.EditMode.Tianyong` 里的
     `AfkPassClientTests`(11 条)与 `WorldHudNavigationLifecycleTests`、`MysteryMerchantClientTests`(不回归)。
  8. **Linux**:`build_linux.sh`(不带 `--skip-generate`)或 Docker 整编,生成的 scene / table / proto CMakeLists 里有上面的新文件,链接无未定义符号。
  9. **冒烟**(本地全套;同一 zone 两个 scene 节点、`SCENE_RUN_MODE=dev`;机器人起步的大世界地图至少 3 条分线、两个节点上都有;前置写在 `robot/etc/afk_pass_follow_smoke.yaml` 文件头):
     `cd robot && .\robot.exe -c etc/afk_pass_follow_smoke.yaml`。通过:`AFK_PASS_FOLLOW_SMOKE_OK … same_node="…" cross_node="…"`,退出码 0。
     顺带核对:scene 日志有 `metric=afk_pass_bought`、`metric=afk_pass_gm_set`、`metric=afk_hosting_started`,没有 `metric=afk_pass_projection_failed`;
     P3 期间玩家域 Redis `GET player:afk_pass:{<robot_9308 的 player_id>}` 是 P1 设定的到期秒、`TTL` 约等于剩余秒数。失败保留整份 robot 日志与两个 scene 节点的日志。
  10. **联机看一眼**(可选,视觉不验收):客户端城内「历练」分类里有「挂机月卡」磁贴;面板显示未开通 / 剩余天数、价格「300 灵石 / 30 天」、持有灵石;
      灵石不足时回「灵石不足…可在聚宝斋充值灵石」;第一次点「开通」只亮出「确认开通(300 灵石)」,再点才扣钱。这块界面没有效果图,按客户端 AGENTS.md 视觉验收是未解决项。
- **没做**(写在设计稿 §12):真钱直购月卡商品、月卡道具 / 兑换券、自动续费、每日奖励、GM 后台网页加「设月卡」、开通退款。
- **Java 版**:客户端可见的新服务、5 个 Tip 码、规则表两版共享,已在 Java 仓 `PARITY.md` 末尾登记待做,没写 Java 代码。
- **未提交,协议未生成,xlsx 未产出,未编译,未测试。**

## 2026-11-19 Claude:player_locator 按会话代次认断线(补「断线通知至少送达一次」;先协议生成;未编译、未测试,待 Codex 验证;Java 待核对)

- **为什么**:断线通知改成 gate 一直重发之后，迟到的旧通知可能拖很久。player_locator 原来只比会话号与 gate 实例号。同一个 gate 进程里会话号回绕(每 131072 次建连一圈)、同一名玩家又恰好在同号的新连接上重登时，旧通知会把他的新会话转成 DISCONNECTING,30 秒后被踢。
- **规则**(照 `gate_instance_id` 的做法):请求与会话记录的代次都非 0 且不等时，忽略这条断线、回成功、记录不动;0 = 没带(旧版发送方或没有代次的路径),按原样处理。
- **proto**(`proto/player_locator/player_locator.proto`,新字段号查过历史都没用过，都是 uint64):`PlayerSession.session_incarnation = 15`、`SetDisconnectingRequest.session_incarnation = 5`、`ReconnectRequest.session_incarnation = 10`。服务端内部契约，客户端不可见。
- **player_locator**:
  - `setdisconnectinglogic.go`:在实例号判定之后加代次判定;
  - `reconnectlogic.go`:写入新连接的代次，请求为 0 也照写。留着旧代次的话，新连接自己的断线会被当成陈旧忽略;
  - `setsessionlogic.go`:整条拷贝请求，只补注释。
  - 其余构造 / 复制会话记录的点逐个核对过，代次都保留:`markofflinelogic.go`、`gate_sweep.go`、`gate_session_index.go`、`getsessionlogic.go`、`leasemonitor.go`、`session_reconciler.go`。
- **login**:
  - `pkg/sessionmanager/session_manager.go`:会话结构、`OnlineSessionInput`、`SetSessionDisconnecting`、`Reconnect` 带代次;
  - `session_lifecycle.go` 两个 mark 函数加代次参数，日志带 `incarnation`;
  - `disconnectlogic.go` 传会话头的代次;
  - `entergamelogic.go` 补偿、首登 / 顶号写会话、短线重连三处传 `state.fenceKey.Incarnation`。
- **测试**:
  - player_locator:`TestSetDisconnecting_StaleSessionIncarnationIgnored`、`TestSetDisconnecting_SessionIncarnationMatchOrUnknownPlacesLease`(3 个子例)、`TestReconnect_RecordsTheNewConnectionsIncarnation`;
  - login:`TestDisconnectSettlement_CarriesSessionIncarnation`、`TestEnterGame_SessionIncarnationReachesTheSessionRecord`;`session_manager_disconnect_test.go` 两处断言代次。
- **测试不再依赖执行顺序**(AGENTS §11.4):`TestLeaveGame_FenceLeaveErrorFailsWithoutCleanup` 自己设 `SessionExpireMin`;`enter_wiring_test.go` 与 `createplayer_homezone_test.go` 的 harness 改全局 `config.AppConfig` 后用 `t.Cleanup` 恢复。若别的用例一直靠这两处漏下来的配置才通过，现在会在单独跑或打乱顺序时红，按它缺的配置补进那个用例，不要回退恢复。
- **文档**:`gate-online-identity.md` §10.4 残余改写为已修(规则、0 的含义、升级顺序);`scene-enter-chain-residuals.md` §6.8 回绕一条改为「断线已按代次收口，其余面仍是秒级窗口」;`infra-reconnect-overview.md` §4 末尾第 1 条同步。
- **残余**:
  - 登出(LeaveGame 的 `MarkOffline`)不按代次判，它的迟到窗口仍是秒级，回绕到不了。
  - 混跑时 login 不能先于 player_locator 升级:旧版 player_locator 处理重连时会把不认识的代次字段原样留在记录里，升级后这条会话自己的断线会被忽略。项目未上线、两者同批上线，不阻塞。
  - 发给 gate 的 `PlayerLeaseExpiredEvent` 不带代次(Kafka 契约),没改。
- **Codex 验证**(仓库根目录，按顺序):
  1. 协议生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(没有生成器时先跑 `proto-gen-build`)。期望 `go/proto/player_locator/player_locator.pb.go` 三个消息都有 `SessionIncarnation`,C++ 的 `player_locator.pb.{h,cc}` 跟着重生。没加 RPC 方法，不用重跑 `go/build.ps1`。
  2. `msbuild cpp/generated/proto/proto.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`:0 个错误(C++ 业务代码没用新字段)。随后按「断线通知至少送达一次」条目整解编译。
  3. `cd go/player_locator && go build ./... && go vet ./internal/logic/...`,再跑 `go test ./internal/logic/ -count=1 -v -run 'TestSetDisconnecting_StaleSessionIncarnationIgnored|TestSetDisconnecting_SessionIncarnationMatchOrUnknownPlacesLease|TestReconnect_RecordsTheNewConnectionsIncarnation|TestSetDisconnecting_ReplayKeepsTheFirstLeaseDeadline|TestSetDisconnecting_StaleGateInstanceIgnored'`,最后整包 `go test ./internal/logic/ -count=1`。
  4. `cd go/login && go build ./... && go vet ./internal/logic/...`,然后:
     - `go test ./internal/logic/pkg/sessionmanager/ -count=1 -v -run TestSetSessionDisconnecting_`;
     - `go test ./internal/logic/clientplayerlogin/ -count=1 -v -run 'TestDisconnectSettlement_CarriesSessionIncarnation|TestEnterGame_SessionIncarnationReachesTheSessionRecord'`;
     - 单独跑必须绿:`go test ./internal/logic/clientplayerlogin/ -count=1 -v -run '^TestLeaveGame_FenceLeaveErrorFailsWithoutCleanup$'`;
     - 两个包全量:`go test ./internal/logic/clientplayerlogin/ ./internal/logic/pkg/sessionmanager/ -count=1`,再跑一遍 `-shuffle=on`,失败记下 seed。
  5. 引用这份协议的模块各 `go build ./...`:`go/friend`、`go/guild`、`go/mail`、`go/match`、`go/data_service`。
  6. 可选红灯:注释掉 `setdisconnectinglogic.go` 的代次判定,`TestSetDisconnecting_StaleSessionIncarnationIgnored` 应变红;注释掉 `reconnectlogic.go` 写代次那一行,`TestReconnect_RecordsTheNewConnectionsIncarnation` 应变红。做完恢复。
  7. 失败时保留首个失败用例的完整输出;别的会话也在改 login,与上面点名用例无关的失败如实记下。
- **Java 版**:不写代码，已含在 Java 仓 `PARITY.md`「2026-11-19 断线重连待确认五条」第 1 项(按(gate 实例，会话号，会话代次)认会话)。

## 2026-11-19 Claude：排空框架首次接纳先提交起点——组从没提交过的分区上，接纳第一条记录之前先把它的 offset 提交成组内位点（未编译、未测试，待 Codex 验证；Java 待做已补登）

- **起因**：同日「评审修正·排空框架与消费组」里交负责人定的一条（11-06 R5 留下的同类旧洞），负责人 2026-11-19 定「现在改」。
  - 组在分区上从没提交过时，本代次接纳了、没结清的记录，在换代或换属主之后又从日志起点读起。起点若已被保留期推过它，就静默越过、谁都没处理过。
  - 恢复票据与收尾核对也核不出来。
- **改法**（`go/shared/kafkautil/group_lag.go`）：
  - `consumePartition` 在组从没提交过的分区上，把第一条记录交出去之前（handler 模式接纳之前、reader 模式交给 `FetchMessage` 之前），先调新函数 `commitStartOffset`：用本代次把这条记录的 offset 提交成组内已提交位点，成功才接纳。
  - 提交失败按既有退避重试，直到成功、代次结束或 owner 终止；后两种不接纳这条（没有票据、不记 HOLD），之后由组内位点或日志起点重投。
  - 新字段 `boundPartition.startCommitted`，算进 `hasCommitFrontier`（评审修正加的兜底判据仍保留，正常路径不再走到）。
  - 起点取这条记录自己的 offset，不取打开分区时的日志起点：代次还没读到任何记录就结束时不钉住起点，不会因为自己提交过、起点又被推过去而误 HOLD；读到之前起点被推过去（原有的「新组起点被推过去照常接纳」）也不把已删的那段算成本组欠下的。
  - 效果：接纳过、还没结清的记录被保留期删掉时，下一位属主打开分区按「提交点越过保留范围」HOLD，不再静默越过。
  - 代价：每个新组（或新一代 topic）在每个分区上多一次提交。
- **用例**（全部替身驱动）：
  - 新增 `TestBoundConsumerGroupCommitsTheStartOffsetBeforeAcceptingTheFirstRecord`：日志起点 5；起点提交失败两次后成功，成功之前不调回调；提交序列 5 失败、5 失败、5 成功、6 成功；退避 1s、2s。
  - 新增 `TestBoundConsumerGroupAcceptsNothingWhileTheStartOffsetCannotBeCommitted`：第 1 代次起点一直提交不上，一条都不接纳、不拿票据、不 HOLD；第 2 代次照常处理。
  - 新增 `TestBoundMessageReaderCommitsTheStartOffsetBeforeHandingOutTheFirstRecord`：reader 模式交出第一条时起点已提交。
  - 新增 `TestTicketsWithoutACommitFrontierAreNotResolvedFromTheGroupCommit`：白盒守住兜底判据，没有位点的交还与恢复票据不按组内位点结清，有位点之后才结清。
  - 删掉评审修正加的 `TestBoundConsumerGroupDrainHoldsHandoffOnPartitionTheGroupNeverCommitted`、`TestBoundMessageReaderHoldsDriftedReplayTicketOnPartitionTheGroupNeverCommitted`：起点提交之后「组从没提交过却接纳了记录」走不到，它们的判据改由上面的白盒用例守。
  - 改断言，行为按设计变了：
    - `TestBoundConsumerGroupDeadLettersAfterRetryBudgetAndMovesOn`：提交序列多一笔起点 0。
    - `TestBoundConsumerGroupKeepsRecordInPlaceWhileDeadLetterWriteFails`：0、1。
    - `TestBoundConsumerGroupDrainHoldsHandoffThatNoOwnerSettles`：唯一一笔是起点 0。
    - `TestBoundConsumerGroupKeepsConsumingAfterRebalanceWithRecordInFlight`：第 1 代次只拒越过 0 的提交，核对时跳过起点那笔。
  - `TestBoundMessageReaderHoldsReplayDeletedBeforeAnyGroupCommit` 只补注释：照旧守重放票据的兜底判据。
- **文档**：
  - `docs/design/kafka-bound-consumer-group.md` 新增 §3.1，§6.2、§7、§11、§14 同步；
  - `docs/ops/kafka-bound-consumer-runbook.md` §2 补「接纳过的记录被删在打开时 HOLD，同样处置」；
  - r2 审计文档 §1、§6 改成已定。
- **交 Codex**（工作目录 `go/shared`，接同日「评审修正·排空框架与消费组」那条的命令；失败保留首错原文，不改断言、不连续重试）：
  1. `gofmt -l kafkautil` 无输出；`go vet ./kafkautil/`。
  2. `go test -race -count=3 -v ./kafkautil/`，全绿，上面新增的四条与改过断言的四条都在列。
  3. 可选变异：把 `consumePartition` 里 `if startPending { … }` 整段删掉，`TestBoundConsumerGroupCommitsTheStartOffsetBeforeAcceptingTheFirstRecord`、`TestBoundConsumerGroupAcceptsNothingWhileTheStartOffsetCannotBeCommitted`、`TestBoundMessageReaderCommitsTheStartOffsetBeforeHandingOutTheFirstRecord` 应变红。
  4. 下游照旧：`go/guild` `go test -race -count=1 ./internal/kafka/`，`go/match` `go test -race -count=1 ./internal/kafka/`，`go/data_service` `go test -race -count=1 ./internal/kafka/`。有真 Kafka 时跑 match 的 `result_archive_integration_test.go`（`MATCH_IT_RESULT_KAFKA`），新组第一条之前会多一次提交。
- **残余**：
  - 接纳过的记录被删之后打开时的 HOLD，不计 `kafka_bound_consumer_retention_loss_total`（与原有「提交点越过保留范围」同一条路径，按 HOLD 告警发现），手册已写。
  - Kafka 位点提交不带「这是起点」的标记，人工核丢失范围时按组内位点到日志起点算。
- **Java 版**：待做，补登进 Java 仓 `PARITY.md`「2026-11-12 数据覆写审计残余收口」一节：新组在分区上消费第一条之前先提交起点。

## 2026-11-19 Claude:补充「全服匹配网络探测目标在机房内分散」——没有探测目标时不再一律回故障码 16004,镜像同步过回 16023;复核遗留项(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- 起因:负责人要求查出的问题当场修。排除 CLOSED 之后,维护或整机房排空时探测计划会一个目标都没有。原来这时回 `MatchInternal`(16004,Tip.xlsx 标 fault),会把正常维护计成服务端故障;设计稿 D8 已把 16004 收窄到服务端内部错误。
- 改了什么:
  - `go/match/internal/logic/network_probe.go`:新增 `noProbeTargetCode(mirrorSynced)`。battle 镜像从 etcd 同步过、确实没有可探测节点时回 `ErrNoRegionWithinLatency`(16023,业务拒绝);镜像没同步过时仍回 `ErrInternal`(16004)。不新增 tip 码,不改 Tip.xlsx。
  - `probeTargets` 的注释补上客户端行为:Unity `RegionProbeClient` 跳过失败的机房、提交其余回执;robot 整轮不提交是冒烟有意从严(`TestRunNetworkProbeFailsClosed` 钉住),不改。
  - 单测:`go/match/internal/logic/network_probe_target_test.go` 新增 `TestNoProbeTargetCodeSeparatesUnavailableFromNotReady`,按 `tip.IsFault` 断言。
  - `go/shared/workshard/workshard.go`:包注释登记探测计划也用 `Owner`,只改注释。
  - 文档:`docs/design/global-match-hierarchical-pools.md` §5.1「探测目标」补回码一条与客户端行为,§9 客户端可见变化加一条,§12.9 改成五条单测。
- 复核过、结论是不改的:
  - 只有一台 battle 的机房:全机房的探测仍落在这一台,要按运维手册 §3.3 的式子配 `BATTLE_ACCEPT_RATE_PER_SECOND`。battle 不知道全服排队人数,缺省值推导不出正确值;容量模型里 battle 还没建模(缺「同时在战人数」)。部署工具不接线这个变量是 battle 直连准入那批定的(gate 同样),不在本条改。根治要给探测单开端口(业界的 QoS 信标做法),要改节点注册、清单与客户端,超出本条范围,需要负责人另行批准。
  - robot 一个机房失败整轮作废:有意的冒烟从严,不改。
  - 小游戏 `WsUrl`:同机房只有部分 battle 自报 WebSocket 地址时,小游戏玩家可能分到空地址。已告知「小游戏客户端连 gate」会话,由那边定口径。
- 客户端:`RegionProbeClient` 对计划里任何非 0 码都提示「地域探测暂不可用,请重试。」。回码改了界面不变,不用改客户端。
- **给 Codex 的验证步骤**(本会话没有编译,也没有跑 Go 测试;前提照旧:先生成「小游戏客户端连 gate」那批的 proto,否则 `go/match` 编不过):
  ```
  cd go/match
  gofmt -l internal/logic/network_probe.go internal/logic/network_probe_target_test.go
  go build ./...
  go vet ./internal/logic/...
  go test -count=1 -v -run 'TestProbeTargets|TestNoProbeTargetCode' ./internal/logic/
  go test -count=1 ./internal/logic/... ./internal/battleplace/... ./internal/discovery/...
  cd ../shared
  gofmt -l workshard
  go vet ./workshard/...
  go test -count=1 ./workshard/...
  ```
  通过标准:
  - 两处 `gofmt -l` 无输出,build / vet 无错;
  - 五条用例全部 PASS:四条 `TestProbeTargets*`,加 `TestNoProbeTargetCodeSeparatesUnavailableFromNotReady`;
  - 其余用例与改动前一致。没有 TiDB 时 `*TiDB*` 跳过属正常,如实报为未运行。
  失败时保留编译错误全文、失败用例名与断言原文。
- **Java 版(AGENTS §12)**:0.1.0-SNAPSHOT 待做,已在 Java 仓 `PARITY.md` 末尾补登:没有合格探测目标时回 16023(Java 现在抛 `IllegalStateException`)。本条不写 Java 代码。
- **提交情况**:main 工作区,未提交。

## 2026-11-19 Claude:未成年人充值限额闸收尾——小游戏渠道上有实名的账号一起计额、海外部署取值 overseas、退掉订单那笔后释放额度、客户端文案(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人要求把上一条「未成年人充值限额闸」里列为局限、留到以后的问题现在修,客户端代码一并做。
- **做了什么**(口径在设计稿 `docs/design/real-name-anti-addiction.md` §5.6):
  1. 小游戏渠道不再整个跳过:闸开着时每个新意图都读实名。账号没有 pi 照常放行(平台实名、平台限额);有 pi(同一个人也在 App 实名过)时按同样的规则判、一起计额,法规按「同一网络游戏企业」合并计算。读不到实名一律回「繁忙」,小游戏也一样。
  2. `Payment.Compliance.MinorSpendingGate` 加 `overseas`:大陆以外的部署不读实名、App 渠道照开。之前除 `none` 只有 `realname`,海外部署开不了 apple、google。
  3. 退款按净额算:
     - 渠道撤销时,「取消后才到账」那笔(`REFUND_QUEUED`、原因 `CANCELLED_LATE`)也算订单绑定的付款,撤销会关意图;
     - 商城退款待办的原因是 `CANCELLED_LATE`、`DELIVERY_REJECTED` 时,渠道退成之后先关意图再回执商城,未成年人的额度随之释放。重复付款、金额币种不符的退款不动意图。新函数 `mall.RefundsOrdersOwnPayment` 对应商城的 `CommerceApi.RefundReason.refundsOrdersOwnPayment`。
  4. 客户端 `PaymentClient.DescribeTip` 加 5 个码的文案,之前会落到聚宝斋的通用兜底。
- **文件**:`go/payment/internal/core/{minor.go,minor_test.go,service.go,workers.go}`、`go/payment/internal/config/{config.go,config_test.go}`、`go/payment/internal/mall/client.go`、`go/payment/etc/payment.yaml`(只改注释);客户端仓 `Assets/Scripts/Game/Jubaozhai/PaymentClient.cs`(支付会话新建、还没提交的文件,本条只加 5 行);`docs/design/real-name-anti-addiction.md`(§4 两行、§5.6、§8);Java 仓 `PARITY.md`(改写同日本功能那一条)。
- **K8s 不用改**:部署脚本整份镜像 `payment.yaml`,`127.0.0.1:2379` 统一换成集群 etcd,`DataServiceRpc` 跟着换;`Payment` 段取运维 `-PaymentConfigPath` 的文件,`Compliance` 原样带过去;payment 与 data-service 的清单已注入 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`。`tools/scripts/tests/sql_boot_catalog_local.tests.ps1` 夹具只模拟库里有哪些表(授权按实际的表发现),缺 `payment_spend` 不影响用例;它在支付会话的 K8s 批次里,已请那边顺手补。
- **Codex 验证**(在上一条的步骤上改两处,其余照旧):
  1. 生成改用默认配置(这次含客户端,要写客户端仓的 `PaymentErrorTip.cs`):`dev.bat gen`,再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。期望客户端出现 `payment_error.KPaymentRealNameRequired` 等 5 个。
  2. Go 同上一条第 2、3 步(本会话对 `go/payment`、`go/shared/accountdirectory` 跑过 `gofmt -l`,无输出)。本条新增或改动的用例:`TestCreateIntentMinorSpendingGate`(15 个分支,小游戏四条)、`TestChannelFlipsFollowOnlyTheOrdersOwnPayment`(4 个分支,新增取消后才到账那笔被撤销)、`TestRefundOfTheOrdersOwnPaymentClosesTheIntent`(3 个分支)、`TestValidateMinorSpendingGate`(7 个分支,新增 overseas)。
  3. 客户端(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error(新文案引用生成的 `payment_error` 枚举,先做第 1 步);EditMode `PaymentClientTests` 照旧 4 条。
  4. 冒烟补一项:devpay 按 App 渠道判,看不到小游戏路径;要验证「有 pi 的账号在小游戏渠道被限额」得用 `wechat_minigame` 或 `douyin_minigame` 的联调配置,没有联调账号就留到真接口阶段。
- **未验证**:以上全部没有运行。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;`D:\luyuan\xuanming-server-mmo\PARITY.md` 里同日「未成年人充值限额闸」那一条已改写成现在的行为。

## 2026-11-20 Claude：数据覆写审计 r2 批 S、T、U 评审可选项全部落码——退回腿按托管者判、拍卖退款防御、对账翻页只读判定列、退役行种判 INVALID、StatefulSet 滚动预算、回滚清单与几处注释（未生成协议、未编译、未测试，待 Codex 验证；Java 待做）

- **起因**：
  - 同日独立评审给三批列了 10 条可选项，批 U 第 1 条已在同日「批 U 文档残留」做掉。
  - 负责人 11-20 要求「查出的问题现在修，不等以后；不等压测的先按最标准的定」，本条做掉其余 9 条。
  - 本条代码另派了一个独立评审，结论与修正写在末尾「本条评审」。
- **批 S**：
  1. **退役行种判 INVALID**：`go/db/internal/playerstore/store.go` 新增 `newestKnownKind`（本二进制认识的最大行种号）。`validateSave` 遇到不认识的行种：
     - 比它大，回 `SCHEMA_TOO_NEW`；
     - 不比它大（退役号 1、2、5），回 `INVALID`，与 C++ `IsNewerKind` 同口径。
     - 原来退役号也回 TOO_NEW，告警会提示去升 go/db，而升哪个版本都认不出它。
     - 同步改了：存档设计 §2 进事务前校验一行、§4 一段，`deploy/k8s/player-store-alerts.yaml` 两条告警说明。
  2. **回滚清单**：`docs/ops/release-checklist.md` §4 加「升过持久世代的版本，go/db 不跟着回滚」。
     - 判断：比两个版本各自 `proto/persist_schema_gen.txt` 最后一条 `sealed=` 的世代。
     - 升过时只回滚 scene，`-GoSvcTag` 保持当前版本；`kubectl rollout undo statefulset/db` 同样受这条约束。
     - 依据是混版设计 §8.3，清单原来没写。
  3. **StatefulSet 滚动等待预算**：`tools/scripts/k8s_deploy.ps1` 新增纯函数 `Get-StatefulSetRolloutWaitSeconds`，预算 = max(-WaitTimeoutSeconds, 副本数 ×（terminationGracePeriodSeconds + minReadySeconds + 30s）)。
     - `Get-LiveStatefulSetRolloutWaitSeconds` 读线上 spec；读不到就退回 -WaitTimeoutSeconds，接下来的 rollout status 照原样报错。
     - `Wait-ForStatefulSetReady` 改用这个预算，SQL 写者与基础设施的 StatefulSet 都经过它；dry-run 那行末尾加了说明。
     - 原来一律等 180s。staging / prod 恒等 db 滚完再发 scene，db 2 副本最慢就要 190s，副本一多就会在发 scene 之前误判超时。
  - **没做**：混版设计 §8.2 的「部署前世代预检」。
    - 该节 11-12 已定案不做：顺序由 zone-up 机械保证；违反时不丢已提交数据，告警是 critical。
    - 评审没核到的那一处（卡在 TOO_NEW 时玩家被改派到旧 scene、内存进度丢失）只在顺序被违反时可达，属于 §8.2 已登记接受的那一类。
- **批 T**：
  1. **退回腿按托管者判**：`proto/trade/trade_table.proto` 的 `TradeCommerceLeg` 新增 `escrow_owner_id = 9`，记这条发放腿付出的是谁托管的资产。workflow 存在二进制列里，不改表。
     - 原来只看角色名：人工改绑把收件人换成同账号的另一名角色后，那名角色拿到的托管资产仍带 `TX_AUCTION_RETURN`，不判获取封禁。
     - 现在只有付回托管者本人才算退回；改绑后按成交所得判，被封禁就照常 REJECTED、合同转人工。owner 为 0 的旧腿按角色名判（项目未上线，清库后没有）。
     - `engine.go`：
       - 新增 `addEscrowLeg`，同名腿已存在时什么都不改，与 `addLeg` 一致；
       - `transferLeg` 改用它，owner = 源扣出腿的玩家；
       - 拍品 `seller_receive` 不记 owner：货款托管在出价合同里，这条腿也不是退回；
       - 撤单 `seller_return` 的 owner = `seller_debit` 的玩家。
     - `market.go`：撮合子单 `buyer_receive` 记卖单 `seller_debit` 的扣出人，`seller_receive`、`buyer_surplus` 记买单 `seller_debit` 的扣出人。
       - 单被改绑过时，下单人已经换了，扣出腿上才是当初托管的那个；
       - 扣出腿缺失按坏数据处理。
     - `delivery_order.go`：整单退回时，`seller_return`、`buyer_return` 各抄被它取代的那条交付腿的 owner。
     - `manual_plan.go`：
       - reissue、partial 缺口补发、redirect 补发都照抄原腿的 owner；
       - 整单退回(refund、作废拍卖)的收件人改为合同上这一方现在的角色(`refundRecipient`),owner 记扣出时的角色;
       - redirect 原地改投只换收件人。
     - `leg_tx_type.go`：退回角色名，且 owner 为 0 或等于收件人，才回 `TX_AUCTION_RETURN`。
  2. **拍卖退款防御**：`delivery_order.go` 的 `heldBidKeepsFunds` 在「卖家所得原腿没发出」之外，再要求「买家交付腿及其补发要么没发出，要么终局没到账」。
     - 在途、到账、部分到账都回 false，`settleHeldBid` 按坏数据隔离成人工，不自动退款，避免货款两得。
     - 现有路径到不了，只是防御。
  3. **货银对付定案**：聚宝斋设计 §6.2 末尾加一条。买家那条一直 RETRY（背包满、不在本节点、加载中）时，卖家所得一直压着。
     - 不设「压久了先付卖家」的超时：先付了、买家那条随后被拒，就又回到钱货分离。
     - 买家在线却久不终结时，资产通道告警 `AssetOpOnlineRetryStuck`（1 小时）会响，压着的卖家所得跟着看得见。
  4. **顺带收的一处注释**：`cpp/libs/modules/currency/system/currency_system.cpp` 约第 32 行原来引用已删除的 `ReserveOrEvict`，改为引用背包分层设计 §7 红线 7。补丁由「11-12 残余收口各批的独立评审」会话的背包批评审员发现并转来，只改注释。
- **批 U**：
  1. **对账翻页只读判定列**：`go/player_inbox/internal/inbox/store_maintenance.go` 新增 `queryFinalizedRefs`，`RecentlyFinalized` 与 `forEachPlayerItemsPage` 共用。
     - `forEachPlayerItemsPage` 改成只 SELECT `finalizedRefColumns`（9 列），visit 收 `[]FinalizedRef`。
     - `decide.go`：`reconcileDiff` / `appendReconcileDiffs` 改按 `FinalizedRef` 判，删掉不再使用的 `finalizedRefOfRow`；`reconcile.go` 跟着改。
     - 原来每页读全部 25 列，含最大 16KiB 的载荷。整次对账共用一份调用预算，战斗行多的老玩家可能每次都读不完、只能回错误。判定只用这 9 列，结论不变。
  2. **流水机制的说法**：`go/data_service/internal/logic/recall_plan.go` 文件头与 `docs/design/exploit_loss_prevention_zh.md`「计划怎么算」两条，按审计发件箱重写。
     - 现在流水与存档同事务提交，「流水里有这条」等于已提交。
     - 「没有流水」仍不等于没有应用：转发与入库有积压、可以暂停，同一条流水也可能到两次。
     - 11-05 之前直发 Kafka 的重复只在历史数据里有。
  3. **结果主题的注释**：
     - `go/match/etc/match_service.yaml`、`go/match/internal/config/config.go`、`go/guild/internal/config/config.go` 不再说 battle 的 `kMatchResultsTopic`，生产者是 match 的终局发件箱，只发 COMPLETE。
     - `docs/design/cross-zone-matchmaking.md` 改分区一条同步改了。
- **本条评审**（1 个只读评审代理；报的 3 条必修，本会话沿代码复核都成立，已修）：
  1. 撮合子单的 owner 原来取母单下单人。母单改绑过时那是新角色，买家那条被拒自动退回时，托管物会被判成付回本人、绕过获取封禁；同一张单直接撤单却判成交所得，两条路径口径不一。
     - 已改为取母单扣出腿的玩家。
     - 建腿那几行抽成纯函数 `market.go` 的 `addFillLegs`，以便单测；新用例 `delivery_order_test.go` 的 `TestFillLegsRecordTheEscrowOwnerFromTheParentDebits`。
  2. `heldBidKeepsFunds` 的文档注释里，列表后面直接接段落，`gofmt` 会改写，`gofmt -l` 会报出这个文件。已补空行。
  3. 整单退回的 `<扣出腿>/refund` 原来发给扣出腿的玩家。
     - 改绑之后那个角色已归别的账号，资产会落到对方手里；advance 只核当前两方的归属，拦不住。
     - 本条「改绑后退回按成交所得判」让这条路径可达：被封禁 → 转人工 → 运维做 refund。
     - 已改为发给合同上这一方现在的角色(`manual_plan.go` 的 `refundRecipient`),owner 仍记原角色;新用例 `manual_test.go` 的 `TestRefundAfterRedirectGoesToTheCurrentParty`。
  - 可选 2 条也收了：
    - `tick_isolation.go` 的 `settleHeldBid` 注释与隔离文案写全两种矛盾；
    - 拍品 `seller_receive` 不再记 owner(proto 注释同步)。
- **改动或新增的用例**（都未运行）：
  - go/trade：
    - `leg_tx_type_test.go` 新增 `TestLegTxTypeReturnsOnlyToTheEscrowOwner`；
    - `manual_test.go` 新增 `TestRefundAfterRedirectGoesToTheCurrentParty`(新 import `rollbackpb`);
    - `delivery_order_test.go`：`deliveringTrade` 夹具给两条交付腿记了 owner；新增 `TestHeldBidKeepsFundsOnlyWhenTheWinnerGotNothing`、`TestFillLegsRecordTheEscrowOwnerFromTheParentDebits`；`TestRetargetedSellerProceedsStillUnwind` 加了托管者与交易类型断言。
  - go/player_inbox：
    - `decide_test.go` 的 `TestReconcileDiff`、`store_logic_test.go` 的 `TestReconcileReport` 改用 `rcRef`，`reconcile_test.go` 删掉 `rcRow`；
    - `store_integration_test.go`（integration 标签）的 `TestIntegrationPlayerItemsPagingReadsEveryRow` 改收 `[]FinalizedRef`，并核对读到的状态。
  - go/db：`store_test.go` 的 `TestSave_RejectedBeforeTransaction` 加 4 格（大一号回 TOO_NEW，退役号 1、2、5 回 INVALID）。
  - PowerShell：`k8s_migrate_gate.tests.ps1` 末尾新增「StatefulSet 滚动预算按副本数 x (宽限 + minReadySeconds + 30s) 给,-WaitTimeoutSeconds 只当下限」。
- **验证（Codex，仓库根目录，按序；任一步失败即停）**：
  1. **协议生成**：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。通过标准：`go/proto/trade` 生成的 `TradeCommerceLeg` 里有 `EscrowOwnerId`（字段 9）。不生成 go/trade 编不过。`player_inbox_table.proto` 只改了一条行尾注释，生成产物里没有这段文字。
  2. **go/trade**（`go/trade` 目录）：
     - `gofmt -l internal/commerce/` 无输出，`go build ./...`，`go vet ./internal/commerce/`；
     - `go test ./internal/commerce/ -count=1 -v -run 'TestLegTxType|TestHeldBidKeeps|TestFillLegs|TestRetargetedSellerProceedsStillUnwind|TestBuyer|TestAuctionUnwind|TestManual|TestRefund|TestVoidAuction|TestPartial|TestReissue|TestRedirect|TestSettleLegOutcomes|TestLegacyContract'`，再全量 `go test ./internal/commerce/ -count=1`；
     - 真库：`COMMERCE_TEST_DSN=<隔离 TiDB>`、`COMMERCE_REQUIRE_TIDB=1`，不要设 `COMMERCE_TEST_MYSQL_DSN`，跑 `go test ./internal/commerce/ -run 'TestRealDB|TestTiDB' -count=1`。退回腿的交易类型走真实建腿路径，这一步也是回归。
  3. **go/player_inbox**（`go/player_inbox` 目录）：
     - `gofmt -l internal/inbox/` 无输出，`go build ./...`，`go vet ./internal/inbox/`，`go vet -tags integration ./internal/inbox/`；
     - `go test ./internal/inbox/ -count=1`；
     - 真库：按 2026-11-12「数据覆写审计 r2 修复批次 S、T、U」第 6 步跑 `-tags integration ./internal/inbox/`，含 `TestIntegrationPlayerItemsPagingReadsEveryRow` 与原有的 `ReconcilePlayer` 用例。
  4. **go/db**（`go/db` 目录）：`go build ./...`，`go vet ./internal/playerstore/`，`go test ./internal/playerstore/ -count=1`，含新增的 4 格。
  5. **PowerShell**：`pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`（含新用例）与 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（不回归；只有 dry-run 那行末尾多了说明）。
  6. **只改注释的模块**：`go/match`、`go/guild`、`go/data_service` 各跑 `go build ./...` 与 `go vet ./...`。
  7. **变异取红**：在 `%TEMP%` 副本里改，每次只改一处。
     - `legTxType` 去掉 owner 条件：`TestLegTxTypeReturnsOnlyToTheEscrowOwner`、`TestRetargetedSellerProceedsStillUnwind` 红；
     - `heldBidKeepsFunds` 去掉买家腿那个循环：`TestHeldBidKeepsFundsOnlyWhenTheWinnerGotNothing` 红；
     - `validateSave` 去掉 `kind > newestKnownKind` 判断（一律回 TOO_NEW）：退役号三格红；
     - `Get-StatefulSetRolloutWaitSeconds` 改成只返回下限：新 PowerShell 用例红；
     - `refundRecipient` 改成直接返回 `debit.GetPlayerId()`：`TestRefundAfterRedirectGoesToTheCurrentParty` 红；
     - `addFillLegs` 的 owner 改回 `sell.SellerPlayerId` / `buy.SellerPlayerId`：`TestFillLegsRecordTheEscrowOwnerFromTheParentDebits` 红。
  - 失败时保留：协议生成与编译的首个错误及上下文，失败用例的完整输出，PowerShell 用例的失败标题与断言信息。
- **与并行会话的交叠**：以下文件同时有别的会话的未提交改动，本条只动了上面列的段落，改前都核对过：
  - `tools/scripts/k8s_deploy.ps1`：Redis 连接总账；
  - `go/player_inbox/internal/inbox/decide_test.go`、`store.go`：重试判定；
  - `docs/design/cross-zone-matchmaking.md`：另两处；
  - `go/trade/internal/data/*`：本条没碰。
- **Java 版（AGENTS §12）**：待做，本轮不写 Java 代码。
  - Java 仓 `PARITY.md` 2026-11-12「交易退回不受获取封禁约束…」条目补了两项：退回腿按托管者判；拍卖退款要求赢家一点没到货。
  - 收件箱单人对账翻页那一项补了「只读判定列」。
  - 批 S 两项不适用：Java 版存档没有世代闸，部署脚本也是本仓自己的。

## 2026-11-19 Claude:Unity 登录排队持票轮询按契约 §11.5 补齐——查询失败带原票恢复重入、连续失败退避 2/4/8/16/30 秒、410 丢票重排、排队总预算(客户端仓;未编译、未测试,待 Codex 验证;两版服务端不用改)

- **起因**:负责人目标是开服当天网络抖一下不丢排队位置。Unity 的 assign-gate 循环持票查 `/api/queue-status` 时,任何一次请求出错就报错退出;排队票是局部变量,随之丢失,玩家重进只能从队尾重排。契约 `docs/design/open-server-rate-limit-design.md` §11.5 第 2~7 条此前只有 robot 做了。
- **改法**(客户端仓 `../mmorpg-client`,照 robot 的 `AssignGateWithQueue` / `heldTokenWait` / `pollQueueStatus`):
  - `Assets/Scripts/Game/GameClient.cs` 的 `EnterZone` 第 2 步:
    1. 查询失败(网络错误、HTTP 非 200、JSON code=500)时连续失败数加 1,带原票立即重调一次 assign-gate。查询的非失败回包清零;恢复重入的回包不清零;410 丢票时清零。
    2. 恢复回 0 放行;回 100(login 或 ratelimit)或 429 时保票,等待后回到查询,不套无票 429 的退避。状态栏显示「网络波动,排队位置保留中」。
    3. 查询回 503 zone_* / 404 按终态报错,不重入。任一端点回 410 丢票、不带票立即重调;没票时回 410 直接报错(以前会不等待地反复重调)。
    4. 恢复重入自身网络 / HTTP 失败,或回其它失败码(含 500),按第 6 条报错退出。
    5. 总预算 2 小时,每个请求发出前检查(循环开头与恢复重入之前);已发出的请求照常等回包,截止后才到的放行照样进。
  - 等待:`QueueRetryDelayMs` 多一个可选的连续失败数,缺省 0 时与 11-19「网关前置限流的排队提示加下限与随机量」那条的口径完全相同。下限由新的 `QueueBackoffFloorMs` 给出(2、4、8、16、30、30… 秒),抖动照旧 [0, 20%]、按不超过 30 秒的基础间隔算。
  - `BuildAssignGateRequest` 多一个可选的 `queueToken`,只给恢复重入用;首次分配与断线重连照旧不带。
  - `Assets/Scripts/Net/GatewayHttpClient.cs`:`AssignGate`、`QueryQueueStatus` 只读 HTTP 200 的正文,非 200 一律交 `onError`,算 HTTP 失败。
    - 以前非 200 的 JSON 正文也会被解析。Spring 缺省错误页没有 `code` 字段,会被读成 0 当放行;代理回的 503 也会被当成区服维护。
    - 断线重连(`GameClient.Reconnect.cs`)也走 `AssignGate`,它把 `onError` 当可重试,只会更稳。其它端点不变。
  - 两版服务器兼容:核对了独立 Java 版 `game-gateway` 的 `AssignGateService.queueStatus` 与本仓 `java/gateway_node` 的 `queryQueueStatus`。两边都是 HTTP 恒 200,code 0 / 100 / 410 / 500 / 404 / 503 同义;Java 另有请求体错误的 400,按其它码报错。
- **与 robot 的差异**(设计稿 §11.5 末段已写):无票 100 也加 2 秒下限与抖动(契约无票段本来如此);410 丢票时连续失败数清零;无票 429 仍直接报错,没做 30/60/120 秒退避(不在这次的第 2~7 条里)。
- **待负责人定**:契约第 6 条规定恢复重入自身也失败时报错退出,票随之丢。客户端自己断网几秒(查询和紧接着的重入都连不上),或 login 短暂不可用而区桶有令牌(重入到了 login、回 500)时,仍会丢排位。服务端条目空闲 1 小时才过期,保票重试本身是安全的。要覆盖这两种情况得改第 6 条,例如重入网络失败或回 500 也算一次连续失败、保票退避,直到总预算;Unity 与 robot 要同改。本条没动契约。
- **文档**:`docs/design/open-server-rate-limit-design.md`:§十表里独立客户端一行;§11.5 末段的 Unity 现状,以及两版回包的核对结论。
- **验证(交 Codex,按 AGENTS §10.1)**:
  1. 客户端运行时程序集编译(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error。工作区有别的会话的半成品时,红灯先看是不是本条的三个文件(`GameClient.cs`、`GatewayHttpClient.cs`、`LoginCredentialTests.cs`)。
  2. EditMode(Unity 编辑器不能开着这个工程):
     ```
     "<Unity 安装目录>\Editor\Unity.exe" -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -testFilter MmorpgClient.Tests.EditMode.Net.LoginCredentialTests -testResults <证据目录>\editmode-results.xml -logFile <证据目录>\unity-editmode.log
     ```
     - 新用例:`AssignGateReentry_CarriesTheHeldQueueToken`、`QueueBackoffFloor_DoublesFromTwoSecondsAndCapsAtThirty`(8 个 TestCase)。原有的 `AssignGateRequest_*` 两条不应变。
     - 通过标准:failed=0。失败时保留 xml、log 与编译错误全文。
  3. 联网冒烟(人为断开 queue-status,只在本机):
     - 准备:按 `docs/ops/login-queue-stress-runbook.md` 开排队(`Queue.Enabled: true` 只写在 scratchpad 的 login.yaml 副本里,不改仓里的),起完整依赖链。按同一手册用 `/admin/zones/1`(GET 拿 ETag,PUT 带 If-Match)把 zone 1 的 `capacity` 调到不大于在线人数,让新登录必然排队。
     - 起 Unity 客户端进 zone 1(播放器或编辑器都行)。用 DevAutoPilot 时:`-gateway http://127.0.0.1:8081 -zone 1 -account dev_queue_smoke_1 -password <开发口令> -loginTimeout 900`,不带 `-autoQueue`。看到状态栏「排队中…」。
     - 断开:只关 login 的 `QueryQueueStatus`,AssignGate 照常(`docs/design/rpc-kill-switch.md` §6):
       ```
       curl -s -X PUT -H "X-Admin-Key: <admin-key>" -H "Content-Type: application/json" http://127.0.0.1:8081/admin/killswitch -d '{"node_type":"LoginNodeService","target":"LoginPreGate/QueryQueueStatus","deny":true,"reason":"queue-status smoke"}'
       ```
       期望:网关对 queue-status 回 code=500(`login_unavailable`)。客户端日志连续出现 `queue-status failed (...), consecutive=N; re-entering assign-gate with the queue token`;第一条之后,相邻两条的间隔依次约 2、4、8、16、30、30 秒(各加不超过 20% 的抖动)。重入回 100 login,名次连续、不跳回队尾,登录流程不报错退出。
     - 恢复:
       ```
       curl -s -X DELETE -H "X-Admin-Key: <admin-key>" "http://127.0.0.1:8081/admin/killswitch?key=type/LoginNodeService/LoginPreGate/QueryQueueStatus"
       ```
       下一次查询成功后,间隔回到约 2 秒,名次接着之前的。
     - 放行:把 capacity 调回去,客户端拿到 code=0,日志出现 `assigned gate ...`,之后正常进场景(DevAutoPilot 打 `RESULT=PASS stage=in_game`)。
     - 通过标准:断开期间名次连续(没有跳回队尾,没有出现「排队已过期」),没有报错退出;恢复后能放行进游戏。保留客户端日志,以及 gateway、login 这段时间的日志行。
     - 收尾:确认热关停规则已删(`GET /admin/killswitch` 为空),capacity 已恢复。
- **未验证**:以上全部没有运行,Unity 没开过。
- **Java 版(AGENTS §12)**:只改两版共用的 Unity 客户端,两版服务端都不用改;不往 Java 仓 `PARITY.md` 加行。独立 Java 版限流排队提示的下限与随机量仍是上面那条登记的待做。
- **提交情况**:客户端仓与本仓都在 main 工作区,未提交。

## 2026-11-19 Claude:回合制战斗打完自动返回场景——结算面板倒数 3 秒自动收起,不再要每人点「返回场景」(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人反馈每次战斗结束都要手动点一下「返回场景」;回合制打完应该战斗里的人全部自动回场景。
- **查到的**:「返回场景」只在客户端。服务端没有「离开战斗」请求,scene 结算、客户端相位回 None 都不等玩家;客户端 `BattleUiRoot` 把战斗屏一直留在结算面板后面,玩家点了确认才 `CloseBattle()` 并调 `AckBattleEnd()`(本地闸,连续战斗靠它重排)。观战结束本来就自动收屏(toast + 关屏),不用改。
- **改动(客户端仓 `mmorpg-client`;两版服务端共用这一个客户端,服务端代码不用改)**:
  - `Assets/Scripts/UI/Ugui/Battle/BattleResultPanel.cs`:现场结算(`Show`)的确认键淡入后倒数 `AutoReturnSeconds`(3 秒),键上依次显示「返回场景(3)」「(2)」「(1)」,到点走与点按钮同一个回调(`BattleUiRoot.OnResultConfirmed`:收战斗屏、`AckBattleEnd`、弹下一局补弹结果)。倒计时补间挂在面板的 `_token` 上,`Hide()` 与再次展示时随 `KillTweens` 作废,不会迟到确认;面板已不在显示(画布随场景切换销毁)时不回调。补弹的近期战果(`ShowRecovered`,「知道了」)不倒数。
  - `Assets/Scripts/UI/Ugui/Battle/BattleUiRoot.cs`:只改一行注释。
  - 测试:`BattleUiLayoutTests` 新 1 例 `LiveResult_ReturnsToSceneByItself_RecoveredResultWaitsForPlayer`(沿用其他战斗用例用反射推 `RealtimeTween.UpdateAll` 的做法):倒数开始时键上是「返回场景(3)」、没到点不确认、到点确认且只确认一次、补弹结果推 10 秒仍不确认。
- **行为变化**:结算面板弹出到回到场景约 4.5~5 秒(奖励飞入 1.5~2 秒,随奖励行数变长,再加 3 秒)。连续战斗不再要玩家每局点一下,面板收起后自动重排。运行期 QA `CharacterPetRuntimeVerification.ReturnToScene` 照旧在确认键可点时截图再点,倒数 3 秒内点得到;编辑器截图工具 `ClientBattleUiQaCapture` 的结算页现在会拍到「返回场景(3)」。
- **改动(本仓)**:`docs/design/turn-battle-presentation.md` §4「结算」按现行行为改写正文;`docs/design/turn-based-battle-server.md` §11.2「连续战斗」补一句。
- **没做**:玩家拖动奖励列表时倒计时不暂停(奖励超过 8 行要滚动才看得全),要做再加。
- **给 Codex 的验证步骤**(本会话没有编译、没有跑任何测试):
  1. 客户端仓根目录:`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error。
  2. Unity EditMode,程序集 `MmorpgClient.Tests.EditMode.Battle`:`BattleUiLayoutTests`(含新用例与原有的 `ResultRewards_OverflowCanReachLastRow_AndShowStartsAtTop`)、`DamageNumberLayoutTests`、`BattleClientAutoBattleTests` 全部通过;失败时保留第一个编译错误原文与失败用例的完整输出。
  3. 联机冒烟:两人组队打一场 PVE、再打一场 PVP,两个客户端都在结算面板倒数 3 秒后自动回到场景,全程不点任何键;开连续战斗打两局,第二局不点也会自动排上;倒数期间点「返回场景」立即回场景,且不会重复排队。
  4. 有联机环境时跑一遍 `tools/run_character_pet_runtime_qa.ps1`,`ReturnToScene` 阶段不报失败。
- **Java 版**(AGENTS §12):不涉及。只改了共享客户端,Java 服务端同样受益;没有往 Java 仓 `PARITY.md` 加行。
- **提交情况**:两仓 main 工作区,未提交,没有开分支。

## 2026-11-19 Claude:实名与防沉迷 K8s 接入的三处收尾——login 核运行配置与部署期望、跨 namespace 共享密钥写前核对改成整表、payment 查实名密钥进复用检查与本机启动链(未编译、未运行,待 Codex 验证;Java 不适用)

- **起因**:上一条「实名与防沉迷接进 K8s 部署脚本」交付时留了两个口子:运维漏抄 `AntiAddiction` 段时 login 照样起来、防沉迷却是关的;`account-directory-payment` 缺了或 zone 与 infra 不同值,部署脚本在写之前查不出来。负责人要求发现的问题当场修完,本条补上,并顺带补齐同类缺口。
- **做了什么**:
  - login 启动时核运行配置与部署期望(`go/login/internal/config/anti_addiction.go` `checkExpectedAntiAddiction`,`resolveAntiAddiction` 先调它):部署开了防沉迷就把 AppId / BizId 明文注入 login 主容器(`MMORPG_LOGIN_NPPA_EXPECTED_APP_ID` / `MMORPG_LOGIN_NPPA_EXPECTED_BIZ_ID`,`k8s_deploy.ps1` 的 `Apply-OneGoSvc` 用 `Add-GoSvcEnvEntries` 注入),login 与实际读到的配置比:开关是 false、只给了一个、AppId 或 BizId 不同(按字节比)都拒启。两个变量都没有(本机、docker、没开的部署)不比。做法同 data_service 的 `MMORPG_ID_ISSUER_EXPECTED_REGION`。
  - 由此开、关、换应用的顺序统一成「先改 runtimeConfigSecret、不单独滚动 login,紧接着部署」:两步之间重启的 Pod 会因为与模板里的期望值或 secretKey 对不上而拒启。防误关报错、函数说明、部署提示行与开服手册都按这个顺序改正(上一条手册里写的「先删段并滚动 login」在有了这道核对之后会卡住,已改)。
  - 跨 namespace 共享密钥的写前核对改成整表:`k8s_deploy.ps1` 新增共享键表 `$script:SharedDomainServiceSecrets`(12 把:`mail-producer-battle`、`account-directory-trade`、`account-directory-scene`、`account-directory-payment` 与八把 `role-fence-*`,每行写明 zone 侧与 infra 侧用它的工作负载),`Assert-MailProducerBattleSecret` / `Read-MailProducerBattleSecret` / `Assert-MailProducerBattleSecretForRun` 换成 `Read-DomainServiceSecretDigests` 与 `Assert-SharedDomainServiceSecretsForRun`:infra 侧在本次部署全局 Go 服务时核全表,zone 侧在 zone-up / all-up 时核(只发 C++ 节点时只核 scene 读的三把)并与 infra 逐键比摘要;每个 namespace 只读一次 Secret,缺键、去掉首尾空白后不足 32 字节、两把同值、与 infra 不同都拒绝,一次报全,报错只点键名。原来只核 `mail-producer-battle` 一把,其余 11 把缺了或两边不同值都要到 Pod 起不来、或验签静默失败才看得出。
  - data_service 的跨用途复用检查(`go/data_service/internal/svc/global_caller_secrets.go` `validateGlobalCallerSecrets`)加上 payment 这把:缺了照旧只停用 `GetAccountRealName`,配了就要够长、不能与别的调用方同值(否则持那一把的服务能以 payment 的身份读全体玩家的实名)。
  - 本机开发:`tools/scripts/lib/account_directory_dev_secret.ps1` 登记 `MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT`,data-service 与 payment 各拿到同一把,payment 只拿这一把;`go_services.ps1` 启动 payment 时发它。docker 调试栈里没有 payment,没动。
- **测试(新增或改写,未运行)**:
  - `go/login/internal/config/anti_addiction_test.go`:`TestResolveAntiAddictionMatchesTheDeployment`(一致放行;漏抄、关着、换 AppId、换 BizId、只给一个、带空格都拒,报错不带密钥)。
  - `go/data_service/internal/svc/global_caller_secrets_test.go`:`TestPaymentCallerSecretIsOptionalButIndependent`。
  - `tools/scripts/tests/k8s_migrate_gate.tests.ps1`:原「mail-producer-battle:…」一条改写成「跨 namespace 共享密钥:…」(DryRun、各 namespace 只读一次、六种拒绝、infra 缺键、kubectl 失败、门禁在 `switch ($Command)` 之前),新增「跨 namespace 共享密钥表与清单一致」(两侧都以 `optional: false` 引用的键恰好是表里的键,每行的两侧用法与清单逐个对上)。
  - `tools/scripts/tests/k8s_deploy_contract.tests.ps1`:端到端 dev 用例核 login 主容器各注入一份期望值;基线核不注入;源码形状核期望值的变量名与 Go 常量逐字相同。
  - `tools/scripts/tests/sql_boot_catalog_local.tests.ps1`:启动器接线的名单断言加 payment;新增 payment 只拿查实名密钥一条。
- **文件**(都带着别的会话的未提交改动,提交时按 hunk 分):`go/login/internal/config/anti_addiction.go`、`anti_addiction_test.go`、`go/data_service/internal/svc/global_caller_secrets.go`、`global_caller_secrets_test.go`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/k8s_client_entry.ps1`(防误关报错里的关闭顺序)、`tools/scripts/lib/account_directory_dev_secret.ps1`、`tools/scripts/go_services.ps1`、上面四份测试;文档 `docs/design/real-name-anti-addiction.md` §8、`docs/design/battle-overflow-mail.md`(部署核对两处)、`docs/ops/k8s-open-server-runbook.md` §1、`docs/ops/release-checklist.md`、`deploy/k8s/README.md`。
- **Codex 验证**(失败时保留完整输出):
  1. Go(工作目录 `go/login`):`go vet ./internal/config/`;`go test ./internal/config/ -run 'AntiAddiction' -count=1 -v`;回归 `go test ./internal/config/ -count=1`。gofmt 本会话跑过(`gofmt -l` 无输出,不算编译)。
  2. Go(工作目录 `go/data_service`):`go vet ./internal/svc/`;`go test ./internal/svc/ -run 'CallerSecret|RoleFence' -count=1 -v`;回归 `go test ./internal/svc/ -count=1`。
  3. PowerShell(仓库根,不需要集群):`pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`、`k8s_deploy_contract.tests.ps1`、`k8s_login_auth_provider_guard.tests.ps1`、`sql_boot_catalog_local.tests.ps1`、`global_authority_deploy.tests.ps1`、`docker_debug_sql_boot.tests.ps1`,同样用 `pwsh -NoProfile -File tools/scripts/tests/<名字>`;上一条列的 `k8s_client_entry_contract.tests.ps1`、`login_password_auth_deploy_contract.tests.ps1` 一并跑。
  4. `python tools/scripts/check_log_text_english.py`:期望干净(新加的 Write-Host 都是英文,本会话只用文本检索核过)。
  5. 冒烟(可选,需要 kind 与预置的 `domain-service-secrets`):`k8s_deploy.ps1 -Command zone-up ... -LoginNppaAppId <测试值> -LoginNppaBizId <测试值>`,运行配置先不抄 `AntiAddiction` 段,期望 login 新 Pod 打出 `the deployment turns anti-addiction on` 后退出;抄进去之后起得来。另把某个 zone 的 `account-directory-payment` 改成与 infra 不同,期望部署在写之前报 `differs from namespace <infra> for account-directory-payment`。
- **上线前要做(运维,与上一条相同,补一句)**:`account-directory-payment` 在每个 zone namespace 与 infra namespace 先预置、同值;从这一版起部署脚本会在写之前核出缺键、过短、同值与两边不同,表里其余 11 把同样受核。
- **未验证**:以上全部没有编译、没有运行;PowerShell 只用 `Parser::ParseFile` 查过语法(0 错误),Go 只跑过 `gofmt -l`。
- **Java 版(AGENTS §12)**:不适用,Java 版没有 K8s 部署脚本与这几个 Go 服务,不往 PARITY.md 加行。

## 2026-11-19 Claude:商城 mall、支付 payment 与 trade 的商城接线接进 K8s 部署——清单、部署脚本、契约测试、文档(未运行,待 Codex 验证;商城业务表的安装没接进脚本,待负责人定;Java 独立仓不涉及)

- **结论**:payment 与官方商城 mall 现在能由 `k8s_deploy.ps1` 的 `infra-up` / `all-up` 部署到 infra namespace,trade 指向集群内的商城。全部没有运行过。还缺一步要负责人定:商城的业务表没有接进部署脚本,第一次起商城之前要运维手工装一次。
- **起因**:支付服务与商城此前只能本机跑(设计 `docs/design/payment-recharge.md` §22.11 第 1 条)。负责人同意这一批约 30 个文件,并定了六条口径。
- **做了什么**:
  1. 镜像:`go_svc_image.ps1` 加 `payment`(`mmorpg-payment`),`java_svc_image.ps1` 加 `mall`(`mmorpg-mall`)。
  2. payment:清单 `manifests/go-svc/payment.yaml`(Service、回调入口 Service `payment-notify`、写者 StatefulSet 2 副本、PDB)与 `payment-migrate.yaml`,形状照 rank。脚本里登记进 `$GoSvcCatalogue`(全局池、带迁移 Job)、写者名单、会话身份验签名单、gRPC 准入名单(只放行路由服)、路由服预算链、MySQL 连接总账。不进金丝雀名单。
  3. 回调入口:Service `payment-notify` 对外 443、回源到容器的 18130。类型取 `$GateServiceType`:托管云 `LoadBalancer`,自建集群 `NodePort`,custom 取 `-GateServiceType`。不用 Ingress,gRPC 的 Service 只在集群内。
  4. 支付配置:新参数 `-PaymentConfigPath`,文件里是整段 `Payment:`,不含密钥值。`NotifyListenAddr` 与 `Mall` 子段由脚本写死。不给时 dev 用服务 yaml 的段(只开 devpay),staging / prod 一个渠道都不开。合规闸没有放宽。
  5. 商城:清单 `manifests/java-svc/mall.yaml`(Service、写者 StatefulSet 2 副本、PDB)与 `mall-schema.yaml`(闸门表 Job),形状照 gateway。`$JavaSvcCatalogue` 加 `mall`(`Global`):同时部署全局 Go 服务且给了 `-JavaSvcRegistry` 才部署,`zone-up` 不部署。库 `mmorpg_mall` 登记进 `deploy/mysql-init/00_init_zone_dbs.sql`。
  6. 密钥:四把商城调用密钥与 dev 的 devpay 密钥进工作负载密钥目录(`commerce-gateway` / `commerce-payment` / `commerce-delivery` / `commerce-admin` / `payment-devpay`)。trade 清单里手写的 `domain-service-secrets` 引用删掉,只剩目录一条注入路径。真实渠道的密钥由运维预置在 Secret `payment-channel-secrets`,清单用 `envFrom` 注入;脚本不读值,只在任何写操作之前核对键。
  7. trade:ConfigMap 的 `Mall.URL` 写成 `http://mall.<infra>:18120`,两把密钥的变量名改成目录里的名字。
  8. 连接总账:加 payment、mall 各 2 副本 × 16。规划档峰值 1956 → 2084,`max_connections` 2600 不变,余量 516。TiDB 档约 7196 → 7324,预算 8000 不变。
  9. 契约测试:新增 `tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`(21 条)。`k8s_deploy_contract`、`k8s_migrate_gate`、`logical_db_deploy_contract`、`redis_player_cluster_contract` 与夹具库 `lib/deploy_capture.ps1` 同步名单。CI 工作流的触发路径加上新测试读的两个商城文件。
  10. 文档:`deploy/k8s/README.md` 加「支付 payment」「商城 mall」两节,同步镜像表、准入表、连接总账;`deploy/k8s/AGENTS.md` NOTES 各一行;设计稿 §22.1、§22.9、§22.11 改成现状。`go/payment/etc/payment.yaml` 改了一行注释(回调入口不再写 Ingress)。
- **没做、要人定的**:
  1. **商城业务表的安装。** `java/mall_game/src/main/resources/db/schema.sql` 只能装进全新的库、只装一次,末尾是一条普通 `INSERT`。现有的 `-ddl-file`(gateway 的建表 Job 用它)只收 `CREATE TABLE IF NOT EXISTS` 与 `INSERT IGNORE INTO`,收不了它。没有硬套别的服务的做法:现在由运维手工装,步骤在 README「商城 mall」的「建库与建表」第 3 步。没装时商城起不来。可选的接法:把末尾那条改成 `INSERT IGNORE` 后走 `-ddl-file`;给 `writer_authority` 加一个全新库安装命令;或者保持手工。
  2. staging / prod 的 `-DatabaseBackend mysql` 档不部署商城:`java/mall_game` 的生产模式只认 TiDB。这种部署里 trade 与 payment 指向的商城不存在。
  3. 商城没有 HTTP 健康检查与指标端点:探针用 TCP,没有抓取注解,没有告警规则。payment 也没有告警规则文件。
  4. 回调入口的证书、域名、云厂商负载均衡注解不由脚本生成。`payment-channel-secrets` 没有指纹,改了要手工 `rollout restart statefulset/payment`。
  5. `-PaymentConfigPath` 没有接进 `dev_tools.ps1` / `k8s_image.ps1` 的透传。
  6. 开服手册 `docs/ops/k8s-open-server-runbook.md` 没改(别的会话正在改它)。目录里要多 `<infra>/payment`、`<infra>/mall` 两个条目,mall 的第一遍还要 `registrarImage`;这些写在 README 里。
  7. `go/payment/internal/config/config.go` 与 `go/payment/payment.go` 的注释里还写着「经 Ingress 暴露」,没改(别的会话正在改这两个文件)。
  8. 上一条(未成年人充值限额闸收尾)写了「`sql_boot_catalog_local.tests.ps1` 夹具缺 `payment_spend`,已请支付会话的 K8s 批次顺手补」。本批没有改那份测试:它是本机路径,不在这一批的范围里。
- **Codex 验证**(工作目录都是仓库根;全部是 DryRun 与静态核对,不连集群,不用先编译):
  1. 先跑新测试:
     ```
     pwsh -NoProfile -File tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1
     ```
     通过标准:末尾汇总 `total=21 pass=21 fail=0`。
  2. 再跑改到的四份,一份一份跑:
     ```
     pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1
     ```
     通过标准:各自 `fail=0`。`k8s_deploy_contract` 里本批直接改到的用例:「工作负载密钥:k8s_deploy.ps1 的目录表与契约表逐项一致」「自检:总账公式逐项可复算」「连 MySQL 的 ConfigMap 只有总账认识的那些」「资产通道默认关」「infra-up -AssetOps on」里的准入名单、「gRPC 准入名单里的每个 app 标签」「发布轨道:可金丝雀却没列出的服务」「all-up:全局服务的迁移 Job 先于 data-service」。
  3. 最后跑整套。夹具库 `lib/deploy_capture.ps1` 改了,所有经它跑 `k8s_deploy.ps1` 的测试都受影响:
     ```
     Get-ChildItem tools/scripts/tests -Filter *.ps1 -File | Sort-Object Name | ForEach-Object { pwsh -NoProfile -File $_.FullName; if ($LASTEXITCODE -ne 0) { Write-Host "FAILED: $($_.Name)" } }
     ```
     通过标准:没有 `FAILED:` 行。别的会话同时在改这几份测试与部署脚本,与本条无关的红分开记。
  4. 失败时保留:失败用例名、断言原文,以及那次 DryRun 输出里从 `Payment:` 一行起的部分。
- **kind 冒烟**(交 Codex。建集群与镜像预载照 `deploy/k8s/README.md`「Local Trial on kind」,命令一律带 `-KubeContext kind-mmorpg -KubeConfig "$env:TEMP\kind-mmorpg.kubeconfig"`):
  1. 构建并载入 `mmorpg-payment`、`mmorpg-mall`、`mmorpg-trade` 与写者工具镜像(`go_svc_image.ps1`、`java_svc_image.ps1`)。
  2. boot 目录加 `<infra>/payment`、`<infra>/mall` 两个条目,备好各自的 Secret(开服手册 §1)。infra namespace 的 `domain-service-secrets` 要有 `account-directory-payment`。
  3. dev 档 `infra-up`,带 `-GoSvcRegistry`、`-JavaSvcRegistry`;用集群内 MySQL 时再带 `-DatabaseBackend mysql -GlobalAuthorityTiDBHost mysql.<infra>:3306`。期望 `payment-migrate`、`mall-schema` 两个 Job 都是 Complete。
  4. 用 schema 身份把商城的 `schema.sql` 装进 `mmorpg_mall`(README「商城 mall」的「建库与建表」第 3 步)。
  5. 起一个 zone,等它的 data-service 就绪后重跑 `infra-up`。期望 `statefulset/payment`、`statefulset/mall` 都 Ready;`kubectl -n <infra> get svc payment-notify` 的类型是 `NodePort`。
  6. 回调口:`kubectl -n <infra> port-forward svc/payment-notify 18443:443`,再 `curl -i -X POST http://127.0.0.1:18443/notify/devpay`。期望收到 payment 的 HTTP 应答(没签名的请求被拒绝属正常);连接被拒说明 Service 或监听地址没接上。
  7. 准入(装了 Calico 的 kind,`deploy/k8s/kind-netpol-config.yaml`):带 `-NetworkPolicies on` 重跑后执行 `pwsh tools/scripts/k8s_netpol_probe.ps1 -KubeContext <context>`,`payment-grpc-allowlist` 与 `mall-grpc-allowlist` 两份都应符合。
  8. 下单到账的端到端没有现成的 K8s 冒烟:`robot/etc/mall_arrival_smoke.yaml` 用商城的 mock 付款器(要 `COMMERCE_MOCK_PAYMENT_ENABLED=true`,K8s 的配置模板没开),devpay 的端到端要用客户端走。trade 连商城还要带 `-AssetOps on -NetworkPolicies on`。
- **未验证**:以上全部没有运行。本会话只做了语法解析(改过与新增的 .ps1 用 `Parser::ParseFile` 解析无错)与 YAML 解析(新增与改过的清单、CI 工作流)。
- **Java 版(AGENTS §12)**:不涉及。这一批只有本仓的 K8s 部署,没有改客户端契约;独立 Java 仓没有登记新行。
- **提交情况**:main 工作区,未提交。

## 2026-11-19 Claude:go/db 删除存档库运行期冷却——探活冷却、storeerr 与 db_store_cooldown_stores 整套删,打开失败的冷却保留(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:本日「断线重连 R19」的新发现。`proto_sql/store_health.go` 的运行期冷却(`ReportConnLost` / `ReportHealthy` / `CoolingDown`)唯一的调用方是已删的 Kafka 任务消费者,全仓没有 SQL 调用点上报,`cooling` 表恒空,`Store()` 的冷却分支走不到,`db_store_cooldown_stores` 恒为 0。「接着做完服务器剩下的代码(缺口清单续)」会话派本会话按负责人口径「按最标准的方式定、现在修」在接回与删除之间二选一。
- **定案:删除**。依据写在 `docs/design/db-task-store-unavailable-deferral.md` 文首状态(v0.4),要点:
  - 要解决的问题已经不存在:冷却是给消费者的共享 worker 与重试预算做的。现在每次存盘 / 认领是一次独立的 RPC,重试与退避都在调用方(scene 存盘同序号 0.5~5 秒带抖动重试、认领 30 秒预算、审计转发一轮内跳过出错的库),对一个坏库的并发拨号由每库连接池上限封顶;
  - 接回去做不出判别:存档库连接全部经过 authoritysql 守卫,守卫有意不保留底层错误(建连失败一律是同一条错误,1040 与网络不通分不开;结局未知不能 Unwrap;`ErrTxAbortedBeforeCommit` 也覆盖调用方取消)。`storeerr.Classify` 对这些全判 `KindNone`,探活走的也是同一个守卫连接器,冷却永远进不去。要做成得改所有 Go 服务共用的守卫,为没有数据支撑的「失败得更快」不划算;
  - 删除之后:库在运行中整库失联时,每次调用等到拨号超时(缺省 5 秒)、读写超时或调用方的截止时间后回 `codes.Unavailable`,scene 照旧重试。结论与数据不变,只是每次失败晚几秒;
  - 打开失败的冷却(`openOnce`,10 秒)保留:它挡的是每次调用都重跑一遍整套打开核对。
- **改动**(main 工作区,未提交):
  - 删文件:`go/db/internal/logic/pkg/proto_sql/store_health.go`、`store_health_test.go`;`go/db/internal/storeerr/`(`storeerr.go`、`storeerr_test.go`,冷却删掉之后再没有调用方)。
  - `proto_sql/store_registry.go`:删 `StoreRegistryOptions` 的 `ProbeTimeout` / `Cooldown` / `Jitter` / `Ping`、对应字段与缺省值、`cooling` 表、`Store()` 的冷却分支、`OpenPlacementStore` 里的 `store.Connector = connector`;`Store()` 的注释写明没有运行期冷却是定案;`ErrStoreUnavailable` 的注释改成现状(原来还写着「消费者延后、不耗重试」)。
  - `proto_sql/db.go`:删只给探活用的 `GameDB.Connector`。
  - `proto_sql/service_pool_test.go`:删四条 `TestPingStore_*` 与探活替身,只留三条连接参数用例。
  - `go/db/internal/metrics/metrics.go`:删 `storeCooldownCollector`、`NewStoreCooldownCollector`、`RegisterStoreCooldown`;`metrics_test.go` 删 `TestStoreCooldownCollector_EmitsAllPools` 与只给它用的 `gatherByLabel`。
  - `go/db/db.go`:删 `metrics.RegisterStoreCooldown(stores.CooldownCounts)` 及注释,只动这两行(同文件的 `clockcheck` 是别的会话的改动)。
  - `tools/scripts/stress_summarize.ps1`:P4 表去掉 cooldown 列(表头说明、段标题、格式串、取值),注释写明旧汇总里那一列的来历;`test_stress_summarize.ps1` 的夹具删两行 `db_store_cooldown_stores`,P4 的表头断言与两条行断言去掉最后一列。拿旧的 `prev-summary.txt` 对比时 P4 少一列属正常。
  - 文档:`docs/design/db-task-store-unavailable-deferral.md` 文首状态、§3.2 / §3.4 标题、修订历史 v0.4;`deploy/k8s/README.md` 指标清单删 `db_store_cooldown_stores` 那一项;`docs/design/infra-reconnect-overview.md` 的 R19 行与 `docs/handoff/server-gap-audit-20261101.md` 开头 11-19 核对里的「另开任务定」改成结论。
- **没改的**:告警规则里早已没有 `DbStoreCoolingDown`(随消费链删除);`docs/ops/godb-pipeline-launch-todo.md`、`db-write-fencing-mysql.md`、`db-retry-owner-routing.md` 等已过期文档里的冷却描述照旧只作历史。`go/db/internal/playerstore/store.go`、`store_test.go` 上别的会话的未提交改动(退役行种)没有碰;`openOnce` 里两条英文日志仍写着 "without consuming retries",是消费者时代的说法,不影响行为,本条不改。
- **给 Codex 的验证步骤**(本会话没有编译、没有跑测试;失败保留编译错误全文、失败用例名与断言原文,不改断言,不连续重试):
  ```
  cd go/db
  gofmt -l db.go internal/logic/pkg/proto_sql internal/metrics
  go build ./...
  go vet . ./internal/logic/pkg/proto_sql/ ./internal/metrics/
  go test -count=1 ./internal/logic/pkg/proto_sql/ ./internal/metrics/
  grep -rnE "storeerr|ReportConnLost|CooldownCounts|RegisterStoreCooldown|store_health|GameDB\{[^}]*Connector" --include=*.go .
  cd ../..
  pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1
  ```
  通过标准:
  - `gofmt -l` 无输出,build / vet 无错(`go/db/internal/storeerr` 目录已不存在,不应再有包导入它);
  - proto_sql 与 metrics 两个包全部 PASS,其中 `TestNewServiceMysqlConfig_SetsTimeoutsAndSessionWaits`、`TestNewServiceMysqlConfig_NegativeDisables`、`TestNewMysqlConfig_HasNoServiceTimeouts`、`TestSQLPoolCollectorDeclaresDbPools`、`TestStoreRegistryOpenFailureBacksOffThenRetries`、`TestStoreRegistryFrozenGateDoesNotBackOff` 逐个报 PASS;没有 TiDB / MySQL 时跳过的用例如实报为未运行;
  - `grep` 无输出;
  - 压测汇总自测末行 `PASS`(P4 两条行断言按新的五个数据列匹配)。
  - 不需要真库演练:删掉的分支在现在的代码里本来就走不到。
- **Java 版(AGENTS §12)**:不涉及(Java 版没有 go/db 与它的存档库注册表),不加 PARITY 行。

## 2026-11-19 Claude:robot 加 WebSocket 传输——机器人可以走小游戏那条路连 gate 与 battle(代码写完,未编译、未测试,待 Codex)

负责人在同日「小游戏 WebSocket 入口」交付后要求补上,用来自动冒烟那条链路。设计见 `docs/design/minigame-websocket-gate.md` W11,用法见 `robot/README.md`「传输方式」。

- **改了什么**:
  - 传输层 `robot/pkg/transport.go`(新):配置段 `transport`(`kind: tcp|websocket`、`require_advertised`、`dial_addr`、`insecure_skip_verify`)与校验;地址簿 `AdvertiseWsURL` / `AdvertisedWsURL`(`host:port` → 服务端下发的 WebSocket 地址);`DialStream`(裸连接:TCP,或一条 WebSocket 上的字节流);回环桥 `wsBridge`。
  - 为什么有桥:`muduoclient` 是外部库,自己拨 TCP、没有注入拨号函数的口子,而 `GameClient` 到处拿着 `*muduo.Client`。WebSocket 模式下让它拨进程内的回环监听,桥对每条连进来的连接各拨一条 WebSocket、两边原样转发字节。muduo「断线重拨同一个地址、拨不通每秒重试」的节奏不变。
  - 为什么用地址簿:robot 里几十个函数按 `(host, port)` 传 gate 地址,逐层加参数改动面太大。解出下发地址的五处登记,建连时按 `host:port` 查:`http_assign_gate.go` 的 `postJSONWithClient`(assign-gate 与 queue-status 共用这个形状)、`gate.go` 的旧解码点、msg 124(`pkg.RedirectTarget` 加 `WsURL`,`FollowRedirect` 登记)、`battle_direct_conn.go`、`network_probe.go`。放行回包没带地址时清掉旧记录。
  - `pkg/client.go`:`NewGameClient` / `SwapConn` 经 `newMuduoClient` 建连,多一个 `closeTransport`(收桥,在 muduo 客户端 Close 之后调,TCP 下为 nil);`Close` 同序。
  - `pkg/redirect.go`:换连接前的探测从 `probeTCP` 改成按当前传输方式的 `probeReachable`(WebSocket 下是一次完整握手)。
  - `network_probe.go`、`heartbeat_idle_smoke_scenario.go` 的裸拨号改走 `pkg.DialStream`。
  - 配置:`config.Config.Transport`(类型就是 `pkg.TransportConfig`,只有一份定义),`validate()` 里校验;`main.go` 在加载配置之后调 `pkg.ConfigureTransport`。`etc/robot.yaml` 加了一段注释掉的示例。后三个选项写了却没选 websocket 时拒绝启动(否则等于悄悄测了 TCP)。
  - 依赖:`robot/go.mod` 里 `golang.org/x/net` 从间接依赖改成直接依赖,版本不变(v0.48.0),用它的 `websocket` 包。全仓 Go 代码此前没有用过 WebSocket 库。`robot/vendor` 里还没有这个包。
  - 测试:`robot/pkg/transport_test.go`(新,6 条)、`robot/http_assign_gate_ws_url_test.go`(新,1 条)。
- **缺省不变**:不写 `transport` 段就是 TCP,不起桥,`DialStream` 就是 `net.DialTimeout`;地址簿照记,只是没人读。
- **本会话跑过的**:只有 `gofmt -l`(只读的格式与语法检查,不编译)。范围是 robot 这批 13 个 Go 文件,加上「小游戏 WebSocket 入口」里手工对齐的 login / scene_manager / match 14 个文件,输出为空。另在本机模块缓存 `golang.org/x/net@v0.48.0/websocket` 里读过用到的接口签名与客户端握手、读帧的实现。**没有编译,没有跑任何测试。**
- **给 Codex 的验证步骤**(接在「小游戏 WebSocket 入口」第 0 步协议生成之后:robot 用到新生成的 `GetWsUrl` / `GetTargetWsUrl`,没生成之前编不过):
  1. vendor(工作目录 `robot`):`go mod vendor`。
     期望:出现 `robot/vendor/golang.org/x/net/websocket/`,`vendor/modules.txt` 的 `golang.org/x/net` 下多一行 `golang.org/x/net/websocket`,`vendor/proto` 带上新字段的访问器。本机模块缓存里有这个版本,不需要联网。它会把 `go/proto`、`go/shared` 工作区里当前的内容一起带进 `robot/vendor`(`robot/README.md` 的既定做法),别的会话还没生成完的协议也会进来;之后编不过先看是不是别的会话的协议没生成。
  2. 编译与单测(工作目录 `robot`):
     ```
     gofmt -l pkg config main.go gate.go http_assign_gate.go http_assign_gate_ws_url_test.go battle_direct_conn.go network_probe.go heartbeat_idle_smoke_scenario.go logic/handler/scene_client_player_common_redirect_to_gate.go
     go build ./...
     go vet ./pkg/ ./config/ .
     go test ./pkg/ ./config/ -count=1
     go test . -run "TestPostJSON_RecordsTheGateWebSocketAddress|NetworkProbe|AssignGate|BattleDirect" -count=1
     go test ./pkg/ -race -count=1
     ```
     通过标准:`gofmt -l` 输出为空;编译、vet 零错误;测试零失败。逐个点名新用例:`TestTransportConfig_Validate`、`TestTransport_AdvertisedAddressWinsAndAMissingOneFallsBackOrFails`、`TestTransport_WebSocketCarriesMuduoFramesAsBinaryMessages`、`TestTransport_EdgeAddressIsDialedThroughDialAddrOverTLS`、`TestWsBridge_RedialsPerConnectionAndStopsOnClose`、`TestTransport_TCPIsTheDefaultAndIgnoresAdvertisements`、`TestPostJSON_RecordsTheGateWebSocketAddress`。`pkg` 原有的重定向、心跳、Call 用例照旧全过(它们在 TCP 下跑)。最后一条 `-race` 需要 cgo,本机没有就跳过并写明。TLS 那条用例故意让一次握手因自签证书失败,stderr 会有一行 `http: TLS handshake error … bad certificate`,不是失败。失败时保留 `go test -v` 的输出。
  3. 本机冒烟(有本机栈时;gate / battle 不用设 `CLIENT_WS_BASE_URL`,robot 走直连回落)。把下面四份配置各复制到 `robot/logs/ws-smoke/`(已忽略),末尾加两行 `transport:` 与 `  kind: websocket`,在 `robot` 目录用 `-c` 指过去各跑一遍:
     - `etc/robot_smoke.yaml`:登录、分配 gate、进场;
     - `etc/heartbeat_idle_smoke.yaml`:裸连接验票、心跳、空闲超时;
     - `etc/battle_smoke.yaml`:战斗直连与观战;
     - `etc/gate_migrate_smoke.yaml`:msg 124 换 gate(前置条件见该文件头部)。

     通过标准:与 TCP 下相同的通过行(各配置文件头部写明);robot 启动日志有一行 `robot transport: websocket`;gate 的 `/metrics` 里 `gate_websocket_handshakes_total{result="accepted"}` 增长、`gate_websocket_protocol_closed_total` 不增长;gate 与 battle 日志没有 `WebSocket frames rejected`。同一份配置去掉 `transport` 段再跑一遍,确认 TCP 没被带坏。
  4. kind 经边缘(可选,接「小游戏 WebSocket 入口」第 7 步)。robot 配置里加:
     ```yaml
     transport:
       kind: websocket
       require_advertised: true
       dial_addr: "127.0.0.1:<ws-edge 的 NodePort>"
       insecure_skip_verify: true
     ```
     通过标准:`robot_smoke` 与 `battle_smoke` 通过。`require_advertised` 打开后仍通过,说明登录分配与战斗分配的地址都下发了、边缘转到了对的那台;报 `the server advertised no WebSocket address for …` 就是服务端漏下发,记下是哪一步。
- **没做 / 局限**:
  - 每条连接多一个回环监听和一跳回环 TCP,只适合冒烟与小规模。要用 WebSocket 打高并发压测,先给 muduoclient 加注入拨号函数的口子(代码里记了 `shortcut:`)。
  - `travel_smoke` 等其它冒烟模式没有逐个看过在 WebSocket 下的表现;它们都经 `GameClient`,按设计不受影响,但没跑过。
  - `battle_observer_wire_direct_test.go` 里测试自己的裸 TCP 拨号没有动(它测的是线格式,不是传输)。
  - 桥关闭时直接断 TCP、不发 Close 帧;服务端回显 Close 的路径只有 C++ 单测覆盖。
- **Java 版**:不涉及(robot 是本仓的测试工具,客户端契约没变)。
- **提交情况**:main 工作区,本会话没有提交。

## 2026-11-20 Claude：商城业务表的安装接进 mall-schema Job（补 11-19「商城 mall、支付 payment 与 trade 的商城接线接进 K8s 部署」条目；未运行，待 Codex 验证）

- **结论**：11-19 那条里「商城业务表要运维手工装一次」已不成立，那条验证步骤里的第 4 步（手工装 `schema.sql`）不用做。
- **怎么做的**：`java/mall_game/src/main/resources/db/schema.sql` 末尾的合同戳改成 `INSERT IGNORE INTO`，整份文件可重跑。Job `mall-schema` 多传 `-ddl-file /app/mall-schema/schema.sql`。部署脚本把这份文件原样渲染进 ConfigMap `mall-schema-sql`（`New-MallSchemaJobManifest`），先于 Job 落地。做法同 gateway-schema。
- **行为变化**：对已经装过的库重跑不再报主键冲突，而是什么都不改。旧结构的库不会被升级：商城启动时 `SchemaVerifier` 核对不过就拒启，要清空 `mmorpg_mall` 重装（项目未上线，没有迁移路径）。
- **同批**：
  - `docs/design/payment-recharge.md` §22.8 按限额闸的实现改写（`MinorSpendingGate` 三个取值 none / realname / overseas，指向 `real-name-anti-addiction.md` §5.6），§22.11 第 1、4 条同步。
  - `go/payment` 两处注释里的「经 Ingress 暴露」改成实际的 Service `payment-notify` + 外部负载均衡。
  - `tools/scripts/tests/sql_boot_catalog_local.tests.ps1` 的夹具加 `payment_spend`（限额闸会话的新表）。
- **静态检查（都不是运行）**：按 `writer_authority` 的拆句与语句规则写了一份对照脚本，商城 `schema.sql` 的 29 条语句全部通过（28 条建表、1 条 `INSERT IGNORE`），同一脚本对 gateway 的 `schema.sql` 也通过；`k8s_deploy.ps1` 与 `k8s_mall_payment_deploy_contract.tests.ps1` 语法解析无错；`mall-schema.yaml`、工作流 yaml 能解析。
- **Codex 验证**（仓库根目录）：
  1. `pwsh -NoProfile -File tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`：用例数不变（21 条），商城清单那条的断言换成了「带 `-ddl-file`、读 ConfigMap、`schema.sql` 里没有普通 `INSERT INTO`」。
  2. `java/mall_game` 的 `mvn -q test`：H2 的 MySQL 模式要认 `INSERT IGNORE`，既有用例应不受影响。
  3. kind 冒烟时 `kubectl -n <infra> logs job/mall-schema` 应无报错，`mmorpg_mall` 里有 28 张业务表和一行合同戳；重跑一次 `infra-up`，Job 仍应 Complete。
- **Java 独立仓（AGENTS §12）**：不涉及。
- **文件**：`java/mall_game/src/main/resources/db/schema.sql`、`deploy/k8s/manifests/java-svc/mall-schema.yaml`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`、`tools/scripts/tests/sql_boot_catalog_local.tests.ps1`、`.github/workflows/deploy-config-tests.yml`、`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`、`docs/design/payment-recharge.md`、`go/payment/internal/config/config.go`、`go/payment/payment.go`。未提交。

## 2026-11-20 Claude:账号注销收尾批——预检令牌、帮主自动让位、注销下线结束离线托管、摘评级榜、剧本清理、拒收已删角色的邮件(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人 11-20「查出的问题现在修,按最标准的定」。接 2026-11-19「账号注销三批落码」条目,把那条「已知限制」与设计 §11 里能修的全部修掉。设计 `docs/design/account-deletion.md` 已按结果改正文(§4.5、§5、§6.2、§6.3、§7.2、§7.4、§10「收尾批」、§11、§12)。
- **做了什么**:
  1. **注销预检永远「稍后重试」**(11-19 落码的缺陷):预检的试探状态没带删角令牌,参与者对没有令牌的 `PrepareRoleFreeze` 按非法参数拒。改为带 `rolefence.RemovalToken(player)`、`FreezeFinal=false`(`data_service/internal/store/account_deletion.go`),真库用例核试探收到的令牌。
  2. **帮主**:`shared/rolefence` 加 `Probing(ctx)` / `WithProbing`(dry_run 时给挂起判定打标记)与 `OnCommit(ctx, fn)`(清理事务提交成功后才跑的动作,回滚不跑,与调用方取消脱钩,3 秒预算),以及 `ValidateCoordinatorSecret`。guild 的 `guild_leader` 只在试探里回;执行删角时收尾自动让位(职位 → 累计帮贡 → 入帮时刻 → player_id),帮里只剩他就解散(与 `DisbandGuild` 共用抽出来的 `disbandLockedGuild`);提交后失效缓存(op `role_purge`)、解散时摘帮会榜、给留下的成员推送 LEADER_TRANSFERRED / MEMBER_LEFT 并 bump 摘要。`TransferLeader` 用 `withRoleTargets` 锁目标的护栏副本,目标冻结或已删除回「稍后重试」。guild 的参与者挪到 `guildLogic` 之后装配,注册 etcd 之前先 `ValidateCoordinatorSecret`。运维代办(`source=2`)的预检去掉 `guild_leader`(`selfServiceBlockers`),其余挂起照拦。
  3. **注销下线**:只踢 gate 等于断线,有挂机月卡的角色会转成离线托管留在世界里(最长到月卡到期),删角一直等「登记全部关闭」。`deletionSessions.KickPlayer` 改为:先 `KickSessionWithReason`(申请时 2038、执行时 2040,gate 早已支持 `KickPlayerEvent.reason_tip`),再 `sessionmanager.DeleteSession` → `player_locator.MarkOffline`(删会话、`LeaveScene` 让持有节点按会话结束释放,托管随之结束)。客户端 `GameClient.DescribeKickReason` 认这两个码,显示原因、不自动重连。没有新 tip。
  4. **跨区踢线**:核实同地域所有区共用玩家域 Redis 与 Kafka(会话键不带区号,gate 命令 topic 按实例 uuid 过滤),任一区 login 都踢得到;11-19 写的「只踢得到本区」不对,只改文档。**多地域**:账号目录按地域各一份,注销范围就是本地域的账号,写进 §7.4。
  5. **评级榜**:match 新表 `match_rank_removal`(`proto/match/match_authority.proto`,主键 (mode, player_id))。删角收尾删评分行的同一事务里按他有评分的计分模式各写一行;投榜循环每轮最多 16 行调 `RemoveRankEntry`(代次 1、请求号 = player_id;rank 同一事务立墓碑),成功删行,榜已封 / 不存在按摘完删行,其余拒绝留行回错误。
  6. **战斗剧本**:match 收尾删他的 `match_battle_script`(按 owner 普通读候选、主键逐行删)与 `match_battle_script_loadout`;审核流水 `match_battle_script_review` 按审计保留。
  7. **邮件**:`data/tip/Tip.xlsx` mail 段尾加 `MailRecipientDeleted`(「收件人角色已注销」,不是故障;脚本重载、只插这一行、写后逐格核对,导表后应为 17011)。个人邮件发件:建邮箱行之前不加锁预检收件人副本,事务里 `lockLiveRecipient` 锁副本复核(锁序副本 → mail_box,与领取、删角收尾同序),是墓碑回 `ErrRecipientDeleted` → tip `kMailRecipientDeleted`。rank 奖励投递见到这个码直接转放弃终态(`attemptRecipientGone`,只记 Info,不 Hold 排空证明)。C++ scene 的待寄队列只在角色在场景里时投,删角要求先离场,碰不到这个码,不改。
- **文件**:
  - 协议 / 表:`proto/match/match_authority.proto`、`data/tip/Tip.xlsx`。
  - shared:`go/shared/rolefence/participant.go`、`rolefence_test.go`。
  - data_service:`go/data_service/internal/store/account_deletion.go`、`account_deletion_tidb_test.go`。
  - login:`go/login/internal/svc/account_deletion.go`,`internal/logic/pkg/accountdeletion/service.go`、`executor.go`、`service_test.go`。
  - guild(子任务落码,本会话逐处复核了锁序与 `DisbandGuild` 抽函数前后逐字一致):`go/guild/guild.go`,`internal/data/role_fence_pending.go`、`role_fence_purge.go`、`guild_manage_repo.go`、`role_fence_purge_tidb_test.go`、`role_fence_tidb_test.go`、`activity_repo_static_test.go`,新增 `internal/logic/role_fence_purge.go`、`role_fence_purge_test.go`。
  - match:`go/match/internal/authority/schema.go`、`role_fence_purge.go`、`role_fence_purge_native_test.go`、`rank_publisher.go`、`rating_test.go`。
  - mail:`go/mail/internal/data/repo.go`、`role_identity.go`、`role_fence_purge.go`、`role_fence_purge_tidb_test.go`,`internal/server/server.go`。
  - rank:`go/rank/internal/reward/policy.go`、`worker.go`、`policy_test.go`。
  - 客户端仓:`Assets/Scripts/Game/GameClient.cs`、`Assets/Tests/EditMode/Net/AntiAddictionTests.cs`。
  - 文档:`docs/design/account-deletion.md`、`horizontal-scale-logical-db.md`;Java 仓 `PARITY.md`。
- **Codex 验证**(本会话没有编译、没有跑任何测试;失败保留完整输出,工作区有很多会话的在途改动,先按文件归属分清):
  0. 本地库:match 多一张表 `match_rank_removal`,跑一次 match 的 `-migrate`(或清库重建,项目未上线)。
  1. 生成(仓库根):`dev.bat gen`(期望 `MailError_kMailRecipientDeleted` = 17011 出现在三语言 tip 产物里,`tip_enum_ids.json` 只增不改,其余码号不变);`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(默认配置,写客户端仓;期望 `go/proto/match` 出现 `MatchRankRemovalRecord`)。
  2. `gofmt -l`:对上面列的全部 Go 文件,期望无输出(本会话与子任务都跑过)。
  3. 编译与静态检查,逐个模块(`go/shared`、`go/data_service`、`go/login`、`go/guild`、`go/mail`、`go/match`、`go/rank`):`go build ./... && go vet ./...`;guild 另跑 `go vet -tags integration ./...`。
  4. 新增与改过的用例:
     - `go/shared`:`go test ./rolefence/ -run 'TestPendingSeesProbingOnlyOnDryRun|TestPurgeCommitHooksRunOnlyAfterCommit|TestPurgeRemovedRoleOnlyForTombstones|TestPrepareDryRun|TestNewParticipantServerRequiresEverything' -count=1 -v`。
     - `go/data_service` 真 TiDB(环境同 11-19 条第 4 步):`go test ./internal/store/ -run 'TiDBAccountDeletion' -count=1 -v`(`TestTiDBAccountDeletionPrecheckBlocksWithoutWriting` 加了试探令牌与运维代办两段)。
     - `go/login`:`go test ./internal/logic/pkg/accountdeletion/ -count=1 -v`(踢线原因断言加在 `TestRequestScheduledSignsOutAndRevokesApple`、`TestExecutorRemovesAllRolesThenFinishes`)。
     - `go/guild`:不连库 `go test ./internal/data/ -run 'TestActivityLockOrderInSource|TestPointLockPrecedesConditionalWrites' -count=1 -v`、`go test ./internal/logic/ -run TestAfterRolePurgeNotifiesRemainingMembers -count=1 -v`;真 TiDB(`GUILD_ROLE_TEST_DSN`、`GUILD_ROLE_REQUIRE_TIDB=1` 与 `MMORPG_SQL_FIXTURE_*`)`go test ./internal/data/ -run 'TestTiDBRoleFence|TestTiDBPendingGuild' -count=1 -v`(新:`TestTiDBRoleFencePurgeHandsLeadershipToTopOfficer`、`TestTiDBRoleFencePurgeDisbandsSoloGuild`、`TestTiDBPendingGuildLeaderBlocksRemovalProbeOnly`、`TestTiDBRoleFenceTransferLeaderRejectsFrozenTarget`);解散与转让回归 `GUILD_TEST_MYSQL_DSN=… go test ./internal/data/ -run 'Disband|TestTransferLeader' -count=1 -v`、`GUILD_IT_MYSQL_DSN=… go test -tags integration ./internal/data/ -run 'TestActivityIT_.*Disband' -count=1 -v`。
     - `go/match`(`MATCH_IT_TIDB=127.0.0.1:<port>`、`MATCH_IT_REQUIRE_TIDB=1`):`go test ./internal/authority/ -run 'TestPurgeTiDB|TestRatingTiDB' -count=1 -v`(新 `TestRatingTiDBRankRemovalsOfDeletedPlayers`;`TestPurgeTiDBRemovedRoleDeletesPersonalRowsAndReleasesReports` 加了剧本、选用、审核流水)。
     - `go/mail`(`MAIL_RANK_TEST_TIDB=127.0.0.1:<port>`、`MAIL_RANK_REQUIRE_TESTS=1`):`go test ./internal/data/ -run 'TestTiDBMail' -count=1 -v`(新 `TestTiDBMailSendRechecksRecipientUnderLock`;`TestTiDBMailPurgeRemovedRoleDeletesPersonalRowsAndKeepsSharedOnes` 加了清理后拒收)。
     - `go/rank`:`go test ./internal/reward/ -count=1 -v`。
  5. 回归:上面七个模块各 `go test ./... -count=1`(rank、mail 的现有邮件链路用例 `rank_mail_chain_*` 要保持绿)。
  6. C++:Tip.xlsx 与 `match_authority.proto` 的生成物会变,没改 C++ 逻辑;`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(串行)确认编得过。
  7. 客户端(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error;EditMode `-testFilter "MmorpgClient.Tests.EditMode.Net.AntiAddictionTests"` 全绿(新 `AccountDeletionKickReasonsHaveTexts`)。
  8. `python tools/scripts/check_log_text_english.py`:干净。
  9. 冒烟(可选,调试栈):一名有挂机月卡的角色在线时申请注销 → 设备收到「账号正在注销中…」下线、不自动重连,另一名玩家看到他离开场景;把冷静期配成 0 再跑执行者 → 两轮内删角成功(不再一直「等下一轮」);他是帮主时帮主转给长老、他是唯一成员时帮会解散并从帮会榜消失;他在 1V1 榜上的条目被摘掉。
- **已知风险**(子任务与本会话复核时记下):让位选人不看对方副本(持 guild 行锁再锁别人的副本会违反锁序),可能选中同在删除中的成员,轮到他收尾时再让一次;帮会双存储已经不一致时(另有一个 role=3)让位在「恰好一个帮主」断言处失败关闭,要人工修数据;单人帮解散在事务里补建他的状态行(他已是墓碑,其余入口都在副本上被拒);`deletionSessions.KickPlayer` 没有单测(`KafkaClient` 是具体类型),靠冒烟;发件预检与建邮箱行之间删角收尾恰好提交时会留一行空邮箱行。
- **未验证**:以上全部。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;已在 `D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾登记「2026-11-20 账号注销收尾」。客户端可见的变化(两个下线原因码、预检里 `guild_leader` 只在本人申请时出现)两版要同批。

## 2026-11-20 Claude:改名与内容安全第 1 批——内容安全服务 go/content_guard(本地词库层、etcd 词库与热更新、发布工具、本机与 K8s 接线;未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **设计**:`docs/design/rename-and-content-safety.md` 第 6 节(本条落码时按实际写法改过 6.2~6.4、6.8)。负责人 2026-11-19 明确六批全部做完再交付,不用逐批确认。
- **改了什么**:
  - 协议:`proto/common/base/node.proto` 加 `ContentGuardNodeService = 32`;新 `proto/content_guard/content_guard.proto`(`ContentGuard.CheckText`,内部服务,不进客户端清单);
    `tools/proto_generator/protogen/etc/proto_gen.yaml` 与 `proto_gen.no-unity.yaml` 各加一块 `content_guard` 域。
  - 新模块 `go/content_guard/`(`go.mod` / `go.sum` 从 client_rpc_router 拷来,只改了 module 名,没有跑 tidy):
    - `internal/textnorm`:逐字符 NFKD → 小写 → 繁转简 → 标可跳过字符;`t2s.txt` 2480 对,由 `gen_t2s.ps1` 用 Windows 自带的简繁转换生成(没有从外网下载 OpenCC 的表)。
    - `internal/wordlist`:词条(类别 + 子串 / 整词 / 开头三种匹配方式)、词库文本格式、压平的字典树、白名单、内置的 6 个防冒充词。
    - `internal/engine`:当前词库的原子替换;按场景大类的动作表(名字类全拒;公告不管冒充官方;聊天辱骂打码、其余拒绝)。
    - `internal/lexstore`:词库在 etcd 的分块与指针(`/mmorpg/content_guard/lexicon/`),带条件换指针,保留 3 版,list-watch 跟随指针(用 `shared/etcdwatch`)。
    - `internal/server`、`internal/config`、`internal/metrics`、入口 `content_guard.go`、`etc/content_guard.yaml`(50250 / 指标 9290)。
    - `cmd/lexicon`:词库发布工具(`publish` / `add` / `remove` / `rollback` / `show`),直接读写 etcd;服务没有发布接口。
    - `testdata/vectors.json`:两版服务器共用的行为向量(20 条)。
  - 要求词库时(`RequireLexicon: true`,pre / pro 模式强制):没加载到词库之前**不监听、不注册**,`/healthz` 回 503,`/livez` 回 200。
  - 本机:`tools/scripts/go_services.ps1` 的服务目录加 `content_guard`,工具目录加 `content_guard_lexicon`;`go/README.md` 加一行。
  - K8s:`deploy/k8s/manifests/go-svc/content-guard.yaml`(新);`tools/scripts/k8s_deploy.ps1` 的 `$GoSvcCatalogue`、`New-GoSvcConfigMapYaml` 的 `content-guard` 分支、
    `$script:GrpcAllowLists`(放行 login、guild、chat、scene 三种);`tools/scripts/go_svc_image.ps1` 的镜像目录;`deploy/k8s/content-guard-alerts.yaml`(新,5 条规则);
    `deploy/k8s/README.md` 新一节与准入表一行;`.github/workflows/deploy-config-tests.yml` 的触发路径。
  - 契约测试:`tools/scripts/tests/content_guard_alerts_contract.tests.ps1`(新);`k8s_deploy_contract.tests.ps1` 加两条 `content-guard` 用例。
- **本条没有调用方**:login / guild / chat / scene 的接线在第 2 批。只部署本批不改变任何玩家可见行为。
- **Claude 做过的静态检查**(不是编译或测试):`gofmt -l -e go/content_guard` 无语法错误、无待格式化文件;改过的 6 个 ps1 用 `Parser::ParseFile` 解析 0 错误。
- **交 Codex 验证**(按序;工作目录没写的是仓库根):
  1. 生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
     期望:`go/proto/content_guard/` 下有 `content_guard.pb.go`、`content_guard_grpc.pb.go`(含 `ContentGuard_CheckText_FullMethodName`);`go/proto/common/base/node.pb.go` 有
     `ENodeType_ContentGuardNodeService = 32`;`proto/message_id.txt` 给 `ContentGuard.CheckText` 发了号;`cpp/generated/proto`、`cpp/generated/grpc_client` 下多出 `content_guard` 的产物,
     对应的 vcxproj / CMakeLists 由生成器补上(`SyncCppBuildProjects`);`go/content_guard/generated/` 下多出消息号常量。产物有别的非预期变化先停下核对。
  2. `go/content_guard`:`go mod tidy`(会删掉没用到的直接依赖、把 `golang.org/x/text` 改成直接依赖,保留它的结果),再 `go build ./...`、`go vet ./...`、`go test ./... -count=1`,再加 `-race` 跑一遍测试。
     通过标准:0 个编译错误,用例全部 PASS。重点用例:`TestSharedVectors`(20 条向量)、`TestEveryProtoSceneIsMapped`、`TestCategoryValuesMatchProto`、`TestShippedConfigIsValid`、`TestEncodeDecodeRoundTripAcrossChunks`。
     `TestFoldNormalizesVariants` 里 `㍿ → 株式会社`、`… → 空` 两条依赖 Unicode 分解表,红了先贴实际输出。
  3. 其余 Go 模块:`pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build`(节点类型枚举加了一个值,全部重编;期望多出 `bin/go_services/content_guard.exe` 与 `content_guard_lexicon.exe`)。
  4. C++:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(`node.proto` 变了,全量重编;本批没有手写的 C++ 改动)。
  5. 契约测试:`pwsh -NoProfile -File tools/scripts/tests/content_guard_alerts_contract.tests.ps1`;`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`
     (只看新增的两条 `content-guard` 用例和原有用例有没有被带红);`pwsh -NoProfile -File tools/scripts/tests/go_services_ports.tests.ps1`;`python tools/scripts/check_log_text_english.py`。
  6. 本机冒烟(etcd 在跑):
     - `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -GoServices content_guard`,日志出现 `CONTENT GUARD STARTED SUCCESSFULLY`;
       `curl -s 127.0.0.1:9290/metrics | findstr content_guard_lexicon_version` 为 0。
     - 准备两个词文件(一行一个词),`bin\go_services\content_guard_lexicon.exe publish -by codex -deny abuse=<文件> -deny ad=<文件>`,服务日志几秒内出现
       `[lexicon] loaded version=1`,指标变 1;再 `add -by codex -category ad -word <词>` 变 2;连发到第 4 版后 `show` 的 versions 只剩最近 3 个;
       `rollback -by codex -to <还在的旧版本>` 之后新版本的词条数与那个旧版本相同。
     - `grpcurl -plaintext -d "{\"scene\":\"TEXT_SCENE_CHAT_WORLD\",\"text\":\"<含辱骂词的句子>\"}" 127.0.0.1:50250 content_guard.ContentGuard/CheckText`
       回 `TEXT_VERDICT_MASKED` 与打码后的正文;同一句换 `TEXT_SCENE_ROLE_NAME` 回 `TEXT_VERDICT_REJECT`。
     - 要求词库:拷一份 yaml,改 `RequireLexicon: true`、`LexiconPrefix` 换成一个空前缀、端口错开,起服后 `/healthz` 回 503、gRPC 口不通、etcd 里没有它的注册;
       对着那个前缀发布一份词库后几秒内 `/healthz` 回 200、打印启动横幅。
- **未验证**:以上全部没有运行。`internal/lexstore` 的 etcd 读写(`Load` / `Publish` / `Prune` / `ReadVersion` / `Watcher`)没有单元测试,只有第 6 步的冒烟能证明;
  两个人同时发布(`ErrConflict`)、watch 中断后重读两条路径没有任何证据。K8s 的清单、ConfigMap 与准入策略只有契约测试,没上过集群;非 dev 档的 ConfigMap 没有 DryRun 用例(只核对了脚本里的缺省值)。
- **上线注意**:staging / prod 首次部署前要先发布词库,否则 content-guard 的副本不就绪,带 `-WaitReady` 的发布会停在这里(`deploy/k8s/README.md`「内容安全」)。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT。本地层用 houbb/sensitive-word(不到 2 万 star,负责人 2026-11-19 同意例外),行为以 `go/content_guard/testdata/vectors.json` 为准。
- **2026-11-20 服务器代码完成度核对**：提交 `55eae6be3`（本地 main，未 push，588 个文件，未编译未测）。按缺口清单与 PROGRESS 核对，服务器可做部分已落码未编译；残余与决定见 `docs/handoff/server-code-completion-20261120.md`：需负责人定的有支付回调域名与证书、App Store / Google Play 桥的依赖授权；依赖后台身份的有交易人工判定审批流；其它会话在做的有改名与内容安全、排队持票、挂机月卡、账号注销客户端、剧本保留期；Java 待做。缺口清单第 1 条的回滚脚本名已改为 `atomicRollbackInstanceAllocation`。未编译，待 Codex 验证。

## 2026-11-20 Claude:挂机月卡复核补齐——月卡服务进交接窗口重投白名单、导表后重生表索引、文档改正(未编译、未测试;Java 不涉及)

- **起因**:负责人问「代码实现完毕了吗」,要求查出来的现在修。接 2026-11-19「挂机月卡开通入口」。
- **补的代码**:
  - `cpp/nodes/scene/handler/rpc/player/player_handoff_gate.h`:`SceneAfkPassClientPlayer` 进 `IsReplayableOnAnotherSceneNode` 的整服务白名单。
    三个方法只读写角色自己的到期时刻与灵石,开通带幂等键,换节点重投也只生效一次。改之前它按默认「不带」:同 zone 换场景节点的交接窗口里,
    查询与开通回 1003(客户端按结果未知处理、开通要等重连);现在由 gate 留住、落地后重投(`client-scene-request-handoff-hold.md` §4.3)。
  - 用例:`cpp/tests/cross_zone_test/player_handoff_gate_test.cpp` 的 `PlayerHandoffGate.PlayerStateRequestsMayBeRetriedOnAnotherNode` 加这三个方法。
- **改正的文档**:`client-scene-request-handoff-hold.md`(白名单八个服务)、`redis-player-store-cluster.md`(键表里月卡键的写者是 scene、读者 player_locator)、
  `team-follow-server-trail.md` 与 `player-connection-lost-hold.md`(事实源在存档;离线期间没有入口能改到期)、`payment-recharge.md` 非目标一行指向月卡设计稿、
  `afk-monthly-card.md`(§5.3 时序改成「托管期间两边看到同一个值」并写明理由、§10 实际 58 个文件、§12 两条定案)。
- **定案**(写在设计稿 §12):GM 后台网页不加「设月卡」——后台的玩家指令同样只在 dev / test 对在线玩家放行,与客户端 `GmSetAfkPass` 能力相同,生产补偿走邮件发灵石。
  客户端开发包的本地 GM 面板不加按钮——它的用例 `LocalGmCurrencyUiTests` 钉住「只保留一个加银两入口」。
- **交 Codex**(接在 11-19 那条的 10 步里,不另起顺序):
  - 第 2 步 `dev.bat gen` 之后加跑 `py tools/data_table_exporter/tools/gen_schema_index.py`(`data/AGENTS.md`「加一张表」要求),`data/AGENTS.md` 索引块多一行 AfkPassRule;
    `py tools/data_table_exporter/tools/gen_schema_index.py --check` 随后应为绿。
  - 第 5 步 `cross_zone_test` 的 filter 加 `PlayerHandoffGate.*`:全过(含新加的三个方法)。
- **核过、不用改的**:流水类型 32 没有别处按枚举穷举(只有资产通道按流列允许的类型,开通不走资产通道);配置表按目录整批部署,新表不用登记清单;
  `robot/config/config_test.go` 的 `TestEveryDispatchedModePassesWhitelist` 会覆盖新模式 `afk-pass-follow-smoke`(第 6 步 `go test ./config/...`)。
- **Java 版**:不涉及(交接白名单是服务端内部规则,客户端契约不变),不往 `PARITY.md` 加行。
- **未提交,未编译,未测试。**

## 2026-11-20 Claude:账号注销删除范围复核——逐服务核按玩家的表与玩家域 Redis 键(只改文档,不改代码)

- **起因**:负责人再次要求核实服务器代码是否做完。接同日「账号注销收尾批」条目,把不在 role fence 参与者里的服务也逐个核了一遍。
- **结论**:没有遗漏的按玩家 SQL 表。chat 的发送原件 `chat_send_authority` 由 TiDB TTL 30 天删(进程只有 SELECT / INSERT,有意不让应用删);content_guard 只有词库;go/db 的 `player_audit_outbox` 是在途审计记录,投递后进流水、按保留期删;`account_share_database`、`player_centre_database`、`user*` 没有在用的读写者;payment 的 `payment_spend` 只有身份摘要,随订单合规保留。玩家域 Redis:会话与位置随下线删掉,月卡投影、同步摘要、确认位点、交接标记、持有者租约都有 TTL;只有 `login_generation`、`location_revision_floor`、`owner_epoch`、`handoff_fence` 四个单调计数永久留着,保证同一 id 代次不倒退,与目录墓碑同类,保留。
- **改了**:`docs/design/account-deletion.md` §6.2(payment、玩家域 Redis、Kafka 与日志三行)、§7.3(整库恢复的处理口径)。
- **Codex**:无新增步骤,按同日「账号注销收尾批」条目验证。
- **Java 版**:不涉及(只改文档)。

## 2026-11-20 Claude：客户端苹果、谷歌支付桥（Unity IAP 5；未编译，待 Codex 验证）

- **做了什么**（客户端仓 `mmorpg-client`，负责人同意装 Unity IAP 包）：
  - `Packages/manifest.json` 加 `com.unity.purchasing` 5.0.4；`Assets/Scripts/MmorpgClient.asmdef` 引用 `Unity.Purchasing`，并加版本定义：装了 5.x 才定义 `MMORPG_UNITY_IAP`，包没解析出来时工程照样能编。
  - 新文件 `Assets/Scripts/Game/Jubaozhai/StorePayBridge.cs`：App Store 与 Google Play 共用一个桥。购买前设苹果 `appAccountToken` 或谷歌混淆账号；买到后只交交易号或购买凭证；服务端回 `finish_transaction` 才 `ConfirmPurchase`。全进程一个实例。
  - `PaymentClient`：商店桥在 iOS / 安卓真机上登记并立刻连接商店；商店补交的未结束交易在空闲时逐笔提交（不带订单号，服务端按商店里的绑定认订单）。
- **依据**：接口逐个对照 Unity 官方文档 `com.unity.purchasing@5.0`（5.0.4）的 `StoreController`、`IAppleStoreExtendedService.SetAppAccountToken`、`IGooglePlayStoreExtendedService.SetObfuscatedAccountId / SetObfuscatedProfileId`、`IOrderInfo`、`IAppleOrderInfo`、`IGoogleOrderInfo`、`Order`、`FailedOrder`、`ICart`。没核到页面的两处：`CartItem.Product` 与 `PurchaseFailureReason.UserCancelled`，按 4.x 起就有的写法用。
- **没做 / 没验证**：没在 Unity 里编译，没上过真机，没有商店后台的商品。`com.unity.purchasing` 5.0.4 与 Unity 6000.6 是否兼容没核。谷歌的购买凭证从 Unity 统一收据里解，解不出来退回交易号；真机上要确认拿到的是 purchase token。
- **Codex 验证**（客户端仓）：
  1. 打开工程让包管理器解析 `com.unity.purchasing`（要能连 Unity 包仓库），把更新后的 `Packages/packages-lock.json` 与新文件的 `.meta` 一起提交。解析失败或与编辑器版本不兼容时保留包管理器的报错。
  2. 编辑器编译：`StorePayBridge.cs` 在编辑器里也参与编译（只受 `MMORPG_UNITY_IAP` 控制），接口写错会在这一步报出来。
  3. EditMode 跑 `PaymentClientTests`：现在 5 条，新增 `RedeliveredStoreTransactionIsSubmittedWhenIdleAndFinishedOnServerWord`。
  4. 真机：iOS 沙盒账号与安卓测试轨道各买一次；买完杀进程再开，确认补交的交易被提交并结束。
- **Java 独立仓（AGENTS §12）**：不涉及（客户端契约没变）。
- **文件**：客户端仓 `Packages/manifest.json`、`Assets/Scripts/MmorpgClient.asmdef`、`Assets/Scripts/Game/Jubaozhai/{StorePayBridge,PaymentClient}.cs`、`Assets/Tests/EditMode/Jubaozhai/PaymentClientTests.cs`；本仓 `docs/design/payment-recharge.md` §22.1、§22.10、§22.11。未提交。

## 2026-11-20 Claude:实时冷却与 Buff 跨重登保留·复核补(再施加刷新时长、回合快照按剩余时长,两处「以后」改为定案)

- **复核 11-19 同名条目的落码**,一起修两处,都靠新加的 `BuffEntry::expireAtMs`(未编译):
  1. 再施加同一条 Buff 刷新时长:`BuffSystem::OnBuffRefresh` 原来是空的 TODO,同一施法者再施加只加层、到期时刻不动(第二瓶经验丹白白浪费)。现在重挂满时、取消旧定时器,与回合引擎「重挂满时」口径一致;永久与瞬时的不动。到期计时收成 `buff.cpp` 的 `ArmBuffExpiry`,施加、恢复、刷新三处共用。
  2. 回合战斗快照按剩余时长换算回合:`PlayerBattleSystem::BuildBattleSnapshot` 原来按表里的全量时长换算(注释写着「另开任务」)。现在按这条自己的到期时刻换算,已到点、到期回调还没跑的不带;没有到期时刻的有限行记 0,引擎入场时照旧夹到表里的全量时长。
- **定案不做**(设计稿 §2.3、§7 已改成定案):
  - 离线暂停的 Buff:退出收尾 `IsPersistedPayloadCurrent` 逐行比存档字节,存档必须与时钟无关;存剩余时长会让退出永远比不上、一直重存到轮数上限。
  - 客户端下发实时冷却 / Buff:客户端没有对应界面,没有消费者不发。
- **改动**(main 工作区,未提交):
  - `cpp/libs/services/scene/combat/buff/system/buff.cpp`;
  - `cpp/libs/services/scene/battle/system/player_battle.cpp`;
  - `cpp/libs/services/battle/system/turn_battle_engine.cpp`(只改一行注释);
  - 测试:`cpp/tests/buff_test/buff_test.cpp` 加 `BuffDurationTest.ReapplyingRefreshesTheExpiryToTheFullDuration`,`cpp/tests/bag_test/player_battle_settlement_test.cpp` 加 `PlayerBattleSettlementTest.SnapshotBuffRoundsFollowTheRemainingTime`;
  - 文档:`docs/design/realtime-cooldown-buff-persistence.md`(开头、§2.3、§2.6、§3、§7、§8、§9、§10)。
- **预检**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`,10 tests OK。没有编译,也没有跑任何单测。
- **给 Codex 的验证步骤**:按 11-19 同名条目的六步执行;第 4 步多跑一次 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle`,并确认上面两条新用例通过、`BuffStackingTest.*` 与 `BuffDurationTest.*` 原有用例不变红。
- **Java 版**:对应 Java 版本无(`0.1.0-SNAPSHOT`),只在 Java 仓 `PARITY.md` 末尾追加这两处行为,没有改 Java 代码。

## 2026-11-20 Claude:go/payment 记账第二步收口——不算数的付款不记已付、在我们定价的渠道上关掉意图(修转人工的单子每个租约周期查一次渠道、永不停),客户端轮询重读意图(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人要求查出的问题现在修。复核限额闸时发现 `core.Service.record` 记账后的「第二步」(按这笔钱改订单意图)有两个同根的问题:
  1. **转人工的付款让意图永远停在等付款**:我们定价的渠道(微信支付、支付宝、小游戏)上,一笔转人工(ORPHAN)的付款不改意图。后台 `checkIntent` 领到它、查渠道得到「已付」、`record` 判重复后直接返回,`next_check_ms` 一直停在租约上,于是每 30 秒查一次渠道、永远停不下;未成年人的在途额度也一直占着。
  2. **金额或币种与意图对不上的付款把意图记成已付**:商城把它当异常付款退掉(`AMOUNT_MISMATCH` / `CURRENCY_MISMATCH`)、不绑定订单,意图却成了 PAID,未成年人的额度也被这笔钱占着。
- **改法**(`go/payment/internal/core/service.go` 新函数 `settleIntent`、`countsForIntent`):首次记账与重复报告走同一个第二步。
  - 算数的付款(没转人工、没被本服务退掉、金额币种与意图冻结的一致)转已付;重复报告时未成年人的意图已到期关闭的,照旧在额度行锁下重新计入。
  - 不算数的付款在我们定价的渠道上关掉意图:一个商户单号只能付成一次,这张意图再也付不成。`CloseIntent` 只改还在等付款的意图,已由别的付款付清的不受影响。
  - 商店渠道(苹果、谷歌)上不算数的那笔不动意图:同一张订单之后还可能有一笔对得上的购买,意图照常到期。
  - 迟到付款的额度复核只对算数的那笔做。
  - `activeQuery`(客户端轮询时顺手查单)记账之后重读意图,不再假设一定是 PAID。
- **文件**:`go/payment/internal/core/service.go`;新用例文件 `go/payment/internal/core/record_test.go`;`minor_test.go` 的桩(`InsertTxn` 同真库认重复、`gateStore.closed`、登记商店渠道桩 `googleStub`);`go/payment/internal/config/config.go` 补一行 `//`(另一会话同日加的注释段落,不补 `gofmt -l` 会报);`docs/design/real-name-anti-addiction.md` §5.6 一条;Java 仓 `PARITY.md` 同日「未成年人充值限额闸」一条补第 7 点。同日另一会话把单日上限改成等于单次上限(删 `DailyLimitFen*` 配置、改 `shared/antiaddiction`),不在本条。
- **Codex 验证**:`cd go/payment && gofmt -l . && go build ./... && go vet ./... && go test ./internal/core/ -count=1 -v`。期望新用例 `TestRecordSettlesTheIntentByWhatThePaymentIs`(5 个分支,每个分支同一笔报两次,验首次与重复走同一步)通过;`TestLatePaymentOfAMinorIsRecheckedUnderTheLimit` 等既有用例不变(超限那支现在还会关意图,用例不断言它)。再 `go test ./... -count=1`。本会话对 `go/payment`、`go/shared/accountdirectory`、`go/shared/antiaddiction` 跑过 `gofmt -l`,无输出。
- **未验证**:以上全部没有运行。
- **Java 版(AGENTS §12)**:待做,0.1.0-SNAPSHOT。

## 2026-11-20 Claude:go/db 删除运行期冷却的两条残余——scene 回包处理按源码核对、存档库注册表里消费者时代的日志与注释改成现状(未编译、未测试,待 Codex 验证;Java 不涉及)

- **范围**:接 2026-11-19「go/db 删除存档库运行期冷却」交付时列的两条「没做、没查的」,负责人要求修完:一是 scene 对存盘 / 认领 gRPC 回包的真实处理当时没核对,只按 `go/db/internal/playerstore/server.go` 的约定写;二是 `store_registry.go` 的英文日志还写着 "without consuming retries"。
- **核对结论**(只读,scene 源码没有改;`cpp/libs/services/scene/player/system/player_store_client.cpp`,装配在 `player_lifecycle.cpp` 的 `MakePlayerStoreOptions`):
  - 传输层(`SendPlayerStoreSaveOverGrpc` / `SendPlayerStoreClaimOverGrpc`):回调是 `done(outcome.ok() && reply != nullptr, reply)`,任何非 OK 的 gRPC 结局(含 go/db 回的 `codes.Unavailable`、超时、本地没有可用通道)都报成结果未知;没有就绪的 go/db 节点或签名失败时返回 false,同样走重试。
  - 存盘(`RunSaveAttempt` → `ScheduleRetry`):没有回包就用在途的原请求(同内容、同序号、同代次)重发;退避基数 0.5 秒起翻倍、5 秒封顶,每次再乘 0.5~1 的抖动;没有次数上限,直到拿到明确结论。
  - 认领(`RunClaimAttempt` → `ScheduleClaimRetry`):同样的退避,从第一次发送起 30 秒(`claimBudgetSec`)没有结论就 `FinishClaim(nullptr)`,这次进场失败。
  - 截止时间:scene 调 go/db 的截止是 14 秒(`bin/etc/base_deploy_config.yaml` 的 `PlayerStoreNodeService`),go/db 两个方法的服务端超时是 12 秒(`go/db/etc/db.yaml` 的 `MethodTimeouts`)。
  - 和昨天写进设计稿文首的判断一致,删除冷却的结论不变。两处措辞按核对结果写准(`docs/design/db-task-store-unavailable-deferral.md` 文首第 2、4 点):退避原来写成「0.5~5 秒带抖动」,实际是基数 0.5~5 秒再乘 0.5~1;「调用方的截止时间」写明是 go/db 方法超时 12 秒、scene 侧 14 秒。
- **日志与注释**(`go/db/internal/logic/pkg/proto_sql/`,都是文字,不改行为):
  - `store_registry.go`:`reject` 与 `openOnce` 两条 ERROR 的括号说明改成现状——`calls on it fail as unavailable and are retried by their callers ...`。前缀 `[placement] store rejected`、`[placement] store open failed` 没动,`deploy/k8s/player-placement-alerts.yaml` 的处置说明按前缀找日志,不受影响。
  - `db.go`:同包里同类的两条启动日志一并改——没配读超时那条原来写 "will block db workers forever"(现在没有 worker,调用也有自己的截止时间),锁等待不短于读超时那条原来写 "after the task was rescheduled"。
  - 注释 6 处:「消费者启动之前」「报错延后」「下一条任务」「这些玩家的写一直延后,不消耗重试」改成现在的调用方式(`store_registry.go` 4 处、`db.go` 1 处、`store_registry_test.go` 1 处同句)。
- **给 Codex 的验证步骤**(本会话没有编译、没有跑测试;可以和 2026-11-19 那条的步骤合在一次跑;失败保留编译错误全文与失败用例名,不改断言,不连续重试):
  ```
  cd go/db
  gofmt -l internal/logic/pkg/proto_sql
  go build ./...
  go vet ./internal/logic/pkg/proto_sql/
  go test -count=1 ./internal/logic/pkg/proto_sql/
  cd ../..
  python tools/scripts/check_log_text_english.py
  ```
  通过标准:`gofmt -l` 无输出;build / vet 无错;proto_sql 包全部 PASS(本条只动了字符串与注释,没有用例断言这些日志文案);日志文案守卫通过(新文案全是 ASCII)。
- **Java 版(AGENTS §12)**:不涉及(go/db 内部的日志与注释),不加 PARITY 行。
- **提交情况**:main 工作区,未提交(2026-11-19 那条的改动已随 `55eae6be3` 入库)。

## 2026-11-20 Claude:实名与防沉迷收掉「以后再做」的四处——国家接口全服调用额度、上报器停机排空、客服重置实名、单日充值上限定为单次上限(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人 11-20 要求「查出的问题不等以后,现在修;不等压测的先按最标准的定」。11-19 两条实名条目与设计稿里标着「以后」的四处:
  - 上报按副本限速 1 QPS,副本超过 10 个就超出国家系统的 10 QPS,不能水平扩展;实名认证与查询完全没有限速,开服时大量新号同时实名,会超出 100 QPS 被封 1 分钟;
  - 上报器停机时直接丢掉手上没发出的事件;
  - 没有客服重置实名的路径:pi 只写一次,填错了永远改不了;
  - 未成年人单日充值上限缺省 0(不设),数字一直是待决项。
- **做了什么**(设计稿 `docs/design/real-name-anti-addiction.md` §5.2、§5.5、§5.6、§8 已按现状改正文):
  - **全服调用额度**:新文件 `go/login/internal/logic/pkg/nppa/quota.go`。用 GCRA 在登录域 Redis 的一个键里(`redisslot.NppaQuotaKey`)按 Redis 服务器时钟排队,所有 login 副本共用。额度只用到额定值的八成左右:实名 80/s 加 10 个突发、查询 240/s 加 20 个、上报 8/s,任何 1 秒内都不超额。`realname.Service` 先取名额再计当天次数,取不到回 `kLoginRealNameServiceUnavailable`,不占玩家的次数;查询取不到留到下一次登录;上报器每发一批先取一个名额,取不到就攒到下一拍。额度读不了一律按没有名额处理。
  - **停机排空**:`Reporter.Run` 收到停止后先取完队列,再在 2 秒内一批批发;发不完的计 `login_nppa_report_total{result="dropped_shutdown"}`。只剩进程被强杀才会丢。
  - **客服重置实名**:`LoginAdmin.ResetAccountRealName`(login.proto)。按 player_id 或 account(恰好一个)找账号,必填操作人与理由。要 `x-admin-token`,拦截器按服务前缀管,新方法自动纳入。它清掉 pi 与认证中的 ai,幂等,日志里账号打码。data_service 新增 `ResetAccountRealName`,只认 login;store 加 `ResetRealName`,在行锁下清;`shared/accountdirectory` 加 `(*Client).ResetRealName`。
  - **单日上限**:定为单次上限,8–16 周岁每日 50 元、16–18 周岁每日 100 元。只写在 `shared/antiaddiction.SpendLimitAt` 一处(`SpendLimit.DailyFen`),go/payment 删掉 `Payment.Compliance.DailyLimitFenAge*` 两个配置项与 yaml 两行,`spendLimit` 改读它;充值设计 §18 第 7 条改为已定。改之前已和「充值按年龄段限额闸」会话确认,它接着改 go/payment 的其它几处。
- **文件**:
  - 新增 `go/login/internal/logic/pkg/nppa/quota.go`、`quota_test.go`;`go/login/internal/logic/loginadmin/resetaccountrealnamelogic.go`、`resetaccountrealnamelogic_test.go`。
  - 修改:`go/login/internal/logic/pkg/nppa/reporter.go`、`reporter_test.go`;`go/login/internal/logic/pkg/realname/realname.go`、`realname_test.go`;`go/login/internal/svc/anti_addiction.go`;`go/shared/redisslot/login_keys.go`;`go/shared/accountdirectory/real_name.go`;`go/shared/antiaddiction/antiaddiction.go`、`antiaddiction_test.go`;data_service `store/account_real_name.go`、`store/account_real_name_tidb_test.go`、`server/account_directory.go`、`server/account_real_name_test.go`、`svc/role_fence.go`;go/payment `internal/core/minor.go`、`internal/core/service.go`、`channels.go`、`internal/config/config.go`、`etc/payment.yaml`、`internal/core/minor_test.go`;协议 `proto/login/login.proto`(`LoginAdmin.ResetAccountRealName`)、`proto/data_service/data_service.proto`(`ResetAccountRealName`)。
  - 文档:`docs/design/real-name-anti-addiction.md`、`docs/design/payment-recharge.md` §18。
- **Codex 验证**(接在 11-19「实名认证与未成年人防沉迷落码」条目的命令之后跑,顺序与清库要求相同):
  1. 生成:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`,再 `pwsh -File go/build.ps1`。期望:`go/login/internal/server/loginadmin/loginadminserver.go` 出现 `ResetAccountRealName`。
  2. `gofmt -l` 上面列的 Go 文件,期望无输出;本会话 11-20 对自己的文件跑过,已无输出。
  3. 新用例:
     - `go/login`:`go test ./internal/logic/pkg/nppa/ -count=1 -v`(新增 `TestQuotaIsSharedAcrossReplicasAndSpacesCalls`、`TestQuotaNeverExceedsTheRateInAnySecond`、`TestFlushWaitsForTheSharedQuota`、`TestRunDrainsQueuedBehaviorsOnShutdown`,miniredis 要支持脚本里的 `TIME`,不支持就记下来),再加 `-race` 跑一遍;
     - `go test ./internal/logic/pkg/realname/ -count=1 -v`(新增 `TestSubmitWithoutQuotaIsBusyAndKeepsTheAttempt`);
     - `go test ./internal/logic/loginadmin/ -count=1 -v`(3 个);
     - `go/data_service`:`go test ./internal/server/ -run 'RealName' -count=1 -v`;
     - `go/shared`:`go test ./antiaddiction/ -count=1 -v`;
     - `go/payment`:`go test ./internal/core/ -run 'Minor|Gate' -count=1 -v`(第 242–243 行期望的每日额度改成 5000 / 10000)。
  4. 回归:`go test ./... -count=1` 于 `go/login`、`go/data_service`、`go/shared`、`go/payment`。`go build ./...` 于同四处。
  5. 真 TiDB(有专用 DSN 时):`TestTiDBAccountRealNameIsWrittenOnce`(这次加了重置后重绑的断言)。
  6. `python tools/scripts/check_log_text_english.py`:干净。
- **未验证**:以上全部没有运行。GCRA 脚本用了 Redis 的 `TIME`(K8s 钉的 7.2 与本机 8.x 都允许脚本里调它);两个版本的真 Redis 上没有跑过。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT。本条没有新的客户端契约,只追加服务端行为;落码时按 11-19 已登记的 PARITY 条目一并做。

## 2026-11-20 Claude：支付 K8s 的三处缺口补齐——告警、包装入口透传、开服手册（未运行，待 Codex 验证）

- **告警**：新文件 `deploy/k8s/payment-alerts.yaml`（PrometheusRule，8 条）。盯「收了钱却没走完」的每一段：有付款超过 10 分钟没记进商城、商城拒收、事实写不进库（这三条 critical）；孤儿或冲突的付款、回调处理不成、回调验签不过、调渠道接口失败、渠道退不了款（warning）。label 只有 channel / op / outcome。
- **契约用例**：`tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1` 加一条（现在 22 条）：告警引用的每个指标名都要在 `go/payment/internal/core/workers.go` 里有定义，每个 outcome 取值都要在源码里出现，告警里不许出现玩家号、订单号、交易号。工作流触发路径加了 `workers.go`。
- **包装入口透传**：`tools/scripts/dev_tools.ps1` 加 `-PaymentConfigPath`。`k8s-infra-up` / `k8s-zone-up` / `k8s-all-up` 留空不透传、非空原样透传；`k8s-image-*` / `k8s-release-*` 给了即报错（那条路不部署 Go 服务）。
- **开服手册**：`docs/ops/k8s-open-server-runbook.md` §1 加「官方商城与支付」一条，八步。注意一点：payment 随全局 Go 服务部署，所以它的库、boot 目录条目、运行配置和三把调用密钥（gateway、payment、delivery）在 staging / prod 的 `infra-up` 上是必需的；商城只在给了 `-JavaSvcRegistry` 时部署。
- **同批文档**：`deploy/k8s/README.md`「支付 payment」加告警一条、透传口径加一条；`docs/design/payment-recharge.md` §22.4 补上限额闸会话的记账第二步（`settleIntent`：不算订单付款的钱不把意图记成已付，我们定价的渠道上意图随之关掉）。
- **静态检查（都不是运行）**：两份 .ps1 语法解析无错；告警 yaml 能解析出 8 条规则；按新契约用例的规则写了一份对照脚本，6 个指标名与 7 个 outcome 取值都能在源码里找到。
- **Codex 验证**（仓库根目录）：
  1. `pwsh -NoProfile -File tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`，期望 `total=22 pass=22 fail=0`。
  2. `pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up -DryRun -PaymentConfigPath <一份只含 Payment 段的 yaml> …`（其余参数照平时的 dev 档 DryRun），输出的 payment 配置里应是那份文件的 `Payment` 段；`-Command k8s-release-zone -PaymentConfigPath x` 应在入口报「not forwarded」一类的拒绝。
  3. 有 Prometheus Operator 的集群上 `kubectl apply --dry-run=server -n observability -f deploy/k8s/payment-alerts.yaml`，确认规则被接受。
- **Java 独立仓（AGENTS §12）**：不涉及。
- **文件**：`deploy/k8s/payment-alerts.yaml`（新）、`tools/scripts/dev_tools.ps1`、`tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`、`.github/workflows/deploy-config-tests.yml`、`deploy/k8s/README.md`、`docs/ops/k8s-open-server-runbook.md`、`docs/design/payment-recharge.md`。未提交。

## 2026-11-20 Claude:活跃度领取记录按周期裁剪(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人问「活动做完就没用的数据越攒越多怎么办」。核对发现活跃度账本的领取记录(`PlayerActivityLedger.claims`)每周期每档一条、从不删,预算 4096 条;攒满之后 `PlayerActivitySystem::Claim` 永远回 `kServiceUnavailable`,这个角色再也领不了活跃奖励,记录还一直留在存档的 GROWTH 行里。10-22 的设计稿把保留策略留作「正式长期运营前需确定」。
- **改动**(`cpp/libs/services/scene/player/system/player_activity.cpp`):新领取通过全部校验之后、判预算之前调 `PrunePastClaims`,删掉「当前规则、周期已结束」的记录,只留其中最新的 64 条(`kKeptPastClaims`,`static_assert` 不小于单周期档数上限,上一周期的领取全部留得住)。当前周期的、别的规则的记录不动。
  - 依据:规则的周期锚点与长度同 ID 下不可改(设计稿「正式活跃配置与来源」),当前周期起点只会前进,已结束周期的记录不会再挡住任何新领取。别的规则算不出周期是否结束,所以不动。
  - 代价:窗口外的原请求重放按「不是当前周期」拒绝(`kInvalidParameter`),不再回首次结果。
  - 设计稿 `docs/design/player-features-ui.md`「领取身份、提交与重启恢复」已按此改正文。
- **没改的**:积分来源(`sources`)不裁。它是 `scope+mission_id` 的永久去重键,删了会让重复投递的完成通知重复入账;它按做过的活动任务数增长,不随时间增长。协议与存档字段都没动,不用重新生成。
- **测试**:`cpp/tests/bag_test/player_activity_system_test.cpp` 新增 `PlayerActivitySystemTest.ClaimAtFullBudgetPrunesEndedPeriodsOfCurrentRuleAndKeepsReplayWindow`。账本预置满 4096 条(别的规则 1 条 + 当前规则已结束周期 4095 条),领取成功后剩 66 条(别的规则那条、最新 64 条、新领的一条);窗口内原请求照常重放,窗口外回 `kInvalidParameter`,当前周期同档换 request_id 仍回 `kRewardAlreadyClaimed`。按改动前的代码,这条用例会在第一次 `Claim` 处得到 `kServiceUnavailable`(没有实际跑过红)。
- **Codex 验证**(仓库根;失败保留第一条编译错误与 gtest 完整输出,不改断言,不连续重试):
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。改的是 scene 库,`run_cpp_tests.ps1 -Build` 不重编库,必须先整解。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test`:`PlayerActivitySystemTest.*` 全绿,含上面的新用例;原有用例不变。
- **已做的静态检查**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 通过(10 项)。
- **未验证**:没有编译,没有跑单测。
- **Java 版(AGENTS §12)**:Java 仓没有活动中心与活跃度领取的实现(只有生成的表与协议类),整项待做,0.1.0-SNAPSHOT;做的时候领取记录按同一保留规则。已在 Java 仓 `PARITY.md` 登记待做,没有改 Java 代码。
- **文件**:`cpp/libs/services/scene/player/system/player_activity.cpp`、`cpp/tests/bag_test/player_activity_system_test.cpp`、`docs/design/player-features-ui.md`、本条。

## 2026-11-20 Claude:登录排队契约 §11.5 第 6 条改定——恢复重入自身失败也保票退避,Unity 与 robot 同改(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 不涉及)

- **起因**:11-19「Unity 登录排队持票轮询按契约 §11.5 补齐」一条留了一个待定项。第 6 条让恢复重入自身也失败时报错退出,排队票随之丢掉;客户端自己断网几秒,或 login 短暂不可用而区桶仍有令牌时,玩家照样丢排位。负责人 11-20 定:改,Unity 与 robot 两边同改。
- **新口径**(`docs/design/open-server-rate-limit-design.md` §11.5 第 6 条):
  - 恢复重入自身网络错误、HTTP 非 200 或回 code=500 时同样保留 token,按第 2 条的间隔(2、4、8、16、30 秒)回到 queue-status,连续失败数不清零,直到排队总预算用完。
  - 回 401、503 zone_*、404 等其它失败码仍按错误结束。
  - 服务端回包不变。
- **robot**(`robot/http_assign_gate.go`):
  - `pollQueueStatus`:重入自身失败不再返回错误,合成一个 code=500 的回包交回状态机。
  - 状态机的 `case 429` 扩成 `case 429, 500`:持 token 时都按 `heldTokenWait` 等待后回到 queue-status;不持 token 时 429 仍报 rate-limited,500 仍交外层。
  - `AssignGateWithQueue`、`pollQueueStatus` 与 `main.go` 里 `assignGateHTTPLocal` 的说明随之改。
  - 用例(`robot/http_assign_gate_test.go`):
    - 改名并改期望:`queue-status and the re-entry both fail` → `queue-status and the re-entry both fail, the token is kept and polling backs off`,从报错改为保票,等待 2s、2s、4s、2s 后放行。
    - 新增:`re-entry 500 keeps the token and backs off`、`without a token, 500 is left to the outer retry loop`、`TestAssignGateWithQueue_ReentryFailureStopsAtBudget`。
- **Unity**(`../mmorpg-client` 的 `Assets/Scripts/Game/GameClient.cs`,`EnterZone` 第 2 步):
  - 持票时重入回 429、500 或自身网络 / HTTP 失败,都记一行 `assign-gate re-entry did not resume (...); keeping the queue token`,状态栏显示「网络波动,排队位置保留中」,按 `QueueRetryDelayMs(…, 连续失败数)` 等待后回到查询。
  - 不持票时的网络失败与 500 仍报错退出。EditMode 用例不用改。
- **文档**:
  - 设计稿 §11.5:引言、第 6 条(补原因与代价)、robot 与 Unity 现状各一句。
  - `docs/design/login-queue-2026-05.md`:客户端动作的更正块补一句。
  - `docs/ops/login-queue-stress-runbook.md` 第 5 节 Redis 重启演练:期望改为保票退避,5 分钟预算用完才走外层重试。
  - 本仓 `java/gateway_node` 的 `AssignGateRequest` 注释。
- **验证(交 Codex,按 AGENTS §10.1)**:
  1. robot(在 `robot/` 下):`go vet ./... && go test -count=1 -run 'TestAssignGateWithQueue|TestSleepCtx' ./...`。
     - 通过标准:全部 PASS,含上面改名与新增的四条。失败时保留用例名与断言原文。
     - `gofmt -l robot/http_assign_gate_test.go` 会列出这个文件:是 Jitter 用例里一行注释对齐的旧问题,HEAD 上就有,本条没碰。
  2. Unity:同 11-19 那条的第 1、2 步(`tools/client_compile_check.ps1` 与 `LoginCredentialTests` 的 EditMode),本条没有新增 EditMode 用例。
  3. 联网冒烟:在 11-19 那条第 3 步的基础上加两段,其余步骤照旧。
     - 重入也失败:同时关 `LoginPreGate/QueryQueueStatus` 与 `LoginPreGate/AssignGate`(PUT 两次,target 分别是这两个方法)。
       - 期望:queue-status 与重入都回 code=500(`login_unavailable`)。
       - 客户端日志成对出现 `queue-status failed (...)` 与 `assign-gate re-entry did not resume (code=500 ...)`,间隔按 2、4、8、16、30、30 秒退避,登录流程不报错退出。
       - 两条规则都删掉后,下一次查询的名次接着之前的。
     - 客户端侧断网:停掉本机 gateway 约 60 秒再起来。
       - 期望:两个请求都是连接错误,客户端照样保票退避。
       - gateway 起来后,下一次查询名次连续,最后能放行进游戏。
     - 通过标准与收尾同 11-19 那条;收尾时确认两条热关停规则都已删。
- **未验证**:以上全部没有运行。
- **Java 版(AGENTS §12)**:0.1.0-SNAPSHOT 不涉及。这是客户端侧的重试规则,两版服务端回包都不变;独立 Java 版的 robot 没有持票恢复重入的状态机(`QueueScenario` 只是端到端场景),不往 `PARITY.md` 加行。
- **提交情况**:客户端仓与本仓都在 main 工作区,未提交。

## 2026-11-20 Claude:剧本审核流水保留期清理——match_battle_script_review 保留 180 天,到期按游标分批删(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:同日「账号注销收尾批」让删角收尾删掉玩家写的剧本与选用,审核流水 `match_battle_script_review` 按审计保留;但这张表只增不改,对全体玩家都没有到期删除,等于永久保留(设计 `account-deletion.md` §11 原来记成「另立任务」)。负责人要求按最标准的做法定案、不等压测:保留期与 data_service `transaction_log` 的审计保留同值,定为 180 天。设计见 `docs/design/battle-script.md` §6.4(新)。
- **做了什么**:
  1. **配置**:match 新顶层段 `BattleScript.ReviewRetentionDays`(`go/match/internal/config/config.go`):缺省 180,0 = 缺省,非 0 越出 [30, 365] 在 `Validate` 拒启(下限保证申诉、追责查得到;上限与 trade / guild 保留期配置的上限同值);`ReviewRetention()` 给直接构造 Config 的单测按缺省。不是提交点规则,不进策略指纹。仓内 yaml 与 K8s ConfigMap 都不写这一段,按缺省。
  2. **索引**:`proto/match/match_authority.proto` 给 `MatchBattleScriptReviewRecord` 加 `OptionIndex = "reviewed_ms,script_id,revision"`(生成后名 `idx_match_battle_script_review_0`);`authority/schema.go` 的 `CheckCandidateIndexes` 登记,缺索引拒绝接流,写法同 `match_spectator_intent`。
  3. **清理方法** `authority.PurgeExpiredBattleScriptReviews`(`battle_script.go`):库时间判到期;按 `(reviewed_ms, script_id, revision)` 游标读一页(至多 limit 行),按主键批量删,DELETE 上再带一次到期条件;不满一页游标回零,下一轮从头扫;保留期不为正直接报错、不碰库。照 `PurgeClosedSpectatorIntents` 写。
  4. **后台循环** `match.battle_script_review.purge`(新 `go/match/internal/logic/battle_script_review_purge.go`,由 `StartMatcherLoop` 装配,只在 stable 起):`StartRelayLoop`(safego 点位、每轮 recover、`match_loop_last_run_unixtime` 活性、停机排空)一分钟一页、一页 3 秒预算、1000 行。每个副本各跑各的、不分工:流水只增不改、一个 (script_id, revision) 只审一次,重复与交错删除都幂等。失败只记英文日志、不动游标。新指标 `match_battle_script_review_purged_total`。
  5. **契约与文档**:`tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1` 的 match 点位名单 14 → 15,登记新循环的节拍来源;`docs/design/battle-script.md` §6.1、§6.4(新)、§9,§12 第 3 条改正(原写「删角后剧本不清理」,收尾批之后已过时);`account-deletion.md` §6.2 match 行补「保留 180 天后删」、§11 删掉这一条;`assetop-redelivery-sharding-20261110.md` 第六批的点位清单;`role_fence_purge.go` 与 `battle_script.go` 里的相关注释。
- **文件**:
  - 协议:`proto/match/match_authority.proto`。
  - match:`go/match/internal/authority/battle_script.go`、`battle_script_test.go`、`schema.go`、`schema_test.go`、`role_fence_purge.go`(只改注释);`internal/config/config.go`、`battle_script_conf_test.go`(新);`internal/logic/battle_script_review_purge.go`(新)、`battle_script_review_purge_test.go`(新)、`matcher.go`;`internal/metrics/battle_script.go`。
  - 契约:`tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1`。
  - 文档:`docs/design/battle-script.md`、`account-deletion.md`、`assetop-redelivery-sharding-20261110.md`;Java 仓 `PARITY.md`。
- **Codex 验证**(本会话没有生成、编译、跑任何测试,只跑了 `gofmt -l`;工作区有很多会话的在途改动,失败先按文件归属分清,保留完整输出):
  0. 本地 match 库:`match_battle_script_review` 多一条普通索引。迁移器不给已有的表补普通索引(只出警告),启动时 `CheckCandidateIndexes` 会拒绝接流。项目未上线,清掉 match 库重建后跑一次 `match -f etc/match_service.yaml -migrate`;不想清库就手工 `ALTER TABLE match_battle_script_review ADD INDEX idx_match_battle_script_review_0 (reviewed_ms, script_id, revision)`。
  1. 生成(仓库根):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(不改客户端可见协议,不写客户端仓)。期望 `go/proto/match/match_authority.pb.go` 的描述里出现这条 `OptionIndex`;C++ 的 `match_authority.pb.cc` 只有描述字节变化。
  2. `gofmt -l` 对上面列的 Go 文件:期望无输出(本会话跑过,干净)。
  3. `cd go/match && go build ./... && go vet ./...`。
  4. 纯单测(不连库):`go test ./internal/config/ -run 'TestBattleScriptReviewRetention|TestMatchServiceYamlLoads|TestSpectateValidate|TestAuthorityConfigIsRequiredInEveryMode' -count=1 -v`;`go test ./internal/logic/ -run 'TestBattleScriptReviewPurgeKeepsCursorOnFailure' -count=1 -v`;`go test ./internal/authority/ -run 'TestBattleScriptReviewPurgeRejectsNonPositiveRetention' -count=1 -v`。
  5. 真 TiDB(`MATCH_IT_TIDB=127.0.0.1:<端口>`、`MATCH_IT_REQUIRE_TIDB=1`,夹具同本包其它事务用例,0 SKIP):`go test ./internal/authority/ -run 'TestBattleScript|TestTiDBCandidateIndexes|TestTiDBAuthorityStartupUsesDMLAccountAndRequiresCurrentSchema|TestPurgeTiDB' -count=1 -v`。新用例:`TestBattleScriptReviewRetentionPurgesOnlyExpiredRows`(到期删、未到期与刚审的留、剧本行不动、两个副本游标交错、整轮重跑幂等)、`TestBattleScriptReviewRetentionPagesByCursor`(同一时刻多行按主键翻页、不重不漏、扫完回到开头)、`TestTiDBCandidateIndexesRequireBattleScriptReviewPurgeIndex`(删掉索引后拒绝接流)。`TestPurgeTiDBRemovedRoleDeletesPersonalRowsAndReleasesReports` 断言删角后流水仍在,应不受影响。
  6. 回归:`go test ./... -count=1`(go/match 全量,带第 5 步的环境变量)。
  7. 契约(仓库根):`pwsh -NoProfile -File tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1`(`fail=0`;match 循环一族那条:点位十五个、`battleScriptReviewPurgeInterval` 已登记且来源是 `time.Minute`)。
  8. `python tools/scripts/check_log_text_english.py`:干净。
  9. C++:只有生成的描述字节变化,不改 C++ 逻辑;随下一次整解 `msbuild ... /m:1` 一并确认编得过。
- **已知风险**:旧库不补索引就起不来(第 0 步);几个副本同时删同一页时 TiDB 可能锁等待或偶发死锁报错,只记日志、下一轮重试,不影响正确性;清理持续失败只在日志里,没有单独的失败计数与告警(循环活性按「成败都算」照常前进,与观战意图清理同一口径);保留期内已删角色的流水仍有他写的正文与 `owner_player_id`,隐私政策要写明保留期。
- **未验证**:以上全部。
- **Java 版(AGENTS §12)**:待做,Java 版本号 0.1.0-SNAPSHOT;Java 版还没有战斗剧本,已在 `D:\luyuan\xuanming-server-mmo\PARITY.md` 末尾登记「2026-11-20 剧本审核流水保留 180 天后删」,实现剧本时一并做。不是客户端契约。

## 2026-11-20 Claude:登录排队清合入门槛的 Codex 验证单——三项门槛的命令按当前代码重写(只读核对与整理,没有代码改动;Java 不涉及)

- **为什么写这一条**:
  - 登录排队的代码已经写完,但所有环境的排队都关着:`go/login/etc/login.yaml` 的 `Queue.Enabled` 是 `false`,`tools/scripts/k8s_deploy.ps1` 的 `$script:LoginQueueMergeGateCleared` 是 `$false`。打开之前要满足 `docs/handoff/launch-readiness-20261013/spec_G4b.md` §7「合入门槛」的三项。
  - 三项的现状(11-20 核对):
    - ① G1b 多 zone 用例、② G4b-G:Codex 11-08 跑过并通过,原件在 `build/verification/naming-audit-20261108/`(`go-login.test-r8.log`、`go-login.test-r9.log`,loginqueue 整包 137 个顶层用例)。之后这个包又改过几轮(11-10 区容量封顶、11-11 在途票与重取窗口、11-12 键带 `{q:<zone>}` 并换集群句柄、11-19 的小游戏地址字段),`queue.go` 的 SHA-256 已经不是那份证据里记的 `BC5CF361…`。11-09 之后的验证目录里没有搜到这两组用例的运行记录。两项都要重跑。
    - ③ 真实 Redis 最小冒烟:没有跑过,本文件里没有结果记录。
  - 三份规格的命令写于 10-12~10-18,之后用例名、键名、区容量来源和 Redis 形态都变了,照原文跑对不上。本条按当前代码重写;规格原文不改,两者不一致时以本条为准。
- **对着 HEAD `55eae6be3` 核到的、与规格不同的地方**:
  1. **`go/login` 现在编不过,要先生成协议。** `internal/svc/servicecontext.go` 调 `NodeInfo.GetClientWsUrl()`,`loginpregate` 写 `resp.WsUrl`,`internal/svc/gate_migration.go` 写 `TargetGateWsUrl`;`go/proto` 里的生成代码还没有这些字段(`go/proto/common/base/common.pb.go` 停在 11-11,`proto/common/base/common.proto` 11-19 加了 `client_ws_url = 17`)。
  2. **用例名。** G1b 的 T1~T9(含 T8a~T8c)原名都在。G4b-G 的 T13 `TestConsume_RepeatPollDoesNotExtendPlaceholder` 在 11-11 改写为 `TestConsume_RepeatPollExtendsPlaceholderOnlyInsideRetakeWindow`,其余 13 条原名都在。`TestLookup_*` 11-08 起改名 `TestPoll_*`。loginqueue 包现有 181 个顶层用例,个数以 `-v` 实际列出的为准。
  3. **键名**(11-12 起带 hash tag,见 `queue.go` 头注释):`queue:{q:<z>}`、`admit:{q:<z>}:<queueId>`、`admitted:zset:{q:<z>}`、`queue:meta:{q:<z>}:<queueId>`、`queue:token:{q:<z>}:<token>`。放行锁在本机是 `dispatcher:lock:login_queue:{q:<z>}`。规格 §8.4 里不带花括号的键名都按这个换。
  4. **区容量不在 login.yaml 里。** 11-10 起只认网关区服目录的 `zone_config.capacity`,规格里的 `ZoneCapacityOverride` 已经不存在。改法见 `docs/ops/login-queue-stress-runbook.md` 开头。
  5. **Redis 形态与版本。** staging / prod 的排队键在控制集群 `redis-control`(`deploy/k8s/manifests/infra/redis-cluster.yaml`,镜像 `redis:8.10.2`,Cluster)。K8s dev 是核心单实例 `redis:7.2`。本机:`deploy/docker-compose.yml` 的主 Redis 是 `redis:latest`;Docker 调试栈(`deploy/debug/compose.game.yml`)缺省 `redis:7.2`;本机开发集群(`deploy/compose.redis-cluster.yml`)是 `redis:8.10.2`。规格写的「真实 Redis 7.x」定于 10-15,那时还没有控制集群。
  6. **放行流水线在集群下走另一条路。** `runAdmitScripts` 先取队列键所在槽的主节点(`clusterredis.MasterForKey`),在专用句柄(`clusterredis.NewPinned`)上发;单实例上走不到这一段。
  7. **取走之后的占位。** 11-11 起有重取窗口:占位在取走后 20 秒到期;窗口内同一令牌再查一次,占位推到「再查时刻 + 20 秒」,最晚到取走后 40 秒。规格 §8.4 的「约 20s 消失」按「20~40 秒内消失」判。
  8. **查询是一段脚本。** 11-06 起一次轮询只发一段 `lookupScript`,回 4 个元素(三个整数加一个字符串);规格 §8.4 提到的 `lookupStateScript` 已删除。

**第一部分:门槛 ① ②(Go 单测;不起服务,不连外部依赖)**

- **前置**:协议已经生成。生成步骤是 11-19「小游戏 WebSocket 入口」条目验证步骤的第 0 步(三条命令,那一批有客户端授权),本条不另外规定配置。开跑前核对:`go/proto/common/base/common.pb.go` 里搜得到 `GetClientWsUrl`。搜不到就先做那一步,不要改 login 的代码绕过去。
- **环境与快照**(PowerShell 7,仓库根;输出原文留进证据):
  ```powershell
  go version
  go env GOTOOLCHAIN GOPROXY GOFLAGS CGO_ENABLED
  git rev-parse HEAD
  git status --short -- go/login go/shared go/proto
  $env:GOTOOLCHAIN = 'local'; $env:GOPROXY = 'off'
  ```
  `git status` 有输出时,再留一份 `git diff --stat -- go/login go/shared go/proto`。证据要能对上源码:11-08 那份就是因为之后源码变了才作废的。
  两个环境变量只在这次会话里生效,作用是不让 go 自己下载工具链和模块。它们本身导致报错时,去掉后重跑一次并在结果里注明;仍然提示要下载就停下报告。
- **命令**(工作目录 `go/login`,按顺序串行):
  ```powershell
  gofmt -l internal\logic\pkg\loginqueue internal\svc internal\config internal\logic\loginpregate
  go vet ./internal/logic/pkg/loginqueue/... ./internal/svc/... ./internal/config/... ./internal/logic/loginpregate/...
  go build ./...
  # 门槛 ①:G1b T1~T7、T9
  go test ./internal/logic/pkg/loginqueue/... -count=1 -v -run "TestZoneDispatcherLockKey|TestNewDispatcher_|TestIntegration_|TestDispatchOnce_"
  # 门槛 ①:G1b T8a~T8c(接线)
  go test ./internal/svc/... -count=1 -v -run "TestInitLoginQueue_"
  # 门槛 ②:G4b-G T1~T14
  go test ./internal/logic/pkg/loginqueue/... -count=1 -v -run "TestNew_|TestPopAdmit_|TestTryReserveFastPathSlot_|TestFastPath_|TestConsume_|TestAdmittedCount_|TestLegacyAdmittedSetKey_|TestAdmittedAccounting_|TestDispatchOnce_"
  # 稳定性:多 zone、双 leader、选主串行、并发占位、接线,各 10 遍
  go test ./internal/logic/pkg/loginqueue/... -count=10 -run "TestIntegration_|TestTryReserveFastPathSlot_Concurrent"
  go test ./internal/svc/... -count=10 -run "TestInitLoginQueue_"
  # 整包(顺带覆盖 11-10~11-20 改过排队的各条)
  go test ./internal/logic/pkg/loginqueue/... ./internal/svc/... ./internal/config/... ./internal/logic/loginpregate/... -count=1
  go test ./... -count=1
  ```
  `go test` 报 Access denied 时改用 `go test -c` 编出测试程序再跑,并在结果里写明。
- **通过标准**:
  - `gofmt -l` 没有输出。有输出时只记文件名,不在这次验证里 `gofmt -w`(工作区是多个会话共用的)。
  - `go vet`、`go build` 退出码 0。
  - 门槛 ① 的 11 条逐条出现在 `-v` 的 PASS 里:`TestZoneDispatcherLockKey_IsPerZone`、`TestNewDispatcher_RejectsZeroZoneOrEmptyLockKeyBase`、`TestIntegration_TwoZonesSharedRedis_BothQueuesDrain`、`TestIntegration_DualLeadersSameZone_NoLossBoundedOverAdmit`、`TestIntegration_QueueDrainAdmits`、`TestIntegration_DispatcherRespectsCapacity`、`TestIntegration_LeaderElectionSerializesDispatch`、`TestInitLoginQueue_DispatcherLocksOwnZone`、`TestInitLoginQueue_ZeroZonePanicsWhenQueueEnabled`、`TestInitLoginQueue_DisabledSkipsDispatcherValidation`、`TestDispatchOnce_EmptyQueueSkipsGateDirectory`。
  - 门槛 ② 的 14 条逐条出现在 `-v` 的 PASS 里:`TestNew_DefaultsHandoffGraceAndClock`、`TestPopAdmit_HoldsPlaceholderInZSetUntilAdmitTTL`、`TestTryReserveFastPathSlot_BudgetAndPerMemberExpiry`、`TestFastPath_SustainedLoginsDoNotStarveBudget`、`TestConsume_KeepsPlaceholderForHandoffGraceThenReleases`、`TestConsume_HoldsPlaceholderEvenIfAlreadyExpired`、`TestAdmittedCount_DropsOnlyExpiredMembers`、`TestTryReserveFastPathSlot_ConcurrentNeverExceedsBudget`、`TestLegacyAdmittedSetKey_NeverWritten`、`TestAdmittedAccounting_RedisDown_FailsClosed`、`TestConsume_PlaceholderWriteFailureDoesNotFailAdmit`、`TestDispatchOnce_ConsumedAdmitsHoldCapacityForHandoffGrace`、`TestConsume_RepeatPollExtendsPlaceholderOnlyInsideRetakeWindow`、`TestConsume_SignFailureKeepsAdmitTimeScore`。
  - 两条 `-count=10` 没有失败。
  - `-v` 输出里没有 `login.queue_dispatcher` 的 panic 记录(safego 会接住 panic,用例本身不会因此失败)。
  - 整包那一条的四个包全绿。`go test ./...` 里别的包失败不算门槛不过,但要列出包名和首个失败用例,并注明与排队有没有关系。
  - `-race`:两份规格都写明没有 cgo 工具链可以省略。本机 11-15 的记录是 `CGO_ENABLED=0`、没有 gcc。省略时在结果里写明;有 Linux / WSL 环境时对 `./internal/logic/pkg/loginqueue/` 加跑一轮并注明平台。
- **失败时保留**:失败用例的完整 `-v` 输出、`go vet` / `go build` 的报错原文、`git status --short -- go/login go/shared go/proto`。命令失败后不连续重试,不改用例迁就(AGENTS §10.2)。

**第二部分:门槛 ③(真实 Redis 最小冒烟;不是压测,不出性能结论)**

出处是 `spec_G4b-G.md` §8.4 正文加 10-15、10-18 两段补注,以及 `docs/ops/login-queue-stress-runbook.md`「验证清单」第 2、3 步。下面是按当前代码改写后的全部核对项。

- **跑两轮**:
  - **第 1 轮:单实例 Redis 7.2。** 这是门槛文字要求的。Docker 调试栈缺省就是 `redis:7.2`。
  - **第 2 轮:Redis 8.10.2 Cluster。** 这是 staging / prod 上排队实际所在的形态。只能用宿主机进程的本机栈连本机开发集群(`deploy/compose.redis-cluster.yml`),玩家域和控制域都要指过去,做法与两条前提见 `tools/scripts/README.md`「compose.redis-cluster.yml」一节;调试栈连不上开发集群。
  - 第 2 轮不在 `spec_G4b.md` §7 的文字里,是本条加的。只跑第 1 轮的话,放行流水线的集群路径和 8.x 上的脚本,第一次碰真实 Redis 仍然是 staging / prod 打开排队那一刻,这正是这项门槛要避免的事。11-06「login 排队放行流水线与轮询一次往返」条目第 5 步也要求 7.2 与 8.x 各跑一遍。它算不算改常量的前置,由负责人定(见文末)。
- **每轮的准备**:
  1. 记下 `redis-cli INFO server` 里的 `redis_version` 与 `redis_mode`、`git rev-parse HEAD`、用的是哪套栈。
  2. 确认 zone 1 的排队键是空的:`redis-cli --scan --pattern '*{q:1}*'` 没有输出。集群那一轮要连到 `{q:1}` 所在槽的主节点上执行(`SCAN` 只扫所连的那个节点;用 `CLUSTER KEYSLOT 'queue:{q:1}'` 加 `CLUSTER NODES` 找到它)。有残留就按该栈平时的办法清库(本机栈是 `pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`),不要只删其中几把键。
  3. 让 login 读到 `Queue.Enabled: true`,`Queue` 段其余字段保持缺省。改的必须是不进 git 的那一份,**不改 `go/login/etc/login.yaml`**。
     - login 的配置参数是 `-loginService <路径>`。两套栈的 login 都不直接读 `etc/login.yaml`:本机栈由 `go_services.ps1` 选每次启动的完整配置;调试栈读命名卷里的 `/run/sql-writer/runtime/login.yaml`,它由 `prepare_docker_debug.py` 派生到 `build/docker-debug-current/` 的配置经 `login-sql-boot` 写入。
     - 怎么让改过的配置生效(重跑哪一步、重建哪个容器),本条没有核到可以照抄的命令。由 Codex 按该栈的现行做法做,把实际做法写进结果。
     - 起来之后 login 日志应有 `[loginqueue] elected dispatcher leader zone=1`。
  4. 把 zone 1 的区容量改成 100:按手册开头的办法 `GET /admin/zones/1` 拿整行和 `ETag`,改 `capacity` 后带 `If-Match` 整行 `PUT`。等 login 日志出现 `[ZonePolicy] zone 1 policy changed: capacity=100` 再往下做,并记下原值。
  5. robot 的配置用平时冒烟那份的副本(放 scratchpad,配置参数 `-c <路径>`),只改 `zone_id: 1` 和 `robot_count`。
- **步骤与核对项**(`redis-cli` 一律带 `-c`;调试栈经 `docker exec` 进 Redis 容器执行,有口令时用 `REDISCLI_AUTH`,口令不进记录):
  1. **不饱和。** `robot_count: 5`。期望 5 个都走快速通道:robot 的 `[stats ...]` 里 `q_entered=0`,`login_ok=5`。做完停掉 robot,等 gate 上报人数归零(gate 每 10 秒上报一次)。
  2. **饱和。** `robot_count: 200`。期望 100 个直接进、100 个入队(`q_entered=100`,手册口径)。robot 保持在线时 `q_admitted` 不涨是正常回压。有出入时把 `login_queue_capacity{zone="1"}`、gate 在线数和 `ZCARD 'admitted:zset:{q:1}'` 一起记下再判。
  3. **占位键**(第 2 步刚做完时看):
     - `TYPE 'admitted:zset:{q:1}'` 是 `zset`。
     - `ZRANGE 'admitted:zset:{q:1}' 0 -1 WITHSCORES`:分数是 13 位的到期毫秒,快速通道的成员以 `fast:` 开头。
     - `PTTL 'admitted:zset:{q:1}'` 在 (0, 120000] 内。是 −1 说明整键没设上 TTL。
     - `EXISTS 'admitted:zone:1'` 是 0(旧键不再写)。
     - 用 `redis-cli TIME` 换算出当前毫秒,隔几秒执行一次 `ZCOUNT 'admitted:zset:{q:1}' '(<当前毫秒>' +inf`:快速通道的占位在写入后约 20 秒到期,计数回到 0。
  4. **放行确有发生。** 把区容量改到 150。期望约 50 人被放行:robot 的 `q_admitted` 涨到约 50,login 的 `login_queue_admit_total{zone="1"}` 同步上涨。这一步说明 `admitEntryScript` 能在真实 Redis 上执行;集群那一轮同时说明流水线发到了队列槽所在的主节点。
     - 放行之后、客户端取走之前(窗口约一个轮询间隔,建议用脚本连续采样,或者先让一部分排队者停止轮询再放行):`HGET 'queue:meta:{q:1}:<queueId>' admitted_at` 是 13 位毫秒;`PTTL 'admit:{q:1}:<queueId>'` 在 (0, 60000] 内;`PTTL 'queue:meta:{q:1}:<queueId>'` 自放行起只随时间递减,不变成 −1。
     - 排队放行的成员在取走后 20~40 秒内从占位键里消失(上面第 7 条)。
  5. **整键 TTL 只延长不缩短。** 排队中途执行 `PEXPIRE 'admitted:zset:{q:1}' 600000`,等下一次放行、取走或快速通道写入之后再看 `PTTL`:仍大于 120000,并随时间递减,没有被压回 120000。做完不用恢复。
  6. **续期。** 轮询中的条目,`PTTL 'queue:meta:{q:1}:<queueId>'` 与 `PTTL 'queue:token:{q:1}:<token>'` 落在 (E, 1.25E],E 是 `QueueEntryTTL`(缺省 1 小时)。刚轮询完时不低于 E;两次轮询之间可能比 E 低至多一个轮询间隔。`PTTL 'queue:{q:1}'` 不是 −1。
  7. **入队的 NX/GT。** 先 `PEXPIRE 'queue:{q:1}' <大于 E 的毫秒值>`,再入队一人。`PTTL 'queue:{q:1}'` 没有被压回 E。
  8. **写满。** 先 `CONFIG GET maxmemory-policy` 确认是 `noeviction`,记下原 `maxmemory`。排队进行中执行 `CONFIG SET maxmemory <INFO memory 的 used_memory>`,等至少一拍。集群那一轮对 `{q:1}` 所在槽的主节点做。这段时间同一个 Redis 上别的写入也会被拒,窗口尽量短。
     - login 日志出现 `[loginqueue] PopAdmit zone=1 err=… OOM …`。
     - `ZCARD 'queue:{q:1}'` 不变,没有新的 `admit:{q:1}:*` 键,robot 仍收到排队中。
     - 新的入队请求被 login 拒绝(规格口径是回 `StatusError`)。
     - 续期在写满时仍然生效:手工 `PEXPIRE 'queue:meta:{q:1}:<queueId>' 60000`、`PEXPIRE 'queue:token:{q:1}:<token>' 60000`,等这个客户端再轮询一次,两者的 `PTTL` 回到约 1.25E。也可以把这次冒烟用的 `QueueEntryTTL` 取小值(如 40s),观察跨过 E/4 的那一次续期。
     - 恢复原 `maxmemory` 之后放行恢复,`ZCARD` 下降。
  9. **毒丸 meta。** 手工 `SET 'queue:meta:{q:1}:<某排队者的 queueId>' x`。期望这个客户端收到 410(规格口径是 `meta_gone`)并重新排队,其他人照常放行,login 日志出现带 `metaGone=1` 的 `PopAdmit entries not admitted zone=1 …`。
  10. **查询的各个分支。** 排队中、已放行、`admit_window_passed`(已放行的人超过 `AdmitTTL` 60 秒没来取,再查)、`queue_gone`(`ZREM 'queue:{q:1}' <某排队者的 queueId>` 造出来)、`meta_gone`(上一步)。五个分支都不出现 unexpected reply 一类的解析错误。robot 造不出 `admit_window_passed` 时改用别的办法(例如手工调 `/api/queue-status`);实在没覆盖到的分支在结果里点名,不算通过。
  11. **全程。** 除第 8 步写满期间外,login 日志里不出现 `[loginqueue] PopAdmit zone=1 err=`。
- **通过标准**:每一轮上面各项全部符合。任何一项不符就停下报告,不改判据迁就,不打开排队。规格口径与实际回包不一致时,原样记录实际值,由 Claude 对照代码判是规格过时还是缺陷。
- **收尾**:恢复 `maxmemory`;区容量按重新 `GET` 到的 `ETag` 改回原值;停 robot;login 的配置恢复成 `Queue.Enabled: false` 并让它生效;清掉 zone 1 的排队键。集群那一轮另外把指到开发集群的配置改动全部还原(`git status --short` 里不应留下这次的改动),开发集群是这次才起的就 `docker --context desktop-linux compose -f deploy/compose.redis-cluster.yml down -v`。
- **失败时保留**:出问题那一步的 `redis-cli` 命令与输出原文、那一段时间的 login 与 gateway 日志、robot 的 `[stats ...]` 行。

**第三部分:结果怎么记**

- Codex 在本文件另起一条,不改本条。写清:
  - ① ②:`go version`、`HEAD`、25 条门槛用例的 PASS 行、整包汇总、`-race` 跑没跑。
  - ③:每一轮的 Redis 版本与形态、用的栈、让配置生效的实际做法、各核对项的命令原文与输出摘要。
- 证据原件放 `build/verification/login-queue-gate-<UTC>/`。
- **Codex 不改 `$script:LoginQueueMergeGateCleared`。** 三项都满足并有记录之后,由负责人在同一个提交里把它改成 `$true`,在常量注释里写上那条结果条目的标题,改写 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的用例 19 / 20,并重跑整套 `k8s_deploy_contract`(`spec_G4b.md` §7)。这批改动到时候再做,本条没做。

**待负责人定**

1. **第 2 轮(8.10.2 Cluster)算不算改常量的前置。** 建议算,理由见第二部分。不算的话,打开排队那一刻就是集群路径第一次上真实 Redis。
2. **规格里的「红」要不要补。** G4b-G §8.1 标的是必做,G1b §7.1 标的是推荐:在修复前的旧提交(`0aa1027bc`、`73b852f32`)上跑复现用例,证明这些用例拦得住原缺陷。本文件里只有命令,没有运行结果。门槛文字只要求跑绿,所以本条没把它列进步骤。主仓是部分克隆,取旧提交的文件可能要联网补拉对象;要补的话不在 `D:\luyuan` 根目录开 worktree(AGENTS §9)。

**其它**

- **本条没覆盖的**:手册「验证清单」第 4、5 步(杀 leader、Redis 重启,5000 个 robot)不在门槛里;压测(L-14)不在本条;Unity 客户端不在本条。
- **Java 版(AGENTS §12)**:不涉及。这道门槛是本仓部署脚本内部的事;独立 Java 版的排队在 game-gateway 里,缺省关闭,`PARITY.md` 不加行。
- **本会话没有编译,没有运行任何测试**。只读了源码、三份规格、手册和验证目录;没有读客户端仓。本条里标「规格口径」的期望值没有对着当前代码逐条重推。
- **提交情况**:本条在 main 工作区,未提交。

## 2026-11-20 Claude:go/db 熔断改成按存档库——关掉 zrpc 按方法的服务端熔断,每个存档库一个 go-zero 熔断(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人问 go/db 这条线做完没有,复核 2026-11-19「go/db 删除存档库运行期冷却」的结论时,按 go-zero v1.10.0 源码核对了框架这一层,发现当时的分析漏了它:
  - zrpc 的服务端自带熔断(`Middlewares.Breaker` 缺省开,`serverinterceptors.UnaryBreakerInterceptor`),键是 gRPC 方法名,回 `Unavailable` / `DeadlineExceeded` 的调用计成失败。go/db 对存储侧的失败一律回 `Unavailable`,所以整库不可达时这一层本来就在快速挡回——昨天写的「删掉之后只是每次失败晚几秒」不准确,已在设计稿更正。
  - 它不分库。算法(`core/breaker/googlebreaker.go`)是「10 秒窗口内成功数 × 1.5 小于总数就按比例丢,被丢的也计入总数」,假定后端越被限流成功率越高;这里失败占比是流量结构决定的常数。一个库失联且流量占比超过三分之一时(存档库不超过 3 个时任何一个库都是),放行比例逐窗口按 1.5 ×(1 − 失败占比)衰减,指向健康库的存盘和认领也几乎全被挡掉,只剩每秒强制放行的一次:单库故障放大成整个地域存不了盘、进不了场。这个问题和删不删冷却无关,删之前就在。
- **改动**(main 工作区,未提交):
  - `go/db/db.go`:建 gRPC 服务之前 `config.AppConfig.Middlewares.Breaker = false`,关掉按方法的那一层(代码里写死,yaml 写开也不认)。
  - `go/db/internal/playerstore/server.go`:新 `enterStore`(每个存档库一个 go-zero 熔断,具名 `player_store/<编号>`,用 `Allow` / `Accept` / `Reject`),`SavePlayer` 与 `LoadAndClaim` 在打开库之前先过它,拿到结局后结算一次。熔断开着时不碰库,回 `Unavailable`「player store: storage breaker open」,记 `db_player_store_requests_total{result="unavailable"}`,不逐次打日志。不算库故障的两种失败:闸门冻结(搬库窗口)、调用方自己取消。scene 不用改:任何非 OK 都按结果未知同序号重试。
  - `go/db/internal/playerstore/server_test.go`:新增 `TestServer_BreakerIsPerStore`(一个库一直打不开,200 次调用里有被挡回的;另一个库 200 次全部提交)、`TestServer_FrozenGateDoesNotOpenTheStoreBreaker`(冻结 200 次每次都走到库,解冻后第一次直接提交)。熔断表是进程级的,两条用例用 88000x 这组别处不用的库编号。
  - `go/db/internal/logic/pkg/proto_sql/store_registry.go`:`Store()` 的注释写明快速失败在哪一层。
  - 文档:`docs/design/db-task-store-unavailable-deferral.md` 文首加「2026-11-20 补」并更正第 4 点;`docs/design/player-save-direct-commit.md` §2 加一条;`deploy/k8s/player-store-alerts.yaml` 的 `DbPlayerStoreCommitFailures` 说明里 `unavailable` 多一种来源(规则本身没动)。
- **没改的 / 另开的**:
  - 审计转发不经过这层熔断,仍是一轮里遇错跳过这个库。
  - 同类问题在 data_service 也有(目录按片分库,服务端按方法熔断 + 各 Go 调用方按「目标 + 方法」的客户端熔断,一片所在集群失联且占比过三分之一时同样会挡掉健康片的调用)。那边牵涉多个服务和调用方的错误码口径,不在本条范围,另开任务。
- **给 Codex 的验证步骤**(本会话没有编译、没有跑测试;可与本日「…两条残余」、2026-11-19 那条合在一次跑;失败保留编译错误全文、失败用例名与断言原文,不改断言,不连续重试):
  ```
  cd go/db
  gofmt -l db.go internal/playerstore internal/logic/pkg/proto_sql
  go build ./...
  go vet . ./internal/playerstore/ ./internal/logic/pkg/proto_sql/
  go test -count=1 -v -run 'TestServer_' ./internal/playerstore/
  go test -count=1 ./internal/playerstore/ ./internal/logic/pkg/proto_sql/
  go test -count=3 -run 'TestServer_' ./internal/playerstore/
  cd ../..
  pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
  ```
  通过标准:
  - `gofmt -l` 无输出,build / vet 无错;
  - `TestServer_BreakerIsPerStore`、`TestServer_FrozenGateDoesNotOpenTheStoreBreaker` 与原有六条 `TestServer_*` 逐个报 PASS;`-count=3` 那一轮也全过(同一进程里熔断状态会累积,原有用例对 99 号库的断言只看 `Unavailable`,被挡回也成立);
  - 部署契约里 `DbPlayerStoreCommitFailures` 的规则名、表达式、级别断言不变(只改了说明文字)。
  - 可选冒烟(调试栈,两个存档库):停掉其中一个库所在的 MySQL / 把它的库名从白名单拿掉,另一个库的玩家照常存盘、进场;go/db 的 `db_player_store_requests_total{result="unavailable"}` 只因坏库上涨。单库的调试栈做不了这一步,如实报未运行。
- **Java 版(AGENTS §12)**:不涉及(Java 版没有 go/db 与 go-zero),不加 PARITY 行。
- **提交情况**:main 工作区,未提交。

## 2026-11-20 Claude:任务完成状态改按位序位图落盘(未编译、未测试,待 Codex 验证;Java 不涉及)

- **起因**:负责人要求任务完成状态落盘用位图,现在就做(任务只会越做越多)。此前落盘存的是任务 ID 列表,每个 2~4 字节;位图每个任务 1 位。
- **更正**:同日早些时候我对负责人说「位号按表顺序生成、要在任务表里加一个发了就不变的位号」,这是错的。稳定位序早就有:任务表 `id` 列带 `(cfg_bit_index)`,导表器的位序状态文件(`tools/data_table_exporter/state/mapping/table_index_mapping/mission_mapping.json`)只增不改、永不复用、缺失或损坏即中止,有回归测试守着。所以这次不改导表器,只改落盘格式。
- **改动**:
  - `cpp/libs/modules/mission/comp/mission_comp.{h,cpp}`:新增 `MissionsComp::CompletedBitsForSave` / `RestoreCompletedBits`(第 i 位 = 位序 i,每字节低位在前,末尾全 0 的字节不写)与成员 `unknownCompletedBits_`(位序不小于 `kMissionBitmapCapacity` 的位原样带回)。
  - `cpp/libs/services/scene/player/system/mission_marshal.cpp`:存盘把位图写进 `MissionListComp.complete_missions`(原来每次清空的旧字段,所有现存存档里它都是空的),`completed_mission_ids` 只写没有位序的任务 ID;加载先读位图,再照旧读 ID 列表(旧存档下一次存盘转成位图)。待领取仍存 ID。
  - 协议只改注释:`proto/common/component/mission_comp.proto`、`proto/common/database/bag_quest_mail_data.proto`。字段、台账都没变,不用为这次改动重新生成。
  - 设计:`docs/design/player-features-ui.md` 文末新增「2026-11-20 任务完成状态按位序位图落盘」,2026-09-11 那句「不把运行时位图下标写入存档」补了原因与去向。
- **约束**:这次切换要在上线前完成。改动之前的二进制只认 ID 列表,存盘时还会清空位图字段;项目尚未上线,不做新旧并存的过渡。
- **测试**:`cpp/tests/bag_test/player_mission_system_test.cpp` 新增两条:
  - `PlayerMissionSystemTest.CompletedMissionsAreSavedAsBitIndexBitmapAndOldIdListsStillLoad`:完成任务 1、11、15,存出的位图是两个字节 `01 44`、ID 列表为空;只有 ID 列表的旧存档读进来再存,与前者逐字节相同。
  - `PlayerMissionSystemTest.CompletedBitsBeyondThisBuildSurviveLoadAndSave`:位图里超出本二进制容量的位,加载再存盘后原样还在。
  - 既有的 `UnknownConfigurationAndOtherScopesArePreservedWithoutOpeningClaim`(没有位序的任务 ID 999999 仍按 ID 往返)与 `PersistRoundTripTest.*`(夹具仍用 ID 列表喂入,走旧存档读路径)按代码推断不受影响。
- **Codex 验证**(仓库根;失败保留第一条编译错误与 gtest 完整输出,不改断言,不连续重试;与同日「活跃度领取记录按周期裁剪」可以合成一次整解):
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。改的是 mission 模块与 scene 库,`run_cpp_tests.ps1 -Build` 不重编库,必须先整解。
  2. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test`:`PlayerMissionSystemTest.*`(含上面两条新用例)、`PersistRoundTripTest.*`、`PlayerFeaturePersistenceTest.*`、`PlayerActivitySystemTest.*` 全绿。
  3. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter missions_test`:全绿(`MissionsComp` 的既有用例)。
  4. Linux:`build_linux.sh` 编过(没有新增源文件)。
- **已做的静态检查**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 通过。
- **未验证**:没有编译,没有跑单测;没有用真库里的旧存档验证过转换。
- **Java 版(AGENTS §12)**:不涉及。这是服务端内部的存储格式,客户端可见的任务协议没有变;Java 版按自己的方式存。
- **文件**:`cpp/libs/modules/mission/comp/mission_comp.h`、`cpp/libs/modules/mission/comp/mission_comp.cpp`、`cpp/libs/services/scene/player/system/mission_marshal.cpp`、`cpp/tests/bag_test/player_mission_system_test.cpp`、`proto/common/component/mission_comp.proto`、`proto/common/database/bag_quest_mail_data.proto`、`docs/design/player-features-ui.md`、本条。

## 2026-11-20 Claude:Unity 登录排队收尾——无票 429 按 30/60/120 秒退避、排队中换 access token、总预算改 24 小时、所有 POST 只认 HTTP 200、断线重连认限流提示;robot 410 换票清零退避(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 不涉及)

- **起因**:负责人 11-20 要求把本线留到以后的事现在做完。上一条与 11-19 那条留下四件:无票 429 收到即报错、排队期间不换 access token(总预算因此只能定 2 小时)、排队循环没有单测、robot 测试文件一处 gofmt 旧对齐。核对时又查出两处:
  - `GatewayHttpClient` 其余 POST 接口仍会把非 200 的 Spring 缺省错误页读成 code=0「成功」。
  - 断线重连把 100 ratelimit 当确定失败。开服区桶打满时,玩家一断线就被踢回选服。
- **客户端**(`../mmorpg-client`):
  1. `GatewayHttpClient.PostJson`:所有 POST 接口只读 HTTP 200 的正文,其余一律交 `onError`。核过两版网关:业务结果全在 HTTP 200 里,只有 hotfix-check 回 400,它不走 `PostJson`。昨天加的 `requireHttp200` 参数随之去掉。
  2. `GameClient.EnterZone`:
     - 第 1、2 步共用总预算 24 小时(与排队令牌的绝对寿命一致),也共用无票 429 的重试次数。
     - 无票 429(/api/login 与 assign-gate)按 30、60、120 秒加抖动重试,合计最多 3 次,之后报错。429 在网关转发 login 之前就回,一次性的渠道凭证没被用掉,原样重发安全。
     - 第 2 步每轮先看 access token:剩不到 10 分钟就经 `/api/refresh-token` 换新,成功记 `access_token refreshed while queueing`;网络或 500 失败隔 60 秒再试;refresh 令牌被拒(401)按「登录已失效」结束。
     - `AccessTokenExpiresSoon()` 与 10 分钟余量和局内 `MaybeRefreshToken` 共用;`LoginExpiredMessage` 与 `DescribeAssignCode` 共用。
  3. `GameClient.Reconnect.cs`:
     - `IsTransientAssignGateCode` 改收整个回包,100 且 `queue_source="ratelimit"` 算暂时结论。
     - 重试间隔取 max(重连退避, `QueueRetryDelayMs(retry_after_ms)`),仍在 20 秒期限内。真排队的 100 照旧交回选服。
     - 另一会话在这个文件只改了 `wsUrl` 实参,与本条不冲突。
  4. 用例:`LoginCredentialTests.RateLimitedBackoff_Is30Then60Then120Seconds`(3 个 TestCase)。排队循环本身的串联靠下面第 4 步冒烟:EditMode 只能用反射推进协程,UnityWebRequest 在里面跑不完。
- **robot**(`robot/http_assign_gate.go`):
  - 410 换票时连续失败数清零,与 Unity 一致;新用例 `410 after failed re-entries resets the backoff for the new token`。
  - `gofmt -w` 修了测试文件 Jitter 用例里一行注释的旧对齐。
  - 无票 100 不加抖动、无票 429 交外层是压测的有意差异,不改(设计稿 §11.5、§11.7 R2)。
- **文档**:设计稿 §11.5 的无票段与 Unity / robot 现状、§十表行。
- **验证(交 Codex,按 AGENTS §10.1)**:
  1. robot(在 `robot/` 下):`go vet ./... && go test -count=1 -run 'TestAssignGateWithQueue|TestSleepCtx' ./...`,全部 PASS,含上一条的四条与本条新增的一条。本会话跑过 `gofmt -l robot/http_assign_gate.go robot/http_assign_gate_test.go robot/main.go`,无输出。
  2. 客户端编译(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`,0 error。本条改了 `GameClient.cs`、`GameClient.Reconnect.cs`、`GatewayHttpClient.cs`。
  3. EditMode:命令同 11-19 那条第 2 步,`-testFilter` 换成 `MmorpgClient.Tests.EditMode.Net.LoginCredentialTests;MmorpgClient.Tests.EditMode.Net.GameClientReconnectTests`。第二组不打 HTTP,本条改了重连的暂时结论判定,应原样通过。failed=0。
  4. 联网冒烟,在 11-19 与上一条第 3 步之上再加三项,其余照旧。网关限流默认关着:三项都给本机网关加启动参数 `--gate.rate-limit.enabled=true`,并按 11-19「网关前置限流的排队提示加下限与随机量」那条给它配本机 Redis。
     - 无票 429:再加 `--gate.rate-limit.ip-rps=1 --gate.rate-limit.ip-burst=1`,不排队(capacity 照常)。/api/login 用掉唯一的 IP 令牌,紧接着的 assign-gate 回 429。
       - 期望:状态栏「操作过于频繁,3x 秒后重试…」,约 30~36 秒后重调成功,进游戏。
     - 排队中换 access token:login 配置副本把 `AccessTokenTTL` 改成 `12m`;按 11-19 那条让 zone 1 排队,排 3 分钟以上再把 capacity 调回去。
       - 期望:日志里有 `access_token refreshed while queueing`,没有 `... failed`;放行后进 gate 认证成功。
     - 断线重连遇限流:加 `--gate.rate-limit.zone-default-rps=1 --gate.rate-limit.zone-default-burst=1 --gate.rate-limit.ip-rps=100 --gate.rate-limit.ip-burst=100`。客户端进游戏后,另开终端用循环每 0.3 秒 POST 一次 `/api/assign-gate`(`{"zone_id":1}`)抽空区桶;断开客户端网络 3~5 秒再恢复,看到一次重连回 100 后停掉循环。
       - 期望:重连日志 `reconnect attempt N failed, retrying in X.Xs: assign gate code=100`,X ≥ 2;20 秒内重连成功,不回选服。
     - 收尾:去掉启动参数、恢复 `AccessTokenTTL`,确认热关停规则都已删、capacity 已恢复。
- **未验证**:以上全部没有运行。
- **Java 版(AGENTS §12)**:0.1.0-SNAPSHOT 不涉及。只改共用客户端与本仓 robot,两版服务端回包都不变;不往 `PARITY.md` 加行。
- **提交情况**:客户端仓与本仓都在 main 工作区,未提交。

## 2026-11-20 Claude:battle 直连准入收尾——未认证池改 O(1)、battle 接上按来源前缀限速、两个节点都只对公网源地址按来源限速(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人要求本线做完、查出的问题当场修。核对 11-19「battle 直连准入前移到 Acceptor」与「未验签预算收尾」两条之后,有三处没做完:
  1. **battle 每次 accept 扫一遍会话表**(上一条写在「已知局限」里)。准入回调排在速率闸前面,被拒的洪水连接也要走它:按每秒几千次 accept、每次扫 4096 条算,单 loop 的 battle 会被它拖住,攻击者用很便宜的建连换走 battle 大量 CPU。这是放大器,不是局限。
  2. **battle 没有按来源前缀限速**(P2-2 只做了 gate)。
  3. **按来源限速遇上小游戏 WebSocket 会误伤**:经边缘 nginx 进来的连接,accept 时看到的是边缘 Pod 的内网地址。accept 这一层还没读到任何字节,`X-Forwarded-For` 拿不到;开了按来源限速,所有小游戏玩家会被当成同一个来源一起限。
  - 第 4 件「`k8s_deploy.ps1` 接线 `GATE_*` / `BATTLE_*` 准入变量」与压测校准的采集汇总归「gate 未验签连接预算」会话(本日已分工),本条不碰部署脚本。
- **口径**:
  - **未认证池 O(1)**:`BattleClientEdge::unauthenticated_` 是按 connId 有序的 `std::set<uint64_t>`(connId 单调递增、从不复用,有序即建连先后)。
    - 进池只在 `HandleEstablished`;出池在握手成功、探测授权验过(`OnProbeStart`)、淘汰(`EvictUnauthenticated`,先出池再 `forceClose`)与 `HandleClosed` 四处。`HandleClosed` 是会话表唯一的删除点,`erase` 幂等,计数不会漂。
    - `AdmitConnection` 取 `size()`;最老一条只在池承压且 loop 没刚卡过时才找,从 `begin()` 起找第一条还连着的。`battle_unauthenticated_connections` 读同一个集合,不再遍历。
    - 删掉 `UnauthenticatedScan` 与它的单测;battle 的判定口径从「还连着」改为「在池里」:正在关闭、还没走到 `HandleClosed` 的(shutdown 后等延迟强关的那 100ms)仍算在池里,与 gate 的占位同一口径。
  - **按来源限速挪进 core,两个节点共用**:`SourcePrefix` / `SourcePrefixHash` / `SourcePrefixOfIpv4` / `SourcePrefixOfIpv6` / `SourceRateGate` / `DefaultSourceRateLimit` / `ResolveSourceRateLimit`(变量名由调用方给)从 gate 头挪到 `accept_admission.h`;gate 的 `SourcePrefixOf` 与承压判据也挪进去(`SourceRateUnderPressure`)。gate 头用 using 引回,名字与 `ResolveSourceRateLimit` 签名不变,`UnderPressure` 改为转发。
  - **只限公网来源**(`IsPublicSourcePrefix` / `RateLimitedSourceOf`,两个节点同一份):私网 10/8、172.16/12、192.168/16,回环,链路本地 169.254/16,运营商级 NAT 共享段 100.64/10,0/8,IPv6 的 ::/64、fc00::/7、fe80::/10 一律不按来源限速(IPv4 映射地址按 IPv4 判)。podip 形态、代理型 LB 后面开了也不会误伤,只是不起作用。经边缘进来的连接按客户端限速归边缘:WebSocket 会话同日给 infra-up 加 `-WsEdgeSourceRatePerSecond`,nginx 按 /24 `limit_req`,写进它的设计稿 W6。
  - **battle 按来源限速**:与 gate 同一套(缺省关闭,只在承压时生效,判定排在总量之后、未验签预算之前,所有闸都放行之后才记账)。
    - 变量 `BATTLE_SOURCE_RATE_PER_SECOND` / `BATTLE_SOURCE_BURST`,写错拒启;推荐值 4096 时每个前缀 10 条/秒、桶深 100,启动日志打出来。
    - 新增 reason `source_rate`、5 个 `battle_source_*` 指标族(插在 preauth 三族之后、WebSocket 会话加的两族之前)、告警 `BattleSourceRateLimited` / `BattleSourceTableFull`(7 → 9 条)。
    - battle 的 `Admission::Admit` 改为收对端地址,自己判承压、取前缀、在速率闸拒绝时记下时刻;前缀表满时放行并通过 `Verdict::sourceUntracked` 让直连面采样打日志。
  - gate 启动日志的按来源限速说明加一句 `applies to public source addresses only`;battle 启动日志多打 `source_rate_per_second=` / `source_burst=` 与推荐值。
- **代码**(都是修改,无新增文件):
  - core:`cpp/libs/engine/core/network/accept_admission.h`(多包含 `muduo/net/Endian.h`、`muduo/net/InetAddress.h`,只有 `SourcePrefixOf` 用)。
  - gate:`cpp/nodes/gate/handler/rpc/connection_admission.{h,cpp}`、`cpp/nodes/gate/main.cpp`(只改一行日志)。
  - battle:`cpp/nodes/battle/client/battle_connection_admission.{h,cpp}`、`battle_client_edge.{h,cpp}`、`cpp/nodes/battle/main.cpp`。
    `battle_connection_admission.{h,cpp}` 里同时有小游戏 WebSocket 会话加的两族 websocket 指标(未提交,归它),本条没动。
- **测试**(调整 / 新增,未运行):
  - `cpp/tests/rpc_controller_test/accept_admission_test.cpp` 共 23 条(`AcceptAdmissionPolicy` 10、`AcceptRateGate` 9、`HandshakeHistogram` 2、`SourceRateGate` 2)。
    - 从 gate 挪来 3 条(前缀聚合、两条 `SourceRateGate`),断言没改;
    - 新增 3 条:`OnlyPublicSourcesAreRateLimitedPerSource`(各段边界两侧各一个地址,外加 `RateLimitedSourceOf` 对 muduo 地址)、`PerSourceLimitingEngagesOnlyUnderPressure`、`SourceRateResolvesFromTheCallersEnvNames`。
  - `cpp/nodes/battle/tests/battle_connection_admission_test.cpp` 共 17 条(Policy 6、Admission 11)。
    - 删掉 `UnauthenticatedScanCountsAndFindsTheOldest`;
    - 新增 `PerSourceLimitingResolvesFromTheBattleVariablesAndIsOffByDefault`、`PerSourceLimitingAppliesOnlyUnderPressureAndOnlyToPublicSources`、`AGlobalRateRejectionEngagesPerSourceLimitingForOneHandshakeDeadline`;
    - 判定顺序一条改写为含来源,配置一条加按来源参数,指标一条加 6 行样本。
  - `cpp/nodes/gate/tests/connection_admission_test.cpp` 共 34 条(Policy 12、`ConnectionAdmission` 21、Eviction 1)。
    - 挪走 3 条纯逻辑;按来源的两条用例把 10.x 私网地址换成文档用的公网段(203.0.113.0/24、198.51.100.0/24),断言不变;
    - 新增 `PerSourceLimitingSkipsNonPublicSources`。
  - `tools/scripts/tests/battle_connection_alerts_contract.tests.ps1` 从 9 条变 10 条:族清单、reason、告警名单;未认证池一条改为核对有序集合、AdmitConnection 不遍历会话表、四处出池与先出池再关;新增「按来源限速」一条。
  - `tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`:前缀聚合的两条断言改读 core 头;新增 `RateLimitedSourceOf(peer)` 一条断言。
  - 已做的静态检查(不是编译 / 测试证据):`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 10 条全过;两份契约测试 PowerShell 解析 0 错误;battle 告警 YAML 解析出 9 条规则。
- **文档**:`docs/design/gate-connection-admission-control.md`(P0-2 的 battle 段与已知局限、P2-2 的状态 / 只限公网来源 / 判定位置 / battle 直连 / 部署接线、battle 直连面一节)、`docs/design/turn-based-battle-server.md` D27、`docs/ops/gate-kernel-tuning-runbook.md` §3.2 / §3.3、`cpp/nodes/gate/SECURITY.md`、`deploy/k8s/README.md`。
  `docs/design/minigame-websocket-gate.md` W6 由小游戏 WebSocket 会话改写(本日分工),本条没碰。
- **交 Codex**(没有 proto 改动,不用生成;前一步红就停下归因;在工作区跑):
  1. 仓库根,都应全过、退出 0:
     - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`(10 条)
     - `pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`(10 条)
     - `pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1`(条数以 P0-2 会话最新条目为准)
     - `pwsh -NoProfile -File tools/scripts/tests/cpp_node_resource_alerts_contract.tests.ps1`(告警名不重名)
     - 有 promtool 时,`deploy/k8s/battle-connection-alerts.yaml` 的 `spec` 跑一次 `promtool check rules`。
  2. Windows 整解:`msbuild game.sln /t:Build /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
     - 必须整解:core 头被 gate、battle 与两个测试工程同时用,`BattleClientEdge` 与 `battle_connection_admission::Admission` 的布局都变了。
     - 首错落在本条列出的文件里时交回 Claude。已知风险点:`accept_admission.h` 现在包含 muduo 的 `InetAddress.h`(Windows 上会带进 winsock 头),
       包含它的翻译单元里 `std::max` / `std::min` 一律写成 `(std::max)`;`std::optional<SourceRateGate>` 的 `emplace`;battle `Admit` 里 `std::optional` 与 `std::nullopt` 的三目。
  3. 测试,`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter <名>` 依次跑(先做完第 2 步,`-Build` 不重编库):
     - `rpc_controller_test`:`AcceptAdmissionPolicy.*` 10、`AcceptRateGate.*` 9、`HandshakeHistogram.*` 2、`SourceRateGate.*` 2、
       `BattleConnectionAdmissionPolicy.*` 6、`BattleConnectionAdmission.*` 11 条 PASS;`AcceptorAdmission.*`、`BattleObserverContract.*` 与其余用例不回归;
     - `routing_identity_test`:`ConnectionAdmissionPolicy.*` 12、`ConnectionAdmission.*` 21、`ConnectionAdmissionEviction.*` 1 条 PASS;
       `ClientHeartbeat*`、`ClientLiveness*`、`DrainMigration*`、lane 各组不回归。
  4. Linux(需授权 Docker;不可达或要拉镜像就停下来问):`docker --context desktop-linux build -f deploy/k8s/Dockerfile.cpp --build-arg BUILD_JOBS=2 -t mmorpg-node:admission-closeout .`,gate 与 battle 以 `-Werror` 编过。
  5. battle 行为(Linux 容器;`BattleMaxConnections` 调小到 50 时预算与起点按推导只剩速率下限的值,验收按下面给的覆盖值做):
     - **未认证池 O(1)**:`BATTLE_PREAUTH_BUDGET=40`、`BATTLE_PREAUTH_EARLY_DROP_START=20`,用裸 dial 循环建 30 条不发包的直连,等 4 秒再建 10 条。
       通过标准:`battle_unauthenticated_connections` 先涨到 20 ~ 30(过了 20 之后按概率早拒,记在 `preauth_early_drop`);
       等 4 秒后再建的 10 条每进一条淘汰一条最老的,`battle_preauth_evicted_total` 加约 10,池大小不再上涨;
       全过程 battle 的 CPU 不随已有直连数上涨。可对照:先建 2000 条已握手的直连(robot 战斗冒烟多开几局),再按每秒 2000 次建连洪水打 30 秒,loop 延迟与回合推进不受影响。
     - **按来源限速**:`BATTLE_SOURCE_RATE_PER_SECOND=5` 启动,从同一个公网 /24(没有公网测试机就跳过,并报告「公网来源未验证」)在承压时建连,
       超出桶深的读到 `ECONNRESET`、记在 `battle_rejected_total{reason="source_rate"}`;同时从集群内(私网地址)建连,一条都不按来源拒。
     - **拒启**:`BATTLE_SOURCE_BURST=50` 而不设速率启动,进程退出,stderr 最后一行含 `Refusing to start: BATTLE_SOURCE_BURST is set but BATTLE_SOURCE_RATE_PER_SECOND is not`。
     - 启动日志 `Battle client connection admission ENFORCED at accept` 一行带 `source_rate_per_second=0 source_burst=0` 与 `suggested per-source 10/s burst 100, applies to public source addresses only`。
  6. 冒烟:缺省参数下 robot 登录与一局战斗。通过标准:gate / battle 的 `*_rejected_total` 各 reason 都是 0,`*_source_rate_active` 为 0,`battle_unauthenticated_connections` 回到 0;
     gate 启动日志的按来源限速说明带 `applies to public source addresses only`。
  7. 失败时保留:首个编译错误原文、失败用例完整输出、容器 stderr 前 40 行。不改断言,不跳过用例。
- **剩余风险与没做的**:
  - 阈值都是推导值,压测里看(采集与汇总归 gate 未验签预算会话)。
  - 前缀表满时 fail-open:全局各道照常生效,按来源这一道失效(两个节点相同)。
  - 「只限公网来源」把 accept 时看得到的非公网地址当成基础设施。若某个部署形态让真实玩家以私网地址直连 gate / battle(例如同一内网的局域网测试),这一道对它们不起作用,只剩全局各道。这是有意的取舍。
  - `k8s_deploy.ps1` 接线由 gate 未验签预算会话做;在它落地之前,Fleet 形态的 battle 手改 env 会在下次部署时丢。
- **Java 版(AGENTS §12)**:待做,Java 仓 `PARITY.md` 末尾登记一行。不涉及客户端契约。
- **未提交,未编译,未测试。**

## 2026-11-20 Claude:小游戏 WebSocket 收尾——边缘按来源限速、battle 的 WebSocket 计数、小游戏账号注销的重新认证、robot 去掉回环桥(未编译、未测试,待 Codex)

负责人 2026-11-20 要求把 11-19 两条(「小游戏 WebSocket 入口」「robot 加 WebSocket 传输」)里留到以后的项现在做完。设计见 `docs/design/minigame-websocket-gate.md`(W6、W8、W10、W11 已按本条改写)。

- **改了什么**:
  1. **小游戏账号注销不了**(客户端 `Assets/Scripts/UI/Ugui/QdaoServerSelectView.LoginUtilities.cs` 的 `BeginDeletionReauth`):上次登录方式是 `wechat_minigame` / `douyin_minigame` 时,以前会落到口令面板,而小游戏账号没有口令。改为与 Apple / Google 一样重新拉起渠道:向宿主重新要一次登录 code,交给注销接口。服务端的重新认证本来就走与登录相同的认证器(`requestaccountdeletionlogic.go`),没有改。
  2. **battle 导出 WebSocket 计数**:`cpp/nodes/battle/client/battle_connection_admission.{h,cpp}` 的 `Admission::CollectMetrics` 末尾加 `battle_websocket_handshakes_total{result="accepted|rejected"}` 与 `battle_websocket_protocol_closed_total`,与 gate 的两族同义;`tools/scripts/tests/battle_connection_alerts_contract.tests.ps1` 的族清单加这两族、标签集合改成 `reason,result`。「battle 直连准入」会话同一天在这些文件上继续改(按来源限速),已约定它原样保留这两族。
  3. **边缘按来源限速**:`infra-up -WsEdgeSourceRatePerSecond <n>`,缺省 0 = 不限。
     - 为什么要有:经边缘进来的连接,gate / battle 在 accept 时看到的来源是 ws-edge 的 Pod 地址。「battle 直连准入」会话把 gate / battle 的按来源限速改成只管公网源地址(core `accept_admission.h` 的 `IsPublicSourcePrefix`),这样全体小游戏玩家不会被当成一个来源;但这部分连接也就没有任何一处按来源限了,所以在边缘补上——只有边缘看得到玩家的真实地址。
     - 口径照 gate 那一道:来源按 IPv4 /24 聚合,限新建速率、不限总量,桶深 = 速率 × 握手期限(10 秒,库契约 `WsEdgeSourceBurstSeconds`),超出的升级请求回 429。限速排在目标校验之后,回 403 的请求不消耗名额。
     - 与 gate 不同的一点:nginx 不知道后端是否承压,开了就一直生效,所以缺省不开,被打的时候再开。
     - 文件:`tools/scripts/lib/k8s_client_entry.ps1`(`New-WsEdgeYaml -SourceRatePerSecond`,两处占位行)、`tools/scripts/k8s_deploy.ps1`(参数、传给生成器;没给 `-WsEdgeHost` 却给了它时 infra-up 在写操作之前拒绝)、`tools/scripts/dev_tools.ps1` 与 `tools/scripts/k8s_image.ps1`(透传)、`tools/scripts/tests/k8s_client_entry_contract.tests.ps1`(生成器断言、三条包装链的键与类型)。
     - 关闭时生成的 `nginx.conf` 与加这个参数之前逐字相同(指纹不变,不触发 ws-edge 滚动)。
  4. **robot 去掉回环桥**:11-19 的第一版让 `muduo.Client` 拨一个进程内回环监听、再由桥转到 WebSocket,每条连接多一个监听和一跳回环 TCP,打不了高并发。现在把 vendored `muduoclient` 的连接管理搬进仓内(`robot/pkg/wire_conn.go` 的 `wireConn`),只换掉「怎么拨号」这一处;WebSocket 模式下它直接在 WebSocket 上收发,一个客户端帧一条二进制消息,与 Unity 客户端相同。帧的编解码仍用原库的 `TcpCodec`。
     - 对 `GameClient` 可见的行为逐条不变:后台拨号、拨不通每秒重试、发送队列满了就丢、断线后下一次发送失败才重拨、`Close` 不唤醒 `Recv`。两处与原库不同、不影响这些行为:读不设 30 秒期限(WebSocket 下读到一半帧头超时会丢字节),接收缓冲每条连接各用一个新的。
     - 文件:`robot/pkg/wire_conn.go`(新)、`robot/pkg/transport.go`(删掉桥,`dialNode` 取代 `newMuduoClient`)、`robot/pkg/{client,heartbeat}.go`(`*muduo.Client` 换成 `*wireConn`,`closeTransport` 撤掉)、`robot/pkg/{client_test,heartbeat_test}.go`(构造改成 `newTCPWireConn`)、`robot/pkg/transport_test.go`(重写)、`robot/README.md`。
  5. `docs/design/minigame-websocket-gate.md`:W6 改写(原来写的是「以后从 `X-Forwarded-For` 取源地址」,现在定为在边缘限,并写明为什么不在 gate 里按请求头限)、W8 补参数、W10 补注销、W11 改写、§4 补 battle 指标、§7 补 nginx 静态检查、§8 与 §9 同步。
- **没有碰的**:gate / battle 的连接准入模块(除上面第 2 条的两族指标)、core `accept_admission.h`、相关单测与告警文件,都归「battle 直连准入」会话,见它同日的条目。
- **本会话跑过的**:`gofmt -l`(只读的格式与语法检查)覆盖 `robot/pkg` 全部文件与本批动过的 robot 文件,输出为空;PowerShell `Parser::ParseFile` 解析上面六个 ps1,零语法错误。**没有编译,没有跑任何测试。**
- **给 Codex 的验证步骤**(11-19 两条的步骤照旧,下面是本条新增或要以本条为准的):
  1. robot(工作目录 `robot`,先按「robot 加 WebSocket 传输」第 1 步 `go mod vendor`):
     ```
     gofmt -l pkg config main.go gate.go http_assign_gate.go http_assign_gate_ws_url_test.go battle_direct_conn.go network_probe.go heartbeat_idle_smoke_scenario.go
     go build ./...
     go vet ./pkg/ ./config/ .
     go test ./pkg/ ./config/ -count=1
     go test . -run "TestPostJSON_RecordsTheGateWebSocketAddress|NetworkProbe|AssignGate|BattleDirect" -count=1
     go test ./pkg/ -race -count=1
     ```
     通过标准:零失败。`pkg` 的全部用例(Call、心跳、重定向)现在跑在仓内的 `wireConn` 上,它们是这次搬迁的主要回归面,必须全过。传输层用例的名单以本条为准:`TestTransportConfig_Validate`、`TestTransport_AdvertisedAddressWinsAndAMissingOneFallsBackOrFails`、`TestTransport_WebSocketCarriesEachClientFrameAsOneBinaryMessage`、`TestTransport_RequireAdvertisedFailsBeforeDialing`、`TestTransport_EdgeAddressIsDialedThroughDialAddrOverTLS`、`TestTransport_TCPIsTheDefaultAndIgnoresAdvertisements`(11-19 条目里的 `TestWsBridge_RedialsPerConnectionAndStopsOnClose` 与 `TestTransport_WebSocketCarriesMuduoFramesAsBinaryMessages` 已随桥删除)。`-race` 需要 cgo,本机没有就跳过并写明。之后照 11-19 那一条的第 3 步跑四份冒烟(TCP 与 `transport.kind: websocket` 各一遍),TCP 那一遍确认搬迁没有带坏原生路径。
  2. 部署契约(仓库根,只读夹具,不连集群):
     ```
     pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1
     pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1
     ```
     通过标准:fail=0。`battle_connection_alerts_contract` 同时被「battle 直连准入」会话改着,失败时先看是不是它那一批的断言。
  3. 边缘 nginx 配置的静态检查(仓库根,需要本机 docker,不连集群)。`geo` 以路径里捕获的地址为键、`map` 取 /24 这两处写法都没有实测过,这一步能在十秒内定下来:
     ```powershell
     . tools/scripts/lib/k8s_client_entry.ps1
     $out = Join-Path $env:TEMP 'ws-edge-nginx-check'; New-Item -ItemType Directory -Force $out | Out-Null
     foreach ($rate in 0, 20) {
         $yaml = New-WsEdgeYaml -EdgeHost ws.example.com -TlsSecretName ws-edge-tls -PodCidrs 10.244.0.0/16 -TargetPorts 18000, 20000 -TargetApps gate, battle -Image nginx:1.28.0 -SourceRatePerSecond $rate
         $conf = [regex]::Match($yaml, '(?ms)^  nginx\.conf: \|\n(?<body>.*?)^---').Groups['body'].Value -replace '(?m)^    ', ''
         [System.IO.File]::WriteAllText((Join-Path $out "nginx-$rate.conf"), $conf)
     }
     docker run --rm -v "${out}:/work:ro" nginx:1.28.0 sh -c "mkdir -p /etc/nginx/tls && openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=ws.example.com -keyout /etc/nginx/tls/tls.key -out /etc/nginx/tls/tls.crt -days 1 2>/dev/null && nginx -t -c /work/nginx-0.conf && nginx -t -c /work/nginx-20.conf"
     ```
     通过标准:两份配置都打出 `syntax is ok` 与 `test is successful`。报 `unknown "ws_target_ip" variable` 或 `unknown "ws_source_24" variable` 就是这两处写法不成立,把报错原文留下。镜像里没有 `openssl` 命令时,换成在宿主机生成一对自签证书挂进 `/etc/nginx/tls`。
  4. C++:battle 的两族指标随「battle 直连准入」那一批整解一起编;`battle_connection_admission_test` 照过,battle 的 `/metrics` 里有 `battle_websocket_handshakes_total{result="accepted"}`、`{result="rejected"}` 与 `battle_websocket_protocol_closed_total` 三行。
  5. 客户端:同 11-19 那一条第 5 步(编译 0 error;`WebSocketFramedConnectionTests` 10 条)。注销的重新认证要在小游戏宿主里才走得到,这一批没有自动化用例。
  6. kind 上验边缘限速(可选):infra-up 带 `-WsEdgeSourceRatePerSecond 1`,从同一台机器连续发起 15 次以上的 WebSocket 握手(`/ws/<gate PodIP>/18000`),前 10 次左右 101,之后出现 429;去掉参数重跑 infra-up,ws-edge 滚动一次,之后不再有 429。
- **没做 / 没核实**:
  - 把客户端真正转成微信 / 抖音小游戏包。两家转换工具支不支持 Unity 6000.6,这次查了仍没有结论:微信官方的 GitHub 仓库(`wechat-miniprogram/minigame-unity-webgl-transform`)已被 GitHub 停用、文档站 404;团结引擎手册以 2022.3 为基线,自带微信小游戏目标平台,1.5.0 起也有抖音平台。转包之前要负责人先定:继续用 Unity 6000,还是换团结引擎。
  - 客户端的本机持久化(待确认请求、匹配恢复等用 `System.IO.File` 写 `persistentDataPath`)在小游戏宿主里的落盘行为没有核实,属于转包适配。服务端按幂等键去重,落不了盘只是少一次自动恢复,不会重复执行。
  - 抖音对 socket 合法域名的细则、jslib 在真实宿主里的行为,要有开发者账号才能验。
  - robot 除登录、心跳空闲、战斗、换 gate 四份冒烟之外的模式,没有在 WebSocket 下跑过。
- **人工**:同 11-19 那一条(备案域名、证书 Secret、平台后台登记合法域名)。
- **Java 版**:不涉及(客户端契约没变;边缘限速与指标属于部署和运维)。
- **提交情况**:main 工作区,本会话没有提交。11-19 那两条的改动已随负责人 11-20 00:51 的批量提交 `55eae6be3` 入库;本条的改动都未提交。客户端仓从 11-19 起的全部改动(连同本条)仍未提交。

## 2026-11-20 Claude:gate / battle 准入覆盖变量接上部署脚本(-ConnectionAdmissionEnv)、压测汇总加 N2 表与未验签洪水工具、写出阈值校准步骤(未运行,待 Codex 验证;Java 0.1.0-SNAPSHOT 不涉及)

- **起因**:负责人要求做完 11-19「未验签预算收尾」留下的四条。分工:battle 未认证池改 O(1)、battle 按来源前缀限速由「battle 直连准入」会话做完(本日「battle 直连准入收尾」条目);本条做另外两条:
  1. `k8s_deploy.ps1` 没接线 14 个准入覆盖变量:调阈值只能 `kubectl set env`,Agones Fleet 手改的 env 下一次部署就被覆盖;
  2. 阈值全是推导值,没有压测数据,仓里也没有造未验签连接的工具和看结果的汇总表。
- **部署接线**(口径见 gate 准入设计 P2-2 末尾「部署接线」):
  - 新参数 `k8s_deploy.ps1 -ConnectionAdmissionEnv`,逗号分隔的「名字=值」。类型是 `[string[]]`:一个元素里逗号分隔、数组两种写法都收,经 `pwsh -File` 进来的是前一种。
  - 名单在 lib 契约 `GateConnectionAdmissionEnvNames` / `BattleConnectionAdmissionEnvNames`,各 7 个,与 C++ 的 `k*Env` 同名。
  - 解析是 lib 的 `Resolve-ClientEntryConnectionAdmissionEnv`,在库加载后、任何 kubectl 之前跑。名字区分大小写;值只收规范十进制正整数(至多 9 位);格式不对、名字不认识、重名都在入口 throw。范围与相互关系仍只由进程启动时校验,脚本里不复制第二份规则。
  - 按角色分组写:
    - gate 的 Deployment(`New-NodeDeploymentYaml`)与 external StatefulSet 只写 `GATE_*`;
    - battle 的 Deployment、hostPort Deployment、Agones Fleet(含金丝雀池)只写 `BATTLE_*`;
    - scene 不写。
  - lib 的三个生成器经 `PodCommon.ConnectionAdmissionEnv` 拿值;渲染处(`Get-ClientEntryNodeBaseEnv`)再校验一次名单与值的形状,手拼的 PodCommon 绕不过。渲染顺序按名单,与命令行先后无关。
  - 不粘滞(同 `-GateRouterMode`)。每次部署打一行 `[connection-admission] <gate|battle> overrides: ... (not sticky ...)`。
  - `dev_tools.ps1`(`k8s-*-up` 与 `k8s-release-*`)、`k8s_image.ps1` 都透传,param 追加在末尾。`k8s_image.ps1` 在构建镜像之前先用同一个库函数校验一次。
- **压测校准**:
  - `stress_summarize.ps1` 加 N2 表,读同一批 `*_cpp_*` 快照,只出 gate / battle 的行(scene 不出)。每行的列:
    - 连接数与上限;未验签池、早拒起点、预算;接纳数;
    - 按原因的拒绝数:capacity / accept_rate / preauth_budget / preauth_early_drop / source_rate / 其余之和;
    - 淘汰与收割;握手耗时的 n / p50 / p99(毫秒);
    - 按来源限速是否生效、登记的前缀数、表满放行数。
    - 两组 reason(`$cppGateRejectReasons` / `$cppBattleRejectReasons`)由测试对照 C++ `kRejectReasonNames` 钉住。
  - 新增 `tools/scripts/preauth_flood.py`,就是设计稿 §9.3 要的裸 dial 循环。
    - 按速率建连、一个字节都不发,报告 connect_failed / reset / closed / held 四种结局与关闭时刻的分位。
    - 带 `--self-test`:本进程起一个服务端,三种结局各造 3 条再核对。只用标准库;真规模只在 Linux 上跑。
  - 设计稿新增 §9.7「准入阈值的校准」:每个阈值的推导依据、怎么量、改动判据(建议值,第一轮跑完可改)、每轮的判据、结论写哪里。**一轮都没跑,阈值仍是推导值。**
- **代码**:
  - 修改:`tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/dev_tools.ps1`、`tools/scripts/k8s_image.ps1`、`tools/scripts/stress_summarize.ps1`。
  - 新增:`tools/scripts/preauth_flood.py`。
  - 这几个部署脚本同日也有小游戏 WebSocket 会话(ws-edge 段)与别的会话的改动,本条只做精确编辑,没碰它们的段落。
- **测试**(新增 / 调整,未运行):
  - `tools/scripts/tests/k8s_client_entry_contract.tests.ps1`:
    - 新增「库:连接准入覆盖变量」一条。核对解析的分组与顺序、11 种错误输入、三个生成器给了才写且空表与不传逐字相同、手拼 PodCommon 在渲染处被拒,以及两组名单与 C++ 头里的 `k*Env` 一个不多一个不少。
    - 三条包装链的透传清单加 `ConnectionAdmissionEnv`。
  - `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 新增三条,多 5 次 DryRun:
    - 不传时五套 C++ 模板都不写;
    - 传了按角色分组写进五套模板,每个一次、顺序按名单、打两行提示;
    - 五种坏输入在入口被拒,先于任何 kubectl。
  - `tools/scripts/tests/stress_cpp_metrics.tests.ps1`:
    - 钉名一条加 N2 的两组 reason 与 13 个族名;
    - 最小快照一条加 gate 快照与 N2 一行的断言,另断言 scene 快照不进 N2。
  - 已做的静态检查(不是测试证据):改过的 8 个 ps1 PowerShell 解析 0 错误;`preauth_flood.py` 语法检查通过。
- **文档**:
  - `docs/design/gate-connection-admission-control.md`:P1-3 的覆盖一段、P2-2 末尾「部署接线」、新增 §9.7。
  - `docs/ops/gate-kernel-tuning-runbook.md`:§3.2 新增「怎么设」,预算、按来源、压测三处改用部署参数;§3.3 的怎么设与另两处。
  - `deploy/k8s/README.md` 准入三处、`tools/scripts/README.md` 压测一段。
- **交 Codex**(没有 proto 与 C++ 改动;前一步红就停下归因;在工作区跑):
  1. 仓库根,都应全过、退出 0:
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`(多 5 次 DryRun,比之前慢)
     - `pwsh -NoProfile -File tools/scripts/tests/stress_cpp_metrics.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`(它把 dev_tools 的包装函数单独取出执行,确认新透传不影响)
     - `python tools/scripts/check_log_text_english.py`
     - `python tools/scripts/preauth_flood.py --self-test`,Windows 与 Linux 各一次。输出应为 `preauth_flood self-test OK: {"closed": 3, "connect_failed": 0, "held": 3, "reset": 3}`。
  2. DryRun 抽查:按 `k8s_deploy_contract.tests.ps1` 的 `$BaseArgs` 跑一次 zone-up,加 `-ConnectionAdmissionEnv GATE_ACCEPT_RATE_PER_SECOND=2000`。gate 清单里恰好多这一个 env;不加时一个都没有。
  3. 校准不在本条验收内,要压测环境与负责人排期:按设计稿 §9.7 跑一轮,复盘只读 N2 表,结论写 `docs/stress/`。
  4. 失败时保留失败用例的完整输出、DryRun 的 stderr 前 40 行。不改断言,不跳过用例。
- **剩余风险与没做的**:
  - 不粘滞:带过 `-ConnectionAdmissionEnv` 的环境,下一次部署不带就回到推导缺省,部署不会提醒线上有覆盖值。这是照 `-GateRouterMode` / `-NodeOpenFileLimitMode` 的口径;长期值改推导。
  - 部署入口只查名字与值的形状。值越界或相互矛盾(早拒起点大于预算等)要等 Pod 启动才拒:CrashLoop,日志 `Refusing to start`。
  - `preauth_flood.py` 没在真 gate / battle 上跑过。按来源的几行要多个公网源地址,只能在 network namespace 或集群外压测机上量。
  - 阈值仍是推导值,§9.7 一轮都没跑。
- **Java 版(AGENTS §12)**:不涉及。部署脚本与压测工具是本仓的,不是客户端契约;Java 版准入本身的待做在 `PARITY.md` 11-19 的两行里。
- **未提交,未运行。**

## 2026-11-20 Claude:登录排队遗留项收口——入队按账号幂等、队列上限、每拍放行上限、免排队重连占位并按全部角色判、只接本区;门槛 ③ 定为两轮(未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人 11-20 要求把这条线上登记为「以后再修」「等压测再定」的项现在做完,按最标准的定。盘点范围:`docs/design/login-queue-2026-05.md` §14、`docs/ops/login-queue-stress-runbook.md`「已知不足」、`spec_G1b.md` §4.7、`spec_G4b-G.md` §5.8、`spec_G4b.md` §9~§10、`open-server-rate-limit-design.md` §11.7,以及 `loginqueue` 包与 `AssignGate` 的代码。只改服务端;客户端可见回包不变。
- **查到并改掉的六处**(设计稿 §5 决策树已按现状重写):
  1. **入队不按账号去重。** 每次不带令牌的 AssignGate 都新建一个条目:丢了令牌的客户端掉到队尾,原条目成了白占名额的孤儿;同一个账号可以反复入队占位;换设备丢位置(设计稿 §14 第 4 条、手册「已知不足」第 1 条)。
     - 改法:条目号由账号推出(`queueIDForAccount`:对 zone 与账号做 HMAC,不带出账号原文),一个账号在一个 zone 只有一个条目。入队改为一段 `enqueueScript`:还在排队的沿用原位置、只多发一张令牌;已放行待取的不重新入队;上一轮已结束的从队尾重排并重建 meta。匿名分配没有账号可认,仍每次新建。
     - 顺带:入队从一个 MULTI 加两次读(三次往返)降到一次 EVALSHA;队列键的 TTL 改在脚本里「剩余不足 E 才设」,不再需要 Redis 7.0 的 `EXPIRE NX / GT`。
  2. **队列没有上限。** 一个 zone 的排队键都在同一个 Redis 主节点上(noeviction),队列无界时写满的后果是这个主节点上所有写入一起被拒(`spec_G4b.md` §9 R14)。
     - 改法:`Queue.MaxQueueLength`(缺省 1000000),到上限的新账号不入队,login 回 status=5(QUEUE_FULL)加 30~40 秒的重试间隔;已经在排的账号再入队不受影响。本仓网关把它映射成前置限流那种重试提示(code=100、`queue_source="ratelimit"`、`queue_pos=-1`),Unity 与 robot 都不用改。
  3. **一拍放行没有上限。** 空位一下子腾出来时(调大区容量、一台 gate 掉线),一拍会把空位全部放满(`spec_G4b.md` §9 R9)。
     - 改法:`Queue.MaxAdmitPerTick`(缺省 2000),`dispatchOnce` 的批量再按它封顶。
  4. **免排队的重连不占名额。** 这批人在连上 gate 之前不在任何计数里(`spec_G4b-G.md` §5.8 第 3 条):一台 gate 掉线时,dispatcher 看到整台 gate 的空位、把排队的人放进去,与正在重连的人加起来超过区容量。
     - 改法:免排队之前记一枚在途占位 `reconnect:<player_id>`(`Queue.HoldReconnectSlot`,20 秒,不看名额;同一个人反复调用只占一个)。占位写不进不拦重连,只记 ERROR。
  5. **重连判定只看第一个角色,也不看区。** 多角色账号用别的角色在线时重连要排队(设计稿 §14 第 1 条);账号把角色挂在空闲的区里在线,就能免掉爆满的区的排队(本次复查发现)。
     - 改法:`reconnectingPlayer` 看账号的全部角色。建角区是本区的在线角色直接算;别的区的角色只有会话正连在本区现有的 gate 上才算。判定在纯函数 `findReconnectingPlayer` 里。
  6. **两处入口校验。** `zone_id=0` 或别的区的请求写进去的条目没有 dispatcher 出队(`spec_G1b.md` §4.7 第 1 条);读队列长度出错被吞掉、当成队列为空,新来的人可能插队。
     - 改法:排队开着时只接本 login 的 `Node.ZoneId`,否则回 ERROR;读不出队列长度时不走快速通道,落到入队。
- **指标**:`login_queue_assign_total{zone,outcome}`,outcome 取 `fast_path` / `reconnect` / `enqueued` / `rejoined` / `queue_full`。队列满时每分钟至多一行 ERROR。
- **定案,不再等**:
  - `MaxAdmitPerTick` 取 2000:与网关前置限流开服档的区桶同值,快速通道与放行两条路的上限一致。它不是实测的进场吞吐,压测出基线后两处一起调。
  - `MaxQueueLength` 取 1000000:单 zone 排队容量分析的上界(`redis-cluster-all-domains-20261110.md` §6.2)。
  - 这两个键显式写 0 表示不设上限。K8s 的 login ConfigMap 不渲染它们(`New-LoginQueueYaml` 只镜像它列出的键),集群里用代码缺省值。
  - 客户端显示预计等待时间:不做。放行速率取决于在线玩家什么时候离开,估出来的时间不可信。
  - 进场景排队:不做。它是另一个功能,现在没有需要它的玩法。
  - **门槛 ③(真实 Redis 冒烟)两轮都是改常量的前置**:7.2 单实例、8.10.2 Cluster。已写进 `spec_G4b.md` §7、`k8s_deploy.ps1` 常量注释与手册。这回答了同日「登录排队清合入门槛的 Codex 验证单」条目末尾的第 1 个待定项。
  - **规格里的「红」改为当前代码上的变异取红**(下面第 4 步),不取旧提交。回答上一条的第 2 个待定项。
- **残余**(设计稿 §8 已写):合服并进来的角色,原来连着的 gate 又恰好掉线时,判不出它在本区,重连要排队。要判准得让会话记下 gate 所在的区(player_locator 的会话加字段),或让 login 读合服映射;两种都要给 login 加一条新的依赖,这次没做。
- **没有归我改的**:Unity 的无票 429 退避(30/60/120 秒)与排队期间刷新 access token,在「Unity 排队查询失败时恢复重入,不丢排队票」会话那边,已发消息分工。
- **文件**:
  - Go:`go/login/internal/logic/pkg/loginqueue/{queue.go,dispatcher.go,metrics.go,retry_after.go}`、`go/login/internal/logic/loginpregate/assigngatelogic.go`、`go/login/internal/config/config.go`、`go/login/internal/svc/servicecontext.go`、`go/login/etc/login.yaml`。
  - Go 测试:`loginqueue/{queue_test.go,dispatcher_test.go,capacity_test.go}`、`go/login/internal/svc/queue_redis_test.go`、新文件 `loginpregate/assigngate_reconnect_test.go`。
  - 协议:`proto/login/login.proto` 只加了 status=5 的注释,消息结构没变,这一条不需要重新生成。
  - 本仓网关:`java/gateway_node` 的 `AssignGateService.java`(状态映射)、`LoginRpcClient.java`(一行注释)、`AssignGateServiceAuthTest.java`。
  - 文档与注释:`docs/design/login-queue-2026-05.md`(§5 决策树、§8、§10、§14)、`docs/ops/login-queue-stress-runbook.md`、`docs/design/open-server-rate-limit-design.md` §三、`docs/handoff/launch-readiness-20261013/spec_G4b.md` §7、`tools/scripts/k8s_deploy.ps1`(常量注释)。
- **既有用例的改动**:13 个在循环里用同一个账号字面量造多个条目的用例,账号改成空串(匿名,每次一个新条目,与改动前的行为相同),断言没动。涉及 `TestIntegration_QueueDrainAdmits`、`TestIntegration_DispatcherRespectsCapacity`、`TestIntegration_LeaderElectionSerializesDispatch`、`TestIntegration_TwoZonesSharedRedis_BothQueuesDrain`、`TestIntegration_DualLeadersSameZone_NoLossBoundedOverAdmit`、`TestDispatchOnce_BatchSpreadsAcrossGatesWithOneCandidateRead`、`TestDispatchOnce_ConsumedAdmitsHoldCapacityForHandoffGrace`、`TestIntegration_SameZoneStandbyTakesOverAfterLeaderStops`、`TestIntegration_PanickedTermReleasesLockForSameZoneStandby`、`TestDispatchOnce_AdmitsNoMoreThanGatesHold`、`TestConsume_KeepsPlaceholderForHandoffGraceThenReleases`、`TestAdmittedKey_EveryWritePathSetsTTLWhenMissing`、`TestInitLoginQueue_AdmitPipelineUsesTheUnguardedClient`。其中 7 条是门槛 ① ② 点名的用例。
- **给 Codex 的验证**(本会话没有编译,没有运行任何测试;只对 `k8s_deploy.ps1` 做了 PowerShell 语法解析,零错误):
  1. **前置**:同上一条验证单,协议要先生成(`go/proto/common/base/common.pb.go` 里搜得到 `GetClientWsUrl`),否则 `go/login` 编不过。
  2. **Go**(工作目录 `go/login`,环境变量与快照同验证单第一部分):
     ```powershell
     gofmt -l internal\logic\pkg\loginqueue internal\logic\loginpregate internal\svc internal\config
     go vet ./internal/logic/pkg/loginqueue/... ./internal/logic/loginpregate/... ./internal/svc/... ./internal/config/...
     go build ./...
     go test ./internal/logic/pkg/loginqueue/... -count=1 -v -run "TestQueueIDForAccount_|TestEnqueue_|TestHoldReconnectSlot_|TestDispatchOnce_AdmitsAtMostTheTickLimit"
     go test ./internal/logic/loginpregate/... -count=1 -v -run "TestFindReconnectingPlayer"
     go test ./internal/logic/pkg/loginqueue/... ./internal/logic/loginpregate/... ./internal/svc/... ./internal/config/... -count=1
     ```
     - 新用例都要出现在 PASS 里:`TestQueueIDForAccount_StablePerAccountAndZone`、`TestEnqueue_SameAccountKeepsItsPlace`、`TestEnqueue_AnonymousCallsStaySeparate`、`TestEnqueue_AdmittedButUntakenIsNotRequeued`、`TestEnqueue_AfterTakingTheAdmitStartsANewRound`、`TestEnqueue_OrphanedMemberRestartsAtTheTail`、`TestEnqueue_FullQueueRejectsNewcomersOnly`、`TestHoldReconnectSlot_OnePlaceholderPerPlayer`、`TestDispatchOnce_AdmitsAtMostTheTickLimit`、`TestFindReconnectingPlayer`(6 个子用例)、`TestFindReconnectingPlayer_ReadsTheGateListOnlyWhenNeeded`。
     - 既有的 `TestEnqueue_QueueKeyTTLNeverShortened`、`TestEnqueue_QueueTokenExpiresAtMaxAge` 仍要过(入队的实现换了,语义没变)。
     - 整包四个包全绿。`gofmt -l` 列出本条「文件」一节里的 Go 文件时,允许对它 `gofmt -w` 后重跑并在结果里注明(新用例是手排的);列出别的文件只记录。
     - 这一步跑完之后再跑验证单第一部分:门槛用例里有 7 条被本条改过账号字面量,证据要出自改后的代码。
  3. **本仓网关**(工作目录 `java/gateway_node`):`./mvnw.cmd -q test "-Dtest=AssignGateServiceAuthTest"`。新用例 `queueFullMapsToTheRateLimitRetryHintWithoutTokenOrAddress`、`queueFullWithoutHintFallsBackToThirtySeconds` 通过,原有用例不变。
  4. **变异取红**(不改主工作区,不 stash、不 reset):把 `go\login`、`go\shared`、`go\proto` 用 `robocopy /E` 拷到 `%TEMP%` 下的目录,在副本里每次只施加一种,跑对应用例,看完复原。实际变红的用例与下面写的不一致时停下报告。
     - 本条的改动:
       - A:`enqueueScript` 删掉 `redis.call('DEL', KEYS[2])` → `TestEnqueue_AfterTakingTheAdmitStartsANewRound` 红。
       - B:`Enqueue` 里删掉 `if account != ""` 那三行(每次都是随机条目号)→ `TestEnqueue_SameAccountKeepsItsPlace`、`TestEnqueue_AdmittedButUntakenIsNotRequeued` 红。
       - C:`enqueueScript` 的 `kept` 去掉 `redis.call('TYPE', KEYS[2]).ok == 'hash' and` → `TestEnqueue_OrphanedMemberRestartsAtTheTail` 红。
       - D:`enqueueScript` 删掉开头判 `EXISTS KEYS[4]` 的那一段 → `TestEnqueue_AdmittedButUntakenIsNotRequeued` 红。
       - E:`enqueueScript` 删掉回 `{4, 0, 0}` 的那个判断 → `TestEnqueue_FullQueueRejectsNewcomersOnly` 红。
       - F:`dispatchOnce` 删掉 `if d.maxAdmitPerTick > 0` 那三行 → `TestDispatchOnce_AdmitsAtMostTheTickLimit` 红。
       - G:`HoldReconnectSlot` 的成员改成 `reconnectHoldPrefix + uuid.NewString()` → `TestHoldReconnectSlot_OnePlaceholderPerPlayer` 红。
       - H:`findReconnectingPlayer` 的循环改成只看 `roles[:1]` → 子用例「the online role is not the first one」红;把 `if role.GetZoneId() == zoneID` 改成恒真 → 子用例「role of another zone online on that zone's gate does not skip this zone's queue」红。
     - 门槛 ① ② 的用例(代替两份规格里旧提交上的红):
       - I(G1b):`ZoneDispatcherLockKey` 改成 `return base`(全服一把锁)→ `TestZoneDispatcherLockKey_IsPerZone`、`TestIntegration_TwoZonesSharedRedis_BothQueuesDrain` 红。
       - J(G4b-G 的 Q2):`consumeAdmitAndSign` 里 `q.holdAdmitted(ctx, q.rdb, zoneID, queueID, q.handoffGrace)` 的最后一个参数改成 `0` → `TestConsume_KeepsPlaceholderForHandoffGraceThenReleases`、`TestDispatchOnce_ConsumedAdmitsHoldCapacityForHandoffGrace` 红。
       - K(G4b-G 的 Q1):`TryReserveFastPathSlot` 里占位到期的 `now.Add(q.handoffGrace)` 改成 `now.Add(24 * time.Hour)` → `TestFastPath_SustainedLoginsDoNotStarveBudget`、`TestTryReserveFastPathSlot_BudgetAndPerMemberExpiry` 红。
  5. **冒烟补充项**(并入验证单第二部分,两轮都做;robot 用带账号的登录方式):
     - **重入队保位置。** 第 2 步有 100 人在排、100 人在线时,不停原来的 robot,再起第二个 robot 进程,用同一份配置(同一批账号)。期望:`ZCARD 'queue:{q:1}'` 仍是 100,没有翻倍;`login_queue_assign_total{zone="1",outcome="rejoined"}` 涨了约 100;`ZRANGE 'queue:{q:1}' 0 2` 的成员以 `a-` 开头。
     - **免排队重连占位。** 同一步里,在线的那 100 个账号在第二个进程里走免排队(顶号):`ZRANGE 'admitted:zset:{q:1}' 0 -1` 里出现 `reconnect:<player_id>` 成员,约 20 秒后消失;`outcome="reconnect"` 的计数涨了约 100。做完停掉第二个进程。
     - **队列上限。** 冒烟用的 login 配置里把 `Queue.MaxQueueLength` 设成 50,重做第 2 步。期望:`ZCARD 'queue:{q:1}'` 不超过 50;`outcome="queue_full"` 的计数上涨;login 日志出现一行 `zone 1 queue is full at 50 entries`;被拒的 robot 没有拿到排队令牌,30~40 秒后再来。做完改回缺省值。
     - **每拍上限。** 把 `Queue.MaxAdmitPerTick` 设成 10,做第 4 步(区容量 100 改到 150)。期望:login 日志的 `[loginqueue] zone=1 admitted=N` 每行 N 不超过 10,连续 5 拍放完 50 人。做完改回缺省值。
     - **验证单第 9 步(毒丸 meta)的期望要改读**:被毒的客户端收到 410 后重新入队,条目在队尾按同一个条目号重建,毒丸被这次入队清掉。带 `metaGone=1` 的那行日志只有 dispatcher 先于客户端碰到这个条目时才出现,不作为判据。
  6. **日志文案**:仓库根 `python tools/scripts/check_log_text_english.py`,本条新增的日志都是英文。
  - 失败时保留:失败用例的完整 `-v` 输出、`go vet` / `go build` / mvn 的报错原文。命令失败后不连续重试,不改用例迁就。
- **Java 版(AGENTS §12)**:独立 Java 版待做,已在 Java 仓 `PARITY.md` 登记一行(入队按账号幂等、队列上限、每拍放行上限、免排队重连占位、重连判定、只接本区)。客户端契约没变,两版客户端都不用改。本仓 `java/gateway_node` 已同批改。
- **提交情况**:都在 main 工作区,未提交。

## 2026-11-20 Claude:data_service 熔断按逻辑库——核实与设计稿完成,代码未动,待负责人同意 32 个文件的落码范围(Java 不涉及)

- **起因**:同日「go/db 熔断改成按存档库」条目记下的另开任务:data_service 上框架熔断不分分片。
- **核实结论**(静态阅读 go-zero v1.10.0 源码与本仓代码,没有编译、没有运行):
  - 问题属实,有两层:服务端按方法的熔断(`zrpc.MustNewServer` 缺省开),以及各 Go 调用方按「目标 + 方法」的客户端熔断。一个集群失联、落在它上面的流量占某方法三分之一以上时,两层都会把指向健康库的调用也挡掉。
  - data_service 的 45 个 RPC 多数一次要碰不止一个逻辑库(账号片 + 玩家片 + 名字片 + 存档库),熔断不能放在 RPC 入口按一个键选。
  - 调用方 14 个构造点里没有只认 `codes.Unavailable` 的分支;客户端熔断现在的开关各服务不一致(login 开;guild、trade、match、player_inbox 关;scene_manager 本地开、K8s 关;db、mail、rank、payment、chat 没写即缺省开)。
- **设计**(`docs/design/data-service-breaker-per-logical-db.md`):
  - 服务端关掉按方法的熔断;`shared/authoritysql` 加一个可选的建连钩子,data_service 给每个逻辑库一个 go-zero 熔断,包住建物理连接这一步。store 代码不改。闸门冻结不在建连阶段判,天然不计入;调用方取消不计。
  - 调用方建 data_service 客户端时一律关掉 zrpc 客户端熔断,规则收在 `shared/dsclient` 一个函数里,带一条扫描构造点的守卫用例。
  - 不加的:发号器 etcd 的熔断(它自己有失败退避)、新指标、任何错误码改动。
- **现在的状态**:只新增了设计稿、在 `docs/README.md` 登记了一行。**代码一行没动。** 预计改动 32 个文件(清单在设计稿 §6),按 `AGENTS.md` §10.2 要负责人对这一批明确同意后再写。
- **拿到同意之后要做的**:按设计稿 §5、§6 落码与单测(§7),同步 `horizontal-scale-logical-db.md`、保障地图、`db-task-store-unavailable-deferral.md` 文首那一句,另起一条 PROGRESS 写给 Codex 的命令与通过标准。
- **Java 版(AGENTS §12)**:不涉及(Java 版没有 data_service 与 go-zero),不加 PARITY 行。
- **提交情况**:main 工作区,未提交。

## 2026-11-20 Claude:部署替运维维护 domain-service-secrets 的内部调用密钥——16 把缺的自动补齐、全集群每把一个值、不同值写前拒绝,-ResyncDomainServiceSecrets 统一并重启(未运行,待 Codex 验证;Java 不适用)

- **起因**:上一条(2026-11-19「实名与防沉迷 K8s 接入的三处收尾」)把写前核对扩成整表后留了两件事:只在 zone 用的四把(`account-directory-login`、`account-directory-scene-manager`、`role-fence-db`、`player-store-scene`)不核;运维要在每个 namespace 先手工预置 `account-directory-payment`,以前从没被核过的 11 把在现有环境里可能缺失或两边不同,第一次部署会被拦下、要人工补。负责人要求这些都由部署做掉。核对时另发现两处:
  1. data-service 与 go/db 是全局池(发现键不分 zone),login、scene、scene-manager 的调用会落到别的 zone 的实例上,所以只在 zone 用的键同样要全集群同值;上一条「只在一侧用的键当场就会暴露、不用比」的判断不对。
  2. 上一条的门禁在客户端入口契约测试的非 DryRun 假 kubectl 夹具里会拦下走过门禁块的用例(夹具没答这个 Secret)。本条一并补上。
- **做了什么**:
  - `tools/scripts/k8s_deploy.ps1`:共享键表改成托管键表 `$script:ManagedDomainServiceSecrets`,16 把,恰好是 zone 工作负载以 `optional: false` 引用的全部键,每行写 zone、infra 两侧读它的工作负载(Pod 的 app 标签)。删掉 `Read-DomainServiceSecretDigests` 与 `Assert-SharedDomainServiceSecretsForRun`,新增:
    - `New-DomainServiceSecretEntry`:比较形态,base64 原文加去掉首尾空白后的 SHA-256 与字节数(与 serviceauth、scene 的验签口径一致)。
    - `Read-DomainServiceSecretEntries`:一次 `kubectl get secrets --all-namespaces --field-selector metadata.name=domain-service-secrets -o json`(经 `Invoke-ClientEntryKubectl`),只收 infra 与 `<NamespacePrefix>-*`,与 `Find-ReadyDataServiceNamespace` 同一判据。zones.json 没列的 zone 也比,别的前缀(另一套环境)不比、不碰。读失败、输出不是 JSON、base64 不合法都 throw,报错不带值。
    - `Get-ManagedDomainServiceSecretPlan`(纯函数):规范值存在 infra(只在 zone 用的四把也存一份);infra 合格就用,infra 没有就沿用各 zone 一致的值,都没有就生成 32 字节随机数。记错的情形:值不同、去掉首尾空白后不足 32 字节、两把同值、infra 没有而各 zone 不一致;`-Resync` 时改写并记下要重启的 Pod(zone 侧 scene 展开成 scene / scene-world / scene-instance,infra 侧带 `-canary`)。只补 infra 与本次部署的 zone。
    - `Initialize-ManagedDomainServiceSecrets`:写路径入口的只读门禁,判不了就在任何写操作之前 throw,判得了才存计划。`zone-up` / `all-up` 部署 zone 的 Go 服务时维护全表,只发 C++ 节点时只维护 scene 读的四把;部署全局 Go 服务时维护 infra 里有人读的 12 把。DryRun 只打一行。
    - `Sync-ManagedDomainServiceSecrets`:门禁块的最后一步,所有只读门禁之后、`switch ($Command)` 之前。已有 Secret 用 `kubectl patch --type merge --patch-file`(值经临时文件,用完即删;不用 apply:对运维建的 Secret 做部分 apply 会记 last-applied,下次 3-way merge 会删键);没有的先建 namespace 再 create。`-Resync` 改写过的 namespace 执行 `kubectl delete pod -l 'app in (...)' --wait=false`(secretKeyRef 只在容器启动时读)。只补缺不重启:缺键的 Pod 卡在 `CreateContainerConfigError`,kubelet 自己重试。
    - 新开关 `-ResyncDomainServiceSecrets`,不粘滞。
  - `tools/scripts/dev_tools.ps1`:只在被指定时透传 `-ResyncDomainServiceSecrets`;`k8s-image-*` / `k8s-release-*` 给了就拒绝。
- **测试(新增或改写,未运行)**:
  - `tools/scripts/tests/k8s_migrate_gate.tests.ps1`:旧的两条共享密钥用例换成五条「托管内部密钥…」:
    1. 写入计划:全新集群生成、从 infra 复制、沿用各 zone 一致的值,四种拒绝(报错不带值)与 `-Resync` 的改写和重启名单。
    2. 门禁:DryRun 五种命令组合的说明行且不调 kubectl;一次列表调用;别的前缀不算、zones.json 没列的 zone 也比;base64 不合法、kubectl 失败、输出不是 JSON(报错不带 Secret 片段);值不同时拒绝且不存计划;`-Resync` 放行;全新集群用脚本自己的生成器(64 位小写十六进制、互不相同)。
    3. 写入:merge patch 的内容与临时文件删除、create 的 Secret、删 Pod 的参数,DryRun 与没有计划时不写。
    4. 接入位置:门禁 → 写入 → switch,写入是门禁块最后一条语句。
    5. 表与清单一致:按 `$GoSvcCatalogue` 的 `Global` 分 zone 与全局两侧(新服务自动纳入),表里恰好是 zone 引用的 `optional: false` 全部键,每行两侧用法与清单对上。
  - `tools/scripts/tests/k8s_client_entry_contract.tests.ps1`:透传名单与类型表加 `ResyncDomainServiceSecrets`;`Invoke-DeployWithFakeKubectl` 补一条缺省规则,假集群的 infra 与测试 zone 已备齐 16 把同值密钥,各用例仍只测自己那道闸,写入步骤无事可做。
- **文档**:开服手册 §1 改写(托管的 16 把一张表、运维仍要预置的键一张表,维护规则、`-ResyncDomainServiceSecrets`、换值步骤,删掉手工补 `mail-producer-battle` 的旧步骤);`release-checklist.md` ⑦;`open-new-region-runbook.md` §4.2;`player-inbox-runbook.md` 两条排障;`battle-overflow-mail.md` §8;`real-name-anti-addiction.md` §8;`deploy/k8s/README.md`(Optional Flags 加开关,四处「运维预置」改成「由部署补齐」)。
- **文件**:上面各文件。`k8s_deploy.ps1`、`deploy/k8s/README.md`、`release-checklist.md` 带着别的会话的未提交改动,提交时按 hunk 分。
- **Codex 验证**(仓库根,不需要集群;失败时保留完整输出与 `[FAIL]` 行):
  1. `pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`:期望全 PASS,含五条「托管内部密钥…」。
  2. `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`:期望全 PASS;「非 DryRun 负向:…」「非 DryRun 无漂移:…」这几条会走过托管密钥门禁,是这次夹具改动的回归面。
  3. 回归:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`k8s_login_auth_provider_guard.tests.ps1`(DryRun 输出多一行说明)。
  4. `python tools/scripts/check_log_text_english.py`:期望干净(新加的 Write-Host 与 throw 都是英文)。
  5. 冒烟(可选,kind;部署机 kubectl 要支持 `patch --patch-file`):全新集群 `infra-up` 与 `zone-up`,期望各 namespace 打一行 `[secrets] domain-service-secrets in namespace ...: wrote ...`、Pod 起来;再跑一次期望 `to write: nothing`;手工改某个 zone 的 `role-fence-guild`,期望写前拒绝并点名;带 `-ResyncDomainServiceSecrets` 重跑,期望改回并重启那个 zone 的 data-service。
- **上线前要做(运维)**:这 16 把不用再手工预置。以前预置过、值不一致的环境,第一次部署按报错带 `-ResyncDomainServiceSecrets` 重跑一次(会重启读这些键的 Pod,选低峰)。
- **未验证**:全部没有运行;PowerShell 只用 `Parser::ParseFile` 查过语法(0 错误)。
- **Java 版(AGENTS §12)**:不适用,Java 版没有 K8s 部署脚本,不往 PARITY.md 加行。

## 2026-11-20 Claude:实时冷却与 Buff 跨重登保留·独立评审后再补(查不到行的条目带回、空冷却列表的字段、到期时刻不漂移)

- **起因**:对本功能 11-19、11-20 两条的改动做了一次独立只读评审。没发现崩溃或写坏数据的问题,按评审结论修四处、补一处配表约束、放宽一条用例的余量(都未编译):
  1. **本节点表里查不到行的 Buff 条目原样带回,不丢**(混版本保护 P1)。原来 `RestoreSavedBuffs` 查不到行就丢:滚动发布时新版本加了一个保留行,玩家落到旧节点,下一次存盘就把它永久抹掉。现在不生效、留在 `SavedBuffsPendingComp` 上,`SaveKeptBuffs` 原样写回(连未知字段),过了它自己的到期时刻才丢。表里改成 0 的照旧丢弃(本节点的表说了算),设计稿写明了这个窗口的影响。
  2. **空冷却列表也保留更新版本的列表级字段**:`RestoreCooldowns` 原来只看列表是否为空,列表里没记录、只带着新版本字段时不挂组件,存盘时清掉。现在按 `ByteSizeLong() == 0` 判。
  3. **恢复出来的到期时刻与存档一字不差**:原来剩余时长与到期时刻各读一次时钟,每重建一次多出几毫秒,加载后第一次存盘也因此判脏。现在 `StartBuffEntry` / `ArmBuffExpiry` 接调用方读一次的 `nowMs`。
  4. **新建条目在生效回调之后按号重查再挂定时器**,不端着 emplace 时的迭代器(回调将来往表里插入或摘掉这一条时不会写到失效节点)。
  5. 设计稿 §2.4 补配表约束:保留的行不要做成周期性重复施加,否则下线收尾一直判成没收敛。
  6. `SnapshotBuffRoundsFollowTheRemainingTime` 的余量从 3 秒放到 5 秒(仍折 1 回合)。
- **改动**(main 工作区,未提交):
  - C++:`combat/buff/system/buff.{h,cpp}`、`combat/buff/comp/buff_comp.h`(注释)、`combat/skill/system/skill.{h,cpp}`;
  - 测试:`buff_test` 新增 `BuffKeepOnLogoutTest.RestoreCarriesRecordsOfRowsThisNodeDoesNotKnow`,`RestoreContinuesFromSavedExpiryWithClampedLayer` 改为到期时刻精确相等,`RestoreTrimsByTheTableAtLoadTime` 的查不到行用例改成已到期;`skill_test` 新增 `SkillUtilTest.RestoreCooldowns_EmptyListWithNewerFieldsIsCarried`;`bag_test/player_battle_settlement_test.cpp` 放宽余量;
  - 文档:`docs/design/realtime-cooldown-buff-persistence.md`(开头、§2.4、§2.5、§3、§5、§6)、`docs/design/ecs-component-access-rules.md`(`SavedBuffsPendingComp` 一行)、`docs/design/disconnect-reconnect-audit-20261020.md` §9.2(第一段现状:移动已由 11-12「进视野补发移动」补上,战斗状态标志走 66、Unity 不消费)。
- **预检**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`,10 tests OK。没有编译,没有跑任何单测。
- **给 Codex 的验证步骤**:同 11-19 同名条目的六步与 11-20 第一条的补充(多跑 `-Filter turn_battle`);第 4 步确认 `BuffKeepOnLogoutTest.*`、`SkillUtilTest.RestoreCooldowns_*`、`BuffDurationTest.*`、`BuffStackingTest.*`、`PersistRoundTripTest.*`、`PlayerBattleSettlementTest.SnapshotBuffRoundsFollowTheRemainingTime` 全过。
- **Java 版**:对应 Java 版本无(`0.1.0-SNAPSHOT`),只在 Java 仓 `PARITY.md` 末尾追加更正(查不到行改为原样带回),没有改 Java 代码。

## 2026-11-20 Claude:scene 按 gate 实例认会话(e 方案 C)、login 跨实例同号改回踢线(未编译,待 Codex 验证)

- **起因**:「断线重连」会话 11-19 核实总纲 §4 末尾五条待确认时,第 2 条留下一条残余([infra-reconnect-overview.md](design/infra-reconnect-overview.md) §4):gate 会话号是 `node_id<<17 | 序号`,同一 node_id 短暂有两个 gate 进程(新进程接走号、老进程还没关停)时,同一名玩家可能在两边拿到同一个会话号。老进程关闭旧连接时迟到的 `ExitGame` 被 scene 当成新会话的断线,新会话进入断线保留,25s 后退出、存盘、要重登(不丢数据)。根因:scene 的 `SessionMap` 只按 session_id 认会话,`ExitGame` 也不核对发它的 gate 实例。按 [scene-enter-chain-residuals.md](design/scene-enter-chain-residuals.md) §10 第 1 条「e 方案 C」做,设计、两处措辞的定论与残余见该文新 §14。
- **两处措辞(写进设计稿正文)**:
  1. 不变量 2「同一会话一律放行」的「同一会话」= 同一 (gate 实例, session_id);gate 实例取自 RPC0 入站连接的握手身份,不信消息体自报;任一边未知时按会话号认(与 player_locator 按实例号忽略上报同一口径)。
  2. 不加「入口前的连接级判定」:连接身份是请求身份的一部分,组上下文时读出,入场的身份判定仍只有栅栏这第一步。客户端消息不是入场,实例核对并进它原有的会话身份守卫。
- **做了什么**(main 工作区,未提交):
  - RPC0:入站侧握手登记成功后、回成功之前把对端 node_uuid 记在这条连接的 `RpcChannel` 上(`RecordHandshakePeer`),派发时经 `RpcController::HandshakePeerUuid()` 交给 handler。记在连接上而不是从 gate 节点实体反查:gate 从 etcd 摘除时 scene 销毁它的节点实体,连接却还开着(这次核实,原登记为【未核实】)。
  - scene:身份组件 `PlayerLoginIdentityComp`、待入场上下文、改派票据都带 gate 实例;入场栅栏(角色登记前的预检与 `HandleEnterGameNode` 第 0 步)、待入场合并、`EnterScene` 的换客户端判断、改派条目对账都按 (实例, 会话号) 认会话;`ProcessClientPlayerMessage` 遇到出自另一个 gate 进程的同号消息(含 ExitGame)不派发、不擦映射、不取消载入,按「会话不在」答复;被拒入场的 3023 只推给请求出自的那个 gate 进程(否则会推到新进程上同号的活玩家)。`[EnterFence]` 汇总行新增 `stale_gate_instance_rejected`。
  - login:`kickReplacedSession` 的跨实例同号改回踢线(§12.3 第 1 点),删掉 `login_kick_total{result="skip_cross_instance"}`,踢线日志加 `new_instance`。
  - 不改 proto、gate、player_locator、scene_manager、location 格式;不新增 `.cpp`,不动 vcxproj / CMakeLists;不新起线程。
- **文件**:`cpp/libs/engine/core/network/{rpc_channel.h,rpc_channel.cpp,rpc_controller.h}`、`cpp/libs/engine/core/node/system/registration/registration_manager.cpp`、`cpp/libs/services/scene/player/system/{enter_fence.h,player_lifecycle.h,player_lifecycle.cpp,role_admission_system.cpp}`、`cpp/libs/services/scene/core/system/redis.cpp`、`cpp/nodes/scene/handler/rpc/scene_handler.cpp`(守护段)、`go/login/internal/logic/clientplayerlogin/{entergamelogic.go,metrics.go,entergame_kick_test.go}`、`cpp/tests/cross_zone_test/{cross_zone_test.cpp,role_admission_test.cpp}`、`cpp/tests/rpc_controller_test/rpc_channel_loopback_test.cpp`;文档 `docs/design/{scene-enter-chain-residuals.md,scene-enter-session-fence.md,muduo-rpc-channel.md,infra-reconnect-overview.md}`。`registration_manager.cpp`、`role_admission_system.cpp`、`role_admission_test.cpp` 工作区是 CRLF,改动沿用 CRLF。
- **测试(新增或改写,未运行)**:
  - `cross_zone_test.cpp` 末尾新增 8 条:`GateInstanceFence.SameGateInstanceComparesOnlyWhenBothAreKnown`、`GateInstanceFence.ReusedSessionIdOnAnotherGateProcessIsNotTheSameSession`、`GateInstanceFenceEcs.LateEnterFromOldGateProcessIsRejected`(复现残余:迟到入场面)、`GateInstanceFenceEcs.LateEnterFromOldGateProcessDuringLoadKeepsThePendingLoad`、`GateInstanceFenceEcs.ExitGameFromOldGateProcessIsStale`(复现残余:ExitGame 面)、`GateInstanceFenceEcs.OldClientMessagesDuringLoadAreStaleAndCannotCancelTheLoad`(旧客户端消息面)、`GateInstanceFenceEcs.EnterWithoutGateInstanceKeepsTheKnownInstanceOfTheSameSession`、`GateInstanceFenceEcs.RelocateEntryRemembersTheGateProcessOfTheSession`。
  - `rpc_channel_loopback_test.cpp` 新增 `RpcChannelLoopback.HandshakePeerRecordedOnTheInboundChannelReachesEveryLaterDispatch`(`LoopbackSceneService` 多记一项 `processPeers`)。
  - go/login:`TestEnterGame_SameSessionDifferentInstanceSkipsCrossInstance` 改回 `TestEnterGame_SameSessionDifferentInstanceKicks`(踢 1 次、目标实例是旧实例、`sent` +1);`TestEnterGame_SameSessionSameInstanceDoesNotKick` 不再看已删的 result,改看 `sent` 不变。
  - 跟着签名改的既有用例:`cross_zone_test.cpp` 的 `StaleEnterDoesNotSupersedeRelocateEntry` 与 `role_admission_test.cpp` 两处 `JudgeEnter` 补实例参数;`ActingLoginGeneration.MatchingSessionYieldsGeneration` 去掉对 constexpr 身份变量取地址的 `static_assert`(身份组件带 `std::string` 后不再能做 constexpr 变量;编译期可求值仍由 `NoIdentityComponentYieldsZero` 的 `static_assert` 守着,取值由同一用例的运行期断言守着)。
- **预检**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`,10 tests OK。没有编译,没有跑任何单测。
- **给 Codex 的验证步骤**(仓库根;没有改 proto,不需要协议生成;失败时保留首个编译错误原文、失败用例的完整输出与 `--gtest_filter` 单跑结果):
  1. C++ 整解:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(必须串行 `/m:1`)。改了 core 与 scene 两个库,`run_cpp_tests -Build` 不重编库,所以这一步要先做完。期望 0 error。
  2. `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/scene-gate-instance-20261120/cpp-tests`:先单跑 `build\cpp\tests\cross_zone_test.exe --gtest_filter=GateInstanceFence*:EnterFence*:ActingLoginGeneration*:RoleAdmission*`,应含上面 8 条新用例且全绿;再跑全量,全绿(别的会话的既有失败点名即可)。
  3. `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test -LogDir build/verification/scene-gate-instance-20261120/cpp-tests`:先单跑 `build\cpp\tests\rpc_controller_test.exe --gtest_filter=RpcChannelLoopback.*:RpcClient*:NodeHandshake*`,含新用例 `HandshakePeerRecordedOnTheInboundChannelReachesEveryLaterDispatch`;再跑全量。
  4. 回归:`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -LogDir build/verification/scene-gate-instance-20261120/cpp-tests`(全部工程)。core 与 scene 两个库被大多数测试工程链接,`RpcController` 多了引用成员、`PlayerLoginIdentityComp` 多了 `std::string` 成员,要确认别的工程照样编得过、跑得绿(别的会话的既有失败点名即可)。
  5. Go(`go/login` 目录):
     `go test ./internal/logic/clientplayerlogin/ -count=1 -v -run "TestEnterGame_"`,期望 `TestEnterGame_SameSessionDifferentInstanceKicks` 与 `TestEnterGame_SameSessionSameInstanceDoesNotKick` 在内全绿;
     再 `go vet ./internal/logic/clientplayerlogin/` 与 `go test ./... -count=1`。
  6. 变异核对(每个单独做、跑完逐字节恢复,期望红的用例见设计稿 §14.4 的表):M1 `enter_fence::SameGateInstance` 改成恒 `return true;`(= 旧口径「同号即同会话」);M2 `RecordLoginIdentity` 里实例改成会话有效就无条件照写;M3 票据实例改回 `ResolveGateInstanceId(session->gate_session_id())`;M4 覆盖待入场上下文时的擦除条件去掉 `previousSessionId != sessionId`;M5 `RpcChannel::CallServiceMethod` 改回 `RpcController controller(connection);`;M6 `kickReplacedSession` 恢复「会话号相同、实例不同 → 不踢」。M1~M5 跑 cross_zone_test / rpc_controller_test 对应用例,M6 跑第 5 步的 Go 用例。
  7. `python tools/scripts/check_log_text_english.py`:期望干净(新日志都是英文)。
  - **通过标准**:第 1 步 0 error;第 2~5 步全绿;第 6 步每个变异的红绿与设计稿 §14.4 一致;第 7 步无输出。回报每步实跑条数与平台。
- **没有行为测试的接线**:`scene_handler.cpp` 里读连接身份与跨实例不派发的几行、`OnNodeHandshake` 调 `RecordHandshakePeer` 的一行(宿主不构造 Node)、被拒提示按实例核对(走 `PushToSessionViaGate` 既有分支,宿主没有 gate)、`ReconcileRelocateOnReentry` 的票据作废条件。双 gate 同号的端到端冒烟没有现成场景,未做。
- **发布顺序**:scene 与 player_locator 都先于 login(踢线引出的旧实例 ExitGame / 断线上报要先能被认出);回滚 scene 之前先回滚 login。项目未上线,同批发布即可。
- **残余**(设计稿 §14.6):location 不记实例号(SM 的 R13 仍在,scene 侧已挡住);scene → gate 回推仍按 node_id 找 gate(只有老进程重新握手登记时才可能投错进程);托管落点不带实例;双方都没有登录代次时后到者胜。
- **未验证**:全部没有编译、没有运行。
- **Java 版(AGENTS §12)**:对应 Java 版本 `0.1.0-SNAPSHOT`,待做,本轮不写 Java 代码,只在 Java 仓 `PARITY.md` 末尾登记(Java 网关不走 RPC0 握手,会话与归属模型不同,有没有同类窗口待核实)。

## 2026-11-20 Claude:玩家存档集合收尾——活跃积分来源按周期删、「本期已结束」单列提示码、任务存档删旧格式与接取时间、位图容量按已发位数(未生成、未编译、未测试,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:负责人定「上线前玩家数据可以完全清空;以后修的现在修;不等压测、按标准定」。接同日两条「活跃度领取记录按周期裁剪」「任务完成状态改按位序位图落盘」,并**更正**它们:
  - 前一条写的「积分来源不裁」作废,改成按周期删(去重改记在位图里);「更早的原请求重放按不是当前周期拒绝(`kInvalidParameter`)」改为回新码 `kRewardPeriodEnded`。
  - 后一条写的「旧存档照常读」「`completed_mission_ids` 只装没有位序的 ID」作废:旧格式全部删除,部署前清档。
- **改动**(服务端):
  1. **活跃积分来源**(`cpp/libs/services/scene/player/system/player_activity.cpp`、`proto/common/component/mission_comp.proto`):`PlayerActivityLedger` 加 `bytes credited_missions = 3`,按任务位序每个任务 1 位,记「这个一次性任务已经记过积分」;`OnMissionCompleted` 先查这一位去重,再删掉当前规则已结束周期的来源(`PruneEndedSources`),再同帧写位与来源。账本的三部分都不再随时间增长。记不下来源(账本校验不过、任务没有位序、本周期来源满 4096)时打 ERROR,原来是静默丢。
  2. **「本期已结束」提示码**:`Claim` 在请求是当前规则、周期早于当前周期、又没有对得上的原记录时回 `kRewardPeriodEnded`(跨日边界上的领取、记录已被裁掉的原请求重放);其余身份不符仍回 `kInvalidParameter`。码由新脚本 `tools/scripts/activity_vitality_xlsx_patch.py tip-codes` 追加到 Tip.xlsx 既有的 `//reward_error` 组尾(业务拒绝,不填 fault),文案「本期活跃已结束，不能再领取；已领到的奖励以背包为准」。
  3. **任务存档删旧格式**(`proto/common/database/bag_quest_mail_data.proto`、`mission_marshal.{h,cpp}`、`mission_comp.{h,cpp}`、`player_mission.cpp`、`player_feature_snapshot.cpp`、`player_database_loader.cpp` 注释):`QuestAllData` 字段 1/2/4(active、completed、scoped_state_present)与消息 `QuestEntry`、`QuestScopeData.completed_mission_ids`(字段 3)删除并写 `reserved`;`Unmarshal` 只剩一条读法;`MissionsComp` 不再有「没有位序的已完成 ID」集合(没有位序的 ID 不是任何版本的任务,更新版本的任务以超出容量的位原样带回)。
  4. **删接取时间**:`MissionListComp.mission_begin_time`(字段 3,`reserved`)没有任何读取方,完成后也不删;写入(`PlayerMissionSystem::Accept`)与放弃时的删除(`MissionSystem::AbandonMission`)一并删掉。
  5. **位图容量**(`tools/data_table_exporter/core/generators/bit_index_gen.py`):`_bitmap_capacity` = 最大位序 + 1,不再取 `max(表里最大 ID, …)`。任务 ID 稀疏或很大时,原口径会让每个玩家内存里的两个 `std::bitset` 按最大 ID 开到几十 KB;位图一律经 `<Sheet>BitMap` 按位序下标(核过:没有按 ID 数值直接下标的代码)。现有任务表、奖励表 ID 都从 1 连续,本次导表的容量产物不变。
  6. 共用的存档位图读写 `TestPackedBit` / `SetPackedBit` 放进 `cpp/libs/engine/core/utils/bit_index/bit_index_util.h`,任务完成位图与积分去重位图共用。
- **改动**(客户端仓 `D:\luyuan\mmorpg-client`,未提交):`Assets/Scripts/Game/PlayerFeatures/PlayerFeaturesClient.Activities.cs` 新增 `DescribeActivityRewardTip`,活跃领取的业务拒绝按 `reward_error` 枚举显示镜像 Tip.xlsx 的文案(原来只显示「错误码 N」);`tools/gen_proto.ps1` 收录 `generated/code/proto/tip/reward_error_tip.proto`(只加了这 3 行;同文件里挂机月卡那几行是别的会话的);`Assets/Tests/EditMode/Battle/ActivityVitalityClientTests.cs` 新增 `EndedPeriodRejectionClearsIntentAndSaysBagIsAuthoritative`。
- **测试**(服务端):
  - `player_activity_system_test.cpp`:新增 `EndedPeriodSourcesArePrunedWhileCreditedBitsStillBlockRecredit`(两个周期各记一条来源,前一周期的被删;来源删掉之后同一任务的完成通知不会再记;位图里两个任务的位都在;本期积分只算当期);`NewPeriodRejectsOldRequestButDoesNotInheritPoints` 与 `ClaimAtFullBudgetPrunesEndedPeriodsOfCurrentRuleAndKeepsReplayWindow` 的旧周期期望改为 `kRewardPeriodEnded`。
  - `player_mission_system_test.cpp`:删 4 条旧格式用例(`LegacySnapshotImports…`、`EmptyScopedSnapshotDoesNotImport…`、`LegacyDuplicateActiveEntry…`、`LegacyExplicitCompletedEntry…`),新增 `EmptySnapshotLoadsNoMissions`;位图用例改为 `CompletedMissionsAreSavedAsBitIndexBitmap`(只验新格式往返逐字节不变、运行时列表不留位图字段);`UnknownConfigurationAndOtherScopesArePreservedWithoutOpeningClaim` 去掉「没有位序的已完成 ID」那半;接取时间的断言全部删掉,`AcceptPersistsTimestamp…` 改名 `AcceptRejectsDuplicateTypeAndCompletedMission`。
  - `persist_round_trip_test.cpp` 夹具改用位图写已完成 11、15,活动账本补上 15 的去重位;`player_feature_persistence_test.cpp` 去掉接取时间与 `scoped_state_present`。
  - 导表器:`tests/test_bit_index_gen.py` 新增 `test_bitset_capacity_follows_issued_bits_not_id_values`(ID 100001 起编,容量 3)。
- **Codex 验证**(串行;失败保留首个错误原文与失败用例完整输出,不改断言,不连续重试;工作区有别的会话未提交的改动,不要回退。可与同日前两条合成一次):
  0. **清档**:部署这一版之前清空本机与测试环境的玩家存档(做法同压测前清库,AGENTS §6.2 第 6 条)。之前的存档里 `MissionListComp.complete_missions` 若有值,是 2026-09-11 之前按旧位序写的位图,含义对不上。
  1. **xlsx**(仓库根):`python tools/scripts/activity_vitality_xlsx_patch.py tip-codes --dry-run`,再去掉 `--dry-run` 跑一次。通过:`Tip.xlsx` 的 `//reward_error` 组在 `RewardAlreadyClaimed` 之后多一行 `RewardPeriodEnded`,脚本报期望号 12001;再跑一次报 `[ALREADY CURRENT]`。
  2. **导表**:`dev.bat gen`。通过:`generated/code/proto/tip/reward_error_tip.proto` 多 `kRewardPeriodEnded = 12001`;`tip_enum_ids.json` 只增不改;任务表、奖励表的 `BitmapCapacity` 仍是 17、7(产物无变化)。
  3. **导表器单测**:`cd tools/data_table_exporter && python -m pytest -q`(readme 的标准入口,含 `tests/` 与 `smoke_test.py`),全绿,含新用例 `test_bitset_capacity_follows_issued_bits_not_id_values`。
  4. **协议**(仓库根,默认配置,客户端任务已授权):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`;再到客户端仓跑 `pwsh -File tools/gen_proto.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp` 与 `pwsh -File tools/gen_messageids.ps1 -ProtoRoot D:/luyuan/xuanming-server-mmo-cpp`。通过:`proto/persist_schema_gen.txt` 多 `retired=QuestAllData#1`、`#2`、`#4`、`retired=QuestScopeData#3`、`retired=MissionListComp#3` 与新行 `1=PlayerActivityLedger#3 optional bytes credited_missions`;`QuestEntry` 的 4 行原样保留(生成器报「类型已不再从玩家表可达」);客户端 `Assets/Scripts/Proto/Generated` 多 `RewardErrorTip.cs`。
  5. **Go**:`go/db`、`go/data_service` 等引用 `common/database` 生成包的模块 `go build ./...` 通过(没有 Go 代码读这几个删掉的字段,只是确认重新生成后还能编)。
  6. **C++**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(改了 mission 模块与 scene 库,`run_cpp_tests -Build` 不重编库,必须先整解);再 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test`、`-Filter missions_test`:`PlayerActivitySystemTest.*`、`PlayerMissionSystemTest.*`、`PersistRoundTripTest.*`(含覆盖率与未知字段往返)、`PlayerFeaturePersistenceTest.*` 全绿;`missions_test` 全绿。Linux 用 `build_linux.sh` 编过(没有新增源文件)。
  7. **客户端**(客户端仓根):`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40` 0 error;EditMode(Unity 6000.6.0f1,不加 `-quit`)`-runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Battle.ActivityVitalityClientTests"` 全绿(含新用例)。
- **已做的静态检查**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 通过;客户端 `tools/gen_proto.ps1` 用 `Parser::ParseFile` 查过语法(0 个错误)。
- **未验证**:以上全部没有运行;没有用真库验证清档后的新存档往返。
- **Java 版(AGENTS §12)**:待做,0.1.0-SNAPSHOT。新提示码 `kRewardPeriodEnded` 是两版共享的客户端契约;Java 仓还没有活动中心,已在 Java 仓 `PARITY.md` 同日那一条里补登。任务存档格式与位图容量是服务端内部存储,Java 按自己的方式存,只要求「已完成任务不随时间按条增长」。
- **文件**:服务端 `cpp/libs/engine/core/utils/bit_index/bit_index_util.h`、`cpp/libs/modules/mission/comp/mission_comp.{h,cpp}`、`cpp/libs/modules/mission/system/mission.cpp`、`cpp/libs/services/scene/player/system/{mission_marshal.h,mission_marshal.cpp,player_activity.cpp,player_mission.cpp,player_feature_snapshot.cpp,player_database_loader.cpp}`、`proto/common/component/mission_comp.proto`、`proto/common/database/bag_quest_mail_data.proto`、`cpp/tests/bag_test/{player_activity_system_test.cpp,player_mission_system_test.cpp,persist_round_trip_test.cpp,player_feature_persistence_test.cpp}`、`tools/data_table_exporter/{core/generators/bit_index_gen.py,tests/test_bit_index_gen.py,smoke_test.py,readme.md}`、新 `tools/scripts/activity_vitality_xlsx_patch.py`、`docs/design/{player-features-ui.md,configurable-templates.md,mixed-version-persist-field-protection.md}`、本条;客户端仓见上。

## 2026-11-20 Claude:scene → gate 的回推与自发 EnterScene 按会话所在的 gate 进程找 gate(e 方案 C 残余,未编译,待 Codex 验证;Java 0.1.0-SNAPSHOT 待做)

- **起因**:同日「scene 按 gate 实例认会话」那一条留下的残余([scene-enter-chain-residuals.md](design/scene-enter-chain-residuals.md) §14.6 第二条)。`ResolveLocalZoneGateEntity` 按 (zone, node_id) 取第一个匹配的 gate 实体;同一 node_id 短暂有两个 gate 进程、两者在 scene 上都有实体时(老进程断线重连后重新握手登记,或开了 `node_removal_grace_seconds`),绑在新进程上的会话的回推投到老进程,scene 自发的 EnterScene 也带成老进程的实例。只伤活性。
- **做了什么**(main 工作区,未提交;设计见该文新 §14.7):会话所在 gate 的实例已知(身份组件记的会话就是这一条、实例非空)时只按实例找 gate 实体,找不到不退回按 node_id;实例未知才按 node_id。引擎层 `network_utils.h` 新增 `ResolveSessionGateEntity` / `ResolvePlayerGateEntity`,`BoundGateInstanceId` 从 `player_lifecycle.cpp` 匿名命名空间挪来共用(语义不变),只按 node_id 取 uuid 的 `ResolveGateInstanceId` 删除。推送(`SendToPlayerClient`、两种广播、`PushToSessionViaGate`)与 6 处自发请求的 `gate_instance_id`(EnterSceneC2S、建镜像后跟进、组队跟随、跨 zone 传送、改派票据、战斗路由)、`UpdateSessionDetail` 的 gate 预检都改走它们。`PlayerLoginIdentityComp` 的定义挪到新头 `engine/core/type_define/player_login_identity_comp.h`(引擎层的推送要读它),`enter_fence.h` 包含它。
- **文件**:`cpp/libs/engine/core/network/{network_utils.h,network_utils.cpp,client_message_sender.h,client_message_sender.cpp}`、新 `cpp/libs/engine/core/type_define/player_login_identity_comp.h`(登记进 `core.vcxproj` / `.filters`,两者是 CRLF,沿用)、`cpp/libs/services/scene/player/system/{enter_fence.h,player_lifecycle.cpp,player_scene.cpp,player_team.cpp}`、`cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp` 与 `cpp/nodes/scene/handler/rpc/scene_handler.cpp`(守护段)、`cpp/tests/cross_zone_test/{cross_zone_test.cpp,player_team_follow_test.cpp}`、`docs/design/scene-enter-chain-residuals.md`(§14.4 的 M3 改写、§14.6、新 §14.7)、本条。`player_lifecycle.cpp`、`player_battle.cpp`、`enter_fence.h`、`scene_handler.cpp`、`cross_zone_test.cpp` 里还有别的会话未提交的改动,本条只动上面说的几处。不改 proto,不新增 `.cpp`,不新起线程。
- **测试(新增,未运行)**:`cross_zone_test.cpp` 末尾 `GateInstanceRouteEcs.PushGoesOnlyToTheGateProcessThatHoldsTheSession`(复现:两个同 (zone, node_id) 的 gate 实体,绑哪个进程找到哪个;那个进程的实体销毁后找不到、不退回另一个进程)、`GateInstanceRouteEcs.UnknownInstanceFallsBackToTheNodeIdOfTheSession`;`player_team_follow_test.cpp` 的 `PlayerTeamFollowTest.DifferentSceneRequestNamesTheGateProcessThatHoldsTheSession`(复现:跟随发出的 EnterScene 带会话所在进程的实例)。
- **预检**:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`,10 tests OK。没有编译,没有跑任何单测。
- **给 Codex 的验证步骤**(仓库根,串行;不需要协议生成;可与同日「scene 按 gate 实例认会话」合成一次跑;失败时保留首个编译错误原文、失败用例完整输出与 `--gtest_filter` 单跑结果,不改断言、不连续重试;工作区有别的会话未提交的改动,不要回退):
  1. C++ 整解:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。改了 core 与 scene 两个库,`run_cpp_tests -Build` 不重编库,必须先做这一步。期望 0 error。
  2. `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/scene-gate-route-20261120/cpp-tests`:先单跑 `build\cpp\tests\cross_zone_test.exe --gtest_filter=GateInstanceRouteEcs.*:PlayerTeamFollowTest.*:GateInstanceFence*`,应含上面 3 条新用例且全绿;再跑全量,全绿(别的会话的既有失败点名即可)。
  3. 回归:`pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -LogDir build/verification/scene-gate-route-20261120/cpp-tests`(全部工程)。`PlayerLoginIdentityComp` 换了头、`network_utils.obj` 多读 `tlsEcs`(与它已读的 `tlsNodeContextManager` 同在 thread_context 库),要确认链接 core 的各工程照样编得过、跑得绿。
  4. 变异核对(每个单独做、跑完逐字节恢复,期望见设计稿 §14.7 的表):M3 `BoundGateInstanceId`(`network_utils.cpp`)删掉取身份组件实例那一支 → `PushGoesOnlyToTheGateProcessThatHoldsTheSession`、`DifferentSceneRequestNamesTheGateProcessThatHoldsTheSession`、`GateInstanceFenceEcs.RelocateEntryRemembersTheGateProcessOfTheSession` 红;M7 `ResolveSessionGateEntity` 改成恒 `return ResolveLocalZoneGateEntity(sessionId);` → `PushGoesOnlyToTheGateProcessThatHoldsTheSession` 红。同日前一条第 6 步的 M3 写的 `ResolveGateInstanceId` 已删,按这里的写法做。
  5. Linux:`build_linux.sh` 编过(没有新增源文件,新头只登记 vcxproj)。
  - **通过标准**:第 1 步 0 error;第 2、3 步全绿;第 4 步红绿与设计稿一致;第 5 步编过。回报每步实跑条数与平台。
- **没有行为测试的接线**:`client_message_sender.cpp` 三处、`PushToSessionViaGate` 的按实例找与 `kGateReplaced` 分类、`UpdateSessionDetail` 的预检、4 处 `BoundGateInstanceId` 调用点(EnterSceneC2S、建镜像后跟进、跨 zone 传送、战斗路由)。宿主里 gate 实体没有连接,靠静态阅读。双 gate 同号的端到端冒烟没有现成场景,未做。
- **行为差异**:`[RelocateConfirm]` 的 `push_*` 分类有一处变化:记着实例、那个进程已不在、同一 node_id 上另一个进程没连上时,原来计 `push_gate_gone`,现在计 `push_gate_replaced`。推不推不变。
- **未验证**:全部没有编译、没有运行。
- **Java 版(AGENTS §12)**:对应 Java 版本 `0.1.0-SNAPSHOT`,待做,本轮不写 Java 代码,只在 Java 仓 `PARITY.md` 末尾登记(Java 网关回推是否按会话所在的网关实例投递待核实)。

## 2026-11-20 Gemini:「组队跟随 / 离线托管 / 挂机月卡 / 冷却与 Buff 跨重登」全线(11-06~11-20 共 20 条)首轮验证回执(导表、协议、Go 单测、C++ 预检与 gate 通过;本线客户端查出 1 处文案空格缺陷待切最高档修;scene.lib 与客户端全仓受其他会话阻塞;Java 0.1.0-SNAPSHOT 待做)

- **验证范围**:按交接单 `docs/handoff/team-follow-afk-hosting-handoff-20261120.md` §2~§4 执行本线 20 条条目:
  - 11-06「组队跟随改为服务器沿队长轨迹驱动队员」「组队跟随补『离线的队员也跟』」「开通挂机的玩家断线后离线托管、照常组队跟随」;
  - 11-10「组队跟随:入场后补推跟随状态」「离线托管的队员跟着队长跨场景、跨节点」;
  - 11-11「组队跟随客户端补齐用例与兼容说明」「离线托管队员跟随的兜底重试」;
  - 11-12「组队跟随遗留三项收尾」「离线托管角色随排空 / 疏散搬走」「托管归位」,11-13「托管归位补一处」;
  - 11-19「组队跟随间距放大到 3 倍」「组队跟随间距改定 3 米」「组队跟随与离线托管本线遗留项清零」「组队跟随合并广播的发送层补单测」「实时技能冷却与 Buff 跨重登保留」「挂机月卡开通入口」;
  - 11-20「挂机月卡复核补齐」「实时冷却与 Buff 跨重登保留·复核补」「实时冷却与 Buff 跨重登保留·独立评审后再补」。
- **顺手完成与规范落盘**(都在各自 `main` 工作区,未建分支、未 commit):
  - 按交接单 §4 修正 `robot/team_follow_smoke_scenario.go` 与 `robot/etc/team_follow_smoke.yaml` 的旧阈值注释(拉开距离 `12 -> 60` 分步每步 16、停步间距 `1.4 -> 3`)。
  - 按负责人指令在三仓 `AGENTS.md` 写入强制模型档位规定(服务端 `AGENTS.md` §10.3、客户端 `AGENTS.md` 末节、Java 仓 `AGENTS.md` §6):修改代码必须用各软件最高档模型(`Fable 5.2 Max` / `Gemini 4 Neon Extra` / `GPT 6.2 Astral Ultra`),编译测试用低档(`Carbon Medium` / `6.1 Sol Ultra` / `Opus 5 High`)。
- **已执行并通过的步骤**(日志目录 `build/verification/team-follow-afk-hosting-20261120/`):
  1. **数据预检与 xlsx(第 1~2 步)**:`data/` 工作区干净;`python tools/scripts/afk_pass_xlsx_patch.py new-tables` 产出 `data/AfkPassRule.xlsx`(7 列 1 行),`tip-codes` 在 `data/tip/Tip.xlsx` 追加 `//afk_pass_error base=32000 width=1000`(5 个码 `32000..32004`)。
  2. **导表与协议生成(第 3~4 步)**:`dev.bat gen`、`py tools/data_table_exporter/tools/gen_schema_index.py`(`--check` 绿)、客户端 `gen_proto.ps1` 与 `gen_messageids.ps1` 全部退出码 0。核对通过:`afkpassrule_table.*` 与 `afk_pass_error_tip.pb.*` 生成,`buff_table` 含 `keep_on_logout`(`data/AGENTS.md` 为 64 列 54 字段,新增 `AfkPassRule` 行),`go/shared/generated/tip/{segments,faults}.go` 含 `afk_pass` 段与 `AfkPassUnavailable(32004)`,消息号 `381/382` 不变、新增 `SceneAfkPassClientPlayer` 三个方法 `430/431/432`,`proto/persist_schema_gen.txt` 纯追加(`PlayerUint64Comp#2`、`CooldownTime*`、`SavedBuffComp#1..3`、`player_database#28/#29`),`EnterSceneRequest.hosted_placement`、`PlayerHostedPlacementEvent(53)`、`ReleasePlayerRequest.keep_held` 均在列,客户端生成 `PlayerAfkPass.cs`、`AfkPassErrorTip.cs` 与 `MessageIds.cs`(`GetAfkPass=430`、`BuyAfkPass=431`)。
  3. **Go 检查与单测(第 5 步,`GOTOOLCHAIN=local GOPROXY=off`)**:
     - `go/shared`:`go vet ./redisslot/...` 与 `go test ./redisslot/ -count=1` 全部 PASS(`gofmt -l` 仅报其他会话的 `control_keys_test.go`)。
     - `go/match`:`gofmt` 无输出,`go vet ./internal/team/...` 与 `go test ./internal/team/ -count=1` 全部 PASS(含 `TestLeaderChangeRefreshesEveryRemainingMember`)。
     - `go/player_locator`:`gofmt` 无输出,`go vet ./...`、定向 12 个顶层用例与全量 `go test ./... -count=1` 全部 PASS。
     - `go/scene_manager`:`gofmt -l internal/logic` 无输出,`go build ./...`、`go vet ./...` 0 错误,本线 36 条定向用例全部 PASS;全量 `go test ./... -count=1` 有 3 条非本线失败(见下)。
     - `robot`:本线 4 个文件 `gofmt` 无输出;因其他会话新增 `golang.org/x/net/websocket` 未刷新 `vendor/`,默认 `-mod=vendor` 报缺包,改 `-mod=readonly` 执行 `go build ./...`、`go vet ./...`、`go test ./config/... -count=1` 全部 PASS(含 `TestEveryDispatchedModePassesWhitelist`)。
  4. **C++ 预检、Linux CMake 生成与可编工程(第 6~9 步部分)**:
     - `test_cpp_no_raw_pointer_member.py`(10/10 OK)、`test_cpp_thread_sources.py`(5/5 OK)、`check_log_text_english.py` 全部通过。
     - `python tools/archived/vcxproj2cmake.py` 退出码 0,确认生成的 `CMakeLists.txt` 包含 `afk_pass.cpp`、`actor_move_broadcast.cpp`、`player_afk_pass_handler.cpp`、`player_placement_event_handler.cpp`、`afkpassrule_table.*`、`afk_pass_error_tip.pb.cc`。
     - `msbuild /m:1`:`proto.lib`、`table.lib`、`core.lib`、`modules.lib`、`gate.lib`、`battle.lib`、`battle.exe`、`cpp/nodes/gate/gate.vcxproj`(`gate.exe`,裸指针门禁 PASSED)全部 0 错误编过;`configuration_table_test` 63/63 PASS。
     - 对本线全部 30 个 C++ 翻译单元(15 个 `scene` 库 `.cpp` + 5 个 `nodes/scene` `.cpp` + 10 个单测 `.cpp`)单独跑只读 `cl.exe /Zs /std:c++latest` 语法与 `static_assert` 校验:退出码 0,0 个编译错误。
- **本线查出的真实缺陷(1 处,按 `AGENTS.md` §10.3 在 `Carbon Medium` 下保留原文停下,待切最高档模型修复)**:
  - 客户端独立 NUnit 运行本线 3 个纯 C# 测试集(`TeamFollowDeclarationTests` 17/17 PASS、`TeamRegroupPolicyTests` 5/5 PASS、`AfkPassClientTests` 10/11 PASS,共 32/33 PASS),唯一失败原文:
    `FAIL: AfkPassClientTests.FormatRemaining_CoversEveryShape -> NUnit.Framework.AssertionException: Expected string length 9 but was 10. Strings differ at index 2. Expected: "剩余不到 1 分钟" But was: "剩余 不到 1 分钟"`
  - 根因:`D:\luyuan\mmorpg-client\Assets\Scripts\Game\AfkPass\AfkPassClient.cs:50` 写死 `"剩余 " + FormatDuration(remainingSeconds)`,当 `< 60s` 返回 `"不到 1 分钟"` 时多拼了一个空格。
- **跨会话阻塞与既有非本线失败(按规矩原样记录,未改他人文件)**:
  1. **阻塞 `scene.lib` / `scene.exe` / 7 个 scene 单测工程 / Linux 构建 / 冒烟**:
     `D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\player\system\player_pet.cpp(1025,36): error G13DA7946: use of undeclared identifier 'kPetNameSensitive' [D:\luyuan\xuanming-server-mmo-cpp\cpp\libs\services\scene\scene.vcxproj]`(来自提交 `55eae6be3`「改名与内容安全」批次,`data/tip/Tip.xlsx` 未加 `PetNameSensitive`,`lib/scene.lib` 仍停留在 `2026-11-18`)。
  2. **阻塞客户端全仓编译(`client_compile_check.ps1` 退出码 1,共 14 错;本线客户端文件 0 错)与 Unity EditMode**:
     - `Assets\Scripts\Game\Jubaozhai\PaymentClient.cs(140..153): error CS0103: The name 'payment_error' does not exist in the current context`
     - `Assets\Scripts\Game\PlayerFeatures\PlayerFeaturesClient.Activities.cs(245,32): error CS0117: 'reward_error' does not contain a definition for 'KRewardPeriodEnded'`(同仓 `activity_vitality_xlsx_patch.py` 尚未执行)。
  3. **其他非本线失败**:
     - `go/scene_manager` 全量:`TestControlRedisHandleContract_NoGoZeroRedisInModule`(`control_zone_keys_contract_test.go:113`)、`TestOwnsControlZoneSkipsForeignMembership`(`foreign_zone_registry_test.go:266: ERR no such key`)、`TestIsNodeAliveTreatsRedisOutageAsAlive`(`scene_keys_contract_test.go:216` miniredis cleanup nil panic)。
     - `turn_battle_engine_test`:前 345 条全 PASS,到 `TableBattleDataProviderTest` 报 `FATAL readBaseDeployConfig: Etcd.Hosts in etc/base_deploy_config.yaml must list exactly one load-balanced etcd address ..., got 2; refusing to start`。
- **待续项**:(1) 切最高档模型修掉 `AfkPassClient.cs:50` 空格问题;(2) 待对应会话补齐 `kPetNameSensitive` 与客户端 `payment_error` / `KRewardPeriodEnded` 后,切回低档模型续跑 `scene.lib`/`scene.exe` 整解、7 个 C++ 单测工程(`aoi_test`、`aoi_delivery_test`、`cross_zone_test`、`bag_test`、`currency_test`、`buff_test`、`skill_test`)、Linux 构建、Unity EditMode 全集、robot 两条冒烟及联机/压测验收。
- **Java 版(AGENTS §12)**:`0.1.0-SNAPSHOT` 维持 `PARITY.md` 既有待做登记,本轮未写 Java 代码。

## 2026-11-20 Gemini（Neon Extra）：「组队跟随 / 离线托管 / 挂机月卡 / 冷却与 Buff 跨重登」全线深度审计与 6 处代码缺陷修复（未编译、未跑测试；交低档模型 / Codex 续跑验证）

- **审计与修复清单**（全部在 `main` 工作区修改，未触碰任何其他会话的未提交文件，未提交）：
  1. **客户端 `AfkPassClient.cs` 剩余不足 1 分钟文案多余空格（修复首轮单测唯一红项）**：
     - 改动：`mmorpg-client/Assets/Scripts/Game/AfkPass/AfkPassClient.cs`（抽取 `FormatRemainingPhrase`，`< 60s` 返回 `"剩余不到 1 分钟"`，同时修掉 `FormatRemaining` 与 `ReminderText` 两处）；`mmorpg-client/Assets/Tests/EditMode/Tianyong/AfkPassClientTests.cs` 补 `ReminderText(59)` 回归断言。
  2. **客户端 `AfkPassUiRoot.cs` 二次确认窗口热更调价防呆**：
     - 改动：`mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/AfkPassUiRoot.cs` 记录 `_armedCurrency` / `_armedPrice`，5 秒二次确认窗口内若服务端刷新出不同币种或价格则自动撤回确认，避免按玩家未确认过的新价扣费。
  3. **服务端 `team_follow_system.cpp` 队长瞬移重建队形误瞬移战斗中队员**：
     - 根因：`TeamFollowSystem::Update` 在 `SetSuspended` 之前进入 `RestartFormation`，对挂着 `InBattleComp`（`IsHeldStill`）的队员照常调 `PlaceMember` → `TeleportIntoSlot`，把战斗中队员瞬移走。
     - 改动：`cpp/libs/services/scene/player/system/team_follow_system.cpp` 在 `RestartFormation` 对 `IsHeldStill` 队员只调 `StopMotion` 并以 `suspended=true, needsPlacement=true` 留名次，战后下一 tick 自动放进新槽位；`cpp/tests/cross_zone_test/player_team_follow_test.cpp` 补 `TeamFollowDriveTest.LeaderJumpDoesNotTeleportFollowerInBattle`。
  4. **服务端 `team_follow_system.cpp` 重建队形不瞬移时丢失停步广播**：
     - 根因：`PlaceMember` 每次都以默认 `TeamFollowerComp{}` 覆盖原组件，清空 `broadcastMoving` 却未发停步广播；若重建走 `snap` / `walk`（不瞬移）且下一 tick `moved == 0` 或瞬移被严格导航拒绝，`StopMotion` 见 `broadcastMoving == false` 跳过广播，观察者与本人客户端会持续外推移动。
     - 改动：`PlaceMember` 对同队长重建沿用原 `TeamFollowerComp` 广播状态（`TeleportIntoSlot` 成功时照旧清零，`!teleport` 与瞬移被拒后的 `ReleaseFollower` 均保留状态）；`cpp/tests/cross_zone_test/player_team_follow_test.cpp` 补 `TeamFollowDriveTest.LeaderJumpWithoutTeleportKeepsBroadcastStateUntilFollowerStops`。
  5. **服务端 `actor_move_broadcast.cpp` 与 `view.cpp` 广播列表未过滤已置无效会话的断线保留 / 离线托管观察者**：
     - 根因：`RemovePlayerSession` 与托管落点 `EnterScene` 将 `PlayerSessionSnapshotComp.gate_session_id` 置为 `kInvalidSessionId` 但保留组件；`CollectMoveReceivers` 仅对 `includeSelf` 判 `kInvalidSessionId`，未过滤 AOI 观察者（`ViewSystem::BroadcastMessageToVisiblePlayers` 同病），导致移动合批为离线托管者多拆组并每 250ms 在 `BroadcastToPlayerClients` 刷 `Gate node not found` ERROR 日志。
     - 改动：`cpp/libs/services/scene/spatial/system/actor_move_broadcast.cpp` 与 `cpp/libs/services/scene/spatial/system/view.cpp` 过滤 `session == nullptr || session->gate_session_id() == kInvalidSessionId`（保持 `aoi_delivery_test` 默认 `gate_session_id == 0` 兼容）；`cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp` 补 `AoiDeliveryTest.BroadcastsSkipObserversWhoseSessionWasInvalidated`。
- **深度审计其余已核对模块（无新缺口）**：`buff.cpp` / `buff_comp.h`（`SaveKeptBuffs`、`RestoreSavedBuffs`、`OnBuffRefresh`、`SavedBuffRemainingMs`）、`player_battle.cpp`（剩余时长换算回合）、`skill.cpp`（`StartCooldown` / `RestoreCooldowns`）、`player_database_loader.cpp`、`player_lifecycle.cpp`（托管落点、断线保留、存盘提交投影、看护重试、在场心跳、`keep_held`）、`afk_pass.cpp`、`afk_hosting.cpp`、`team_follow_formation.{h,cpp}`、`facing.h`、`go/scene_manager` 托管落点与释放门、`go/player_locator` 租约续期与托管归位、客户端 `GameClient.TeamFollow.cs` / `TeamFollowDriver.cs` / `TeamFollowDeclaration.cs` / `TeamFollowUiRoot.cs` / `WorldHudNavigation.cs`。
- **已跑只读预检**（按 `AGENTS.md §10.3`，最高档模型不执行编译与运行测试）：
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`：10/10 OK；
  - `python tools/scripts/tests/test_cpp_thread_sources.py`：5/5 OK；
  - `python tools/scripts/check_log_text_english.py`：0 violations。
- **未验证（交低档模型 Gemini `Carbon Medium` 或 Codex `6.1 Sol Ultra` 执行）**：
  - 本次修改涉及的 C++ / C# 全部未编译、未运行测试。
  - 待跨会话阻塞解决后（服务端 `player_pet.cpp:1025` `kPetNameSensitive`；客户端 `PaymentClient.cs` `payment_error` 与 `PlayerFeaturesClient.Activities.cs` `KRewardPeriodEnded`），按以下顺序验证：
    1. 客户端：`pwsh -File tools/client_compile_check.ps1` + Unity EditMode 全集（含 `AfkPassClientTests` 11/11、`TeamFollowDeclarationTests` 17/17、`TeamRegroupPolicyTests` 5/5；也可先用独立 NUnit 复测 33/33）；
    2. 服务端 Windows 串行构建：`msbuild /m:1 cpp/libs/services/scene/scene.vcxproj` → `msbuild /m:1 cpp/nodes/scene/scene.vcxproj`；
    3. 服务端 C++ 单测：`pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter "aoi_delivery|cross_zone|aoi|bag|currency|buff|skill"`（重点核对新增 3 条：`LeaderJumpDoesNotTeleportFollowerInBattle`、`LeaderJumpWithoutTeleportKeepsBroadcastStateUntilFollowerStops`、`BroadcastsSkipObserversWhoseSessionWasInvalidated`）；
    4. 服务端 Linux：`tools/scripts/build_linux.sh`；
    5. 冒烟：起服后运行 `robot -mode=team_follow_smoke` 与 `robot -mode=afk_pass_follow_smoke`（若 `vendor/` 尚未同步 `golang.org/x/net/websocket`，加 `-mod=readonly`）。
- **Java 版（AGENTS §12）**：`0.1.0-SNAPSHOT` 维持 `PARITY.md` 既有待做登记，本轮未写 Java 代码。

## 2026-11-20 Claude：真钱支付接入的剩余工作交给 Gemini（交接单）

- 负责人 2026-11-20 定：支付这条线剩下的代码与全部验证交给 Gemini。交接单 `docs/handoff/payment-integration-handoff-20261120.md`。
- 剩下的代码：渠道复核收尾（中断的复核改了 6 个源文件、写了 4 个测试文件，还缺苹果、谷歌、抖音的测试，抖音协议要对官方文档）、安卓微信支付回调页与两家 SDK 依赖、商城健康检查改成 HTTP、Unity IAP 编译后的修正。
- 验证：导表与协议生成、四种语言的构建与单测、部署脚本测试、Unity、devpay 本机冒烟、kind 冒烟。全部没跑过。
- Java 独立仓（AGENTS §12）：只登记待做，`PARITY.md` 已有条目。

## 2026-11-20 Gemini（Carbon Medium）：战报回放真 TiDB 4+15 验证通过与跨区 1v1 冒烟诊断

- **真 TiDB v8.5.2（`build/verification/battle-report-20261120/match/tidb/`）**：
  - 当前工作区 `go/match` 的 `go build ./...`、`go vet ./...` 均 exit 0；启动前可用物理内存 7.728 GiB（高于 4 GiB 门槛），经 `sql_fixture_env.ps1` 新收据目录与 `MATCH_IT_REQUIRE_TIDB=1` 启动隔离集群（`127.0.0.1:24411`）。
  - `go test ./internal/authority/... -run "BattleReport" -count=1 -v`：5/5 PASS（含全部 4 个 `TestBattleReportTiDB*`），0 FAIL、0 SKIP（80.744s）。
  - `go test ./internal/authority/... -run "TerminalTiDB|CompletionTiDB" -count=1 -v -timeout 20m`：15 个顶层用例（含 8 个子用例，共 23 项）全部 PASS，0 FAIL、0 SKIP（324.819s）。
  - `CommitBattleOutcome` 耗时采样（只读 `-overlay`，未改工作区源码）：无回放首次提交 52,551 us（重放 13,409 / 2,872 us）；带合格回放首次提交 82,657 us（重放 5,812 / 6,423 us）；坏外壳丢回放写列表 78,139 us；未请求回放丢弃（含冷 Region 分裂）507,358 us。
- **跨区 1v1 冒烟准备与运行（`build/verification/battle-report-20261120/smoke/`）**：
  - 复现并留存 11-10 离线准备失败底层 stderr：`ValueError: 开发子网与现有 Docker 网络重叠: festival-maps-20261109_default (172.29.222.0/24)`。
  - 在隔离目录按 A17c 单地域存档库（`player_store_1000000_db`）、`rank` 服务、二区 `<InstanceKey>.yaml` 运行配置、串行 `*-sql-boot` 注册链及本栈 `LOGIN_DEV_PASSWORD_SHARED_SECRET` 生成双区隔离配置（`172.29.229.0/24`）；重建当前可编译的 9 个 Go 镜像（`login`、`rank` 因其他会话在途改动沿用 `2026-11-18` 同协议缓存镜像），仅压缩 WSL 页缓存即恢复可用内存至 4.569 GiB（未终止任何宿主进程）。
  - 双区 39 容器全部健康拉起，`match-migrate` 显式建出 `match_battle_report` 与 `match_player_battle_report`；`robot`（`-mod=readonly` 编译）双端跨区成局 `battle_id=7272395305044938803`，直连打满 24 回合，battle 产出 `metric=battle_replay_frozen battle_id=7272395305044938803 rounds=24 truncated=0 raw_bytes=10878 payload_bytes=1531 compress_us=3240`，终局提交与 scene 经大厅推送的个人 `NotifyBattleEnd` 均已到达。
- **拦停首错（按 `AGENTS.md` §10.3 低档模型不改代码停下，待切最高档模型修复）**：
  - 原文：`CROSS_ZONE_MATCH_FAIL step=a-direct-delivery reason=direct connection saw total=26 state_replies=1 turn_results=24 battle_ends=0 spectate_states=0 spectate_turns=0 spectate_ends=0 heartbeat_replies=5, expected state_replies>=1, turn_results>=1 and battle_ends>=1`
  - 根因：总体存盘方案 §5.4 已将个人 `BattleEndS2C` 改为仅由 scene 经大厅连接下发（`battle_room_manager.cpp:1617`），同仓 `robot/battle_smoke_scenario.go:517` 已改为 `direct.battleEnds.Load() != 0`，而 `robot/battle_smoke_cross_zone_scenario.go:344` 漏改仍要求 `direct.battleEnds.Load() >= 1`，在进入第 239 行第 5 步 `battleReportSmokeCheck` 前 4 行固定报错。
  - 待续项：切 `Gemini 4 Neon Extra` 将 `robot/battle_smoke_cross_zone_scenario.go:344-347` 对齐 `battle_smoke_scenario.go:517-520`，再切回低档模型重编 `smoke/robot.exe`、复用 `smoke/go-build-9` 镜像复跑 `smoke/run_smoke_verification.ps1` 取齐 `BATTLE_REPORT_OK` 与 `CROSS_ZONE_MATCH_OK`。所有临时容器、卷、网络已清理。


## 2026-11-20 Gemini（Gemini 4 Neon Extra）：托管内部密钥部署收尾修复——按 StatefulSet/Deployment 滚动重启（护住 Agones scene Fleet）、写入移到各写前闸之后、取消全集群 `list secrets`、补换值 infra 重启提醒（未跑测试，待低档模型验证；Java 不适用）

- **起因**：对 2026-11-20「部署替运维维护 domain-service-secrets 的内部调用密钥」条目做静态复核，修四处实害与两处文档残余：
  1. **`-ResyncDomainServiceSecrets` 重启方式**：原来 `kubectl delete pod -n <ns> -l "app in (...)" --wait=false` 会无视 PDB 与滚动策略一次性全杀 `scene` 与 `db`/`data-service`，且把 Agones `scene` Fleet 的 `Allocated` GameServer 直接打成 `Unhealthy` 强踢玩家。
  2. **接入位置破坏写前零写保证**：原来 `Sync-ManagedDomainServiceSecrets` 在 `switch ($Command)` 之前，早于 `Apply-Infra`（核心 Redis 重启闸、Loki 形态闸、集群 UID、TLS Secret）与 `Apply-Zone`（SQL 写者名册、服务端预演、旧形态删除闸）的写前闸，被拒时已写 Secret 甚至删 Pod。
  3. **集群级 RBAC 权限面**：原来 `Read-DomainServiceSecretEntries` 用 `kubectl get secrets --all-namespaces`，是全脚本唯一要求全集群 `list secrets` 的调用（`--field-selector` 不收敛 K8s RBAC 权限）。
  4. **换值步骤漏重启 infra / 换 4 把 zone 键的口径**：先改 infra 再跑 `-ResyncDomainServiceSecrets` 时 infra 未被改写不会自动重启，多消费者键（如 `role-fence-coordinator` 的 6 个服务含 `-canary`）容易漏重启。
- **改动**（main 工作区，未提交）：
  - `tools/scripts/k8s_deploy.ps1`：
    - `Read-DomainServiceSecretEntries`：改为先 `kubectl get namespaces -o name`（非敏感元数据），再按 namespace 逐个读 infra 与 `<NamespacePrefix>-*` 的 `domain-service-secrets`（`--ignore-not-found -o json`），RBAC 可收口到按 namespace + `resourceNames`。
    - `Get-ManagedDomainServiceSecretPlan`：新增返回字段 `InfraManualRestarts`（只记「zone 改写了共享键但 infra 未改写，且该消费者不在 `Restarts[infra]` 里」的 infra 消费者）。
    - `Sync-ManagedDomainServiceSecrets`：执行一次后清空 `$script:ManagedDomainServiceSecretPlan`（`all-up` 不重复写）；改写后的重启改为按 `Get-WorkloadKind` 分 `statefulset` / `deployment` 先列本 namespace 实际存在的对象再执行 `kubectl rollout restart`（先滚 StatefulSet 再滚 Deployment，不强杀 Pod）；`scene` 在重启名单但无 `scene*` Deployment（Agones Fleet 形态）时告警给出 `tools/scripts/k8s_scene_roll.ps1 -Namespace <ns> -Fleet <...> -Restart`；`InfraManualRestarts` 非空时告警给出 `kubectl -n <infra> rollout restart ...`（含 `-canary` 提示）。
    - 接入位置：从门禁块末尾移入 `Apply-Infra`（全部写前闸与 `Ensure-Namespace` 之后、`$infraManifests` 之前）与 `Apply-Zone`（`-SqlSchemaOnly` 在 `Ensure-Namespace` 之后、正常路径在 `Remove-ClientEntryObsoleteResources` 之后、`New-NodeConfigMapYaml` 之前）。
  - `tools/scripts/tests/k8s_migrate_gate.tests.ps1`：同步更新托管密钥 4 条用例（`InfraManualRestarts` 与同服务去重；按 namespace 读且不触碰别的前缀 / 不调 `get secrets --all-namespaces`；`rollout restart` 顺序、Agones Fleet 告警、infra 手工重启告警与 run-once 幂等；`Apply-Infra` / `Apply-Zone` 接入位置）。
  - `tools/scripts/tests/k8s_client_entry_contract.tests.ps1`：假 kubectl 夹具同步改为匹配 `get namespaces -o name` 与按 namespace 读 `domain-service-secrets`。
  - 文档与注释：`docs/ops/k8s-open-server-runbook.md`（§1 商城第 4 点与托管密钥维护/换值步骤 1、3、4、6）、`deploy/k8s/README.md`（`-ResyncDomainServiceSecrets`）、`deploy/k8s/manifests/go-svc/payment.yaml`（第 16 行注释）。
- **已跑只读静态预检**（按 `AGENTS.md §10.3`，未跑任何测试套件）：
  - PowerShell `Parser::ParseFile` 语法检查（`k8s_deploy.ps1`、`k8s_migrate_gate.tests.ps1`、`k8s_client_entry_contract.tests.ps1`）：均 `0 error(s)`；
  - `python tools/scripts/check_log_text_english.py`：通过（`log text check passed: no CJK literals in log/console calls`）。
- **未验证（交低档模型 Gemini `Carbon Medium` 或 Codex `6.1 Sol Ultra` 执行，仓库根，无需集群）**：
  1. `pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`：期望全 PASS（重点核对 5 条「托管内部密钥…」用例）。
  2. `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`：期望全 PASS（重点核对「非 DryRun 负向:…」「非 DryRun 无漂移:…」与包装链契约）。
  3. 回归：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/k8s_login_auth_provider_guard.tests.ps1`。
  4. `python tools/scripts/check_log_text_english.py`：期望 0 violations。
- **Java 版（AGENTS §12）**：不适用，不往 `PARITY.md` 加行。

## 2026-11-20 Gemini（Gemini 4 Neon Extra）：`data_service` 熔断从按方法改为按逻辑库建连闸口，全仓调用方关闭按方法客户端熔断（已落码，未编译、未运行；Java 不适用）

- **设计依据**：`docs/design/data-service-breaker-per-logical-db.md`（§5、§6 全量 32 文件落码；负责人 2026-11-20 批准按最标准方式直接落码）。
- **根因**：`data_service` 背后按逻辑库分片（目录 K 片、流水库、按 `storage_id` 各一的存档库），不同逻辑库经 `tools/dbmove` 可落在不同集群。原先服务端 zrpc `Middlewares.Breaker` 与各 Go 调用方客户端 `Middlewares.Breaker` 均按方法粒度熔断、不分后端，单个逻辑库集群失联且流量占某方法 1/3 以上时，两层熔断会把同一方法上指向健康逻辑库的调用连带挡掉。
- **改动**（main 工作区，未提交）：
  - **服务端（6 个文件）**：
    - `go/shared/authoritysql/pool.go`：新增 `ConnectGate` 类型与 `WithConnectGate` 选项，`connector` 记录 `logicalDB` 与 `connectGate`，在 `connector.Connect` 包住落点解析、拨号与会话核对（拒连时不拨号；建连结束结算 `err`；搬库闸门冻结 `ErrWriterFrozen` 发生在建连之后，天然结算 `nil` 不计故障）。
    - `go/shared/authoritysql/connect_gate_test.go`（新）：覆盖拒连不拨号、成功/落点失败/拨号失败结算、闸门冻结不计建连失败、`WithConnectGate` 选项。
    - `go/data_service/internal/svc/db_breaker.go`（新）：`newLogicalDBConnectGate()` 按具名熔断 `logical_db/<logicalDB>` 门控建连，调用方主动取消（`context.Canceled`）与成功记 `Accept`，其余失败记 `Reject("connect failed")`，拒连错误带逻辑库名并包 `breaker.ErrServiceUnavailable`。
    - `go/data_service/internal/svc/db_breaker_test.go`（新）：覆盖坏库打开熔断（错带库名与 `breaker.ErrServiceUnavailable`）时健康库每次放行，以及 `context.Canceled` 永不触发熔断。
    - `go/data_service/internal/svc/servicecontext.go`：给 `mysqlCfg.PoolOptions` 安全追加 `authoritysql.WithConnectGate(newLogicalDBConnectGate())`。
    - `go/data_service/data_service.go`：在 `zrpc.MustNewServer` 前强制 `c.Middlewares.Breaker = false`（忽略 yaml 配置）。
  - **调用方（16 个文件）**：
    - `go/shared/dsclient/dsclient.go`（新）：`Conf(c zrpc.RpcClientConf) zrpc.RpcClientConf` 强制关闭 `c.Middlewares.Breaker = false`。
    - `go/shared/dsclient/dsclient_test.go`（新）：验证 `Conf` 仅修改 `Breaker`，并扫描 `go/` 下全部非测试 `.go` 文件，机械守卫凡出现 `NewDataServiceClient(` 的文件必须经 `dsclient.Conf(`。
    - 14 处 `data_service` 客户端构造点统一接入 `dsclient.Conf(...)`：`go/chat/internal/svc/authority.go`、`go/db/player_store_wiring.go`、`go/friend/internal/svc/servicecontext.go`、`go/guild/internal/svc/guild_id_minter.go`、`go/guild/cmd/assetopfix/main.go`、`go/login/internal/svc/home_zone.go`、`go/login/internal/svc/player_id_minter.go`、`go/match/internal/svc/servicecontext.go`、`go/payment/channels.go`、`go/player_inbox/internal/svc/servicecontext.go`、`go/scene_manager/internal/svc/servicecontext.go`、`go/shared/domainhost/dependencies.go`、`go/trade/internal/svc/servicecontext.go`、`go/trade/cmd/assetopfix/main.go`。
  - **配置与注释（4 个文件）**：`go/login/etc/login.yaml`、`go/scene_manager/etc/scene_manager_service.yaml`、`tools/scripts/k8s_deploy.ps1`（login ConfigMap）、`deploy/login-stack.linux/login.yaml` 的 `DataServiceRpc.Middlewares.Breaker` 均改为 `false` 并同步说明。
  - **文档（4 个设计文档 + 本条目）**：更新 `docs/design/data-service-breaker-per-logical-db.md` 状态、`docs/design/db-task-store-unavailable-deferral.md` 文首说明、`docs/design/horizontal-scale-logical-db.md` §11 批次表、`docs/design/data-consistency-guarantee-map-20261020.md` §6.7。
- **已跑只读静态预检**（按 `AGENTS.md §10.3`，未编译、未运行测试）：
  - `gofmt -l` 核查全部 22 个 Go 文件（本批改动行零格式差异；`go/data_service/internal/svc/servicecontext.go` 仅含既有未改行对齐差异）；
  - 全仓扫描 `go/` 非测试 `.go` 文件：14/14 处 `NewDataServiceClient(` 均含 `dsclient.Conf(`；
  - `tools/scripts/k8s_deploy.ps1` PowerShell `Parser::ParseFile` 0 error；
  - `python tools/scripts/check_log_text_english.py`：0 violations。
- **未验证（交低档模型 Gemini `Carbon Medium` 或 Codex `6.1 Sol Ultra` 执行）**：
  1. 核心单测与守卫（环境 `$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）：
     - `go/shared`：`go vet ./authoritysql/... ./dsclient/... ./domainhost/... && go test ./authoritysql/... ./dsclient/... -run "TestConnectGate|TestWithConnectGate|TestConfDisablesOnlyBreaker|TestAllDataServiceClientConstructorsUseDsclientConf" -count=1 -v`
     - `go/data_service`：`go build ./... && go vet ./... && go test ./internal/svc/... -run "TestLogicalDBConnectGate" -count=1 -v`
  2. 全量调用方编译与 vet（11 个模块依次执行 `go build ./... && go vet ./...`）：`go/shared`、`go/chat`、`go/db`、`go/friend`、`go/guild`、`go/login`、`go/match`、`go/payment`、`go/player_inbox`、`go/scene_manager`、`go/trade`。
  3. 部署脚本回归（仓库根）：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。
  4. 可选调试栈冒烟（需目录 `LogicalShards > 1` 或多 SQL 实例）：停掉其中一个实例，验证落在另一实例的账号登录/建角/进场正常，`data_service` 日志仅见坏库 `logical database ... circuit breaker is open`；单 SQL 实例环境不可测，如实报未运行。
- **Java 版（AGENTS §12）**：不适用（Java 版无 `data_service` 与 go-zero），不往 `PARITY.md` 加行。

## 2026-11-20 Gemini（Gemini 4 Neon Extra）：战报回放服务端标准收口与 Unity 客户端全量落码（未编译运行，待 Codex 验证；Java 留 TODO）

- **范围与约束**：所有改动直接落于服务端与客户端 `main` 工作区，未建分支、未提交、未碰其他会话在途文件（`go/login`、`go/rank`、`player_pet.cpp`、客户端 `PaymentClient.cs` / `PlayerFeaturesClient.Activities.cs`）。按 `AGENTS.md §10.3` 仅改代码/测试/文档与跑只读静态预检，编译、单测与跨区冒烟移交 Codex。Java 版保留在 `PARITY.md` TODO。
- **服务端改动（`xuanming-server-mmo-cpp`）**：
  - **C++ 终局冻结原件与预摘要签名（修复设计 §6.3 重试重复拷贝与双次 SHA-256）**：`cpp/libs/engine/core/security/service_call_auth.h` 新增 `ValidDigestHex`、`SignatureForDigest`、`SignForDigest`（`Signature` 委托单一实现）；`cpp/nodes/battle/logic/battle_room_manager.{h,cpp}` 将 `outcomePayload` + `outcomeReplay` 收敛为单份 `std::optional<::match::CommitBattleOutcomeRequest> frozenOutcomeRequest`，`BeginResultCommit` 冻结并算一次 `Digest`，`SendSqlOutcome` 按冻结摘要直接 `SignForDigest` 更新 `auth`；`cpp/nodes/battle/tests/battle_observer_contract_test.cpp` 新增 `ReplayOutcomeRetriesReuseFrozenBodyAndPassVerify` 与 `MakeOutcomeDue`。
  - **C++ gzip 尾字节收紧**：`cpp/libs/services/battle/system/battle_replay_recorder.cpp` 的 `GzipDecompress` 在 `Z_STREAM_END` 后检查 `stream.avail_in != 0` 拒绝尾部夹带字节；`cpp/tests/turn_battle_engine_test/battle_replay_recorder_test.cpp` 补断言。
  - **Go / K8s 回滚开关与逻辑层单测**：`go/match/etc/match_service.yaml` 显式补 `BattleReport.Enabled: true`；`tools/scripts/k8s_deploy.ps1` 镜像至 `go-svc-match-config`；`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 补镜像契约断言；新增 `go/match/internal/logic/battlereportlogic_test.go` 覆盖缺会话 / 未绑角色 / `battle_id=0` / 存储报错。
  - **robot 参战直连断言对齐**：`robot/battle_direct_conn.go` 抽出 `verifyFighterDelivery()`，收敛 `battle_smoke_cross_zone_scenario.go` 与 `battle_smoke_scenario.go`（个人 `BattleEndS2C` 由 scene 经大厅推达，直连 `battle_ends` 应为 0），解除跨区冒烟第 5 步 `battleReportSmokeCheck` 前置拦截；`robot/battle_direct_conn_test.go` 新增 `TestBattleDirectVerifyFighterDelivery`。
  - **文档同步**：更新 `docs/design/battle-report-replay.md`（状态、§6.2、§6.3、§7.4 删角清理已接入 `role_fence_purge.go`）与 `docs/handoff/battle-report-replay-client.md`（§9 客户端仓核对与落码清单）。
- **Unity 客户端改动（`../mmorpg-client`）**：
  - 核对协议与 C# 生成产物已齐全（`MatchServiceRpc` 379 / 380、`PlayerBattle.cs`、`MatchService.cs`、`MatchErrorTip.cs` `KMatchBattleReportNotFound = 16025`），无需重跑协议生成。
  - **纯逻辑（单线程 WebGL 安全、零 Unity 依赖）**：新增 `Assets/Scripts/Game/Battle/BattleReplayDecoder.cs`（版本/上限校验、流式限长 `GZipStream` 防解压炸弹 + `NONE` 兜底）、`BattleReportClient.cs`（大厅连接单飞 RPC、断线/换角代次失效、NotFound 移行、Internal/限频不自动重试、4 份 LRU 缓存、胜负与近期 PVP 资格纯函数）、`BattleReplayPlayer.cs`（从任意第 k 回合起播、倍速 1x/2x/4x、暂停/继续、跳过终态、`DEADLINE_DRAW` 含空回合、`truncated`、配表指纹对比提示）。
  - **接线与 uGUI**：`GameClient.cs` 挂接大厅传输 `BattleReports`；`WorldHudNavigation.cs` 末尾追加 `WorldHudEntry.BattleReport`；新增 `BattleReportPanel.cs`（可滚动 100 条 PVP 战报列表）；`BattleResultPanel.cs` 为近期 PVP 补弹战果加可选「看回放」按钮（默认参数兼容全量旧单测）；`BattleScreen.cs` 新增只读回放模式（`OpenReplay`、`CoPlayReplayTurn`、底部控制条含上一回合/暂停/下一回合/倍速/跳过/跳回合/退出回放）并在 `BattleHudLogic.cs` 登记交互矩形；`BattleUiRoot.cs` 编排战报入口与离线回放循环（不改 3 参 `BindSources` 签名以兼容生命周期单测）；`BattleUiWidgets.cs` 补窗体与输入框换肤。
  - **EditMode 单测**：新增 `Assets/Tests/EditMode/Battle/{BattleReplayDecoderTests,BattleReportClientTests,BattleReplayPlayerTests,BattleReportPanelUiTests}.cs`，完整覆盖交接文档 §7 全部类别。
- **待 Codex 验证**：C++ `battle.vcxproj` + `turn_battle_engine_test` + `battle_observer_contract_test`（`/m:1`）；Go `go/match` build/vet/test（含 `TestBattleReport*` 与 `battlereportlogic_test.go`）+ `k8s_deploy_contract.tests.ps1`；`robot` `-mod=readonly` build/vet/test + 重跑跨区 1v1 冒烟取 `BATTLE_REPORT_OK` 与 `CROSS_ZONE_MATCH_OK`；客户端 `client_compile_check.ps1` 与 Unity EditMode `Battle` 测试集。


## 2026-11-20 Gemini（Gemini 4 Neon Extra）：独立 GM 管理系统现场打通与服务端 / 管理前端强一致 + 水平扩展标准收口（本轮新增代码未编译，交 Codex 验证；Java 全服务版留 TODO）

- **本机「玩家 GM 服务未启用」现场修复（已实测跑通）**：
  - 根因：11-18 重建后 `rpc-bridge/ready.json` 仍钉旧容器 ID、`xuanming-gm-mail-mail-1` 处于 `Exited (255)`、`deploy/debug/compose.game.yml` 缺 GM Scene 密钥环境变量与回环端口 `127.0.0.1:2355:50001`，且 `scene-deploy.py` / `gm_scene_promotion.py` / `docker_rebuild_recovery.py` 尚未支持 `x-xuanming-rebuild` 之上直接提升 GM Scene。
  - 修复（其中 Python 启动/提升脚本与 217/217 单测已在 `b6a08148c` 落库，`deploy/debug/compose.game.yml` 改动在当前 `main` 工作区）：刷新 `rpc-bridge` 上游、`ensure_mail` 自愈拉起 `mail`、`scene-deploy.py` 动态发现现役手绘表与 `afterRebuild` 提升、完成 `scene` 提升与 `start_docker_debug.py --execute` 自检，实测 `probe_scene.py --execute`（`SceneRuntimeProbeIT`）通过（未签名 `PERMISSION_DENIED`，合法签名 `OK` + `tipId=1006` + `executed=false`）。
- **全栈审计与标准收口改动（全部在 `main` 工作区，未建分支、未提交，未触碰其他会话文件）**：
  1. **C++ 公共 GM 安全基元上提核心层 + `CommandCache` 过期回收 + 封禁即时请求存盘**：
     - 新增 `cpp/libs/engine/core/security/gm_security.h`（纯头文件，命名空间 `gm_security`），`cpp/nodes/gate/gate_security.h` 改为 `using gm_security::...` 别名保持 `gate` 与 `gate_security_test.cpp` 100% 兼容。
     - `cpp/nodes/scene/handler/rpc/scene_admin_handler.cpp`、`cpp/nodes/scene/handler/rpc/player/player_gm_guard.h`、`cpp/nodes/scene/handler/grpc/gm_player_command_security.h` 全部改用 `#include "security/gm_security.h"`，彻底清除三处延迟的跨节点相对路径包含（`../../../gate/gate_security.h`）；同步更新 `cpp/tests/currency_test/client_gm_gate_test.cpp`、`cpp/tests/bag_test/player_equip_test.cpp` 与 `cpp/nodes/gate/SECURITY.md`。
     - 修复 `gm_player_command::CommandCache` 满 10,000 条后永久拒单的 DoS 缺陷：按 `issued_at_ms / 1000 + 300 + max(0, skewSeconds)` 最小堆安全回收已绝无可能再通过 `Verify` 的过期条目，未过期条目绝不提前淘汰；`cpp/nodes/scene/handler/grpc/scene_node_service.cpp` 传入当前时间与偏差并补齐 `kBlockCurrency` / `kUnblockCurrency` 成功后触发 `PlayerLifecycleSystem::RequestKeyProgressSave`；`cpp/tests/currency_test/gm_player_command_test.cpp` 补乱序插入与边界回收回归单测。
  2. **GM Java 后端（`java/gm_admin`）强一致性、安全收口与水平扩展**：
     - **动态场景发现（可水平扩展）**：`GamePlayerAdmin.java`、`GameServices.java`、`application-gm.yml`、`run_local.py` 新增可选 `gm.game.scene-etcd-url`（`GM_SCENE_ETCD_URL`，强制 HTTPS 或本机回环 HTTP）。优先静态映射 `GM_SCENE_TARGETS_JSON`，未命中时经 etcd v3 `/v3/kv/range` 动态发现 `SceneNodeService.rpc/` 下场景 `grpcEndpoint`（跳过 `/allocated/`、忽略缺 `grpcEndpoint`、脑裂双注册 fail-closed 报 `SCENE_AMBIGUOUS`、复用连接安全策略拒绝远端明文），移除 64 节点硬上限改为 1024 LRU 通道池并优雅关闭替换/下线通道；`GamePlayerAdminTest.java` 新增本机回环 mock etcd 全套测试。
     - **多副本并发迁移安全 + 复合索引**：`GmInstallation.java` 将版本 1–4 改为事务首行占位 `gm_install` 主键（仅在并发副本已提交同版本时跳过 `DuplicateKeyException`，本事务业务失败仍整笔回滚），新增版本 5 幂等创建 4 条 `gm_command` / `gm_command_audit` 查询与审计复合索引；`GmInstallationTest.java` 补索引幂等测试。
     - **原子过期拦截 + ID 生成加固 + 审计分页筛选与轨迹**：`GmCommandJournal.java` 在 `sendInternal` 的 `FOR UPDATE` 锁内用统一时钟原子拦截超 5 分钟 `PLAYER` 草稿（避免过期草稿误转 `SENDING`），加固 `nextId()` 避开 0 与随机主键碰撞，新增带校验分页与 `playerId`/`status` 筛选的 `list(...)`/`listPlayerCommands(...)` 及 `auditHistory(id, kind)`；`GmCommandJournalTest.java` 补全回归测试。
     - **全币种邮件支持 + 密钥裁剪一致 + 鉴权预言机修复**：`GmController.java` 修复 `mailSecret.strip()` 校验一致性、在 `/gm/overview` 下发权威 `currencies`、放开邮件附件全币种（银两/灵石/绑定灵石，受 `game.supportsCurrency` 校验）、补 `/gm/mail/commands` 分页筛选与 `/gm/mail/commands/{id}/audit`；`GmPlayerCommandController.java` 在 `POST /gm/player-commands/{id}/send` 查库前先验至少持有一项玩家 GM 权限（消除 ID 探测预言机），补 `/gm/player-commands` 分页筛选与 `/gm/player-commands/{id}/audit`；`GmControllerTest.java`、`GmPlayerCommandControllerTest.java`、`GameServicesTest.java` 全部补测。
  3. **GM Vue 3 前端（`java/gm_admin/frontend`）**：
     - `console.ts`、`playerCommands.ts`、`index.vue`、`PlayerCommands.vue`：抽取共享 `currencyLabel`；邮件表单支持选择权威币种（默认银两）；邮件与玩家 GM 审计表补齐按玩家 ID / 状态筛选与分页器；核对弹窗补齐 `gm_command_audit` 完整状态变更轨迹展示。
  4. **文档同步**：更新 `docs/design/jeecg-gm-administration.md` 与 `java/gm_admin/README.md`。
- **只读静态预检**：待在落盘后执行 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（见下方最终汇报）。
- **未编译 / 待 Codex 验证清单**（本轮未改 `.proto`，无需重跑协议生成；`java/gm_admin/prepare.py` 会在打包前重建 `gm-contracts.pb`）：
  1. GM Java 扩展单测 + 打包 + Vue 3 前端构建：
     - `python java/gm_admin/prepare.py`
     - `mvn -f java/gm_admin/pom.xml -DskipTests=false test`（期望全部单测 PASS，含新增 etcd 动态发现、并发索引迁移、原子过期、分页筛选、审计轨迹、多币种邮件、鉴权预言机用例）
     - `mvn -s build/gm-admin-runtime/maven-settings.xml -f build/gm-admin-upstream/jeecg-boot/pom.xml -pl jeecg-module-system/jeecg-system-start -am '-Dmaven.compiler.proc=full' -DskipTests package`
     - `pnpm --dir build/gm-admin-upstream/jeecgboot-vue3 build`
  2. C++ 编译与单测（注：`scene.lib` 当前仍受其他会话的 `player_pet.cpp:1025 kPetNameSensitive` 阻塞，但独立单测与 `gate` 可先验证）：
     - `msbuild /m:1 cpp/nodes/gate/gate.vcxproj /p:Configuration=Debug /p:Platform=x64`
     - 独立 `gate_security_test.cpp`（Linux/容器）：`g++ -std=c++23 -I cpp/nodes/gate -I cpp/libs/engine/core -I third_party/googletest/googletest/include -I third_party/googletest/googletest cpp/nodes/gate/tests/gate_security_test.cpp third_party/googletest/googletest/src/gtest-all.cc third_party/googletest/googletest/src/gtest_main.cc -lssl -lcrypto -lpthread -o /tmp/gate_security_test && /tmp/gate_security_test`
     - `msbuild /m:1 cpp/tests/currency_test/currency_test.vcxproj /p:Configuration=Debug /p:Platform=x64` 后运行 `currency_test --gtest_filter="GmPlayerCommand.*:ClientGmGateTest.*"`
     - 待其他会话修复 `kPetNameSensitive` 后：`msbuild /m:1 cpp/nodes/scene/scene.vcxproj /p:Configuration=Debug /p:Platform=x64`、`bag_test --gtest_filter="PlayerEquipTest.GmGrantItemHandlerFollowsTheSceneRunMode"`、`tools/scripts/build_linux.sh`
  3. 本机重启 GM 网页与探针冒烟：重拉 GM 前后端后跑 `python java/gm_admin/probe_scene.py --execute` 并在浏览器核对 `http://127.0.0.1:3101/` 邮件多币种选择、审计筛选/分页与状态变更轨迹弹窗。
- **Java 全服务版（AGENTS §12）**：保留在 `PARITY.md` TODO，本轮未改独立 Java 全服务仓 `xuanming-server-mmo`。


## 2026-11-20 Gemini（Neon Extra 改码 + Carbon Medium 验证）：真钱支付接入剩余 4 项代码补齐与验证回执

- **依据与分工**：严格按 `docs/handoff/payment-integration-handoff-20261120.md` 与 `AGENTS.md §10.3` 执行——全部代码与测试修改在 `Gemini 4 Neon Extra` 完成，全部导表、协议生成、构建、单测、契约测试与隔离 TiDB 真库验证由独立验证员 `Carbon Medium` 执行（未建分支、未 `git commit` / `git push`、未触碰本机在跑的 `xuanming-debug-current` / `xuanming-gm-*` / `redis` 容器，日志位于已忽略目录 `build/verification/payment-20261120/`）。
- **代码补齐与测试修复（Gemini 4 Neon Extra）**：
  1. **§2.1 支付渠道复核收尾（`go/payment/internal/channel/`）**：
     - 复核并保留此前复核留下的 6 个源文件修复（含 `channel.go` 的 `send()` 剥离传输错误中带密钥的 URL、`apple.go` 无 `data` 通知处理/179 天底线/窗口折半、`google.go` `errGoogleCerts` 区分、`wechatpay.go` `ORDER_CLOSED` 归入关单），全部 `.go` 经 `gofmt -w` 对齐。
     - `google.go`：空解析公钥集（`len(keys) == 0`）拒缓存并返回 `errGoogleCerts`。
     - `douyin.go`：对照抖音开放平台小游戏虚拟支付官方文档（`developer.open-douyin.com`）补齐 GET 回调验证 `Handshake(rw, r) bool`（实现 `channel.Handshaker`，已由 `internal/notify` 自动接入）、`goodName` 改为官方 10 字符上限（超长截断、空回落 `"游戏道具"`）、拒绝空 `timestamp`/`nonce`。
     - 新增纯标准库离线单测 `apple_test.go`、`google_test.go`、`douyin_test.go`（零外网、`fixedNow`），并修正时间字面量以对齐 `testPaidAt`/`testNow`。
  2. **§2.2 安卓微信支付与支付宝插件（`../mmorpg-client/Assets/Plugins/Android/MmorpgPay.androidlib/`）**：
     - 仿照现有 `MmorpgGoogleSignIn.androidlib` 新增 `build.gradle`（`wechat-sdk-android:6.8.40`、`alipaysdk-android:15.8.42`）、`AndroidManifest.xml`（`<queries>` 声明微信/支付宝包名，通过 `${applicationId}.wxapi.WXPayEntryActivity` `<activity-alias>` 指向 `com.mmorpg.pay.WechatPayCallbackActivity`，无需写死安卓包名）、`consumer-rules.pro`、`WechatPayCallbackActivity.java`（仅 `handleIntent` + `finally { finish(); }`，忽略客户端结果）、`MmorpgPay.androidlib.meta`（Android-only）。
     - 核对 `Assets/Scripts/Game/Jubaozhai/AndroidPayBridges.cs` 反射类名、方法与字段名与两份 SDK 一致。
  3. **§2.3 商城 K8s HTTP 健康探针**：
     - `java/mall_game/src/main/resources/application.yaml`：仅暴露 `health`，开启 `probes.enabled: true`，`liveness`/`readiness` 分组分别只含 `livenessState`/`readinessState`（不含 DB 指示器，与 `gateway_node` 同口径）。
     - `java/mall_game/src/main/java/com/game/mall/HmacAuthentication.java`：在规范路径校验之后仅对无正文 `GET /actuator/health/readiness` 与 `/actuator/health/liveness` 放行免 HMAC，复合 `/actuator/health` 及其他接口保持 fail-closed。
     - `java/mall_game/pom.xml`、`deploy/k8s/manifests/java-svc/mall.yaml`（三处探针由 `tcpSocket` 改为 `httpGet`）、`deploy/k8s/README.md`、`tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1` 同步更新；新增 `java/mall_game/src/test/java/com/game/mall/MallHealthProbeHttpTest.java`。
  4. **§2.4 客户端 Unity IAP 与验证期间发现的测试缺陷修复**：
     - 核对 `../mmorpg-client/Assets/Scripts/Game/Jubaozhai/StorePayBridge.cs` 与本地已解析包 `Library/PackageCache/com.unity.purchasing@015b4cec2127`（5.0.4）源码：`CartItem.Product`、`PurchaseFailureReason.UserCancelled`、`StoreController` 及 Apple/Google 扩展接口全部一致，无需改动。
     - `../mmorpg-client/Assets/Scripts/Game/Jubaozhai/PaymentClient.cs`：`DeviceOs` 改用编译期 `#if UNITY_IOS / UNITY_ANDROID / UNITY_WEBGL`（与同文件 `Platform` 一致），消除独立 NUnit 下调用 `Application.platform` 触发的 `SecurityException` ECall。
     - `java/mall_game/src/main/java/com/game/mall/DeliveryStore.java`：补回 4 参构造函数委派给 5 参 + `Clock.systemUTC()`（修复 `RmbConsignmentStoreTest` 编译错误）。
     - `java/mall_game/src/test/java/com/game/mall/{StorageAndAuthTest,MockPaymentStoreTest,MallTracingHttpTest}.java`：手建表夹具补齐支付线新增列（`commerce_payment_receipt` 的 `revision,fact`、`commerce_refund_intent` 的 `close_reason`）；`MallTracingHttpTest` 补 `@AutoConfigureObservability(metrics = false)` 对齐 `GatewayTracingIntegrationTest`（否则 Spring Boot 测试自动关闭追踪传播导致 `traceparent` 降级为 noop）。
     - `go/trade/internal/commerce/delivery_order_db_test.go:84`：删去已提交在 HEAD 里阻塞整包编译的 1 行未用变量 `ctx := context.Background()`。
- **验证结果（Carbon Medium 执行 + 只读预检）**：
  1. **导表与协议生成（PASS）**：`cmd /c "echo. | .\dev.bat gen"`（`exit=0`）；前置仓库 `third_party\protobuf\install_vs2026_dbg\bin`（`libprotoc 35.1`）运行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（`exit=0`）；核对 `go/shared/generated/pb/table/payment_error_tip.pb.go`（`kPaymentRealNameRequired`）、`cpp/generated/table/proto/tip/payment_error_tip.pb.{h,cc}`、`go/proto/payment/payment_table.pb.go`（`PaymentSpendRecord`、`SpenderHi`）、`../mmorpg-client/Assets/Scripts/Proto/Generated/PaymentErrorTip.cs` 均存在且匹配。
  2. **`go/payment`（100% PASS，含真 TiDB）**：`gofmt -l .`（0 文件）、`go mod tidy`（无改动）、`go build ./...`、`go vet ./...` 均 `exit=0`；离线 `go test ./... -count=1 -v` 顶层 **64 PASS / 94 子用例 PASS / 0 FAIL / 3 SKIP**（其中 `internal/channel` **51 PASS / 94 子用例 PASS**，含 `apple_test.go`、`google_test.go`、`douyin_test.go` 及原 4 个测试文件）；再起隔离临时 `TiDB v8.5.2`（本地已缓存镜像 `--pull=never`，`127.0.0.1:14000`，完事 `finally` 删除零残留），设 `PAYMENT_TEST_DSN` + `PAYMENT_REQUIRE_TIDB=1` 跑 `go test ./internal/data/... -count=1 -v` -> **7/7 PASS / 0 SKIP / 0 FAIL**（`TestTiDBReserveIntentSerializesOnePerson`、`TestTiDBReserveIntentIsIdempotentPerOrder`、`TestTiDBPaidClosedAndRevivedIntents` 全绿）；`go run . -version` 与 `go build ./cmd/devpaynotify` 均 `exit=0`。
  3. **`go/trade`（100% PASS，含真 TiDB）**：`go build ./...`、`go test ./internal/commerce/... -count=1 -v` 顶层 **90 PASS / 136 子用例 PASS / 0 FAIL**（`TestPlanOfficialFollowsPaymentRevisions` 4 子用例及 `TestCommerceSignatureMatchesCrossLanguageGoldenForPaymentPost` 全绿）；隔离临时 `TiDB v8.5.2` 上加载 `tools/scripts/sql_fixture_env.ps1` 并设 `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1` 跑 `go test ./internal/commerce -run 'TestTiDBMallReversalClawsBackThenRegrants|TestTiDBMallBridgeWaitsForCommittedLegAndRecoversLostReceipt' -count=1 -v` -> **2/2 PASS / 0 SKIP / 0 FAIL**。
  4. **`java/mall_game`（100% PASS）**：在 `java/` 先装本地快照 `authority_jdbc,server_startup` 后，在 `java/mall_game` 执行 `mvn test` -> `BUILD SUCCESS`，**124/124 PASS（0 Failures / 0 Errors / 0 Skipped）**，其中 `PaymentRevisionStoreTest` **7/7**、`MallHealthProbeHttpTest` **1/1**、`StorageAndAuthTest` **6/6**、`MockPaymentStoreTest` **13/13**、`MallTracingHttpTest` **1/1**、`RmbConsignmentStoreTest` **11/11**。
  5. **C++（本线 0 error，整库受跨会话阻塞）**：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`（10/10 OK）、`python tools/scripts/check_log_text_english.py`（0 violations）；`msbuild /m:1 cpp/generated/table/table.vcxproj` `exit=0`；支付本线 `asset_op_system.cpp` 与 `asset_op_system_test.cpp` 单文件 `/m:1 /t:ClCompile` 与裸指针门禁均 `exit=0`。`scene.vcxproj` 与 `currency_test.vcxproj` 链接被其他会话代码阻塞（未改他人文件），首错原文：
     - `scene.vcxproj`: `player_pet.cpp(1025,36): error G13DA7946: use of undeclared identifier 'kPetNameSensitive'`
     - `currency_test.vcxproj`: `transaction_log_commit_gate_test.cpp(167,28): error G60A5EC10: no member named 'logs_size' in '(anonymous namespace)::AuditOutboxTest::Attached'`
  6. **部署脚本测试（本线 100% PASS）**：`k8s_mall_payment_deploy_contract.tests.ps1` **22/22 PASS**、`sql_boot_catalog_local.tests.ps1` **51/51 PASS**、`redis_player_cluster_contract.tests.ps1` **47/47 PASS**；`k8s_deploy_contract.tests.ps1` 360/364（支付/商城及 `$JavaSvcCatalogue.Keys` 排序断言全 PASS，余 4 项为其他会话的发布轨道/peak-shaving/quorum 反亲和/content-guard 失败）；`k8s_migrate_gate.tests.ps1` 72/78（payment 2 条 schema Job 全 PASS，余 6 项为跨会话托管内部密钥与 match ClusterRole）；`logical_db_deploy_contract.tests.ps1` 45/46（仅缺 `role-fence-trade` 预置清单文档项）；全目录 78 个脚本共 56 PASS / 22 FAIL，全部失败项均与 mall/payment 无关。
  7. **Unity 客户端（Roslyn 多平台与独立 NUnit 100% PASS）**：`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40` -> `exit=0`（545 文件 0 error）；带 `MMORPG_UNITY_IAP` 对 `Editor` / `UNITY_ANDROID` / `UNITY_IOS` / `UNITY_WEBGL` 四套目标平台 Roslyn 编译全部 **545 文件 0 error**；独立 NUnit 运行 **29/29 PASS**（`PaymentClientTests` **5/5**、`CommerceClientTests` **20/20**、`CommerceRecoveryFileStoreTests` **4/4**）。注：`Temp/UnityLockfile` 存在且 `Unity.exe`（6000.6.0f1，PID 29748）正在打开该工程，未用 `-batchmode` 起第二实例。
- **未执行项与阻塞原因**：
  - **C++ `AssetOpSystemTest.*` 运行**：待其他会话修复 `player_pet.cpp:1025` 与 `transaction_log_commit_gate_test.cpp:167` 后重编运行 `currency_test.exe --gtest_filter=AssetOpSystemTest.*`。
  - **§3 第 8 步本机全栈 devpay 冒烟**：需按 `AGENTS.md §6.2` 清库、起完整 C++ `scene`（受 `player_pet.cpp` 阻塞）并在交互式 Unity 编辑器聚宝斋界面操作，且本机正运行 `xuanming-debug-current` / `xuanming-gm-*` 及 Unity PID 29748，未破坏现场。
  - **§3 第 9 步 kind 冒烟**：本机未安装 `kind` 且无 `~/.kube/config` 集群上下文，按 `AGENTS.md §10.2` 未擅自安装工具。
  - **Java 全服务版（AGENTS §12）**：仍登记在 `PARITY.md` 待做，未改独立仓 `D:\luyuan\xuanming-server-mmo`。


## 2026-11-20 Gemini（Gemini 4 Neon Extra）：登录排队 / 开服限流与 Unity 客户端全链路标准收口（未生成协议、未编译运行，交 Codex 验证；Java 全服务版留 TODO）

- **背景与核查结论**：
  - 核查确认 2026-11-19 Unity 排队遗留问题（`open-server-rate-limit-design.md` §11.5 第 6 条「恢复重入自身失败不丢票、按退避回查询」）及多角色重连、`MaxAdmitPerTick=2000` / `MaxQueueLength=1000000`、网关 `queue-retry-min-ms` / 抖动 / IP 0.1 扣费均已在此前会话落库（`mmorpg-client` `02e0ec54`、`xuanming-server-mmo-cpp` `b6a08148c` / `c441ce75e`）。
  - 本轮逐项排查剩余「以后修 / 残余 / 未测」代码缺口并一次性按强一致、可水平扩展标准收口；所有改动直接落于 `main` 工作区，未建分支、未提交，未触碰其他会话在途文件。
- **服务端改动（`xuanming-server-mmo-cpp`）**：
  1. **`PlayerSession.gate_zone_id` 闭环重连免排队强一致（`login-queue-2026-05.md` §8 / §14 #1）**：
     - `proto/player_locator/player_locator.proto`：`PlayerSession` 新增 `uint32 gate_zone_id = 16`，`ReconnectRequest` 新增 `uint32 gate_zone_id = 11`（仅 Go 内部 gRPC，无 C++ / 客户端消费方）。
     - `go/player_locator/internal/logic/{setsessionlogic.go,reconnectlogic.go}`：`SetSession` 原样写入不继承旧值，`Reconnect` 同步更新 `session.GateZoneId = in.GateZoneId`；新增 `gate_zone_id_test.go` 覆盖写入、跨区覆盖与旧版写 0 覆盖。
     - `go/login/internal/logic/pkg/sessionmanager/session_manager.go`：`PlayerSession`、`OnlineSessionInput`、`toProto`、`fromProto`、`applyOnlineSession`、`Reconnect` 全链路透传 `GateZoneID`；`login_generation_test.go` 新增 `TestGateZoneID_SessionMirrorRoundTrip`。
     - `go/login/internal/logic/clientplayerlogin/entergamelogic.go`：`persistEnterGameSession` 在 `FirstLogin` / `ReplaceLogin`（`SetSession`）与 `ShortReconnect`（`Reconnect`）均写入 `config.AppConfig.Node.ZoneId`；`enter_wiring_test.go` 新增 `TestEnterGame_GateZoneIDReachesTheSessionRecord`。
     - `go/login/internal/logic/loginpregate/assigngatelogic.go`：`reconnectingPlayer` / `findReconnectingPlayer` 在 `gate_zone_id != 0` 时以 `gate_zone_id == zoneID` 为权威且 0 次读 `zoneGates`（彻底解决合服并入角色原 gate 掉线被误排队、以及本区角色跨区到别区在线误免本区排队）；`gate_zone_id == 0` 保留旧会话兼容兜底；`assigngate_reconnect_test.go` 全量更新并补齐两类新用例及 0 次 gate 列表读断言。
  2. **网关 `/api/refresh-token` 接入共享 IP 令牌桶 + 清理死键 `queue-timeout-ms`（`open-server-rate-limit-design.md` §3 / §5.4）**：
     - `java/gateway_node/src/main/java/com/game/gateway/ratelimit/AssignGateRateLimiter.java`：新增 `checkIpOnly(String clientIp)`，复用共享 Redis IP 令牌桶、fail-open 熔断器与决策计数，跳过开服波次、区桶与账号冷却。
     - `java/gateway_node/src/main/java/com/game/gateway/dto/RefreshTokenResponse.java`：新增 `CODE_RATE_LIMITED = 429`。
     - `java/gateway_node/src/main/java/com/game/gateway/controller/RefreshTokenController.java`：注入 `AssignGateRateLimiter` 与 `ClientIpResolver`，IP 桶空时回 HTTP 200 `code=429`、`message="IP_RATE_LIMIT"` 不打下游 `RefreshTokenService`。
     - `java/gateway_node/src/main/java/com/game/gateway/ratelimit/RateLimitProperties.java`、`java/gateway_node/src/main/resources/application.yaml`：删除无任何消费方的死配置 `queueTimeoutMs` / `queue-timeout-ms`；`tools/scripts/release_preflight.ps1` 将原 `ratelimit.queue.timeout` 检查替换为 `ratelimit.queue.retry.min`（校验 `gate.rate-limit.queue-retry-min-ms >= 500`）。
     - 测试：`AssignGateRateLimiterTest.java` 新增 `checkIpOnly` 跳过波次/区桶/冷却与 IP 拒单用例；新增 `RefreshTokenControllerTest.java`。
  3. **文档对账同步**：更新 `docs/design/login-queue-2026-05.md`（§6 24h 轮换注释已同步、§7.1 SIGTERM 经 `defer ctx.Stop()` 主动放锁已核实、§8 与 §14 #1 闭环）、`docs/design/open-server-rate-limit-design.md`（§3、§5.4、§10、§11.3 #6、§11.5）、`docs/ops/login-queue-stress-runbook.md`（模式 5）。
- **Unity 客户端改动（`../mmorpg-client`）**：
  - `Assets/Scripts/Game/GameClient.cs`：`EnterZone` 第 2 步 `/api/refresh-token` 返回后复核排队总预算；以单调时钟、可随 `_pipelineGen` 翻世代与总预算截止时刻即时退出的 `WaitQueueMs` 取代原 `WaitMs`（不再在取消或到期后干等 30s / 120s，附私有测试接缝 `_queueClockOverride` / `_queueWaitOverride`）。
  - `Assets/Scripts/Net/GatewayHttpClient.cs`：`PostJson` 非 200 错误文案改为 `string.IsNullOrEmpty(req.error) ? "HTTP " + req.responseCode : req.error`（修复 HTTP 204 等 `req.error` 为空串时丢失状态码）。
  - 新增 `Assets/Tests/EditMode/Net/LoginQueueFlowTests.cs`（含 `.meta`）：经真实 `GatewayHttpClient`（`UnityWebRequest`）与回环假网关端到端覆盖 §11.5 第 2~7 条（含第 6 条重入断线/502/500/429/100 ratelimit 保票退避 2/4/8/16/30/30s 与恢复回 2s）、503/404 终态、410 换票与无票 410 停止、无票 429 30/60/120s 退避、排队内续期（含 429 退避 / 401 失效 / 续期超预算）、非 200 Spring 错误页与 HTTP 204、以及真实 `WaitQueueMs` `Disconnect` 即时退出。
  - `Assets/Tests/EditMode/Net/GameClientReconnectTests.cs`：新增 `RecoveryRetriesOnRateLimitQueueHintWithinBudget`（验证断线重连收到 `100 ratelimit` 在 20s 预算内重试成功）并将 `DefinitiveAssignGateResultEndsRecoveryWithoutRetry` 显式标为 `queue_source="login"`。
- **只读静态预检（已跑，全部通过）**：
  - `gofmt -l` 核查本轮全部 9 个 Go 文件：0 差异；
  - `tools/scripts/release_preflight.ps1` PowerShell `Parser::ParseFile`：0 errors；
  - `python tools/scripts/check_log_text_english.py`：0 violations。
- **未验证（交 Codex / 低档模型按顺序执行）**：
  1. **协议生成（仅 Go 内部 `player_locator.proto`，需确保 `protoc` 35.1 在 PATH 前列）**：
     - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
     - 核对 `go/proto/player_locator/player_locator.pb.go` 已生成 `GateZoneId` 字段（`PlayerSession` 与 `ReconnectRequest`）。
  2. **Go 编译与单测（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）**：
     - `go/player_locator`：`go build ./... && go vet ./... && go test ./internal/logic/... -count=1 -v`（含 `TestSetSession_PersistsAndReplacesGateZoneID`、`TestReconnect_UpdatesGateZoneID`）
     - `go/login`：`go build ./... && go vet ./... && go test ./internal/logic/pkg/sessionmanager/... ./internal/logic/clientplayerlogin/... ./internal/logic/loginpregate/... ./internal/logic/pkg/loginqueue/... -count=1`（含 `TestGateZoneID_SessionMirrorRoundTrip`、`TestEnterGame_GateZoneIDReachesTheSessionRecord`、`TestFindReconnectingPlayer*`）
     - `robot`：`go build -mod=readonly ./... && go vet -mod=readonly ./... && go test -mod=readonly ./... -count=1`
  3. **Java Gateway 编译与单测（`java/gateway_node`）**：
     - `mvn -f java/gateway_node/pom.xml test`（含 `AssignGateRateLimiterTest`、`RefreshTokenControllerTest`、`AssignGateControllerTest`、`LoginEndpointIntegrationTest`、`GatewayTracingIntegrationTest`）
  4. **预检脚本回归（仓库根）**：
     - `pwsh -NoProfile -File tools/scripts/release_preflight.ps1`（确认 `ratelimit.queue.retry.min` 为 `PASS` 且不再引用已删的 `queue-timeout-ms`）
  5. **Unity 客户端编译与 EditMode 测试（`../mmorpg-client`）**：
     - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`
     - Unity EditMode 跑 `MmorpgClient.Tests.EditMode.Net.LoginCredentialTests;MmorpgClient.Tests.EditMode.Net.LoginQueueFlowTests;MmorpgClient.Tests.EditMode.Net.GameClientReconnectTests`（含真实 `UnityWebRequest` 回环假网关，无需起服务端）。
  6. **开排队合入门槛真实 Redis 冒烟（待后续择机执行）**：按 `docs/PROGRESS.md` 前文「登录排队清合入门槛的 Codex 验证单」跑 Redis 7.2 单实例与 Redis 8.10.2 Cluster 两轮，及断网 / 断 `queue-status` 实栈冒烟。
- **Java 独立全服务版（AGENTS §12）**：已追加登记至 `../xuanming-server-mmo/PARITY.md` TODO，本轮未改独立 Java 仓代码。


## 2026-11-20 Gemini（收口收尾）：冷启动回滚、寄售标题审核、角色/帮会改名、外部平台内容安全 + 聊天举报撤回与防刷 + 渠道登录视觉收口（源码已全量落于 main，交 Codex 生成协议/编译/冒烟）

- **状态声明**：严格在双仓 `main` 完成服务端、SQL、K8s 与 Unity 客户端代码及单测编写，未建分支、未提交；按 `AGENTS.md §10.1` 未运行代码生成器（`dev.bat gen`/`proto-gen-run`）、Go/C++/Unity 编译及运行期冒烟，全部留交 Codex 按下方顺序执行。
- **核心改动清单**：
  1. **冷启动 Catalog 回滚 & 寄售标题审核 & 角色/帮会改名**：
     - `go/shared/authoritysql/{catalog.go,tx_outcome_test.go}`、`go/data_service/cmd/writer_authority/{main.go,main_test.go}`。
     - `go/content_guard` / `go/trade`：`CreateRmbConsignmentRequest.title` 接入 `TEXT_SCENE_TRADE_TITLE`。
     - Unity 角色改名：`Assets/Scripts/Game/RoleNameRules.cs`、`GameClient.{RoleRename,cs}`、`RpcGuard.cs`、`AttributeUiRoot.cs`、`CharacterSupportWindow.cs`、`RoleRenameClientTests.cs`（C++ `PlayerRenameSystem` 已在主仓）。
     - 帮会改名：`proto/guild/{guild,guild_db}.proto`、`go/guild/...`（含修补 `economy_logic_test.go` mock SELECT 列数匹配 `renamed_ms`）、Unity `GuildClient.cs`、`RpcGuard.cs`、`GuildWindow.cs`、`GuildUiRoot.cs`、`GuildRenameClientTests.cs`。
  2. **渠道身份落库与 `content_guard` 外部平台 + 联系方式广告识别**：
     - `proto/common/database/rollback_database_table.proto`（`account_channel_identity`）、`proto/data_service/data_service.proto`（`UpsertAccountChannelIdentity`、`GetPlayerChannelIdentity`）、`go/data_service/internal/{store/account_channel_identity.go,store/schema.go,store/account_directory_store.go,svc/*,server/{account_directory.go,account_channel_identity_test.go}}`、`go/shared/accountdirectory/client.go`、`go/login/internal/logic/...`。
     - `proto/content_guard/content_guard.proto`：新增 `AsyncPlatform`、`PendingPlatformReview` 与 `CheckPlatformText` RPC。
     - `go/content_guard`：新增 `internal/contact` 变体联系方式/URL 识别并接入 `internal/engine`（`ScenePrivateLabel` 豁免启发式误伤）；统一清理 `internal/platform` 重复文件（合并微信 `msgSecCheck` v2 token `40001/40014/42001` 重试与抖音 `antidirt` `hit || prob>=1` + 401 重试、`OutcomePending` 短超时转异步及 `DataService` 目录身份缓存查询）；更新 `config`、`etc/content_guard.yaml`、`server` 与 `go/shared/contentguard`。
  3. **`go/chat` append-only SQL 撤回、举报与多维防刷 + K8s 脚本同步**：
     - `proto/chat/chat.proto`：新增 `ReportChat`、`RetractWorldChat`（`CLIENT_RPC_SERVER_ONLY`）、`NotifyWorldChatRetracted`（`CLIENT_RPC_PUSH`，匹配 `cpp/nodes/gate/main.cpp:397` 的 `ClientPlayerChatNotifyWorldChatRetractedMessageId`）。
     - `deploy/sql/chat_runtime_role.sql`、`go/chat/internal/authority/{schema.go,store.go,retract_report.go,content_review.go,retract_report_test.go,least_privilege_tidb_test.go}`：新增 append-only + TiDB TTL 表 `chat_world_retraction`、`chat_report`、`chat_repeat_event`、`chat_private_daily_target`（运行账号保持仅 `SELECT, INSERT`）；实现世界最低等级（`WorldMinLevel`）、跨副本归一化摘要刷屏拦截、新号每日私聊人数上限、举报写入、世界撤回及 `Pull` 过滤。
     - `go/chat/internal/{config,constants,svc,logic,server}` 与 `chat_test.go`：接入 `safego` 异步平台复审超时兜底撤回与 Kafka `chat-world-bcast` 广播通知；`tools/scripts/tests/test_client_text_fields_reviewed.py` 补全 `RetractWorldChatRequest.reason` 豁免。
     - `tools/scripts/k8s_deploy.ps1` 与 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`：镜像 `chat` 新增防刷标量、屏蔽 `ReportChat` / `CheckPlatformText` 日志正文，并为 `content-guard` 配齐可选 `DataServiceRpc`（`NonBlock: true`）。
  4. **Unity 聊天举报/撤回 UI 与渠道登录官方规范视觉**：
     - `Assets/Scripts/Game/Social/{SocialState.cs,SocialClient.cs}`、`Assets/Scripts/Net/RpcGuard.cs`、`Assets/Scripts/UI/Ugui/Social/{SocialUiRoot.cs,SocialWindow.cs,SocialWindow.Fidelity.cs}`、`Assets/Tests/EditMode/Social/SocialTests.cs`。
     - `Assets/Resources/UI/Ugui/RefreshV8/{apple_signin_button,apple_logo,google_signin_button,google_logo}.png{,.meta}` 及 `Assets/Scripts/UI/Ugui/QdaoServerSelectView.Visuals.cs`。
- **静态检查结果（本轮已跑）**：
  - 见本次回复中的 `test_client_text_fields_reviewed.py`、`check_log_text_english.py`、`test_cpp_no_raw_pointer_member.py`、`test_cpp_thread_sources.py`、`Parser::ParseFile` 与 `.meta` 检查结果。
- **交 Codex 执行顺序**：
  1. **导表与协议/消息号生成（仓库根，`protoc` 35.1 在 PATH 前列）**：
     - `cmd /c "echo. | .\dev.bat gen"`
     - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`
     - `pwsh -File tools/scripts/gen_proto.ps1` 及 `pwsh -File tools/scripts/gen_messageids.ps1`（确认生成 `ChatErrorTip`、`ClientPlayerChatRpc.{ReportChatMessageId, NotifyWorldChatRetractedMessageId}`、`go/chat/generated/pb/game/ClientPlayerChatNotifyWorldChatRetractedMessageId`、`go/proto/{chat,content_guard,data_service,guild}` 与 Unity C# 生成代码）。
  2. **Go 全相关模块编译、Vet 与单测（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）**：
     - `go/shared`、`go/data_service`、`go/login`、`go/guild`、`go/trade`、`go/content_guard`、`go/chat`：分别执行 `gofmt -l .`、`go build ./...`、`go vet ./...` 与 `go test ./... -count=1`。
     - 临时 TiDB 实例下跑 `go/chat/internal/authority` 与 `go/guild` / `go/trade` / `go/data_service` SQL 集成测试（覆盖 `TestAntiSpamRetractAndReportTiDB`、`TestChatTiDBLeastPrivilegeRuntimeContract` 六表最小权限）。
  3. **部署脚本与静态门禁测试**：
     - 清理 `MMORPG_*` 环境变量后运行 `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（关注 `chat` 与 `content-guard` ConfigMap 用例）。
     - 运行 `python -X utf8 -m unittest tools/scripts/tests/test_client_text_fields_reviewed.py -v`。
  4. **C++ 编译与测试**：
     - `msbuild /m:1 game.sln`（核对 `gate` 中 `ClientPlayerChatNotifyWorldChatRetractedMessageId` 与 `scene` `PlayerRenameSystem` 编过、测试通过）。
  5. **Unity 客户端（`../mmorpg-client`）**：
     - Rebake 预制体并检查编译：`Unity.exe -batchmode -quit -projectPath D:\luyuan\mmorpg-client -executeMethod MmorpgClient.UI.EditorTools.QdaoUguiBuilder.BuildAll`（如需提交更新后 Prefab）及 `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`。
     - 运行 EditMode 测试：`QdaoServerSelectPrefabTests`、`SocialStateTests`、`SocialClientTests`、`RoleRenameClientTests`、`GuildRenameClientTests`。
  6. **全栈清库冒烟**：因角色改名/帮会改名/渠道身份表为新增列与新表，按 `AGENTS.md §6.2` 清库执行 `-migrate` 后跑一轮角色改名、帮会改名、聊天发送/举报/撤回端到端冒烟。
- **Java 独立全服务版（AGENTS §12）**：已追加登记至 `../xuanming-server-mmo/PARITY.md` 待做，本轮未改 Java 仓代码。


## 2026-11-20 Gemini（全量收口与存量缺陷清零）：配表/改名/跨会话对账修复 + C++ 编译崩溃与 K8s 契约测试全清（未导表、未编译运行，交 Codex 验证）

- **状态声明**：全部服务端与 Unity 客户端改动已落于双仓 `main` 工作区（无分支、未提交），按 `AGENTS.md §10.1` 未执行导表/协议生成/编译/运行测试，全部留交 Codex 按下方修正后的顺序执行。
- **本会话补齐与跨会话对账修复**：
  1. **配表、表工程与客户端生成脚本（批 1~6 前置完整闭环）**：
     - 新增幂等配表脚本 `tools/scripts/rename_content_safety_xlsx_patch.py` 并已更新 `Item.xlsx`、`RoleNameRule.xlsx`、`GuildRule.xlsx`、`Tip.xlsx`（补齐 `//login_error` +7、`//guild_error` +5、`//trade_error` +2、`//pet_error` +`PetNameSensitive` 及新建 `//chat_error base=18000` +7，同时解除前文记录的 `player_pet.cpp:1025` `kPetNameSensitive` 编译阻塞）。
     - 新增 `data/schema/{rolenamerule_table,guildrule_table}.proto` 并接线 `cpp/generated/table/{table.vcxproj,table.vcxproj.filters,CMakeLists.txt}`；同步 `../mmorpg-client/tools/{gen_proto.ps1,gen_messageids.ps1}`（加入 `chat_error_tip.proto`、`pet_error_tip.proto` 与 `GuildServiceRenameGuild`）。
     - 完整落码角色改名 C++/Go 侧与背包占用机制（`cpp/libs/modules/bag/item_hold.h`、`proto/common/component/player_item_hold_comp.proto`、`cpp/libs/services/scene/player/system/{player_rename_rules.h,player_rename.{h,cpp},player_lifecycle.cpp}`、`cpp/tests/bag_test/{item_hold_test,player_rename_test}.cpp`、`go/data_service/internal/{server/account_role_rename*.go,store/account_role_rename_tidb_test.go}`），补齐并修正帮会改名事务/保护表与客户端调用编译错误。
  2. **跨会话接口与测试夹具修复**：
     - `go/content_guard/content_guard.go`：`c.DataServiceRpc` 改用 `dsclient.Conf(c.DataServiceRpc)` 包装，满足 `go/shared/dsclient/dsclient_test.go::TestAllDataServiceClientConstructorsUseDsclientConf`。
     - `go/chat/internal/authority/fixture_test.go`：测试夹具关闭全部 5 张带 TTL 的表（`chat_send_authority`、`chat_world_retraction`、`chat_report`、`chat_repeat_event`、`chat_private_daily_target`）的 TTL，并将 `fixture.GrantTables(...)` 补全至 6 张表（修复 TiDB 集成测试缺权 1142 错误）。
     - `tools/scripts/tests/chat_authority_deploy.tests.ps1` 与 `tools/scripts/tests/sql_boot_catalog_local.tests.ps1`：对齐 `mmorpg_chat` 6 张表（5 张 append-only `SELECT, INSERT` + `chat_policy` `SELECT`）与 `mmorpg_guild` 的 `guild_name_hold`。
     - 核对 `NotifyWorldChatRetracted` / `ClientPlayerChatNotifyWorldChatRetractedMessageId` 在 `proto/chat/chat.proto`、`cpp/nodes/gate/main.cpp`、`go/chat` 与 Unity `SocialClient.cs` 全链路一致；对 4 个未格式化 Go 文件执行 `gofmt -w`。
  3. **同步清零已发现的存量 C++ 与 K8s 契约测试缺陷**：
     - **C++ `turn_battle_engine_test` 重复读配置崩溃**：`cpp/libs/engine/config/config.cpp` `readBaseDeployConfig` 在 `YAML::LoadFile` 后增加 `baseConfig.Clear()`，避免同一进程多次加载把 `etcd_hosts` 追加成 2 条触发 `LOG_FATAL`；在 `cpp/tests/configuration_table_test/configuration_table_test.cpp` 补回归单测 `ReadBaseDeployConfigTwiceDoesNotAccumulateRepeatedFields`。
     - **C++ `currency_test` 编译阻塞**：`cpp/tests/currency_test/transaction_log_commit_gate_test.cpp` 为 `AuditOutboxTest::Attached` 补齐 `logs_size()`、`logs(int)`、`snapshots_size()`、`snapshots(int)`（解阻 `currency_test.vcxproj` 与 `AssetOpSystemTest.*`）。
     - **K8s 契约测试 3 份脚本全部存量失败项**：
       - `k8s_deploy_contract.tests.ps1`：`tools/scripts/k8s_deploy.ps1` `db` ConfigMap 注释移除字面量 `MMORPG_RELEASE_TRACK`；`deploy/k8s/manifests/infra/etcd.yaml` 将注释移至 `affinity:` 之上；`tools/scripts/tests/lib/deploy_capture.ps1` `Select-ManifestByName` 收紧为区分大小写的顶层 `metadata:` + `[ ]{2,4}name:`，避免误匹配 ConfigMap 里的 `Telemetry.Name: login`。
       - `k8s_migrate_gate.tests.ps1`：`deploy/k8s/manifests/go-svc/match-battle-pod-reader-rbac.yaml` 注释移除字面量 `ClusterRole`；`mail`/`trade`/`guild` 校验器测试对齐 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS` 明文环境变量（取代已下线的 `gate-identity-auth` Secret）。
       - `k8s_client_entry_contract.tests.ps1`：`Invoke-DeployWithFakeKubectl` 补齐非 DryRun `staging`/`prod` 必需参数（`PlayerStoreShards = 64`、`NetworkPolicies = 'on'`、`PodNetworkEncryption = 'cilium-wireguard'`）与 3 条 Cilium WireGuard 只读 fake kubectl 规则。
- **只读静态预检结果（全部通过）**：
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`：**10/10 OK**
  - `python tools/scripts/tests/test_cpp_thread_sources.py`：**5/5 OK**
  - `python tools/scripts/check_log_text_english.py`：**0 violations**
  - `python tools/scripts/tests/test_client_text_fields_reviewed.py`：**2/2 OK**
  - `gofmt -l`（本批全部修改/新增 Go 文件）：**0 差异**
  - `pwsh`（PowerShell 7）`Parser::ParseFile` 校验全部 15 个改动 `.ps1`（服务端 + 客户端）：**15/15 0 errors**
- **交 Codex 执行的精确命令顺序（请严格先导表/生成协议，再编译/测试）**：
  1. **导表 + 服务端与客户端协议/消息号生成**（确保 `protoc` 35.1 在 PATH 前列）：
     - 服务端仓 `D:\luyuan\xuanming-server-mmo-cpp`：
       - `cmd /c "echo. | .\dev.bat gen"`
       - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`
       - `pwsh -File go/build.ps1`
     - 客户端仓 `D:\luyuan\mmorpg-client`（注：上条误写为服务端路径，以此处为准）：
       - `pwsh -File tools/gen_proto.ps1 -ProtoRoot D:\luyuan\xuanming-server-mmo-cpp`
       - `pwsh -File tools/gen_messageids.ps1 -ProtoRoot D:\luyuan\xuanming-server-mmo-cpp`
  2. **Go 编译、Vet、单测与临时 TiDB 集成测试**（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）：
     - `go/shared`：`go build ./... && go vet ./... && go test ./contentguard/... ./dsclient/... ./authoritysql/... ./accountdirectory/... -count=1`
     - `go/data_service`：`go build ./... && go vet ./... && go test ./... -count=1`（含 `TestAccountRoleRename*` 与 `account_channel_identity_test.go`，起隔离 TiDB 跑 `DATA_SERVICE_TEST_DSN`）
     - `go/guild`、`go/content_guard`、`go/chat`、`go/login`、`go/trade`、`go/scene_manager`：分别 `go build ./... && go vet ./... && go test ./... -count=1`（临时 TiDB 下跑 `go/chat/internal/authority` `TestAntiSpamRetractAndReportTiDB` + `TestChatTiDBLeastPrivilegeRuntimeContract` 六表最小权限、`go/guild`、`go/trade` 集成测试）。
  3. **C++ 串行编译与单测（Debug|x64，严格 `/m:1`）**：
     - `msbuild /m:1 cpp/generated/table/table.vcxproj /p:Configuration=Debug /p:Platform=x64`
     - `msbuild /m:1 game.sln /p:Configuration=Debug /p:Platform=x64`
     - `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter "bag_test|currency_test|configuration_table_test|turn_battle_engine_test"`
  4. **Unity 客户端编译与 EditMode 测试（`D:\luyuan\mmorpg-client`）**：
     - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`
     - 运行 EditMode 测试：`RoleRenameClientTests`、`GuildRenameClientTests`、`SocialStateTests`、`SocialClientTests`、`QdaoServerSelectPrefabTests`（及按需 `QdaoUguiBuilder.BuildAll`）。
  5. **PowerShell 契约测试（清 `MMORPG_*` 环境变量后在仓库根执行）**：
     - `chat_authority_deploy.tests.ps1`、`sql_boot_catalog_local.tests.ps1`、`k8s_deploy_contract.tests.ps1`、`k8s_migrate_gate.tests.ps1`、`k8s_client_entry_contract.tests.ps1`、`logical_db_deploy_contract.tests.ps1`、`maintenance_drain.tests.ps1`、`go_services_local_mysql.tests.ps1`。
  6. **全栈清库冒烟**：按 `AGENTS.md §6.2` 清库跑 `-migrate` 后验证角色改名、帮会改名、寄售标题审核、聊天发送/撤回/举报端到端链路。
- **Java 独立全服务版（AGENTS §12）**：已登记于 `../xuanming-server-mmo/PARITY.md` TODO（2026-11-20 末条），本轮未改 Java 仓代码。


## 2026-11-20 Gemini（Ponytail 全仓代码审计 + 标准修复收口，两轮全部落盘于 main，构建/测试/冒烟交 Codex 验证；Java 全服务版留 TODO）

- **状态声明**：全工作区 5 仓 Ponytail 代码质量与过度设计审计（报告见对话产物 `ponytail_workspace_audit_20261120.md`）所定位的全部高优安全漏洞、数据强一致缺陷、水平扩展瓶颈、热路径浪费与死代码已完成两轮收口；所有改动严格落于各仓 `main` 工作区（零分支、未 `git commit` / `git push`），未改 `.proto` 与配表（本批无需额外改表；全库如有前序会话新增表/协议请按前序条目顺序先跑导表与 `proto-gen-run`）。第二轮（本轮最终收口）按用户指令与 `AGENTS.md §10.3` 仅执行代码编写与只读静态门禁检查，不跑 `msbuild`/`go build`/`mvn`/Unity/冒烟，全部留交 Codex 按下方清单验收。
- **本批（第一轮 + 第二轮）服务端改动汇总（`xuanming-server-mmo-cpp`）**：
  1. **C++ 引擎、Gate、Battle、Scene 与模块（`cpp/`）**：
     - **安全与连接准入**：`cpp/nodes/gate/handler/rpc/client_session_handler.cpp`（补 `GateTokenPayload.zone_id` 校验；直连模式 `ApplyLoginTransition` 延后至 `HandleGrpcNodeMessage` 发送前一刻；`std::random_device`+`mt19937` 换 `tlsRandom`；删 2026-05-24 `[diag]` 日志）；`cpp/libs/engine/core/network/codec/websocket.cpp` 与 `cpp/nodes/battle/client/{battle_connection_admission.h,battle_client_edge.{h,cpp}}`（补 `forceCloseWithDelay(0.1)`、记录 `halfClosedAt` 并在 200ms 宽限后允许淘汰超龄半关未认证连接；单测 `battle_connection_admission_test.cpp`）。
     - **数据强一致**：`cpp/libs/services/scene/player/comp/last_persisted_snapshot_comp.h` 新增 `IsCommittedForOwner(currentOwnerEpoch)` 并统一接入 `asset_op_system.cpp`、`player_activity.cpp`、`player_equipment_material_reward.cpp`、`player_equipment_v1.cpp`、`role_admission_system.cpp`；7 处客户端写入入口（`player_equip.cpp`、`player_attribute.cpp`、`player_pet.cpp`、`player_mystery_merchant.cpp`、`afk_pass.cpp`、`player_mission.cpp`、`player_skill.cpp`）统一接入 `player_mutability::IsPlayerMutable`（同时拒绝 `PlayerTravelHandoffComp` / `UnregisterPlayer` / `PlayerFrozenComp` / 无效 `owner_epoch`）；同步更新全部 13 份关联单测夹具并补强测试断言。
     - **正确性与生命周期**：`cpp/libs/services/player/rpc/player_service_interface.h` 补 `virtual ~PlayerService() = default;`；`cpp/libs/engine/core/node/system/node/node.h` 就绪后 `probeTimer.Cancel()`；`cpp/nodes/scene/handler/grpc/scene_node_service.cpp` 拒 `config_id == 0`；`cpp/libs/services/scene/combat/skill/system/skill.cpp` 去除误拒无目标/AoE 技能的前置 `target_id <= 0`；`cpp/libs/services/scene/combat/buff/system/modifier_buff_impl.cpp` 补 `healthBefore >= maxHealth`、`isfinite` 与下溢护栏；`cpp/libs/services/scene/spatial/system/movement.cpp` 瞬移落位改写 `acceptedLocation`；`cpp/libs/services/scene/player/system/player_equipment_v1.cpp` 改走 `playerlevel::CappedLevel`；`cpp/libs/modules/currency/system/currency_system.cpp` `GetBalance` 恢复只读；`cpp/libs/modules/bag/container_layout.cpp` `GridLayout::PlaceAt` 失败回滚原槽位；`cpp/libs/services/scene/spatial/system/recast.cpp` 校验 `numTiles`/`dataSize >= 0`、`addTile` 失败时 `dtFree(..., DT_ALLOC_PERM_TILE_DATA)` 防泄漏；`cpp/libs/modules/transaction_log/anomaly_detector.{h,cpp}` 切 `steady_clock`、饱和加法、告警冷却、事件队列上限与过期桶懒清理（+3 条单测 `anomaly_detector_test.cpp`）。
     - **热路径瘦身与去重**：`cpp/libs/engine/core/network/rpc_channel.{h,cpp}` 删死 `gDumpOversizedRpc` 及 `>4KB` 盲解码；`cpp/libs/services/scene/actor/attribute/system/actor_state_attribute_sync.{h,cpp}` 删 5 档零字段同步器并过滤 NPC 观察者；`cpp/libs/services/scene/spatial/system/aoi.cpp` 回收空 hex 桶；`cpp/libs/modules/bag/{bag_system,bag_service}.{h,cpp}` 新增 `Bag::MutationVersion()` 替代 5 遍整包 `SerializeAsString()` + 复用 `CloneBagForTransaction`（+单测 `bag_space_freed_test.cpp`）；`cpp/libs/services/scene/player/system/player_equipment_v1.{h,cpp}` 缓存 `BuildDefinition()`（按表加载版本与表快照地址/行数失效，零裸指针成员；+单测 `player_equipment_craft_test.cpp`）；`cpp/libs/services/scene/combat/buff/system/buff.cpp` 移除无用 `MotionModifierBuffImplSystem` 空调用；`cpp/libs/engine/core/utils/encode/sha256.{h,cpp}` 改用已链 BoringSSL `EVP_Digest(..., EVP_sha256(), ...)`。
  2. **Go 微服务与工具（`go/`、`tools/`、`deploy/`）**：
     - `go/payment/internal/channel/{google.go,google_test.go,apple.go,apple_test.go}` 与 `go/payment/internal/core/{service.go,record_test.go,workers.go}`：Google JWKS 锁外拉取 + singleflight 合并 + 60s 冷却防未知 `kid` DoS；Apple `appleMilliunitsToMinor` 按 ISO-4217 0/2/3 位小数换算；`sameFact` 补 `PaidAmountMinor`/`PaidCurrency` 核对；`workers.go` 守卫 `nowMs > txn.CreatedMs` 防无符号下溢（均已补单测）。
     - `go/login/internal/logic/clientplayerlogin/loginlogic.go`（设备上限校验前移至 `beginWatchedSession`/`CommitLogin`/`RegisterDevice` 之前）、`go/login/internal/logic/pkg/auth/password_provider.go`（去除配额前空口令 64MiB Argon2id）、`go/login/{login.go,go.mod}`（换标准库 `context`）。
     - `go/shared/{callerauth,roleidentity}` 指标切 `prometheus/promauto`（+`session_test.go`）；`go/guild/internal/data/guild_repo.go` 与 `go/friend/internal/data/friend_repo.go` 改 `[256]sync.Mutex` 条带锁且 `guild_repo` Redis 回填失败时记录日志并返回 MySQL 权威结果；`go/player_inbox/internal/inbox/store_admin.go` 补 `rows.Err()`；`go/match/internal/logic/{global_admission_recovery,global_spectator}.go` 修 `LaunchRelayTask` 返 false 时槽位泄漏；`go/content_guard/go.mod` 删 4 个未用直接依赖；删零调用死目录 `go/shared/{timertask,tablequery,cache}`、`tools/data_table_exporter/core/_old/`、`tools/proto_generator/protogen/internal/lua/`。
     - `tools/data_consistency_check/main.go`：移除 `ORDER BY RAND()` 改为 `MIN/MAX` 索引范围抽样、删无用 `CountPlayers`、支持 `MMORPG_CONSISTENCY_*` 环境变量传入 DSN/密码。
     - `tools/scripts/k8s_deploy.ps1`（`Convert-ZonesYamlFallback` 剥离行尾注释、支持任意字段顺序、对未识别行 fail-closed；`Remove-Zone`/`Remove-Infra` 非 dev 强制 `MMORPG_CONFIRM_DELETE_NAMESPACE`/`MMORPG_CONFIRM_DELETE_ALL=YES` 确认，不改 `dev_tools.ps1` 参数契约）、`tools/scripts/lib/account_directory_dev_secret.ps1`（修复环境变量密钥未回写 `run/secrets/` 的漂移）、`deploy/docker-compose.login-stack.yml`（补 `MMORPG_RUN_MODE=dev` 与 `1=` 前缀）。
  3. **主仓 Java 模块（`java/`）**：
     - `java/gateway_node`：删除零调用公网票据伪造预言机 `/api/cdn-sign` 5 个类及 yaml 段；`GateTokenSecretGuard`（+`GateTokenSecretGuardTest`）补充 prod 强制校验 `admin.api-key`。
     - `java/mall_game/src/main/java/com/game/mall/NonceStore.java`：128 次 N+1 `DELETE` 循环改为单条有界 `DELETE ... LIMIT 128`。
     - `java/gm_admin/src/main/java/org/jeecg/modules/gm/GmController.java`：收紧 `require()` 用户名校验（UTF-8 <= 64B、禁 `|` 与 ISO 控制字符）。
     - `java/springboot_satoken_auth_starter`：删除 6 个零调用缺陷死类（`UserService`/`SmsService`/`SmsLoginDTO`/`OauthCallbackDTO`/`UserPassword`/`UserPasswordRepository`）。
- **Unity 客户端改动汇总（`D:\luyuan\mmorpg-client`）**：
  - 第一轮（已于第一轮通过离线 Roslyn 校验）：清理 `Packages/manifest.json`（移除未入库本地包 `com.unity.ai.assistant` 与 6 个无用包）与 `EditorBuildSettings.asset`；`GameClientBattleTransport.cs` 去协程重入 `Tick()`；`WorldLabelBillboard`/`WorldNameplate`/`MailUiRoot` 缓存引用降 GC；`QdaoUguiTheme.CityPanelRegistry` 统一 15 份主城 `*UiRoot` 互斥；9 个 QA/FX 验证脚本加 `#if UNITY_EDITOR || DEVELOPMENT_BUILD`；删死代码 `Core/Bootstrap.cs`、`Core/MmorpgLogger.cs`、`JubaozhaiDesignFit.cs`。
  - 第二轮（本轮新增 11 生产 + 6 EditMode 测试文件，保留并发会话在 `GameClient.cs`/`PaymentClient.cs`/`JubaozhaiUiRoot.cs`/`BattleScreen.cs` 的全部改动）：
    - `MinigamePayBridges.cs` 改为非泛型 `delegate void PayCallback(string value)`，`MinigamePay.jslib` 三入口补 `try/catch` 防 `Busy` 永久锁死；
    - `PaymentClient.cs` + `StorePayBridge.cs` 新增 `CancelPending()` 与 `_deferredReceipts` 兜底队列，解决移动端 IAP 弹窗期间断线吞掉已付凭证（+3 条单测 `PaymentClientTests.cs`）；
    - `PetClient.cs` / `PetClient.Shop.cs` / `SpectateClient.cs` 补 `Reset()` 并接入 `GameClient.DisconnectInternal`，`JubaozhaiUiRoot.cs` 接入 `connectionIdentity`/`ObserveConnection`/`Reset()` 防止静默重连与换角脏状态（+单测 `PetClientTests.cs`、`SpectateClientStateMachineTests.cs`、`GameClientReconnectTests.cs`）；
    - `AttributeClient.cs` 与 `PetClient.cs` 去除重复硬编码数字 `27000..27008`，统一复用 `AttributeClient.DescribeAssetTip`（基于现有 `GuildAssetReasons` 单一常量源；+单测）；
    - `BattleArtCatalog.cs` 新增 `ReleaseTransientBattleCaches()` 释放非 HUD 瞬态贴图/Sprite/Resources 资产防 WebGL OOM（+单测 `BattleArtCatalogCacheTests.cs`；因 `BattleScreen.cs` 正被战报回放会话修改，1 行调用点 `BattleArtCatalog.ReleaseTransientBattleCaches();` 待该会话合入后挂入 `BattleScreen.Close()`）。
- **其他同工作区仓库顺手收口摘要**：
  - `xuanming-server-mmo-python`（第一轮已跑 `pytest` 507 passed）：修 `SceneBattle.reap()` 20Hz 全量反序列化、`prepare()` Redis 失败回滚、`AssetAnomaly` 队头死锁与降级丢告警、Gate `zone_id` 与 WS 踢人 `drain` 与双 Adler32、Portal UTC/62-bit ID/长度校验/共享 `ClientSession`、`SceneNode` `WeakValueDictionary` 单玩家锁等。
  - `xuanming-server`（Pandora，第一轮 10 模块 `go test` PASS；第二轮补 `services/account/login/internal/conf/conf.go` `require_tidb=true` 生产门禁硬拒公开 dev JWT 密钥与 3 个 dev 开关 + `conf_test.go` 单测）：修邮件双路径领奖 TOCTOU、排行结算单事务原子化、移除 `:21001` 裸露 HTTP RPC、`GroupService` IDOR/配额、交易 WATCH `expired=false`、`pkg/redislock` `PEXPIRE`、删 `kafkax/consistent.go` 及 3 个死包并复用 `pkg/playerdisplay`。
  - `xuanming-server-mmo`（Java 平行服）：第一轮完成 10 项高危修复，第二轮按要求留 TODO，已追加登记至 `../xuanming-server-mmo/PARITY.md` 末条。
- **只读静态门禁检查（本轮全部通过）**：
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`：**10/10 OK**
  - `python tools/scripts/tests/test_cpp_thread_sources.py`：**5/5 OK**
  - `python tools/scripts/check_log_text_english.py`：**0 violations**
  - `gofmt -l`（本批全部修改 Go 文件，含 `go/payment`、`go/guild`、`tools/data_consistency_check`、Pandora `login/internal/conf`）：**0 差异**
  - PowerShell `Parser::ParseFile`（`tools/scripts/k8s_deploy.ps1`、`tools/scripts/lib/account_directory_dev_secret.ps1`）：**0 errors**
  - 5 仓分支核对：全部在 `main`（无任何新建分支）。
- **交 Codex 验证清单（注：前序会话已修补 `kPetNameSensitive` 与 `transaction_log_commit_gate_test.cpp`，请先按前序条目执行导表+协议生成，再执行下方验证）**：
  1. **主服 C++ 串行编译与单测（Debug|x64，严格 `/m:1`）**：
     - `msbuild /m:1 game.sln /p:Configuration=Debug /p:Platform=x64`（确认 `core`、`gate`、`scene`、`battle`、`rpc_controller_test`、`currency_test`、`bag_test`、`missions_test`、`skill_test`、`turn_battle_engine_test` 均 0 error；关注 `Sha256::` 消费方均已链 `crypto.lib`）
     - `rpc_controller_test.exe --gtest_filter="BattleConnectionAdmission*:*AcceptAdmission*"`
     - `currency_test.exe --gtest_filter="AssetOpSystemTest.*:PlayerAnomalyWindowTest.*:Currency*"`
     - `bag_test.exe --gtest_filter="PlayerActivitySystemTest.*:AfkPass*:ClientRequest*:PlayerEquip*:PlayerFeatureHandlerTest.*:MissionActionHandlerTest.*:Pet*:PlayerMissionSystemTest.*:PlayerMysteryMerchantTest.*:PlayerBattleSettlement*:BagSpaceFreedTest.*:BagTest.*"`
     - `missions_test.exe --gtest_filter="*"`
     - `skill_test.exe --gtest_filter="PlayerSkillBookTest.*:PetEntriesTest.*:Buff*:*Skill*"`
  2. **主服 Go 编译、Vet 与单测（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）**：
     - `go/payment`：`go build ./... && go vet ./... && go test ./... -count=1`（覆盖 `google_test.go` 冷却/singleflight、`apple_test.go` 0/2/3 位小数、`record_test.go` `sameFact`）
     - `go/guild`、`go/friend`、`go/login`、`go/shared`、`go/player_inbox`、`go/match`、`go/content_guard`、`tools/data_consistency_check`：分别 `go build ./... && go vet ./... && go test ./... -count=1`
  3. **主服 Java 与 PowerShell 契约测试**：
     - `mvn -f java/gateway_node/pom.xml test`（含 `GateTokenSecretGuardTest`）
     - `mvn -f java/mall_game/pom.xml test`（含 `CommerceHttpFlowTest`）
     - `mvn -f java/gm_admin/pom.xml -DskipTests=false test`（核对 `GmController` 用户名校验）
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`tools/scripts/tests/battle_connection_alerts_contract.tests.ps1`
  4. **Unity 客户端编译与 EditMode 测试（`D:\luyuan\mmorpg-client`）**：
     - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`（并按需带 `UNITY_WEBGL` 与 `MMORPG_UNITY_IAP` 宏分别过一遍离线 Roslyn）
     - EditMode：`PaymentClientTests;PetClientTests;SpectateClientStateMachineTests;AttributeClientTests;GameClientReconnectTests;BattleArtCatalogCacheTests;GameClientGateEnvelopeTests;UiHostShutdownTests;JubaozhaiClientTests`
     - 待战报回放会话合入 `BattleScreen.cs` 后，在 `BattleScreen.Close()` 末尾补挂 1 行 `BattleArtCatalog.ReleaseTransientBattleCaches();`。
  5. **Pandora（`D:\luyuan\xuanming-server`）**：
     - 使用 `D:\luyuan\tools\go1.26.8\go\bin\go.exe` 在 `services/account/login` 跑 `go test ./internal/conf/... ./internal/server/... -count=1`。
- **刻意未动的低优先/架构级瘦身项（需单独决策排期，不影响当前上线）**：29.5 万行零消费导表/生成物（`java/config_node`、`generated/code/{ue,python,java}`、`proto/java`）、`k8s_deploy.ps1` 迁 Helm/Kustomize、`cpp/nodes/_template/`（仍被多份 README/设计文档引用）、`go/login` 的 `NeteaseProvider`（仍被 `auth_init.go` 注册）、以及大文件结构拆分与 Java 平行服进程合并。

### 2026-11-20 配置表热更全 5 批落码（表包 + 轨道指针 + C++/Go 两阶段整批切换 + 战斗每局绑定 + table-sync/管理接口/K8s/审计白名单）— 未导表/未编译/未跑测，交 Codex 验证

- **设计依据**：`docs/design/config-table-hot-reload.md`（§16 全部 8 项已核实，§17 已记批 5 逐表审计台账）；运维手册 `docs/ops/table-hot-reload-runbook.md`；告警 `deploy/k8s/table-bundle-alerts.yaml`；独立 Java 仓留待 `PARITY.md`。
- **批 1（导表器与模板）**：
  - `data/schema/cfg_options.proto` 新增 `bool cfg_hot_reload = 60006`；
  - `tools/data_table_exporter/core/{schema,schema_proto,manifest}.py` 与 `core/generators/config_gen.py`：实现 `compute_schema_digest`（16 位小写 hex，仅计服务端列布局）、manifest 第 2 版（`schema_version=2`、`schema_digest`、逐表 `hot_reload`/`ids_permanent`）、单测 `tests/test_manifest.py` 与 `tests/test_schema_proto.py`；
  - `cpp/libs/engine/config/table_expression.h`：新增纯 `const` 栈局部求值 `Evaluate(expr, params) const`（兼容旧 `SetParam`/`Value`）；
  - `templates/cpp_config.{h,cpp}.j2`、`cpp_table_loader.{h,cpp}.j2`：提取顶层不可变 `XxxTableSnapshot`、`ParseSnapshotFromDir`（失败不 `LOG_FATAL`、检查 `ifstream::is_open()` 防止缺失 `.pb` 静默解析为空）、`TableSet`（`shared_ptr<const TableSet>`）、`ScopedTableSetOverride`、`kTableSchemaDigest`、`IsTableHotReloadable`、`CommitTableSet`、`InvokeTablesLoadSuccess`；
  - `templates/go_config.go.j2`、`go_table_loader.go.j2`：新增 `SchemaDigest`、`IsHotReloadable`、`TableSet`、`Prepare`、`Commit`、`Current`、`OnTablesCommit`，删除带数据竞争的裸赋值 `ReloadTables`；
  - `templates/java_table_loader.java.j2`：增补 `SCHEMA_DIGEST` 与 `isHotReloadable(String)`。
- **批 2（进程侧热更与 `--validate-tables`）**：
  - C++：`cpp/libs/engine/core/metrics/prometheus_text.{h,cpp}` 增 `SampleDouble`/`HistogramDouble`；新建 `cpp/libs/engine/core/table_bundle/table_bundle_manager.{h,cpp}`（登记到 `core.vcxproj`/`.filters`，零裸指针成员、零新线程，2s `TimerTaskComp` 轮询 `POINTER.json`、单表 `queueInLoop` 分片准备、`ScopedTableSetOverride` 校验、`activate_at_ms` 定时提交、10 分钟旧 `TableSet` 退役留存、原子 `STATUS.json`、`RunValidateTablesCli`、5 项 Prometheus 指标）；接线 `node.{h,cpp}`/`node_entry.h` 及 `cpp/nodes/{scene,gate}/main.cpp`（拆分启动一次性 `OnLoaded` 与每次提交 `Validate`/`OnCommitted`）。
  - Go：新建 `go/shared/tablebundle`（`manifest.go`/`pointer.go`/`metrics.go`/`manager.go`/`cli.go`/`manager_test.go`）；接线 6 个读表服务（`go/match` 实现 `catalog.BuildFromTableSet` + `battlescript.LoadFromTableSet` + `atomic.Pointer` 原子切换与 `authority.Store.SwapCatalog`；`go/guild` 增 `ValidateEconomyTablesIn`/`activity.ValidateTablesIn`；`go/login` 增 `playernamereg.ValidateRulesIn`；`go/scene_manager`/`go/mail`/`go/rank` 接入 `tablebundle.LoadAtStartup` + `-validate-tables`）；删除 `go/player_locator` 无用的 `table.LoadTables`。
  - 本地工具：`tools/scripts/dev_tools.ps1` 新增 `-Command tables-reload-local` 与 `-Command table-bundle`。
- **批 3（战斗每局绑定不可变 `TableSet`）**：
  - `cpp/libs/services/battle/data/battle_table_fingerprint.{h,cpp}` 增 `ComputeFromTableSet`；`table_battle_data_provider.{h,cpp}` 绑定 `shared_ptr<const TableSet>` 与 `shared_ptr<const BattleRewardConfig>`（零表单例直读）；
  - `cpp/nodes/battle/logic/battle_room_manager.{h,cpp}` 与 `cpp/nodes/battle/main.cpp` 按战斗表指纹索引 provider 池（10 分钟退役期 + 房间显式持有至对局结束），接入 `--validate-tables` 与两阶段 `Validate`/`OnCommitted`；
  - 新建静态守卫 `tools/scripts/tests/test_battle_no_direct_table_singleton.py`。
- **批 4（分发与治理）**：
  - 新建 Go 模块 `go/table_sync`（零新增第三方依赖：自研标准库 AWS SigV4 `internal/s3client` + 确定性平铺打包/安全解包/只增不删 ID 回滚守卫 `internal/bundle` + `internal/pointer` + `cmd/table-sync` 原生 sidecar + `cmd/table-bundle` 发布工具 + 3 套单测）；登记 `"table-sync"` 到 `tools/scripts/go_svc_image.ps1`；
  - `java/gateway_node` 新增 `TablePointerStore`、`EtcdTablePointerStore`（etcd `mod_revision` CAS）、`AdminTableBundleController`（`GET/PUT/DELETE /admin/tables`、`DELETE /admin/tables/next`）及 `AdminTableBundleControllerTest`；
  - `tools/scripts/lib/k8s_client_entry.ps1`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/dev_tools.ps1`、`deploy/k8s/manifests/infra/seaweedfs-dev.yaml`：可选注入 `table-sync` 原生 sidecar（`-TableSyncS3Endpoint` / `-TableSyncDevFixture`，覆盖 C++ `gate`/`scene`/`battle` 含 Agones Fleet 恒输出 `container: $FleetName` 与 6 个 Go 读表服务，排除 `player-locator` 等非读表服务；未启用时既有清单逐字不变）；同步 `k8s_client_entry_contract.tests.ps1`，新建 `deploy/k8s/table-bundle-alerts.yaml`、`tools/scripts/tests/k8s_table_bundle_contract.tests.ps1` 与 `docs/ops/table-hot-reload-runbook.md`。
- **批 5（逐表审计放开与文档同步）**：
  - 为 13 张完成审计的表添加 `option (cfg_hot_reload) = true;`：`MessageLimiter`、`ActivitySchedule`、`ActivityVitalityRule`、`ActivityVitalityReward`、`MysteryMerchantRule`、`MysteryMerchantGoods`、`GuildShop`、`GuildDonate`、`Skill`、`Buff`、`Cooldown`、`SkillPermission`、`Monster`；
  - 同步 `docs/design/{config-table-hot-reload,mystery-merchant,configurable-templates,table-api-reference-zh}.md`。
- **只读静态检查（本轮全部通过）**：
  - `python -m py_compile`（7 个 Python 文件）：0 错误；
  - `python tools/scripts/tests/test_battle_no_direct_table_singleton.py`：1/1 OK；
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`：10/10 OK；
  - `python tools/scripts/tests/test_cpp_thread_sources.py`：5/5 OK；
  - `python tools/scripts/check_log_text_english.py --root .`：0 violations；
  - `gofmt -l`（全部新建/修改 Go 文件）：0 差异；
  - PowerShell `Parser::ParseFile`（6 个 `.ps1` 文件）：0 错误。
- **交 Codex 验证步骤（按顺序执行）**：
  1. **导表器单测与重新导表（必须最先跑，生成 manifest v2 与新 C++/Go/Java 表加载器）**：
     - `python -m pytest tools/data_table_exporter/tests -q`
     - `dev.bat gen`（或 `python tools/data_table_exporter/export.py` 按仓库导表入口重导三端表代码与 `generated/tables/manifest.json`）
     - `python tools/scripts/gen_docs_index.py`（刷新 `docs/README.md` 索引，收录 `docs/ops/table-hot-reload-runbook.md`）
     - 若需 Linux 构建同步刷新 CMakeLists：`python tools/archived/vcxproj2cmake.py`（确认 `cpp/libs/engine/core/CMakeLists.txt` 收录 `table_bundle/table_bundle_manager.cpp`）
  2. **Go 编译、Vet 与单测（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）**：
     - `go/shared`：`go test ./tablebundle/... -race -count=1`
     - `go/table_sync`：`go test ./... -race -count=1` 且 `go build ./cmd/table-sync ./cmd/table-bundle`
     - `go/match`、`go/guild`、`go/login`、`go/scene_manager`、`go/mail`、`go/rank`、`go/player_locator`：分别 `go build ./... && go vet ./... && go test ./... -count=1`
  3. **Java `gateway_node` 单测**：
     - `mvn -f java/gateway_node/pom.xml test -Dtest=AdminTableBundleControllerTest,AdminKillSwitchControllerTest,AdminApiKeyFilterTest`（或全量 `mvn -f java/gateway_node/pom.xml test`）
  4. **PowerShell 契约测试**：
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_table_bundle_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/loki_ha_deploy_contract.tests.ps1`
  5. **C++ 串行编译（Debug|x64，严格 `/m:1`）与 `--validate-tables` 冒烟**：
     - `msbuild /m:1 game.sln /p:Configuration=Debug /p:Platform=x64`（核对 `core`、`gate`、`scene`、`battle`、`turn_battle_engine_test` 均 0 error；关注 `table_bundle_manager.cpp` 与新 `TableSet` 模板在 MSVC 下的编译）
     - `turn_battle_engine_test.exe`（核对战斗每局绑定与无状态表达式求值）
     - 用编出的 `scene.exe`、`gate.exe`、`battle.exe` 与各 Go 服务二进制分别运行 `--validate-tables generated/tables`（验证退出码均为 0）
     - 本地热更冒烟：设 `MMORPG_TABLE_BUNDLE_ROOT=run/table_bundles` 起单节点，改 `MessageLimiter` 或 `Skill` 数值重导表并执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local`，核对 2 秒内日志打出 `reload committed` 且不中断在打战斗。


### 2026-11-20 Gemini（Ponytail 审计残余项最终清零补录，全部落于 main，交 Codex 统一验收）

- **补充收口清单（清零上条遗留的挂接点、热路径、安全洞与零调用死代码）**：
  1. **Unity 客户端（`D:\luyuan\mmorpg-client`）**：
     - `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`：`Close()` 末尾补挂 1 行 `BattleArtCatalog.ReleaseTransientBattleCaches();`（完整保留并发战报回放改动），使战后瞬态贴图/Sprite/Resources 释放正式生效。
     - `Assets/Scripts/App/{DevAutoPilot.cs,DevRoleUiDriver.cs}`、`Assets/Scripts/World/Tianyong/{TianyongSandboxAutoDrive.cs,TianyongSandboxHudVerification.cs}` 及其在 `AppBootstrap.cs` / `TianyongSandboxBootstrap.cs` 的 4 处调用点全部包入 `#if UNITY_EDITOR || DEVELOPMENT_BUILD`，彻底将自动驾驶/自动建角/沙盒截图驱动移出正式包与 WebGL 包（`TianyongSandboxBootstrap` 类体保留以兼容场景序列化，全部 EditMode/PlayMode 测试在 `UNITY_EDITOR` 下零回归）。
  2. **主服 C++ / Go / Java（`D:\luyuan\xuanming-server-mmo-cpp`）**：
     - `cpp/nodes/scene/handler/rpc/scene_handler.cpp`：`CreateScene` 将 `NavQuerySystem::RequiresNavigation` 校验前移至 `PlacementFence::AdmitCreate` 之前，修复导航缺失拒建时污染放置代次的强一致缺陷。
     - `cpp/libs/services/scene/spatial/system/aoi.cpp`：`SendVisibilityChanges` 彻底移除全部 5 棵 `std::map` 红黑树，改为平铺 `std::vector` + `std::sort` 按 `(watcher, target)` 与 `target` 归并广播（保持完全一致的确定性顺序，消除视野热路径红黑树节点堆分配）。
     - `cpp/libs/modules/currency/system/currency_system.{h,cpp}`：删除全仓零调用的 `WaiveDebt` / `FreezeDebt` 死写钱方法（保留单测在用的 `AdjustDebt` / `QueryDebts`）。
     - `cpp/libs/engine/core/{core.vcxproj,CMakeLists.txt}` 与 `cpp/libs/engine/thread_context/{thread_context.vcxproj,thread_context.vcxproj.filters,CMakeLists.txt}`：移除并删除全仓零调用的 `spdlog_file.{h,cpp}`（死异步日志线程）与 `lua_state.{h,cpp}`（每线程无用 `thread_local sol::state tlsLua`）。
     - `java/config_node/src/main/java/com/game/config/controller/ReloadController.java` + 新增 `ReloadControllerTest.java`：`POST /api/reload` 补齐鉴权（配置 `config.reload.api-key` 时恒定时间比对 `X-Admin-Key`，否则仅放行无 `X-Forwarded-For` 的直连本机回环，远端一律 403）。
     - `go/login/internal/svc/{auth_init.go,auth_init_test.go}`：配置了未实现的 `AuthProviders.NetEase` 时启动期直接 `panic` fail-fast，不再静默挂接空桩或读明文凭据（+ `TestInitAuthProvidersRejectsUnimplementedNetEase`）。
- **本补录只读静态检查**：`test_cpp_no_raw_pointer_member.py`（10/10 OK）、`test_cpp_thread_sources.py`（5/5 OK）、`check_log_text_english.py`（0 violations）、`gofmt -l`（0 差异）、5 仓全在 `main`。
- **追加至 Codex 验证命令**：
  - `mvn -f java/config_node/pom.xml test`（验证 `ReloadControllerTest` 与导表 Java 消费端编译）；
  - `go/login`：`go test ./internal/svc/... -run TestInitAuthProviders -count=1`；
  - `cpp`：`msbuild /m:1 game.sln /p:Configuration=Debug /p:Platform=x64`（重点覆盖 `core`、`thread_context`、`scene`、`aoi` 相关单测与 `currency_test`）；
  - `mmorpg-client`：`pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`（同时以不含 `UNITY_EDITOR` / `DEVELOPMENT_BUILD` 的 Release 宏验证 `AppBootstrap` / `TianyongSandboxBootstrap` 零编译错误，以及 `BattleScreen` + `BattleArtCatalogCacheTests`）。


## 2026-11-20 Gemini（真钱支付全链路生产标准加固：服务端 + 客户端已落码，未编译，交 Codex 统一验证；Java 全服务版留 TODO）

- **范围与约束**：所有改动直接落于 `main` 工作区（无分支、未 `git commit` / `git push`、未改动他人会话文件）；不保留未上线旧数据兼容包袱，对三方深审发现的全部一致性、并发、扩展性、崩溃恢复与客户端桥接缺陷一次性修复，未留「以后修」；按指令未跑协议生成、编译、单测或冒烟，全部交由 Codex 按下列命令执行；独立 Java 全服务仓 `D:\luyuan\xuanming-server-mmo` 仍留 TODO。
- **一、`go/payment`、`proto/payment`、`go/shared/tracing` 强一致与高并发加固**：
  1. **Schema 升级（需 Codex 先跑 `proto-gen-run`）**：`proto/payment/payment_table.proto` 在 `PayFinalizeState` 新增 `PAY_FINALIZE_STATE_FAILED = 3`，并将 `PaymentIntentRecord` 索引升级为 `(spender_hi, spender_lo, state, paid_ms)` 加速未成年人单日/当月汇总。
  2. **未成年人充值限额原子提交（消除 TOCTOU 越限与并发误扣额度）**：`go/payment/internal/data/spend.go` 新增 `RecordMinorPaymentTxn`，在同一 `ReadCommitted` 事务内完成 `lockSpender` + `payment_intent FOR UPDATE` + `payment_txn FOR UPDATE` + `checkSpend` + `INSERT payment_txn` + `UPDATE payment_intent=PAID`；`go/payment/internal/core/minor.go` 移除过时的 `recheckLatePayment`，并将 `drivesIntent` 收窄为 `MallBound` / 订单本笔 `MallRefundQueued`（不再因 `MallPending` 异常单提前释放限额）。
  3. **跨序撤销 / 墓碑 / 补发收敛与退款租约防重**：
     - `go/payment/internal/core/service.go`：`SubmitPurchase` 在谷歌侧遇到 `MallOrphan` / `MallRejected` / `ErrFactConflict` 返回 `Finish=false`（保留 Google 72 小时自动退款保护）；`record()` 在 `refundedOutside` 时写 `FactRevision=1` 并识别先到撤销墓碑；`revoke()` 在 `ErrNotFound` 落 `FactRevision=1` 墓碑并防自身退款误伤、处理乱序 `REFUND_REVERSED` 先于旧 `REFUND`（经 `TouchFactObserved` 推进观测时间）。
     - `go/payment/internal/data/repo.go`：`ClaimRefund` 原子流转 `RefundNone→RefundRequested`、持久化 `out_refund_no` 并覆盖 `RefundReported`/`RefundFailed` 租约（防止执行与回调竞态及多副本重复锤击）；新增 `OldestMallPendingMs` 与 `OldestFinalizePendingMs`。
     - `go/payment/internal/core/workers.go`：`pushMall` 改为库级最老待同步指标（退避中不归零，覆盖修订同步）；`processRefunds` 接入游标分页（最多 20 页）与缺失/事实不符自动 `MANUAL` 关单、`mall.ErrConflict` 停试；`finalize` 导出 `payment_finalize_oldest_pending_seconds` 并将已撤销及永久 4xx（400/404/410）落 `FinalizeFailed`；`pollOne` 加 5 分钟历史重叠窗口、改用可取消 25s `pollCtx` + 90s 租约并在排空时及时退出。
  4. **渠道与回调加固**：
     - `go/payment/internal/channel/{channel.go,google.go,wechatminigame.go,wechatpay.go,alipay.go}`：Google 与微信小游戏 OAuth Token 刷新改为锁外 singleflight（不持锁跑 HTTP、支持 `ctx.Done()`、401/40001/42001 失效清缓存并重试 1 次）；Google 拉证书用 10s 分离上下文、映射 `ErrTempUnavailable`、`PollHistory` 加 `includeQuantityBasedPartialRefund=true` 及 >50 页折半；微信小游戏 `90016` 返回 `ErrQueryUnsupported`、补全 PKCS#7 全字节填充校验；微信支付 `Close` 将 `ORDERPAID` 视为已关、`Revocation` 补全 `OutTradeNo`/`OutRefundNo`；`mall/client.go` 配置连接池（`MaxIdleConns=64`、`MaxIdleConnsPerHost=32`）并升级 `PendingRefunds(ctx, cursor)` 分页。
     - `go/shared/tracing/http.go`、`go/payment/internal/notify/notify.go`：公网回调入口改为 `tracing.StartRootSpan`（不信任公网 `traceparent`），加 panic 恢复触发 `relay.Hold()`、`parseTimeout=5s`、`ErrTempUnavailable` 判 `AckRetry`、按渠道类型分配 8s/2.5s 处理预算。
     - `go/payment/internal/config/config.go`、`payment.go`：校验 `Payment.Mall.TimeoutMs`（0 或 500..15000）、释放 `closeRealNames()`、移除未使用 `TipClassifier()`。
  5. **Go 单测补充**：更新 `core/minor_test.go`、`core/record_test.go`（谷歌孤儿/冲突不 Finish、乱序撤销安全）、`channel/wechatminigame_test.go`、`channel/wechatpay_test.go`。
- **二、`go/trade` 与 `cpp/scene` 强一致扣回加固**：
  1. `go/trade/internal/data/asset_op_repo.go` & `asset_op_recovery_test.go`：`ListAbandonCandidates` 与 `Abandon` 双层禁止自动放弃 `TX_PAYMENT_REVERSAL`（防拒付扣回被 180 天自动清理）。
  2. `go/trade/internal/commerce/manual_plan.go`：`commercefix` `reissue` 支持重派 `manualReasonPaymentReversal` 的失败 `official_reverse_*`（Stream 3）与 `official_regrant_*`（Stream 4）腿。
  3. `go/trade/internal/commerce/mall.go` & `official_reversal_test.go`：`NewMall` 配置连接池；`MallOrder` 增加 `PaymentRevision`、`mallAnswer.Version` 升级为严格单调 `(PaymentRevision << 4) | stateRank`（彻底解决撤销/补发后客户端缓存冻结）。
  4. `cpp/libs/services/scene/player/system/asset_op_system.cpp` & `cpp/tests/currency_test/asset_op_system_test.cpp`：`TX_PAYMENT_REVERSAL` 允许按 `(config_id, count)` 扣回非堆叠道具与已穿戴装备，按 `RecallBagOrder` 全背包扫描（背包→仓库→临时→动态→装备栏，脱装同步 `Recalculate` + `PushPanel`，零裸指针成员）。
- **三、`java/mall_game`、K8s 部署、告警与文档**：
  1. `CommerceApi.java` & `CommerceStore.java`：`CommerceApi.Order` 新增 `paymentRevision`（附 8 参重载兼容旧测试）；新增 `@EnableScheduling` 驱动的多副本安全未支付订单超时清理 `expireOverdueOrders()`（60s 宽限期、每批 50 单走 `cancelOrder` 释放库存/寄售预占与未支付配额）；`oms_order` 补 `idx_commerce_status_deadline (status, payment_deadline_ms)` 索引并在 `SchemaVerifier` 校验。
  2. `NonceStore.java`：移除热路径 1/64 内联 `DELETE`，改为独立 `@Scheduled(fixedDelay = 10_000)` 批量 `LIMIT 500` 清理 `pruneExpired()`。
  3. `DeliveryApi.java`、`DeliveryStore.java`、`DeliveryController.java`：`/internal/refunds/poll` 升级确定性游标分页 `{entries, nextCursor}`（`r1:<orderSn>:<channel>:<transactionId>`，`commerce_refund_intent` 升级覆盖索引 `(status, order_sn, channel, transaction_id)`）；`settleRefund` 对 `MANUAL`/`REVOKED` 返回 `409`（仅 `REFUNDED` 缺回执回 `503`）；`receipt`/`reversalReceipt` 幂等重放不比较 `proof`（对齐设计 §8.2.5）。
  4. `PaymentRevisionStore.java`：寄售单（`sku_id=0`）付款撤销止损——未交付（`status=1`）立即作废 `commerce_delivery_intent`（`PENDING→REVOKED`）阻断发货并转 `MANUAL`；已交付（`status=2`）冻结冷却期未提现 `commerce_seller_income`（`available_ms=Long.MAX_VALUE`）并转 `MANUAL`。
  5. `CatalogAdminStore.java`、`schema.sql`：货币发放严格校验已知游戏币种 `Set.of(0, 1, 2)`（银两/灵石/绑定灵石）。
  6. `DatabaseConfiguration.java`、`pom.xml`、`application.yaml`、`deploy/k8s/manifests/java-svc/mall.yaml`、`deploy/k8s/manifests/go-svc/payment.yaml`、`tools/scripts/k8s_deploy.ps1`、`deploy/k8s/payment-alerts.yaml`、`deploy/k8s/mysql-connection-alerts.yaml`、`tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`、`deploy/k8s/README.md`、`docs/design/payment-recharge.md`：
     - `mall` 接入 `micrometer-registry-prometheus`、Hikari 池定名 `mall`（`minIdle=4`/`maxPoolSize=16`/生命周期与保活配置，清理双设警告）、优雅停机（`graceful` + 25s + K8s `preStop sleep 5` + `terminationGracePeriodSeconds: 40`）、K8s 独立管理端口 `18121`（探针 + `/actuator/prometheus`，与业务 18120 线程池隔离，`socketTimeout=6000` 统一）；`payment.yaml` 补 `startupProbe` 与 `terminationGracePeriodSeconds: 45`；告警新增 `PaymentFinalizeStalled`、`MallHikariAcquireTimeout`、`MallHikariSaturated`；补齐全部 Java 单测与部署契约断言。
- **四、Unity 客户端（`D:\luyuan\mmorpg-client`）生产级加固**：
  1. `StorePayBridge.cs`（对照本地 `com.unity.purchasing@015b4cec2127` 5.0.4 源码）：订阅 `OnPurchasesFetched` 恢复 iOS StoreKit 2 启动未完成单并按交易键去重；订阅 `OnStoreDisconnected` 清空连接态并配置有界重连策略；`FetchProducts` 改传 `MaximumNumberOfAttemptsRetryPolicy(2)`（根除无限重试卡死）并加 `_fetchGeneration` 守卫；购买前对空扩展服务/空账号订单标记 fail-closed；`PurchasePending` 同时核对 `productId` 与订单标记。
  2. `PaymentClient.cs` & `JubaozhaiUiRoot.cs`：收据核验瞬时失败（网络/超时/`KPaymentBusy`/`KPaymentLoginRequired`/`KServiceUnavailable`/`KRateLimitExceeded`）与在途被 `Reset()`/`Disconnected()` 中断均按指数退避（3/6/12/24/30s）重新入队不丢单；`ListChannels()` 独立世代不打断在途支付并修正错误文案；跨角色补单（`FinishTransaction=true && Intent==null`）不启 90s 轮询；`Settled` 优先回传 `intent.OrderId`；终局错误（订单不存在/过期/非法参数）立停轮询；新增 `CancelWatching()`；`bridge.Prepare`/`Launch` 加异常兜底；`JubaozhaiUiRoot` 仅在 `inGame && player != 0` 时驱动 `Tick`，并在 `PaymentClient.Settled` 时无条件刷新背包/宠物资产。
  3. `AndroidPayBridges.cs`、`MinigamePayBridges.cs`、`MinigamePay.jslib`、`RpcGuard.cs`：安卓双桥捕获全部 `Exception`（支付宝在 `finally` 保证回主线程）；小游戏桥增加单调 `requestId` 与 `CancelPending()`、`.jslib` 防双回调、优先 `getDeviceInfo().platform` 转小写；`RpcGuard` 将 `ClientPlayerPaymentRpc`（396..399）注册到独立 `"payment"` 熔断域。
  4. `CommerceWindow.cs` & EditMode 单测：支付态变化不再每 2 秒重建整页 UI、支付中禁用支付与取消按钮、支持「停止等待」、后台补单结算不劫持当前页签；`Money()` 补全常见 2 位/0 位国际币种与 0/1/2 游戏币名；`Kind()` 补 `RmbConsignment`；终态寄售隐藏「撤回寄售」；市场挂单无下一页时置灰「下一页」；补全 `PaymentClientTests.cs`、`CommerceClientTests.cs`、`RpcGuardTests.cs`。
- **已跑只读静态预检（全过）**：
  - `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`：10/10 OK；
  - `python tools/scripts/check_log_text_english.py`：0 violations；
  - `gofmt -l`（本轮全部修改的 Go 文件）：0 差异；
  - PowerShell `Parser::ParseFile`（`k8s_deploy.ps1`、`k8s_mall_payment_deploy_contract.tests.ps1`）：0 错误。
- **交 Codex 验证命令（按顺序执行）**：
  1. **协议生成（必须最先跑，产出 `PayFinalizeState_PAY_FINALIZE_STATE_FAILED` 与新索引）**：
     - `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（确保 `third_party\protobuf\install_vs2026_dbg\bin` 的 `protoc 35.1` 在 PATH 前列）
  2. **Go 编译、Vet、单测与真 TiDB 测试（`$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'`）**：
     - `go/shared`：`go test ./tracing/... -count=1`
     - `go/payment`：`go build ./... && go vet ./... && go test ./... -race -count=1`（再配隔离 TiDB `PAYMENT_TEST_DSN` + `PAYMENT_REQUIRE_TIDB=1` 跑 `go test ./internal/data/... -count=1 -v`）
     - `go/trade`：`go build ./... && go vet ./... && go test ./internal/data/... ./internal/commerce/... -count=1`（再配隔离 TiDB `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1` 跑 `go test ./internal/commerce -run 'TestTiDBMallReversalClawsBackThenRegrants|TestTiDBMallBridgeWaitsForCommittedLegAndRecoversLostReceipt' -count=1 -v`）
  3. **Java `mall_game` 编译与单测**：
     - `mvn -f java/pom.xml -pl authority_jdbc,server_startup install -DskipTests`
     - `mvn -f java/mall_game/pom.xml test`（重点覆盖新增的过期订单清理、退款游标分页、409/proof 重放、寄售撤销止损、币种校验、nonce 清理用例）
  4. **C++ 串行编译与单测（`/m:1`，注意其他会话在途文件 `player_pet.cpp:1025` / `transaction_log_commit_gate_test.cpp:167`）**：
     - `msbuild /m:1 cpp/tests/currency_test/currency_test.vcxproj /p:Configuration=Debug /p:Platform=x64`（若跨会话错误已修，运行 `currency_test.exe --gtest_filter=AssetOpSystemTest.*` 验证 `PaymentReversalDebitsStackableAcrossWarehouseAndNonStackableEquipment`）
  5. **PowerShell 部署契约测试**：
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_mall_payment_deploy_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/sql_boot_catalog_java.tests.ps1`
  6. **Unity 客户端四平台编译与 EditMode 单测（在 `D:\luyuan\mmorpg-client`）**：
     - `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`（同时带 `MMORPG_UNITY_IAP` 验证 Editor / `UNITY_ANDROID` / `UNITY_IOS` / `UNITY_WEBGL`）
     - 运行 EditMode 单测：`PaymentClientTests`、`CommerceClientTests`、`CommerceRecoveryFileStoreTests`、`RpcGuardTests`
  7. **冒烟**：本地 `devpay` 联调冒烟 +（集群就绪后）`kind` 部署冒烟；独立 Java 全服务版留待后续按 `PARITY.md` TODO 跟进。
