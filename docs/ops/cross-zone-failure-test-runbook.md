# 跨 Zone 失败场景测试 Runbook

> **状态**: v1 — 2026-05-19
> **范围**: 跨 zone 三件套(PlayerFrozenComp + ACK + reaper)的失败场景验证 SOP。设计参考 `cross-zone-readiness-audit.md §7 失败场景处理`。
> **执行环境**: 双 zone K8s 集群(本地 minikube / staging / pre-prod 均可)
> **执行频率**: 每次 cross-zone 三件套代码改动 + 每个版本上线前
> **目标**: 4 种失败场景(A/B/C/D)必须全部通过

---

## 0. 前置准备

```bash
# 1. 双 zone 拉起来
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z1 -ZoneId 1
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName z2 -ZoneId 2

# 2. Robot 在 z1 创建测试玩家,给他塞 100 件物品(等 #21 bag-to-entity attach 真接通后可用)
go run -C robot/clients/cmd/multi_zone_seeder . -zone=1 -count=1 -bag-items=100

# 3. 准备客户端连 z1 gate(用 robot 的客户端模拟即可)
go run -C robot/clients/cmd/single_player . -zone=1 -account=test001 -password=x

# 4. 三个独立终端各 tail 一个 log,准备观察行为:
#    Terminal A: kubectl logs -f deploy/scene -n mmorpg-zone-z1   (源 zone)
#    Terminal B: kubectl logs -f deploy/scene -n mmorpg-zone-z2   (目的 zone)
#    Terminal C: kubectl logs -f deploy/kafka -n mmorpg-infra     (Kafka)
```

**验证指标基线**(没失败时):

```
玩家 P 从 z1 → z2 切场景,~50-200ms 完成。
Terminal A 出现:
  metric=migration_start player_id=P from_zone=1 to_zone=2 attempt=1
  [CrossZone] Sent player transfer to zone 2: P
  [CrossZone] ACK confirmed for player P (zone=2); destroying source-side entity.
  metric=migration_done player_id=P
Terminal B 出现:
  HandlePlayerMigration ... player P
  [CrossZone] Published ACK for player P (from_zone=1, to_zone=2).
```

如果基线都不通,后续 4 个失败场景测试都没意义,先修主链路。

---

## 失败 A:Kafka broker 临时不可达(transient)

**模拟**:暂停 Kafka pod ~40 秒(超过 reaper 30s per-attempt deadline,但不超过 3 次 × 30s = 90s),然后恢复。

```bash
# 1. 玩家 P 在 z1,触发跨 zone 到 z2
#    (通过 client 走门,或者直接 grpcurl 调 scene 的 CrossZoneTransfer RPC)

# 2. 立刻暂停 Kafka(在 player_migrate 发出 → ACK 到达前)
kubectl scale deploy/kafka -n mmorpg-infra --replicas=0

# 3. 等 ~40 秒,观察 Terminal A:
#    应该看到:
#      metric=migration_start player_id=P attempt=1
#      (然后等 deadline...)
#      [CrossZoneReaper] cannot republish: local entity ... is gone.   <-- WRONG
#      或
#      [CrossZoneReaper] republished player_migrate for player P       <-- RIGHT
#      metric=migration_start player_id=P attempt=2

# 4. 恢复 Kafka
kubectl scale deploy/kafka -n mmorpg-infra --replicas=1

# 5. 等 Kafka 重连 + 重发的 player_migrate 被 z2 消费
#    Terminal A 应该看到 metric=migration_done 在 attempt=2 或 3
#    Terminal B 应该看到 HandlePlayerMigration 处理迟到的 player_migrate
```

**通过标准**:
- ✅ 玩家最终在 z2 上线
- ✅ z1 的 PlayerFrozenComp 被清除(grep `metric=migration_done`)
- ✅ z1 的 entt registry 里玩家实体被 DestroyPlayer(`tlsEcs.GetPlayer(P)` == null)
- ✅ Redis `player_migration:{P}` 被 DEL
- ✅ 玩家 bag/技能/货币数据在 z2 完整(查 `player:{P}:*` Redis keys)
- ❌ 不能出现 `metric=reaper_failed`(那是 3 次 attempts 都用光的标志)

**失败的诊断**:
- 如果出现 `cannot republish: local entity is gone`,说明源端实体被错误 DestroyPlayer(三件套件 2 没工作)
- 如果 attempt 从 1 直接跳到 2 但 Kafka 还在 down 期间,reaper deadline 太短(应该 30s)

---

## 失败 B:目的节点崩溃 / 不可达(persistent)

**模拟**:玩家跨 zone 期间持续杀目的节点 ~120 秒(超过 reaper 3 次 attempts × 30s = 90s 总额度)。

```bash
# 1. 触发跨 zone P: z1 → z2

# 2. 立刻杀 z2 scene 节点,并阻止重启:
kubectl scale deploy/scene -n mmorpg-zone-z2 --replicas=0

# 3. 等 ~120 秒,观察 Terminal A:
#    应该看到 attempt 1/2/3 依次失败,然后:
#      metric=reaper_failed player_id=P from_zone=1 to_zone=2 attempt=3
#      [CrossZoneReaper] declaring migration FAILED for player P (attempt=3, max=3).
#      Unfreezing, sending tip, keeping player on source zone.

# 4. 验证客户端:
#    应该收到 kEnterSceneFailed (3023) tip
#    UI 应该 dismiss 跨服 loading 蒙版,玩家能在 z1 继续移动

# 5. 恢复 z2(为了下个测试):
kubectl scale deploy/scene -n mmorpg-zone-z2 --replicas=1
```

**通过标准**:
- ✅ 玩家**留在 z1**,可以继续移动 / 战斗 / 消费货币
- ✅ z1 的 `PlayerFrozenComp` 被清除(grep `metric=reaper_failed`)
- ✅ z1 玩家实体**还活着**(NOT DestroyPlayer'd)
- ✅ 客户端收到 `kEnterSceneFailed = 3023` tip
- ✅ Redis `player_migration:{P}` 被 DEL
- ✅ z1 玩家货币 / bag 数据**完整无丢失**(Frozen 期间业务系统正确 reject 了写入,数据没飘)
- ❌ 玩家**不能**在 z2 上线(z2 都没起来,这是反向验证)

**失败的诊断**:
- 如果玩家在 z1 也消失了,说明 Frozen + reaper unfreeze 路径有 bug
- 如果客户端收到 `kSceneTransferInProgress`(13000)而不是 `kEnterSceneFailed`(3023),reaper 用错 tip code

---

## 失败 C:源节点重启(in-flight migration 丢失)

**模拟**:跨 zone 期间杀源节点,然后 K8s 自动重启它,验证 reaper restart-recovery 能清掉 stale Redis state。

```bash
# 1. 触发跨 zone P: z1 → z2

# 2. 立刻杀 z1 scene 节点(模拟源节点崩溃):
kubectl delete pod -l app=scene -n mmorpg-zone-z1 --grace-period=0 --force

# 3. K8s 自动重启 z1 scene pod (~10s)

# 4. 观察 Terminal A(新 pod 的日志):
#    应该看到 reaper 启动时跑 ScanAndRecover:
#      metric=reaper_started
#      [CrossZoneReaper] (ScanAndRecover 处理 stale `player_migration:{P}` Redis key)

#    两个可能分支:
#    分支 a) 在重启窗口期间,z2 已经收到原始 player_migrate 并 ACK 完成:
#      → 不会重发,直接 DEL Redis key
#    分支 b) z2 没收到 / ACK 没到达 z1 重启前:
#      → republish + attempt++,走正常 reaper 链路
```

**通过标准**:
- ✅ z1 重启完成
- ✅ Redis `player_migration:{P}` 在 60-120s 内被清(分支 a 立即 DEL,分支 b 通过 migration_done DEL)
- ✅ 玩家**最终**在 z1 或 z2 之一上线,**不会两边都没**
- ❌ 不能 Redis key 永远存在(说明 reaper restart hook 没跑)
- ❌ 不能 z1 重启后又同时存在玩家实体 + z2 也有玩家实体(双在线)

**失败的诊断**:
- 如果 Redis key 还在,看 `cpp/nodes/scene/main.cpp` 的 `CrossZoneReaper::StartTick(n.GetLoop())` 是否真在 `SetAfterStart` 跑了
- 如果出现双在线,说明 player_locator 没正确指向最终 home_node

---

## 失败 D:Kafka 重复投递(broker rebalance)

**模拟**:用 kafka-consumer-groups.sh 强制重置 consumer offset 让 `player_migrate` 重投,验证目的端幂等。

```bash
# 1. 玩家 P 完成正常跨 zone z1 → z2(基线测试通过)

# 2. 重置 z2 的 cross-zone consumer group offset 到上次成功之前 -1
kubectl exec -n mmorpg-infra deploy/kafka -- \
  kafka-consumer-groups.sh \
    --bootstrap-server kafka:9092 \
    --group scene-cross-zone-<z2-node-id> \
    --topic player_migrate \
    --reset-offsets --shift-by -1 --execute

# 3. 观察 Terminal B:
#    应该看到 z2 收到第二次 player_migrate for P,但:
#      [CrossZone] HandlePlayerMigration rejected player_id=P from zone 1
#      (player already exists / duplicate)
#    或者目的端再次 InitPlayerFromAllData + 再次 publish ACK,这是不安全的

# 4. 观察 Terminal A:
#    z1 应该再次收到 ACK,但因为 PlayerFrozenComp 已经被清(玩家早就 DestroyPlayer'd),
#    HandlePlayerMigrationAck 应该 log:
#      ACK for player P arrived but entity is gone — assuming already-handled, ignoring.
```

**通过标准**:
- ✅ 重复 ACK 不会触发第二次 DestroyPlayer(玩家实体已经不在)
- ✅ 玩家在 z2 不会出现重复实体 / 物品翻倍
- ✅ 货币 / bag 数据不会因为 Unmarshal 跑第二次而出问题(`ResetFromSnapshot` 是幂等的)

**已知缺陷**(待修):
- 当前 `HandlePlayerMigration`(目的端)**没做幂等检查**。如果重复消费,会再次 `InitPlayerFromAllData` 创建第二个 entt entity。修法:在 `HandlePlayerMigration` 入口查 `tlsEcs.GetPlayer(player_id) != null` → skip
- 这个测试**目前会失败**,因为没幂等保护。请在修复幂等保护(任务 #32)之前不要走这个失败场景

---

## 通过整体验证

4 种失败场景全过 ⇒ 跨 zone 三件套生产可用。

记录到 `docs/ops/cross-zone-test-results-<date>.md`:

```markdown
# Cross-Zone Failure Test Results — YYYY-MM-DD

Operator: <name>
Build: <commit-sha>

| Scenario | Result | Notes |
|---|---|---|
| A (Kafka transient) | PASS / FAIL | attempt=2/3 succeeded at <time> |
| B (dest persistent down) | PASS / FAIL | reaper_failed at <time>, client got kEnterSceneFailed(3023) |
| C (source restart) | PASS / FAIL | ScanAndRecover cleaned Redis at <time> |
| D (Kafka duplicate) | PASS / FAIL / SKIP-pending-#32 | |
```

---

## 失败时的 fallback

如果任一场景失败:

1. **不要 ship**。先修 root cause
2. 抓现场:`kubectl cp pod:/var/log /tmp/crash-<date>` + `redis-cli --no-auth-warning -h <redis> --rdb /tmp/redis-<date>.rdb`
3. 在 `docs/design/cross-zone-readiness-audit.md` §10(实施发现)加一条新发现
4. 创建对应 task 追踪修复

---

## Changelog

- **2026-05-19 v1**: 初版。覆盖 4 个失败场景。#32(目的端幂等)作为已知缺陷标注,跑场景 D 前必须修
