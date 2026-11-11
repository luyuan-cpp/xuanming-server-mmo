# 开服线待拍板与运维前置清单(2026-10-20 汇总)

> **用途**:交接 [handoff-launch-line-remaining-20261019.md](../handoff-launch-line-remaining-20261019.md) §3 第 4 项,整理好后请负责人逐项拍板。本文只汇总、不做决定。每项的出处列在表里,原文以出处为准。
> **怎么来的**:一名只读代理逐条读完下列来源,按「同一件事只列一次」去重;再在 PROGRESS 里搜 10-17 之后的负责人决定,已决定的不列为待定。Claude 抽查了出处。来源:
> - tidb-phase1 §9.3 / §5.3 / §7;
> - shared-redis-split-by-purpose(G4a)§25.2 与原 §13 剩余条;
> - release-checklist §6.1;
> - gate-kernel-tuning-runbook §3.1;
> - 交接 §3 第 4 项;
> - D1 提案 §12.1「仍待负责人决定」;
> - 交接 §3 第 6 项「无主小项」核查。
>
> **不在本文展开、只作交叉引用的**:
> - 落点 / D1 线自己的拍板项,见 [placement-line-handoff-20261019.md](../placement-line-handoff-20261019.md) §7;
> - 容量模型 §6 的 #2、#3、#4、#9、#10、#14、#16、#17、#18;
> - R9 回档工具超出授权文件数的确认;
> - 混版本持久字段保护设计 §13 第 1~5 条。

## 0′. 2026-11-11 现状(先读这节)

本清单是 10-20 的快照。之后负责人陆续定了几条大方向,下列条目已被决定、已落码,或者对象已不存在;**其余未列的条目,以各自所属线的设计稿和 [server-gap-audit-20261101.md](../server-gap-audit-20261101.md) 为准,本清单不再维护**:

| 条目 | 现状 | 依据 |
|---|---|---|
| G-1、G-2、G-3、G-6、G-9、G-13、G-17、G-21、OPS-1 | 对象已不存在。玩家存档 10-24 定为只存 TiDB,10-31 落码,核心 Redis 上已没有存盘脚本、父快照、补投意图与 `dirty_keys_set` 写入;`go/db/internal/saveoutbox` 已删,K8s 回档脚本 10-31 退役 | 总体方案 `docs/design/player-data-tidb-scaling-plan.md` |
| G-4、G-7、G-10、G-15、G-18 | 方向已定:11-10 负责人要求全部 Redis 上 Cluster,不再按 zone 组分单主。规格与分批见设计稿,7 批都还未开工 | `docs/design/redis-cluster-all-domains-20261110.md` |
| T-9 | 已定:永久 ID 由发号器发打散过的计数器值 | AGENTS.md §7 第 1 条;`docs/design/player-data-tidb-scaling-plan.md` §6 |
| T-12 | 已定:逻辑库自治,一个逻辑库只归一个服务,扩容靠搬逻辑库 | AGENTS.md §7 第 6 条;`docs/design/horizontal-scale-logical-db.md` |
| U-1 | 已落码:refresh token 改成按家族轮换,轮换过的令牌被重放时吊销整个家族;每账号最多 32 个家族,超出时淘汰最久没轮换的那个 | `go/login/internal/logic/pkg/token/{token,family}.go` |
| U-2 | 已对齐到 45000(实测爬坡终点,待 L-14 压测确认):`k8s_deploy.ps1` 的 `LoginQueueZoneCapacity` 与 `java/gateway_node` 的 `schema.sql` 缺省值 | 同左 |
| B1-1 | 已落码:etcd 存活集为空时一直压住;骤降超过一半时压住 30 分钟或等缺席数恢复;阈值规则在 `shared/liveview` | `go/player_locator/internal/logic/gate_sweep.go` |
| X-3 | 已落码:stress_snap 抓 data_service(:9260)与 Redis INFO,stress_summarize 出 data_service 归属读、存盘接口、Redis 负载各表 | AGENTS.md §6.1(2026-10-23 起) |
| X-2 | 已做:`deploy/k8s/AGENTS.md` 已加「C++ 启动壳不加 `ulimit`」(负责人 10-22 确认) | 开服线交接 §3 第 5 项 B0-4 注 |

## 0. 建议先定的几项

下面几项要么卡住后续开工,要么错过时限就不可逆:

| 编号 | 一句话 | 为什么先定 |
|---|---|---|
| T-13 | 点名 Codex 可以操作的环境(专用压测环境 / kind / staging) | L-14 开服尖峰压测、L-18 第 5 步、TiDB 演练都卡在这里 |
| T-9 | 发号打不打散 | 容量模型要求在生产铸出第一个 player_id 之前定;两份文档的时限互相冲突 |
| T-8 | 三张主表去不去 `AUTO_INCREMENT` | 必须在 TiDB 首次建表前定 |
| T-12 | TiDB 是一套集群,还是 OLTP 与审计分开 | Phase 1 文档与容量模型 §4.5 冲突,决定 Phase 1 的部署形态 |
| G-13 | 核心闭包键名是否在首次上线前改成带 `{pid}` | 上线后再改要停写切换 |
| G-7 | mapping 三族的 hash tag 与键名 | 选「开服前做 1c」就要与键名同窗口定 |
| U-2 | 单 zone 目标在线 C_z(45k 还是 10 万) | 5000 的默认准入容量与两种口径都冲突 |
| X-3 | 压测观测怎么补(INFO 快照还是 exporter,stress_summarize 加哪些段) | L-18 复核会话已确认,要先加指标再改脚本,这是单独的代码任务,待负责人指派 |

## 1. 已定,不再问

| 编号 | 事项 | 结论与出处 |
|---|---|---|
| T-21 | D1 `route_meta` 是否设为 TiDB 缓存表 | S2 档起必选、S1 可选(tidb-phase1 §9.3 #21 的 10-15 更正注);这条语句归 Phase 1 部署步骤执行。还剩一项执行要求:freeze-timeout ≥ `tidb_table_cache_lease` + 2s |
| L-4 | 一次登录里的两份角色列表要不要去掉一份 | 负责人 2026-10-19 定:两份都保留(PROGRESS「负责人决定:登录两份角色列表保持现状」) |
| T-18 | zone 回档时按 zone 清理 Redis 的键清单由谁定 | 已被取代:10-17 定 K8s 回档不做 FLUSHDB,键范围由 R9 zone_rollback_discard v3 承担。剩下的是执行项:tidb-phase1 §6.3 第 6 步要按 R9 改写 |
| — | 交接 §2 的「负责人已拍板」表 | 容量目标、核心 Redis 路线、B0-3、B0-1、其余按推荐、项目尚未上线,一律不再列为待定 |
| B1-1 | etcd 存活集合理性闸门的阈值 | 2026-11-05 负责人交 Claude 代定,内容见 §5 同号行;状态机 `go/shared/liveview`,player_locator 死 gate 清扫与 scene_manager fullSync 共用(未编译) |

## 2. TiDB Phase 1(出处:tidb-phase1-and-launch-readiness.md §9.3,下称 §9.3)

| 编号 | 要定的事 | 选项 / 文档推荐 | 时限 | 出处 |
|---|---|---|---|---|
| T-1 | 部署方式 | 推荐 staging / prod / 压测环境用 TiDB Operator v1.6.x;本机继续用 compose;手写 StatefulSet 不进生产 | Phase 1 开工前 | §9.3 #1、§2.1 |
| T-2 | 版本钉到哪个 patch | 必须 ≥ v8.5.5:按库表过滤的 PITR、日志备份运行中做 PITR 都从 v8.5.5 起才有。dev compose 现为 v8.5.2,要同步升级 | Phase 1 开工前 | §9.3 #2、§2.2 |
| T-3 | prod 拓扑与规格;kind 上跑不跑 Operator | 推荐同城三 AZ(3 PD / ≥3 TiKV / ≥2 TiDB / TiCDC×2),规格用压测定。另有待定值 `graceful-wait-before-shutdown`(要 >0)。规格公式见容量模型 §4.5:S1 需 tidb-server 4~8 台,S3 需 28~61 台 | Phase 1 开工前 | §9.3 #3、§2.2、§2.5;容量模型 §4.5 |
| T-4 | namespace 与 pvReclaimPolicy | 推荐用独立 namespace `mmorpg-tidb`(不放进 InfraNamespace);prod / staging 用 Retain,压测环境用 Delete | Phase 1 开工前 | §9.3 #4 |
| T-5 | 迁移方式;哪些环境有必须保留的数据 | 推荐:没有存量的环境全新建库,有存量的用停服窗口 Dumpling + Lightning;开服前不推荐 DM。prod 没有存量(项目未上线);staging、内测服是否有存量未核实 | Phase 1 开工前 | §9.3 #5、§3.0、§3.1 |
| T-6 | TiCDC 反向通道保留天数、MySQL 退役时点、最后一次全量备份保存期 | 文档没给推荐,写明由负责人定。T-5 选全新建库时,反向通道可能用不上 | Phase 1 开工前 | §9.3 #6、§3.4 |
| T-7 | 快照去重在 TiDB 上的形态 | A1 / A2 / B;推荐 B(去掉「可空」,两种方言共用一份 DDL),按 expand / 回填 / contract 三步 | Phase 1 开工前 | §9.3 #7、§4 #1 |
| T-8 | 三张主表去不去 `player_id` 的 `AUTO_INCREMENT` | 推荐去掉;**必须在 TiDB 首次建表前定** | TiDB 首次建表前 | §9.3 #8、§4 #7 |
| T-9 | 发号:不动 / 打散 / 段边界预切;存量 id 审计,还是收紧 `MaxIDExclusive` | 两份文档冲突:TiDB §5.3 写 Phase 1 不动、Phase 2 之前再定;容量模型 A5 第 4 条写「生产铸出第一个 player_id 之前定」。目标已是 2000 万在线,按容量模型的口径要在首次上线前定。打散会改变客户端看得到的 id 分布,要与 Java 同批评估。连带:`player_name` 的 owner 唯一键改为 `(owner_shard, player_id)`,也要在建表前定 | 首次上线前 | §9.3 #9、§5.3、§7 L-13;容量模型 A5 第 3、4 条、§6 #8 |
| T-10 | friend 推荐查询怎么钉执行计划 | (c) 全局绑定 /(a) SQL 提示 /(b) 按方言两份 SQL;推荐先测 (c),不行退到 (a);导入后必须 ANALYZE | Phase 1 开工前 | §9.3 #10、§4 #3 |
| T-11 | 备份存哪、保留多久、PITR 的 RPO 目标 | S3 / OSS / MinIO;保留期至少与 MySQL 现在一样(dump 30 天、binlog 14 天);RPO 没有推荐值 | Phase 1 开工前 | §9.3 #11、§6 |
| T-12 | 全部库同批迁到一个集群,还是 OLTP 与审计分开 | 文档冲突:Phase 1 文档推荐 (a) 一套集群(go/db 只有一个 Hosts、merge_zone 只有一个 DSN);容量模型 §4.5 推荐 S1 以上至少两套、单元化后每单元一套。D9「单独回退」也要随之修订。2026-10-20 已在 Phase 1 文档 §1.2 / §2.2 / §2.7 加注,把冲突摆了出来 | Phase 1 开工前 | §9.3 #12、§2.7;容量模型 §4.5、§6 #11;D9 |
| T-13 | 点名 Codex 可操作的环境 | 逐个授权专用压测环境 / kind / staging。推荐:L-14 只在专用压测环境做,并先跑 MySQL 基线;staging 不做登录压测;kind 只做功能验证 | Phase 1 开工前(L-14 / L-18 之前) | §9.3 #13、§3.5、§7 L-14;D1 提案 §12.2 第 5 步 |
| T-14 | 是否授权 Phase 1 超过 30 个文件(估计 45~60 个) | 需单独授权;D1 的授权不覆盖 Phase 1 | Phase 1 开工前 | §9.3 #14、§9.2 |
| T-15 | 是否授权安装 helm、TiDB Operator、CRD、tiup | 按 AGENTS §10.2 先授权;生产 apply 由人执行;kind 上每条命令都显式写 `--kube-context kind-mmorpg` | Phase 1 开工前 | §9.3 #15 |
| T-16 | go/db 迁移方式 | 推荐新增 db-migrate Job;另一选项是沿用手工 cmd/migrate | Phase 1 开工前 | §9.3 #16 |
| T-17 | login 口令认证门禁脚本(MySQL 存储过程,TiDB 不支持)怎么办 | 迁库前在 MySQL 上跑完,或改写成应用侧检查;文档没给推荐。若正式服不开口令登录(容量模型 §6 #15),影响只限开了 PasswordAuth 的环境 | Phase 1 开工前 | §9.3 #17、§4 #13 |
| T-19 | §7 各项 P0 的负责人与完成时点 | L-01 排队 leader 锁带不带 zone、L-02 削峰阈值用压测定、L-04 设计评审、L-05 = 本文 L-1、L-06 = 本文 G 组 | 首次上线前 | §9.3 #19、§7 |
| T-20 | Java 版那三件要不要同批评估 | 三件是:发号打散对 Java 的影响;排队 / 限流阈值两版对齐;Java 将来上 TiDB 时的共性阻塞。「PARITY 登记不适用」已按 10-19 约定不做 | Phase 1 开工前 | §9.3 #20、§10 |

## 3. 拆 Redis 与单元化(出处:shared-redis-split-by-purpose.md,下称 G4a)

标【新】的是 G4a 修订稿 2026-10-18 新出现的项。§25.2 里标「2026-10-18 新增」的共 12 处:PROGRESS「2026-10-18 G4a 拆 Redis 设计稿修订」条目列的 10 项,再加第 7、9 条的子项,这两个子项已并入 G-7、G-9。

| 编号 | 要定的事 | 选项 / 文档推荐 | 时限 | 出处 |
|---|---|---|---|---|
| G-1 | 围栏硬依赖:A 值封装,还是 B 四键同实例 / 同 slot(排序锁、子表缓存、资产证据、save_barrier)。【新】子项:选 A 时,证据与屏障 ACK 是否仍要在同一原子步骤里核排序锁令牌 | 文档不自行决定;子项交围栏线裁决 | G4a 第 2、3 批开工前 | G4a §25.2 第 1 条、§18.7 |
| G-2 | 【新】go/db 的 RedisClient 拆成「控制面」与「父缓存所在核心」两个句柄,加防指错的启动闸 | 第 2 批要先拆 | 第 2 批开工前 | §25.2 第 2 条 |
| G-3 | 【新】补投分桶索引 `player:save_pending:*` 在 Cluster 下怎么改形 | 照 B0-1 按 slot 分桶,或移出父快照脚本另行设计 | 第 4 批开工前 | §25.2 第 3 条、§20.4 |
| G-4 | 单元规模、单元内主节点数 | 初值建议单元 ≤ 50 万 PCU,等 V2 实测单主上限后再定 | S1 前 | §25.2 第 4 条;容量模型 §3.0 |
| G-5 | dbctl 的 zone 组大小,跨组合服怎么做 | merge_zone 同时持有源、目标两个句柄,或规定只在组内合服;组大小只有推算 | 第 2 批开工前 | §25.2 第 5 条、§18 |
| G-6 | redis-cache 的形态。【新】子项:子表缓存 10-17 起已没有生产读者,还写不写,还要不要单独部署 | Cluster,或按 zone 组分实例;停写要围栏线同意 | 第 3 批开工前 | §25.2 第 6 条、§19 |
| G-7 | mapping 三族的 hash tag:什么时候改、怎么改、键名;读方就绪脚本与读脚本拼键在 Cluster 下怎么改(交 D1) | 推荐键名和 tag 现在就定(例如 `pmap:{<id>}:zone`);开服前做 1c 就同窗口改名,1c 晚于开服就走路径 B。原则 10-13 已定(按 player_id 分片) | 首次上线前(选开服前做 1c 时) | §25.2 第 7 条、§21;D1 提案 §12.1 |
| G-8 | 账号目录 `account:<a>` 落库位置,主键用 account 还是哈希 | 推荐在全局薄层建表,库作提交点,Redis 降为缓存;首次上线前落库就没有回填与混跑窗口 | 第 4 批前置 | §25.2 第 8 条、§22 |
| G-9 | 核心 maxmemory 初值、冷玩家卸载阈值与条件、新实例 maxmemory、补投意图常驻上限 | S1 前必须定;在线热集约 200~420 GB [I],单实例放不下,要与单元化绑定 | S1 前 | §25.2 第 9 条、§23 |
| G-10 | redis-queue 在 2000 万下按 zone 组分实例,还是上 Cluster 打 `{zone}` tag | 排队轮询约 300 万往返/s [I],单实例放不下 | S1 前 | §25.2 第 10 条 |
| G-11 | redis-match-cluster 要不要纳入拆分稿 | 容量模型 A8 建议拆成两个集群;现状 volatile-lru 会淘汰 match 票据与锁 | 无时限 | §25.2 第 11 条 |
| G-12 | 逐节点 SCAN 后 friend 在线目录的游标格式 | 服务端改新格式,兼容读 v1;客户端与 Java 同批评审 | 第 4 批开工前 | §25.2 第 12 条 |
| G-13 | 【新,含第 22 条】核心闭包围栏键怎么迁移,是否首次上线前就改成带 `{pid}` 的键名 | 推荐首次上线前改名(以后上 Cluster 只做重分片)。新键名不能用 `player:{<id>}:…`,那会进入 data_service 的玩家字段命名空间 | 首次上线前(选上线前改名时) | §25.2 第 13、22 条、§20.7 |
| G-14 | 第 2 批门禁「home_zone_unknown 与 class-2 / 3 计数在全部 zone 稳定为 0」怎么判读 | 给 C++ 补指标出口,或按 30s WARN 汇总判读 | 第 2 批开工前 | §25.2 第 14 条 |
| G-15 | 【新】C++ 集群客户端选型 | hiredis-cluster / redis-plus-plus 等;要走新依赖评审,连接数计入 maxclients 与 nofile 预算 | 第 4 批开工前 | §25.2 第 15 条 |
| G-16 | 【新】guild 什么时候从 DB 2 迁到 DB 0 | 迁移时机 | 第 4 批前置 | §25.2 第 16 条 |
| G-17 | 【新】zone_rollback_discard / save_barrier_restore 在拆分 / Cluster 拓扑下的契约 | 按新拓扑重写;1c 之后回档 `-Apply` 必然拒绝 | 首次上线前(契约定稿) | §25.2 第 17 条、§18.6 |
| G-18 | 新实例是否也走高可用;mapping 与核心谁先;哨兵 / haproxy 怎样过渡到 Cluster 自带的故障转移;拆分与 HA 的排期 | 核心 HA 已落码(未验证);HA 只解决可用性,不解决容量 | 首次上线前 | §25.2 第 18 条;D1 提案 §12.1 |
| G-20 | 【新】单元边界语义:跨单元旅行改成转区能否接受;「逻辑单一」的作用域是全服还是单元 | 要与客户端、Java 同批评审 | S1 前 | §25.2 第 20 条 |
| G-21 | 【新】补投工作器容量:缺省约 160 名/s/副本,2000 万口径下可能常态积压 [I] | 按实测调参,或 ACK 齐时同段回收意图(要围栏线评审) | S1 前(L-14 之后) | §25.2 第 21 条、§16.2 |
| G-23 | 拆 Redis 与 L-02(打开排队与限流)谁先 | 推荐先拆 queue | 首次上线前 | 原 §13 第 10 条 |
| G-24 | purge 前浸泡多久、谁执行 | 缺省 24h | 第 1c 批 | 原 §13 第 12 条 |
| G-25 | data_service / go/db 启动时查映射句柄上的墓碑(命中拒启) | 做或不做 | 第 1 批 | 原 §13 第 15 条 |
| G-26 | 拆分拓扑每次部署都带 `-RedisSplit`,还是存进 infra ConfigMap | 二选一 | 第 1a 批 | 原 §13 第 16 条 |
| G-27 | login 的 Redis 超时参数没传进 client,要不要单独修 | 修(DialTimeout 由 5s 变 3s,要单独评审)或不修 | 无时限 | 原 §13 第 18 条 |
| G-28 | G4a 第 1a~4 批怎么排期;第 1a 批是否授权超过 30 个文件 | 按 §24 分批表排期;第 1c 批路径 A 只在开服前或 S1 以内可行 | 首次上线前 | §24 |

## 4. 运维前置(要人执行,不是设计拍板)

| 编号 | 事项 | 做法 | 时限 | 出处 |
|---|---|---|---|---|
| OPS-1 | 存量 `dirty_keys_set` 在哪个环境、何时清 | 维持 10-17「暂不执行」,或指定长期共享环境(如 staging)由人执行;首次上线环境从全新 Redis 起步时没有存量。门禁是版本盘点,只用 UNLINK,24 小时后复查。落码提交是 `e76f6f747`,已回填到 §6.1 | 无时限(必须在门禁之后) | release-checklist §6.1 |
| OPS-2 | B0-4 发布前读各集群、各节点池的容器 nofile 硬限 | 按进程名读 `/proc/<pid>/limits`,记进 PROGRESS。不够时先调低准入上限,`NODE_OPEN_FILE_LIMIT_MODE=warn` 只作短时手段。注意:项目未上线,目标集群可能没有旧 Pod 可 exec,读法要另定;`k8s_deploy.ps1` 还没接这个开关 | 首次上线前(发布带 B0-4 的镜像之前) | gate-kernel-tuning-runbook §3.1 |
| OPS-3 | B0-1:ConfigMap 的 Prometheus 段与 manifest 9190 同批发布;手工 apply `player-locator-alerts.yaml` | 从同一提交跑部署就满足同批。另外,抓取 job 的 relabel 要产出 `namespace`、`pod` 标签,否则按 is_leader 过滤的规则对不上。目标集群有没有这套配置未核实 | 首次上线前 | B0-1 条目;告警文件头 |
| OPS-4 | B0-3 上线顺序 | 首次上线整批起步,不是阻塞项;对已部署过第 1 批 login 的环境,按 zone 金丝雀发 go/db;告警两侧全量后再 apply | 首次上线前 | godb-pipeline-launch-todo §10 |
| OPS-5 | B0-5:手工 apply `scene-manager-alerts.yaml`;长期环境一次性补登记 `scene_manager:zones` | 生产首发没有历史数据,不用补登记;哪些长期环境有历史数据,与 OPS-1 一起答 | 首次上线前(告警) | scene-node-role-split §6.1 |

## 5. 登录、归属与其他

| 编号 | 要定的事 | 选项 / 文档推荐 | 时限 | 出处 |
|---|---|---|---|---|
| L-1 | 是否打开新号免冷读(`NewPlayerSeed.Enabled`)作为开服前置 | 容量模型 §6 #12 建议默认打开;代码已落码、默认关、未验证 | 首次上线前 | D1 提案 §12.1;tidb-phase1 §7 L-05 |
| L-2 | 单台 gate 的连接上限怎么让 login 知道;死 gate 新鲜度过滤做不做 | 做或不做;随机二选一要每 zone ≥3 台 gate 才有实际收益 | 无时限(L-18 核对) | D1 提案 §12.1、§12.2 第 4 步 |
| L-3 | 运维摘孤儿 `RemovePlayersFromAccounts` 要不要接上 gRPC 入口 | 接线(另开任务)或保持没有生产入口。**2026-11-01 已定:接线**,见 [account-role-authority.md](../../design/account-role-authority.md)「摘孤儿角色」(未编译) | 无时限 | D1 提案 §12.1 |
| U-1 | refresh token:每账号最多 32 枚,签发时不作废旧枚 | (a) 不改协议,把上限收紧到 `MaxDevicesPerAccount` 量级(3~5),或签发时作废同账号旧令牌;(b) 每设备 1 枚,`LoginRequest` 要加 device_id,属于客户端契约,两版同批改。Java 有同样问题(`RedisLoginTokens.MAX_REFRESH_PER_ACCOUNT = 32`) | 首次上线前 | 交接 §3 第 6 项;`go/login/internal/logic/pkg/token/token.go` |
| U-2 | 单 zone 目标在线 C_z | 45k 或 10 万。定了之后同批改:`java/gateway_node` 的 schema.sql 与 ZoneConfig 默认值 5000、`k8s_deploy.ps1` 的 `LoginQueueZoneCapacity = 5000` 及契约测试。打开排队前这两处 5000 都不会拒登。Java 版排队直接拿 capacity 当放行预算,同样要改 | 打开排队前 | 容量模型第 781 行附近第 4 条;交接 §3 第 6 项 |
| B1-1 | B0-1 的 etcd 存活集合理性闸门:etcd 成功返回空的或残缺的 gate 存活集时,约 2 拍后会把全部在线会话判死 | 需要另做设计;方向是存活集为空时整拍跳过,骤降超过某个比例时进确认态并告警。阈值请负责人定。**2026-11-05 已定(负责人交 Claude 代定,已落码、未编译)**:①存活集为空而基线不空 → 一直压住,不自动放行;②基线不少于 4 个且缺席超过一半 → 压住;③缺席数连续 T 不变才放行,变了(有成员回来或又少了)从头计时;放行后 T 内照常放行;什么时候算恢复按消费方分:scene_manager 放行后立刻清扫、没有去抖,缺席降到 0 才复位(恢复到一半以下也继续压),player_locator 每个 gate 还要连续两拍缺席才判死,缺席不再过半就复位(否则 gate 滚动发布里一次短暂过半就压 30 分钟);④T 按压住的代价分别定:player_locator 30 分钟(压住只推迟死 gate 上会话的清理,误判是全员被踢,留出人工介入时间),scene_manager 60 秒(压住时整条 load reporter 停摆;C++ 节点失租后几秒内按原 node_id 重新注册);⑤基线:player_locator 是本副本持段 slot 登记集里的 gate,scene_manager 按 zone 取旧镜像 ∪ 领导者读到的负载集(进程刚启动、镜像为空时只有负载集能比);⑥压住的拍不算失败,两边各有 Held / Released 两条 critical 告警;⑦watch 的 DELETE 是真实删除,不过闸门。比例取「超过一半」:Kubernetes 节点控制器在 zone 不健康比例超过 55% 时停 / 减速驱逐,Envoy 健康主机低于 50% 时不再信健康检查,本项目单 zone 节点数下两者只差一台,取一半好理解。设计见 player-locator-gate-session-index.md §4.2、scene-creation-architecture.md「etcd 存活集闸门」;2026-11-01 第一版(按拍计 30 拍)的计数会被让出分片清零,已一并修正 | 无时限(既有行为,不是本线回归) | 交接 §3 第 5 项;player-locator-gate-session-index.md §8 |
| X-1 | AGENTS §6.2 清库口径(负责人亲改) | 补 TiDB 的清库口径;Redis 改为按实例清库;是否加「全局库路由表一并清」(D1 Q8 推荐加) | 首次上线前(L-14 之前) | tidb-phase1 §9.5;G4a 原 §13 第 8 条;D1 提案 §11.2 Q8 |
| X-2 | `deploy/k8s/AGENTS.md` 的拓扑描述要不要改(AGENTS 文件,须负责人确认) | 改「redis 单副本」两处;补单元化方向;加「启动壳不加 ulimit」(B0-4) | 无时限 | tidb-phase1 §9.5;G4a 原 §13 第 14 条 |
| X-3 | 压测观测怎么补:INFO 快照还是 redis_exporter;stress_summarize 加 Redis 段与归属查询段 | L-18 复核会话 10-20 指出:scene_manager 的 `resolveHomeZone` 不在任何计时段里,data_service 没有归属读耗时直方图,login 的 `RefreshRoleZones` 回落连计数都没有,所以要先加指标再改脚本,是一项单独的代码任务,待负责人指派。调研输入是会话草稿里的 `l18_stress-prereq.json` | 首次上线前(L-14 / L-18 之前) | D1 提案 §12.2 第 5 步;G4a 原 §13 第 6、9 条 |
| X-4 | 全球审计管道:快照消息上限二选一、积压保留策略、批量落库何时打开 | 保留策略是阻塞项 | 首次上线前 | tidb-phase1 §7 L-07 |

## 6. Java 版

- 本文只汇总拍板项,不改代码。
- 定下来之后,凡涉及客户端契约的(T-9、G-12、G-20、U-1,以及 U-2 的排队口径),都要与 Java 同批改。Java 待做在定案后登记进 Java `PARITY.md`;「不适用」的不登记。
