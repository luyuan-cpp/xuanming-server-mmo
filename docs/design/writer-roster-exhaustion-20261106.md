# 写者名册会用完、死写者会堆积(缺口清单第 22 条,2026-11-06)

状态:**2026-11-10 定案(§4),11-10~11 按 §5 落码完毕,未生成、未编译、未测试,待 Codex 验证(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」)。** 负责人要求「待拍板项按有依据的标准做法直接定案」,两个待定项按 §4 定了。改动跨 `go/shared/authoritysql`、`writer_authority` 注册器、proto、排空收据、部署脚本与手册,超过 30 个文件,按 §4 分 4 批落码,每批不超过 30 个文件。
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
- 名册格式改为 `SQL_WRITER_BOOT_ROSTER_V2`,沿用 `sql_writer_boot_roster` 消息:一条 entry 是一个槽位,下标就是 StatefulSet 序号。entry 里每个逻辑库一份 REGISTER 模板,固定账号、逻辑库、锁原语与资格、`generation`、维护 run;身份字段(`request_id`、`writer_key`、`instance_id`、`registration_digest`、`registered_ms`、`boot_seq`、`deployment_id`)必须为空,由注册器现场生成。条数等于这个服务副本数的上限,不随启动次数增长。V1 名册整份拒绝:项目未上线,不做兼容。

### 5.2 注册事务(`Catalog.Register`)

一个逻辑库一个事务,提交即生效,这就是提交点:

1. 事务外先查同 `request_id` 的收据,在就原样返回(幂等重放);再预读这个账号的最新一行,知道要锁谁。
2. 锁序与 `Retire`、冻结相同:上一代的 64 个 slot → `route_meta` → 这个账号的注册行。没有 ACTIVE 的上一代(第一次启动,或上一代已被人工、排空退休)就只锁 meta。
3. 拿到锁后再查一次同 `request_id` 的收据:并发重放、上一次提交结局未知,都在这里收口。
4. 复核:世代、`writable`(冻结回 `ErrWriterFrozen`,确定没写)、维护 run 的规则与原来相同;请求的 `boot_seq` 必须大于这个账号已有的全部 `boot_seq`(CAS,挡住陈旧请求,也挡住同序号另一个 Pod 抢先注册之后的重放);ACTIVE 至多一行,有的话必须就是锁住的那一代;有 ACTIVE 的上一代时维护 run 必须没开(排空期间不换代)。
5. 写:新注册行、64 个 slot(当前 epoch)、REGISTER 收据(版本 r+1);有上一代就把它的 64 个 slot 与注册行置 REVOKED,追加 RETIRE 收据(版本 r+2,`expected_epoch` 为当前 epoch,`successor_request_id` 指向这次 REGISTER,`deployment_id` 是新 Pod 的);`route_meta.catalog_revision` 推到最后一份的版本。
6. 旧代怎么被挡住:旧进程的在途事务持着自己的 slot 行锁,第 2 步要等它们提交完;提交之后,旧进程下一笔事务在 `enter` 读到自己的 slot 已 REVOKED,回 `ErrWriterDenied`,写不进去。纯读不受影响(读闸门只核 `route_meta`),读不会改数据。

RETIRE 排在 REGISTER 之后,是为了让带 `expected_catalog_revision` 的 REGISTER 仍满足「收据版本 = 期望版本 + 1」。两份在同一事务里,外面看不到先后。

人工退休(缩容后留下的末代)和排空 run 的退休照旧由运维准备 RETIRE 原件,`writer` 要原样复制这一代 REGISTER 收据里的 `writer`,包括 `boot_seq`。`boot_seq` 为 0 的生命周期请求,闸门(`validateLifecycle`)和注册器(`validateRequestIdentity`)都在发 SQL 之前拒绝;退休的 UPDATE 按 `writer_key`、`instance_id`、`sql_principal`、`registration_digest`、`boot_seq` 一起匹配那一行。`successor_request_id` 只有换代事务能填,手工 RETIRE 带它也在发 SQL 之前被拒。

### 5.3 注册器

- 新参数 `-pod-name`(K8s 注入 `metadata.name`):序号取最后一个 `-` 之后的十进制数,选 `entries[序号]`;序号超出名册就拒绝,说明副本数比槽位多。
- 启动记录 `<boot-output>.register.started.protojson`(本 Pod 私有 emptyDir,独占创建):第一次运行时现场生成 `writer_key`、`instance_id`、每个库的 `request_id`、`registered_ms`,`deployment_id` 是 Pod UID;`boot_seq` 取各库「这个账号最大序号 + 1」里最大的那个,各库用同一个号。顺序是先逐库预检,再写启动记录,再逐库注册。同一个 Pod 的 init 重跑只读启动记录、重发同样的请求,不再生成新身份;启动记录里除身份以外的字段必须和名册模板逐字相同。
- 原来「结局未知就只能查原请求,查不到就停」,现在可以重发:注册事务在锁内先查收据,`boot_seq` CAS 挡住陈旧请求,同一请求最多落地一次。
- 输出(grant 集合、选中配置、环境文件)不变。`RegistrationConsumedByOtherDeployment` 与「这条被别人用了就换下一条」的逻辑删除;单 Request 的注册入口(`-command register -request`)删除,注册只走槽位名册;`retire`、`status` 不变。

### 5.4 权限

注册账号在 `writer_registry`、`writer_slot` 上加 `UPDATE`:换代要在同一事务里退休上一代。注册账号本来就能插入新的 ACTIVE 写者,多出来的 `UPDATE` 只能退休(写者只会变少)或改状态,不比插入更危险。常驻账号的权限不变。

### 5.5 部署

- 写者服务(db、data-service、login、gateway 和 infra namespace 的全局写者,含 `<svc>-canary`)从 Deployment 改成 StatefulSet:`serviceName` 用同名 Service;`podManagementPolicy: Parallel`,扩缩容和首次部署不按序号排队;`updateStrategy: RollingUpdate`,一次换一个副本,不能 `maxSurge`。Pod 模板注入 `POD_NAME`,registrar init 加 `-pod-name`。
- 名册校验改成「格式 V2、槽位数 ≥ 副本数、≤ 1024」,不再要求副本数 × 6。
- 首次切换要先删同名的旧 Deployment:同一个 label 选择器不能同时归两个控制器。`k8s_deploy.ps1` 在 apply 写者之前删(`Remove-LegacySqlWriterDeployment`;gateway 排在建表 Job 之后),所以首次切换时这个服务有一段没有副本,直到 StatefulSet 的 Pod 就绪;项目未上线,不做无中断迁移。
- 回收 CronJob 不变:删掉主容器以 78 退出的 Pod,StatefulSet 按同序号重建,新 Pod 换代。
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
| 缩容 | 被缩掉的序号最后一代留着 ACTIVE(没有下一代来退休它),再扩回来时被换代。下一次排空之前按非 run 的 RETIRE 退休,和原来一样,但数量不超过槽位数 |

代码回滚:项目未上线,名册、注册与账号整批重建,不做 V1 兼容。

### 5.7 测试

- 单测:`validateLifecycle` 对 `boot_seq`、`successor_request_id` 的规则;注册器的序号解析、启动记录的生成与重放、模板比对;同一槽位连续换代 1100 次(超过旧上限 1024)不需要新的名册条目。
- 真库(`MMORPG_SQL_FIXTURE_ADMIN_DSN`,MySQL 与 TiDB):首次注册;同账号换代(上一代 REVOKED,两份收据的版本与 `successor_request_id` 正确,旧代事务被拒);同序号重复与陈旧请求被 CAS 拒绝;冻结时换代确定没写;重放同一请求只落地一次;排空 run 开着时有上一代就拒绝换代,run 之前换代的新一代计入 P0,SQL_RELEASED 之后上一代已退休只注册。共享夹具解冻之后用同一账号重新注册(原来会撞唯一键,真库用例一直没跑过),改成换代。

### 5.8 不做与残余

- REVOKED 的注册行和 64 个 slot 行永久保留,每个逻辑库每次启动多 65 行,和原来一样;原来 1024 条的名册上限顺带限住了行数,现在不限。2026-11-11 复核后先不做瘦身,理由:不影响正确性;这些行只在 `tools/dbmove` 搬库比对时被整表读,其余路径都按 `writer_key` 主键查;按 AGENTS §11.3,性能改动要等有数据。要做时的做法:换代事务里把上一代的 64 个 slot 行删掉(注册账号要加 `writer_slot` 的 DELETE),注册行与 RETIRE 收据留作永久证明,writer key 也就不会被重新启用。运行期闸门查不到 slot 行时已经回 `ErrWriterDenied`(`rawOne` 读到 EOF);目录侧 `lockSlotRows` 现在原样返回「没有行」,删行之前要先改成回 `ErrWriterDenied`,并补「被换代的旧进程仍然只收到拒绝」的真库用例。
- 缩容留下的末代(§5.6 最后一行)先不自动退休:数量不超过槽位数;忘了退休时,下一次排空收不了尾、停下来(没有进程替它关闭),不会写坏数据;自动化要新加注册器命令和部署脚本里的收尾 Job。缩容变频繁时再做:部署脚本发现副本数变小、等多出来的序号的 Pod 删完之后,跑一次 Job,按名册把这些槽位当前 ACTIVE 的那一代用非 run 的 RETIRE 退休(写者已经没了,退休只会挡住它,总是安全的)。
- Java 版(`D:\luyuan\xuanming-server-mmo`)只在 `PARITY.md` 登记待做。本仓库的 Java 网关只消费 grant,核 REGISTER 原文用的 descriptor 在构建时由 proto 生成,不用改代码,要重新构建。
