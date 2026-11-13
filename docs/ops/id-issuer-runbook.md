# 发号器运维手册

发号器是 `data_service` 的 `AllocateIdSegment` 背后的实现(`go/data_service/internal/idissuer`),给 player_id、guild_id、物品 guid、流水号等所有永久 ID 发号。设计见 [玩家数据只存 TiDB 总体方案](../design/player-data-tidb-scaling-plan.md) §6。

> 2026-11-05 起有三份预留记录和全局号源登记处,首次初始化和事故恢复是两个入口。**未编译、未测试、未演练**,第一次按本手册操作之前先在 staging 走一遍 §5。
> 2026-11-11 起地域码绑定加上部署身份(本进程所在 K8s 集群的 kube-system UID,运行期读),在用的码只能从绑定它的地域退役([交接 §8](../handoff/global-id-issuer-registry-20261105.md)),同样**未编译、未测试**。
> 2026-11-11 第二批([交接 §9](../handoff/global-id-issuer-registry-20261105.md)):登记处只走双向 TLS;地域码只能按顺序认领;dev、test 不能连共用登记处;换码只换发号码(`-IdIssuerRegion`),存档库的地域编号不变。同样**未编译、未测试**。
> 2026-11-12 第三批([交接 §10](../handoff/global-id-issuer-registry-20261105.md)):本地记录也写上部署身份,登记处丢码之后拷来的记录不能用来重新登记;没认领过的码只能按顺序跳过,不能隔空补墓碑;退役别的码用 `-id-issuer-retire-code`。同样**未编译、未测试**。
> 2026-11-12 第四批([交接 §11](../handoff/global-id-issuer-registry-20261105.md)):登记处整个丢了时先声明重建(`-id-registry-rebuild`)再补墓碑,没有 `next_region` 时补墓碑一律拒绝,不再有手工删登记处记录的例外;登记处丢码时重新登记的水位加上时钟推得出的上界,本地两份随即抬齐;活着的发号进程每 10 秒给三份记录作证,被回档或删掉的抬回去(`records_behind_issuer`)。同样**未编译、未测试**。

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
- **两份本地记录里也写着部署身份**:每次预留、初始化、恢复建记录时,写下它的那个 K8s 集群的身份。登记处有这个码时以登记处的绑定为准,不看它。登记处丢了这个码时,它是「谁是绑定的那一份」唯一幸存的证据:只有写下本地记录的那个环境能重新登记(§4.3)。
- 地域码按从小到大、一个接一个认领。登记处的 `/id_registry/v1/next_region` 是下一个可认领的码,认领一次就推过去。初始化只能认领它本身:填大了报 `region codes are claimed in order`,什么都不写,免得一次填错就把中间的码永久烧掉。**用过的码永不再分配**:哪怕它那条记录丢了,它也小于 `next_region`,初始化照样拒绝。
- `next_region` 有三种状态:
  - 不存在:新世界还没开启,或者登记处整个丢了数据。
  - 重建中(值是 `{"next":N,"rebuilding":true}`):`-id-registry-rebuild` 声明的。补进来的记录都按它核对,初始化一律拒绝(`being rebuilt`)。
  - 已开放(`{"next":N}`):`-id-registry-open` 之后,初始化可以认领 N。

  有地域记录却没有 `next_region` 的登记处是丢过数据的,`-id-registry-open` 拒绝直接开启(`has region entries but no next claimable code`),要先声明重建(§4.3)。
- 有了 `next_region` 之后(重建中或已开放),**有记录的码都小于它**,发号器在每次补记录时守住这一条:
  - 没有 `next_region` 时补墓碑,拒绝(`the id registry is not opened`):没有东西可以对照,填错的码写进去之后,声明指针就被它卡住。
  - 给一个从没认领过、又大于 `next_region` 的码补墓碑,拒绝(同样报 `region codes are claimed in order`)。放它进去的话,顺序认领走到这个码就既认领不了、也跳不过去,后面的码全部卡死。
  - 墓碑恰好补在 `next_region` 上,就是「这个码不用了,跳过」:写墓碑,指针推到下一个(§4.6)。
  - 在用的地域凭本地记录重新登记到 `next_region` 或之上(重建时声明得太低,台账漏记了它),指针跟着推到它后面;重建中推完仍在重建中。
- **活着的发号进程是另一个证人。** 它记得自己确认过的水位:自己三份都写成功的块尾,以及作证检查读到的登记处水位。预留从它之上开始;每 10 秒读一次三份记录,有一份低于它(被回档、被删)就抬回去,指标 `records_behind_issuer`。
  - 三份记录一起被回档,或者本地两套回档的同时登记处丢了这个码,记录自己证明不了什么,靠的就是它。
  - 登记处丢了这个码时只抬本地两份,不替它建登记处的记录(那是 `-id-issuer-recover` 的事,要人看过)。
  - 码退役或 rebind 到别处之后,它不再碰任何记录。
- 第四道防线是时间下限:每块的起点不低于当前时间对应的计数器值。它只会把起点往上抬,坏时钟不会造成重号。登记处丢码、凭本地记录重新登记时,它还给水位加一道上界(§4.3),同样只往上抬。
- 记录的值:本地是 `{"counter":<计数器>,"authority":"<32 位十六进制>","deployment":"k8s:<uid>"}`(dev 写的记录没有 `deployment`);登记处是 `{"state":"active|retired","authority":...,"deployment":"k8s:<uid>","primary_cluster":...,"secondary_cluster":...,"high_water":...,"blocks":...}`。计数器是 `地域 << 57 | 秒 << 24 | 序号`,秒从 2026-01-01 UTC 起算。不认旧的纯数字格式。
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

   输出 `id registry opened, the next claimable region code is 1`。已开启时什么也不做。staging 和 prod 共用这一个登记处,和[地域码登记表](open-new-region-runbook.md) §3 是同一套码。

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
| `the id registry is being rebuilt` | 登记处整个丢过数据,正在按 §4.3 重建,还没开放认领 | 等重建做完(`-id-registry-open`)再初始化。什么都没写 |
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
| `result="ahead_of_clock"` | 计数器比时钟超前 7 天以上:时钟曾跳到未来,或者预留快过了时钟(崩溃循环)。登记处丢码、重新登记之后的几分钟里也会出现:重新登记把计数器放到了时钟前面 7 天 | 不影响唯一性。修时钟或崩溃循环,记下来。**这个地域出现过它,登记处丢码时就不能重新登记**,时间上界不一定盖得住(§4.3) |
| `result="records_behind_issuer"`,日志 `which this process already confirmed` | 有记录低于活着的发号进程确认过的水位:那套 etcd 被旧数据覆盖过,或者记录被删了。日志带三份记录的值 | 发号不受影响,作证检查已经把低的抬回去(登记处丢了这个码时只抬本地两份)。按 ID 安全事件复盘,查是谁恢复或删除了 etcd 数据。三份都低于它,说明三套一起出了事,对照 §4.1、§4.3 |
| 日志 `belongs to another region`(`outcome="refused"`) | 记录的地域和配置不一致 | 配错了 `IdSegment.Issuer.Region`。改回去,不要改记录 |
| 日志 `same etcd cluster`(`outcome="refused"`) | 三套 etcd 里有两套是同一套 | 配错了地址。生产必须三套独立 etcd |
| 启动报 `RegistryEtcd must be empty when AllowAutoSeed=true` | dev、test 档配了显式的登记处地址 | dev、test 不核地域码绑定,不能连共用登记处。删掉这一项,登记处回落到本地 etcd |
| 启动报 `reserved for dev and test` | `Mode` 不是 dev、test,`IdSegment.Issuer.Region` 漏写或写了 0 | 配上这个地域的码,见[开新地域手册](open-new-region-runbook.md) §3 |
| 启动报 `RegistryEtcd are all required` | 生产没配登记处地址 | 在 Secret 和部署参数里补上 `RegistryEtcd` / `-IdRegistryEtcd` |
| `outcome="exhausted"` | 本地域的值域见底 | 事故。只可能是时钟曾跳到很远的未来。换一个没用过的地域码(§4.5) |
| `outcome="unknown_tag"`,`biz_tag="unregistered"` | 种类没登记。`biz_tag` 这个 label 只原样记登记过的种类,其余记 `unregistered`(格式合法)或 `invalid`(格式不对),基数有界 | 加进 `IdSegment.Tags`(`config.go`、`data_service.yaml`、`k8s_deploy.ps1` 三处) |

## 4. 恢复

原则:**上界只取幸存的证据(三份记录、活着的发号进程确认过的水位、程序按时钟推出的上界),不猜起点,不手算时间余量,不手工改记录。** 拿不到可信上界就拒绝发号,换一个新码。

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
| 回档(记录都在) | 回档(记录都在) | 回档 | 活着的进程从它确认过的水位之上继续,并把三份抬回去 | §4.1 |
| 回档或丢了 | 回档或丢了 | 丢了 | 拒绝;活着的进程先把本地两份抬回去,`-id-issuer-recover` 重新登记时再加时间上界 | §4.3 |
| 都丢了 | 都丢了 | 丢了 | 拒绝,不能恢复,换码(活着的进程把本地两份建回来了的话,按上一行) | §4.4 |

**旧发号进程不需要先停。** 三份记录每次预留都重读,旧进程下一次预留只能落在可信上界之上,或者被拒;它们内存里的块都在上界之下,接着发也不重号。恢复命令也不需要重启任何服务。

### 4.1 本地一份丢了或回退,或者本地两套一起回档

不用停服,不用改记录,下一次预留自动对齐:

- 一份丢了或回退:另一份和登记处盖得住,指标 `one_record_missing` / `record_behind`。把坏的那套恢复起来就行(方法见 [etcd 运维手册](./etcd-maintenance-runbook.md))。
- 两套都被旧快照覆盖、记录都还在:登记处盖得住,下一块从登记处水位之上开始,指标 `records_behind_registry`。按 ID 安全事件复盘,查清楚为什么两套被同时恢复。
- 本地两套和登记处三份一起被旧快照覆盖:记录自己证明不了什么。活着的发号进程记得自己确认过的水位,下一块从它之上开始,作证检查在 10 秒内把三份抬回去,指标 `records_behind_issuer`。出事时还在跑的进程越多,盖得越全;全部进程都重启过之后才出事的,这一条帮不上忙,按 ID 安全事件处理,核对出事之后发出的号有没有和之前的重复。

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
- **登记处丢了一个码**:这个地域预留和初始化都拒绝,日志 `not registered`。在这个地域跑 `-id-issuer-recover`(不带 rebind)重新登记,输出 `re-registered region <码> ... at high_water=<水位> and raised the local records to it`。
  - 水位取本地两份记录的最大值和「现在的时间下限 + 7 天 + 一块」里大的那个,本地两份随即抬到这个水位。为什么要加时间上界:本地记录自己证明不了没被回档。同一个 K8s 集群里本地两套 etcd 被恢复到旧快照、登记处又同时丢了这个码,本地记录就比发过的号旧。每块的起点比当时的时间下限超前 7 天以上就会告警(`ahead_of_clock`);没告警过,发过的号就都在这个上界之下。代价是计数器一次跳到时钟前面 7 天,之后几分钟会有 `ahead_of_clock`,值域浪费可以忽略。
  - **重新登记之前,先查指标历史,确认这个地域从没出现过 `ahead_of_clock`。** 出现过的话,时间上界不一定盖得住,按 §4.4 换码,不要重新登记。
  - 出事时还在跑的发号进程会先把本地两份抬到它们确认过的水位(§1),所以这种情况下本地记录本身就够高。
  - 报 `more than 7 days ahead of the clock`:本地记录本身就超前时钟 7 天以上,推不出上界。拒绝,什么都不写,按 §4.4 换码。
  - 报 `outside the representable range`:时钟坏了,修好再跑。什么都没写。
  - 报 `raising the local records to it failed`:登记处已经重新登记了,抬本地记录时某一套 etcd 写失败。修好那套 etcd 后重跑 `-id-issuer-recover`,输出 `raised the local records`。不重跑也不会重号,只是下一次预留会误报 `records_behind_registry`。
  - 只有写下本地记录的那个环境能重新登记:发号器比对本地记录里的部署身份和本进程所在的 K8s 集群。
  - 报 `local reservation records were not written by this deployment`,并列出记录里的身份和本环境的身份:这套环境的 etcd 数据是从别的环境拷来的(集群级备份恢复),或者是 2026-11-12 之前写的、没有身份的记录。拷来的记录可能比原地域发过的号旧,不是可信的上界,所以这里不能重新登记,加 `-id-issuer-rebind` 也不放行。登记处在的时候 rebind 是安全的,因为水位由登记处盖住;现在登记处没有这个码,过期的水位没有任何东西纠正。
  - 这时分两种情况处置。**先不要退役这个码**:补了墓碑,原地域要是还在,就再也登记不回来了。
    - 这套环境是克隆(原地域还在):它不该用这个码。去原地域跑 `-id-issuer-recover`;克隆要发号,就给它自己的码。
    - 原地域确实已经不在,这里是它的灾备:这个码恢复不了。先确认原地域的 data-service 已经全部停掉,再按 §4.5 给这个地域换一个发号码。新码发号正常之后,才用 `-id-issuer-retire -id-issuer-retire-code <旧码>` 给旧码补墓碑,登记表里标「退役」。
- **登记处整个丢了**:所有地域拒绝新块,新地域初始化报 `not opened`。重建,不从备份恢复。顺序是:先声明下一个可认领的码,再补记录,最后开放认领。
  1. 重新部署登记处 etcd(托管集群 `infra-up -HostIdRegistry`),数据目录是空的。沿用原来的 Secret `etcd-id-registry-tls`,也就是同一个世界 CA、同一张服务端证书:换了 CA,所有地域都连不上,还得全世界重签客户端证书。换托管集群时,新的访问地址要先在服务端证书的 SAN 里(§6)。
  2. 在任意一个地域的 data-service Pod 里声明重建,下一个可认领的码取[地域码登记表](open-new-region-runbook.md) §3 里用过的最大码 +1:

     ```bash
     kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-registry-rebuild -id-registry-next <最大码+1>
     ```

     输出 `id registry rebuild started at next claimable region code <值>`。从这一刻起,补进来的记录都按它核对,初始化一律拒绝(`being rebuilt`)。已经声明过时什么也不做。有地域先跑过第 3 步时,`<最大码+1>` 必须高于它们,否则报 `already has an entry`:台账漏记了那个码,先改台账。
  3. 每个在用的地域各跑一次 `-id-issuer-recover`,重新登记(上面「登记处丢了一个码」那一条,先查 `ahead_of_clock`)。台账漏记的在用码会把指针推到它后面,这时补上台账。在用的地域也可以早于第 2 步重新登记,让发号尽早恢复,第 2 步会核对。
  4. 第 3 步全部做完之后,按登记表,对每个用过、但第 3 步没有重新登记的码补墓碑。这包括已退役的码,也包括整个地域已经不在、记录全丢的码:不补的话,它会被当成没用过的码重新认领,新地域发的号就和它当年发过的撞上。做法:在任意一个地域的 data-service Pod 里,对每个码跑一次 `-id-issuer-retire -id-issuer-retire-code <码>`(§4.6)。
     - 不要加 `-id-issuer-retire-unbound`。登记处里没有这个码时本来就不核绑定;码已经被第 3 步重新登记成在用的,不加它就会被绑定核对拒绝,说明台账错了,停下来查。
     - 墓碑按指针核对:小于指针的写入;填错成大于指针的拒绝(`region codes are claimed in order`),什么都不写;恰好等于指针的就是「跳过这个码」,指针加一。
     - 第 2 步之前补墓碑一律拒绝(`the id registry is not opened`)。
  5. 开放认领:`-id-registry-open -id-registry-next <值>`,值等于登记处里现在的指针(第 3、4 步可能把它推高过)。对不上报 `does not match the one the id registry holds`,什么都不写:用 `etcdctl get /id_registry/v1/next_region` 查现在的值,对着台账确认之后用它重跑。输出 `id registry opened, the next claimable region code is <值>`。这一步之前任何初始化都被拒绝。

  跳过第 2 步直接开启会被拒绝(`has region entries but no next claimable code`):有地域记录却没有指针,就是丢过数据的登记处,直接开启会跳过「重建期间不许认领」那段窗口。只有一条记录都没有的登记处(新世界)才能直接开启。

登记处和本地同时出事(本地被回档或丢了):活着的发号进程会先把本地记录抬回去(§1),重新登记还加时间上界,所以多数情况照常按上面恢复。这个地域出现过 `ahead_of_clock`,或者重新登记报 `more than 7 days ahead`,走 §4.4。

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

地域已经下线、没法在那里执行时:确认它的 data-service 已经全部停掉、不会再发号,再从别的地域的 data-service Pod 里执行,用 `-id-issuer-retire-code` 指明要退役的码,加 `-id-issuer-retire-unbound` 跳过上面的核对:

```bash
kubectl -n <zone-namespace> exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-retire -id-issuer-retire-code <码> -id-issuer-retire-unbound
```

`-id-issuer-retire-code` 让这一次退役操作别的码,不用为此去改配置里的地域码(配置里的码有部署注入的值把关,改了进程拒绝启动)。它必须是 1..63,显式给了不合法的值直接报错,不会回落到配置里的码。不带 `-id-issuer-retire-unbound` 时,在用的码照样被绑定核对拒绝。登记处里没有这个码时(§4.3 补墓碑、§4.4)不核绑定,不需要 `-id-issuer-retire-unbound`。

登记处里没有记录的码,能不能补墓碑看它和 `next_region` 的关系。没有 `next_region` 时一律拒绝(`the id registry is not opened`),登记处整个丢了时先按 §4.3 第 2 步声明重建;重建中和开放之后都按下表:

| 这个码 | 结果 |
|---|---|
| 小于 `next_region`(用过,记录丢了) | 写墓碑 |
| 等于 `next_region` | 写墓碑,`next_region` 推到下一个。这就是「跳过一个码」:这个码不给任何地域用了。命令是 `-id-issuer-retire -id-issuer-retire-code <next_region 的值>` |
| 大于 `next_region`(从没认领过) | 拒绝,输出 `region codes are claimed in order ... there is nothing to retire`,什么都不写。多半是地域码填错了 |

### 4.7 不要做的事

- 不要手工改记录。只增不减是唯一性的前提。
- 不要从备份恢复登记处,更不要和本地 etcd 同时恢复。
- 不要用同一个备份、同一时刻恢复本地两套 etcd。它们能自动恢复,是因为登记处没有一起回档。
- 不要把三套 etcd 里任意两套配成同一个集群。生产上发号器拒绝地址有交集的配置,也会比集群号,但改 DNS 指到同一套,配置校验查不出来。
- 原来那一份(原来的 K8s 集群或那两套 etcd)还在给这个码发号时,不要 rebind。
- 不要把用过的地域码分给别的地域。
- 不要对一个还有地域在用的码加 `-id-issuer-retire-unbound`。它跳过绑定核对,退了就收不回来。
- 登记处丢了码的时候,不要在数据是拷来的环境里退役这个码(§4.3)。发号器已经拒绝它重新登记;在这里补墓碑,会把原地域的码永久废掉。
- 登记处丢码时,出现过 `ahead_of_clock` 的地域不要重新登记,换码(§4.3)。
- 登记处整个丢了时,不要在在用的地域重新登记完之前补墓碑(§4.3 第 4 步):墓碑补到一个还没登记回来的在用码上,那个地域就再也登记不回来了。
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
3. 演练用的 data-service 配置把 `RegistryEtcd`、`RegistryTLS` 指向这套,用它自己的 `-id-registry-open -id-registry-next 1` 开启,再 `-id-issuer-init`,然后做下面 1~10。

演练完删掉这套 etcd、证书和配置。

1. 删掉本地主记录:发号不停,下一块把它补回来。
2. 把本地副记录改小:发号不停,下一块跳到主记录之上。
3. 先拍本地两套 etcd 的快照,再发几块号,再把两套都恢复到那份快照:发号不停,新块在登记处水位之上,指标 `records_behind_registry` 增长。
4. 删掉本地两份记录:发号器拒绝;`-id-issuer-recover` 不带 rebind 拒绝;带 rebind 之后恢复。
5. 在另一个 K8s 集群里,用同一份清单和同一个地域码部署。这时两边本地 etcd 的集群号相同,只有部署身份不同。预期:初始化报 `was already claimed`,发号报 `not the ones bound`,登记处不变。再把原地域的 etcd 数据目录恢复过去(克隆快照),照样拒绝。
6. 删掉登记处这个码的记录:发号器拒绝。先在第 5 步那个带克隆数据的集群里跑 `-id-issuer-recover`,带不带 `-id-issuer-rebind` 都试一次:都拒绝,报 `were not written by this deployment`,登记处仍然没有这个码。再在原地域跑 `-id-issuer-recover`:重新登记,发号恢复。
7. 在另一个 K8s 集群里,对这个在用的码跑 `-id-issuer-retire`:拒绝,登记处不变。
8. 用 `-id-issuer-retire -id-issuer-retire-code <码>` 退役一个远大于 `next_region` 的码:拒绝,`next_region` 不变。再对 `next_region` 本身跑一次:写墓碑,`next_region` 加一,之后的新码照常初始化。
9. 演练登记处整个丢失:删掉演练登记处的全部键。补一个墓碑:拒绝(`not opened`)。在一个地域跑 `-id-issuer-recover`;`-id-registry-open` 报 `has region entries but no next claimable code`;`-id-registry-rebuild` 给一个不大于已登记码的值,报 `already has an entry`;给对的值声明;`-id-issuer-init` 报 `being rebuilt`;补一个小于指针的墓碑(写入)、一个远大于指针的墓碑(拒绝);`-id-registry-open` 给错的值报 `does not match`,给登记处里的值开放;之后新码照常初始化。
10. 三份一起回档:拍本地两套 etcd 和演练登记处的快照,发几块号,再把三套都恢复到快照,发号进程不重启。预期 10 秒内日志出现 `which this process already confirmed`、指标 `records_behind_issuer` 增长,三份记录回到恢复前的水位;之后新起的进程从那之上发号。再做一遍,这次把演练登记处这个码的键删掉而不是恢复:本地两份被抬回去,登记处不出现这个码;`-id-issuer-recover` 重新登记的水位不低于恢复前的水位。

单机上的自动化演练(真 etcd、真快照恢复)由 Codex 按 [交接说明](../handoff/global-id-issuer-registry-20261105.md) 执行。

## 6. 证书怎么签

用 openssl 1.1.1 以上,在一台不进任何集群的运维机上做;世界 CA 的私钥离线保存。下面 `<infra>` 是托管集群的 infra 命名空间,`<私网地址>` 是别的集群访问登记处的地址,每个都要写进 SAN:域名写 `DNS:<域名>`,IP 写 `IP:<地址>`。发号器按 `RegistryEtcd` 里写的主机名或 IP 核对证书,配置里写的是什么,SAN 里就要有什么。

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
