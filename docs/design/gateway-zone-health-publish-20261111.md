# 网关区服健康:每 zone 发布汇总记录,网关只读汇总(2026-11-11)

> 状态:**已落码,未编译、未测试,待 Codex 验证**(命令见 §9)。对应[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md)第 29 条残余:健康探测与 login 发现 11-05 已改成 list + watch 镜像,但每个网关副本仍 watch 全服的 gate / scene 前缀,事件扇出随 zone 数平方增长。
> 相关:[区服列表静态发布提案](serverlist-static-publish-2026-05.md)(未实施)、[连接与发现扇出](connection-fanout-subsetting-20261106.md)、[控制面按 zone 化](control-plane-zone-scoping-20261105.md)。

## 1. 结论

1. **每个 zone 的健康由本 zone 的网关副本自己算、自己发布。** 网关副本只 list + watch 本 zone 的 gate 与 scene 前缀(`GateNodeService.rpc/zone/<z>/`、`SceneNodeService.rpc/zone/<z>/`),每 5 秒算一次本 zone 的汇总(健康状态、在线人数、负载档分母),写进 etcd 的一条记录。记录挂在发布进程的租约上,进程死了记录随租约消失。
2. **网关读区服健康只读这些汇总**,不再 watch 全服 gate / scene,也不再每 5 秒 `findAll` 区服目录。读方对汇总前缀做 list + watch,每个 zone 取最近写入的一条。
3. **只在显示会变时才写。** 状态或分母变了立即写;在线人数漂移满分母的 1% 时,负载档跨档立即写,否则同一条记录 30 秒内最多写一次。稳态几乎不写,汇总的写入量与 gate 心跳脱钩。
4. **login 发现保持全服。** 客户端只有一个网关入口(`gateway_addr`),任一网关都要能把任一 zone 的 assign-gate 转给那个 zone 的 login;login 的登记只在进程启动、失租时写,没有周期重写,全服 watch 的扇出可以忽略(§6)。
5. **不做区服列表静态发布**(§7):它要客户端改读法、要对象存储与 CDN,而动态的网关入口本来就得留着;本次只把「每个网关为全服算健康」这个根因去掉。
6. 每个副本负责哪些 zone 由新属性 `gateway.home-zone-ids` 给出:K8s 的网关 ConfigMap 写 `[<本 zone>]`,本机缺省 `[1, 2, 3]`。区服策略发布(「Zone 人数满的处理」会话写 Redis 给本区 login 读)共用这个属性。

## 2. 现状(2026-11-11 核对)

| 项 | 位置 | 现状 |
|---|---|---|
| 网关部署 | `deploy/k8s/manifests/java-svc/gateway.yaml`、`k8s_deploy.ps1` 的 `Apply-JavaSvcManifests` | 每个 zone namespace 一个 StatefulSet,2 副本;所有 zone 共用 infra 的 etcd 与全球 SQL 入口 |
| 客户端入口 | Java 仓 `docs/reference/mmorpg-client-contract-robot.md` 第 37 行 | `gateway_addr` 是唯一入口,server-list 与 assign-gate 都打它;列表里没有按区的网关地址 |
| 健康探测 | `ZoneHealthProbeService.probe()` | 每副本每 5 秒:`findAll` 区服目录,读全服 gate 与 scene 镜像,按 zone 分组算 DOWN / DEGRADED / HEALTHY、在线人数、负载档分母(11-10 起按 gate 水位封顶) |
| 节点镜像 | `etcd/NodeInfoMirror.java`(11-05) | 三个全服前缀各一份 list + watch;稳态只收变更,但每台 gate 每 10 秒无条件重写一次 etcd 记录(`cpp/nodes/gate/main.cpp` 约 682 行),这些事件送到每一个网关副本 |
| login 发现 | `grpc/LoginNodeDiscovery.java` | 读全服 `LoginNodeService.rpc/`,按 zone 建 login 通道池;login 登记一次后只续租约(`go/login/internal/logic/pkg/etcd/registry.go`),不周期重写 |
| scene 登记 | C++ scene | 只在注册、重新注册时写,不周期重写 |

根因:区服列表是全服的,而网关按 zone 部署;每个 zone 的网关都在为全服算健康,于是每个网关副本都要收全服 gate 的心跳。

## 3. 方案比较与定案

候选一(**采用**):每个 zone 一份汇总,由本 zone 的网关副本发布,网关只读汇总。

候选二:区服列表改静态发布(一个发布者算全服列表,经对象存储 / CDN 分发),网关只看本 zone。

| | 候选一 | 候选二 |
|---|---|---|
| 去掉的东西 | 网关副本 watch 全服 gate / scene;读方每 5 秒 `findAll` | 同左,另外列表的 HTTP 读也离开网关 |
| 读方扇出 | 2Z 个副本 × 汇总写入量;写入量由显示变化决定,与 gate 心跳无关(§6) | 列表读量交给 CDN,与网关副本数无关 |
| 新依赖 | 无(etcd 已在用) | 对象存储、CDN、发布任务、客户端读取顺序与回退、版本与过期规则(提案 §3 列的四类验证都没做) |
| 客户端 | 不改 | 要改(独立客户端仓,本会话无授权) |
| 动态入口 | 不变 | 提案明确网关的动态入口不能提前切成「仅兜底」,兜底仍要全服视图,网关上的问题照旧存在 |
| 故障半径 | 一个 zone 的发布者全挂,只有这个 zone 显示 UNKNOWN | 单一发布者挂了,全服列表一起过期 |
| zone 隔离 | 只读写本 zone 的数据,与控制面按 zone 化同一不变量 | 发布者要读全服 |

取候选一的理由:根因是「每个网关为全服算健康」,候选一正好把计算收回到各自 zone,读方只剩「每个网关要知道每个 zone 的一行汇总」,这是「每个 zone 的网关都要能回全服列表」本身的代价,写入频率由显示需要决定;候选二要客户端、对象存储与 CDN 一起落地,而且网关的动态入口还在,网关上的问题并没有消失。候选二留作以后的方向:真要让区服列表的读与 zone 数无关,做法是把列表从各 zone 的网关挪出去,那时直接读这些汇总记录生成静态文档即可,本设计的记录可以原样复用。

候选一内部的取舍:

- **发布者是网关,不是 login。** login 也按 zone 部署、也看本 zone 的 gate,但它不看 scene(DEGRADED 要用),改它要跨语言定记录格式、动 Go 服务;网关本来就有这段计算(11-10 刚加的分母封顶也在这里)。
- **不选主,每个副本各写各的键。** 选主要多一套竞选、观察、让位与写入防护,换主期间这个 zone 没有记录;各写各的键时,一个副本重启另一个的记录还在,滚动发布不出空窗。代价是同一次变化写两条,读方每 zone 取 ModRevision 最大的一条(最近一次写入反映最近一次观察到的变化;卡住的发布者不再写,自然让位)。这与节点登记本身同一种模式:每个实例一条带租约的键,读方汇总。
- **汇总带算好的分母**:发布者读本 zone 的 `zone_config.capacity`(按主键点查),与本 zone 的 gate 一起算 `effectiveCapacity`,记录里写最终分母。读方不再需要目录行,负载档与负载率直接由记录算;1% 的写入阈值也按真实分母算。
- **不放 Redis**:读方是所有 zone 的全部副本,要「有变化才推送」;Redis 只能轮询,轮询读量就是 2Z 个副本 × Z 条,又回到平方。etcd 的 watch 与租约正好表达「变化推送」与「发布者还活着」。

## 4. 设计

### 4.1 记录

- 键:`/mmorpg/zone_health/zone/<zoneId>/publisher/<instanceId>`。前缀沿用 `/mmorpg/<用途>/`(同 `/mmorpg/killswitch/`、`/mmorpg/db_placement/`)。`instanceId` 是发布进程启动时生成的随机 UUID。
- 值:普通 JSON(Jackson),字段只做加法演进:`{"zoneId":1,"status":"HEALTHY","onlineCount":1234,"capacity":36000}`。
  - `status` 只取 `HEALTHY` / `DEGRADED` / `DOWN`,与原探测同一判定(没有 gate 判 DOWN;有 gate 没 scene 判 DEGRADED)。发布者从不写 UNKNOWN:没有记录就是不知道。
  - `onlineCount` 是本 zone 全部 gate 的 `playerCount` 之和。
  - `capacity` 是负载档的分母,`effectiveCapacity(zone_config.capacity, 本 zone 的 gate)`,口径同 11-10(目录容量按 gate 到 90% 水位能接的人数封顶;有 gate 没声明上限不封顶;目录没配时只看 gate;都没有时为 0,负载档恒为流畅)。
- 校验(读方):键形不对、JSON 解析失败、键里的 zone 与值不一致、`status` 不是三者之一、数值为负,都是坏记录。
- 租约:每个发布进程一份,TTL = `zone-probe.status-ttl-ms`(缺省 15 秒),每拍续一次。要求 `interval-ms × 2 ≤ status-ttl-ms`,否则拒绝启动,保证错过一拍也不失租。

### 4.2 发布者(`service/ZoneHealthPublisher`)

每 `zone-probe.interval-ms`(5 秒,固定频率)一拍,`gateway.home-zone-ids` 为空时什么都不做(不申请租约):

1. 续租(`LeasedEtcdWriter.renewLease`)。拿到的是新租约时,挂在旧租约上的记录已随它消失,本拍全部重写。续租失败(etcd 不可达)本拍不写,记录随旧租约到期消失。
2. 对每个本副本负责的 zone:
   - 读本 zone 的 gate、scene 镜像(`GateWatcher.fetchZoneGateNodes` / `fetchZoneSceneNodes`,zone 前缀的 list + watch)与目录行,算出汇总。
   - 判断要不要写(`shouldPublish`,纯函数):还没写过(或租约换了)→ 写;状态或分母变了 → 写;在线人数与已写的相差不到分母的 1%(向上取整,至少 1)→ 不写;相差够了且负载档变了 → 立即写;相差够了、档没变 → 距上次写满 30 秒才写。分母为 0 时在线人数不影响显示,只随别的变化一起写。
   - 「不写」也算核实:已写的记录仍在阈值内描述这个 zone。
3. 一条记录连续 `status-ttl-ms` 都没被核实(读镜像失败、遇到坏 NodeInfo、读目录失败、写失败),发布者就把它删掉(撤回)。这是「查询失败保留上一轮结果、过期回 UNKNOWN」在发布侧的对应:在 TTL 内读方照旧看到上一份,过了 TTL 读方看不到这条,当 UNKNOWN。
4. 进程正常停机时撤销租约,本进程的记录立即消失;同 zone 另一个副本的记录还在。

只在调度线程上运行,状态不跨线程共享。续租靠发布循环自己(每拍 `keepAliveOnce`),不交给 jetcd 的后台续租:循环卡死时记录随租约过期,而不是被后台线程一直续着、内容却不再更新。

### 4.3 读方(`service/ZoneHealthProbeService`)

- 每 5 秒(固定间隔)读一次汇总镜像(`ZoneHealthWatcher.latestByZone`):每个 zone 取 ModRevision 最大的那条,整份结果作为一个快照原子发布,与原来的 `ProbeSnapshot` 同一结构(状态、负载档、在线人数、分母,加采集时刻)。
- 读失败(镜像需要重新整段读而 etcd 不可达,或有坏记录)保留上一份快照;快照超过 `status-ttl-ms` 或从没成功过 → `getAutoStatus` 回 UNKNOWN、`getLoadLevel` / `getLoadRatio` 回 null、`getOnlineCount` 回 0。连续 3 次失败升 ERROR。全部同原来。
- 快照里没有某个 zone(没有任何发布者的记录)→ 这个 zone 是 UNKNOWN,同「刚加进目录、还没探测到」。
- `getLoadLevel` / `getLoadRatio` 的口径不变(在线 / 分母,档位阈值 0.5、0.8),`ServerListService` 的推荐区不用改。

### 4.4 login 发现

保持读全服 `LoginNodeService.rpc/`,不收窄:

- 客户端只配一个网关地址,server-list 与 assign-gate、queue-status、login 都打它(Java 仓机器人契约 §1)。区服列表里没有按区的网关地址,客户端没法换到目标 zone 的网关。所以任一网关都要能找到任一 zone 的 login。
- 扇出可以忽略:login 的登记由 `NodeRegistry` 写一次、之后只续租约,失租才重写;scene 同理。全服 watch 只在 login 进程启停时有事件。

### 4.5 配置与部署

- `gateway.home-zone-ids`(`config/HomeZoneProperties`):区号必须为正且不重复,否则拒绝启动;空列表 = 本副本只读、不发布。jar 内缺省 `[1, 2, 3]`(本机一个网关服务全部本机区,与 `login.grpc.endpoints` 的三个区一致);测试配置写空列表。
- K8s:`k8s_deploy.ps1` 的 `New-JavaSvcConfigMapYaml` 给网关 ConfigMap 写 `gateway.home-zone-ids: [<本 zone>]`(块式序列)。挂载的配置与 jar 内配置按属性合并、列表整体替换,不会拼出 `[1, 2, 3, z]`。契约测试钉住。
- 顺带改正:原 ConfigMap 把探测周期写成 `zone.probe.interval-ms`,而代码读 `zone-probe.interval-ms`(`ZoneProbeProperties` 与 `@Scheduled` 都是这个键),这一行在 K8s 上从没生效过(值恰好等于缺省 5000)。改成 `zone-probe.interval-ms`。
- 本机多 zone:一个网关进程发布 1、2、3 三个区。只起了 zone 1 时,2、3 没有 gate,发布 DOWN,列表显示维护,与改动前相同。
- 部署闸:每个有 gate 的 zone 都要部署本 zone 的网关(`-JavaSvcRegistry`);只部署了部分 zone 的网关时,没部署的 zone 在列表里是 UNKNOWN。

### 4.6 语义对照

| 语义 | 改动前 | 改动后 |
|---|---|---|
| 查询失败 | 保留上一份快照,TTL 内照用 | 读方同左;发布方读本 zone 失败时保留已写的记录,TTL 内照用 |
| 坏记录 | 任一 NodeInfo 解析失败 → 整次读失败,全服快照过期后 UNKNOWN | 汇总记录同左(读方整次读失败);坏 NodeInfo 只让它所在 zone 的发布者算不出,TTL 后这一个 zone 的记录被撤回、显示 UNKNOWN,别的 zone 不受影响 |
| 快照过期 | UNKNOWN,不下发负载档 | 同左 |
| 没有 gate | DOWN → 手工 OPEN 显示维护 | 同左(发布者写 DOWN) |
| 整个 zone 连同它的网关都没了 | 别的 zone 的网关看到没有 gate,判 DOWN,显示维护 | 这个 zone 没有发布者,记录随租约消失,显示 UNKNOWN(开放、不带负载档) |
| 目录里有、从没部署过的 zone | DOWN,显示维护 | UNKNOWN,显示开放、不带负载档 |
| 负载数据新鲜度 | 约 5~15 秒(探测 5 秒 + gate 心跳 10 秒) | 档位跨档同左;负载率(推荐区排序用)最多晚 30 秒、偏差不到 1% |

后两行是本设计接受的代价:「探测面不可用不等于区服 DOWN」是这套探测原有的取舍(`ServerListService.resolveDisplayStatus` 的注释),真正不可登录仍由 assign-gate 的实时选点 fail-closed 拒绝。

## 5. 失败与一致性

- **发布者进程死了**:租约 15 秒内过期,记录消失;同 zone 另一个副本的记录照常。两个都死,这个 zone 显示 UNKNOWN。
- **发布者活着、看不清**(etcd 读本 zone 失败、坏 NodeInfo、目录读失败):TTL 内保留已写记录,过了 TTL 主动删掉(§4.2 第 3 条)。
- **发布者活着、watch 悄无声息地停了**:它不再写;同 zone 另一个副本照常写,读方取最近写入的那条。两个都停,镜像最多旧 5 分钟(`EtcdPrefixMirror.MAX_AGE_MS` 强制重新整段读),与改动前的镜像上限相同。
- **发布循环卡死**:不再续租,记录 15 秒后消失,读方 UNKNOWN;不会出现「记录一直在、内容不再更新」。
- **两个发布者短暂不一致**(事件送达有先后):读方取最近写入的一条,落后的那个随后观察到同一变化再写一次,收敛。
- **etcd NOSPACE**:写被拒、删与续租可用;记录过了 TTL 被撤回,读方 UNKNOWN。与 gate 心跳同时停写是同一类故障。
- **读方 etcd 不可达**:同改动前,保留快照到 TTL。
- **调度延迟**:网关的 `@Scheduled` 任务共用一个线程。发布者用固定频率、每拍先续租;同一线程上其他任务卡到超过 TTL(要 etcd 整段读超时与 SQL 读超时同时出现)时这一拍续不上租,记录短暂消失,下一拍重新申请、重写,读方期间显示 UNKNOWN。是安全方向的失败。
- **混跑窗口**(只在上线后有意义,项目尚未上线):没升级的 zone 不发布记录,升级过的网关把它们显示为 UNKNOWN。首次上线整批起步;上线后这项改动按全部 zone 的网关一起升级。

## 6. 扇出前后量级

记号:Z 个 zone,每 zone 2 个网关副本,每 zone G_z 台 gate(按 4.5 万 / zone、单台 2 万上限取 3),gate 每 10 秒重写一次 etcd 记录。Z 取[容量模型](capacity-model-hok-scale.md)的 S1 23、S2 112、S3 223,以及 2000 万在线目标约 445(2000 万 ÷ 4.5 万)。字节数按 NodeInfo 约 350 B、汇总记录(键加值)约 150 B 估。都是推算,没有实测。

| 项 | 公式 | S1 | S2 | S3 | Z≈445 |
|---|---|---|---|---|---|
| 改动前:网关收到的 gate 事件 | 2Z × Z·G_z/10 = 0.6 Z² | 317/s | 7.5k/s | 3.0 万/s(约 10 MB/s) | 11.9 万/s(约 42 MB/s) |
| 改动前:网关读目录 | 每副本每 5 秒 `findAll`,0.4 Z² 行/s | 212 | 5.0k | 2.0 万 | 7.9 万 |
| 改动后:发布者收到的本 zone 事件 | 2 × G_z/10 × Z = 0.6 Z | 14/s | 67/s | 134/s | 267/s |
| 改动后:发布者读目录 | 每副本每 5 秒按主键点查本 zone,0.4 Z 次/s | 9 | 45 | 89 | 178 |
| 改动后:读方收到的汇总事件,上界 | 4Z² × w,w ≤ 1/30 s | 71/s | 1.7k/s | 6.6k/s | 2.6 万/s(约 4 MB/s) |
| 同上,晚高峰爬坡 | 0→80% 用 2 小时,每 90 秒漂移 1%,w = 1/90 s | 24/s | 558/s | 2.2k/s | 8.8k/s(约 1.3 MB/s) |
| 同上,稳态 | 在线数波动不到分母 1% | 只有状态、分母变化 | | | |
| 读方读目录(健康) | 无 | 0 | 0 | 0 | 0 |

- 改动前的平方项是常数项:gate 心跳不停,扇出就一直在。改动后,读方事件只随显示变化,稳态接近 0;上界比改动前少约 4.5 倍(事件)、约 10 倍(字节),由 30 秒的写入间隔封顶。
- 剩下的 4Z²·w 是「每个 zone 的网关都要能回全服列表」本身的代价。要让它与 zone 数无关,只能把列表移出各 zone 的网关(§3 候选二)。压测或线上看到 etcd watch 出向成为瓶颈时再做;做时读这些汇总生成静态文档。
- login 发现:2Z 个副本 × login 进程启停次数。全部 zone 每天各滚动一次 login(每 zone 约 8 个事件)时,Z≈445 平均约 37 事件/s,集中在滚动时段。

## 7. 被否决的做法

- **区服列表静态发布**:见 §3。
- **选主后单个发布者**:换主空窗、竞选与写入防护的代码量,换来的只是事件减半。
- **一个全服发布者汇总成一份文档,网关只 watch 一个键**:事件数随 Z 线性,但每次变化重发整份(Z 条),字节仍是 Z²;一个进程替全部 zone 算,故障半径是全服,还要跨 zone namespace 选主,违背按 zone 隔离。
- **汇总放 Redis**:见 §3 末条。
- **汇总按固定周期重写**(不挂租约、靠时间戳判新鲜):每条记录每个周期都写,读方事件 4Z²/周期,与改动前同量级。
- **发布者只写原始数(gate 数、水位和),读方用目录行算分母**:读方又要读目录;发布者不知道真实分母,1% 阈值在目录容量远小于 gate 承载时会粗到 7% 以上。
- **login 发现只看本 zone**:见 §4.4。

## 8. 残余与风险

1. 读方仍是 Z² 交叉项(§6),由写入策略封顶,没有消除。
2. 「整个 zone 连同网关都没了」与「目录里有、从没部署」显示 UNKNOWN 而不是维护(§4.6)。可选的补法是读方再看 login 发现:一个 zone 一个 login 实例都没登记时判 DOWN。没做:login 发现有静态兜底配置,「没登记」不等于「不可登录」,要另定规则。
3. 没加指标与告警。原探测也没有;要加时在读侧数「目录里有、汇总里没有」的 zone。
4. 汇总记录的格式只能做加法;读方遇到不认识的 `status` 值按坏记录处理(整次读失败)。以后加状态值要先升级全部读方。
5. 30 秒、1% 两个常数是初值,没有压测依据。

## 9. 文件与验证(交 Codex)

改动(都在主工作区,未编译):

- 新增 `java/gateway_node/src/main/java/com/game/gateway/config/HomeZoneProperties.java`、`etcd/EtcdPrefixMirror.java`(从 `NodeInfoMirror` 抽出的 list + watch 引擎,带 ModRevision)、`etcd/LeasedEtcdWriter.java`、`etcd/ZoneHealthRecord.java`、`etcd/ZoneHealthWatcher.java`、`service/ZoneHealthPublisher.java`。
- 改 `etcd/NodeInfoMirror.java`(只剩 NodeInfo 解码)、`etcd/GateWatcher.java`(去掉全服 gate / scene,加按 zone 的两份镜像)、`service/ZoneHealthProbeService.java`(改成读汇总;静态规则保留并供发布者调用)、`src/main/resources/application.yaml`、`src/test/resources/application-test.yaml`。
- 用例:新增 `LeasedEtcdWriterTest`、`ZoneHealthWatcherTest`、`ZoneHealthPublisherTest`;改 `GateWatcherTest`、`ZoneHealthProbeServiceTest`;`NodeInfoMirrorTest` 不改(经 `NodeInfoMirror` 覆盖抽出的引擎)。
- 部署:`tools/scripts/k8s_deploy.ps1`(ConfigMap 两行与两处连接总账注释)、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(新用例)。

验证:

1. `cd java && mvn -pl gateway_node -am test -Dtest=NodeInfoMirrorTest,GateWatcherTest,LeasedEtcdWriterTest,ZoneHealthWatcherTest,ZoneHealthPublisherTest,ZoneHealthProbeServiceTest,ServerListServiceTest,ServerListServiceZoneRowsCacheTest -Dsurefire.failIfNoSpecifiedTests=false`,再全量 `mvn -pl gateway_node -am test`。依赖的 `authority-jdbc` 快照照平常做法先装好。
2. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:新用例「gateway ConfigMap 写 gateway.home-zone-ids 与 zone-probe.interval-ms」通过,其余用例与改动前同样结果。
3. 本机冒烟(可选):起 etcd、MySQL、zone 1 的 gate 与 scene、网关。`etcdctl get --prefix /mmorpg/zone_health/` 应看到 zone 1、2、3 各一条(2、3 为 DOWN);`GET /api/server-list` 里 zone 1 有负载档。停掉 zone 1 的全部 gate,15 秒内 zone 1 的记录变 DOWN、列表显示维护。停掉网关,`etcdctl` 里三条记录立即消失。

## 10. Java 版(独立仓)

不适用,不登记 `PARITY.md`。Java 版的 `game-gateway` 是全服共用的一组副本,不按 zone 部署;`ZoneHealthProbe` 按区读节点目录,读量随 zone 数线性增长,没有「每个 zone 的网关都读全服」的平方项。
