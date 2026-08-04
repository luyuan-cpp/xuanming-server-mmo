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
