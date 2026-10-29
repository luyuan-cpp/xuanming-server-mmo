# 玄冥移植:开工前决策记录(2026-09-10)

> **2026-10-15 现行范围入口：本文末 D-15、D-16、D-17。** friend / chat / guild / mail / leaderboard / login / auction / trade 八域与区域优先、等待扩区的全球匹配继续；最新用户决定采用 mall 适配聚宝斋、游戏买卖和拍卖，优先直接复用。D-17 覆盖此前“商城仅比较”的边界。目标 DAU 1 亿 / CCU 2000 万，独立客户端同步已授权；逐域比较并选择更可扩展实现，正确性/一致性/兼容不降级。其余排除项与证据门禁保持。

**状态**:已定。本文替代 `handoff-tip-axis-and-port-20260903.md` §2.2 的「11 条待拍板」。
**前置**:`xuanming-port-feasibility-20260902.md`(三桶分法与 ADR 全集,仍然有效)。
**口径**:凡与本文冲突的旧文档,以本文为准。

> 决策原则:**取标准做法**。目标系统(B)已有的形态优先于源系统(A)的形态;
> 有持久化数据的编码一律不改;有书面权衡的既有决策不推翻;可见产出排在基础设施前面。

---

## D-1 目标定义:**「在 B 上跑起来」**

移植的验收物是**可运行的软件**,不是代码行数。每一批的完成判据固定为三条,缺一不算完成:

1. 进程起得来(进 `go_services.ps1` 的 `$ServiceCatalogue`,本地一键栈能拉起)
2. 客户端调得到(路由表 `ClientProtocol=true`,gate 不拒绝,Unity 侧有 handler)
3. 有一条冒烟(至少一个 end-to-end 用例,不是单测)

**代价**:总量比「代码进仓库」多约 10%。**换来的**:每批都可验证,不会攒到最后才发现接不上。
本仓已有四个反例——`friend`/`guild`/`inventory`/`mission` 都是「代码在、测试绿、跑不起来」,
正是因为没有这条判据。

---

## D-2 zone / global 归属:**按数据归属定,不按服务定**

判据:**表里有没有跨 zone 的行**。有 → 全局池;只服务本 zone → zone-scoped。

| 服务 | 判据 | k8s `Global` |
|---|---|---|
| `friend` | `friend` / `friend_request` 表**无 `zone_id` 列**(`deploy/mysql-init/guild_friend_tables.sql:44-59`),好友关系本来就跨 zone | **`$true`** |
| `guild` | `guild` 表虽有 `zone_id`(:14),但 `UNIQUE KEY uk_name (name)` 是**全局唯一**(:17)⇒ 是单张共享表,`zone_id` 是**数据维度不是部署维度** | **`$true`** |
| `chat` | 频道跨 zone | **`$true`** |
| `team` | 权威数据在全服单实例 SharedRedis,`zone_id` 是记录里的**数据维度不是部署维度**(同 guild);`AllowCrossZone=true` 时同一队伍跨 zone。部署上寄宿在 `match` 进程([xuanming-port-feasibility-20260902.md](xuanming-port-feasibility-20260902.md) §8 D7,不是本文的 D-7),与本判据无关 | **`$true`**(部署随 match 镜像) |
| `mail` | 收件人跨 zone | **`$true`** |

> **2026-10-12 修订 team 行**:原文为「按 §D7 与 `match` 同进程,`match` 已是 `Global = $true` | **`$true`**(随 match)」。
> 那是按进程归属推出的结论，没有走本节"按数据归属定、不按服务定"的判据;改为按数据论证后结论不变。
> 这样 D-2 不再依赖 D7:将来 team 拆出独立服务，本行也不用改。复核依据见
> [team-system.md](team-system.md)「定案理由复核与扩展路径(2026-10-12)」。

### ⚠ 两个轴不要混

- **`Global = $true`**(在 `tools/scripts/k8s_deploy.ps1` 的 `$GoSvcCatalogue`)= **部署维度**:
  只在 infra 阶段部署一份,zone-up 跳过。
- **`NodeUtils::IsGlobalPoolNodeType`**(`cpp/libs/engine/core/node/system/node/node_util.cpp:127`)
  = **路由维度**:`PickRandomNode` 对这类不比对 zone。

**新增五个服务要前者,不要后者。** 依据是 B 自己的头注释
(`node_util.h:113-114`):

> 只列明确的全局池类型,不扩大到其他非 zone-scoped 服务(**friend/guild/chat 的路由语义保持原样**)

⇒ `node_util.cpp` 的两个 switch **一行都不改**。friend/guild/chat/team/mail 既不进
`IsZoneScopedNodeType`(那里只有 Gate/Scene/Login/PlayerLocator),也不进 `IsGlobalPoolNodeType`
(那里只有 Match / Battle / ClientRpcRouter 三类)。

### 四处登记(一次性全做,不要分批)

| 文件 | 加什么 |
|---|---|
| `tools/scripts/go_services.ps1` `$ServiceCatalogue` | `Dir` / `Entry` / `Port` / `Desc` / `ConfigFlag` / `ConfigFile` / `AllowMultiInstance` / `Tier`。**无 `Global` 字段**,别加 |
| `tools/scripts/go_svc_image.ps1` `$Catalogue` | `Dir` / `Entry` / `ImageName`(必须与下一行的 `ImageName` 逐字一致,否则 infra-up 拉不到镜像) |
| `tools/scripts/k8s_deploy.ps1` `$GoSvcCatalogue` | `ConfigMap` / `Manifest` / `Port` / `ConfigFlag` / `ConfigFile` / `ImageName` / **`Global = $true`** |
| `deploy/k8s/manifests/go-svc/` | 每服务一个 yaml |

**端口**:必须避开已观测的 Windows 保留区(49728-49927 / 50000-50171 / 51573-51872)。
`match`=50500、`client_rpc_router`=50600 已占;新服务从 **50700** 起按 100 递增。

---

## D-3 反向移植禁令:**成立,写进 AGENTS.md**

A 的 `pkg/` 里有四件**文件头自陈「抽自 mmorpg」**,它们是 B 的旧副本:

| A 侧文件 | 自陈行 |
|---|---|
| `pkg/cache/cache.go` | :4 |
| `pkg/redislock/redislock.go` | :3 |
| `pkg/kafkax/consumer.go` | :3 |
| `pkg/configtable/manifest.go` | :3 |

**规则**:这四件(及任何新发现的同类)**一律不搬**——搬回去是循环依赖,而且 B 侧版本更新
(例:B 的 `cache` 修了 singleflight defer bug,A 没修)。
**例外**:A 反向修过的具体补丁可以 cherry-pick,但必须逐条说明修的是什么,不许整文件覆盖。

**动任何 `pkg/` 件之前,先看文件头。** 这是本次移植唯一的反向事故风险。

---

## D-4 公会 role 编码:**不动 B 的 `0/1/3`**

| | 编码 | 方向 |
|---|---|---|
| A | `LEADER=1 / OFFICER=2 / MEMBER=3`(`proto/pandora/guild/v1/guild.proto:45-48`) | 数字越小权越大 |
| **B(保留)** | `member=0 / officer=1 / leader=3`(`go/guild/internal/constants/constants.go:10-13`) | 数字越大权越大,**跳过 2** |

三条理由:

1. **目标系统有持久化数据**。`guild_member.role` 是 `TINYINT UNSIGNED`,改编码 = 数据迁移。
2. **B 自己已经为这事栽过一次并留了判据**。`deploy/mysql-init/guild_friend_tables.sql:22-23`
   原文:「role 取值必须与 `go/guild/internal/constants/constants.go` 一致:0=member, 1=officer,
   3=leader(**旧注释 1/2/3 与代码不符,以代码为准**)」。
3. 改成 A 的编码会让这条已经收敛的判据再次分叉。

### 落地纪律(必须照做,否则权限会静默翻转)

- **禁止直接比较 role 枚举值**。定义 `func rank(role uint8) int`,所有权限判断比 `rank`。
  从 A 搬过来的任何 `role <= OFFICER` 式比较,**逐条重写**,不许机械替换。
- `go/guild/internal/constants/constants_test.go` 加断言钉死三个值
  (`member==0 && officer==1 && leader==3`),防后来者"顺手改成连续的"。
- **跳过 2 是刻意的,保留**。不要"整理"成 0/1/2。

---

## D-5 ~ D-7:三条原以为要拍、实际不用拍的

| # | 旧说法 | 实况 | 决定 |
|---|---|---|---|
| D-5 | `go/shared/go.mod` 加 `go-sql-driver/mysql` + `miniredis` 需授权 | **不是硬卡口**。落点放 `go/db`(已 require mysql 驱动)即绕开;且「会波及 5-8 个 module」已被证伪,实际只波及 friend/guild | 无需授权,换落点 |
| D-6 | `callerauth.go:52-56`「刻意不上 Redis」要不要推翻 | 那段权衡(避免抬登录 P99)今天仍成立,无新证据 | **不推翻**。标准做法:不改有明确书面权衡的既有决策 |
| D-7 | protoc 恰 35.1 是真卡口 | **判据是错的**。仓库自带**两份** protoc,`handoff-tip-axis-and-port-20260903.md` §2.2 第 4 条只看了 31.1 那份 | 已具备,该条作废 |

---

## D-8 批次顺序:**零依赖且用户可见的排前面**

```
第 0 档  ≈8~12 人日   底座(见下)
第 1 档  ≈30~40 人日  ①经验入账 → ②friend 可达化 → ③chat 服务端 → ④D0 背包挂载 → ⑤D1 发物入口
第 2 档  ≈50~70 人日  team / mail / leaderboard / guild 审批流+职位 / friend 黑名单 / push / 小件
```

**第 1 档内部顺序的理由**:①②③ 合计 ≈16 人日、**三条全部零依赖**,做完能看到三个可见变化
(能升级 / 有好友 / 能聊天);④⑤ 合计 18~26 人日且**中间没有任何可见产出**。
把可见的排前面,风险暴露得早,也便于按 D-1 的三条判据逐批验收。

### ⚠ 经验入账不依赖背包(原排期把它压在后面是错的)

`LevelComp` 只有 `uint32 level`(`proto/common/component/actor_comp.proto:70-72`,**全仓无 exp 字段**),
它走 `cpp/libs/services/scene/battle/system/player_battle.cpp:1077` 分支,
背包走 `:1083` —— **两条路**。可以独立开工。

### 第 0 档清单

- `noderegistry` 六份逐字副本收进 `go/shared`(`registry.go:5-13` 自陈六处副本;
  再加五个服务就是十一份)—— 2~3 人日
- **`Tip.xlsx` 批量开段**:`mail` / `chat` / `rank` / `trade` / `dialogue` **全无段**
  (`go/shared/generated/tip/segments.go:29-47` 实测);`team`=4000(18 码已发)/ `mission`=5000 /
  `bag`=6000 / `reward`=12000 已开好 —— 1~2 人日。**不许手写数字,只能由导表器发号**
- `proto2mysql` 扩展号冲突(500001/2/6/11/12 与 B 的 `OptionTableName` 同号同 extendee)消除法钉死 —— 1 人日
- `go_package` / import 根改写模板(A 是 `pandora/` 无 `go_package`,B 是 `proto/` 裸 protoc)—— 每域 0.5
- `MinIDAt` 按 B 的 epoch 补 `GuardEpochSec` 守卫 —— 0.5
- 客户端三语言生成纪律:`gen_proto.ps1` 硬编码清单 + `HandlerRegistry` 覆盖语义 —— 1 人日

---

## D-9 安全前置:**「身份从会话取」必须先于「客户端可达」**

今天 B 的 `friend` / `guild` 每个 Request 都带 `player_id`
(`proto/friend/friend.proto:34-36`),`go/friend/internal/logic/friend_logic.go:30` 直接用请求体;
拦截器链(`go/friend/friend.go:105-130`)只有 grpcstats / killswitch / serverbase,**没有会话解码层**。

⇒ **先翻 `ClientProtocol` 再补身份 = 开放伪造**(公会侧后果更重:可冒充会长踢人、转让)。

**顺序固定为**:`ctxkeys` + 拦截器(照抄 `go/match/internal/pkg/ctxkeys`,29 行)
→ 逻辑层改读 `ctxkeys` → proto 加 `option (OptionIsClientProtocolService) = true` → 重跑生成。

会话本来就到得了:`go/client_rpc_router/internal/logic/forwardlogic.go:29` 按 `x-` 前缀
整体透传 `x-session-detail-bin`,`proto/common/base/session.proto:8` 里就有 `player_id`。

### 按方法分级

`NotifyOnline` / `NotifyOffline` 是 **gate→friend 的东西向调用**,
**不能**跟着一起对客户端开放(否则任何客户端可声明任意玩家在线)。
`OptionIsClientProtocolService` 是**服务级**的 —— 这两个方法要么拆到另一个 service,
要么在拦截器里按方法名拒绝客户端来源。**开工时二选一,不要留到接线后再补。**

---

## D-10 B 侧已有实现的处置:**搬不变量,不搬服务**

`friend` / `guild` 不是空目录,它们有 A 没有的东西,**移植时绝不能被覆盖**:

| 服务 | B 独有、必须保留 |
|---|---|
| `guild` | `zone_id` 归属 / 合服闸门 / 双 ZSET 分区榜 / id 分段发号器 |
| `friend` | `friend_capacity` 显式计数锁行 + migration ready gate(`internal/data/friend_repo.go:222-230`、`:59`)/ versioned cache generation(`:518`) |

⇒ 形态固定为 **「搬 A 的不变量,按 B 的形状重写」**,不是整服替换。
任何改动必须连跑 `go/guild/internal/data/rank_zone_integration_test.go`
与 `go/guild/internal/logic/merge_fence_test.go`。

---

## 记账纪律

上一轮清点时,D0 / D1 / inventory / player 四条把**同一批能力数了三遍**
(背包挂载 ×3、幂等发物 ×3、经验 ×3),按行相加得到 50+ 人日,**去重后真实规模 25~35**。

**再做这类清单:先按「能力」去重再加总,不要按服务行相加。**

另外「不搬代码 ≠ 零成本」——明确不做的那些服务,每个仍要计 **0.5 人日**开 tip 段。

---

## 仍然需要人执行的(Claude 不做,见 AGENTS.md §10.1)

1. 全量导表(需 protoc 35.1)、C++ 构建、Java 构建
2. `git add` / `git commit`
3. 本文件是**未跟踪文件**,要不要进仓由你定

---

> 以下三条为 2026-09-14 追加,来源 [microservice-zone-contract-20260914.md](./microservice-zone-contract-20260914.md)
> (Go 微服务接入 zone 契约 v1)。它们推翻或补充了既有决策,按本文「凡冲突以本文为准」的口径生效。
> 相关代码**未编译、未运行**,行为以 Codex 验证结果为准。

## D-11 Go 服务注册的失租口径:**先重夺原 id;抢不回时,无状态全局服务换 id 继续,持久身份服务退出**

(来源:契约 §2)

**结论**

- `go/shared/noderegistry` 的 keepalive 丢租后,一律先用 CAS 重夺原 node_id。allocKey 不存在,或值仍是本进程 uuid,都算重夺成功。
- 原 id 已被别的实例占用时,按 `Spec.OnReclaimFailed` 走:
  - `ReallocateNewID`(默认,chat 用):分配新 id,改写发现键,回调 `OnNodeIDChanged`,进程继续服务。
  - `ExitProcess`(login,以及任何用 NodeId 派生持久身份或 per-node topic `{type}-{id}` 的服务):Revoke 新租约 → 冲日志 → `os.Exit(1)`,交给编排器重启。
- **发现租约与发号器租约分开,不共用 lease。** noderegistry 不提供 `Lost()`;snowflake worker id 丢失所有权,只听 `snowflakealloc.Handle.Lost()`。

**理由**

1. 对 chat 这类无状态全局服务,node_id 只是发现路径里的一个编号。换号只让路由服镜像里的 key 变一下,不碰任何持久身份。为这个退出进程,等于把一次 etcd 抖动放大成一次重启。
2. 真正怕换号的,是「node_id = worker id / topic 名」的服务。这条等式只对 C++ 节点和 login 成立,所以退出策略留给它们,不强加给全体 Go 服务。
3. 两把租约合并后,发现键一抖就会连带 fence 发号器。发号器的正确性不能寄托在发现键上。

**被推翻的旧条目**:`xuanming-port-feasibility-20260902.md` §8「D9 纠正」(:233)与 D9 行(:250)。它们要求第②层(注册重注册)一律按 login / C++ 口径「重夺同 id,否则退出」,不按 friend 的「换 id 继续」。

**前提为何不成立**:那条纠正的依据是「C++ 注册的 node_id 就是 snowflake worker id」,然后推广到全体 Go 服务。但 Go 全局服务的 worker id 由 `shared/snowflakealloc` 独立分配(friend 重注册注释里「换 ID 安全」的理由就是这个),chat v1 更是根本没有发号器(契约 §4)。对它们来说 node_id ≠ worker id,「换号 = 丢身份」不成立。login 仍按原口径(`ExitProcess`),C++ 不动。

**证据**

| 事实 | file:line |
|---|---|
| 两档策略、两把租约分开、不提供 `Lost()`,写在包注释里 | `go/shared/noderegistry/registry.go:20-37`、`:76-82` |
| 发号器失租信号 | `go/shared/snowflakealloc/allocator.go:682`(`func (h *Handle) Lost()`) |
| friend 既有副本就是换 id 继续,理由是 worker id 独立分配 | `go/friend/internal/node/node.go:223-231`、`:278-287` |
| login 的原 id 被接管就退出;发号器失租也退出 | `go/login/internal/logic/pkg/etcd/registry.go:147-150`;`go/login/login.go:278-281` |
| C++ 重夺条件是「allocKey 不存在,或值是我的 uuid」 | `cpp/libs/engine/core/node/system/etcd/etcd_manager.cpp:103-106` |
| 被推翻条目原文 | `docs/design/xuanming-port-feasibility-20260902.md:233`、`:250` |

**本条不改的**:既有 7 份非 login 副本和 login 的异源实现,本轮都不动。后续把 login 迁到 shared 版时,必须选 `ExitProcess`。

---

## D-12 新 Go 服务的客户端入口:**只承诺路由服模式;「翻转」落在部署层,C++ 默认值不改**

(来源:契约 §1)

**结论**

- chat,以及之后经 gate 暴露给客户端的 Go 业务服务(guild 已按同口径接入),只保证在 `GATE_CLIENT_RPC_ROUTER=1` 下可达,即 gate → client_rpc_router → 服务。直连模式下不可达是设计内的,**不补 gate 直连白名单**。
- C++ 默认值和它的单测一字不改:`gate_router_mode.h` 规定未设即直连,`gate_security_test.cpp` 断言「默认必须落在旧模式」。
- 翻转落在部署层:
  - **本地**:`start_game.ps1 -GateRouterMode`,默认 `'1'`。起 gate 前设 env,脚本结束还原旧值。`dev_tools.ps1 dev-start-zones` / `cpp_nodes.ps1` 由父 shell 设 env。
  - **K8s**:`k8s_deploy.ps1 -GateRouterMode`,默认 `"0"`,只注入 gate Deployment。翻成 1 要同时满足三件事:K8s 上以路由模式跑通过一次 battle-smoke、路由服 manifest 已落地、路由服用 POD_IP 通告。
    - (2026-09-29 已被文末「D-12 修订」取代:K8s 默认改为 `"1"`;前提① battle-smoke 由用户豁免、事后补验,前提②③ 已满足。原文保留作历史。)
- 回退到直连,等于 chat 等只承诺路由模式的服务同时不可达。回退前先用 killswitch 关掉对应方法,并发公告。

**理由**

1. 每加一个服务就改 gate 白名单、重编 gate、滚动重启踢在线玩家,正是 `client-rpc-router.md` §1 要消灭的成本。给 chat 补直连白名单,等于走回旧路。
2. C++ 默认值是灰度开关的安全兜底(拼错宁可留在直连)。改它会连带 battle 等既有直连路径的回归面;部署层参数则可以逐个环境翻,一行就能回退。
   - (2026-09-29 已被文末「D-12 修订」部分取代:前半句「C++ 默认值是安全兜底」仍成立、不改;后半句「battle 等既有直连路径的回归面」在 gate 中继战斗代码删除后(turn-based §22 D66 / D67)不再成立。原文保留作历史。)
3. K8s 路由服 manifest 与 POD_IP 通告已补齐,但尚未在 K8s 上以路由模式跑通 battle-smoke。因此 K8s 默认仍为 0,本地默认仍为 1;按 D34 验证后再切换 K8s 默认值。
   - (2026-09-29 已被文末「D-12 修订」取代,turn-based §22 D65 / D75:K8s 默认改为 `"1"`,battle-smoke 前提由用户豁免、事后补验。原文保留作历史。)

**补充的旧条目**:`client-rpc-router.md` D34(:31):gate 双模式,默认旧模式,默认值在冒烟通过后翻转,再删旧路径。

**前提为何不再完整**:D34 写作时,所有客户端可达的服务在两种模式下都可达,「翻转」可以理解成一次性修改 C++ 默认值。chat(以及 guild)出现后,有了**只在路由模式下可达**的服务:翻转不再是可选的清理,而是这些服务的上线前提。本地和 K8s 的就绪程度又不同,只能在部署层按环境分别翻。D34 的「默认旧模式、C++ 默认值不动」保留;「何时删旧路径」仍待拍板。

**证据**

| 事实 | file:line |
|---|---|
| 直连白名单里没有 Chat;路由模式白名单只有 Scene + ClientRpcRouter | `cpp/nodes/gate/main.cpp:207-209` |
| C++ 默认关,只认 `1` / `true` / `on` | `cpp/nodes/gate/gate_router_mode.h:31-39`、`:51-55` |
| 单测钉死默认旧模式 | `cpp/nodes/gate/tests/gate_security_test.cpp:102-105` |
| 启动日志打「出口模式=router/direct」,这两个词被单测钉死 | `cpp/nodes/gate/main.cpp:225`;`cpp/nodes/gate/tests/gate_security_test.cpp:170-172` |
| 本地参数、设置 env、还原旧值 | `tools/scripts/start_game.ps1:20`、`:33`、`:397-403`、`:451` |
| K8s 参数与注入 gate env | `tools/scripts/k8s_deploy.ps1:120-135`、`:806-812` |
| chat 两条路由表项 `ClientProtocol: true` | `go/client_rpc_router/generated/pb/game/route_table.go:39-40` |
| 路由服注册地址直接取 ListenOn 的 host | `go/client_rpc_router/client_rpc_router_service.go:69-70` |
| D34 原文 | `docs/design/client-rpc-router.md:31` |

---

## D-13 全局服务 yaml 不注册 go-zero 发现键:**有 `Etcd` 段就显式写 `Key: ""`,不写 `<svc>.rpc`;等首个 Go 调用方出现再同批拍 `.z<N>` 豁免**

(来源:契约 §7 yaml 锚点;措辞按 go-zero 源码核对结果修正)

**结论**

- 全局 Go 服务(`$GoSvcCatalogue` 里 `Global = $true`,chat 起)的 zrpc 服务端 `Etcd` 段,只为 noderegistry / killswitch 提供 `Hosts`,`Key` 显式留空。
- **不能整行省略 `Key`**:go-zero v1.10.0 的 `discov.EtcdConf.Key` 没有 optional 标签,`Etcd` 段存在而缺 `Key` 时,`conf.MustLoad` 直接 Fatal(`"Etcd.Key" is not set`)。
- yaml 里根本没有 zrpc 顶层 `Etcd` 段的服务(如 guild 用自定义的 `Registry.Etcd`),不写即可。
- 服务自己的 `config.Validate` 应拒绝非空的 `Etcd.Key`(chat 已做)。
- 不声明任何指向全局服务的 `RpcClient.Etcd.Key`。首个需要经 go-zero 发现某个全局服务的 Go 调用方出现时,同批拍板两件事:该 key 叫什么;`go_services.ps1 -Zone` 如何对它豁免 `.z<N>` 后缀。

**理由**

1. 全局服务靠 C++ 约定的 `<ENodeType名>.rpc/zone/...` NodeInfo 被 gate / 路由服发现。go-zero 键此时没有任何消费者,只是多一条没人读的注册。
2. 本地多 zone 启动时,`go_services.ps1 -Zone` 会给**所有** `Key: <id>.rpc` 行加 `.z<N>`,把一个全局服务切成按 zone 隔离的负载均衡组。这和「全局一份」正相反,而且出错时完全静默。
3. `Key` 为空串时 `RpcServerConf.HasEtcd()` 返回 false,go-zero 不发布键;空串也匹配不上 `-Zone` 的正则,不会被改写。
4. 没有调用方就先拍 `.z<N>` 豁免规则,是在为假想需求做设计(AGENTS.md §11.2 YAGNI)。

**被修正的旧口径**:goctl 模板和既有 zone 内服务一律写 `Etcd.Key: <svc>.rpc`(例:`go/client_rpc_router/etc/client_rpc_router.yaml:9`),`go_services.ps1 -Zone` 的正则则按「每个 Go 服务都按 zone 部署」一刀切加后缀。

**前提为何不成立**:D-2 之后,friend / guild / chat / team / mail 进了全局池,不再每 zone 一份。对它们来说,「每个 key 按 zone 隔离」这个前提本身就是错的。另外,契约草案 §7 原写「不写 Key」,核对 go-zero 源码后改为「显式留空」(K8s ConfigMap 如果按「不写」生成,Pod 会 CrashLoop),以本条为准。

**证据**

| 事实 | file:line |
|---|---|
| `RpcServerConf.Etcd` 整段是 optional,但段内 `Key` 不是 optional | go-zero v1.10.0 `zrpc/config.go:41`;`core/discov/config.go:15` |
| 段存在时,缺非 optional 标量字段即报错 | go-zero v1.10.0 `core/mapping/unmarshaler.go:966-969` |
| `HasEtcd()` 要求 Hosts 和 Key 都非空;为 false 时不发布 | go-zero v1.10.0 `zrpc/config.go:78-80`、`zrpc/server.go:42-43` |
| `-Zone` 给所有 `.rpc` key 加 `.z<N>` | `tools/scripts/go_services.ps1:360-370` |
| chat 本地 yaml 显式写 `Key: ""` | `go/chat/etc/chat.yaml:14-21` |
| chat 的 Validate 拒绝非空 Key | `go/chat/internal/config/config.go:119-120` |
| K8s ConfigMap 同样写 `Key: ""` | `tools/scripts/k8s_deploy.ps1:1898-1907` |
| guild yaml 没有 zrpc 顶层 Etcd 段,发现走自定义 `Registry.Etcd` | `go/guild/etc/guild.yaml:6-8`、`:44-48` |

**遗留**:路由服也是全局池。它的 K8s ConfigMap 已按本条写 `Key: ""`(`k8s_deploy.ps1:1957-1964`),但本地 `go/client_rpc_router/etc/client_rpc_router.yaml:9` 仍是 `Key: client_rpc_router.rpc`。本批未改,留待路由服迁 shared 版 noderegistry 时一并处理。

---

## D-14 新全局服务的库归属与建表方式:**每服务一库;表以 proto 为源;迁移用 go/db runner 的语义,抽成独立 module,由服务二进制 `-migrate` 在 K8s Job 里跑**

(来源:契约 §4「首个建表服务开工前先拍 D4」,聚宝斋 J-6。注意:本条与可行性文档 §8 表里的 D14「mission 事实源」不是同一条。)
(2026-09-14 本条先落决策;2026-09-15 随首个建表服务 trade 落地 `go/schemamigrate`、runner 缺陷修复与 migrate Job。2026-09-16 已完成迁移模块、trade 迁移入口及隔离 MySQL / kind 发布门禁验证,结果见 PROGRESS 同日 D-14 条目;聚宝斋 P1 全链路与 TiDB 实例验收另计。)

**结论**

1. **库归属**
   - 每个要建表的新全局服务(mail、trade 起)独占一个逻辑库 `mmorpg_<svc>`,本地和 K8s 同名。
   - 不落 `zone_{N}_db`,不借 data_service 的 `mmorpg_global`,也不往共享库 `mmorpg` 加表。
2. **表结构以 proto 为唯一事实源**
   - 每张表对应一个 message,`OptionTableName` 锁定表名,启动和迁移时都要过表名守卫。
   - TiDB 方言只用 proto 选项 500021-500024 表达,业务代码不手写 `/*T!*/`。
   - 列形状受 proto2mysql 表达力约束:
     - 主键只用整数列:id_segment 或 snowflake 发的 `uint64`,或由整数、枚举列组成的复合键。**禁止 string/bytes 主键。**
       (2026-09-28 补:proto2mysql `v0.2.0` 起库本身已支持 string/bytes 主键,放宽条件见第 7 条末项已满足;但**本条对新建表服务仍然有效**——`go/schemamigrate/plan.go` 照旧拒绝非整数主键。要放宽须另行拍板,并同批改 plan.go 与其测试、评估 TiDB NONCLUSTERED/SHARD 方案。)
     - ~~string 列可以进索引或唯一键,但应用层必须限长 ≤191 字符,因为库只按 191 前缀建索引。~~ —— 2026-09-28 随 `v0.2.0` 修订:
       主键/唯一键里的 string/bytes 列建成 `VARCHAR(N) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL DEFAULT ''` /
       `VARBINARY(N) NOT NULL DEFAULT ''` **整列索引**,N 默认 191,可用 proto2mysql 的 `max_length`(字段号 600101)调
       (string 1..768、bytes 1..3072,单索引合计 ≤3072 字节)。比较**区分大小写和尾部空格**。经 proto2mysql 写入时超长或非法
       UTF-8 返回 `ErrInvalidKeyValue`(不截断);手写 SQL 写入在严格模式下报 1406 —— 所以应用层**仍须先限长**。
       普通索引里的非键 string 仍是 MEDIUMTEXT + 191 前缀。旧形态键列(前缀索引、`*_ci` 或 PAD SPACE 排序规则)
       不会被自动改写:库的同步入口返回 `ErrLegacyKeyColumn` 并附迁移 SQL,schemamigrate 报「列类型漂移」退出码 4。
     - 每表最多一个 `UNIQUE KEY`。(`v0.2.0` 仍成立。)
   - 超出上述约束时,要在该服务的设计文档里书面申请「手写预建 DDL + 列存在性漂移检查」例外(照 `id_segment` 先例),不许默默手写。
3. **迁移器取 go/db runner 的语义,不取 data_service 的 `CreateOrUpdateTable` 形态**
   - 把 `go/db/internal/migrate` 的 `plan.go` 和 `runner.go` 抽成独立 module `go/schemamigrate`。只保留 `ProtoSource`,不做 `SQLSource`。
   - 保证项:
     - `schema_migrations` 台账,dirty 时拒绝继续;
     - 按库名 `GET_LOCK` 跨实例互斥;
     - 会话级 `lock_wait_timeout`,外加每条语句的硬超时;
     - 断言 `SELECT DATABASE()` 等于代码里的库名常量;
     - 漂移默认只执行 `ADD COLUMN`;类型漂移、多余列、缺主键只报告,要显式 `-allow-modify` 才生成 `MODIFY`。
   - 抽取时必须先修一个缺陷:基线跑过之后,再新增的表永远建不出来(见证据表)。
   - 库名断言自带实现,不复用 `dbguard`:它是 go/db 的 internal 包,语义也是按 ZoneId 推导。
4. **入口与执行时机**
   - 入口是服务自己二进制上的 `-migrate` flag,形状照 data_service,和服务同一个镜像。
   - `-migrate` 调用 `go/schemamigrate`,退出码:0 成功(含「多余列」WARN);1 失败;3 锁忙;4 需人工(类型漂移、缺主键、表清单异常)。
   - dev 档:启动期允许 `Schema.AutoMigrate=true`,有锁保护,多副本并发安全。
   - staging/prod 档:
     - 固定 `false`;
     - 每个服务一个 `<svc>-migrate` Job,照 `kafka-topic-init`:同一镜像和 ConfigMap,args 为 `-f <yaml> -migrate`;
     - `k8s_deploy.ps1` 在 infra 阶段、服务 Deployment 之前,先确认旧 Job 及其 UID 所属 Pod 均终态,再 delete / apply;在途或状态未知时等待,超时拒删(ACTIVE=0 仍可能有 terminating Pod);
     - staging/prod 必须等迁移 Job Complete 后才 apply 服务 Deployment;失败或超时中断发布,此门禁不受 `-WaitReady` 控制;
     - 退出码 3 由 Job 的 backoff 重试;1 或 4 直接中断发布。
   - `AutoMigrate=false` 时,启动路径只跑一次 plan(只读)。发现待执行语句或需人工项就拒绝启动(fail-closed),并打印补救命令。
5. **建库**
   - `CREATE DATABASE IF NOT EXISTS mmorpg_<svc>` 加 `GRANT ... TO 'appuser'@'%'` **只登记一处**:`deploy/mysql-init/00_init_zone_dbs.sql`。K8s ConfigMap 会原样带入这份文件,不要再在 `k8s_deploy.ps1` 里生成第二份。`02_k8s_global_db.sql` 是因为本地和 K8s 库名不同才单独生成的,不是范式。
   - initdb 只在空数据卷执行。已初始化的本地卷和已有数据的 PVC,要在 `deploy/k8s/README.md` 写手工步骤,照 `mmorpg_global` 的写法。库不存在时,migrate Job 以 1 退出并点名库名。
   - `deploy/mysql-init` 从本条起**禁止新增业务表**。存量 `gateway_tables.sql` 保留不动;`guild_friend_tables.sql` 只保留 friend 三表(见 §8 修订)。
6. **跨服务**
   - 跨服务读数据只经对方 gRPC,不跨库 JOIN。mail 取公会成员经 guild gRPC。
   - 各服务共用 `appuser`,它对多个库有 ALL 权限,所以「不跨库访问」目前**只是约定**,机械防线是第 3 条的库名断言。每服务独立账号在 TiDB 用户体系重议时再定。
7. **proto2mysql 版本口径**
   - `go/schemamigrate` 必须 require 一个**未被移动过、至少含 TiDB 选项和 191 索引前缀**的 tag(≥ `v0.1.1`)。
   - **2026-09-28 起全仓统一 `v0.2.0`**(tag → `588c30869859ae87d3b8fc30bfc88836cf48d1fb`):7 个主调用模块
     `go/db`、`go/data_service`、`go/schemamigrate`、`go/friend`、`go/guild`、`go/trade`、`tools/proto_generator/protogen`
     一律 `require github.com/luyuancpp/proto2mysql v0.2.0` + `replace github.com/luyuancpp/proto2mysql v0.2.0 => github.com/luyuan-cpp/proto2mysql v0.2.0`;
     go.sum 只有两行:`github.com/luyuan-cpp/proto2mysql v0.2.0 h1:uLFpdq+Qik8MMqUmIBe3MOM3/4Wn2WgqGoSf6DaoOBQ=` 与
     `.../go.mod h1:090lbeAkv2cZ++KKKNpVaaP89Z4cQe0n9/ZrGN+saVQ=`(`v0.2.0` 的 go.mod 与 `v0.1.1` 逐字相同,依赖图不变)。
     `v0.1.1` 与 `go/db` 的 f3b308f 伪版本退役;f3b308f 是 588c308 的祖先,旧伪版本仍可解析。
   - 不许 require `v0.1.0`:这个 tag 被移动过。
   - **2026-09-16 实测补充**:旧 module 路径 `github.com/luyuancpp/proto2mysql@v0.1.1` 的代理缓存仍是无 TiDB / unknown-fields 解码的旧内容,不能仅按版本字符串验收。`go/schemamigrate` 与主调用模块 `go/trade` 均保留旧 import/require,使用版本限定的远程映射 `replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`。新仓名发布包由官方 Go 代理核验到 `e90a5f0360eaf65794550713514a77f5a57c52a8`,包校验值 `h1:GsmiAKiXCiGjZaRCVutAsrlpShyZRcz71mVrAf+7U5A=`;两份 go.sum 由正常 tidy 生成,不关闭校验。以后新增主调用模块也须声明这条映射,因为依赖 module 的 replace 不传递。
   - **2026-09-20 补:`tools/proto_generator/protogen` 也是主调用模块,当时漏了**(`internal/generator/go/db_model.go` 用 `NewDB` / `RegisterTable` / `GetCreateTableSQL`)。它一直 require `v0.1.0`,go.sum 在两台机器之间来回改(`c149b7c57` 改 `JrtG…`、`d5f5a32ed` 改回 `da7Y…`,后者才是 sumdb 记录的原文)。缓存里没有 zip 的新机器,代理对该版本返回 404,只能回落 direct 拉移动后的 tag,每次都报 `SECURITY ERROR`。已改为 require `v0.1.1`,加同一条 replace,go.sum 换成 `GsmiAK…` / `090lb…` 两行。v0.1.1 的 go.mod 与 v0.1.0 逐字相同,依赖图不变,无需 tidy。换版本不改变任何入库文件:SQL 落盘分支因路径拼接问题从不执行,只在控制台多出 3 行 unique-key 警告。新加任何 import proto2mysql 的 module,先 `git grep -n proto2mysql -- '*go.mod'` 对齐这条映射。
   - 不许 replace 到仓库外目录。
   - **replace 只在主模块生效**(2026-09-17 补,帮会二期 B1):`go/schemamigrate` 若以 module 路径 replace 解析
     proto2mysql(例如 `=> github.com/luyuan-cpp/proto2mysql v0.1.1`),依赖它的建表服务 module(trade、guild)
     必须写逐字相同的 replace,并让各自 `go.sum` 的 `h1:` 与 schemamigrate 一致;schemamigrate 改为 require
     正式 tag 后同步删除。这不属于"replace 到仓库外目录"。
     (2026-09-28 更正:「改 require 正式 tag 后删除 replace」的前提不成立——`v0.2.0` 仍声明旧 module 路径
     `github.com/luyuancpp/proto2mysql`,仓库在 `luyuan-cpp`,旧路径的代理缓存不可信。replace 继续保留;只有库改 module 路径时才同批删除。)
   - ~~本地主键 `VARCHAR(191)` 修复只在未推送的分支上。它进入某个 tag 之前,第 2 条的「禁止 string 主键」不放宽。打 tag 需要人执行(AGENTS.md §9)。~~
     —— 2026-09-28:该修复已随 `v0.2.0` 进 tag(整列 `VARCHAR/VARBINARY(N)` + `utf8mb4_0900_bin`,唯一键同样整列),放宽前提已满足;
     是否放宽第 2 条的「禁止 string 主键」**待用户拍板**,拍板前 schemamigrate 保持拒绝。
8. **存量不动,guild 除外**(2026-09-17 修订,帮会二期 B1,用户决策)
   - friend 表与 gateway 的 `zone_config` 表留在 `mmorpg`,不变量按 D-10 保留。
   - **guild 例外**:项目未上线、无存量数据,`guild` / `guild_member` 与帮会二期新表迁入独占库 `mmorpg_guild`,
     以 `proto/guild/guild_db.proto` 为源,由 `go/schemamigrate` 迁移(`-migrate` + dev 档 `Schema.AutoMigrate`,照 trade);
     后续表由各批次在同一 proto 里追加。
   - `guild_friend_tables.sql` 删除帮会表与遗留迁移过程;`guild_schema_migration` 门表、`MigrateLegacyRankScores`、
     friend 的 `friend_capacity_backfill_v1` 门一并删除。friend 缺容量行时按 friend 表的权威边重算。
   - 帮名唯一键改为 `uk_guild(name_norm)`,规范化(NFKC → TrimSpace → 小写)在 Go 侧完成,不依赖库的排序规则;
     tools/merge_zone 与 data_consistency_check 经 `-guild-schema`(默认 `mmorpg_guild`)限定库名(B1b)。
   - 开发期改表纪律(只追加;索引组只追加到末尾;缺普通索引即拒启)见 `docs/design/guild-phase2/01-storage.md` §6.4。
   - §9 清单对 `mmorpg_guild` 的结论:带 zone 的行只有 `guild.zone_id`(merge_zone 步骤 3 已覆盖);
     biz_tag `guild` 已在 BootstrapTags,`guild_asset_op` 已于 B5a(2026-09-20)加入四处清单(config / store / 服务 yaml / k8s_deploy.ps1);TiDB BR 按库恢复清单加入 `mmorpg_guild`。
   - TiDB Phase 1 是逻辑库对逻辑库迁移,库名不改。
   - data_service 和 go/db 本轮不改迁移路径(见遗留)。
9. **每个新库的上线清单**
   - 新 biz_tag 进 data_service 的 `BootstrapTags`;
   - 在设计文档里声明行是否带 `zone_id` / `home_zone`,决定是否需要 merge_zone 步骤;
   - 补 audit auditor 和 data_consistency_check;
   - 进 TiDB BR 按库恢复清单。
   - 备份 CronJob 是 `--all-databases`,无需改。
10. **不引入** golang-migrate,不建 `go/tools/migrate`,不写「两工具互拒台账」。

**理由**

1. **B 手里有两套现役建表形态,其中一套对另一套有书面反对。**
   - go/db runner 的包注释写明它存在的理由:多副本并发 ALTER 会造成 MDL 阻塞;proto2mysql 自动 `MODIFY` 是「一次无人批准的在线 DDL」。
   - data_service 恰好是这种形态:无台账、无锁、无 `lock_wait_timeout`。
   - 按决策原则「有书面权衡的既有决策不推翻」,选 go/db 的语义。data_service 只贡献入口形状(同二进制 flag,镜像里就有)。
2. **无锁形态在 B 上已经不安全。**
   - data_service 自己的注释承认多副本并发 ALTER 的风险,可它在 K8s 上是随 zone 部署的(不是 Global),全局库只有一份。多 zone 时,dev 档的 AutoMigrate 已经会并发。
   - 新全局服务的模板是 `replicas: 2`。
3. **为什么抽 module,而不是直接 import 或放进 go/shared。**
   - `go/db/internal/...` 受 Go internal 规则约束,其他 module 导入不了。
   - go/shared 是 `go 1.24.5`,proto2mysql 是 `go 1.26.5`。推断:放进 shared 会在 tidy 时把所有依赖 shared 的 module 的 go 指令抬到 1.26.5。D-5 就是出于同样的理由换了落点。
   - 独立 module 只波及真正建表的服务。
4. **入口用服务 flag,不用 `cmd/migrate`。** 镜像只编 ENTRY 这一个二进制,`cmd/migrate` 不在镜像里(P1-03 的证据之一)。用服务 flag,Job 可以直接复用服务镜像。
5. **为什么不取 A 的 golang-migrate(推翻可行性文档 :230、:397)。**
   - B 已有同职责的 runner(原则 1)。
   - 手写 SQL 迁移文件等于和 proto 并行维护一份结构,正是 `ProtoSource` 注释引 AGENTS.md §3 所禁止的。
   - 新增依赖需要说明现有能力为何不足(§11.2),这里说明不了。
   - 「互拒台账」是同时养两套迁移器才有的代价。
   - D-10 规定「按 B 的形状重写」,A 的关系表改写成 proto message。
6. **为什么每服务一库。**
   - `mmorpg_global` 装着 `id_segment`,恢复它是一次 ID 安全事件。业务表混进去,会把业务库的恢复和发号水位绑在一起。
   - `mmorpg` 是 merge_zone DSN 的默认库,里面还有 mysql-init 手写的存量表。
   - `zone_{N}_db` 不能装跨 zone 的行(D-2)。
   - 单独成库后,按库备份和恢复(TiDB BR)、日后按库授权,都有自然边界。
7. **为什么版本 ≥ v0.1.1。**
   - data_service 钉的 `9ad991c` 不认 TiDB 选项,生成的 DDL 会静默丢掉方言。
   - 更早的版本对 string 索引列不加前缀,直接报 MySQL 1170。
   - `v0.1.0` 这个 tag 在缓存里和仓库里指向不同的 commit,新 module 拉到哪份不确定。

**被推翻 / 修订的旧条目**

- `xuanming-port-feasibility-20260902.md` :230、:397(§10 #4)「D4 O2 收窄版:A 的 tools/migrate 搬进 B 作 go/tools/migrate + 两工具互拒」:**推翻**。
- 同文件 :245「D4 原推荐」:**部分保留**。
  - 保留:runner 升格为 `go/schemamigrate`、proto 表走 `ProtoSource`、每服务一库、关闭 mysql-init 新增业务表。
  - 删去:`SQLSource`,以及「移植 A 的 expand-only 门禁」。runner 的漂移默认只 ADD,本身就是 expand-only。
- 同文件 :253「D12 禁直接 require proto2mysql」:**修订**为「建表的新 module 经 `go/schemamigrate` 依赖 proto2mysql,go 指令 1.26.5;其余新 module 仍按 D12 钉 1.24.5」。同时 :25「CI builder 1.24-alpine」一并作废。
- 同文件 :202 D4「手写 mysql-init vs ProtoSource」:定为 `ProtoSource`。
- 同文件 :291「D4 落地前用 `deploy/mysql-init/grant_tables.sql` 过渡」:**作废**,该文件不存在。
- `jubaozhai-market.md` J-6「每服务一库 + 迁移器」:按本条落地。§8「DDL 带 `/*T!*/`」改由 proto 选项生成。

**前提为何不成立**

- **:230 的第一个前提**是「必须加 `SQLSource` 装 A 的 96 个 golang-migrate SQL,而 runner 的 Up 写死 Baseline+Drift,要重写应用循环」。
  - 按 D-10,搬入服务的表按 B 的形状改写进 proto,不需要逐文件台账。
  - 对 `ProtoSource` 来说,Baseline+Drift 恰好够用。
- **:230 的第二个前提**是「runner 零测试」。现在 `plan_test.go` 已有 283 行,只有 `runner.go` 仍无测试。这是抽取时必须补的项,不是否决理由。
- **:253 的前提**是「builder 为 1.24-alpine + GOTOOLCHAIN=local」。
  - 现行 builder 已是 `golang:1.26-alpine`,CI 按各 module 的 go.mod 取版本。
  - data_service 早已直连 require proto2mysql,并且在镜像目录里。
- **本条草案曾把「data_service -migrate」当成「B 已接进 K8s 的形态」**,这也不成立。K8s 上没有任何 Job、initContainer 或命令去执行 `-migrate`,staging/prod 首次拉起时无人建表。go/db 的同类缺口(P1-03)仍是 open。所以本条必须补 Job,不能写成「照已有」。

**证据**

| 事实 | file:line |
|---|---|
| go/db runner 的存在理由与五项保证(台账 / 只 up / GET_LOCK / 超时 / 不自动改列) | `go/db/internal/migrate/plan.go:1-19` |
| 书面反对「proto2mysql 无版本、无锁超时地自动 MODIFY」 | `go/db/internal/logic/pkg/proto_sql/db.go:48-54` |
| ProtoSource 不手写 SQL 的理由;基线 checksum 只算表名集合;`AllowModifyColumn` 默认 false | `plan.go:75-78`、`:86` |
| 漂移只把缺列变成 ADD COLUMN,其余进 warnings | `plan.go:122`,`:141`(查不到表只告警) |
| **缺陷**:基线 version 已应用、checksum 变了(= 新增了表)就只打 WARN 不重跑;漂移阶段对查不到的表也只告警 ⇒ 后加的表永远建不出来 | `go/db/internal/migrate/runner.go:397-404`;`plan.go:141` |
| Up 固定为 Baseline + Drift 两步 | `runner.go:174`、`:188` |
| 库名白名单走 internal 包 dbguard;会话超时;GET_LOCK;台账建表 | `runner.go:12`、`:244`、`:265`、`:295`、`:323` |
| cmd/migrate 按 ZoneId 推导库名;`-allow-modify`;退出码 3 锁忙、4 需人工 | `go/db/cmd/migrate/main.go:53`、`:63`、`:178`、`:197` |
| data_service 迁移直接 CreateOrUpdateTable,无锁、无台账;自陈多副本并发 ALTER 风险 | `go/data_service/internal/store/schema.go:117-127`、`:150-170` |
| data_service 唯一手写 DDL 例外(string 主键撞 1170)与只做列漂移检查 | `schema.go:28-51`、`:150-157` |
| 表名守卫是复制件(不能依赖 go/db internal) | `schema.go:66-68` |
| 已有表不补索引,靠 ensureIndexes | `schema.go:215-221` |
| `-migrate` flag 形态,跑完即退 | `go/data_service/data_service.go:41-44`、`:225-240` |
| data-service 随 zone 部署(非 Global),全局库只有一份 | `tools/scripts/k8s_deploy.ps1:387`;`deploy/k8s/README.md:321` |
| 全局服务模板是多副本 | `deploy/k8s/manifests/go-svc/chat.yaml:41`;`match.yaml:36` |
| data-service Deployment args 只有 `-f`;K8s 命令集没有 migrate;go/db 同类缺口 open | `data-service.yaml:54`;`k8s_deploy.ps1:8`、`:1495-1496`;`docs/handoff/handoff-backlog-2026-09-05.md:202` |
| staging/prod 固定 AutoMigrate=false | `k8s_deploy.ps1:1500-1512` |
| 一次性 Job 接进部署流程的先例(infra manifest 之后、zone 之前;先 delete 再 apply) | `deploy/k8s/manifests/infra/kafka-topic-init.yaml:15-16`、`:42-57`;`k8s_deploy.ps1:2814-2816` |
| mysql-init 目录原样打进 K8s ConfigMap;02_ 只因库名与本地不同才生成 | `k8s_deploy.ps1:2597-2601`、`:2620-2632`、`:424-429` |
| initdb 只对空卷执行;存量 PVC 手工补建先例 | `deploy/mysql-init/00_init_zone_dbs.sql:18-23`;`deploy/k8s/README.md:339-341` |
| MYSQL_USER 只自动授权 `mmorpg`,其余库要显式 GRANT | `00_init_zone_dbs.sql:1-3` |
| mysql-init 现有业务表(存量豁免) | `deploy/mysql-init/guild_friend_tables.sql:6-67` |
| data_service require `v0.1.0`,go.sum 与缓存为 `9ad991c`;仓库里 `v0.1.0^{}` = `2aca007`(tag 被移动) | `go/data_service/go.mod:3`、`:9`;`go.sum:131`;模块缓存 `proto2mysql/@v/v0.1.0.info`;`E:/work/proto2mysql` `git rev-parse v0.1.0^{}` |
| go/db replace 到仓库外;镜像靠命名构建上下文带入 | `go/db/go.mod:10`、`:132`;`tools/scripts/go_svc_image.ps1:110-118` |
| TiDB 选项 500021-24 在 B 的 proto 已声明;`9ad991c` 的 options.go 无 TiDB | `proto/db/proto_option.proto:93-100`;`git grep -i tidb 9ad991c -- options.go` 零命中 |
| string 非主键列一律 MEDIUMTEXT;191 索引前缀从 v0.1.1 起 | proto2mysql `83fed85` `proto2mysql.go:403`、`:599`;`e90a5f0`(v0.1.1)`proto2mysql.go:485` |
| 每表只有一个 UNIQUE KEY | proto2mysql `83fed85` `proto2mysql.go:927-938` |
| 主键 VARCHAR(191) 修复只在未推送分支 `codex/save-local-key-column-20260914` | proto2mysql `f3b308f` `proto2mysql.go:135`;`git branch -a --contains f3b308f` |
| 库自陈:无版本概念时 MODIFY/CHANGE 会在新旧副本间来回翻 | proto2mysql `83fed85` `proto2mysql.go:65-71`、`:185-191` |
| builder 已是 1.26;CI 按 module go.mod 取版本;shared / chat 仍 1.24.5 | `deploy/k8s/Dockerfile.go-svc:43-46`;`.github/workflows/go-modules-ci.yml:189`;`go/shared/go.mod:3`;`go/chat/go.mod:3` |
| 镜像只 COPY proto / shared / 目标服务 | `Dockerfile.go-svc:65-70` |
| TiDB D3「不在业务代码手写 DDL」;D9 只定迁 TiDB 不定库名;迁移是逻辑库对逻辑库;TiDB 建库权限未做 | `docs/design/global-data-layer-tidb-decision.md:85`、`:138-141`、`:173`、`:172` |
| 聚宝斋 §8 每张表至多一个唯一键;发号 biz_tag 由 idsegment | `docs/design/jubaozhai-market.md:208`、`:213`、`:216-217` |
| 契约要求首个建表服务先拍 D4 | `docs/design/microservice-zone-contract-20260914.md:216-217` |
| 被推翻 / 修订条目原文 | `xuanming-port-feasibility-20260902.md:25`、`:202`、`:230`、`:245`、`:253`、`:291`、`:397` |

**代价**

- **工作量**:抽 `go/schemamigrate`、修「后加表」缺陷、库名断言、补 runner 测试、接 Job,推断 4~6 人日(未核算)。可行性文档 :245 原估 15 人日,那个数含 SQLSource 和 A 的门禁。
- **A 侧改写量未清点**:A 的关系表改写成 proto message 的量没算过。A 的 96 个 SQL 里有多少属于本期要搬的服务,需要按服务清点。
- **proto2mysql 表达力**:只能用整数主键;string 索引列只按前 191 字符生效;每表一个唯一键。超出的只能走书面例外。
- **只做加法**:迁移不删列、不改名、不自动改类型,也不给已有表补索引。破坏性变更要单独设计,人工带 `-allow-modify` 或走运维 DDL,并按 AGENTS.md §11.3 配版本、灰度、回滚。
- **建表服务的 go 指令升到 1.26.5**,与其余新 module 分成两档。
- **存量环境补建库是手工步骤**。「不跨库」在共用 `appuser` 下只是约定。

**本条不改的 / 遗留**

- **data_service**:`-migrate` 仍然无锁,且随 zone 多实例。后续迁到 `go/schemamigrate`(`id_segment` 手写例外保留)。本轮不动。
- **go/db**:仍用 internal 的 runner,replace 到仓库外。抽取落地后改为 import `go/schemamigrate`,并与 P1-03 的 Job 同批处理。
- **proto2mysql 版本**:data_service(`9ad991c`)和 go/db(本地分支 `f3b308f`)要对齐到同一个不可变 tag。主键修复需要进 tag,由人执行。
- **guild 没有 K8s ConfigMap 和 manifest**(`deploy/k8s/README.md:357`),mail 经 guild gRPC 这条路在 K8s 上暂不可达。
- **TiDB 上 `mmorpg_<svc>` 由谁建、授权给谁**:随 TiDB 决策 §6 第 4 项一并定。

---

> 以下一条为 2026-09-18 追加,随 friend 移植 F1 批落盘。它**修订 D-10 的一个子项**,
> D-10 原文与其余条目一字不改;该子项上冲突时以本段为准(AGENTS.md §5「没写文档 = 没说过」)。
> 相关代码**未编译、未运行**,行为以 Codex 验证结果为准。

## D-10 修订(2026-09-18)`friend_capacity` 回填就绪门禁:**退役;保留容量锁行与「缺行按权威边数、绝不猜 0」**

(来源:friend 移植 F1 批冻结规格 §6.1,主会话裁定。D-10 原文见本文 :177-:189,被修订的是 :184 表格行里的 "migration ready gate" 一项。)

**结论**

- D-10 为 friend 列出的「必须保留」项里,`friend_capacity_backfill_v1` 这道 durable readiness gate **在 friend 移植中退役**,不随服务搬到 `mmorpg_friend`。
  代码上删除的是:`FriendRepo.RequireFriendCapacityReady` / `requireFriendCapacityReady` / `friendCapacityMigrationStateError` / `migrationStateQuerier` / `friendCapacityMigrationKey` / `ErrFriendCapacityMigrationNotReady`,
  以及它的三个调用点(`ensureFriendCapacityRows` 开头、`AcceptFriend` 与 `RemoveFriend` 事务内的 `FOR SHARE` 复查)和 `friend.go` 启动期那次拒启探测。
- D-10 表格行里 friend 的**另外两项照旧保留**:`friend_capacity` 显式计数锁行、versioned cache generation。
- **仍然有效的 D-10 实质不变量(一个都没动)**:
  1. `friend_capacity` 的显式计数行是好友数硬上限;所有写 friend 边的路径都在同一把容量锁里,按 `RowsAffected` 增减 `friend_count`。
  2. **缺容量行时按 `friend` 表的权威边数建行,绝不猜 0**(`ensureFriendCapacityRows` 里的 `SELECT COUNT(*) FROM friend`)。
  3. 锁序纪律照 B 侧**现有**形态,本次一字未改:事务外先补齐容量行(自动提交的 `INSERT IGNORE`,避免同一接收者行上的 insert-intention 死锁)→ 事务内先按主键 `FOR UPDATE` 锁定校验申请行 → 再按 `player_id` 升序 `FOR UPDATE` 锁双方容量行 → 最后写入。
     ⚠ 注意申请行锁**排在容量锁之前**,不是反过来:提前把 `status` 从 1 推进到 2 会让并发事务在 `idx_to_player` 前缀上删/插而触发 1213(理由写在 `friend_repo.go` 的 `AcceptFriend` 里)。这不构成 ABBA —— 申请行锁按 `(from_player_id, to_player_id)` 主键天然切分,不参与容量锁的升序环;`RemoveFriend` 与 `AddFriendRequest` 都不会在持有申请行锁的同时去等容量锁。
     被退役的门禁原本是事务内的**第一条**语句(`FOR SHARE` 同一行全局台账),删掉它只是砍掉链条最前面的一环,申请行锁与容量锁的相对顺序不变。
  4. versioned cache generation + Lua CAS,写路径提交后失效缓存。
- 与之配套:`friend_repo_mysql_test.go` 删掉只测被删函数的 `TestFriendCapacityMigrationStateRequiresExplicitReady`,
  并把原先「半迁移 fail-closed + 缺行用权威计数」的双职责用例收成 `TestMissingCapacityRowUsesAuthoritativeFriendCount`,继续钉住第 2 条不变量。

**理由**(按权重)

1. **它读的是别的库的表,违反 D-14 第 6 条(不跨库访问)。** 门禁执行 `SELECT state FROM guild_schema_migration ...`,而这张表按 D-14 第 8 条(本文 :384)「存量不动」留在旧共享库 `mmorpg`;friend 的 `config.Validate` 又断言 `MySQL.DBName == data.DatabaseName`(`mmorpg_friend`)。所以这条查询在任何合法配置下都不可能命中 —— 门禁是可证明的死代码。
2. **「表不存在就放行」不是没有代价的保险。** 那是第一轮裁决选的处置(MySQL 1146 放行),代价是:`ensureFriendCapacityRows` 每次写调一次、`AcceptFriend` / `RemoveFriend` 的事务内还各带一次 `FOR SHARE` 再调一次。结果是 MySQL 持续记 1146 错误、每次写多一个注定失败的往返,而门禁什么也没挡住。
3. **它挡的风险在新库里结构性不存在。** 门禁针对的是「在 `mmorpg` 里给历史 friend 边原地回填 `friend_capacity`,DDL 隐式提交导致半迁移」这一个具体场景。`mmorpg_friend` 由 `go/schemamigrate` 从基线一次建全,库里没有任何历史边,不存在需要回填的存量,也就没有「半份 0」可暴露。
4. **AGENTS.md §11.2(KISS / YAGNI)与 §11.3(不得静默降级)。** 一个名字叫「就绪闸」、实际永远放行的机制比没有它更坏:下一个人会以为写路径有这层保护。删代码优于加特例。

**证据**

| 事实 | file:line |
|---|---|
| D-10 原文把 friend 的 ready gate 列为「必须保留」 | `docs/design/xuanming-port-decisions-20260910.md:184` |
| D-14 第 8 条:`guild_schema_migration` 门表留在 `mmorpg` 不动 | 同上 `:384` |
| D-14 第 6 条:跨服务只经对方 gRPC,不跨库 | 同上 `:375` |
| friend 独占库库名常量 | `go/friend/internal/data/tables.go:22` |
| `config.Validate` 断言配置库名等于该常量 | `go/friend/internal/config/config.go:296` |
| 保留项:缺行从 friend 权威边数算初值 | `go/friend/internal/data/friend_repo.go:405` |
| 保留项:双方容量行按 `player_id` 升序 `FOR UPDATE` | 同上 `:192`(AcceptFriend)、`:308`(RemoveFriend) |
| 保留项:按 `RowsAffected` 增减 `friend_count` | 同上 `:258` |
| 保留项:versioned cache generation + Lua CAS | 同上 `:78`(invalidate 脚本)、`:501`(读路径) |
| 门禁退役的推翻依据(F1 §6.1 四条理由) | friend 移植 F1 批冻结规格(未跟踪文件,内容已逐条抄进本段「理由」) |

**代价与残留**

- **旧共享库 `mmorpg` 里的 friend 三张表(`friend` / `friend_request` / `friend_capacity`)成为孤儿**:新服务只读写 `mmorpg_friend`,旧表不再有任何写者。清理(以及 `deploy/mysql-init/guild_friend_tables.sql` 里那段 friend 建表与回填脚本的处置)属 friend F3 批,本批不做。孤儿表留着不影响正确性,只占空间并会误导排障的人。
- **~~`guild_schema_migration` 表本身属 guild,不动~~ —— 2026-09-19 合并时作废**:写这条时 friend 分支基于的 main 上那张表还在,而合并回 main 才发现**帮会二期的 B1 迁库已经把它连同 `guild_friend_tables.sql` 里的帮会表一起删了**(见本文 D-14 §8 修订的第三个圆点:「`guild_schema_migration` 门表、`MigrateLegacyRankScores`、friend 的 `friend_capacity_backfill_v1` 门一并删除」)。也就是说帮会那边**独立地**得出了与本条相同的结论,只是理由不同(他们是因为迁库带走了门禁表,本条是因为跨库读违反 D-14 §6 且新库无历史边)。
  合并的实际结果:`deploy/mysql-init/guild_friend_tables.sql` **整个文件已删除** —— 两边各搬走一半之后它已无内容。本条退役门禁的四条理由不受影响:理由 1(跨库读)描述的是写这条时的事实,今天连被读的表都不存在了,结论只会更强。
- **不再有「历史容量未回填」的机械防线**:结论成立的前提是「`mmorpg_friend` 永远由 `schemamigrate` 从基线建、库里没有历史边」。若将来真要把 `mmorpg` 的存量 friend 边导进新库,**必须先重新设计一次带 durable 标记的回填门禁**,不能直接跑导入 —— 届时靠的是这条记录,而不是代码里的残留。
- **D-10 原文的行号引用已失效**:本文 `:184` 里引的 `internal/data/friend_repo.go:222-230`、`:59`、`:518` 是 B 侧旧形态的行号,经移植与本次删除后已全部对不上(现行位置见上面的证据表)。按「不改原文」的口径未回头修正,属已知残留 —— 读 D-10 时以本段证据表的行号为准。
- **历史文档残留（2026-09-18 记录，2026-10-23 已清理）**：当时的好友持久化说明与公会／好友旧实现笔记仍按 B 侧旧形态描述这道门禁，本批没有修改。现行说明见 [好友持久化架构](./friend-persistence-architecture.md) 与 [公会与好友中文说明](./guild_friend_service_notes_zh.md)，两者均明确旧门禁已退役；旧主文与英文副本已删除。

**验证状态**:未编译、未运行测试(AGENTS.md §10.1),待 Codex 验证。

---

> 以下一条为 2026-09-29 追加,随 battle 直连收缩批(`turn-based-battle-server.md` §22 D65–D75)落盘,仿本文「D-10 修订(2026-09-18)」的先例。
> 它**修订 D-12 的 K8s 默认值、前提与一条理由**;D-12 原文(本文 :252-:290)不删不改,只在被修订的三处旁加了就地标注(:263、:270、:272,指向本段),冲突时以本段为准(AGENTS.md §5「没写文档 = 没说过」)。
> 相关脚本与代码**未编译、未运行测试、未上集群**,行为以 Codex 验证结果为准。

## D-12 修订(2026-09-29)K8s 默认翻为路由模式:**`k8s_deploy.ps1 -GateRouterMode` 默认 `"1"`;C++ 默认值与单测仍不改;回退态不粘滞**

(来源:turn-based §22 D65「一次做完收缩,豁免前提①、事后补验」与 D75「K8s 默认路由模式」,2026-09-29 用户拍板。被修订的是 D-12 结论第三条的 **K8s** 子项(:262)、理由 2 的后半句(:269)与理由 3(:271)。D-12 其余结论——新 Go 服务只承诺路由模式、不补直连白名单、回退前先 killswitch 并公告——不变。)

**结论**

- **K8s 默认翻为 `"1"`**:`k8s_deploy.ps1 -GateRouterMode` 默认由 `"0"` 改为 `"1"`,`ValidateSet("0","1")` 与字符串类型保留,仍只注入 gate(`k8s_deploy.ps1:163-164`;注入点两处:podip / Deployment 形态 `k8s_deploy.ps1:1594-1601`,external 的 gate StatefulSet 形态 `lib/k8s_client_entry.ps1:772-782` `New-GateStatefulSetYaml`,参数 Mandatory、无默认值,由 `k8s_deploy.ps1` 传入)。本地 `start_game.ps1 -GateRouterMode` 默认 `'1'` 不变(`start_game.ps1:22`)。
- **D-12 原前提的现状**(原文 :262「翻成 1 要同时满足三件事」):
  1. 「K8s 上以路由模式跑通过一次 battle-smoke」——**未满足,由用户豁免,改为事后补验**(D65)。补验之前,K8s 默认值属于「已落码、未实测」。
  2. 「路由服 manifest 已落地」——已满足:`deploy/k8s/manifests/go-svc/client-rpc-router.yaml`,`$GoSvcCatalogue` 条目与 ConfigMap case 见 `k8s_deploy.ps1:150-155` 注释列出的部署链。
  3. 「路由服用 POD_IP 通告」——已满足:`go/client_rpc_router/client_rpc_router_service.go:141-145`(`advertisedHost`,POD_IP 优先)。
- **C++ 默认值与单测一字不改**:`gate_router_mode.h:35-55` 未设变量、或不是 `1` / `true` / `on`,一律直连;`gate_security_test.cpp:160` `EmptyAndUnsetDefaultToDirectMode` 照旧断言。**「进程默认直连、部署默认路由」是有意的分层,不是漂移**;看到两者不一致不要去「对齐」C++ 默认值。
- **包装入口「留空不覆盖」**:`dev_tools.ps1`(:83,透传 :652-653 / :855-856,回滚入口 :1603)、`k8s_image.ps1`(:69,透传 :411-412)、`k8s_zone_rollback.ps1`(:139-150)各有 `-GateRouterMode`,`ValidateSet("", "0", "1")`,默认空串 = 不传,由 `k8s_deploy.ps1` 的默认值接管;默认值只存在于 `k8s_deploy.ps1` 一处。
- **翻转后 gate 硬依赖路由服**:路由模式下 gate 的依赖门等 `ClientRpcRouter + Scene`,不等 Login(`cpp/nodes/gate/main.cpp:401-404`)。infra-up 必须已把 client-rpc-router 部署就绪(不带 `-SkipGoSvc`,且给了 `-GoSvcRegistry`);否则 zone-up 后 gate 卡在依赖门,登录 / 匹配 / 聊天全部 `no_target`(`k8s_deploy.ps1:150-152` 注释)。
- **回退态不粘滞**(本修订新增的运维约束):`k8s_deploy.ps1` 每次部署都把本值原样重写进 gate env,不从集群读回旧值(`k8s_deploy.ps1:157-160`)。以 `"0"` 回退运行的 zone,之后**每一次**重新部署——日常 zone-up / release-zone、**合服后的 zone-up**、**灾备回滚 `k8s_zone_rollback.ps1` 的 Step 6**、以及经 `dev_tools.ps1` / `k8s_image.ps1` 包装入口——都必须显式再传 `-GateRouterMode 0`,否则静默落回 `"1"`;回退的原因(例如路由服不可用)若仍在,gate 就卡在依赖门。
  - **回滚脚本第 0 步预检**:`k8s_zone_rollback.ps1 -Apply` 且未传 `-GateRouterMode` 时,在 Step 1 停服**之前**只读一次 zone namespace 里的 gate(StatefulSet 与 Deployment 两种形态都认),取出 `GATE_CLIENT_RPC_ROUTER`;与 `k8s_deploy.ps1` 的默认值不一致、或判不出(namespace 已删 / kubectl 失败 / `valueFrom` / 变量重复 / 两种 gate 同时存在)即拒绝,要求显式传 `0` 或 `1`(函数定义:`Assert-ZoneEntryKeptOnRollback` :496、`Get-ClusterZoneEntryState` :390、`Get-GateRouterModeFromPodSpec` :273;脚本头说明 :18、:35-45)。同一预检也核对集群外入口的 `-ClientEntryMode` / `-GatewayIngressHost`(D76–D93 的不粘滞参数)。`-AllowDisruptiveSwitch` 只透传到 Step 6,**不豁免第 0 步**。
  - **合服 zone-up 没有同类预检**:合服 runbook 的 zone-up 与各处 `k8s-zone-up` 示例目前都不带 `-GateRouterMode`,回退态 zone 合服时须人工补 `-GateRouterMode 0`。
- **集群外部署同窗口**:集群外入口(`k8s-client-entry.md`,D76–D93)与本修订同批落码;`ingress_final` §6 原定「第 4 批才在 K8s 默认启用 GateRouterMode=1」作废。集群外部署须与 `-GateRouterMode 1` 在同一窗口启用 `-ClientEntryMode external`(`k8s_deploy.ps1:176` 注释)。

**理由**(按权重)

1. **前提①保护的窗口不存在,且它要验证的交互已经消失。** 项目未上线、没有老客户端(turn-based §19 D36 已据此作废「客户端直连失败率」前提)。D66 起 gate 在两种路由模式下都不中继战斗、不连 battle(`gate_router_mode.h:11-13`;白名单代码 `gate/main.cpp:216-219`,说明注释 `:199-203`),battle-smoke 在两种模式下走同一条战斗路径;它对路由模式还能提供的证据只剩登录、匹配这些经路由服的 RPC,由事后补验覆盖。
2. **原理由 2 前半句仍成立,后半句在 D37 落地后不成立。** 前半句「C++ 默认值是灰度开关的安全兜底(拼错宁可留在直连)」仍是 C++ 默认值不改的依据。后半句「改它会连带 battle 等既有直连路径的回归面」写于 gate 仍中继战斗之时;D37 规定的删码(gate 战斗中继、Kafka Bind/Unbind 契约)已由 D66 / D67 执行,`GATE_CLIENT_RPC_ROUTER` 不再影响任何战斗路径,battle 已不在它的回归面里。
3. **原理由 3「按 D34 验证后再切换 K8s 默认值」被 D65 的豁免取代**;验证没有取消,只是从「翻转前置」改成「翻转后补验」,见下方残留第 1 条。
4. **翻转仍只落在部署层**:部署参数可以逐个环境翻,出问题一行回退;这正是 D-12 原结论选部署层的理由,本修订只改了 K8s 这一层的默认值。

**证据**

| 事实 | file:line |
|---|---|
| K8s 默认 `"1"`、ValidateSet、参数注释(前提、前置、回退不粘滞) | `tools/scripts/k8s_deploy.ps1:145-164` |
| K8s 只给 gate 注入 `GATE_CLIENT_RPC_ROUTER` | `tools/scripts/k8s_deploy.ps1:1594-1601`(Deployment 形态);`tools/scripts/lib/k8s_client_entry.ps1:772-782`(external 的 gate StatefulSet 形态) |
| 本地默认 `'1'`、起 gate 前设 env | `tools/scripts/start_game.ps1:22`、`:554` |
| C++ 默认直连、只认 1/true/on;战斗与本开关无关 | `cpp/nodes/gate/gate_router_mode.h:6-13`、`:35-55` |
| 单测钉死默认直连 | `cpp/nodes/gate/tests/gate_security_test.cpp:137`、`:160` |
| 路由模式依赖门 = ClientRpcRouter + Scene | `cpp/nodes/gate/main.cpp:401-404` |
| 包装入口留空不覆盖 | `tools/scripts/dev_tools.ps1:83`、`:652-653`、`:855-856`、`:1603`;`tools/scripts/k8s_image.ps1:69`、`:411-412` |
| 回滚第 0 步预检 | `tools/scripts/k8s_zone_rollback.ps1:139-150`(参数)、`:273`、`:390`、`:496`(三个函数定义) |
| 契约用例:默认路由 / 回退 0 / 拼错被拒 / 包装透传 | `tools/scripts/tests/k8s_deploy_contract.tests.ps1:732`、`:740`、`:745`、`:801` |
| 契约用例:回滚透传与预检 | `tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1:89`、`:101`、`:450`、`:466`、`:484`、`:555` |
| 路由服 POD_IP 通告 | `go/client_rpc_router/client_rpc_router_service.go:141-145` |
| D36 / D37 原文 | `docs/design/turn-based-battle-server.md` §19.1 D36 / D37(2026-09-29 工作树 `:660`、`:661`;该文件有并行改动,行号可能再漂移,以决策号为锚) |

**代价与残留**

- **K8s 路由模式 battle-smoke 仍未跑**:补验是本修订成立的前提,补验前 K8s 默认值未经实测。补验通过后应在 `PROGRESS.md` 与 turn-based §22 登记结果。
- **没有「路由模式却没部署路由服」的 fail-fast**:`k8s_deploy.ps1` 只在参数注释里写明前置,未对「`GateRouterMode=1` 且 infra-up 带 `-SkipGoSvc` 或缺 `-GoSvcRegistry`」告警或拒绝;`k8s_image.ps1` 不透传 `-GoSvcRegistry`,release-zone 依赖 infra-up 另行部署路由服。是否加 fail-fast 待拍板。
- **合服 zone-up 与文档示例**不带 `-GateRouterMode`,回退态下只能靠人工补传(见结论)。
- **D34 遗留的「何时删 C++ 直连模式代码」仍待拍板**:直连模式目前仍是 login / scene_manager / match 这些既有服务的回退面(回退时 chat 等只承诺路由模式的服务不可达,见 D-12 原文 :264),C++ 默认值不改的前提下它不能删。

**验证状态**:未编译、未运行测试、未上集群(AGENTS.md §10.1),待 Codex 验证。Codex 在仓库根执行:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` 与 `pwsh -NoProfile -File tools/scripts/tests/k8s_zone_rollback_gate_router_mode.tests.ps1`,通过标准 fail=0;C++ `gate_security_test` 回归(`GateRouterMode.*` 全过,默认仍直连);K8s 路由模式 battle-smoke 由用户或 Codex 在集群上补跑。

---

## D-15 八域对比选型与容量目标(2026-10-15，用户最终范围)

### D-15.1 八域范围与源码基线

用户最终明确本轮对比 `D:/luyuan/xuanming-server` 与本仓的范围为以下八域：

| 本轮域 | 源仓目录 | 本仓对应入口 |
|---|---|---|
| 好友 | `services/social/friend` | `go/friend` |
| 聊天 | `services/social/chat` | `go/chat` |
| 公会 | `services/social/guild` | `go/guild` |
| 邮件 | `services/social/mail` | 待补邮件服务与协议 |
| 排行榜 | `services/runtime/leaderboard` | 待补通用排行榜服务与协议 |
| 登录 | `services/account/login` | `go/login` |
| 拍卖 | `services/economy/auction` | 待补拍卖/撮合域 |
| 玩家交易 | `services/economy/trade` | 现 `go/trade` 是聚宝斋，需区分双人交易 |

容量目标为 **DAU 1 亿 / CCU 2000 万**；用户已授权本项目独立客户端仓 `../mmorpg-client/` 同步，按依赖分批端到端完成。源仓的 UE 客户端不在本次读取、修改范围。本次只读核对源 HEAD 为 **`b1f6c1ebf4583b45a9370ad0522c64196dc792b2`**，2026-10-15 核对时 `git status --porcelain` 为空。后续批次以此可追溯基线对账，源仓更新须另记差异，不能静默漂移基线。

**明确排除：dialogue、player、inventory、mission、scene_manager。** 用户随后纳入 `services/economy`，结合此前 inventory 不迁的约束，新增 auction / trade，inventory 仍排除。team、matchmaker、ds_allocator、hub_allocator、owner、player_locator、data_service 等仅记录本轮八域所需接缝，不独立移植。登录仍用现有会话、位置和进场接口，邮件/榜奖励/交易仍用现有资产权威；适配调用契约不等于获准搬入这些服务。

本条覆盖可行性文档 §3.2 / §5 / §6、本文 D-8 / D-10 及 `friend-port-20260918.md` §1.1 中据“本仓已有”而仅挑增量的**八域任务范围**。历史正文保留，不再是跳过八域能力的依据；对排除域不作移植承诺。首批好友申请代次修复只是一项切片，不代表整体完成。逐能力对账与依赖批次见 [可行性文档 §13](xuanming-port-feasibility-20260902.md#13-八域能力对比与依赖批次2026-10-15)。

**最高选型准则已由用户更新为“对比原来，哪个能承载更多玩家用哪个”。** 逐域比较两仓，在正确性、数据一致性、安全和兼容不降级的前提下选择更可扩展实现；原样复用源业务模块、保留本仓实现或在清晰边界适配都可以，不能预先固化为整服照搬或沿 B 任意重写。业务能力不能在选型时静默缩水，差异与代价须明确。记录源/本仓代码、配置、查询计划、热点与事务边界的证据；有可比实测时以测量判断，无实测只给出静态候选与待验证假设，不能宣布哪边实际承载人数更多。源仓缺口同样登记。D-1 的启动、可达、端到端冒烟继续有效，并扩展为本节的完整能力和故障验收。

### D-15.2 源仓分布式形态的现行事实

- **好友分布式事务由 TiDB 提供。** 源 `friend_repo.go` 使用 `database/sql` 显式事务不等于单机存储；`services/social/friend/etc/friend-dev-tidb.yaml` 指向 TiDB，原 `FOR UPDATE`、双向建边在同一数据库事务内完成。本地 `tools/scripts/run_services.ps1` 为 friend / chat / guild / mail 默认选择 `*-dev-tidb.yaml`，`-SocialOnMysql` 才切回 MySQL。`gen_cluster_config.ps1` 参数本身仍默认 `SocialStore=mysql`，而 `start.ps1` 的 K8s 编排显式传 `-SocialStore tidb`；两个层级不得混写成所有入口一律默认 TiDB。
- **没有完成应用分库 Saga。** `services/social/friend/internal/biz/friend_sharding.go` 只提供路由判定、幂等键和观测，明确不改现有建边事务；不能写成已接 Kafka 双向建边消费者，也不能据此否认 TiDB 提供的跨节点事务。
- **八域在源仓均有真实服务入口与装配。** 源 mail / leaderboard / auction / trade 有真实仓储和业务装配，不能拿本仓设计文档、聚宝斋只读接口代替；同名服务也须逐接口/规则/失败语义对账。源 team 独立而本仓寄宿 match 等只登记为八域边界，不自动立项整服替换。
- **推送只有分段保证。** 源 `services/runtime/push/internal/biz/consumer.go` 消费 Kafka 后调用 `AssignAndBuffer` 原子分配游标并写投递缓冲，失败拒绝成功消费，连接写者支持重放。好友仓储提交后由 `FriendUsecase` 发 `PushFriendEvent`，没有与好友事务同提交的 outbox；提交成功到 Kafka 成功仍有丢通知窗口。消费后缓冲不等于全链路零丢失。权威列表/申请补拉保留；八域所需可靠事件须解决生产侧事务边界、去重与恢复，不能以存在 push 代替闭环。

### D-15.3 必须适配并保留的本仓约束

1. 客户端使用本仓 gate 帧、消息号、会话鉴权、tip 配表与协议生成链；同批适配独立客户端、robot、路由服及受影响节点，不把源 gRPC-Web / JWT 接口原样暴露到本仓。身份、目标实例防僵尸、Kafka 业务实体 key、超时与失租规则继续成立。
2. C++ scene 仍是现行在线物品、货币、经验等资产权威，inventory / player / mission 不迁移。邮件附件、排行榜奖励、交易/拍卖托管结算适配现有通道与唯一权威；出口缺能力时列具体阻塞与最小接缝，不擅自建第二份 Go 资产权威。登录不得借移植重写被排除的 scene_manager / owner 等管理面。
3. 好友已有容量计数、缺行按权威边数建行、完整主键点锁与固定锁序、缓存 generation、列表上限、推荐 SQL 有界性及 MySQL / TiDB 方言约束继续保留。不能被旧源 SQL、旧文档的 `IN/OR` 锁定查询或无界推荐覆盖。D-10 修订中的历史锁序已有后续更正，以当前生产 SQL 和真实库回归为准。
4. 保留每服务数据所有权、版本迁移和账号权限、跨服务接口、明确失败语义、安全与持久化不变量。源社会服共用 `pandora_social`，本仓当前每服务独占库；接 TiDB 时明确逻辑库/权限/迁移，不直接把本仓 DSN 指向源库，也不绕过现有门禁。
5. 双版本要求继续有效。本次 Java 状态为**待逐能力对账与对齐**；已有 Java 局部实现不代表八域完成。每批分别记录版本号、PARITY 状态与实际验证证据。

### D-15.4 分批执行、容量与完成门禁

批次与依赖见可行性文档 §13.3。用户已授权八域的比较选型与必要实现、超过 30 文件的必要改动及独立客户端同步；分批用于可审阅、可运行、可回滚，不能变回只挑增量。排除域的新业务或独立移植不包含在授权内；缺少关键接口、改变资产权威或扩大范围时报告具体影响。每域留选型记录：两仓真实实现、容量瓶颈候选、已有正确性修复、适配差异、复用部分的源版本/内容校验，以及未验证项。

每批对齐生成源与产物、业务实现、迁移、真实装配、本地与 K8s 登记、客户端/robot 调用和恢复入口，再进入验收。按风险运行构建、静态检查、确定性回归、真实 TiDB / Redis / Kafka 集成、跨区冒烟、并发及崩溃恢复；跳过、未跑与仅静态确认分别报告。资产交付与可靠通知覆盖提交后崩溃、重复/重放、结果未知及补偿。纯函数、提案、默认关闭分支或局部单测通过不能替代接线。

**DAU 1 亿 / CCU 2000 万是目标，不是达标证据。** 尚缺每域行为频率、峰值/重连风暴、好友边与列表规模、缓存命中、消息扇出、保留期、地域/存储/网络与故障预算、硬件和成本约束；需据此形成负载模型，再按本仓压测纪律取基线、对比与故障 SLO 证据。

源 HEAD 也不提供“2000 万 CCU 已达标”的证据：`deploy/k8s/services/services.yaml` 多数业务单副本，online overlay 只显式抬 login / player-locator 并保留 push=1；开发 TiDB 清单的 PD / TiKV / TiDB 各为单副本。online overlay 不据此证明实际线上基础设施，开发配置不是生产高可用拓扑，复制配置也不能证明吞吐。两仓都是逐域比较对象，不是容量认证。

**本条状态：仅最终范围与静态对账落档；未改实现、未编译、未测试、未压测，尚无新增完成或容量证据。**

## D-16 全球匹配策略与商城比较边界（2026-10-15，后续用户决定）

**决定：匹配纳入本轮全球同服对齐，采用区域内优先、等待后跨区域兜底。** 全球玩家身份、组队与玩法规则共享，地域只决定候选优先级和战斗放置，不成为不可跨越的服务器边界。候选地域必须来自每位参战玩家到各候选机房的有效实测 RTT；玩家归属区、ID 哈希区和队长地域均不能代替网络延迟。组队时检验所有成员，队伍不可拆散来绕过约束。缺失、过期或不可信探测须有明确拒绝/重新探测语义，不能默认为零延迟。

等待扩大只放宽已声明的地域/评分窗口，不绕过玩法、容量、版本、封禁、队伍完整性或每位玩家的延迟上限；阈值通过已有配置/配表接缝管理，并以真实负载与等待/成局/延迟分布定标，不凭空指定秒数。跨域候选扩展须有界，不能让每个节点全扫全服票据。原始入队时间在重试/回队中保留，避免无限续等。

成局领取必须原子地校验全部成员的票据代次与状态，取消/重复请求、旧 leader、超时后迟到分配和节点切换都不能让同一玩家同时进两局；机房选择必须传到实际 battle 分配并回核，只有日志不算接通。分配结果未知时按稳定成局 ID 查询/恢复，不能直接再开第二场。验证覆盖本区足够、本区不足扩区、跨区预组队、单成员超延迟、探测过期、取消竞态、重复/失租和分配响应丢失。设计细则见 cross-zone-matchmaking.md §12；当前是已接受的目标，未宣称已实现。

原仓 matchmaker 在 internal/biz/match.go:3671 已实现区域内优先与跨区剩余票据撮合，但 ticketRegion 以队长的逻辑归属路由为锚；:2689 的 battlePlacement 仍仅写日志，尚未进入 AllocateBattleRequest。不能把该实现直接登记为满足以上地域延迟与分配闭环。

**商城比较：用户指定 macrozheng/mall 作为比较对象，随后澄清只比较并回查既有“不要引入太大系统”的决定；不构成引入 mall、mall-swarm 或替换资产权威的授权。** 原仓 docs/design/decision-revisit-auction-engine.md:109 已否决整套 Java 电商替代拍卖，允许借鉴订单状态机和防超卖。旧文档的单语言/headless 理由不能直接当成本仓双版本禁令；当前取舍依据是业务适配、资产一致性、接入与运维成本。游戏固定价商城、玩家交易与价格时间优先的拍卖撮合是不同职责，不能以通用电商的订单模块代替拍卖。

商城比较优先评估商品配置/限购/订单/幂等扣币发货的最小完整能力及可复用后台；mall、Medusa、Vendure 的功能与依赖仅作候选事实，Star 数不证明本游戏吞吐、TiDB 兼容或全球强一致。玩家物品与货币仍由现有 C++ 权威管理，任何框架都不得新增并行资产真相；跨服务资产操作、可靠投递、结果未知与恢复必须逐条验收。尚未决定采用新的商城框架，也未修改原仓。

## D-17 采用 mall，分批适配全球游戏经济（2026-10-15，覆盖 D-16 的比较边界）

**用户最新明确授权：按 macrozheng/mall 适配聚宝斋、游戏道具买卖和拍卖行，能复用的直接复用，全球同服、强一致性；D-15 八域与 D-16 匹配继续移植。** 本条替代 D-16 末两段和可行性 §13.6 / §13.7 中“仅比较、未采用”的当前决策，不删除历史核查。dialogue、player、inventory、mission、scene_manager 仍不迁移；现有 C++ 在线资产权威保持。独立客户端的既有授权继续有效。

### D-17.1 复用来源与职责

- 官方源固定为 `macrozheng/mall@dcaa93b3150352e5044708d7211b5bed0af4509f`，Apache-2.0。选定代码保留 LICENSE、上游路径、内容 SHA-256 与修改说明；不把本机 build 下的只读快照当部署依赖，不自动执行上游 Docker 构建或导入整份示例数据库。
- 复用 PMS 商品/SKU、分类属性与运营管理，OMS 订单/明细/历史及可适配的后台；库存、支付、取消等经济写路径必须加固，并关闭原样直接改支付状态、无归属查单及后台 `REPLACE` 重置预占的入口。框架提供的会员积分、库存与购物车不是玩家背包或第二份游戏钱包。
- 聚宝斋人民币寄售、官方固定价商店、玩家一口价寄售、限时竞价与同质材料价格时间撮合分别保留规则。mall 不自带后两种撮合，原 Go auction 的撮合/成交意图按既有能力适配为游戏扩展；唯一装备、宠物和角色不能降格成 `config_id + quantity`。
- 主仓新增独立 `java/mall_game` 进程边界，不把订单事务放入 zone 目录网关。Java 版按其独立服务/存储与 namespace 约束另行适配，仅共享既有客户端契约；不把跨版本混部或共享内部源码当双版本已完成。
- **第三方 schema 的限定例外**：mall PMS/OMS 的游戏适配表采用审查后的 SQL 版本迁移，不强制反向生成一套同义 proto；首批为裁剪/扩展后的独立存储合同，金额改最小单位整数，不能指向上游实物电商库。游戏客户端契约、现有 trade 表仍按 proto 与原迁移器；不得直接写生成树，也不把 mall 示例 DROP/账号/种子写进游戏库。该例外只覆盖实际复用的第三方 commerce 表，不改变 D-14 对其他服务的要求。

### D-17.2 全球权威与强一致边界

所有区域的同一市场写入同一个逻辑 TiDB 权威域，不建立各区独立可成交副本；地域是访问/战斗放置属性，不是市场隔离。订单、预占、托管所有权和卖家结算的可见状态按同一事务内行锁/CAS与唯一约束裁决，失败回滚；Redis、搜索和异步展示不裁决余额、可买数量或成交。权威读不能静默读陈旧副本。生产连接验证真实 TiDB，物理连接保持 RC 与 pessimistic；跨地域部署延迟和故障下的可用性须实测，不宣布 2000 万 CCU 达标。

原子范围只含同一数据库事务实际覆盖的事实。C++ 内存、Redis、Kafka、外部支付渠道不自动参与该事务：

1. 玩家商品开放前，先按唯一操作号与 owner_epoch 围栏持久托管，确定卖家不能再使用；资产快照与托管所有权只有一处权威。SQL 中的商品占用标记不能代替玩家侧真实扣出。
2. 成交在 TiDB 内原子确定托管归属、订单和结算意图，并同事务写交付 outbox；玩家背包以相同操作号幂等接收。未获得持久交付证明保持“交付中”，不报完成；结果未知查询/重投原操作，不创建第二次成交。
3. 支付回调需独立可信边界、签名/商户/金额/币种/订单匹配与渠道流水唯一性。付款、取消与退款互斥推进；普通玩家及运营接口不能自行声称已支付。外部渠道与本地账本按可查询、可补偿状态机恢复，不能称跨系统单次 ACID 提交。
4. 当前 C++ AssetOp 的 durable 依据 Redis 成功快照，DBTask 的 acks=all 仅证明 Kafka 投递，并非 SQL 提交；当前实例扣出/快照也尚未接通。以上缺口补齐及故障验收前，人民币寄售和唯一实例交易不能按“强一致已完成”开放。Java 资产通道的 SQL 确认较强，不代表两版交易闭环均已完成。

### D-17.3 首批与后续验收

先落一个真实可运行的官方固定价 SKU 订单切片：复用上游模型，受鉴权入口、全局数据库的幂等下单/原子预占、支付/取消竞争与待交付状态。没有资产接线不得伪造发货、没有支付证明不得写已付；这批不是完整聚宝斋或拍卖交付。随后补持久资产确认与实例托管、现有 trade 的门面/协议和客户端、卖家结算与恢复、竞价/订单簿，再接 mall 管理后台的受限经济命令。聊天等其余已确认服务并行按原仓对账，不等待商城全部完工。

每批交付列实际复用代码、真实调用链、两版差异和运行证据；并发库存/幂等重放、支付取消竞争、故障回滚、提交后崩溃、跨区权威读与资产重复交付逐项验收。单元/H2 证据不替代真实 TiDB，安全拒绝未配置支付不等于已接支付渠道，局部可运行不等于全域或容量完成。

### D-17.4 首批实际交付与边界（2026-10-15）

主仓 `java/mall_game` 与 Java 仓独立 `game-mall` 已落可运行的官方 SKU 订单进程。首批直接复用固定提交的 `PmsSkuStock`、`OmsOrder` 模型及库存双计数结构，去除展示用 Swagger 注解，逐文件保留修改声明；不是全量 mall-admin/portal 移植。原后台、类目与运营功能仍在后续复用清单。两版各自保存上游/适配文件 SHA，实际 Boot jar 含 `META-INF/LICENSE`、`META-INF/UPSTREAM.properties` 和本版 SQL 安装文件。

当前真实入口为受签名保护的下单/查单/取消与可信支付收据。订单、库存预占/扣退、唯一渠道流水及交付意图同事务，支付与取消争同一订单行锁，幂等键换 SKU/数量拒绝；未付单数在每买家独立 guard 下复核。完整 uint64 玩家身份使用规范十进制字符串，不挤入 mall 有符号会员号；不透明请求/流水/nonce 按二进制比较，金额最小单位整数并校验溢出。默认未付上限 5、支付窗口 15 分钟；没有渠道关单证明不自动释放未知支付，取消后迟到付款依赖渠道适配器持久回执与退款/对账，尚未接完。

生产模式不能退回非 TiDB；每个物理连接 RC+pessimistic+leader、禁陈旧读与低精度 TSO、严格模式与有界等待。版本/必需列/金额类型/二进制标识/唯一约束在 HTTP 接客前核查。nonce 独立短事务先做本请求非锁主键点读，确保 TiDB lazy BEGIN 已初始化，再由同事务 TSO 验窗和唯一键消费；真库首败日志保留，未以本机时钟或跳断言替代修复。依据：[TiDB 当前事务 TSO](https://docs.pingcap.com/tidb/stable/tidb-functions/#tidb_current_tso)、[低精度 TSO 会读旧数据](https://docs.pingcap.com/tidb/stable/system-variables/#tidb_low_resolution_tso)。

| 运行证据 | 结果 | 日志（主仓 build 下，独立测试产物） |
|---|---|---|
| 主仓默认 H2/真实 Tomcat HTTP/启动拒绝及打包 | 21/21，无跳过，package 成功；覆盖 context-path 与 servlet-path | `mall-game-role-prefix-package-20261015.log` |
| 主仓实际 TiDB 8.5.2 原订单契约＋环境 | 12＋2＝14/14，无跳过；四条同时持有连接验会话 | `mall-game-tidb-final-20261015.log` |
| 主仓生产门禁＋真实 TiDB＋真实 Tomcat 全 HTTP 链 | 支付角色修复后 1/1，无跳过，随机库清理成功 | `mall-game-role-tidb-http-20261015.log` |
| Java 独立版 H2/HTTP/namespace＋真实 TiDB | 19/19，其中 4 项真 TiDB，无跳过；六 reactor package 成功 | `mall-java-verify-20261015/final-tidb-package.log` |

本地 Docker 恢复后才完成真库验证；随机新库按前缀隔离并清理，没有重用已有游戏库。主仓 30 个事务测试随机库清理审计 `mall-game-tidb-cleanup-audit-20261015.log` 为 remaining=0，HTTP 与 Java 本版另有各自 fixture 清理。Java 状态已在 `PARITY.md` 登记，版本仍 `0.1.0-SNAPSHOT`，本轮未发布。

支付权限另有真实 HTTP 红→绿证据：部署 URL 前缀后，旧实现按完整 URI 选择角色，错误接受网关签名付款；两版已改为按 servlet 内业务路径识别角色，完整原始 URI 继续参与签名，并在付款控制器再次要求支付角色。主仓两种前缀和 Java 的前缀/内部 forward/错误签名均有回归。失败日志分别为 `mall-prefix-red-577cd4a9/prefix-test.log` 与 `mall-java-verify-20261015/context-path-red-complete.log`，未通过删除断言或跳测试处理。

**尚未完成：** gate/独立客户端购买接线、完整 mall 管理后台、人民币渠道关单/退款/提现、玩家唯一实例托管、C++ SQL 持久交付确认、角色过户、P2P 寄售、限时竞价/原 Go 订单簿适配。当前付款只到 `DELIVERY_PENDING`，不会假报已发货；D-15 其余服务与 D-16 匹配仍按逐能力清单继续，不据本批声明整域完成。没有跨地域生产部署/故障与 2000 万 CCU 的运行证据。

### D-17.5 权威读连接收尾（2026-10-15，覆盖上一节的产物指纹）

两版商城的每个新物理连接现显式固定 `READ-COMMITTED`、`pessimistic`、`tidb_snapshot=''`、`tidb_read_staleness=0`、`tidb_low_resolution_tso=OFF`、`tidb_read_consistency='strict'`、`tidb_replica_read='leader'`。仅悲观事务不能约束事务外列表或鉴权时间窗读取，禁止历史快照和 weak 必须明确设置。启动复核在同一条被持有的实际连接上执行全部检查；不通过即不接客，不能分别借还连接后拼接证明。强制策略覆盖 JDBC URL 中的弱读/历史会话，连接重建仍执行；没有修改 TiDB GLOBAL 参数。[历史快照](https://docs.pingcap.com/tidb/stable/read-historical-data/)与[弱读变量](https://docs.pingcap.com/tidb/stable/system-variables/#tidb_read_consistency)的官方语义用于确定边界；follower 的正常 ReadIndex 路径本身仍可保证一致性，本项目固定 leader 不代表“所有 follower 读都是陈旧读”。

本轮真实 TiDB 8.5.2 的两版各三个新增/受影响执行例通过：同时持有四个不同物理连接逐项验证，以及分别污染 weak/历史 snapshot 后拒绝启动；旧代码对应红证据保留。随后对同一源码重新执行主仓完整订单/环境 16/16、真实 TiDB＋Tomcat HTTP 1/1，Java 全商城 21/21（含 6 项真 TiDB）及六 reactor package，全部零失败、零错误、零跳过。主仓当前默认 21/21 和 package 也通过。真实 MySQL 未单独复跑，默认 H2 不作其替代证据。

最终证据为 ignored `build/mall-strict-session-20261015/summary.json`、`business-summary.json` 与 `business-` 日志；376 个构建输入及六处源改动与被测快照一致，随机商城测试库剩余零。主仓 jar SHA-256 为 `CAACB12ADF89F6487186C32AB76F5CCF84B95400C9B57A53F315E2028680259D`，Java 最终业务 package 的 jar 为 `0EC60A8C092009959457BF9EAC6B39B481A46222225AF03BD9E5B3C9C1042A2D`，许可证、上游来源及 SQL 仍与输入相符。此前 19/14 项与旧 jar 是上一快照证据，不能混作最终产物；功能及全游强一致的未完边界仍按 D-17.4。

### D-17.6 聚宝斋人民币寄售的落点与两处已批准偏离（2026-10-19，用户拍板）

用户在 2026-10-19 确认：[人民币寄售计划](jubaozhai-rmb-consignment-plan.md) §5 中 D1–D4 全部按推荐执行；D5、D6 已由相关会话对齐。现有设计保存在该计划中，独立正式设计稿尚未落入仓库。

- **落点**
  - 人民币订单与唯一买家：`java/mall_game` 的一次性挂单商品（库存 = 1），复用现有的 `lock_stock` CAS，以及"付款与取消争同一行锁"的逻辑。
  - 柜台原件：`go/trade` 新开合同 kind 9，原件仍按完整实例字节托管。
  - 卖家人民币收益账：商城库，与发货回执在同一事务里写。
  - 客户端：新开 4 个专用 RPC，不复用游戏币寄售的入口。
- **偏离一（D-17.2 导语段与第 2 条）**
  - 原文要求托管归属、订单、结算意图、交付 outbox 同在一个 TiDB 事务里裁决。本方案中，原件在 trade 库，订单和收益在商城库，做不到同一事务。要做到只有两条路：两个服务写同一个库，违反 D-14"每服务一库"；或者原件进商城，违反"商城不持有游戏资产"。
  - 替代机制：
    - 唯一买家由商城库存 CAS 裁决；
    - 退货前必须拿到商城的下架结果 CLOSED，并写墓碑；
    - 付款意图必须命中合同上冻结的快照，并按订单号幂等；
    - 收益账与发货回执在同一事务里写；
    - 三方常驻对账。
  - 这不是 D-17.2 第 3 条意义上的"外部渠道恢复"。第 3 条只管外部支付渠道与本地账本之间。
- **偏离二（D-17.4"没有渠道关单证明不自动释放未知支付"）**
  - 本机 mock 阶段与真钱上线前：挂单订单到期由商城直接关单、释放库存，关单后才到的付款只退款、不复活订单。
  - 真钱阶段（计划第 13 步）：改为先向渠道查单或关单，再释放库存。
- **仍不放宽的部分**：D-17.2 第 4 条的全局强一致边界、真实渠道与合规门禁不变；本节不代表人民币寄售可以上线。
