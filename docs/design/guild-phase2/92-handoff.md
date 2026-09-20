# 92 交接说明(2026-09-19,给接手帮会二期剩余批次的人)

> 写给**下一个动手的人 / AI 会话**。前面 91 是批次计划,这一份是"到今天为止实际发生了什么、你从哪一行接着写"。
> 与 91 冲突时以本文的「已落码」与「待办」两节为准(91 的顺序与依赖仍然有效)。

## 0. 一句话状态

帮会二期 20 个批次里,**6 批已落码**(B1、B1b、B2s、B2c、B3a-1、B4a-client),
资产通道 **B4a-1 / B4b 由聚宝斋会话按本期设计落码**;**全部未编译、未跑测试、未跑导表器与 proto-gen**。
剩下 **11 批**(B3a-2、B3b、B5a–B5d、B6a-srv/cli、B6b-srv1/srv2/cli)+ 两个门禁(B4c、BK8s)。

## 1. 动手前必读(效力从高到低)

1. `README.md` §2/§3 —— **用户拍板,效力最高**。特别是 D2(帮会侧**不退款**)、U1(个人按游戏日 / 帮会按档期)、
   U2(历练**阵亡也得奖**,只排除逃跑)、货币三项改名(金币→银两、钻石→灵石、绑定钻石→绑定灵石)。
2. `90-consistency.md` 全文 —— **效力高于各节正文**。X-01~X-16 是阻断级,Y-01~Y-20 是重要级,G-01~G-08 是无人认领的全局缺口。
   **落码前先读点名本批的条目**,再读本节正文。
3. `05-economy.md` / `06-activities.md` 等各节顶部的「决策覆盖」「接口终稿」块 —— 效力高于同一文件的正文。
4. 各节正文。
5. 仓库 `AGENTS.md`(§7 不变量、§9 禁令、§10.1 分工、§11 工程原则)。

## 2. 已落码(未编译)

| 批次 | 内容 | 关键结果 |
|---|---|---|
| B1 | 帮会迁入独占库 `mmorpg_guild` | proto 为源 + `go/schemamigrate`;`-migrate` 入口;帮名唯一键 `uk_guild(name_norm)`;删 `guild_schema_migration` 门表 |
| B1b | `merge_zone` / `data_consistency_check` 的 `-guild-schema` | 帮会 SQL 全部改「库名.表名」;重名探测改比 `name_norm` |
| B2s | 管理与审批服务端(32 文件) | 删 `JoinGuild` 改申请制;8 个新 RPC + 下行 `NotifyGuildChanged`;`guild_manage_repo.go` 事务基座;`GuildRule`/`GuildLevel` 配表;9 个 tip |
| B2c | 客户端 + 限流 + robot 管理段(9 文件) | `GuildClient.cs` 222→462 行;MessageLimiter 8 行新号 + 19 号重定;robot M1–M10 |
| B3a-1 | 名字注册表(29 文件) | `go/shared/playername` 规则包;data_service 的 store/cache/logic 三层;`RoleNameRule` 配表;`profile_component = 15` |
| B4a-client | Pet/Attribute 客户端 27000–27008 镜像(2 文件) | 冻结/封禁/GM 路径会透传资产段码,原先只显示 `tip=N` |
| (他人)B4a-1 / B4b | 通用资产通道 | `go/shared/assetop` + `go/shared/scenenode` + scene 三个 RPC + 账本 |

**B4a-2 不用做**:客户端面的 GM 统一闸门已由 P0-a 那批做掉(gate 按消息号闸 + scene 的
`player_gm_guard.h`,判据 `GATE_RUN_MODE`/`SCENE_RUN_MODE`,**未设 = prod = 拒**)。
`04-asset-channel.md` §4.12/§4.34 已标注"以代码为准,不存在 `client_gm_gate.h` 与 `MMORPG_ALLOW_CLIENT_GM`"。

## 3. 剩余批次(按依赖顺序)

| 序 | 批次 | 手改文件 | 依赖 | 正文 |
|---|---|---|---|---|
| 1 | **B3a-2** | 23 | B3a-1 | `03-names.md` §3.9–§3.17 |
| 2 | **B3b** | 20 | B2c、B3a-2 | `03-names.md` §3.21–§3.23 |
| 3 | **B5a** | 20 | B2c、B4b | `05-economy.md` + `90-consistency.md` part2 §1 |
| 4 | **B5b** | 23 | B5a | `05-economy.md` §5.15–§5.29 + 顶部「接口终稿」块 |
| 5 | **B5c** | 12 | B5b | `05-economy.md` §5.34–§5.40 |
| 6 | **B5d** | ≤18 | B5b | **需先补详细设计**(S4 4.38/4.39 只给了接口) |
| 7 | **B6a-srv** | 30 | B5b | `06-activities.md` |
| 8 | **B6a-cli** | 9 | B6a-srv | 同上 |
| 9 | **B6b-srv1** | 17 | B6a-srv | 同上;与组队、属性会话协调 |
| 10 | **B6b-srv2** | 21 | B6b-srv1 | 同上 |
| 11 | **B6b-cli** | 6 | B6b-srv2 | 同上 |
| 门禁 | **B4c** | ≤12 | B4a-1 | 玩家存盘属主围栏。**任何共享/预发环境开启帮会资产操作之前必须先落** |
| 上线 | **BK8s** | 另计 | B4c | `90-consistency.md` G-05 |

### 每批的关键坑(照这个查,别只看正文)

- **B3a-2**(login 建角与补齐):`profile_component` 列必须先按 `03-names.md` §3.15a 迁移(**阻断项**)。
  `merge_zone` 要改成**按列名拷贝**(现在是按位置),否则加列之后合服会串列。
  Tip 加在 `//login_error` 组尾。robot 账号按 Y-09 分配。
- **B3b**(成员显示名):Y-07 要求清单 +1(`guild_manage_logic.go` —— `ListGuildApplications` 的
  `GuildApplicantView.name` 与 `ListMyGuildApplications` 的 `leader_name` 也要走批量取名)。
  Y-08:成员页标签从"按编号查找"改成"按名字或编号",宽 230 放不下时字号改 26。
- **B5a**(资产三表 + 经济 RPC):表定义**以 `90-consistency.md` part2 §1 为准**(X-01:各节都没给全定义)。
  索引名 `idx_<表>_<n>` **从 0 起**(X-02),第三组是 `idx_guild_asset_op_2`。
  状态枚举要有 `APPLIED_PARTIAL = 5`(X-15)。**D2:不要退款分支**(不加 `GUILD_ASSET_OP_KIND_DONATE_REFUND`、
  不加 `TX_GUILD_DONATE_REFUND`、scene 白名单不动)。
- **B5b**(经济服务):**照 `05-economy.md` 顶部「B4b 落地后的接口终稿」块写,别照正文** ——
  `ListDue` 是两段非锁读(阈值用导出的 `assetop.FreshAttemptLimit`,不许写 3)、`Claim` 六参(含 `poisonUntilMs`)、
  `Reschedule` 的 `RowsAffected==0` 必须回 `assetop.ErrLeaseLost`。隔离级**不用显式传**(默认已是 RC)。
  X-13:计数器用**带上限的 upsert**,不要"锁不存在的行再插入"。
  开发密钥从 `run/secrets/assetop-dev.env` 取,**不要写死** `change-me-dev-…`(90 清单 G-04 已作废那个固定值)。
- **B5c**(客户端经济):X-05(`Accept` 的 switch 只加 `KGuildFundsInsufficient` 起的 10 个,别重复加 B2 已有的,
  否则 **CS8510**)、X-07(`DrainQueued` 的最终形态见 X-07 代码块,**必须保留**"申请列表不可见时 Refresh 刷角标"那一支,
  B2 的测试依赖它)、X-11(`UnsupportedActionsAreClearlyDisabled` 删到最后一个用例时要**删整个方法**,
  否则 NUnit 留下带参数无用例的方法)、X-12(总览断言文案改成 `"0 / 101"`、`"0 / 987"`)。
- **B5d**(回档 fail-closed):**先补详细设计再落码**。要点:data_service 回档前查帮会已应用的资产操作,默认拒绝。
- **B6a**(灯会/团圆):U1 —— 个人次数按**游戏日**(UTC+8 每天 05:00 重置),帮会级进度按**活动档期**。
  X-14:解散事务要在删成员之前删 `guild_activity_progress`(B6a)与 `guild_trial_battle`(B6b),
  位置在 `guild_manage_repo.go` 的 `DisbandGuild`(**不是** `guild_repo.go`)。
  Y-03/Y-04:`changeKindLabel` 与 `invalidateAfterCommit` 的集合 B2s 已经一次写全,**B6 只调用不改**。
- **B6b**(同道历练):U2 —— **阵亡也得奖**,`candidates = team 0 − fled_player_ids`(不再减 `dead_player_ids`,
  该字段保留供统计)。

## 4. 生成器待办(必须按这个顺序,在任何编译之前)

本会话按 `AGENTS.md §10.1` 全程没跑构建/测试/生成命令,所以下面这些是**已知待办**,不是缺陷:

1. **导表** + **proto-gen**(仓库根):`dev.bat gen`,随后 `py tools/data_table_exporter/tools/gen_schema_index.py`。
   - 期望产物:`generated/tables/{guildrule,guildlevel,rolenamerule}.json`;
     `generated/code/proto/tip/*` 含 guild 9 码(14013–14021)、login 新码、asset 9 码(27000–27008);
     `proto/message_id.txt` 含 216–223(**19 号已由 `JoinGuild` 易主给 `SetGuildMemberRole`**)。
   - ⚠ `data/tip/Tip.xlsx` 同时有多个会话在写。改它**必须** openpyxl `load → insert_rows → save`,
     **禁止整表写回**(二进制不能 3-way 合并,整写等于删别人的码)。
2. **Go proto 产物**:`cd go && build.bat`。
   - `go/data_service` 现在**编译不过**是预期的:它引用 `dbpb.PlayerName`、`data_service.ReservePlayerName*` 等
     尚未生成的类型。跑完这步才谈编译。
   - `go/friend` 同理(friend 会话的移植也没跑生成器)。
3. **客户端**(`E:\work\mmorpg-client`):`pwsh -File tools/gen_proto.ps1`、`pwsh -File tools/gen_messageids.ps1`。
   - 现在 `MessageIds.cs` 只有 `ApplyJoinGuild=218`,其余 8 个要跑完才有。
4. **C++**:MSBuild **必须串行 `/m:1`**(并发会报假的 C1041/LNK1104)。
5. 之后才是各服务的 `go build ./... && go vet ./... && go test ./... -count=1`。

各批的完整 Codex 验证序列在 `91-batches-and-codex.md` §2 与各节的"Codex 验证"小节。

## 5. 多会话协作规矩(踩过坑,别重蹈)

- **动手前先问归属**:用 `ListAgents` 看还有谁在跑,`SendMessage` 问一句"这些文件是不是你的"。
  本仓同时有 4–5 个会话在写(帮会、聚宝斋/资产通道、friend 移植、跨 zone 传送、k8s 治理)。
- **只提交自己的文件**:`git add <逐个路径>`,不要 `git add -A`。工作区里常年有别人的在途改动。
  仓库有一个**每小时自动 WIP 提交**的任务,它会把所有脏文件一起提交 —— 这是既成事实,但你自己提交时别这么干。
- **改名/删符号之后要互扫**:两边改的不是同一行时,3-way 合并**零冲突**,但合完之后一侧可能引用了对面刚改名的符号。
  冲突检测抓不到,只有编译器能抓 —— 而现在没有编译器。做法:列出你删改的符号,在 main 上 grep,
  **排掉注释行与 `vendor/`、`generated/`、`*.pb.go`**(否则"历史说明"注释会刷出一堆假命中)。
  本会话靠这一条挡下过一次真编译错误(`tradeSchemaNamePattern` 改名后的悬空引用)。
- **别在别人 regen 的时段跑发号器/导表器**:会把号发在半成品上。

## 6. 本会话留下的欠账(如实记录)

- `5814bc5f8` 这次提交**夹带了 3 个 B2s/B2c 收尾文件**进 B3a-1 的提交,使那一批没法按提交单独复核或回滚。
  已推送,不重写历史,记在这里。
- **B5d 的详细设计还没补**(`90-consistency.md` D3 新增的子批,S4 4.38/4.39 只给了接口)。
- `data/AGENTS.md` 的配表索引与"合计 N 张表"计数,每加一张新表都要同步(B5/B6 还会加表)。
- `go/friend` 与 `go/trade` 目前在 main 上都是**编译不过**的状态(各自会话的移植没跑生成器),
  你做编译体检撞到它们,**不是帮会这条线引起的**。

## 7. B3a-1 收尾时新产生的欠账(2026-09-19,接手前先看)

1. **`assertUniqueColumns` 没有 DB 级用例**。这是本批新加的**启动期 fail-closed** 守卫:
   迁移时查 `INFORMATION_SCHEMA.STATISTICS`,要求 `player_name` 上存在**恰好只含 `name_norm` 一列、
   且不带前缀长度**的唯一索引。判据比"存在 NON_UNIQUE=0 的索引"更严,理由是
   `UNIQUE KEY (name_norm, player_id)` 也满足后者,但它只保证组合唯一,**每个 player_id 仍能再占一次同名**。
   落点应是 `schema_integration_test.go` 或 `player_name_store_integration_test.go`(建表后 `DROP INDEX`、
   或建成组合唯一键、或建成前缀索引,再跑 `migrateSchemaOn` 期望报错)。
2. **运维处方要跟上**:任何已有 `player_name` 表但缺这把键的环境,data_service 从此**拒绝启动**。
   处方 `ALTER TABLE player_name ADD UNIQUE KEY uk_player_name (name_norm)`,
   **执行前可能要先清掉 `name_norm` 重复行**,否则 ALTER 自己会 1062 失败。这段该进部署/运维文档。
3. **TiDB 上 `SUB_PART` 的取值没实测**。若 TiDB 对全长索引返回 0 而不是 NULL,
   `sql.NullInt64.Valid` 会为 true,守卫会把正常的 `uk_player_name` 误判成前缀索引 → **误拒启动**。
   验证:本机 TiDB 空库跑一次 `-migrate` 期望退出 0,再人工
   `SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE, SUB_PART FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_NAME='player_name'` 看一眼。
4. **`data/AGENTS.md` 的配表索引是生成块**(`<!-- BEGIN GENERATED: schema-index -->`,块头写着"不要手改")。
   本会话一度手写了 `RoleNameRule` 行,**已回退** —— 等 `gen_schema_index.py` 产出。
   跑完生成器后比对一次 `git diff`:预期新表 7 列 / 7 字段 / 1 行,合计 31→32 张表、361→368 列、222→229 字段;
   **不一致以生成器为准**。
5. **`Release` 用尽 3 次重试后返回 error**(上层映射成 Internal),不再误报 `ReleaseOutsideWindow`。
   这比原来诚实,但 login 补偿路径的错误分支与告警口径**可能要跟着调**(logic/server 层这次没动)。
6. **还有两处 data_service 的"四张表"陈旧注释不归帮会线**:`deploy/k8s/README.md:422`、
   `tools/scripts/k8s_deploy.ps1`(485/2141/2241/3749/3796),归 k8s 治理会话。
   **注意别误改 friend 的**:`mmorpg_friend` 确实是四张表。
