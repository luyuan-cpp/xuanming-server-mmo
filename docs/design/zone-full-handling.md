# 区服满员处理（保留选区）

> 2026-11-10 定案并落码（负责人选「保留选区」：zone 对玩家仍可选，满员时排队、标爆满、把新玩家引到空区）。
> 状态：服务端、网关、部署脚本、客户端都已落码，**未生成协议、未编译、未测试**，验证命令见 `docs/PROGRESS.md` 2026-11-10「区服满员处理」条目。
> Java 版待做，登记在 Java 仓 `PARITY.md`。

## 1. 要解决的问题

玩家在区服列表里选区，每个区（zone）同时在线有上限。改动前：

- 登录排队的区容量（login 配置 `Queue.ZoneCapacityOverride`，K8s 开服档 45000）与 gate 台数（`deploy/k8s/zones.json` 每区 2 台、每台 `GateMaxConnections` 20000）各写各的。排队一打开，第 40001～45000 名被正常放行、拿票连 gate 才被无声拒掉（gate 拒连是 `forceClose`，不回提示）。
- 区服列表的「爆满」按网关区服目录 `zone_config.capacity` 算，与排队用的容量是两份数字，gate 不够时列表还显示「流畅 / 繁忙」，玩家点进去却在排队。
- 「推荐」是运维手工勾的，区满了也不会让开，新玩家默认被选进满员区。
- 没有「满员区不再接受新角色」的手段。

## 2. 方案总览

| 环节 | 做法 | 位置 |
|---|---|---|
| 区容量的唯一来源 | 网关区服目录 `zone_config.capacity`（运维经 `PUT /admin/zones/<id>` 改）。login 不再有自己的容量配置 | §3 |
| 区服策略传给 login | 本区网关把 `capacity` 与 `create_role_closed` 写到 etcd `/mmorpg/zone_policy/zone/<id>`，本区 login 每 2 秒读一次 | §3 |
| 排队按 gate 承载封顶 | 实际区容量 = min(目录容量, 在接客的 gate 到 90% 水位一共能接的人数) | §4 |
| 列表「爆满」 | 负载档的分母用同一个封顶后的容量 | §5 |
| 推荐区 | 只推荐开放、能建角、不爆满的区；一个都不剩时推荐最空的开放区 | §5 |
| 暂停创建新角色 | 开关 `create_role_closed`：已有角色照常进，login 拒绝 CreatePlayer；列表标「暂停创角」。运维手工开关，或设本区角色上限 `role_capacity`，到了由网关自动关上 | §6 |
| 默认 gate 台数 | zone 清单与运维模板改为每区 3 台（3 × 18000 = 54000 ≥ 45000） | §7 |

在线高峰由登录排队承担；「暂停创角」只跟本区角色总量走（运维手工，或按 `role_capacity` 自动），不跟随某一时刻的在线人数自动开关（否则排队进来的新玩家会在建角时被拒，体验更差）。

## 3. 区服策略：一个来源，经 etcd 发布

- **权威数据**：`zone_config` 表（网关自有逻辑库 `mmorpg`）。新列 `create_role_closed BIT(1) NOT NULL DEFAULT 0`。
  - 只经 `POST /admin/zones/{id}/create-role/close`、`/create-role/open` 改（同事务先按主键加锁判存在，再单列 UPDATE，再回读；与 `/maintenance`、`/open` 同一写法）。POST 新建取库缺省（开着），PUT 整行替换不碰它。
- **发布**：`java/gateway_node` 的 `service/ZonePolicyPublisher`。
  - 只发布本副本所在区（`gateway.home-zone-ids`，K8s 渲染成 `[本 zone]`，类 `config/HomeZoneProperties`，与健康汇总发布者共用），写量随 zone 数线性增长。
  - 每 5 秒一拍：点读 etcd 现值，与区服目录这一行不同才写（读不产生 etcd 版本，稳态零写入）；同区多副本写同一个值，不选主。
  - 不挂租约：策略是持久配置，网关进程退出不该让它消失。区服目录里没有本区时不写也不删（assign-gate 回 404，玩家到不了 login）。
  - 值是 JSON `{"capacity":45000,"createRoleClosed":false}`。字段名是 Java / Go 之间的契约：金样 `go/login/internal/logic/pkg/zonepolicy/testdata/policy.golden.json`，两边单测都核对；键前缀 Java 单测从 Go 源码核对。
  - 不用网关 Spring 的全局 ObjectMapper（它按 SNAKE_CASE 改名）。
- **读取**：`go/login/internal/logic/pkg/zonepolicy`（`Reader`），装配在 `svc/zone_policy.go`。
  - 每 2 秒点读本区一个键；读失败、值不合法保留上一次的值；键不存在清掉。
  - 读不到区服策略时：排队容量退回软上限（`max(在线,1) × SoftCapMultiplier`，仍按 gate 承载封顶），建角不拦（fail-open：这是运营节奏上的软开关，拦错的代价是整区建不了角）。
- **为什么不用 Redis、不用 RPC**：读方只有本区 login、只读一个键，etcd 单键点读足够便宜；策略是配置，要持久、强一致，不该随网关存活而消失。login 不能跨库读网关的表（AGENTS §7.6），反过来让网关调 login 也不对。
- **生效时延**：运维改表 → 网关至多 5 秒写到 etcd → login 至多 2 秒读到。区服列表随 `zone_config` 行缓存 1 秒生效。

## 4. 排队按 gate 承载封顶

代码 `go/login/internal/logic/pkg/loginqueue/capacity.go`（`capByGates`），规则 `go/shared/gatepick/capacity.go`（`WatermarkCapacity`、`AdmissionCapacity`）。细节与推导见 [login-queue-2026-05.md](./login-queue-2026-05.md)「FreeSlots 公式」：

- 每台 gate 到水位的人数 = ceil(max_connections × 90%)，恰是挑 gate 判「满」第一次成立的人数（区级准入与挑 gate 同一条线）。
- 只数在接客的一档（没停写、没排空的；没有时退到没停写的；全部停写时数全部），在线人数仍数全部候选。
- 有 gate 没声明上限（dev 下 `GateMaxConnections = 0`）时不封顶。
- 观测：`login_queue_capacity{zone}`、`login_queue_configured_capacity{zone}`；前者小于后者持续 10 分钟报 `LoginQueueCapacityLimitedByGates`（`deploy/k8s/login-alerts.yaml` 组 `login.queue-capacity`）。读区服策略的结果看 `login_zone_policy_refresh_total{outcome}`。

## 5. 区服列表：爆满与推荐

- **爆满**（`ZoneHealthProbeService.effectiveCapacity`）：负载档分母 = min(`zone_config.capacity`, Σ 本区 gate 的 ceil(max_connections × 90%))。目录容量 ≤ 0 时只看 gate；有 gate 没声明上限时不封顶。网关看不到 gate 的排空标记与停写判定，这里数全部 gate：排空、卡住期间列表比排队乐观一点，接受。90% 常量 `GATE_FULL_PERCENT` 由单测从 Go 源码核对。
- **推荐**（`ServerListService.applyRecommendation`，`recommended` 字段的含义从「运维手工值」变为「服务端算出的推荐」）：
  - 运维勾的推荐只在区服显示 OPEN、没暂停创角、负载档不是爆满时保留（负载档未知时照留）；
  - 一个都不剩时，推荐负载率（在线 / 分母）最低的开放且能建角的区，并列取 sort_order 靠前的；这些区全爆满时仍推荐最空的；负载率都未知时不补推荐。
  - 维护、关闭、预告中的区不再推荐（默认选中它就登不上）。
- 新字段 `create_role_closed`（区服列表 JSON，旧客户端忽略）。

## 6. 暂停创建新角色

- login 在 CreatePlayer 第 2 步（取到账号）之后、第 3 步（建角锁）之前判断，拒绝路径不碰任何存储：回 tip `kLoginZoneCreateRoleClosed`「该区服已暂停创建新角色，请选择其他区服」（`data/tip/Tip.xlsx` 登录段新行，码号由导表器分配）。
- 只拦新建：账号在本区的已有角色照常登录进场；Login 角色列表、EnterGame 不受影响。
- 客户端：区服列表副标题加「暂停创角」；建角被拒时显示上面的文案并回选服界面（`GameClient.CreatePlayerCo`）。
- 什么时候开关：按本区角色总量（开新区导量时关老区）。运维可以手工开关（`POST /admin/zones/<id>/create-role/close|open`），也可以给本区设角色上限，到了自动关，见下面「按角色上限自动关」。

### 按角色上限自动关（2026-11-12）

- 配置：`zone_config.role_capacity`，0 = 不设上限（缺省）。只经 `PUT /admin/zones/<id>/role-capacity`（Body `{"role_capacity": 30000}`）改，PUT 整行替换不碰它，新建区服取库缺省 0。
- 角色数：本区一共建过多少角色，即 `player_route` 里归属落在本区的行数。按合服之后的最终区算（被合进来的源区一起数；问一个已被合走的区，按它的最终区回答，`placement.MergedZones.Members`），删角不减（删角只在账号目录写墓碑，路由行不删）。这是导量口径的「注册角色数」，不是活跃人数。
- 链路：本区网关的 `RoleCapacityGuard` 每 10 秒（`role-capacity.check-interval-ms`）问本区 login `LoginPreGate.GetZoneRoleCount`，login 转问 data_service `CountZoneRoles`。data_service 在目录 K 片上各数一次再求和，走 `idx_route_home(home_zone, …)` 的索引范围，只读索引不回表；任一片失败整次报错，不回部分和。只有设了上限的区会被问，问量随设了上限的区数线性增长，与网关副本数无关。
- 网关不直连 data_service：沿用已有的按区 login 通道（分区选路、保活、发现都现成），不新增网络边与部署配置；login 只把本区的数原样带回，问到别区回 InvalidArgument。
- 关闭：角色数不低于上限、开关还开着时，比较并写 `UPDATE … SET create_role_closed = TRUE WHERE zone_id = ? AND role_capacity = <判断时读到的上限> AND role_capacity > 0 AND create_role_closed = FALSE`。运维在判断之后调了上限、置 0 或已手工关上，这次关闭一行都不改，下一拍按新值重判；同区几个网关副本同时判断，也只有一个生效。关上之后的生效路径与手工关完全相同（列表、推荐、login 拦截），网关记一条 WARN。
- 只关不开，电平判断：角色数不低于上限时，手工打开会在下一拍被关回去。要放开，先调高上限或置 0，再 `/create-role/open`。
- 问不到不关：login 或 data_service 不可用时只记这一区的 error（`gateway_role_capacity_check_total{outcome="error"}`），下一拍再问。问不到不等于满了，建角本身也要经过这两个服务。持续 10 分钟问不到由 `GatewayRoleCapacityCheckFailing` 告警（`deploy/k8s/gateway-zone-policy-alerts.yaml`）。
- 软上限：到线之后，至多一个检查周期（10 秒）、一次策略发布（5 秒）与 login 读策略（2 秒）内建出的角色会超出，约 17 秒的建角量。硬上限要在建角事务里串行化一个按区计数行，一个区的建角吞吐被压成单行写，与水平扩展冲突；导量上限本来就是软目标，所以按软上限定。
- 观测：`gateway_zone_role_count{zone}`、`gateway_zone_role_capacity{zone}`（只有设了上限的区有序列，上限置 0 后后者回 0）；data_service 侧 `data_service_route_read_total{path="zone_count"}`。

## 7. gate 台数

`deploy/k8s/zones.json`、`zones.sample.json`、`zones.sample.yaml`、`zones.ops-recommended.yaml` 每区 gate 由 2 改为 3：区服目录缺省容量 45000，每台 gate 到水位 18000，2 台只到 36000。`zones.10zones.yaml`（单机 10 区联调）保持每区 1 台，容量由封顶自动压到 18000。部署脚本的 `-GateReplicas` 缺省（只用于不带 zone 清单的单区 zone-up，排队关着）不变。运维规则：gate 副本 × 18000 ≥ `zone_config.capacity`，否则改小容量或加 gate。

## 8. 迁移与兼容

- 项目未上线，`zone_config` 加列不做在线迁移：已有的本机 / K8s `mmorpg` 库要重建，或手工执行
  `ALTER TABLE zone_config ADD COLUMN create_role_closed BIT(1) NOT NULL DEFAULT 0 AFTER sort_order;` 与
  `ALTER TABLE zone_config ADD COLUMN role_capacity INT NOT NULL DEFAULT 0 AFTER create_role_closed;`（网关 `ddl-auto: validate`，缺列拒启）。
- 角色上限的链路加了两个 RPC（login `LoginPreGate.GetZoneRoleCount`、data_service `CountZoneRoles`），都是新增方法。新网关配旧 login 时调用回 UNIMPLEMENTED，按「问不到」处理、不关；不设上限时根本不调。
- login 删了 `Queue.ZoneCapacityOverride`，部署脚本不再渲染它。旧 ConfigMap 里残留这个键时 go-zero 忽略未知键，不影响启动。
- 区服列表加字段、`recommended` 改为服务端计算，都是向后兼容的 JSON 变化。新 tip 码要 Codex 跑导表器（服务端与客户端生成物）。

## 9. 残余与后续

- 排队仍受合入门禁关着（`$script:LoginQueueMergeGateCleared = $false`）：排队关着时快速通道不判容量，§4 不起作用，gate 硬上限兜底。
- 按角色上限自动暂停创角（§6）2026-11-12 落码，未编译、未测试。
- Java 版：区服目录、推荐、建角拦截、排队容量封顶、按角色上限自动关都待做（Java 的排队在 game-gateway 里，容量本来就取区服目录；gate 还没有连接上限字段）。
