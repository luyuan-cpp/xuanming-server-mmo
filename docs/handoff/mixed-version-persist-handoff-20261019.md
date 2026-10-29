# 混版本持久字段保护：交接（2026-10-19）

> **一句话**：设计已经拍板（v0.5）。各批状态：
> - 第 1 批（台账、生成器、冻结 option、戳字段）已由 Codex 验证，并已入库；
> - 第 2 批（go/db 的 C0 / C1 / C1b / C2）主体已入库，go/db 模块构建通过，单测还没跑；补齐部分（单测、配套工具、告警、运维手册）在工作区，未提交；
> - 第 3a 批（C++ 对象内携带、统一存盘入口、盖戳、往返一致性测试）刚写完，**未编译、未测试、未提交**；
> - 只读核对发现 3a 有 **3 处已知缺陷**，要在第 3a′ 批先修（§3.1）；
> - 第 3b、4、5 批没开工，其中 3b 动手前**必须先改设计 §5.2**（§3.2）；
> - Java 版全部待做。
>
> **规格**：[docs/design/mixed-version-persist-field-protection.md](../design/mixed-version-persist-field-protection.md) v0.5。本文只记做到哪、还差什么、怎么验证、有哪些坑；机制细节以设计稿为准，本文与设计稿冲突时，以本文 §3.2 列出的待改点为准，并先把设计稿改掉。
>
> **起草**：「灰度升级 / 混版本丢字段」会话（Claude）。交接前用一次只读多视角核对（事实、开工可用性、未编译代码风险）复查过本文：风险项都经对抗复核，均未被驳倒。用户换账号继续。

---

## 0. 新会话开工前

1. 按 AGENTS.md 会话启动门禁读完；再读设计稿 §3（世代与规则）、§4（机制一）、§5（机制二）、§9（分批与实施状态）、§13（已拍板项）。
2. `git status`。工作区同时有多个会话在改，本主题的文件只认 §7、§8 列出的那些，**其他会话的改动不要动**（§6 第 2 条）。
3. 先向用户要第 3a 批的 Codex 结果（§5.2），再做 3a′（§3.1）。3b 要改的 `player_lifecycle.cpp`、`currency_test.cpp` 和 3a 有重叠；3a 编译不过就开 3b，两批会搅在一起。

## 1. 已拍板（不要再讨论）

负责人 2026-10-19 通过设计 §13 第 1–5 条：

| 项 | 结论 |
|---|---|
| go/db 写法 | **不做"只写改动列"**。写方世代超前就延后（C1），倒退就拒写（C1b），未盖戳就拒写（C0），其余整行写（§6.1） |
| option 读取 | 生成器从 descriptor 的未知字节里动态读取 `OptionPersistFrozen`（message，500040）和 `OptionPersistUncarried`（field，600040），不升级外部模块 |
| C3 / C4 | 先上 warn，再切 enforce。切换条件见 §3.5 |
| 引导期 | 台账 `state=bootstrap` 期间新增字段一律记世代 1，不升世代 |
| 开发约束 | 接受冻结清单、R2 / R4 / R5 和 §4.4 的守卫：背包布局层与任务容器层不能直接加字段；带新属性的物品在旧版里不合堆；带新字段的欠款在旧版里不能再挂新欠款 |

写代码时直接照用的不变量：
- **P1**：每一跳要么完整保留（输出世代 = max），要么拒绝 / 延后。
- **戳 0 按 1 处理**。
- 存盘戳 = max(本二进制世代, 两表加载世代的**最小值**)，两张表盖同一个值。
- login 新号种子**不盖戳**（§5.4）。
- 世代号只在主干上发放（`proto/persist_schema_gen.txt`）。`cpp/generated/persist_schema/persist_schema_gen.h` 是生成产物，不要手改。

## 2. 各批状态

| 批 | 内容 | 状态 | 证据 / 入库 |
|---|---|---|---|
| 1 | 台账、生成器（`tools/proto_generator/protogen/internal/persistschema`）、两个 option、两表 `persist_schema_gen` 字段、生成常量 | **已验证** | `build/verification/enter-game-smoke-20261019-044832/`：生成通过、生成器单测通过、本地库迁移出两列。已入库 |
| 2 主体 | go/db C0 / C1 / C1b / C2、延后与死信原因、引导标记、指标 | 构建通过，**单测未跑** | 同上记录里，go/db 模块原生退出 0（输入含 `kafka/persist_schema.go`）。随 `53361d055` 入库 |
| 2 补齐 | 单测、C1b 真库用例、`data_stress` 盖戳、`debug_import` 世代前置检查、告警规则、运维小节 | 已写，**未编译、未测试、未提交** | 文件见 §8 |
| 3a | C++ 携带、统一入口、盖戳、守卫、往返一致性测试 | 已写，**未编译、未测试、未提交** | 文件见 §7；命令见 §5.2 |
| 3a′ | 3a 的 3 处已知缺陷 | 未开工 | §3.1 |
| 3b | 严格屏障 Lua 的 C3、`OnSaved` 返回值 2 / 3 与新回调、部署配置 | 未开工；**先改设计** | §3.2 |
| 4 | login C4 | 未开工 | §3.3 |
| 5 | §12 的两个持久值域删数据点 | 未开工 | §3.4 |

另外，`save_barrier_restore` 接受 `schema_gen`、按两行世代的最大值重建屏障，由 Codex 在 `da0f1822c` 完成，不属于本会话的改动。

## 3. 下一步

### 3.1 第 3a′ 批：3a 的已知缺陷（先修）

三条都经过逐行核对和对抗复核。之所以没在 3a 里顺手修掉，是因为 3a 的改动面已经接近 30 个文件（AGENTS §10.2）。三处源码里都已写了注释，标明缺口与归属。

**① 收件箱执行器把版本错位记成了终态拒绝（中，语义错误）**
- 现状：
  - `CurrencySystem::AttachDebt` 遇到带未知字段的既有欠款时，返回 `kFeatureUnavailable`（1006）。
  - 生产上唯一的调用方是 `gm_sanction_inbox_executor.cpp` 的 `AttachCurrencyDebt`（约 124 行）。它用 `FromTip` 把任何非 `kSuccess` 都映射成 `Rejected`。
  - `player_inbox_system.cpp` 约 880 行据此记 `PLAYER_INBOX_OUTCOME_REJECTED`、推进水位、确认。
  - 结果：玩家之后落到新版本节点，这笔 GM 欠款也不会再挂。这与 `player-inbox-service.md` 的规定冲突："版本错位绝不能当成终态的 REJECTED"（约 955 行、§11 第 13 行）。
- 修法：
  - 在执行器里，`HasCurrencyState` 通过之后、调用 `AttachDebt` 之前做纯判定：已有同币种欠款的 `unknownFields` 非空，就 `return Deferred();` 并打 WARN。
  - 不要用"返回码 == 1006"去反推，1006 是通用码。
  - 更干净的写法：在 `CurrencySystem` 上加只读谓词（例如 `DebtCarriesUnknownFields(entity, CurrencyType)`），让执行器与 `AttachDebt` 共用。
- 同步改：
  - `gm_sanction_inbox_executor.h` 的结局清单；
  - `player-inbox-service.md` 约 959 行"v1 的执行器只在持久货币组件缺失时返回 DEFER"；
  - 设计稿 §4.4 表；
  - `currency_system.cpp` 守卫上的注释。
- 测试（`gm_sanction_inbox_executor_test.cpp`）：
  - 用 `LoadFromProto` 灌入一笔带未知字段的金币欠款，断言 `kDefer`、owed 不变、未知字段保留；
  - 另一个币种照常 `kApplied`。

**② `Bag::AddItems` 的规划没跟上新判定（低，潜在问题）**
- 现状：
  - 提交侧的 `CanStack(item, proto)` 对**传入**物品也判 `IsIndependentInstance`。
  - 规划侧 `bag_system.cpp` 约 646–648 行仍只按 `has_equip()` 把物品分进 `separateInstances` / `stackUnits`。
  - 于是带未知字段、`maxStack > 1` 的传入物品会少铸号：Debug 下命中"预取身份数与规划不一致"的 assert，Release 下返回 `kInvalidTableData`。
  - 现有调用方都按 config + count 新建物品，触发不到。但 `vector<InitItemParam>` 入口本来就是为搬完整实例准备的。
- 修法：
  - 把 `ItemStore::IsIndependentInstance` 从 private 移到 public，紧挨 `CanStack`；
  - `bag_system.cpp` 646–648 行改用它分流；
  - 改准 `item_store.h` 那段注释。
- 测试（`bag_instance_data_test.cpp`）：
  - 已有同 config 半堆时，传入一条带未知字段的 `InitItemParam`：期望 `kSuccess`、半堆不变、另开新实例、未知字段保留；
  - 同批两条同 config 的未知字段条目：期望建出 2 个实例。

**③ 宝石拆卸预检与容器层判据不一致（低）**
- 现状：`player_equip.cpp` 约 439–447 行的 `HasStackWithRoom` 仍只判 `!item.has_equip()`。背包满、唯一有余量的同种宝石堆带未知字段时，预检会放行，然后落到容器层会打 ERROR 加调用栈的不足分支。结果仍是正确拒绝，不丢数据。
- 修法：用一个只有 `config_id` 的探针实例调 `ItemStore::CanStack(item, probe)`，再加 `item.size() < maxStackSize`。**不要**改用 `CheckSpaceFor`，它失败时同样走 `PrintStackAndReturnError`，不是静默的。
- 测试：背包占满、唯一未满的同种宝石堆带未知字段时，`RemoveGem` 返回 `kEquipGemBagFull`，且没有调到 `BagService::AddItems`。

### 3.2 第 3b 批：C++ 校验（设计 §5.2、§9 表 3b 行、§10）

**开工前先改设计 §5.2**，核对确认现有设计有以下问题：

1. **"第一次存盘"的判定按现在写法有确定的失败路径（高）**。设计说：只在实体发起的第一次存盘里传真实加载世代，之后 `ARGV[9] = ARGV[8]`。这样做有三个问题：
   - 用"首存已落地"（例如 `PlayerLastPersistedSnapshotComp` 是否在场）当标记：
     - 这个组件只在 `HandlePlayerAsyncSaved`（`player_lifecycle.cpp` 约 2396 行）里创建；
     - `OnSaved` 成功时如果有排队值，会直接发排队值、不回调（`redis_client.h` 约 999–1006 行）；
     - 所以首存 A 在途时排进的 B，入队时仍带真实加载世代。A 落地后 hash 升高，B 就被判倒退。enforce 下返回 3，组件永远建不起来，之后每次存盘都被拒，S-A 重现。
   - 用"已发起"当标记：首存可能还没落地就被后来的值顶替，C3 一次都没比较。顶替路径有三条：
     - 排队值顶替（约 680–686 行）；
     - 空应答或 ERROR 后的 `QueueSaveForRetry`（约 856–862 行）；
     - `OnReconnected`（约 437–441 行）。
   - "回包丢失后重发的一定是同一元素"这个前提不成立：有排队值时发出的是新版本，`pending ≠ ARGV[3]`，不走重放分支。

   二选一，写进设计后再动手：
   - (a) 交给 Lua 判定：C++ 一律传真实加载世代；脚本只在"上一次写屏障的不是本 owner 代次"时做 C3，即已存 pending 的 owner 前缀 ≠ `ARGV[2]`。要先核实"同一 owner_epoch 只对应一个加载实体"。
   - (b) 留在 C++ 判定：
     - "仍需比较"的状态挂在 `Element` 上；
     - 未证实提交就被顶替时，后继元素继承这个状态；
     - `ARGV[9]` 在 `IssueSave` 发送时求值，不在 `EnqueueSave` 时定死；
     - 另外要论证"已提交但回包丢失"时不会误报。

   两种都要在设计 §10 补三个用例：首存在途时有排队值；首存遇空应答后被顶替；首存遇 ERROR 后被顶替。

   附带一点：按设计原文"一旦发起过存盘"，被 enforce 拒绝的那次也算"发起过"，于是第二次就绕过了 C3。owner_epoch = 0 的 `Save` 也会把标记置上。选 (b) 的话要写明：只有严格屏障存盘返回 1 / 2 才结束"首存"状态，返回 3、epoch = 0 的 `Save`、`SaveWithCommitBarrier` 返回 false 都不算。选 (a) 则这个问题随之消失。
2. **C3 在脚本里的位置没规定（中）**。现有脚本的顺序：
   - 参数校验；
   - owner ≠ `ARGV[2]` 返回 0（约 153 行）；
   - pending 比 `ARGV[3]` 新则报错（约 154 行）；
   - 同版本冲突与重放（约 157–162 行）。

   C3 必须放在**最后**，即这些判定之后。否则已被废黜的旧持有者会收到 3 而不是 0，新回调又把它当"其余情况"保留，它就不会被 `DestroyDeposedPlayer` 销毁。另外补三条规则，各配一个单测：
   - hash 里的 `schema_gen` 损坏时，写入值取 `ARGV[8]`；
   - `ARGV[9] > ARGV[8]` 按参数错误返回 error_reply；
   - 返回 3 时，如果排队值带着不同的 `guard_expected`，照 0 分支的做法照发（约 944–961 行）。
3. **模式配置必须归一化（中）**。proto3 的 string 缺省是空串。旧 ConfigMap 漏了这个键或写错时，`ARGV[10]` 就不合法，脚本返回 error_reply；`OnSaved` 对 ERROR 一律无限重试（约 930–933 行），结果是全服存不进盘。规定：
   - 读配置时，缺键或空串按 `warn` 处理；
   - 其他非 `warn` / `enforce` 的值拒绝启动；
   - `ARGV[10]` 只传归一化后的两个字面量；
   - 配一条配置层单测，k8s 契约测试钉住 ConfigMap 取值；login 同理。
4. **没有能生效的日志告警通路（中）**。Loki ruler 没挂规则目录，也没配 `alertmanager_url`，`deploy/k8s/owner-epoch-alerts.yaml` 的注释已写明这一点；ruler 接线归上线批 BK8s。沿用 `[OwnerEpoch]` 的先例：
   - C++ 侧计数，在 `redis.cpp` 的 30 秒汇总里另起一行；
   - LogQL 作为值班固定查询写进 `deploy/k8s/README.md`「玩家持久 schema 世代」小节；
   - **不得宣称"告警已配"**。

**规则**（其余照设计 §5.2 原文）：
- 参数从 7 个增加到 10 个：`ARGV[8]` 输出世代，`ARGV[9]` 加载世代，`ARGV[10]` 模式。
- 同版本重放（`pending == ARGV[3]` 且父快照字节相同）跳过 C3，返回 1。
- 新增 `num_gt`：先比长度，再按字节比。现有的 `gt` 只适用于等长字符串。
- 写入成功时，在同一个 `HSET` 里写 `schema_gen = max(存储值, ARGV[8])`，只升不降。
- `OnSaved`：返回 1、2 都算提交证据，返回 2 另打计数。返回 3 不重试、不触发提交钩子，同 key 的排队值丢弃（例外见上面第 2 条），走新回调。新回调不能复用 `HandlePlayerSaveRejected`，分派如下：
  - 退出中走 `DestroyDeposedPlayer`；
  - 交接中走 `AbortTravelHandoff`；
  - 其余情况保留实体、打日志、本次不落盘。

**代码位置**（`redis_client.h` 的行号按 2026-10-19 工作区，均为约数）：
- `cpp/libs/engine/infra/storage/redis_client/redis_client.h`：
  - 头注释约 63–64 行；`kSaveWithCommitBarrierLuaScript` 约 66 行；脚本里 `#ARGV ~= 7` 约 98 行；
  - `Element` 结构体约 235–265 行：要带上 `ARGV[8..10]`，重试、NOSCRIPT 重发、`OnReconnected` 才能发出同一组参数；
  - 新回调类型约 301 行附近，setter 约 323 行附近（`SetSaveRejectedCallback` 在那里），成员约 1143 行附近；
  - `SaveWithCommitBarrier` 约 344 行；
  - `EnqueueSave` 约 638–652 行，要拷贝新字段；
  - `SendSaveCommand` 约 743–781 行：EVALSHA 与 EVAL 两个格式串都从 11 个 `%b` 改为 14 个，"七个 ARGV"的注释同步改；
  - `OnSaved` 约 892 行：2 / 3 两个分支以 `!barrier_key.empty()` 为条件，放在约 973 行"只认整数 1"之前；
  - `ScriptSlot`（约 619 行）**不用改**：SHA 按脚本文本计算，脚本一改自动更新。
- `cpp/libs/services/scene/core/system/redis.cpp:29`：接线 `SetSaveRejectedCallback`；新回调也在这里接，30 秒汇总也在这个文件。
- `cpp/libs/services/scene/player/system/player_lifecycle.cpp`：
  - 调用点约 4490–4501 行；
  - 模式从 `tlsNodeConfigManager.GetBaseDeployConfig()` 读；
  - 输出世代 = `message->player_database_data().persist_schema_gen()`（统一入口盖的戳）；
  - 加载世代 = `player_persist::LoadedSchemaGen(player)`。
- 部署配置：
  - proto 改 `proto/common/base/config.proto` 的 `BaseDeployConfig`（约 75 行），改完由 Codex 跑 proto 生成（AGENTS §4）；
  - C++ 读取改 `cpp/libs/engine/config/config.cpp` 的 `readBaseDeployConfig`。注意不是 `services/scene/core/config/config.cpp`；
  - 还有 `bin/etc/base_deploy_config.yaml`、`tools/scripts/k8s_deploy.ps1` 的 ConfigMap 及其契约测试、`start_game.ps1`。
- `cpp/tests/currency_test/currency_test.cpp` 要改写的用例：
  - `RedisCommitBarrierErrorsNeverAdmitSql`：约 1005 行 `malformedAck = IntegerReply(2)` 断言"返回 2 不是提交证据"，要改；
  - `RedisSaveScriptsTouchOnlyDeclaredKeys`：脚本字符串字面量白名单 `barrierLiterals`（约 1461–1469 行），新增的 `'schema_gen'`、`'warn'`、`'enforce'` 和新错误文案都要登记；
  - `RedisCommitBarrierSemanticsAgainstRealRedis`：`saveIntent`（约 1666–1677 行）是 7 参，要改成 10 参。**陷阱**：别名用例（约 1743–1749 行）也是 7 参。脚本改成 10 参后，它会先撞参数个数错误，照样拿到 ERROR、用例保持绿，却不再检验 "keys must differ"。要改成 10 参并断言这条错误文案；
  - `PlayerSqlTasksRequireParentCommitSourceContract`：按源码文本定位，用 `"\n\t};\n\tif (ownerEpoch != 0)"` 找钩子末尾（约 1190 行）。在 `publishDbTasks` 与 `if (ownerEpoch != 0)` 之间插代码会断；
  - `SaveWithCommitBarrier` 的测试调用点（约 965、967、996、1064、1081 行）随签名一起改；
  - `RedisCommitBarrierRejectionDiscardsSameOwnerHooks`（约 1052 行）只覆盖返回 0；返回 3 的用例要用带 `barrier_key` 的元素，可参照约 961–963 行的手工设置；
  - 新增用例：返回 2 算提交证据；返回 3 不触发钩子、丢弃同 key 排队值、走新回调；新回调在退出中、交接中、在线三种状态下的分派。

**owner_epoch = 0 的兼容路径**（`Save`，不走屏障）不经过 C3，设计里已登记为残余风险。

### 3.3 第 4 批：login C4（设计 §5.1、§5.4，§9 表 4 行）

- `go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go`：
  - `beginParentLoad`（约 71 行）的脚本，在返回的三元组后追加第 4 个元素 `HGET schema_gen`；
  - **同时**把约 77 行的 `len(values) != 3` 改成 4，给 `parentLoadSnapshot` 加字段。只改 Lua 的话，所有快路径 EnterGame 都会报"父快照屏障回复长度错误"；
  - `publishParentLoadScript` 复用脚本时只读前 3 个元素，不受影响；
  - C4 只在 `raw ≠ ""` 的快路径上判；
  - `beginParentLoad` 是自由函数，拿不到配置，模式要改签名传进去。调用点有 `ensure_player_all_data_async.go:63`，以及 `parent_commit_barrier_test.go` 约 170、214、231、298 行。
- 行为：
  - warn：照常 EnterGame，计数 `login_persist_schema_regression_total`；
  - enforce：返回错误，走现有失败路径（`notifyEnterGameFailed` 推固定的 `kEnterSceneFailed`），**不新增 tip**。
- 配置：
  - `go/login/etc/login.yaml` 加同名项；
  - `go/login/internal/config/config.go` 的结构体加字段；
  - **`tools/scripts/k8s_deploy.ps1` 的 login ConfigMap 模板**（约 8100 行起，`Get-AuthoritativeScalar` 逐键镜像）及其契约测试也要加，否则集群里永远是零值，切 enforce 时悄悄不生效；
  - 归一化规则同 §3.2 第 3 条。
- 测试：
  - 种子不盖戳时，`isUnownedLoginSeed`（同文件约 119 行）的判断不变；
  - `backfillPlayerIdentity` **不在 dataloader**，在 `go/login/internal/logic/clientplayerlogin/player_class_backfill.go:26`；往返测试（未知字段和戳都保留）加进同目录的 `player_identity_backfill_test.go`。
- 运维：`deploy/k8s/README.md` 补 login 侧说明。可选：go/db 能力心跳带世代号，`k8s_deploy.ps1` 发布 scene 前做只读预检。

### 3.4 第 5 批：持久值域（设计 §12，scene 无条件回滚的前提）

- **越界 `bag_type`**：
  - `bag_marshal::Unmarshal` 目前直接丢弃越界条目，要改为保留并原样存回；
  - 运行时的包是 `std::array<Bag, kBagTypeCount>`，越界条目没有地方放，`BagAllData` 又是冻结类型。所以要新增一个持久数据类载体，并触发 §6 第 6 条的整套同步清单；
  - `capacities` 也不能只输出 `kBagTypeCount` 个。
- **查不到表的技能**：`SanitizeSkillList` 改为保留但不生效。之后所有消费 `skill_list` 的路径都要跳过查不到表的 id，否则会把空的表查询当成合法技能。消费路径包括施放、下发客户端、冷却、战斗快照；先列出全部消费点，再配回滚场景测试。
- 这两处修好之前，scene 的发布单必须写明"本版本没有扩展持久值域"，否则不能回滚 scene（§8.3）。

### 3.5 上线前的运维前置（设计 §8.1；项目尚未上线，不阻塞开发）

- 发布顺序：DDL → go/db（不开 C0，`G = 1`）→ login → scene（C3 / C4 用 warn）。
- 引导期关闭：
  - 前提：go/db 侧原值为 0 的写 body 计数归零，并持续一个完整存盘周期；
  - 然后由负责人逐个 zone 写引导关闭标记 `db:persist_schema:bootstrap_closed:zone:{z}` = `closed`（开启 C0），再去掉台账的"引导中"标记；
  - 台账状态不能从 `active` 改回 `bootstrap`。
- 切 enforce 的放行条件：
  - 在 staging 上做真实升世代演练：加一个测试用的持久字段让 gen 升到 2，新旧两套二进制按 §7 矩阵混跑；
  - 重点验证"新版接手旧戳数据并连续存盘"和"回包丢失后重放"两项；
  - **演练中 C3 / C4 的倒退计数为 0**，**线上 warn 观察期内的计数也为 0**，两条都满足才能切换。没跑完之前，不能对外说"混版本安全"。

## 4. 已知限制（不是缺陷，但要知道）

- 往返测试的覆盖率断言只检查"从两张表可达、却没在 `BuildEveryPersistedType` 里出现"的消息类型。其他手搭组件清单漏改时它不会点名，要靠对应用例因 marshal 拒存而失败才能发现（§6 第 6 条）。
- 3a 的新代码都没有经过编译。核对时没有发现会让编译失败的问题，但这不是编译证据。

## 5. 待 Codex 验证（照抄执行）

通用约定：
- 失败时保留首个错误或首个失败用例的完整输出，不改断言、不连续重试；
- 在拿到 Codex 结果之前，Claude 不得宣称"编译通过"或"测试绿"。

### 5.1 第 2 批补齐（Go）

工作目录 `go/db`：
1. `go vet ./...`
2. `go test ./internal/kafka/ ./internal/metrics/ -count=1`
3. 配了 `ASSET_FENCE_TEST_DSN` 时，再跑 `go test ./internal/kafka/ -run TestIntegrationPlayerSQLRejectsPersistSchemaRegression -count=1`。
   - 自起一次性 MySQL，不要借 `codex-*` 容器，它们会被撤；
   - 遇到 Access denied，改用 `go test -c` 编出测试二进制再执行。
4. `go build ./cmd/data_stress/`

工作目录 `go/data_service`：`go test ./cmd/debug_import/ -count=1`。

### 5.2 第 3a 批（C++）

工作目录：仓库根。MSBuild 一律串行 `/m:1`；`$msb` 用 `tools/scripts/run_cpp_tests.ps1` 里 `Resolve-MSBuild` 找到的那个。

1. 编译 game.sln，输出落盘：

   ```
   & $msb game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal 2>&1 | Tee-Object build/verification/persist-3a/game-sln.build.log
   ```

   - `game.sln` 的 Debug|x64 已包含 bag_test、currency_test、cross_zone_test 三个测试工程，这一步会把测试代码一起编。通过标准：0 错误。
   - **注意**：工作区里还有其他会话没编译过的 C++ 改动，也会在这一步一起编，例如 r2-gate 修复涉及的 `cpp/nodes/gate/handler/...`、`cpp/libs/services/gate/session/comp/session_info_comp.h`、`cpp/tests/routing_identity_test/routing_identity_test.cpp`。失败时先对照 §7 清单看首个错误出在哪个文件：不在 §7 里的，报给对应会话，不算 3a 失败。
2. 运行三个测试工程：

   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-3a
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-3a
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/persist-3a
   ```

   - 脚本 `-Build` 时只把首个编译错误截 160 字符写进汇总表，`-LogDir` 只保存 gtest 输出。所以某个工程编译失败时，要对它单独重跑一次，留下完整日志：

     ```
     & $msb cpp\tests\<x>\<x>.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal 2>&1 | Tee-Object build/verification/persist-3a/<x>.build.log
     ```

   - 通过标准：gtest 全绿，且下列新用例都在列并通过：
     - `PersistRoundTripTest.*`（4 条）；
     - `BagInstanceDataTest.CanStackRefusesAnySideCarryingUnknownFields`；
     - `BagInstanceDataTest.StacksCarryingUnknownFieldsAreNeverMergedInto`；
     - `AssetOpLedgerTest.LedgerResetForEpochDropsUnknownFieldsAndReceipts`；
     - `CurrencyTest.DebtUnknownFieldsTravelWithTheDebtOnly`；
     - `CurrencyTest.AttachDebtRefusesToMergeIntoDebtWithUnknownFields`。
   - bag_test 的日志里不应出现 `[AssetOp] Ledger corrupted` 和 `[PlayerInbox] metric=inbox_ledger_invalid`。往返夹具已经断言两本账本自洽。
3. 如果 `PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave` 失败，保留它打印的 lost / extra 清单和未覆盖类型清单原文，交回 Claude。不要改夹具或断言，这份清单就是定位依据：
   - lost：某类型的装卸是"拆成运行时结构再逐字段拼回"，没有携带未知字段；
   - extra：未知字段翻倍或挪了位置；
   - 未覆盖类型：夹具 `BuildEveryPersistedType` 里没有能撑过加载的该类型节点。

## 6. 坑与约定

1. **Claude 不跑编译、测试、压测**（AGENTS §10.1），交付必须写"未编译，待 Codex 验证"。
2. **工作区多会话并行**。
   - 以下同目录文件**不是本主题的**，不要动：
     - `go/db/etc/db.yaml`；
     - `go/db/internal/saveoutbox/*`（r2-db 审计修复）；
     - `deploy/k8s/db-task-retry-alerts.yaml`；
     - `go/data_service` 下除 `cmd/debug_import` 以外的改动（路由对账）；
     - gate 与 `routing_identity_test`（r2-gate 修复）。
   - `deploy/k8s/README.md` 里也有其他会话的改动，本主题只占「玩家持久 schema 世代」一节。
   - 其他会话会频繁把整个工作区一起提交（例如 `53361d055` 就带走了第 2 批主体）。动手前以 `git status` / `git log` 重新核对，不要轻信本文写的提交状态。
3. **`docs/PROGRESS.md`**：
   - 只追加、不改旧条目；配了 `merge=union`，索引是 LF（`.gitattributes` 是 `text eol=lf`，提交时会归一）；
   - 工作区副本的行尾会被其他会话的工具改写，目前是 CRLF 为主的混合。追加时跟随文件当前的主流行尾，不要在一条里混用；
   - 本主题的条目按标题找，不要按"末条"找：Claude 写的 6 条（设计稿 v0.2、v0.3、第 1 批、第 2 批两条、第 3a 批），另有 Codex 的第 1 批验证与冒烟、`schema_gen` 历史恢复等条目，再加本次交接的指引条。
4. `bag_test.vcxproj` 与 `.filters` 都是 LF；新测试文件两处都要登记。bag_test 没有 CMake。
5. **protobuf 是 35.x**：
   - `Reflection::MapBegin` 等 map 迭代器接口是私有的，map 字段只能走重复字段的反射视图（`FieldSize` / `GetRepeatedMessage` / `MutableRepeatedMessage`）；
   - `Descriptor::full_name()` 返回 `absl::string_view`，比较前按 `data()` / `size()` 转换（见 `persist_round_trip_test.cpp` 的 `AsView`）；
   - `Descriptor::options()` 要显式包含 `google/protobuf/descriptor.pb.h`。
6. **新增持久组件时**要同步改的地方：
   - 手写 loader 的 Unmarshal 与 Marshal（Marshal 缺组件整份拒存）；
   - `player_feature_persistence_test.cpp` 的 `CompletePersistedComps` 和 `MarshalRefusesWhenAnyPersistedComponentIsMissing`；
   - `player_equip_attribute_test.cpp` 的同名辅助；
   - `bag_test.cpp` 的 `GuidSourcePolicyTest`（手工 emplace 全套持久组件后调 `CaptureAndSend`）；
   - `cross_zone_test` / `db_task_dispatch_test` / `save_seq_test` 的 `CreatePersistedPlayerComps`；
   - `persist_round_trip_test.cpp` 的 `BuildEveryPersistedType`。

   `BuildEveryPersistedType` 漏了，覆盖率断言会点名缺的类型；其余清单漏了，对应用例会因 marshal 拒存而失败。
7. **把 PB 拆成运行时结构、存盘再逐字段拼回的新代码**：必须用 `unknown_field_carry::Capture` / `Restore` 让本层未知字段跟着对象走，或者在 proto 上标冻结 / 未携带，按设计 §3.2 处理。逻辑重置一律用 `Clear()` 后重设身份字段，不要逐字段清。
8. **会改变新字段语义的旧代码操作**（合堆、合并、等价判定、逻辑重置）要守卫。守卫拒绝的是版本错位，调用方要**延后**，不要落成终态拒绝（见 3a′ ①）。
9. **测试里手拼的"上次落盘快照"**：凡是要和 `SavePlayerToRedis` 的脏比较对齐的，必须走 `player_persist::MarshalPlayerForPersist`（带戳）。只比账本的 durable 判定不受影响。
10. **workflow 扇出**：本会话早先的多 agent workflow 因 403 和额度失败过。大任务分批跑；只读子代理禁止 `git show` 重定向到临时文件。
11. **项目尚未上线**（2026-10-18 确认）。混跑、降级、灰度类的残余问题上线前不阻塞；上线前按 §3.5 整批起步。

## 7. 第 3a 批文件清单（工作区，未提交）

新增：
- `cpp/libs/engine/core/utils/proto/unknown_field_carry.h`：取、解析、替换消息本层未知字段（存成序列化字节，因为 `UnknownFieldSet` 不可拷贝）。
- `cpp/libs/services/scene/player/comp/player_persist_carry_comp.h`：`PlayerDatabaseCarryComp` / `PlayerDatabase1CarryComp`，持久数据类，只在两张表的手写 Unmarshal 里创建。
- `cpp/libs/services/scene/player/system/player_persist_marshal.h`：统一入口 `player_persist::MarshalPlayerForPersist`，以及 `LoadedSchemaGen`、`EffectiveSchemaGen`。只有头文件，原因见设计 §15 v0.5。
- `cpp/tests/bag_test/persist_round_trip_test.cpp`：往返一致性测试，4 个用例。

修改：
- 加载与存盘：
  - `player_database_loader.cpp` / `player_database_1_loader.cpp`：创建载体、带回顶层未知字段，缺载体就拒存；
  - `player_lifecycle.cpp`：`MarshalPlayerForSave` 转调统一入口；
  - `modules/snapshot/snapshot_system.cpp`：回档快照改走统一入口。
- 对象内携带：
  - `modules/currency/comp/player_currency_comp.h`：`CurrencyDebt.unknownFields`，`SaveToProto` 改为返回 `[[nodiscard]] bool`；
  - `mission_marshal.cpp`：携带 `MissionListComp` 本层未知字段，重复 scope 只取首份；
  - `player_skill.cpp`：`SanitizeSkillList` 替换前并回本层未知字段。
- 守卫：
  - `modules/currency/system/currency_system.cpp`：`AttachDebt` 遇到带未知字段的既有欠款，返回 `kFeatureUnavailable`。注释已写明调用方须延后，执行器修正见 3a′ ①；
  - `asset_op_ledger.cpp`：`ResetAssetOpStreamForEpoch` 改为 `Clear()`；
  - `modules/bag/item_store.{h,cpp}`：新增 `IsIndependentInstance`，包内既有堆三处共用。注释已标出传入物品一侧的缺口，见 3a′ ②。
- 测试：
  - `bag_test/{bag_test,bag_instance_data_test,player_equip_attribute_test,player_feature_persistence_test}.cpp`；
  - `bag_test/bag_test.vcxproj` 与 `.filters`；
  - `cross_zone_test/{cross_zone_test,db_task_dispatch_test,save_seq_test}.cpp`；
  - `currency_test/{currency_test,asset_op_ledger_test}.cpp`。
- 文档：
  - 设计稿（§4.4、§5.2 开头的待修订提示、§9 状态、§15 v0.5）；
  - `docs/PROGRESS.md` 中「混版本持久字段保护第 3a 批落码……」一条，以及本次交接的指引条；
  - `docs/README.md` 索引；
  - 本文。

仓库根的 `game.sln.metaproj`、各处 `*.vcxproj.metaproj`，以及 `*.tmp.*` 临时文件都是构建残留，不属于本主题。

## 8. 第 2 批补齐文件清单（工作区，未提交）

- `go/db/internal/kafka/persist_schema_test.go`（新增）；
- `go/db/internal/kafka/persist_schema.go`：把复查单拍拆成 `refreshPersistBootstrap`，测试因此不依赖墙钟；
- `go/db/internal/kafka/run_db_op_test.go`：拒写结局加入 `schema_regression`；
- `go/db/internal/kafka/player_row_fence_integration_test.go`：C1b 真库用例 `TestIntegrationPlayerSQLRejectsPersistSchemaRegression`；
- `go/db/cmd/data_stress/main.go`：写 body 时盖本二进制世代；
- `go/data_service/cmd/debug_import/bundle_import.go` 及 `_test.go`：拒绝导入世代高于工具的行；
- `deploy/k8s/persist-schema-alerts.yaml`（新增，go/db 侧 5 条告警）；
- `deploy/k8s/README.md`：「玩家持久 schema 世代」小节。

## 9. Java 版（AGENTS §12）

Java 仓在 `D:\luyuan\xuanming-server-mmo`，版本 0.1.0-SNAPSHOT。
- Java 把玩家状态整份存在 `player_state.data` 这一个 BLOB 里，机制三（go/db 列写）不适用；
- 机制一（对象内携带、往返测试、语义守卫）与机制二（世代戳、加载 / 写入校验）都要做，按 Java 自己的存储形态实现，不照抄 C++；
- 状态：**全部待做**，`PARITY.md` 未登记。第 1 批落码时就该登记一行，至今没补，下次碰 Java 仓时先补上。

## 10. 续：第 3a′ 批与第 5 批落码（2026-10-19，换账号后的会话）

> 本节由接手会话追加（只追加，不改上文）。上文 §2 的状态表以本节为准更新：3a′、第 5 批已落码；3b、4 由另一个接手会话负责。

### 10.1 分工

同一份交接有两个会话在接手，2026-10-19 约定：
- **本会话**：3a′ 全部、第 5 批，以及为 §5.2 做的只读取证与裁决（结论转交 3b 会话）。
- **另一会话（「混合版本持久化交接文档」）**：§5.2 修订、第 3b 批、第 4 批，以及两批共用的 `k8s_deploy.ps1` 与契约测试、barrierrestore 字段白名单。
- 设计稿分节归属：本会话改 §4.4、§8.3 scene 行、§9 的 3a′ 与第 5 批、§12、§15；3b 会话改 §5.x、§10、§9 的 3b / 4 行。另有「数据覆盖审计交接文档」会话在 §8.1 末尾加了第 6 条（r2-ps#1，台账须在首次带新持久字段的发布前切 `active`）。
- `currency_test.cpp`：本会话只在 `AttachDebtRefusesToMergeIntoDebtWithUnknownFields` 里加了 4 行谓词断言；3b 的屏障用例由 3b 会话改。

### 10.2 第 3a′ 批（§3.1 三条）

均**未编译、未测试**。
- ① `CurrencySystem::DebtCarriesUnknownFields`（只读谓词，缺组件 / 越界币种 / 无欠款都返回 false）；`AttachDebt` 改为调它；`GmSanctionInboxExecutor::AttachCurrencyDebt` 在 `HasCurrencyState` 之后、调原语之前判，命中打 WARN 回 **`kVersionSkew`**（最初写的是 `kDefer`，对抗复核 A-1 后改掉，见 §10.6）。同步：执行器头文件的结局清单、`AttachDebt` 头注释的返回码列表、`player-inbox-service.md` §7.5 / §7.6、`player-inbox-runbook.md` §5.2 A′、设计稿 §4.4。
- ② `ItemStore::IsIndependentInstance` 移到 public（紧挨 `CanStack`），`Bag::AddItems` 规划按它分流；`item_store.h` 那段"已知缺口"注释改为正式口径。
- ③ `HasStackWithRoom` 改为用只有 `config_id` 的探针实例调 `ItemStore::CanStack`，再加 `size < maxStackSize`。
- 用例：
  - `GmSanctionInboxExecutorTest.AttachCurrencyDebtReportsVersionSkewWhenExistingDebtCarriesUnknownFields`（新增；A-1 前名为 `AttachCurrencyDebtDefersWhenExistingDebtCarriesUnknownFields`，见 §10.6）；`MissingCurrencyComponentsDeferWithoutSideEffects` 补一条谓词断言；
  - `CurrencyTest.AttachDebtRefusesToMergeIntoDebtWithUnknownFields` 补谓词断言；
  - `BagInstanceDataTest.AddItemsPlansIncomingInstancesCarryingUnknownFieldsAsSeparate`（新增）；
  - `PlayerEquipTest.RemoveGemPrecheckSkipsAStackCarryingUnknownFields`（新增）。"没有调到 `BagService::AddItems`"没有直接的注入点，用 muduo 日志出口探针 `StackTraceDumpProbe` 数容器层不足分支打的 `Stack trace (error code: ` 行，用例开头先直接打一次做校准。

### 10.3 第 5 批（§3.4 两处持久值域）

均**未编译、未测试**。
- **越界包类型**：与 §3.4 原写法的差别——没有新增独立的持久数据组件，而是在 `PlayerBagsComp` 里加成员 `unsupportedBagTypes { std::vector<ItemEntry> items; std::vector<uint32_t> capacities; }`：
  - 理由：它和固定包同生共死（只由 `bag_marshal::Unmarshal` 整体重填、`Marshal` 原样追加），不需要单独的创建点和缺失语义，也就不触发 §6 第 6 条的整套同步清单；存整份 `ItemEntry`，拷贝不会失败，`Marshal` 不需要改成返回 bool（它有 40 处调用，改签名会牵动 6 个测试文件）。
  - 写盘顺序：容量接在 `kBagTypeCount` 个之后（下标仍对应 bag_type），条目接在已知包之后（bag_type 都 ≥ `kBagTypeCount`，仍按 bag_type 升序），同一输入每次写出字节相同。
  - 每次还原有越界记录时打一行汇总 WARN（不逐条打）。
  - 用例：`CrossZoneBagMarshal.UnmarshalCarriesOutOfRangeBagTypeVerbatim`（原 `UnmarshalDropsOutOfRangeBagType` 改写；含字节级写回比对、多出容量的下标、重新还原后不残留）。
- **查不到表的技能**：
  - `SanitizeSkillList` 只去掉 0 与重复项，查不到表的技能保留；有这类技能时每次加载打一行汇总 WARN；
  - 新增 `PlayerSkillSystem::IsActiveSkill`（非 0 且技能表查得到）；`HasSkill` 只认生效的项；`ListSkills` 只下发生效的项；
  - 消费点清单（读码，[C]）：`ReleaseSkill` 先按表拒、再 `HasSkill`；战斗快照 `player_battle.cpp` 本来就跳过查不到表的项；`RegisterPlayer` 只做去重补默认技能；冷却按请求里的技能号、不读列表；`scene_admin_handler` 只打调试串。
  - 用例：`PlayerSkillSystemTest.SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates`（原 `SanitizeSkillListRemovesUnknownAndDuplicateSkills` 改写）、`SanitizeSkillListLeavesACleanListUntouched`（新增）。`ListSkills` 的过滤没有单测：处理器不在任何测试工程里。
- 设计稿 §8.3 scene 行已改为"已落码、未验证；回滚目标版本本身也要带第 5 批"，在 Codex 验证之前发布单仍须写明"没有扩展持久值域"。

### 10.4 待 Codex 验证（在 §5.2 的命令之外追加）

第 3a、3a′、第 5 批落在同一批文件里，按 §5.2 一起编译、一起跑即可，另加：
1. `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter skill_test -LogDir build/verification/persist-3a`
2. 通过标准在 §5.2 的清单上追加这些用例在列且通过：
   - `GmSanctionInboxExecutorTest.AttachCurrencyDebtReportsVersionSkewWhenExistingDebtCarriesUnknownFields`
   - `PlayerInboxSystemTest.ExecutorVersionSkewHaltsAfterConfirmingThePrefixAndNeverResumesLocally`
   - `AssetOpSystemTest.CreditRejectsAnInstanceAlreadyCarriedInAnUnsupportedBagType`
   - `BagInstanceDataTest.AddItemsPlansIncomingInstancesCarryingUnknownFieldsAsSeparate`
   - `PlayerEquipTest.RemoveGemPrecheckSkipsAStackCarryingUnknownFields`
   - `CrossZoneBagMarshal.UnmarshalCarriesOutOfRangeBagTypeVerbatim`
   - `PlayerSkillSystemTest.SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates`
   - `PlayerSkillSystemTest.SanitizeSkillListLeavesACleanListUntouched`
3. 旧用例名 `CrossZoneBagMarshal.UnmarshalDropsOutOfRangeBagType`、`PlayerSkillSystemTest.SanitizeSkillListRemovesUnknownAndDuplicateSkills`、`GmSanctionInboxExecutorTest.AttachCurrencyDebtDefersWhenExistingDebtCarriesUnknownFields` 不应再出现（已改写或改名）。

### 10.5 本节文件清单（工作区，未提交）

在 §7 之外新增改动的文件：
- `cpp/libs/modules/currency/system/currency_system.h`
- `cpp/libs/services/scene/player/system/gm_sanction_inbox_executor.{h,cpp}`
- `cpp/tests/currency_test/gm_sanction_inbox_executor_test.cpp`
- `cpp/libs/modules/bag/bag_system.cpp`
- `cpp/libs/services/scene/player/system/player_equip.cpp`
- `cpp/tests/bag_test/player_equip_test.cpp`
- `cpp/libs/modules/bag/comp/player_bags_comp.h`
- `cpp/libs/services/scene/player/system/bag_marshal.{h,cpp}`
- `cpp/libs/services/scene/player/system/player_skill.h`
- `cpp/nodes/scene/handler/rpc/player/player_skill_handler.cpp`（守护段内）
- `cpp/tests/skill_test/skill_test.cpp`
- `docs/design/player-inbox-service.md`

§7 里的文件本节又改过的：`currency_system.cpp`、`item_store.h`、`player_skill.cpp`、`currency_test.cpp`、`bag_instance_data_test.cpp`、`cross_zone_test.cpp`、`persist_round_trip_test.cpp`（一处注释）、设计稿。

Java 仓 `PARITY.md` 已补登一行「Java 待做」（§9 要求的那一行），未提交。

对抗复核后的修正（§10.6）又改了：
- `cpp/libs/services/scene/player/system/player_inbox_registry.h`（新结局 `kVersionSkew`）
- `cpp/libs/services/scene/player/comp/player_inbox_runtime_comp.h`（停用原因 `kVersionSkew` 与名字表）
- `cpp/libs/services/scene/player/system/player_inbox_system.cpp`（执行器分支）
- `cpp/tests/currency_test/player_inbox_system_test.cpp`
- `docs/ops/player-inbox-runbook.md`（§5 告警表、§5.2 A′）
- `cpp/libs/modules/bag/bag_system.h`（`Bag::ContainsGuid`）
- `cpp/libs/services/scene/player/system/asset_op_system.cpp`（入账查重改用 `PlayerBagsComp::ContainsGuid`）
- `cpp/tests/currency_test/asset_op_system_test.cpp`

### 10.6 对抗复核与修正（2026-10-19 ~ 10-20）

落码后跑了一次七视角只读对抗复核（编译、收件箱、合堆、越界包类型、技能、测试、文档），每条发现由两名复核者尝试驳倒。第一轮"测试""文档"两个视角因额度上限没跑完，已续跑，并加了一个视角专门复核下面的修正（结果见本节末）。确认的发现与处理：

| 编号 | 严重度 | 发现 | 处理 |
|---|---|---|---|
| A-1 | 中 | 版本错位用 DEFER 会按本地旧副本续做，看不到服务端之后挂的 VOID；欠款在旧节点被还清删除后错位消失，续做就把一条可能已作废的 GM 欠款挂上并报 APPLIED（违反收件箱设计 §7.5 评审 L1 的前提） | 已修：新结局 `kVersionSkew`，收件箱停用本域（`reason=version_skew`）、已定前缀照常确认、实体重建后重拉（退出保留期内重登复用实体，不重拉）。没选"每次重拉"：每次 Fetch 让 `delivery_count` +1，几分钟内就触发毒消息告警 |
| D-1 | 低 | 资产通道入账的跨包 guid 查重只扫运行时包，看不到第 5 批携带的越界包类型记录；同一实例可能在新旧两个包各落一份，升回新版本后两份都还原 | 已修：`PlayerBagsComp::ContainsGuid`（固定包、动态包、携带记录的 item_uuid 或实例 item_id） |
| B-1 | 低 | 按 config 计数和扣数量（资产通道数量扣出、强化材料、镶嵌宝石、战斗消耗）不看 `IsIndependentInstance`；文档读起来像已守住 | 只改文档：设计 §4.4 新增一行标"暂无守卫"，§0、R5、§13 评审清单收窄说法；补代码守卫要另立一批，修法写在 §4.4 那一行 |
| F-1 | 低 | 设计 §8.3 引用了不存在的"§12 待核对行" | 已修：§12 新增"其他按配置表解释的持久值：未核对"一行；§8.3 写明第 5 批只解除包类型、技能两类值域 |
| E 附注 | — | 技能表行 id 若被复用，留在老玩家数据里的不生效 id 会自动生效 | 设计 §12 加约束：删除的技能 id 不得复用 |

复核还确认了这些没有问题（摘要）：A–E 没有确定的编译缺陷；规划与提交铸号数在各种组合下相等；越界包类型往返字节稳定、重新还原清干净、容量下标不错位；技能的五条消费路径都只认生效的项。以上都来自读码，没有编译。

### 10.7 第二轮复核、订正与运行旁证（2026-10-20 ~ 10-22）

**运行旁证**：这不是本主题专门的验证。见 PROGRESS「2026-10-20 客户端消息发送模块命名整理收尾」。
- 本节改动已随别的会话的整仓提交入库（`c35e1133d`、`10fa5d939` 等）。
- Codex 在含这些改动的工作区里串行构建了 9 个目标，全部通过：Scene 共享库，Scene / Gate / Battle 节点，`bag_test`，以及另外 4 个目标。
- 跑过的相关用例 0 失败：`PersistRoundTripTest.*` 4 条、`BagInstanceDataTest.StacksCarryingUnknownFieldsAreNeverMergedInto`，以及其他背包、装备用例。
- 为了编译，Codex 改了两处测试：
  - `bag_instance_data_test.cpp`：检查函数参数改为 `Bag&`；
  - `persist_round_trip_test.cpp`：include 之后清除 Windows SDK 的 `GetMessage` 宏。
- **仍未运行**：3a′ 与第 5 批的新用例，`currency_test`、`cross_zone_test`、`skill_test` 三个工程也都没跑。§5.2 与 §10.4 的命令照旧要跑。

**第二轮复核**：先续跑第一轮没跑完的"测试""文档"两个视角，再加一个视角专门复核 A-1、D-1 的修正。
- 确认 11 条，全部是文档与注释不一致，没有代码缺陷。
- 修正视角确认 A-1、D-1 的修法本身没有问题。
- 11 条都已订正：
  - **重拉时机**：停用对当前实体一直有效，实体重建（退出完成后再登录，或换节点）后才重拉；退出保留期内重登会复用实体，不重拉。涉及代码注释 9 处，以及设计 §4.4 / §15、收件箱设计 §7.5 / §7.6、运行手册 §5.2、本文 §10.6。
  - **运行手册 A′**：删掉"回滚到同一版本"这个解法，补上整体回滚后的三种解除方式，并提醒同域排在后面的处置会一起停住。
  - **设计稿**：
    - §0 / R5 / §4.4 改正"纪元重置是拒绝执行"的说法，实际是 `Clear()` 后照常重置；
    - §4.4 开头"三类"改为"四类"；
    - §4.3 与 §12 两行的现状改标为"第 5 批前"；
    - §9 删掉残留的"应回 kDefer"；
    - 头部状态改为 v0.7。
  - **容量注释**：`bag_marshal.cpp` 里两段"capacities 恰好 kBagTypeCount 个"的注释已改；`proto/common/database/bag_quest_mail_data.proto` 中 `capacities` 字段的注释也已改。proto 只改了注释，下次 `proto-gen-run` 时 Go 绑定里的注释会跟着变，不影响行为。
  - **用例名**：§10.2 / §10.4 换成新名，旧名补进"不应再出现"一条。

**新确认的持久值域问题**（设计 §12）：
- **等级上限** `playerlevel::kMaxLevel`：
  - 加载时压回超限等级，并连带整池返还加点、取消宝宝出战，随存盘落库。已确认会删数据，单列一行。
  - 修好之前，提高等级上限的版本不能回滚 scene。
  - 修法要另立一批，并由负责人拍板。
- **新币种**：
  - 旧版收件箱执行器把不认识的币种当作载荷非法，记终态 REJECTED；资产通道在已有改动之后记 APPLIED_PARTIAL。
  - 复核者因额度没跑完，由本会话读码核实（`gm_sanction_inbox_executor.cpp` 的 `IsKnownCurrencyType`，Go 侧不校验取值）。
  - 已记入 §12 载荷行：在 Go 侧按完整取值表校验之前，新增币种必须整批换版。
- **属性加点维度与装备槽位**：
  - 属性加点维度：加载时删掉查不到表的维度；
  - 装备槽位：快照槽位不被接受时挪到同部位第一个空槽；
  - 两项都作为"未核对"行的已知线索登记。

**(a″) 写方令牌的三路对抗攻击与裁决**：结论已转交 3b 会话，落不落地记在 §11。
- 三路都没推翻 (a″)。
- 成立的中等问题：
  - warn 下返回 2 的证据会随回包丢失而消失；
  - 令牌不变量缺行为级测试；
  - C3 的 Lua 语义只有真 Redis 用例覆盖，没配 Redis 时会被 SKIP。
- B5 幽灵写方一条，三路意见有分歧，裁决结果见下面一条。
- **裁决（2026-10-22）**：(a″) 可以采用。
  - 驳回 B5 幽灵写方一条：B5 要求标记原样还在，目标节点的 A2′ 先删标记再建实体，两者互斥，按代码不可达。也不采纳"proven 基线"修法，它会把首存状态和回包依赖带回来。
  - 成立 7 条：返回 2 的证据丢失、令牌不变量的测试、真 Redis 跳过、返回 3 的排队值规则文字、merge_zone 文字、前任实体残留存盘、疏散退出不踢线。
  - 全文与 §5.2 / §10 应补的规则、用例已转交 3b 会话。其中证据字段与按令牌去重，3b 会话在工作区已按同一做法在改，未编译。

## 11. 续：§5.2 修订、第 3b 批与第 4 批落码（2026-10-19 ~ 10-20，另一个接手会话）

> 本节由「混合版本持久化交接文档」会话追加（只追加，不改上文）。§2 的状态表以 §10、本节为准：3b、4 已落码，**均未编译、未测试**。代码已随用户的工作区快照 `c35e1133d` / `10fa5d939` 入库，之后的少量修正可能仍在工作区，以 `git status` 为准。

### 11.1 §5.2 改为写方令牌（设计 v0.6）

§3.2 第 1 条给的 (a) / (b) 经接手会话取证都不成立（取证要点见设计 §15 v0.6 的方案表）：
- (a) 的前提"同一 owner_epoch 只对应一个加载实体"两头都不成立：同一实体会换代次（退出保留期间重登铸出 E+1 又派回本节点复用实体；B5 采纳 E+2），换代后第一次存盘确定误报；同一代次也会先后加载出两个实体（同落点重连时上一个实体已退出销毁、废黜后回流、同号新进程接替），漏检。
- (b) 分不出"首存已提交、回包丢失、随后被排队值顶替"：被丢弃的元素不执行任何回调。
- 中间方案 (a′)（实体基线世代）还有一个残余竞态：首存已提交但回包丢失、同时换到 E+1 并排队了新值，基线没被证实抬高，误报后 enforce 下永久存不进盘。

采纳 (a″) **写方令牌**：每个加载实体一个随机 UUID 令牌（`PlayerPersistWriterComp`，存盘写入口懒建，实体销毁即作废），随屏障写进 hash 的 `schema_writer`；Lua 只在 `schema_writer ≠ ARGV[11]` 且不是同版本重放时比较，`ARGV[9]` 永远是真实加载世代，C++ 不维护任何"首存"状态。逐情形结论见设计 §5.2 表。交接 §3.2 第 2–4 条（C3 放在末尾、模式归一化、无日志告警通路）都按原写法落实。

### 11.2 第 3b 批（C++ C3、配置与部署）

- `redis_client.h`：严格 Lua 改为 11 参（`ARGV[8]` 输出世代、`ARGV[9]` 加载世代、`ARGV[10]` 模式、`ARGV[11]` 令牌），新增 `num_gt`；参数校验先于任何写；C3 排在 owner（返回 0）、版本倒退、同版本冲突之后；倒退时 warn 照写返回 2、enforce 一个键都不写返回 3；写入时同一个 `HSET` 写 `schema_gen = max(存储值, ARGV[8])`（损坏取 `ARGV[8]`）与 `schema_writer`。`RedisSaveSchemaGen` + `IsValidRedisSaveSchemaWriter`，`SaveWithCommitBarrier` 入队前同口径校验；`Element` 带 `schema_*` 四个字段，所有重发路径发同一组参数；`OnSaved`：2 是提交证据并先调计数回调，3 不重试、不准入，排队值 guard 与令牌都相同才丢（否则照发、不回调，与 0 分支共用 `IssuePendingWithNewerGuard`），2 / 3 只对屏障元素有含义。
- scene：调用点传输出世代（统一入口盖的戳）、加载世代（`LoadedSchemaGen`）、模式、令牌；`HandlePlayerSaveSchemaWarned`（计数 + `metric=persist_schema_regression where=redis mode=warn` 日志）、`HandlePlayerSaveSchemaRejected`（退出中 `DestroyDeposedPlayer`、交接标记未发 `AbortTravelHandoff`、其余保留实体）；`persist_schema_stats` 与 `redis.cpp` 的 30 秒 `[PersistSchema]` 汇总。
- 配置：`BaseDeployConfig.persist_schema_regression_mode = 23`；`persist_schema_regression_mode.h` 归一化（缺键 / 空串 = warn，其余非法 LOG_FATAL）；`bin/etc/base_deploy_config.yaml` 默认 `warn`；`k8s_deploy.ps1` node 与 login 两份 ConfigMap 镜像并在生成期只认两个字面量；契约测试两条；`start_game.ps1` 启动前校验两份 yaml。
- `go/db/internal/barrierrestore/scripts.go` 字段白名单接受 `schema_writer`（另一个会话同时在同一脚本改了 r2-mz#1 的空落点分支，两处互不重叠）+ 单测。
- 测试：`currency_test` 新增 `RedisCommitBarrierSchemaGenAgainstRealRedis`（真 Redis，C3 全部判定）、`RedisCommitBarrierSchemaWarnedIsCommitEvidence`、`RedisSchemaRepliesOnNonBarrierSavesAreNotEvidence`、`RedisCommitBarrierSchemaRejectedDiscardsSameOwnerHooks`、`RedisCommitBarrierSchemaRejectedIssuesPendingWithNewerGuard`、`RedisCommitBarrierSchemaRejectedIssuesPendingFromAnotherWriter`、`RedisCommitBarrierCarriesAndValidatesSchemaArguments`、`PersistSchemaRegressionModeNormalization`；改写 `RedisCommitBarrierErrorsNeverAdmitSql`（坏回包改用 4）、`RedisSaveScriptsTouchOnlyDeclaredKeys`（字面量白名单）、`RedisCommitBarrierSemanticsAgainstRealRedis`（11 参，别名用例断言 `keys must differ`）、`PlayerSqlTasksRequireParentCommitSourceContract`（调用文本）以及全部 `SaveWithCommitBarrier` 调用点。`cross_zone_test` 新增 `ExitPersistEcs.SchemaRejectOnExitingPlayerDiscardsWithoutReleaseMark`、`SchemaRejectDuringHandoffBeforeMarkAbortsHandoff`、`SchemaRejectOnOnlinePlayerKeepsEntity`、`SchemaWarnOnlyCountsAndGonePlayerIsNoop`。`barrierrestore` 新增 `TestSchemaGenRestoreAcceptsAndKeepsSchemaWriter`。
- 运维：`deploy/k8s/README.md`「玩家持久 schema 世代」补 C3 / C4 / 切 enforce 三条；C3 只有 LogQL 值班固定查询（Loki ruler 未接线，**告警未配**）。

### 11.3 第 4 批（login C4）

由一个落码代理按 §3.3 实现，两路评审（契约、测试）发现 3 条低危，修复代理撞额度没跑，由本会话核实后修正：
- `beginParentLoad` 脚本追加 `schema_gen` 原文与存在标记（第 4、5 个元素，长度检查改为 5）：字段存在但为空串按**损坏**判，与 scene C3、`save_barrier_restore` 同一口径（评审第 1 条）。
- C4 在身份、水位、种子校验之后、只在快路径判；warn 计数放行，enforce 返回错误走现有失败路径（不新增 tip）。新增用例钉住这个顺序：水位不符 / 身份不符时返回原错误、计数不变（评审第 3 条）。
- `login_persist_schema_regression_total{reason=regression|malformed, mode=warn|enforce}`；`PrimePersistSchemaRegressionMetrics` 在 `login.go` 紧挨 `PrimeEnterGameMetrics` 预置 4 条序列，`increase` 能抓到第一次（评审第 2 条）。告警 `LoginPersistSchemaRegression` 加在 `deploy/k8s/persist-schema-alerts.yaml` 的 `login.persist-schema` 组。
- 配置：`config.go` 字段 + `persist_schema_regression.go` 归一化（严格匹配，非法值 `login.go` 启动即 panic 拒绝）；`go/login/etc/login.yaml` 与 `deploy/login-stack.linux/login.yaml` 都写 `warn`。
- 测试：见设计 §10 与本节 §11.4 的清单；种子不盖戳（`TestNewPlayerSeedStaysUnstamped`）、backfill 往返保留四层未知字段与两表戳（`TestBackfillPlayerIdentity_PreservesUnknownFieldsAndSchemaStamps`）。

### 11.4 待 Codex 验证（照抄执行；在 §5、§10.4 之外追加）

通用约定同 §5。**顺序有依赖**：proto 必须先生成，否则 C++ 找不到 `persist_schema_regression_mode()`。

1. 仓库根：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`（只多了 `BaseDeployConfig` 字段 23；生成器源码没变，不需要 `proto-gen-build`）。通过标准：退出 0，生成的 `config.pb.*` 出现 `persist_schema_regression_mode`。
2. C++：按 §5.2 第 1 步编 `game.sln`（`/m:1`，输出 Tee 到 `build/verification/persist-3b/game-sln.build.log`），再：

   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-3b
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/persist-3b
   ```

   通过标准：gtest 全绿，§11.2 列出的新用例在列且通过（真 Redis 用例在没有 `MMORPG_TEST_REDIS_ADDR` 时是 SKIPPED，见第 3 步）。
3. 真 Redis（必跑，C3 的判定只在这里验证）：自起一次性 Redis，设 `MMORPG_TEST_REDIS_ADDR=127.0.0.1:<port>` 后单独跑 currency_test 的 `--gtest_filter=*AgainstRealRedis*`。**7.2 与 8.10.x 各跑一轮**（K8s 钉 7.2，本地 compose 与 Java 版是 8.10.x），日志里记 `redis_version`。通过标准：`RedisCommitBarrierSemanticsAgainstRealRedis`、`RedisCommitBarrierSchemaGenAgainstRealRedis` 两轮都 PASS 且不是 SKIPPED。
4. Go：
   - 工作目录 `go/login`：`go build ./...`；`go vet ./internal/config/... ./internal/logic/pkg/dataloader/... ./internal/logic/clientplayerlogin/...`；`go test -count=1 ./...`。
   - 工作目录 `go/db`：`go test -count=1 ./internal/barrierrestore/`。
   - `dataloader` 的用例沿用它们现有的 Redis 夹具。通过标准：全部退出 0，且这些用例在列并 PASS：`TestParentFastPathPersistSchemaRegression`、`TestParentFastPathMalformedBarrierSchemaGenIsRegressionEvidence`（含空串）、`TestParentSchemaRegressionCheckRunsAfterIdentityAndWatermark`、`TestPrimePersistSchemaRegressionMetricsExposesAllSeries`、`TestColdLoadSkipsPersistSchemaRegression`、`TestColdRecoveryUnderEnforceDoesNotJudgeSchemaGen`、`TestEnsurePlayerAllDataPassesConfiguredRegressionMode`、`TestNewPlayerSeedStaysUnstamped`、`TestBackfillPlayerIdentity_PreservesUnknownFieldsAndSchemaStamps`、config 包四条（`TestParsePersistSchemaRegressionMode` 等）、`TestSchemaGenRestoreAcceptsAndKeepsSchemaWriter`。
5. PowerShell：`pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`，fail=0；新增 / 改动的用例名含「PersistSchemaRegressionMode」与「login ConfigMap 的 Timeout/Node/Locker/Kafka 关键值」。工作区里 `k8s_deploy.ps1` 与该测试有别的会话的大量改动，失败先归因。可选：`promtool check rules` 校验 `deploy/k8s/persist-schema-alerts.yaml` 的 spec。
6. 失败时：C++ 首个错误若不在本节或 §7 / §10.5 的文件里，报给对应会话；真 Redis 用例失败保留整段 gtest 输出与 Redis 版本，不改断言。

### 11.5 仍未做 / 未决

- **切 enforce 前须拍板**：`save_barrier_restore` 的 `schema_gen` 取两行最大值且不降，与 C3 / C4 取最小值冲突，恢复后的玩家会被判倒退（设计 §13 第 8 条，有推荐改法）。本批没改那个工具。
- 首存在途时排队的后继、首存被顶替、同落点重连在同一代次下重载等异步时序没有端到端单测宿主；判定本身由真 Redis 用例逐条覆盖，端到端靠设计 §8.1 第 5 步的 staging 真实升世代演练。
- C3 的告警只有 LogQL 值班查询，Loki ruler 接线归上线批 BK8s。
- 可选项（§3.3 末）没做：go/db 能力心跳带世代号、`k8s_deploy.ps1` 发布 scene 前的只读预检。
- Java 版：`PARITY.md` 那一行由 §10 的会话登记；写方令牌的口径追加在那一行的备注里（Java 不用 Redis 父屏障，令牌规则只作为"判断上一次写方是不是本次加载实体"的参考）。

### 11.6 本节文件清单

- 新增：`cpp/libs/engine/config/persist_schema_regression_mode.h`、`cpp/libs/services/scene/player/comp/player_persist_writer_comp.h`、`go/login/internal/config/persist_schema_regression.go` 及 `_test.go`。
- 修改：`proto/common/base/config.proto`、`cpp/libs/engine/config/config.cpp`、`cpp/libs/engine/infra/storage/redis_client/redis_client.h`、`cpp/libs/services/scene/player/system/player_lifecycle.{h,cpp}`、`cpp/libs/services/scene/core/system/redis.cpp`、`cpp/tests/currency_test/currency_test.cpp`、`cpp/tests/cross_zone_test/cross_zone_test.cpp`、`bin/etc/base_deploy_config.yaml`、`tools/scripts/{k8s_deploy.ps1,start_game.ps1,tests/k8s_deploy_contract.tests.ps1}`、`go/db/internal/barrierrestore/{scripts.go,schema_gen_test.go}`、`go/login/{login.go,etc/login.yaml,internal/config/config.go}`、`go/login/internal/logic/pkg/dataloader/{parent_commit_barrier.go,parent_commit_barrier_test.go,ensure_player_all_data_async.go,metrics.go}`、`go/login/internal/logic/clientplayerlogin/player_identity_backfill_test.go`、`deploy/login-stack.linux/login.yaml`、`deploy/k8s/{README.md,persist-schema-alerts.yaml}`、设计稿（§0、§5.1–§5.4、§7、§9、§10、§13 第 8 条、§15 v0.6）、本文。

### 11.7 续：3b 评审、写方令牌裁决之后的修正（2026-10-22，同一会话）

3b 落码后做了两路只读评审（Lua 与客户端状态机 / 生命周期、测试与部署），§10 的会话另对写方令牌做了三路攻击与裁决（结论：(a″) 可以采用）。据此改了下面这些，**均未编译、未测试**：

- **warn 证据不随回包丢失消失**（评审与裁决都判中）：判倒退那次的回包一丢，重放同一元素、改发同令牌后继都跳过比较、返回 1，计数永远为 0，切 enforce 门禁假阴性。改为 warn 判倒退时在 hash 写 `schema_regressed = 写方令牌`，之后同令牌写入照写并继续返回 2；别的写方写成功**不清**（只在等于本令牌时起作用；清掉的话"判倒退后回包丢失、随即被废黜"那次就永不计数），只有 `save_barrier_restore` 清。`SaveSchemaWarnedCallback` 增加写方令牌参数，`HandlePlayerSaveSchemaWarned` 按令牌去重（`PlayerPersistWriterComp::regressionWarnReported`），令牌对不上（别的实体留下的在途写）照常计数。切 enforce 门禁改为三项都为 0：C++ `regression_warned`、`login_persist_schema_regression_total`、巡检 `schema_regressed` 存量（`deploy/k8s/README.md` 给了只读 SCAN 命令）。
- **屏障 11 参在线格式没有端到端测试**（评审判低）：新增真连接用例 `RedisCommitBarrierWireFormatThroughClientAgainstRealRedis`，经 `MessageAsyncClient::SaveWithCommitBarrier` 发出，第一次 EVAL、第二次 EVALSHA，参数取可区分的值。测试对端加了只读访问器 `BarrierScriptSha`。
- **返回 3 时排队值处置**：guard 相同但写方令牌不同（同一代次下另一个加载实体的存盘）也照发（`IssuePendingWithNewerGuard(..., sameWriterOnly)`），新增 `RedisCommitBarrierSchemaRejectedIssuesPendingFromAnotherWriter`。
- **疏散 / 排空退出遇 enforce 拒写**：会话仍绑定时先 `SendTipAndKickToClient(kEnterSceneFailed)` 再 `DestroyDeposedPlayer`，不新增 tip。
- **恢复工具**：字段白名单再接受 `schema_regressed`；写入时 `HDEL schema_writer schema_regressed`（可选加固：以 SQL 为准后任何写方都重新比较）。用例改名 `TestSchemaGenRestoreAcceptsAndClearsWriterFields`。
- **令牌取用可测**：`PersistWriterToken` 挪成 `player_persist::WriterTokenForSave`（`player_persist_marshal.h`，写入口懒建），新增 `ExitPersistEcs.PersistWriterTokenIsPerLoadedEntity`。
- **按裁决撤掉的**：我一度加了"本实体写入被证实提交后，`ARGV[9]` 改用输出世代"（防 B5 幽灵目标节点插写）。裁决判该路径按代码不可达（B5 只认标记原样还在，目标 A2′ 先删标记再建实体），而这条加固会把首存状态与回包依赖带回 C++，已撤掉；前提与 dev 旁路 `AllowUnsafeCrossNodeHandoff` 例外写进设计 §5.2"前提"一行，新增真 Redis 用例 `RedisCommitBarrierForeignWriteBetweenSameEntityWritesIsComparedAgainstRealRedis` 钉住判据本身。
- **文字**：设计 §3.3 / §5.2 / §5.4 的 merge_zone 说法改为"前向 copy 合服保留 hash、照常比较"；§10、§15 的排队值口径（0 只看 guard，3 看 guard 与令牌）；§15 v0.6 补三条决策；§13 新增第 9 条。
- **新登记、切 enforce 前须落地**：设计 §13 第 9 条"前任实体的存盘元素活得比实体长"（X1 被不存盘销毁时还有未落地存盘，同落点重连出的 X2 读到旧父快照，A 落地后 X2 永久拒存），两种修法见那一条，本批没做。

**§11.4 的验证口径追加**：
- 第 3 步真 Redis：用一次性本机 Redis，`--gtest_filter=*AgainstRealRedis*`，**`[ SKIPPED ]` 必须为 0**，Redis 7.2 与 8.10.x 各跑一轮并在日志里记版本；有任何 SKIP 一律报"未验证"。本节新增的真 Redis 用例：`RedisCommitBarrierSchemaWarnEvidenceSurvivesLostReplyAgainstRealRedis`、`RedisCommitBarrierForeignWriteBetweenSameEntityWritesIsComparedAgainstRealRedis`、`RedisCommitBarrierWireFormatThroughClientAgainstRealRedis`（后者是真连接，走 muduo EventLoop）。
- 第 2 步 C++ 新增 / 改动的用例：`RedisCommitBarrierSchemaRejectedIssuesPendingFromAnotherWriter`、`RedisCommitBarrierSchemaWarnedIsCommitEvidence`（回调带令牌）、`ExitPersistEcs.PersistWriterTokenIsPerLoadedEntity`、`ExitPersistEcs.SchemaWarnOnlyCountsAndGonePlayerIsNoop`（去重）、`PlayerSqlTasksRequireParentCommitSourceContract`（调用文本改为 `WriterTokenForSave`）。
- 第 4 步 go/db：`TestSchemaGenRestoreAcceptsAndClearsWriterFields`（原 `...KeepsSchemaWriter` 改名、期望改为清掉两个字段）。

本节新增改动的文件：`cpp/libs/engine/infra/storage/redis_client/redis_client.h`、`cpp/libs/services/scene/player/comp/player_persist_writer_comp.h`、`cpp/libs/services/scene/player/system/{player_persist_marshal.h,player_lifecycle.h,player_lifecycle.cpp}`、`cpp/tests/currency_test/currency_test.cpp`、`cpp/tests/cross_zone_test/cross_zone_test.cpp`、`go/db/internal/barrierrestore/{scripts.go,schema_gen_test.go}`、`deploy/k8s/README.md`、设计稿、本文。注意 `player_lifecycle.cpp` 同时有别的会话在调日志级别（`HandlePlayerAsyncSaved` / 存盘收尾行），与本节无关。

### 11.8 续：§13 第 9 条落码与第二路评审修正（2026-10-22，同一会话）

均**未编译、未测试**。
- **§13 第 9 条「前任实体的存盘元素活得比实体长」按修法 (2) 落码**（与 §10 会话分工：本条归本会话，第 6 批归它）：`MessageAsyncClient::AsyncLoad` 在同 key 还有本客户端未落定的存盘时把加载挂进 `loads_awaiting_saves_`，`RetryDuePending` 每拍放行已落定的；`HasPendingLoad` 计入挂起项；`LogQueueSnapshot` 行尾多 `loads_awaiting_saves=N`。做在客户端内部、没碰节点层入口（生产调用方 `role_admission_system.cpp` 属别的会话）。用例 `CurrencyTest.RedisLoadWaitsForUnsettledSaveOfTheSameKey`。
- **第二路评审（生命周期 / 测试 / 部署）只查到一条低危，已修**：YAML 空值（`PersistSchemaRegressionMode:` 或 `~`）是 Null 节点，yaml-cpp 的 `as<std::string>()` 返回字面量 `"null"`，C++ 会拒绝启动，与"空串 = warn"及 `start_game.ps1` 预检不一致。`readBaseDeployConfig` 改为把 Null 节点当缺键；`start_game.ps1` 预检把 `~` / `null` 也当空值；新增 `CurrencyTest.ReadBaseDeployConfigNormalizesPersistSchemaRegressionMode`（读临时 YAML：缺键、空值、`~`、`''`、`warn`、`enforce`）。K8s 生成期仍要求显式写 `warn` / `enforce`（`bin/etc` 注释已写明）。
- 其余评审结论：生命周期分派、令牌生命期、配置链、测试有效性均未发现问题。

**§11.4 / §11.7 的验证口径追加**：currency_test 新增 `RedisLoadWaitsForUnsettledSaveOfTheSameKey`、`ReadBaseDeployConfigNormalizesPersistSchemaRegressionMode`（后者会在系统临时目录写并删除 6 个小文件）。`start_game.ps1` 的预检没有自动化测试，Codex 可选手工核对：把 `bin/etc/base_deploy_config.yaml` 的键临时改成 `~`、`Warn` 各跑一次 `Assert-PersistSchemaRegressionModes`（前者通过、后者拒绝），改回原值。

本节改动的文件：`cpp/libs/engine/infra/storage/redis_client/redis_client.h`、`cpp/libs/engine/config/config.cpp`、`cpp/tests/currency_test/currency_test.cpp`、`tools/scripts/start_game.ps1`、`bin/etc/base_deploy_config.yaml`（注释）、设计稿 §13 第 9 条、本文。

## 12. 续：§12「其他持久值域」逐项核对（2026-10-22，只读会话）

> 本节由只读核对会话追加（只追加，不改上文）。只读代码、只改文档，没有改 C++ / Go，没有编译，也没有跑测试。

- **结果**：设计稿 §12 那一行已改为"已核对"，逐项清单在新增的 **§12.1**；§8.3 scene 行已同步。
  - 会删 7 项（D1–D7）：属性 / 宝宝加点维度（`SanitizeSchemes`、`SanitizeAllocations`）、属性 / 宝宝加点超额整池返还（`ConvergeOverAllocation`、`RecalculateOne`）、职业技能重数按 `max_rank` 压回（`SanitizeSkillList`，新发现）、技能改职业 / 通用归属后整条删或清 0（新发现）、战斗结算账本 `kMaxAppliedRecords = 64` 的运行期淘汰（新发现）。
  - 会改写 3 项（W1–W3）：装备栏槽位挪位、入场坐标按导航网格吸附或拉回出生点（`EnsureValidEnterLocation`，新发现）、当前 HP / MP 夹到本版本上限。
  - 不适用 15 项（N1–N15）：物品、任务 / 条件、宝宝种类、越界币种、动态包 profile 等都原样保留；资产通道 / 收件箱账本遇到新枚举值只停用该玩家的通道，不丢数据。
- **待负责人拍板**：见设计 §12.1 末尾 6 条。D5（删掉加载期的重数压回）改动小，建议先做；其余建议作为发布约束，或接受。
- **附注**：`IsActiveSkill` 目前只有 `skill_test` 调用，消费侧各自用 `FindSkillRow` 判，口径一致；`player_skill.cpp` 的注释措辞可以顺手改准。
- **未做**：没有配回滚场景用例；D1–D7 都没改代码。Java 版：待做，`PARITY.md` 已有的那一行覆盖本主题，没有新增。

### 11.9 续：恢复工具 schema_gen 改为两行最小值（设计 §13 第 8 条，2026-10-22）

未编译、未测试。`go/db/internal/barrierrestore/restore.go`：`schemaFloor`（两行最大值）改为 `restoredSchemaGen`（两行最小值，0 按 1）；`scripts.go`：不再保留 hash 里更高的旧 `schema_gen`，旧值格式损坏也覆盖。用例：`TestSchemaGenRestoreUsesSQLMinimumAndOverwritesIndex`（原 `...SQLMaximumAndExistingFloor`）、新增 `TestSchemaGenRestoreOverwritesMalformedIndex`、`TestSchemaGenRejectsInvalidRedisFieldsWithoutWrites` 去掉 schema_gen 损坏用例、`TestSchemaGenRecoveryAcrossRedisSplitAndDBFirst` 期望 7→4、真库集成 `TestIntegrationBarrierRestoreSchemaGeneration` 期望同步。运维手册 `docs/ops/mysql-backup-pitr-runbook.md` 追加 2026-10-22 口径变更小节。Codex：工作目录 `go/db`，`go test -count=1 ./internal/barrierrestore/`；有真库环境时再按该文件头的环境变量跑集成用例。
