# 独立 JeecgBoot GM 管理系统

2026-11-17：用户确认采用配套 Vue 3 前端和 Java 后端，授权开始接入。GM 管理员身份独立于游戏玩家账号。

## 职责与边界

浏览器只调用 JeecgBoot；其原有登录、会话、角色及权限体系负责管理员认证。新增 `/gm/**` 每次在服务端检查认证及具体权限，操作者从认证主体读取，不接受请求提供的操作者。游戏侧密钥仅保留在后端环境中。

固定上游 v3.9.6，提交 `49c29032e18df784f1a75123d90647bc8f74cedb`。`java/gm_admin` 保存自有扩展、配置模板及构建脚本；上游源码和构建产物放 `build/gm-admin-upstream`，由准备脚本校验提交后应用扩展，避免把第三方源码混入本仓改动。独立管理库保存账号、权限及管理日志，不跨库 SQL 修改玩家。

接入范围：区服只读列表、按玩家 ID 精确查询公开资料及在线状态、个人邮件多附件发放（按权威 `CurrencyType` 支持最多 4 种不重复货币 + 最多 16 种不重复物品且总数 ≤ 4096）、全服纯文本系统公告邮件（`SendSystemMail`）、三段式幂等批量活动发奖（`SendSystemMail → ImportMailRecipients → SealMailCampaign`），以及已有七项玩家 GM：增加货币、扣除货币、封禁/解禁货币获取、设置等级、发放宝宝、发放物品。玩家 ID、金额、邮件号保持十进制字符串，不经过 JavaScript Number。查询不返回登录令牌或账号认证材料。审计列表支持按玩家 ID / 状态筛选与分页，核对弹窗可查看完整的 `gm_command_audit` 状态变更轨迹。

## 七项在线玩家 GM

既有七项 RPC 原本绑定客户端自身会话；独立后台不模拟玩家登录。新增 `scene_node.SceneNodeGrpc/GmExecutePlayerCommand`，以 oneof 复用七个现有请求类型，只调用已有域逻辑，保留原来的 dev/test 限制。目标玩家、当前节点、会话号、登录代次和审计身份全部进入签名。场景必须核实真实在线实体、就绪状态、归属代次与入场授权，并拒绝战斗、冻结、交接或退出中的目标。

管理员在网页选择操作、币种及参数，先创建冻结草稿再确认；草稿有效期五分钟。币种目录来自权威 `CurrencyType` 描述，当前包括银两、灵石、绑定灵石。等级沿用现有 1–85 范围，直接发物品沿用单次 99 个上限；邮件附件使用自身规则。

后台经 `PlayerLocator/GetSession(include_placement=true)` 在同一 Redis Cluster 槽位通过单次 `MGET` 原子读取 `player:{id}:session` 与 `scene_manager` 的权威位置键 `player:{id}:location`（`include_placement` 默认 `false`，登录热路径零额外开销，也不改写 `PlayerSession` 字段），获取玩家真实的 `(zone_id, node_id, scene_id)` 与跨区交接状态（未入场 `PLAYER_NOT_IN_SCENE`、交接中 `PLAYER_HANDOFF`、会话代次未对齐 `SESSION_MISMATCH` 均 fail-closed）。场景路由优先命中静态映射 `GM_SCENE_TARGETS_JSON`（支持 `"<zoneId>/<nodeId>"` 与单区 `"<nodeId>"`），未命中时通过可选 `GM_SCENE_ETCD_URL`（仅允许 HTTPS 或本机回环 HTTP）经 etcd v3 `/v3/kv/range` 动态发现 `SceneNodeService.rpc/` 下按 `(zoneId, nodeId)` 隔离的场景 `grpcEndpoint`（不同区同号节点互不冲突，跳过 `/allocated/` 占位键，拒绝无 `grpcEndpoint` 或同 `(zoneId, nodeId)` 脑裂双注册 `SCENE_AMBIGUOUS`，维护最多 1024 个场景连接池并在换址/下线时优雅关闭旧连接），浏览器永远不能提供地址。专用 `MMORPG_GM_PLAYER_COMMAND_SECRET` 与原停机/实体检查密钥分离，Java 使用 `GM_SCENE_ADMIN_SECRET`；HMAC 绑定完整方法、节点、操作者、冻结时间、nonce 和清空 auth 后的确定性 protobuf SHA-256。C++ 公共 GM 安全基元已统一上提到引擎层 `cpp/libs/engine/core/security/gm_security.h`（消除 scene 对 `gate_security.h` 的跨节点相对包含），`CommandCache` 按「草稿 5 分钟有效期 + 允许时钟偏差」安全回收已绝不可能再通过 `Verify` 的旧条目（未过期条目绝不提前淘汰），`BlockCurrency`/`UnblockCurrency` 成功后立即调用 `RequestKeyProgressSave` 对齐其余五项操作的持久化时机。

玩家命令与邮件使用独立命令类型及权限。七个权限为 `gm:currency:add`、`gm:currency:deduct`、`gm:currency:block`、`gm:currency:unblock`、`gm:player:level`、`gm:pet:grant`、`gm:item:grant`，创建和发送两次均检查（发送接口在查库前先验证至少持有一项玩家 GM 权限，消除 ID 探测预言机）。草稿过期（玩家 GM 5 分钟有效期、邮件草稿设定到期时间 `expireMs`）均在 `GmCommandJournal` 的 `FOR UPDATE` 事务内基于统一时钟原子拦截，超时草稿不会转入 `SENDING`。支持单命令查询（`GET /gm/mail/commands/{id}`、`GET /gm/player-commands/{id}`）与审计轨迹查询，保障弹窗内刷新不受分页与筛选条件影响。管理库迁移 `GmInstallation`（版本 1–5）通过事务首行占位 `gm_install` 主键支持多副本并发安全启动，幂等创建查询与审计复合索引，并配置 Spring Boot 优雅停机（`server.shutdown=graceful`）与最小化 K8s 存活/就绪探针放行（仅 `GET /actuator/health/liveness` 与迁移就绪后的 `GET /actuator/health/readiness`）。直接 GM 无跨进程持久去重承诺：命令一旦进入 SENDING，超时、进程退出或 UNKNOWN 后都永久禁止同单重发；完成后只返回原 EXECUTED 记录。场景另有 nonce 与进程内命令缓存，但不把它宣称为跨重启 exactly-once。EXECUTED 表示同步域操作已成功，落盘仍走现有场景持久化流程。

仓库还包含收件箱处置、资产回收、排行榜、交易人工处置、区服公告等管理 API；它们不是这七项客户端 GM，也没有统一文本命令解释器。本轮未接入这些额外操作的网页流程，不把它们列为已完成。

## 发放契约

正式发放复用 `mailpb.MailAdmin`：单人发放走 `SendPersonalMail`（支持最多 4 种不重复货币 + 最多 16 种不重复物品，总数 ≤ 4096，兼容既有单币种/单物品字段）；全服纯文本公告走 `SendSystemMail`（零玩家扩散写，拒绝附件与单人/帮会字段）；批量附件活动发奖走三段式幂等协议 `SendSystemMail → ImportMailRecipients（每片至多 1000 人）→ SealMailCampaign`（`MailCampaignManifest` 与 `go/mail/internal/data/campaign.go` 逐字节一致计算分片 SHA-256 与清单 `manifestHash`，网页单次上限 10,000 人）。均不借玩家登录、不调用仅开发服开放的客户端 GM RPC、不绕过资产围栏直接修改数据库。发送成功表示邮件已投递或活动已封存发布，附件在玩家领取后进入资产流程。

网页先创建草稿：管理员、客户端幂等号、目标玩家（系统/活动邮件为 `"0"`）、原因、标题正文、附件、活动分片与清单摘要、请求号及绝对过期时间持久化（批量活动在 HTTP 视图层仅回传前 20 位收件人样例与汇总规模，发送层读取完整冻结分片）。确认发送只接受草稿号，不能临时替换目标、名单或附件。数据库条件更新保证同一命令单飞（个人/系统邮件租约 60 秒，批量活动 120 秒）；实际 RPC 在数据库事务外执行，调用前先持久记录发送意图。超时、进程退出或活动三段中途失败后的不确定状态保留原请求，显式重试始终复用原 `sender_kind/request_id/campaign_id/manifest_generation/expire_ms` 和冻结载荷，服务端邮件幂等负责最终去重。发送不自动重试，也不把未知结果记为失败或成功。

调用签名严格采用权威 `ServiceCallAuth`：清空 auth 后确定性 protobuf 序列化摘要，HMAC 绑定完整 RPC 方法、caller=admin、时间戳和载荷。协议描述由现有 `proto/` 编译生成，不复制手写消息结构。

权限：`gm:zone:read`、`gm:player:read`、`gm:mail:prepare`、`gm:mail:send`、`gm:system-mail:prepare`、`gm:system-mail:send`、`gm:audit:read`（由 `GmInstallation` 版本 6 并发安全迁移落库并授予 `admin` 角色）。原生账号管理维护角色分配；新增经济命令的操作者、目标、原因、冻结载荷和结果在管理库留存。未配置邮件服务或签名密钥时拒绝发放；不使用假响应填充成功状态。

## 部署与验证

管理前后端、MySQL/Redis 独立启动，初始仅绑定本机 loopback。启动材料不使用上游示例口令；管理员口令与基础设施口令随机生成到忽略目录，启动时从环境注入。连接现有本机服务使用明确的 loopback 端口；非本机 gRPC 采用 TLS。

日常入口统一为 `python -X utf8 -B tools/scripts/start_docker_debug.py --execute`：游戏就绪后恢复原 GM 数据库、Redis、读取桥，再启动 Java 与网页并验收 HTTP。缺省模式仍只读预检；不下载、重建、迁移、重置口令或替换持久卷。已健康运行的组件只检查；任一 GM 阶段失败时整体失败且保留已启动服务。读取桥只接受原封印身份及严格证明的 Docker 空 DNS 表示差异，刷新动态启动状态前保留原回执；不能借此为不同容器或代码重新签发证明。

原 DataService 与 PlayerLocator 仅在 Docker 内注册真实 RPC 端口 9000/53200；宿主 2347/2349 为 Delve，不能当作 RPC。独立 GM 转发容器将固定两路发布到回环 2356/2357，运行于原游戏网络，不更改或重启这两个服务。就绪验证绑定源码、转发镜像、宿主端口、实际容器身份和 etcd 注册目标；后台读地址默认为空，未验证桥时保持禁用。Scene 写通道还须通过最终镜像与私有签名配置的独立就绪验证。

验收分别覆盖：鉴权与越权拒绝、真实协议签名、64 位 ID、草稿幂等、并发发送、邮件超时同号重放、直接 GM 未知结果禁止重发、审计持久化、前端构建及真实登录。网页地址为 `http://127.0.0.1:3101/`，已完成原生验证码登录、退出重登、十二项权限展示和七项操作目录检查，并经真实 DataService 查询不存在的角色验证读取链路。集成检查不对用户已有角色发放资产。独立 mail 已完成正式迁移、写者登记及健康检查；原有游戏容器未重启。

2026-11-20 状态更新：随着 11-18 全量重建完成并补齐 `equipartifact.json` / `equipv1affix.json`、`deploy/debug/compose.game.yml` 补入 GM Scene 环境变量与回环端口 `127.0.0.1:2355:50001`、以及 `gm_rpc_startup.py` / `gm_admin_startup.py` / `gm_scene_promotion.py` / `scene-deploy.py` 支持重建后提升与重启自愈，本机 Scene 写通道与独立 Mail 通道均已激活并通过 `probe_scene.py --execute`（未签名 `PERMISSION_DENIED`、合法签名无副作用业务拒绝 `OK` + `tipId=1006`）。同日完成服务端 C++ 与 GM 前后端强一致/水平扩展标准收口（详见 `docs/PROGRESS.md`），协议生成/编译/单测/冒烟移交 Gemini 验证。

本模块首先接当前 C++/Go/Java 网关混合服务端。独立 Java 全服务版使用不同内部管理协议，保留在 `PARITY.md` TODO，不假称已兼容。
