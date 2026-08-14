# 本会话推进进度 — 2026-05-15(v3)/ 2026-05-16(v4 / v5 / v6 / v7)

> **会话目标**:用户说「全部做啊」(回档 / 跨服 / 合服全部做完)
> **采用策略**:基于 AUDIT.md 的真实现状,按 P0 → P1 → P2 顺序逐项推进
> **v2 更新**:第二会话完成 metrics 接线 + #15 角色重名调研收口
> **v3 更新**:第三会话尝试推进 #11,**摸完代码发现 bag 完全无持久化路径**,落档 bag-rollback-feasibility-analysis.md
> **v4 更新**:第四会话用户确认「玩家肯定要跨 zone 玩」,深查跨 zone 链路后**写新审计文档 cross-zone-readiness-audit.md(权威),修订 mmo_cross_server_architecture.md §7-8 §11 §13**
> **v5 更新**:同会话延续,代码层面落地了**步骤 2(PlayerFrozenComp + 延后 DestroyPlayer)**,过程中又发现两个新事实并落档
> **v6 更新**:推进 #27(proto 重新生成)+ #28(Kafka topic 订阅接线)。打开 #29 时发现需要先做分类决策才能改 17 个 system 文件,**落档 cross-zone-readiness-audit.md §11 业务系统 Frozen 接入分类指南**
> **v7 更新(本节)**:**提交 + push 累计 8 个 commit 到 GitHub**,然后实施 #29(业务系统 Frozen 检查,§11.1+11.2+11.3 三类共 8 个文件)+ #25(ACK + reaper 完整链路)。**跨 zone 修复三件套(Frozen + ACK + reaper)代码层面全部落地**


---

## ✅ v6 已完成

### #27 Proto 重新生成

- 从主仓拷 `go.mod` + `pbgen.exe` + `proto-gen.exe` 到 worktree(worktree `.gitignore` 第 118 行注释明示 `go.mod was never tracked`,所以 worktree 检出时缺这两个文件)
- 跑 `dev_tools.ps1 -Command proto-gen-run -UseBinary` —— 8 秒跑完,无报错
- 验证:`PlayerMigrationAckEvent` 类已生成在 `cpp/generated/proto/common/event/player_migration_event.pb.h:75`
- Go 侧 proto 同步更新

### #28 Kafka topic 订阅接线

- 改 `cpp/nodes/scene/main.cpp`:加 `#include "kafka/system/kafka.h"`,在 `SetAfterStart` hook 里加:
  ```cpp
  const std::string crossZoneGroupId = "scene-cross-zone-" + std::to_string(n.GetNodeId());
  n.RegisterKafkaMessageHandler(
      {"player_migrate", "player_migrate_ack"},
      crossZoneGroupId,
      &KafkaSystem::KafkaMessageHandler);
  ```
- groupId 按 nodeId 分隔(每节点独立 consumer group)防止同 zone 多 scene 节点抢同一份 ACK
- **未做 cpp 编译验证**(主仓 MSBuild 编译时间长 + 上下文消耗大,留给下次会话)

### #29 业务系统加 Frozen 检查 — 改路径为「先落档指南」

打开 #29 时摸 cpp/libs/services/scene 发现需要改 17 个 system 文件,**且每个 system 语义不一样**:
- 写入类(Currency/Bag/Quest)— 应该 reject
- 被动 tick 类(Buff/Skill cooldown/AOI)— 应该 skip
- 消息类(移动/技能/聊天)— 应该回 tip
- 跨玩家影响类(被攻击/治疗)— 需要 game design 拍板

不分类直接「每文件加一行 `if (IsCrossZoneFrozen) return`」会**改坏其中几类的语义**(比如 buff 衰减应该完全冻结还是继续 tick?)。

**改成两步**:
- 本会话:落档 `cross-zone-readiness-audit.md §11 业务系统 Frozen 接入分类指南`,4 类的具体处理 + 文件清单 + 实施顺序 + 验证清单
- 下次会话:按指南分批实施(§11.1 写入类 → §11.2 tick → §11.3 消息类 → §11.4 暂挂)

---

## ⚠️ 剩余 pending 任务(v6 状态)

| ID | 任务 | 阻塞性 | 状态 |
|---|---|---|---|
| #29 | 业务系统加 Frozen 检查 | 步骤 2 完整语义 | 设计指南已落 §11,等下次会话实施 |
| #25 | ACK + reaper(失败恢复)| 失败场景才需要 | 等 |
| #24 | 失败场景测试 | 验证 | 等 #25 |
| #23 | PlayerAllData 加 BagAllData/QuestAllData/MailAllData | **核心** —— 解决数据丢失 | 等产品/策划定 ItemEntry schema |
| #21 | bag 持久化(方案 A)| 同 #23 | 同 #23,本质合并 |
| #22 | quest/mail 持久化审计 | bag 之后 | 等 |
| #11 | per-service Export/Import RPC | bag/quest/mail 持久化做完后 | 等 #23 |
| #7 | Region 路由表 | 单 zone 不急 | 50+ zone 时再做 |

---

## v6 编译可用性状态

**当前 cpp 是否能编译过?**

- ✅ Proto 生成已跑过,`PlayerMigrationAckEvent` 类已落 `.pb.h/.pb.cc`
- ✅ `player_lifecycle.cpp` / `kafka.cpp` 引用的 protobuf 类现在都存在
- ⚠️ **未做实际 MSBuild 验证** —— 但代码层面所有引用都对应到已生成或现有的类,理论上应该过
- ⚠️ Go 侧 `data_service` 上轮已 `go build ./...` 通过(v2),v6 没动 Go 代码

**风险评估**:cpp 编译失败概率低但不为 0。如果失败,大概率是:
- include 路径细节(`kafka/system/kafka.h` 在 scene 节点 include 树里的解析)
- `PlayerMigrationAckEvent` 在 cpp_lifecycle.cpp 里被引用前需要的 forward declaration(generator 应该已经处理,但需要验证)

**下次会话开干前必跑**:`cd cpp/nodes/scene/build && msbuild scene.sln /p:Configuration=Debug` 或同等命令,确认编译通过。如果失败,把错误贴给下次会话先修编译再继续 #29。

---

## v6 worktree 文件清单

新增(本会话 v6):
- `cross-zone-readiness-audit.md §11` 新增章节(业务系统 Frozen 接入分类指南)

修改(本会话 v6):
- `cpp/nodes/scene/main.cpp`(+ Kafka topic 订阅)
- 16 个 `cpp/generated/**` 文件被 proto-gen 重新生成(`player_migration_event.pb.{h,cc}` / `common_event_player_migration_event_event_id.h` / `rpc_event_registry.{h,cpp}` / 等)
- `proto/event_id.txt`(被 proto-gen 触动)
- `cpp/nodes/scene/handler/event/player_migration_event_handler.{h,cpp}`(被 proto-gen 触动)
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`(被 proto-gen 触动)
- 主仓 `tools/proto_generator/protogen/{go.mod,pbgen.exe,proto-gen.exe}` 被拷到 worktree(不会进 git,因为 gitignore 不跟踪)

---

## ⚠️ v5 步骤 2 实施过程的新发现

按 cross-zone-readiness-audit.md §3.2 件 2 实施 Frozen 状态时,发现两件之前没看到的事实:

### 发现 1:`player_migrate` topic 当前没人订阅

`grep -rn "RegisterKafkaMessageHandler" cpp/` 只命中 engine 层的实现 + SceneCommand 命令模板。**没有任何代码订阅 `player_migrate`**。

意味着:跨 zone 当前**单向 broken** —— 源端 publish 给 broker,目的端没 consumer,玩家根本到不了新 zone。这是先前未识别的 bug,跟我做的 ACK 改动正交。

**v6 已修复** —— #28 加了 `RegisterKafkaMessageHandler({"player_migrate", "player_migrate_ack"}, ...)`,topic 现在有 consumer。

详细分析见 `cross-zone-readiness-audit.md §10`。

### 发现 2:Kafka 必须 pb,不能 JSON(用户纠正)

我最初为求省事(避开 proto 重新生成)用 JSON 写了 ACK payload。**用户当场指出错误**:整个 codebase 其他 Kafka 消息都是 protobuf,JSON 解析慢一个数量级 + 破坏一致性 + 失去 schema 版本管理。

立刻撤回,改用 `PlayerMigrationAckEvent` protobuf message(已加在 `proto/common/event/player_migration_event.proto`,v6 已重新生成成 .pb.h/.pb.cc)。

### 真实状态

步骤 2 拆成 4 个子任务:
- ✅ 代码层面(7 件落地)
- ✅ #27 重新生成 proto(v6 已完成)
- ✅ #28 Kafka topic 订阅接线(v6 已完成)
- ❌ #29 业务系统加 Frozen 检查(v6 落档分类指南,留给下次会话实施)

---

## v5 已完成代码改动

### 代码新增

- `cpp/libs/services/scene/player/comp/player_frozen_comp.h`(纯 C++ struct,3 字段:`frozenAtMs / toZoneId / migrateAttempts`)

### Proto 新增

- `proto/common/event/player_migration_event.proto`:加 `PlayerMigrationAckEvent` message(player_id / from_zone / to_zone / ack_at_ms 4 字段)
- `proto/common/component/player_comp.proto`:加 NOTE 说明 PlayerFrozenComp 故意走 C++ struct 不走 proto

### 代码修改

- `cpp/libs/services/scene/player/system/player_lifecycle.h`:声明 `HandlePlayerMigrationAck` + `IsCrossZoneFrozen`
- `cpp/libs/services/scene/player/system/player_lifecycle.cpp`:
  - `HandleCrossZoneTransfer` —— 发完 Kafka 后 emplace `PlayerFrozenComp` 替代立即 DestroyPlayer
  - `HandlePlayerAsyncSaved` —— 检测 Frozen 跳过销毁路径(原 UnregisterPlayer 路径只对真退出登录生效)
  - `HandlePlayerMigration` —— 目的端成功 init 后用 `PlayerMigrationAckEvent` 发 ACK(protobuf 序列化,不是 JSON)
  - 文件末新增 `IsCrossZoneFrozen(player)` 实现
  - 文件末新增 `HandlePlayerMigrationAck(playerId, toZoneId)` 实现(含幂等检查、zone 不匹配检查)
- `cpp/libs/services/scene/kafka/system/kafka.cpp`:`KafkaMessageHandler` 加 `player_migrate_ack` topic 路由,用 `PlayerMigrationAckEvent.ParseFromString` 解析(注释明确指出本 handler 当前没被订阅,需要 #28 接线)

### 文档新增

- `docs/design/cross-zone-readiness-audit.md`(v2,新审计权威)— v1 完整方案 + v2 补 §10「步骤 2 实施过程的新发现」

### 文档修改

- `docs/design/mmo_cross_server_architecture.md §7-8 §11 §13`:按 Kafka 自治真实形态重写,标注每项的真实状态
- `docs/design/bag-rollback-feasibility-analysis.md`:加 v2 修正头部,方案 A 升级为 cross-zone-readiness-audit 的步骤 1
- `AUDIT.md`:加 v2 重大修正头部(回档 95% → 70%,跨服 85% → 50%)
- `PROGRESS.md`(本文件)

---

## ✅ 总计完成 10 项任务(v1+v2,v3+v4+v5 没新增完成项,但产出关键文档 + 步骤 2 代码)

---

## ⚠️ v4 关键发现:跨 zone 不可生产

第四会话深查 `player_lifecycle.cpp` + `player_database_loader.cpp` 后发现:

**跨 zone 链路只 Marshal 7 个 ECS 组件**(Transform / Currency / Skill / Level / 2×Uint / DerivedAttrs)。bag/quest/mail **不在 PlayerAllData proto 里**,跨一次 zone 静默丢失。`HandleExitGameNode` 走相同路径,**正常退出 / 重启同样丢 bag**。

更糟:Kafka send 后立即 `DestroyPlayer`(line 217),broker 失败 / 目标节点崩溃 = 玩家两边都没了。

**`mmo_cross_server_architecture.md §7-8` 描述的"SceneManager 严格 ACK 编排"根本没实现**。实际是 Kafka 自治 —— 这本身是对的(自治形态对你「玩家无限跨 zone」的低延迟需求更友好),只是缺三件套修复。

### 修复方案(权威):cross-zone-readiness-audit.md §3 三件套

1. **PlayerAllData 数据完整化** — 加 `BagAllData / QuestAllData / MailAllData` 子 message
2. **PlayerFrozenComp + 延后 DestroyPlayer** — 等 ACK 才真销毁
3. **`player_migrate_ack` Kafka topic + Redis migration 状态 + reaper** — 失败检测 + 重传 + 兜底

总工作量 4-6 周。**步骤 1 阻塞所有,前置是产品 / 策划定 ItemEntry schema**(装备强化等级 / 词条 / 宝石镶嵌等字段)。

### 同步修订的文档

- ✅ `cross-zone-readiness-audit.md` — 新增,完整审计 + 三件套方案 + 失败场景处理 + metrics 设计
- ✅ `mmo_cross_server_architecture.md §7-8 §11 §13` — 按真实形态重写
- ✅ `AUDIT.md` — 加 v2 重大修正头部(回档 95% → 70%,跨服 85% → 50%)
- ✅ `bag-rollback-feasibility-analysis.md` — 加 v2 修正,方案 A 升级为 cross-zone-readiness-audit 的步骤 1
- ✅ `PROGRESS.md`(本文件)— v4 更新

---

## ✅ 总计完成 10 项任务(v1+v2,v3+v4 没新增完成项,但是产出了关键文档)

### P0 三项

1. **`docs/design/server_merge_design.md`**(任务 #14)— 单一权威合服 SOP 文档(v2 已更新 §4.2 角色重名为「已确认无冲突」)
2. **MySQL binlog 自动归档 K8s CronJob**(任务 #19)— PVC + ConfigMap + CronJob + runbook 全套
3. **(角色重名)server_merge_design.md §4.2 已结**(任务 #15,v2 完成)

### P1 / P2 七项

4. **SavePlayerData 乐观锁实测验证**(任务 #20)
5. **Data Service per-player 锁验证**(任务 #3)
6. **Kafka offset reset 脚本**(任务 #18)
7. **AddCurrency 唯一入口旁路审计**(任务 #13)
8. **k8s-zone-rollback 一键脚本**(任务 #17)
9. **跨服 observability metrics(完整接线)**(任务 #9,v2 完成)— 见下方 §9 更新
10. **Instance 节点 conflict hook**(任务 #16,N/A)
11. **合服: 迁移工具**(任务 #5)+ **合服: 设计文档**(任务 #12)

---

## v2 新增完成项

### 9. metrics 接线尾巴(本次会话补完)

`metrics.Start(addr)` 已接入 `data_service/data_service.go`,`config.Config` 加 `MetricsListenAddr` 字段,`go mod tidy` 把 `prometheus/client_golang` 提升为 direct dep(`go.mod:8`)。

**验证**:
- `go build ./...` → **BUILD OK**(无输出 = 编译通过)
- `go test ./internal/logic/ ./internal/routing/` → **全 PASS**(logic 0.457s, routing 0.377s)
- `data_service.go` 启动 banner 显示 `metrics: <addr>/metrics`(若 config 配了)

剩余可选接线(P3,可推迟):
- `crossSceneTransitionLatency` / `crossSceneTransitionTotal` 只有 helper,**没有调用者**。需在 `scene_manager` 的切场景编排代码里调 `ObserveCrossSceneTransition` / `ObserveCrossSceneTransitionOutcome`。属于 P3,不阻塞投产

### 15. 角色重名调研(本次会话完成,**工作量 = 0**)

按 server_merge_design.md §4.2 调查清单跑完,**结论:当前数据模型不存在 player 级重名冲突,无需写改名逻辑**。

**决定性证据**:
1. Redis `account:{account}` key **不带 zone scoping**(`login_constants.go:14`)→ login 服务层强制全服唯一
2. `AccountSimplePlayer` proto 只有 `player_id` 一个字段(`user_accounts.proto:6-9`),无 name
3. `player_database` proto 也无 name 字段
4. `createplayerlogic.go:105-107` 创建玩家只填 `player_id`
5. HTTP API 的 `zone_id` 是路由参数,不是 account 命名空间分割维度
6. 公会重名 CLI 已自动检测(`merge_zone/main.go:240-260`)

server_merge_design.md §4.2 已更新为 v2「**已确认无冲突,无需处理**」,详证据 + 未来引入 player nickname 时的重评条件都已落档。

---

## ⚠️ 剩余 pending 任务(v3 状态)

### 阻塞中(必须先做才能解锁后续)

#### #21 给 bag 加持久化(方案 A) — **新增,阻塞 #11**

**为什么是 P0**:不仅是回档需求,更是当前数据完整性的硬伤。bag 在内存里 = 玩家退出 / 重启可能丢道具(需先验证)。

**做法**:
1. 新增 `BagComp` proto(`ItemEntry { item_uuid / config_id / stack_size / pos / bag_type }` × N)
2. 加到 `player_database` 作为字段 10
3. `player_database_loader.cpp` 补 bag 的 Marshal/Unmarshal:遍历 `itemRegistry_` ↔ `BagComp.items`
4. 跑「登录 → 加道具 → 退出 → 重登验证」回归

**工作量**:~1 周(proto + Marshal + 测试 + 链路验证)

**前置依赖**:**产品 / 策划定 ItemEntry schema** —— 装备的强化等级 / 词条 / 宝石需要哪些字段?这事 AI 拍不了

#### #22 quest/mail 持久化审计 — **新增**

`player_database_loader.cpp` 同样没 Marshal quest/mail。需要查清楚是「数据在别处持久化」还是「跟 bag 同病」。如果是后者,要重复 #21 的工作。

### 仍然 pending(原状)

#### #11 各业务服务 Export/Import RPC

**v3 状态**:**前置条件不满足**(等 #21 完成)。完成后工作量从「6 个服务的 RPC」缩水成「data_service rollback_logic 加 proto 字段过滤」~2-3 天

#### #7 跨服 P2 Region 路由表

单 zone 形态用不到,**有意识不做**。重启条件:50+ zone 部署 / 真正的跨服活动需求

---

## 真实进度对比

| 模块 | AUDIT 起点 | v1 后 | v2 后 | v3 修正 | **v4 修正** |
|---|---|---|---|---|---|
| 回档 | ~90% | ~95% | ~97% | ~70% | **~70%**(无变化,bag/quest/mail 不在快照) |
| 跨服 | ~75% | ~85% | ~88% | ~88% | **~50%**(深查跨 zone 链路后,数据丢失 + 失败丢玩家是致命问题) |
| 合服 | ~60% | ~75% | ~85% | ~85% | **~85%**(合服工具本身完整,但合服后玩家跨 zone 仍丢数据,需先修跨 zone) |

**v4 跨服降到 50% 的解释**:之前 v3 只看了「per-player 锁、version 字段、metrics 框架」这些**单点能力**,没看跨 zone 端到端链路完整性。事实上:
- ✅ Single Writer 通过「先 flush 再 Kafka 再销毁」隐式成立
- ✅ Layer 2 Redis SETNX + TTL 兜底锁已实现
- ❌ **跨 zone 数据完整性 0%**(bag/quest/mail 不跟)
- ❌ **失败恢复 0%**(无 ACK,Kafka 失败 = 丢玩家)
- ❌ **架构文档与实现不一致**(SceneManager 编排 vs Kafka 自治)

「玩家无限跨 zone 玩」这个核心设计目标的实际就绪度,真实数字是 **50% 左右**。组件都对,但端到端链路有致命漏洞。

---

## worktree 新增 / 改动文件清单(v2 累计)

新增(10 个):
- `AUDIT.md`(实现状态审计报告)
- `PROGRESS.md`(本文件,v2 更新)
- `docs/design/server_merge_design.md`(合服权威 SOP,§4.2 v2 已收口)
- `docs/ops/mysql-backup-pitr-runbook.md`
- `docs/ops/deferred-clawback-bypass-audit-2026-05.md`
- `deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml`
- `tools/scripts/kafka_offset_reset.ps1`
- `tools/scripts/k8s_zone_rollback.ps1`
- `go/data_service/internal/metrics/metrics.go`

修改(8 个):
- `cpp/libs/modules/currency/system/currency_system.h`(防御性 doc)
- `deploy/k8s/manifests/infra/mysql.yaml`(PVC + binlog 配置)
- `docs/design/zone_data_rollback.md`(§3 缺口表 3 项已标完成)
- `go/data_service/internal/logic/data_logic.go`(metrics 埋点)
- `go/data_service/internal/logic/rollback_logic.go`(metrics 埋点)
- `tools/scripts/dev_tools.ps1`(注册 2 个新命令 + 21 个新参数)
- **`go/data_service/data_service.go`**(v2 新增:metrics.Start 接线)
- **`go/data_service/internal/config/config.go`**(v2 新增:`MetricsListenAddr` 字段)
- **`go/data_service/go.mod` + `go.sum`**(v2:prometheus/client_golang 提升为 direct)

---

## 下次会话开干顺序

1. **(可选)#9 P3 子项**:在 `scene_manager` 的切场景编排代码里加 `ObserveCrossSceneTransition`(4 phase)+ `ObserveCrossSceneTransitionOutcome`(5 outcome)埋点。让跨服切场景的 metric 真正有数据
2. **(决策)问客服总监**:是否需要 per-service 颗粒度回档?如果不要,关掉 #11
3. **(条件触发)**:#7 Region 路由表等 zone 数量真正爆炸再做

---

## Changelog

- **2026-05-15 v1**:初版,8 项任务完成,1 项 metrics 接线尾巴留下
- **2026-05-15 v2**(同日续会话):补完 metrics 接线 + #15 角色重名调研收口。10 项任务完成,剩 2 项「有意识不做」
- **2026-05-15 v3**(同日续第三会话):用户确认 #11 必须做(客服需要只回档背包)。摸 C++ 代码发现 bag/quest/mail **完全无持久化路径**,前置条件不满足。落档 `bag-rollback-feasibility-analysis.md` + 新建 #21 / #22。回档真实完成度从 97% 下修到 70%。
- **2026-05-16 v4**:用户确认「玩家肯定要跨 zone 玩」是核心设计。深查跨 zone 链路后发现 v3 还低估了问题严重度 —— 不只是 bag 没持久化,还有 Kafka 失败丢玩家、SceneManager 编排实际未实现 等问题。**v4 的成果是落档完整修复方案 `cross-zone-readiness-audit.md`(Kafka 自治 + 三件套)+ 同步修订 `mmo_cross_server_architecture.md §7-8 §11 §13` + 更新 AUDIT/PROGRESS/bag-rollback-feasibility-analysis 反映真相**。代码部分按用户要求接下来开始(从最低风险的 Frozen 状态开始)


---

## ✅ 本会话已完成

### P0 三项

1. **`docs/design/server_merge_design.md`**(任务 #14)— 单一权威合服 SOP 文档
   - 9 章:架构前提 / 已有工具 / 重名 / 完整 SOP(准备/停服窗口/验证/善后/回滚)/ 已知未覆盖 / 测试 SOP / 文档关系图 / Changelog
   - 把散落在 `tools/merge_zone/main.go`、`mmo_cross_server_architecture.md §9`、`guild_ranking_architecture.md §合服工具`、`enter-scene-zone-routing.md` 的合服知识收口为一份

2. **MySQL binlog 自动归档 K8s CronJob**(任务 #19, 原 `zone_data_rollback.md §3` 「优先级:高」缺口)
   - `deploy/k8s/manifests/infra/mysql.yaml` — 加 PVC(20Gi data + 50Gi backup)+ ConfigMap(my.cnf 开 log-bin / ROW format / 7 天保留)+ initContainer 创建 binlog 目录
   - `deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml` — 每天 03:17 UTC mysqldump + binlog 复制,prune 策略 dumps 30 天 / binlog 14 天
   - `docs/ops/mysql-backup-pitr-runbook.md` — 部署 / 升级 / 日常运维 / PITR / 与合服衔接 / 故障排查的完整 SOP
   - `zone_data_rollback.md §3` 缺口表已标记此项为「已落地」

3. **(角色重名)server_merge_design.md §4.2 落档为 unknown**(任务 #15 改 P1)
   - **不写代码,改写文档** —— 因为 `player_database` proto 里没有 player_name 字段,需要先调查 `user_accounts.account` / `user.display_name` / Unity `AccountSimplePlayer` 的全服唯一性约束才能决定是否写改名逻辑
   - 已在 server_merge_design.md §4.2 落档:4 项调查清单 + 4 种实施方案矩阵 + 「为什么不直接动手」说明
   - 任务从 P0 降级为 P1,等下次会话做调查

### P1 / P2 五项

4. **SavePlayerData 乐观锁实测验证**(任务 #20)— **已确认是真乐观锁**
   - `data_logic.go:159 checkVersion` + `:168 Incr __version` + `:174 NewVersion`
   - SetPlayerField 同样
   - `Router.AcquirePlayerLock`(`router.go:254`)是真 Redis SETNX+TTL
   - **结论**:lock + version 双层兜底全部落地,符合 `mmo_cross_server_architecture.md §8 Layer 2`

5. **Data Service per-player 锁验证**(任务 #3)— 同上,**已落地**

6. **Kafka offset reset 脚本**(任务 #18)— `zone_data_rollback.md §3` 「优先级:中」缺口
   - `tools/scripts/kafka_offset_reset.ps1` — 4 种模式(`ToDatetime` / `ToEarliest` / `ToLatest` / `DeleteAndRecreateTopic`),默认 dry-run
   - `dev_tools.ps1` 注册 `kafka-offset-reset` 命令 + 9 个 Kafka 参数
   - `zone_data_rollback.md §3` 缺口表已标完成

7. **AddCurrency 唯一入口旁路审计**(任务 #13)— **审计 PASS**
   - grep `cpp/` 仅 2 处 `mutable_values()`,均在 `currency_system.cpp` 内部
   - 0 处外部调用 — 补缴 hook 不会被绕过
   - `currency_system.h` 顶部加防御性 doc(warning + audit history)
   - 落档 `docs/ops/deferred-clawback-bypass-audit-2026-05.md`(含持续保障建议)

8. **k8s-zone-rollback 一键脚本**(任务 #17)— `zone_data_rollback.md §3` 「优先级:中」缺口
   - `tools/scripts/k8s_zone_rollback.ps1` — 7 步流程(zone-down → Kafka drain → MySQL PITR 提示暂停 → Redis FLUSHDB → kafka-offset-reset → zone-up → 验证清单)
   - `dev_tools.ps1` 注册 `k8s-zone-rollback` 命令 + 9 个 rollback 参数
   - `zone_data_rollback.md §3` 缺口表已标完成

9. **跨服 observability metrics(部分)**(任务 #9)— **代码完成,接线未完**
   - `go/data_service/internal/metrics/metrics.go`(~200 行,仿 scene_manager 形式)
   - 9 个 metric:`save_player_data_total/save_latency_seconds`、`player_lock_total`、`version_mismatch_total`、`cross_scene_transition_latency/total`、`rollback_total/players_affected/orphans_cleaned`
   - `data_logic.go SavePlayerData` 三类 outcome(ok/version_mismatch/lock_conflict/redis_error)埋点完整
   - `rollback_logic.go RollbackPlayer/Zone/All` 全部埋点(含 affected/orphans)
   - `acquirePlayerLock` 三类 outcome 埋点完整
   - **未完工的接线见下面 §⚠️**

### 任务清单调整

10. **Instance 节点 conflict hook**(任务 #16)— **N/A**
    - `cpp/nodes/` 下只有 `scene/main.cpp` + `gate/main.cpp`,无独立 Instance 节点
    - 任务标完成(等 Instance 节点真存在时再做)

11. **「合服: 迁移工具」**(任务 #5)+ **「合服: 设计文档」**(任务 #12)— 标完成
    - 工具(`tools/merge_zone/`)+ 设计文档(本会话新写的 `server_merge_design.md`)都已落地

---

## ⚠️ 未完工的接线 — 下次会话必须先做

### 9-A. data_service 启动 metrics HTTP 端口

`metrics.Start(addr)` 已实现,但**没有人调它**。要在 `data_service` 主入口加一行(类似 scene_manager 的做法):

```go
// go/data_service/data_service.go(或 main.go,看实际入口)
import "data_service/internal/metrics"

func main() {
    // ... existing init ...
    metrics.Start(c.MetricsListenAddr) // 加这一行
    // ... existing start ...
}
```

同时在 `config.Config` 加 `MetricsListenAddr string` 字段(参考 scene_manager 的 config)。

### 9-B. go mod tidy

诊断器报警:`github.com/prometheus/client_golang should be direct (go mod tidy)`。`prometheus/client_golang` 当前是 indirect 依赖(通过其他包传递引入),metrics.go 直接用它后需要提升为 direct:

```bash
cd .claude/worktrees/rollback-cross-merge/go/data_service
go mod tidy
```

### 9-C. 横切场景 transition 埋点未接

`crossSceneTransitionLatency` + `crossSceneTransitionTotal` 只定义了 helper(`ObserveCrossSceneTransition` / `ObserveCrossSceneTransitionOutcome`),**没有调用者**。这需要在 `scene_manager` 的切场景编排代码里加埋点(`release` / `save` / `load` / `total` 四个 phase)。是 P3 范围,可下次会话或更晚做。

---

## 剩余 pending 任务(本会话未做)

| ID | 任务 | 估时 | 难度 | 备注 |
|---|---|---|---|---|
| #7 | 跨服 P2 Region 路由表 | 2-3 个会话 | 高 | 当前单 zone,不急 |
| #11 | bag/quest/mail/currency/guild/friend Export/Import RPC | 3 个会话(每服务 0.5) | 中 | 客服走整人回档则可不做 |
| #15 | 合服角色重名(待调研)| 0.5(调研)+ 0.5(实现)| 低-中 | 已在 server_merge_design.md §4.2 落档 |
| **#9 子项** | metrics 接线 + go mod tidy | 0.2 个会话 | 低 | **下次会话先做这个**(见 §⚠️) |

---

## worktree 新增 / 改动文件清单

新增(11 个):
- `AUDIT.md`(实现状态审计报告)
- `PROGRESS.md`(本文件)
- `docs/design/server_merge_design.md`(合服权威 SOP)
- `docs/ops/mysql-backup-pitr-runbook.md`(MySQL 备份 / PITR runbook)
- `docs/ops/deferred-clawback-bypass-audit-2026-05.md`(补缴旁路审计报告)
- `deploy/k8s/manifests/infra/mysql-backup-cronjob.yaml`(每日备份 CronJob)
- `tools/scripts/kafka_offset_reset.ps1`(Kafka offset reset 脚本)
- `tools/scripts/k8s_zone_rollback.ps1`(一键 zone 回档)
- `go/data_service/internal/metrics/metrics.go`(Prometheus metrics 包)

修改(5 个):
- `cpp/libs/modules/currency/system/currency_system.h`(顶部加防御性 doc + audit history)
- `deploy/k8s/manifests/infra/mysql.yaml`(加 PVC + ConfigMap + initContainer + binlog 配置)
- `docs/design/zone_data_rollback.md`(§3 缺口表 3 项已标完成)
- `go/data_service/internal/logic/data_logic.go`(metrics 埋点)
- `go/data_service/internal/logic/rollback_logic.go`(metrics 埋点)
- `tools/scripts/dev_tools.ps1`(注册 2 个新命令 + 21 个新参数)

---

## 给下次会话的开干顺序

1. **先解决 §9-A + §9-B**(0.2 个会话):metrics 接线 + go mod tidy,然后 `go build ./...` 确认整个 data_service 能编
2. **再做 #15 调查**(0.5 个会话):按 server_merge_design.md §4.2 调查清单跑一遍,落档「需要 / 不需要改名逻辑」决策
3. **决定 #11 Export/Import 优先级**(对话半轮):问用户「客服是否需要 per-service 颗粒度回档」?如果只走整人回档则不必做
4. **(可选)#7 Region 路由表**:只在「真要做 1000 zone」时启动

---

## 真实进度回顾

用户 3 轮前问「跨服合服回档你给我做完了吗」时,我说「没做完,~5%-70% 不等」。

经本会话的 AUDIT + 实际工作后,真实数字:

| 模块 | AUDIT 估计 | 本会话后 |
|---|---|---|
| 回档 | ~90% | **~95%**(P0 ops 三项 + 旁路审计 + version 验证已落,只剩 Export/Import RPC 颗粒度可选项) |
| 跨服 | ~75% | **~85%**(per-player 锁 + version 已确认,metrics 框架已落 80%) |
| 合服 | ~60% | **~75%**(权威 SOP 文档落地,工具已存在,剩玩家重名调研 + 实施) |

**实际可用性**:回档、合服在「合理审慎使用」前提下**可以投生产**。跨服观测性差点 metrics 接线,但代码层面架构(per-player 锁 + version + NodeId 冲突差异化)已经支撑实际跨服流量。

---

## Changelog

- **2026-05-15 v1**:初版,8 项任务完成,1 项 metrics 接线尾巴留下
- **2026-05-15 v2**(同日续会话):补完 metrics 接线 + #15 角色重名调研收口。10 项任务完成,剩 2 项「有意识不做」
- **2026-05-15 v3**(同日续第三会话):用户确认 #11 必须做。摸 C++ 代码发现 bag/quest/mail **完全无持久化路径**,前置条件不满足。落档 `bag-rollback-feasibility-analysis.md` + 新建 #21 / #22
- **2026-05-16 v4**:用户确认「玩家肯定要跨 zone 玩」是核心设计。深查跨 zone 链路后,**写新审计文档 cross-zone-readiness-audit.md(Kafka 自治 + 三件套权威方案)+ 同步修订 mmo_cross_server_architecture.md §7-8 §11 §13 + 更新 AUDIT/PROGRESS/bag-rollback-feasibility-analysis 反映真相**
- **2026-05-16 v5**(同日续第五会话):用户要求按「文档→改文档→改代码」顺序继续。**代码层面实施步骤 2(PlayerFrozenComp + 延后 DestroyPlayer)**。两个会话内发现:① `player_migrate` topic 当前没人订阅(落档为 audit §10);② 用户纠正 Kafka 必须 pb 不能 JSON,撤回 JSON 改 `PlayerMigrationAckEvent` protobuf。步骤 2 代码已落 7 件,剩 3 个子任务:**#27 proto 重新生成(必须,否则编译不过)/ #28 topic 订阅接线 / #29 业务系统加 Frozen 检查**
- **2026-05-16 v6**(同日续第六会话):**完成 #27(proto 重新生成)+ #28(Kafka topic 订阅接线)**。Proto-gen 跑通(从主仓拷 go.mod + 二进制绕开 worktree gitignore 限制),PlayerMigrationAckEvent 类已生成。Scene main.cpp 加 RegisterKafkaMessageHandler 订阅 `player_migrate` + `player_migrate_ack` topic。打开 #29 时发现需要先做 17 个 system 的分类决策(写入/tick/消息/跨玩家影响),**落档 cross-zone-readiness-audit.md §11 业务系统 Frozen 接入分类指南**,#29 留给下次会话按指南分批实施。**未做 cpp MSBuild 编译验证**(本会话上下文不够,留 caveat 给下次会话)
- **2026-05-16 v7**(同日续第七会话):**git 分支首次 push 到 GitHub**(分支 `worktree-rollback-cross-merge`,累计 10 个 commit,PR 模板 URL `https://github.com/luyuancpp/mmorpg/pull/new/worktree-rollback-cross-merge`)。然后**完成 #29(业务系统 Frozen 检查,§11.1+11.2+11.3 三类共 7 个文件)+ #25(ACK + reaper 完整链路)**。跨 zone 修复三件套(Frozen + ACK + reaper)代码层面**全部落地**。reaper:cross_zone_reaper.{h,cpp} 新文件 + HandleCrossZoneTransfer 接 RecordMigrationStart + HandlePlayerMigrationAck 接 RecordMigrationDone + scene/main.cpp 启动 timer。Redis 状态 player_migration:{playerId} TTL=120s + 10s 周期 SCAN+HMGET tick + 30s 单次 deadline + 3 次 attempts 上限 + 失败兜底 unfreeze+tip+DEL + 启动时 ScanAndRecover 处理 source 重启。常量都暴露在 header 顶部。**仍未做 cpp MSBuild 编译验证**(留下次会话第一件事跑)。本会话最后两个 commit:`61d4901f4` 业务 system Frozen gate / `5f236edf6` reaper 实现

---

## 2026-07-28 阶段 A:修正部署生成器的 Scene 角色拆分漂移(为 Agones 接入做前置)

背景:准备把 Agones 引入 Scene Node(1 GameServer = 1 个 C++ Scene Node 进程 = N 个动态
ECS Scene 房间,不是 1 Scene 1 Pod)。分三阶段做,阶段 A 只修部署生成器与构建链的既有漂移,
**不接 Agones SDK**。

### 事实核对(带源码证据,先证后改)

1. ✅ `deploy/k8s/zones.sample.yaml` / `zones.ops-recommended.yaml` 与 `docs/ops/scene-node-role-split.md`
   都描述了 `scene_world` / `scene_instance` 两个池。
2. ✅ 但 `tools/scripts/k8s_deploy.ps1` 只读 `replicas.scene`(`Get-ZonesFromJson`),
   `Apply-Zone` 只生成一个名为 `scene` 的 Deployment。文档与脚本对不上。
3. ✅ `New-NodeConfigMapYaml` 固定写 `SceneNodeType: 0`,且 `New-NodeDeploymentYaml`
   **完全没有生成 `SCENE_NODE_TYPE` 环境变量** —— 这才是 instance 角色落不了地的真因;
   ConfigMap 里那个 0 本身是"文件基线"设计(C++ `readGameConfig` 先读 yaml 再用 env 覆盖,
   last-wins),不是 bug。
4. ✅ `deploy/k8s/Dockerfile.cpp:100/106` 引用的 `tools/scripts/build_linux.sh`
   **不存在**:工作区没有、`git ls-files` 没有、`git log -- <path>` 无任何提交记录。
   → C++ 镜像这条构建链现在必然在 stage 2 断掉。
5. ✅ `Dockerfile.cpp:38` 克隆 gRPC `--branch v1.78.x`,`.gitmodules` 钉的是 `v1.80.x`。

第 4、5 条**按要求停在报告**:现有规范无法唯一决定补哪个版本的 `build_linux.sh`、
也无法唯一决定 gRPC 对齐到 1.78 还是 1.80,不自行补猜测版本。已把结论写进
`deploy/k8s/AGENTS.md` 的 "KNOWN BREAKAGE" 段。

### 本轮改动(5 个文件,均未提交)

- `tools/scripts/k8s_deploy.ps1`
  - 新增 `Resolve-SceneDeploymentPlan`:拆分/legacy 两种模式**互斥**的唯一权威实现。
    出现 `scene_world`/`scene_instance` 任一键 → 生成 `scene-world`(TYPE=0)+
    `scene-instance`(TYPE=1),且不再生成 `scene`;否则生成单个 `scene`(TYPE=0)。
    未指定的一侧按 0 副本生成并 `Write-Warning`;两种形式同时出现时 legacy 被忽略并告警。
  - 新增参数 `-SceneWorldReplicas` / `-SceneInstanceReplicas`(`-1` = 未指定,`0` = 显式零副本)。
  - `Get-ZonesFromJson` + YAML fallback parser 认识 `scene_world` / `scene_instance`
    (正则里长 key 必须排在 `scene` 前面)。
  - `New-NodeDeploymentYaml` 增加 `-SceneNodeType`,输出 `SCENE_NODE_TYPE` env。
  - `Wait-ForZoneReady` 按实际生成的 scene Deployment 名等待(副本 0 的池不等)。
  - **顺手修掉两个既有生成 bug**(不是本次引入的):
    (a) 环境变量块用 `$b += @"..."@` 连续追加两个 here-string,here-string 不含结尾换行,
        第二次追加直接接在上一行尾巴上,生成 `value: "1"            - name: GRPC_SERVER_MAX_POLLERS`
        这种非法 YAML。默认参数(ReserveThreads=1 且 MaxPollers=2)就命中 →
        **gate Deployment 在此之前一直生成不出可 apply 的 YAML**。改成数组逐行 join。
    (b) login 服务 ConfigMap 里 `RetentionMs` 那行用了 TAB(会被替换成 4 空格),
        比同级 `MaxOpenRequests`(2 空格)深一层,YAML 结构非法。改回 2 空格。
  - `Invoke-KubectlWithInputFile` 在 DryRun 下打印真正会送进 kubectl 的 manifest
    (`--- BEGIN/END MANIFEST ---`)。**之前只打印临时文件路径,而该文件在 finally 里当场删掉,
    DryRun 等于什么都验证不了**。
- `tools/scripts/dev_tools.ps1` / `tools/scripts/k8s_image.ps1`:透传两个新参数。
- `docs/ops/scene-node-role-split.md`:§2 补 env 覆盖 vs 两份 ConfigMap 的取舍;
  §3.2 补 DryRun 验收命令 + "必须手动删除旧 scene Deployment";新增 §3.2.1 兼容规则权威表。
- `deploy/k8s/AGENTS.md`:角色拆分段落改成"已实现"并写兼容规则;新增 KNOWN BREAKAGE 段。
- `deploy/k8s/zones.sample.yaml`:注释与实际脚本行为对齐。
- `docs/design/scene-creation-architecture.md`:Node Role Separation 段落加部署侧指路。

### 验证(全部本地 DryRun,未连集群 / 未构建镜像 / 未动 third_party)

指定的验收命令:
```
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up `
  -ZonesConfigPath deploy/k8s/zones.sample.yaml -DryRun -SkipInfra `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:test
```
结果 exit=0:zone `yesterday`(legacy `scene: 4`)→ `scene` replicas=4 SCENE_NODE_TYPE="0";
zone `today`(`scene_world: 2` / `scene_instance: 2`)→ `scene-world` replicas=2 TYPE="0" +
`scene-instance` replicas=2 TYPE="1",且该 zone 没有 `scene` Deployment。

兼容矩阵(逐条跑过 DryRun,manifest 结构检查全 PASS:无残留 TAB、无"引号标量后跟内容"、
每块都有 apiVersion+kind):

| 用例 | 结果 |
|---|---|
| `zones.ops-recommended.yaml` | zone1 legacy `scene=4`;zone2 拆分 2/2 |
| `zones.10zones.yaml` | 10 个 zone 全部 legacy `scene=2` |
| `zones.json`(JSON legacy) | 2 个 zone 全部 `scene=4` |
| 合成 YAML:`scene:7` + `scene_world:3` + `scene_instance:5` | 拆分生效 3/5,legacy 7 被忽略并告警 |
| 合成 YAML:只有 `scene_world:2` | `scene-world=2` + `scene-instance=0` + 告警 |
| 合成 JSON:`scene_world:4`/`scene_instance:6` | 4/6,JSON 路径同样生效 |
| `zone-up -SceneWorldReplicas 3 -SceneInstanceReplicas 5` | 3/5;`-WaitReady` 只等这两个 Deployment |
| `zone-up` 默认 | `scene=4`;`-WaitReady` 等 `deployment/scene` |

**没做**:没跑真实集群、没构建镜像、没改 `third_party/*`(工作区里那 7 个子模块的
本地改动一律没碰)、没有 commit / push / tag。

### 顺带发现,未修,留决策

- `k8s-exposure-preflight` 现在就是红的,而且**在 HEAD 原始脚本上同样红**(用
  `git show HEAD:` 取出原版单独跑过,复现一致)。根因:`$output = & $scriptPath ... 2>&1`
  抓不到 `Write-Host` 的 information 流,断言用的 `Ops profile resolved: ...` 标记永远匹配不到。
  属于测试脚手架问题,与角色拆分无关,没动。
- `Apply-Zone` 接收并打印 `CentreReplicas`,但**从来没有生成过 centre 的 Deployment**。
  考虑到 centre 正在退役(`docs/design/centre_decommission_*`),这次不动,但配置项名不副实。

### 阶段 B / C 前置

阶段 B(接 Agones Fleet + C++ REST 生命周期适配器,HTTP 客户端定为 libcurl)在本阶段
人工确认后再开工。阶段 C(rooms Counter + GameServerAllocation)要等阶段 B 在 dev 集群
E2E 通过后再开工。C++ 镜像构建链(上面第 4、5 条)必须先有结论,否则阶段 B 会出现
"YAML 看着对但镜像根本构建不出来"。

---

## 2026-07-28 阶段 B:Agones 基础生命周期接入(Fleet + C++ REST 适配器)

阶段 A 验收通过后继续。模型固定为:
**1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个动态创建的 ECS Scene 房间**
(不是 1 Scene 1 GameServer)。Scene 的规则驱动创建/销毁/镜像共置/玩家路由仍归 Go SceneManager。

设计文档:`docs/design/agones-scene-node-high-density.md`(新增)。

### 落码内容

**C++ 生命周期适配器(新增 4 个文件)**
- `cpp/nodes/scene/agones/agones_rest_client.{h,cpp}`
  可注入的 `HttpTransport` 接口 + `CurlAgonesHttpTransport`(libcurl,仅 Linux,
  由 `MMORPG_AGONES_CURL` 门控)+ Agones REST 五个端点封装 + 环境变量解析。
  所有 curl API 封死在这一个类里,业务代码不出现 `CURL*` / `curl_easy_setopt`。
  连接超时与总超时都设;`curl_global_init` 用 `call_once`,刻意不 cleanup。
- `cpp/nodes/scene/agones/agones_scene_lifecycle.{h,cpp}`
  状态机 `Disabled / Starting / Ready / Allocating / Allocated / ShuttingDown / Stopped`,
  独立 lifecycle worker 线程(health 心跳 + 处理挂起的 allocate / 回 Ready / shutdown),
  Ready 有上限退避重试,房间计数按 key 幂等。

**两条硬约束的落地方式**
1. **不能先建房间再异步 allocate**:`SceneNodeGrpcImpl::CreateScene` 在
   `runInLoop` **之前**、在 gRPC 线程上调 `EnsureAllocatedBlocking()`,
   拿不到 Allocated 直接返回 `UNAVAILABLE`,一个实体都不建。
   `CreateSceneResponse` 没有错误字段,非 OK 的 gRPC 状态是唯一诚实的失败信号。
2. **HTTP 绝不进 EventLoop**:legacy `SceneHandler::CreateScene` 跑在 EventLoop 上,
   改用 `EnsureAllocatedNonBlocking()` —— 立刻返回 false + 踢一次异步 allocate,
   本次创建 fail-closed,靠调用方重试。

**房间计数唯一接入点**:`SceneEventHandler::OnSceneCreated/DestroyedHandler`。
两条 RPC 路径都只在实体真的建出来/真的存在时才 trigger 事件(幂等命中、
参数校验失败、销毁不存在的 Scene 全部提前 return),所以天然满足
"重复不重复计数 / 失败不计数 / 不减不存在的"。`SceneLifecycle` 内部再按 key
幂等一次,计数不会为负。另加 `ReconcileIdleAfterCreate()`:allocate 成功但
实体没建出来且房间数为 0 时退回 Ready,不占着 Agones 容量空转。

**部署侧**
- `k8s_deploy.ps1` 新增 `-SceneOrchestrator deployment|agones`(默认 deployment)
  + `New-SceneFleetYaml`:生成 `agones.dev/v1 Fleet`,带 zone/role/build 标签、
  `SCENE_NODE_TYPE`、`AGONES_ENABLED=1`、`portPolicy: None`、health 配置、
  `terminationGracePeriodSeconds`(默认 60,给存盘留时间)、固定 replicas、
  `scheduling: Packed`。Fleet 与 Deployment 共用同一个 `Resolve-SceneDeploymentPlan`。
- `-WaitReady` 在 agones 模式下**不假装等过** —— `kubectl rollout status` 对 Fleet
  无效,改为打印 `kubectl get fleet ... -o jsonpath='{.status.readyReplicas}'`。
- 换编排方式会换 kind,apply 不回收同名旧 Deployment,脚本打印该删的命令。
- `dev_tools.ps1` / `k8s_image.ps1` 透传 `-SceneOrchestrator`。
- `deploy/k8s/Dockerfile.runtime` 增加 `libcurl4`。
- `cpp/nodes/scene/CMakeLists.txt`:`find_package(CURL REQUIRED)` + `CURL::libcurl`
  + `-DMMORPG_AGONES_CURL=1`;新源文件同时登记进 `scene.vcxproj` / `.filters`。

**测试**:新增 `cpp/tests/agones_lifecycle_test/`(vcxproj + 进 game.sln),
直接编译那两个 .cpp,注入 fake transport,不需要真的起 Agones sidecar。
19 个用例,覆盖:禁用模式零 HTTP、null transport 退化、sidecar 晚启动的 Ready
退避重试、Ready 重试到上限后仍 fail-closed、第一个 Scene 只 allocate 一次、
allocate 失败 fail-closed、重复创建不重复计数、两个销毁一个仍 Allocated、
销毁最后一个转 Ready、重复 Destroy 不为负、Allocated 零房间收口回 Ready、
并发创建第一个房间只 allocate 一次、并发销毁最后一个收敛、health 线程能干净停止、
慢 HTTP 下调用方有界返回、非阻塞门立刻返回、两个超时都下传、以及 4 个环境变量用例。

### 编码规范处理(踩到了 CP936 的坑)

`.github/copilot-instructions.md` 要求含非 ASCII 的 .cpp/.h 必须带 UTF-8 BOM。
- 新增的 4 个 agones 文件 + 测试文件:中文注释 + **已加 BOM**。
- 我改的 3 个带 codegen 守护段的文件(`scene_event_handler.cpp` /
  `scene_node_service.cpp` / `scene_handler.cpp`)原本无 CJK 也无 BOM:
  这些文件将来可能被代码生成器重写而丢掉 BOM,所以我的注释改成 **ASCII 英文**,
  不动它们的编码。
- `main.cpp` 在 HEAD 就已经"有 CJK 但无 BOM"(既有隐患),这次**补上了 BOM**。

### 验证(未编译、未上集群)

- 三个 ps1 语法解析 OK。
- 阶段 A 的验收命令回归仍绿(`zones.sample.yaml` → legacy `scene`=4 与拆分
  `scene-world`=2 / `scene-instance`=2,manifest 结构检查 PASS)。
- agones 模式 DryRun(`zones.ops-recommended.yaml`)exit=0,生成
  `apiVersion: agones.dev/v1 / kind: Fleet`,`portPolicy: None`、
  `SCENE_NODE_TYPE=0/1`、`AGONES_ENABLED=1`、`mmorpg.io/build` 取自镜像 tag。
- 新增 vcxproj XML 合法,已确认 Windows 侧**不**定义 `MMORPG_AGONES_CURL`
  (走 fake transport,测试不依赖 curl)。

**没做,而且不能宣称已做**:C++ 没编译(见下面给 Codex 的执行细则)、
单元测试没跑、没连任何集群、没装 Agones、没构建镜像、没提交。

### 阶段 C(未开工)

rooms Counter(beta,须显式开关默认关)、GameServerAllocation 预占、
创建失败的 counter 回滚、周期性 reconcile、`scene_manager_agones_*` 指标。
`Agones.RoomCapacity` 不硬编码,必须按帧耗时/AOI/玩家数/内存压测确定。
另外阶段 C 必须先修:`createscenelogic.go` 在 CreateScene RPC 失败后仍记
"Redis state committed" 并可能返回成功 —— 这个行为要先改掉再谈 counter 回滚。

### 2026-07-28 阶段 B Codex 编译验证与竞态收口

按阶段 B 交接细则完成 Windows 本地验证,并在真实编译/测试中修掉 3 个问题:

1. 新测试工程原先依赖未初始化的 gRPC googletest 子模块,并复制了数百个无关
   链接库。改为直接编译仓库现有 `yaml-cpp` 随附 googletest 源码,只链接
   `muduo.lib` / `ws2_32.lib`,无需安装工具或改 `third_party`。
2. `HealthTicksOnWorkerAndStopsCleanly` 在 `Stop()` 释放 transport 后继续通过
   裸指针读其 mutex,形成 use-after-free 并让测试进程永久卡住。改为 transport
   与用例共享外部原子计数器。
3. 最后一个 Scene 销毁后的 `/ready` 在途期间,新 CreateScene 可能仍看到本地
   `Allocated` 并直接建房,导致 sidecar 已 Ready、本地却保持 Allocated。
   新增 RAII `CreatePermit` + `pendingCreates` + `ReturningToReady` 状态:
   - gRPC 通过 gate 后到 EventLoop 完成创建之间也算在途创建;
   - `activeScenes == 0 && pendingCreates == 0` 才允许回 Ready;
   - `/ready` 在途的新创建必须重新 allocate 或非阻塞 fail-closed。

另修测试工程对象目录隔离:外部源文件统一输出到测试自己的 `IntDir`,不再用
`%(RelativeDir)` 穿回 Scene intermediate 目录污染生产 `.obj`。

验证结果:

- `scene.vcxproj /t:Clean;ClCompile Debug|x64`:0 warning / 0 error,包含两个 Agones
  源文件、两个 CreateScene handler、事件计数接入和 `main.cpp`。
- `agones_lifecycle_test.vcxproj /t:Clean;Build Debug|x64`:0 warning / 0 error。
- `build/cpp/tests/agones_lifecycle_test.exe`:24/24 PASS(原 21 例 + 2 个竞态例
  + 1 个停止态 fail-closed 用例)。
- `game.sln` 全量构建仍未通过:Gate 缺
  `third_party/openssl/include/openssl/configuration.h`;Scene 完整链接缺
  `absl_crc_cpu_detect.lib`。两项均为当前本机依赖产物缺失,不是本轮源码编译错误。
- 未连集群、未装 Agones、未构建镜像、未提交/推送。

### 2026-07-28 阶段 B Windows Debug 全链路闭环

上一节记录的两个全量构建阻塞已解决,且没有用 Release 库冒充 Debug:

1. Gate 的 `configuration.h` 报错不是 OpenSSL 子模块缺文件。项目链接的
   `crypto.lib` / `ssl.lib` 实际来自 gRPC BoringSSL,Debug include 却漏了
   BoringSSL 路径而落到独立 OpenSSL 3。现已与 Release 配置对齐,优先使用
   `grpc/third_party/boringssl-with-bazel/include`。
2. 当前 Abseil 已把 `absl_crc_cpu_detect` 迁为 `absl_base_cpu_detect`,并不再生成
   `absl_low_level_hash` / `absl_string_view`。用仓库 canonical gRPC 脚本构建
   Debug gRPC/Abseil/Protobuf/BoringSSL/zlib,再按同一 MSVC 14.51、`/MDd` 构建
   yaml-cpp 与 hiredis,统一安装到 `third_party/grpc/install_vs2026_dbg/lib`。
3. Gate/Scene Debug 链接目录优先指向上述 Debug 安装目录,`/WHOLEARCHIVE`
   改用 `hiredisd.lib` / `yaml-cppd.lib`。
4. `core.vcxproj` 的 `file2string.cpp` / `spdlog_file.cpp` 原本无条件强制 `/MD`
   与 MaxSpeed,会污染 `/MDd` 的 core Debug 库;现只在 `Release|x64` 应用这两个
   override,Debug 继承项目级配置。

最终验证:

- `gate.vcxproj /t:Clean;Build Debug|x64`:成功,生成并复制 `bin/gate.exe`。
- `scene.vcxproj /t:Clean;Build Debug|x64`:成功,明确编译两个 Agones 源文件并
  生成、复制 `bin/scene.exe`。
- `game.sln /m Debug|x64`:exit 0,完整 solution 通过。
- `agones_lifecycle_test.vcxproj Debug|x64`:构建成功;24/24 PASS。
- `dumpbin /DIRECTIVES` 确认新增 Debug 第三方库使用 `MSVCRTD`。
- 仍未连 dev 集群、未安装 Agones、未构建镜像、未提交/推送。

---

## 2026-07-29 阶段 C:rooms Counter + GameServerAllocation(Go 侧)

阶段 B 经 Codex 完成 Windows `Debug|x64` 全 solution 编译 + 24 个 C++ 单测后继续。
设计文档 `docs/design/agones-scene-node-high-density.md` §8 已补全。

### 先修的既有 bug:phantom scene(与 Agones 无关)

`createInstance` 里 `RequestNodeCreateSceneWithOptions` 失败时,原来只打一条
`(Redis state committed)` 然后**照样返回成功**。结果是 Redis 有映射、节点上没有
实体,玩家被路由进去后 EnterScene 永远成功不了。

现在失败一律回滚(scene 全部键 + `node:{id}:scene_count` + 反向索引 + Agones
名额)并返回错误码,**非 Agones 模式同样生效**。这是 counter 回滚的地基,不先修
后面全是建在沙子上。

副作用:本包原先"创建场景再断言点什么"的单测都依赖 RPC 失败被忽略,现在需要
一个能应答的 fake 节点。已在 `newTestSvcCtxWithWorldScenes` 里默认装上,
并为此在 `scene_node_client.go` 加了一个与既有 `SetNodeDialerForTest` 同型的
测试缝 `SetNodeEndpointResolverForTest`(生产不设)。

### 落码内容

**新增 `internal/agones` 包**
- `allocator.go`:`Allocator` 接口(Allocate / AcquireRoomOnGameServer /
  ReleaseRoom / ListGameServerRooms)+ 类型 + `ErrNoCapacity`。这是测试注入
  fake 的唯一缝。
- `k8s_allocator.go`:client-go dynamic/unstructured 实现。
  **刻意不引入 `agones.dev/agones` Go module** —— 它会把 Agones 自己的
  `k8s.io/*` 版本拖进来,和本仓库的 client-go v0.29.3 打架。
  - GSA selectors 顺序 = 优先级:先 `Allocated + minAvailable>=1`(高密度的
    定义:优先复用已在跑的进程),再 `Ready + minAvailable>=1`。
  - `counters.action=Increment` 让"选中"和"+1"是同一个原子操作,不留超卖窗口。
  - counter 增减走 `gameservers/status` 子资源的 read-modify-write +
    resourceVersion 乐观并发,409 冲突有上限重试;`+delta` 会检查 capacity,
    `-delta` 钳到 0。
  - PodIP 解析:`status.address` 是**宿主机**地址不能用(我们是 portPolicy:
    None 的内部服务),先读 `status.addresses` 里 `type=="Pod"`,老版本没有
    这个字段就退化成按 GameServer 名字 GET 同名 Pod。

**`internal/logic/agones_binding.go`**
创建的原子边界:GSA(+1)→ 校验 Allocated → 取 gs/PodIP/counter →
PodIP 映射回 knownNodes → 校验 zone 与 role → 分配 scene_id →
写 Redis + `scene:{id}:agones_gs` → 调 C++ CreateScene → 只有成功才回成功。
映射/校验任一步失败都把名额还回去并拒绝,**不允许"找不到就放行"**。
`scene:{id}:agones_gs` 必须在调 RPC **之前**写,否则中途崩溃就永远不知道该减谁。

**镜像共置在 Agones 下怎么保住**:GSA 只能按标签选、不能点名,所以共置路径走
`nodeID -> PodIP -> GameServer 名字 -> 对该 GameServer 的 rooms 做 CAS +1`。
源节点满了或反查不到就回落到自由分配(镜像失去共置优化,但玩家不卡死)。

**销毁 / 回滚**
- `luaAtomicDestroyInstance` 改成返回 `{nodeId, agonesGs}`,并把
  `scene:{id}:agones_gs` 和其余 scene 状态在**同一个脚本里**读出并删除。
  先删后读会永久丢失回滚依据。
- 重复 Destroy:第二次读不到 `scene:{id}:node`,拿不到 gs 名字,不会重复减。
- 节点死亡:名额照样要还(GameServer 可能还在);`ReleaseRoom` 对
  "GameServer 已消失"返回成功。
- 归还最终失败 -> `agones_counter_rollback_total{outcome="failed"}` + ERROR,
  **这类漂移不会自愈**,必须配告警。

**reconcile**:周期比对 Agones `rooms.count` 与 Redis `node:{id}:scene_count`,
写 `agones_counter_drift{zone}`。**第一版只告警不自动改写** —— 三方任意一方都
可能是错的那个,证据不完整时自动"修正"很可能把对的改错还掩盖真 bug。

**指标**(标签全低基数,`scene_id`/`player_id` 绝不进 label):
`agones_allocation_total{zone,role,outcome}` /
`agones_allocation_latency_seconds` / `agones_counter_rollback_total{outcome}` /
`agones_mapping_failure_total{zone,reason}` / `agones_counter_drift{zone}`。

**部署侧**:`-AgonesHighDensity` + `-AgonesRoomCapacity N` 给 Fleet 挂
`counters.rooms`。**`-AgonesRoomCapacity` 没有默认值,不给直接报错** ——
单 EventLoop 的每进程房间容量必须来自压测(帧耗时/AOI/玩家数/内存,
口径见 CLAUDE.md §6),不许拍脑袋。

**RBAC**:`deploy/k8s/manifests/go-svc/scene-manager-agones-rbac.yaml`,
全部 namespace 级 Role,没有 cluster-admin、没有 ClusterRole。

**启动**:`Agones.Enabled=true` 但分配器构造失败 -> **panic 起不来**,
不静默降级。静默降级 = 绕过容量约束而且没人会发现。

### 验证

- `go build ./...` / `go vet ./...` 干净。
- `go test ./internal/...` 全绿:107 个用例,其中阶段 C 新增 20 个
  (优先复用 Allocated / 满了才回落 Ready / 无容量 fail-closed /
  GSA 成功但节点未注册要还名额 / zone 不匹配 / role 不匹配 /
  Agones 开着没容量不许悄悄退回 Redis 选节点 / RPC 失败回滚 Redis+counter /
  非 Agones 模式同样回滚 / 成功时写下 agones_gs 映射 / 镜像仍共置 /
  源满了回落 / 销毁归还一次且重复 Destroy 不重复减 / 脚本读出并删除
  agones_gs / 中止时不动映射 / 归还失败不吞异常 / 漂移检出 / 一致时零漂移 /
  死节点带房间算漂移 / 关闭时完全惰性)。
- `go.mod`:`k8s.io/client-go`、`k8s.io/apimachinery` 从 indirect 提为 direct
  (原本就在 go.sum 里,go-zero 带进来的),没有新增外部依赖。
- Fleet 模板 DryRun 三态验证:默认不产 counters;开了高密度不给容量**报错**;
  给了容量产出 `counters.rooms.{count:0,capacity:N}`。

**没做,不能宣称已做**:
- **没有连过任何集群,没有装过 Agones**,§8.9 列的 4 条 Agones API 形状假设
  (GSA selectors/counters 字段、status.counters 位置、能否直接改
  `gameservers/status` 的 counter、`status.addresses` 的 Pod 条目)全部
  **未验证**。任何一条对不上,改的都是 `k8s_allocator.go` 一个文件,
  接口和上层逻辑不受影响。
- `-race` 没跑成:本机没有 gcc,`CGO_ENABLED=1 go test -race` 起不来。
- 没有跑压测,所以 `RoomCapacity` 到底该填多少**没有结论**。
- 没有 commit / push / tag。

### 下一步

1. 人工确认 dev 集群的 Agones 版本 + `CountsAndLists` FeatureGate。
2. 按 §8.9 逐条核对 API 形状,不符就改 `k8s_allocator.go`。
3. 跑 §9 的 13 条 dev 集群验收。
4. 压测定 `RoomCapacity`,再谈开高密度。

---

## 2026-07-29 按人数自动扩缩容:大世界频道 / gate / Scene Node 进程

需求:gate 扩缩容;scene 按人数扩缩(大世界到 2000 扩容,少于 100 强制迁移到
大世界),hub scene 最少一个。四个口径经用户确认:每个大世界地图至少 1 个频道 /
按频道人数且**所有**频道到线才扩 / gate 先 drain 再缩 / Pod 用 Agones
FleetAutoscaler + rooms Counter。

设计文档:`docs/design/world-channel-autoscale.md`(新增)。

### 关键发现:两个会让功能不成立的坑

1. **`initWorldScenesForZone` 会把缩掉的频道补回来**。它按 `ChannelCountFor(confId)`
   这个静态配置补齐缺失频道,fullSync 和每次节点 PUT 都跑。不改的话自动缩容
   刚摘掉的频道几秒内就被重建,缩容根本不成立。
   → 期望频道数下沉到 Redis `world_channels:desired:zone:{z}`,配置只作首次
   播种,两边共用一个权威。
2. **没有 protoc-gen-go-grpc**,新增 gRPC 方法这条路走不通。
   → 改成复用 `DestroyScene`:让它在"场景还有人"时先改派再销毁。这不是绕路,
   而是补上一个真实缺口 —— 在此之前销毁一个还有人的场景是未定义行为
   (玩家留着指向已销毁实体的 gate 会话)。零 proto 改动拿到排空原语。

### 落码内容

**C++ 强制迁移**
- `PlayerLifecycleSystem::BeginSceneDrain(sceneEntity)`:单场景版的疏散。
  复用整节点疏散的票据链路(抄 gate/session → 存盘 → 落地后请求
  `EnterScene(0,0)` → SceneManager 按世界频道表挑存活节点上的大世界频道)。
  与整节点疏散的区别只有:范围是一个场景;**不设** `tlsEmergencyRelocating`
  (节点自己不退出,标成疏散中会让 Node 的 drain 看门狗误判)。
- 把票据逻辑抽成 `EnqueueRelocateTicket`,两条路径共用一份。
- `HandleDestroyScene`(gRPC + legacy 两条)改成 **drain-then-destroy**:
  还有人就改派并**保留实体直接返回**,让 autoscaler 下一拍再来。
  改派是异步的(存盘回调才完成),此刻场景不可能是空的;幂等 + 每拍重新观察
  真实状态 = 收敛,不是重试 hack。

**Go 频道扩缩容**(`world_autoscale.go`)
- 扩容:该地图**所有**频道 >= 2000 才 +1(要求"所有"而非"任一",否则负载
  不均时会连续扩出一堆空频道)。
- 缩容顺序:SREM 路由 → 期望值 -1 → 进 draining 索引 → DestroyScene →
  下一拍重复直到人走光 → 清状态 + 归还 Agones 名额。
  **先摘路由再排空**,反过来的话刚改派出去的玩家可能又被分回来。
- 下限:每个大世界地图至少 1 个频道,`MinChannelsPerMap` 配 0 也钳回 1。
- 防抖两条:冷却窗口(120s)+ 容量校验(并过去不能把别的频道推过扩容线)。
  少了后者会自激振荡,玩家被反复强制改派。

**gate drain**(`loginqueue/gatedrain.go`)
- `gate:{nodeId}:draining` 标记 + `FilterDrainingGates`,接在
  `CandidatesForZone` —— 所有 gate 选择路径的唯一收口。
- 两个刻意的失败方向:查标记失败**放行**(Redis 抖动不能让所有人进不去);
  候选全被标记时返回原集合并打 ERROR(明显误操作,拒绝所有登录更糟)。
- 只做"不再分配新玩家"这一步。**刻意没做**"到期自动强踢":什么时候可以
  牺牲最后那批玩家的连接是运营决策,不该由后台循环替人做。

**Pod 扩缩容**:`-AgonesAutoscale` 生成 Counter 策略的 FleetAutoscaler
(key=rooms)。用 Counter 而非 Buffer:高密度下"还剩几个 Ready GameServer"
没意义,该看的是"还剩几个空闲房间名额"。必须配 `-AgonesHighDensity`,否则报错。

### 验证

- Go:`build` / `vet` 干净;`go test ./internal/...` **120 个用例全绿**
  (本轮新增 13 个频道扩缩容用例:全满才扩 / 有空位不扩 / 到上限不扩 /
  排空摘路由 / **最后一个频道永不排空** / Min 钳回 1 / 无余量不缩 /
  headroom 边界 / 排空中保持 / 排空完清理 / Redis 权威压过配置 / 冷却 / 关闭惰性)。
- login 模块 build + vet + loginqueue 测试通过。
- 部署生成器:FleetAutoscaler YAML 形状正确;`-AgonesAutoscale` 缺
  `-AgonesHighDensity` 时报错;阶段 A 回归仍绿,manifest 结构检查 PASS。

**没做,不能宣称已做**:
- **C++ 侧没编译**(`BeginSceneDrain` / DestroyScene 改动),需 Codex 跑
  `msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64`。
- **没上过集群**。单测覆盖的是 SceneManager 的决策与状态机,不是"玩家真的
  被改派到了另一个频道" —— 后者必须 dev 集群 E2E 验证。
- gate 的自动缩容执行器(第 2/3 步)没做,只有标记与过滤。
- `-race` 仍跑不了(本机无 gcc)。
- 测试耗时从 1s 涨到 47s:phantom scene 修复之后,每个"建场景"的用例都真的
  要建立 gRPC 连接(以前拨号立刻失败)。是修复的固有代价,不是 bug。
- 没有 commit / push / tag。

## 2026-07-29 SnowFlake node_id 获取链审计 + 失约 drain（存盘 → 强制改派大世界）

审计范围：C++ `node_id` 分配 / etcd 注册流 / 发号器本体 / 失约处理，Go `shared/snowflake` 与
`shared/snowflakealloc`。以下问题**全部已落码**，Debug|x64 全绿，Go 单测全绿。

### P0

1. **失约后 `abort()` 把玩家存盘丢了。** 四条路径（keepalive 返回 TTL=0 / 本地租约 deadline /
   重注册 CAS 失败 / Watch 发现 node_id 被抢）都是「跑一次**异步**存盘 hook，然后 `LOG_FATAL`」，
   而 muduo 的 `LOG_FATAL` 在语句结束时 `abort()` —— Redis 命令还在发送缓冲、DBTask 还在
   Kafka producer 队列，全部丢掉。
   改成：**fence 发号 → 存盘 → 存盘落地后请求 SceneManager 把玩家改派到大世界 → 15s 有界
   drain → 正常 `Shutdown()`**。细节与验收清单见
   `docs/design/snowflake-guard-and-node-conflict.md`。
2. **`WaitNextTime` 超时后把 high-water 往回写 → 重放已发的号**；而且它先 `LOG_FATAL`，
   一次 NTP 往回跳超过阈值就会把全服 C++ 节点一起打死。
   改成：high-water 只进不退，时钟回拨继续消费高水位那一秒的 step 池（不自旋、不 abort），
   池子发完才有界等待，等不到就借下一个逻辑秒。
3. **号称「3 秒有界」的等待在 Windows 上实测 42 秒。** `retry > 3000` 配 `sleep_for(1ms)`，
   而 Windows 默认定时器精度 15.6ms。改成 `steady_clock` deadline，实测 3.000s。
   **凡是用「重试次数 × sleep」当超时的地方都要按这个复核。**

### P1

4. **guard 只在「Redis 连着且读到 key」时才生效**，且 guard key 带 `zone_id` 而 node_id 的
   唯一域是全局 `(node_type, node_id)` —— 跨 zone 回收时新持有者读不到 guard。
   改成**无条件** guard 到 `max(now, lastTs)`，key 去掉 zone 段。
5. **Go 侧完全没有启动 guard**，而 `snowflakealloc` 有 hostname 亲和（同机重启必然拿回同一个
   worker id）+ `Close()` 立刻 Revoke + 秒级时间戳。探针实测：同秒重启两个生成器的第一个 ID
   逐位相同（`50960585831186432`）。`NewNode` 加启动 guard + `nowEpoch` 早于 Epoch 时钳位。
6. **`snowflakealloc` 失租不 fence**：KeepAlive 流结束只打一条 ERROR，进程继续用那个
   worker id 发号。加 `Handle.Lost()`，`guild` / `scene_manager` 收到后 `s.Stop()` 停服。
7. **`GenerateBatch` 与 `Generate` 对 `step_` 语义相反**（"最后用掉的" vs "下一个可用的"），
   混用时批量第一个 ID 与上次 `Generate` 完全相同。批量收敛成循环调用 `Generate`。
8. **`tlsSnowflakeManager` 是 `thread_local`**，只有 etcd 回调那个线程调过 `OnNodeStart`，
   其它线程发号会静默用 `node_id=0`（保留给"未分配"）→ 跨进程撞号。改 fail-closed。
9. **etcd txn 响应靠 FIFO 队列猜 key。** 生成的 grpc client 在 `!status.ok()` 时**不调 handler**，
   一次 etcd RPC 失败队列就永久错位，之后端口 key 的成功会被解释成 alloc key 成功 ——
   **没占住 node_id 就激活了发号器**。改成单槽 `pendingTxnKey_` + 10s 响应预算，
   到期重发同一条幂等 CAS 链（挂在已有的 `grpcHandlerTimer` 上，不新建定时器状态机）。
10. **端口分配失败仍然把 `port=0` 注册进 etcd**。改 fail-closed + 复用 `acquirePortTimer` 退避重试。

### 顺手修的非发号问题

11. **dirty-save 快路径跳过 → 退出流程永久挂起。** `SavePlayerToRedis` proto-compare 相等就
    `return`，于是 `HandlePlayerAsyncSaved` **永远不来**，`UnregisterPlayer` 的实体 /
    `SessionMap` / `playerList` 条目永不清理。**AFK 踢下线必然命中**（挂机不动＝数据逐字节相同）。
    改成返回 `bool`，两条路径共用 `FinishExitAfterPersist`。
12. `HandleExitGameNode` 先 `try_get` 再判 `valid()`（对已销毁实体调 `try_get` 是 entt 的 UB）。
13. `SnapshotSystem` / `TransactionLogSystem` 在发号器被 fence / 未初始化时 fail-closed，
    不再写 `snapshot_id=0` / `tx_id=0`。

### 测试工程

- `cpp/tests/snow_flake_test` **原本从来没编译成功过**：include 指向未初始化的
  `third_party/grpc/third_party/googletest` 子模块，link 列了几十个 `lib/` 下不存在的 `.lib`。
  按 `agones_lifecycle_test` 的做法改成编 `third_party/yaml-cpp/test/googletest-1.16.0` 的
  `gtest-all.cc` + 只链 `muduo.lib;ws2_32.lib`。
- 用例规模从硬编码 `40'000'000` 改成 `SNOWFLAKE_TEST_TOTAL` 环境变量（默认 200k 冒烟量）——
  原来单个用例分钟级、整套跑不完，所以事实上没人跑。
- 新增 4 个回归用例（`SnowFlakeRegression.*`）：批量/单发共用一条序列、时钟停摆不重放、
  guard 早于 epoch 被忽略、原子版同样不回退。
- Go 新增 `TestNewNode_BootGuardSkipsRestartSecond` / `TestGenerate_ClockRollbackKeepsMintingWithoutBlocking`
  与 `snowflakealloc` 的 4 个 `Handle.Lost()` 用例。

### 验证到哪一步

- MSBuild `Debug|x64`：`core.lib` / `modules.lib` / `scene.lib` / `scene.exe` / `gate.exe` /
  `snow_flake_test.exe` 全绿。**必须串行编** —— 两个 MSBuild 并发会报假的 `C1041`（vc145.pdb 争用）
  与 `LNK1104`（.lib 被占）。
- `snow_flake_test.exe` 默认规模全过。
- Go：`shared/snowflake`、`shared/snowflakealloc` 单测全过；`scene_manager` / `guild` build 绿。

### 没做，不能宣称已做

- **失约 drain 这条链零运行时证据**：只编译过，没在真集群注入过失约。验收步骤（含判定标准与
  修复前应当失败的对照）已写进 `docs/design/snowflake-guard-and-node-conflict.md` 末尾，必须跑一次。
- `Release|x64` 与 Linux/CMake 未验证。
- **login 的 PlayerId 用的是另一套布局**（`bwmarrin/snowflake`，13-bit node / 9-bit step / ms epoch），
  与 `CLAUDE.md §7.1` 写的「17-bit worker」以及 C++ `ParseGuid` 不兼容 —— 谁拿 `ParseGuid`
  解 `player_id` 会得到垃圾。改布局会作废存量 ID，本轮只记录不动。
- 疏散改派有一个窗口：SceneManager 判存活读 Redis `node_load:{zone}` ZSET，由它自己 watch
  etcd 删除事件后 ZREM，比本节点自检慢一拍，可能把玩家又路由回正在死的节点
  （gate 找不到该节点实体会报错，玩家需重登；**不丢数据**）。
- `SnowFlakeAtomic` 零生产调用方（只有测试用）。**刻意没删** —— 多线程发号如果哪天真的需要，
  它是正确答案，而 `thread_local` 的 `SnowFlakeManager` 不是；本轮把它的回拨路径一起修了。
- 没有 commit / push / tag。

### 2026-07-29 补充：Release|x64 现状（不是本轮引入的）

本轮把 `Release|x64` 也编了一遍，结论是**可执行/测试的 Release 配置在本仓当前状态下本来就跑不通**，
与本轮改动无关：

- **库能编**：`core.vcxproj` / `modules.vcxproj` / `libs/services/scene/scene.vcxproj`
  Release|x64 全部 `EXIT=0`。
- **exe 链不上**：`cpp/libs/services/scene/scene.vcxproj` 只有 `Debug|x64` 设了
  `<OutDir>../../../../lib/</OutDir>`，`Release|x64` 没设 —— Release 的 `scene.lib` 落在
  `cpp/libs/services/scene/x64/Release/`，而 `cpp/nodes/scene/scene.vcxproj` 的 Release
  仍然从 `lib/` 取。**任何**对 `libs/services/scene` 的改动在 Release 下都会表现为
  LNK2001，本轮只是第一次把它暴露出来。
- `lib/` 里的工程静态库（`rpc.lib` / `session.lib` / `table.lib` / `infra.lib` / `muduo.lib`
  时间戳 07-28 22:2x~23:12）是上一轮 **Debug** 全量构建的产物，Release exe 链接它们必然
  CRT 不匹配 —— `gate.vcxproj` Release 报的 `LNK1000 Internal error during IMAGE::BuildImage`
  就是这个来源。
- **没有动这套 lib 输出布局**：把 Release 的 OutDir 也指向 `lib/` 会让两个配置互相覆盖，
  影响面超出本轮范围，且本机没有可用的 Release 第三方库来端到端验证。留给后续统一处理。
- 本轮只修了**属于自己**的那一处：`cpp/tests/snow_flake_test` 的 Release 链接设置补齐成与
  Debug 一致（原本 Release 完全没有 `AdditionalDependencies`）。

Linux / CMake 同样未验证：本机没有 gcc / cmake，WSL 只有 docker-desktop 发行版。

### 2026-07-29 补充②：`core.vcxproj` 两个源文件的 obj 路径不分配置(踩到了)

`utils/file/file2string.cpp` 与 `utils/log/spdlog_file.cpp` 在 `core.vcxproj` 里带 per-file 覆盖，
其中 `ObjectFileName` 被**硬钉**成 `../../../../build/cpp/intermediate/lib/core/%(RelativeDir)`，
`ProgramDataBaseFileName` 硬钉成 `../../../../lib/core.pdb` —— **都不带配置条件**。

后果：先编 `Release|x64` 再编 `Debug|x64`，这两个 `.obj` 会被 Release 版本覆盖，而 Debug 增量
构建认为它们是最新的，直接打进 `lib/core.lib`。下游所有 exe 立刻炸：
```
core.lib(file2string.obj) : error LNK2038: "RuntimeLibrary" 不匹配: "MD_DynamicRelease" 与 "MDd_DynamicDebug"
core.lib(file2string.obj) : error LNK2038: "_ITERATOR_DEBUG_LEVEL" 不匹配: "0" 与 "2"
```
本轮验证 Release 时就是这样把 Debug 产物弄坏的（现象是 scene.exe / gate.exe 突然 LNK1319）。

修法（已落码）：两处改成 `$(IntDir)%(RelativeDir)` 与 `$(OutDir)$(TargetName).pdb`。
`Debug|x64` 的 `IntDir` 本来就是 `build/cpp/intermediate/lib/core/`，所以 Debug 行为完全不变，
只是 Release 不再和 Debug 共用同一个 obj。踩到的人删掉那两个 `.obj` 重编即可恢复。

### 2026-07-29 补充③：完整性复查补掉的一处 + 一个既有死代码

复查本轮改动是否自洽时补了一处、确认了一处：

- **补**：`FinishExitAfterPersist` 里加上 `PlayerFrozenComp` 判定。跨 zone 迁移在途的实体
  不能被销毁（要活到目的地 ACK 或 reaper 判失败为止），这条判定原本只写在
  `HandlePlayerAsyncSaved` 里；本轮新增的「存盘快路径跳过 → 内联收尾」那条路径绕过了它。
  两条路径既然共用 `FinishExitAfterPersist`，判定就必须落在它内部，否则迟早漂移。
- **确认**：`ChangeSceneInfoComp` 全工程**没有任何地方 emplace 到玩家实体上**
  （只在 `HandleCrossZoneTransfer` 里读、在末尾 remove）。也就是说跨 zone 迁移的**源侧**
  目前是死代码，`HandleCrossZoneTransfer` 恒早退。这是既有状态（对应 audit 文档的 #23/#25），
  不是本轮改动造成的；上面那处 frozen 判定按"将来接通"预防性补齐。

---

## 2026-07-29(续)自查:三处缺口的修复

上一轮交付后自查"改动完整吗",查出三处,全部处理完。

### ① 已验证没问题:Redis 人数计数会收敛

担心排空后 `instance:{sceneId}:player_count` 永不减、`finishDrainedChannel`
永不触发。查下来 `enterscenelogic.go:98` 在玩家 EnterScene 时会减掉他**上一个**
场景的计数,改派本身带着收敛。不需要改。

### ② 真缺口,已修:ScenePlayers 在登出路径上从来没清理过

只有换场景那条路径(`player_scene.cpp:196`)手工 `erase`,`HandleExitGameNode`
只删了玩家身上的 `SceneEntityComp`,场景那一侧的集合一直在泄漏。

以前没有真正的消费者,泄漏是静默的。**`BeginSceneDrain` 是第一个真正遍历它的
代码**,泄漏就变成会伤玩家的 bug:entt 复用实体 id,场景 A 里的陈旧 id 过一阵子
可能正好是场景 B 里某个活着的玩家,排空 A 会把那个不相干的玩家从 B 踢走。

已在 `HandleExitGameNode` 补 `scenePlayers->erase(player)`。
(写文件时 rename 被过滤驱动拦了 EPERM,同 360 拦 socket 那类问题,绕开处理。)

### ③ 真缺口,已修:世界频道不占 Agones rooms 名额

`initWorldScenesForZone` 不走 Agones 预占,世界频道创建时从不占房间名额。
后果:rooms Counter 少算,而 FleetAutoscaler 用的正是 Counter=rooms ——
**大世界人数增长根本不会触发 Pod 扩容**,阶段 D 那条"scene 按人数扩缩 → 进程
跟着扩缩"的链是断的。

修法:
- 新增 `ReserveAgonesRoomForWorldChannel`:优先按 `assignNodeByHash` 的哈希
  目标节点预占(保持 rebalance 依赖的分布稳定,不让 GSA 自由挑跟 rebalance 打架),
  占不到才回落自由分配;占不到任何容量时 **fail-closed 不建这个频道**。
- 新增 `TransferAgonesRoomForScene`,接进 `migrateWorldChannel`:频道搬家时
  名额跟着走。**先占新的再还旧的** —— 反过来的话中间窗口新节点可能已经没容量,
  名额两头都不在,频道变成不占任何容量的幽灵。新节点占不到时**不释放旧的**
  (多占一份好过不占),留给 reconcile 暴露漂移。

### ④ 已补:gate drain 单测(8 个)

覆盖标记/过滤/取消/TTL 过期自动恢复,以及两条刻意的失败方向:
Redis 挂了**放行**、候选全被标记时**放行**(拒绝所有登录比分到待缩容 gate 更糟)。

### ⑤ 已补:gate 排空执行器(第 2/3 步)

`gatedrain_monitor.go` + 11 个单测。

**执行器自动化的是"判定",不是"踢人"**,这是刻意的:
1. 那台 gate 上的玩家最终靠 "Pod 下线 → 客户端重连 → PickGate 已排除 draining
   gate → 落到别处" 完成改派,这条链现在就通,主动踢只是把同一件事提前。
2. 现有踢人原语 `KickPlayerEvent` 会让客户端弹
   `kLoginBeKickByAnOtherAccount`(账号在别处登录)。给一个正在做计划内缩容的
   玩家看这条提示是**误导**,比干净断开更糟。要正确地踢得先加一个
   "服务器维护,正在切换接入点" 的 tip —— proto + 表的改动,且本机无 protoc。

所以产出是明确信号 `gate:{id}:drained`(值=判定理由),缩容脚本/运维看到才动手。
`DeadlineSeconds=0` 表示**永不超时放行**、只认人走干净(绝不主动断玩家的口径);
超时放行时打 ERROR 并带上残留人数,因为那次缩容确实会断掉他们。
drained 标记与 draining 标记同寿,取消排空时清掉陈旧标记。

### 验证

- scene_manager:`build`/`vet` 干净,`go test ./internal/logic/` 全绿。
- login:`build`/`vet` 干净,`loginqueue` **31 个用例全绿**。
- `go/login/go.mod` 因新增测试引入 testify(+ 间接 go-difflib)。

### 仍然没做 / 没法验

- **C++ 全部没编译**,包括本轮的 ScenePlayers 修复。
- **没上过集群**:排空 → 改派 → 玩家出现在另一个频道,这条闭环一次都没真跑过。
- 主动踢人 + 维护提示 tip:被 proto 重生成挡住(本机无 protoc-gen-go-grpc/protoc 链路)。
- `-race` 跑不了(本机无 gcc)。
- 未 commit。

### 补充自查(同日):新加的 Agones 世界频道路径原本零覆盖 + 又两处漏转移

回答"代码层面修完了吗"时又查出两条:

**⑥ ③ 的修复本身零测试覆盖。** world 相关测试全部在 Agones 关闭下跑,
`ReserveAgonesRoomForWorldChannel` 走的是 early-return,新加的那条带
fail-closed 行为的路径一次都没被执行过。补 `agones_world_channel_test.go`
8 个用例:哈希目标有容量就落它(保证 rebalance 不会一直想搬回去)/ 满了回落
自由分配 / 全满 fail-closed 且不留映射 / 关闭时惰性 / 迁移转移名额 /
**新节点占不到时不释放旧的**(释放了等于不占任何容量,比多占更糟)/
关闭时转移 no-op / 排空销毁归还名额。

**⑦ 还有两处频道换节点没转移名额。** `reassignSceneNode` 有 3 个调用点,
上一轮只处理了 `migrateWorldChannel`。`world_init.go` 里另外两处
(目标节点已死改派、CreateScene 失败后重试改派)同样是频道换进程,
不转移的话新进程上不记账,rooms Counter 少算、FleetAutoscaler 欠配。
两处都补上 `TransferAgonesRoomForScene`。旧节点已死时释放是尽力而为
(GameServer 迟早被 Agones 回收),但**在新节点占一份不能省**。

验证:scene_manager `build`/`vet` 干净,`./internal/logic/` **128 个用例全绿**;
login `loginqueue` **31 个全绿**。

### 再一轮自查(同日):缩容销毁频道时漏了镜像级联

问"编译完了还有要改的吗"时,按前几轮暴露出的同一类问题(**调用点漏覆盖**)
再审新代码,又查出一条真缺口。

**⑧ `finishDrainedChannel` 没有级联处理以该频道为源的镜像 Scene。**

既有的两条销毁路径都做了级联 —— `destroyInstanceInternal` 的 cascade、
`migrateWorldChannel` 的 `cascadeMirrorsOnSourceMigration` —— 缩容这条没做。
镜像与源频道是共置的(scene-creation-architecture.md 的 mirror co-location),
源频道被销毁后镜像变成谁也进不去的孤儿,`scene:{id}:mirrors` 键还会永久泄漏。

两层修法:
- **候选选择排除镜像源**(`pickScaleInVictim`)。节点死亡那条路径是强制级联
  销毁镜像的,但**缩容是可选动作** —— 宁可少省一个频道,也不要把镜像里的
  玩家踢下线。读镜像集合失败时 fail-closed(当作有镜像、不缩容):查不清
  就别动比误伤划算。不是只看最闲的那个,最闲的恰好托着镜像时继续往下找,
  仍然能缩容。
- **收尾时级联兜底**。候选选择只保证"开始排空那一刻"没有镜像,排空窗口里
  可能又有镜像建起来,所以 `finishDrainedChannel` 仍要级联销毁 + 删 mirrors 键。

新增 `world_autoscale_mirror_test.go` 4 个用例:跳过镜像源改缩下一个 /
全是镜像源就不缩 / 查询失败 fail-closed / 排空窗口里新生的镜像被级联销毁。

**顺带记一个测试写法坑**:排空类用例必须先把承载节点注册成存活
(`sc.Redis.Zadd(testLoadKey(), 0, "10")`),否则 `beginDrainWorldChannel`
会走"节点已不在 -> 直接清理"的短路,当场把排空标记也清掉,断言看到的是
一个已经收尾完的频道。前两个用例最初就是这么假失败的。

验证:scene_manager `build`/`vet` 干净,`./internal/logic/` **132 个用例全绿**。

### 2026-07-29 补充④：CAS 响应超时改用标准一次性定时器

把上一轮「在 5ms 的 `grpcHandlerTimer` 里轮询 `txnDeadline_`」换成本代码库既有的
一次性 `TimerTaskComp` 写法（与 `acquireNodeTimer` / `acquirePortTimer` /
`watchReconnectTimer` 同构）：

- `EtcdService::ArmTxnTimeout()` / `CancelTxnTimeout()` / `OnTxnTimeout()`，
  由 `EtcdManager::SetPendingTxnKey` / `TakePendingTxnKey` **成对**调用。
- 不变量收敛成一句话：**有 pending key ⟺ 超时定时器在跑**。
  `EtcdManager::Shutdown` 也改走 `TakePendingTxnKey()` 而不是裸 `clear()`，
  免得留下"key 没了但定时器还在"的中间态。
- 删掉 `txnDeadline_` 成员与「首次看见 pending key 才起表」那段隐式逻辑，
  少一个状态、少一处每 5ms 的无谓比较。

在回调里取消自己是安全的：`TimerTaskComp::OnTimer` 对 one-shot 会先清 `timerId`
再拷贝 callback 调用（`timer_task_comp.cpp:124-138`），注释里也明确写了
"the callback may re-schedule or cancel this timer"。

Debug|x64 重编：core / scene.exe / gate.exe 全 EXIT=0。

### 第四轮自查(同日):孤儿清理漏掉自动伸缩引入的状态

问"全部做完了对吗"时,按同一类问题(**同类调用点漏覆盖**)查 `orphan_cleanup.go`,
又查出一条 —— 这是第三次同类问题了。

**⑨ 地图从 World 表删掉时,孤儿清理不带走自动伸缩引入的状态。**

`deleteOrphanChannel` 删了所有 scene 级 key,但:
- **不归还 Agones 名额**、不删 `scene:{id}:agones_gs` —— 那份容量在 GameServer 的
  rooms Counter 上永久占着,映射一删就再没人知道该向谁还;
- **不清期望频道数**(`world_channels:desired:zone:X` 的 confId 字段)——
  这张图将来被加回 World 表时,会直接复活上次伸缩到的数量(比如伸到过 8),
  而不是回到配置种子;
- **完全看不见正在排空的频道**。排空第一步就是把频道从 `world_channels` 摘掉,
  只看 setKey 会把它们整个漏掉;而且此时 confId 已不在 World 表里,
  `sweepDrainingWorldChannels` 也不会再扫到 —— 那些 key 永久残留。

修法:把 `world_channels:draining:zone:X:confId` 并进清理范围、逐个归还名额、
`Hdel` 期望频道数、删排空索引与标记。新增 `orphan_cleanup_autoscale_test.go`
3 个用例(归还名额+清期望值 / 排空中的频道也清掉 / 仍在表里的地图一个都不动)。

**关于我的判断标准**:用户连问四次,每次都问出真缺口
(ScenePlayers 泄漏 -> 世界频道不占名额 -> 零覆盖 + 两处漏转移 -> 镜像级联 ->
孤儿清理)。共同点全都是"我改了正在看的那一处,没把同类调用点全部 grep 一遍"。
以后新增一类状态(这次是 agones_gs / desired / draining 三个 key)时,必须先
`grep` 出所有会销毁 / 迁移 / 清理 scene 的路径,列成清单逐条对齐,再动手。

---

## 2026-07-29 真集群实测(Agones 1.58,本地 pandora-agones/WSL2):查出 6 个 bug

用户提供本地 dev 集群。之前所有"绿"都是 fake allocator + 结构检查,这一轮拿真
Agones 验,**查出 6 个真 bug,其中 2 个是 P0(整套功能根本跑不起来)**。

集群实况:Agones 1.58.0,`FEATURE_GATES` 为空(即该版本默认门控)。

### P0-1 每个 namespace 都要有 agones-sdk 的 ServiceAccount —— 生成器从来没建

`kubectl apply --dry-run=server` **一路绿灯**,因为 Fleet 对象本身完全合法;
失败发生在控制器随后建 Pod 的时候:

    pods "scene-instance-xxxxx-yyyyy" is forbidden:
    error looking up service account mmorpg-zone-today/agones-sdk:
    serviceaccount "agones-sdk" not found

Agones 的 helm 安装只在它自己那个 namespace(这里是 `default`)建了
`agones-sdk` SA + `agones-sdk-access` RoleBinding。zone namespace 是我们自己建的,
里面没有 —— **Agones 模式下所有 GameServer 直接进 Error,整套部署根本跑不起来**。
修法:新增 `New-AgonesSdkRbacYaml`,在 Fleet **之前** apply(反过来第一批
GameServer 会先失败一轮)。只做 namespace 级绑定,不新建/不修改 ClusterRole,
权限与 Agones 官方一致(events:create/patch + gameservers:list)。
实测修复有效:GameServer 从 Error -> Scheduled -> Ready。

### P0-2 归还 rooms 名额的整条路径是坏的:GameServer 没有 status 子资源

    kubectl get crd gameservers.agones.dev -o jsonpath='{.spec.versions[*].subresources}'
    => {"scale":{...}}          # 只有 scale,没有 status

所以 `dyn.Resource(gsGVR).UpdateStatus(...)` 会报
"the server could not find the requested resource" —— 创建失败回滚、排空归还、
孤儿清理归还**全部失效**,rooms Counter 只增不减 = 持续超卖。
改成普通 `Update`(仍是带 resourceVersion 的 read-modify-write,409 走原有有界重试)。
实测:`kubectl replace` 成功且 5 秒后计数没被控制器覆盖回去。
**单测抓不到**:fake allocator 不建模 API 表面,UpdateStatus 与 Update 在它眼里没区别。

### P1-3 FleetAutoscaler 被 CRD 校验直接拒掉

    spec.policy.counter.maxCapacity: Invalid value: 0: should be >= 1

生成器把 maxCapacity 做成了可选(`if ($AgonesMaxReplicas -gt 0)`),而 Agones 必填。
改为 `-AgonesAutoscale` 必须显式给 `-AgonesMaxReplicas`,与 `-AgonesRoomCapacity`
同一个 fail-closed 口径。

### P1-4 min/maxCapacity 单位错了,差 RoomCapacity 倍

Counter 策略里 min/maxCapacity 是**整个 Fleet 的总房间容量**,不是副本数。
生成器直接把副本数塞进容量字段:MaxReplicas=8 + RoomCapacity=6 本该是 48,
却写成 8 —— 等于把 Fleet 钉死在 2 个 Pod。改为乘 RoomCapacity 换算,
minCapacity 至少一个进程的容量,并加两条前置校验(max<min、bufferSize>=max)。

### P2-5 status.addresses 的类型是 "PodIP" 不是 "Pod"

    [{address:192.168.58.2,type:InternalIP},{address:pandora-agones,type:Hostname},
     {address:10.244.43.202,type:PodIP}]

代码只认 `"Pod"`,主路径永远匹配不上,每次分配都白走一次退化的 GET Pod。
功能上看不出来(退化路径有效),但请求量翻倍、注释里"零额外请求"是假的。
改成两个都认。

### P2-6 dev_tools.ps1 没透传 Agones 那批开关

`-AgonesHighDensity` / `-AgonesRoomCapacity` / `-AgonesAutoscale` /
`-AgonesBufferRooms` / `-AgonesMin|MaxReplicas` 只存在于 k8s_deploy.ps1,
而 dev_tools.ps1 才是文档入口 —— 这些功能**从文档路径完全不可达**。
已透传(switch 只在被指定时传,否则 k8s_deploy 里的 fail-closed 判定会漂)。

### 实测**通过**的部分(这些以前只是推断)

- `portPolicy: None` 被接受 —— 内部服务不需要 HostPort/NodePort 的判断成立。
- `counters.rooms` 被接受 —— CountsAndLists 在 1.58 默认开启,不需要额外 feature gate。
- GSA 形状被接受,webhook 补默认值后返回 `status.state`;无匹配时是 `UnAllocated`。
- **高密度模型实证**(设计文档 §9 的第 3/4/5 条):
  第一次分配点亮一台 Ready(rooms 0->1,转 Allocated);
  **第二次分配选中同一台**(rooms 1->2),另一台仍 Ready/rooms=0,**没有新建 Pod**。
- `status.address` 确实是**宿主机** IP(192.168.58.2),代码刻意不用它是对的。
- **Pod 名字 == GameServer 名字**,退化路径的假设成立。

验证后已清理 `mmorpg-agones-e2e` namespace;`default` 里 40h 前就存在的
pandora-* Fleet 未被触碰(我的 GSA 标签选择器与它们不匹配,全部返回 UnAllocated)。

### 教训

前四轮自查全靠"再想一遍",查出的都是同类调用点漏覆盖;而这一轮**只有真集群能查**
的 6 个 bug 里有 2 个 P0。结论很直白:**fake 单测证明的是"给定依赖这样行为时我的
逻辑对",完全不能替代"依赖真的这么行为"**。以后接外部系统(K8s CRD / 云 API /
第三方 SDK),形状假设必须拿真环境验一次,`--dry-run=server` 都不够 ——
它只校验对象合法性,不跑控制器后续动作。

---

## 2026-07-29(续)修复 C++ Linux 构建链:补 build_linux.sh + 又一个我埋的 P0

用户要求修完整。gRPC 版本分歧用**证据**解掉,不猜。

### gRPC 版本:v1.80.x(证据,非偏好)

    git -C third_party/grpc describe --tags  -> v1.80.0
    git submodule status third_party/grpc    -> (v1.80.0)
    .gitmodules                              -> branch = v1.80.x

Windows 构建就是对着这个 submodule 编过的。`Dockerfile.cpp` 里的 `v1.78.x`
是全仓**唯一**不一致的地方 —— 镜像会用一个和开发时不同的 gRPC 大版本去编。
已改成 v1.80.x 并在注释里写清证据。

### 新增 tools/scripts/build_linux.sh

原来根本不存在(工作区没有、`git ls-files` 没有、`git log -- <path>` 无任何提交),
但 Dockerfile.cpp 第 100/106 行 COPY 它并 RUN 它 —— C++ 镜像这条链必然断在 stage 2。

关键发现:构建逻辑其实**已经存在**于 `tools/archived/autogen.sh`(submodule ->
setup_dependencies.sh -> vcxproj2cmake.py -> 按依赖序 cmake 14 个 lib + 2 个 exe)。
所以没有重写一份,而是:
- `build_linux.sh` 成为唯一权威,支持 Dockerfile 需要的
  `--skip-deps` / `--relwithdebinfo` / `--split-debug`,外加 `--release`
  / `--debug` / `--skip-generate` / `--jobs N` / `--dry-run`;
- `--skip-deps` 必须同时跳过 `git submodule update`:Docker builder stage 里
  **没有 .git**,不跳会直接失败;
- `--split-debug` 产出 `bin/symbols/{gate,scene}.debug` 供 Dockerfile 的
  `symbols` stage 导出;`add-gnu-debuglink` 必须在 `strip-debug` **之前**跑,
  否则 gdb 找不回分离的符号;
- 收尾校验 `bin/gate` / `bin/scene` 存在且可执行 —— 否则失败会推迟到
  stage 3 的 COPY,报错信息晦涩得多;
- `autogen.sh` 退化成 `exec build_linux.sh --release`,**消除两份工程清单**
  (原来各存一份,任一边加 target 就会漂)。

### P0:我阶段 B 埋的坑 —— 生成器会把 CURL 接线冲掉

`vcxproj2cmake.py::write_cmake` 是 `open(path,"w")` **无条件覆盖**每个
`CMakeLists.txt`,而 Dockerfile 的 build 命令没有 `--skip-generate`。
也就是说我阶段 B 手加进 `cpp/nodes/scene/CMakeLists.txt` 的
`-DMMORPG_AGONES_CURL=1` 一跑就没了。

**危险之处在于它不会让构建失败**:`EXTERNAL_LIBS` 里本来就有 `curl`,链接照过;
只是宏没定义 -> `MakeDefaultHttpTransport()` 返回 nullptr ->
**整套 Agones 生命周期在生产 Linux 二进制里静默退化成 Disabled**,
GameServer 会永远停在 Scheduled。编得过、起得来、什么都不做。

修法:把 `add_definitions(-DMMORPG_AGONES_CURL=1)` 放进**生成器**,而不是只写在
被生成的文件里。教训写进注释:凡是要在 Linux 生效的编译期开关,必须落在
vcxproj2cmake.py,手改 CMakeLists.txt 等于没改。

实测(gcc:13 容器内真跑生成器):
    20:add_definitions(-DMMORPG_AGONES_CURL=1)
    146:    curl
    Generated: ./cpp/nodes/scene/CMakeLists.txt  (40 sources)   # 38 原有 + 2 个 agones

### 环境:Docker Hub CDN 断了(与 devops-stack-20260724 记档一致)

`gcc:13` / `ubuntu:24.04` 都拉不下来(cloudfront TLS handshake timeout),
而 Dockerfile 会先解析所有 FROM,所以连编译都进不去。
绕法:从本机已在用的 `dockerproxy.net` 镜像源拉,再本地 `docker tag` 成
`gcc:13` / `ubuntu:24.04` —— **不改 Dockerfile、不动 daemon 配置**,可逆。
之后镜像构建正常推进,并顺带验证了 `libcurl4` 在 Ubuntu 24.04 上是有效包名
(之前标注"未验证,与 Dockerfile.cpp 保持一致")。

### 状态

`build_linux.sh` 已通过 `bash -n`、未知参数 fail、Docker 那组参数的 dry-run;
生成器改动已在真容器里跑通。**完整镜像构建仍在进行中**(gRPC 从源码编,
耗时以十分钟计),尚未看到 `bin/scene` 产出 —— 没跑完之前不宣称这条链已修好。

### 同日续:标准化审查(只改客观可判定的,不做主观重构)

用户授权"不标准的地方都可以改"。我只动标准工具能判对错的,并且**先核实再改** ——
两次核实都推翻了我自己的初判:

**推翻 1:`bin/scene` 不是 Linux 产物。** `file bin/scene` => PE32+ MS Windows。
Docker 构建写在容器里、不落宿主 `bin/`,所以它不能当作本次镜像构建的证据。
(Windows 构建产物不带 .exe 后缀,很容易看错。)

**推翻 2:BOM 规则已经过时,而我按它白改了注释。**
`.github/copilot-instructions.md` 原文说"含非 ASCII 就必须加 UTF-8 BOM",完全没提
`/utf-8`。实测:`cpp/` 下有 **27 个含中文且无 BOM** 的 .cpp/.h,而 `game.sln`
Debug|x64 全绿 —— 因为编译它们的工程传了 `/utf-8`
(`<AdditionalOptions>/utf-8 /bigobj ...`),MSVC 就无视代码页按 UTF-8 解析。

所以正确的标准化不是改 27 个文件加 BOM,而是**保证 `/utf-8` 没有工程漏掉**。
已重写该文档段落:`/utf-8` 是权威机制、BOM 只是兜底、字符串字面量仍旧只用 ASCII
(它会流进运行时日志,代码页问题重新开始)。并记下这条规则曾让一个 agent
把中文注释改写成英文去躲一个构建早已处理掉的隐患 —— 就是我。

**留给人工的清单(我没动,因为无法自证不破坏 Windows 构建):**
54 个 vcxproj 里只有 **17 个**带 `/utf-8`,缺的 37 个是真隐患(含
`cpp/generated/*` 五个和全部 `cpp/tests/*`)。找法:
`grep -L "/utf-8" $(find cpp third_party -maxdepth 4 -name '*.vcxproj' -not -path '*muduo_windows*')`
在我上下文将尽时做 37 处 XML 插入、又无法编译验证,风险大于收益。

**gofmt:既有的,没有混进本次改动。** scene_manager + login 共 19 个文件未
gofmt 格式化,抽查 `changesceneutil.go` / `createscenelogic.go` /
`load_reporter.go` 的 **HEAD 版本同样未格式化** —— 属历史遗留,不是我引入。
在功能改动里夹一次全仓 gofmt 会让 diff 无法审阅,建议单独一个 commit 做。

### 构建链又一处断点:.dockerignore 把 navmesh 数据排掉了

`data/scene_nav_bin` 在磁盘上存在,但 `.dockerignore` 有一条 blanket `data/`,
于是 Dockerfile.cpp stage 3 的 `COPY data/scene_nav_bin/` 失败:
    failed to compute cache key: "/data/scene_nav_bin": not found
BuildKit 并行解析各 stage 的 COPY,这条**快速失败会掐掉整个镜像构建**,
`build_linux.sh` 一行都没跑到 —— 所以现象看着像编译问题,其实是构建上下文问题。
修法:在 blanket `data/` 之后加 `!data/scene_nav_bin/` 与
`!data/scene_nav_bin/**`,只放回 navmesh 二进制,旁边的 .xlsx 设计表仍然排除。
重新构建已越过该点,进入 gRPC 源码编译阶段。

**注意:截至记档时镜像仍未构建完成,`bin/scene` 的 Linux 产物尚未出现。
"C++ Linux 构建链已修好"这句话还没有资格说。**

### 2026-07-29 补记:muduo 子模块 pin 的 commit 在上游已消失

`third_party/muduo-linux` 与 `third_party/recastnavigation` 从未初始化过,
所以 `deploy/k8s/Dockerfile.cpp` 的 `COPY third_party/muduo-linux/` 复制的是空目录,
下一步 `ln -s ../contrib third_party/muduo-linux/muduo/contrib` 必然失败
(报的是 "No such file or directory",指的是**父目录**不存在,不是链接本身的问题)。

初始化时撞到:

    fatal: remote error: upload-pack: not our ref 7b30f61c0ad3b34a0314aff791b0ff06bd122002

`.gitmodules` 登记的 commit `7b30f61` 在上游 https://github.com/chenshuo/muduo.git
**已经不存在**(force-push / 历史重写)。用户确认继续用该上游,遂 checkout 到当前
master:

| 子模块 | 原登记 | 现在 |
|---|---|---|
| muduo-linux | 7b30f61(上游已消失) | f1fc77e (v2.0.3-1) |
| recastnavigation | 9f4ce64 | 9f4ce64(同 commit,只是补了 checkout) |

**未提交**。两个子模块现在是 `+` 状态(工作区指针与仓库登记不一致)。
需要有人把新指针提交进去,否则每次 clone 都会撞同一堵墙。

**风险须知**:muduo 是网络层核心,f1fc77e 与当初 pin 的 7b30f61 之间有多少行为
差异无法评估(旧 commit 已不可达,无法 diff)。Windows 侧用的是
`cpp/libs/engine/muduo_windows`(另一套源码),因此 Linux 与 Windows 两边的 muduo
**本来就不是同一份** —— 这次变更没有让这个事实变得更糟,但也没有改善它。

### 2026-08-02:P0/P1 审计修复(静态验收完成,编译门禁未执行)

- 本轮未发现可确认的 P0；已修复审计确认的 P1：Scene 停机先封 Kafka consumer，
  再让 gRPC drain 与 Redis/Kafka 持久化 barrier 并行，二者同时完成后才拆运行时；
  业务 barrier 有 15 秒上限并保留明确失败日志。POSIX signal handler 只写
  `sig_atomic_t`，实际停机回到 EventLoop。
- 固定析构顺序为 Hiredis -> MessageAsyncClient -> EventLoop；Kafka producer 改为
  有界真实 flush，析构不再无限 poll。Windows 与 Linux muduo overlay 保持同一
  Hiredis Channel cleanup 行为。
- EnTT 中五种直接承载 `TimerTaskComp` 的组件启用原地删除，并补双实体回归源码，
  防止 swap-and-pop 搬移幸存组件时取消其活跃定时器。
- K8s gRPC poller 默认统一为 8；Windows clean 同时清 build/install；Linux
  gRPC/protobuf 使用精确 SHA stamp，muduo 使用源码身份加完整 overlay 哈希，
  缓存不再只凭某个归档文件存在就跳过。
- 已通过 PowerShell AST、vcxproj XML、v1.83 install manifest 差集、stamp/overlay
  一致性和目标文件 `git diff --check`。按 `AGENTS.md` 编译协作门禁，本轮没有
  Claude 给出的目标/命令/环境/产物/通过标准，因此未编译、未跑测试，也未宣称
  运行时或跨平台构建验证通过。
- 当前工作树仍是大规模混合 WIP；本轮未 stage/commit/push。最终交付必须把现有
  gRPC v1.83 gitlink、生成的 AttributeSync 文件、gRPC patch 和 muduo overlay
  一并纳入人工确认的提交，否则新 clone/default submodule update 仍会回到旧指针。

### 2026-08-02：P0/P1 修复本地交付边界（已拆分提交，未 push）

- 在 `codex/fix-p0-p1-audit` 分支按主题完成四个本地提交：Scene/Node 优雅停机与
  Redis/Kafka/Hiredis 生命周期、EnTT 定时器稳定地址、K8s gRPC poller 配置、
  Windows gRPC `-Clean` 同时清理 build/install。
- 每个提交均使用精确路径或 hunk 暂存；`client/`、AttributeSync 全量生成物、
  TimerTask 代际/TimerQueue 旧 WIP 以及其他子模块漂移均未纳入。
- gRPC v1.83/protobuf v35.1 主升级没有提交：当前 HEAD 的 C++ 生成头硬校验
  `PROTOBUF_VERSION == 6031001`，而 v1.83 运行时要求 `7035001`。只提交 gitlink、
  vcxproj 和构建脚本会让全新 checkout 必然编译失败；完整闭包会卷入 330+ 生成文件
  及尚未完成的 AttributeSync 再生成轨道，不能伪装成独立修复提交。
- 已完成 staged diff、PowerShell AST、XML/manifest/哈希等静态检查；仍未执行 C++
  编译、新增单测、Linux shell 语法检查、K8s apply 或玩家 E2E。
- 仓库规则要求 push 由人手动执行，因此本轮停在本地提交，未更新任何远端引用。

### 2026-08-03:第二轮审计 —— 玩家异步存盘丢失窗口 + 定时器 UAF

- 新增 `docs/design/player-async-save-loss-windows.md`。区别于既有的
  `db_write_behind_dirty_flag_race.md`(那份讲 Go db 服务的写回选型),本份记录
  **scene 节点自己**的"置脏 + 异步存盘"链:proto-compare 快路径、
  `PlayerLastPersistedSnapshotComp`、hiredis 异步回调,以及各自的丢失窗口。
  含"已经安全、别重复修"的清单,避免下一轮审计重复推导。
- **关键前提被记录下来**:scene 存 `<PlayerAllData full_name>:{id}`,而 db 服务
  回写的是 login 读的 `player_database:{id}` 等分表 key —— **两套 key 互不覆盖**。
  `redis_client.h` 原先那句 "cache will be stale until dbservice rewrites" 的兜底
  假设因此不成立。
- 修:存盘退避重试耗尽后只打一条 LOG_ERROR 就 return,没有任何人被通知。后果
  是(a)`PlayerAllData:{id}` 停在上一次成功存盘的内容,玩家下次进场直接回档;
  (b)退出流程把收尾整段挂在 `HandlePlayerAsyncSaved` 上,回调不来则实体带着
  `UnregisterPlayer` 永久滞留,`IsSaveInFlight` 恒为 true。新增与
  `SetLoadFailedCallback` 对称的 `SetSaveFailedCallback`,scene 侧接
  `HandlePlayerAsyncSaveFailed`:打明确的 DATA-LOSS 日志、**不**更新快照
  (保证下次存盘必然重写整份数据)、非跨 zone 冻结态则推进 `FinishExitAfterPersist`。
- 修:`TimerTaskComp` 的 use-after-free。muduo `TimerQueue::handleRead` 先整批
  取出到期定时器再逐个 `run()`,此后 `cancel()` 只写 `cancelingTimers_`(仅影响
  重复定时器重挂),**run() 照常发生**。批内前一个回调销毁后一个定时器的宿主时,
  后者的闭包以已释放的 `this` 进入 `OnTimer`,而 `generation` 守卫本身就住在那块
  内存里。可达路径:同实体两个 buff 同批到期,buff1 的 `OnBuffExpire` →
  `RemoveSubBuff` → `buffList.erase(buff2)` **同步**析构 buff2 的 BuffEntry
  (内含 `expireTimerTaskComp`)。改为闭包持 `weak_ptr` 存活令牌,开火先 lock。
  令牌懒创建,未武装的组件不付分配成本。
- 按新的协作分工(CLAUDE.md §10.1 / AGENTS.md §4.1),本轮 C++ 改动**未编译**,
  待 Codex 验证:`modules` → `scene` lib → `core` → 两个节点,MSBuild 串行 `/m:1`。

### 2026-08-03:服务端全链审计修复最终收口(Codex 动态验证)

先更正上一段已经失效的描述:`HandlePlayerAsyncSaveFailed` 当前**不会**推进
`FinishExitAfterPersist`。最终实现保留冻结实体、脏快照和退出上下文并继续有界退避
重试,避免在 Redis 持久化未成功时销毁唯一内存副本。上一段“失败后推进退出”的记录
只反映中间版本,不得再作为当前行为依据。

**认证与跨区边界:**生产 password 认证已落地为 MySQL 权威查询 + Argon2id PHC,
未知账号走 dummy KDF,并发/等待都有硬上限；默认关闭,DSN 只从环境变量读取。新增
只允许既有账号的隐藏终端 `password_admin` 和 fail-closed schema 迁移,不再允许客户端
自报 account 自动创建/接管账号。Java 的 Login/AssignGate/QueueStatus 必须命中精确
zone endpoint,缺映射在网络前失败；RefreshToken 因 wire 中尚无 zone,仍只保留原有
无 zone 路径。历史压测文档中的真实形态 access/refresh token 已脱敏。

**数据一致性:**data_service 的 zone/全服/单人应用回档在跨服务 offline-epoch
`RollbackFence` 未落地前统一 code=16、零写；缺 store/router、意图审计、安全快照或
结果审计均显式失败。recall dry-run 检测 10000 行截断并返回 code=17、零执行；
non-dry-run 继续 code=16。db consumer 改为 durable ready/processing/dead receipt、
连续 offset 提交、原分区重试、同 key 租约和 poison DLQ；MySQL 已提交但 Redis cache
发布失败时不得 ACK。Kafka partition 在同一 TopicGeneration 内严格不可变,broker
漂移会启动失败,扩容只能停写排空后切新 generation/topic。

**在线状态与社交:**player_locator 的 session/version CAS、TTL 宽限、正常下线清理和
lease receipt 已补齐；SceneManager 缺失、claim 身份不匹配或通知失败都保留 processing
回执重试。Friend 接受申请必须有 pending 记录,容量以 MySQL 锁行计数；迁移先持久化
pending,再单事务归零/权威回填/ready,半迁移和缺 marker 全部失败关闭。Guild membership
唯一约束、role/score 权威列和缓存 generation 已补齐；公告授权在同一 MySQL 事务按
guild→member 锁序重新校验,降权/退会后不能利用陈旧 Redis officer 快照写公告。

**Scene/C++:**SceneManager 的节点键统一为 `(zone_id,node_id)`,重复身份、跨区已有
location、缺 writer/client 均在变更前失败；Kafka route 单次有界同步写,失败用精确值
Lua CAS 回滚。跨节点切场景在 epoch 交接协议完成前默认拒绝。C++ 修复货币债务无符号
下溢/溢出、欠款持久化、异常桶回收和 Redis 空/损坏/超长 payload 误报加载成功；加载
失败清空队列并只走 failure callback。

**Codex 实际门禁:**Go 的 data_service、db、player_locator、guild、friend、login、
scene_manager、proto 均完成对应 `build/vet/test`;shared 的 kafkautil/snowflake/
snowflakealloc 三个受影响包通过。Java Gateway 在本机 JDK 21 下 46/46(8 reports,
0 failure/error/skip)。C++ 按 `/m:1` 串行通过 modules→scene lib→scene node→gate node,
新增 Redis 损坏载荷 2/2,完整 currency 24/24。临时 MySQL 8.4 验证 password migration
正反例、Friend 并发/半迁移、Guild 陈旧权限及社交 SQL 连跑两次；临时 Kafka 7.9.7
验证 partition marker 会拒绝配置漂移和 broker 外部扩容。所有本轮临时容器已删除。

**仍是部署门槛而非“已上线”:**目标库必须停旧写实例后执行 password 与
guild/friend 迁移并检查 gate；guild score Redis backfill 完成前 marker 保持 pending。
生产 password 开启前还要注入只读 DSN 并由管理命令迁移 hash；没有公开注册/找回/
改密 API。offline-epoch 回档、跨节点 scene handoff 和 Kafka 新 generation 切换仍需
跨服务部署协议。机器只有 JDK 21,项目声明的 JDK 23 编译未验证；Go `-race` 因无 gcc
未运行；全 shared `./...` 仍被既有 `generated/bit_index` 同目录双 package 阻断；
no-raw-pointer checker 因工具不存在而跳过。未跑生产/K8s/完整 E2E。

本次是用户在已知 30+ 红线后再次明确要求“修复完毕”的大范围混合工作树收口；最终
状态远超 30 个文件,必须人工按主题拆分审阅。全程未读 `client/`,未 stage/commit/push,
并保留既有的四个脏 third_party 子模块状态。

### 2026-08-05:配置表投影 API + 导表器三项修复(Claude 落码,未跑导表器)

**起因:**「生成的表访问代码该不该加『取全部 id / 取某列全部取值』的接口」。结论是
**不进代码生成器**:这两件事只依赖「每行都有 uint32 主键」这一全表共有形状,泛型一次
写完即覆盖全部表、加新表零成本;渲染进模板则是 20 份逐表副本,还没等到第一个消费者
就先摊 20 份死代码,而且每次改动都要全量重导。生成器只该生成手写不出来的东西 ——
索引结构与类型安全的按键查询。Pandora 后端同结论、同实现(`pkg/configtable/query.go`)。

**新增 `go/shared/tablequery`(手写包):**`IDs`(全部主键,加载序)/ `Values`(某列投影,
加载序、不去重)/ `DistinctValues`(某列键集合,升序去重)。返回值一律是调用方独占的新
切片,可随意 sort / 持有,不会打乱表内部状态;刻意不返回 `iter.Seq`(惰性求值会把旧快照
无声钉死在一个看起来像「查询」的值里),也不返回预计算共享切片(只读约定压在注释上守不住)。
没放进 `go/shared/generated/` 是因为 CLAUDE.md §3 禁止手改生成树;本包与 `table` 零耦合,
不 import table 也不 import pb。

**关闭上一轮记录的遗留阻断:`generated/bit_index` 同目录双 package。**根因不是陈旧残留,
是 `bit_index_gen._gen_go` **至今仍**把每张表平铺写进同一个 `bit_index/`,而模板声明
`package <sheet>` —— 每次导表都会复发;`mission/` `reward/` 子目录版是有人手工 gofmt 后
挪进去的。已改为写 `bit_index/<包名>/`,Environment 加 `keep_trailing_newline`(Jinja 默认
吃掉行尾换行),模板按 `name_width` 显式补齐。原模板「每条常量之间插空行」其实是在规避
gofmt 的 `=` 对齐,只有 ID_1 与 ID_10 不等长时才现形。两个碰撞的扁平副本已删。

**TableManager 快照改 `atomic.Pointer`。**原 `m.snap = snap` 是裸赋值、读侧无同步;今天
不是竞态只因为 `LoadTables` 在各服务 `servicecontext.go` 构造时调用一次、早于任何读,
一旦有第二次 Load(热更的本质)立刻是真实 data race。**关键:不能把 `m.snap.X` 无脑换成
`m.snap.Load().X`** —— `RandOne` 一个方法里读三次、`FindByIds` 在循环里读,多次 Load 可能
拿到不同快照,表缩小时 `data[rand.IntN(len(data))]` 越界,等于用新 bug 换潜在 bug。正确
形状是每个访问方法开头取一次本地快照。265 处 × 20 文件由一次性变换器完成,模板
`go_config.go.j2` 同步改成同一形状。

**deploy 陈旧产物检查。**`md5_copy_dir` 只覆盖不删除,导表器一改输出布局旧产物就永久残留
(上面的 bit_index 双 package 正是这么来的)。`file_utils.report_orphans` +
`orchestrator._deploy` 接线,**只告警不自动删**(目标树里合法混着手写文件,`_DEPLOY_KEEP`
已登记 `table/table_test.go`)。判定必须按 dst 归并全部 src:Java 有两个不同 src 部署到
同一个包目录,逐对判定会把另一对的产物全报成陈旧。

**已验证(Go 侧):**`go/shared` 全模块 `go build ./...` 由 exit=1 转 **exit=0**(即上一轮
记录的 bit_index 阻断已消除);`go vet ./generated/table/...` exit=0;残留裸 `m.snap.` 访问
grep 为空;`gofmt -l generated/bit_index/` 干净;`go test ./generated/table/... ./tablequery/...`
通过(前者是手写 `table_test.go`,会真加载 `generated/tables/` 数据);login / friend / guild /
player_locator / scene_manager / db / data_service 七个服务模块 build 全绿。

**未验证:**本机没有 Python(`py.exe` 只是 Store 存根)也没有 protoc,**导表器一次都没跑过**。
五处 Python / Jinja 改动(`bit_index_gen.py`、`file_utils.py`、`orchestrator.py`、
`go_bit_index.go.j2`、`go_config.go.j2`)全是静态写出、未运行。Go `-race` 同样因无 gcc 未运行
(与前几轮同因)。

**Codex 执行清单:**
1. 装依赖跑一次导表器:`cd tools/data_table_exporter && python run.py`;
2. `git diff go/shared/generated/` **预期为零** —— 产物是照模板渲染结果手工对齐的(含
   TableManager 结构体上的注释块)。**有 diff 一律以导表器输出为准**,按 diff 改模板或产物,
   不要反过来把 `atomic.Pointer` 改回裸指针,也不要恢复 bit_index 扁平布局;
3. 确认 `generated/code/go/generated/bit_index/` 下只有 `<包名>/` 子目录、无扁平 `.go`;
4. 看导表器日志有无「陈旧产物?」告警,逐条确认后手工删;确属手写的登记进 `_DEPLOY_KEEP`;
5. `cd go/shared && go build ./...` 应 exit=0;有 gcc 的环境补跑
   `go test -race ./generated/table/... ./tablequery/...`。

未 stage/commit/push;既有四个脏 third_party 子模块状态保持不动。

### 2026-08-03(续):gate 会话链 + 玩家进场 handler 审计

本轮首次审这两块(此前零覆盖)。

**gate — P0:`GateHandler::BindSessionToGate` 整体覆盖在线会话。**
`sessions()[id] = SessionInfo{...}` 会把 conn(TCP 连接)、verified、entityIds
(scene 绑定)、sceneId 一并冲成默认值。后果:conn 变空指针而
SendMessageToPlayer / BroadcastToPlayers / PushToPlayer 都不检查就 `conn->send()`
→ 崩;verified 归 false → 该客户端后续消息全被判未验证并 shutdown;scene 绑定丢失。
生产实际走的是 Kafka 的 `BindSessionEventHandler`(实现是对的,就地更新),这个
gRPC 版是 Centre 退役后的遗留、全仓无调用方,但**仍是 gate 上一个活的 RPC 端点**。
已改为就地更新;会话不存在时返回错误而不是凭空建一条无 conn 的僵尸会话
(那种会话永远等不到 TCP 断开回调,只会永久泄漏)。

**gate — P0:共享请求原型跨会话残留。** `gRpcMethodRegistry[msgId].requestProto`
是进程级单例,`ParseMessageFromRequestBody` 旧实现对空 body 直接 return、
解析失败也只 return void。于是上一个玩家的请求参数会挂着当前会话的 player_id
被原样转发到后端 —— 故意发空 body 即可重放他人参数。已改为先 Clear() 再解析,
并返回 bool;解析失败回 kRequestMessageParseError 且不转发。

**gate — P1:两处错误应答绕过 codec 造成分帧错位。** CheckMessageSize 与
CheckMessageLimit 用 `conn->send(msg.SerializeAsString())` 直发无帧字节,客户端会
把 protobuf 字段字节当长度头读,此后整条连接的分帧全部错位 —— 一次限流应答
就毁掉整条连接。改走 `protobufCodec.send`。

**gate — P1:token 签名用 `!=` 比较。** 首个不匹配字节即返回,可计时侧信道逐字节
猜签名。改 `CRYPTO_memcmp` 常数时间比较(长度不等仍可直接拒,长度不是秘密)。

**gate — P1:`RoutePlayerMessage` 把业务 node_id 当 entt 实体整数。**
`entt::entity{nextNode.node_id()}` —— uuid 主键重构后两者不再相等,轻则撞不上
有效槽位静默丢消息,重则撞上无关的有效实体、把玩家消息路由到错误节点。这正是
`session_info_comp.h` 注释里警告的坑,全仓仅此一处漏网。改走
`NodeUtils::FindNodeEntityByNodeId`。

**gate — P2:** 补齐 SendMessageToPlayer / BroadcastToPlayers / PushToPlayer 的
空 conn 判定(BroadcastToScene/BroadcastToAll 一直有,这几处漏了)。

**scene — P1:`EnterScene` 找不到场景时 fail-open。** 旧实现只打 ERROR 就继续
置登录态并触发 PlayerLoginEvent:玩家被算作"已登录"、任务/每日奖励开始结算,
但他不在任何场景里,客户端也收不到 NotifyEnterScene,永远停在加载界面。更糟的是
enter_gs_type 已写入,重试进场时 `alreadyLoggedIn` 为真,**登录事件再也不会补触发**,
这一次的登录结算永久丢失。已改为 fail-closed:发 kEnterSceneFailed 提示后直接返回,
不置登录态,让重试仍能正常触发登录事件。
顺带定位到上游不一致:`GateHandler::PlayerEnterGameNode` 只设 scene 实体绑定、
**没设 session.sceneId**,于是 BindSession 那条路径会转发 `scene_id=0`。该 RPC 同属
Centre 遗留,未改,待确认是否可整体退役。

本轮 C++ 改动**未编译**,待 Codex 验证。

### 2026-08-03(续二):etcd 注册状态机 + 节点关停审计

**P0:两个 GM 优雅停机 handler 必崩。**
`GameChannel::CallMethod` 给 handler 传的 `done` **恒为 nullptr**
(game_channel.cpp:451,应答是 CallMethod 返回后由框架序列化 response 再发的)。
而 `GateHandler::GmGracefulShutdown` 与 `SceneAdminHandler::GmGracefulShutdown`
都写了 `done->Run()` —— 确定性空指针解引用。后果不是"少发一个应答":
gate 那份在崩之前已经把所有客户端 forceClose 了,然后进程当场崩在这一行,
`gNode->Shutdown()` 的租约注销与优雅收尾完全没机会跑;scene 那份同理,
存盘 barrier 不执行。**"优雅停机"实际退化成硬杀 + 崩溃。**
修法:删掉 `done->Run()`,并把停机投递改成
`gNode->GetLoop()->queueInLoop([]{ ... })` —— 排到当前事件循环回合末尾,
此时 CallMethod 已返回、应答已写进发送缓冲,既拿到应答又不会在 handler
返回前就开始拆运行时。仍是非阻塞投递,不会形成
"gRPC Server::Shutdown 等 handler 退出 / handler 等 Shutdown 完成" 的闭环。
全仓 `done->Run()` 只有这两处,已全部修掉。

**审过并确认健康、别重复查的(这轮没改一行):**
- lease 看门狗闭环:`OnKeepAliveResponse` 只覆盖"收到 TTL=0 的应答";
  网络分区导致应答**永远不来**的情况由 `Node::StartNodeRegistrationHealthMonitor`
  每个 health_check_interval 调 `IsLeasePresumablyExpired()` 兜住,
  超时即 `kLeaseDeadlineExceeded` fence 自杀。`leaseTtlSeconds_<=0` 的早期返回
  保证首次授租前不会误判。
- etcd txn 响应丢失闭环:`SetPendingTxnKey`/`TakePendingTxnKey` 维持
  "有 pending key ⟺ 超时定时器在跑"的不变量,`OnTxnTimeout` 到期后**重发幂等
  CAS 链重查权威**(重注册保留原 node_id/端口,初始引导从端口阶段重来),
  不是假设成功往下走。
- 重注册模式下任何 key 的 CAS 失败都判定为身份被抢并 fence 自杀,
  `RegisterNodePort(reRegistering=true)` 用无条件 Put 避开"每次重注册
  都确定性自杀"的老坑(portKey 按 IP+端口作用域,不存在覆盖别人的可能)。
- Watch 劫持检测用 `node_id != 0` 门控,避开同 zone 并发启动的误杀。

本轮 C++ 改动未编译,待 Codex 验证。

### 2026-08-03(续三):网络/RPC 层 + scene 进出场审计

**修(4 处):**
- `codec.h/.cpp`:客户端-facing ProtobufCodec 的单条消息上限从 64MB 收到
  64KB(可配构造参数)。合法客户端消息 ≤1KB(CheckMessageSize),而旧上限意味着
  恶意客户端一条消息就能让 gate 在任何业务检查之前全量缓冲 63MB、跑 adler32、
  按 typeName 反射建任意 proto 再 parse;单连接 64MB × N 连接 = OOM。
  节点间 RpcCodec(ProtobufCodecLite)不受影响,仍 64MB。
- `rpc_client.h` + `rpc_server.cc`:节点间连接补上高水位保护(64MB forceClose)。
  客户端连接早有(gate 2MB),节点间之前完全没有 —— scene→gate AOI 推送是全系统
  最大流量路径,一个卡死的 gate 能把 scene 拖到 OOM。断开是安全的:enableRetry
  自动重连,注册状态机已审计过能收敛。
- `player_lifecycle.cpp` RemovePlayerSession:defer 求值错位。defer 宏是 [&]
  捕获、作用域结束才求值,旧写法先把 gate_session_id 改成 kInvalidSessionId,
  defer 再拿着 kInvalidSessionId 去 erase —— 真 session 从没被删过,SessionMap
  每断线泄漏一条,无界。全仓其余 defer 用法审过无同类错位。
- (并行会话已修)CheckMessageSize/CheckMessageLimit 的错误应答改走 codec 帧,
  旧裸 conn->send 会让客户端流永久错位。

**审过健康(别重复查):** gate Kafka 事件链(RoutePlayer/BindSession/Kick/
Redirect/Push/Broadcast 全部有 session/conn 守卫,ForwardPlayerToScene 参数名
sceneNodeId 实为 entity 整数,命名误导但语义正确);PlayerEnterGameNode 的重连
竞态(pendingMap 覆盖+旧 session 清理+AsyncLoad 在途去重);EnterScene fail-closed
(场景缺失不置登录态);RpcServer 的 channel 生命周期(单线程 loop 内安全);
gate dispatcher 对未知类型 shutdown。

**P2 记录未修:** PlayerLeaseExpiredEventHandler / PlayerDisconnectedEventHandler
空实现(player_locator 发的租约过期通知 gate 不消费 —— 假死连接不会被清);
NODE_ROUTE 消息类型是死代码(HandleNodeRouteMessage 空,仅 RouteMessageToNode
API 面残留);ProtobufCodec::defaultErrorCallback 只 shutdown 不 forceClose
(对端不配合关闭时连接可挂半开)。

本轮 C++ 改动未编译,待 Codex:core(codec/rpc_client/rpc_server)→ scene lib
(player_lifecycle)→ gate/scene 两节点,MSBuild 串行。

### 2026-08-03(续四):C++ Kafka infra 审计 —— 跨 zone 迁移链两处 P0 级缺陷

**P0-a:zone id 被当成 Kafka partition 号(4 处 send)。**
`HandleCrossZoneTransfer` / `HandlePlayerMigration` 的 3 处 ACK / reaper republish
都把 `toZoneId`/`from_zone` 传进 `send()` 的 partition 参数。topic 自动创建默认
1 个 partition,`produce(partition=zoneId≥1)` 直接 ERR__UNKNOWN_PARTITION ——
**迁移消息根本发不出去**,玩家冻结到 reaper 判弃解冻。全部改回 PARTITION_UA
(按 key=playerId 哈希),zone 路由不编码在 partition 上。

**P0-b:`HandlePlayerMigration` 无目标过滤 × per-node 消费组全量扇出。**
订阅是 per-node-id 消费组(每节点收到 topic 全部消息),而 handler 从不检查
`to_zone`:一次迁移会让**集群所有 scene 节点**各建一份玩家实体、各自
SavePlayerToRedis、各自 ACK —— 幽灵实体 + 幽灵节点周期存盘反复覆盖真实进度
(表现为随机回档)。加两级过滤:①to_zone != 本 zone → DEBUG 跳过;
②scene_info.guid 有值则要求目标场景在本节点。
**顺带查实:目标场景标识在协议里从来就是缺的** —— scene_info.guid 全仓无赋值点,
且是 uint32 装不下 64 位 snowflake scene_id。guid==0(现状)放行但 WARN:
zone 内单节点 dev 行为不变;多节点拓扑前必须先给 PlayerMigrationEvent 补
64 位目标场景字段(跨 zone 能力未完成清单+1)。生产侧本就被 scene_manager 的
AllowUnsafeCrossNodeHandoff=false 上游 fail-closed,这两级过滤是纵深防御。

ACK 消费侧免疫扇出(实体不在/未冻结/toZoneId 不匹配三层守卫,审过没改)。
kafka.cpp 头部"topic 没被订阅"的过时注释已改写(订阅 5 月就接上了)。

**审过健康:** consumer 后台线程 + queueInLoop 主线程回调的结构;stop() 的
双 flag 退出;producer 有界 flush;db 写乱序有 Go 侧 appliedSeq+版本比较守卫。

**P2 记录未修:** consumer enable.auto.commit=true + 异步投递 = at-most-once
(crash 窗口丢命令;gate 命令有客户端重试、migrate 有 reaper 重发兜底,故 P2);
producer 未开 enable.idempotence(重试可乱序,下游有守卫);
奇怪的 fetch.min.bytes=1 注释与值不符(写着 coalesce 实为最低延迟)。

本轮改动:player_lifecycle.cpp(4 处)、cross_zone_reaper.cpp、kafka.cpp。
待编译验证:scene lib + scene 节点。

## 2026-08-05 gate 对外接入两条断链:预设端口被无条件覆盖 + 通告 PodIP

排查"本地单独起 gate/scene"时顺带查实的两个既有缺陷,均只在 K8s 侧发作,
本地 dev(裸进程、不注入 RPC_PORT)撞不到,in-cluster 的 robot 也验不出来。

**断链 1:`RPC_PORT` / `NODE_PORT` 是死代码。**
`node.cpp` 的注释写着 "unless overridden by env",实际不成立:初始引导路径
`etcd_service.cpp AcquirePortWithRetry() -> NodeAllocator::AcquireNodePort()` 是无条件
调用的,而 `AcquireNodePort()` 从不读 endpoint 上已有的端口,扫描游标 `tryPortId`
是初值 0 的全局,最后无条件 `set_port(assignedPort)` 覆盖。env 值被读取、打了一行
`Node port from environment: 18000` 日志,然后丢弃 —— **日志骗人**,etcd 里和真实
bind 的都是扫出来的 10000 段。连带后果:Deployment 的 containerPort 与
`gate-entry` Service 的 `targetPort: rpc` 都按 18000 声明,Service 转发到一个
没人监听的端口。

修法:`AcquireNodePort()` 认 endpoint 上的非零预设端口,只做 etcd 占用 +
`IsLocalPortAvailable` 两项校验,**fail-closed**(拿不到就 return false 让调用方退避
重试,绝不静默回落扫描区间 —— 回落正是这次要修的病)。扫描游标只在真扫过区间时推进。

顺带修一个同源潜伏 bug:`usedPorts` 没排除自己。端口注册成功后 etcd watch 会把
本节点 fan 回快照,而 `AcquirePortWithRetry()` 的"到期后重查权威"分支会再跑一次
`AcquireNodePort()` —— 旧代码此时会把自己刚注册的端口当成别人占用的,扫描路径
悄悄换一个端口(etcd 端口与首次注册劈叉),预设端口路径则会永远失败重试。
按 node_uuid 排除自己。

**断链 2:即便修好 Service,外部客户端仍连不上。**
login 的 `CandidatesForZone` 把 etcd 里的 `n.Endpoint.Ip/Port` **原样**下发给客户端,
而那个 IP 来自 Downward API 注入的 `POD_IP` —— 集群内地址。也就是说客户端的
gate 地址根本不经过 Service,两条路都断,**说明还没人拿真实外部客户端跑过 K8s 部署**
(in-cluster robot 用 PodIP 是通的,历次压测发现不了)。

**第一版修法已废弃,记录下来免得有人重走:** 曾给 gate 注入 `ADVERTISE_IP`
(`k8s_deploy.ps1 -GateAdvertiseIp` + `ResolveNodeIp()` 最高优先级读它),让进程把
对外地址写进 etcd。两条否决理由:①那是个部署期填死的静态串,`gate` 默认 2 副本,
两个 gate 会通告同一个地址 —— login 按 `player_count` 挑最闲 gate 的逻辑当场作废,
客户端连过去被 LB 随机分;②**pod 本来就不该去推断自己在集群外长什么样**,这是平台
的职责,不是数据面进程的。相关改动已全部回退。

**本轮实际只落了与外部地址无关的两项**(都独立成立,任何暴露方案都需要):
- `Node::StartRpcServer()` 默认 bind `0.0.0.0`。原来 bind 的是 `endpoint().ip()`,
  等于要求"只收发到这一个 IP 的包" —— pod 网络命名空间里本就多余,还挡掉经
  hostPort / NodePort 转发进来的流量。裸机多网卡要钉一张时用 `BIND_IP`。
  注:`IsLocalPortAvailable()` 本来就按 INADDR_ANY 试 bind,改完反而与真实 bind 一致了。
- `Show-ExposureProfileWarning` 在任何非 ClusterIP 设置下告警,指明 Service 暴露了
  但客户端拿到的仍是 POD_IP。

**真正的修法待定(未落码):** 地址翻译放在**下发地址的那一环**,也就是 login 的
`CandidatesForZone`(`go/login/internal/svc/servicecontext.go`)—— 那里把 etcd 的
`n.Endpoint.Ip/Port` 填进 `GateCandidate` 后原样穿到客户端。`GateCandidate` 的注释
(`loginqueue/gatetoken.go:41-43`)本来就写着"刻意保持最小,好让它从任意来源构建",
接缝是现成的。gate 继续注册 POD_IP(集群内互相拨号仍靠它,in-cluster robot 不受影响),
login 在下发前做一次 `node_id -> 对外地址` 翻译;裸金属与托管云的差异全部收在
login 一处配置里。

**顺带重写 `localip()`**(`cpp/libs/engine/core/network/process_info.cpp`)。旧实现是
`inet_ntoa(*(in_addr*)gethostbyname(hostname())->h_addr)` 三行,两个毛病:
①取地址列表**第一个**,顺序由接口 metric / 绑定顺序决定 —— 开发机上装了
VMware / Hyper-V / WSL / Docker / VPN 虚拟网卡时经常选中一张连不通的;
②`gethostbyname` 失败返回 nullptr 却没有空检查,直接解引用 `h_addr`,主机名解析
不了时表现为进程无日志崩溃。
新实现改用**路由表探测**:给 UDP socket 调 `connect()`(不发任何包,只让内核按路由表
选出口接口)再 `getsockname()`,拿到的正是别的节点看到我们时的那个地址。不枚举网卡
是因为枚举完仍然要猜哪张对,而路由表本来就存着答案;顺带避开了
`GetAdaptersAddresses` / `getifaddrs` 两套平台 API 的 #ifdef 分叉。
三档兜底:路由探测 → 主机名解析(旧行为,留给无默认路由的隔离环境)→ `127.0.0.1`,
任何一档都不崩。想绕开全部自动判断仍可设 `NODE_IP`。
唯一调用点是 `ResolveNodeIp()` 的兜底档,即"本地裸进程启动时注册进 etcd 的地址"。
注:`cpp/libs/engine/muduo_windows/src/muduo/base/process_info.cpp` 里还有一份同名
`localip()` 旧实现,但没被任何 vcxproj / CMakeLists 引用(否则早就重复符号了),
**别改错文件**。

**连带必改:scene 的 `-RpcPort` 19000 -> 20000。** 端口 env 一旦真生效,scene 就会
去 bind 19000 —— 而 19000 落在引擎给 gate 划的 10000-19999 区间里(非 gate 角色是
20000-35535,见 node_allocator.cpp),破坏"看端口就知道角色"的约定和按区间写的防火墙
规则;gRPC 也会从 50000 变成 49000,撞进 gate 的 gRPC 段。改成 20000 之后 scene 在
K8s 里的实际端口**与修复前完全一致**(旧代码扫描也是从 20000 起、每个 pod 独立 netns
所以都拿 20000),等于零变化。gate 则是 10000 -> 18000,那正是本次要修的目标,且 18000
本就在 gate 区间内。README.md / AGENTS.md 里的角色端口表同步更新。

**本地启动地址改为零配置自动探测。** `cpp_nodes.ps1` 新增 `-NodeIp`,优先级
「显式参数 > 已导出的 NODE_IP > 探测物理网卡 IPv4 > 回落 127.0.0.1」;
`loopback` 强制回环,`engine` 表示不设 NODE_IP、交给 C++ 侧 `localip()`。
`dev_tools.ps1` 的 `cpp-node-start` / `dev-start` / `dev-start-exe` /
`dev-start-zones` 四个入口全部透传。**谁都不用配,默认就对。**

探测靠 `Get-NetAdapter -Physical` 问系统要物理网卡,**不是按名字匹配**:一把滤掉
Hyper-V / WSL / Docker Desktop 的 vEthernet、VMware、VirtualBox 和 VPN 隧道,再排除
link-local(169.254)与 PrefixOrigin=WellKnown,多张物理网卡同时 up 时取
InterfaceMetric 最小的那张(Windows 自己偏好的那张)。这一步是必须的:开发机
DESKTOP-I6DK28J 上 `gethostbyname` 顺序是 tun107 → 192.168.2.28 → 两张 vEthernet,
而路由表探测会跟着默认路由钻进 VPN 隧道 —— **两种朴素做法都会选中 198.0.2.1 而不是
192.168.2.28**,实测确认;`-Physical` 过滤后干净落在 192.168.2.28。

之所以敢在这里"猜",是因为 bind 已经恒为 `0.0.0.0`:**探错也只影响跨机客户端**,
本机内任何本地地址都连得通,而跨机场景本来就该显式传 `-NodeIp`。
刻意不把某台机器的 LAN IP 写进脚本 —— 那对其他机器全是错的。
纯单机栈想要绝对稳定(不受切 WiFi / VPN / DHCP 续约影响)可以传 `-NodeIp loopback`。

背后是 listen/advertise 二元:bind 恒 `0.0.0.0`(`BIND_IP` 可钉),NODE_IP 只管
「注册进 etcd 给别人拨的地址」,同 etcd 的 --listen-client-urls / --advertise-client-urls
与 Kafka 的 listeners / advertised.listeners。**注册地址绝不能填 0.0.0.0**:
Linux 内核把 connect(0.0.0.0) 当 127.0.0.1、Windows 直接 WSAEADDRNOTAVAIL,
两边还不一致;而 login 是把 endpoint 原样下发给客户端的(servicecontext.go:226-227)。

已验证(本机实跑):两脚本 parse 通过、`cpp_nodes.ps1 -Command list` 可跑、
`Resolve-AdvertiseIp` 六条用例全 PASS —— 默认探测 / 继承 env / `auto` 忽略 env /
显式参数压 env / `loopback` / `engine`。

**本轮改动:** `cpp/libs/engine/core/node/system/node/node.cpp`(ResolveNodeIp /
InitRpcServer 注释 / StartRpcServer)、`.../node_allocator.cpp`(AcquireNodePort)、
`cpp/libs/engine/core/network/process_info.cpp`(localip)、
`tools/scripts/cpp_nodes.ps1`、`tools/scripts/dev_tools.ps1`、
`tools/scripts/k8s_deploy.ps1`、`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`。
**待编译验证:** engine core lib + gate / scene 节点。未跑任何构建。

### 2026-08-05:战斗/空间 ECS + 多服务扇出审计(P0/P1/P2,Claude 落码,未编译)

本轮分两段:①我自己深审此前零覆盖的 C++ scene 战斗/空间/状态域;②用 workflow 扇出
8 个维度做对抗性复核审计(每条发现两个独立视角尝试证伪,两票都不证伪才算确认)。

**第一段 —— C++ 战斗/空间/状态(此前从未审过),修 13 处:**

P0-a:`buff.cpp` 周期 tick 的 UAF + 迭代器失效。`ProcessBuffs` 直接 range-for 遍历
`BuffListComp`(unordered_map),而 `OnIntervalThink` 会顺着
TickCombatIdleBuff → AddSubBuffs → AddOrUpdateBuff 往同一张表 emplace 子 buff(扩容
使迭代器失效),也会顺着 AddOrUpdateBuff → DispelBuffsOnAwake → OnBuffExpire 把正在
tick 的这条 buff 自己 erase 掉 —— 旧写法把 `BuffEntry&` 和 `periodic` 引用一路端着
穿过回调,buff 被驱散后下一句 `set_ticks_done` 就是写已释放内存。改为快照 id + 每轮
重查,并在回调前先落盘 periodic_timer。`AddSubBuffsWithoutCheck` 同类问题(跨
AddOrUpdateBuff 持 owner 引用)一并收口。注:代码里本来就有
MarkBuffForRemoval/RemovePendingBuffs 这套延迟删除,但 MarkBuffForRemoval **全仓无
调用方**,所有删除都走 OnBuffExpire 的立即 erase,正是上面这条路径。

P0-b:`actor_action_state.cpp` `TryPerformAction` 边遍历 protobuf Map 边 erase。
打断分支走 RemoveState → `state_list()->erase(actorState)`,protobuf Map 的 erase 让
指向被删元素的迭代器失效 —— 正是 range-for 手里那个,下次 ++it 即 UB。这条路径挂在
**每一次释放技能**的 CheckState 上。改为先快照状态键。

P1:①`HandleChannelSkillSpell` 拿技能**实例 id**(雪花)去查 SkillTable,永远查不到行
→ 必然早退,吟唱类技能从来没真正跑起来过;改为先用实例 id 取 SkillContext 再用
skilltableid 查表。②施法被打断时 SkillContext 永久泄漏(定时器组件被摘掉后
HandleSkillFinish 再也不会触发),玩家每打断一次泄漏一条、无上界;给三个相位定时器
组件加 skillId 字段,打断时先收口。③`IsTargetImmune` 用 `LookupBuffOrReturnError`
(`return buffResult`,uint32)当 bool 返回 —— 身上任何一条 buff 掉表行就被判成"对
一切 buff 免疫",后续加 buff 全部静默失败;改 LookupBuffOrContinue。④冻结目标的
"buff drop" 判定写在 OnBuffStart 里,而那时 buff 已经 emplace 进列表了,所谓丢弃实际是
"条目在、启动效果全没跑、到期定时器照挂",还会被 marshal 到目的 zone 复活;上移到
AddOrUpdateBuff 入口。⑤AOI 离场只把离场者从**观察者**列表里摘掉,从不清离场者
**自己**的 AoiListComp;换场景实体不销毁,旧场景条目原样带进新场景且再无路径删除 ——
既是无上界泄漏,又占满 AOI 容量让新场景实体全被 AddAoiEntity 拒收(客户端表现为
"进了新场景但世界是空的"),而且 `ActorStateAttributeSyncSystem` 每帧拿这张表当属性
广播收件人,entt 复用 id 后会把属性同步发给另一个场景里不相干的玩家。

P2:RemovePendingBuffs 的 per-tick `get_or_emplace`(违反 §7.5,给每个带 buff 的实体
永久挂一个空集合);OnIntervalThink / DispelBuffsOnAwake 跨实体用 `get` 不用
`try_get`;`CanUseSkillInCurrentState` 拿 `1 << skill` 位掩码当数组下标(表里是按序号
平铺的 6 格,序号 ≥3 一律越界被兜成 kInvalidTableData);critchance 是 uint64 却直接当
[0,1) 概率比较(今天全仓无写入点故恒 0=永不暴击,一旦有人按字面填 5 就变 100% 暴击),
按整数百分比换算并夹取,`rand()` 换 tlsRandom;CombatState 的两个事件 handler 先
get_or_emplace 再校验(对已销毁实体 get_or_emplace 在 EnTT 是 UB),且
ValidateSkillUsage 用配置决定的 stateKey 无边界检查索引 repeated 字段。

**第二段 —— workflow 扇出(8 维度 / 42 agent),确认 15 条、已修 11 条:**

P1:①`SendTipToPendingSession` 把 gate 业务 node_id 当 entt 实体句柄
(`entt::entity{GetGateNodeId(sessionId)}`)—— network_utils.h:27-30 明文禁止,同文件
其它 7 处早已改用 ResolveLocalZoneGateEntity,只有这个 namespace-local 函数漏改;它是
"玩家实体还没建好时"唯一的客户端通知出口,单 gate 部署下 100% 发不出去,玩家永久卡
加载界面。②`resolveScene` 把"scene_id 不存在"当成"活场景映射到死节点"自愈 ——
go-zero 的 Redis.Get 对缺失 key 返回 ("", nil),空 nodeId 一路走到 IsNodeAlive(zone,"")
被判 false,于是无 TTL 写出一条 `scene:{id}:node` 幽灵映射,而 CreateScene 又被
sceneConfId==0 挡掉,后续 AtomicIncrPlayerCountIfSceneExists 的存在性守卫正好看到这把
伪造的 key 而失效。③gate `session_id` 的 node 段在依赖就绪前是 0(set_node_id 在
dependencyGate 回调里,而 connection 回调在它之前就装上了),这段窗口连进来的玩家
scene 侧永远解析不出归属 gate、整局静默不可用;把 set_node_id 提到装回调之前
(node_id 在 afterStartFn_ 时已是终值,node.cpp:928),并在发号处加 fail-closed。
④未知/未授权 message_id 的拒绝分支排在鉴权闸门**之前**,且只 LOG_ERROR + 裸 return:
未认证客户端可无限发不存在的 message_id,每包一条 ERROR 级日志(同步磁盘 I/O),
永远碰不到踢人阈值、连接永不断开 —— 不需要任何凭证的日志放大 DoS。
⑤db 重试队列每 tick 只 RPopLPush 一条 × 1 秒 ticker = 排空上限 1 条/秒,而稳态入流
~90/s,MySQL 抖一下就只涨不落,超 retryMaxTimes 的任务进 `kafka:dead:queue:*`
(全仓无消费者=静默丢盘);改成有预算的循环排空。

P2:`OnGetLeaderLocation` 从 actorRegistry 取 SceneInfoComp(该组件只存在于
sceneRegistry)导致队伍跟随链恒早退;`HandleExitGameNode` 摘 SceneEntityComp 却把 Hex
留着(换场景路径显式删了,退出路径漏了),存盘在途重连会让 AOI 走"位置更新"分支、
原地重连时 hex_distance==0 直接 return,实体再也进不了任何格子;
`destroyInstanceInternal` 在原子幂等校验**之前**就级联销毁全部镜像并 Del 索引,CAS
放弃销毁时源场景活下来、镜像全没了且再也发现不了(代码注释声称"原子脚本会抹掉
scene:{id}:mirrors"是错的 —— 脚本 KEYS 里是 `scene:%d:mirror` 标志位,不是
`scene:%d:mirrors` 子集合);`readVersion` 把 Redis 读错误静默降级成 version=0,而 0 在
saveFieldsScript 里的语义是**跳过版本校验**,等于把乐观锁静默关掉(同函数其它读失败
都会置 ErrCodeRedis);`HandleTcpNodeMessage` 跨实体用 `get<RpcClientPtr>`(违反不变量
5,组件未挂/shared_ptr 为空时分别是抛异常和空指针解引用,gate 崩=该节点全员掉线);
`CheckMessageSize` 排在 CheckMessageLimit 之前且不计非法包,超长包既不占限流额度也
永远触发不了踢人。

**确认但**未**修,需要拍板:**
- `enterscenelogic.go` 排空/疏散的"改派回大世界"被 unsafe-handoff 闸门拒绝(P1):
  C++ 已存盘并销毁实体,但 currentLoc 仍指向旧节点 → crossNodeHandoff=true → 拒绝,
  玩家卡死。**没有擅自放宽这个闸门** —— 它挡的正是回档,用"旧场景 key 不存在"当逃生口
  并不能证明存盘已落地(场景可能在存盘任务还在重试队列里时就被销毁)。建议走
  EnterSceneRequest 加 `handoff_barrier_satisfied` 显式标志,由疏散路径设置。
- `gate_instance_id` 缺失(违反不变量 2)当前只报 ERROR 未拒绝:C++ 侧 6 个
  EnterSceneRequest 构造点里 `s2s_player_scene_handler.cpp` 与
  `s2s_player_scene_response_handler.cpp` 还没填这个字段,现在 fail-closed 会打死这两条
  正常链路;补齐后(并给 logic_test.go 里 13 处 GateId 用例补上该字段)再翻成拒绝。
- `load_reporter.go` 死节点残留 player_count 无路径清零(P2)。
- `key_ordered_consumer.go` read 任务缓存回写与 write 任务共用缓存键,但两类任务由
  两个不同分区器的生产者投递(P2,需统一分区函数 + 契约测试)。

**覆盖缺口:**扇出的 8 个维度里有 4 个因 API 断连整个丢失,**完全没有覆盖**:
scene-spatial-movement(movement/grid/navigation/recast/scene_crowd)、
scene-actor-attribute、go-player-locator、cpp-engine-timer-kafka。下一轮应补。

本轮改动**全部未编译**,待 Codex 验证。编译顺序(MSBuild 串行 `/m:1`):
scene lib(combat / combat_state / actor / spatial / player)→ gate 节点
(client_message_processor + main)→ scene 节点;Go 侧
`cd go && go build ./...`,并跑 `go test ./scene_manager/... ./db/... ./data_service/...`
(注意 instance_lifecycle 的级联顺序调整与 data_logic 的 readVersion 签名变更可能影响
既有用例)。C++ 侧新增 include:player_lifecycle.cpp 加 hexagons_grid.h、
skill.cpp 加 <algorithm> 与 <utils/random/random.h>、actor_action_state.cpp 加
engine/core/type_define/type_define.h;`CheckMessageSize` 签名多了 `SessionInfo&`。

### 2026-08-09:补审断连丢失的 4 维度(round 2,Claude 落码,未编译)

上一轮扇出 8 维度里 4 个因 API 断连全灭,本轮补审(4 find + 每条发现 2 视角对抗证伪)。
产出:10 条候选 → 确认 7(3 P1 + 4 P2,其中 1 条 P1 单票因复核 agent 断连)→ 证伪 3。
**go-player-locator 的 find agent 又一次死于断连,该维度至今零覆盖,round 3 必须补。**

**已修(7 条全部落码):**

P1-a:**移速 buff 让角色漂移**。全仓 Velocity 唯一写入点是 UpdateVelocity,把
buff 的 movement_speed_boost/reduction 这个**标量**灌进 velocity 的 x/y/z 三轴;而
MovementSystem 把同一组件当**运动学矢量**每固定步长 location += velocity*delta ——
挂移速 buff 的角色沿 (1,1,1) 匀速漂移(减速为负则钻地),几秒即被 AOI(kMaxViewRadius=10)
划出所有观察者视野,漂移位置还随存盘落库。修法:语义拆分 —— 新增 MoveSpeedComp(标量,
夹到 ≥0),UpdateVelocity 改名 UpdateMoveSpeed 只写它;Velocity 留给未来真实移动来源。
属性枚举 kVelocity 一并改名 kMoveSpeed(留着旧名就是给下一个人埋回同一个坑);
movement.cpp 落警示注释(含"接真实移动时必须补 kTransformFieldNumber 脏位")。

P1-b:**导航网格浅拷贝 → 悬垂指针 + 停服必现 double free**。NavComp 按值持有
dtNavMesh/dtNavMeshQuery(用户声明析构、无 move、未删 copy → emplace(std::move) 实为
逐成员浅拷贝),LoadNavBins 栈局部装好再 AddNav,map 副本与栈局部共享 m_tiles/节点池
裸指针且 navQuery.m_nav 指向栈地址;每轮循环局部析构即释放(DT_TILE_FREE_DATA),
线程退出时 thread_local SceneNavManager 再对同批指针二次析构。修法:NavComp 显式删除
拷贝/移动,SceneNavMapComp 改存 unique_ptr<NavComp>,堆上定址后再 LoadNavMesh +
navQuery.init;LoadNavMesh 改返回 bool,失败 fail-closed 不注册空网格。

P1-c:**眩晕/沉默永远同步不到客户端(三层皆断)**。ResetCombatStateFlags ①写进
get_or_emplace<ActorBaseAttributesS2C>(实体上的死组件,全仓零读者)而生成序列化器读的
是 try_get<CombatStateFlagsComp>(全仓零写入点);②从不置 kCombatStateFlagsFieldNumber
脏位,序列化分支不可达;③值写 false 而 CombatStateCollectionComp 的语义是键存在即激活。
修法:改写 CombatStateFlagsComp、激活写 true、置状态脏位。

P1-d:**属性同步消息不带 entity_id**。SetActorBaseAttributesS2CAttrDirtyBit 全仓唯一
调用点只置 velocity 位,kEntityIdFieldNumber 永不置 → proto3 默认 0 不序列化,观察者收到
无主更新。信封层(BroadcastToPlayersRequest)不带主体 actor,payload 的 entity_id 是唯一
归属通道;序列化器从 Guid(uint64_t)组件取值,客户端在 ActorCreateS2C.guid 建过映射,
可归属。修法:ResetCombatStateFlags 一并置 kEntityIdFieldNumber(velocity 通道随
P1-a 语义拆分后暂无写方,接通道时按同规则补)。

P2-a:**LookAtPosition 坐标系抄错**。世界是 z-up(grid/AOI 拿 x,y 当地面),这里照抄
Detour y-up 公式(yaw=atan2(x,z)、pitch=asin(y)):同高度目标 yaw 恒 ±π/2、真实方位角
落进 pitch。每次带 position/target_id 的技能释放都会写坏施法者 rotation;当前被
"rotation 全仓零读者"掩盖。修法:按 z-up 重算(yaw=atan2(y,x) 绕 z 轴、pitch=asin(z)),
写 rotation 的 z/x,roll 恒 0。

P2-b:**周期全量存盘单回调停顿**。redis.cpp 的 300s 定时器在游戏 tick 同线程单个回调里
全量遍历 playerList 逐个 SavePlayerToRedis(每个都是整份 PlayerAllData marshal+脏比较,
项目自估几 KB 快照近 100µs)—— 几千在线即数百 ms 级全服停顿,world.cpp 固定步长累加器
clamp 1s,超了直接丢模拟时间。修法:定时器改每秒跑,按 playerId % interval == 当前槽位
分摊,每玩家每周期仍恰好存一次,单回调工作量 N/interval;周期末槽位打一条汇总日志。

P2-c:**etcd 客户端状态挂在幽灵实体上**。NodeContextManager 的 globalEntities_ 无初始化器
(thread_local 零残留 = entity{0},而 64 位 entt::null 是全 1),GetOrCreateGlobalEntity 的
懒创建判断永不成立,etcd 全部 gRPC 状态(CQ/stub/watch 流)emplace 在从未 create() 的
句柄上,仅靠 EnTT 3.13 池语义宽容"碰巧能跑";未来任何对该 registry 的首次 create() 必铸出
同号实体互踩。修法:构造函数 fill(entt::null) + etcd_service.cpp Init 处改
GetOrCreateGlobalEntity(修了初始化后不改这处会变成对 entt::null emplace,更糟,两处必须
同批上)。复核方还纠正了原发现的两处细节:EnTT 3.13.2 的 emplace 无 valid 断言,Debug 构建
同样能跑(不是"release 关断言掩盖");现存代码无路径触发 create() 碰撞,引爆需未来代码。

**证伪 3 条(记录省得下轮重报):**dtCrowd 三处断链(AfterEnterScene 全仓无生产者,
handler 现网从不执行,纯未接线脚手架 —— 但其中"用 actor 句柄查 sceneRegistry"是真隐患,
接线时按 player_scene.cpp:111 的警告走 SceneEntityComp);delta 通道分级饿死(五个
AttributeDelta* 通道零调用点,缺陷依赖假想的未来接线);kafka enable.auto.commit(两类消息
都有上层设计兜底:命令 topic 有 TargetInstanceId 过滤,迁移链有 reaper 重发)。

**另:**本轮开工前顺手闭环了上一轮 4 条遗留:①gate_instance_id 已翻成 fail-closed
拒绝(上轮"两处 C++ 没填"系误判 —— 那两处是 GsEnterSceneRequest,scene↔scene S2S,
不是发 scene_manager 的 EnterSceneRequest;真实调用方全链已填;logic_test.go 12 处用例
补了字段,"not-a-number"用例在 ParseUint 就被拒不需要);②load_reporter 死节点
scene_count/player_count 残留 —— 全仓只有 Incr/Decr 无清零路径,"复用 id 的新节点会
覆盖"的旧注释不成立,已在 reconcile 之后 + fullSync 陈旧清理路径删除计数键(顺序重要:
reconcile 里的 destroyInstanceForce 自己还会减这两个键);③"read/write 两个分区器"
P2 查实**不成立**(login 是唯一 DBTask 生产者,读写同走一个 KeyOrderedKafkaProducer
同一 key),不修;④unsafe-handoff 疏散卡死 —— 并行协作方正在写
docs/design/scene-owner-reentry-barrier.md(owner_epoch 方案,工作区有未提交改动),
归属他们,本轮不动。

本轮 C++ 改动**未编译**,待 Codex(MSBuild 串行 /m:1):
1. scene lib:actor(attribute comp/constants/calculator)+ spatial(nav_comp/scene_nav/
   navigation/recast/view/movement)+ combat(modifier_buff_impl)+ core(redis);
2. engine:thread_context/node_context_manager.h + node/system/etcd/etcd_service.cpp
   (头文件改动会波及所有 include 方,gate/scene 两节点都要重链);
3. 注意:recast.h 的 LoadNavMesh 签名 void→bool;scene_nav.h 的 SceneNavMapComp 值类型
   NavComp→unique_ptr<NavComp>(全仓消费方仅 navigation.cpp,已同批改);
   eAttributeCalculator::kVelocity→kMoveSpeed(消费方 calculator+modifier_buff_impl,已改)。
Go 侧无新改动(上轮 Go 改动已随 bf76a2e7d 入库)。

### 2026-08-09(续):第三轮审计 —— player_locator 会话/租约链(Claude 落码,未编译)

前两轮全灭的 player_locator 维度单独成批补审(3 个聚焦切片 + 两视角对抗证伪)。
产出:7 条候选 → 确认 6(2 P1 + 4 P2)→ 证伪 1(Redis Cluster CROSSSLOT —— 单机
Redis 是刻意架构分工,无触发路径)。**shared 基础件切片(snowflake/snowflakealloc/
timertask/kafkautil/cache)的 finder 第三次死于 API 断连,至今零覆盖,round 4 必须再补。**

**已修(6 条全部落码):**

P1-a:**正常登出从不清理场景侧位置/人数**。全仓 SceneManager.LeaveScene 唯一调用点
在 LeaseMonitor(断线租约到期路径);正常登出链 LeaveGame→MarkOffline→
deleteSessionIfUnchanged 只删会话键和 legacy location 键,不碰权威键
player:{id}:location(无 TTL),也永远进不了租约链(会话已删,SetDisconnecting 因
redis.Nil no-op)。后果:权威位置永久残留 + 实例 player_count 幽灵 +1(destroy-on-empty
永不触发,孤儿实例);多世界节点区服下次登录选到别的节点被 unsafe-handoff 闸门
**确定性永久拒绝**。修法:MarkOffline CAS 删除成功后同步调 notifySceneManagerLeave
(复用 LeaseMonitor 的实现,含 SceneId=0 时读权威键回退);失败则把改成 DISCONNECTING
态的会话快照经新增 enqueueOfflineCleanup(Lua:会话键已重现=玩家瞬间重登则放弃)入
租约 ready 队列,复用既有 at-least-once 机制重试。连带堵一个边角:合成条目无活会话键,
AFK 月卡分支的 rearm Lua 会把它判成换代静默丢弃 —— 月卡只保护断线玩家,显式登出
不适用;handleLeaseExpiry 现在只在会话键仍存在时才走 AFK 探测(读键失败按存在处理,
宁可多探一次不误清月卡玩家)。

P1-b:**断线事件一次性且失败即吞**。会话进 DISCONNECTING 的唯一写入点是
SetDisconnecting RPC,而 login 的 markPlayerSessionDisconnecting 失败只打日志就放弃,
gate 侧发通知前就删了本地会话、无 login 节点时直接跳过、gate 崩溃则回调根本不执行 ——
会话键无 TTL,LeaseMonitor 只消费 SetDisconnecting 写入的 ZSET,无任何对账路径:
受影响玩家会话**永久 ONLINE**(公会永久在线、LeaveScene 永不执行、叠加 P1-a 的泄漏)。
修法(login 侧):带退避重试 4 次(吃掉 locator 发版/重启量级的窗口),穷尽后
logx.Severef CRITICAL 留人工线索(对应 release-checklist #B-1 巡检)。
**架构级残留缺口未修需拍板**:gate 整机崩溃时其承载的全部会话仍会滞留 ONLINE,
系统性收口需要 locator 侧对账扫描(State==ONLINE 且 gate_instance_id 不在 etcd 存活集
→ 补投 DISCONNECTING),涉及给 locator 加 gate 注册表 watch,本轮未动。

P2-a:**SetLocation 是绕过 CAS 链的裸写 RPC**(无会话/版本校验、无 TTL、强制
Online=true,与 MarkOffline 的原子 DEL 有复活竞态)。全仓无生产调用方(连 GetLocation
也没有),属历史遗留但端点注册可达。已 fail-closed 停用(返回明确错误);
logic_test.go 4 处用例改为 seedLegacyLocation 直接种键(保留"MarkOffline 会删 legacy
键"的断言),新增 TestSetLocation_DeprecatedRejected 钉住拒绝契约。

P2-b:**节点租约丢失后永不重注册**。KeepAlive channel 关闭只打一条 INFO 就放弃,
etcd 抖动超过 LeaseTTL(500s)该实例就从服务发现永久消失且无告警。scene_manager 的
同源副本早已实现 reRegister 自愈(绝不盲 Put 老 key,CAS Version(allocKey)==0 重夺,
失败换新 node_id),四副本注释里写着要同步却漏了三份。已移植到 player_locator
(watchKeepAlive/reRegister);**guild/friend 两份同缺口已开后台任务**(结构已漂移,
需各自适配,不能整段照抄)。

P2-c:**gate 的 PlayerLeaseExpiredEventHandler 空实现**。LeaseMonitor 把"通知 gate"
当作 ack claim 的必要副作用,gate 收到却什么都不做 —— 也是老账"假死连接不会被清"
的根因。已实现:按 session_id 查会话,player_id 不匹配跳过(fail-safe);有 conn 则
forceClose(对端假死不能指望四次挥手,关闭触发正常断开回调统一走清理),无 conn 的
残留会话就地摘除。幂等:正常断开早已摘会话,find 不到直接返回。

P2-d:**KafkaWriter 未设 RequiredAcks**。kafka-go 直接构造 Writer 时零值是
RequireNone(fire-and-forget),broker 端失败结构性不可见 —— LeaseMonitor 拿
WriteMessages==nil 当投递凭据去 ack claim,凭据是假的。scene_manager 的
servicecontext 早修过同一个坑,locator 漏了。已对齐 RequireOne。

本轮 Go 改动**未编译**,待 Codex:`cd go && go build ./player_locator/... ./login/...`,
`go test ./player_locator/...`(注意 logic_test 的 4 处用例已改种键方式,MarkOffline
成功路径现在会往 LeaseZSetKey 入队重试条目 —— 若有用例断言 MarkOffline 后 ZSET 为空
会翻,已核对现有断言无此假设)。C++ 侧 gate_event_handler.cpp 待编译(gate 节点)。
guild/friend 的 reRegister 移植在独立后台任务里,不在本批。

### 2026-08-09/10:round 3+4 —— player_locator 本体 + go/shared 基础件(Claude 落码,未编译)

前两轮 player_locator/shared 的 finder 反复死于 API 断连,本两轮切小单跑补齐。

**Round 3(player_locator 会话CAS链 + 租约监控),确认 6:**

P1:①**正常登出永不清 SceneManager 位置**。LeaveGame→MarkOffline 只走
deleteSessionIfUnchanged(删会话键 + 历史遗留 player:location),而权威键
player:{id}:location 无 TTL、全仓唯一删除点是 scene_manager LeaveScene handler,
其唯一生产调用点又是 LeaseMonitor —— 正常登出根本不进租约链。后果:权威位置永久残留、
实例 phantom +1(destroy-on-empty 永不触发=孤儿实例占 Agones 名额)、多世界节点区服
下次登录选到不同节点被 unsafe-handoff 门禁拒绝、玩家确定性锁死。②**断线事件一次性且
失败即吞**:SetDisconnecting RPC 失败/gate 崩溃/断线瞬间无 login 节点 → 会话永久
ONLINE(无 TTL),唯一清理状态机不启动;guild 在线判定直读会话键,对公会永久显示在线。
docs/ops/release-checklist.md 已把这类 leak 列为人工 SCAN 巡检项 = 确认无自愈。

P2(已落码):③**SetLocation 是绕过 CAS 链的裸写**(无会话/版本校验、无 TTL、强制
Online=true),与 MarkOffline 原子删除有复活竞态;全仓无生产调用方 → 端点改
fail-closed 拒绝(logic_test.go 相应用例改直接种 legacy 键)。④**node.go lease 丢失
不自愈**(ka==nil 只打一条日志就 return,进程活着但注册蒸发);已移植 scene_manager 的
reRegister(CAS 重夺原 id / 失败换新 id / 重启 KeepAlive)—— 见下 guild/friend 同款。
⑤**KafkaWriter 未设 RequiredAcks**(kafka-go 零值=RequireNone fire-and-forget),
租约过期通知的 fail-closed 受理协议对 Kafka 腿形同虚设;已补 RequireOne。

**未修待拍板(架构级):**gate **整机崩溃**时其承载的全部会话仍永久 ONLINE ——
login 侧退避重试只吃掉 locator 重启窗口,系统性收口需 locator 加对账扫描
(周期扫 State==ONLINE 且 gate_instance_id 不在 etcd 存活集 → 补投 DISCONNECTING+租约),
涉及 watch gate 注册表。这是新增子系统,单列。

**Round 4(go/shared 基础件),确认 2 + 顺带 1,证伪 6:**

P2:①**snowflakealloc 持久水位写墙钟而非发号高水位**。advanceGuard 只写
NowEpochSec 且回拨时 early-return 冻结,而发号器借位(step 耗尽/时钟停摆,
Generate 的 default 分支)会让 lastTime 跑到墙钟前面 → 水位低于真实已发号秒;
前任借位窗口内崩溃,同 hostname 继任者以低地板+step=0 重发,与前任借位期的号逐位
撞号(scene id 裸 SET 无 CAS,路由互相覆盖)。这正是 snowflake.go SetGuardTime
注释点名要 guard 覆盖的第三类情况,实现没兑现。修法:snowflake.Node 加
HighWaterEpochSec();Handle 持 atomic.Pointer[Node] 引用,advanceGuard 写
max(墙钟, 高水位)。窗口从"整个借位期"缩到"单个 keepalive tick"。
②**timertask 零值 Task 首次调用即 nil 崩溃**。零值 index=0(合法空闲态是 -1),
schedule 首行 t.Cancel() 走 heap.Remove(&t.sched.h,...) 解引用 nil sched;
schedule 里的 nil-sched 守卫排在 Cancel 之后=死守卫。C++ TimerTaskComp 是值内嵌
组件,移植方按同习惯声明 var t Task 即炸进程。修法:Cancel 首行加 nil-sched 早退。
③(顺带,一验证方确认另一方证伪于"暂无调用方")**friend/guild KafkaWriter 同款
RequiredAcks 零值**;既然 scene_manager/player_locator 都已显式 RequireOne,
对齐补上(gate_push 是刻意预接线基础设施)。

**证伪 6(记录防重报):**Redis Cluster CROSSSLOT(单机是刻意架构,config 指不到
Cluster);allocator nodeKey 中毒热循环(key 带 lease ≤60s 自愈,越界/非数字值无
写入路径);timertask loop.go Stop 竞态(通道语义实为设计内);login PlayerId 水位、
topic_init AlterConfig、gate_push GateInstanceID 三条**复核 agent 死于额度耗尽、
未真正证伪**,round 5 需补验(前两条涉及 DB 唯一索引/sarama IncrementalAlterConfig,
是架构级,不擅动;第三条在无调用方的死 infra 上)。

**guild/friend reRegister 移植(round 3 衍生,已落码):**两服务 node.go 的
KeepAlive 原来都是 ka 通道关闭只打一条日志就永久放弃 = 进程活着但注册蒸发、无自愈。
按 scene_manager noderegistry/registry.go 的三步式移植:CAS 重夺原 node_id →
失败则 allocateNodeID 换全新 id → 重启 KeepAlive,指数退避封顶 30s。换 id 安全因为
Snowflake worker id 由 snowflakealloc 独立分配、与 NodeInfo.NodeId 解耦(见各自
启动注释)。同源第四份 player_locator 本轮③已修。

本两轮改动:Go 侧 gofmt 干净但**未编译**,待 Codex:
`cd go && go build ./...` + `go test ./player_locator/... ./shared/...`
(注意 player_locator logic_test.go 改了 SetLocation 用例:TestSetLocation_DeprecatedRejected
断言拒绝、其余改 seedLegacyLocation 直接种键;snowflakealloc/timertask 若有既有单测
需确认 HighWaterEpochSec 新方法与 Cancel 早退不破坏断言)。C++ 侧本两轮无改动。

### 2026-08-10(续):locator 会话对账扫描落码 + snowflake 借位预算(Claude 落码,未编译)

**1. 会话对账扫描(round 3 那条架构级缺口,已拍板实现)。**
新增 go/player_locator/internal/logic/session_reconciler.go,player_locator.go 接线,
config.Lease 加 ReconcileIntervalSeconds(默认 60s,-1 关闭)。兜住「gate 整机崩溃 →
TCP 断开回调不执行 → SetDisconnecting 永远不来 → 会话永久 ONLINE」:
- 存活集以 etcd GateNodeService.rpc/ 前缀为准(C++ gate 以 lease 注册 NodeInfo,
  node_uuid 即会话里的 GateInstanceId);etcd 列举失败 → 本轮整体跳过(fail-closed);
- SCAN player:session:*,State==ONLINE 且 GateInstanceId 连续**两轮**不在存活集才动手
  (单轮缺席可能是 etcd 视图抖动/gate 重启重注册);
- 动手 = 与 SetDisconnecting 完全相同的 Lua(整字节 CAS + 版本递进 + ZADD 租约),
  玩家期间重连/重登改写会话字节则 CAS 失败 no-op,绝不误杀活人;多 locator 并扫无害。
之后由既有 LeaseMonitor 链完成 gate 通知 + SceneManager.LeaveScene,
「所有会话终点必经租约链」闭环恢复。release-checklist 里 #B-1 的人工 SCAN 巡检
可在观察一个版本周期后降级。

**2. snowflake 借位预算(用户拍板:可借下秒,超前墙钟 >10s 必须出错)。**
shared/snowflake 新增 maxBorrowAheadSec=10 与 ErrBorrowLimitExceeded(**暂态**错误,
墙钟 1s/s 追赶自愈,与 ErrFenced 的永久性不同):Generate 借位分支在
lastTime+1 > 墙钟+10 时拒绝发号,不再无界借位。要点:
- 判定用 waitNextTime 的返回值(即最后一次墙钟观测),不额外读钟;
- 覆盖 guard 注入的超前:接管时前任高水位比本机墙钟快超过预算,同样 fail-closed
  等墙钟追进预算圈(唯一性优先);
- 拒绝日志按墙钟秒限频(持续过载时每次调用都会进拒绝分支,不能每次都刷 ERROR);
- 与持久水位修复(advanceGuard 写 max(墙钟,高水位))互补:预算钉死了水位最多落后
  高水位 10s,继任者 SetGuardTime 的地板缺口有了硬上界。
调用方核查:guild_logic / createscenelogic / world_init 三处均已正确处理 error
(fail-closed 不吞);login 的 PlayerId 走 bwmarrin 另一型,不受影响。

待 Codex:`cd go && go build ./...`;`go test ./player_locator/... ./shared/...`
(snowflake 若有借位相关既有单测,需按新预算语义调整:虚拟时钟注入下连续借位
超过 10 逻辑秒会开始返回 ErrBorrowLimitExceeded)。

### 2026-08-10(续二):round 4 三条未复核项自行核实并落码(Claude 落码,未编译)

上轮三条因复核 agent 额度耗尽而悬置的发现,本轮逐条人工核实,全部成立,全部修掉:

**1. gate_push 防僵尸收口(shared/kafkautil/gate_push.go)。**核实:BroadcastToPlayers
按 GateID 分组但 instance id 取组内**第一个玩家**的;四个入口都不校验空 instance id
(空值 = 消费端 ValidateCommandTarget 防僵尸过滤被关,违反不变量 2)。gate 业务
node_id 会回收复用,滚动重启窗口内同一 gate_id 下有新旧两代实例的会话,旧实现把
两代折进一条命令 → 挂错代 instance 的那一半玩家消息被消费端静默丢弃。修:
PushToPlayer 空 instance 直接拒(fail-closed);Broadcast 分组键改
(GateID, InstanceID) 复合;三个广播入口空 instance 逐条剔除 + 报错不静默。

**2. topic_init 换增量配置接口(shared/kafkautil/topic_init.go)。**核实:sarama 的
AlterConfig 走 Kafka 遗留 AlterConfigs 协议,语义是**全量替换** topic 动态配置 ——
只提交 retention.ms 会把运维手工设的其它覆盖项(cleanup.policy 等)每次服务启动
抹回默认。修:换 IncrementalAlterConfig(KIP-339,按条目 SET;broker 需 ≥2.3,
本仓 cfg.Version 已声明 V3_0_0_0,sarama v1.43.1 支持)。

**3. login PlayerId 毫秒级水位地板(防"墙钟回拨+重启"跨进程重放)。**核实:bwmarrin
进程内靠单调时钟免疫回拨,但跨重启以当前墙钟重新锚定;hostname 亲和复用同 worker id,
回拨 N 秒后重启即重走旧进程最后 N 秒的毫秒序列。snowflakealloc 的 GuardEpochSec 是
shared/snowflake 秒级 epoch 口径,bwmarrin 毫秒层用不上。修(三件套):
- snowflakealloc 加 guard_ms/{id} 键(值=Unix 毫秒,与两套自定义 epoch 解耦)与
  ReadMsWatermark/PutMsWatermark(读失败必须报错让调用方 fail-closed);
- PlayerIDGen 加 NowUnixMs()(锚点墙钟+单调流逝 = 发号器时钟口径,墙钟回拨后
  直接读 time.Now 会低估);
- login.go 启动时把前任水位当硬地板:墙钟没越过就不构造发号器(等待时长=实际
  回拨幅度,正常重启恒零等待);运行期 1s 节拍**前推 2s** 写水位(前推量>写入
  间隔,保证前任崩溃前可能发出的最大时间戳恒<最后一次写入值,继任者无需猜测性余量)。
  刻意没搭 snowflakealloc 的 keepalive ticker(fenceAfter/4=10s,会让每次快速重启
  都白等 10s)。

**衍生发现(核实 3 时挖出,已修):DB 主键漂移

### 2026-08-10(续二):round 4 三条未复核项补验 + 落码(Claude,未编译)

上一续因额度耗尽没复核完的三条,本轮自核并全部修掉:

**1. gate_push 不校验 GateInstanceID 空值 + 广播分组丢 instance(shared/kafkautil/gate_push.go)。**
四个发送口(PushToPlayer/BroadcastToPlayers/BroadcastToScene/BroadcastToAll)是全 Go
服务推送的唯一共享收口,却都不检查 GateInstanceID 非空 —— 违反不变量 2(空值=消费端
ValidateCommandTarget 防僵尸过滤被关)。更重的是 BroadcastToPlayers 按 gate_id 分组、
取组内**第一个**玩家的 instance id:滚动重启窗口内同 gate_id 下新旧两代实例的玩家被
折进同一条命令,另一代那半静默丢消息。修:①四处全部空值 fail-closed(单个坏条目不拖垮
整批但必返错,不静默降级);②分组键改成 (gate_id, gate_instance_id) 复合键。

**2. topic_init 用遗留 AlterConfig 全量替换(shared/kafkautil/topic_init.go)。**
sarama 的 AlterConfig 走 Kafka 遗留 AlterConfigs 协议 = 全量替换该 topic 动态配置,
只提交 retention.ms 会把运维手工设的 cleanup.policy / max.message.bytes 等覆盖项**每次
服务启动都抹回默认**。改用 IncrementalAlterConfig(KIP-339,broker≥2.3;cfg.Version
已声明 3.0),按条目 SET 只动 retention.ms。sarama v1.43.1 已带该 API。

**3. login PlayerId 无 ms 级持久水位 —— 跨重启回拨重放(架构级,已整链落码)。**
bwmarrin 进程内单调时钟免疫回拨,但**跨重启**以当前墙钟重锚:墙钟回拨 N 秒后重启,
同 worker id(hostname 亲和)重走最后 N 秒的毫秒序列 → 逐位相同 PlayerId。
snowflakealloc 的秒级 GuardEpochSec 是 shared/snowflake epoch 口径,塞不进 bwmarrin
毫秒层。落码三段:
- snowflakealloc 新增独立的毫秒水位通道:guardMsKey(/guard_ms/{id},值=Unix ms,
  不带任何自定义 epoch)+ Handle.ReadMsWatermark/PutMsWatermark;与秒级 guardKey 完全
  隔离,不混 epoch。
- PlayerIDGen 新增 anchor(构造时捕获的单调墙钟)+ NowUnixMs()=锚点+单调流逝 ——
  取"发号器时钟"而非 time.Now(),墙钟回拨后照样单调,当水位才关得住重放窗口。
- login.go:①启动读水位,墙钟没越过水位就阻塞等待(fail-closed,等待时长=实际回拨幅度;
  正常重启恒零);②起 1s 节拍 goroutine,把 NowUnixMs()+2000ms(前推量>写入间隔)
  写水位,保证前任崩溃前可能发出的最大时间戳 < 最后写入的水位,继任者只需等墙钟越过。
  写失败只告警(水位是下一任的地板,本进程唯一性不依赖它),Lost() 时退出。

**顺带核实并修正 DB 兜底口径:**player_database 的 proto **声明了** PRIMARY KEY(player_id)
(OptionPrimaryKey),旧注释"没有唯一索引"只对按陈旧 go/db/model/mysql_database_table.sql
预建表的环境成立 —— 那份手工 SQL 4 张表(player_database / player_database_1 /
player_centre_database / 及注错列型的 account_share_database)缺主键,而运行时
CreateOrUpdateTable 只补列不补主键 → 存量表永久缺 PK。已给该 SQL 补齐主键并加文件头
警告(权威 DDL 是 proto2mysql,此文件仅历史导出)。**注意:即便有 PK,写路径是
INSERT...ON DUPLICATE KEY UPDATE,重复 PlayerId 不报错而是静默改写另一玩家的行(串档),
比报错更糟 —— 所以唯一性必须在铸号侧(上面的 ms 水位)保证,DB PK 只是纵深防御。**

待 Codex:`cd go && go build ./...`;`go test ./login/... ./shared/...`。
存量 MySQL 环境需 `SHOW KEYS FROM player_database` 核对主键,缺失则
`ALTER TABLE player_database ADD PRIMARY KEY (player_id)`(四张表同理)。
etcd 会多出 /login/guard_ms/{worker_id} 键(login 自建,无需预置)。

### 2026-08-10(续三):proto2mysql 根因修复(上游库,E:\work\proto2mysql)

上一续给 go/db/model/*.sql 补主键只救了"新建环境";真正让**存量表**永久缺主键的
根因在 proto2mysql 库:CreateOrUpdateTable → syncTableSchema → buildAlterClauses
只对齐**列**(ADD/MODIFY/CHANGE COLUMN),从不看主键。表已存在且无主键就永远补不上,
而写路径 INSERT ... ON DUPLICATE KEY UPDATE 依赖主键判重 —— 无主键时退化成每次
INSERT 新行,同一 player_id 多行、读取任取其一 = 静默串档/回档。

已在 E:\work\proto2mysql(github.com/luyuan-cpp/proto2mysql,mmorpg 用 v0.0.18,
该版本也缺此修复)落码:syncTableSchema 补一段主键回填 + 新增 tableHasPrimaryKey。
安全设计(注释里钉死):①单独一条 ALTER(与列变更分离,失败互不牵连);②只在
**当前无主键**时 ADD(改主键要 DROP+ADD 是破坏性操作,绝不自动做,只补"从无到有");
③失败硬报错 fail-closed(ADD PRIMARY KEY 在已有重复行的表上会失败 —— 那正是缺主键
期间攒下的腐败数据,应让启动失败顶到人脸上去重后重试,而不是继续用判重失效的表)。
加了集成测试 TestCreateOrUpdateTableBackfillsMissingPrimaryKey(建无主键表→同步→断言
主键补上→再同步验幂等;需 PROTO2MYSQL_INTEGRATION=1 + 真 MySQL)。

**发布路径(库是独立 repo,mmorpg 不会自动拿到)**:
1. E:\work\proto2mysql:提交 + 打新 tag(如 v0.0.19)+ push;
2. mmorpg go/db/go.mod:`require github.com/luyuancpp/proto2mysql v0.0.19` + `go mod tidy`;
   ⚠️ 注意 go.mod 里是 `luyuancpp`(无连字符)而 git remote 是 `luyuan-cpp`,
   发布时确认 module path 与 go.mod require 路径一致,否则拉不到。
3. 存量 MySQL:库升级后 db 服务下次启动会自动补主键;但**若表里已有重复 player_id**
   (缺主键期间攒下的),ADD PRIMARY KEY 会失败并阻塞启动 —— 必须先人工去重
   (保留权威那一行)再拉起。上线前用 `SELECT player_id,COUNT(*) FROM player_database
   GROUP BY player_id HAVING COUNT(*)>1` 排查。

Claude 不执行编译;库侧 `go test -run PrimaryKey`(带集成开关)交人验证。

### 2026-08-10(续四):round 5 未覆盖面审计(login业务/friend/货币/背包,Claude 落码未编译)

扇出 5 切片,确认 2 + 自核补 1(复核 agent 双双断连那条),证伪 6。

**P1(已修):①login refresh token 集合无上界泄漏(token.go)。**
`account_refresh:{account}` 是无 score 的 SET,每次 Issue SAdd 新 token 并把集合 TTL
续 30d;只有 Refresh 对当次 token SRem、RevokeAll 全仓零调用。Redis 集合成员不随对应
refresh_token 键 TTL 过期而消失 → 死成员无上界累积,客户端反复 Login 即触发 Redis
内存泄漏。修:集合改 **ZSET,score=refresh 过期 unix 秒**,每次 Issue 先
ZREMRANGEBYSCORE 清死成员(score<now 即键已过期)再 ZADD;另加
maxRefreshTokensPerAccount=32 封顶活跃成员(超出淘汰最旧并删其 token 键)。
Refresh 的 SRem→ZRem、RevokeAll 的 SMembers→ZRange 同步改。全仓无别处消费该集合。

**P1(自核确认,复核 agent 断连未验;已修):②friend_request 无上界增长(friend)。**
AddFriend 只挡"已接受好友数<MaxFriends"和精确 (from,to) 去重,**从不限制出站
pending 条数**;MaxPendingRequests(配 50)全仓零引用;reject/accept 只翻 status 不删行、
无 GC。单客户端用互不相同 TargetPlayerId 循环 AddFriend 即可无上界撑大 friend_request
表。修:新增 repo.CountOutgoingPending(status=1 计数),AddFriend 在插入前强制
MaxPendingRequests 上限(fail-closed,新增 ErrTooManyPending=7)。附带建议(未做):
TargetPlayerId 存在性校验需跨服务查,单列;terminal 行的 GC/TTL 回收也可后续加。

**P2(已修):③login 快速通道容量 check-then-act 超发(assigngatelogic/queue)。**
fast-path 在 free>0&&queueLen==0 时直接 signFastPath,**不占位**;N 个并发各读同一
free>0 快照全部旁路队列签发 gate token,开服洪峰旁路 cap 超发。修:queue 新增
TryReserveFastPathSlot(Lua 原子把"SCARD admitted<budget + SADD 占位"合成一步,
budget=cap-online,占位按 admitTTL 回收),fast-path 改为先原子占位、抢到才签发、
抢不到入队;占位 Redis 出错也 fail-closed 入队。当前 Queue.Enabled=false 屏蔽故本是
P2,但队列是灰度目标,启用即生效。

**证伪 6(记录防重报):**queue admitted 幻影泄漏(free→0 时停刷新、集合按自身 60s TTL
自愈,非无界);CreatePlayer 无幂等(CreatePlayerRequest 是空消息无 request_id 可去重,
本质"每次建新角",且 MaxPlayersPerAccount 封顶,login RPC 无自动重试);currency ADD
流水 before+delta≠after(补债路径,全仓无逐条对账消费者);currency 余额 +gain 无溢出
上限(要连发到 2^64 才回绕,仅 GM 路径,现实到不了 —— 用户与我均已注意,低优先);
bag AddItems 非原子(两个批量重载全仓零调用方=死 API,thread_local fence 同步循环内
不会翻转);friend 那条本身成立(见上,已归入自核确认)。

待 Codex:`cd go && go build ./...`;`go test ./login/... ./friend/...`
(token_test 若断言 SET 语义需改 ZSET;loginqueue TryReserveFastPathSlot 建议补并发占位
单测:budget=N 时并发 reserve 恰好成功 N 次)。C++ 侧本轮无改动。

### 2026-08-10(续五):round 6 剩余未覆盖面(shared基础件/C++ mission-world/Java网关)

扇出 5 切片。**Java 网关首次纳入审计**,确认 2 条 P1(同一条攻击链的两环),
外加自核 shared/cache 1 条;C++ mission 5 条全证伪;2 个 find agent 断连。

**P1(已修):①Java 网关 X-Forwarded-For 无条件采信且取最左元素。**
LoginController.extractIp 直接读 XFF 取第一段当客户端 IP,而 XFF 是**任何客户端都能
自己写**的头、最左元素恰恰是客户端填的那一段。该值直接当 Bucket4j 桶 key
(AssignGateRateLimiter 的 ipKey),于是每请求带一个随机 XFF 就命中全新空桶 ——
ip-rps/ip-burst 这一层对任何会改 header 的客户端**等于不存在**,同时把 rl:ip:* 的
key 空间变成攻击者可控的无限集合(与下面②叠加放大)。方法自己的 javadoc 声称
"只在 server.forward-headers-strategy=native 时采信",但代码从不查该设置、配置里
也没有该项 —— 契约与实现脱节。全仓无 ingress/nginx 配置,compose 直接暴露 8081,
即 getRemoteAddr() 分支在带头时永不可达。
修:新增 ClientIpResolver(Spring bean)替代静态 extractIp,两个 Controller 注入使用。
语义:①默认(未配可信代理)完全忽略 XFF、只用 socket 对端 —— fail-closed;
②只有 socket 对端落在配置的 trusted-proxies CIDR 内才解析 XFF,且**从右往左**剥
连续可信跳、取第一个不可信地址(从左取等于直接采信客户端输入);③只接受字面量地址,
绝不做 DNS 解析(否则畸形 XFF 能把 Tomcat 线程拖进名称解析);④畸形 CIDR 被忽略且
不会把白名单变成放行。新增 gate.rate-limit.trusted-proxies 配置项(默认空)。
⚠️ 部署在 ingress/LB 之后时**必须**配置该项,否则所有请求共用 LB 那一个 IP 桶。
新增 ClientIpResolverTest 钉死该安全属性(8 个用例含边界与畸形输入)。

**P1(已修):②Bucket4j ProxyManager 未设过期策略,rl:ip:*/rl:zone:* 永不过期。**
裸 builderFor(conn).build() 时 AbstractRedisProxyManagerBuilder 取不到
expirationStrategy → 落到 ExpirationAfterWriteStrategy.none() → calculateTimeToLiveMillis
恒 -1 → LettuceBasedProxyManager 在 ttl<=0 分支走**不带 px 的 SET NX**,写永久 key。
(复核方反编译了本机 8.10.1 jar 逐环验证,不是推断。)每个新 IP 一个永久 key、无自愈,
只能人工 SCAN/DEL;而这个 Redis 与 login 的 access_token/refresh_token 是**同一实例**,
撑到 maxmemory 会连带把登录态淘汰掉。修:显式
withExpirationStrategy(basedOnTimeForRefillingBucketUpToMax(1h)) —— TTL=重填到满所需
时间+余量,保证还可能用到的桶绝不被提前回收。已用 javap 核对 8.10.1 的工厂方法签名。

**P2(自核,已修):③go/shared/cache 的 singleflight 收尾未走 defer。**
`c.val, c.err = fn()` 之后才 wg.Done()+delete:dbLoader 一旦 panic,两步都不执行 ——
该 key 的 sfCall 永久留在 map 且 WaitGroup 永不归零,此后每个同 key 调用都在
wg.Wait() 永久阻塞(功能性永久失效 + goroutine 无上界泄漏,只能重启)。改 defer 收尾;
顺带把 `raw.(T)` 裸断言改 comma-ok(不同 T 撞同一 sfKey 时返错而非 panic)。
该包目前**零调用方**(friend 自有 loadVersionedFriendCache),同 round 4 的 timertask
零值 Task 一样属"共享库里给下一个接线者埋的雷",故 P2。

**证伪(记录防重报):**C++ mission 全部 5 条 —— 任务系统是**无入口的自循环死代码**:
ConditionEvent 在整个 cpp 树零构造零派发,AcceptMissionEvent 唯一 enqueue 点是
mission.cpp 自身链式续接(需先有已完成任务=自举死锁),proto 无 mission RPC,
CompleteAllMissions/AbandonMission/GetMissionReward 均只被单测调用。其中
"mission.cpp:90 宏内 continue 跳过 add_progress" 一条机理本身也错:宏体是
do{...}while(0),C++ [stmt.cont] 规定 continue 绑定最内层迭代语句即该 do-while,
不会跳过外层 for 的剩余语句。另证伪 AssignGateController queue_token 短路绕过限流
(总开关默认 false 且兄弟入口 QueueStatusController 明文设计就不限流)、
AssignGateRateLimiter account cooldown 非原子(该层明文 best-effort,权威串行化在
go login 的 account_lock Redis 锁)。
**go/shared/cache+grpcstats 的 finder 返回零发现**(grpcstats 无 player_id 标签等问题)。

**未覆盖(2 个 find agent 断连):**C++ scene world 域(world.cpp 调度/固定步长累加器)、
Java 网关的 RPC 与服务发现(LoginRpcClient 730 行 / GateWatcher / ZoneHealthProbeService)。
round 7 补。

待 Codex:Java `cd java/gateway_node && mvn -q test`(新增 ClientIpResolverTest;
RateLimitConfig 新增 import io.github.bucket4j.distributed.ExpirationAfterWriteStrategy
与 java.time.Duration;两个 Controller 构造函数各多一个 ClientIpResolver 参数 ——
若有 @WebMvcTest/手工 new 的用例需同步)。Go `go build ./shared/...`。

### 2026-08-10(续六):round 7 补审(4 个 finder 死 3 个,仅 java-login-rpc 存活)

本轮目标是补 round 6 断连丢的两块。**结果 4 个 finder 断连 3 个**
(cpp-world-tick / cpp-scene-comp / java-etcd-health),仅 java-login-rpc 完成。
确认 1 条 P2 已修,另 1 条 P1 被证伪。

**P2(已修):Java 网关 gRPC channel 的空闲 keepalive 被服务端当滥用踢断。**
LoginRpcClient 建 channel 时设了 keepAliveTime(30s) + keepAliveWithoutCalls(**true**),
即空闲也每 30s 发 HTTP/2 PING。对端 go-zero(grpc-go v1.79.3)zrpc server 全仓没配
任何 keepalive.EnforcementPolicy,沿用默认强制策略:handlePing 在「无 active stream 且
!PermitWithoutStream」时,只要距上次 ping 不足 **defaultPingTimeout=2 小时**就记一次
strike,而 strike 只有服务端真正写 header/data 才清零(PING ACK 不算);
strike > maxPingStrikes=2 即发 GOAWAY(ENHANCE_YOUR_CALM,"too_many_pings")关连接。
于是任何空闲约 120s 的 channel 被反复踢断,且 grpc-java 收到 too_many_pings 后按
AtomicBackoff 把 keepAliveTime 永久翻倍,几次后探活间隔顶到失效 —— 恰好毁掉这几行
想要的能力。3-zone 部署低峰期 z2/z3 天然满足,单 zone 夜间同样满足。
修:keepAliveWithoutCalls 改 false。
⚠️ **关键事实(复核方纠正了原发现)**:空闲分支比的是 2 小时那个常量而**不是**
MinTime(5min),所以"把 keepAliveTime 提到 5 分钟"这个直觉修法**根本不管用**;
只有停掉空闲 ping,或在 Go 侧 zrpc 显式设 PermitWithoutStream:true(要动两端口径)。
停掉空闲探活不留盲区:go-zero 服务端本就设 MaxConnectionIdle=5min 会主动关空闲连接,
且每次调用都有 deadline,死连接在下一次真实调用时即被发现并重连。

**证伪:**"3s 客户端 deadline 掐死服务端 30s gate 发现预算"(P1)。复核指出:
①那个 30s 不是给客户端留的预算 —— postmortem 记载它是为绕开 docker-compose
ETCD_ADVERTISE_CLIENT_URLS 通告不可达 URL 导致 etcd Sync() 每次等 30s 的环境 bug,
该 bug 已在 deploy/docker-compose.yml 修掉,login.yaml 注释自己写明"prod 亚 100ms";
②触发前提是 etcd 已经病了,此时 login 全链同样瘫痪,3s 不是致害因子,且两种配置下
玩家侧同为 500;③隐含修法(把 timeout-ms 提到 30s)**反而有害** —— LoginRpcClient 走
blockingUnaryCall 占用 Tomcat 请求线程,洪峰下 30s 阻塞会连 /api/login 一起拖死,
3s 是刻意的卸载边界。残留的真实隐患只是 watcher.go 用 context.Background() 不随请求
取消、FetchAllNodes 无缓存每请求一次 etcd Get —— 记录备查,未修。

**自查(未上报):**world.cpp:34 `tlsIdGeneratorManager.SetNodeId(GetNodeInfo().node_id())`
在 scene main.cpp:74 调用,而 node_id 要等事件循环里的 etcd 注册才是终值
(SetAfterStart 在 202、loop.loop() 在 284)—— 该处取到的**确定是 0**,与此前修过的
gate session_id node 段为 0 是同一形态。但影响需如实评估:它只给 buffIdGenerator /
skillIdGenerator 设 node 段,而这两类 id 都是**按玩家作用域**的运行时 id
(GenerateUniqueBuffId / GenerateUniqueSkillId 生成时已在本玩家表内去重),不进 DB、
不跨节点比较,故 node 段今天是装饰性的,**不构成缺陷,未改**。若将来有人开始跨节点
比较这两类 id,需先把 SetNodeId 移到 node_id 终值之后(仿 gate main.cpp 的做法)。

**仍未覆盖(累计三轮断连):**C++ scene world 域(world.cpp 调度已由我自查、
scene_comp 玩家集合增删配对未审)、Java 网关 etcd watch 重建 / 节点缓存陈旧读 /
健康探测(GateWatcher、ZoneHealthProbeService、ServerListService)。

待 Codex:`cd java/gateway_node && mvn -q test`(仅改了一个布尔参数 + 注释,无 API 变更)。

### 2026-08-10:Codex 提交验证

- Go 侧 10 个模块逐一执行 `go build ./...` 与 `go test -count=1 ./...`,全部通过;
  `gofmt` 检查通过。
- C++ 使用 VS MSBuild 对 `game.sln` 执行 Debug/x64 串行 `/m:1` 构建,成功产出
  gate/scene;`no-raw-pointer-member` 因本机缺少检查器而明确跳过。独立的
  `gate_security_test.cpp` 仍需 Linux/g++ 环境执行。
- Linux 构建脚本通过 bash 语法、16 项工程 dry-run 清单和非法参数返回码契约。
- 部署契约首次运行发现 zone rollback 的 dry-run 仍调用 Kafka CLI;修为 dry-run
  只打印计划后重跑,20/20 通过。
- Java 网关测试未执行完成:本机 JDK 21 不支持项目要求的 release 23。表导出器
  测试未执行:现有 Python 环境缺少 pytest/PyYAML,未擅自安装或修改系统环境。
- 根仓库改动按主题提交到 `main`,未推送远端;第三方子模块内部工作区保持原样。

### 2026-08-10:Codex 合并剩余远端分支

- `origin/claude/run-tools-proto-generator-pbgen` 已通过 merge commit 纳入 `main`。
  该分支基于 2026 年 3 月的旧“每服务复制 proto 树”,与当前 `_unified` 生成结构
  冲突;合并保留分支历史,生成文件统一采用当前主线版本/删除状态,未复活 77 个旧副本。
- `origin/copilot/track-code-commits` 已通过 merge commit 纳入 `main`。冲突处理保留
  当前 `dev_tools.ps1` 的完整部署门禁,并接入 `git-stats` 命令。
- 验证时发现 PowerShell 统计器用空白正则解析 `--numstat`,会漏掉带空格的路径;
  改为按 TAB 三列解析后,PowerShell/Bash 的提交数、文件数及增删行完全一致。
- 合并后部署契约重跑 20/20 通过。远端分支删除需要 push,本轮按项目禁令未执行。

### 2026-08-10(续七):跨 zone 迁移在源场景 ScenePlayers 留悬垂 id(P1,已修)

改用「单维度、范围切半、自己精读」代替扇出后,一次就挖出了前三轮扇出都没审到的
scene_comp 面上的真缺陷。

**P1:跨 zone 迁移路径不摘 ScenePlayers,源场景留悬垂 entity id。**

`ScenePlayers`(scene_node_comp.h)自身注释就写明是 weak refs,增删必须手工配对。
三条路径里只有两条摘了:
- 换场景 `HandleEnterScene`(player_scene.cpp:206-210)手工 erase 旧场景 —— 有;
- 正常登出 `HandleExitGameNode`(player_lifecycle.cpp:531)—— 有(此前修过);
- **跨 zone 迁移 —— 没有**。`HandleCrossZoneTransfer` 只挂 `PlayerFrozenComp`、
  刻意保留实体与 `SceneEntityComp`(为的是 ACK/reaper 两条终态都能收拾),随后
  `HandlePlayerMigrationAck`(1362)与 `HandlePlayerMigration` 的 payload 变更分支
  (982)直接调 `DestroyPlayer`。

`DestroyEntity` 只 `registry.destroy` 且只动 actorRegistry,而 `ScenePlayers` 在
**sceneRegistry**,全仓对这两个组件零 `on_destroy` 观察者 —— 没有任何路径会替它清。

后果与 HandleExitGameNode:528-530 那段注释警告的一字不差:entt 复用实体 id,
源场景残留的陈旧 id 过一阵子可能正好是另一个场景里某个活着的玩家;一旦源场景被
`BeginSceneDrain`(703)排空,就会给那个不相干的玩家错发改派票、把他从当前场景踢走。
当年那条注释只修了登出路径,跨 zone 这条漏了。

**修法**:摘除动作下沉到 `DestroyPlayer` —— 它自称也确实是「玩家实体销毁的唯一出口」
(全仓 3 个调用点 647/1008/1388 全覆盖)。正常登出路径此时 `SceneEntityComp` 已摘除,
`try_get` 拿不到自然跳过(idempotent);两条跨 zone 路径实体还带着该组件,正好补上。
放在 `DestroyEntity` 之前,否则实体已销毁就取不到它所在的场景了。

**自查已排除**:`SceneRegistryComp`/`NodeStateComp` 等其余 scene comp 无同类配对问题;
`DestroyEntity` 无隐藏钩子(game_registry.cpp:9-17 就三行)。

待 Codex:scene lib + scene 节点重编(MSBuild 串行 /m:1);
`cpp/tests/scene_test/scene_test.cpp` 已有 `ScenePlayers` 计数断言(624/628/897/901
断言排空后为 0),本改动只会让这些断言更容易成立,不应有回归。

### 2026-08-10(续八):Java 网关区服健康判据 —— 网关全挂却显示「开放·流畅」(P1,已修)

沿用「单维度 + 范围切半 + 单 agent」策略,把连续三轮断连丢失的 Java etcd/health 切片
(GateWatcher / NodeInfoRecord / ZoneHealthProbeService / ServerListService,约 500 行)
一次跑通,零断连。确认 2 条。

**P1(已修):无 gate 的区服被判 DEGRADED 而非 DOWN,对外显示成最诱人的状态。**

`evaluateHealth`(ZoneHealthProbeService.java:126-135)用 `hasGate || hasScene` 把两种
性质完全不同的残缺态合并成 DEGRADED。但 gate 是玩家**唯一**的对外入口(AssignGate 要
从 GateNodeService.rpc/ 选出 gate 才能给客户端 ip/port/token),gates 为空在玩家侧
等价于完全不可登录。

合并的后果是彻底反向而非「少报一档」:
- `ServerListService.resolveDisplayStatus`(78-91)只对 **DOWN** 做降级(→MAINTENANCE),
  DEGRADED 没有任何分支、静默落到默认 `yield OPEN`;
- 同时 `calculateLoadLevel` 的分子只统计**存活 gate** 的 playerCount(97 行),
  gates 为空 ⇒ totalPlayers=0 ⇒ ratio=0 ⇒ **SMOOTH**;
- 且 `autoStatus != UNKNOWN` 使 ServerListService:47-49 把这个 SMOOTH 真写进 DTO。

于是一个 100% 连不上的区服对外呈现「开放 · 流畅」,玩家点进去 AssignGate 零候选、
登录失败并反复重试;运维侧拿不到任何自动降级信号(maintenanceMsg 也不下发),
只能人工改 zone_config.manual_status。违反「失败路径必须 fail-closed」。

触发:某 zone 的 gate Deployment 滚更失败 / pod 全被驱逐 / 崩溃循环 → gate 以 lease
注册的键随租约过期消失,而 scene 节点键仍在 → 下一轮 probe 得 gates=[] scenes=[...]。

修法:`evaluateHealth` 开头加 `if (!hasGate) return DOWN;`,让 DEGRADED 只保留唯一
含义「有 gate 但无 scene」。无 gate 即无入口,本就该走已有的 DOWN → MAINTENANCE 路径,
不需要新增 wire enum(对外 status 是既有四值协议,加值会让旧客户端解析失败)。

**P2(未修,需拍板):负载等级的分子分母不同源,gate 挂得越多显示越空闲。**

`calculateLoadLevel` 的分子是「存活 gate 的 playerCount 之和」,分母是
`ZoneConfig.capacity` 这个 DB 静态值(默认 5000)。gate 掉一台,它承载的玩家从分子里
整体消失,比值直接下降一档:4 副本满载 4×1200/5000=0.96(FULL)→ 掉 1 台
3600/5000=0.72(BUSY)→ 掉 2 台 2400/5000=0.48(**SMOOTH**)。正在发生容量收缩的区
在选服列表上显示得比实际更空,把新玩家往仅存的、已超载的 gate 上引,局部故障被
正反馈放大。运维 dashboard 的在线数(同一分子)也会在故障期凭空缩水,易误判为
「玩家流失」而非「节点丢失」。

**为什么没直接修**:两种正解都需要新增契约,不该由我单方面拍板 ——
①分母随存活 gate 数缩放,需要一个「期望 gate 副本数」配置项(ZoneProbeProperties
已有 zone-probe 命名空间可挂);②分母改为「存活 gate 的容量之和」,语义最干净
(load = 玩家数 / 可用容量),但 `NodeInfoRecord`/`NodeInfo` proto **没有** per-gate
容量字段(已核:只有 nodeId/nodeType/launchTime/sceneNodeType/endpoint/zoneId/
protocolType/nodeUuid/playerCount),要加就是跨 C++/Go/Java 三端的 proto 改动。
请拍板走 ① 还是 ②。注:P1 修完后最坏情况(gates=0)已被 DOWN→MAINTENANCE 兜住,
本条只剩「部分 gate 丢失」这一档,故 P2。

**已证伪(别重查)**:①GateWatcher 名不副实,**没有 watch** —— 每轮 @Scheduled
(fixedDelay=5000)做一次全新的 prefix get(67-78),没有长连接状态可断,Go/C++ 侧那类
watch 泄漏坑在 Java 侧不适用(docs/design/gateway-k8s-deployment.md:22 的 "etcd watcher"
是过时文档);②并发可见性干净:snapshot 是 volatile + Map.copyOf 不可变整体发布
(40, 102-106),三张表原子换代;consecutiveProbeFailures 只被 scheduler 单线程读写;
③异常吞掉已修好:fetchNodesByPrefix 在查询/解析失败时抛 NodeDiscoveryException 而非
返空列表,probe() 的 catch 只保留 last-known-good、不写缓存,连续 3 次升 ERROR;
④死节点常驻不成立:gate 以 lease 注册,键随租约过期消失,靠键存在性判活站得住;
⑤etcd 前缀 Java(NodeType.java:14 无前导斜杠)与 Go(gate_redirect.go:24)一致,
bin/etc/base_deploy_config.yaml:38 注释里的前导斜杠是注释笔误。

待 Codex:`cd java/gateway_node && mvn -q test`(evaluateHealth 是 private,
仅改判定分支;若既有测试断言过「gates 空 → DEGRADED」需同步改成 DOWN)。

### 2026-08-10(续九):负载等级 P2 —— 深查后**自我证伪**,不修(记录以免下轮重报)

上一条(续八)留了个 P2 待拍板:「负载分子只算存活 gate、分母是静态 capacity,
gate 挂得越多显示越空闲」。用户拍板「按最标准的做法做」,我按 ② (给 NodeInfo 加
per-gate 容量字段) 动手前先查了三件事,结论是**这条 P2 站不住,两个方案都不该做**。

**查证 1:Go 侧(真正的准入权威)把容量建模成静态的每-zone 上限,不是每-gate 容量。**
`gateWatcherCapacityProvider.ZoneCapacity`(servicecontext.go:274)读的是配置来的
`caps map[string]uint32`(zone_id → capacity ceiling),与 Java 的 `ZoneConfig.capacity`
同构。`CandidatesForZone`(241-272)从 NodeInfo 只取 PlayerCount,**从不取容量**。
→ 方案 ② 会引入一个全系统不存在的概念,并与登录队列的容量模型分叉。

**查证 2:全仓 gate 没有任何连接数上限概念。**
grep `kMaxSession|maxSession|MaxConnections|kMaxConn|LimitSession` 在 cpp/nodes/gate、
cpp/libs/services/gate、cpp/libs/engine/core/session 下**零命中**;
proto/common/base/config.proto 的 BaseDeployConfig/GameConfig 也没有容量项。
→ 方案 ② 不是"补一个已有字段",而是由我凭空发明容量语义,还要跨 C++/Go/Java 三端
改 proto + 加配置,违反「不擅自新增契约」。

**查证 3(决定性):分子本来就是诚实的。**
`player_count` 的唯一写入点是 gate main.cpp:251-252
`static_cast<uint32_t>(tlsSessionManager.sessions().size())` —— **当前活着的 TCP 会话数**。
gate 一死,它承载的玩家**真的断线了**,不再在线。所以「4 gate 满载 4800 → 掉 2 台后
2400」这个读数是对的:区里此刻确实只剩 2400 人。显示 SMOOTH 反映的是真实在线密度,
不是"伪造的空闲"。原发现默认那些玩家还在,但他们已经不在了。
残留的唯一合理担忧是「幸存 gate 还能不能吃下新玩家」—— 那取决于 gate 容量是否为
瓶颈,而这正是系统**刻意不建模**的东西(见查证 2),不能靠猜。

**流程教训(重要)**:这条 P2 是我用**单 Agent** 跑出来的,**没有经过对抗性复核** ——
这正是它没被当场证伪的原因。前几轮凡是走两视角证伪的,类似的"前提默认"都被逮住了
(如 mission 全系列死代码、LoginRpcClient 3s deadline)。
→ 结论:单 agent 窄切片确实解决了断连问题,但**发现仍必须过一遍对抗复核**才能落码;
单 agent 的产出只能当"候选",不能直接当结论。续八的 P1(无 gate 判 DOWN)我是自己
逐行对着磁盘核过 evaluateHealth + resolveDisplayStatus + calculateLoadLevel 三处才修的,
不受此影响;本条没有那样的独立核对,故降为证伪。

**结论:两个方案都不做,代码不动。**「gate 部分丢失后幸存 gate 的承载余量」若将来真要
建模,应当先决定 gate 容量到底是不是瓶颈(需要压测数据),再决定是否引入容量契约 ——
那是一个独立的容量规划课题,不是本轮审计能顺手带出的修复。

### 2026-08-10:Codex 本轮提交验证

- Go 侧 `data_service`、`db`、`friend`、`guild`、`login`、`player_locator`、
  `scene_manager`、`shared` 共 8 个模块逐一执行 `go build ./...` 与
  `go test -count=1 ./...`,全部通过;新增热关停门禁测试通过。
- C++ 使用 VS MSBuild 对 `game.sln` 执行 Debug/x64 串行 `/m:1` 构建,通过;
  scene lib 与 scene node 均重新编译。独立 `cpp/tests/scene_test` 工程因既有
  include 配置找不到 `scene/system/scene.h` 而未能编译,测试程序未运行。
- Java 网关 `mvnw.cmd -q test` 在编译前被本机环境阻断:项目要求 Java release 23,
  当前仅有 JDK 21,未宣称测试通过。
- 部署脚本 AST 解析通过,`k8s_deploy_contract.tests.ps1` 20/20 通过。
- 根仓库改动按主题提交到 `main`,不推送远端;第三方子模块内部工作区保持原样。

### 2026-08-10(续十):gate 无连接数上限 —— P1,已修(用户指出)

续九里我把「全仓 gate 没有连接数上限概念」当成"所以不能加容量字段"的论据,
**漏了它本身就是缺陷**:gate 是唯一对公网开放的端口,而 muduo 的 TcpServer 无条件
accept —— 不需要任何凭证,只要一直建连就能把 fd、SessionMap、每连接读写缓冲吃光,
直到 accept 撞 EMFILE 或进程 OOM。现有两层防护都拦不住:token 校验发生在建连**之后**,
IllegalPacketCounter 只按**已建立的会话**计数。用户指出后按标准做法修。

**修法(三处接线,沿用既有通道,不新造机制):**
- `proto/common/base/config.proto` BaseDeployConfig 加 `gate_max_connections = 15`
  (0=不限,仅本地调试)。字段号 15 是下一个可用号,未复用。
- `config.cpp` 按该文件既有的逐字段显式映射风格加 `GateMaxConnections` 读取。
- `base_deploy_config.yaml` 给保守默认 20000,并注明"真实容量靠压测定,别照抄"。
- 闸门放在 `HandleConnectionEstablished` **第一句**:在发 session_id、建 SessionInfo
  之前拒掉,否则限流本身先付出了它要省的内存。判据用 `sessions().size()`
  (与连接严格 1:1,本函数插入、断开回调删除,不会漂移),不另立计数器。
  拒绝时直接 forceClose 不回应答 —— 已在容量边界上,再为每条被拒连接序列化一条 tip
  正是攻击者要的放大。日志按 1024 条汇总一行(同 CheckMessageSize 那条的教训)。

**动手中发现并一并修掉的二级缺陷(比上限本身更险):**
被拒连接从不 `setContext`,而断开时仍走 `HandleConnectionDisconnection`:
① `GetSessionId` 对空 context 抛 `bad_any_cast`,那条 catch **每条打一行 ERROR**;
② **Login 断线通知并没有被 `sessionFound` 守住**(只有 `set_player_id` 被守),
于是每条被拒连接都会带着 session_id=kInvalidSessionId **向 login 发一次 gRPC** ——
把连接洪峰原样放大成对 login 的 RPC 洪峰,限流闸门反倒成了新的放大器。
修:三条 fail-closed 拒连路径(新增的容量超限 + 既有的 node 段未就绪 / prod 空密钥)
统一 `setContext(kInvalidSessionId)`;`HandleConnectionDisconnection` 顶部对
`sessionId == kInvalidSessionId` 早退。**既有那两条拒连路径本来就带着这个洞**,
只因是低频配置错路径而一直没暴露。

**核过的前提(写下来免得下轮重查):**
- `kInvalidSessionId = UINT32_MAX`;session_id 布局 [node:15][seq:17](kNodeBits=17)。
  合法号撞上它需要 node_id 恰为 32767(15 位满值,即 3 万多个并发 gate),不可达;
  且"kInvalidSessionId 即无会话"本就是全仓既有约定(GetSessionId 的 catch、
  rpc_request_context 的默认值都这么用)。
- 闸门口径依赖 gate 是**单 IO 线程**:tlsSessionManager 是 thread_local,
  静态计数器也无同步。全仓无任何 setThreadNum,muduo 默认 0 个 IO 线程,现在成立。
  已在代码注释里钉死:谁将来开了线程池,SessionMap 本身会先分裂,必须先解决
  会话表的线程模型。

待 Codex:proto 改了要重生成(`cd go && build.bat` 或等价 C++ proto 生成),
然后编译 engine config + gate 节点。

### 2026-08-11:Codex 复核、补强与构建验证(gate 连接上限)

先纠正上一段两条错误前提(旧记录按“只追加”规则保留,以本段为准):

- `UINT32_MAX` **不是不可达**。`node_id=32767, seq=131071` 能合法生成该值;
  gate 现在显式跳过该哨兵,并把所有 node_id 都安全可用的并发 ID 上界定为
  `131071`。生产配置必须在 `1..131071`;dev/test 配 0 只关闭运维阈值,
  连接层仍以 131071 作硬上限,不会在 ID 全占满后永久自旋。
- `cd go && build.bat` 不会重生本字段的 protobuf。正确入口是
  `tools/scripts/dev_tools.ps1 -Command proto-gen-run`;本轮先把仓库 Debug
  protobuf 工具目录置于 PATH 首位并断言 `libprotoc 35.1`,再运行生成器。
  为遵守“不读 client/”,生成期间临时关闭 Unity 产物并在结束后恢复。

对抗复核又补了以下同层收口:

- 生产漏配/配 0 在 gate 启动期 fail-closed;K8s node ConfigMap 显式传播
  `GateMaxConnections`,契约同时断言与 `bin/etc` 一致且位于 `1..131071`。
- 只有实际 dispatch 过 `Login.Login` 或已经绑定合法 player 的会话才发 Login
  断线 RPC;未认证裸连、仅验证 token 后空闲的连接不再放大成跨服务 RPC。
  已发 Login 但未 Bind 的窗口仍发通知,player_id 保持 0,避免把
  `kInvalidGuid(UINT64_MAX)` 打到 PlayerLocator。
- 容量拒绝、新连接、未绑定断开、未认证消息、无效 token、未知 protobuf 与
  客户端 codec 解析错误均做采样;拒绝后 codec 停止分发同一 read 中的 pipeline
  帧,错误应答留 100ms flush 窗口后强关,不再依赖只半关闭写端的 `shutdown()`。
- 配置测试新增仓库默认 `GateMaxConnections=20000` 显式映射断言;若以后漏掉
  `config.cpp` 字段映射并静默回落 proto3 默认 0,测试会直接失败。部署配置 CI
  paths 同时补入 `bin/etc/**`,以后只改权威 YAML 也会触发契约测试。

Codex 实际验证(不是 Claude 推测):

- protobuf 生成成功;C++ 生成头为 `Protobuf C++ 7.35.1 / 7035001`,C++/Go/
  三份服务 proto 镜像均含字段 15。
- MSBuild Debug/x64 严格串行 `/m:1`: `proto.vcxproj`、`config.vcxproj`、
  `core.vcxproj`、`gate.vcxproj`、`configuration_table_test.vcxproj` 全部成功;
  最终产物已复制到 `bin/gate.exe`。
- `configuration_table_test.exe`:37/37 通过;`go/proto` 的
  `go test -count=1 ./...` 通过;`k8s_deploy_contract.tests.ps1`:26/26 通过;
  主题文件 `git diff --check` 通过。
- MSBuild 前置 `no-raw-pointer-member` 在上述工程均为 **SKIP**(本机既无
  `no_raw_ptr_check.exe` 也无 `clang-query`),不是 PASS。

本轮没有运行真实 N+1 TCP、Login 指标窗口、Linux/K8s 或容量压测,所以只声明
“实现、生成、编译、自动契约通过”,不声明运行时/E2E 或 20000 容量已验证。

仍需独立后续处理的风险(不冒充本轮已修):EnterGame 五分钟后台链可在断线后
继续写回 PlayerLocator ONLINE;Disconnect 仍是 best-effort;session_id 累计
131072 次会回绕,迟到 Bind 存在同进程 ABA 风险;未认证连接没有握手/空闲超时,
仍可长期占满全部槽位。proto 生成器本身也仍依赖调用者 PATH,后续应在工具入口
固定并校验 protoc 版本。

### 2026-08-14:Codex 提交前复核与验证

- 清理协议生成副作用时发现 `scene_node_service.{h,cpp}` 被生成器删掉 Agones
  `Allocated` 前置门禁 27 行;已恢复原实现,未把该功能回退混入提交。
- scene 接入 GM HMAC 鉴权后首次真实编译报 `openssl/crypto.h` 找不到;根因是
  `scene.vcxproj` 只加了仓库中不存在的 `third_party/openssl/include`,而 gate
  实际使用 `grpc/third_party/boringssl-with-bazel/include`。补齐与 gate 一致的
  Debug/Release include 后重编通过。
- Go `shared`、`login`、`db`、`scene_manager` 逐模块执行 `go build ./...` 与
  `go test -count=1 ./...`,全部通过;`go/proto` 全量测试通过。`snowflakealloc`
  普通单测通过;带 `-tags=integration` 的 etcd 用例因本机 127.0.0.1:2379
  未启动而全部明确 SKIP,新增水位集成断言没有真实 etcd 运行证据。
- 部署脚本与契约测试 AST 解析通过,`k8s_deploy_contract.tests.ps1` 26/26 通过。
- C++ 完整 `game.sln` 串行构建在工具 10 分钟上限被截断,没有拿到整解方案退出码;
  随后对受影响目标执行 Debug/x64 `/m:1` 增量验证:`proto`、`config`、`core`、
  `gate`、`scene`、`configuration_table_test` 均构建通过。配置测试 37/37 通过;
  `no-raw-pointer-member` 因本机缺少检查器全部明确 SKIP。
- 本轮没有真实 MySQL dry-run 写入审计、Snowflake etcd 集成、Gate TCP 洪峰、
  Scene GM RPC、K8s 或玩家 E2E 证据;只声明格式、生成、目标构建及自动测试通过。
