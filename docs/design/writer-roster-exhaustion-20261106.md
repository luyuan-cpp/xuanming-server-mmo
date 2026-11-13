# 写者名册会用完、死写者会堆积(缺口清单第 22 条,2026-11-06)

状态:**2026-11-10 定案(§4),11-10~11 按 §5 落码完毕,未生成、未编译、未测试,待 Codex 验证(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」)。11-11 残余回收定案并落码(§5.8:slot 行只属于在役的一代,一代结束时同一事务删掉;登记行与收据永久保留),同样未编译,验证见 `docs/PROGRESS.md` 2026-11-11「写者名册残余:一代结束即删 slot 行」。11-11~12 缩容收尾定案并落码(§5.9:每个写者 StatefulSet 一份 CronJob,按目录与集群的现状结束被缩掉槽位的末代,不再要人工退休),同样未编译,验证见 `docs/PROGRESS.md` 2026-11-12「写者缩容收尾:被缩掉槽位的末代自动结束」。11-12 复查补「归属」(§5.9:REGISTER 记下登记它的工作负载身份,收尾只结束本集群登记的一代,名册被带到别的集群时不误伤),改了协议,未生成、未编译,验证见 `docs/PROGRESS.md` 2026-11-12「写者缩容收尾补归属」。** 负责人要求「待拍板项按有依据的标准做法直接定案」,两个待定项按 §4 定了。改动跨 `go/shared/authoritysql`、`writer_authority` 注册器、proto、排空收据、部署脚本与手册,超过 30 个文件,按 §4 分 4 批落码,每批不超过 30 个文件。
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

## 4. 定案(2026-11-10)与分批

### 4.1 两个待定项

1. **账号按槽位,不按启动。** 采用 §3.B:槽位的 SQL 账号长期不变,每次启动换一代 writer key 与 instance(两者仍每代唯一,REVOKED 仍永久)。不做「每槽位两个账号轮换」:所有写路径都经闸门(Go 的 `authoritysql`、Java 网关的 `WriterAuthority`),旧代下一笔事务就被注册表拒掉;轮换账号只多挡「绕过闸门的写」,而这种写在本项目不存在,多出一倍账号与授权换来的是假想场景。依据:Kafka 事务生产者(固定 `transactional.id` + producer epoch)、etcd / ZooKeeper 租约的 fencing token 都是「身份长期不变、按代围栏」。
2. **槽位 = StatefulSet 序号。** 写者服务从 Deployment 换成 StatefulSet,Pod 名后缀就是槽位,K8s 保证同一序号同一时刻只有一个 Pod;节点失联后强删重建时,新 Pod 换代即围栏掉旧的。不选「Deployment + 租约领槽」:要多一套续期写入,续期断了还会把活着的进程误判成死的、互相围栏。代价:StatefulSet 滚动一次换一个副本、不能 `maxSurge`,发布慢一些;写者服务都无本地状态,PVC 不用加。

### 4.2 不变量改写(落码前先改规格)

- `db-write-fencing-mysql.md` 2026-10-26 节:「主体和 instance 永久唯一」改为「writer key 与 instance 每代唯一、REVOKED 永久;SQL 主体按槽位,同槽位上下两代共用」。
- `horizontal-scale-logical-db.md` §2「写资格什么时候消费」:一次性的是**一代**,不是一个账号;写后崩溃的 Pod 被回收、同序号重建后换代,名册不再消耗。
- 排空收据:RETIRE 多一个来源——同槽位的下一代在换代事务里退休上一代(非 run 的 RETIRE,收据照写全文)。「每个 key 恰一次 RETIRE」「ACTIVE 等于初始集合减合法退役」两条规则不变;排空期间(`freeze_run_id` 非空)不换代,与现在「冻结时注册器什么也不写」相同。

### 4.3 分批(每批不超过 30 个文件,批内一起编译验证)

| 批 | 内容 | 主要文件 |
|---|---|---|
| 1 协议与目录 | 名册改成槽位模板(账号、逻辑库、权限档位、锁原语资格);新表或新列记每个槽位的当前代次与当前 writer key;`Catalog.Supersede`:每个逻辑库一个事务,锁上一代的 64 个 slot 与 meta,核未冻结,非 run RETIRE 上一代、注册新一代、代次 +1,幂等(同一 Pod UID 重放回原收据);真库用例(MySQL 与 TiDB) | `proto/common/database/*.proto`、`go/shared/authoritysql/catalog.go` 与测试 |
| 2 注册器 | `writer_authority register` 从 `POD_NAME` 取序号选槽位模板,现场生成 instance,按模板拼 REGISTER 并调 `Supersede`;消费标记语义不变(写后崩溃仍以 78 退出、Pod 被回收、同序号重建) | `go/data_service/cmd/writer_authority/*`、`tools/scripts/lib/sql_boot_catalog_*.ps1` |
| 3 排空收据 | 排空 run 的 ACTIVE 记账接受「同槽位下一代」的 RETIRE;用例覆盖「换代发生在 run 之前 / 之后」「run 期间拒绝换代」 | `go/shared/authoritysql/maintenance.go` 与测试 |
| 4 部署与手册 | 写者服务换 StatefulSet;名册校验「槽位数 ≥ 副本上限」取代「条数 ≥ 副本数 × 6」;回收 CronJob 不变(删 78 退出的 Pod,StatefulSet 按同序号重建);开服手册 §1、§9、`deploy/k8s/README.md`;契约测试 | `tools/scripts/k8s_deploy.ps1`、`deploy/k8s/manifests/go-svc/*`、`tools/scripts/tests/*` |

- 批 1 到批 3 在 Go 内部,先落;批 4 动部署,和正在改 `k8s_deploy.ps1` 的会话先分工再动。
- Java 网关(`java/gateway_node` 的 `WriterAuthority`)只消费 grant:grant 格式不变就不用改,变了在批 2 同批改。Java 版仓库(`D:\luyuan\xuanming-server-mmo`)只在 `PARITY.md` 登记待做。
- 项目未上线:存量名册、注册与账号整批重建,不做迁移。

## 5. 落码细节(2026-11-10)

### 5.1 数据模型

- `writer_registry` 加 `boot_seq`(pb:11):这个 SQL 账号上第几次启动,从 1 起,严格递增,允许有空洞。账号就是槽位:同一个账号在一个逻辑库里同一时刻至多一行 ACTIVE,而且就是 `boot_seq` 最大的那一行。唯一键 `uk_writer_principal(sql_principal)` 换成 `uk_writer_principal_boot(sql_principal, boot_seq)`,挡住同一账号同一序号注册两次;`writer_key`、`instance_id` 仍然永久唯一。
  - 不另建槽位表:槽位和账号一一对应,账号已经在注册表里,再存一份槽位名就是第二份真相。「这个槽位现在是哪一代」= 按账号取 `boot_seq` 最大的一行,走唯一键前缀。
- `sql_writer_lifecycle_receipt.Request` 加 `successor_request_id`(pb:13):换代退休时填退休它的那份 REGISTER 的 `request_id`,人工退休和排空退休为空。看收据全文就知道这次 RETIRE 是谁做的。
- `sql_writer_lifecycle_receipt.Request` 加 `registered_by`(pb:14,2026-11-12):登记这一代的工作负载身份,只在 REGISTER 里有意义,算进注册摘要。不进登记表,要看时按登记行的摘要读回 REGISTER 收据(`Catalog.Registration`)。用途见 §5.9「归属」。
- 名册格式改为 `SQL_WRITER_BOOT_ROSTER_V2`,沿用 `sql_writer_boot_roster` 消息:一条 entry 是一个槽位,下标就是 StatefulSet 序号。entry 里每个逻辑库一份 REGISTER 模板,固定账号、逻辑库、锁原语与资格、`generation`、维护 run;身份字段(`request_id`、`writer_key`、`instance_id`、`registration_digest`、`registered_ms`、`boot_seq`、`deployment_id`)必须为空,由注册器现场生成。条数等于这个服务副本数的上限,不随启动次数增长。V1 名册整份拒绝:项目未上线,不做兼容。

### 5.2 注册事务(`Catalog.Register`)

一个逻辑库一个事务,提交即生效,这就是提交点:

1. 事务外先查同 `request_id` 的收据,在就原样返回(幂等重放);再预读这个账号的最新一行,知道要锁谁。
2. 锁序与 `Retire`、冻结相同:上一代的 64 个 slot → `route_meta` → 这个账号的注册行。没有 ACTIVE 的上一代(第一次启动,或上一代已被人工、排空退休)就只锁 meta。
3. 拿到锁后再查一次同 `request_id` 的收据:并发重放、上一次提交结局未知,都在这里收口。
4. 复核:世代、`writable`(冻结回 `ErrWriterFrozen`,确定没写)、维护 run 的规则与原来相同;请求的 `boot_seq` 必须大于这个账号已有的全部 `boot_seq`(CAS,挡住陈旧请求,也挡住同序号另一个 Pod 抢先注册之后的重放);ACTIVE 至多一行,有的话必须就是锁住的那一代;有 ACTIVE 的上一代时维护 run 必须没开(排空期间不换代)。
5. 写:新注册行、64 个 slot(当前 epoch)、REGISTER 收据(版本 r+1);有上一代就结束它(2026-11-11 起删掉它的 64 个 slot 行、注册行置 REVOKED,见 §5.8),追加 RETIRE 收据(版本 r+2,`expected_epoch` 为当前 epoch,`successor_request_id` 指向这次 REGISTER,`deployment_id` 是新 Pod 的);`route_meta.catalog_revision` 推到最后一份的版本。
6. 旧代怎么被挡住:旧进程的在途事务持着自己的 slot 行锁,第 2 步要等它们提交完;提交之后,旧进程下一笔事务在 `enter` 找不到自己的 slot 行(11-11 之前是读到 REVOKED),回 `ErrWriterDenied`,写不进去。纯读不受影响(读闸门只核 `route_meta`),读不会改数据。

RETIRE 排在 REGISTER 之后,是为了让带 `expected_catalog_revision` 的 REGISTER 仍满足「收据版本 = 期望版本 + 1」。两份在同一事务里,外面看不到先后。

缩容留下的末代由收尾 CronJob 自动退休(§5.9),它拼的 RETIRE 与人工退休同一形状。人工退休(收尾一直失败时的兜底)和排空 run 的退休照旧由运维准备 RETIRE 原件,`writer` 要原样复制这一代 REGISTER 收据里的 `writer`,包括 `boot_seq`。`boot_seq` 为 0 的生命周期请求,闸门(`validateLifecycle`)和注册器(`validateRequestIdentity`)都在发 SQL 之前拒绝;退休的 UPDATE 按 `writer_key`、`instance_id`、`sql_principal`、`registration_digest`、`boot_seq` 一起匹配那一行。`successor_request_id` 只有换代事务能填,手工 RETIRE 带它也在发 SQL 之前被拒。

### 5.3 注册器

- 新参数 `-pod-name`(K8s 注入 `metadata.name`):序号取最后一个 `-` 之后的十进制数,选 `entries[序号]`;序号超出名册就拒绝,说明副本数比槽位多。
- 启动记录 `<boot-output>.register.started.protojson`(本 Pod 私有 emptyDir,独占创建):第一次运行时现场生成 `writer_key`、`instance_id`、每个库的 `request_id`、`registered_ms`,`deployment_id` 是 Pod UID;`boot_seq` 取各库「这个账号最大序号 + 1」里最大的那个,各库用同一个号。顺序是先逐库预检,再写启动记录,再逐库注册。同一个 Pod 的 init 重跑只读启动记录、重发同样的请求,不再生成新身份;启动记录里除身份以外的字段必须和名册模板逐字相同。
- 原来「结局未知就只能查原请求,查不到就停」,现在可以重发:注册事务在锁内先查收据,`boot_seq` CAS 挡住陈旧请求,同一请求最多落地一次。
- 登记身份(2026-11-12):现场生成一代时,注册器把自己的工作负载身份写进各库 REGISTER 的 `registered_by`(K8s 里是 `k8s-sa:<namespace>/<ServiceAccount>/<UID>`,集群外为空),缩容收尾按它判这一代归不归自己,换代不看它。细节与理由见 §5.9「归属」。
- 输出(grant 集合、选中配置、环境文件)不变。`RegistrationConsumedByOtherDeployment` 与「这条被别人用了就换下一条」的逻辑删除;单 Request 的注册入口(`-command register -request`)删除,注册只走槽位名册;`retire`、`status` 不变。

### 5.4 权限

注册账号在 `writer_registry` 上加 `UPDATE`:换代要在同一事务里退休上一代。注册账号本来就能插入新的 ACTIVE 写者,多出来的 `UPDATE` 只能退休(写者只会变少)或改状态,不比插入更危险。常驻账号的权限不变。

2026-11-11 回收(§5.8)之后:一代结束时删掉它的 slot 行,所以注册账号对 `writer_slot` 是 `SELECT,INSERT,DELETE`(不再改写 slot 行,不给 `UPDATE`;MySQL 上 `FOR UPDATE` 锁读由 `DELETE` 权限满足),维护账号对 `writer_slot` 是 `SELECT,UPDATE,DELETE`(人工与排空退休要删,冻结、解冻推进 epoch 要改)。授权模板 `deploy/sql/authority_operator_role.sql`。

缩容收尾(§5.9)用注册账号做非 run 的退休,不加任何授权:结束一代要的 slot 表 `DELETE`、登记表 `UPDATE`、收据 `INSERT`、`route_meta` 的 `UPDATE`,注册账号为换代本来就有。不用维护账号:它能冻结闸门、推进排空,放进集群常驻的 Job 是扩大权限。

### 5.5 部署

- 写者服务(db、data-service、login、gateway 和 infra namespace 的全局写者,含 `<svc>-canary`)从 Deployment 改成 StatefulSet:`serviceName` 用同名 Service;`podManagementPolicy: Parallel`,扩缩容和首次部署不按序号排队;`updateStrategy: RollingUpdate`,一次换一个副本,不能 `maxSurge`。Pod 模板注入 `POD_NAME`,registrar init 加 `-pod-name`。
- 名册校验改成「格式 V2、槽位数 ≥ 副本数、≤ 1024」,不再要求副本数 × 6。
- 首次切换要先删同名的旧 Deployment:同一个 label 选择器不能同时归两个控制器。`k8s_deploy.ps1` 在 apply 写者之前删(`Remove-LegacySqlWriterDeployment`;gateway 排在建表 Job 之后),所以首次切换时这个服务有一段没有副本,直到 StatefulSet 的 Pod 就绪;项目未上线,不做无中断迁移。
- 回收 CronJob 不变:删掉主容器以 78 退出的 Pod,StatefulSet 按同序号重建,新 Pod 换代。
- 每个写者 StatefulSet 另有一份缩容收尾 CronJob `sql-writer-slot-retirer-<StatefulSet>`(§5.9),和写者清单一起渲染、写者 apply 之后落地;金丝雀收尾删 `<svc>-canary` 时不删它。
- 节点失联:StatefulSet 不会在别的节点自动重建同序号的 Pod,要等节点对象被删或人工强删。强删是安全的:新 Pod 换代,旧代下一笔事务被拒。

### 5.6 失败与回滚

| 情形 | 结局 |
|---|---|
| 预检时冻结或资格不符 | 什么都没写,不留启动记录;init 退出,原地重试 |
| 提交时冻结 | 事务回滚,确定没写;启动记录留着,重跑时重发同一请求 |
| 部分库已注册,init 重跑 | 已注册的查到收据,没注册的接着注册,还是同一个身份 |
| 部分库已注册,Pod 没了 | 同序号新 Pod 换代,已注册的那几份由新一代在换代事务里退休,不用人工 |
| 结局未知 | 重跑时重发同一请求,锁内查收据收口,最多落地一次 |
| 同序号出现两个 Pod(强删之后旧节点其实还活着) | 后注册的那个换代,先注册的那份被退休、写不进去;旧 Pod 拿着过期的启动记录重跑时被 CAS 拒绝,停在 Init,告警 `SqlWriterRegistrarFailing`,删掉它即可 |
| 两个工作负载误用同一份名册 | 同一账号会被两边轮流换代,谁都写不稳。部署脚本在 apply 之前核全部写者名册的账号互不重复 |
| 名册连同注册账号被带到另一个集群(备份恢复、备用集群、两个环境共用目录) | 部署脚本核不到另一个集群。那边起了写者 Pod,就按上一行轮流换代(换代凭持有槽位账号,不看是谁登记的);那边只跑着缩容收尾的 CronJob、写者缩到 0,收尾也不会结束这边的一代:它只结束本 namespace 登记的(§5.9「归属」),对别处登记的记 `registered-elsewhere`、Job 失败、告警 |
| 缩容 | 被缩掉的序号没有下一代来换代;缩容的那次部署当场按非 run 的 RETIRE 结束它们的末代,等到 `pending=0` 才往下走,部署结束就可以排空;没做完只警告,收尾 CronJob(每 5 分钟)兜底(§5.9)。再扩回来的序号只注册新一代 |

代码回滚:项目未上线,名册、注册与账号整批重建,不做 V1 兼容。

### 5.7 测试

- 单测:`validateLifecycle` 对 `boot_seq`、`successor_request_id` 的规则;注册器的序号解析、启动记录的生成与重放、模板比对;同一槽位连续换代 1100 次(超过旧上限 1024)不需要新的名册条目。
- 真库(`MMORPG_SQL_FIXTURE_ADMIN_DSN`,MySQL 与 TiDB):首次注册;同账号换代(上一代 REVOKED,两份收据的版本与 `successor_request_id` 正确,旧代事务被拒);同序号重复与陈旧请求被 CAS 拒绝;冻结时换代确定没写;重放同一请求只落地一次;排空 run 开着时有上一代就拒绝换代,run 之前换代的新一代计入 P0,SQL_RELEASED 之后上一代已退休只注册。共享夹具解冻之后用同一账号重新注册(原来会撞唯一键,真库用例一直没跑过),改成换代。
- 回收(§5.8,2026-11-11)的用例见 §5.8 末尾;缩容收尾(§5.9)的用例见 §5.9 末尾。

### 5.8 回收(2026-11-11 定案)与残余

11-10 落码时,每次启动在每个逻辑库留下 1 行 REVOKED 登记行、64 行 REVOKED slot 行和 2 份收据(REGISTER、换代 RETIRE)。原来 1024 条的名册上限顺带限住了行数,换代之后不再有上限,随启动次数无界增长。其中 slot 行占 64/67,而且 `writer_slot` 是每笔写事务都要锁的表。11-11 按下面定案落码,本节原来「先不做瘦身」的结论作废。

**定案:slot 行只属于在役的一代;登记行与收据永久保留。**

| 行 | 保留几代 | 谁删、什么时候删 |
|---|---|---|
| `writer_slot`(每代 64 行) | 只留在役的这一代;结束的一代 0 行 | 结束这一代的事务自己删:换代(`Register` 里的 `retireSuperseded`)、人工退休与排空退休(`Retire`)都走 `endGeneration`,删 64 行、登记行置 REVOKED、写 RETIRE 收据在同一事务 |
| `writer_registry`(每代 1 行) | 全部代 | 不删,结束时置 REVOKED |
| `writer_lifecycle_receipt`(每次换代 2 份) | 全部 | 不删 |

不变量:`writer_slot` 的行数恒等于「ACTIVE 写者数 × 64」,不随启动次数增长;`writer_slot.state` 从此只会是 1(列与检查约束保留,闸门照旧核 `state=1`,不改表结构)。

**什么时候可以删(逐条对照删除条件)**

- **上一代已被继任或已退休**:删除与「登记行置 REVOKED、追加 RETIRE 收据」在同一事务里。提交之前,外面看不到「slot 没了但登记还 ACTIVE」或「已 REVOKED 但 slot 还在」的中间态。
- **没有在途事务**:删之前,这个事务已按 slot 号顺序锁齐这一代的 64 行,并核过它们在当前 epoch 上 ACTIVE(`lockSlotRows` + `activeSlotsAt`)。每笔写事务从进闸门到提交一直持着自己轮到的那一行,所以锁齐就说明这一代在途的写都已提交或回滚;已提交的写照常保留,不丢。删除语句带 `state=1` 与 epoch 条件,必须正好删 64 行,否则整个事务回滚。
- **删了之后仍挡得住僵尸**:这一代再开写事务,闸门找不到自己的 slot 行。Go 闸门 `LOCK_TOUCH` 的条件写影响 0 行、随后的读读到 EOF,`SELECT_ONLY` 的锁读读到 EOF,都回 `ErrWriterDenied`;Java 网关的 `WriterAuthority.begin` 条件锁未命中或读不到行即抛错。这和读到 REVOKED 行是同一结局。TiDB 上若旧事务在换代提交之前已拿到快照,读到旧版本的 ACTIVE 行,会按冻结回可重试,重试即被拒——与置 REVOKED 时完全相同。新连接核注册(`verifyBootRegistration`)读的是登记行,登记行不删,照旧拒绝。删掉的 slot 行也不会被重建:同一个 writer key 的登记行还在,主键挡住再注册。
- **收据链**:排空 run 只读 P0 之后的生命周期收据(`plannedActive` / `foldRetirements`)与 ACTIVE 登记行(`active`);冻结只锁预期 ACTIVE 全集的 slot;发布就绪只核新一代自己的 slot。没有路径读已结束一代的 slot 行。收据一份不删,链不受影响。run 开着时不换代(§5.2),排空退休删的是被退休者自己的 64 行。
- **审计**:slot 行只有锁、epoch 与状态,不是证据。每一代的全文证明在 REGISTER / RETIRE 收据里,登记行(REVOKED)记着退休它的请求与时间。删 slot 行不丢任何审计信息。

**由谁删:结束这一代的事务自己删,不另做清理任务。** 删除需要的条件(锁齐 64 行、核 epoch 与 ACTIVE、与 REVOKE 原子)正是结束这一代的事务已经拿到并核过的。独立任务要再实现一遍锁序(slot → meta)和「现在删安全吗」的判断,另加调度、权限与监控,两次清理之间仍有 REVOKED slot 行堆着。三条结束路径共用 `endGeneration` 一段写,规则只在一处。

**为什么登记行与收据不删(不做「保留 N 代」)**

- 登记行是唯一约束的墓碑:`PRIMARY KEY(writer_key)`、`UNIQUE(instance_id)`、`UNIQUE(sql_principal, boot_seq)` 由库保证「writer key 与 instance 每代唯一、REVOKED 永久」([MySQL 写入围栏](db-write-fencing-mysql.md) 2026-10-26 节)。删了就只剩靠随机数不撞,被删 slot 行的 writer key 理论上还能再注册一次。每个账号最新的那一行是 `boot_seq` CAS 的高水位,无论如何不能删。
- 收据是规格里的永久证明(排空收据链、重放幂等、审计)。删收据是数据保留策略,不是清理,要负责人另定。
- 量小,不在热路径:每次启动每个库多 1 行登记(约 200 字节)加 2 份收据(各约 0.5–1 KB)。闸门按 `writer_key` 主键点读登记行;ACTIVE 全集走 `idx_writer_state` 的 `state=1` 段;槽位当前代走 `uk_writer_principal_boot` 前缀倒序取 1 行;收据按主键或 `(kind, request_digest)`、`(catalog_revision, request_id)` 索引读。只有 `tools/dbmove` 搬库时的全表校验和会读全表。粗估:一个库 64 个槽位、每个每天重启 10 次(已经很极端),一年约 23 万代,登记行约 50 MB、收据约 300 MB;正常发布频率比这小两个数量级。
- 定案(2026-11-11,负责人要求不等数据、按标准做法定):登记行与收据永久保留,不做「保留 N 代」的瘦身。登记行是唯一约束的墓碑,收据是排空链与审计的证明;两者都是低量的审计数据(每次启动每库约 1.2–2.2 KB),标准做法就是永久保留。真要删,只能作为负责人另定的数据保留策略:先导出归档,再删「早于最近一次排空 run 的 P0、不属于 ACTIVE 写者、也不是各账号最新一代」的收据与登记行,并同时改 2026-10-26 节的不变量措辞。这不是挂起的缺陷。

**落码(未编译)**

- `go/shared/authoritysql/catalog.go`:新增 `endGeneration`(删 64 个 slot 行 → 登记行置 REVOKED),`retireSuperseded` 与 `Retire` 改调它。`lockSlotRows` 一行都没有时回空集,只缺一部分回 `ErrWriterDenied`(判定抽成 `lockedSlots`),不再把 `sql.ErrNoRows` 交给调用方:
  - `lockSlots`(人工与排空退休、冻结与解冻、排空冻结)经 `activeSlotsAt` 回确定的 `ErrWriterDenied`;
  - `Register` 拿到空集,说明预读之后上一代已被别的事务结束(人工退休,或同序号另一个 Pod 先换了代),持 meta 锁复核槽位,结局与原来读到 REVOKED 行时相同。
- 授权模板 `deploy/sql/authority_operator_role.sql`,见 §5.4。
- 运行期闸门(Go `writer.go`、Java `java/authority_jdbc` 的 `WriterAuthority`)与表结构不改。
- 存量:项目未上线,按 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」第 0 步清库重建;库里已有的 REVOKED slot 行不迁移,没有路径读它们。

**用例(未运行)**

- 单测 `catalog_succession_test.go` `TestLockedSlotsOutcome`:空集、整代、缺一部分、计数不符;空集过不了 `activeSlotsAt`。
- 真库 `writer_succession_tidb_test.go`(MySQL 与 TiDB):
  - `TestSlotSuccessionRetiresPreviousGeneration`:换代后上一代 slot 0 行、登记行 REVOKED;旧进程连写两次都是拒绝,不是冻结;已被换代的一代再人工退休,回 `ErrWriterDenied`(不是 `sql.ErrNoRows`),不写目录。
  - `TestSlotSuccessionChainKeepsOneActiveGeneration`:连续换代 20 次后整表 slot 行 = ACTIVE × 64;登记行 21 行;REGISTER 与 RETIRE 收据各多 20 份。
  - `TestSlotSuccessionWaitsOutFreeze`:解冻后换代,上一代 slot 行按解冻后的 epoch 删掉,RETIRE 收据记这个 epoch。
  - `TestSlotSuccessionAndDrainRun`:run 期间被拒的换代不动在役一代的 64 行。
  - 新增 `TestSlotSuccessionWaitsForInFlightTransaction`:上一代开着一笔写事务时,换代在 slot 锁上等(按 TiDB `DATA_LOCK_WAITS` / MySQL `performance_schema.data_locks` 观测,不 sleep);那笔提交后换代落地,写保留,上一代 slot 0 行,下一笔写被拒。
  - 新增 `TestManualRetireEndsGenerationAndSlotRegistersAgain`:人工 RETIRE(带 `bootSeq`)删 slot 行、登记行指回这份 RETIRE;重放原样返回,不推进目录;旧进程写被拒;同一槽位下一代只注册(目录 +1),整表 64 行;已退休的一代换 request_id 再退,回 `ErrWriterDenied`。
- 真库 `writer_tidb_test.go` 的排空用例:排空退休之后这一代 slot 0 行,随后的空 ACTIVE 冻结照常推进。
- 脚本契约 `sql_boot_catalog_local.tests.ps1`:注册账号对 `writer_slot` 是 `SELECT,INSERT,DELETE`,维护账号是 `SELECT,UPDATE,DELETE`。

**其余残余**

- 缩容留下的末代:11-11~12 定案并落码,见 §5.9。
- Java 版(`D:\luyuan\xuanming-server-mmo`)只在 `PARITY.md` 登记待做(换代与 11-11 回收各一节)。本仓库的 Java 网关只消费 grant,核 REGISTER 原文用的 descriptor 在构建时由 proto 生成,不用改代码,要重新构建。回收也不用改它:它只在自己的事务里锁自己的 slot 行,行没了 `begin` 抛错、启动时 `verifySlots` 拒启,和读到 REVOKED 同一结局;注册与退休都由 Go 的 `Catalog` 做。

### 5.9 缩容收尾(2026-11-11 定案,11-12 补齐)

缩容之后,被缩掉的序号不再有 Pod 启动,它们在每个逻辑库的末代没有下一代来换代结束。留着它有两个问题:下一次排空收不了尾(没有进程替它关闭,只能人工 RETIRE);被缩掉的 Pod 若其实还活着(Pod 对象被强删、节点失联),它仍能写。负责人 11-11 要求现在做,不等「缩容变频繁」。

**定案:每个写者 StatefulSet 一份收尾 CronJob,每一轮按目录与集群的现状对账,不靠部署脚本发现「副本数变小」。**

- CronJob `sql-writer-slot-retirer-<StatefulSet>`(含 `<svc>-canary` 与 Java 的 gateway),每 5 分钟一轮,跑 `writer_authority -command retire-removed-slots`(`go/data_service/cmd/writer_authority/retire_removed_slots.go`,读集群在 `kube_workload.go`)。清单 `deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml`;`k8s_deploy.ps1` 的 `New-SqlWriterSlotRetirerManifest` 和写者清单一起在任何集群写操作之前渲染(目录条目不对就在这里拦下),写者 apply 之后紧跟着 `Apply-SqlWriterSlotRetirer`。
- 为什么不按本条原来的写法「部署脚本发现副本数变小、等 Pod 删完再跑一次 Job」:那是边沿触发。脚本在缩容与建 Job 之间断了、Job 失败、或者有人不经脚本 `kubectl scale`,末代都会漏掉,还得另做一份「待办」标记让下次部署重跑。按现状对账是 K8s 控制器的标准做法(level-triggered):每一轮都从目录和集群重新判,失败的下一轮自己补上,不在任何地方记状态。代价是每 5 分钟一次只读扫描:副本数以内的槽位不读;被缩掉的槽位每个逻辑库一次按 `uk_writer_principal_boot` 前缀的点读,没有在役的被缩掉槽位时不再读集群。
- 部署脚本在写者缩容与金丝雀收尾时**当场**按同一份 CronJob 模板起一轮、等它做完(11-12 补,见下文「部署当场收尾」),部署返回时末代已经结束;定时那一份是兜底。事件触发加周期对账,与控制器「watch + resync」同一结构。

**判据与顺序(安全性就在这里)**

1. 读 StatefulSet 的 `spec.replicas`,只扫序号 ≥ 它的槽位。StatefulSet 不存在算 0 个副本:金丝雀收尾会删整个 `<svc>-canary`,这份 CronJob 留着,把金丝雀各槽位的末代结束掉。这一步只缩小扫描范围,不做判定。
2. 读这些槽位在名册里每个逻辑库的最新一代(`LatestBoot`),ACTIVE 的记作候选。这是目录快照。
3. 目录读完之后再读一次副本数,逐个查候选序号的 Pod(`<StatefulSet>-<序号>`):只结束「序号 ≥ 这次读到的副本数、这个名字的 Pod 对象已经没了」的候选。Pod 对象还在(优雅退出没走完,带 `deletionTimestamp` 也算在)就留给下一轮,不打断它最后的写。
4. 归属:Pod 已经没了的候选,读这一代的 REGISTER 原件(`Catalog.Registration`),只有它是本 namespace 的工作负载登记的才往下走(见下面「归属」)。不是就不动它,记 `registered-elsewhere`,这一轮 Job 失败。
5. 结束:每个候选一份非 run 的 RETIRE,`writer` 原样是快照里的登记行(含 `boot_seq`),按当时的 meta 绑 epoch 与目录版本,走 `Catalog.Retire` → `endGeneration`(64 个 slot 行删掉、登记行置 REVOKED、收据永久保留),和人工退休同一条写路径。

为什么不会误退在役的一代:

- 槽位 i 只会被名字是 `<StatefulSet>-i` 的 Pod 注册:注册器按自己的 Pod 名选名册下标(§5.3),名册只挂进这个工作负载的 Pod,部署脚本在 apply 之前核全部名册的账号互不重复(§5.6)。这一条只在一个集群、一份部署目录里成立;名册连同账号被带到别的集群时,由第 4 步的归属兜住。
- 第 2 步读到的一代 g,是在第 2 步之前由某个 `<StatefulSet>-i` 注册的。第 3 步这个名字的 Pod 对象已经没了,说明注册 g 的那个 Pod 已被删除:K8s 只在容器终止之后删 Pod 对象,例外只有强删,那时旧进程是僵尸,退休恰好把它围栏掉(与同序号新 Pod 换代同一结局)。
- 第 2 步之后才建出来的同名 Pod 注册的是更新的一代,不在快照里;RETIRE 按 g 的全部身份字段与 `boot_seq` 匹配那一行,新一代永远不会被它结束。新 Pod 的换代和这里的退休锁的是同一代的 slot 行:退休在先,新 Pod 看到上一代已结束,只注册;换代在先,这里的退休读不到 slot 行、被拒,重读得知已结束(`already-ended`)。
- 反过来「先看集群、后读目录」就不成立:看完集群之后才注册的新一代会进快照,被误结束。所以顺序是硬约束,单测 `TestRetireRemovedSlotsReadsCatalogBeforeCluster` 守着。

**幂等与失败**

- 不在本地记任何状态,RETIRE 的 `request_id` 每次现场随机生成:重跑时落地过的一代已经不在役、不再是候选;没落地的照常再退;目录保证一代只结束一次(`endGeneration` 要求 `state=1`、正好 64 行)。同一轮里只在确定被拒时重读、换一份新请求重试(最多 3 次,拒绝多半是读 meta 之后目录版本被同库别的注册推进了);结局未知不重发,报错留给下一轮按目录状态收口。
- 闸门冻结(搬库、排空)、排空 run 开着:这一轮跳过,不算失败。排空期间的退休归排空记账,非 run 的 RETIRE 本来就会被拒。
- 连不上库或 API server、权限不够、结局未知、试满还被拒:报错,Job 失败(`backoffLimit: 0`),下一轮按现状重来。连续 30 分钟没有一次成功由告警 `SqlWriterSlotRetirerFailing`(`deploy/k8s/logical-db-alerts.yaml`,读 kube-state-metrics 的 `kube_cronjob_status_last_successful_time`,从没成功过的按 `kube_cronjob_created` 补)发现,warning:末代留着不写坏数据,只是下一次排空之前要人工退休。

**归属:只结束本集群登记的一代(2026-11-12 复查补)**

- **洞**:前三步只证明「本集群里这个序号的 Pod 不在了」,没证明这一代是本集群的 Pod 登记的。同一份名册和注册账号 Secret 被带到第二个集群时,那边的 Pod 当然不在:集群级备份恢复把 namespace 连 CronJob、Secret 一起恢复出来;备用集群把写者缩到 0 却没停 CronJob;两个环境的部署目录误共用名册。那边的收尾每 5 分钟就会把正式集群在役的写者全部结束,写者进程不会自己重启,正式集群的写一直被拒。数据不会坏(围栏是 fail-closed),但这是一个缩到 0 的副本就能触发的全量写中断。
- **规矩**:按「不在」做删除的自动化,只能动自己建的东西。K8s 垃圾回收按属主 UID 判,云控制器按集群标签只回收自己建的负载均衡与卷,是同一条。换代不受它约束:新持有者(拿着槽位账号、活着)围栏旧的,不需要旧的归自己,这正是围栏要的「不靠旧持有者配合」。只有「推断没人了」这一步需要归属证明。
- **身份**:登记进程的 ServiceAccount 连同这个对象的 UID,写成 `k8s-sa:<namespace>/<ServiceAccount 名>/<UID>`,存在 REGISTER 的 `registered_by`(`sql_writer_lifecycle_receipt.Request` 字段 14),算进注册摘要,登记之后改不了。
  - UID 由所在集群的 API server 在建对象时分配。备份恢复、克隆 namespace 得到的 ServiceAccount 是新对象、新 UID,带不过去。带得过去的只有连 etcd 一起克隆整个控制面,那已经是同一个集群的两份,和发号器用的 kube-system UID 同一个前提。
  - 注册器在运行期问 API server 得到它(`SelfSubjectReview`,K8s 1.28 起正式),不由部署脚本渲染:渲染进清单的值会跟着备份走(发号器的部署身份同一个理由)。这个接口所有已认证身份默认可调,12 个写者不用加任何 RBAC,也不用改清单。
  - 不选 kube-system 的 UID:读它要给每个写者的 ServiceAccount 加一条集群级授权。ServiceAccount 的 UID 还多分得清一层:同一个集群里两个 namespace 误共用名册也对不上。
- **注册器**(`workload_identity.go` 的 `registrarIdentity`):现场生成一代时问一次,各库的 REGISTER 写同一个值;init 重跑读启动记录,不再问。在 K8s 里读不到就报错、不登记(停在 init,`SqlWriterRegistrarFailing` 会响):带着空身份登记的一代以后缩容不会被自动结束,宁可当场看见原因。集群外(本机脚本、调试栈)登记为空,那里没有收尾。名册模板里预先写了 `registeredBy` 的,注册器和部署脚本都拒绝。
- **收尾**(`retire_removed_slots.go` 的 `registeredHere`):`registered_by` 的 namespace 是自己的,而且本 namespace 现在这个名字的 ServiceAccount 就是那个 UID,才结束。别的 namespace、别的集群的 UID、这个 ServiceAccount 已被删掉或重建、空身份、认不出的文本,一律记 `registered-elsewhere`:不动它,算 `pending`,Job 失败,靠 `SqlWriterSlotRetirerFailing` 让人来看。读不到 REGISTER 收据或 API server 报错记 `failed`,同样不结束。只在 Pod 已经没了、真要结束时才核,Pod 还在或又扩回来的候选不读。
- **代价**:namespace 或 ServiceAccount 被删掉重建之后,重建之前登记的末代不会被自动结束,要人工 RETIRE(手册 §9);本条上线之前登记的一代没有身份,同样处理。项目未上线,存量按 PROGRESS 的前置清库重建。

**权限**:注册账号(与这个写者 init 里的 registrar 同一个 Secret),SQL 授权不变(§5.4);集群权限只有本 namespace 里这一个 StatefulSet 的 `get`、`pods` 的 `get` 与 `serviceaccounts` 的 `get`(核归属),不能列、不能改,不能签发令牌。只挂名册所在的 Secret,不挂运行配置。

**部署当场收尾(2026-11-12)**

11-11 落码时只有定时这一份,留下两条残余:缩容之后要等一轮(最多约 5 分钟)才能开排空,否则 P0 会把没有进程的末代算进 ACTIVE 全集,还得按 run 人工退休;金丝雀删掉之后它的收尾 CronJob 一直留着,删金丝雀的目录条目与 Secret 之前得先手工删它。负责人 11-12 要求现在修。

- **为什么在部署这一侧修,不在排空那一侧**:排空 run 要每个 ACTIVE 写者在 P0 里有存活进程给出的闭合原件(`maintenance_drain.ps1` 的 issuers),没有进程的末代给不出,只能由运维按 run 准备退休原件。所以正确的做法是不让它们留到排空:写者的副本数只经部署脚本改(目录 `replicas` 与清单逐字核对),缩容的那一次部署就负责把末代结束掉。
- **缩容**:`Apply-OneGoSvc`(稳定版与金丝雀)、`Apply-JavaSvcManifests`(gateway)在 apply 写者 StatefulSet 之前读线上副本数(`Get-SqlWriterLiveReplicas`,读不到就不 apply),收尾 CronJob 落地之后比较(`Complete-SqlWriterScaleDown`):新副本数小于旧的,就当场跑一轮(`Complete-SqlWriterSlotRetirement`)。扩容、不变、首次部署不做事。
- **一轮怎么跑**:`kubectl create job <CronJob 名>-d<9 位秒> --from=cronjob/<CronJob 名>`(镜像、凭据与权限都是那一份,Job 名不超过 63 个字符),等 Job 进终态(复用迁移 Job 的 `Get-GoSvcMigrateJobState`,Failed 立刻停),读日志最后一行汇总。Job 成功而且汇总行 `pending=0` 才算做完;`pending` 是被缩掉、这一轮还没能结束的在役一代(`pod-present`、`gate-frozen`、`drain-run-open`、`failed`),`retired`、`already-ended`、`scaled-back-up` 不算。没做完多半是被缩掉的 Pod 还在优雅退出,隔 10 秒再起一轮;总预算 max(`-WaitTimeoutSeconds`, 180 秒)。做完的 Job 删掉(结果已经打进部署日志),没做完的最后一个留着查日志。
- **门禁**:与迁移 Job 相同,staging/prod 恒等,dev 仅 `-WaitReady`;不等时只起一轮就返回。
- **没做完只警告、不中断部署**:部署按服务顺序 apply,中途 throw 会让后面的服务没部署上;没做完的原因(闸门冻结、排空 run 开着、收尾失败)CronJob 会接着做。警告写明排空之前要等哪个 CronJob 报 `pending=0`。
- **金丝雀收尾**:`Apply-GlobalGoSvcCanaries` 删掉没列出的 `<svc>-canary` 之后,对 SQL 写者调 `Complete-SqlWriterCanaryRemoval`:它的收尾 CronJob 还在,就当场跑一轮(`-AlwaysWait`,不看门禁,要靠结果决定删不删),做完才删这份 CronJob 与同名的 ServiceAccount、Role、RoleBinding;没做完就留着,下一次部署(金丝雀仍没列出)再来一遍;CronJob 不在(从没起过或已收完)什么都不做;存在与否读不到就不动它。之后删金丝雀的目录条目与 Secret,不会留下每轮都起不来的 CronJob。
- **手工 Job 的回收**:`kubectl create job --from=cronjob` 起的 Job 属主不带 `controller: true`,不算进 CronJob 的 `successfulJobsHistoryLimit` / `failedJobsHistoryLimit`,所以 jobTemplate 加 `ttlSecondsAfterFinished: 86400`,结束一天后自动删(两种 Job 都管)。告警读 CronJob 的 status,不受影响。
- **一个坑**:部署脚本的 `Invoke-Kubectl` 不重定向 kubectl 的标准输出,在要返回真假的函数里直接调用,输出会混进返回值、让判断永远为真(没收完也会删金丝雀的 CronJob)。这几个函数里的 kubectl 输出一律 `| Out-Host`,契约测试的桩照样往标准输出写一行来守住。

**时延与排空**:经部署缩容时,部署结束就可以开排空;部署打了「没做完」的警告,或者有人绕过部署脚本 `kubectl scale`,才要等定时那一份(Pod 优雅退出完之后最多约 5 分钟)。在末代结束之前开排空,P0 会把它们算进 ACTIVE 全集,还要按 run 人工退休(开服手册 §9「没有进程的写者」)。

**落码与用例(未编译、未运行)**

- Go:`retire_removed_slots.go`、`kube_workload.go`、`main.go` 的 `retire-removed-slots` 分支与 `-statefulset` 参数(11-11 写,已随 `4edb25640` 入库)。
- 单测 `retire_removed_slots_test.go`(内存里的目录与集群):只结束 Pod 已经没了的被缩掉槽位、先读目录后看集群、读完目录又扩回来跳过、整个 StatefulSet 删了全部结束、冻结与排空 run 跳过、别处先结束了算 `already-ended`、目录版本竞争重试后放弃、结局未知报错、没有候选什么都不做、查不到 Pod 就停、命令行参数;`kube_workload_test.go`:API server 回 200 / 404 / 其他状态码与凭据缺失。
- 真库 `retire_removed_slots_tidb_test.go`(MySQL 与 TiDB,真实 `Catalog` + 内存集群,11-12 补):`TestRetireRemovedSlotsEndsLastGenerationOnRealCatalog`(Pod 还在、扩回来都不动;Pod 没了在两个库结束,收据非 run、无继任者、登记行指回它、slot 行删掉、旧进程写被拒;重跑不写;同槽位扩回来只注册)、`TestRetireRemovedSlotsWaitsOutFrozenGateOnRealCatalog`(冻结的库跳过、解冻后下一轮按新 epoch 结束)、`TestRetireRemovedSlotsNeverEndsANewerGenerationOnRealCatalog`(读目录之后同序号新 Pod 先换代:收尾被拒记 `already-ended`,新一代照常在役)。
- 部署契约 `tools/scripts/tests/sql_writer_slot_retirer_contract.tests.ps1`(11-12 补):凭据与权限、TTL、命令行与 Go 参数一致、金丝雀条目、名字长度、渲染在写操作之前、落地在写者 apply 之后、告警与手册;部署当场收尾(11-12 第二轮):汇总行格式与 Go 单测样本一致、读线上副本数(不在回 0、读不到或不是数字拒绝)、只有缩容才收尾、Pod 还在时隔轮重试直到 `pending=0`、Job 失败或汇总行属于别的 StatefulSet 不算做完、预算用完警告、门禁、金丝雀收尾做完才删 CronJob 与 RBAC、kubectl 输出不混进返回值、调用顺序(apply 之前读副本数、收尾 CronJob 落地之后判缩容、删金丝雀 StatefulSet 之后收尾)。告警名单 `logical_db_deploy_contract.tests.ps1`。`k8s_migrate_gate.tests.ps1` 真跑 `Apply-OneGoSvc`,给收尾的四个函数补了只记事件的桩。
- Go 单测 `TestRetirementReportCountsPendingCandidates`(11-12 第二轮):汇总行的 `pending` 计哪些结局。
- 归属(11-12 复查补,要先重新生成协议):
  - 协议 `proto/common/database/rollback_database_table.proto` 的 `sql_writer_lifecycle_receipt.Request.registered_by = 14`。
  - `go/shared/authoritysql/catalog.go`:`Registration`(按登记行读回 REGISTER 原件)、`validRegisteredBy`(只限长度与字符);夹具 `sqltest/fixture.go` 加 `Options.RegisteredBy`。
  - `go/data_service/cmd/writer_authority`:新文件 `workload_identity.go`(身份的文本形状、`registrarIdentity`);`kube_workload.go` 拆出 `kubeClient`,加 `selfIdentity`(SelfSubjectReview)与 `ServiceAccountUID`;`main.go` 的 `mintBoot` 写 `registered_by`、模板与启动记录的核对;`retire_removed_slots.go` 的 `registeredHere`。
  - 清单 `sql-writer-slot-retirer.yaml` 的 Role 加 `serviceaccounts` 的 `get`;`k8s_deploy.ps1` 名册模板预填检查加 `registeredBy`、没做完的警告点名这种结局;告警说明。
  - 单测:`catalog_succession_test.go` `TestLifecycleBoundsRegisteredBy`;`main_test.go` `TestBootRecordsWhoRegisteredIt`、`TestIdentityFailureRegistersNothing`、`TestBootOutsideKubernetesHasNoIdentity`、`TestBootRecordWithMixedIdentityRefused`;`workload_identity_test.go`;`kube_workload_test.go` `TestSelfIdentityAsksTheAPIServerWhoThisPodIs` 等;`retire_removed_slots_test.go` `TestRetireRemovedSlotsLeavesGenerationsRegisteredElsewhere`(别的集群的 UID、别的 namespace、空身份、认不出、ServiceAccount 不在)、`TestRetireRemovedSlotsChecksOwnershipOnlyBeforeRetiring`、`TestRetireRemovedSlotsDoesNotRetireWhenOwnershipCannotBeRead`。
  - 真库:`writer_succession_tidb_test.go` `TestRegistrationReadsBackTheRegisterRequest`;`retire_removed_slots_tidb_test.go` 的夹具那一代改为带本 namespace 的身份,新增 `TestRetireRemovedSlotsLeavesGenerationRegisteredElsewhereOnRealCatalog`(别的集群登记的、集群外登记的:目录一个字节不写,那一代照常能写)。
  - 契约:`sql_writer_slot_retirer_contract.tests.ps1` 的 Role 断言改为三条 `get`、不含令牌与 Secret;`k8s_deploy_contract.tests.ps1` 加「模板预填 `registeredBy` 在渲染前拒绝」。
- Java 版只在 Java 仓 `PARITY.md` 登记待做。本仓库的 Java 网关不用改代码:收尾由 Go 的 `writer_authority` 做,gateway 的收尾 CronJob 用它的 registrar 镜像;`registered_by` 是 REGISTER 的新字段,`java/authority_jdbc` 核 REGISTER 原文的描述符由 proto 生成,要重新构建,否则按未知字段拒绝带身份的 REGISTER。
