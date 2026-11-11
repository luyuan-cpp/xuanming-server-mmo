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
| 暂停创建新角色 | 运维开关 `create_role_closed`：已有角色照常进，login 拒绝 CreatePlayer；列表标「暂停创角」 | §6 |
| 默认 gate 台数 | zone 清单与运维模板改为每区 3 台（3 × 18000 = 54000 ≥ 45000） | §7 |

在线高峰由登录排队承担；「暂停创角」按本区角色总量由运维决定，不跟随某一时刻的在线人数自动开关（否则排队进来的新玩家会在建角时被拒，体验更差）。

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
- 什么时候开关：运维按本区角色总量判断（开新区导量时关老区）。本批不做按角色数自动开关：要一份按区的角色计数（`player_route` 归 data_service），等有运营需求再加，不影响这里的接口。

## 7. gate 台数

`deploy/k8s/zones.json`、`zones.sample.json`、`zones.sample.yaml`、`zones.ops-recommended.yaml` 每区 gate 由 2 改为 3：区服目录缺省容量 45000，每台 gate 到水位 18000，2 台只到 36000。`zones.10zones.yaml`（单机 10 区联调）保持每区 1 台，容量由封顶自动压到 18000。部署脚本的 `-GateReplicas` 缺省（只用于不带 zone 清单的单区 zone-up，排队关着）不变。运维规则：gate 副本 × 18000 ≥ `zone_config.capacity`，否则改小容量或加 gate。

## 8. 迁移与兼容

- 项目未上线，`zone_config` 加列不做在线迁移：已有的本机 / K8s `mmorpg` 库要重建，或手工执行
  `ALTER TABLE zone_config ADD COLUMN create_role_closed BIT(1) NOT NULL DEFAULT 0 AFTER sort_order;`（网关 `ddl-auto: validate`，缺列拒启）。
- login 删了 `Queue.ZoneCapacityOverride`，部署脚本不再渲染它。旧 ConfigMap 里残留这个键时 go-zero 忽略未知键，不影响启动。
- 区服列表加字段、`recommended` 改为服务端计算，都是向后兼容的 JSON 变化。新 tip 码要 Codex 跑导表器（服务端与客户端生成物）。

## 9. 残余与后续

- 排队仍受合入门禁关着（`$script:LoginQueueMergeGateCleared = $false`）：排队关着时快速通道不判容量，§4 不起作用，gate 硬上限兜底。
- 按角色总量自动暂停创角（§6 末）未做。
- Java 版：区服目录、推荐、建角拦截、排队容量封顶都待做（Java 的排队在 game-gateway 里，容量本来就取区服目录；gate 还没有连接上限字段）。
