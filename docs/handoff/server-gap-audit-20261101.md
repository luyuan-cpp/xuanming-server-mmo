# 服务器代码缺口清单(2026-11-01)

负责人问「服务器代码做完了吗」时做的盘点。按当天工作区逐条对代码核对,只读,没有编译或运行。接手前先核对 `git log` 和本文状态:工作区有多个会话并行。

**负责人目标(2026-11-02)**:「我要达到全链路都能水平扩展」。下面第 4、5 条是已核实的阻塞,但本清单只列了这两条,不等于全链路只差这两条。额度重置后先按 [消除单节点总纲](../design/no-single-node-horizontal-scaling-20261001.md) 的口径逐环节重新盘点全链路:gate、login、client-rpc-router、data_service、scene_manager、scene、go/db、player_locator、各全局服务、Redis、Kafka、etcd、TiDB,看加机器能不能真正扩吞吐,把新发现补进本文,再按依赖顺序落码。现有的全服或多实例选主要逐个核:scene_manager 编排、player_locator 会话对账、guild / trade / player_inbox 资产通道。

**接手先看这段(2026-11-01 第二轮)**:第 6、1 条和第 2 条的终结工具已落码,都没有编译,验证见 [progress.md](player-save-direct-commit-20261030/progress.md) §16「清单第 1、2、6 条」。11-02 又落了第 3 条的大部分和第 11、12 条。11-05 落了第 2 条后半、第 3 条剩余,收尾了第 6 条,都没有编译。11-05 起多个会话分工:第 2、3、6 条在「按缺口清单补完服务器代码」会话;第 1 条残余和待确认里的 Agones 渲染在「按缺口清单做完服务器代码(11-05 额度重置后)」会话(11-05 已落码,未编译);第 9 条 11-05 移交「Scene manager zone selection and etcd threshold」会话;第 4、5 条 11-05 由「控制面按 zone 化与控制域 Redis 分片(缺口第 4、5 条)」会话认领(原归上一会话,对方未动手、同意移交),先写设计再分批落码;第 8、10、13 条和全链路盘点在「服务器代码缺口清单」会话;第 7 条的残余 11-05 由「服务器代码缺口补完」会话做完(未编译)。生成器不清的残留头文件 `player_rollback_service_metadata.h` 要在重新生成之后删。

结论:**没有全部做完。** 下面「未做」的项还在。负责人要求按最标准的方式做完;2026-11-01 暂停在这里是因为每周额度已用 94%(5 个会话共用,4 天后重置),不是判断它们不用做。

## 这一轮已补(未编译,待 Codex 验证,命令见 [progress.md](player-save-direct-commit-20261030/progress.md) §16)

| 项 | 改了什么 |
|---|---|
| 帮会幂等第 3 道查重失败 | 查询失败按故障回,不回会让客户端作废原键的业务拒绝。原来停在 worktree `peaceful-haibt-6a19a3` |
| gate 直连模式会话头 | `player_id` 在唯一构造点归一,不再带出 UINT64_MAX。原来停在 worktree `laughing-wright-5dc98a` |
| Refresh token | 宽限窗口、重用检测与家族吊销。负责人 10-22 批准,Java 版已做,Go 版停在 worktree `relaxed-cerf-d5da4d`。按主线账号单槽布局重写 |
| `TeleportRequest` | 明确回 1006,和 Java 版一致。原来回空应答,客户端以为传送成功 |
| Java 网关 `/api/hotfix-check` | 按配置比较版本。原来恒回「不用更新、1.0.0」 |
| scene 12 条 GM 回档桩 | 任何模式都回 1006。开发模式原来回空应答 |
| etcd 存活集闸门(B1-1) | player_locator:全空一直压住,骤降超过一半压住 30 拍。scene_manager:全量同步回空时不把节点全判消失 |
| 重连漏发入场基线 | 新会话复用还在目标场景的实体时,先离场再走完整入场 |
| 单 zone 容量默认值(U-2) | 5000 改 45000,对齐压测基线。Java 版记待做 |
| TiDB 开发镜像(T-2) | v8.5.2 改 v8.5.5 |
| 角色列表回落计数(X-3) | `homezone.go` 失败放行时计 `login_role_list_home_zone_fallback_total`。验证命令见 `docs/PROGRESS.md` 2026-11-01「映射直读收尾补回落计数」 |

## 未做,按影响排序

规模是估计的文件数。证据行号是 11-01 的,动手前重新核对。

### 会造成错误行为

1. **(11-01 已落码,未编译,验证见 progress.md §16「清单第 1、6 条」)scene_manager 写路径的非原子残余(R3、R5、排空)。**
   - 回滚收进脚本 `luaRollbackInstanceAllocation`:映射仍指向这次分配才认领,认领到才还名额。
   - 空闲回收脚本回显式状态,有人时在脚本里 `ZADD XX` 重置时钟;映射缺失时摘掉条目、交回 agones_gs,不再加回。
   - 三段认领脚本带 token 和 `scene:{id}:claim` 记录(TTL 600s),重发返回原快照;Eval 回错时 token 记在 `SceneClaimTokens`,同进程下一次认领接手。
   - force 认领以映射存在为前提(`absent` 模式除外)。
   - 排空的移动、期望数、标记合成一段脚本 `luaBeginDrainWorldChannel`。
   - 回归:`scene_claim_atomic_test.go`。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「新场景登记改成一段脚本」)新场景的登记**:`allocateScene` 和建实例、铺世界频道的其余写入合成一段脚本(`scene_registration.go` 的 `luaRegisterInstance` / `luaRegisterWorldChannel`),带一次性 token,记在自己的 `scene:{id}:registration`(不与销毁认领共用记录键);EVAL 回错时用同一 token 跑 `luaAbortSceneRegistration` 问清结局,只有确认没落地、以后也不会落地才还 Agones 名额;同一 token 重放「已登记」前先核对映射仍指向原节点,被销毁或改派就回 conflict。新铸的号已有映射或别的 token 的记录时一个键都不写。指标 `scene_manager_scene_registration_total`,告警 `SceneManagerSceneRegistrationConflict` / `SceneManagerSceneRegistrationUnknown`,回归 `scene_registration_test.go`。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「世界频道改派提交后与排空收尾」「改派 CAS 与 Agones 名额转移加重发回执」「回执改为按 token 分键」三条)世界频道改派与排空收尾(10-23 残余 R2、R4)**:init 的死节点改派改为先建后改(`moveWorldChannelOffDeadNode`),排空死属主分支过再入屏障、不扣死节点计数;改派 CAS 与名额转移每次调用一把 token 回执,go-redis 重发原样回结论,Eval 回错时用同一 token 问清结局并作废(`luaResolveCasReceipt`)。回归 `world_channel_commit_order_test.go`、`world_channel_cas_replay_test.go`。
   - **仍剩**:Agones 预占在登记之前,进程在两者之间崩溃时名额不还(Agones 对账告警暴露);进程在认领之后、副作用之前崩溃,副作用仍然丢(R1,靠对账和告警)。

2. **(11-05 已落码,未编译,验证见 progress.md §16「清单第 2 条后半」)交易卡死资产指令的终结工具和 Manual 合同的恢复入口。**
   - 终结工具:`go/trade/cmd/assetopfix`(11-01)。
   - 恢复入口:`go/trade/cmd/commercefix`(list、show、resolve),动作 resume、reissue、refund,只改合同行,新腿由引擎发号。转人工时把原阶段和原因记进 workflow。设计见 `jubaozhai-market.md` §15.7,手册 §4.2。
   - 残余:原主已换、部分发放、竞价与拍品互相引用、已部分成交的市场单没有工具出口,要人工判定;没有 RPC 和管理后台。

3. **(11-05 剩余部分也已落码,未编译,验证见 progress.md §16「清单第 3 条剩余与第 6 条」)审计快照管道(X-4、L-07)。**
   - 11-02:C++ 生产者 `message.max.bytes` 16MiB、lz4;快照 topic 的 `max.message.bytes` 16MiB;发送失败带 `metric=audit_snapshot_send_failed`;`store.RunAuditRetention` 分批删过期行(快照 30 天、流水 180 天),索引、指标、告警。
   - 11-05:`game_config.yaml` 的 `audit_snapshot` 段(进场、离场各自开关,按 player_id 稳定抽样,缺省全开全量),判定在 `modules/snapshot/audit_snapshot_policy.h`,K8s node-config 整段复制;`k8s_deploy.ps1` 的 `-AuditKafkaRetentionBytes` 渲染进 data-service 并计入 Kafka 盘预算,合同测试 `audit_pipeline_deploy_contract.tests.ps1`。
   - 残余:`-AuditKafkaRetentionBytes` 三档缺省都沿用 broker 缺省,值要按压测的审计写入速率和要扛住的消费中断时长定(`deploy/k8s/README.md`「审计 topic 的 retention.bytes」),定之前积压超过 broker 缺省时未消费的审计消息仍会被删。

### 挡水平扩展

4. **控制面没有按 zone 化(B0-5 A1),30~45 个文件。**
   - C++ 每 2 秒全前缀 Range(`service_discovery_manager.cpp:16`、`:29-32`),gate 每 10 秒把人数写 etcd(`cpp/nodes/gate/main.cpp:559-563`)。
   - scene_manager 全服一把锁、全服 watch(`scene_manager_service.go:94-97`、`load_reporter.go:31`、`:886`),领导者循环跑全部 zone。
   - scene 和 gate 连接全部 scene_manager 实例(`cpp/nodes/scene/main.cpp:107`、`cpp/nodes/gate/main.cpp:371`)。
   - 后果:etcd 读量约按进程数平方增长,容量模型算 S2 档是参照值的 6.9~134 倍(`docs/design/capacity-model-hok-scale.md:449-457`)。收窄 watch 前要先改 `load_reporter.go:197-198` 的判定。

5. **控制域 Redis 全服单主,15~25 个文件,依赖第 4 条。**
   - `k8s_deploy.ps1:3819` 只拆了排队;`:10175-10176` scene-manager 所有 zone 都连 `redis.<infra>`;`scene_atomic.go:123-129` 的键没有 hash tag。
   - 所有 zone 的场景路由、世界频道、成员集合和选主锁在一个主节点上,加机器不能扩写入,这个主节点故障时全服进不了场景。
   - progress.md「已知残余」和 `player-data-tidb-scaling-plan.md:447` 写的「按 zone 组分实例」与代码不符。

### 删除和小项

6. **(11-05 收尾:重新生成已做,残留的 `player_rollback_service_metadata.h` 和两条工程条目已删,未编译)scene 12 条 GM 回档桩的 proto 删除。** 同时删了只被它们引用的 `SnapshotDiff*`。生成器不清退 `cpp/generated/rpc/service_metadata/player_rollback_service_metadata.h`(`rpc.vcxproj` 的条目也不管),没有编译单元包含它,无害;重新生成之后由 Claude 删掉它和两条工程条目。当时列的文件:`proto/scene/player_rollback.proto`、`proto/message_id.txt`(102、103、104、106、107、109、110、112、113、115、116、117 改 `reserved:`)、`cpp/nodes/scene/handler/rpc/player/player_rollback_handler.{h,cpp}`、`scene.vcxproj`、`.filters`、`CMakeLists.txt`、`player_service_interface.cpp`、`scene_handler.cpp`、`player_gm_guard.h`、`gate_gm_client_messages.h`、`player_inbox_system.cpp`、`cpp/tests/currency_test/client_gm_gate_test.cpp`、`tools/proto_generator/protogen/internal/client_rpc_kind{,_test}.go`。然后重新生成。
7. **(11-05 已落码,未编译,需先重新生成协议)gate 上限和新鲜度(L-2)。** login 挑 gate 前剔除停写的 gate(三个上报周期没重写 etcd 记录,按 ModRevision 判,与镜像新鲜度分开)和人数到自报 `max_connections` 90% 的 gate,全部不合格时回落全集并计 `login_gate_pick_fallback_total`;`FreeSlots` 不变。NodeInfo 加 `max_connections = 13`,由 C++ gate 启动时写入。设计见 `gate-load-balancing-design.md`「满载与停写的 gate」,验证命令见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤」。
   - (11-05 残余也已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤的残余」)scene_manager 跨 zone 重定向同样剔除这两类 gate:满载读 NodeInfo 的 `max_connections`,停写在本进程按每次直读到的 ModRevision 记历史判(`gatepick.RevisionTracker`);排队取走的两个签票闭包合成 `loginqueue.NewAdmitSigner`,签之前复核放行时选定的 gate,停写、满载或已不在候选里就重挑,读不到候选照签。剔除与回落的规则挪进 `go/shared/gatepick`,四处共用。
   - (11-05 又补,规则未过 promtool、未部署,验证见 `docs/PROGRESS.md` 2026-11-05「挑 gate 回落告警」)两条 fallback 指标加了告警:`deploy/k8s/login-alerts.yaml` 的 `LoginGatePickFallbackStale`(critical)、`LoginGatePickFallbackFull`(warning),`scene-manager-alerts.yaml` 的 `SceneManagerGateRedirectFallback`(warning);静态契约 `tools/scripts/tests/gate_pick_alerts_contract.tests.ps1`。
   - 仍剩:scene_manager 第一次重定向到某个 zone 时判不出停写(只有本进程的历史);Java 版三项都记待做。
8. **(11-05 已落码,未编译)连接池指标(L-08)。** 新增 `go/shared/sqlpoolstats/register.go`(`Register` / `Single` / `PrimaryPool`),chat、friend、guild、mail、match、rank、trade 在入口各注册一个 `primary` 池,login 只在打开 PasswordAuth 时注册 `password_auth` 池;指标同为 `mmorpg_sql_pool_*{sql_client,pool}`,`mysql-connection-alerts.yaml` 的 sql-pool.client 规则按 `(namespace, sql_client, pool)` 分组,新服务不用改规则。Java 网关加 `micrometer-registry-prometheus`,池名固定为 `gateway`,只在 K8s 管理端口 8082 的 `/actuator/prometheus` 上暴露(ConfigMap 打开 exposure,抓取注解由 `k8s_deploy.ps1` 与口令指纹一起生成),告警是新组 `sql-pool.gateway`。player_inbox 按它自己的设计继续导出 `go_sql_*`。验证命令见 `docs/PROGRESS.md` 2026-11-05「连接池指标与压测工具前置」。
9. **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「etcd 存活集闸门定阈值并补齐 scene_manager」)scene_manager 存活集残缺。** 这一轮只拦了全空,部分缺失还会按消失处理。
   - 阈值已定(决策清单 B1-1):空集一直压住;基线不少于 4 个且缺席过半就压住,缺席数连续不变满 T 才放行,变了从头计时。T:scene_manager 60 秒,player_locator 30 分钟。状态机 `go/shared/liveview`,两处共用。
   - scene_manager:fullSync 按 zone 比「旧镜像 ∪ 领导者读到的负载集」(`node_view_gate.go`),补上进程刚启动、镜像为空时比不出骤降的洞;陈旧清扫用闸门同一份负载集读数,zone 范围只经 `nodeViewZones`(按 zone 选主落地时改成只本 zone)。
   - player_locator:11-01 版的压住计数会被让出分片的空拍清零,上限永远到不了,改为按时间计、压住不算失败、不让出段。
   - 两边各加 Held / Released 两条 critical 告警。残余:watch 的 DELETE 风暴不过闸门(etcd 里真实发生的删除),同源的拆实例问题见第 32 条。
10. **(11-05 已落码,未运行)压测工具前置。** `stress_snap.ps1` 缺省加抓 :9170(`t<N>m_match.txt`)与 :9190(`t<N>m_pl.txt`),这两个后缀进 Redis target 保留名;`stress_summarize.ps1` 新增 G1 / G2 / G3(按池撮合总览、按范围层级的成局与等待、按模式)和 PL1(player_locator 对账与服务端 RPC),L1 末列加 `bgz_fb` = `login_role_list_home_zone_fallback_total`,新参数 `-MatchRoundIntervalMs`(缺省 500);robot 新增 `join_queue` 动作与 `match-stress` 档(先地域探测再入队,排满 `match_queue.dwell_seconds` 撤单,成局后等这局结束再排;stress 档权重不变),配置样例 `robot/etc/robot.match-stress-100.yaml`。取值数组由 match、scope、player_locator 三处 Go 钉名测试守住。现状基线还没跑(要 Codex 按 AGENTS.md §6.2 跑一轮)。
11. **(11-02 已加 `SceneManagerDeadNodeReconcileAbandoned`,未部署验证)`dead_node_reconcile_total{outcome="abandoned"}` 没有告警规则。**
12. **(11-02 已删,未编译)login 死配置。** `TaskWaitTimeout`、`LoginTotalTimeout` 从 `config.go` 和 3 份配置里删掉。
13. **(11-05 已落码,未编译,验证见 [mixed-version-persist-handoff-20261019.md](mixed-version-persist-handoff-20261019.md) §17)只在混版本时触发,上线前不阻塞。** 字段台账的机器门禁改为发布前封存:台账加 `sealed=` 行,生成器每次核对已封存世代不变,发布预检 `release.persist_schema.sealed` 要求封存到最大世代(设计 §3.1「封存」);运行期字段集合指纹没做,理由见设计 §13 第 11 条。战斗结算遇到不认识的币种改回 `kVersionSkew`,坏载荷照旧 `kDefer`。

### 全链路水平扩展盘点(2026-11-05,只读)

按「给这个组件加机器,吞吐能不能跟着涨」逐环节核对当前代码,三路并行读,行号是 11-05 的。「抽查」= 本会话又按行号读过一遍;其余是盘点代理读到的,动手前重新核对。第 4、5、7、9 条已有专人,下面不重复;与它们相关的新方面单独标出,已转给对应会话。

**确认能扩、不用改**:go/db 存盘(每 zone 2 副本,只锁单个玩家的写权行,每个 Pod 有自己的 writer_key);发号 `AllocateIdSegment`(内存切段,用完才对 etcd CAS 一次);主键全是打散过的号,目录生产 64 片;C++ battle 房间状态只在进程内、选点随机二选一;friend 推送单收件人;C++ 节点必需密钥在 Deployment 与 Fleet 两种形态都注入了(「服务器代码缺口补完」11-05 核)。

**阻塞:加机器不涨吞吐**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 14 | login 排队(开排队时) | 全部 zone 的排队在一台 Redis(`*redis.Client`,不支持 Cluster,脚本注释 (g));每 zone 两把热键 `queue:zone:<z>`、`admitted:zset:zone:<z>`;每次轮询 GET + GETDEL + EVALSHA;每 zone 一个放行 leader,`PopAdmit` 对本拍成员逐条串行跑脚本,放行速率约等于 1 / Redis 往返 | `loginqueue/queue.go:200`、`:271-272`、`:413`、`:912`、`:751-777`(抽查)、`dispatcher.go:63` | 放行按批流水线或一段脚本放一批;名次改用入队序号差加进程内缓存,少一次往返;排队实例按 zone 组分开 | 本会话接 |
| 15 | 审计流水链路 | 全服一个流水库;流水 topic 默认 6 分区、快照 3 分区,不许原地扩,消费者最多 6 / 3 个;流水每进程一个循环同步落库,快照默认同时只落一批;`timestamp_sec`、`created_at` 等单调索引让全服追加写打在同一个尾部 Region;清理时各副本删同一批最老的行 | `data_service/internal/config/config.go:489`、`:495`、`:555`(抽查)、`kafka/txlog_consumer.go:94-149`、`store/audit_retention.go:55`、`proto/common/database/rollback_database_table.proto:20,68` | 放开扩分区(落库前已按 tx_id 重排并 IGNORE,扩分区不破坏正确性,`store/transaction_log_store.go:118`);按分区并行落库;索引加分桶列或按天分区,清理改 DROP PARTITION | 本会话接(与第 3 条的保留与采样分开,不碰 player_lifecycle.cpp 与 Kafka 渲染段) |
| 16 | trade 合同推进 | 每个副本一个协程逐行跑全局 `Due` 的前 100 行,各副本读同一个队头靠行锁互撞;`advance` 每次都 `Save(updated_ms=now)`,挂单被反复轮询,新成交排在全部活跃合同之后 | `trade/internal/commerce/engine.go:918-953`、`data/commerce_repo.go:50`、`:165`、`svc/assetchannel.go:474-505` | 按 commerce_id 分片归属;没变化不重存;按事件唤醒 | 待认领(原拟转「按缺口清单补完服务器代码」,该会话已结束;它留在工作区的 commerce 未提交改动要先核清再动) |
| 17 | rank 写 | 同一张榜的每次 Submit 都先 `SELECT … FROM rank_board … FOR UPDATE` 再给 `committed_cursor` +1,同榜写入全部串行 | `rank/internal/data/repo.go:146`、`:222`(抽查) | 游标挪到 (榜, 片) 行,封榜时求和 | 待认领 |
| 18 | rank 读 | `Page` / `Around` 每次都对全榜做 `ROW_NUMBER() OVER` + `COUNT`,开销随榜长线性增长,全压在库上 | `rank/internal/data/read.go:54-63`、`:111-113`(抽查) | 名次用索引范围计数,首页与热门页缓存;大榜考虑定期快照 | 待认领 |
| 19 | chat 世界频道 | 没有推送扇出,全员靠拉 SQL;全服只有一个 `stream_key = "world-v2"`,读写挤在 `idx_chat_stream` 同一段尾部 | `chat/internal/authority/store.go:174`(抽查)、`:337`、`schema.go:29`(抽查) | 按 gate 批量推送 + 近窗内存缓存;流键按时间分桶 | 待认领(设计级) |

**部分:能扩,但有放大或上限**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 20 | 写者闸门 | 每笔事务读 `route_meta(id=1)` 两次,并按实例轮转锁或改写 64 个 `writer_slot` 行之一;所有实例的事务都改写这张小表,TiDB 上可能是单 Region 写热点;玩家档每 zone 一个库(`NewPlayerStorageId` 静态) | `shared/authoritysql/writer.go:17`、`:196-262`(抽查) | 先在压测里看热点视图(KeyViz)再定;改用已锁 slot 上的 epoch、少读一次 route_meta | 待认领(先测) |
| 21 | 目录读 | data_service 的连接池全是写者权限,纯读也开闸门事务;go/db 每次认领都 RPC 查一次落点,不缓存 | `shared/authoritysql/driver.go:556`、`data_service/internal/svc/servicecontext.go:305`、`db/internal/playerstore/server.go:142` | 读走 Reader 池;go/db 缓存 storage_id(要先确认合服 pin 之外它确实不变) | 待认领 |
| 22 | 写者名册 | 每启动一次 Pod 永久用掉一条,上限 1024;频繁扩缩容或重启会用完 | `tools/scripts/k8s_deploy.ps1`「Every Pod start uses one entry」一段(该文件在改,行号会变) | 名册回收或按 StatefulSet 序号复用 | 待认领 |
| 23 | guild 排行缓存 | 改分、重建、解散摘榜、合服折叠都要持全服一把 Redis 锁(SETNX,20ms 轮询,TTL 5 分钟),全服公会改分串行;锁是为了让缓存重建与改分不互相覆盖 | `guild/internal/data/guild_repo.go:608`、`:666`、`:759`、`:861`、`:914-944`(抽查) | 改分走带版本的 Lua 写入(版本落一张 hash,解散写墓碑),重建按版本合并而不是整键替换,热路径不再拿锁 | 待认领 |
| 24 | match | 一个池只归一个副本,热池上限就是单副本能力;Pending 全局先 LIMIT 32 再按归属过滤;5 个队伍维护循环每个副本都跑,每 500ms 给每支排队整队开一个锁全员票的事务;投榜没选主,每个副本都串行投全部 64 片 | `match/internal/logic/global_pool_matcher.go:40,80-86`、`authority/store.go:980`、`team/sql_recovery.go:85-145`、`authority/team_maintenance.go:30-66`、`authority/rank_publisher.go:174-218` | 热池拆归属键;Pending 按归属查;维护循环与投榜套用 poolowner 分工 | 回合制缺口会话(投榜选主随 R5 榜单改动一起落;热池归属键先与「PVP匹配队伍组队算法」对齐) |
| 25 | 资产重投 | guild / trade 的 assetop 与 match 终局发件箱,所有副本读同一个到期队头,靠租约 CAS 互斥,空 CAS 随副本数增长 | `shared/assetop/reconcile.go:577-611`、`match/internal/authority/outcome_outbox.go:159,293` | 按玩家分片归属 | 待认领 |
| 26 | player_inbox | 入箱、拉取能扩;补发与对账只在单个 leader 上串行(`SweepTerminal` 恒回 0 是有意的:终态行是永久幂等收据) | `player_inbox/internal/inbox/loops.go:22,339-357` | 64 片分给各副本 | 待认领 |
| 27 | mail | 帮会邮件计入全服活跃上限 100(还没有调用方);活动导入逐片锁活动行再全量 COUNT,名单越大越接近平方 | `mail/internal/data/repo.go:254-261`、`campaign.go:129,157` | 按帮计数;片表累加 | 待认领 |
| 28 | client_rpc_router | 每个请求扫描并拷贝全服镜像选目标,开销随全服实例数增长;连全服全部目标 | `client_rpc_router/internal/discovery/node_watcher.go:269-300`、`:313-344` | 按 zone 建索引 + 原子快照,选点 O(1) | 待认领(该文件正被「发布轨道」那条线改着,等它落定) |
| 29 | Java 网关 | assign-gate 限流每 zone 一把 Bucket4j 键、CAS 版,副本越多冲突越多,热键超 500ms 即放行;健康探测每副本每 5s 全前缀读全服 gate + scene | `java/gateway_node/.../AssignGateRateLimiter.java:97-104,167`、`RateLimitConfig.java:6,91-96`、`ZoneHealthProbeService.java:68-77` | 限流改单条 Lua 原子扣减;探测改 watch 或共享快照 | 待认领(第 4、5 条会话不接) |
| 30 | gate | 连本 zone 全部 scene 和全服全部路由服(G×S + G×R);场景广播给每台 gate 各发一条整包,login 挑 gate 不看场景亲和;gate-cmd 固定 256 分区,每个节点解码全分区再过滤;每台 battle watch 全部同类节点 | `cpp/nodes/gate/main.cpp:376-378`、`cpp/libs/engine/core/.../client_message_sender.cpp:134-150`、`node_command_topic.h:39`、`cpp/nodes/battle/main.cpp:133` | 每台只连 k 个路由服;按场景亲和挑 gate;命令 topic 按 zone 拆或换代扩分区 | 待认领(第 4 条已把 SceneManager 归 zone 内、C++ 发现前缀按 zone 收窄,gate→全服路由服与 battle 全量 watch 不在它的改动里) |
| 31 | 每次 AssignGate | 两次 MGET 读控制域 Redis 的排空标记 | `login/internal/logic/pkg/loginqueue/gatedrain.go:77`、`login/internal/svc/servicecontext.go:540` | 排空标记进 gate 目录镜像 | 待认领(第 5 条只搬 scene_manager 的控制域;排空标记按 G4a §25.3 留在核心实例,login 这两次 MGET 不受第 5 条影响) |

**未核实,要压测或读客户端才能定**:10 万人排队、每 2s 轮询约 15 万条命令 / s 是否打满单台 Redis;路由服到每个目标只有一条 h2 连接的吞吐上限;客户端拉世界频道的频率(没读客户端仓)。

### 11-05 新发现:会造成错误行为

32. **(11-05「Scene manager zone selection and etcd threshold」会话做第 9 条时发现,未做)节点失租后同一进程重新注册,死节点收尾仍会销毁它名下的实例。**
   - C++ 节点 keepalive 回 TTL=0 或本地租约超时后只重新拿租约、按原 node_id 重新注册(`etcd_service.cpp` `OnKeepAliveResponse`、`node.cpp` 约 2367 行),进程和实例都还在;只有分配键 CAS 失败才走 15 秒疏散后退出。
   - scene_manager 在 watch DELETE 时已经写 death_at、摘负载集,并把判死时刻的场景快照写进收尾任务。重新注册的 PUT 只清 death_at(`clearNodeDeath`)、取消推迟摘除,不撤收尾任务;清掉 death_at 之后 `CanReclaimDeadNode` 立即放行,下一拍 `drainPendingDeadNodeReconciles` 按快照强制销毁那些实例,节点此时已回到负载集,`destroyInstanceInternal` 还会向它发 DestroyScene。
   - 后果:单个节点失租超过 TTL(etcd 抖动、网络闪断)就能把它上面的副本全部拆掉,里面的玩家被踢出;etcd 回档造成的大面积失租会放大成大面积拆实例。存活集闸门(第 9 条)只拦 fullSync,拦不住这条。
   - 方向:收尾任务记下判死那一代的 `node_uuid`,重新注册的是同一个 uuid 就撤掉任务(场景不动),是新 uuid 才按快照收尾。涉及 `reentry_barrier.go` 的任务脚本与 `handleWatchEvent` 的 PUT 分支,与按 zone 选主的会话同文件,动手前先对段落。只读核对过代码,没有复现。

### 待确认

- battle 不上报负载(R6;11-05 已归回合制缺口并落码,未编译,待 Codex 验证:可分配状态 `NodeInfo.admission` 只在变化时写 etcd、按回读对账,房间数随 CreateBattle 应答捎回,match 跳过 CLOSED 并按房间数 + 在途选点;验证见 `global-match-hierarchical-pools.md` §12.4)、评分不分模式(R5):是否已归回合制缺口。
- (11-05 核对:不是有意的,已补,未运行,验证见 `docs/PROGRESS.md` 2026-11-05「新场景登记改成一段脚本」)`-SceneOrchestrator agones -AgonesHighDensity` 时 Fleet 的 rooms Counter 和 FleetAutoscaler 都按 rooms 计数,scene_manager 的 ConfigMap 却没有 Agones 段,Go 侧 `Agones.Enabled` 落回 false,rooms 没人记账;访问 Agones API 的 RBAC 也只能手工 apply。现在 `k8s_deploy.ps1` 的 `Test-SceneManagerAgonesEnabled` 为真时写 Agones 段、给 Pod 挂 `scene-manager` 身份并在 Deployment 之前 apply `scene-manager-agones-rbac.yaml`,不接管时删掉这三个对象。只开 agones 编排、不开高密度时仍不接管:一个 GameServer 一次只接一个房间的分配模式不适合世界频道与副本共用进程的 scene。
- 按玩家查询近期战斗终局的 RPC 没找到(`disconnect-reconnect-audit-20261020.md` §9.3),可能归回合制缺口。**结论(11-05):确认是缺口,已补,服务端落码未编译。** 结果在结算应用的同一帧写进存档里的有界列表、与奖励同一次提交,新协议 `SceneBattleResultClientPlayer.GetRecentBattleResults` / `AckBattleResults`;设计与给 Codex 的验证见 [战斗结果重登恢复](../design/battle-result-recovery.md) §10,客户端改动 §7 交 Codex,Java 版待做。
- match 投影表退役(R4)要等压测证据。

## 明确不做

按玩家搬库、全局账号目录、跨地域实时对战、实时战斗的 buff 和技能 TODO(负责人:做成实时 MMORPG 之后才改)、NetEase 登录(等接入)、月卡 AI 托管。

## 查过,已经做了或已作废

MessageSyncRedisClient、redis_test、B0-3 全部、reassignSceneNode CAS、match 旧队列兼容、存盘 INFO、B0-4 部署接线、T-7~T-10、T-12、T-16、T-17、G-1~G-3、G-6、G-8、G-11~G-17、G-21、G-25~G-27、L-1、L-3、L-01、L-04、L-05、L-09~L-12、L-16、X-3。聚宝斋旧商品对侧账 TODO 只挂在没有生产调用方的 `EnqueueEscrowDebit` 上。
