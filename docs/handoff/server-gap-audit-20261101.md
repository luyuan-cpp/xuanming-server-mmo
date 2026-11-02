# 服务器代码缺口清单(2026-11-01)

负责人问「服务器代码做完了吗」时做的盘点。按当天工作区逐条对代码核对,只读,没有编译或运行。接手前先核对 `git log` 和本文状态:工作区有多个会话并行。

**接手先看这段(2026-11-01 第二轮)**:第 6、1 条和第 2 条的终结工具已落码,都没有编译,验证见 [progress.md](player-save-direct-commit-20261030/progress.md) §16「清单第 1、2、6 条」。周额度到 98% 停下。下一步:第 2 条的 Manual 合同恢复入口,然后第 3~5 条(第 4、5 条先写设计),再第 7~13 条和「待确认」。生成器不清的残留头文件 `player_rollback_service_metadata.h` 要在重新生成之后删。

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
   - **残余**:`allocateScene` 和建实例的其余写入仍是多步,进程在中途崩溃可能留下映射而不在 active 集合里;进程在认领之后、副作用之前崩溃,副作用仍然丢(R1,靠对账和告警)。

2. **交易卡死资产指令的终结工具 11-01 已落码(未编译);Manual 合同的恢复入口未做,8~12 个文件。**
   - 已做:`go/trade/cmd/assetopfix`(list、resolve,前置和退出码与 guild 版相同)、`AssetOpRepo.GetOp`、`ListStuck`。`Pipeline.ResolveManually` 仍没有生产调用方,工具走包级 `assetop.ResolveManually`。
   - 未做:合同隔离后托管的物品和货币还是出不来。`proto/trade/trade_admin.proto:44` 只有 SeedListing。要另立设计,覆盖鉴权、操作留痕、沿用原腿和原 seq 的幂等。手册 `docs/ops/trade-commerce-quarantine-runbook.md` §4.2、§6。

3. **审计快照管道(X-4、L-07),8~12 个文件。**
   - `cpp/libs/modules/snapshot/snapshot_system.cpp:110-115` 发送失败只打日志;`kafka_producer.cpp:93-118` 没设消息上限和压缩;`go/data_service/internal/kafka/start.go:145-146` 没有 retention.bytes。
   - `snapshot_store.go:992` 的 `DeleteOldSnapshots` 没有调用方,`transaction_log` 也没有清理路径。
   - `player_lifecycle.cpp:3364`、`:4284` 进退场无条件发整档,没有开关和采样。
   - 后果:超过约 1MB 的快照静默缺失;积压时未消费的快照被 Kafka 删掉;两张表无限增长。

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

6. **(11-01 已删,待 Codex 重新生成并编译)scene 12 条 GM 回档桩的 proto 删除。** 同时删了只被它们引用的 `SnapshotDiff*`。生成器不清退 `cpp/generated/rpc/service_metadata/player_rollback_service_metadata.h`(`rpc.vcxproj` 的条目也不管),没有编译单元包含它,无害;重新生成之后由 Claude 删掉它和两条工程条目。当时列的文件:`proto/scene/player_rollback.proto`、`proto/message_id.txt`(102、103、104、106、107、109、110、112、113、115、116、117 改 `reserved:`)、`cpp/nodes/scene/handler/rpc/player/player_rollback_handler.{h,cpp}`、`scene.vcxproj`、`.filters`、`CMakeLists.txt`、`player_service_interface.cpp`、`scene_handler.cpp`、`player_gm_guard.h`、`gate_gm_client_messages.h`、`player_inbox_system.cpp`、`cpp/tests/currency_test/client_gm_gate_test.cpp`、`tools/proto_generator/protogen/internal/client_rpc_kind{,_test}.go`。然后重新生成。
7. **gate 上限和新鲜度(L-2),4~6 个文件。** `gatetoken.go:45-51` 的候选没有上限和存活时间,卡死的 gate 会持续吸人,直到租约到期。
8. **连接池指标(L-08),8~12 个文件。** 只有 data_service、db、player_inbox 导出;chat、friend、guild、mail、match、rank、trade、login 和 Java 网关没有。
9. **scene_manager 存活集残缺。** 这一轮只拦了全空,部分缺失还会按消失处理。
10. **压测工具前置,约 4 个文件。** `stress_snap.ps1:76-79` 不抓 9170、9190;summarize 没有匹配段;robot 压测动作没加 JoinQueue;登录回落计数 `login_role_list_home_zone_fallback_total` 还没进 summarize。
11. **`dead_node_reconcile_total{outcome="abandoned"}` 没有告警规则,1 个文件。**
12. **login 死配置,2~3 个文件。** `TaskWaitTimeout`、`LoginTotalTimeout`(`config.go:624-625`)没有读者。
13. **只在混版本时触发,上线前不阻塞。** 字段台账没有机器门禁;结算遇到不认识的币种回 kDefer。

### 待确认

- battle 不上报负载(R6)、评分不分模式(R5):是否已归回合制缺口。
- `-SceneOrchestrator agones` 时 scene_manager 配置不渲染 Agones 段,是否有意。
- 按玩家查询近期战斗终局的 RPC 没找到(`disconnect-reconnect-audit-20261020.md` §9.3),可能归回合制缺口。
- match 投影表退役(R4)要等压测证据。

## 明确不做

按玩家搬库、全局账号目录、跨地域实时对战、实时战斗的 buff 和技能 TODO(负责人:做成实时 MMORPG 之后才改)、NetEase 登录(等接入)、月卡 AI 托管。

## 查过,已经做了或已作废

MessageSyncRedisClient、redis_test、B0-3 全部、reassignSceneNode CAS、match 旧队列兼容、存盘 INFO、B0-4 部署接线、T-7~T-10、T-12、T-16、T-17、G-1~G-3、G-6、G-8、G-11~G-17、G-21、G-25~G-27、L-1、L-3、L-01、L-04、L-05、L-09~L-12、L-16、X-3。聚宝斋旧商品对侧账 TODO 只挂在没有生产调用方的 `EnqueueEscrowDebit` 上。
