# 发号器运维手册

发号器是 `data_service` 的 `AllocateIdSegment` 背后的实现(`go/data_service/internal/idissuer`),给 player_id、guild_id、物品 guid、流水号等所有永久 ID 发号。设计见 [玩家数据只存 TiDB 总体方案](../design/player-data-tidb-scaling-plan.md) §6。

> 2026-11-05 起有三份预留记录和全局号源登记处,首次初始化和事故恢复是两个入口。**未编译、未测试、未演练**,第一次按本手册操作之前先在 staging 走一遍 §5。

## 1. 要知道的事实

- 发号器从内存切号段,只有预留新块时才写存储。每块 2^32 个号。
- 预留记录有三份,都只增不减,一块号要三份都写成功才发:

  | 记录 | etcd | 键 |
  |---|---|---|
  | 主记录 | 本地域的 `etcd.<infra>:2379` | `/id_issuer/counter/primary` |
  | 副记录 | 本地域的 `etcd-id-secondary.<infra>:2379` | `/id_issuer/counter/secondary` |
  | 登记处 | 全局号源登记处 `etcd-id-registry`,全世界一套 | `/id_registry/v1/region/<两位地域码>` |

- 任何一份或两份丢失、回退,剩下的那份都盖得住发过的号,下一块自动跳到它之上。本地两套同时回档时靠登记处;登记处丢失或回档时靠本地两份。
- **登记处同时是地域码的唯一绑定。** 每个码一条记录:发号世系(`authority`,初始化时随机生成,两份本地记录里也有)、绑定的两套本地 etcd 的集群号、水位、预留次数。发号器每次预留都核对:本地记录的世系和登记处一致,两套本地 etcd 报的集群号和登记处绑的一致,不一致就拒绝。所以重复部署、克隆旧配置和快照,在发号之前就被拒绝。
- 地域码按从小到大认领。登记处的 `/id_registry/v1/next_region` 是下一个可认领的码,认领一次就推过去。**用过的码永不再分配**:哪怕它那条记录丢了,它也小于 `next_region`,初始化照样拒绝。
- 第四道防线是时间下限:每块的起点不低于当前时间对应的计数器值。它只会把起点往上抬,坏时钟不会造成重号。
- 记录的值:本地是 `{"counter":<计数器>,"authority":"<32 位十六进制>"}`;登记处是 `{"state":"active|retired","authority":...,"primary_cluster":...,"secondary_cluster":...,"high_water":...,"blocks":...}`。计数器是 `地域 << 57 | 秒 << 24 | 序号`,秒从 2026-01-01 UTC 起算。不认旧的纯数字格式。
- 发号器不用全局库。**恢复 `mmorpg_global` 不影响发号。**
- **两套独立本地 etcd 是强规定**(AGENTS §7,负责人 2026-10-31 定)。配置校验要求三组地址两两没有交集;每次预留和运维操作再比 etcd 自己报的集群号,三套两两相同就拒绝,错误里带 `same etcd cluster`。
- dev 档(`Mode: dev` 且 `IdSegment.AllowAutoSeed=true`)三份记录放同一套 etcd,缺什么第一次预留时自动补,不需要下面任何操作。压测前清空 etcd 没有问题。
- **登记处不许从备份恢复。** 本地两套一起回档时,它就是证据;和本地一起回档,就什么也证明不了。它丢了数据就按 §4.3 重建。
- 别的集群经私网(VPC 对等、专线)访问登记处,只放行各集群 data-service 的出口。它和现有 etcd 一样没开认证,不能暴露到公网。

## 2. 首次部署

两种首次部署要分开:整个世界第一次部署(开启登记处),和一个新地域第一次部署(认领地域码)。事故不走这一节,走 §4。

### 2.1 世界第一次部署:托管并开启登记处

全世界只做一次。

1. 选一个集群托管登记处,一般是第一个生产集群。它的 `infra-up` 带 `-HostIdRegistry`,部署 `deploy/k8s/manifests/infra/etcd-id-registry.yaml`。
2. 给别的集群开私网访问,记下它们访问登记处用的 `host:port`。
3. 第一个 data-service 就绪之后,在任意一个 data-service Pod 里开启登记处,第一个可认领的码是 1:

   ```bash
   kubectl -n <zone-namespace> exec deploy/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-registry-open -id-registry-next 1
   ```

   输出 `id registry opened, the first claimable region code is 1`。已开启时什么也不做。staging 和 prod 共用这一个登记处,和[地域码登记表](open-new-region-runbook.md) §3 是同一套码。

### 2.2 新地域第一次部署:认领地域码

新地域的登记处里没有这个码,发号器拒绝发号,scene 的「id segments ready」门禁不放行,建角也会失败,日志里是 `REFUSED ... not registered in the id registry`。这是刻意的。

部署时 `-Region` 给这个地域的码,`-IdRegistryEtcd` 给登记处地址(托管登记处的集群也可以带 `-HostIdRegistry`,地址取本集群的 `etcd-id-registry` Service)。运维预置的 Secret 里的 `IdSegment.Issuer` 要有同样的 `Region` 和 `RegistryEtcd`,ConfigMap 只是模板。

在第一个 zone 的 data-service 就绪之后、放流量之前,在任意一个 data-service Pod 里跑一次:

```bash
kubectl -n <zone-namespace> exec deploy/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-init
```

输出 `region code claimed in the id registry and both reservation records created` 就完成了。重复执行是安全的:这个码已经由这两套 etcd 初始化过时输出 `already initialized`,什么也不做。认领之后、建本地记录之前中断过,再跑一次接着做完。不需要重启任何服务,scene 和 login 的客户端按退避自动重试。

初始化拒绝的几种情况,都不写任何东西:

| 输出 | 含义 | 处置 |
|---|---|---|
| `was already claimed in the id registry` | 这个码被认领过,或者小于 `next_region` | 换一个没用过的码。查登记表,别的地域已经在用了 |
| `is retired` | 这个码已退役 | 换一个没用过的码 |
| `the id registry is not opened` | 登记处没开启:新世界没走 §2.1,或者登记处整个丢了 | 新世界走 §2.1;否则是事故,走 §4.3 |
| `the local records exist but the code is not registered` | 本地有记录,登记处没有:登记处丢了这个码 | 事故,走 §4.3 |
| `both local reservation records are missing` | 这个码发过号,本地两份都没了 | 事故,走 §4.2 |

`-id-issuer-init` 不并进 `-migrate`:部署每次都跑 `-migrate`,一次普通发布不能悄悄盖过事故。

## 3. 指标和处置

指标是 `data_service_id_issuer_reserve_total{result}`(发号器预留块的结果)和 `data_service_id_segment_allocate_total{biz_tag,outcome}`。

| 现象 | 含义 | 处置 |
|---|---|---|
| `result="ok"` 偶尔增长 | 正常预留 | 无 |
| `result="conflict"` 偶尔增长 | 多个副本同时预留 | 无 |
| `result="one_record_missing"` | 一份本地记录不存在,已按另外两份继续并补上 | 发号不受影响。查那套 etcd 为什么丢数据,§4.1 |
| `result="record_behind"` | 两份本地记录不相等 | 预留中途崩溃也会这样,下一块就恢复。持续增长或差很多,查是不是有一套被回退了 |
| `result="records_behind_registry"` | 两份本地记录都落后登记处:本地两套都被旧数据覆盖过 | 发号不受影响,下一块已经在登记处水位之上。按 §4.1 复盘,查是谁恢复了两套 etcd |
| `result="registry_behind"` 偶尔增长 | 登记处落后一块以内:预留写完本地、写登记处之前中断,或两个副本同时预留 | 无 |
| `result="registry_behind"` 且日志 `more than one block behind` | 登记处被回档过 | 发号不受影响。查是谁恢复了登记处,§4.3。登记处不许从备份恢复 |
| `result="store_error"` 持续增长 | 读写三套 etcd 中的某一套失败 | 手里的块还能发,按峰值也有一小时以上。先修 etcd 或到登记处的网络。用完就 fail-closed |
| `result="refused"`、`outcome="records_missing"` | 登记处没有这个码,或者本地两份都不存在 | 新地域走 §2.2;否则是 ID 安全事件,按日志走 §4.2 或 §4.3 |
| `result="refused"`、`outcome="refused"` | 这两套 etcd 不是登记处给这个码绑的那两套,或者码已退役 | 日志 `not the ones bound`:重复部署、克隆、或换了 etcd 没有 rebind,§4.2;日志 `retired`:这个地域不该再发号 |
| `result="ahead_of_clock"` | 计数器比时钟超前 7 天以上,时钟曾跳到未来 | 不影响唯一性。修时钟,记下来 |
| 日志 `belongs to another region` | 记录的地域和配置不一致 | 配错了 `IdSegment.Issuer.Region`。改回去,不要改记录 |
| 日志 `same etcd cluster` | 三套 etcd 里有两套是同一套 | 配错了地址。生产必须三套独立 etcd |
| 启动报 `reserved for dev and test` | `Mode` 不是 dev、test,`IdSegment.Issuer.Region` 漏写或写了 0 | 配上这个地域的码,见[开新地域手册](open-new-region-runbook.md) §3 |
| 启动报 `RegistryEtcd are all required` | 生产没配登记处地址 | 在 Secret 和部署参数里补上 `RegistryEtcd` / `-IdRegistryEtcd` |
| `outcome="exhausted"` | 本地域的值域见底 | 事故。只可能是时钟曾跳到很远的未来。换一个没用过的地域码(§4.5) |
| `outcome="unknown_tag"` | 种类没登记 | 加进 `IdSegment.Tags`(`config.go`、`data_service.yaml`、`k8s_deploy.ps1` 三处) |

## 4. 恢复

原则:**上界只取幸存的记录,不猜起点,不手算时间余量,不手工改记录。** 拿不到可信上界就拒绝发号,换一个新码。

先按现象对表,再看对应小节:

| 本地主记录 | 本地副记录 | 登记处这个码 | 发号器 | 小节 |
|---|---|---|---|---|
| 丢了或回退 | 正常 | 正常 | 自动继续,补齐 | §4.1 |
| 正常 | 丢了或回退 | 正常 | 自动继续,补齐 | §4.1 |
| 回档(记录都在) | 回档(记录都在) | 正常 | 自动从登记处水位之上继续 | §4.1 |
| 都丢了 | 都丢了 | 正常 | 拒绝,`-id-issuer-recover -id-issuer-rebind` | §4.2 |
| 换成新集群(集群号变了) | | 正常 | 拒绝,`-id-issuer-recover -id-issuer-rebind` | §4.2 |
| 正常 | 正常 | 回档 | 自动从本地水位之上继续 | §4.3 |
| 正常 | 正常 | 丢了 | 拒绝,`-id-issuer-recover` | §4.3 |
| 都丢了 | 都丢了 | 丢了 | 拒绝,不能恢复,换码 | §4.4 |

**旧发号进程不需要先停。** 三份记录每次预留都重读,旧进程下一次预留只能落在可信上界之上,或者被拒;它们内存里的块都在上界之下,接着发也不重号。恢复命令也不需要重启任何服务。

### 4.1 本地一份丢了或回退,或者本地两套一起回档

不用停服,不用改记录,下一次预留自动对齐:

- 一份丢了或回退:另一份和登记处盖得住,指标 `one_record_missing` / `record_behind`。把坏的那套恢复起来就行(方法见 [etcd 运维手册](./etcd-maintenance-runbook.md))。
- 两套都被旧快照覆盖、记录都还在:登记处盖得住,下一块从登记处水位之上开始,指标 `records_behind_registry`。按 ID 安全事件复盘,查清楚为什么两套被同时恢复。

本地 etcd 按原来的成员地址和引导令牌原地恢复,集群号不变,不需要 rebind。换了令牌或成员地址,集群号就变了,走 §4.2。

### 4.2 本地两份都丢了,或者本地 etcd 换成了新集群

发号器拒绝发号,日志 `both local reservation records are missing` 或 `not the ones bound to this region code`。本地两份都没了,就证明不了现在这两套 etcd 还是原来那两套;集群号变了也一样。所以恢复要把这个码的发号资格转到这两套上(rebind),由人确认。

1. 确认这两套 etcd 就是这个地域的,不是别的环境误配过来的;确认原来那两套已经不在,或者不再给这个码用;确认没有别的部署在用这个码。
2. 在这个地域任意一个 data-service Pod 里跑:

   ```bash
   kubectl -n <zone-namespace> exec deploy/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-recover -id-issuer-rebind
   ```

3. 输出 `bound region <码> to these etcd clusters (...) above high_water=<水位>`。两份本地记录按登记处水位重建,原来那两套 etcd 从此预留被拒。不带 `-id-issuer-rebind` 时只报 `rerun with -id-issuer-rebind`,不写任何东西。
4. 确认发号恢复,按 ID 安全事件复盘。

认领之后从没发过号的码(没做完的初始化),`-id-issuer-recover` 不带 rebind 也能做完。

### 4.3 登记处丢了数据或被回档

- **登记处被回档、本地完好**:不用处理,发号器按本地记录继续并把登记处抬上去,指标 `registry_behind`、日志 `more than one block behind`。查清楚是谁恢复了登记处。
- **登记处丢了一个码**:这个地域预留和初始化都拒绝,日志 `not registered`。在这个地域跑 `-id-issuer-recover`(不带 rebind),按本地两份记录的最大值重新登记,输出 `re-registered region <码>`。
- **登记处整个丢了**:所有地域拒绝新块,新地域初始化报 `not opened`。重建,不从备份恢复:
  1. 重新部署登记处 etcd(托管集群 `infra-up -HostIdRegistry`),数据目录是空的。
  2. 每个在用的地域各跑一次 `-id-issuer-recover`,按本地记录重新登记。
  3. 按[地域码登记表](open-new-region-runbook.md) §3,对每个已退役的码补墓碑:复制一份 data-service 配置,把 `IdSegment.Issuer.Region` 改成那个码,跑 `-id-issuer-retire`。
  4. 开启登记处,下一个可认领的码取登记表里用过的最大码 +1:`-id-registry-open -id-registry-next <最大码+1>`。这一步之前任何初始化都被拒绝;`next` 不大于已登记的码时开启也被拒绝。

重新登记的前提是本地记录这时没有被回档。登记处和本地同时出事,本地记录取不到可信上界,走 §4.4。

### 4.4 本地和登记处都没有这个码的记录

没有任何东西盖得住这个码发过的号。发号器拒绝,`-id-issuer-recover` 报 `no surviving record covers`,初始化报 `was already claimed`,这是刻意的:不能猜起点。

处置是换一个新的发号空间:

1. 停这个地域的建角,按事故流程处理。
2. 退役旧码:`-id-issuer-retire`(登记处里没有这条记录也会写墓碑)。
3. 在登记表里选一个没用过的码,登记,按 §2.2 用新码部署并初始化。新码的值域和旧码不相交,旧码发过的 ID 都保留原值,继续有效。

### 4.5 换地域码

值域见底时用。旧码退役(`-id-issuer-retire`),新码按 §2.2 部署并初始化。正在跑的发号器发完手里的块就停在旧码上,新部署用新码。**用过的码不再分配。**

### 4.6 退役一个地域

地域下线时跑 `-id-issuer-retire`。之后这个码预留、初始化、恢复全部拒绝;正在跑的发号器发完手里的块就停。登记表里把状态改成「退役」。

### 4.7 不要做的事

- 不要手工改记录。只增不减是唯一性的前提。
- 不要从备份恢复登记处,更不要和本地 etcd 同时恢复。
- 不要用同一个备份、同一时刻恢复本地两套 etcd。它们能自动恢复,是因为登记处没有一起回档。
- 不要把三套 etcd 里任意两套配成同一个集群。生产上发号器拒绝地址有交集的配置,也会比集群号,但改 DNS 指到同一套,配置校验查不出来。
- 原来那两套 etcd 还在给这个码发号时,不要 rebind。
- 不要把用过的地域码分给别的地域。

## 5. 演练

定期在 staging 上做,证明上面的流程真的成立。每一条都要断言新发的号比之前预留过的都大(按计数器比较,`idsegment.UnmixID` 可以把 ID 还原成计数器):

1. 删掉本地主记录:发号不停,下一块把它补回来。
2. 把本地副记录改小:发号不停,下一块跳到主记录之上。
3. 先拍本地两套 etcd 的快照,再发几块号,再把两套都恢复到那份快照:发号不停,新块在登记处水位之上,指标 `records_behind_registry` 增长。
4. 删掉本地两份记录:发号器拒绝;`-id-issuer-recover` 不带 rebind 拒绝;带 rebind 之后恢复。
5. 另起一对本地 etcd,用同一个地域码部署:初始化报 `was already claimed`,发号报 `not the ones bound`,登记处不变。把第一对的记录拷进去(克隆快照),照样拒绝。
6. 删掉登记处这个码的记录:发号器拒绝;`-id-issuer-recover` 重新登记之后恢复。

单机上的自动化演练(真 etcd、真快照恢复)由 Codex 按 [交接说明](../handoff/global-id-issuer-registry-20261105.md) 执行。
