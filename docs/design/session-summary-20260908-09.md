# 本轮会话总览与遗留清单（2026-09-08 ~ 09-09）

> 这一轮从「scene node id 从 etcd 拿是不是大厂标准」一个提问开始，最后落成三条工作线的落码 + 一次合并推送。
> 本文按「做完了什么 / 没做完什么 / 为什么没做完」组织，供交接与排后续。
>
> 详细文档：
> - [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) —— 发号改造方案（§7.5 是最终口径）
> - [node-id-overhaul-qa-20260908.md](./node-id-overhaul-qa-20260908.md) —— 问答全记录
> - [node-id-overhaul-changelog-20260908.md](./node-id-overhaul-changelog-20260908.md) —— 逐文件改动
> - [merge-zone-overhaul-20260908.md](./merge-zone-overhaul-20260908.md) —— 合服整改全细节
> - [routing-identity-audit-20260908.md](./routing-identity-audit-20260908.md) —— 寻址身份审计
> - [../ops/merge-zone-runbook.md](../ops/merge-zone-runbook.md) —— 已按真实实现重写

---

## 1. 提交状况

| 提交 | 内容 | 推送 |
|---|---|---|
| `3ac7d2f42` | 永久 ID 改号段 + 寻址身份加代次 + 合服链路整改（333 文件） | ✅ 已推 |
| `86d9a157f` | 合并队友的宠物提交 | ✅ 已推 |
| `581ef327f` | 补齐宠物功能缺失的两套生成产物（227 文件） | ❌ **未推** |
| `aa45f41e5` | AGENTS.md §11.6（另一个会话） | ❌ 未推 |
| `3b88b694c` | boost 升级到 1.87.0 | ❌ **未推** |

**远端 main 仍停在 `86d9a157f`，落后本地 3 个提交，且当前是编译不过的状态。**

推送卡在账号权限，不是代码：

```
remote: Permission to luyuan-cpp/xuanming-server-mmo.git denied to luyuan-go.
```

凭据管理器拿的是 `luyuan-go`，仓库属于 `luyuan-cpp`。需要你本人执行：

```bash
cd D:\luyuan\wuxingqitan\mmorpg && git push origin main
```

如果仍默认用错账号，先清缓存：`cmdkey /delete:LegacyGeneric:target=git:https://github.com`

---

## 2. 三条工作线，做完了什么

### 2.1 发号（永久 ID 改号段）

问题：node_id 兼任 snowflake 的 17 位 worker，而它按「最小空闲位」立刻复用 —— 释放出来的槽恰是前任最可能还活着的那个（冻结 / GC 停顿 / 网络分区），跨重启撞号。

- player_id / guild_id / item guid / tx_id / snapshot_id **全部改走号段**（`data_service.AllocateIdSegment`，表 `id_segment`，值域 `[1, 2^55)`）。号段无 worker、无时钟、无 lease；崩溃只出空洞不出重号。
- 与存量 snowflake 号**值域不相交**，新旧号同表共存，**不迁一条数据**。
- 动态 step（Leaf 口径）：<15min 用完 ×2、>30min ÷2，`[MinStep, MaxStep]` 钉住。
- scene **彻底退出** snowflake 槽位协议；槽位协议只剩 Go 四种服务。
- 剩余槽位协议改「最久未用 + 4h 隔离期 + 持久水位 + 2h 自 fence」；lease 只做活性，失租重挂不自杀；etcd 不通可用本地缓存启动。
- 5+12 位集群/槽切分**只在分配器层**拼 worker 值，snowflake 类不知道集群的存在。
- etcd 改 3 副本 StatefulSet + PVC。

### 2.2 寻址（node_id 回归纯寻址）

- 控制面 Kafka 主题改**固定分区**（`partition = node_id % 256`），消费端直接 `assign()`，无消费组、无重平衡 —— 不再按节点数膨胀。10 万节点先塌的是主题数量，这条是为那个规模准备的。
- 推送面加**会话身份栅栏**（此前 scene→gate→client 只认 session_id，会错投别人 socket）；反向面（gate→scene）补 `player_id` 校验。
- 结算改**先落库再投递**，未销账每 10s ×12 轮重新解析目标重投（此前按位置寻址且发完不管，目标 scene 换进程即永久丢失）。

### 2.3 合服

**三条会真正丢数据或锁死玩家的：**

1. **玩家主数据仍在 `zone_N_db`**（TiDB Phase 2 未做），合服原来根本没搬 —— 玩家会以空号进目标区并把空数据写死。现在 remap 前强制逐表拷贝、主键重复即中止、校验行数、失效缓存，前置 Kafka lag 与重试队列门禁。
2. **生产代码从未写过 `player:zone` 映射**，合服在真实区上是**静默 no-op**。现在建角 fail-closed 注册，工具加 `-backfill-home-zone`，收集为空即拒绝。
3. **`RemapHomeZoneForMerge` 按值匹配且无鉴权**，文档的 R2 回滚会把目标区原住民一起搬走。已删该步骤，RPC 加 `AdminToken`（空 = 禁用）+ 必须有 fence 标记。

**两条从未生效过的：**

- 合服通知写 mapping 库、login 从自己的库读 —— **上线起一次都没触发过**。
- 审计唯一的阻断门禁在错误的库上查在线键，恒「全部离线 ✅」。
- `-verify-merged` 只改了日志标题，四条断言一条没实现。

**关键订正**：go-zero 的 `RedisConf` **没有 DB 字段**，yaml 里 `DB: 15` 被静默忽略，mapping Redis 恒为 DB 0。此前「服务是 15、工具默认 0 所以扫错库」的结论是**反的**。

---

## 3. 顺带修掉的既有缺陷

- login 把 `EnterScene` 的 `ErrorCode≠0` 当成功 → 玩家卡死且重试撞 `kLoginSessionNotFound`。
- k8s scene-manager ConfigMap 缺 `ZoneId` → 所有 zone 都注册成 zone 1。
- node ConfigMap 发的 `GuidSegment` 键名与 C++ 读的 `IdSegments` 对不上，**形状不对是静默失败**（kinds 为空、门禁恒真、玩家进得去铸不出号）。
- `tx_id` / `snapshot_id` 两个 Kafka 主题全仓无消费者且未注册，回滚审计没落地。
- data_service 只注册 go-zero 的 key，C++ 发现不到 → scene 等不到号段。
- `robot/logic/handler` 与 robot 根包**在 HEAD 上就编不过**（`SignalBattleAssigned` 调用点存在但实现从未提交）。
- 回滚/offset-reset 工具的 topic 默认值 `db_task_topic` **全仓不存在**，回滚第 5 步会「成功」重置一个空 topic。
- 队友的宠物提交漏了**两个**生成器的产物（协议生成器 + 导表器），main 上 C++ 与 robot 都编译不过。
- `third_party/curl` 的 gitdir 是搬目录留下的绝对旧路径，导致任何 `git add -A` 直接 fatal 中断。

---

## 4. 验证状况

| 项 | 结果 |
|---|---|
| C++ 全量 `msbuild game.sln Debug\|x64` | **0 errors**（boost 1.87.0 上跑通） |
| C++ 测试 | bag_test 143 / currency_test 24 / snow_flake_test 24，全过 |
| Go 全部模块 build + vet + test | 十个模块零失败 |
| 集成测试 | 真 etcd（snowflakealloc 34 例）、真 MySQL（data_service、guild）全过 |
| PowerShell | 四个脚本解析零错误 |
| k8s 渲染 | 干跑断言全过；27 项 Pester 契约测试全过 |
| 跨语言一致性 | 15 例选号向量 C++ 与 Go 逐一相符 |

---

## 5. 没做完的（按紧急度）

### 5.1 必须做：推送

见第 1 节。远端 main 编译不过，三个提交压在本地。

### 5.2 上线前必须处理

| # | 事项 | 后果 |
|---|---|---|
| A1 | **Unity 客户端不响应 `RedirectToGate`（msg 124）** | 任何服务端重定向都是把玩家卡死。`HomeZone.RedirectOnEnterEnabled` 生产**必须保持 false**。robot 已实现完整流程可作参考 |
| A2 | **C++ Kafka 审计主题代际后缀是编译期常量** | `transaction_log_topic_g1` / `player_snapshot_topic_g1` 写死在两个 `.h` 里，Go 侧是运行时从 `Kafka.TopicGeneration` 拼的。换代要人工同时改两处并重打镜像，漏一边 = 生产者写进没人消费的 topic，且不报错 |
| A3 | **真集群合服演练一次都没跑过** | 本轮全部验证都是本机真 MySQL/Redis + 单元与集成测试，**没有停服窗口的实战**。运维手册已按真实命令重写，上线前应照它走一次完整演练 |

### 5.3 子模块里的编译修复：**结构性提交不了**

工作树里三个子模块有未提交改动，都是给 MSVC 加 `/utf-8`（中文注释必需）：

| 子模块 | 上游 | 改动 | 能否提交 |
|---|---|---|---|
| `librdkafka` | `confluentinc/librdkafka`（**上游**） | 9 个 vcxproj 加 `/utf-8` + BOM，+39/−10 | ❌ **不能**。主仓只存 commit 指针，改动在上游仓库里；推不上去 |
| `ue5navmesh` | `luyuancpp/ue5navmesh`（**你的 fork**） | `Navmesh.vcxproj` 加 `/utf-8`；`DetourNavMeshBuilder.cpp` 补 `#include <cassert>`（另有整文件行尾变更） | ⚠️ 可以，但要**先在 fork 里提交并推送**，再回主仓 bump 指针。指针指向一个没推上去的 commit 会让别人 clone 后拉不到 |
| `cppcodec` | `tplgy/cppcodec`（上游） | 工作树被清空（0 文件、49 条 staged 删除） | ❌ 这是**损坏状态**不是改动，见 5.4 |

**风险**：这些 `/utf-8` 修复是**未提交的工作树改动**。任何人跑 `git submodule update`、重新 clone、或换机器，**全部丢失**，而且丢了之后是编译错误，不是静默问题。

**三个可选出路**：

1. **打成补丁进主仓**（推荐，成本最低）：把 diff 存成 `third_party/patches/*.patch`，在构建脚本里 `git apply`。可版本化、可审阅、`submodule update` 不会冲掉。
2. **fork librdkafka**：改 `.gitmodules` 指向自己的 fork，在 fork 里提交。彻底但多一个要维护的仓库。
3. **就地留着**：现状。换机器就重来一遍，不推荐。

`ue5navmesh` 因为已经是自己的 fork，走「提交 + 推 fork + bump 指针」是最正规的，但要注意那个文件的整体行尾变更会让 diff 很吵，提交前先确认是有意为之。

### 5.4 third_party 的损坏状态（搬目录遗留）

仓库从 `D:/luyuan/mmorpg` 搬到 `D:/luyuan/wuxingqitan/mmorpg` 时留下的：

| 子模块 | 状态 | 处理 |
|---|---|---|
| `curl` | gitdir 指向已不存在的绝对旧路径；工作树空（56M 对象但从未检出） | ✅ **本轮已修** gitdir 与 `core.worktree`（改成相对路径，与 boost 一致）。**注意这个修复不需要提交**——那两个文件都不被 git 跟踪，纯本地管道。工作树仍是空的，构建不需要它 |
| `cppcodec` | 工作树 0 文件、49 条 staged 删除 | ❌ 未处理。需要 `git submodule update --init third_party/cppcodec` 重新检出（要联网） |
| `redis` | `.gitmodules` 里有声明，但目录是**未跟踪**的完整源码（gitlink 丢了） | ❌ 未处理。需要判断是重新 init 还是清掉 |

**建议一次性修完**：

```bash
cd D:\luyuan\wuxingqitan\mmorpg
git submodule update --init --recursive third_party/cppcodec third_party/curl
# redis 要先确认那份未跟踪源码是否有本地改动，再决定 init 还是删
```

### 5.5 更早记下、本轮未动的

- `docs/design/zone_data_rollback.md` 里描述的回滚流程仍按旧 topic 名 `db_task_topic` 写，本轮只修了脚本没修文档。
- `go/db` 在本机编不过：`go.mod` 里 proto2mysql 的 replace 指向不存在的目录。既有问题，与代码无关。
- `go/guild/internal/node` 的 `TestAllocDifferentTypeReuse` 在 integration 标签下红：旧 schema 分支扫 NodeInfo 时不按 node_type 过滤。看着是测试脚手架问题，生产节点类型前缀各不相同。
- `robot` 里队友的 `pet_smoke_scenario.go` 依赖的宠物 tip 码本轮已由导表器生成；但宠物功能本身标注「开发中」，实际联调未做。

---

## 6. 这一轮值得记住的方法论

- **静默失败是主要敌人**：合服的失败模式几乎全是静默的 —— 写错 Redis 库、ConfigMap 键名对不上、收集到空集合、重置一个不存在的 topic，每一条都报告成功。整改的核心动作就是把它们逐条改成「要么正确、要么明确拒绝」。
- **配置结构体没有的字段，yaml 里写了也是空气**：go-zero 的 `RedisConf` 没有 `DB`，`DB: 15` 被静默忽略。任何「服务 yaml 里的某某配置」都要先确认那个结构体真的有这个字段。
- **改契约值时 grep 源码符号不够**，必须同时查配表数据里有没有裸值（tip 码轴改造那次的教训，本轮沿用）。
- **灰度兼容是双向的**：新二进制避开旧的不够，旧二进制看不见新的也会撞。
- **别人报的绿要自己复核**，尤其「我已补齐 X」这类。
- **生成物必须与源文件同一个提交**：队友的宠物提交只交了 `.proto` 和 `.xlsx`，漏了两个生成器的产物，直接让 main 编译不过。
