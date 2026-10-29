# 交接：第二轮数据与覆盖回档审计（2026-10-19）

> 只读审计：不改代码、不跑构建测试、不提交。上一个会话做完第一、二波后换账号，下一个会话按本文续做第三、四波并定稿。
> 审计正文：[data-overwrite-rollback-audit-r2-20261019.md](../../design/data-overwrite-rollback-audit-r2-20261019.md)（下称 r2 文档）。第一轮：[data-overwrite-rollback-audit-20261018.md](../../design/data-overwrite-rollback-audit-20261018.md)。

## 1. 现状

> **2026-10-24 更新（以这一段为准，下面是 10-19 的原文）**：第三波 10 路与第一轮 22 条 low 复核已做完（r2 文档 §3.6–§3.8）。第三波 6 条 medium 已按文件归属分给各会话修，状态见 r2 文档 §5：r2-lc#1/#2、r2-rr#5 数据侧由续做会话修完并经独立评审，r2-login#1 由 zone Redis 回退会话修，r2-guild#1 / social-match-battle#0 / r2-battle#2 由修复会话修，r2-match#1 负责人已拍板首发开全球匹配。**只剩换视角查找**：等周额度 2026-10-29 20:00 UTC 重置后，用本目录 `audit-workflow-wave34.js` 跑，`args` 为 `{"dRound": 0, "seenTitles": [...]}`，每次一轮，连续两轮无新发现才停，新出的 high / medium 用同一脚本的 `verify` 参数复核。开跑前先把脚本 `PRE` 里的 HEAD 与【在途文件】说明改成当时的状态（现在写的是 2026-10-22 的情况）。另有一项落码待办：负责人 2026-10-24 定为等周额度重置后，按 [location-release-binding.md](../../design/location-release-binding.md) §17.8 落 `no_qualification` 收口机制（scene_manager 带围栏撤销），先核对 §17.3 的不变量 I-1，再协调 go/scene_manager 的文件归属。然后定稿：r2 文档 §1 总结论、第一轮 §7 表末加一行指向 r2 文档、PROGRESS 定稿条目。修复全部未编译，Codex 命令见 PROGRESS 2026-10-24「第二轮审计 medium 修复：r2-lc#1/#2 与 r2-rr#5 数据侧」。

- 第一、二波共 10 路增量评审已完成。累计 **3 条 medium、22 条 low，没有 high**，全部写在 r2 文档 §3。
- 第三波（补覆盖与余项增量）、第四波（low 复核、换视角查找、对抗复核）**都没跑**。
- 定稿登记**没做**：README 登记、第一轮 §7 指向、PROGRESS 定稿条目。
- 停下的原因是旧账号周额度到 93%（2026-10-24 22:00 UTC 重置）。**周额度按账号计**，换账号后先查新账号自己的用量，不必等这个时间。

三条 medium（缺省配置下都不触发，都没做对抗复核）：

| 编号 | 一句话 | 门控 | 何时必修 |
|---|---|---|---|
| r2-battle#1 | §2.4 修复的落库重试轮无条件重写全部成员 pending，已销账的结算复活，重登重复发奖 | `GlobalMatch.Enabled`（缺省 false） | 开全球匹配前 |
| r2-mz#1 | copy 合服删 `save_barrier`、留 owner_epoch，被拷老玩家整批登录不了，恢复工具还拒收 | `-player-rows-mode copy` | 首次 copy 合服前 |
| r2-ps#1 | 字段保护台账 bootstrap 期把新增持久字段记世代 1，上线后不切 active 时滚动发布丢新列 | 仅混版窗口 | 首次上线前（改动极小）。严重度两份输出有分歧（medium / low），待复核裁决 |

前两条主会话抽查过代码路径，都属实。

## 2. 开工前必读（按顺序）

1. `AGENTS.md` 会话启动门禁，重点是「上线状态」：项目尚未上线，混版类问题降一级。
2. r2 文档全文，重点 §3（全部发现）、§5（修复方向和别的会话已修的标注）、§6（后续波次）。
3. 第一轮审计 §2–§4、§7、§8。
4. `docs/PROGRESS.md` 里这几条：
   - 「第二轮数据与覆盖回档审计：第二波完成、第三四波等周额度重置」；
   - 「location 释放绑定第二轮审计修复」；
   - 「混版本持久字段保护第 3a 批落码」；
   - go/db 的修复说明在 [db-write-fencing-mysql.md](../../design/db-write-fencing-mysql.md) 文末「2026-10-19 第二轮审计修复」。
5. 如果新账号能读到本机 `~/.claude` 下的项目记忆，再读 `session-quota-workflow-fanout`、`readonly-agents-leave-temp-copies`；读不到也没关系，要点已抄在本文 §5。

## 3. 别的会话在改的（第三、四波避开这些在途文件）

| 会话（交接时状态） | 做什么 | 在途文件 |
|---|---|---|
| 「删 location 前确认持有节点已释放」及其两个子会话（enterscene、java-gate-r2-gate-1，交接时仍在跑） | 已落码 r2-gate#1、r2-sm#1/#2/#3（未编译） | `go/scene_manager`、`cpp/nodes/gate` |
| 「go/db 存盘协议世代门禁与补投上限」 | 已落码 r2-db#1、r2-db#3（未编译），r2-db#2 延后到上线后 | `go/db/internal/saveoutbox` |
| 混版本持久字段保护 | 第 3a 批（C++ 携带、统一存盘入口、盖戳、往返测试）已落工作区、未提交 | `cpp/libs/engine/core/utils/proto/`、玩家存盘与背包、货币相关 |

- 第一波 6 条修复缺陷的复核已从第四波撤下，改到代码冻结后的增量审计（第一轮 §8 第 5 步）里查。
- 第二波审字段保护时，第 3a 批还是未跟踪的半成品，所以第三波要补审第 3a 批（见 §4.1 的 persist-schema-3a）。

## 4. 下一步

### 4.1 第三波：补覆盖与余项增量

每波最多 6 个代理，建议拆成两批。

| 批 | 路 | 来源 | 范围要点 |
|---|---|---|---|
| 3a | assets-misc | 脚本 `WAVE3` 已有 | C++ 收件箱、`id_segment_bootstrap`、`asset_op_system`、trade `asset_op_repo` 与 engine、friend 清扫、邮件清扫与 `mailadmin` |
| 3a | lifecycle-unread | 脚本已有 | player_lifecycle 未读段（交接取证、改派待确认表、EnterScene 应答分派、`HandlePlayerAsyncSaved` 等）、`redis_manager` 改动 |
| 3a | write-entries-gates | 脚本已有 | 各业务持久组件写入口 vs 冻结闸 / 退出闸，附覆盖表 |
| 3a | persist-schema-3a | **需新增** | 字段保护第 3a 批：`unknown_field_carry.h`、`player_persist_carry_comp.h`、`player_persist_marshal.h`、统一存盘入口、盖戳、往返测试；运行期整体替换持久组件的路径（任务日常重置、属性或宠物重建等）是否丢未知字段；`bag_marshal`；生成器 `generate.go` / `render.go`。同时核对 r2-ps#2、r2-ps#5 在 3a 落码后是否仍成立 |
| 3a | login-rest | **需新增** | loginqueue（`queue.go`、`gatetoken.go`、`dispatcher.go`、`capacity.go`）：同一账号或角色能否被并发放行两次；dataloader（`ensure_player_all_data_async.go`、`parent_commit_barrier.go`）：异步加载与在途存盘并发、与字段保护世代口径；`node_cache` / `watcher`；`entergamelogic.go` :420 之后；`writeAccountBlob`；疑点：`go/data_service/cmd/debugutil/bundle.go:399` 让 debug_import 不拿账号锁写回 `account:{account}` |
| 3b | guild-rollback | 脚本已有 | 帮会逻辑正文（资金、贡献、成员、解散）、`k8s_zone_rollback.ps1` 正文 |
| 3b | match-rank-db | 脚本已有 | 组队匹配、评分排行、go/db `durable_enqueue` / `retry_stranded` 余项 |
| 3b | merge-zone-rest | **需新增** | `route_store.go`、`route_publish.go`、`fence.go`、`relocate_run.go`（单玩家迁库双写者窗口）、`unmerge.go` 正文、guild `merge_fence.go`（围栏期间资金 / 成员 / 贡献写入被拒还是丢失）、合服后 player / item / mail / 帮会 / 交易 ID 撞号与号段续接、`tools/data_consistency_check` |
| 3b | route-reconcile-rest | **需新增** | `routing/route_publish.go` 的 W1 发布失败语义（影响 r2-rr#1 受害面）、`route_read.go` / `store/route_reads.go`、`route_store.go` 的 RegisterHome / ScanRoutes / GetRoutes 正文、`shared/placement/route_ready.go` 读脚本与 VerifyRouteReady |

新增的 4 路按脚本里 `WAVE3` 的写法补 scope：范围、读哪些文件、重点问题。第二波各路的"未读部分"原文在 r2 文档 §6。

### 4.2 第四波：复核与换视角

- **(c) low 复核**：脚本 `LOW_REVIEW` 两路（Sonnet），对象是第一轮 §4 的 22 条。第 8 条（login 第 1 条「账号 blob 两个写者不走建角锁」）已由第二波证实不成立，见 r2 文档 §4。第 4 条「存盘不保留未知字段」要按第 3a 批是否已提交来判。
- **(d) 换视角连续查找**：脚本 `D_ROUNDS`，连续两轮无新发现才停，`dMaxRounds` 缺省 3（脚本里定义了 5 轮）。到上限仍有新发现时要在文档里写明"因轮数上限停止"。
- **对抗复核**：r2-battle#1、r2-mz#1、r2-ps#1（裁决 medium 还是 low），加上第三、四波新出的 high / medium。脚本对 high 用两个视角，对 medium 用一个。

## 5. 怎么跑

- **脚本**：本目录 [audit-workflow.js](audit-workflow.js)，是原会话工作流脚本的原样副本。用 Workflow 工具的 `scriptPath` 指向它；跑工作流前要用户已开 ultracode 或明确同意。
- **只能全新运行**：`resumeFromRunId` 只在原会话有效，新会话一律全新跑。
  - `args.waves` 不要再带 2（第二波已完成），例如 `[3]`、`[4]`、`[5]`。
  - 先 `findOnly: true` 只跑查找，看额度再跑复核。
  - `wave1Titles`：传入第一、二波全部 25 条的"编号 + 标题"（取 r2 文档 §3 各表的编号列与问题列），给换视角查找去重用。
  - `extraVerify`：传入 r2-battle#1、r2-mz#1、r2-ps#1。字段按脚本里的 `FINDING` 结构，可以只填要点，并写"详见 r2 文档 §3.3"。
  - 同一次运行里，复核阶段只看本次的发现加 `extraVerify`。分多次运行时，前几次的 high / medium 要手工放进 `extraVerify`。
- **额度规则**：
  - 开跑前查用量；周额度超过 80% 时只跑风险最高的一波；每波不超过 6 个代理；每个代理返回后查一次，周额度到 92% 就停，已返回的结果写进 r2 文档，没跑的标待续。
  - 旧账号实测：只读代理每个约占 5 小时窗口的 2.5%、周额度的 1%；新账号套餐不同时另估。
- **两个坑**：
  1. **不要给工作流里正在跑的代理发消息让它收尾。** 消息会另起一个实例并行跑，原实例照样跑完，费用翻倍、输出两份（第二波就是这样，r2 文档按并集登记）。要缩规模，就在开跑前定好代理数，或者 TaskStop 整条工作流。
  2. **只读代理按提交读文件时，可能用 `git show … > 文件` 在临时目录留副本。** 提示里已写明禁止；跑完仍要核对代理转录里有没有 Edit / Write 调用和重定向。
- **只读约束**：本审计不改代码、不跑构建测试（AGENTS §10.1），不提交。只写 r2 文档、本交接目录、README 与 PROGRESS。

## 6. 定稿清单

- [ ] r2 文档：状态改为"完成"；§1 总结论；§3 补第三、四波发现和复核结论；§4、§5、§6 收尾；§7 补额度与证据。别的会话在 §5 写的"已修 / 延后"标注要保留。
- [ ] `docs/README.md` 设计索引登记 r2 文档一行（第一轮在同一节，约第 48 行）。
- [ ] 第一轮审计 §7 表末加一行指向 r2 文档。
- [ ] `docs/PROGRESS.md` 纯追加定稿条目，写明 Java 版状态。
- [ ] 把三条 medium 的修复建议交给负责人，由负责人决定是否另开修复会话。

## 7. 不在本审计范围

- Java 版（`D:\luyuan\xuanming-server-mmo`）同类审计，按该仓库 `AGENTS.md` 执行。
- 第一轮 §8 的第 1 步（Codex 验证已落修复）、第 4 步（运行期一致性压测）、第 5 步（冻结后增量审计）。

## 8. 整条审计线的修复待办（2026-10-19 主会话补记，不属于只读审计）

本节由发起两轮审计的主会话补写。第一轮审计结论、修复派发与验证都在 [第一轮审计](../../design/data-overwrite-rollback-audit-20261018.md) §7 / §8。换账号后，新会话按下面的顺序接手。

### 8.1 已写完代码、待 Codex 验证（Claude 不再动）

按 AGENTS §10.1，协议生成、编译、测试、冒烟都交给 Codex。命令和通过标准见各 PROGRESS 条目（按标题检索，行号会漂移）：

| 审计项 | PROGRESS 条目标题 | 现状 |
|---|---|---|
| 第一轮 §2.1 | 「删 location 绑定持有者释放」「登录撞上未释放的持有者」「会话结束释放带覆盖身份」（含「评审补两条测试」）「删 location 绑定持有者释放:静态评审复核两项修正」 | 已过：生成、6 个 Go 服务编译、C++ 增量 Build、scene_manager 全量。未过：player_locator 测试、`routing_identity_test`、`cross_zone_test`、真实环境故障演练（scene_manager 须 `AllowUnsafeCrossNodeHandoff: false`）。`EnterFenceEcs.LoadCallbackBindsNewestAdmitted` 夹具失败，Codex 已备补丁，等负责人决定 |
| r2-gate#1、r2-sm#1/#2/#3 | 「location 释放绑定第二轮审计修复:r2-gate#1 / r2-sm#3 / r2-sm#1 / r2-sm#2」及其 Java 状态更正 | 未编译、未测试 |
| r2-sm#3 同源 | 「EnterScene 陈旧位置判定补注册表正面证据」 | 未编译、未测试 |
| 第一轮 §2.2 | 「存盘协议世代门禁与补投有界」，共 7 步 | 一步都没跑 |
| r2-db#1、r2-db#3 | 「第二轮审计 r2-db#1 / r2-db#3 修复：补投发布失败不计次、在途戳截断」 | 未编译、未测试 |
| 第一轮 §2.3 | 「修跨账号登录态继承」 | Codex 已跑 login 两包，249 通过，vet 与 build 也过；没有真实多 gate 验证 |
| 第一轮 §2.4 | 「审计 §2.4 战斗结算三处：丢数据修复」 | 未编译、未测试；Java 在 PARITY.md 登记为待做 |

### 8.2 还没写代码的修复（新账号的 Claude 落码）

| 优先级 | 编号 | 何时必修 | 修复方向出处 |
|---|---|---|---|
| 1 | r2-ps#1 字段保护台账 bootstrap 世代 | 首次上线前（改动极小；严重度待第四波裁决） | r2 文档 §3.3 / §5 |
| 2 | r2-battle#1：§2.4 修复的落库重试轮复活已销账的 pending | 开全球匹配前；和 §2.4 同一批验证最省事 | r2 文档 §3.3 / §5 |
| 3 | r2-ps#2：欠款守卫遇到未知字段时，GM 追缴被判终态而丢失（low） | 开收件箱消费前 | r2 文档 §3 / §5 |
| 4 | r2-mz#1：copy 合服后被拷玩家无法登录 | 首次 copy 合服前 | r2 文档 §3.3 / §5 |
| 5 | 第一轮 §2.5 三条：全球匹配票据卡死、Commerce.Tick 队头阻塞、被放弃的托管腿滞留资产 | 开全球匹配 / 交易资产通道前，作为开通门禁 | 第一轮 §2.5 |
| 6 | r2-ds#1 / r2-ds#2：debug_import 身份闸、重铸 epoch（low，调试工具） | 运维要用 bundle 导入之前 | r2 文档 §5 |

做法沿用这条线的惯例：
- 每条先对照当前代码复核，成立才修；
- 先写回归测试再修；
- 不编译、不提交；
- PROGRESS 纯追加，写明给 Codex 的命令；
- 在 r2 文档 §5 或第一轮 §7 补修复指向，写明 Java 版状态。

### 8.3 其余待办

- 本文 §4 的第三、四波和 §6 定稿，只读。
- **Java 版同类审计**：在 `D:\luyuan\xuanming-server-mmo` 按该仓库 `AGENTS.md` 执行，还没开始；§2.4 的 Java 修复在其 PARITY.md 登记为待做。
- 第一轮 §8 第 4 步（运行期一致性压测，L2–L4）、第 5 步（冻结后增量审计），交 Codex 或后续会话。

### 8.4 额度

旧账号周额度已到 97%，10-24 22:00 UTC 重置。新账号接手前先查自己的用量。只读审计按 §5 的规则分波跑。8.2 的修复每条一个会话即可，不需要 workflow。

### 8.5 §8.2 修复的落码状态（2026-10-24，「数据覆盖审计交接文档」会话）

§8.2 六项与审计会话后来分来的 r2-battle#2、r2-guild#1 都已落码，**全部未编译、未测试，待 Codex 验证**。命令与通过标准见 PROGRESS 里 8 条「2026-10-24 Claude：…」条目；修复说明见 r2 文档 §5 与第一轮 §2.5 的「已修 / 修复」标注。

| 项 | 状态 | 评审 |
|---|---|---|
| r2-ps#1 | 只改文档（release-checklist §2.1 ⑦、§6.2，混版本设计 §8.1 第 6 条）；机器门禁待办 | 不需要 |
| r2-battle#1 / #4 | 已落码；本局落库标记判重、槽位守卫、快慢两段重试 + 6 小时总跨度、滞留提醒 | 两轮评审，必修项已修 |
| r2-ps#2 / #5 | 归混版本会话 3a′，不在本线 | — |
| r2-mz#1 | 已落码；前向 copy 保留屏障、`verify:save_barrier` 照 login 冷加载分三档、恢复工具 `-allow-home-fallback` | 评审后已修正 |
| 第一轮 §2.5 social-match-battle#0 | 已落码（上线阻塞）；无原件短路、过截止不再续租 | 评审必修项已修 |
| 第一轮 §2.5 go-asset-services#0 / #1 | 已落码；单行隔离、预算从父截止推出、放弃腿补偿 | 评审必修项已修 |
| r2-ds#1 / #2 | 已落码；本地标记证明、Redis run_id 不再算证明、重铸代次补屏障令牌；账号目录疑点不成立 | 评审必修项已修 |
| r2-battle#2 / r2-guild#1 | 已落码（上线阻塞），主路径依赖并行「v1 终局接缝」轨道整批落地 | **独立评审未跑**（周额度 81% 时推迟） |

还开着的：
- r2-battle#2 / r2-guild#1 的独立语义评审，以及 r2-guild#1 修复方向里的「带审计的人工释放 CLI」。
- 截止竞态（强制平局的 COMPLETE 与到期 ABORT 抢同一主键）归并行 v1 轨道，须在其验收前加宽限。
- cross-service-assets#1（player_battle.cpp 在线无组件分支把锁不在当作废而删 pending）由另一会话修，它是 r2-battle#4 低频阶段对在线成员生效的前置条件。
- Java 待做项已登记 Java 仓 `PARITY.md`：§2.4 行补 r2-battle#1/#4 要求、交易单行预算余量、debug 导入 Redis 身份闸、全球成局无原件出口、全球局 v1 终局与帮会占用释放。
