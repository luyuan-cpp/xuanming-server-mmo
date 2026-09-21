# 玄冥移植:开工前决策记录(2026-09-10)

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
| `team` | 按 §D7 与 `match` 同进程,`match` 已是 `Global = $true` | **`$true`**(随 match) |
| `mail` | 收件人跨 zone | **`$true`** |

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
- 回退到直连,等于 chat 等只承诺路由模式的服务同时不可达。回退前先用 killswitch 关掉对应方法,并发公告。

**理由**

1. 每加一个服务就改 gate 白名单、重编 gate、滚动重启踢在线玩家,正是 `client-rpc-router.md` §1 要消灭的成本。给 chat 补直连白名单,等于走回旧路。
2. C++ 默认值是灰度开关的安全兜底(拼错宁可留在直连)。改它会连带 battle 等既有直连路径的回归面;部署层参数则可以逐个环境翻,一行就能回退。
3. K8s 路由服 manifest 与 POD_IP 通告已补齐,但尚未在 K8s 上以路由模式跑通 battle-smoke。因此 K8s 默认仍为 0,本地默认仍为 1;按 D34 验证后再切换 K8s 默认值。

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
     - string 列可以进索引或唯一键,但应用层必须限长 ≤191 字符,因为库只按 191 前缀建索引。
     - 每表最多一个 `UNIQUE KEY`。
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
   - 不许 require `v0.1.0`:这个 tag 被移动过。
   - **2026-09-16 实测补充**:旧 module 路径 `github.com/luyuancpp/proto2mysql@v0.1.1` 的代理缓存仍是无 TiDB / unknown-fields 解码的旧内容,不能仅按版本字符串验收。`go/schemamigrate` 与主调用模块 `go/trade` 均保留旧 import/require,使用版本限定的远程映射 `replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`。新仓名发布包由官方 Go 代理核验到 `e90a5f0360eaf65794550713514a77f5a57c52a8`,包校验值 `h1:GsmiAKiXCiGjZaRCVutAsrlpShyZRcz71mVrAf+7U5A=`;两份 go.sum 由正常 tidy 生成,不关闭校验。以后新增主调用模块也须声明这条映射,因为依赖 module 的 replace 不传递。
   - **2026-09-20 补:`tools/proto_generator/protogen` 也是主调用模块,当时漏了**(`internal/generator/go/db_model.go` 用 `NewDB` / `RegisterTable` / `GetCreateTableSQL`)。它一直 require `v0.1.0`,go.sum 在两台机器之间来回改(`c149b7c57` 改 `JrtG…`、`d5f5a32ed` 改回 `da7Y…`,后者才是 sumdb 记录的原文)。缓存里没有 zip 的新机器,代理对该版本返回 404,只能回落 direct 拉移动后的 tag,每次都报 `SECURITY ERROR`。已改为 require `v0.1.1`,加同一条 replace,go.sum 换成 `GsmiAK…` / `090lb…` 两行。v0.1.1 的 go.mod 与 v0.1.0 逐字相同,依赖图不变,无需 tidy。换版本不改变任何入库文件:SQL 落盘分支因路径拼接问题从不执行,只在控制台多出 3 行 unique-key 警告。新加任何 import proto2mysql 的 module,先 `git grep -n proto2mysql -- '*go.mod'` 对齐这条映射。
   - 不许 replace 到仓库外目录。
   - **replace 只在主模块生效**(2026-09-17 补,帮会二期 B1):`go/schemamigrate` 若以 module 路径 replace 解析
     proto2mysql(例如 `=> github.com/luyuan-cpp/proto2mysql v0.1.1`),依赖它的建表服务 module(trade、guild)
     必须写逐字相同的 replace,并让各自 `go.sum` 的 `h1:` 与 schemamigrate 一致;schemamigrate 改为 require
     正式 tag 后同步删除。这不属于"replace 到仓库外目录"。
   - 本地主键 `VARCHAR(191)` 修复只在未推送的分支上。它进入某个 tag 之前,第 2 条的「禁止 string 主键」不放宽。打 tag 需要人执行(AGENTS.md §9)。
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
| data-service Deployment args 只有 `-f`;K8s 命令集没有 migrate;go/db 同类缺口 open | `data-service.yaml:54`;`k8s_deploy.ps1:8`、`:1495-1496`;`docs/design/handoff-backlog-2026-09-05.md:202` |
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
- **文档面残留**:`docs/design/friend-persistence-architecture.md:58-59`、`:79` 与 `docs/design/guild_friend_service_notes*.md:34`、`:38` 仍按 B 侧旧形态描述这道门禁。它们是 B 侧现状文档,本批不改;读到那几段时以本条为准。

**验证状态**:未编译、未运行测试(AGENTS.md §10.1),待 Codex 验证。
