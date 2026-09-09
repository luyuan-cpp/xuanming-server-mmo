# 会话制对局(MOBA 形态)目标架构基准

**Created:** 2026-08-31 · **Updated:** 2026-09-05
**状态:** 目标形态基准 + 本仓现状映射。**2026-09-05:第 7 步「客户端直连 + 票据入场」已落码、已过静态评审与编译/单测(C++ Debug 0 error、gtest 18+22 全绿、Go/robot 全绿),整栈冒烟待本机基础设施**
(见 [turn-based-battle-server.md §18](./turn-based-battle-server.md#18-客户端直连-battle-节点票据入场战斗流量零字节经-gate2026-09-05已落码待验证));
写本文时 battle 节点尚不存在,现状映射一节按 2026-09-05 的仓库状态重写。
**来源:** 架构讨论。通用骨架部分为业界标准形态(LoL / Dota2 / 王者 / 绝地求生这类"会话制对局"的通用做法),现状映射部分已逐项对照本仓代码核实。
**关联:** [moba-ds-server-interview-qa.md](./moba-ds-server-interview-qa.md)(DS 内部设计细则)、[moba-non-ds-server-interview-qa.md](./moba-non-ds-server-interview-qa.md)(外围系统)、[ARCH.md](./ARCH.md)(现有 MMO 拓扑)、[player_login_flow.md](./player_login_flow.md)(票据入场的现有实现)、[session-extractability-mmo-slg.md](./session-extractability-mmo-slg.md)(本文这套判据能/不能套到 MMO、SLG 的哪些层)

> **一句话判据:判断一个对局架构标不标准,只看一件事——战斗进程能不能被随时 kill 掉而不损坏任何持久数据。**
> 能,就是标准的;不能,就还是"网关/场景服里长了个战斗模块"。

> **定位声明:本文是在 MMO 旁边新增一个会话制对局子系统,不是把 MMO 改成 MOBA。**
> scene(常驻大世界、AOI、持久化)一行不动;battle 是与 scene 并列的新节点类型。
> 两者不能复用一套架构的论证见 [moba-ds-server-interview-qa.md](./moba-ds-server-interview-qa.md) Q1。

---

## 一、标准形态:五个角色,四条边

```
客户端 ──① 大厅长连接──▶ Gate ──▶ 业务服(Go) ──▶ DB(持久层)
   │                              │
   │                        ② Matchmaker(撮合)
   │                              ▼
   │                        ③ Allocator(申请落点 + 签票据)
   │                              │
   └──④ 战斗直连(带票据)──▶ Battle 节点 ──⑤ result 回写──▶ 结算服 ──▶ DB
                              (纯内存·不连玩家库)
```

**角色边界(这是"标准"的核心,不是进程数量):**

| 角色 | 唯一职责 | 有没有玩家库连接 | 状态可不可以丢 |
|---|---|---|---|
| Gate | 连接、鉴权、编解码、路由 | 无 | 可丢(重连即恢复) |
| 业务服 | 玩家持久化数据的**唯一权威** | 有 | 不可丢 |
| Matchmaker | 只产出"一场 match = 成员+模式+参数" | 无 | 可丢(重新排队) |
| Allocator | match → 落点地址 + 一次性票据 | 无(只写路由表) | 可丢 |
| Battle 节点 | 跑 tick、算胜负、产出一份 result | **不连** | **可丢(这是设计目标)** |

---

## 二、三条硬契约

### ① 入场靠票据,不靠信任

Allocator 签发一次性 token:`{match_id, user_id, server_id, exp, sig}`。
客户端拿它直连战斗服,战斗服**本地验签**即可放人——不查库、不回问大厅。

这是唯一不可伪造的入场通道。没有票据,战斗服要么裸奔(谁连上谁是自己人),
要么每次进人都回查大厅(把大厅变成对局链路的运行时依赖)。

**本仓优势:这个模式已经在生产链路上跑通一遍**——
`login.AssignGate` 签 HMAC `GateTokenPayload`,客户端拿 `{gate_ip, gate_port, gate_token, deadline}`
TCP 直连 gate,gate 侧 `ClientTokenVerifyRequest` 本地验签
(见 ARCH.md §3.1 八步、gate `main.cpp` 的 `ValidateGateTokenSecretOrDie` 启动门禁)。
战斗票据 = 同一模式换 payload,不是新发明。

**本仓的落地变体(2026-09-05,与上面"标准形态"两处有意偏离,理由见 turn-based-battle-server.md §18 D24/D25):**
- 签发者是 **battle 节点自己**而不是 Allocator(match):battle 是唯一知道房间名单的一方,少一处持密者;
  且 match 与 battle 各自 Kafka 生产者,由 match 签票会让"分配包"与"开战包"跨生产者无序,battle 自签
  能在同 key 上保证先分配再开战。"本地验签、不回问大厅"仍成立。
- 票据**不是一次性短 exp**,而是寿命 = 房间作废期限、可重复用于重连,不做 jti:房间销毁即全部票据失效,
  一次性表没有增益;票据绑定 `(battle_id, player_id, node_id, 实例 UUID, role)`,节点重启后 node_id 复用
  也不会让旧票复活;客户端丢票经大厅会话 `RequestBattleTicket` 补签。

### ② 战斗服吃快照、吐结果,中间不碰库

- **入场快照**:分配时把这一局需要的玩家属性(等级/装备/技能配装,算好的最终数值)
  **一次性打包下发**给战斗服。战斗服拿到的是值,不是引用,不是能回查的句柄。
- **出场结果**:`{match_id, duration, per_player:[{uid, kills, damage, rank, alive}], winner}`
  ——一个**可序列化的 DTO**,不是活对象。

这条决定了战斗服能不能真的独立成进程/容器。快照的权威源就是 data_service
(已有 `CreatePlayerSnapshot` / 事务日志 / 回滚 RPC 族),由 Allocator 在分配时拉一次打包;
**battle 节点自身不得出现 data_service / db 的客户端连接**。

### ③ 回写幂等,且不依赖发送方还活着

标准做法是 **result 先落地、再发奖**:

```
Battle ──result──▶ 结算服
                    ├─ put_unique(match_results, match_id)   ← 只落一次
                    ├─ 事务/逐人 grant_once(match_id:uid)     ← 只发一次
                    └─ status: settling → done
```

关键在于 **result 必须在战斗服之外持久化一次**。战斗服崩了、消息重发了、结算服重启了,
任何一方都能靠 `match_id` 把这一局重放到底。配套补偿:battle 心跳超时(如 15s)→
判 abandoned → 走同一条幂等结算链回滚/免罚,不能出现"崩了只能人工补单"。

> 完整参照实现在隔壁 Pandora/XuanMing 仓:`battle_result` 服务
> (同一 match_id 只落库一次、no-show 惩罚、abandoned 补偿、DS 只报事实不报数值)。
> 概念和坑都趟过一遍,照抄语义即可,不必重新踩。

---

## 三、存储的标准分层

对局子系统的存储不是"战斗服也连一个库",而是**三层各写各的**:

| 层 | 谁写 | 存什么 | 丢了会怎样 |
|---|---|---|---|
| 持久层(MySQL→TiDB) | 只有业务服/结算服 | 账号、角色、背包、货币、**战绩表** | 事故 |
| 协调层(Redis/etcd) | 各服 | `uid→gate`、`match_id→battle 落点`、匹配队列、锁 | 重连/重排,可恢复 |
| 局内状态 | battle 内存 | 位置、血量、buff、仇恨 | **设计上就允许丢** |
| 回放/大对象 | battle 异步 | replay 文件 → 对象存储 | 只影响回放功能 |

三条红线:
1. **battle 不写玩家库**(写了就毁掉"可丢"这个前提);
2. **玩家在局中,业务服要有闸**——`in_battle` 标记挡住局中改背包/交易/换装;
3. **路由表(`match_id → battle 落点`)进共享存储**(Redis/etcd),不能是进程内 dict
   ——否则重连找不回落点、多副本各持一份。

---

## 四、落点粒度:一进程一局 vs 一进程 N 房(唯一需要按项目拍板的点)

| | 一进程/一 Pod 一局(经典 DS) | 一进程 N 房(high-density) |
|---|---|---|
| 适合 | 重型 DS,单局几百 MB、几十分钟 | 纯逻辑轻量房间,单局几 MB、几分钟 |
| 调度 | 分配器直接要一台 | 计数装箱,塞满一台再开下一台 |
| 启停开销 | Pod 拉起 10~60s,要靠 Ready 预热池盖住 | 摊薄到接近零 |
| 隔离 | 一局崩只死一局 | **一局崩,同进程全死** |
| 排空 | 打完即 Shutdown | 必须等 rooms 归零才 Shutdown |

本仓 battle 是 C++/muduo 纯逻辑房间(无重型引擎),倾向 high-density;
但**未实测单房 tick 成本前不拍死**。选 high-density 就必须同时补隔离:
每房 tick 包 try/catch、单房超时熔断,一个房间的异常不能掀翻整个进程。

---

## 五、为什么战斗流量必须直连、不过 gate

**大厅一条连接(走 gate),战斗另一条连接(直连 battle,票据入场)。**

战斗流量是全场最高频的(每人每 tick 一个输入、每 tick 一份快照)。
把它塞进大厅 gate,等于让所有对局共享 gate 的序列化/带宽通道——
这时候在战斗侧加多少进程都没用,瓶颈不在那儿。
(隔壁 proj_base 项目实测过这个天花板:房间进程随便加,网关这个共享序列化漏斗卡死在 69 房。)

本仓现状对号:
- MMO 大世界流量(client→gate→scene 中继)**维持现状不动**——那是 MMO 的设计,
  gate 是纯转发层,见 [gate-scene-relay-architecture.md](./gate-scene-relay-architecture.md);
- **对局流量从第一天起就不进 gate**,客户端第二条连接直连 battle;
- ARCH.md §2 硬约束"游戏 TCP 链路零中间代理"与直连**天然一致**,不违反任何既有决策;
- gate 的客户端面基建(muduo + ProtobufCodec + session 管理 + HMAC 验签)
  整套可搬进 battle 节点做客户端入口,不必重写。

直连的代价要认:每个 battle 落点需要**可寻址的入口**(hostPort / NodePort / per-pod 端口段)、
客户端两条连接的独立生命周期管理(断线重连、票据过期重签、两边互不拆台)。

---

## 六、本仓现状映射:已有 / 缺口

### 已有(2026-09-05 核实;8 月 31 日写本文时 battle / match 尚不存在,一周内由回合制战斗一、二期补齐)

| 资产 | 位置/证据 | 对应标准角色 |
|---|---|---|
| Battle 节点(纯内存房间、不连玩家库、崩溃即作废) | `cpp/nodes/battle`,设计 `turn-based-battle-server.md` D1/D2/§8;不变量"battle 对玩家权威数据零写权" | Battle Server ✅ |
| Matchmaker + Allocator(凑单 → 选 battle 节点 → gather) | `go/match`(队列 / 挑战 / 观战匹配 / 评分),gather 管线 §3.1,补偿矩阵 §3.2 | Matchmaker ✅ / Allocator ✅(合一) |
| 入场快照 / 出场结果 DTO | `BattlePlayerSnapshot`(scene PrepareBattle 出)/ `BattleSettlementData` + `BattleSettlementEvent`(Kafka 回 scene)+ `BattleResultEvent`(回 match 评分) | 契约② ✅ |
| 幂等结算 | scene 按 `InBattleComp.battle_id` 匹配才应用,重复投递丢弃;离线 pending 7 天 | 契约③ ✅(result 落地 = Kafka at-least-once + scene 幂等) |
| 局中闸 | `InBattleComp` 冻结清单(排队 / 交易 / 改属性道具 / 切场景) | 存储红线 2 ✅ |
| 路由表 | match 侧 `spectate:*` / 票据;battle 落点由 gather 决定并写进快照路由 | 存储红线 3 ✅(Redis) |
| **客户端直连 + 票据入场** | `turn-based-battle-server.md` §18(2026-09-05 落码):battle 自签 HMAC 票据、自身 TCP 端口开客户端面、S2C 直连优先 Kafka 回落 | 契约① / 第五节直连 ✅(待编译 + 冒烟) |
| 跨 zone 匹配 + 水平扩展 | `cross-zone-matchmaking.md`;battle / match 全局池 | — |

### 仍是缺口

1. **收缩阶段**:gate 仍中继战斗消息(回落路径),Unity 客户端尚未接直连(客户端仓独立);等直连失败率数据后删 gate 中继;
2. **部署形态**:battle 无 K8s manifest,直连需要 hostPort / NodePort 暴露(C 档待办);
3. 落点粒度已由事实拍板为 **一进程 N 房**(单 battle 进程多房间,§8),隔离(单房异常不掀翻进程)只有 timer 回调按 battle_id 重查这一层,无 try/catch 熔断 —— 待补;
4. 票据吊销 / 每消息 HMAC / 观众连接上限单独配置(§18.7)。

### 已有(2026-08-31 原文,保留作历史对照)

| 资产 | 位置/证据 | 对局子系统里的用途 |
|---|---|---|
| 票据签发+本地验签全链路 | login `AssignGate` HMAC → client 直连 gate 验签(ARCH.md §3.1) | 战斗票据照抄换 payload |
| 节点框架加新类型 | `cpp/nodes/_template/` 三个 main 样例 + etcd 注册发现 + `CanConnectNodeTypeList` 白名单 + 生成的 rpc registry(`targetNodeType` 路由) | battle 节点骨架 |
| gate 客户端面基建 | muduo TCP + ProtobufCodec + SessionManager + token 验证 | battle 的客户端入口 |
| 玩家数据快照权威 | data_service:`CreatePlayerSnapshot` / 事务日志 / 回滚 RPC 族 | 入场快照来源 |
| 场景分配器雏形 | scene_manager(EnterScene 分配落点) | Allocator 的参照(参照,不是复用改造) |
| Kafka 解耦 | gate 命令通道、ARCH.md 设计原则 4 | result 回写/事件通道可选载体 |
| DS 设计共识 | moba-ds-server-interview-qa.md(2026-05-18)已写清 DS≠Scene、一局一进程定位 | 设计不必重新论证 |
| 会话权威 | Login + player_locator | `in_battle` 闸的挂点 |

### 缺口(按依赖顺序,前面不做后面做了也白做)

1. **battle 节点类型**:净新增,从 `_template` 起,客户端面搬 gate 的;**不动 scene**。
   先拍板落点粒度(§四),它决定进程模型和排空语义。
2. **战斗票据**:复用 login HMAC 模式签 `{match_id, user_id, server_id, exp, sig}`,
   battle 本地验签、一次性、短 exp。
3. **Matchmaker + Allocator**(Go 新服务,照既有 go-zero 服务模板):
   match 产物 = 成员+模式+参数;Allocator 写路由表 `match_id → battle 落点` 进 Redis/etcd,
   签票据,推入场快照给 battle。
4. **result DTO + 幂等结算**:result 先持久化再发奖;battle 心跳超时 → abandoned 补偿。
   参照隔壁 XuanMing `battle_result` 的语义。
5. **业务服 `in_battle` 闸**:局中挡背包/交易/换装写操作。
6. **客户端第二条连接**(Unity):独立生命周期、断线回局重连(票据重签)、
   两连接互不拆台;断线重连必须支持(会话制对局的硬需求,见 interview-qa Q1)。
7. **部署形态**:k8s 补 battle 的 Deployment/进程表;Ready 预热池或装箱调度,按 §四 的拍板走。

> 排序理由:1–5 是服务端闭环(做完即可用机器人整局跑通:匹配→分配→入场→打完→结算落库);
> 6 是客户端工程,可与 3–5 并行;7 放最后——前面不闭环,部署只是把跑不通的进程放进容器。

---

## 七、验收判据(开工后逐条打勾)

- [ ] 随时 `kill -9` 任一 battle 进程:局内玩家掉线、该局判 abandoned、补偿结算,**零持久数据损坏**
- [ ] battle 进程内查不到任何 db/data_service 连接(代码审查 + 运行时 netstat 双证)
- [ ] 同一 `match_id` 的 result 重发 N 次,战绩/奖励只入账一次
- [ ] 结算服重启后,已落地未发奖的 result 自动续跑,无人工补单
- [ ] 客户端杀进程重启,凭有效票据(或重签)回到原局
- [ ] 对局全程抓包:战斗流量零字节经过 gate
- [ ] (high-density 时)单房抛异常,同进程其余房间正常出结算
