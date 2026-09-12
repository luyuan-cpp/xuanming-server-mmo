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
