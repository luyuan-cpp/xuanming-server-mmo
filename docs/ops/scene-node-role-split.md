# Scene Node Role Split Runbook

Split a zone's `scene` workload into two pools — a persistent main-world pool
and an on-demand instance pool — so that WoW-style routing (world vs
dungeon/battleground) is honoured end to end.

This runbook assumes you have already applied the Go-side changes in
`docs/design/scene-creation-architecture.md` ("Node Role Separation") and
that `scene_manager_service.yaml` ships with `StrictNodeTypeSeparation:
true`.

## 1. How the pieces connect

```
K8s Deployment (scene-world)     K8s Deployment (scene-instance)
  env SCENE_NODE_TYPE=0            env SCENE_NODE_TYPE=1
  etcd register → eSceneNodeType=0   etcd register → eSceneNodeType=1
           \                               /
            \                             /
       SceneManager LoadReporter
         mirrors scene_node_type → redis node:zone:{zoneId}:{nodeId}:scene_node_type
                     |
                     v
       GetBestNodeForPurpose(world|instance)
         chooses only pods that match the requested purpose
```

`scene_manager_service.yaml` keys that matter:

| Key | Role |
|-----|------|
| `StrictNodeTypeSeparation` | `true` → routing never crosses pools; `false` → falls back to any pod if the matching pool is empty (useful during rollout) |
| `WorldChannelCountByConfId` | Per-confId channel override, independent of the role split |
| `NodeLoadWeightSceneCount` / `NodeLoadWeightPlayerCount` | Composite load score tuning |
| `Timeout` | zrpc 服务端超时(毫秒),现 `8000`。与角色拆分无关,列在这里是提醒:§3 里改 yaml / 重新 apply ConfigMap 时**别动它**。必须为正,且 ≥ `HomeZoneLookupTimeoutMs` + `KafkaWriteTimeoutSeconds`×1000 + 1500(现 1500 + 5000 + 1500);不许改用 `MethodTimeouts`(`k8s_deploy.ps1` 只镜像标量 `Timeout`,K8s 上会静默退回全局值)。K8s ConfigMap 的 `Timeout` / `KafkaWriteTimeoutSeconds` / `HomeZoneLookupTimeoutMs` 三项由 `k8s_deploy.ps1` 从本 yaml 镜像,不等式由 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的 `Get-SceneManagerTimeoutBudgetViolations` 对服务 yaml 与 ConfigMap 各判一次;调大它还要同步调大 `bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.SceneManagerNodeService`(现 10000,须 ≥ 本值 + 2000,部署入口 `Assert-GrpcClientDeadlineBudget` 不满足即拒绝部署)。为什么是 8000 见 `docs/design/cross-zone-scene-travel.md` §12.2。2026-09-28 落码,契约测试未跑 |

## 2. C++ override knobs

The scene container reads two environment variables in addition to
`etc/game_config.yaml`:

| Env var | Effect |
|---------|--------|
| `SCENE_NODE_TYPE` | Overrides `GameConfig.scene_node_type` at startup. `0` = main world, `1` = instance, `2` = main world cross, `3` = instance cross. |
| `GAME_CONFIG_PATH` | Overrides the YAML file path (default `etc/game_config.yaml`). Use when you prefer two separate ConfigMaps over two env-var overrides. |

`bin/etc/game_config_instance.yaml` is shipped as a minimal reference file.

K8s 部署走的是 **env 覆盖**这一条路,不是两份 ConfigMap:`k8s_deploy.ps1` 只生成
一个 `node-config` ConfigMap(gate / scene 共用),里面的 `SceneNodeType: 0` 只是
文件基线;角色由 Deployment 上的 `SCENE_NODE_TYPE` 环境变量决定。生效点在
`cpp/libs/engine/config/config.cpp::readGameConfig` —— 先读 yaml,再用 env 覆盖
(last-wins),非法值只告警并保留 yaml 值。

## 3. Rollout order (zero-downtime)

The safe order is: loosen Go routing → bring up the new pool → tighten Go
routing again. This way the switch does not race with pod readiness.

### 3.1 Phase 1 — Prepare
1. Confirm all existing scene pods still have `SCENE_NODE_TYPE` unset
   (defaults to `0`, treated as world-capable).
2. Temporarily set `StrictNodeTypeSeparation: false` in
   `scene_manager_service.yaml` and re-apply the ConfigMap. This makes
   the selector fall back to the whole zone pool if a purpose-specific
   pool is empty, so no request can be rejected during the cutover.
3. Scale down any in-flight instance creations if convenient (not
   required; rejection cannot happen while the flag is `false`).

### 3.2 Phase 2 — Add the instance pool
1. Edit the zone definition (for example
   `deploy/k8s/zones.ops-recommended.yaml`) to use the split form:
   ```yaml
   replicas:
     centre: 1
     gate: 2
     scene_world: 2
     scene_instance: 2
   ```
2. Re-run your `k8s-zone-up` / `k8s-all-up` command. `tools/scripts/k8s_deploy.ps1`
   emits two Deployments per zone:
   - `scene-world`    with `env.SCENE_NODE_TYPE=0`
   - `scene-instance` with `env.SCENE_NODE_TYPE=1`

   先用 DryRun 确认生成结果再连集群(不访问集群、不构建镜像):
   ```bash
   pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up \
     -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml \
     -DryRun -SkipInfra -NodeImage ghcr.io/luyuancpp/mmorpg-node:test
   ```
   DryRun 会把每份将要 apply 的 manifest 原样打印在
   `--- BEGIN MANIFEST --- / --- END MANIFEST ---` 之间,并在每个 zone 前打印
   `Scene pools resolved: scene-world=2(SCENE_NODE_TYPE=0) scene-instance=2(SCENE_NODE_TYPE=1)`。
3. Wait for both Deployments to report ready. Verify:
   ```bash
   kubectl -n mmorpg-zone-<zone> get deploy
   redis-cli SMEMBERS scene_nodes:zone:<zoneId>
   redis-cli GET node:zone:<zoneId>:<nodeId>:scene_node_type  # expect "0" or "1"
   ```
4. **删掉旧的单池 Deployment**。`kubectl apply` 不会删除上一版留下的
   `scene` Deployment,不删的话这个 zone 会同时存在「拆分池」和「未分角色的
   legacy 池」,`GetBestNodeForPurpose` 的容量统计出现两套语义:
   ```bash
   kubectl -n mmorpg-zone-<zone> delete deployment scene
   ```
   脚本在拆分模式下会打印同样内容的 NOTE 提醒。

### 3.2.1 副本数来源与兼容规则(唯一权威)

实现在 `tools/scripts/k8s_deploy.ps1::Resolve-SceneDeploymentPlan`,文档以代码为准:

| zones 配置 / 命令行 | 生成的 Deployment | SCENE_NODE_TYPE |
|---|---|---|
| 出现 `scene_world` 或 `scene_instance` 任一键 | `scene-world` + `scene-instance` | 0 / 1 |
| 只有 `scene`(或什么都没写,取 `-SceneReplicas`) | `scene` | 0 |

- 两种模式**互斥**:拆分模式下不会再生成名为 `scene` 的 Deployment;
  legacy 模式下不会生成 `scene-world` / `scene-instance`。
- 拆分模式里没写的那一侧按 **0 副本**生成(Deployment 仍然创建),对应 §3.4
  回滚动作"把 instance 池缩到零";脚本会 `Write-Warning` 提示该池为空时
  `StrictNodeTypeSeparation: true` 会返回 `ErrNoNodeForPurpose`。
- 同一个 zone 里 `scene` 与 `scene_world`/`scene_instance` 同时存在时,拆分模式
  生效、`scene` 被忽略,并 `Write-Warning` 要求把 `scene` 键删掉。
- 命令行等价开关:`-SceneWorldReplicas` / `-SceneInstanceReplicas`(`-1` = 未指定,
  `0` = 显式缩到零副本),`dev_tools.ps1` / `k8s_image.ps1` 均已透传。
- `OpsProfile` 的副本下限(`managed-cloud` / `bare-metal` 把 scene 抬到 4)**只作用于
  legacy 的 `-SceneReplicas`**。拆分池不做静默抬高,避免"实际部署 ≠ 配置文件"。

### 3.3 Phase 3 — Tighten routing
1. Set `StrictNodeTypeSeparation: true` in `scene_manager_service.yaml`,
   re-apply, restart scene_manager.
2. Watch logs for `ErrNoNodeForPurpose` — if it fires, either pool is
   smaller than expected or `scene_node_type` failed to propagate.
3. Run smoke: create a dungeon instance and enter a main-world scene,
   confirm they land on the intended pods (compare scene id in
   `scene:{id}:node_id` against the pod's `scene_node_type`).

### 3.4 Rollback
- Flip `StrictNodeTypeSeparation` back to `false` and scale the instance
  Deployment to zero. Existing main-world scenes are untouched; the
  LoadReporter will clean up the instance pods' Redis entries within
  one `LoadReportInterval`(代码常量 5s,`go/scene_manager/internal/logic/load_reporter.go`,
  不可配置;这里以前写的 `LoadReporterInterval` / 默认 10s 与代码不符,与 §4.2「`refreshLoadScores`(5s 一拍)」
  是同一个节拍)。Redis 拒写时 GO-6 的推迟摘除会让死节点在负载集里多留若干拍,见 §7
  `SceneManagerNodeDetachWithoutDeathMark`。

## 4. Observability

### 4.1 Redis surface

Keys worth a Grafana / redis-cli dashboard:

| Key | Meaning |
|-----|---------|
| `scene_nodes:zone:{zoneId}` | Live node IDs in the zone |
| `scene_nodes:zone:{zoneId}:load` | ZSET, lowest score = most attractive |
| `scene_manager:zones` | SET of zone ids(十进制)— 2026-10-16 起的 zone 登记集合,领导者清扫陈旧负载集与孤儿频道时靠它发现 zone(替代全库 SCAN);只加不减,唯一删除方是 `merge_zone -clear-source-hot-state`。计数读失败、又不在负载集里的节点以 `1e12` 的哨兵分加入负载集(选点排最后,下一拍读成功后纠正),`redis-cli` 里看到这个分数不是负载 |
| `node:zone:{zoneId}:{nodeId}:members` | SET of player ids on that pod; `SCARD` = online players |
| `node:zone:{zoneId}:{nodeId}:scene_node_type` | 0=world, 1=instance, 2=world-cross, 3=instance-cross |
| `instance:{id}:members` | SET of player ids in that scene; `SCARD` = per-scene online players (subset of the above) |

### 4.2 Prometheus metrics

Set `MetricsListenAddr: ":9150"` in `scene_manager_service.yaml` to expose
`/metrics`. Gauges (all labelled `{node_id, zone_id, role}` unless noted):

| Metric | Meaning |
|--------|---------|
| `scene_manager_node_player_count` | Mirrors `SCARD node:zone:{zoneId}:{nodeId}:members` |
| `scene_manager_node_scene_count` | Mirrors `node:zone:{zoneId}:{nodeId}:scene_count` |
| `scene_manager_node_load_score` | Composite score actually used for scheduling |
| `scene_manager_nodes_by_role{zone_id,role}` | Live-node count per role — use for alerting when a role pool empties. 空池发布 0 而不是序列消失(2026-09-28 起,补 0 的范围见下方 Recommended alerts 第一条) |

Recommended alerts:
- Instance (or world) pool empty while strict mode is on — paging alert. Use
  the rules that ship in `deploy/k8s/scene-manager-alerts.yaml`
  (`SceneManagerInstancePoolEmpty` / `SceneManagerWorldPoolEmpty`)。两条都由两支
  `or on (zone_id)` 组成(World 那条把 role 换成 `main_world|main_world_cross`):
  ```
  (
    count by (zone_id) (scene_manager_nodes_by_role > 0)
    unless on (zone_id)
    count by (zone_id) (scene_manager_nodes_by_role{role=~"instance|instance_cross"} > 0)
  )
  or on (zone_id)
  (sum by (zone_id) (scene_manager_nodes_by_role) == 0)
  ```
  - **补 0 语义**(2026-09-28 落码,未编译、未测试):`metrics.go` 的 `SetNodesByRole` 每次 Reset 之后,
    对「当前有存活节点的 zone」与「本 scene_manager 自己配置的 `ZoneId`」两者的并集,四种声明角色
    (`main_world` / `instance` / `main_world_cross` / `instance_cross`)都先写 0,再用实际计数覆盖;
    调用点是 `load_reporter.go` 的 `refreshLoadScores`(5s 一拍)。`unknown` 不补 0(由
    `SceneManagerUnknownSceneNodeType` 按 > 0 盯着)。本进程配置的 `ZoneId` 为 0 时不为它补(单测的
    默认配置就是 0);有节点注册的 zone 一律补,包括注册成 zone 0 的节点所在的 zone 0。
  - **两支各管什么**:左支(旧写法,原样保留)抓「zone 里还有别的角色存活、却没有承载该用途的节点」;
    右支抓整 zone 全灭 —— 该 zone 的 scene 节点都没了时左支为空,右支由该 zone 自己的 scene_manager
    补的 0 求和为 0,触发。用 `sum` 不用 `min`:多副本、以及别的 zone 的 scene_manager 也在发布同一 zone 时,
    只要有一份 > 0 就不触发。
  - **新旧二进制都有效**:旧版 Go 不补 0,右支没有序列,整条退化为旧行为,两条 critical 不会因此失效。
    这里以前写的「不要写 `sum … == 0`」只在旧版 Go 下成立(那时池子空了是序列消失、`== 0` 永远是空向量);
    新版 Go 下 `== 0` 能触发,但左支仍保留 `count … > 0 unless …` 的写法,正是为了兼容仍在跑的旧二进制。
  - **残余盲区**(alerts 文件注释里同样记着):
    1. 该 zone 自己的 scene_manager 也全挂了,或它的 `ZoneId` 配错(配错还会给那个空 zone 误报)——
       整 zone 全灭只在这两种情况下仍不响;
    2. 某个 scene_manager 卡在 fullSync 的 3s 重试循环(etcd 不可达)时不再走 `refreshLoadScores`,它最后一次
       发布的 > 0 旧值会一直留着、压住这两条告警(左右两支都受影响)。这种形态应由 etcd / scene_manager
       存活类告警覆盖,alerts 文件里没有。
  - **更正(2026-10-16,发布者语义变了;落码未编译、未测试)**:上面「多副本、以及别的 zone 的 scene_manager
    也在发布同一 zone」不再成立。负载分刷新改为只由领导者按 zone 批量做(`load_refresh.go`),`nodes_by_role`
    随之改为:**领导者**发布它 etcd 镜像里的全部 zone,**跟随者**只发布自己配置的 `ZoneId`;两者都给自己的
    `ZoneId` 补 0。所以每个 zone 的发布者是「领导者 + 该 zone 自己的 scene_manager 副本」,各自的值都来自
    etcd 快照、彼此一致。两条 PoolEmpty 只做 `> 0` / `== 0` 判定,表达式不用改;看板的聚合从 `sum by` 改成了
    `max by`(`sum` 会按发布者个数放大)。残余盲区第 2 条相应细化为:卡住的若是**领导者**,压住的是**全部**
    zone;卡住的是跟随者,只压住它自己的 zone。领导者卡住时新告警 `SceneManagerLoadRefreshStalled` 会响
    (见 §7),可作旁证。
  - **每节点序列只由领导者导出(2026-10-16)**:`scene_manager_node_player_count` / `node_scene_count` /
    `node_load_score` 只来自领导者,跟随者每拍清空自己的这三类序列。换主时序列从一个 pod 换到另一个 pod,
    中间空缺最长约一个选主锁 TTL 加一拍(约 35s),超过 30s 的规则评估间隔,所以 `SceneManagerWorldNodeSaturated`
    (for 10m)与 `SceneManagerLoadScoreDispersionLow`(for 15m)的 pending 计时会在换主时清零。外部若有按 pod
    求和的每节点报表,数值会变(过去被副本数放大)。
    领导者只对「计数读成功、且所在 zone 的分数 `ZADD` 确认写成功」的节点更新这三类序列。读不到计数或分数没写进去
    的节点,序列停在上一次成功的值、不删(单拍抖动若删序列,会把上面两条告警的 for 计时清零),所以**这两种情况下
    看板与 Redis 里的实际分数不一致**:持续读不到的节点一直显示旧值(不在负载集里的还一直挂着哨兵分、选点排最后,
    等于少了一份容量),由 `SceneManagerLoadRefreshNodeErrors` 盯;写失败由 `SceneManagerLoadRefreshFailing` 盯(见 §7)。
  - **静默(silence)步骤**:补 0 之后,「zone 里一个 scene 节点都没有」本身 2 分钟后就会同时触发两条 critical。
    下面两种是预期内的,要事先在 Alertmanager 对该 `zone_id` 加 silence:
    1. **新 zone 开服**:在 `k8s-zone-up`(或含该 zone 的 `k8s-all-up`)之前加。该 zone 的 scene_manager 进入
       watch 循环后的第一拍(5s)起就为自己的 `ZoneId` 发布 0,scene Pod 还没注册时两条都会响。确认 `scene_manager_nodes_by_role{zone_id="<zoneId>"}` 里
       world 类(`main_world` / `main_world_cross`)与 instance 类(`instance` / `instance_cross`)都有 > 0 之后再解除。
    2. **整 zone 维护**(scene 全停、scene_manager 仍在跑):停 scene 之前加,维护结束后按同一判据确认再解除。

    matcher 取 `zone_id="<zoneId>"` 加 `alertname=~"SceneManager(Instance|World)PoolEmpty"`,只压这两条,
    不要把同 zone 的其它告警一起压掉;时长按预计窗口给,宁短勿长(不够再续)。amtool 示例:
    ```bash
    amtool silence add 'zone_id="<zoneId>"' 'alertname=~"SceneManager(Instance|World)PoolEmpty"' \
      --duration=2h --comment="zone <zoneId> 开服 / 维护" --alertmanager.url=<alertmanager-url>
    ```
- `SCARD node:zone:{zoneId}:{nodeId}:members` stays ≥ 90% of expected cap for > 5 min on
  any world pod → consider bumping `WorldChannelCountByConfId`.
- ZSET score dispersion on `scene_nodes:zone:{zoneId}:load` < 10% →
  consider raising `NodeLoadWeightPlayerCount` so live players carry
  more weight than scene count.
- `scene_node_type` value outside `{0,1,2,3}` (logged as `[LoadReporter]
  unknown scene_node_type`) — almost always a stale image or env typo;
  page ops.

## 5. Rebalance on node join / leave

When a world-hosting node joins or leaves the zone, SceneManager runs
`RebalanceWorldChannelsForZone` which moves channels toward the new hash
distribution, subject to two safety rules:

1. **Urgent**: channels whose current node is dead or no longer
   world-hosting are always re-homed (possibly to the same target node
   where the C++ `CreateScene` handler idempotently re-creates the ECS
   scene).
2. **Opportunistic**: channels on a live node but mapped to a non-ideal
   hash target are only moved when their `player_count == 0`. Hot
   channels stay put — live player migration is out of scope and would
   need cross-node state transfer that this codebase does not have.

`MaxRebalanceMigrationsPerTick` (default 10, set to 0 to disable) caps
how many moves fire per event. Rebalance runs in three contexts:

1. **Event-driven** — on every etcd PUT/DELETE of a world-hosting node.
2. **Periodic** — every `RebalanceCheckIntervalSeconds` (default 5 min)
   catches drift that has no corresponding etcd event (e.g. a hot
   channel drained naturally).
3. **Startup** — `fullSync` calls rebalance for every zone so drift that
   built up while SceneManager was down converges on reboot.

Watch for this log line to confirm rebalance fired:

```
[Rebalance] zone=1 urgent=2 opportunistic=0 migrated=2 skipped=0 pending=0 budget=10
```

If `skipped` is consistently non-zero, the target nodes are refusing
`CreateScene` — check the C++ side logs on those pods.

### 5.1 Inspecting the queue without triggering migrations

`GET /debug/rebalance-plan` on `MetricsListenAddr` returns the current
planner output as JSON (urgent + opportunistic lists per zone), without
actually migrating anything. Add `?zone=<id>` to narrow scope.

```
$ curl -s localhost:9150/debug/rebalance-plan | jq '.zones[].urgent | length'
0
```

Pair with the Prometheus gauges `scene_manager_rebalance_pending{reason}`
and counter `scene_manager_rebalance_migrations_total{reason,outcome}`
for dashboards and alerting (see `deploy/k8s/scene-manager-alerts.yaml`).

## 6. Orphan cleanup

When `CleanupOrphanChannelsOnStartup: true` (default), every fullSync
iteration scans Redis for `world_channels:*` sets whose confId is no
longer in `World.json` and removes them (plus their per-scene
`scene:{sid}:node` / `zone` / `player_count` keys, and best-effort
`DestroyScene` on the hosting node).

Safety guards:

- Refuses to run when `worldConfIds()` returns empty (treats it as a
  table-load failure, not an intentional wipe).
- Only matches on `confId not in World.json`; unknown zones with valid
  confIds are skipped so a zone restart does not trigger deletion.
- Scans via `SCAN` cursor in batches of 256 keys to avoid blocking Redis.

Disable with `CleanupOrphanChannelsOnStartup: false` when sharing a
Redis cluster across incompatible World.json versions (e.g. a canary
deploy alongside stable), otherwise leave it on.

### 6.1 更正(2026-10-16):不再 SCAN,改为按 zone 登记集合点查(落码未编译、未测试)

上面「Scans via `SCAN` cursor in batches of 256 keys」与开头的「scans Redis for `world_channels:*` sets」已不成立。
SCAN 遍历的是整个共享库,开服规模(数十亿键,推算未实测)下单次是小时级,而且跑在 load reporter 的 goroutine 上,
期间 watch、负载刷新、推迟摘除全部停摆。领导者 fullSync 里原有两次全库 SCAN,都已换掉:

| 用途 | 旧做法 | 现做法 |
|------|--------|--------|
| 孤儿频道清理(`orphan_cleanup.go`) | `SCAN world_channels:zone:*` | 候选 zone = 登记集合 `scene_manager:zones` ∪ 有活节点的 zone;每个 zone 一条 `HKEYS world_channels:desired:zone:{z}`,字段里 confId 不在 World 表的组按 `(zone, confId)` 点名清理 |
| 陈旧负载集成员清扫(`sweepStaleLoadSetMembers`) | `SCAN scene_nodes:zone:*:load` | 扫描范围 = etcd 快照里的 zone ∪ 登记集合 |

- **登记集合 `scene_manager:zones`**(SET,成员是 zone 十进制):领导者每拍刷新负载分时先 `SADD` 本拍涉及的全部
  zone,播种 / 写回频道期望数之前也会登记,每个副本启动时登记自己的 `ZoneId`。只加不减,唯一删除方是
  `merge_zone -clear-source-hot-state` 的最后一步。登记失败只打 Error、不挡主流程,有活节点的 zone 每拍自愈
  (契约见 `go/scene_manager/internal/logic/zone_registry.go` 文件头)。
- **删除顺序**:期望数字段是新的发现索引,所以 `deleteOrphanChannel` 改为最后才删它;中途失败时下一次 fullSync
  还能重新发现这组孤儿,对已空的集合重跑是幂等的。
- **覆盖不到的情形**(只会残留垃圾键,不影响路由):
  1. `DesiredWorldChannelCount` 的 `HSETNX` 出错兜底路径建出来的「有频道集合、没有期望数字段」的组;
  2. 升级前就已整体下线、负载集或期望数还留着残留、又从未登记过的 zone(旧版本不登记)。这类 zone 复活时会
     重新进 etcd 快照,下一次 fullSync 照常清扫;一直不复活的,按下面的步骤一次性补登记。
- 反过来,「频道集合已空、期望数字段还在」的残留以前 SCAN 找不到,现在能清掉。

**升级:一次性补登记(只对有历史数据的长期环境)**

生产代码里**不保留**任何全库 SCAN,也没有「打开就 SCAN」的配置开关(忘关就是把小时级 SCAN 原样带回)。
补登记由运维在首轮滚动升级完成后手工跑一次:

- 生产在开服前上线这一版时没有历史数据,**不需要**;本机按 AGENTS.md §6.2 压测前会 FLUSHALL,也不需要;
- dev / staging 等长期环境:对 scene_manager 配置里 `Redis` 段指向的那个实例与库执行下面的命令。
  `--scan` 是游标扫描,但仍会遍历整个库,**只在维护窗口或低峰跑,不要在开服后的生产上跑**;它只做 `SADD`,
  重复执行无害。

```bash
# 连接参数按环境替换;host / port / 库号以 scene_manager 配置的 Redis 段为准。
R="redis-cli -h <host> -p <port> -n <db>"
{ $R --scan --pattern 'scene_nodes:zone:*:load'      | sed -E 's/^scene_nodes:zone:([0-9]+):load$/\1/'
  $R --scan --pattern 'world_channels:desired:zone:*' | sed -E 's/^world_channels:desired:zone:([0-9]+)$/\1/'
} | grep -E '^[0-9]+$' | sort -un | while read -r z; do $R SADD scene_manager:zones "$z" >/dev/null; done
$R SMEMBERS scene_manager:zones   # 核对:应包含上面扫出的全部 zone
```

回滚到旧二进制:旧代码不读登记集合,照旧 SCAN,行为完全回到原样;留下的 `scene_manager:zones` 无害,可以手工 `DEL`。
设计与取舍见 `docs/design/scene-creation-architecture.md`「负载刷新与 zone 登记集合(2026-10-16)」。

## 7. Pre-packaged alerts

`deploy/k8s/scene-manager-alerts.yaml` ships a PrometheusRule covering(下表只列部分规则,不是全量;
文件里现有 19 条,以 `grep -c -- '- alert:' deploy/k8s/scene-manager-alerts.yaml` 为准):

| Alert | Severity | Trigger |
|-------|----------|---------|
| `SceneManagerInstancePoolEmpty` | critical | no instance-hosting pods in zone for 2 min(zone 里还有别的角色存活,或整 zone 全灭;后者靠补 0,新 zone 开服 / 整 zone 维护要先静默,见 §4.2 Recommended alerts) |
| `SceneManagerWorldPoolEmpty` | critical | no world-hosting pods in zone for 2 min(同上) |
| `SceneManagerWorldNodeSaturated` | warning | max player_count > 1800 for 10 min |
| `SceneManagerLoadScoreDispersionLow` | info | stddev/mean of load_score < 0.1 for 15 min |
| `SceneManagerRebalanceStalled` | warning | urgent migration queue non-zero for 5 min |
| `SceneManagerRebalanceFailureRate` | warning | >50% migration failure rate for 10 min |
| `SceneManagerUnknownSceneNodeType` | warning | any pod publishes scene_node_type outside {0,1,2,3} |
| `SceneManagerNodeDetachWithoutDeathMark` | warning | 10 分钟内 `scene_manager_node_detach_deferred_total` 的 `outcome` 为 `expired` 或 `abandoned` 的计数有增加,不设 for、任一次即告警(2026-09-28 落码,未编译、未实测)。背景(GO-6):已判死的 scene 节点写不进 `death_at` 或摘不出负载集时,scene_manager 不再照样摘除,而是把它留在负载集里(按存活处理)、每 5s 重试。`expired` = 自首次尝试起满一个再入屏障(默认 20s)仍写不进 `death_at`,不带标记摘除 —— 这一步本身安全,但说明 Redis 在拒 SET;`abandoned` = 10 分钟仍摘不出负载集、放弃,死节点留在负载集里,`CreateScene` / `EnterScene` 可能被派到它上面,直到下一次 fullSync。健康时恒为 0。处置:查 Redis(内存与 maxmemory、慢日志、连接、go-zero 熔断),scene_manager 日志搜 `[ReentryBarrier][DeferDetach]`;`abandoned` 且 Redis 已恢复时可重启 scene_manager 领导者触发 fullSync 重扫。判读细节见 alerts 文件注释与 `docs/ops/cross-zone-failure-test-runbook.md` |
| `SceneManagerSceneOrphanReconcileSpike` | warning | 单 zone 孤儿场景对账 `scene_manager_scene_orphans_reconciled_total` 超过 50 个/分钟、持续 5 分钟 —— 多半是某个 scene 节点 Pod 挂了(CrashLoopBackOff / OOMKilled);反复出现说明节点在抖。那些实例里的玩家已被断开,预期随后有一波重连 |
| `SceneManagerLoadRefreshStalled` | warning | (2026-10-16 新增,阈值待压测基线)`sum(increase(scene_manager_load_refresh_total[2m])) == 0 or absent(...)` 持续 5 分钟 —— 没有领导者在刷新负载分(选主失败 / 全部副本 `LeaderEligible=false` / 领导者卡在 fullSync 重试循环)。查 `sum(scene_manager_is_leader)` 与领导者的 `[LoadReporter] full sync failed` 日志。滚动升级期间领导者若还是旧版本会误报,升级窗口内加 silence |
| `SceneManagerLoadRefreshFailing` | warning | (同上)10 分钟内 `outcome` 为 `read_failed` / `registry_failed` / `write_failed` 的刷新有增加,持续 10 分钟。`read_failed` 只记传输层失败(熔断拒绝 / 取连接 / 网络 / 超时),只按错误类型判、与批怎么切无关。查 Redis 与领导者 `[LoadRefresh]` 日志;个别键的回复错误或坏值(WRONGTYPE / 空串 / 非数字)不论批多大都只记 `partial`,不在本规则里,由下面的 `SceneManagerLoadRefreshNodeErrors` 盯 |
| `SceneManagerLoadRefreshSlow` | warning | (同上)一拍刷新 p99 > 2s(拍长 5s)持续 15 分钟 —— Redis 慢,或全服节点数增长到单领导者扛不住(A1:按 zone 选主) |
| `SceneManagerLoadRefreshNodeErrors` | warning | (同上)`sum(increase(scene_manager_load_refresh_node_errors_total[10m])) > 0` 持续 30 分钟 —— 有节点的计数持续读不到。这些节点保留旧分或一直挂着哨兵分(选点排最后,等于少了一份容量),每节点序列停在旧值(见 §4.2)。领导者日志 `[LoadRefresh] ... node counter reads failed` 的 `first` 给出 zone / node,用 `redis-cli TYPE` / `GET` 查 `node:zone:{z}:{node}:scene_count` 与 `player_count` |

> 更正(2026-10-16):上面「文件里现有 19 条」已过时,新增四条负载刷新告警后是 23 条(仍以 `grep -c` 为准)。

Apply with:

```
kubectl apply -n observability -f deploy/k8s/scene-manager-alerts.yaml
```

Thresholds are starting points — tune `SceneManagerWorldNodeSaturated`
against your actual per-pod player cap and the `RebalanceFailureRate`
ratio against your observed steady-state failure rate.

## 8. Verifying the pipeline in CI

`go/scene_manager/internal/logic/integration_test.go` wires the rebalance
pipeline end-to-end against in-process fakes and is the cheapest way to
catch regressions before a cluster rollout:

| Test | What it locks in |
|------|-------------------|
| `TestIntegration_Rebalance_NodeGone_EndToEnd` | Dead node → new node actually receives CreateScene; Redis `scene:{sid}:node` flips; scene_count counters move; dead node is NOT called for DestroyScene. |
| `TestIntegration_Rebalance_BetterHome_EndToEnd` | Hash-ring opportunistic move between two live nodes; old (live) node gets best-effort DestroyScene. |
| `TestIntegration_Rebalance_CreateSceneFailure_KeepsOldMapping` | If the new node rejects CreateScene, Redis mapping STAYS on the old node — no player is ever routed to a scene that doesn't exist. |
| `TestIntegration_Rebalance_ActivePlayersBlockMigration` | A channel with online players is never migrated, even when the hash ring wants to move it. |

The harness uses `google.golang.org/grpc/test/bufconn` for the scene-node
gRPC server and miniredis for Redis, so it runs without Docker, without
etcd, and in under a second. When adding a new rebalance safety rule,
add a matching integration test here before landing the production
change; the `fakeSceneNode` counters double as a contract for what C++
RPCs the SceneManager is allowed to issue on any given code path.

Swappable seams exposed for test authors:

- `logic.SetNodeDialerForTest(dialer)` — routes `CreateScene` / `DestroyScene`
  / `ReleasePlayer` traffic through a caller-supplied dialer (bufconn in
  the stock harness). Always pair with `t.Cleanup(restore)`.
- `logic.ResetNodeConnCacheForTest()` — evicts cached gRPC ClientConns so
  a second sub-test doesn't re-use a previous test's already-closed
  listener. Call in the test setup, not just teardown.

## 9. Tuning checklist

| Symptom | Action |
|---------|--------|
| Capital city laggy | Add entry to `WorldChannelCountByConfId`, e.g. `1: 4` |
| Instance creation rejected (`ErrNoNodeForPurpose`) | Verify `SCENE_NODE_TYPE=1` is set on the instance Deployment; confirm pods are Ready |
| All instances landing on one pod | Check that etcd NodeInfo publish includes `sceneNodeType`; verify Redis ZSET score is being updated |
| Mirrors spread across pods instead of co-locating | Expected only for brand-new mirrors when the source pod is at capacity; otherwise check mirror co-location path in `createscenelogic.go` |
