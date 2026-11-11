# 服务器代码缺口清单(2026-11-01)

负责人问「服务器代码做完了吗」时做的盘点。按当天工作区逐条对代码核对,只读,没有编译或运行。接手前先核对 `git log` 和本文状态:工作区有多个会话并行。

**负责人目标(2026-11-02)**:「我要达到全链路都能水平扩展」。下面第 4、5 条是已核实的阻塞,但本清单只列了这两条,不等于全链路只差这两条。额度重置后先按 [消除单节点总纲](../design/no-single-node-horizontal-scaling-20261001.md) 的口径逐环节重新盘点全链路:gate、login、client-rpc-router、data_service、scene_manager、scene、go/db、player_locator、各全局服务、Redis、Kafka、etcd、TiDB,看加机器能不能真正扩吞吐,把新发现补进本文,再按依赖顺序落码。现有的全服或多实例选主要逐个核:scene_manager 编排、player_locator 会话对账、guild / trade / player_inbox 资产通道。

**接手先看这段(2026-11-01 第二轮)**:第 6、1 条和第 2 条的终结工具已落码,都没有编译,验证见 [progress.md](player-save-direct-commit-20261030/progress.md) §16「清单第 1、2、6 条」。11-02 又落了第 3 条的大部分和第 11、12 条。11-05 落了第 2 条后半、第 3 条剩余,收尾了第 6 条,都没有编译。11-05 起多个会话分工:第 2、3、6 条在「按缺口清单补完服务器代码」会话;第 1 条残余和待确认里的 Agones 渲染在「按缺口清单做完服务器代码(11-05 额度重置后)」会话(11-05 已落码,未编译);第 9 条 11-05 移交「Scene manager zone selection and etcd threshold」会话;第 4、5 条 11-05 由「控制面按 zone 化与控制域 Redis 分片(缺口第 4、5 条)」会话认领(原归上一会话,对方未动手、同意移交),先写设计再分批落码;第 8、10、13 条和全链路盘点在「服务器代码缺口清单」会话;第 7 条的残余 11-05 由「服务器代码缺口补完」会话做完(未编译)。生成器不清的残留头文件 `player_rollback_service_metadata.h` 要在重新生成之后删。

**负责人 2026-11-06 定:第 15 条起全部做完。** 第 15 条及之后还没做完的条目,连同已落码条目的「仍剩」,等下次每周额度重置之后全部做完。重置时刻是本机时间 2026-11-12 15:00,重置前不再开新条目。截至 11-06 晚还要做的:

- 没人接:第 15、20、21 条;
- 已有认领、重置时先核落到哪一步:第 22 条(「服务器代码缺口补完」认领),第 25、26 条(原归「灰度发布、金丝雀发布和水平扩展」,该会话已结束);
- 只做了一部分:第 17、19、27、28 条的剩余部分;
- 设计已写、待落码:第 30 条(11-10 四批都已落码、未编译,待 Codex 验证;剩场景亲和与命令 topic 换代,等批 4 两项计数的压测数据再定,见第 30 行);
- 已落码条目的「仍剩」:第 16、24 条;第 1 条的 Agones 名额收敛(11-10 补登,原文在第 1 条「仍剩」;11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领并写了设计(`agones-scene-node-high-density.md` §13),还没落码;**同日负责人让「场景销毁认领:回滚原子化与重发幂等」会话现在做,已由该会话落码,未编译**,验证见 `docs/PROGRESS.md` 2026-11-10「Agones rooms 名额改为按身份的 List 条目」);第 7 条的「跨 zone 重定向第一次判不出停写」(11-10 补登,原文在第 7 条「仍剩」;11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话认领,没动代码;**同日负责人让「玩家切换zone和gate链接」会话现在做,已由该会话落码,未编译**,验证见 `docs/PROGRESS.md` 2026-11-10「跨 zone 重定向读 login 发布的停写标记」);
- 第 23 条:负责人 11-10 看过「暂不改」后要求现在就改,11-11 由「帮会榜积分写入去掉全服维护锁(C3)」会话落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」。
- 11-10 负责人新增第 34 条(全部 Redis 上 Cluster):「Redis 分布式水平扩展」会话认领,设计已写,7 批都没开工。

做法照旧:动手前在本表对应行认领;先写设计再落码;每批不超过 30 个文件;编译、测试与冒烟交 Codex;Java 只登记待做。

**11-10 进度(第 15 条起;都未编译,验证命令见 `docs/PROGRESS.md` 各条)**:

- 已落码:第 15 条(「服务器代码缺口补完」);第 16 条,含按事件唤醒;第 25、27 条;第 20 条的测量指标与测量方案。这四条都是「Turn battle gap closure handoff」做的。
- 已结论不改:第 21 条(收尾归「服务器代码缺口补完」);第 26 条(补发单领导者是有意的)。
- 别的会话已落码(11-10 晚核对,都未编译):
  - 第 22 条:定案并按设计稿 §5 落码,归「缺口第 22 条:写者名册回收」,部署契约的失败也由它跟进;11-11 落码完毕(含 11-10 Codex 那轮两套部署契约失败的修复),未生成、未编译,验证见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」;
  - 第 30 条:四批都已落码。批 1、4 是「服务器代码缺口补完」做的,批 2、3 是「缺口第 30 条:gate 连接与解码扇出」做的;
  - 第 25 条「仍剩」三项(mail / data_service 回收的重投分片、终态行清理、告警)归「G4a Redis 设计稿修订应用」。
- 第 23 条:11-11 已落码(未编译),归「帮会榜积分写入去掉全服维护锁(C3)」;全服帮会榜已删。
- 等压测证据再定:
  - 第 17 条按片拆游标;
  - 第 20 条要不要改闸门;
  - 第 24 条热池;
  - 第 30 条的场景亲和与命令 topic 换代;
  - 第 15 条的 `snapshot_guid` 分桶与按天分区。

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
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「世界频道改派提交后与排空收尾」「改派 CAS 与 Agones 名额转移加重发回执」「回执改为按 token 分键」三条)世界频道改派与排空收尾(10-23 残余 R2、R4)**:init 的死节点改派改为先建后改(`moveWorldChannelOffDeadNode`),排空死属主分支过再入屏障、不扣死节点计数;改派 CAS 与名额转移每次调用一把 token 回执,go-redis 重发原样回结论,Eval 回错时用同一 token 问清结局并作废(`luaResolveCasReceipt`)。回归 `world_channel_commit_order_test.go`、`world_channel_cas_replay_test.go`。11-06 补:改派落空后回收预建实体先上了 60 秒回收栅栏,当天下午换成放置代次(`docs/design/world-channel-placement-generation.md`,PROGRESS 2026-11-06「世界频道放置代次」):proto 的 CreateScene / DestroyScene 加 `placement_generation`,scene_manager 每次换节点分配代次、回收先抬高水位,C++ 节点按代次忽略晚到的旧拆除、拒收被回收放置的晚到建场景;超过 60s 才送到的 DestroyScene 也拆不到之后合法改派过来的实体。回归 `world_channel_placement_generation_test.go`、`cpp/tests/cross_zone_test/scene_placement_fence_test.cpp`。同日再补:显式 DestroyScene 拒绝拆世界频道(以前按副本拆,删映射却不摘在役集合,频道被懒改派复活,不带代次的那次拆除晚到会拆掉复活的实体)。剩下的是孤儿实体(建场景超时晚到、改派者预建后崩溃),由第 33 条节点归属对账收。
   - **(11-05 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「场景拆除任务」)认领之后的副作用不再丢**:认领脚本在提交的同一步写拆除任务 `scene:{id}:teardown` 并进本 zone 的待办 `scene_teardown:zone:{z}:pending`,认领方当场做副作用、做完才删;没做完的(进程退出、回包丢失、某一步失败)由领导者扫描在 90s 租期后重做。scene_count 与属主反向索引挪进认领脚本同一步改;Agones 归还按任务至多一次。11-01 的进程内接手 `SceneClaimTokens` 随之删除。设计见 `scene-creation-architecture.md`「拆除任务」,告警 `SceneManagerSceneTeardownStuck`。
   - **(11-10 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-10「Agones rooms 名额改为按身份的 List 条目」)Agones 名额收敛**:原来 rooms 是计数,三个来源会多记、收不回来(预占在登记之前崩溃;拆除任务的归还「至多一次」,置位之后退出或调用本身失败),改派时新 GameServer 满了还会让新节点少记;对账只告警。
     - 做法(设计见 `agones-scene-node-high-density.md` §13):rooms 改为 Agones List,每次占用一个按身份的条目(scene_id 加随机串),记录 `scene:{id}:agones_gs` 写「GameServer/条目」;要名额之前先写在途占用 `agones_rooms:zone:{z}:holds`,登记与改派转移的脚本在同一步消费它,对账只取消过期的在途占用(取消之后晚到的提交被挡住,回 `fenced`);增删按条目幂等,拆除任务的归还改为失败重做到成功;对账逐条判,两个方向都收敛(删孤儿、取消过期在途占用、补缺失条目、把改派时没转过来的名额转过来)。
     - 11-10 先由「按缺口清单做完服务器代码(11-05 额度重置后)」会话写了设计初稿;同日负责人让「场景销毁认领:回滚原子化与重发幂等」会话现在做,落码时改了两处(条目每次占用一个、不用裸 scene_id;在途占用代替「两轮加宽限期」),原因写在 §13.7。
     - **仍剩**:真集群上的三条假设没核对(GSA 列表字段形状、直接 Update 改 `status.lists`、FleetAutoscaler 的 List 策略,§13.8);GameServer 满了时补不上、转不过来的只能告警(`SceneManagerAgonesRoomsNotConverging`)。根因是世界频道改派(rebalance、懒改派、死节点改派与自愈)挑目标不看容量、事后才转名额,目标满了那台就被超卖一份;标准改法是改派与新建一样「先占名额再搬」,要动世界频道 / 放置代次那条线的改派代码,还要定「属主已死、哪台都满」时搬不搬,写在 §13.10。**11-11 负责人让「场景销毁认领:回滚原子化与重发幂等」会话在这里做,已认领,在做。**
   - **验证状态(11-10 核对)**:Codex 11-06 全量跑过 scene_manager(build、vet、`./internal/logic/...` 756 PASS,见 PROGRESS 同日「修正 scene_manager 五项旧夹具」),本条各批 Go 用例都在内;11-08 命名统一后相关检查也通过。各批 PROGRESS 条目写的变异取红都还没跑。
   - **(11-06 已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-06「销毁路径只改本 zone 的键」)销毁只改本 zone 的键**:认领脚本在同一步比对 `scene:{id}:zone`,不是认领方的 zone(或记录损坏)就一个键都不动;镜像级联与源迁移级联跳过登记在别的 zone 的子镜像;镜像只从本 zone(或本实例里没有 zone 记录)的源的子集合摘自己;显式 DestroyScene 只接本进程 zone 的请求,`zone_id` 没填按本进程的 zone。设计见 `scene-creation-architecture.md`「销毁只改本 zone 的键」,回归 `teardown_zone_scope_test.go`。
   - **仍剩(11-06 补记)**:建实例回滚补发的 DestroyScene 若早于节点处理完在途的 CreateScene,实体仍会留在节点上、没有映射。scene_manager 一侧收不了,由第 33 条「节点侧按 `scene:{id}:node` 对账」收:节点连续两次读到映射不是自己、过 60 秒宽限就排空销毁(11-06 已落码;11-08 Codex 命名统一那轮 `game.sln` 增量构建通过,`cross_zone_test` 里它的 `SceneOwnership*` 20 条与放置代次 `ScenePlacementFence.*` 8 条全 PASS,日志在 `build/verification/naming-audit-20261108/cpp-tests/cross_zone_test/`;真机冒烟没做)。

2. **(11-05 已落码,未编译,验证见 progress.md §16「清单第 2 条后半」)交易卡死资产指令的终结工具和 Manual 合同的恢复入口。**
   - 终结工具:`go/trade/cmd/assetopfix`(11-01)。
   - 恢复入口:`go/trade/cmd/commercefix`(list、show、resolve),动作 resume、reissue、refund,只改合同行,新腿由引擎发号。转人工时把原阶段和原因记进 workflow。设计见 `jubaozhai-market.md` §15.7,手册 §4.2。
   - 残余:原主已换、部分发放、竞价与拍品互相引用、已部分成交的市场单没有工具出口,要人工判定;没有 RPC 和管理后台。

3. **(11-05 剩余部分也已落码,未编译,验证见 progress.md §16「清单第 3 条剩余与第 6 条」)审计快照管道(X-4、L-07)。**
   - 11-02:C++ 生产者 `message.max.bytes` 16MiB、lz4;快照 topic 的 `max.message.bytes` 16MiB;发送失败带 `metric=audit_snapshot_send_failed`;`store.RunAuditRetention` 分批删过期行(快照 30 天、流水 180 天),索引、指标、告警。
   - 11-05:`game_config.yaml` 的 `audit_snapshot` 段(进场、离场各自开关,按 player_id 稳定抽样,缺省全开全量),判定在 `modules/snapshot/audit_snapshot_policy.h`,K8s node-config 整段复制;`k8s_deploy.ps1` 的 `-AuditKafkaRetentionBytes` 渲染进 data-service 并计入 Kafka 盘预算,合同测试 `audit_pipeline_deploy_contract.tests.ps1`。
   - 残余:`-AuditKafkaRetentionBytes` 三档缺省都沿用 broker 缺省,值要按压测的审计写入速率和要扛住的消费中断时长定(`deploy/k8s/README.md`「审计 topic 的 retention.bytes」),定之前积压超过 broker 缺省时未消费的审计消息仍会被删。

### 挡水平扩展

4. **(11-05 已落码;Go 11-06、C++ 11-08 已由 Codex 跑通,部署契约与 kind 冒烟未跑,见 [control-plane-zone-scoping-20261105.md](../design/control-plane-zone-scoping-20261105.md) §8)控制面没有按 zone 化(B0-5 A1),30~45 个文件。**
   - 落码:C++ zone-scoped 类型(含 SceneManager)只 Range / watch 本 zone 前缀,兜底全量 Range 2s → 60s±20%,watch 取消按前缀退避重拉;gate 10s 写 etcd 保留(它是第 7 条停写判定的心跳),扇出随监听方收窄到本 zone;scene_manager 按 zone 选主(`scene_manager:leader:lock:zone:<z>`)、只 watch 本 zone、领导者循环只处理 `ZoneId`;别的 zone 的持有者判定改为 etcd 直读加本副本首次观察屏障;K8s 上 go-zero 发现键按 zone 加 `.z<zone>`。残余见设计稿 §6。
   - 以下是 11-01 的原文。
   - C++ 每 2 秒全前缀 Range(`service_discovery_manager.cpp:16`、`:29-32`),gate 每 10 秒把人数写 etcd(`cpp/nodes/gate/main.cpp:559-563`)。
   - scene_manager 全服一把锁、全服 watch(`scene_manager_service.go:94-97`、`load_reporter.go:31`、`:886`),领导者循环跑全部 zone。
   - scene 和 gate 连接全部 scene_manager 实例(`cpp/nodes/scene/main.cpp:107`、`cpp/nodes/gate/main.cpp:371`)。
   - 后果:etcd 读量约按进程数平方增长,容量模型算 S2 档是参照值的 6.9~134 倍(`docs/design/capacity-model-hok-scale.md:449-457`)。收窄 watch 前要先改 `load_reporter.go:197-198` 的判定。

5. **(11-05 已落码;11-10 修两处缺陷并补齐连接总账与 redis-split-status,这一轮未运行;部署契约与 kind 冒烟未跑,见 [control-plane-zone-scoping-20261105.md](../design/control-plane-zone-scoping-20261105.md) §4、§7 末尾、§8)控制域 Redis 全服单主,15~25 个文件,依赖第 4 条。**
   - **11-10 负责人改口径**:「我要分布式水平扩展」。按 zone 组分实例不算完成,控制域也要上 Redis Cluster,见第 34 条。本条的不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」照用,它是按 zone 打 tag 的前提。
   - 11-10 修的两处缺陷:K8s 没建玩家域集群时,scene_manager 的玩家域会回落到控制域实例(改为回落到 `CoreRedis` 指向的核心);首次部署会被控制域入口闸拒绝(`$null` 被转成空串)。
   - 落码:选「按 zone 组分实例」,不上 Cluster(理由见设计稿 §4.1)。控制域实例 `redis-ctl-g<N>`,组号沿用 zones 配置的 `redisQueueGroup`,模板与形态规则复用排队实例,不受 `-RedisSplit` 控制;键名不改,选主锁与死节点收尾任务表改按 zone,zone 登记集合删除;新不变量「zone z 的控制域键只由 zone z 的 scene_manager 读写」(跨 zone 预检改用 etcd、别的 zone 的成员集合不写);C++ scene 新句柄 `scene_control_redis` 读分线目录与神秘商人,落点唤醒 PUBLISH 留在核心;gate 排空标记经 `CoreRedis` 读核心。没有跨 zone 的原子操作(设计稿 §4.4 逐脚本核对)。`player-data-tidb-scaling-plan.md` §8.3 与 progress.md「已知残余」的说法已改正。残余见设计稿 §6。
   - 以下是 11-01 的原文。
   - `k8s_deploy.ps1:3819` 只拆了排队;`:10175-10176` scene-manager 所有 zone 都连 `redis.<infra>`;`scene_atomic.go:123-129` 的键没有 hash tag。
   - 所有 zone 的场景路由、世界频道、成员集合和选主锁在一个主节点上,加机器不能扩写入,这个主节点故障时全服进不了场景。
   - progress.md「已知残余」和 `player-data-tidb-scaling-plan.md:447` 写的「按 zone 组分实例」与代码不符。

### 删除和小项

6. **(11-05 收尾:重新生成已做,残留的 `player_rollback_service_metadata.h` 和两条工程条目已删,未编译)scene 12 条 GM 回档桩的 proto 删除。** 同时删了只被它们引用的 `SnapshotDiff*`。生成器不清退 `cpp/generated/rpc/service_metadata/player_rollback_service_metadata.h`(`rpc.vcxproj` 的条目也不管),没有编译单元包含它,无害;重新生成之后由 Claude 删掉它和两条工程条目。当时列的文件:`proto/scene/player_rollback.proto`、`proto/message_id.txt`(102、103、104、106、107、109、110、112、113、115、116、117 改 `reserved:`)、`cpp/nodes/scene/handler/rpc/player/player_rollback_handler.{h,cpp}`、`scene.vcxproj`、`.filters`、`CMakeLists.txt`、`player_service_interface.cpp`、`scene_handler.cpp`、`player_gm_guard.h`、`gate_gm_client_messages.h`、`player_inbox_system.cpp`、`cpp/tests/currency_test/client_gm_gate_test.cpp`、`tools/proto_generator/protogen/internal/client_rpc_kind{,_test}.go`。然后重新生成。
7. **(11-05 已落码,未编译,需先重新生成协议)gate 上限和新鲜度(L-2)。** login 挑 gate 前剔除停写的 gate(三个上报周期没重写 etcd 记录,按 ModRevision 判,与镜像新鲜度分开)和人数到自报 `max_connections` 90% 的 gate,全部不合格时回落全集并计 `login_gate_pick_fallback_total`;`FreeSlots` 不变。NodeInfo 加 `max_connections = 13`,由 C++ gate 启动时写入。设计见 `gate-load-balancing-design.md`「满载与停写的 gate」,验证命令见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤」。
   - (11-05 残余也已落码,未编译,验证见 `docs/PROGRESS.md` 2026-11-05「gate 停写与满载过滤的残余」)scene_manager 跨 zone 重定向同样剔除这两类 gate:满载读 NodeInfo 的 `max_connections`,停写在本进程按每次直读到的 ModRevision 记历史判(`gatepick.RevisionTracker`);排队取走的两个签票闭包合成 `loginqueue.NewAdmitSigner`,签之前复核放行时选定的 gate,停写、满载或已不在候选里就重挑,读不到候选照签。剔除与回落的规则挪进 `go/shared/gatepick`,四处共用。
   - (11-05 又补,规则未过 promtool、未部署,验证见 `docs/PROGRESS.md` 2026-11-05「挑 gate 回落告警」)两条 fallback 指标加了告警:`deploy/k8s/login-alerts.yaml` 的 `LoginGatePickFallbackStale`(critical)、`LoginGatePickFallbackDegraded`(warning;11-06 由 `LoginGatePickFallbackFull` 改名,见下一条),`scene-manager-alerts.yaml` 的 `SceneManagerGateRedirectFallback`(warning);静态契约 `tools/scripts/tests/gate_pick_alerts_contract.tests.ps1`。
   - (11-06 复核后改,未编译,验证见 `docs/PROGRESS.md` 2026-11-06「挑 gate 排空并入分档」)排空并入 `gatepick` 的一次分档:完全合格 > 连得上的(排空或满载)> 全部(全部停写时)。此前排空在调用方先剔、停写与满载再剔,两层各自回落,「没排空的全部停写、只剩排空中的健康」会把票签给卡住的 gate;login 的排空还在 `candidatesFromNodes` 就剔掉,`FreeSlots` 少算排空 gate 上的在线人数、排空期间多放人。现在候选只打 `Draining` 标记、容量照数。另加 `go/shared/gatepick/report_period_crosslang_test.go` 钉住 C++ gate 每 10s 无条件重写 etcd 记录(停写判定的前提)。
   - 仍剩:scene_manager 第一次重定向到某个 zone 时判不出停写(只有本进程的历史);Java 版三项都记待做(11-06 补:Java 的 `GatePicker` 也按同一分档做)。
     - (11-10 已落码,未编译、未测试,验证见 `docs/PROGRESS.md` 2026-11-10「跨 zone 重定向读 login 发布的停写标记」)目标 zone 的 login 把 `NodeWatcher` 判出的停写写成带 30s 过期的 `gate:<node_id>:stale`(核心 Redis,不主动删),scene_manager 读排空标记的那次 MGET 一并读出,与本进程历史任一成立即算停写;两个键名收进 `go/shared/gatepick/marks.go` 只定义一份。设计见 `gate-load-balancing-design.md`「满载与停写的 gate」的「停写标记」一条。剩 Java 版三项待做。
8. **(11-05 已落码,未编译)连接池指标(L-08)。** 新增 `go/shared/sqlpoolstats/register.go`(`Register` / `Single` / `PrimaryPool`),chat、friend、guild、mail、match、rank、trade 在入口各注册一个 `primary` 池,login 只在打开 PasswordAuth 时注册 `password_auth` 池;指标同为 `mmorpg_sql_pool_*{sql_client,pool}`,`mysql-connection-alerts.yaml` 的 sql-pool.client 规则按 `(namespace, sql_client, pool)` 分组,新服务不用改规则。Java 网关加 `micrometer-registry-prometheus`,池名固定为 `gateway`,只在 K8s 管理端口 8082 的 `/actuator/prometheus` 上暴露(ConfigMap 打开 exposure,抓取注解由 `k8s_deploy.ps1` 与口令指纹一起生成),告警是新组 `sql-pool.gateway`。player_inbox 按它自己的设计继续导出 `go_sql_*`。验证命令见 `docs/PROGRESS.md` 2026-11-05「连接池指标与压测工具前置」。
9. **(11-05 已落码;scene_manager 用例在 11-06 Codex 全量里全过,player_locator 与 liveview 单测还没有跑,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;原验证命令见 2026-11-05「etcd 存活集闸门定阈值并补齐 scene_manager」)scene_manager 存活集残缺。** 这一轮只拦了全空,部分缺失还会按消失处理。
   - 阈值已定(决策清单 B1-1):空集一直压住;基线不少于 4 个且缺席过半就压住,缺席数连续不变满 T 才放行,变了从头计时。T:scene_manager 60 秒,player_locator 30 分钟。恢复口径:scene_manager 缺席清零才复位,player_locator 不再过半就复位(它每个 gate 还有两拍去抖)。状态机 `go/shared/liveview`,两处共用。
   - scene_manager:fullSync 按 zone 比「旧镜像 ∪ 领导者读到的负载集」(`node_view_gate.go`),补上进程刚启动、镜像为空时比不出骤降的洞;陈旧清扫用闸门同一份负载集读数,zone 范围只经 `nodeViewZones`(按 zone 选主落地时改成只本 zone)。
   - player_locator:11-01 版的压住计数会被让出分片的空拍清零,上限永远到不了,改为按时间计、压住不算失败、不让出段。
   - 两边各加 Held / Released 两条 critical 告警。残余:watch 的 DELETE 风暴不过闸门(etcd 里真实发生的删除),同源的拆实例问题见第 32 条。
10. **(11-05 已落码,未运行)压测工具前置。** `stress_snap.ps1` 缺省加抓 :9170(`t<N>m_match.txt`)与 :9190(`t<N>m_pl.txt`),这两个后缀进 Redis target 保留名;`stress_summarize.ps1` 新增 G1 / G2 / G3(按池撮合总览、按范围层级的成局与等待、按模式)和 PL1(player_locator 对账与服务端 RPC),L1 末列加 `bgz_fb` = `login_role_list_home_zone_fallback_total`,新参数 `-MatchRoundIntervalMs`(缺省 500);robot 新增 `join_queue` 动作与 `match-stress` 档(先地域探测再入队,排满 `match_queue.dwell_seconds` 撤单,成局后等这局结束再排;stress 档权重不变),配置样例 `robot/etc/robot.match-stress-100.yaml`。取值数组由 match、scope、player_locator 三处 Go 钉名测试守住。现状基线还没跑(要 Codex 按 AGENTS.md §6.2 跑一轮)。
11. **(11-02 已加 `SceneManagerDeadNodeReconcileAbandoned`,未部署验证)`dead_node_reconcile_total{outcome="abandoned"}` 没有告警规则。**
12. **(11-02 已删,未编译)login 死配置。** `TaskWaitTimeout`、`LoginTotalTimeout` 从 `config.go` 和 3 份配置里删掉。
13. **(11-05 已落码,未编译,验证见 [mixed-version-persist-handoff-20261019.md](mixed-version-persist-handoff-20261019.md) §17)只在混版本时触发,上线前不阻塞。** 字段台账的机器门禁改为发布前封存:台账加 `sealed=` 行,生成器每次核对已封存世代不变,发布预检 `release.persist_schema.sealed` 要求封存到最大世代(设计 §3.1「封存」);运行期字段集合指纹没做,理由见设计 §13 第 11 条。战斗结算遇到不认识的币种改回 `kVersionSkew`,坏载荷照旧 `kDefer`。

### 全链路水平扩展盘点(2026-11-05,只读)

**11-06 进度(会话「服务器代码缺口清单」)**:第 17、18、27(活动导入)、28(选点)、29、31 条已落码,都没有编译,验证命令见 `docs/PROGRESS.md` 2026-11-05「排行榜读写按名次索引走」「缺口清单第 27、28、29 条落码」「缺口清单第 31 条落码」三条。还没人做的是 16、19、20、21、22、30 和 27、28 的另一半,这一轮没动的原因写在各行:16 所在的 trade/commerce 正被两个数据覆写审计会话和「服务器数据一致性问题」会话审;21 的 data_service 有多个会话的未提交改动,读池还要新的只读账号;20 要先在压测里看热点;19、22、30 和 28 的「连全服全部目标」要先定设计(19 的推送要动客户端协议,22 的名册复用动防僵尸写者的保证,28、30 要做确定性子集或按 zone 部署);30 里 `node_command_topic.h` 另有会话在改。

**11-06 续(同一会话,写设计稿)**:负责人要求把剩下的设计级条目写成设计稿。已写第 30 条与第 28 条后半([connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md))。每周额度到 80% 停在这一批的边界,当时还没写的第 15、16、20、21、22 条,之后都已由别的会话接手(**11-10 核对**:15、16 已落码,20 测量指标已落码,21 收尾归「服务器代码缺口补完」,22 由「缺口第 22 条:写者名册回收」按 writer-roster-exhaustion-20261106.md 落码,状态以各行为准),本段列出的已无遗留。第 15 条那句「本会话接」查不到是哪个会话写的,实际没人在做;第 14 条 11-06 起归另一个「服务器代码缺口补完」窗口,第 19 条归同名的另一个窗口。

按「给这个组件加机器,吞吐能不能跟着涨」逐环节核对当前代码,三路并行读,行号是 11-05 的。「抽查」= 本会话又按行号读过一遍;其余是盘点代理读到的,动手前重新核对。第 4、5、7、9 条已有专人,下面不重复;与它们相关的新方面单独标出,已转给对应会话。

**确认能扩、不用改**:go/db 存盘(每 zone 2 副本,只锁单个玩家的写权行,每个 Pod 有自己的 writer_key);发号 `AllocateIdSegment`(内存切段,用完才对 etcd CAS 一次);主键全是打散过的号,目录生产 64 片;C++ battle 房间状态只在进程内、选点随机二选一;friend 推送单收件人;C++ 节点必需密钥在 Deployment 与 Fleet 两种形态都注入了(「服务器代码缺口补完」11-05 核)。

**阻塞:加机器不涨吞吐**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 14 | login 排队(开排队时) | 全部 zone 的排队在一台 Redis(`*redis.Client`,不支持 Cluster,脚本注释 (g));每 zone 两把热键 `queue:zone:<z>`、`admitted:zset:zone:<z>`;每次轮询 GET + GETDEL + EVALSHA;每 zone 一个放行 leader,`PopAdmit` 对本拍成员逐条串行跑脚本,放行速率约等于 1 / Redis 往返 | `loginqueue/queue.go:200`、`:271-272`、`:413`、`:912`、`:751-777`(抽查)、`dispatcher.go:63` | 放行按批流水线或一段脚本放一批;名次改用入队序号差加进程内缓存,少一次往返;排队实例按 zone 组分开 | **11-06 由「服务器代码缺口补完」会话接手并落码(未编译,验证见 `docs/PROGRESS.md` 2026-11-06「login 排队放行流水线与轮询一次往返」)**:放行第一条单发探路、之后每块 128 条一个 pipeline;轮询的 token 核对、取放行槽、读状态与续期合成一段 `lookupScript`,三次往返降到一次。排队实例按 zone 组拆分本来就有(`-RedisSplit queue`,首次上线显式开),没改。名次改入队序号差没做:它改客户端可见的名次语义,等压测证明 redis-queue 扛不住再做(`login-queue-2026-05.md` §14 第 10 条)。只改了 `loginqueue/{queue,gatetoken}.go` 与测试,`dispatcher.go`、`loginpregate` 没动 |
| 15 | 审计流水链路 | 全服一个流水库;流水 topic 默认 6 分区、快照 3 分区,不许原地扩,消费者最多 6 / 3 个;流水每进程一个循环同步落库,快照默认同时只落一批;`timestamp_sec`、`created_at` 等单调索引让全服追加写打在同一个尾部 Region;清理时各副本删同一批最老的行 | `data_service/internal/config/config.go:489`、`:495`、`:555`(抽查)、`kafka/txlog_consumer.go:94-149`、`store/audit_retention.go:55`、`proto/common/database/rollback_database_table.proto:20,68` | 放开扩分区(落库前已按 tx_id 重排并 IGNORE,扩分区不破坏正确性,`store/transaction_log_store.go:118`);按分区并行落库;索引加分桶列或按天分区,清理改 DROP PARTITION | 本会话接(与第 3 条的保留与采样分开,不碰 player_lifecycle.cpp 与 Kafka 渲染段);**11-06 核对:写这句的会话查不到、多半已结束,实际待认领**,设计稿也还没写(见本节开头 11-06 续);**11-06 由「服务器代码缺口补完」会话认领(负责人要求第 15 条以后都做完),先写设计再落码**;**11-06 已落码(未编译,需先重新生成协议;验证见 `docs/PROGRESS.md` 2026-11-06「审计流水链路水平扩展」)**,设计见 [audit-pipeline-horizontal-scale-20261106.md](../design/audit-pipeline-horizontal-scale-20261106.md):两个审计 topic 只许加分区、少了原地加(`kafkautil.TopicSpec.AllowPartitionGrowth`,四处预建同改);流水每分区一个 worker 并行落库(与快照共用 `kafka/partition_workers.go`,`Kafka.TransactionLogMaxInflightFlushes`);两表加 `shard` 列,按时间的索引都在时间列前插 `shard`;清理每个 (表, 桶) 一把 `GET_LOCK`,各副本各删各的桶。**没做**:`snapshot_guid` 及其唯一键的分桶(要连去重路径一起改,先看压测热点)、按天分区 + `DROP PARTITION`(proto2mysql 表达不了,唯一键也要带分区列)。Java 记待做。**11-10 核对验证证据**:单测已由 Codex 11-08 跑过且全过(`build/verification/naming-audit-20261108/`);真库集成用例(MySQL / TiDB)、部署脚本契约与 compose 冒烟还没跑,清单见 `docs/PROGRESS.md` 2026-11-10「核对第 15 条(审计流水链路)的 Codex 运行证据」 |
| 16 | trade 合同推进 | 每个副本一个协程逐行跑全局 `Due` 的前 100 行,各副本读同一个队头靠行锁互撞;`advance` 每次都 `Save(updated_ms=now)`,挂单被反复轮询,新成交排在全部活跃合同之后 | `trade/internal/commerce/engine.go:918-953`、`data/commerce_repo.go:50`、`:165`、`svc/assetchannel.go:474-505` | 按 commerce_id 分片归属;没变化不重存;按事件唤醒 | **11-06 已落码(未编译,「Turn battle gap closure handoff」会话)**,设计见 [jubaozhai-market.md](../design/jubaozhai-market.md) §15.8,验证见 `docs/PROGRESS.md` 2026-11-06「trade 合同推进按片分工」:合同按 `commerce_id % 64`、收据 ACK 按 `op_id % 64` 分给本 zone 的 stable 副本(新包 `go/shared/workshard`,条件在 `LIMIT` 之前);推进一步没有变化就不重存、不改版本,只等时间或玩家操作的排到截止(最远 1 小时),其余仍每轮复查;撮合失败唤醒下一轮重撮。**11-10 唤醒也已落码(未编译,同一会话)**:资产指令终结(`Finalize` / `Abandon` / 人工终结)提交后唤醒所属合同,成交子单收尾后唤醒母单;在途腿与等子单的母单改为 30 秒兜底复查;设计见 §15.8 第 3 点,验证见 `docs/PROGRESS.md` 2026-11-10「第 16 条按事件唤醒」。**仍剩**:Java 待做。**11-10 核对已收拢**:打分只剩 `shared/workshard.Owner` 一份(11-06 随 1194c4d46 改为 SHA-256,match 的 `poolowner` 与 player_inbox 的 `shardowner` 都调它);成员表与「没就绪时做全部还是跳过」按设计由各服务自己定,不再加开关 |
| 17 | rank 写 | 同一张榜的每次 Submit 都先 `SELECT … FROM rank_board … FOR UPDATE` 再给 `committed_cursor` +1,同榜写入全部串行 | `rank/internal/data/repo.go:146`、`:222`(抽查) | 游标挪到 (榜, 片) 行,封榜时求和 | **11-05 部分落码(未编译,需重新生成协议)**:整批一次读、一次多行写,持锁时间从两百多次往返降到常数次;同榜写入仍串行,按片拆游标行的方案写在 `leaderboard-system.md`「2026-11-05 名次读写走索引」残余第 1 条,等压测证明不够再做 |
| 18 | rank 读 | `Page` / `Around` 每次都对全榜做 `ROW_NUMBER() OVER` + `COUNT`,开销随榜长线性增长,全压在库上 | `rank/internal/data/read.go:54-63`、`:111-113`(抽查) | 名次用索引范围计数,首页与热门页缓存;大榜考虑定期快照 | **11-05 已落码(未编译,需重新生成协议)**:`order_key` 把升降序统一成索引方向,总数取榜行 `entries`,分页顺着索引取,本人名次用索引范围计数,封榜取前 N 也走索引;名次语义与客户端契约不变。残余:深名次的计数与深页 OFFSET 仍随名次增长(同一文档残余第 2、3 条);Java 记待做 |
| 19 | chat 世界频道 | 没有推送扇出,全员靠拉 SQL;全服只有一个 `stream_key = "world-v2"`,读写挤在 `idx_chat_stream` 同一段尾部 | `chat/internal/authority/store.go:174`(抽查)、`:337`、`schema.go:29`(抽查) | 按 gate 批量推送 + 近窗内存缓存;流键按时间分桶 | **11-06 第一批已落码(未编译,验证见 `docs/PROGRESS.md` 2026-11-06「世界频道每副本近窗快照」)**:每个 chat 副本持有世界频道最新一窗的快照,过期才重读、重读单飞,拉取语义与直读共用一份代码,库的读量与在线人数无关,加副本即扩吞吐;本副本提交立刻可见,跨副本最多滞后 `WorldHistoryCacheMs`(缺省 500ms)。设计见 [chat-world-channel-fanout.md](../design/chat-world-channel-fanout.md)。**11-06 第二批推送扇出也已落码(未编译,需先重新生成协议;验证见 `docs/PROGRESS.md` 2026-11-06「世界频道推送扇出」)**:新下行方法 `NotifyWorldChat`;chat 首次提交后写一条 `BroadcastToAllEvent` 进 `chat-world-bcast_g<N>`(4 分区,四处预建);每台 gate 读全部分区、只认这一个消息号,按每台每秒 10 条、突发 20 条的预算推给本机会话,推送开销与全服聊天量无关;推送至多一次,真相仍是拉历史;客户端改动交 Codex。按时间分桶不做:只读最新一窗,分桶不减写热点(理由见设计稿 §5)。Java 记待做 |

**部分:能扩,但有放大或上限**

| # | 环节 | 问题 | 证据 | 修法方向 | 归属 |
|---|---|---|---|---|---|
| 20 | 写者闸门 | 每笔事务读 `route_meta(id=1)` 两次,并按实例轮转锁或改写 64 个 `writer_slot` 行之一;所有实例的事务都改写这张小表,TiDB 上可能是单 Region 写热点;玩家档每 zone 一个库(`NewPlayerStorageId` 静态) | `shared/authoritysql/writer.go:17`、`:196-262`(抽查) | 先在压测里看热点视图(KeyViz)再定;改用已锁 slot 上的 epoch、少读一次 route_meta | **11-10 测量方案已写、测量指标已落码(未编译,「Turn battle gap closure handoff」会话)**:见 [db-write-fencing-mysql.md](../design/db-write-fencing-mysql.md) 文末「2026-11-10 写事务闸门的开销与热点」,验证见 `docs/PROGRESS.md` 2026-11-10「第 27 条帮会邮件按帮计数、第 20 条写者闸门加测量指标」。新指标 `writer_gate_seconds{logical_db, phase}`,闸门判定不变。**仍剩**:按该节「先测」做压测,再按判据决定要不要改。候选从低到高:事务内两条普通读合成一条、预切分 `writer_slot`、加库。不去掉 BEGIN 之前那次 `route_meta` 读 |
| 21 | 目录读 | data_service 的连接池全是写者权限,纯读也开闸门事务;go/db 每次认领都 RPC 查一次落点,不缓存 | `shared/authoritysql/driver.go:556`、`data_service/internal/svc/servicecontext.go:305`、`db/internal/playerstore/server.go:142` | 读走 Reader 池;go/db 的落点缓存要先设计:玩家换库或合服 pin 之后旧缓存会把认领和存盘导到旧库,造成数据分叉,必须带落点版本并在存储侧复核,不能直接加 TTL 缓存(11-05 核对 `go/db/player_store_wiring.go` 的 `newStorageResolver`,SavePlayer 与 LoadAndClaim 每次都经 data_service 路由 RPC) | **11-10 收尾归「服务器代码缺口补完」**:前半「纯读不开闸门事务」已在主干(`authoritysql` 读闸门 `admitRead`,规格 `horizontal-scale-logical-db.md` §2);后半 go/db 落点缓存经核不做,理由见同一规格 §7 末条。「Turn battle gap closure handoff」11-10 曾认领,已撤回、未改文件。没有另开 Reader 池:无闸门读在搬库切换后会读到源库旧数据。未编译,验证见 `docs/PROGRESS.md` 2026-11-10「读闸门收尾」;Java 网关 `WriterAuthority` 未改,Java 记待做 |
| 22 | 写者名册 | 每启动一次 Pod 永久用掉一条,上限 1024;频繁扩缩容或重启会用完 | `tools/scripts/k8s_deploy.ps1`「Every Pod start uses one entry」一段(该文件在改,行号会变) | 名册回收或按 StatefulSet 序号复用 | **11-11 落码完毕,未生成、未编译、未测试,待 Codex 验证**(命令、顺序与通过标准见 `docs/PROGRESS.md` 2026-11-11「写者名册按槽位换代」):账号按槽位、每次启动在槽位上换代(`writer_registry.boot_seq`,注册事务里把上一代退休),名册不再随启动消耗;12 个写者改 StatefulSet,槽位 = 序号;`TestFrequentRestartsNeverExhaustRoster` 守住「同一槽位启动 1100 次」。残余见设计稿 §5.8(REVOKED 行不清理);Java 只登记待做。**11-10 定案(设计稿 §4):账号按槽位、每次启动换代;槽位 = StatefulSet 序号;分 4 批落码。11-10 起由「缺口第 22 条:写者名册回收」会话(「核对其余 worktree 停着的修复有没有进 main」派出)接手,原认领会话「服务器代码缺口补完」已告知。** 以下是 11-06 的分析:**11-06 由「服务器代码缺口补完」会话认领;分析与方案已写,未落码,等负责人拍板**:[writer-roster-exhaustion-20261106.md](../design/writer-roster-exhaustion-20261106.md)。两种修法都要改写者闸门 2026-10-26 的不变量「主体和 instance 永久唯一」(理由是写后崩溃的 boot 排空证明接不上),预计 30 个文件以上。推荐「槽位身份 + 每次启动换代」:名册按槽位批准,每次启动在槽位上原子退休上一代、注册新一代(writer key 与 instance 仍每代唯一,闸门不用改),顺带解决死写者要人工逐个退休。待定:是否放宽「每次启动一个新 SQL 账号」;槽位按 StatefulSet 序号分还是按租约领 |
| 23 | guild 排行缓存 | 改分、重建、解散摘榜、合服折叠都要持全服一把 Redis 锁(SETNX,20ms 轮询,TTL 5 分钟),全服公会改分串行;锁是为了让缓存重建与改分不互相覆盖 | `guild/internal/data/guild_repo.go:608`、`:666`、`:759`、`:861`、`:914-944`(抽查) | 改分走带版本的 Lua 写入(版本落一张 hash,解散写墓碑),重建按版本合并而不是整键替换,热路径不再拿锁 | **11-11 已落码,未编译**(负责人 11-10 看过 11-05 的「暂不改」后要求现在就改;验证见 `docs/PROGRESS.md` 2026-11-11「帮会积分写入不再抢全服锁」):`guild` 表加 `score_version`(改分事务里 +1);改分、建帮上榜、重建合并、折叠合并都走同一段「版本不比已记的旧才写」的脚本;解散在帮会合服组的每个区立墓碑,墓碑按区存、带到期时刻,24 小时后过期,不要清理任务;重建按版本逐条合并,删条目之前回库复核;折叠按版本合并、按读到的版本摘源区。写分、解散摘榜不再拿锁,维护锁只留给重建与折叠互斥,没有删。与左栏原方案的两处不同:不给所有榜键加一个 `{guild_rank}` tag,那会把全部区挤进一个槽;改成每个脚本只碰一个区的键,上 Cluster 时按区打 tag。墓碑不写「最大版本」,改成按区的到期集合,这样不用清理任务。设计见 `docs/design/guild_ranking_architecture_zh.md` §3、§5、§7 |
| 24 | match | 一个池只归一个副本,热池上限就是单副本能力;Pending 全局先 LIMIT 32 再按归属过滤;5 个队伍维护循环每个副本都跑,每 500ms 给每支排队整队开一个锁全员票的事务;投榜没选主,每个副本都串行投全部 64 片 | `match/internal/logic/global_pool_matcher.go:40,80-86`、`authority/store.go:980`、`team/sql_recovery.go:85-145`、`authority/team_maintenance.go:30-66`、`authority/rank_publisher.go:174-218` | 热池拆归属键;Pending 按归属查;维护循环与投榜套用 poolowner 分工 | **11-06 由「全服匹配方案讨论」会话接手并落码(未编译,验证见 `global-match-hierarchical-pools.md` §12.7)**:撮合池、成局推进、整队维护(prepared / queued / forming / completed 四个循环)、闲置队伍到期、投榜共用一条归属规则 `svc.OwnsWork`(成员表为空时全部副本都做);成局推进按 battle_id、整队按发起者、闲置按 team_id 分 64 片,条件放在 SQL 的 LIMIT 之前;投榜按 (模式, 来源片) 分工。残余:已建房票巡检 `ReadyAfter` 同类,文件正由数据覆盖审计会话改,约定其落完后接同一分片条件;热池仍单写者,拆法按压测证据定(设计 §5.2 末条) |
| 25 | 资产重投 | guild / trade 的 assetop 与 match 终局发件箱,所有副本读同一个到期队头,靠租约 CAS 互斥,空 CAS 随副本数增长 | `shared/assetop/reconcile.go:577-611`、`match/internal/authority/outcome_outbox.go:159,293` | 按玩家分片归属 | **11-10 已落码(未编译,「Turn battle gap closure handoff」会话)**,设计见 [assetop-redelivery-sharding-20261110.md](../design/assetop-redelivery-sharding-20261110.md),验证见 `docs/PROGRESS.md` 2026-11-10「资产指令重投与终局发件箱按玩家分片」:assetop 加可选能力 `OwnedDueLister` 与 `Loop.BindWorkOwner`,guild / trade 的重投到期扫描按 `player_id` 分 64 片(本 zone 的 stable 副本,成员表为空时做全部);match 终局发件箱整局结果按 `battle_id`、个人结算按 `player_id`,走 `OwnsWork`;条件都在 `LIMIT` 之前;新指标 `assetop_owned_shards`。**仍剩**:mail 与 data_service 回收的重投循环没分片(成员表怎么圈要另核);到期索引不含 `player_id`,积压大时多扫行,加不加列等压测;guild / trade 的终态行清理每个 stable 副本都跑(重复执行,另记);没加告警规则(设计里给了式子)。**11-10 「仍剩」三项由「G4a Redis 设计稿修订应用」会话认领**(原落码会话「Turn battle gap closure handoff」确认不做)。**11-11 这三项已落码(未编译、未运行,同一会话)**,设计见同一设计稿 §10~§13,验证见 `docs/PROGRESS.md` 2026-11-11「资产指令重投分片第二批」:mail(`mail-assetop`)与 GM 回收(`gm-recall-assetop`)的重投都按 `player_id` 分片、成员表都只圈本 zone(回收的流水库虽全集群一份,但 data_service 随 zone 部署、配置各 zone 自有,圈全服会漏片);guild / trade 终态清理的候选读都能把片条件放在 `LIMIT` 之前,改为复用重投成员表按 `player_id` 分片(类别键 `guild-assetop-cleanup` / `trade-assetop-cleanup`),不改单领导者;告警 `AssetOpOwnedShardsMissing`(`sum by (namespace, service) (assetop_owned_shards) < 64` 持续 5 分钟)与升级规则 `AssetOpOwnedShardsMissingBacklogAging`(再与最老未决年龄 > 30 分钟同时持续 30 分钟)。mail 原先没有任何 assetop 指标,已补。仍不做:到期索引加 `player_id`(等压测)、回收派发循环与 mail 过期清理的分工、循环卡死的活性指标 |
| 26 | player_inbox | 入箱、拉取能扩;补发与对账只在单个 leader 上串行(`SweepTerminal` 恒回 0 是有意的:终态行是永久幂等收据) | `player_inbox/internal/inbox/loops.go:22,339-357` | 64 片分给各副本 | **11-10 核对,不再改**:对账已按 64 片分给各 stable 副本(`internal/shardowner`,11-06 落码,验证见 `docs/PROGRESS.md` 当天条目);清理、补发、gauge 有意保持单领导者:它们的活只随异常增长,单领导者加快速接任就够(`loops.go` 包注释、[消除单节点总纲](../design/no-single-node-horizontal-scaling-20261001.md) §9) |
| 27 | mail | 帮会邮件计入全服活跃上限 100(还没有调用方);活动导入逐片锁活动行再全量 COUNT,名单越大越接近平方 | `mail/internal/data/repo.go:254-261`、`campaign.go:129,157` | 按帮计数;片表累加 | **11-05 活动导入已落码(未编译)**:已导入数改用片行 `recipient_count` 之和,整片一条多行 INSERT、撞主键按冲突整片拒绝(`campaign.go`,用例 `campaign_import_integration_test.go`);帮会邮件计入全服上限(还没有调用方)**11-10 已落码(未编译,「Turn battle gap closure handoff」会话)**:系统邮件仍按全服 `MaxActiveSystemMails`,帮会邮件按帮计 `MaxActiveGuildMailsPerGuild`(缺省 20),发件与建活动共用 `checkSharedMailCapacity`;设计见 `mail-system.md` 文末 2026-11-10 节,验证见 `docs/PROGRESS.md` 同日条目 |
| 28 | client_rpc_router | 每个请求扫描并拷贝全服镜像选目标,开销随全服实例数增长;连全服全部目标 | `client_rpc_router/internal/discovery/node_watcher.go:269-300`、`:313-344` | 按 zone 建索引 + 原子快照,选点 O(1) | **11-05 选点已落码(未编译)**:镜像每变一次在写锁里重建只读索引(按 zone、按发布轨道各一份)并原子替换,选点无锁 O(1);轨道退回仍由 `releasetrack.Select` 在轨道组之间判断,语义不变(用例 `TestPickIndexFollowsEveryMirrorChange`)。「连全服全部目标」**11-10 已落码(未编译,第 30 条批 1)**:全服池目标改用确定性 aperture(同轨道路由服按 node_uuid 排名、在目标环上均匀各占一段,每台约连 `GlobalTargetSubsetSize`=4 台,各目标份额相等、一台不漏;没用设计初稿的 HRW,理由见该文 §5),连接缓存空闲满 10 分钟回收,`ForwardTimeoutMs` 必须短于回收时长;zone-scoped 目标随 gate 侧的 zone 子集收敛(批 2)。见 [connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md) §3.3,验证命令在 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 1、批 4」 |
| 29 | Java 网关 | assign-gate 限流每 zone 一把 Bucket4j 键、CAS 版,副本越多冲突越多,热键超 500ms 即放行;健康探测每副本每 5s 全前缀读全服 gate + scene | `java/gateway_node/.../AssignGateRateLimiter.java:97-104,167`、`RateLimitConfig.java:6,91-96`、`ZoneHealthProbeService.java:68-77` | 限流改单条 Lua 原子扣减;探测改 watch 或共享快照 | **11-05 已落码(未编译)**:限流改成一段 Lua 同时判 IP 桶与区桶(`ratelimit/RedisRateLimitStore.java`),先 IP 后区、区桶空不扣 IP 令牌(顺带了结开服限流设计 R13 / J1),拒绝路径只读、键在补满时过期,阈值随调用传入、滚动即生效;Redis 出错 fail-open 并熔断 5 秒,进程内冷却照判;新增 `gateway_rate_limit_decisions_total` / `gateway_rate_limit_fail_open_total`;去掉 Bucket4j 依赖。健康探测与 login 发现改为 list + watch 镜像(`etcd/NodeInfoMirror.java`),稳态不整段读,watch 出错或满 5 分钟才重新整段读。剩余:全服前缀的 watch 仍是每副本一份,事件扇出随 zone 数平方增长(只有变更,gate 每 10 秒一条);再要降,改由一个发布者汇总成每 zone 一条健康记录。`k8s_deploy.ps1` 与两份 ps1 契约测试的注释仍写着「Bucket4j」(键名与连法没变,只是叫法旧)。Java 版限流已是 Lua(等价),gateway 全服共用、探测读量随 zone 线性增长,不适用 |
| 30 | gate | 连本 zone 全部 scene 和全服全部路由服(G×S + G×R);场景广播给每台 gate 各发一条整包,login 挑 gate 不看场景亲和;gate-cmd 固定 256 分区,每个节点解码全分区再过滤;每台 battle watch 全部同类节点 | `cpp/nodes/gate/main.cpp:376-378`、`cpp/libs/engine/core/.../client_message_sender.cpp:134-150`、`node_command_topic.h:39`、`cpp/nodes/battle/main.cpp:133` | 每台只连 k 个路由服;按场景亲和挑 gate;命令 topic 按 zone 拆或换代扩分区 | **11-06 设计稿已写,待落码**:[connection-fanout-subsetting-20261106.md](../design/connection-fanout-subsetting-20261106.md)。gate→路由服按 zone 取 HRW 子集(k=3);每类 C++ 节点只发现自己用到的类型,battle 只看自己的键;gate↔scene 的 zone 内全连接保持;命令 topic 每节点只 assign 一个分区,先加读放大计数再决定换代;场景亲和先测,要做时按 gatepick 的一维加(挑 gate 规则以「玩家切换zone和gate链接」会话为准)。分 4 批,见该文 §6。**11-10 按批分工**:批 1(路由服全服池子集与空闲回收,含第 28 条「连全服全部目标」)与批 4(读放大与扇出计数)归「服务器代码缺口补完」;批 2(gate 路由服子集与连接准入钩子)与批 3(每类节点的发现集合)原归「Turn battle gap closure handoff」,该会话未动手;**11-10 负责人改派给「缺口第 30 条:gate 连接与解码扇出」会话**,先补设计稿(批 3 补上 node_id 与端口分配对同类快照的依赖)再落码。**11-10 批 1、批 4 已落码(未编译,「服务器代码缺口补完」)**:批 1 见第 28 条;批 4 加了命令 topic 读放大计数 `node_command_consumed_total{node_type,topic,target}` 与告警 `deploy/k8s/node-command-topic-alerts.yaml`,以及广播扇出直方图 `scene_broadcast_gate_fanout{kind}`(Prometheus 写出器补了 histogram 类型)。Java 只登记批 1 的同类问题(gate 经 Dubbo 连全部提供者),验证命令在 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 1、批 4」。**11-10 批 2、批 3 已落码(未编译、未测试,「缺口第 30 条:gate 连接与解码扇出」;验证命令与通过标准见 `docs/PROGRESS.md` 11-10「连接扇出第 30 条批 2、批 3」与 11-11「……批 2、批 3 补」,后者多一条部署契约测试)**:gate 按 zone 号做 HRW,只连、只挑 k 台路由服(`GATE_ROUTER_SUBSET_SIZE`,缺省 3),掉出子集的过调用期限加 1 秒再拆,选点 READY 优先;每类 C++ 节点按 `main.cpp` 声明的发现集合 Range / watch(不再看 Friend、Guild、用不到的全服池类型),battle 只看自己那一条发现键;顺带修了 11-05 发现前缀按 zone 收窄后的逐个撞号:分配键 CAS 失败时同一个 txn 读回全服已分配的号,下次选号跳过。Java 已核对:批 2 的对应项就是上面 Java 登记的那条,批 3 已有等价或不适用(设计稿 §8)。**第 30 条仍剩**:场景亲和与命令 topic 换代,按设计先看批 4 两项计数的压测数据再定 |
| 31 | 每次 AssignGate | 两次 MGET 读控制域 Redis 的排空标记 | `login/internal/logic/pkg/loginqueue/gatedrain.go:77`、`login/internal/svc/servicecontext.go:540` | 排空标记进 gate 目录镜像 | **11-05 已落码(未编译)**:没把标记搬进 etcd(要改 `k8s_gate_drain.ps1` 的原子标记脚本与 scene_manager 的读法),改成 login 进程内按 zone 的读穿透缓存 `loginqueue.DrainMarkCache`:选 gate 的各条路径(快速通道、容量判断、dispatcher、放行复核)共用一份,每个 login 实例每个 zone 每秒至多一次 MGET;并发过期只查一次,查询失败照旧按没人排空放行且同样缓存 1 秒。标记最多晚 1 秒生效,与排空流程本来就容忍的晚到玩家(标记前签出的票、gate 每 10 秒才报在线数)同一类。scene_manager 跨 zone 重定向的 `drainingGateIDs` 调用量小,没改。另一会话的设计稿 `gate-drain-session-migration.md`(未落码)第 2 步「读调用方 gate 的 draining 标记」是纵深防御,落码时要直读 Redis,不要走这份缓存 |

**未核实,要压测或读客户端才能定**:10 万人排队、每 2s 轮询约 15 万条命令 / s 是否打满单台 Redis;路由服到每个目标只有一条 h2 连接的吞吐上限;客户端拉世界频道的频率(没读客户端仓)。

### 11-05 新发现:会造成错误行为

32. **(11-05「Scene manager zone selection and etcd threshold」会话做第 9 条时发现;11-05 同一会话已落码;7 条回归用例在 11-06 Codex 的 scene_manager 全量里全过,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;原验证命令见 2026-11-05「死节点收尾认出失租后回来的同一进程」)节点失租后同一进程重新注册,死节点收尾仍会销毁它名下的实例。**
   - **已改**:收尾任务记下判死那一代的 `node_uuid`(`node:zone:{z}:{n}:reconcile_instance`,watch DELETE 带着它,fullSync 清扫从旧镜像补);drain 在判屏障之前看镜像,同一代回来就撤任务,场景与计数不动(`outcome="instance_back"`);换代、或不知道是哪一代时照旧收尾。结束脚本加了守卫:任务已不在表里就不删计数。改的是 `reentry_barrier.go` 的任务脚本与 drain、`load_reporter.go` 的清扫,PUT 分支没动。
   - **残余**:只覆盖「在屏障放行、drain 动手之前回来」。租约 180s、屏障 20s,失联约 200 秒以上才回来的节点见第 33 条。注册键被误删、租约被撤销、租约集中到期这几种,节点 1~2 秒内就按同一个 node_uuid 重新注册(keepalive 每秒一次,健康检查发现本节点不在发现快照里也会重注册),都在本条覆盖范围内。
   - C++ 节点 keepalive 回 TTL=0 或本地租约超时后只重新拿租约、按原 node_id 重新注册(`etcd_service.cpp` `OnKeepAliveResponse`、`node.cpp` 约 2367 行),进程和实例都还在;只有分配键 CAS 失败才走 15 秒疏散后退出。
   - scene_manager 在 watch DELETE 时已经写 death_at、摘负载集,并把判死时刻的场景快照写进收尾任务。重新注册的 PUT 只清 death_at(`clearNodeDeath`)、取消推迟摘除,不撤收尾任务;清掉 death_at 之后 `CanReclaimDeadNode` 立即放行,下一拍 `drainPendingDeadNodeReconciles` 按快照强制销毁那些实例,节点此时已回到负载集,`destroyInstanceInternal` 还会向它发 DestroyScene。
   - 后果:单个节点失租超过 TTL(etcd 抖动、网络闪断)就能把它上面的副本全部拆掉,里面的玩家被踢出;etcd 回档造成的大面积失租会放大成大面积拆实例。存活集闸门(第 9 条)只拦 fullSync,拦不住这条。
   - 方向:收尾任务记下判死那一代的 `node_uuid`,重新注册的是同一个 uuid 就撤掉任务(场景不动),是新 uuid 才按快照收尾。涉及 `reentry_barrier.go` 的任务脚本与 `handleWatchEvent` 的 PUT 分支,与按 zone 选主的会话同文件,动手前先对段落。只读核对过代码,没有复现。

33. **(11-06 登记;11-06 初版已落码,20 条 C++ 用例在 11-08 Codex 的 `cross_zone_test` 全集里全过,见 `docs/PROGRESS.md` 2026-11-10「核对存活集闸门(B1-1)与缺口第 9、32、33 条的验证证据」;11-10 补完三项残余,已落码,未编译、未测试,用例改为 27 条 C++、新增 15 条 Go(含 etcd 修订号倒退后换纪元的持有者修正,11-10 按父会话评审重做),验证见 [scene-node-ownership-reconcile.md](../design/scene-node-ownership-reconcile.md) §10 与 `docs/PROGRESS.md` 2026-11-10「场景归属对账补完」)失联超过屏障又回来的节点,还托管着已被改派的场景。**
   - **11-10 补完**:去掉「etcd 身份确认有效才核对」,与 etcd 失联期间也按映射收敛(etcd 整体故障时 scene_manager 不判死、映射不变,不会误排空);scene_manager 把每个节点号最近一次登记的进程写进 `node:zone:{z}:{n}:holder`(按 etcd ModRevision 单调,`node_holder.go`),节点读到自己的号被更新的另一代接走就把本地场景全部排空;etcd 修订号倒退(换集群、不带 bump 恢复)时领导者按「先读本 zone 水位、后线性一致读 etcd 当前修订号」认出来,换一次 etcd 纪元,倒退之前的记录让位给新登记(告警 `SceneManagerEtcdRevisionRegressed`,处置见 etcd 运维手册 §7a);开始排空按每轮每节点 200 名住户的预算限速,控制域丢数据时分批改派;新增 `deploy/k8s/scene-ownership-alerts.yaml` 五条告警与契约测试。
   - **11-10 之后仍剩**(设计稿 §9):本进程登记之后 scene_manager 一次都没写过它的持有者记录时认不出被接走,退回重连 etcd 时的冲突疏散;住户预算 200 是没有压测基线的初值;告警没过 promtool、没装进集群;控制域刚丢了修订号水位的那一轮认不出 etcd 倒退;倒退后没重启的老进程认不出被接走(etcd 运维手册 §7a 要求全部重启)。
   - **11-06 初版**(当时只在 etcd 身份确认有效时核对,11-10 去掉了这道门禁):scene 节点每 10 秒用 MGET 读本地全部场景的 `scene:{id}:node`(`tlsSceneControlRedis`,只读);连续两次不是本节点、且距第一次核对或最近一次 CreateScene 按号命中满 60 秒,就调 `SceneLifecycle::DestroyScene`(先 `BeginSceneDrain` 改派住户,没有绑定才销毁)。CreateScene 去重命中已有实体时 `SceneLifecycle::ReuseExistingScene` 把宽限期从头算(盖住 scene_manager「先建后改」迁回本节点的预建)。跨 zone 类型与镜像的口径与普通场景相同(设计稿 §5)。顺带收掉其他「有实体没映射」的孤儿,包括 11-05「场景拆除任务」残余里「回滚补发的 DestroyScene 早于在途 CreateScene」那一条(设计稿 §7)。
   - **11-06 初版的残余**(11-10 已补,见上):一直连不上 etcd 的节点不对账,分裂持续到 etcd 恢复;控制域丢数据时会大面积排空;还没有告警规则。
   - 以下是 11-06 登记时的原文。
   - 条件:节点与 etcd 单边失联(仍连着 gate 和 Redis,玩家还在上面玩)超过租约 180s 加屏障 20s。
   - 这时 scene_manager 已经把它判死:
     - 拆掉它名下实例在 Redis 里的映射。当时它不在负载集,不发 DestroyScene;
     - 把世界频道改派到别的节点。
   - 网络恢复后它按原 node_id、同一个 node_uuid 重新注册,第 32 条的撤销已经晚了。进程里留下两类没有映射的实体:
     - 实例:新玩家进不去,scene_manager 的空闲回收也看不到它;
     - 世界频道:还留在旧实体里的玩家,与新进来的玩家分在两个节点上,互相看不见。
   - 玩家数据不受影响:被接管过的玩家,旧节点的存盘由 owner epoch 拒绝;没被接管的照常存盘。
   - 不选「失租就自己停服」:etcd 整体故障时所有节点都会失租,自停等于 etcd 一挂全服掉线,而节点分不清「etcd 挂了」和「只有我连不上」。11-05 数据一致性那条线(`go/scene_manager/internal/constants/reentry_barrier.go` 文件头的更正)也定的是「老节点继续服务、靠写权行挡回档」。
   - 标准做法是节点回来之后按控制面的期望状态对账:Kubernetes 的 kubelet 回到集群后,把 API server 上已删的 Pod 在本地停掉;Java 版是 scene 拉计划、不在计划里的频道就排空(PARITY「场景实例与主世界频道」行 D3)。
   - 方向:
     - scene 节点在身份确认有效(`IsIdentityConfirmedFresh`)时,定期经 `scene_control_redis` 核对本地每个场景的 `scene:{id}:node`;
     - 连续两次不是自己、且场景建好已超过一个下限(避开 scene_manager「先建后改」的窗口),就 `BeginSceneDrain` 排空并销毁;
     - 跨 zone 场景与镜像的口径要单独核。
   - 只在节点侧做,不需要新协议。涉及 `scene_lifecycle.cpp`、`player_lifecycle.cpp` 的排空和控制域读,这几处同时有别的会话在改,先写设计再落码。

### 11-10 新发现:挡水平扩展

34. **(11-10 负责人要求;「Redis 分布式水平扩展」会话认领;设计已写、未落码,见 [redis-cluster-all-domains-20261110.md](../design/redis-cluster-all-domains-20261110.md))服务器里的 Redis 没有全部分布式水平扩展。**
   - 负责人原话:「我要分布式水平扩展」。按 zone 组分单主实例(`redis-queue-g<N>`、`redis-ctl-g<N>`)和核心单主都不算完成,所有 Redis 都要能加主节点扩写入。
   - 现状(11-10 只读盘点):
     - 只有玩家域集群(玩家 + 登录域,已落码未运行)和 match 私有集群是 Cluster;排队、控制域每个 zone 组一个主;核心 `redis` 全服一个主,上面有 gate 标记、唤醒频道、四个服务的选主锁、guild 与 friend 私有键、player_locator 控制键、名字缓存。
     - 只换地址会出错或静默失效:入队跨槽直接报错;gate 排空过滤与网关限流跨槽后 fail-open,不报错地失效。
   - 方向:
     - 新建控制集群 `redis-control`,多键操作的键按 zone / 帮 / 玩家共用 hash tag;唤醒改分片 Pub/Sub(`player_id % 64`)。
     - Go 句柄全部换成与玩家域同一个构造器,go-zero 的 Redis 句柄退役;C++ scene 合成一个集群句柄,gate、battle 不再连 Redis。
     - 退役核心、queue、ctl 实例。单个 zone 的控制域与排队仍各在一个主节点上(设计 §6.2)。
   - 分 7 批,每批不超过 30 个文件,合计约 170~200 个文件。断线重连清单的 R8 并入句柄替换批,R7 归断线重连会话(11-10 两边约定,设计 §13)。
   - 依赖:唤醒订阅只订本副本分片,要等第 25 条;排队按名次拉长轮询间隔,要先核对客户端遵守 `RetryAfterMs`。

### 待确认

- battle 不上报负载(R6;11-05 已归回合制缺口并落码,未编译,待 Codex 验证:可分配状态 `NodeInfo.admission` 只在变化时写 etcd、按回读对账,房间数随 CreateBattle 应答捎回,match 跳过 CLOSED 并按房间数 + 在途选点;验证见 `global-match-hierarchical-pools.md` §12.4)、评分不分模式(R5;11-05 已归回合制缺口并落码,未编译,待 Codex 验证:评分按 `(player_id, mode)` 存,投榜来源与 outbox 按模式分片、每个计分模式一张榜(board_type 1V1=1、5V5=2、PVP_TEAM=3),封榜按模式停写;回合打满先看个人原件里 0 队是否全灭、旧版结果按模板 `time_limit` 换算的回合上限判平,删 `RatingDrawRoundCap*`;计分局禁观战核对为已做;验证见 `global-match-hierarchical-pools.md` §12.6)。
- (11-05 核对:不是有意的,已补,未运行,验证见 `docs/PROGRESS.md` 2026-11-05「新场景登记改成一段脚本」)`-SceneOrchestrator agones -AgonesHighDensity` 时 Fleet 的 rooms Counter 和 FleetAutoscaler 都按 rooms 计数,scene_manager 的 ConfigMap 却没有 Agones 段,Go 侧 `Agones.Enabled` 落回 false,rooms 没人记账;访问 Agones API 的 RBAC 也只能手工 apply。现在 `k8s_deploy.ps1` 的 `Test-SceneManagerAgonesEnabled` 为真时写 Agones 段、给 Pod 挂 `scene-manager` 身份并在 Deployment 之前 apply `scene-manager-agones-rbac.yaml`,不接管时删掉这三个对象。只开 agones 编排、不开高密度时仍不接管:一个 GameServer 一次只接一个房间的分配模式不适合世界频道与副本共用进程的 scene。
- 按玩家查询近期战斗终局的 RPC 没找到(`disconnect-reconnect-audit-20261020.md` §9.3),可能归回合制缺口。**结论(11-05):确认是缺口,已补,服务端落码未编译。** 结果在结算应用的同一帧写进存档里的有界列表、与奖励同一次提交,新协议 `SceneBattleResultClientPlayer.GetRecentBattleResults` / `AckBattleResults`;设计与给 Codex 的验证见 [战斗结果重登恢复](../design/battle-result-recovery.md) §10,客户端改动 §7 交 Codex,Java 版待做。
- match 投影表退役(R4):11-05 已落码(未编译),投影已无读者,不需要压测证据;见 `global-match-hierarchical-pools.md` §6。

## 明确不做

按玩家搬库、全局账号目录、跨地域实时对战、实时战斗的 buff 和技能 TODO(负责人:做成实时 MMORPG 之后才改)、NetEase 登录(等接入)、月卡 AI 托管。

## 查过,已经做了或已作废

MessageSyncRedisClient、redis_test、B0-3 全部、reassignSceneNode CAS、match 旧队列兼容、存盘 INFO、B0-4 部署接线、T-7~T-10、T-12、T-16、T-17、G-1~G-3、G-6、G-8、G-11~G-17、G-21、G-25~G-27、L-1、L-3、L-01、L-04、L-05、L-09~L-12、L-16、X-3。聚宝斋旧商品对侧账 TODO 只挂在没有生产调用方的 `EnqueueEscrowDebit` 上。
