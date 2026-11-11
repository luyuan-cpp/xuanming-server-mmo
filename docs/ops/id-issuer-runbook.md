# 发号器运维手册

发号器是 `data_service` 的 `AllocateIdSegment` 背后的实现(`go/data_service/internal/idissuer`),给 player_id、guild_id、物品 guid、流水号等所有永久 ID 发号。设计见 [玩家数据只存 TiDB 总体方案](../design/player-data-tidb-scaling-plan.md) §6。

> 2026-11-05 起有三份预留记录和全局号源登记处,首次初始化和事故恢复是两个入口。**未编译、未测试、未演练**,第一次按本手册操作之前先在 staging 走一遍 §5。
> 2026-11-11 起地域码绑定加上部署身份(本进程所在 K8s 集群的 kube-system UID,运行期读),在用的码只能从绑定它的地域退役([交接 §8](../handoff/global-id-issuer-registry-20261105.md)),同样**未编译、未测试**。
> 2026-11-11 第二批([交接 §9](../handoff/global-id-issuer-registry-20261105.md)):登记处只走双向 TLS;地域码只能按顺序认领;dev、test 不能连共用登记处;换码只换发号码(`-IdIssuerRegion`),存档库的地域编号不变。同样**未编译、未测试**。

## 1. 要知道的事实

- 发号器从内存切号段,只有预留新块时才写存储。每块 2^32 个号。
- 预留记录有三份,都只增不减,一块号要三份都写成功才发:

  | 记录 | etcd | 键 |
  |---|---|---|
  | 主记录 | 本地域的 `etcd.<infra>:2379` | `/id_issuer/counter/primary` |
  | 副记录 | 本地域的 `etcd-id-secondary.<infra>:2379` | `/id_issuer/counter/secondary` |
  | 登记处 | 全局号源登记处 `etcd-id-registry`,全世界一套 | `/id_registry/v1/region/<两位地域码>` |

- 任何一份或两份丢失、回退,剩下的那份都盖得住发过的号,下一块自动跳到它之上。本地两套同时回档时靠登记处;登记处丢失或回档时靠本地两份。
- **登记处同时是地域码的唯一绑定。** 每个码一条记录:发号世系(`authority`,初始化时随机生成,两份本地记录里也有)、部署身份(`deployment`)、绑定的两套本地 etcd 的集群号、水位、预留次数。发号器每次预留都核对三样,任何一样不一致就拒绝:
  - 本地记录的世系和登记处一致;
  - 本进程所在 K8s 集群的身份和登记处绑的一致;
  - 两套本地 etcd 报的集群号和登记处绑的一致。

  所以重复部署、克隆旧配置和快照,在发号之前就被拒绝。
- **部署身份是 `k8s:<kube-system 命名空间的 UID>`,data-service 运行期经 K8s API 读。** 只比 etcd 集群号不够:
  - 集群号只由成员地址和引导令牌算出,同一份清单部署到两个 K8s 集群会相同;
  - 集群级备份恢复(Velero、卷快照)会把数据目录里的集群号一起带过去。

  kube-system 的 UID 由 API server 建集群时生成,按对象恢复带不走。读它要求 data-service 的 ServiceAccount 能 get `namespaces/kube-system`,部署脚本随 data-service 一起 apply 这条权限(`deploy/k8s/manifests/go-svc/data-service-identity-rbac.yaml`)。读不到就不预留,指标 `identity_error`,手里的块照发。dev 不读。三套 etcd 的引导令牌也带上这个 UID,新部署出的 etcd 集群号各不相同。
- 地域码按从小到大、一个接一个认领。登记处的 `/id_registry/v1/next_region` 是下一个可认领的码,认领一次就推过去。初始化只能认领它本身:填大了报 `region codes are claimed in order`,什么都不写,免得一次填错就把中间的码永久烧掉。**用过的码永不再分配**:哪怕它那条记录丢了,它也小于 `next_region`,初始化照样拒绝。
- 第四道防线是时间下限:每块的起点不低于当前时间对应的计数器值。它只会把起点往上抬,坏时钟不会造成重号。
- 记录的值:本地是 `{"counter":<计数器>,"authority":"<32 位十六进制>"}`;登记处是 `{"state":"active|retired","authority":...,"deployment":"k8s:<uid>","primary_cluster":...,"secondary_cluster":...,"high_water":...,"blocks":...}`。计数器是 `地域 << 57 | 秒 << 24 | 序号`,秒从 2026-01-01 UTC 起算。不认旧的纯数字格式。
- 发号器不用全局库。**恢复 `mmorpg_global` 不影响发号。**
- **两套独立本地 etcd 是强规定**(AGENTS §7,负责人 2026-10-31 定)。配置校验要求三组地址两两没有交集;每次预留和运维操作再比 etcd 自己报的集群号,三套两两相同就拒绝,错误里带 `same etcd cluster`。
- dev 档(`Mode: dev` 且 `IdSegment.AllowAutoSeed=true`)三份记录放同一套 etcd,缺什么第一次预留时自动补,不需要下面任何操作。压测前清空 etcd 没有问题。dev、test 不核地域码绑定,所以不能连共用登记处:配置里写了 `RegistryEtcd` 或 `RegistryTLS`,启动就报错。
- **登记处不许从备份恢复。** 本地两套一起回档时,它就是证据;和本地一起回档,就什么也证明不了。它丢了数据就按 §4.3 重建。
- **登记处只走双向 TLS,全世界一个 CA(世界 CA)。** 证书怎么签见 §6。
  - **服务端证书**:登记处 etcd 的服务端证书由世界 CA 签,SAN 写全所有访问地址,包括本集群的 Service 名和别的集群走的私网地址。它只放在托管集群 infra 命名空间的 Secret `etcd-id-registry-tls`(`ca.crt`、`tls.crt`、`tls.key`)里,私钥不出这个集群。
  - **客户端证书**:每个地域一张,由世界 CA 签,放在这个地域每个 zone 命名空间的 Secret `id-registry-client-tls`(同样三个键)里。data-service 把它挂在 `/etc/mmorpg/id-registry-tls/`,运维预置的 Secret 里 `IdSegment.Issuer.RegistryTLS` 写这三个文件的路径。
  - **双向校验**:登记处开 `--client-cert-auth`,不收世界 CA 以外的客户端;发号器也只认世界 CA 签的服务端证书。连到别的环境误起的一套登记处,握手就失败,这同时钉住了「地域连的是哪一套登记处」。
  - **换证书**:客户端证书每次重连都从文件重读,换证书不用重启 data-service。世界 CA 要全世界一起换,见 §4.8。
  - **网络**:仍只走私网(VPC 对等、专线),只放行各集群 data-service 的出口,不暴露到公网。

## 2. 首次部署

两种首次部署要分开:整个世界第一次部署(开启登记处),和一个新地域第一次部署(认领地域码)。事故不走这一节,走 §4。

### 2.1 世界第一次部署:托管并开启登记处

全世界只做一次。

1. 按 §6 生成世界 CA(私钥离线保存,不进任何集群),签登记处的服务端证书。服务端证书的 SAN 写全本集群的 Service 名和别的集群将来访问它的私网地址。在托管集群的 infra 命名空间建 Secret `etcd-id-registry-tls`。
2. 选一个集群托管登记处,一般是第一个生产集群。它的 `infra-up` 带 `-HostIdRegistry`,部署 `deploy/k8s/manifests/infra/etcd-id-registry.yaml`;Secret 不在时部署脚本在写之前拒绝。
3. 给别的集群开私网访问,记下它们访问登记处用的 `host:port`,这个地址必须在服务端证书的 SAN 里。
4. 第一个地域按 §2.2 准备好客户端证书。它的 data-service 就绪之后,在任意一个 data-service Pod 里开启登记处,第一个可认领的码是 1:

   ```bash
   kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-registry-open -id-registry-next 1
   ```

   输出 `id registry opened, the first claimable region code is 1`。已开启时什么也不做。staging 和 prod 共用这一个登记处,和[地域码登记表](open-new-region-runbook.md) §3 是同一套码。

### 2.2 新地域第一次部署:认领地域码

新地域的登记处里没有这个码,发号器拒绝发号,scene 的「id segments ready」门禁不放行,建角也会失败,日志里是 `REFUSED ... not registered in the id registry`。这是刻意的。

部署时 `-Region` 给这个地域的码,`-IdRegistryEtcd` 给登记处地址(托管登记处的集群也可以带 `-HostIdRegistry`,地址取本集群的 `etcd-id-registry` Service)。

部署之前按 §6 用世界 CA 给这个地域签一张客户端证书,在这个地域每个 zone 命名空间建 Secret `id-registry-client-tls`;Secret 不在时部署脚本在写之前拒绝。运维预置的 Secret 里的 `IdSegment.Issuer` 要有同样的 `Region`、`RegistryEtcd`,以及 `RegistryTLS` 的三个路径(`/etc/mmorpg/id-registry-tls/ca.crt`、`tls.crt`、`tls.key`);ConfigMap 只是模板。

在第一个 zone 的 data-service 就绪之后、放流量之前,在任意一个 data-service Pod 里跑一次:

```bash
kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-init
```

输出 `region code claimed in the id registry and both reservation records created` 就完成了。重复执行是安全的:这个码已经由本部署环境的这两套 etcd 初始化过时输出 `already initialized`,什么也不做。认领之后、建本地记录之前中断过,在同一个 K8s 集群里再跑一次接着做完。不需要重启任何服务,scene 和 login 的客户端按退避自动重试。

初始化拒绝的几种情况,都不写任何东西:

| 输出 | 含义 | 处置 |
|---|---|---|
| `was already claimed in the id registry` | 这个码被认领过(包括被另一个 K8s 集群里的部署认领、哪怕 etcd 集群号相同),或者小于 `next_region` | 用下一个可认领的码。查登记表,别的地域已经在用了。如果是这个地域灾备到了新的 K8s 集群,不是新地域,走 §4.2 |
| `read the deployment identity` | 读不到本进程所在 K8s 集群的身份:不在 K8s 里跑,或者 ServiceAccount 没有 get `namespaces/kube-system` | 在 data-service Pod 里执行;补上 RBAC(§1)。不写任何东西,补好后重跑 |
| `region codes are claimed in order` | 配的码大于登记处里下一个可认领的码。码只能按顺序认领:跳过去的码永远不能再认领,一次填错(5 写成 50)就会烧掉中间整段,而码一共只有 63 个 | 按登记表用下一个可认领的码,`-Region` 和 Secret 一起改对。什么都没写 |
| `RegistryTLS needs CAFile, CertFile and KeyFile` | 运维预置的 Secret 里没写登记处的 TLS 路径 | 补上三个路径(§1),确认 Secret `id-registry-client-tls` 已挂上 |
| `id registry client certificate` 或 `id registry CA` | 挂进来的证书文件缺失或内容不对 | 按 §6 重签,换 Secret `id-registry-client-tls` |
| 超时,日志 `authentication handshake failed` | 双向 TLS 握手失败,原因跟在后面,见 §3 | 按 §3 那一行处置。什么都没写 |
| `is retired` | 这个码已退役 | 用下一个可认领的码 |
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
| `result="registry_behind"` 偶尔增长 | 复核登记处版本未变后,仍落后本地一块以内:此前预留中断,或另一副本尚未写登记处 | 无 |
| `result="registry_behind"` 且日志 `more than one block behind` | 可能是登记处回档,也可能是多次预留没完成登记处写入 | 发号不受影响。核对三套 etcd 的写失败与恢复记录,§4.3;不能仅凭差距判定回档。登记处不许从备份恢复 |
| `result="store_error"` 持续增长 | 读写三套 etcd 中的某一套失败 | 手里的块还能发,按峰值也有一小时以上。先修 etcd 或到登记处的网络。用完就 fail-closed |
| `result="store_error"`,日志 `authentication handshake failed` | 到登记处的双向 TLS 握手失败。后面跟的原因决定处置 | `certificate signed by unknown authority`:连到的不是世界 CA 签的登记处,核对 `RegistryEtcd`,不要改成信任别的 CA。`certificate is valid for`:服务端证书的 SAN 没有这个地址,按 §6 补 SAN 重签。`bad certificate`、`certificate required`、`expired`:客户端证书不对或过期,按 §6 重签并换 Secret,重连即生效 |
| `result="refused"`、`outcome="records_missing"` | 登记处没有这个码,或者本地两份都不存在 | 新地域走 §2.2;否则是 ID 安全事件,按日志走 §4.2 或 §4.3 |
| `result="refused"`、`outcome="refused"` | 本部署环境(K8s 集群)或这两套 etcd 不是登记处给这个码绑的那一份,或者码已退役 | 日志 `not the ones bound`,带两边的部署身份和集群号:重复部署、克隆、灾备到新的 K8s 集群或换了 etcd 没有 rebind,§4.2;日志 `retired`:这个地域不该再发号 |
| `result="identity_error"` | 读不到本进程所在 K8s 集群的身份:API server 不可达,或 data-service 的 ServiceAccount 没有 get `namespaces/kube-system` | 什么都没写,手里的块照发,下次预留再读。日志里 HTTP 403 就补 RBAC(§1),其他先查到 API server 的网络 |
| `result="ahead_of_clock"` | 计数器比时钟超前 7 天以上,时钟曾跳到未来 | 不影响唯一性。修时钟,记下来 |
| 日志 `belongs to another region`(`outcome="refused"`) | 记录的地域和配置不一致 | 配错了 `IdSegment.Issuer.Region`。改回去,不要改记录 |
| 日志 `same etcd cluster`(`outcome="refused"`) | 三套 etcd 里有两套是同一套 | 配错了地址。生产必须三套独立 etcd |
| 启动报 `RegistryEtcd must be empty when AllowAutoSeed=true` | dev、test 档配了显式的登记处地址 | dev、test 不核地域码绑定,不能连共用登记处。删掉这一项,登记处回落到本地 etcd |
| 启动报 `reserved for dev and test` | `Mode` 不是 dev、test,`IdSegment.Issuer.Region` 漏写或写了 0 | 配上这个地域的码,见[开新地域手册](open-new-region-runbook.md) §3 |
| 启动报 `RegistryEtcd are all required` | 生产没配登记处地址 | 在 Secret 和部署参数里补上 `RegistryEtcd` / `-IdRegistryEtcd` |
| `outcome="exhausted"` | 本地域的值域见底 | 事故。只可能是时钟曾跳到很远的未来。换一个没用过的地域码(§4.5) |
| `outcome="unknown_tag"`,`biz_tag="unregistered"` | 种类没登记。`biz_tag` 这个 label 只原样记登记过的种类,其余记 `unregistered`(格式合法)或 `invalid`(格式不对),基数有界 | 加进 `IdSegment.Tags`(`config.go`、`data_service.yaml`、`k8s_deploy.ps1` 三处) |

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
| 灾备到另一个 K8s 集群(部署身份变了) | | 正常 | 拒绝,`-id-issuer-recover -id-issuer-rebind` | §4.2 |
| 正常 | 正常 | 回档 | 自动从本地水位之上继续 | §4.3 |
| 正常 | 正常 | 丢了 | 拒绝,`-id-issuer-recover` | §4.3 |
| 都丢了 | 都丢了 | 丢了 | 拒绝,不能恢复,换码 | §4.4 |

**旧发号进程不需要先停。** 三份记录每次预留都重读,旧进程下一次预留只能落在可信上界之上,或者被拒;它们内存里的块都在上界之下,接着发也不重号。恢复命令也不需要重启任何服务。

### 4.1 本地一份丢了或回退,或者本地两套一起回档

不用停服,不用改记录,下一次预留自动对齐:

- 一份丢了或回退:另一份和登记处盖得住,指标 `one_record_missing` / `record_behind`。把坏的那套恢复起来就行(方法见 [etcd 运维手册](./etcd-maintenance-runbook.md))。
- 两套都被旧快照覆盖、记录都还在:登记处盖得住,下一块从登记处水位之上开始,指标 `records_behind_registry`。按 ID 安全事件复盘,查清楚为什么两套被同时恢复。

本地 etcd 在同一个 K8s 集群里按原来的成员地址和引导令牌原地恢复,集群号和部署身份都不变,不需要 rebind。换了令牌或成员地址(集群号变了),或者恢复到另一个 K8s 集群(部署身份变了),走 §4.2。

### 4.2 本地两份都丢了、本地 etcd 换成了新集群,或者灾备到另一个 K8s 集群

发号器拒绝发号,日志 `both local reservation records are missing` 或 `not the ones bound to this region code`。本地两份都没了,就证明不了现在这两套 etcd 还是原来那两套;集群号或部署身份变了也一样。所以恢复要把这个码的发号资格转到本部署环境和这两套 etcd 上(rebind),由人确认。

`not the ones bound` 的日志里有两边的部署身份和集群号。部署身份不同而集群号相同,说明这是另一个 K8s 集群用同一份清单部署出来的环境:可能是这个地域的灾备,也可能是误用了这个码的另一套环境。先查清楚是哪一种,误用的那套换码,不要 rebind。

1. 确认这两套 etcd 和这个 K8s 集群就是这个地域的,不是别的环境误配过来的;确认原来那一份(原来的 K8s 集群,或原来那两套 etcd)已经不在,或者不再给这个码用;确认没有别的部署在用这个码。
2. 在这个地域任意一个 data-service Pod 里跑:

   ```bash
   kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-recover -id-issuer-rebind
   ```

3. 输出 `bound region <码> to deployment "<部署身份>" and etcd clusters (...) above high_water=<水位>`。缺的本地记录按登记处水位重建,原来那一份从此预留被拒,它手里的块发完就停。不带 `-id-issuer-rebind` 时只报 `rerun with -id-issuer-rebind`,不写任何东西。
4. 确认发号恢复,按 ID 安全事件复盘。

认领之后从没发过号的码(没做完的初始化),在同一个 K8s 集群里 `-id-issuer-recover` 不带 rebind 也能做完。

2026-11-11 之前写的登记处记录没有部署身份,生产预留一律拒绝(`not the ones bound`,登记处那边的身份是空的),按本节在这个地域 rebind 补上。项目未上线,正常不会遇到。

### 4.3 登记处丢了数据或被回档

- **登记处被回档、本地完好**:不用手工修改水位,发号器按本地记录继续并把登记处抬上去,指标 `registry_behind`。差距超过一块时日志含 `more than one block behind`,但多次未完成的预留也会产生这条日志,须结合写失败和恢复记录判断,不能只凭日志断言回档。确认回档后查清楚是谁恢复了登记处。
- **登记处丢了一个码**:这个地域预留和初始化都拒绝,日志 `not registered`。在这个地域跑 `-id-issuer-recover`(不带 rebind),按本地两份记录的最大值重新登记,输出 `re-registered region <码>`。
- **登记处整个丢了**:所有地域拒绝新块,新地域初始化报 `not opened`。重建,不从备份恢复:
  1. 重新部署登记处 etcd(托管集群 `infra-up -HostIdRegistry`),数据目录是空的。沿用原来的 Secret `etcd-id-registry-tls`,也就是同一个世界 CA、同一张服务端证书:换了 CA,所有地域都连不上,还得全世界重签客户端证书。换托管集群时,新的访问地址要先在服务端证书的 SAN 里(§6)。
  2. 每个在用的地域各跑一次 `-id-issuer-recover`,按本地记录重新登记。
  3. 按[地域码登记表](open-new-region-runbook.md) §3,对每个用过、但第 2 步没有重新登记的码补墓碑。这包括已退役的码,也包括整个地域已经不在、记录全丢的码:不补的话,它会被当成没用过的码重新认领,新地域发的号就和它当年发过的撞上。做法:复制一份 data-service 配置,把 `IdSegment.Issuer.Region` 改成那个码,跑 `-id-issuer-retire`。登记处里没有这个码时不核绑定,不需要 `-id-issuer-retire-unbound`;它要是已经被第 2 步重新登记成在用的码,说明台账错了,停下来查。
  4. 开启登记处,下一个可认领的码取登记表里用过的最大码 +1:`-id-registry-open -id-registry-next <最大码+1>`。这一步之前任何初始化都被拒绝;`next` 不大于已登记的码时开启也被拒绝。

重新登记的前提是本地记录这时没有被回档。登记处和本地同时出事,本地记录取不到可信上界,走 §4.4。

### 4.4 本地和登记处都没有这个码的记录

没有任何东西盖得住这个码发过的号。发号器拒绝,`-id-issuer-recover` 报 `no surviving record covers`,初始化报 `was already claimed`,这是刻意的:不能猜起点。

处置是换一个新的发号空间:

1. 停这个地域的建角,按事故流程处理。
2. 退役旧码:`-id-issuer-retire`(登记处里没有这条记录也会写墓碑,不核绑定)。
3. 在登记表里取下一个可认领的码,登记,按 §4.5 只换发号码:部署时加 `-IdIssuerRegion <新码>`,运维预置的 Secret 里 `IdSegment.Issuer.Region` 改成新码,`-Region` 不变,再按 §2.2 初始化。新码的值域和旧码不相交,旧码发过的 ID 都保留原值,继续有效。

### 4.5 换地域码

值域见底,或者 §4.4 没有可信上界时用。

**换码只换发号码,不换地域的存档库编号。** `-Region` 同时决定这个地域的存档库名(`player_store_<1000000 + 地域码×1000 + 片号>_db`)和 login 的落点。它一换,老玩家所在的库就不在清单里,存不了盘。所以发号码单独用 `-IdIssuerRegion` 给:缺省等于 `-Region`,换码之后只有它变。部署脚本只用它渲染 data-service 的 `Issuer.Region` 和 `MMORPG_ID_ISSUER_EXPECTED_REGION`。

1. 旧码在这个地域的 data-service Pod 里退役(`-id-issuer-retire`,规则见 §4.6)。
2. 在[地域码登记表](open-new-region-runbook.md) §3 取下一个可认领的码,登记为「这个地域的新发号码」。
3. 运维预置的 Secret 里 `IdSegment.Issuer.Region` 改成新码,部署加 `-IdIssuerRegion <新码>`,其余参数(包括 `-Region`)照旧。以后每次部署都要照传。
4. 按 §2.2 初始化新码。

正在跑的发号器发完手里的块就停在旧码上,新部署用新码。**用过的码不再分配。**

### 4.6 退役一个地域

地域下线时,在这个地域的 data-service Pod 里跑 `-id-issuer-retire`。之后这个码预留、初始化、恢复全部拒绝;正在跑的发号器发完手里的块就停。登记表里把状态改成「退役」。

退役没有撤销入口,而 staging 和生产共用登记处,所以码还在用时,发号器只认从绑定它的那个部署环境执行。本地记录的地域和世系、本进程所在 K8s 集群的身份、两套 etcd 的集群号,都要和登记处一致。在别处执行一律拒绝,不写任何东西,输出 `refusing to retire region <码>, which is in use, from here`。

地域已经下线、没法在那里执行时:确认它的 data-service 已经全部停掉、不会再发号,再从别处加 `-id-issuer-retire-unbound` 执行。这个开关跳过上面的核对。登记处里没有这个码时(§4.3 补墓碑、§4.4)不核绑定,不需要它。

### 4.7 不要做的事

- 不要手工改记录。只增不减是唯一性的前提。
- 不要从备份恢复登记处,更不要和本地 etcd 同时恢复。
- 不要用同一个备份、同一时刻恢复本地两套 etcd。它们能自动恢复,是因为登记处没有一起回档。
- 不要把三套 etcd 里任意两套配成同一个集群。生产上发号器拒绝地址有交集的配置,也会比集群号,但改 DNS 指到同一套,配置校验查不出来。
- 原来那一份(原来的 K8s 集群或那两套 etcd)还在给这个码发号时,不要 rebind。
- 不要把用过的地域码分给别的地域。
- 不要对一个还有地域在用的码加 `-id-issuer-retire-unbound`。它跳过绑定核对,退了就收不回来。
- 不要收回 data-service 读 `namespaces/kube-system` 的权限,也不要给它关掉 ServiceAccount 令牌挂载。读不到部署身份,发号器就不预留新块。
- 不要把登记处的服务端私钥拷到托管集群以外,也不要给同一个世界签第二个 CA。别处有了能冒充登记处的证书,发号器就分不出连的是哪一套。
- 不要为了让握手通过,把 `ca.crt` 换成别的 CA 或关掉 TLS。握手失败说明连错了或证书错了,按 §3 处置。
- 换码时不要改 `-Region`:它是存档库编号,改了老玩家存不了盘。只改 `-IdIssuerRegion`(§4.5)。

### 4.8 换证书

- **客户端证书**(过期前、或怀疑泄露):用世界 CA 重签,更新这个地域每个 zone 命名空间的 Secret `id-registry-client-tls`。发号器每次重连都从文件重读,新证书在下一次重连时生效,不用重启;要立刻生效就滚动重启 data-service。
- **服务端证书**:用世界 CA 重签(SAN 照旧写全),更新托管集群的 Secret `etcd-id-registry-tls`。etcd 每次握手都读证书文件,不用重启;旧连接断开重连后用新证书。
- **世界 CA**(到期或泄露,全世界一起做):
  1. 生成新 CA。把新旧两个 CA 拼进同一个 `ca.crt`,同时更新登记处和全部地域的 Secret,滚动重启登记处 etcd 和全部 data-service。
  2. 用新 CA 重签服务端证书和全部客户端证书,逐个替换。
  3. 确认没有旧 CA 签的证书在用之后,`ca.crt` 只留新 CA,再滚动重启一次。

  中间任何一步都不需要停发号:每一步两边都同时认新旧 CA。

## 5. 演练

定期在 staging 上做,证明上面的流程真的成立。每一条都要断言新发的号比之前预留过的都大(按计数器比较,`idsegment.UnmixID` 可以把 ID 还原成计数器)。

**演练只在一套隔离的演练登记处上做,不碰全世界共用的登记处。** staging 和生产共用登记处,而第 6 步要删登记处的键;在共用登记处上做,就是手工改生产记录(§4.7),手滑删错一个码就是一次 ID 安全事件。

演练前的准备:

1. 在 staging 集群另起一套演练用的 etcd:同一份 `etcd-id-registry.yaml`,部署到另一个命名空间。
2. 另起一个演练 CA,给它签服务端和客户端证书(§6)。不要用世界 CA:演练环境的证书拿到真登记处也连不上,配错了也伤不到它。
3. 演练用的 data-service 配置把 `RegistryEtcd`、`RegistryTLS` 指向这套,用它自己的 `-id-registry-open -id-registry-next 1` 开启,再 `-id-issuer-init`,然后做下面 1~7。

演练完删掉这套 etcd、证书和配置。

1. 删掉本地主记录:发号不停,下一块把它补回来。
2. 把本地副记录改小:发号不停,下一块跳到主记录之上。
3. 先拍本地两套 etcd 的快照,再发几块号,再把两套都恢复到那份快照:发号不停,新块在登记处水位之上,指标 `records_behind_registry` 增长。
4. 删掉本地两份记录:发号器拒绝;`-id-issuer-recover` 不带 rebind 拒绝;带 rebind 之后恢复。
5. 在另一个 K8s 集群里,用同一份清单和同一个地域码部署。这时两边本地 etcd 的集群号相同,只有部署身份不同。预期:初始化报 `was already claimed`,发号报 `not the ones bound`,登记处不变。再把原地域的 etcd 数据目录恢复过去(克隆快照),照样拒绝。
6. 删掉登记处这个码的记录:发号器拒绝;`-id-issuer-recover` 重新登记之后恢复。
7. 在另一个 K8s 集群里,对这个在用的码跑 `-id-issuer-retire`:拒绝,登记处不变。

单机上的自动化演练(真 etcd、真快照恢复)由 Codex 按 [交接说明](../handoff/global-id-issuer-registry-20261105.md) 执行。

## 6. 证书怎么签

用 openssl 1.1.1 以上,在一台不进任何集群的运维机上做;世界 CA 的私钥离线保存。下面 `<infra>` 是托管集群的 infra 命名空间,`<私网地址>` 是别的集群访问登记处的地址(域名或 IP,每个都要写进 SAN)。

```bash
# 世界 CA(全世界一次)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
  -subj "/CN=mmorpg-id-registry-ca" -keyout ca.key -out ca.crt

# 登记处服务端证书。用途同时写 serverAuth 和 clientAuth:etcd 开 --client-cert-auth 时,它自己的网关拿服务端证书当客户端证书用
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=etcd-id-registry" -keyout tls.key -out tls.csr
cat > server.ext <<'EOF'
subjectAltName = DNS:etcd-id-registry.<infra>, DNS:etcd-id-registry.<infra>.svc, DNS:etcd-id-registry.<infra>.svc.cluster.local, DNS:<私网地址>
extendedKeyUsage = serverAuth, clientAuth
EOF
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 -extfile server.ext -out tls.crt
kubectl -n <infra> create secret generic etcd-id-registry-tls --from-file=ca.crt --from-file=tls.crt --from-file=tls.key

# 一个地域的客户端证书(每个地域一张,CN 写地域码),在这个地域每个 zone 命名空间各建一份
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=region-<码>" -keyout client.key -out client.csr
printf 'extendedKeyUsage = clientAuth\n' > client.ext
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 -extfile client.ext -out client.crt
kubectl -n <zone-namespace> create secret generic id-registry-client-tls \
  --from-file=ca.crt --from-file=tls.crt=client.crt --from-file=tls.key=client.key
```

证书有效期 825 天,在到期前按 §4.8 换。演练用另起的演练 CA,命令相同。
