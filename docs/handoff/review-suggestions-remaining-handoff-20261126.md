# 11-19 评审建议项的剩余部分（2026-11-26 交接）

> **2026-11-26 晚再定（负责人原话「改由 Claude 写」）**：第 1~5 节的代码改由 Claude 写（Opus 5.5，负责人放行），两个会话分工：
> - 会话「11-12 残余收口各批的独立评审」：§1.1、§1.2、§1.3（login 与客户端），§4 第 2、3、4 条（data_service 坏消息进死信、判陈旧与 create_residual）。
> - 会话「交接单里的代码改动清零与方案入档」：§2 修复身份三项，§3 snowflake LegacyPrefix 整删，§4 第 1 条（人工终结见证比 attempts），§5 文档。
> - **Gemini 不要改这几节的代码**；第 6 节已修五批的验证照旧交 Gemini，只跑，要改代码时保留首错原文停下、交回 Claude。下面「交 Gemini」一段保留作对照。

接 `docs/PROGRESS.md` 2026-11-26「收 11-19 六条「评审修正·」下的「建议，没做」」。那一条做完了五批，下面是没做的。

**交 Gemini（11-26 晚定）**：Claude 的 Fable 周额度当晚用完（100%，12-03 重置），负责人随后把会话切到 Opus 5.5；按 AGENTS §10.3，低档模型不改逻辑代码。负责人的口径是「量不够的时候交接给 gemini 去做」，所以本单各项全部交 Gemini：

- 复核分析与改代码用 Gemini 4 Neon Extra；
- 协议生成、编译、测试、冒烟用 Carbon Medium。

只在 main 上改，不建分支，不用提交；Java 版只登记待做；查出的问题现在修，不留以后。每一项都按第 7 节把分析和取舍写进设计文档。


## 1. 客户端（`D:\luyuan\mmorpg-client`）

> **状态（11-26 晚，会话「11-12 残余收口各批的独立评审」）**：本节三项都已落码，未编译。1.1 按含义拆开 2005（没有用 2007，原因见 `client-request-ordering.md` §6.2）；1.2 补了四条真 TCP 用例（`EnterGameRoleSwitchFlowTests`），选角页回选次数上限在完整登录管线里，没补；1.3 三项都改了。命令见 PROGRESS 2026-11-26「交接单第 1 节与第 4 节第 2~4 条」。

### 1.1 同一角色重发 EnterGame 收到 2005 一律按「前一次还在进行」处理

- **现状**：`GameClient.RoleSwitch.cs` 的 `ClassifyEnterGameReply` 里，重发、目标角色就是本连接绑着的那个、码是 2005 时回 `InProgress`，接着等进场通知。
- **问题**：服务端 EnterGame 回 2005 有四种来源（`go/login/internal/logic/clientplayerlogin/entergamelogic.go`）：
  1. 会话栅栏：本连接绑着别的角色；
  2. 玩家锁被占：前一次进场链在跑；
  3. 角色寄售中（`ownership.TradeListingId != 0`）；
  4. 预加载池满。
  只有第 2 种该等。第 3、4 种会白等 60 秒，再报「等待进入场景超时」。
- **客户端单独改不了**：四种是同一个码，分不出来。
- **建议做法**：让第 2 种回已有的 `kLoginEnteringGame`（2007），其余仍回 2005；客户端把 `InProgress` 的条件改成「重发、同一角色、码是 2007」。不用加新码。动手前先确认：
  - 2007 现在没有别的用法（服务端、Java 版、robot、客户端各 grep 一遍）；
  - robot 与压测汇总里按 2005 统计玩家锁争用的地方要同步认 2007；
  - Java 仓 `docs/reference/mmorpg-client-contract-*.md` 与 `PARITY.md` 登记这条客户端可见的变化；
  - `docs/design/client-request-ordering.md` §6.2 同步。
- **用例**：`EnterGameRoleSwitchTests` 里 `ClassifyEnterGameReply` 的表加两行（重发 + 2007 → InProgress；重发 + 2005 → Rejected）；login 侧给玩家锁被占那一支加断言。

### 1.2 换角编排没有用例

`EnterGameRoleSwitchTests` 只测纯函数。照 `GameClientReconnectTests` 的回环假 gate 补三类，放新文件，不动 `GameClient.cs`（别的会话有未提交改动）：

1. 3023 → 回选角 → 选 B → LeaveGame → EnterGame 回 2028 → Login → EnterGame 成功；
2. LeaveGame 连续三次 1003 之后拆连接；
3. 第 4 次 3023 拆连接（`MaxEnterReselects` = 3）。

### 1.3 小项

- `LeaveThenEnter` 分支实际走不到（进入时已按 `_enterRequestPlayerId` 先 LeaveGame）：留作防御，在注释里写明。
- `ClientRequestKeys.ReportKeyReused` 对任何 1005 都记错误日志：只在确实是键复用时记。
- 临时格面板：未就绪时状态行空白；每次 `OnChanged` 整页重建，焦点会跳。

## 2. 修复身份

> **2026-11-26 晚：代码已改完，Gemini 只验证**（会话「交接单里的代码改动清零与方案入档」，Opus 5.5，未编译）。设计记录在 `docs/design/data-consistency-guarantee-map-20261020.md` §6.8，列级定案在 `docs/design/tidb-phase1-and-launch-readiness.md` §2.6。
> - dbmove：设了 `MMORPG_SQL_REPAIR_PRINCIPAL` 时 `plan` 核修复账号在目标上存在，`cutover` 冻结之前比它在这个库上的授权；**都只打 WARNING、不拦搬库**（`tools/dbmove/mover.go` 的 `reportRepairPrincipal`、`side.go` 的 `hasGrantsOn`、`main.go` 的环境变量）。验证：`cd tools/dbmove && gofmt -l . && go vet ./... && go test -count=1 ./...`，新用例 `TestPlanWarnsWhenRepairPrincipalIsMissingOnTargetButGoesOn`、`TestPlanSkipsRepairPrincipalWithoutGrantsOnThisDatabase`、`TestPlanNotesWhenRepairPrincipalIsUnset`、`TestCutoverWarnsOnMissingRepairGrantsButStillFlips`、`TestCutoverIsQuietWhenRepairGrantsMatch` 都要 PASS。
> - 开片 / 开库后的提醒：go/db `cmd/migrate -command up` 在这次建了表时打一行 `Reminder: repair principal grants are table-level …`；data_service `-migrate` 每次都打（它不回报建了什么）。**不做成自动执行**：GRANT 要管理员账号，部署与迁移都不经手数据库管理口令。验证：`cd go/db && go build ./cmd/migrate && go vet ./cmd/migrate`，`cd go/data_service && go build . && go vet .`（`data_service.go` 顺带改正了 HEAD 里一处 import 顺序，`gofmt -l` 应无输出）。
> - 列级权限：TiDB 文档写明 v8.5.6 起才支持，本项目钉 v8.5.5，**现在保持表级授权**；要收紧须先由负责人批准把 TiDB 升到 ≥ v8.5.6。Gemini 按 tidb-phase1 §2.6 的步骤在一次性 TiDB v8.5.5 上核一次现钉版本的真实表现，结果只写 PROGRESS，不改代码。

- **dbmove 的 plan 不核修复账号**：搬目录片或存档库之后，目标集群要有修复账号和授权。现在只在手册里写了要自己建（`docs/ops/db-move-runbook.md` 第 3 条）。做法：`plan` 多核一项「目标集群上修复主体存在、授权与 `maintenance_drain.ps1 render-repair-role` 渲染的一致」，不一致只报告、不拦搬库（修复账号平时锁着，漏建只会让两个修复工具连不上）。
- **新开目录片或存档库之后要重跑授权**：开服手册第 3 步是人工步骤。做法：开片 / 开库的脚本结束时打印一行提醒并给出 `render-repair-role` 的命令；要不要自动执行由负责人定（自动执行意味着部署脚本要经手 GRANT）。
- **列级收紧**：TiDB 的列级权限行为没在真库核过，先核再定。核法：建一张表，`GRANT UPDATE(col_a) ON …`，用该账号改 `col_b` 应被 1143 拒绝。

## 3. snowflake 的 LegacyPrefix 兼容层整批删

> **2026-11-26 晚：代码已改完，Gemini 只验证**（Opus 5.5 子代理写、本会话复核 Txn 比较条件：只删了旧 id 键那一条）。7 个 Go 文件随别的会话的提交 `76e2b59bb` 入库；设计稿 `snowflake-id-allocation.md`、`node-id-overhaul-plan-20260908.md`、`node-id-overhaul-qa-20260908.md` 在工作区。验证：`cd go/shared && go vet ./snowflakealloc/... && go vet -tags=integration ./snowflakealloc/... && go test ./snowflakealloc/ -count=1 -v`；起 etcd（`docker compose -f deploy/docker-compose.yml up -d etcd`）后 `go test -tags=integration ./snowflakealloc/ -count=1 -v -timeout 15m`，输出里不得有 etcd 不可达的 SKIP；`cd ../scene_manager && go build ./... && go vet ./...`。Java 不适用。

项目未上线，旧二进制不存在，这层滚动升级的过渡代码用不上（代码注释自己写着「发布一版之后删」）。范围：

- `go/shared/snowflakealloc/allocator.go`：`Options.LegacyPrefix`、`resolvedOptions.legacy`、`legacyMirror`、`legacyGuardPrefix` / `legacyIDPrefix` / `legacyNodePrefix` / `legacyIDKey` / `legacyGuardKey`，以及用到它们的约 13 处（申领 Txn 里占旧布局的键、换 lease 时改挂、扫旧布局的已用槽与水位、读地板、`putWatermarkLocked` 的 `legacyKey` 参数）。
- `selection.go` 的 `mergeWatermarks`（只给旧水位合并用）与 `selection_test.go`、`testdata/selection_vectors.json` 里带 `legacy_watermarks` 的向量。
- `allocator_integration_test.go`：`TestLegacyKeysStillQuarantineAndOccupy`、`TestRollingUpgrade_LegacyBinaryCannotTakeALiveSlot` 与辅助函数 `legacyClaimHEAD`。
- `go/scene_manager/internal/svc/servicecontext.go` 的实参 `LegacyPrefix: "/scene_manager"` 与上面那段注释。
- 文档：`docs/design/snowflake-id-allocation.md`、`node-id-overhaul-plan-20260908.md` 里讲过渡镜像的段落改成已删。

删完之后 `go test ./snowflakealloc/` 与 integration tag 的用例都要过；申领 Txn 的比较条件少了旧布局那几条，逐条对照别删多。

## 4. 早期各批没人认领的 4 条（「数据覆写审计 R2 交接」会话 11-26 清点）

> **状态（11-26 晚）**：第 2 条已落码（未编译），第 3 条复核后不改代码，第 4 条 11-12 已收，见 r2 审计文档 §6 各条后面的状态；第 1 条归另一会话。

每条的出处、现象和已核过的代码位置，在 `docs/design/data-overwrite-rollback-audit-r2-20261019.md` §6「2026-11-26 清点」。先逐条复核，成立再改；修完在那一段每条后面标状态，在 §1「仍开着」里划掉，PROGRESS 写命令。

1. **（2026-11-26 晚：代码已改完，Gemini 只验证；guild 的条件写 SQL 待补，见 r2 第 4 轮交接单 §6.1 与设计记录）** **人工终结的见证不比 attempts**（`go/shared/assetop/manual_evidence.go`）：只比租约与发送时刻。副本间时钟偏差超过 30 分钟时，可能把仍在投的行判成已静默。设计记录写进 `exploit_loss_prevention_zh.md` 或资产人工终结所在的设计稿。
2. **data_service 两个 Kafka worker 的坏消息没进死信**：解码失败按旧口径跳过并提交，原件只在原 topic 保留期内可取。牵涉现行约束：reader 模式的 owner 不支持死信（`NewBoundMessageReader` 拒绝 `DeadLetter`），data_service 的取舍见 `kafka-bound-consumer-group.md` §5.5、§12，改了要同步这两节。
3. **判陈旧分不出「角色建成了、从没进过场、名字绑定也没做完」**：这种角色仍会被写墓碑。11-12 加了标保留，也有 `TestTiDBStaleCreateRefOfACommittedRoleIsNotTombstoned`，先复核这条是否仍成立。
4. **同一角色挂在别的账号上的建角 ref，只扫本账号片查不到**：出处同第 3 条。

第 3、4 条的现行判据在 `go/data_service/internal/store/account_role_ref.go`（`reapRef`、`committedEvidence`）与 `create_residual*.go`，设计记录写进 `guild-phase2/03-names.md` §3.4a 与 `horizontal-scale-logical-db.md`「标了保留的建角 ref」一段，手册同步 `docs/ops/mysql-backup-pitr-runbook.md` §4.1a。

## 5. 顺带看到、没动的

> **2026-11-26 晚：第一条已改**（只改文档）：`kafka-topic-retention-strategy.md` 里 DBTask 与 `db_task_zone_*` 的行、表与段落按「2026-10-30 已退役」改正文。第二条是别的会话的文件，照旧不碰（`data_service.go` 的 import 顺序随本会话 §2 的改动一起改了）。

- `docs/design/kafka-topic-retention-strategy.md` 第 2 节 `db_task_zone_<zone>` 一行和第 3 节的 DBTask 档位：代码里已不再声明这类 topic（只剩测试里的样例名），文档像是过时了，要对照「玩家存档直写 TiDB」那条线确认后改正文。
- `gofmt -l go/data_service/internal` 列出 `svc/directory_events.go`、`svc/servicecontext.go`、`store/audit_retention_test.go`、`store/schema.go`：别的会话的文件，格式没对齐。

## 6. 已修五批的验证

命令在 PROGRESS 2026-11-26「收 11-19 六条…」那一条的「交 Gemini」段。要改源码或断言就停下，保留首错原文。两处前提没实测，红了先看它们：

- H2 是否认 ODKU 更新分支里的 `CASE … VALUES(col)`；
- 真 TiDB 上「同值 upsert 之后普通读看不见那一行」。

## 7. 文档要求（负责人 11-26）

重要的流程、设计步骤、问题分析和解决办法都要写进对应的设计文档，尤其是负责人没想到的好方案；不要只写在 PROGRESS 或代码注释里。每条写清五件事：

1. 问题；
2. 方案；
3. 为什么这样选；
4. 被否决的做法；
5. 代价与边界。

写法可以照已修五批的记录：`kafka-bound-consumer-group.md` §15、`horizontal-scale-logical-db.md`「标了保留的建角 ref」一段末尾、`guild-phase2/03-names.md` 的 `bound_name_role_missing`。修完之后在 r2 审计文档 §1a 补一行入口。
