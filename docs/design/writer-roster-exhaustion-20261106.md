# 写者名册会用完、死写者会堆积(缺口清单第 22 条,2026-11-06)

状态:**分析与方案已写,未落码,等负责人拍板。** 推荐方案(§3.B)要改写者闸门的一条既有不变量,改动跨 `go/shared/authoritysql`、`writer_authority` 注册器、proto、部署脚本与手册,预计 30 个文件以上,按 AGENTS §10.2 先报告再动手。
上游:[服务器代码缺口清单](../handoff/server-gap-audit-20261101.md) 第 22 条;相关:[MySQL 写入围栏](db-write-fencing-mysql.md)「2026-10-26:单次 writer 身份与永久排空收据」、[逻辑库自治与水平扩展](horizontal-scale-logical-db.md)「写资格什么时候消费」、[开服手册](../ops/k8s-open-server-runbook.md) §1、§9。

## 1. 现状

- 每个写者进程(db、data-service、login、gateway 与 infra namespace 的全局服务)启动时,init 容器 `writer_authority register` 从名册(`rosterFile`)里取一条**预先批准**的 entry 注册:writer key、instance、独立 SQL 账号、每个逻辑库一份 REGISTER 全文。名册按原字节 SHA 批准,注册器不现场生成身份或账号。
- 一条 entry 只能用一次。不变量写在 `db-write-fencing-mysql.md` 2026-10-26 节:「主体和 instance 永久唯一,重启不能复用旧身份」;退役是永久 REVOKED。
- 为什么一次性(`horizontal-scale-logical-db.md`「写资格什么时候消费」):进程写过之后崩了,内存里的 boot 状态没了,排空证明接不上。所以写过之后崩溃的容器读到消费标记以退出码 78 拒启,`sql-writer-pod-reaper` 删 Pod,新 Pod 领下一条。还没写过就退出的进程原地重启,不烧名册。
- 预算:`k8s_deploy.ps1` 要求条数 ≥ 副本数 × 6、≤ 1024(注册器 `rosterEntryLimit`)。用完时新 Pod 停在 Init,告警 `SqlWriterRegistrarFailing`。换名册 = 新身份 + 新账号 + 授权 + 改目录的 `rosterFile` / `rosterSha256` + 滚动发布(滚动本身每个副本再烧一条)。

## 2. 问题

1. **名册线性消耗,有硬上限。** 每次 Pod 启动(发布、写后崩溃、节点驱逐、扩容)烧一条。上限 1024 ÷ 6 ≈ 170 个副本;两次换名册之间能发布的次数也有限。按负载扩缩容(HPA)会直接把名册烧完,少掉的副本要等人换名册才补得回来。这是写者服务水平扩展的一道上限。
2. **死写者堆积。** 被删 Pod 的注册在各逻辑库里仍是 ACTIVE,下一次排空之前要逐个按非 run 的 RETIRE 退休(手册 §9)。ACTIVE 注册数随**启动次数**增长,不随副本数。
3. **账号线性增长。** 每条 entry 一个 SQL 账号,每次换名册都要建账号、授权,迁移搬库(`tools/dbmove`)时目标集群也要预建全部账号。

## 3. 方案

### A. 运维兜底(不改协议)

- 注册器选中 entry 时算剩余条数,写进运行目录;进程导出 gauge;告警「剩余 < 副本数 × 2」;换名册脚本化。
- 能提前报警,不解决上限,也不解决死写者堆积与账号增长。

### B. 槽位身份 + 每次启动换代(推荐)

业界的标准做法:Kafka 事务生产者是固定的 `transactional.id` 加每次初始化递增的 producer epoch,旧 epoch 的生产者被 broker 拒;etcd / ZooKeeper 的租约用 fencing token。身份长期不变,每次启动换一代,旧代进程(僵尸)下一次写就被拒。

- **批准的对象从「每次启动的身份」改成「槽位」**。名册每个槽位一条模板:固定的 SQL 账号、要写的逻辑库、权限档位;条数 = 这个服务的副本上限(含滚动余量),不再随启动次数增长。账号数 = 槽位数。
- **每次启动在槽位上换代**。注册器在一个事务里(每个逻辑库各一个):锁这个槽位当前 boot 的 64 个 `writer_slot` 行与 `route_meta`(冻结中就不写,与现在相同),把上一代 boot 原子退休(非 run 的 RETIRE,收据照写),再注册新一代:writer key = 由槽位与严格递增的代次派生、instance 随机生成、REGISTER 由模板加这两项现场拼出并记全文收据。
  - 闸门不用改:它每笔事务本来就读 `writer_registry` 并比 writer key、instance、注册摘要。上一代的 key 已经 REVOKED,僵尸下一笔事务就被拒(fail-closed)。在途事务持着 slot 行锁,换代要等它们提交完才拿得到锁,所以「旧代最后一笔」与「新代第一笔」有明确先后。
  - 「writer key 与 instance 永久唯一、REVOKED 永久」两条照旧成立(每代一个新 key);改的只有「每次启动一个新 SQL 账号」这一条。
- **死写者自动收尾**。上一代由下一代在换代事务里退休,ACTIVE 集合恒等于「每个在用槽位一个当前代」,不再需要人工逐个退休。排空证明按 key 记账的规则不变(每个 key 恰一次 RETIRE),只是 RETIRE 多了一个来源:同槽位的下一代。这条要在 2026-10-26 节的收据规则里写明。
- **槽位怎么分给 Pod**,两种:
  1. 写者服务改成 StatefulSet,槽位 = 序号(Pod 名后缀)。同一序号同一时刻只有一个 Pod;节点失联后强删重建时,新 Pod 换代即围栏掉旧的。代价:这几个服务从 Deployment 换成 StatefulSet,滚动不能 `maxSurge`,部署脚本、回收 CronJob、契约测试跟着改。
  2. 保持 Deployment,注册器在库里按租约领一个空闲槽位(租约由进程续期)。不换工作负载类型,但多一套租约与续期写入。
- **代价**:放宽「主体(SQL 账号)每次启动唯一」。以前写者退休后可以 DROP USER 作数据库层面的兜底,现在同一槽位的上下两代共用账号,围栏完全靠闸门(Go 与 Java 网关的闸门都在客户端)。项目所有写者都经闸门,没有绕过闸门的写路径;要保留数据库层兜底,可以给每个槽位两个账号轮换(换代时切到另一个、旧的撤权),账号数是槽位数的两倍。

### C. 集群内现场签发身份(否决)

进程启动时由集群内组件现建账号、现生成 REGISTER:集群里要常驻 CREATE USER / GRANT 权限,违背「运行期不签发身份」的前提,一旦这个组件失守就能凭空造写者。

## 4. 建议与待拍板

- 推荐 B,A 先做当作 B 落地前的告警。
- 落地顺序:先改 `db-write-fencing-mysql.md` 2026-10-26 节与 `horizontal-scale-logical-db.md`「写资格」两节的不变量与收据规则,再分批落码:①proto(名册模板、代次)与 `Catalog` 的换代注册;②注册器;③排空收据接受「同槽位下一代」的 RETIRE;④部署脚本、名册校验(× 6 规则改成「槽位数 ≥ 副本上限 + 滚动余量」)、回收 CronJob、手册;⑤Java 网关核对(它只消费 grant;grant 格式不变就不用改,变了按 Java 惯用方式同批改)。项目未上线,存量名册与注册整批重建。
- **需要负责人确认两件事**:
  1. 同意把「每次启动一个新 SQL 账号」放宽为「槽位账号长期不变、每次启动换代(writer key 与 instance 仍每代唯一)」,或者要每槽位两个账号轮换保留数据库层兜底。
  2. 槽位按 StatefulSet 序号分(§3.B 第 1 种),还是保持 Deployment、按租约领(第 2 种)。
