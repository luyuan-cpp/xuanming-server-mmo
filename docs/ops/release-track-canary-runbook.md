# 灰度与金丝雀发布手册（2026-11-06）

> **状态**：部署脚本、告警与本手册 11-06 已落码，金丝雀 battle 池与战斗按整局 11-10 补上。11-11 Codex 已跑发布轨道的契约测试与告警契约（见 `docs/PROGRESS.md` 2026-11-11「发布轨道五批验证」），**还没上过集群**。同日按设计 §2.8 补了「判定」一节，对应的延迟告警未编译、未跑契约测试。首次真集群演练时按本文逐步核对，发现与实际不符就改本文。
> **设计**：[发布轨道：灰度与金丝雀发布设计](../design/release-track-gray-canary.md)。规则以设计文档为准，本文只写操作步骤。
> **告警**：[`deploy/k8s/release-track-alerts.yaml`](../../deploy/k8s/release-track-alerts.yaml)。

## 0. 先弄清三件事

- **只有两条轨道**：`stable` 和 `canary`。进程的轨道由环境变量 `MMORPG_RELEASE_TRACK` 决定，不设就是 stable，部署脚本只在 canary 时写它。
- **按 zone 灰度**：一个 zone 只用一条轨道。金丝雀 zone 里 gate、scene、login、scene-manager、db 跑新版本；同在这个 zone 的 data-service、player-locator 保持 stable，部署时不重新下发。
- **全局服务跟着会话走**：金丝雀 zone 的玩家（金丝雀会话）经路由服访问 chat、friend、trade、mail、rank、guild、match 时，先找 `<svc>-canary`，找不到就用 stable；stable 会话永远不会进金丝雀副本。服务之间的内部调用和全局后台工作只在 stable 上。

- **战斗按整局**：一局的成员全部是金丝雀会话、且起了金丝雀 battle 池（`battle-canary`）时，这局放到金丝雀 battle 节点；有一个 stable 成员就放 stable。机房仍按 stable battle 节点选，选定的机房里没有可用的金丝雀节点就放 stable。stable 会话不看金丝雀对局：点名观战回「这场不能观战」，随机观战和观战列表里看不到；判据是成局时记下的轨道，不受 match 重启影响。

## 1. 开始之前

1. **stable 侧的保护要先上线。** router 按会话轨道选实例、C++ 发现过滤、match 镜像只收 stable，这几样必须先作为 stable 发布上线，之后的发布才能做金丝雀。金丝雀期间 stable 跑的是旧二进制，旧版本里没有这些保护。首次上线时整批一起带上即可。
2. **新旧版本要双向兼容**（在线协议与持久数据）。持久 schema 升世代的版本不能按 zone 做金丝雀，按[混版本持久字段保护](../design/mixed-version-persist-field-protection.md)的顺序发布。
3. **写 SQL 的全局服务要备好金丝雀的 boot 目录条目。** chat、friend、trade、mail、rank、guild、match 起金丝雀之前，要在 `MMORPG_SQL_K8S_BOOT_CATALOG` 指向的目录里加 `<infra ns>/<svc>-canary`：
   - 自己的注册计划、registrar 与运行配置 Secret，三个都不能和稳定版共用；
   - 自己的名册，按 `route_meta.generation` 写，条数至少是副本数 × 6；
   - `replicas` 等于 `-CanaryReplicas`（缺省 1）。

   两个 Deployment 共用一份名册会领到同一条 boot，所以不借稳定版那份。缺条目或副本数对不上时，`infra-up` 在任何写操作之前拒绝。router 不写 SQL，不需要条目。
4. **match 的金丝雀运行配置**里 `Etcd.Key` 写 `matchservice.rpc.canary`。guild 按 `matchservice.rpc` 发现 match，金丝雀登记在别的键下，guild 的内部调用才不会落到新版本上。键名不以 `.canary` 结尾时金丝雀进程直接拒启。
5. **开着 `-NetworkPolicies on` 的集群**：准入名单会自动带上 `<svc>-canary`。但 scene 的名单在 `zone-up` 时生成，所以每个 zone 都要先用同一版部署脚本跑过一次 `zone-up`，金丝雀的 guild、trade、mail、match 才连得上 scene 的 gRPC 口。
6. **（可选）Prometheus 把轨道变成目标标签**：第 4 节的判定不依赖这一步。路由服按会话轨道分指标，C++ 节点的 `cpp_node_info` 自带轨道，zone 按 namespace 比，对局按成局原件统计。只有想按轨道聚合 Go 服务自己的指标时才要配。金丝雀 Pod 带标签 `mmorpg.io/release-track: canary`，stable Pod 不带。按注解抓取的 job 加两条 relabel：

   ```yaml
   - source_labels: [__meta_kubernetes_pod_label_mmorpg_io_release_track]
     regex: (.+)
     target_label: release_track
   - source_labels: [__meta_kubernetes_pod_label_mmorpg_io_release_track]
     regex: ""
     target_label: release_track
     replacement: stable
   ```

   用 PodMonitor 的写 `podTargetLabels: [mmorpg.io/release-track]`，查询时把缺这个标签的当 stable。C++ 节点不依赖这一步：`cpp_node_info` 自带 `release_track`。
7. 装上告警：`kubectl apply -n observability -f deploy/k8s/release-track-alerts.yaml`。

## 2. 开金丝雀 zone

挑一个人少的 zone，或者专门留一个先锋 zone。其余参数与这个 zone 线上用的相同：

```bash
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName <zone> -ZoneId <id> -ReleaseTrack canary -NodeImage <新镜像>
```

- scene、login、scene-manager、db 随 Deployment 滚动换上新版本；data-service、player-locator 输出 `[keep-stable]`，不动。
- gate 是 OnDelete，要用排空脚本逐台换，每台确认完再换下一台：

  ```bash
  pwsh -File tools/scripts/k8s_gate_drain.ps1 -ZoneName <zone> -Ordinal <n> -DeletePod
  ```

- 核对：`zone-status` 最后一栏列出每个 Pod 的轨道标签，空的就是 stable。这个 zone 里除 data-service、player-locator 外都应是 `canary`。`cpp_node_info{zone_id="<id>"}` 的 `release_track` 也应全是 `canary`。

  ```bash
  pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-status -ZoneName <zone>
  ```

- gate 换到一半时，新旧 gate 并存，各自的会话走各自的轨道，这是正常的。超过 3 小时还没换完，`ReleaseTrackZoneMixedTooLong` 会响。

## 3. 开全局服务的金丝雀副本

要验证某个全局服务的新版本时，在 infra 上起它的金丝雀副本。其余参数与线上的 stable 完全相同，稳定版的 tag 不变：

```bash
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -CanaryServices client-rpc-router,guild -CanaryGoSvcTag <新 tag> -CanaryReplicas 1
```

- 只收 client-rpc-router、chat、friend、trade、mail、rank、guild、match。player-inbox、data-service、player-locator、scene-manager 不提供金丝雀。
- **不粘滞**：每次 `infra-up` 都按这一次的 `-CanaryServices` 收敛，没列出的服务会删掉以前留下的 `<svc>-canary`。所以金丝雀期间再跑 `infra-up`（比如改别的配置）时，要把还在验证的服务照列一遍。
- match 有额外前提：线上 stable 必须已经是「镜像只收 stable」的版本，否则落到金丝雀名下的匹配池会停摆（设计 §7「批 2b 的细节」）。
- 核对：`infra-status` 最后一栏能看到 `<svc>-canary` 的 Pod 带 `canary`；`kubectl -n <infra> get endpoints <svc>` 里不应出现金丝雀 Pod 的 IP（稳定版 Service 不选它）。

### 3.1 开金丝雀 battle 池

要验证战斗的新版本时，在同一次 `infra-up` 里加两个参数（同样不粘滞，还在验证就每次照带）：

```bash
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -CanaryBattleReplicas 2 -CanaryNodeImage <新 C++ 节点镜像>
```

- 另起一个 `battle-canary`（Deployment 形态是 Deployment，Agones 形态是第二个 Fleet），和 stable battle 共用 battle-node-config、机房号与密钥。要求 stable battle 池在（`-BattleReplicas > 0`）；hostPort 形态（`-ClientEntryMode external` 不用 Agones）不支持，两个池会抢 hostPort 20000。
- 只有全员都是金丝雀会话的新成局会放上去，所以先开金丝雀 zone，再开金丝雀 battle 池才有流量。match 先要是 11-10 之后的版本（会读会话轨道、有 canary battle 镜像）。
- 核对：`infra-status` 的轨道一栏里 `battle-canary` 的 Pod 带 `canary`；金丝雀 zone 的玩家开局后，`kubectl -n <infra> logs` 里对局建在 `battle-canary` 的 Pod 上。

## 4. 观察与判定

判定规则在设计文档 §2.8，这里是操作。每一批都按下面六步走：全部通过才开下一批（第 5 节）或晋升（第 6 节），任何一步不过就按第 7 节中止。修好后发新版本，从第 1 批重来。

1. **记下窗口起点，算出最短窗口**：第 1 批 24 小时，并且包含一次晚高峰和一次跨天；第 2 批 2 小时；第 3 批 2 小时，并且包含一次晚高峰。
2. **看流量够不够**。不够的时段不计入窗口，窗口顺延：
   - 金丝雀会话经路由服的请求每秒至少 1 次，按小时看：

     ```promql
     sum(rate(client_rpc_router_forward_by_track_total{release_track="canary"}[1h]))
     ```

   - 开了 `battle-canary` 时，窗口内至少 300 局在金丝雀 battle 上打完，看第 5 步 SQL 里 canary 一行的 `finished`。
3. **查告警**。把 `24h` 换成实际窗口长度，下面这条查询应当是空的：

   ```promql
   count by (alertname) (max_over_time(ALERTS{alertstate="firing", alertname=~"ReleaseTrack.*"}[24h]))
   ```

   金丝雀 zone 的其余告警同样查一遍，再对 stable zone 跑同一条作对照：

   ```promql
   count by (alertname, namespace) (max_over_time(ALERTS{alertstate="firing", namespace=~"<金丝雀 zone 的 namespace,用 | 分隔>"}[24h]))
   ```

   - 只在金丝雀上响过的，判不通过。
   - stable 也在响的是共同原因，先暂停扩大，查清再判。
   - 数据类告警（如存盘被拒 `ScenePlayerStoreSaveRefused`、个人结算积压）只在金丝雀上响过一次就判不通过，不看倍数。
   - 全局服务的告警按 infra namespace 分组，分不出是 `<svc>-canary` 还是稳定版响的，一律按共同原因处理。
4. **查进程**：金丝雀 Pod 的 RESTARTS 列全是 0。部署动作换掉的 Pod 是新 Pod，不算重启。

   ```bash
   kubectl get pods -A -l mmorpg.io/release-track=canary
   ```

5. **算对局作废率**（开了 `battle-canary` 时）。在 match 逻辑库的只读连接上跑（TiDB 用 follower read），一批只跑一次：

   ```sql
   SELECT CASE WHEN f.release_track = 'canary' THEN 'canary' ELSE 'stable' END AS track,
          COUNT(*) AS finished,
          SUM(c.decision = 2) AS aborted,
          SUM(c.decision = 2) / COUNT(*) AS abort_ratio
   FROM match_formation f
   JOIN match_battle_completion c ON c.battle_id = f.battle_id
   WHERE f.created_ms >= <窗口起点的 Unix 毫秒> AND c.decision IN (1, 2)
   GROUP BY track;
   ```

   - `decision` 1 是 COMPLETE，2 是 ABORT。成局原件补列之前的行 `release_track` 是 NULL，算 stable。
   - 通过条件：canary 的 `finished` 至少 300，并且 canary 的 `abort_ratio` 不超过 stable 的 `abort_ratio` × 2 + 0.01。
6. **记录**：把批次、zone 列表、窗口起止、流量、各项判据的值和结论写进当次发布单。不通过时写明按哪一条中止。

失败率与耗时两条判据由第 3 步的告警覆盖：`ReleaseTrackCanaryForwardErrorsHigh`、`ReleaseTrackCanaryForwardLatencyHigh` 都是连续 10 分钟超过门槛才响，窗口内响过一次就不过。

### 4.1 排查时看什么

- **全局服务的客户端请求**：`client_rpc_router_forward_by_track_total` 按 `release_track` 比较失败率。失败口径是 `outcome` 为 `no_target`、`dial_error`、`upstream_timeout`、`upstream_error`：

  ```promql
  sum by (release_track) (rate(client_rpc_router_forward_by_track_total{outcome=~"no_target|dial_error|upstream_timeout|upstream_error"}[10m]))
    / sum by (release_track) (rate(client_rpc_router_forward_by_track_total[10m]))
  ```

  canary 明显高于 stable 时 `ReleaseTrackCanaryForwardErrorsHigh` 会响（门槛见设计 §2.8 判据 1）。
- **全局服务请求的耗时**：按轨道比较 p99：

  ```promql
  histogram_quantile(0.99, sum by (le, release_track) (rate(client_rpc_router_forward_by_track_seconds_bucket[10m])))
  ```

  canary 明显慢于 stable 时 `ReleaseTrackCanaryForwardLatencyHigh` 会响（门槛见设计 §2.8 判据 2）。
- **zone 内的状态**：按 `zone_id` 对比金丝雀 zone 与其它 zone 的登录恢复、场景迁移、存盘拒绝与死信、Kafka lag。Go 服务的指标按 namespace 比（一个金丝雀 zone 就是一个 namespace）；配了第 1 节第 6 条的 relabel，也可以按 `release_track` 筛。
- **全局服务本身**：`<svc>-canary` 的日志与重启次数。canary 副本不跑全局后台，启动日志里有一行说明关掉了哪些后台循环，例如 match 的 `matcher, recovery, team maintenance and result consumer disabled`。

## 5. 扩大（灰度）

按设计 §2.8 的批次扩：第 2 批到 zone 总数的 10%（向上取整，且比上一批至少多 1 个），第 3 批到 50%。新加的 zone 按第 2 节开，再按第 4 节重新判定；上一批通过才开下一批。

zone 少于 2 个时没有同时段的 stable 可比，不能做金丝雀，只能整体发布、按普通告警观察。

## 6. 晋升

1. 第 3 批（50%）判定通过后，逐 zone 换回 stable 轨道，镜像用新版本：`zone-up -ReleaseTrack stable -NodeImage <新镜像>`，gate 照样逐台排空。
2. 全部 zone 换完后，跑一次不带 `-CanaryServices`、`-CanaryBattleReplicas` 的 `infra-up -GoSvcTag <新 tag> -NodeImage <新镜像>`：稳定版（含 battle）换成新版本，随后删掉全部 `<svc>-canary` 与 `battle-canary`。
3. 核对：`zone-status`、`infra-status` 的轨道一栏全是空的；`cpp_node_info` 里没有 `release_track="canary"`。

## 7. 中止

1. **先停新流量**：其余参数照旧，跑一次不带 `-CanaryServices`（或只列要留下的服务）、不带 `-CanaryBattleReplicas` 的 `infra-up`，删掉 `<svc>-canary` 与 `battle-canary`。来不及跑部署时，直接删工作负载：

   ```bash
   kubectl -n <infra ns> delete deployment <svc>-canary
   ```

   ```bash
   kubectl -n <infra ns> delete deployment battle-canary
   ```

   Agones 形态的金丝雀 battle 池是 Fleet，删 `fleet battle-canary`。金丝雀会话随即退回 stable（router 找不到金丝雀副本就用 stable），还没开局的金丝雀局改放 stable battle，稳定版的流量不受影响。`battle-canary` 上正在打的局会随 Pod 结束，和缩容 stable battle 一样；能等就等这些局打完再删。
2. **zone 回退**：`zone-up -ReleaseTrack stable -NodeImage <旧镜像>`，gate 逐台排空。zone 回退受[混版本持久字段保护](../design/mixed-version-persist-field-protection.md)的回退门禁约束，不满足就向前修复，不承诺一键任意回退。

## 8. 常见问题

- **金丝雀会话为什么落到了 stable 实例上**：这个服务没有金丝雀副本，或者副本还没就绪，属于设计内的退回。stable 会话永远不会进金丝雀。
- **gate 自报的轨道认不出**：router 回 `kInvalidParameter`，计入 `outcome="invalid_release_track"`，`release_track` 标签记 `unknown`。这是 gate 的配置错，查 gate 的 `MMORPG_RELEASE_TRACK`（只认小写）。
- **金丝雀进程拒启**：`MMORPG_RELEASE_TRACK` 取值非法；match 的金丝雀登记了 go-zero 键，键名却不以 `.canary` 结尾。
- **金丝雀 battle 池开着，要切换 battle 编排形态（`-BattleOrchestrator`）**：不用先撤池。切换时另一种 kind 的 `battle-canary` 和 stable battle 一起按旧形态删除，同样要带 `-AllowDisruptiveSwitch`，上面正在打的局会结束；这次 `infra-up` 照带 `-CanaryBattleReplicas` 就按新形态重新起金丝雀池。
- **stable 玩家说某场看不了**：这场在金丝雀 battle 上（成局时记下的轨道是 canary），属于设计内的拦截，金丝雀会话照常能看。
- **开着准入策略，金丝雀连不上 scene**：这个 zone 还没用新版部署脚本跑过 `zone-up`，scene 的准入名单里没有 `<svc>-canary`，按第 1 节第 5 条补跑。
