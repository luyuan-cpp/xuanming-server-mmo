# 发号器运维手册

发号器是 `data_service` 的 `AllocateIdSegment` 背后的实现(`go/data_service/internal/idissuer`),给 player_id、guild_id、物品 guid、流水号等所有永久 ID 发号。设计见 [玩家数据只存 TiDB 总体方案](../design/player-data-tidb-scaling-plan.md) §6。

## 1. 要知道的事实

- 发号器从内存切号段,只有预留新块时才写存储。每块 2^32 个号。
- 预留记录有两份,都只增不减,一块号要两份都写成功才发:

  | 记录 | etcd | 键 |
  |---|---|---|
  | 主记录 | `etcd.<infra>:2379` | `/id_issuer/counter/primary` |
  | 副记录 | `etcd-id-secondary.<infra>:2379` | `/id_issuer/counter/secondary` |

- 任何一份丢失或回退,另一份都盖得住发过的号,下一块自动跳到它之上,不用人工处理。
- 第三道防线是时间下限:每块的起点不低于当前时间对应的计数器值。它只会把起点往上抬,坏时钟不会造成重号,也不会让发号停下来。
- 记录的值是 `地域 << 57 | 秒 << 24`,秒从 2026-01-01 UTC 起算。地域现在都是 0。
- 发号器不用全局库。**恢复 `mmorpg_global` 不再影响发号**。
- **两套独立 etcd 是强规定**(AGENTS §7,负责人 2026-10-31 定):配置校验要求两套地址没有交集;每次预留和初始化再比 etcd 自己报的集群号,相同就拒绝,指标 `result=refused`,错误里带 `same etcd cluster`。`AllowAutoSeed=true` 只在 `Mode=dev`、`test` 下生效。
- dev 档(`Mode: dev` 且 `IdSegment.AllowAutoSeed=true`)两份记录放同一套 etcd,记录不存在时自动初始化,不需要下面任何操作。压测前清空 etcd 没有问题。

## 2. 首次部署(staging / prod)

新环境的两份记录都不存在,发号器会拒绝发号。scene 的「id segments ready」门禁不放行,建角也会失败,日志里是 `REFUSED ... both reservation records are missing`。这是刻意的:发号器分不清「首次部署」和「两套 etcd 同时丢了数据」。

确认是全新环境后,在任意一个 data-service Pod 里跑一次:

```bash
kubectl -n <zone-namespace> exec deploy/data-service -- /app/service -f /app/etc/data_service.yaml -id-issuer-init
```

输出 `both reservation records created` 就完成了。只要有一份记录存在,这条命令什么也不做,重复执行是安全的。不需要重启任何服务:scene 和 login 的客户端会按退避自动重试。

`-id-issuer-init` 不并进 `-migrate`:部署每次都跑 `-migrate`,一次普通发布不能悄悄盖过「两份记录都不见了」这个事故。

## 3. 指标和处置

指标是 `data_service_id_issuer_reserve_total{result}`(发号器预留块的结果)和 `data_service_id_segment_allocate_total{biz_tag,outcome}`。

| 现象 | 含义 | 处置 |
|---|---|---|
| `result="ok"` 偶尔增长 | 正常预留 | 无 |
| `result="conflict"` 偶尔增长 | 多个副本同时预留 | 无 |
| `result="one_record_missing"` | 一份记录不存在,已按另一份继续并补上 | 发号不受影响。查那套 etcd 为什么丢数据,按 §4.1 复盘 |
| `result="record_behind"` | 两份记录不相等 | 预留中途崩溃也会这样,下一块就恢复。持续增长或差很多,查是不是有一套被回退了(§4.1) |
| `result="store_error"` 持续增长 | 读写 etcd 失败 | 手里的块还能发,按峰值也有一小时以上。先修 etcd。用完就 fail-closed |
| `result="refused"`、`outcome="records_missing"` | 两份记录都不存在 | 首次部署走 §2;否则走 §4.2,这是 ID 安全事件 |
| `result="ahead_of_clock"` | 计数器比时钟超前 7 天以上,时钟曾跳到未来 | 不影响唯一性,但时间下限在追上来之前不起作用。先修时钟,记下来;追上之前如果发生 §4.2 要手工抬高 |
| 日志 `belongs to another region` | 记录的地域和配置不一致 | 配错了 `IdSegment.Issuer.Region`。改回去,不要改记录 |
| `outcome="exhausted"` | 本地域的值域见底 | 事故。只可能是时钟曾跳到很远的未来。换一个没用过的地域码(§4.4) |
| `outcome="unknown_tag"` | 种类没登记 | 加进 `IdSegment.Tags`(`config.go`、`data_service.yaml`、`k8s_deploy.ps1` 三处) |

## 4. 恢复

### 4.1 只有一套 etcd 丢数据或回退

不用停服,不用改记录。把坏的那套恢复起来就行(恢复方法见 [etcd 运维手册](./etcd-maintenance-runbook.md)),下一次预留自动对齐。

**恢复其中一套时,另一套不许动。** 两套同时回退才可能出问题。

### 4.2 两套同时丢数据或回退(ID 安全事件)

包括两套都从旧备份恢复,以及 `infra-down` 删了 namespace 又重建。

这种情况下时间下限是最后一道防线。只要时钟一直正常,它就盖得住发过的号;发号器崩溃或者重启比较集中时,计数器可能比时钟超前几个块(每块约 256 秒),所以要留余量。

1. 确认当前时钟正确。
2. 停掉所有 data-service,再重启所有持有号段的进程(login、guild、trade、mail、friend、rank、scene):它们手里的段是从旧记录切出来的,不清掉就挡不住它们继续发。
3. 确定写多少:

   - 确认时钟从没出过问题(没有 `ahead_of_clock` 指标,也没有时钟告警):直接跑 §2 的 `-id-issuer-init`,它会留一天余量。两份都存在但值回退了的话,用下面的 `etcdctl` 抬高,余量同样给一天。
   - 不确定:把余量加大到 30 天。值域按时间算有 272 年,30 天不算什么。

   算出要写的值(地域 0):

   ```bash
   echo $(( ($(date +%s) + 30 * 86400 - 1767225600) << 24 ))
   ```

   两份都写上,写之前先看一眼当前值,只能往上抬:

   ```bash
   kubectl -n <infra> exec etcd-0 -- etcdctl get /id_issuer/counter/primary
   ```

   ```bash
   kubectl -n <infra> exec etcd-0 -- etcdctl put /id_issuer/counter/primary <值>
   ```

   ```bash
   kubectl -n <infra> exec etcd-id-secondary-0 -- etcdctl get /id_issuer/counter/secondary
   ```

   ```bash
   kubectl -n <infra> exec etcd-id-secondary-0 -- etcdctl put /id_issuer/counter/secondary <值>
   ```

4. 起 data-service,确认发号恢复,按 ID 安全事件复盘。

### 4.3 不要做的事

- 不要手工把记录改小。只增不减是唯一性的前提。
- 不要把两套 etcd 配成同一个集群。生产上发号器启动时会拒绝地址有交集的配置,但改 DNS 绕过去它查不出来。
- 不要用同一个备份、同一时刻恢复两套。备份和恢复都错开,才算两份独立的记录。

### 4.4 换地域码

值域见底或者多地域部署时用。地域码写在 `IdSegment.Issuer.Region`,值 0 到 63。新地域的值域和旧地域不相交,所以换码本身不会重号。

发号器发现记录里的地域和配置不一致会拒绝预留,所以换码时要同时清掉两份记录,再跑 §2 的初始化。绝不能把用过的地域码再分配给别的地域。

## 5. 演练

定期在 staging 上做,证明上面的流程真的成立:

1. 删掉主记录,发号不停,下一块把它补回来。
2. 把副记录改小,发号不停,下一块跳到主记录之上。
3. 两份都删,发号器拒绝发号;跑 `-id-issuer-init` 后恢复,新号比之前的都大(按计数器比较,`idsegment.UnmixID` 可以把 ID 还原成计数器)。
