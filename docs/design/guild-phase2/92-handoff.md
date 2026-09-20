# 92 交接说明(2026-09-19,给接手帮会二期剩余批次的人)

> 写给**下一个动手的人 / AI 会话**。前面 91 是批次计划,这一份是"到今天为止实际发生了什么、你从哪一行接着写"。
> 与 91 冲突时以本文的「已落码」与「待办」两节为准(91 的顺序与依赖仍然有效)。

## 0. 一句话状态

> **2026-09-20 更新**:换机接手后又落了 **B3a-2** 与 **B3b 的服务端部分**,现状以 §8 为准(含客户端仓与本机 Python 两个阻塞)。

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
| B3a-2(2026-09-20) | login 建角带名字(24 文件) | mint → 占名 → 登记 home zone → **Lua 围栏写账号 blob**;入场补齐 `backfillPlayerIdentity`;角色列表缺名回源;scene/Java 解析;merge_zone `buildCopyColumns`;stress "CreatePlayer stages" 段。细节与欠账见 §8.1 |
| B3b 服务端(2026-09-20) | guild 批量取名(7 文件) | `logic.WithPlayerNames`(Y-02)、成员/帮主/榜单/两个申请视图(Y-07)各一次批量查询、fail-open;robot `member-names`。**客户端 12 个文件未做**(阻塞见 §8.0) |
| (他人)B4a-1 / B4b | 通用资产通道 | `go/shared/assetop` + `go/shared/scenenode` + scene 三个 RPC + 账本 |

**B4a-2 不用做**:客户端面的 GM 统一闸门已由 P0-a 那批做掉(gate 按消息号闸 + scene 的
`player_gm_guard.h`,判据 `GATE_RUN_MODE`/`SCENE_RUN_MODE`,**未设 = prod = 拒**)。
`04-asset-channel.md` §4.12/§4.34 已标注"以代码为准,不存在 `client_gm_gate.h` 与 `MMORPG_ALLOW_CLIENT_GM`"。

## 3. 剩余批次(按依赖顺序)

| 序 | 批次 | 手改文件 | 依赖 | 正文 |
|---|---|---|---|---|
| ~~1~~ | ~~**B3a-2**~~ | 24 | B3a-1 | **已落码 2026-09-20**,待 Codex 验证(§8.3) |
| 2 | **B3b 客户端** | 12 | B2c、B3a-2 | `03-names.md` §3.22–§3.23;服务端 7+1 个文件已落码。**被客户端仓阻塞**(§8.0) |
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

## 8. 2026-09-20 换机接手后的状态(本机 `D:\luyuan\wuxingqitan\mmorpg`)

> 上一个会话(机器 A,`E:\work\xuanming-server-mmo`)周额度耗尽;本节由机器 B 上的会话接着写。
> 本文与各节设计里的 `E:\work\...` 路径一律换算成 `D:\luyuan\wuxingqitan\...`(客户端 `D:\luyuan\wuxingqitan\mmorpg-client`)。

### 8.0 两个阻塞(都需要用户出手,不是代码问题)

1. **客户端仓**:远端 `luhailong-cpp/mmorpg-client` 的 `main` 仍停在 `d2b165a`(09-14),**上面没有任何帮会文件**;帮会客户端基线
   (`GuildClient.cs` 218 行,B2c 之前)在分支 `origin/codex/guild-team-roster-v12`。§2 里写"已落码"的 **B2c 与 B4a-client 在该远端任何分支上都不存在**
   ——机器 A 的客户端领先约 542MB(含一个 320MB 的 PNG 提交),上行慢,整包推送在 100% 后被远端挂断(需分段推 + 调大 `http.postBuffer` + 钉 HTTP/1.1)。
   **在那几个提交落到远端之前,不要在本机旧基线上写任何客户端批次**(B3b 客户端、B5c、B6a-cli、B6b-cli):那会造出第二份分叉的 `GuildClient.cs`。
   接手前先 `git fetch` 客户端仓,确认 `GuildClient.cs` 已含 `ApplyJoinGuild` / `NotifyGuildChanged` 再动手。
2. **本机没有 Python**(`py -3` 报 No Installed Pythons Found):导表器跑不了,B5a / B6a-srv 要用 openpyxl 改的 `Tip.xlsx`、`MessageLimiter.xlsx`
   与新建的 `GuildDonate.xlsx` / `GuildShop.xlsx` / `GuildActivity.xlsx` 也做不了。装工具按 `AGENTS.md §10.2` 需用户授权(Python 3.12 + openpyxl)。
   不要为了绕开它去手拆 xlsx 的 zip/XML——那是多会话共写的二进制,写坏了没有 3-way 合并可救。

### 8.1 B3a-2 落码要点与欠账

**结构**:四个互不相交的文件域并行实现 → 每域对抗评审 → 回修 → 第二轮评审(续跑时评审阶段未命中缓存,等于对回修后的代码又审了一遍)→ 跨域互扫。
两轮共 13 条发现(2 major / 11 minor),全部逐条核实后处理;跨域互扫:清单无漏改,login 包两域契约、全仓改名互扫、未生成符号命名、go.mod 解析四项通过。

**两条 major(都已修,值得知道)**
- **围栏脚本回 -1 ≠ "blob 确定没写"**。设计原文在 -1 分支直接释放名字。但 login 的 Redis 客户端没设 `MaxRetries`,go-redis v9.16 默认 3,
  读超时的 EVALSHA 会被**原样重发**:第 1 次执行时锁还在、SET 已落地、回包超时;重发到达时锁已过期 → -1。照原文释放,结果是**角色已带名落盘、
  登记表里的名字却被删、别人可再占同名**,全程零报错。现在 -1 与"脚本报错"共用三态回读 `readBackAccountBlob`:含 → 成功;不含 → 释放;
  读不出 / 解析不了 → **保留登记 + 记孤儿**。`03-names.md §3.11` 已补修正说明。
- **`stress_summarize.ps1` 的 `name_orphan` 列在健康压测下恒为 n/a**,而验收口径是 orphan=0,分不清"没发生"与"指标没接上"。
  根因是无 label 的 CounterVec 在首次 Inc 前不输出序列。修法两层:login 起服预置为 0(见下),脚本保留"有 stage 序列而无孤儿序列 → 打印 0"作兜底。

**清单外的一处功能改动(已入清单,23 → 24)**:`go/login/login.go` 的 `startServer` 在 `zrpc.MustNewServer` **之后**调
`loginlogic.PrimeCreatePlayerMetrics()`。位置不能动:go-zero `core/metric` 的每次写入都要过 `prometheus.Enabled()`,开关由
`MustNewServer → SetUp → StartAgent` 打开;放进 `init()` / `NewServiceContext` 会被直接丢弃。不预置的话
`increase(login_create_player_name_orphan_total[..]) > 0` 的告警恰好漏掉**第一次**孤儿(序列从无到 1,窗口内只有一个样本)。

**我裁定的一个设计未定项**:`ReservePlayerName` 回了 login 不认识的 result 码 → 拒绝建角 **并补偿释放一次**(`uncertain=false`,无延迟第二次)。
理由:读不懂就不知道它是否意味着"已登记";释放是按 `(player_id, name)` 的条件删除,而该 id 是本次新铸且已放弃的,删了不可能误伤在役角色;
不释放则可能留下一个无日志、无计数的静默孤儿。`TestCreatePlayerName_UnknownReserveResultFailsClosed` 已钉住。**不同意就回退这一处**(`tryReserveName` 的 default 分支)。

**与设计不同、保留的实现选择**
- `tools/merge_zone/player_rows.go`:"按列名拷贝"与"列检查提到任何一张表开写之前"资产通道会话(04 §4.42)**已经落过**,本批没有重造第二套,
  只把判定抽成设计点名的纯函数 `buildCopyColumns`(集合不等 → fail-closed 报错;相等 → dst 顺序;列名含反引号 → 拒绝)。
- C++ gtest 两个用例的输入是 `StockedPlayer()` 存出来的完整记录再改 name,而不是设计写的空 cold 记录:全空 `player_database` 会走
  `health==0` 的新号初始化路径,该路径在本 fixture 里从未被既有用例跑过,无法在不编译的前提下确认它不触发断言。
- `playernamereg_test` 只 Load `RoleNameRule` 单表,不用 `LoadTables`(缺任一表文件就 `log.Fatalf`,整个测试进程退出)。

**欠账(接手前先看)**
1. **login 的 Redis 客户端是裸默认值**:`svc/servicecontext.go` 构造时只传了 Addr/Password/DB,`login.yaml` 里 `Node.RedisClient` 的
   Dial/Read/WriteTimeout 是**死配置**(根本没接线,3s 与 go-redis 默认值相同只是巧合);没开 `ContextTimeoutEnabled`,ctx 截止时间不约束 socket 读写;
   `MaxRetries` 默认 3,Redis 卡住时单条命令最长约 12.5s。`Locker.AccountLockTTL: 20` 因此**不是硬上界**,超时后的正确性完全靠围栏 + 回读。
   同类现象:`pkg/locker` 的 `TryLock`(SET NX)也会被重发——第一次已加锁成功但回包超时,重发得到"key 已存在",于是回 `kLoginInProgress`
   且这把锁无人释放,要等满 20s。是否显式设 `MaxRetries` / `ContextTimeoutEnabled` 并把三个超时接线,影响面是整个 login,**另立任务评估,本批不动**。
2. **`docs/design/team-system.md` §G.2(约 :1038)仍写"name 恒为空、scene 快照明写没有昵称组件(player_battle.cpp:442-443)"**,与本批
   `BuildBattleSnapshot` 改从 `PlayerProfileComp` 取名矛盾。未改:该文件当时有别的会话的在途改动,且 90 清单 G-07 指定由组队会话合入。
   同类:`docs/design/server-merge-gap-fixes.md:23` P0-G 仍写"项目当前根本没有玩家昵称字段"(结论"合服不需改名"仍成立,原因变成全服唯一)。
3. **self-heal 恢复出的 `AccountSimplePlayer` 仍然丢 class_id / gender / zone_id**(本批只多补了 name)。既有问题,未处理。
4. **早于名字功能的旧角色**在注册表里没有名字,每次 EnterGame 与每次 Login 角色列表都会多一次 Lookup(靠 data_service 60s 负缓存吸收;
   按设计不回写账号 blob,所以会一直回源)。缺名账号的 Login 是两跳串行,最坏 2×`HomeZone.RoleListLookupTimeout`(`config.go` / `login.yaml` 注释已写明)。
5. **gofmt 存量基线**:`clientplayerlogin/` 下的 `deprecation.go`、`entergamelogic.go`、`legacy_gate_killswitch.go`、`player_class_backfill.go`,
   以及 `tools/merge_zone/audit_resources.go`、`robot/login.go` 在 **HEAD 就未格式化**(hunk 都不在本批新增行上)。验证时 `gofmt -l` 的通过标准是
   "不超出这条基线",不要为了过检查整文件格式化(`AGENTS.md §11.2`)。
6. **孤儿的运维处置**:ERROR 日志 `[player-name] orphan reservation player_id=%d name=%q account=%s` 是手工释放的输入 → 带 `x-admin-token` 调
   `ReleasePlayerName{player_id,name}`;告警 `increase(login_create_player_name_orphan_total[10m]) > 0`。目前只在 `merge-zone-runbook.md §4.4`
   与 `03-names.md` 里有,还没有独立的 ops 条目(90 清单 G-07 要求进"运维"节)。
7. 三条只经静态核对、要 Codex 运行时留意的测试前提:miniredis 下 `consumePostMergeFlags` 流水线对不存在的 key 回 `redis.Nil`;
   `ants.Pool.ReleaseTimeout` 会等在跑的 worker 退出;`PlayerAllData` 键存在时 `EnsurePlayerAllDataInRedisAsync` 走快路径、不解引用 nil 的
   dispatcher / Kafka。以及 miniredis"脚本内先 SET 再 `error_reply`,SET 不回滚"是推断。这几条若不成立,红的是用例构造,不是生产代码。

### 8.2 B3b 服务端落码要点

- 按 **Y-02** 用函数式 Option `logic.WithPlayerNames`(收到 nil 接口不做任何事),没有写正文里的 `SetPlayerNameResolver`;`03-names.md §3.21` 已补修正说明。
- 按 **Y-07** 两个申请视图也填名字;为了可测,把视图装配抽成私有方法 `applicantViews` / `myApplicationViews`(两个 RPC 的前半段要 MySQL,logic 包单测只有 miniredis 夹具)。
- `BatchResolve` 在发 RPC 之前判一次 `ctx.Err()`:请求 ctx 已结束就不发、**不计** `guild_player_name_lookup_failed_total`。否则 locator Redis 卡住时,
  排在前面的在线 MGET 吃光预算,取名立刻 DeadlineExceeded,告警会把排障引到 data_service 这个错误的依赖上。
- 该指标用 go-zero `core/metric` 是对的:guild 的配置内嵌 `zrpc.RpcServerConf` 且 `etc/guild.yaml` 配了 `Prometheus.Host`,开关会被打开(与 data_service 不同)。
  无 label,首次失败前序列不存在,告警写 `sum(rate(guild_player_name_lookup_failed_total[5m])) or vector(0)`。
- robot 的 `member-names` 步骤**依赖 B3a-2 已验证通过**:此前建的角色没有名字,它会失败并提示清哪些账号(含 Y-09 的 9211–9219)。
- **清单外的旧问题(已单独挂任务,未改)**:`OnlineStatusResolver.BatchResolve` 没有独立超时,locator Redis 客户端同样没开 `ContextTimeoutEnabled`;
  Redis 卡住时 handler 协程约 6s 才退出,活过 3500ms 业务预算与 zrpc 的 4000ms。
- Codex 验证:`go/guild` 下 `gofmt -l guild.go internal/logic` → `go vet ./...` → `go test ./internal/logic/... -count=1 -race`;
  慢响应用例应在约 20ms 内结束,**单个用例耗时接近 5s = resolver 漏设超时、踩到了安全网,必须当失败上报**。robot 本批**不** `go mod vendor`(Y-10)。

### 8.3 B3a-2 Codex 验证序列(串行;全部未执行)

```text
【总则】
- 仓库根是 D:\luyuan\wuxingqitan\mmorpg。设计 §3.20 里的 E:\work\xuanming-server-mmo 是旧机器路径,不要照抄。
- 全程串行,任何一步失败就停下报告,不连续重试。
- 同一时刻只允许一个构建方(90 清单 G-08),MSBuild 必须 /m:1 /nr:false。
- 本批和 B3a-1 都未编译、未运行,所以序列里含 B3a-1 的补验证。
- 工作树里还有 B3b(go/guild、robot/guild_smoke_scenario.go)和其它会话的未提交改动(go/scene_manager、cpp 的 player_lifecycle / asset_op / cross_zone_test 等)。失败先看文件归属,再定是不是 B3a-2 的问题。

步骤 0|生成前快照。
- 工作目录:仓库根。
- 命令:`git status --short data proto generated tools/data_table_exporter/state go/proto go/shared/generated robot/vendor cpp/generated > ..\tmp\b3a2-verify\status-before.txt`(先建目录)。
- 同时与聚宝斋、组队、聊天会话确认:此刻没人在跑导表、proto-gen 或 MSBuild;Tip.xlsx 和 MessageLimiter.xlsx 没有他人未提交修改。
- 通过标准:快照已保存。
- 失败保留:冲突方名单。

步骤 1|导表加 proto 重生。
- 工作目录:仓库根。
- 命令:`.\dev.bat gen`。前置:protoc 35.1 和 protoc-gen-go 在 PATH。
- 通过标准:
  - 退出码 0;
  - 新出现 generated/tables/rolenamerule.json(数据行 id=1,min_chars=2,max_chars=12,generated_prefix=道友,generated_suffix_len=6,max_generate_attempts=5);
  - 新出现 go/shared/generated/table/rolenamerule_table.go 和 go/shared/generated/pb/table/rolenamerule_table.pb.go;
  - go/shared/generated/pb/table/login_error_tip.pb.go 含 LoginError_kRoleNameInvalid / kRoleNameTaken / kRoleNameSensitive(fault 列为空,不进 go/shared/generated/tip/faults.go);
  - 新出现 cpp/generated/table/code/rolenamerule_table.{h,cpp} 和 proto/rolenamerule_table.pb.{h,cc};
  - 生成器的段与码自检无报错。
- 失败保留:导表器和生成器的完整输出。生成器报 kMaxRpcMethodCount 超限时停下,不手改上限(G-06)。

步骤 2|Go 侧 proto 产物。
- 工作目录:go。
- 命令:`pwsh -NoProfile -ExecutionPolicy Bypass -File .\build.ps1`。它等价于 build.bat,但 build.bat 末尾有 pause,非交互环境请直接调 ps1。
- 通过标准:
  - 退出码 0;
  - `grep -c ReservePlayerName go/proto/data_service/data_service_grpc.pb.go` > 0;
  - go/proto/common/base/user_accounts.pb.go 含 `func (x *AccountSimplePlayer) GetName`;
  - go/proto/login/login.pb.go 含 `func (x *CreatePlayerRequest) GetName`;
  - go/proto/common/database/mysql_database_table.pb.go 含 ProfileComponent;
  - go/proto/common/component 含 PlayerProfileComp;
  - cpp/generated/proto/common/component/player_comp.pb.h 含 PlayerProfileComp;
  - cpp 的 scene_node_service.cpp 仍含 AcquireCreatePermitBlocking(G-08);
  - 再跑一次步骤 0 的 git status 存为 status-after-gen.txt,与 before 对比,他人未提交产物不得变化,diff 只允许本批和 B3a-1 的新增。
- 失败保留:build.ps1 输出和两份 status。

步骤 3|B3a-1 补验证(前置能力)。
- 工作目录:go/shared。
  - 命令:`go test ./playername/... ./generated/table/... -count=1`。
- 工作目录:go/data_service。
  - 命令:`go vet ./...`,`go test ./internal/... -count=1`。
  - 然后设 `$env:DATA_SERVICE_IT_MYSQL_USER='root'`,跑 `go test -tags=integration ./internal/store/... -run 'PlayerName|Schema' -count=1`。
- 依次在 go/login、go/match、go/guild、go/db、go/trade、go/scene_manager 里跑 `go build ./...`。
- 通过标准:全部退出码 0。集成测试 Skip 不算通过,改用 root 重跑。
- go/friend 编译不过是 friend 移植会话的既定状态,不计。scene_manager 若失败,先看是不是它自己的未提交改动(owner_epoch)。
- 失败保留:`go test -v` 输出的首个 FAIL 段,`go build` 的首屏错误。

步骤 4|§3.15a zone 库加列。阻断项:必须在任何新版 go/db 或 scene 启动之前做。
- 工作目录:go/db。
- 命令:`go run ./cmd/migrate -f etc/db.yaml -command plan`。
  - 期望只出现 `ALTER TABLE player_database ADD COLUMN profile_component ...`。
  - 若 B4a 的 asset_op_ledger 列也未迁移,允许再多这一条(G-03)。出现其它语句先停下核对。
  - 库名白名单拒绝时,按 cmd/migrate/main.go 的注释设置 DB_ALLOWED_DATABASES。
- 然后跑 `-command up`。
- 每个 zone 库核对:`SHOW COLUMNS FROM player_database LIKE 'profile_component';` 返回 1 行。
- 本机多 zone 时,对每份 zone 的 db 配置重复一遍。run/etc/go_services/z<N>_db.yaml 由启动脚本生成,当前机器上还没有这个目录,以实际存在的配置为准。
- 通过标准:plan 只含允许的 ADD COLUMN,up 退出码 0,每个库 SHOW COLUMNS 返回 1 行。
- 失败保留:plan 全文和 up 输出。

步骤 5|go/login 静态检查。
- 工作目录:go/login。
- 命令 1:`gofmt -l login.go etc internal/svc internal/config internal/logic/pkg/playernamereg internal/logic/clientplayerlogin`。
  - 通过标准:输出不超出存量基线 clientplayerlogin 下的 deprecation.go、entergamelogic.go、legacy_gate_killswitch.go、player_class_backfill.go。这四个在 HEAD 就未格式化,不要顺手格式化。
- 命令 2:`go vet ./...`。
  - 通过标准:0 告警。重点看 lostcancel、copylocks、printf。
- 失败保留:vet 全文。

步骤 6|go/login 单测。
- 工作目录:go/login。
- 命令(a):`go test ./internal/logic/pkg/playernamereg/ -count=1 -v`。TestRules_FromGeneratedTable 依赖步骤 1 产出的 generated/tables/rolenamerule.json。
- 命令(b):`go test ./internal/logic/clientplayerlogin/ -race -count=1 -run 'TestEnterGame_PlayerNameReachesIdentityBackfill|TestRoleListWithCurrentHomeZone_|TestBackfillPlayerIdentity_|TestResolveEnterName|TestFillMissingRoleNames_'`。
- 命令(c):`go test ./internal/logic/clientplayerlogin/ -count=1 -run 'TestReservePlayerName_BudgetBelowOneReserveRPC|TestHasBudget|TestCreatePlayerName_|TestCreatePlayer_'`。
- 命令(d):整体跑 `go test ./internal/logic/clientplayerlogin/... ./internal/logic/pkg/playernamereg/... ./internal/logic/admin/... ./internal/svc/... -race -count=1`。
- 通过标准:全部 PASS,无 DATA RACE。
- 失败保留:`-v` 输出里首个 FAIL 用例的断言行,它上方的 logx ERROR 行,ants 的 `worker exits from panic` 栈(如果有)。

步骤 7|robot(90 清单 Y-10)。
- 工作目录:robot。
- 命令 1:`go mod vendor`。
- 命令 2:`git status --short robot/vendor`。
  - 只允许出现 vendor/proto/**、vendor/shared/generated/** 和 modules.txt。出现他人路径就停。
  - go.mod 和 go.sum 不应变化。
- 命令 3:`go build ./...`。
- 通过标准:
  - 退出码 0;
  - vendor/proto/common/base/user_accounts.pb.go 含 GetName;
  - vendor/proto/login/login.pb.go 含 CreatePlayerRequest.GetName。
- gofmt 基线:robot/login.go 在 HEAD 就未格式化(:349 的 struct 字面量对齐),不算本批的问题。
- 失败保留:go mod vendor 的输出,go build 的首屏错误。报 inconsistent vendoring 时停下报告。

步骤 8|tools/merge_zone。
- 工作目录:tools/merge_zone。
- 命令 1:`go vet ./...`。
- 命令 2:`go test ./... -count=1 -run 'BuildCopyColumns|AuditPlayerNameConflicts'`。
- 命令 3:`go test ./... -count=1`。集成用例带 //go:build merge_integration 标签,不会被带上。
- 通过标准:全部 PASS。
- gofmt 基线:audit_resources.go 在 HEAD 就未格式化(:480 的注释缩进),不算本批的问题。
- 失败保留:`-v` 输出的首个 FAIL。

步骤 9|go/guild(B3b 已在工作树,只做回归编译,不算 B3a-2 的验收)。
- 工作目录:go/guild。
- 命令:`go build ./...`,`go test ./internal/logic/ -count=1 -run 'PlayerName|Names'`。
- 失败单独记到 B3b 名下。

步骤 10|Java 网关。
- 工作目录:java/gateway_node。
- 命令:`mvn -q test -Dtest=LoginRpcClientParseTest,LoginRpcClientRetryTest`。
- 通过标准:
  - BUILD SUCCESS;
  - ParseTest 的 4 个用例(字段 5 有名 / 无字段 5 时 name 为 null / 未知字段 6 被跳过 / 其余)全过;
  - RetryTest 无回归。
- 失败保留:target/surefire-reports 下两个类的 txt 和 xml 报告。

步骤 11|C++(Debug x64,严格串行)。
- 工作目录:仓库根。
- 先按依赖顺序逐工程跑 `msbuild <vcxproj> /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal`,顺序如下:
  1. cpp/generated/proto/proto.vcxproj;
  2. cpp/generated/table/table.vcxproj;
  3. cpp/libs/engine/core/core.vcxproj;
  4. cpp/libs/modules/modules.vcxproj;
  5. cpp/libs/services/scene/scene.vcxproj;
  6. cpp/libs/services/battle/battle.vcxproj;
  7. cpp/tests/bag_test/bag_test.vcxproj;
  8. cpp/nodes/scene/scene.vcxproj;
  9. cpp/nodes/battle/battle.vcxproj。
- 也可以一次性跑 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`,再跑 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test`。
- 运行测试:
  - `build\cpp\tests\bag_test.exe --gtest_filter=PlayerFeaturePersistenceTest.*`,以仓库根为工作目录。缺 zlibd.dll 或 rdkafka*.dll 时先用 run_cpp_tests.ps1 同步 DLL。
  - 再跑整个 bag_test 回归。
- 通过标准:
  - 0 error;
  - PlayerFeaturePersistenceTest 全过,含新增的 ProfileNameRoundTripsThroughDatabaseRecord 和 MissingProfileComponentLoadsEmptyName;
  - bag_test 其余用例无回归。
- no-raw-pointer-member 因缺工具 SKIP,不计为静态检查通过。
- 归因:本批只动了 player_database_loader.cpp、player_battle.cpp、player_feature_persistence_test.cpp 三个文件。错误落在 player_lifecycle、asset_op_system、cross_zone_test 等文件时,属于其它会话。
- 失败保留:msbuild 日志里首个 error 前后 30 行,gtest 的 XML 或控制台输出。

步骤 12|stress_summarize 脚本体检(只读,不是压测)。
- 工作目录:仓库根。
- 命令:对任一含 login 快照的既有 RunDir 跑 `pwsh tools/scripts/stress_summarize.ps1 -RunDir <dir>`。
- 通过标准:
  - 脚本不报语法错;
  - 新增的「CreatePlayer stages」段出现,列为 mint / name / register / account_write / name_orphan;
  - 旧版 login 的快照整行是 n/a,其余各段输出与改动前逐字一致。
- 失败保留:脚本完整输出。

步骤 13|端到端冒烟(需要用户先授权删数据,见 91 文档 §2)。
- 前置清理:
  1. 全栈停。
  2. `redis-cli DEL account:robot_0000 account:robot_0001 account:robot_0002 account:robot_0003 account:robot_9001 account:robot_9002 account:robot_9201 account:robot_9202 account:robot_9203`。
  3. 按 Y-09 追加 account:robot_9211 到 9219,以及 account:robot_9501。
  4. 本机 dev 也可以直接 `redis-cli FLUSHALL`。
  5. 不要清 MySQL 的 testdb(id_segment 水位在里面)。全局库 player_name 可以 TRUNCATE。
- 确认 login 的 TableDir 下有 rolenamerule.json,然后全栈起(新版 go/db、scene、login、data_service、java gateway)。
- 起服核对:
  - login 日志含 `[player-name] registry client ready: name_len=[2,12] generated_prefix="道友" suffix_len=6 max_generate_attempts=5`;
  - login 日志没有 `RoleNameRule config invalid`;
  - `curl -s http://127.0.0.1:9101/metrics | findstr login_create_player_name_orphan_total` 输出值为 0 的序列(这验证 PrimeCreatePlayerMetrics 生效)。
- 冒烟(工作目录 robot):
  - `robot.exe -c etc/robot_smoke.yaml`,退出码 0;
  - MySQL 全局库:`SELECT COUNT(*) FROM player_name WHERE name LIKE '道友%';` 的结果 ≥ 本次新建的角色数;
  - Redis:`GET player:name:<新角色 id>` 与库里一致;
  - zone 库:`SELECT profile_component FROM player_database WHERE player_id=<id>` 在首次入场并存盘后非空;
  - `robot.exe -c etc/battle_smoke.yaml`,观战摘要 player_names 非空;
  - 回归 `robot.exe -c etc/guild_smoke.yaml`,期望 GUILD_SMOKE_OK。member-names 步骤属于 B3b,失败文案会提示清哪些账号。
- 失败保留:robot 日志,login 和 data_service 日志里的 `[player-name]` 与 `[role-name]` 行,/metrics 里 login_create_player_* 的全部序列。

步骤 14|负载(严格按 AGENTS §6.2,漏任何一步就重来)。
- 跑前:
  - 把上一轮 stress_summarize 的输出存成 prev-summary.txt;
  - 清 robot/logs 下的旧 stress-* 目录(留 1 个)、bin/log/*、tools/scripts/.run 的 pid 和 log;
  - Redis FLUSHALL;
  - kafka-offset-reset;
  - 全局库 player_name 可 TRUNCATE,testdb 的 id_segment 保留。
- 命令:`robot\robot.exe -c etc/robot.stress-200.yaml`。
- 期间:`pwsh tools/scripts/stress_snap.ps1 -RunDir robot/logs/stress-<name>-<ts> -StartTime '<yyyy-MM-dd HH:mm:ss>' -Stages 2,5,10,15,18`。
- 跑后:`pwsh tools/scripts/stress_summarize.ps1 -RunDir <同上>`。
- 通过标准(必须有与 prev-summary.txt 的二维对比表才算完成):
  - 「CreatePlayer stages」里 name 段的均值 < 50ms;
  - name_orphan = 0;
  - EnterGame 的 fail% 不劣于基线。
- 压测期间不上传任何日志。
- 没有对比表,不得声称性能结论。

【交付口径】
- Codex 回报每一步的实际退出码和证据路径(建议放 ..\tmp\b3a2-verify\)。
- 任何一步未执行,都要明确写「未验证」。
- Claude 侧在拿到这些结果之前,不得声称编译通过或测试通过。
```

---

## 9. 下一期工作单:B5a(2026-09-20 写,给接手的会话)

> 本节是**可直接照着开工**的工作单。与 §3 的总表冲突时以本节为准(本节更新)。
> 当前 `main` = `94f1ea2f4`(B3a-2 + B3b 服务端已合入并推送)。

### 9.1 本期做什么

**主任务:B5a —— 帮会经济的协议、配表、游戏日与号段登记。** 它是 B5b(经济服务)/ B5c(客户端)的硬前置,自己不含业务逻辑。

**并行可做:B5d 的详细设计**(不是落码)。`91` 把"先补详细设计"列为 B5d 的硬前置,而 S4 4.38/4.39 只给了接口。它是纯设计、不碰配表也不碰客户端,随时可写。要点已在 §9.6。

**不要碰**:B3b 客户端、B5c、B6a-cli、B6b-cli —— 客户端仓缺 B2c 基线,见 §8.0。

### 9.2 效力顺序(冲突时按这个,**比正文重要**)

1. `README.md` §2/§3 —— 用户拍板。
2. **`05-economy.md` 顶部的两个覆盖块**(「决策覆盖 D2」与「B4b 落地后的接口终稿」)—— 它们**明文作废了本节正文的一批写法**,不先读会照着废稿写。
3. `90-consistency.md` —— 尤其 **part2 §1**(资产三表的完整 proto,正文里根本没有)、X-01/X-02/X-04/X-13/X-15、G-03、G-04、Y-12。
4. `05-economy.md` 正文。
5. 仓库 `AGENTS.md`。

### 9.3 落码前先核这几条(我已替你查过,是现状)

- **`proto/guild/guild_db.proto` 现在只有 4 个 message**(`guild` / `guild_player_state` / `guild_member` / `guild_application`),**资产三表一行都没有**。按 X-01,`guild_asset_op` / `guild_player_op_seq` / `guild_daily_counter` 的完整定义由 B5a 照 **`90-consistency.md` part2 §1** 原样写入 —— 不要去 S1 或 §5.6 找"增量列",那两处都不全。
- **`go/guild/internal/data/tables.go` 的 `Tables()` 现在返回 4 张表**,新表要按 part2 §1 末尾给的**锁序**追加:guild → guild_player_state → guild_member → guild_application → guild_player_op_seq → guild_asset_op → guild_daily_counter。顺序即锁序,不能随便排。
- B1 的测试已按 X-09 改成由 `Tables()` 派生,所以加表**不会**打挂 `len(Tables)==4` 那类断言;但新表形状的断言要自己在集成测试里补。
- `go/shared/assetop`、`go/shared/scenenode` 已由聚宝斋会话落码(`795ea9f6a`),**签名以磁盘上的 `go/shared/assetop/reconcile.go` 为准**,别照正文抄。

### 9.4 文件数与清单(正文的 21 是旧数)

`05-economy.md §5.41` 写 21,至少要减两项:

- **减 `data/GuildLevel.xlsx`** —— X-04 / D1:GuildLevel 全表取 B2 的 10 级,B5a 不改它。
- **减 `proto/common/rollback/transaction_log.proto`** —— D2 删退款分支,不新增 `TX_GUILD_DONATE_REFUND`。

→ **19**。另外 D2 还明写「`kAssetOpStreamRules` 的 GUILD_CREDIT 一行**不改**,B4a 的 scene 白名单与 `static_assert` 均不动」,所以清单第 14/15 项(`player_asset_op.cpp`、`asset_op_system_test.cpp`)**要按 D2 复核是否还需要改**;如果确实不用改,文件数再减。开工时自己核一遍并在交付说明里写清最终数。

### 9.5 按"要不要 Python"把 B5a 切成两半

用户已拍板:**Python 由用户自己装,导表器 / proto-gen / 编译 / 测试也由用户自己跑**;Claude 只写代码与配表内容。本机目前 `py -3` 报 `No Installed Pythons Found`,所以:

**A. 现在就能做(不需要 Python,15 个左右)**

- `proto/guild/guild_db.proto` —— 资产三表 + 三个枚举(照 part2 §1)。**索引名 `idx_<表>_<n>` 从 0 起**(X-02),第三组是 `idx_guild_asset_op_2` 不是 `_3`;`GuildAssetOpStatus` 要有 `APPLIED_PARTIAL = 5`(X-15);**不加** `GUILD_ASSET_OP_KIND_DONATE_REFUND`(D2)。
- `proto/guild/guild.proto` —— 经济 RPC 与视图消息(§5.5);`GuildAssetOrderStatus` 同样要 `APPLIED_PARTIAL = 5`。
- `data/schema/guilddonate_table.proto`、`data/schema/guildshop_table.proto`(新)—— 配表的 schema 是 **.proto 文本文件**,不是 xlsx,现在就能写(`data/AGENTS.md` 有完整规矩)。
- `go/shared/gameday/gameday.go` + `_test.go`(新)—— 游戏日:UTC+8 每天 05:00 重置(U1)。B5 是首个使用者。
- `go/guild/internal/data/tables.go` —— 追加三张表(§9.3 的锁序)。**§5.41 漏列了它**,X-09 点名要加。
- `cpp/generated/table/CMakeLists.txt` / `table.vcxproj` / `.filters` —— 登记两张新表的 `.cpp/.pb.cc/.h`。
- `go/data_service` 的 `config.go` / `id_segment_store.go` / `etc/data_service.yaml` + `tools/scripts/k8s_deploy.ps1` —— `BootstrapTags` 加 `guild_asset_op` 号段(G-04)。**这四个文件必须与 B5b 同次或更早部署**,否则 guild 取不到号段,经济写 RPC 全挂。注意 Y-14:data_service 这几个文件多批共改,错误码一律"落码时末码 +1",不写死数字。

**B. 等用户装完 Python 才能做(4 个 xlsx)**

- `data/GuildDonate.xlsx`(新,3 档)、`data/GuildShop.xlsx`(新,11 件)—— 默认数值见 `README.md` §4。
- `data/tip/Tip.xlsx` —— `//guild_error` 组尾追加 10 个码(§5.12 / Y-05 的 B5a 一行)。
- `data/MessageLimiter.xlsx` —— 5 行(G-01 的 B5a 一行),**按 proto-gen 实际发出的号填,所以必须在 proto-gen 之后**。

> ⚠ **改 xlsx 只能 openpyxl `load → 改/insert_rows → save`,禁止整表写回**:这几个表同时有多个会话在写,二进制不能 3-way 合并,整写等于删掉别人的行。friend 会话近期会改 `Tip.xlsx` 的 B211 单元格(`FriendBlocked` 文案),别和它同时写。

### 9.6 B5d 详细设计要写什么(并行任务,无阻塞)

目标:**回档 fail-closed**。现状是 `rollback_logic.go` 把已终结的 guild 行留着不回滚,于是回档会**复制**帮会资金与帮贡(D3 / S4 C10)。

已给定的接口(S4 4.38/4.39,`04-asset-channel.md:1534-1566`):

- guild 新增内部 RPC `ListAppliedAssetOpsSince{zone_id, player_ids, since_ms, after_op_id, limit}` → `{ops, next_after_op_id}`,`GuildAssetOpBrief{op_id, player_id, guild_id, stream, kind, status, funds_delta, contribution_delta, updated_ms}`,limit ≤500。
- data_service 新增内部 RPC `GetPlayerAssetOpLedger{player_id}` → `{found, ledger}`,读 zone Redis 的玩家 blob,只读;它实现 `go/shared/assetop/reconcile.go:173` 已有的 `LedgerReader` 接口(B4b 已落,**先读磁盘上的签名**)。
- 放置规则见 **G-10**:新文件 `proto/guild/guild_internal.proto`,不标客户端协议选项(照 `trade_admin.proto`);guild 的会话拦截器对带会话 metadata 的 `GuildInternal.*` 回 `PermissionDenied`;`GetPlayerAssetOpLedger` 放 `DataService`。两者占消息号,**不进 MessageLimiter、不进客户端白名单**。
- 查询写法见 **part2 §3 最后一行**:`ListAppliedAssetOpsSince` 不要用 S4 原文的 `updated_ms > since_ms`(无索引),改 `status IN (?APPLIED,?APPLIED_PARTIAL) AND next_attempt_ms > ?since` 走 `idx_guild_asset_op_0`,再按 `guild_id` 关联 `guild.zone_id`;**`since_ms` 早于 `now − TerminalRetentionDays` 时无法证明,默认拒绝回档**。

设计要补的是接口之外的东西:**data_service 调 guild 的鉴权与拒绝语义**(91 §2 把它列为"B5d 前"要用户确认的事)、guild 不可达时的行为(Y-13:**调用失败也拒绝**)、`accept_guild_divergence` 显式放行时逐行记什么日志、分页与 500 上限怎么和 `RollbackZone` 的批量玩家清单配合、以及告警 `accept_guild_divergence` 的口径。**不采纳**"自动生成反向 Debit"(评审 #9:玩家可能离线或余额不足,补偿本身又会卡住)。

### 9.7 协作与提交(踩过坑)

- 本仓多个 Claude 会话**共用同一个 `main` 工作树**,现在工作区里还有跨 zone 传送会话的在途改动(`go/scene_manager/**`、`cpp/player_lifecycle`、`asset_op_system`、`cross_zone_test`、`deploy/k8s/scene-manager-*`)。动手前 `ListAgents` + `SendMessage` 报一次文件范围。
- 提交一律 `git commit -- <显式路径>` 或逐个 `git add <路径>`,**不要 `git add -A`**;提交前 `git diff --cached` 逐行看,只核对文件名不够(PROGRESS.md 被卷过行)。
- 要开分支的话**不要在主工作树 `checkout -b`**(会把别的会话的 HEAD 一起带走);用 `git worktree add` 另开目录,逐路径复制后提交。B3a-2 就是这么合的。
- friend 会话近期会跑一次带客户端的全量 proto-gen,会**重写 `../mmorpg-client/Assets/Scripts/Net/Generated/` 并新增 31 个 handler 桩**。如果那时你正好在动客户端,先协调。

### 9.8 交付时必须写清

按 `AGENTS.md §10.1`:Claude 不跑构建 / 测试 / 导表 / proto-gen,交付说明必须如实写「**未编译,待用户验证**」,并给出用户可直接执行的命令序列(工作目录、命令、前置清理、期望产物、通过标准、失败时保留什么)。B5a 的验证序列骨架在 `05-economy.md §5.42`,但**路径要换成本机 `D:\luyuan\wuxingqitan\mmorpg`**,且第 6 步的 `SHOW CREATE TABLE` 断言按 X-02 改成 `idx_guild_asset_op_2`。
