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

## 13. 续：§12 持久值域落码（D1–D6 与等级上限，2026-10-23～30，核对 §12 的会话）

> 本节由核对 §12 的会话追加（只追加，不改上文）。设计见设计稿 §12.2 与 §15 v0.9。代码已随 10-24 起别的会话的整仓快照提交入库（主要在 `5de132e2`，另有 `a3c9b9b7`、`83859448`）。
>
> **运行旁证**：Codex 2026-10-24 在含这批改动的共享工作区实际构建了 `game.sln`（0 错误），并全量运行 bag_test（708 条）、skill_test（76 条）、turn_battle_engine_test（287 条），全部 0 失败。§13.2 列出的用例里，除标了「10-30 新增」的几条和最后一条任务用例，其余都在当天的结果里在列且通过。
> - 出处：PROGRESS「2026-10-24 07:38 UTC」与「07:43 UTC 后继实际结果」；原始 XML 在 `C:/Users/luyua/.codex/tmp/cpp-current1884-post-full-gtests-evidence-r1/run-20261024T071358902Z-25be95de/`，本会话 10-30 按用例名逐条核过。
> - 第 5 批与 3a′ 的新用例也在当天的 currency_test、cross_zone_test 里通过。
> - 证据边界：这不是本主题专门的验证，构建前没有封存源快照；10-29 之后别的会话又改过 `player_battle.cpp`、`class_skill_test.cpp`，并新增 `PlayerMissionSystemTest.ActivityAcceptUsesCappedLevelWithoutChangingStoredLevel`，所以最新状态要按 §13.2 复跑。

### 13.1 做了什么

- **来由**：用户 2026-10-23 确认项目未上线、存量玩家数据可以清空，要求按最标准的方式做完，做到数据强一致、代码可读、可水平扩展。据此把设计 §12.1 里 D1–D6 与等级上限一行改成"加载与重算不改写持久值，原样携带，按本版本生效"。D7 保留为发布约束，W1–W3 接受，都不改代码。
- **过程**：
  - 定方案：四路只读精读（技能 / 角色加点 / 宝宝加点 / 等级），每路配一名对抗复核者。复核推翻了三处初稿：
    - 加点"方案带未知键就整套冻结"：值为 0 的未知键很常见，会大面积误冻；
    - "洗点清掉未知键且免费"：等于旧版本在删新版本的数据；
    - "生效判定把未知键也算进去"：宝宝二期加第二个池后，旧版本会让整只宝宝失效。
  - 落码：三路实现，文件互不重叠。加点那一路在汇报前被额度打断，但代码、测试、proto 注释与两份设计文档其实都已写完；当时的四个评审也没跑成。
  - 评审：10-30 核对现状后补做两路只读评审（逻辑 / 回滚，测试 / 静态编译），结论与修正见 §13.3。
- **改法**（详见设计 §12.2）：
  - **等级**：`playerlevel::CappedLevel` 取代 `ClampToMaxLevel`。加载不再压回等级，超限只打 WARN（`metric=level_above_cap`）。玩法读点全部改走 `CappedLevel`：属性、宝宝、技能、装备穿戴、战斗快照、接任务等级事件、技能伤害、回血 buff。`GmSetLevel` 覆写超上限的存储等级时打 WARN。10-30 评审后补：宝宝的生效等级 = min(存储等级, 本版本种类上限)（`PetEffectiveLevel`），与主人同一条规则。
  - **加点**（角色与宝宝共用 `attribute_allocation_rules.h` 的纯规则）：
    - 删除 `SanitizeSchemes`、`ConvergeOverAllocation`、`SanitizeAllocations`，以及宝宝超额时的 `clear()`；
    - 未知键计入已用、永不生效，洗点不清；
    - 超额的池整池不生效、免费洗点（`metric=allocation_reset_free_ineffective`）；
    - 加载时对不生效的池和未知键各打一条 WARN（`metric=allocation_not_effective`）；
    - `Allocate` 只写变化的键；
    - 宝宝出战只在 `kLevelChanged` 时取消。
  - **技能**：
    - `SanitizeSkillList` 只去 id 0 与重复 id，不再按 `max_rank` 压回、不删 rank 0 的职业技能、不清通用技能的 rank；
    - `GrantInitialSkills` 把 rank 0 的种子原地补到 1 重；
    - `BuildSkillBook` 的已拥有列表改为显式投影；
    - 第 5 批的"查不到表的技能保留但不生效"没有回退。
- **文档**：
  - 设计稿：§0、§8.3、§9 实施状态、§12 两行、§12.1、新增 §12.2、§15 v0.9；
  - `class-skills.md`、`player-attribute-allocation.md`、`player-pet.md`；
  - 三个 proto 的注释：`player_skill_comp.proto`、`player_attribute.proto`、`player_pet.proto`。
- **Java 版（AGENTS §12）**：0.1.0-SNAPSHOT，待做。在 Java 仓 `PARITY.md` 原有的「混版本持久字段保护」一行追加了说明：Java `SceneWorld` 加载时用 `PlayerLevels.clampStored` 压回等级，同属待做（未提交）。

### 13.2 待 Codex 复跑（照抄执行）

10-24 的实跑已经覆盖本批代码；这一轮复跑针对最新的整体状态，包括别的会话 10-29 之后的改动、新增的任务等级用例，以及本会话 10-30 的补充（宝宝生效等级与 5 条新用例）。

工作目录为仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7。C++ MSBuild 必须串行 `/m:1`。日志统一放 `build/verification/persist-value-domain/`。

1. **proto**（只改了注释）：
   ```
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
   ```
   通过标准：退出 0；生成产物里与这三个 proto 相关的差异只有注释。
2. **C++ 构建**：按 §5.2 第 1 步编 `game.sln`（`/m:1`，输出 Tee 到 `build/verification/persist-value-domain/game-sln.build.log`）。通过标准：Scene 共享库与 Scene / Gate / Battle 节点退出 0。
3. **C++ 测试**（依次执行）：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle_engine_test -LogDir build/verification/persist-value-domain
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter skill_test -LogDir build/verification/persist-value-domain
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-value-domain
   ```
   通过标准：三个工程 gtest 全绿（10-24 基线：708 / 76 / 287 条全过，之后别的会话可能新增了用例）；下列用例在列且通过。
   - turn_battle_engine_test：
     - `ClassSkillRulesTest.PlanUpgradeFromAboveMaxRankPlansNothing`
     - `AttributeAllocationRulesTest.TotalPointsNeverDecreaseAsLevelRises`
     - `StoredAllocationRulesTest.*`（6 条）
     - `PlayerLevelRulesTest.CappedLevelReadsStoredLevelsWithinOneThroughMax`
     - `PlayerLevelRulesTest.RankLimitNeverDecreasesAsLevelRises`
   - skill_test：
     - `PlayerSkillSystemTest.SanitizeSkillListCarriesOutOfRangeRanksVerbatim`
     - `PlayerSkillBookTest.InitializeOnLoadDropsOnlyZeroAndDuplicateIdsAndCarriesTheRest`
     - `PlayerSkillBookTest.RollbackRankAboveTableMaxIsStoredButEffectiveAtMax`
     - `PlayerSkillBookTest.UpgradeRefusesRankAboveTableMaxWithoutCharging`
     - `PlayerSkillBookTest.RankZeroClassSkillIsUnlearnedButCarried`
     - `PlayerSkillBookTest.RankZeroClassSeedIsRegrantedInPlace`
     - `PlayerSkillBookTest.LegacySkillRankIsCarriedAndIgnored`
     - `PlayerSkillBookTest.OwnedListIsAnExplicitProjection`
     - `PlayerSkillBookTest.StoredLevelAboveCapReportsCappedPlayerLevel`
   - bag_test：
     - `PersistRoundTripTest.ValueDomainsBeyondThisBuildSurviveLoadAndSave`
     - `PlayerEquipAttributeTest.OverAllocatedPoolIsCarriedButIneffectiveOnEveryRecalculation`
     - `PlayerEquipAttributeTest.UnknownDimensionSurvivesLoadAndIsNotEffective`
     - `PlayerEquipAttributeTest.UnknownDimensionReducesRemainingAndSurvivesReset`
     - `PlayerEquipAttributeTest.ZeroValueUnknownKeyChangesNothing`
     - `PlayerEquipAttributeTest.LoweringAndRestoringTheLevelKeepsTheAllocation`
     - `PlayerEquipAttributeTest.ResetOfAnIneffectivePoolIsFreeAndKeepsHealth`
     - `PlayerEquipAttributeTest.SwitchingToAnOverAllocatedSchemeKeepsItIneffective`
     - `PlayerEquipAttributeTest.StoredLevelAboveCapUsesCappedLevelForTotals`
     - `PlayerEquipAttributeTest.OnlyUnknownKeysResetIsNothingToChangeAndFree`（10-30 新增）
     - `PlayerEquipAttributeTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys`（10-30 新增）
     - `PetAllocationTest.*`（13 条；10-30 新增 `StoredLevelAboveSpeciesCapIsEffectiveAtTheCap`、`SummonAndBattleCompareOwnerLevelWithTheCappedPetLevel`、`AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys`，这三条从未运行过）
     - `PlayerMissionSystemTest.ActivityAcceptUsesCappedLevelWithoutChangingStoredLevel`
   - 旧用例名不应再出现：
     - `PlayerSkillBookTest.InitializeOnLoadCleansBadRecordsAndHidesRetiredEntries`
     - `PlayerEquipAttributeTest.EquipmentChangeDoesNotTriggerAllocationConvergence`
     - `PlayerLevelRulesTest.StoredLevelAboveCapIsClampedDown`
4. **可选冒烟**：robot 属性加点场景（`robot/attribute_smoke_scenario.go`）。预备步骤里的洗点用 `callTolerant`，两种结果都接受，所以"GM 设回 1 级后洗点"从"无变化"变成"成功"不影响判定。
5. **失败时**：保留首个编译错误，以及失败用例的 gtest 原文（往返用例会打出 `MessageDifferencer` 的差异报告）；不改断言重跑。首个错误如果不在 §13.5 的文件里，归给对应会话。

### 13.3 评审结论与修正（2026-10-30）

**逻辑 / 回滚评审**（只读）：等级、加点、技能三块的加载与存盘路径里，没有发现按本版本配置改写存储值的地方。面板、宝宝列表、技能书与写入口用的是同一个判定（`EvaluatePool` / `EvaluatePetPool` / `PlanUpgrade`），没有找到刷血、刷点或扣错钱的路径。四条发现的处理：
1. **【中】宝宝没有生效等级**：存储等级只升不降，高于本版本种类上限时照常生效，比本版本合法宝宝强，与主人的 `CappedLevel` 口径不一致。
   - **已修（10-30，未编译）**：新增 `PetEffectiveLevel` = min(存储等级, `level_cap`)。总点数、二级属性、列表与快照的等级、召唤 / 参战 / 取消出战的等级比较都改读它；成长比较仍读存储等级。
   - 新增用例 `PetAllocationTest.StoredLevelAboveSpeciesCapIsEffectiveAtTheCap`、`PetAllocationTest.SummonAndBattleCompareOwnerLevelWithTheCappedPetLevel`（后者临时改小种类上限，用例结束还原）。
   - 同步了 `player_pet.h` 注释、`player-pet.md` §3.2、设计 §12.1 N12 / §12.2 / §15 v0.9。
2. **【中】资产通道入账遇到本版本不认识的物品 / 宝宝种类，记终态 REJECTED**（`asset_op_system.cpp` 的 `ValidateCreditBundle`）：既有问题，不是本批引入，本节不改。已转给第 6 批会话，对方的结论是这一批也不改代码，登记在设计 §12「收件箱、资产通道载荷」一行与 §13 第 10 条待拍板（见本文 §14）：
   - 理由：改判版本错位（RETRY）的前提是表行只增不删、Go 各入口按同源配置表校验。热更删行时，旧 scene 分不清"新行"和"已删行"，改判 RETRY 会让已删行的入账永远挂起。
   - 交易侧已核：交付被拒后合同转人工（`finalCredits → Manual`），托管原件保留，不丢资产。
3. **【低】池不生效时免费洗点，混版本窗口里可以反复用**：已接受，写进设计 §12.2「已接受的代价」。
4. **【低】技能、装备的等级辅助函数缺 `LevelComp` 时按 1 级继续**：既有行为，加载链保证 `LevelComp` 先挂上，触发不了。记为后续项（§13.4）。

**测试 / 静态编译评审**（只读）：
- 没有发现会编不过或会红的问题。核对过：include 根、改过签名的函数与全部调用点、protobuf v35 接口（`Map::contains` / `at`、`MessageDifferencer`）、tip 枚举名、夹具辅助函数。真表前提也逐条推过：角色池与宝宝池每级 5 点，洗点分别 500 / 300 金，30 级以下免费；宠物 1 的 `level_cap` 是 120。
- 四处覆盖不足的处理：
  1. 战斗快照、技能伤害、回血 buff、穿戴的等级读点没有超上限 / 0 级用例：未补。`player_battle_settlement_test.cpp` 正被别的会话改，记为后续项（§13.4）。
  2. 自动加点改了剩余点口径但没有用例：已补角色与宝宝各一条 `AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys`。
  3. 角色侧"只剩未知键时洗点算无可洗"原用例用的是 0 值未知键，没有区分力：已补 `PlayerEquipAttributeTest.OnlyUnknownKeysResetIsNothingToChangeAndFree`（未知键带 30 点）。
  4. `PetAllocationTest.UnknownDimensionSurvivesLoadAndRecalculation` 末尾的序列化往返是自己比自己，恒真：已删，注释改为指向真实存盘往返用例。

### 13.4 未做 / 风险

- **客户端**：面板、技能书有几处客户端可见的语义变化（设计 §12.2 末段），客户端仓未获授权读取，需要客户端侧确认显示与提交没有问题。
- **约束只能靠评审守**：技能 id 不复用、不改归属、`max_rank` 只增，维度 id 不复用、不改池，按等级的规则单调不减，下调上限须配迁移。§13 第 7 条评审清单由第 6 批会话维护，请其引用设计 §12.2 的约束清单。
- **契约测试没做**：`LevelComp.level()` 读点白名单靠 grep 守，等代码稳定后再按行级白名单补。
- **§4.4 登记**：技能条目、加点方案、宝宝实例本层新字段"暂无守卫，按 R5"一行，由第 6 批会话加。
- **D7**：调大 `kMaxAppliedRecords` 的版本不可回滚 scene，仍是发布约束。
- **缺 `LevelComp` 的语义**：`player_skill.cpp` 与 `player_equip.cpp` 的等级辅助函数缺组件时按 1 级继续，与组件访问规则"持久数据缺失 fail-closed"不一致。加载链保证不会触发；要统一，就改成缺组件即拒绝，并补用例。
- **10-30 的改动没有运行证据**：`player_pet.cpp`（宝宝生效等级）和 5 条新用例都要在本次复跑里确认。
- **等级读点覆盖缺口**：战斗快照（`player_battle.cpp` 的 `BuildBattleSnapshot`）、技能伤害（`skill.cpp` 的目标等级与伤害参数）、回血 buff（`modifier_buff_impl.cpp`）、装备穿戴（`player_equip.cpp`）在存储等级为 0 或大于 85 时没有用例。其中 `skill.cpp` 与回血 buff 的"0 级按 1 级"是新行为。等 `player_battle_settlement_test.cpp` 的并行修改结束后补：快照等级 == 85、`LevelComp` 不变；施法者 / 目标等级为 0 和 100 时的伤害分别等于 1 级和 85 级。

### 13.5 文件清单

- 生产代码（`cpp/libs/services/scene/`）：
  - `player/system/`：`attribute_allocation_rules.h`、`pet_rules.h`、`player_level_rules.h`、`player_attribute.{h,cpp}`、`player_pet.{h,cpp}`（10-30 又改：宝宝生效等级）、`player_skill.{h,cpp}`、`player_database_loader.cpp`、`player_mission.cpp`；
  - `player/system/player_equip.cpp`：只改了顶部的 `PlayerLevel`；
  - `battle/system/player_battle.cpp`：只改了快照等级一行加 include；
  - `combat/skill/system/skill.cpp`、`combat/buff/system/modifier_buff_impl.cpp`。
- 测试：
  - `cpp/tests/skill_test/{player_skill_book_test.cpp,skill_test.cpp}`；
  - `cpp/tests/turn_battle_engine_test/{class_skill_test.cpp,attribute_allocation_rules_test.cpp}`；
  - `cpp/tests/bag_test/{player_equip_attribute_test.cpp,player_feature_handler_test.cpp,persist_round_trip_test.cpp}`（前两个 10-30 又加了用例）；
  - `player_mission_system_test.cpp` 里那条用例是别的会话在 `83859448` 加的，一并验证。
- proto 注释：`proto/common/component/player_skill_comp.proto`、`proto/scene/player_attribute.proto`、`proto/scene/player_pet.proto`。
- 文档：
  - `docs/design/{mixed-version-persist-field-protection.md,class-skills.md,player-attribute-allocation.md,player-pet.md}`；
  - 本文、`docs/PROGRESS.md`；
  - Java 仓 `PARITY.md`（未提交）。

## 14. 续：第 6 批（退役字段残留、按数量扣除守卫、新币种；2026-10-23～30，第 6 批会话）

> **状态**：10-23 写的部分已随整仓快照提交入库（`5de132e2` 等）；10-30 的补齐在工作区，未提交。没有做过本主题专门的验证。
> **运行旁证**：Codex 10-24 全量运行 currency_test（318 条）、bag_test（708 条）全部通过，Go shared 模块全量单测通过，这批 10-23 的新用例都在列。player_inbox、guild、trade、scene_manager 四个 Go 模块没有旁证；10-30 新增与改动的部分未编译、未测试。
> **Java 版**：待做，PARITY 已登记。

### 14.1 做了什么

**① 退役字段的残留不算版本错位**（设计 §4.4）
- 生成器把台账的 `retired=<消息全名>#<字段号>` 行生成为 `persist_schema::kRetiredFields` / `IsRetiredField`。生成器在 `tools/proto_generator/protogen/internal/persistschema/{render,generate}.go`，用例为 `TestRenderOutputs`。
- `unknown_field_carry::CarriesNewerVersionFields` 取代 `HasUnknown`。合堆（`IsIndependentInstance`）、欠款合并（`DebtCarriesUnknownFields`）、按数量扣除（`IsCountableByConfig`）三处守卫统一用它。
- 用例：`UnknownFieldCarryTest.OnlyUnretiredUnknownFieldsCountAsNewerVersionFields`。生产退役清单目前为空，所以用注入谓词钉住过滤逻辑。

**② 按数量扣除守卫**（设计 §4.4）
- 谓词 `ItemStore::IsCountableByConfig`；`TotalCountOf` 与 `DrainStacks` 都只认可计单位。
- 三态预检 `ItemStore::CheckQuantities`，不足优先于版本错位。`Bag::CheckQuantities` 与 `Bag::CheckItemsAvailable` 静默返回，三个码分别是 kSuccess / kFeatureUnavailable / kBagInsufficientItems。
- 调用方：资产通道数量扣出（版本错位回 RETRY + 1006、不记账）、强化、镶嵌（回 1006，排在金币判定之前）、战斗快照（只计可计单位）。
- 10-30 补齐：
  - 资产扣出把版本错位排到按 guid、宝宝这些确定性校验之后。
  - 删除 `ItemStore::HasAll`：它没有调用方，而且会把版本错位并进"不足"。
  - 聚宝斋卖家在托管阶段也可以撤单。材料订单的托管腿没有截止时间，扣款一直回 RETRY 时，原来没有任何出口（`go/trade/internal/commerce/engine.go` 的 `Mutate` 撤单分支）。
  - `RemoveItemByPos` 补了约束注释。
  - `bag_service.h`、`player_equip.h`、`equipment-enhance-inlay.md`、`bag-instance-layout-split.md` 的口径同步。

**③ 新币种**（设计 §12 载荷行）
- 取值表的唯一来源是 `proto/common/base/currency_type.proto`，生成产物已在库。C++ `currency.h` 与 Go `shared/currencytype` 都从它派生；`IsKnownCurrencyType` 收成一份，三份本地副本已删。
- Go 各入口按同源取值表校验：收件箱入箱（`registry.go` 的 gain / debt 两个校验函数）、邮件附件（`mailgrant`）、帮会捐献配置、交易配置（`isTradableCurrency`）与托管包（`reconcile/pipeline.go`）、神秘商人表。
- scene 侧：
  - 收件箱执行器先判与版本无关的条件（解码、欠款额），再判币种；不认识的币种回 `kVersionSkew`，日志 `metric=persist_version_skew where=inbox_currency`。
  - 资产通道新增第 8b 步：回 RETRY + 1006，不记账（`where=asset_currency`）。
  - 原语与神秘商人仍按非法参数处理。
- 10-30 补齐：
  - 战斗节点奖励档的两处判据（`turn_battle_engine.cpp`、`table_battle_data_provider.cpp`）改调 `IsKnownCurrencyType`。
  - 收件箱 `TestValidatePayload` 补 4 条取值表用例。
- 订正：§10.7 与设计 §12 原写"资产通道在已有改动之后遇到不认识的币种记 APPLIED_PARTIAL"，不对。第 6 批之前的实际行为是在第 8 步、任何改动之前记终态 REJECTED(kAssetInvalidBundle)。

**日志关键字**：所有版本错位统一为 `metric=persist_version_skew`，用 `where=` 区分：asset_debit / asset_currency / equip_enhance / equip_inlay / inbox_currency。`deploy/k8s/asset-channel-alerts.yaml` 文件头已补值班查询，`AssetOpOnlineRetryStuck` 的描述也补了说明。

**文档**：
- 设计稿 v0.8：§0、R5、§4.4、§9、§12 载荷行、§13 第 5 / 6 / 7 条与新增第 10 条、§15；
- `04-asset-channel.md`：§4.9 第 8b 步、§4.38 版本错位处置；
- `player-inbox-service.md`：§6.2、§7.6；
- `docs/ops/player-inbox-runbook.md`：§5.2 A′ 的新币种来源，以及毒消息排查前先排除版本错位；
- `jubaozhai-market.md`：托管阶段撤单。

### 14.2 对抗复核裁决（2026-10-30）

10-23 规格出来后跑过三路攻击：一致性、行为、运维与扩展。裁决一步因额度失败，10-30 由本会话按当前代码逐条裁决：

| 发现 | 结论 | 处理 |
|---|---|---|
| 材料订单托管腿无截止、托管阶段不可撤单，6a 改 RETRY 后无出口（高） | 成立 | 托管阶段可撤单，补真库用例 `TestTiDBSellerCancelsMarketOrderStillInEscrow`，`jubaozhai-market.md` 同步 |
| 资产扣出版本错位排在 guid / 宝宝确定性校验之前（中） | 成立 | 调整顺序，补 `AssetOpSystemTest.QuantityDebitDeterministicRejectionsComeBeforeVersionSkewRetry` |
| bool `HasAll` 留着会把错位并进"不足"（中） | 成立 | 删除 |
| 日志关键字与规格不一致（中） | 成立 | 统一为代码实际打的 `persist_version_skew where=...`，文档跟代码 |
| 资产通道版本错位的值班查询与告警描述（中） | 成立 | yaml 补查询与描述，04 §4.38 补处置 |
| 收件箱版本错位停用只打一次日志；PoisonSuspect 指引会作废合法处置（中） | 成立 | runbook A′ 已补排查前置；周期补打与告警描述**未做**（见 14.4） |
| 普遍写入的 `ItemComp` 新字段会让回滚变成全服降级（中） | 成立 | R5 加"分两版发布、`size` 计数含义不变"，§13 第 7 条同步 |
| 6b / 6c 同版本在滚动发布下不够（低） | 成立 | 改为 Go 全量之后才滚 scene，首次上线除外 |
| 人工终结不是零成本出口（低） | 成立 | 04 §4.38、设计 §13 第 6 条写明 |
| 规格的谓词写成 `HasUnknown`（中） | 已不成立 | 实现用的就是 `CarriesNewerVersionFields` |
| 6c 先于 6b 的半截状态、生成步骤缺失（高 / 中） | 已不成立 | 6b 已落，生成产物已在库 |
| 跨版本战斗结算少扣却记 APPLIED（中） | 已由战斗结算会话修 | `HasUnsupportedInboxConsumption` 遇到带新字段的消耗物品回 `kVersionSkew` |
| GM 对新币种的处置可按 ID 透明执行，不必停用整个域（中） | 待拍板 | 设计 §13 第 10 条，推荐维持现状 |
| 新币种启用开关（中） | 待拍板 | §13 第 10 条，推荐到第一次真正新增币种时再做 |
| 新物品 / 宝宝表行在资产通道入账被记终态（低；§12 核对会话 10-30 也转来同一条） | 待拍板 | §12 载荷行、§13 第 10 条：先定表行只增不删，再改判 RETRY；拍板之前守发布次序 |
| 越界包类型的携带记录不参与数量判定（中） | 只在将来新包类型参与资产扣出时触发 | 设计 §13 第 7 条评审清单加一条 |
| `GetTotalItemCount` 改语义未改名（低） | 接受 | 注释已写明；约 100 处测试调用，改名另议 |
| 按 guid 改写带新字段的装备实例（低） | 接受 | 列入 R5"仍未守" |
| `ValidateDebitBundle` 只看可叠加、不看 `equip_kind`（低，既有行为） | 交负责人 → 2026-11-05 按资产路径 fail-closed 收紧（§21） | 14.4 |
| 收件箱入箱校验排在判重之前，Go 混副本时同请求可能不同结局（低） | 已被发布次序覆盖 | Go 全量之后才允许对新币种下处置 |

### 14.3 待 Codex 验证（照抄执行）

工作目录：仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7。C++ MSBuild 必须串行 `/m:1`。日志统一放 `build/verification/persist-batch6/`。

1. **生成**：本批的生成产物已在库，不需要重新生成：`cpp/generated/persist_schema/persist_schema_gen.h` 已含 `kRetiredFields`，`currency_type.pb.*` 已生成。只有别的会话在本轮之前改了 proto，才按 §13.2 第 1 步先跑 no-unity `proto-gen-run`。
2. **C++ 构建**：按 §5.2 第 1 步编 `game.sln`（`/m:1`，输出 Tee 到 `build/verification/persist-batch6/game-sln.build.log`）。通过标准：Scene、Battle 共享库与各节点退出 0。
3. **C++ 测试**（依次执行）：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-batch6
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-batch6
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle_engine_test -LogDir build/verification/persist-batch6
   ```
   通过标准：三个工程 gtest 全绿，下列用例在列且通过。
   - currency_test：
     - `CurrencyTest.CurrencyTypeEnumIsDerivedFromTheProtoTable`
     - `GmSanctionInboxExecutorTest.GainPayloadWithGarbageIsRejected`
     - `GmSanctionInboxExecutorTest.GainPayloadWithCurrencyUnknownToThisVersionReportsVersionSkew`
     - `GmSanctionInboxExecutorTest.AttachCurrencyDebtWithCurrencyUnknownToThisVersionReportsVersionSkew`
     - `GmSanctionInboxExecutorTest.AttachCurrencyDebtRejectsBadAmountBeforeCheckingCurrency`
     - `AssetOpSystemTest.QuantityDebitOfStacksCarryingNewerVersionFieldsRetriesWithoutRecording`
     - `AssetOpSystemTest.QuantityDebitDeterministicRejectionsComeBeforeVersionSkewRetry`（10-30 新增，从未运行）
     - `AssetOpSystemTest.CreditWithCurrencyUnknownToThisVersionRetriesWithoutAnyMutation`
     - `AssetOpSystemTest.DebitWithCurrencyUnknownToThisVersionRetriesWithoutRecording`
     - `AssetOpSystemTest.DuplicateUnknownCurrencyIsStillRejected`
     - `AssetOpSystemTest.AbortStillClosesASeqWhoseBundleCarriesAnUnknownCurrency`
     - `PlayerInboxSystemTest.ExecutorVersionSkewHaltsAfterConfirmingThePrefixAndNeverResumesLocally`（回归）
   - bag_test：
     - `UnknownFieldCarryTest.OnlyUnretiredUnknownFieldsCountAsNewerVersionFields`
     - `BagInstanceDataTest.QuantityPathsSkipStacksCarryingNewerVersionFields`
     - `PlayerEquipTest.EnhanceWithMaterialOnlyEnoughCountingNewerVersionStacksIsAVersionSkew`
     - `PlayerEquipTest.InlayGemWithOnlyNewerVersionGemsIsAVersionSkew`
   - turn_battle_engine_test：全绿即可。奖励档判据只改调同一谓词，行为不变。
4. **Go**（每个模块在各自目录下执行）：
   ```
   cd go/shared;          go test -count=1 ./currencytype/... ./mailgrant/... ; go vet ./currencytype/... ./mailgrant/...
   cd go/player_inbox;    go test -count=1 ./... ; go vet ./...
   cd go/guild;           go test -count=1 ./... ; go vet ./...
   cd go/trade;           go test -count=1 ./... ; go vet ./...
   cd go/scene_manager;   go test -count=1 ./... ; go vet ./...
   cd go/mail;            go test -count=1 ./... ; go vet ./...
   cd go/rank;            go test -count=1 ./... ; go vet ./...
   cd tools/proto_generator/protogen; go test -count=1 ./internal/persistschema/...
   ```
   通过标准：全部退出 0。`TestValidatePayload` 的 4 条取值表子用例、`currencytype` 的 3 条用例要在列。
5. **交易真库用例**（可选，需要一次性 TiDB；不要借 codex-* 容器）：设置 `COMMERCE_TEST_DSN` 与 `COMMERCE_REQUIRE_TIDB=1`，在 `go/trade` 下执行：
   ```
   go test -count=1 -run 'TestTiDBSellerCancelsMarketOrderStillInEscrow|TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren|TestTiDBPlayerTradeRequiresBothEscrowSummariesAndReturnsOnCancel' ./internal/commerce/
   ```
   没有 DSN 时这些用例会 Skip，Skip 不算通过。
6. **失败时**：保留首个编译错误与失败用例的原文，不改断言重跑。首个错误如果不在 14.5 的文件里，归给对应会话。

### 14.4 未做 / 待办 / 风险

- **收件箱版本错位停用的周期补打**：`Halt` 只打一次，`[10m]` 窗口过后告警熄灭，停用却对实体一直有效。做法：对 `reason=version_skew`（以及同样不会自愈的 `unknown_type`）按 `kSceneLogAlertWindowMs` 节拍补打，与 `ledger_invalid` 同口径；`player-inbox-alerts.yaml` 的 PoisonSuspect 描述补一句"先排除 version_skew"（runbook A′ 已写）。10-30 时 `player_inbox_system.cpp`、`player_inbox_runtime_comp.h`、`player-inbox-alerts.yaml` 都有别的会话未提交的改动（C-1 / C-4），所以没动。
- **战斗结算收件箱执行器**：载荷币种不认识时回 `kDefer`（`player_battle.cpp` 的 `CollectSettlementCurrencyGains` 失败分支），与 GM 执行器的 `kVersionSkew` 不一致；DEFER 在会话内只会空转。同一函数的 `>= kCurrencyMax` 判据也还没改调 `IsKnownCurrencyType`。该文件正被战斗结算的在途改动占用，没动，留给那项工作的负责人。
- ~~**交易 Go 侧三处币种字面量**（DRY，行为正确）：`commerce/engine.go` 与 `commerce/rmb.go` 的 `== 2`（绑定币禁售）、`svc/role_trade.go` 的 `c > 1`。改为引用 `currencytype.BindDiamond` / `isTradableCurrency`。~~ 2026-11-10 已改，见 §23。
- 设计 §13 第 10 条三项待拍板；R5 仍未守的场景见设计 §4.4 与 R5 行。
- ~~`ValidateDebitBundle` 只要求可叠加、不看 `equip_kind`：装备行被误配成可叠加时，交易按数量扣出会让装备属性无声丢失。这是既有行为，交负责人决定是否收紧。~~ 2026-11-05 已收紧，见 §21。
- `asset-channel-alerts.yaml` 其余 `{app=...}` 选择器仍匹配不到流（既有问题，契约测试注释已记），本批新加的那条已按正确写法。
- Java 版：取值表、Go 入口校验、版本错位结局、按数量扣除守卫、托管阶段撤单都待做。

### 14.5 文件清单

**10-23 部分（已入库）**：
- 生成器：`tools/proto_generator/protogen/internal/persistschema/{render.go,generate.go,persistschema_test.go}`；
- 实例层与背包：`cpp/libs/engine/core/utils/proto/unknown_field_carry.h`、`cpp/libs/modules/bag/{item_store,bag_system}.{h,cpp}`；
- 货币：`cpp/libs/modules/currency/{constants/currency.h,system/currency_system.{h,cpp}}`；
- scene 玩法：`cpp/libs/services/scene/player/system/{asset_op_system.cpp,gm_sanction_inbox_executor.{h,cpp},player_equip.cpp,player_mystery_merchant.cpp}`、`cpp/libs/services/scene/battle/system/player_battle.cpp`（快照一行）；
- proto：`proto/common/base/currency_type.proto`；
- Go：`go/shared/currencytype/{currencytype.go,currencytype_test.go}`、`go/shared/mailgrant/bundle.go`、`go/player_inbox/internal/inbox/registry.go`、`go/guild/internal/logic/economy_config.go`、`go/trade/internal/{config/config.go,reconcile/pipeline.go}`、`go/scene_manager/internal/logic/mystery_merchant.go`；
- 测试：`cpp/tests/bag_test/{bag_instance_data_test,player_equip_test}.cpp`、`cpp/tests/currency_test/{currency_test,gm_sanction_inbox_executor_test,asset_op_system_test}.cpp`。

**10-30 部分（工作区，未提交）**：
- C++：`cpp/libs/modules/bag/{item_store.h,item_store.cpp,bag_service.h,bag_service.cpp,bag_system.h}`、`cpp/libs/services/scene/player/system/{asset_op_system.cpp,player_equip.h}`、`cpp/libs/services/battle/{system/turn_battle_engine.cpp,data/table_battle_data_provider.cpp}`、`cpp/tests/currency_test/asset_op_system_test.cpp`；
- Go：`go/player_inbox/internal/inbox/registry_test.go`、`go/trade/internal/commerce/{engine.go,engine_tidb_test.go}`；
- 部署：`deploy/k8s/asset-channel-alerts.yaml`；
- 文档：`docs/design/{mixed-version-persist-field-protection.md,guild-phase2/04-asset-channel.md,player-inbox-service.md,jubaozhai-market.md,equipment-enhance-inlay.md,bag-instance-layout-split.md}`、`docs/ops/player-inbox-runbook.md`、本文、`docs/PROGRESS.md`；
- 仓外：Java 仓 `PARITY.md`。

同一批文件上有别的会话未提交的改动：`currency_test.cpp`、`player_battle.cpp`、`result_commit.h`、收件箱几份文件等。编译或用例失败时先按行号归因，不要回退任何一方。

## 15. 续：§12 持久值域收尾核对（2026-11-05，核对 §12 的会话）

> 本节只追加，不改上文。只读核对加改文档，没有改代码，也没有编译或运行。

- **代码状态**：§13 的改动已全部随别的会话的整仓快照入库（`5de132e2`、`fc504300b`，另有 `65c69f4a9` / `ccbb4af69` / `372caca25` 里的小改），这些文件在工作区里没有未提交改动。
  - 10-30 之后别的会话对这几个文件的改动，大多与本主题无关：删除战斗结算账本、`SendToPlayerClient` 改名、装备用例按新的词条上限调小测试值。
  - 只有一处相关：`ccbb4af69` 给两个夹具补载了自动加点方案表（见下）。
  - 玩家存档已改成 `player_store` 分组片段直写，但本主题核对过的入口都还在：加载走 `PlayerDatabaseMessageFieldsUnmarshal` → 各 `InitializeOnLoad`，存盘走 `MarshalPlayerForPersist`。10-22 之后玩家存档没有新增顶层字段，只删了 10、17、18、19 号，所以 §12.1 的逐项结论不用重做。
- **运行结果**：取自 11-02 r3，即玩家存盘重构那条线的 Codex 回归（`build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log`）。
  - 本主题在 bag_test 里有 25 条用例，23 条通过。通过的包括 10-30 新增的 `PetAllocationTest.StoredLevelAboveSpeciesCapIsEffectiveAtTheCap`、`PetAllocationTest.SummonAndBattleCompareOwnerLevelWithTheCappedPetLevel`、角色与宝宝各一条 `OnlyUnknownKeysResetIsNothingToChangeAndFree`，以及 `PersistRoundTripTest.ValueDomainsBeyondThisBuildSurviveLoadAndSave`、`PlayerMissionSystemTest.ActivityAcceptUsesCappedLevelWithoutChangingStoredLevel`。
  - 失败两条：`PlayerEquipAttributeTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys` 回 25013，`PetAllocationTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys` 回 26014。原因：我 10-30 写这两条用例时，没给夹具加载 `AttributeAutoPlan` 表，`AutoAllocate` 回了"无自动加点方案"。`ccbb4af69`（11-02）已在两个夹具的 `SetUp` 里补上 `AttributeAutoPlanTableManager::Instance().Load()`，补完之后还没有复跑。
  - 同一轮 bag_test 还有别的失败：14 条装备词条用例，以及 `PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave` 缺 `AssetRecallLine` 覆盖。它们不属于本主题，`ccbb4af69` 里也已改。
  - skill_test、turn_battle_engine_test：10-24 的全量运行已通过（见 §13 开头），之后这两块的本主题代码没有变。
- **D7 作废**：10-31 起，战斗结算改走 match SQL + player_inbox。`player_database.settlement_ledger`（17 号）已 `reserved`，`BattleSettlementLedgerComp` 与 `kMaxAppliedRecords` 已删除。
  - 设计 §8.3、§12、§12.1、§12.2 里"调大 `kMaxAppliedRecords` 的版本不可回滚 scene"这条发布约束已撤。
  - §12「待确认」那一行结案：`merge_state` 已删除，`stress_test_probe` 按设计每次写盘重新生成。
- **待 Codex**：只需要复跑 bag_test，确认那两条自动加点用例变绿。
  - 玩家存盘那条线的 `docs/handoff/player-save-direct-commit-20261030/progress.md`「给 Codex 的验证入口」已含 `game.sln` 构建与 bag_test 全量（单独报 `PetAllocationTest.*`），跑那一次就能覆盖。
  - 单独跑时：先在仓库根 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`（`run_cpp_tests -Build` 不重编库，库改过要先编），再执行：
    ```
    pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-value-domain
    ```
  - 通过标准：两条 `AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys` 在列且通过，本主题其余用例保持通过。失败时保留 gtest 原文，不改断言重跑。
- **仍在进行**：等级读点（战斗快照、技能伤害、回血 buff、装备穿戴）的超上限与 0 级用例，由用户 11-05 启动的会话「补等级读点的超上限与 0 级回归用例」在做。
- **Java（负责人 11-05 定：留 todo）**：不变，Java 仓 `PARITY.md` 原有那一行已登记待做。

## 16. 续：等级读点的超上限与 0 级回归用例（2026-11-05，补等级读点用例的会话）

> 本节只追加，不改上文。只加了测试，没改生产代码，也没有编译或运行（AGENTS §10.1）。补的是 §13.3「测试 / 静态编译评审」第 1 条和 §13.4 最后一条登记的缺口。

- **口径**：按 `player_level_rules.h` 文件头与设计 §12.2，存储等级原样携带，玩法读 `playerlevel::CappedLevel`（0 按 1，超过 85 按 85）。每条用例都同时断言生效值正确、`LevelComp` 的存储值不变。
- **新增用例**（6 条，四个读点各至少一条）：

  | 读点 | 工程 / 文件 | 用例 | 要点 |
  |---|---|---|---|
  | 战斗快照：`PlayerBattleSystem::BuildBattleSnapshot` | bag_test：`player_battle_settlement_test.cpp`，文件末尾新开一节 | `PlayerBattleSettlementTest.SnapshotLevelIsTheCappedLevelAndTheStoredLevelIsCarried` | 存储 100 时快照 85，存储 0 时快照 1，存储 85 时快照 85。夹具补齐组快照要的组件，会话号非 0 即可 |
  | 技能伤害：施法者等级（`CalculateSkillDamage` 的表达式参数） | skill_test：`skill_test.cpp` | `SkillDamageLevelTest.CasterStoredLevelIsCappedInTheDamageExpression` | 真表 1 号技能，走落点入口 `SkillSystem::HandleSkillSpell`。0 级等于 1 级（新行为，原先打不出伤害），100 级等于 85 级 |
  | 技能伤害：目标等级（`CalculateFinalDamage` 减伤的等级系数） | 同上 | `SkillDamageLevelTest.TargetStoredLevelIsCappedInTheDamageReduction` | 目标护甲 480，受伤比例才随等级变。0 级等于 1 级，100 级等于 85 级 |
  | 回血 buff：`OnHealthRegenerationBasedOnLostHealth` | buff_test：`buff_test.cpp` | `BuffHealthRegenerationLevelTest.StoredLevelZeroAndAboveCapHealAsLevelOneAndTheCap` | 夹具表新增 9027 行（buff_type 42，`level*health/100`），走 `BuffSystem::OnIntervalThink`。0 级等于 1 级（新行为，原先一滴不回），100 级等于 85 级 |
  | 装备穿戴：`player_equip.cpp` 顶部的 `PlayerLevel` | bag_test：`player_equip_test.cpp`，放在 `EquipChecksTheLevelRequirement` 之后 | `PlayerEquipTest.EquipLevelRequirementTreatsAStoredLevelAboveTheCapAsTheCap`<br>`PlayerEquipTest.EquipLevelRequirementTreatsAStoredLevelZeroAsLevelOne` | 在内存里临时改穿戴等级要求，TearDown 重新加载表。存储 100 够不着 86 级、够得着 85 级；存储 0 够不着 2 级、够得着 1 级 |

- **数值推导**（真表 `generated/tables`）：
  - 技能：1 号技能的伤害表达式是 `100*level`。目标护甲取 480，正好等于 1 级的等级系数（360 + 120），所以 1 级目标受伤比例 0.5，85 级目标是 10560 / 11040。
    - 打 1 级目标：1 级施法者掉 50，85 级掉 4250；如果直读存储等级，0 级掉 0，100 级掉 5000。
    - 1 级施法者打目标：1 级目标掉 50，85 级目标掉 ceil(95.65) = 96。
  - 回血：真表 17 号是 `0.013*level*health`，77 级起一跳就回满（0.013 × 77 > 1），85 级和 100 级分不出来，所以夹具行改用同类型、同参数的线性式。已损气血 10000 时，1 级回 100，85 级回 8500，都回不满。
  - 装备：真表样例装备最高 80 级，85 级和 100 级都够得着，所以用例里临时抬高要求。
- **区分力**：每条用例先断言等级确实影响结果（85 级大于 1 级）。技能和回血还断言表达式取值 0 级小于 1 级、100 级大于 85 级，免得相等断言恒成立。
- **已知边界**：目标一侧的 `combatdamage::LevelFactor` 自己也把等级夹到 1..85。所以即使 `skill.cpp` 直读目标的存储等级，目标那条用例的两个相等断言照样成立。它钉的是落到气血上的结果；`skill.cpp` 目标等级那一行是否走 `CappedLevel`，仍靠代码评审守。施法者那条没有这层兜底，能直接区分。
- **改动文件**：
  - `cpp/tests/bag_test/player_battle_settlement_test.cpp`：只在文件末尾加了一节。同一文件中段有别的会话未提交的收件箱币种用例，不是本节写的；
  - `cpp/tests/bag_test/player_equip_test.cpp`、`cpp/tests/skill_test/skill_test.cpp`、`cpp/tests/buff_test/buff_test.cpp`；
  - 本文、`docs/PROGRESS.md`。
  - 生产代码和工程文件都没动，没有新增源文件。
- **给 Codex**（工作目录为仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7，MSBuild 必须串行 `/m:1`，日志放 `build/verification/level-readers/`）：
  1. 库代码如果在上次编译之后改过（别的会话有在途改动），先编库：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。`run_cpp_tests -Build` 只编测试工程，链接的是预编好的库。本节没改库。
  2. 依次执行：
     ```
     pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/level-readers
     pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter skill_test -LogDir build/verification/level-readers
     pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter buff_test -LogDir build/verification/level-readers
     ```
  3. 通过标准：上表 6 条用例在列且通过；三个工程的其余用例不因本节变红。bag_test 在 11-02 那一轮有与本节无关的既有失败，以及之后的修正，见 §15。
  4. 失败时：保留首个编译错误和失败用例的 gtest 原文，不改断言重跑。快照那条如果卡在 `ASSERT_TRUE(...Snapshot...)`，用例消息里带着 tip 码，请一并回报。
- **Java 版（AGENTS §12）**：待做，不在本节范围。Java 加载时仍用 `PlayerLevels.clampStored` 压回等级（§13.1），这几个读点要随那一项一起对齐。Java 仓 `PARITY.md` 原有那一行已登记待做，本节不另加行。

## 17. 续：第 7 批：台账封存门禁、战斗结算币种、收件箱停用补打（2026-11-05，第 6 批会话）

> **状态**：已落码，未编译、未测试，在工作区未提交。用户第三次要求按最标准的方式做完，范围按服务器缺口清单第 13 条（`docs/handoff/server-gap-audit-20261101.md`）加上 14.4 的待办。Java 版待做（PARITY 已登记）。

**10-30 部分的运行证据（补记）**

Codex 11-02 跑过一轮全量 C++ 回归，证据在 `build/player-store-verify-20261102-r3-prep/06-cpp-tests/`。14.3 列出的 16 条 C++ 用例都实际运行并通过，其中包括 10-30 新增的 `AssetOpSystemTest.QuantityDebitDeterministicRejectionsComeBeforeVersionSkewRetry`；currency_test 299 条、turn_battle_engine_test 300 条全绿。

同一轮 bag_test 有 16 条失败，都不是这批的问题：
- 13 条 `PlayerEquipAttributeTest` 失败，原因是测试取值超过了配置表上限（`attr 2 ... 500 vs 400`）。是表数据被改过，不在本线。
- `PetAllocationTest.AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys` 是 §12 核对会话 10-30 新增的用例。
- `player_feature_handler_test.cpp:1053` 那一条也不在本线。
- `PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave` 报新持久类型 `AssetRecallLine` 不在往返夹具里。这是往返测试按设计在拦漏网的新类型；之后已由别人在 `ccbb4af69` 补进夹具，尚未重跑。

Go 侧（`currencytype`、收件箱入箱用例、交易托管阶段撤单）仍没有运行记录。

**做了什么**

1. **台账封存门禁**（设计 §3.1「封存」、§13 第 11 条；审计 r2-ps#1 / r2-ps#10）
   - 生成器 `tools/proto_generator/protogen/internal/persistschema/`：
     - `ledger.go`：新增 `Seal`；`sealed=<世代>:<行数>:<sha256>` 的解析与写回；`SealDigest` / `SealViolations` / `SealLedger`；
     - `apply.go`：每次生成核对封存，引导期加字段、合并带进同世代的行都失败，分别给出处置提示；
     - `generate.go`：`SealLedgerFile`。
   - 命令行：`cmd/root.go` 新增 `--persist-schema-seal`，与 `--grpc-handler` 互斥；`tools/scripts/dev_tools.ps1 -Command persist-schema-seal`。`Invoke-ProtoGenRun` 加了 `GeneratorArgs` 参数，缺省为空，proto-gen-run 的行为不变。
   - 发布预检：`tools/scripts/release_preflight.ps1` 的 H 节新增 `release.persist_schema.sealed`，生产档 FAIL、dev 档 WARN。
   - 台账 `proto/persist_schema_gen.txt` 只改了表头注释（两行），与生成器常量逐字一致。
   - 文档：`docs/ops/release-checklist.md` §2.1 ① / ⑦、§6.2；审计 `data-overwrite-rollback-audit-r2-20261019.md` 的 r2-ps#1 / #10 两行登记已落码。
   - 没做运行期字段集合指纹，理由见设计 §13 第 11 条。
2. **战斗结算的币种版本错位**（设计 §12 载荷行，缺口清单第 13 条后半）
   - `player_battle.cpp`：`CollectSettlementCurrencyGains` 改为三种结论（有效 / 不认识的币种 / 坏载荷）。
   - 收件箱执行器 `ApplyInboxSettlement`：不认识的币种回 `kVersionSkew`（原来是 `kDefer`），日志 `metric=persist_version_skew where=battle_currency`；坏载荷（金额越界、重复币种）先判，照旧 `kDefer`。
   - `ApplySettlementToEntity` 的结局不变。
3. **收件箱域停用的周期补打**（14.4 第 1 条）
   - 代码：`player_inbox_runtime_comp.h` 加 `nextHaltRelogMs`；`player_inbox_rules.h` 加 `kHaltedDomainRelogIntervalMs`（5 分钟，带 static_assert）。`player_inbox_system.cpp` 的 Tick 对停用的域先排期，到点补打 `metric=inbox_halted ... relog=1`（WARN），只打日志、不改状态。
   - 告警与文档：`deploy/k8s/player-inbox-alerts.yaml` 的停用查询注释写明会补打；PoisonSuspect 描述加「先排除版本错位」。运行手册告警表、收件箱设计 §9.4 与 §7.6 同步更新。

**待 Codex 验证（照抄执行）**

工作目录为仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7。C++ MSBuild 必须串行 `/m:1`。日志放 `build/verification/persist-batch7/`。**不要在主仓运行 `persist-schema-seal`**：项目未上线，台账封存之后引导期内就不能再加持久字段。只对要上生产的版本封存（release-checklist §2.1 ①）。

1. 生成器源码变了，先重建：
   ```
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
   ```
   可选：再跑一次 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。通过标准：台账 `proto/persist_schema_gen.txt` 与持久 schema 产物没有新的差异（表头两行已在工作区）。别的会话的 proto 改动带来的差异照常归属。
2. 生成器单测与静态检查（在 `tools/proto_generator/protogen` 下）：
   ```
   go test -count=1 ./internal/persistschema/... ./cmd/...
   go vet ./internal/persistschema/... ./cmd/...
   ```
   通过标准：全绿，下列用例在列：
   - `TestSealLedgerSealsMaxGenAndIsIdempotent`
   - `TestApplyAfterSealAddsFieldsInANewGeneration`
   - `TestApplyRejectsFieldAddedToASealedGenerationInBootstrap`
   - `TestApplyRejectsRowMergedIntoASealedGeneration`
   - `TestSealLedgerFileWritesOnlyWhenSomethingNewIsSealed`
   - 回归：`TestLedgerFormatParseRoundTrip`（已带封存行）、`TestParseLedgerRejectsMalformed`（新增 5 种非法封存行）、`TestRepositoryLedgerIsCanonical`（表头一致）。
3. PowerShell 契约测试：
   ```
   pwsh -File tools/scripts/tests/dev_tools_proto_gen_exit.tests.ps1
   pwsh -File tools/scripts/tests/release_common_version.tests.ps1
   pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
   ```
   通过标准：
   - 前两个退出 0。第一个新增 4 条 `persist-schema-seal（binary-primary / go-run）` 用例；第二个的完整制品正向用例多一条 `release.persist_schema.sealed` 断言。
   - 第三个只看「LogQL 契约:player-inbox-alerts.yaml …」这一条：本批只改了注释与告警描述。该套件的其余既有失败另列，不归本批。
4. C++：按 §5.2 第 1 步编 `game.sln`（`/m:1`）。测试工程链接预编好的库，只跑 `run_cpp_tests -Build` 不会重编 scene 库，所以必须先编整解。然后依次执行：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-batch7
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-batch7
   ```
   通过标准：
   - currency_test 全绿，`PlayerInboxSystemTest.HaltedDomainRelogsOnTheIntervalWhileTheEntityLives` 在列。
   - bag_test：`PlayerBattleSettlementTest.*` 全绿，含新增的 `InboxSettlementWithCurrencyUnknownToThisVersionReportsVersionSkew`；`PersistRoundTripTest.*` 全绿，`AssetRecallLine` 的夹具已补。
   - 上面说的 15 条既有失败（装备属性 13、宝宝加点 1、`player_feature_handler_test` 1）若仍在，另列归属，不算本批失败。
5. Go（10-30 的部分，至今没有运行记录）：在 `go/shared`、`go/player_inbox`、`go/guild`、`go/trade`、`go/scene_manager`、`go/mail`、`go/rank` 各跑 `go test -count=1 ./...` 与 `go vet ./...`。
   - 交易真库用例另跑：设置 `COMMERCE_TEST_DSN` 指向一次性 TiDB、`COMMERCE_REQUIRE_TIDB=1`，在 `go/trade` 下执行：
     ```
     go test -count=1 -run 'TestTiDBSellerCancelsMarketOrderStillInEscrow|TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren' ./internal/commerce/
     ```
     没有 DSN 时这条会 Skip，Skip 不算通过。
   - `go/trade/internal/commerce/engine.go` 当前有别的会话未提交的 manual 相关改动，失败时先按行号归因。
6. **失败时**：保留首个编译错误与失败用例原文，不改断言重跑。

**未做 / 风险**

- 运行期字段集合指纹没做（设计 §13 第 11 条）。部署绕过发布流程时（用未封存分支的产物上线），封存门禁管不到。
- 14.4 里其余待办照旧：
  - ~~交易 Go 侧三处币种字面量。`engine.go`、`rmb.go` 这两个文件当前有别的会话的改动，本批没动。~~ 2026-11-10 已改，见 §23。
  - 设计 §13 第 10 条三项待拍板。
  - ~~`ValidateDebitBundle` 不看 `equip_kind`~~，11-05 已做，见 §21。
  - ~~`asset-channel-alerts.yaml` 其余 `{app=...}` 选择器~~，11-05 已改，见 §21。
- Java 版：封存门禁随 Java 的机制二一起做；战斗结算币种、收件箱停用补打都待做。

**本节文件清单**（工作区，未提交）
- 生成器：`tools/proto_generator/protogen/internal/persistschema/{ledger.go,apply.go,generate.go,persistschema_test.go}`、`tools/proto_generator/protogen/cmd/root.go`；
- 台账：`proto/persist_schema_gen.txt`（只改表头注释）；
- 脚本与契约测试：`tools/scripts/{dev_tools.ps1,release_preflight.ps1}`、`tools/scripts/tests/{dev_tools_proto_gen_exit.tests.ps1,release_common_version.tests.ps1}`；
- C++：`cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/libs/services/scene/player/system/{player_inbox_system.cpp,player_inbox_rules.h}`、`cpp/libs/services/scene/player/comp/player_inbox_runtime_comp.h`；
- C++ 测试：`cpp/tests/bag_test/player_battle_settlement_test.cpp`、`cpp/tests/currency_test/player_inbox_system_test.cpp`；
- 部署：`deploy/k8s/player-inbox-alerts.yaml`；
- 文档：`docs/design/{mixed-version-persist-field-protection.md,player-inbox-service.md,data-overwrite-rollback-audit-r2-20261019.md}`、`docs/ops/{release-checklist.md,player-inbox-runbook.md}`、`docs/handoff/server-gap-audit-20261101.md`、本文、`docs/PROGRESS.md`；
- 仓外：Java 仓 `PARITY.md`。

## 18. 续：读点缺 `LevelComp` 时改为 fail-closed（2026-11-05，补等级读点用例的会话，接 §16）

> 本节只追加，不改上文。用户 11-05 要求把本主题剩下的服务器代码按最标准的方式做完，§13.4「缺 `LevelComp` 的语义」一条在这里落码。改了生产代码，**未编译、未运行**。

- **口径**：按组件访问规则，持久数据缺失时 `LOG_ERROR` 并 fail-closed，绝不补默认值。
  - 等级与职业都在加载时挂上，玩家缺了就是加载不变量被破坏。以前下面几处按 1 级或 0 号职业继续算。
  - `CappedLevel` 的"0 按 1"只换算存储值，不覆盖缺组件。
  - 这条口径已写进权威出处 `player_level_rules.h` 的文件头。
- **改法**：

  | 文件 | 以前 | 现在 |
  |---|---|---|
  | `player_skill.cpp` / `.h` | `PlayerLevel` 缺组件按 1 级，`PlayerClassId` 缺组件按 0 号职业（0 号职业会让旧通用技能生效） | 新增 `HasClassAndLevel`，缺任一项记 ERROR、返回 false：`HasSkill` 答没有，`EffectiveRank` 为 0，`FillBattleSnapshot` 不填技能，`CollectPassiveBonus` 为 0，`GrantInitialSkills` 不补发。两个取值函数改为在确认组件存在之后 `get`，与 `player_attribute.cpp` 的 `CheckAttributeInputs` 写法相同。新增 `CheckSkillUsable`，带原因：缺列表、职业或等级回 `kServiceUnavailable`，没有或不能用回 `kInvalidTableId`；`HasSkill` 等于它回 `kSuccess`。`BuildSkillBook` / `UpgradeSkill` 原来就有 `all_of` 守卫，没动 |
  | `player_skill_handler.cpp`（守护段内） | 缺组件时与"没有这个技能"一样回 `kInvalidTableId` | 改调 `CheckSkillUsable`，原样回它的码：故障回 `kServiceUnavailable`，业务拒绝仍是 `kInvalidTableId` |
  | `player_equip.cpp` | 穿戴门槛缺等级按 1 级、缺职业按 0 号 | 新增 `HasLevelAndClass`，`Equip` 在缺背包的同一处回 `kEquipInternalError`，两个包不动 |
  | `modifier_buff_impl.cpp` | 缺等级按 1 级回血 | 玩家（带 `Player` 标签）缺 `LevelComp`：记 ERROR，这一跳不回血。非玩家没有等级组件，仍按 1 级 |
  | `player_mission.cpp` | 接取后读不到等级，就静默跳过当前等级进度 | `CheckAccept` 对带等级条件（`kConditionLevelUp`）的任务要求 `LevelComp`，缺了在写任务之前回 `kServiceUnavailable`。不带等级条件的任务不受影响 |

- **没改**：
  - `skill.cpp`：施法者缺 `LevelComp` 本来就回 false，玩家目标由 `HasRequiredTargetComps` 拦下；NPC 目标按 1 级是另一条规则。
  - `player_attribute.cpp`、`player_pet.cpp`、`player_battle.cpp`：本来就先确认组件存在。
- **生产路径**：正常玩家走不到新分支，只有坏档才会。
  - 加载器先挂 `PlayerUint32Comp`、`LevelComp`，再调 `PlayerSkillSystem::InitializeOnLoad`；
  - 新号 `InitPlayerFromAllData` 先写等级再 `RegisterPlayer`；
  - 属性重算先过 `CheckAttributeInputs`，组战斗快照先过 `all_of`。
- **新增用例**（5 条）：
  - skill_test：`PlayerSkillBookTest.MissingLevelComponentLeavesNoUsableSkillsAndGrantsNothing`、`PlayerSkillBookTest.MissingClassComponentLeavesNoUsableSkills`。两条都断言 `CheckSkillUsable` 的三种码；
  - bag_test：`PlayerEquipTest.EquipRejectsWhenTheLevelOrClassComponentIsMissing`、`PlayerMissionSystemTest.LevelConditionAcceptIsRefusedBeforeAnyWriteWhenLevelComponentIsMissing`；
  - buff_test：`BuffHealthRegenerationLevelTest.PlayerWithoutLevelIsNotHealedWhileANonPlayerHealsAsLevelOne`。
  - 每条都先断言组件齐全时有这个能力作对照，再断言缺组件时拒绝或不生效、持久数据不变、组件不被补建。
- **改了一条既有用例**：skill_test 的 `PlayerSkillSystemTest.SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates` 在裸实体上断言 `HasSkill(1)` 为真，靠的正是"缺职业按 0 号"的兜底。给它补上已加载玩家本来就有的 `PlayerUint32Comp`（0 号职业）与 `LevelComp`（1 级），断言不变。
- **buff_test 夹具**：`BuffHealthRegenerationLevelTest` 拆成 `NewWounded` / `HealOnce` / `HealedByOneTick` 三个函数。§16 那条用例的实体现在带 `Player` 标签，与真实玩家一致，断言不变。
- **影响面核对**：
  - 真表里带等级条件的任务只有 6 号（条件 24），用例接取它本来就因别的条件回 `kServiceUnavailable`；
  - 调 `PlayerEquipSystem::Equip` 的只有 `player_equip_test.cpp`，夹具两个组件都挂；无背包那条照旧回 `kEquipInternalError`；
  - `HasSkill` 的生产调用方只有施法 handler。
- **改动文件**：
  - 生产代码：`cpp/libs/services/scene/player/system/{player_skill.cpp,player_skill.h,player_equip.cpp,player_mission.cpp,player_level_rules.h}`、`cpp/libs/services/scene/combat/buff/system/modifier_buff_impl.cpp`、`cpp/nodes/scene/handler/rpc/player/player_skill_handler.cpp`；
  - 测试：`cpp/tests/skill_test/{player_skill_book_test.cpp,skill_test.cpp}`、`cpp/tests/bag_test/{player_equip_test.cpp,player_mission_system_test.cpp}`、`cpp/tests/buff_test/buff_test.cpp`；
  - 文档：设计 §12.2「等级上限」一行、本文、`docs/PROGRESS.md`。设计稿上同时有别的会话未提交的改动，本节只动了那一行。
- **给 Codex**：本节改了库，而 `run_cpp_tests -Build` 只编测试工程、不重编库，所以必须先编 `game.sln`：
  1. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`，输出 Tee 到 `build/verification/level-readers/game-sln.build.log`。通过标准：Scene 共享库与 Scene / Gate / Battle 节点退出 0。
  2. 再按 §16 的三条命令依次跑 bag_test、skill_test、buff_test。通过标准：
     - §16 的 6 条与本节 5 条在列且通过；
     - 被改的 `SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates` 通过；
     - 三个工程的其余用例不因本节变红。
  3. 失败时：保留首个编译错误与失败用例的 gtest 原文，不改断言重跑。
- **残余**：
  - `LevelComp.level()` 的直读白名单仍只靠评审守（§13.4「契约测试没做」），没有机器门禁。
  - 客户端：缺组件只在坏档时出现，客户端看到的是这几类请求被拒（施法与接任务回 `kServiceUnavailable`，穿戴回 `kEquipInternalError`），不需要客户端改动。
- **Java 版（AGENTS §12）**：待做，随 §13.1 那一项一起对齐，不另加行。

## 19. 续：第 7 批补充：独立复核修正与存储分组门禁（2026-11-05，第 6 批会话，接 §17）

> **状态**：已落码，未编译、未测试。§17 与本节的改动已被 `0f484db0d` 起的几个整仓提交收进去，都不是本会话提交的；只有下面「台账表头被并发生成改回」那一处修正写在这些提交之后，在工作区未提交。§17 的 Codex 命令照旧执行，本节只列增量。

**独立复核**

对 §17 的改动与 10-30 没有运行证据的 Go 改动做了一次只读复核，没有高危，结论如下：

| 发现 | 结论 | 处理 |
|---|---|---|
| 发布预检只比世代号、不核摘要：合并带进同世代的行之后，封存已被违反，最大世代却仍等于最后一条封存的世代，预检误判 PASS（中） | 成立 | 生成器加只核不写的 `--persist-schema-verify`，入口是 `persistschema.VerifyLedgerFile`、`dev_tools.ps1 -Command persist-schema-verify`。发布预检封存到最大世代之后调它（`go run ./cmd --persist-schema-verify`），摘要口径只在生成器里一份。`TestRepositoryLedgerIsCanonical` 同时核仓库台账的封存 |
| 预检档位与文档不一致（低） | 成立 | 代码沿用脚本其余检查的口径（只有 dev 档降为 WARN），文档改为「内部测试版本用 `-ReleaseProfile dev`」 |
| 预检新函数插在 `Invoke-ReleaseArtifactChecks` 的帮助块与函数之间（低） | 成立 | 挪到帮助块之前 |
| 引导期的违反提示分不清成因，同一根因按封存条数重复报（低） | 成立 | 先核台账本身（合并或手改），再核本次新增行（引导期加字段），分别给处置；只报世代最低的一条，其余计数 |
| 文档指针写成 §2.1 ⑦、错误头写 §3.2（低） | 成立 | 改为 §2.1 ①、§3.1 / §3.2 |
| 契约测试说明「封存不跑生成后钩子」证明不了（低） | 成立 | 说明改为「只调用一次生成器」，同时覆盖 seal / verify 两条命令 |
| 单项模式也会起 pprof（低） | 成立 | 条件加上两个台账开关 |
| 封存只封台账、不核 proto（低，推测） | 成立 | `release-checklist.md` §2.1 ① 写明封存前先跑一次生成，并确认台账与产物无改动 |
| 范围外：`market.go` 选对手单只按 status=3，托管阶段（stage=escrow）的单子也是 3，会被选为最优对手；`matchMarket` 发现阶段不对就本轮不成交 | ~~未核实到底~~ 成立（2026-11-10 核实） | ~~`market.go` 与 `engine.go` 当前有别的会话未提交的改动，本批不动，列入下面的待办~~ 2026-11-10 已修，见 §23 |

**存储分组门禁（数据覆写审计 R2 的 N1，「数据覆写审计 R2 交接」会话转来）**

- **问题**：玩家存储按 `OptionPersistGroup` 把 `player_database` 拆成多行。`player_store_projection.cpp` 的 `AssemblePlayer` 只认字段的当前分组，旧分组行里的同一字段会被 `ClearField`。所以字段换组之后，第一次载入会把旧分组那一行里的字段清掉，而新分组那一行里还没有，第一次存盘就把它永久写丢。`player-save-direct-commit.md` §1 原写「换组必须安全」，实际只防住了两行各有一份时合并翻倍。
- **做法**：
  - 生成器 `schema.go` 读 `OptionPersistGroup`（600041）。
  - 台账给玩家表顶层字段记 `group=<分组>`（`ledger.go` 的 `Field.Group` / `HasGroup`，不进封存摘要）。
  - `apply.go`：分组换掉或分组选项被删时生成失败，提示走 R4 expand / contract；分组出现之前写下的旧行第一次生成时原地补记，留一条 Note。
  - 投影代码只加注释，行为不变。
- **文档**：`player-save-direct-commit.md` §1、混版本设计 §3.1「存储分组不得改」与 R4、v0.10。
- **用例**：`TestCollectSchemaReadsStorageGroupOnTableFieldsOnly`、`TestApplyRejectsStorageGroupChange`、`TestApplyRejectsRemovedStorageGroup`、`TestApplyBackfillsStorageGroupOnce`，以及 `TestParseLedgerRejectsMalformed` 新增的 3 种非法 `group=` 段。
- **Java 版**：不适用。Java 玩家状态是单个 BLOB，没有分组拆行，不往 PARITY 加行。

**给 Codex 的增量（接 §17 的命令）**

1. 第 1 步的 `proto-gen-run` 会改台账：给 `player_database` 的每个顶层字段行一次性补上 ` group=<n>`。这是预期差异，与生成产物一起提交；台账其余行与持久 schema 产物应无差异。
   - 如果报「存储分组由 X 改成了 Y」或「OptionPersistGroup 被删掉了」，说明补记之后又有人改了分组，按提示处理，不要改台账。
   - 补记之前不会有这类报错。
   - **第 1 步必须用当前源码生成**：先 `proto-gen-build`，再 `proto-gen-run`。11-05 23:07 有人编了 `proto-gen.exe`，随后开始的一次生成在 23:11 结束时把整份台账写回。那次生成用的源码早于存储分组改动（23:09），所以台账表头少了 `group=` 那一行注释，`TestRepositoryLedgerIsCanonical` 会因此失败。本会话已把表头改回与 `ledger.go` 的 `ledgerHeader` 常量逐字一致，台账其余行没动。`dev_tools.ps1` 发现 exe 比生成器源码旧时会告警并改走 `go run`（`Get-ProtoGenBinaryStaleReason`），只要走脚本就不会再用旧 exe；直接调用 `proto-gen.exe` 不会经过这道检查。
   - 第 1 步之后台账的预期差异只有 ` group=<n>` 的补记。如果表头又变了，说明生成用的仍是存储分组改动之前的二进制，停下来先重建，不要提交这份台账。
2. 第 2 步的生成器单测在列表上新增：
   - `TestVerifyLedgerFileRequiresAnIntactSealAtTheMaxGeneration`；
   - 上面 4 条存储分组用例；
   - `TestApplyRejectsRowMergedIntoASealedGeneration` 现在走「台账本身已违反封存」那条提示。
3. 第 3 步的 `dev_tools_proto_gen_exit.tests.ps1` 现在有 8 条台账命令用例：seal / verify × exe / go run × 退出 0 / 23。
   - `release_common_version.tests.ps1` 不变：仓库台账未封存，预检在第一步就判 WARN，不会去跑 go。
4. 只读核对可单独跑：`pwsh -File tools/scripts/dev_tools.ps1 -Command persist-schema-verify -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。仓库台账未封存时它应失败，并报「还没有封存过」。这是预期结果，不要为了让它通过去封存主仓台账。

**待办 / 风险（在 §17 之外新增）**

- ~~生成器单测不在 CI 的 Go 模块扫描里~~，11-05 已处理，见 §21。
- ~~交易盘口：托管阶段的材料订单会被当成最优对手单，挡住同价位的撮合（复核的范围外观察，未核实到底）。`market.go` / `engine.go` 正被别的会话改，留给交易线负责人。~~ 2026-11-10 核实成立并已修，见 §23。

**本节文件清单**（在 §17 清单之外新增或再改；除台账表头那一处外已进整仓提交）
- 生成器：`tools/proto_generator/protogen/internal/persistschema/{schema.go,ledger.go,apply.go,generate.go,persistschema_test.go}`、`cmd/root.go`；
- 台账：`proto/persist_schema_gen.txt`（表头注释与生成器常量一致。23:11 被并发生成改回旧表头，之后又改回来了，这一处在工作区未提交）；
- 脚本：`tools/scripts/{dev_tools.ps1,release_preflight.ps1}`、`tools/scripts/tests/dev_tools_proto_gen_exit.tests.ps1`；
- C++：`cpp/libs/services/scene/player/system/player_store_projection.cpp`（只加注释）；
- 文档：`docs/design/{mixed-version-persist-field-protection.md,player-save-direct-commit.md}`、`docs/ops/release-checklist.md`、本文、`docs/PROGRESS.md`。

## 20. 续：`LevelComp.level()` 直读白名单的契约测试（2026-11-05，补等级读点用例的会话，接 §18）

> 本节只追加，不改上文。补的是 §13.4「契约测试没做」一条，以及 §18 残余里的同一项。这是只读源码的检查，**未运行**。

- **测试**：新增 `tools/scripts/tests/test_cpp_level_comp_reads.py`，用 Python 标准库 `unittest`。与 `test_cpp_grpc_dependency_boundary.py` 同一类：只读源码，不编译，不连网。
  - **扫描范围**：`cpp/libs`、`cpp/nodes` 下含 `LevelComp` 或 `level_component` 的 `.h` / `.hpp` / `.cpp` / `.cc`，跳过 build、generated 等目录。
  - **读点识别**：
    - `<LevelComp>(...)` 调用链或 `level_component()` 后面紧跟 `.level()` / `->level()`；
    - 绑定到 `LevelComp` 的变量上的 `.level()` / `->level()`。绑定包括声明、if 初始化语句、函数形参，以及 `auto& x = *y` 这类别名；
    - 注释、字符串、原始字符串、字符字面量先等长换成空白，行号不变。
  - **判定**：
    - 读点落在同一条语句里还没闭合的 `CappedLevel(` 之内（可以跨行），算经过换算；
    - 否则必须在同一行带标记注释 `// 直读存储等级:<场合>`，场合与文件要对得上：加载器 / 存盘 → `player_database_loader.cpp`，GmSetLevel → `player_attribute.cpp`，管理端 dump → `scene_admin_handler.cpp`；
    - 标记行上没有直读的，算陈旧标记，也报错。
  - **用例**：`LevelReadDetectorTest` 用 13 条合成片段钉住识别规则；`RepositoryLevelReadContractTest` 两条，一条扫全仓，一条核对白名单文件还在。
  - **不在守卫范围内**：结构化绑定、经函数返回的引用、宏。这几种靠评审，测试文件头写明了；发现漏网的写法时把它补进识别规则和自测用例。
- **生产代码**：只加标记，行为不变。
  - `player_database_loader.cpp`：超上限告警里的两次直读合并成一次 `const uint32_t storedLevel = levelComp.level();`，带「加载器」标记；
  - `player_attribute.cpp` 的 `GmSetLevel`：取旧值那一行带「GmSetLevel」标记；
  - 存盘（整份 `CopyFrom`）与管理端 dump（`ShortDebugString`）没有 `.level()` 调用，不需要标记；
  - `player_level_rules.h` 文件头写明由这条契约测试守住。文件头里不写标记原文：它也在扫描范围内，写了会被当成陈旧标记。
- **静态推演**（未运行）：按当前代码，经 `CappedLevel` 的读点有 9 处（战斗快照、技能伤害两处、回血、属性、装备、任务、宝宝、技能书），原样直读有 2 处，都已带标记。`pet->level()`、`capRow->level()` 这类别的类型的调用不受影响。
- **提交状态**：截至写本节时，本会话 §16、§18 与本节的代码、测试改动，以及设计稿那一句，已被 `0f484db0d`、`6452b949d`、`571371972`、`f6fddb466` 几个整仓提交收进去，都不是本会话提交的。本节文字与对应的 PROGRESS 条目写在这之后。
- **给 Codex**：在仓库根目录执行，Python 3.8 及以上，只用标准库：
  ```
  python tools/scripts/tests/test_cpp_level_comp_reads.py -v
  ```
  - 通过标准：退出 0，`LevelReadDetectorTest` 的 13 条与 `RepositoryLevelReadContractTest` 的 2 条都是 ok。
  - 失败时保留完整输出：全仓那条会逐行列出 `文件:行号: 原因`。不要为了变绿去改识别规则或补标记；先判断是真违规还是误报，误报回报给 Claude。
  - 这条与 §16、§18 的 C++ 用例互不依赖，可以先跑。
- **Java 版（AGENTS §12）**：这条守卫只扫本仓 C++ 源码。Java 的等级读点随 §13.1 那一项一起对齐，到时按 Java 惯用方式补同类守卫，不另加 `PARITY.md` 行。

## 21. 续：收尾小项：数量扣除拒装备行、C++ 日志查询选择器、台账单测进 CI（2026-11-05，第 6 批会话，接 §19）

> **状态**：已落码，未编译、未测试，在工作区未提交。本节只追加，不改 §20。§14.4 表、§17 待办、§19 待办里对应的条目已划掉并指向本节。

**1. 按数量扣出拒绝装备行**（§14.4 遗留，原「交负责人」）

- **问题**：`ValidateDebitBundle` 只要求配置可叠加（`max_stack_size > 1`）。装备行被误配成可叠加时，交易会按数量扣出，再按数量发给对侧，强化、镶嵌、随机属性这些实例上的数据会无声丢失。
- **做法**：`asset_op_system.cpp` 的 `ValidateDebitBundle` 遇到 `equip_kind ≠ 0` 的配置，按坏包处理：记终态 REJECTED，原因 `kAssetInvalidBundle`，日志 `why=quantity debit does not accept equipment configurations`。依据是资产路径默认 fail-closed（AGENTS §11.3）。
  - 表数据正确时，装备行都是 `max_stack_size == 1`，原本就会被拒，所以行为不变。
- **用例**：`cpp/tests/currency_test/asset_op_system_test.cpp` 的 `AssetOpSystemTest.QuantityDebitRejectsEquipmentEvenWhenMisconfiguredAsStackable`。配置 1 是装备，用例在内存里把它改成可叠加，结束时还原。
- **没做**：导表期的「装备行不得可叠加」约束。导表器目前没有跨列约束；装备系统的各个入口（`player_equip.cpp`）本来就要求 `max_stack_size == 1`。
- **Java 版**：已在 PARITY「通用资产通道 scene 侧」行的「Java 待做」里登记。

**2. C++ 日志查询的流选择器**

- **问题**：C++ 日志由每个 Pod 的 Alloy sidecar 读文件送 Loki，流标签里没有 `app`。以下值班查询都写成了 `{app=...}`，结果恒为空：
  - `asset-channel-alerts.yaml`：battle 4 条、scene 1 条；
  - `data-service-audit-alerts.yaml`：1 条。
- **做法**：
  - battle 改为 `{job="cpp_nodes", service="battle"}`。三种 battle 编排都挂同一个 sidecar，zone 是 `global`；`service` 取自日志文件名前缀（`NodeTypeToShortName`）。
  - scene 改为 `{job="cpp_nodes", service="scene"}`。
  - 同批改了 `docs/ops/audit-topic-generation-bump-runbook.md` 的两条值班查询，以及 `player-inbox-runbook.md` 的待同步清单。
  - `docs/handoff/launch-readiness-20261013/spec_G2a.md` 里的旧写法属于历史交接，没改。
- **契约测试**：`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的「LogQL 契约」改为逐文件核对这三份告警文件。
  - 选择器的抽取口径由 `count_over_time({...` 改为「后面紧跟 `|=` / `|~` 的 `{...}`」，这样才抽得到 data-service-audit 那条不在 `count_over_time` 里的查询。
  - 各文件的条数下限：收件箱 9、资产通道 6、data-service-audit 1。
  - 静态推演：用 Python 按同一正则抽取，三份文件分别抽到 9 / 6 / 1 条，只用到 `job`、`service` 两个标签，未运行 PowerShell。
  - 这个测试文件里有别的会话未提交的改动（295 行、5480 行附近）。本节只替换了这一条用例，换行符保持 CRLF，PowerShell 静态解析 0 错误。

**3. 台账包单测进 CI**

- `.github/workflows/go-modules-ci.yml` 新增 job `persist-schema-ledger`，在 `tools/proto_generator/protogen` 下跑：
  - `go vet ./internal/persistschema/...`
  - `go test -count=1 ./internal/persistschema/...`
- 结果计入「Go 门禁汇总」。PR 的 paths 过滤加上了 `tools/proto_generator/**`。
- 整个生成器模块不进模块矩阵：它在 Linux 上的 build / test 没有实测过，按该 workflow「只收当前绿的」原则，先不收。
- 本地只做了 YAML 静态解析，CI 上还没跑过。

**4. 台账表头被并发生成改回**：见 §19「给 Codex 的增量」第 1 条。

- 23:43 有人重编了 `proto-gen.exe`，23:47 用它重新生成。生成结果是表头与 `ledgerHeader` 常量一致，`player_database` 顶层活字段全部补上了 `group=`（20 行）。这正是 §19 写的预期差异。
- 已退役的 `#10`、`#17`、`#18`、`#19` 没有 `group=`，这是对的：它们在 proto 里是 `reserved`。
- 投影代码对没写 `OptionPersistGroup` 的活字段直接报错（`player_store_projection.cpp:97`），所以「从没有分组选项到加上分组选项」不会悄悄丢数据。

**给 Codex**（接 §17 / §19 的命令，日志放 `build/verification/persist-batch7/`）

1. **C++**：`asset_op_system.cpp` 在库里，`run_cpp_tests.ps1 -Build` 不会重编库。所以先按 §17 串行 `/m:1` 重编 `game.sln`，再执行：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-batch7
   ```
   - 通过标准：`AssetOpSystemTest.QuantityDebitRejectsEquipmentEvenWhenMisconfiguredAsStackable` 在列且通过；`AssetOpSystemTest.*` 的其余用例照旧全绿。
   - 前提：配表随工程部署。如果 `ItemTableAvailable()` 断言失败，先查配表目录，不要跳过用例。
2. **契约测试**：
   ```
   pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
   ```
   - 通过标准：「LogQL 契约:告警文件里 C++ 侧的固定查询…」这条通过。
   - 失败时保留完整输出：断言会写明文件、选择器原文和多出来的标签。
   - 这个文件有别的会话的改动，其他用例红了先判断归属。
3. **CI**：`persist-schema-ledger` 只能推送之后在 GitHub 上看。本地用 §17 第 2 步的命令代替，那组命令覆盖了它。

**本节文件清单**（工作区，未提交）

- C++：`cpp/libs/services/scene/player/system/asset_op_system.cpp`、`cpp/tests/currency_test/asset_op_system_test.cpp`；
- 告警：`deploy/k8s/{asset-channel-alerts.yaml,data-service-audit-alerts.yaml}`；
- 测试：`tools/scripts/tests/k8s_deploy_contract.tests.ps1`（只改「LogQL 契约」一条）；
- CI：`.github/workflows/go-modules-ci.yml`；
- 文档：
  - `docs/ops/{audit-topic-generation-bump-runbook.md,player-inbox-runbook.md}`；
  - `docs/design/mixed-version-persist-field-protection.md`（§3.1 与 §13 第 11 条里关于 CI 的两句）；
  - 本文（§14.4 表、§17 / §19 待办、§19 的状态与表头说明）；
  - `docs/PROGRESS.md`；
- Java 对账本：`D:\luyuan\xuanming-server-mmo\PARITY.md`「通用资产通道 scene 侧」行。

## 22. 续：等级读点线的验证核对与日志英文化（2026-11-10，补等级读点用例的会话，接 §16 / §18 / §20）

> 本节只追加，不改上文。

- **C++ 已有运行证据**：Codex 11-08 的命名统一运行（`build/verification/naming-audit-20261108/`）全量跑了 bag_test、skill_test、buff_test，三个工程都全绿：932 / 80 / 37 条通过。
  - 本线 12 条用例逐条在 stdout 里是 `[       OK ]`：§16 的 6 条、§18 的 5 条，以及 §18 改过的 `PlayerSkillSystemTest.SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates`。
  - 回执 `completion-receipt.json`：`verifiedUtc` 2026-11-08T14:35Z，`cppPassed` 2935，`allRequiredChecksPassed` true。
  - 运行之后源码没变：在 `implementation-final-hashes.json` 清单里的 6 个相关文件，现在的 SHA256 与清单一致；不在清单里的，最后一次提交在 11-06，早于这次运行，命名提交 `74b097888` 也没碰它们；工作区当时没有未提交改动。
- **契约测试还没跑过**：`build/verification` 下搜不到 `test_cpp_level_comp_reads` 的任何运行记录。
- **本节改动**（AGENTS.md §2：服务与脚本的日志文案、诊断输出用英文；注释与错误字符串保持中文）：
  - `player_mission.cpp`：§18 加的那条 `LOG_ERROR` 原先是中文，改成英文 `[PlayerMission] Level-condition accept rejected: LevelComp missing (load invariant broken), entity=… mission=…`。只改了文案，判断与返回码不变。同文件里别处原有的中文日志不是本线写的，没动。
  - `test_cpp_level_comp_reads.py`：违规原因与断言消息改成英文。注释、文档字符串和标记文字 `直读存储等级:<场合>` 保持中文；诊断里原样引用标记文字，方便照抄。识别规则与判定逻辑不变。
- **给 Codex**（仓库根目录）：
  1. 契约测试：`python tools/scripts/tests/test_cpp_level_comp_reads.py -v`。通过标准：退出 0，15 条全是 ok。失败时保留完整输出，误报回报给 Claude，不要为了变绿去改规则或补标记。
  2. 日志文案改动随下一次整解构建一起确认能编过即可：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。不需要单独为它重跑 bag_test，没有用例断言日志文本。
- **状态**：本线服务端代码已完成。C++ 用例已有运行证据；契约测试与这次的日志改动待 Codex。Java 仍待做，`PARITY.md` 原有那一行已登记。

## 23. 续：交易盘口只收已托管订单、交易币种字面量换具名常量（2026-11-10，交易盘口会话，接 §14.4 / §17 / §19）

> **状态**：已落码，未编译、未测试，在工作区未提交。本节只追加；§14.4、§17、§19 里对应的待办已划掉并指向本节。Java 版待做（PARITY 已登记）。

**1. 盘口对手单选择（§19 的范围外观察，核实成立）**

- **核实路径**（从选单 SQL 读到撮合提交点）：
  - 选单 `market.go` 的 `marketCounterparty` 只按 `status=3`、剩余量、价格、不同玩家过滤。
  - 材料订单下单时就是 `status=3, stage=escrow`（`PlaceMarket`）。托管扣款落地后，`plan` 只把 stage 改成 `market_open`，status 不变。阶段只在 `workflow_proto` 里，SQL 分不出两者。
  - 撮合事务在锁内重读同一条 SQL，锁住两单后判 `aw.Stage != "market_open" || bw.Stage != "market_open"`，直接 `return nil`：不成交、不报错、不唤醒。
- **影响比当时写的大**：挡住的不只是同价位。价格更优的托管中订单会挡住 taker 与后面所有已上盘、价格可成交的对手单。
  - 新单只在进入 `market_open` 那一步撮一次，之后按 1 小时复查（设计 §15.8），买卖两边会交叉挂着。
  - 材料订单托管腿没有截止。托管扣款一直回 RETRY 时（§14.1 ② 的托管阶段撤单就是为这种情况留的出口），这本盘口一直撮不动。
  - 深度查询同样只按 status=3 聚合，把托管中的订单算进挂单量。
- **修法**（理由见设计 `jubaozhai-market.md` §15.9）：
  - `trade_commerce` 新增列 `on_book`（`proto/trade/trade_table.proto` 字段 22），盘口索引改为 `kind,market_id,item_config_id,side,status,on_book,unit_price,commerce_id`。
  - 判定只在 `data.OnBook` 一处：kind=4、side≠0、stage=market_open。`CommerceRepo.Insert` / `Save` 按要写的 workflow 现算，与 `workflow_proto` 同一条语句写出，不取记录上的旧值。`SaveStatus` 不动它，状态列已把转人工的行挡在盘口外。
  - 选对手单（预读与锁内重读）和 `GetMarketDepth` 都加 `on_book=1`。
  - `matchMarket` 的 taker 预检加 `!taker.OnBook`：托管中的订单不发起撮合，不发成交号，不抢盘口 guard 行锁。
  - 提交点照旧在锁内按 workflow 复核两边的 stage、撤单与剩余量（AGENTS §11.3），只加了一行注释。
  - 不改 status：它是客户端可见的 `CommerceOrderState`，改它要两版同批改客户端契约。
- **已有开发库**：项目未上线，清库重建。不清库时 `schemamigrate` 补出 `on_book` 列，存量行都是 0；同名的盘口索引不会被改成新列序。存量挂单要等下一次重存才回到盘口上。

**2. 交易 Go 侧币种字面量（§14.4 第 3 条）**

- `commerce/engine.go`：`Policy.Selection` 的 `== 2` 与 `validPrice` 的 `!= 2` 改为 `currencytype.BindDiamond`。
- `commerce/rmb.go`：`selection` 的 `== 2` 改为 `currencytype.BindDiamond`。没配 `CurrencyTypes` 时的默认 `[]uint32{0}` 是同一类字面量，一并改为 `currencytype.Gold`。
- `svc/role_trade.go`：`initRmb` 的 `c > 1` 改调 `config.IsTradableCurrency`。原 `isTradableCurrency` 改为导出，白名单只在 `config.go` 这一份，配置校验与 `svc` 都调它。
- **没改**：`engine.go` 的 `v.CurrencyType != 0` 与 `rmb.go` 的 `v.GetCurrencyType() != 0` 判的是「没填币种」（proto3 零值），不是拿某个币种做比较。
- **行为不变**：对现有取值表（0 / 1 / 2），新旧写法接受与拒绝的集合完全相同；表外的值 `c > 1` 拒绝，白名单也拒绝。

**用例**

- data 包，不连库：新文件 `go/trade/internal/data/commerce_repo_test.go`，复用 `asset_op_recovery_test.go` 的录制驱动。
  - `TestOnBookIsOnlyABookOrderInMarketOpen`
  - `TestCommerceSaveWritesOnBookFromTheWorkflow`
  - `TestCommerceInsertDerivesOnBookFromTheStoredWorkflow`
  - `TestScanCommerceReadsOnBook`
- commerce 包，不连库：
  - 新增 `TestBindDiamondIsNotTradableEvenWhenConfigured`（`engine_test.go`）；
  - `TestRmbSelectionRejectsMixedKindsBoundCurrencyAndUnconfiguredDiamond` 改用具名常量，补「绑定灵石被配进白名单也拒绝」一段；
  - `rmbRuntime` 夹具的 `CurrencyTypes` 改为 `currencytype.Gold`，值不变。
- config 包，不连库：新增 `TestIsTradableCurrencyIsGoldAndDiamondOnly`。
- 真库：新增 `TestTiDBOrderBookSkipsOrdersStillInEscrow`（`engine_tidb_test.go` 末尾）。修复之前它卡在「托管中的更优价不能挡住已上盘的对手单」那条断言。
- 原有 `TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren`、`TestRealDBUntradableCounterpartyIsTakenOffTheBook` 不用改：前者的订单都经 `advance` 进盘口，后者的坏对手单用 `Insert` 直接插入 `market_open` 的行，`on_book` 照样算出 1。

**给 Codex**（工作目录仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7；日志放 `build/verification/trade-book-escrow/`）

1. **生成**：本批改了 `proto/trade/trade_table.proto`，先重生成：
   ```
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
   ```
   - 通过标准：退出 0；`go/proto/trade/trade_table.pb.go` 的 `TradeCommerceRecord` 多出 `OnBook`（字段 22）；`cpp/generated/proto/trade/trade_table.pb.{h,cc}` 同步变化。别的会话的 proto 改动带来的差异照常归属。
   - 生成器源码本批没变，不需要 `proto-gen-build`。
2. **Go**（在 `go/trade` 下）：
   ```
   go test -count=1 ./... ; go vet ./...
   ```
   通过标准：两条都退出 0；上面 data、commerce、config 包的不连库用例在列且通过。
3. **交易真库用例**（需要一次性 TiDB，不要借 codex-* 容器，口径同 §17 第 5 步）：设置 `COMMERCE_TEST_DSN` 与 `COMMERCE_REQUIRE_TIDB=1`，在 `go/trade` 下执行：
   ```
   go test -count=1 -run 'TestTiDBOrderBookSkipsOrdersStillInEscrow|TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren|TestTiDBSellerCancelsMarketOrderStillInEscrow|TestRealDBUntradableCounterpartyIsTakenOffTheBook' ./internal/commerce/
   ```
   - 通过标准：4 条都通过。没有 DSN 时会 Skip，Skip 不算通过。
   - 夹具每例新建随机库并经 `schemamigrate.Up` 建表，新列与新索引随建表出来，不需要手工 DDL。
4. **其余模块**：
   - C++ 只有生成文件变了，没有调用方，随下一次整解构建确认能编过即可。
   - robot 不用 `TradeCommerceRecord`，vendor 里的绑定随它下次 `go mod vendor` 同步，本批不要求。
5. **失败时**：保留首个编译错误与失败用例原文，不改断言重跑。下列文件上同时有别的会话的改动，失败先按行号归因，不要回退任何一方：
   - 「Turn battle gap closure handoff」会话（缺口清单第 16 条：事件唤醒）：`engine.go` 的 `advance`、`schedule.go`、`schedule_test.go`、`engine_tidb_test.go` 的 `settleLegs` 与商城那条用例、`tick_isolation_db_test.go` 末尾、`commerce_repo.go` 的 `Wake`；
   - 别的会话：`asset_op_repo.go`、`reconcile/`、`svc/servicecontext.go`、`svc/assetchannel.go`；「全仓服务日志改英文（AGENTS §2）」会话在改 `commerce_logic.go` 等文件的日志文案。

**Java 版（AGENTS §12）**：盘口待做，Java 仓 `PARITY.md` 新增「2026-11-10 聚宝斋材料盘口只收已托管订单」一节。Java 的 `CommerceMatching` 选单与 `CommerceClientService` 深度查询是同样的写法。币种字面量只是 Go 内部写法，行为不变，Java 不登记。

**本节文件清单**（工作区，未提交）

- proto：`proto/trade/trade_table.proto`；
- Go：
  - `go/trade/internal/data/commerce_repo.go`（`CommerceColumns`、`ScanCommerce`、`OnBook`、`Insert`、`Save`）；
  - `go/trade/internal/commerce/market.go`、`rmb.go`，以及 `engine.go`（只动 import、`Policy.Selection`、`validPrice`）；
  - `go/trade/internal/logic/commerce_logic.go`（`GetMarketDepth`）、`go/trade/internal/config/config.go`、`go/trade/internal/svc/role_trade.go`；
- 测试：`go/trade/internal/data/commerce_repo_test.go`（新）、`go/trade/internal/commerce/{engine_test.go,rmb_test.go,engine_tidb_test.go}`、`go/trade/internal/config/config_test.go`；
- 文档：`docs/design/jubaozhai-market.md`（§15 一句、新增 §15.9）、本文（§14.4、§17、§19 划线与本节）、`docs/PROGRESS.md`；
- 仓外：Java 仓 `PARITY.md`。

## 24. 续：第 8 批：载荷新字段判版本错位、Item / Pet 表行只增不删、§13 第 10 条定案（2026-11-11，第 6 批会话，接 §19 / §21）

> **状态**：已落码，未编译、未测试，在 main 工作区未提交（没开分支）。负责人 11-11 要求「没做完就按最标准的方式做完」，据此把设计 §13 第 10 条的三项待拍板按有依据的标准做法直接定案，并做完 §12 载荷行写明「开启收件箱消费之前必须修」的载荷新字段。设计见 v0.12 与 §9 第 8 批。

**此前各批的验证状态（11-11 按用例名核对 `build/verification`）**

- 已过：第 7 批与 §21 的 C++ 用例，在 11-06 两次 Codex 运行里都通过（`build/verification/aps-1106-014851/resume/`、`build/verification/battle-handoff-20261106-r4/`）：
  - `PlayerBattleSettlementTest.InboxSettlementWithCurrencyUnknownToThisVersionReportsVersionSkew`；
  - `PlayerInboxSystemTest.HaltedDomainRelogsOnTheIntervalWhileTheEntityLives`；
  - `AssetOpSystemTest.QuantityDebitRejectsEquipmentEvenWhenMisconfiguredAsStackable`。
- 找不到运行记录（仍要跑）：
  - §17 第 2 步、§19 的生成器单测（protogen `internal/persistschema`）；
  - §17 第 3 步的两份 PowerShell 契约测试（`dev_tools_proto_gen_exit.tests.ps1`、`release_common_version.tests.ps1`）；
  - §21 的 `k8s_deploy_contract.tests.ps1`「LogQL 契约」。
  - 旁证：11-05 23:47 之后，用新生成器做的 proto 生成已经跑过多次（台账补上了 `group=`，后续字段 25、26 也照常登记），说明生成器能编译、台账门禁能放行正常改动；但这不能代替单测。

**1. 载荷新字段判版本错位（§12 载荷行）**

- **问题**：旧版 scene 照常执行带新字段的收件箱条目、资产通道请求，新字段的语义静默丢失，还记成 APPLIED。
- **判据**：`cpp/libs/engine/core/utils/proto/unknown_field_carry.h` 新增 `TreeCarriesNewerVersionFields`。
  - 逐层看已设置的子消息，repeated、map 条目也看。
  - 字段号落在该层 reserved 段里，或在台账退役清单里，算退役残留，不计入。
  - bytes 字段不往里解，所以资产包 `instance_proto` 里原样携带的实例照旧带着未知字段入账（§4.2）。
- **收件箱**：
  - `player_inbox_registry::PayloadCarriesNewerVersionFields` 按类型规格的 `payload_message` 解码整棵载荷。解不开的载荷返回 false，交给执行器判坏载荷。
  - `player_inbox_system.cpp` 在调执行器之前判；带新字段就按 `kVersionSkew` 停用本域，不调执行器。
  - 日志 `metric=persist_version_skew where=inbox_payload field=<消息#字段号>`，之后照旧是 `Executor reported version skew` 与 `metric=inbox_halted reason=version_skew`。
  - 同局重投不执行，所以不判。
- **资产通道**：第 8b 步先判这一项，`where=asset_request`，回 RETRY + 1006、不记账。中止（第 6 步）与已见 seq 的回放都在它之前，不受影响。
- **用例**：
  - `UnknownFieldCarryTest.TreeCheckWalksSubMessagesAndSkipsReservedNumbers`（bag_test）；
  - `PlayerInboxSystemTest.PayloadCarryingFieldsFromNewerVersionHaltsWithoutExecuting`（currency_test）；
  - `AssetOpSystemTest.RequestCarryingFieldsFromNewerVersionRetriesWithoutRecording`（currency_test）。
- **回归守卫**（现有用例，必须照旧通过）：
  - `AssetOpSystemTest.CompleteItemEscrowReplayAndCreditKeepIdentityGrowthAndUnknownFields`：实例字节里的未知字段不能被误判成版本错位；
  - 收件箱截断载荷那条：坏载荷仍记终态 REJECTED，不被新判据拦成版本错位。

**2. §13 第 10 条定案**

- **第 1 项：Item / Pet 表行 id 只增不删**。
  - `data/schema/cfg_options.proto` 新增表级 option `cfg_ids_permanent`（60005），`item_table.proto`、`pet_table.proto` 写上。
  - 导表器新模块 `tools/data_table_exporter/core/permanent_ids.py`：
    - 台账 `tools/data_table_exporter/state/permanent_ids/<表名小写>_ids.txt`；
    - 表里少了台账里的 id，在外键 / 主键同一位置整批不产出；
    - 生成阶段追加新 id；
    - 台账缺失时按当前表初始化并打 WARNING；台账格式坏了 fail-closed。
  - 规范写进 `data/AGENTS.md`。
- **资产通道「查不到表」改判版本错位**：
  - 第 8 步不再判"行存在"，只判配置号非 0；依赖行内容的规则（按数量扣出要可叠加、不是装备，整件实例要不可叠加）只在行查得到时判。
  - 第 8b 步新增 `where=asset_config`，覆盖物品、整件物品、宝宝三处，回 RETRY + 1006、不记账。
  - 原来的 `CreditUnknownItemRejected` 改名为 `CreditWithItemConfigUnknownToThisNodeRetriesWithoutRecording`，期望改为 RETRY，并补上配置号 0 记终态拒绝。
  - 新增 `DuplicateItemConfigUnknownToThisNodeIsStillRejected`、`QuantityDebitOfItemConfigUnknownToThisNodeRetriesWithoutRecording`、`CompleteInstanceWithConfigUnknownToThisNodeRetriesWithoutRecording`。
  - `AssetOpRecallTest.InvalidBundlesAreRejectedAndRecorded` 里那段 unknownConfig 改为期望 RETRY，并补上配置号 0 记终态拒绝。
  - 附带的安全改进：本节点物品表没加载成功时，带物品的入账一律 RETRY；原先会被逐条记成终态拒绝。
  - `docs/design/guild-phase2/04-asset-channel.md` §4.9 第 8 / 8b 步同步改写；`asset-channel-alerts.yaml` 补上两种 `where=`。
- **第 2 项（GM 对新币种按 ID 透明执行）**：维持现状，不改代码，理由写进设计 §13 第 10 条。
- **第 3 项（新币种启用开关）**：第一次新增币种时再做（YAGNI），不改代码。

**3. ~~没做：战斗结算 C-2 改整笔 DEFER（留给战斗线）~~ 已做，见 §25**

> 2026-11-11 ~ 12 由「战斗结算 C-2」会话做完。本段原先列的现状、理由与改法都搬进了 §25，以 §25 为准。当时没改的原因：11-11 01:50 `player_battle.cpp` 还有别的会话在写。

**4. 客户端核对（设计 §12.2 末段「客户端可见的语义变化」；原登记见本文 761 行附近的客户端待确认项）**

负责人 11-11 授权改客户端代码，核对了 `D:\luyuan\mmorpg-client`：
- **属性面板、宝宝面板**：剩余点、重置费用都直接用服务端下发的 `Remaining` / `RemainingPoints` / `ResetCostGold`（`AttributePanel.cs`、`PetPanel.cs`）。池不生效时服务端给 0，客户端显示为不可加点、免费重置，不需要改。
- **技能书修习**：能否修习同时看 `RankLimit` 与 `maxRank`（`SkillBookModels.cs` 的 `CanUpgrade`），存储重数超上限时不发请求；修习请求带存储重数，与服务端 `expected_rank` 口径一致，不需要改。
- **改了一处显示**：`Assets/Scripts/UI/Ugui/Skills/SkillPanel.cs` 列表行原来显示 `{存储重数}/{maxRank}`，回滚窗口里会出现「12/10」。现在按上限封顶显示，与服务端「按本版本上限生效」一致。
  - 新增用例 `SkillPanelPresentationTests.StoredRankAboveTheCurrentMaxIsShownCappedAndCannotBeUpgraded`（`Assets/Tests/EditMode/Battle/`）。
  - 两个文件都保持 CRLF。已被客户端仓 11-11 02:06 的整仓提交 `0ef7a0a6` 收进（不是本会话提交的）。

**给 Codex**（日志放 `build/verification/persist-batch8/`）

1. **导表器单测**，在 `tools/data_table_exporter` 下：
   ```
   py -m pytest -q
   ```
   - 通过标准：全绿；`tests/test_permanent_ids.py` 7 条（含参数化 2 条）与 `tests/test_schema_proto.py::test_permanent_ids_option_is_read_from_table_options` 在列。
2. **导表一次**，在仓库根目录执行 `dev.bat gen`：
   - 预期日志里 Item、Pet 各有一条 `Permanent row-id ledger is missing; initializing it from the current table`；
   - 预期新增 `tools/data_table_exporter/state/permanent_ids/{item,pet}_ids.txt`，与产物一起提交；
   - 本批不应让任何生成产物出现差异（option 只给导表器读）。有差异先报告，不要提交。
   - 另跑 `py tools/data_table_exporter/tools/verify_schema_parity.py`，通过标准是 0 差异。
3. **C++**：`asset_op_system.cpp`、`player_inbox_*` 都在库里，`run_cpp_tests.ps1 -Build` 不重编库。所以先串行 `/m:1` 编 `game.sln`，再执行：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter currency_test -LogDir build/verification/persist-batch8
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-batch8
   ```
   - 通过标准：上面「用例」与「回归守卫」点名的都在列且通过，`AssetOpSystemTest.*`、`AssetOpRecallTest.*`、`PlayerInboxSystemTest.*`、`UnknownFieldCarryTest.*` 全绿。
   - 失败时保留 gtest 输出。不要为了变绿去改期望，先判断是不是这批改动的问题。
4. **补跑此前没有运行记录的三项**：§17 第 2 步（protogen 单测与 vet）、§17 第 3 步两份 PowerShell 契约测试、§21 第 2 步 `k8s_deploy_contract.tests.ps1`。
5. **客户端**：在 `D:\luyuan\mmorpg-client` 用 Unity 命令行跑 EditMode 测试程序集 `MmorpgClient.Tests.EditMode.Battle`（`-runTests -testPlatform EditMode`，按客户端仓现有口径）。
   - 通过标准：`SkillPanelPresentationTests` 全绿，新用例在列。
   - 编译通过不能代替运行。

**Java 版（AGENTS §12）**：本轮不写 Java 代码。已在 Java 仓 `PARITY.md` 登记待做：
- 收件箱载荷 / 资产通道请求带新字段时按版本错位处理；
- 资产通道「查不到表」改判 RETRY；
- 战斗结算查不到表的掉落改整笔延后。
导表器的行 id 门禁两版共用（Java 读同一份导表产物），不另做。

**本节文件清单**（main 工作区，未提交）
- C++：
  - `cpp/libs/engine/core/utils/proto/unknown_field_carry.h`；
  - `cpp/libs/services/scene/player/system/{player_inbox_registry.h,player_inbox_registry.cpp,player_inbox_system.cpp,asset_op_system.cpp}`；
  - 测试 `cpp/tests/bag_test/bag_instance_data_test.cpp`、`cpp/tests/currency_test/{player_inbox_system_test.cpp,asset_op_system_test.cpp}`。
  - `asset_op_system.cpp`、`player_battle_settlement_test.cpp` 等文件里另有别的会话的未提交改动，本节只做了定点修改。
- 配表与导表器：
  - `data/schema/{cfg_options,item_table,pet_table}.proto`、`data/AGENTS.md`；
  - `tools/data_table_exporter/core/{permanent_ids.py（新）,schema.py,schema_proto.py,orchestrator.py}`；
  - `tools/data_table_exporter/tests/{test_permanent_ids.py（新）,test_schema_proto.py}`。
- 文档与告警：
  - `docs/design/mixed-version-persist-field-protection.md`（§9、§12 载荷行、§13 第 7 / 10 条、v0.12）；
  - `docs/design/guild-phase2/04-asset-channel.md`（§4.9 第 8 / 8b 步）；
  - `docs/ops/player-inbox-runbook.md`（A′）、`deploy/k8s/asset-channel-alerts.yaml`（描述）；
  - 本文、`docs/PROGRESS.md`。
- Java 对账本：`D:\luyuan\xuanming-server-mmo\PARITY.md`。
- 客户端（`D:\luyuan\mmorpg-client`，已随 `0ef7a0a6` 入库）：`Assets/Scripts/UI/Ugui/Skills/SkillPanel.cs`、`Assets/Tests/EditMode/Battle/SkillPanelPresentationTests.cs`。Java 对账本的登记已随 Java 仓 `7f29da40` 入库。

## 25. 续：第 8 批收尾：战斗结算对查不到表的物品 / 宝宝种类改整笔 DEFER（2026-11-11 ~ 12，战斗结算 C-2 会话，接 §24 第 3 段）

> **状态**：代码、用例、文档都已写完，**未编译、未测试**（Claude 不跑编译和测试命令）。
> - 11-11 的物品部分（`player_battle.{h,cpp}`）已随整仓提交 `4edb25640` 入库。此后 11-12 的 Docker 全量 C++ Debug 构建（`build/verification/docker-rebuild-all-20261112T040700.029055Z/`）成功，可作这部分能编过的旁证，不代替下面的验证。
> - 11-12 的宝宝部分、全部用例改动与文档在 main 工作区，未提交（没开分支）。
> - bag_test 自 11-10 之后没有运行记录。从 `4edb25640` 到这次用例改动之间，HEAD 上有 3 条用例还按旧口径断言，这段时间跑 bag_test 会红在这 3 条上，不是别的改动引起的：`InboxBattleResultRecordsActualAmountsNotTheTerminalSource`、`InboxDropWithDeletedConfigRowSkipsOnlyThatDrop`、`InboxConsumptionWithDeletedConfigRowDoesNotHaltDomain`（后两条现已改名）。

**为什么改**

- C-2（2026-10-30）的前提是「配置行可能被热更删掉」：行不会回来，整笔 DEFER 会永远卡住，所以只剔除查不到表的掉落 / 消耗，其余照常 APPLIED，剔除的项记 ERROR 交人工补偿。
- 2026-11-11 起 Item / Pet 表行 id 只增不删，由导表器 `(cfg_ids_permanent)` 门禁守住（§24 第 2 段、`data/AGENTS.md`「行 id 只增不删」）。查不到只可能是这一行还没热更到本节点，或结算来自更新版本的 battle 节点。
- 这时剔除就是丢掉玩家这一局的掉落。整笔 DEFER 没有副作用，这一行热更到本节点之后本地续做即成功，与号段不足、跨区冻结走同一条路。
- 资产通道在第 8 批已对「查不到表」回 RETRY。战斗结算是表行规则定下之后剩下的不一致点。

**1. 物品（11-11）**

改动都在 `cpp/libs/services/scene/battle/system/player_battle.cpp`：
- 删掉 `DropItemsWithoutConfigRow`、`SkippedSettlementItem`，以及应用成功后逐项打 `metric=battle_inbox_item_effect_skipped` 的循环。
- 新增 `SmallestItemConfigUnknownToThisNode`。`ApplySettlementToEntity` 在 `CollectApplicableInboxPets` 之后、`DropBlockedGains` 之前判：消耗或掉落里有本节点 Item 表查不到的配置号，整笔返回 false。
  - 位置必须在剔除封禁项、算实际可扣量之前：查不到的项既不能当「被封」剔掉，也不能当「持有 0」夹紧后照常应用。
  - 日志一条 WARN：`[PlayerBattle] metric=persist_version_skew where=battle_item player_id= battle_id= side=consumed|gained config_id=`。两边都有时报消耗那一边，同一边报最小的配置号，日志确定。
- `HasUnsupportedInboxConsumption` 的函数体没变（查不到行照旧 `continue`，交给上面那一步），只把注释改成新口径。
- 判 DEFER，不判版本错位：`kVersionSkew` 会停用整个域，要等实体重建才重拉；配置表在进程内热更，行到了本地重试就能成功。
- 退避与告警照收件箱现有路径，没有另造：
  - 执行器回 `{kDefer, 0}`，收件箱打 `metric=inbox_deferred reason=executor_defer`，本地按 1s→60s 退避重试，不重新拉取；
  - 原因码是 0，不带「背包放不下」，玩家腾背包不会把它提前；
  - 一直不落定时由现有的 `PlayerInboxDeliveredUnackedTooOld` 告警兜住。

**2. 宝宝（11-12，核对同类入口时发现，一并改）**

- 同一个执行器里，玩家带进这一局的宝宝种类在本节点 Pet 表查不到时，原先只跳过这只宝宝的战后终值回写（`metric=battle_inbox_pet_effect_skipped reason=pet_table_missing`），其余照常 APPLIED。
- Pet 表与 Item 表是同一条表行规则，跳过会永久丢掉这只宝宝的战后终值。现在 `CollectApplicableInboxPets` 遇到这种情况整笔返回 false，日志 `metric=persist_version_skew where=battle_pet`（带 `pet_id`、`pet_table_id`）。
- 没变的两种：宝宝已不在玩家身上（`pet_no_longer_owned`）、宝宝属性池缺配（`pet_pool_missing`）。它们不属于「表行还没到」，仍只跳过这一只、记 ERROR。
- 这一条超出了原任务点名的掉落 / 消耗。负责人 11-12 的要求是查出的问题现在就修，所以没有留到以后。

**3. 用例**（都在 `cpp/tests/bag_test/player_battle_settlement_test.cpp`）

| 用例 | 变化 |
|---|---|
| `PlayerBattleSettlementItemTest.InboxDropWithConfigUnknownToThisNodeDefersWholeSettlement` | 原名 `InboxDropWithDeletedConfigRowSkipsOnlyThatDrop`。期望从 APPLIED 改为 DEFER；同一笔里查得到的掉落、金币、击杀、HP 都不动，不铸号、不记流水；再试一次仍是 DEFER，原因码 0 |
| `PlayerBattleSettlementItemTest.InboxConsumptionWithConfigUnknownToThisNodeDefersWholeSettlement` | 原名 `InboxConsumptionWithDeletedConfigRowDoesNotHaltDomain`。期望从 APPLIED 改为 DEFER；查得到的消耗也不扣 |
| `PlayerBattleSettlementItemTest.InboxSettlementAppliesOnRetryOnceTheItemRowsAreHotUpdated` | 新增。消耗与掉落两行都没热更到时 DEFER，这一局的道具预留原样押着；热更之后同一份原件重试即 APPLIED，消耗从预留里扣、掉落入包、预留释放 |
| `PlayerBattleSettlementTest.InboxPetConfigUnknownToThisNodeDefersWholeSettlement` | 原名 `InboxMissingPetTableSkipsOnlyThatPetEffect`。期望从 APPLIED 改为 DEFER、零副作用 |
| `PlayerBattleSettlementTest.InboxSettlementAppliesOnRetryOnceThePetRowIsHotUpdated` | 新增。种类行没热更到时 DEFER；热更之后重试即 APPLIED，宝宝的战后终值写进去 |
| `PlayerBattleSettlementItemTest.InboxBattleResultRecordsActualAmountsNotTheTerminalSource` | 去掉了「配置行已删的掉落不记」那一项（这种结算现在整笔延后，走不到记结果）；消耗按实际持有夹紧的断言不变 |

- 模拟「还没热更到」的手法：把这一行从表的主键索引里摘掉（查行与 `Exists` 都走这份索引，与 `player_equip_test` 的 `EditableEnhanceIndex` 同一类手段）；热更就是重新 `Load()` 整张表。物品用例用夹具 `ScopedItemRowsNotYetHotUpdated`，析构时无条件再加载一次。
- 宝宝重试用例是这个文件里第一条让宝宝效果真正应用成功的用例，此前几条都是跳过或延后。前置断言了 Pet 表非空、宝宝属性池有配。它失败时先看是不是夹具缺表，再判断是不是本批改动的问题。

**4. 同步改的文档**

- 设计稿 `docs/design/mixed-version-persist-field-protection.md`：§9「第 8 批收尾」与表里一行、§12 载荷行、§13 第 10 条、v0.13。
- 运维手册 `docs/ops/player-inbox-runbook.md`：
  - §6.1 表把「物品配置行被热更删除」一行改成「物品 / 宝宝配置在本节点表里查不到」，宝宝一行去掉 `pet_table_missing`；
  - §5.2 A′ 加一条指路：`where=battle_item` / `where=battle_pet` 不是停用，是整笔延后。
- `docs/design/battle-result-recovery.md` §4.1「只记实际入账」一句。
- 告警规则没有改：
  - `player-inbox-alerts.yaml` 的毒消息告警描述已经写了，见到 `metric=persist_version_skew` 先按版本错位排查；
  - `battle_inbox_item_effect_skipped` 从来没有告警规则。

**给 Codex**（工作目录为仓库根 `D:\luyuan\xuanming-server-mmo-cpp`，pwsh 7，日志放 `build/verification/persist-batch8-battle/`）

1. 先编库。`player_battle.cpp` 在 scene 库里，`run_cpp_tests.ps1 -Build` 只编测试工程、链接预编好的库，不先编库跑的就是旧代码：
   ```
   msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
   ```
   - 必须串行 `/m:1`，并发会报假的 C1041 / LNK1104。
   - 通过标准：0 错误，Scene 共享库与 Scene / Gate / Battle 节点退出 0。输出 Tee 到 `build/verification/persist-batch8-battle/game-sln.build.log`。
2. 再跑 bag_test：
   ```
   pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter bag_test -LogDir build/verification/persist-batch8-battle
   ```
   - 通过标准：上表 6 条都在列且通过；`PlayerBattleSettlementTest.*`、`PlayerBattleSettlementItemTest.*` 全绿；三个旧用例名不再出现。
   - 回归守卫（必须照旧通过）：
     - `PlayerBattleSettlementTest.InboxUnownedPetSkipsOnlyRetiredPetEffect`、`InboxMissingPetPoolSkipsOnlyThatPetEffect`：这两种仍只跳过这一只；
     - `PlayerBattleSettlementItemTest.InboxGlobalItemBlockSkipsOnlyTheBlockedDropAndAppliesTheRest`：封禁仍只剔除这一项；
     - `PlayerBattleSettlementItemTest.InboxUnknownConsumedInstanceCannotBeDowngradedToOrdinaryQuantity`：实例带未知字段仍判版本错位；
     - `PlayerBattleSettlementItemTest.InboxConsumesFromItsOwnHoldAndReleasesIt`。
   - 失败时保留 gtest 输出。不要为了变绿去改期望，先判断是不是这批改动的问题。
3. 本节没有改 proto、配表和 Go，不需要协议生成、导表和 Go 构建。§24「给 Codex」的 5 步仍然要跑，其中第 3 步的 bag_test 可以和上面第 2 步合并成一次。

**没验证的**

- 没有编译，没有跑任何用例。
- 没有走真实的热更入口（scene 节点收到配表更新后重新加载）。用例里的热更是直接调 `Load()`。
- 没有冒烟：新旧 scene 混跑时，一局带新物品的结算先延后、升级后到账，这条链路只有单测口径。

**Java 版（AGENTS §12）**：本轮不写 Java 代码。Java 仓 `PARITY.md` 混版本那一行的第 ③ 条（战斗结算查不到表的掉落 / 消耗整笔延后）11-11 已登记；11-12 把其中「mmorpg 也还没改」改成已改，并补了宝宝种类同口径。

**本节文件清单**

- 已随 `4edb25640` 入库：`cpp/libs/services/scene/battle/system/player_battle.{h,cpp}` 的物品部分。
- main 工作区，未提交：
  - `cpp/libs/services/scene/battle/system/player_battle.{h,cpp}`（宝宝部分）；
  - `cpp/tests/bag_test/player_battle_settlement_test.cpp`；
  - `docs/design/mixed-version-persist-field-protection.md`、`docs/design/battle-result-recovery.md`、`docs/ops/player-inbox-runbook.md`；
  - 本文、`docs/PROGRESS.md`。
- Java 对账本：`D:\luyuan\xuanming-server-mmo\PARITY.md`。

## 26. 续：旧版本可以接手更新世代的存档，正文按存盘直接提交的路径改写（2026-11-12，「玩家数据并发覆盖与版本控制」会话）

- **改了什么**：go/db 认领不再因「存档世代比 scene 新」拒绝；scene 把行里不认识的顶层字段按行保管、写这一行时接回去，更新版本才有的行不读不写；空行释放不判世代倒退，写权行的世代只升不降。设计稿正文 §0～§2、§3.3、§4.2、§5～§8、§10、§13、§14 按现行路径改写，§15 v0.15 记了取舍。
- **Codex 命令、通过标准、文件清单**：见 `docs/PROGRESS.md` 2026-11-12「旧版本可以接手更新世代的存档(混版本持久字段保护 v0.15)」条目。
- **没验证的**：没有编译、没有跑用例；真实的新旧版本混跑从没做过（设计 §10）。
- **Java 版**：待做，Java 仓 `PARITY.md` 混版本那一行末尾已补登。
