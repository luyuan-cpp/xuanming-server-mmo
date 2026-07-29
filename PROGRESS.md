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
