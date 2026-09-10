# ID / 寻址改造 —— 会话总结与遗留清单(2026-09-09)

> 这一轮从「scene node id 从 etcd 拿是不是大厂标准」这个问题开始,最终改了两件事:
> **发号**(永久 ID 改号段)和**寻址**(node_id 加代次、控制面改分区)。
> 详细文档见 [node-id-overhaul-plan](./node-id-overhaul-plan-20260908.md) /
> [changelog](./node-id-overhaul-changelog-20260908.md) /
> [qa](./node-id-overhaul-qa-20260908.md) /
> [routing-identity-audit](./routing-identity-audit-20260908.md) /
> [control-plane-topic-partitioning](./control-plane-topic-partitioning-20260908.md)。
> 本文只回答一个问题:**哪些做完了,哪些还没有。**

---

## 一、已完成并验证

### 发号侧
| 项 | 状态 |
|---|---|
| player_id / guild_id / item guid / tx_id / snapshot_id 全部改号段 | ✅ 真 MySQL 集成测试通过 |
| 号段动态 step(Leaf 口径:<15min ×2、>30min ÷2) | ✅ 单测覆盖四种情形 |
| scene 彻底退出 snowflake 槽位协议 | ✅ `tlsSnowflakeManager` 生产调用点为零 |
| 剩余槽位改「最久未用 + 4h 隔离 + 水位 + 2h 自 fence」 | ✅ 真 etcd 48 例 |
| 本地缓存改「故障期才写」 | ✅ 稳态零写盘,编译期不等式断言 |
| 5+12 位切分只在分配器层 | ✅ `shared/snowflake` 只认不透明 17 位 |
| etcd 改 3 副本 StatefulSet + PVC | ✅ 离线渲染断言通过 |

### 寻址侧(审计八条确认缺陷)
| ID | 缺陷 | 状态 |
|---|---|---|
| R13 | TCP 推送无身份 → 错投别人 socket(**含反向面**) | ✅ 加 `target_player_id`,19 例回归 |
| R07 | 结算按位置寻址且发完不管 → 永久丢失 | ✅ 先落库再投递 + 重投时重新解析 |
| R05/R09 | 僵死节点吞掉继任者命令 | ✅ 改 `assign()` 无消费组后消失 |
| R12 | 10 万节点主题数量塌 | ✅ 256 不可变分区,`node_id % 256` |
| R03 | 裸 `entt::entity{node_id}` | ✅ 复查又找出三处一并修 |
| R10 | `BattleRouting` 抓一次不刷新 | ✅ 会话变更时自愈 |
| R06 | Kafka 跑 emptyDir | ✅ 改 StatefulSet + PVC |
| R11 | 命令主题继承短保留期 | ✅ 显式 1h + 创建后回读校验 |

### 顺带修掉的既有 bug
- Kafka 的 emptyDir **挂错路径**(`/tmp/kafka-logs` vs 镜像写 `/tmp/kraft-combined-logs`),那个卷从来没被用过
- `db_task` 保留期在 15min/24h 间横跳(login 与 db 传值不同,谁后启动听谁的)——玩家存盘通路
- 探针 OOM(脚本继承 1G 堆参数)
- LeaseGrant RPC 失败会永久卡住注册
- scene 的 buff/skill 在 node_id 分配前种成 0
- `.gitmodules` 的 boost 地址指向上游,而指针是 fork 独有 → clone 必失败

### 验证口径
C++ 全量编译 0 错误;四套测试 202 例全过(bag 143 / snowflake 27 / kafka 命令 13 / 路由身份 19);
Go 各模块编译测试全绿,真 etcd 48 例、真 MySQL 12 例;部署侧离线渲染断言全过。

---

## 二、待运维执行(代码已就绪,需人工时机)

| # | 动作 | 前置条件 | 不做的后果 |
|---|---|---|---|
| 1 | 翻 `DisableLegacyPerNodeTopic` → true | 新版本 gate/scene 滚更完成、旧主题写入与积压归零 | 旧的消费路径一直挂着(**生产者侧前置条件已满足**,全仓无旧主题生产者) |
| 2 | R13 兼容位转必填 | 灰度一个版本 | 空 `player_id` 一直放行,栅栏等于没加。代码注释已写明「删掉 `kDeliverUnfenced`,0 直接归 kDrop」 |
| 3 | 首次切 Kafka 到 PVC 后重跑 `infra-kafka-topics` | 选无人时段 | 生产者自动建成 1 分区,gate/scene 的分区契约校验拒绝启动 |
| 4 | 恢复全局库前核对 `id_segment.max_id ≥ 消费侧最大号 + 1` | 每次恢复备份 | 号段计数器回退 → 重发已用的号 → `ON DUPLICATE KEY UPDATE` 静默串档 |

---

## 三、未做 / 阻塞

### 3.1 真集群验证(最大的缺口)
**所有故障注入都没跑过**:冻结进程再解冻、`etcdctl lease revoke`、杀 broker、强删 Pod。
目前只有本机真 etcd / 真 MySQL 的集成测试,以及决策函数的单测。
Kafka 与 Redis 的**实际投递链路**(重投、去重、分区指派)只覆盖了决策函数,没有端到端。

### 3.2 审计未覆盖(两路 agent 被 403 中断)
- **UC2 会话反查完整路径**:`session_id` 位宽、`node_id > 32767` 时 15 位 node 段的 LOG_FATAL 边界、
  seq 回绕后已发给其它服务的 id 的命运。只由 R13/R14 部分覆盖。
- **UC4 规模面**:routing 分配器在 10 万节点下的 etcd 负载、每节点 `ServiceNodeList` 快照体积、
  冷启动 CAS 争用、17 位 id 空间对 10 万节点 + 每日 churn 的余量。
  定性结论是「先塌的是 Kafka 主题数量」(已修),其次才是 etcd watch 扇出。

### 3.3 子模块里的编译修复(需你决定)
这些改动**让当前编译能过**,但都在子模块工作树里没提交:

| 子模块 | 改动 | remote | 能否提交 |
|---|---|---|---|
| `librdkafka` | 8 个 vcxproj 加 `/utf-8` + BOM | confluentinc(上游) | ❌ 推不了。要么 fork 到自有账号改 `.gitmodules`,要么在超级工程存 patch 文件由构建步骤打上 |
| `ue5navmesh` | `DetourNavMeshBuilder.cpp` 补 `#include <cassert>` | luyuancpp(自有) | ⚠️ 可以,但本次推送认证失败(非交互式取不到凭据),需要你先认证 |
| `redis` | `deps/hiredis/.build_ninja_release/` | redis(上游) | 无需处理,是编译产物 |
| `cppcodec` | 曾被清空 | tplgy(上游) | ✅ 本次已恢复 |

**风险**:换一台机器 clone 下来,librdkafka 缺 `/utf-8`、ue5navmesh 缺 `<cassert>`,**编译过不去**。
这是本轮最需要你拍板的一条。

### 3.4 已知但未处理
- `dev_tools.ps1` 不转发 `-ClusterId` 与 `infra-kafka-topics`(只影响 cluster≠0 与手动重跑)
- guild / friend / chat 没有 k8s ConfigMap 与清单(既有缺口);guild 需要 `ClusterId`、`SnowflakeCacheDir`、`DataServiceRpc`、`IdSegment`
- `image: apache/kafka:latest` 是浮动 tag 且无 `imagePullPolicy` —— 配持久化日志目录后是版本漂移风险,生产应钉版本
- C++ 侧没有 `TopicGeneration` 等价物,审计主题升代际要**手工同步两个常量**(yaml 注释已写明)
- proto2mysql 对**已存在**的表只 ADD/MODIFY 列、从不建索引,线上旧表仍依赖 `ensureIndexes` 补建
- `SendBindSessionToGate` 的 `sessionVersion` 参数从未被传输(`GateCommand` 无该字段)

### 3.5 可选(你未拍板)
- `ClusterId` 改为每套 etcd 自存 `/snowflake/cluster_id`,服务配置里一个字都没有
- Go 的 `FenceClock` 是否也从 `snowflake.Node` 挪到包装层(代价:match 与 scene_manager 换包装类型)

---

## 四、这一轮验证过的判断(别再重新推导)

1. **etcd 没选错**。从协调器拿 worker id 是标准做法(Twitter/ZK、Leaf/ZK、UidGenerator/DB)。
   坏的是拿到号之后:最小空闲位回收、硬依赖、拿 lease 当互斥。
2. **寻址不加隔离期是对的**。寻址只要求「同一时刻不重复」,重启换号无所谓;
   发号才要求跨重启唯一。寻址的正解是**代次校验**,不是隔离期——加隔离期只会提前耗尽 id 池。
3. **全国同服 ≠ 一个全球集群**。王者是几百个小区、PVP 跨小区;支撑「一起玩」的是
   全局账号层 + 全局撮合 + 全局战斗池,不是全局发号器。所以控制面**不能**按小区切 Kafka
   (全局池的 battle/match 要触达任意 gate/scene),而要改分区。
4. **各类型 GUID 数值可重合是既定语义**(用户确认)。评审若报成缺陷直接驳回。
5. **号段客户端不持久化游标是对的**。段在 `AllocateIdSegment` 提交时已被库判定用掉,
   崩溃只出空洞不出重号。为省几个号而每发一号落一次盘才是错的。
6. **本地缓存不能换 Redis**。Redis 是另一个协调器,崩了就没了;缓存唯一意义是
   etcd 不通还能起,只能靠本机磁盘。

---

## 五、git 现状

- 分支:**只剩 `main`**(本地与远端各一个),特性分支已删
- 本轮工作在 `3ac7d2f42`(号段 + 寻址 + 合服),已在远端历史
- 本地领先远端若干提交(含 boost 升级、`.gitmodules` 修正、本文与索引补全),**尚未推送**
  —— 推送需要 GitHub 认证,非交互式会失败
- 工作树剩余:`librdkafka` / `ue5navmesh` 的编译修复(见 §3.3)、`redis` 的编译产物
