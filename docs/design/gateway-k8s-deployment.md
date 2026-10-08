# Gateway Service Notes

## Current Implementation: Java (`java/gateway_node/`)
- **Replaced** Go gateway (`go/gateway/`) as of 2026-04-14
- Spring Boot service with MySQL + Redis + etcd
- Much richer feature set than the original Go prototype

### Endpoints
| Category | Endpoints |
|----------|-----------|
| Player-facing | `GET /api/server-list`, `POST /api/assign-gate` |
| Announcements | announcement CRUD + display |
| Hotfix | hotfix check |
| CDN | CDN URL signing |
| Admin | zone config, whitelist, announcement management |
| Health | zone health probe (periodic gate liveness check) |

### Key Services
- `ServerListService` — zone list from MySQL `zone_config` table + etcd gate discovery
- `AssignGateService` — least-load gate selection + HMAC token signing
- `ZoneHealthProbeService` — periodic check, marks zones offline if no gate heartbeat
- `GateWatcher` — etcd watcher for `GateNodeService.rpc/` prefix
- `AnnouncementService`, `CdnSignService`, `HotfixCheckService`

### Config: `application.yaml`
- etcd endpoints, MySQL datasource, Redis, gate token secret/TTL, zone probe interval

## Previous Implementation: Go (`go/gateway/`) — DEPRECATED
- Original prototype (2026-04-05), 15 files, go-zero REST server
- Only had 3 endpoints: server-list, assign-gate, health
- Superseded by Java version for richer admin/ops features

## Design Decisions (still apply)
- **Zone-agnostic prefix**: Gateway uses `GateNodeService.rpc/` to discover ALL gates globally
- **etcd FQDN fix**: `ETCD_ADVERTISE_CLIENT_URLS` must use `etcd.mmorpg-infra.svc.cluster.local:2379` (not short name) to prevent cross-namespace resolution failures
- **HMAC token**: same algorithm shared between gateway and gate node for token verification

## 2026-09-29 更正(按磁盘代码核对;上方原文保留,不删)

- **`AssignGateService` 不选 gate、不签票,只透传 login**。上方「Key Services」里"least-load gate selection + HMAC token signing"与
  「Design Decisions」里"HMAC token: same algorithm shared between gateway and gate node"都已过时:2026-05 登录排队改造后,Java 那份选 gate + 签名的副本已删除,
  `POST /api/assign-gate` 在限流与 zone 准入之后原样转给 go-zero login 的 `LoginPreGate.AssignGate`,回包 1:1 映射成 HTTP DTO
  (`java/gateway_node/src/main/java/com/game/gateway/service/AssignGateService.java:20-48`、`:86-115`);`/api/queue-status` 同样透传 `QueryQueueStatus`(`:125-140`)。
  选最低负载 gate、签 `GateTokenPayload`(绑 `gate_node_id`)、按 `client_endpoint` 选客户端地址与去重,唯一实现都在 login(`docs/design/gate-load-balancing-design.md` 文末更正)。
  HMAC 密钥只在 login 与 gate 之间共享,gateway 不需要 gate token secret。
- **`GateWatcher` 不参与给玩家挑 gate**,有两个用途:① gate / scene 查询(`fetchAllGateNodes` / `fetchAllSceneNodes`,
  `java/gateway_node/src/main/java/com/game/gateway/etcd/GateWatcher.java:53`、`:63`)只供 `ZoneHealthProbeService` 判断 zone 有没有活 gate、算在线数
  (`service/ZoneHealthProbeService.java:76-77`);② 它同时是 `LoginNodeDiscovery` 读 login 节点的 etcd 客户端(`fetchAllLoginNodes`,`GateWatcher.java:76`;
  `grpc/LoginNodeDiscovery.java:37`、`:44`、`:58`,只用 `endpoint` 找 login)。两者都不读 `client_endpoint`;`NodeInfoRecord` 忽略未知字段
  (`@JsonIgnoreProperties(ignoreUnknown = true)`),NodeInfo 新增的 `client_endpoint=11` 不影响它。所以集群外入口(D76–D93)**不改 Java 代码**。
- **K8s 上的 HTTP 入口(集群外入口 D91,未上集群)**:`k8s_deploy.ps1 -GatewayIngressHost` 非空时由 `zone-up` 随 gateway 生成 Ingress `gateway`
  (后端 `gateway:8081`,**只路由 `/api`**,`/admin` 与 `/actuator` 不出集群),可配 `-GatewayIngressClassName`(默认 `nginx`)与 `-GatewayIngressTlsSecret`。
  启用 Ingress 时 `-GatewayTrustedProxies` 必填(写进 `gate.rate-limit.trusted-proxies`),否则全体玩家都被记在 Ingress controller 名下、共用一个限流桶。
  留空 host 不生成、也不删除已有 Ingress。参数细节见 `deploy/k8s/README.md` Optional Flags「集群外客户端入口」。
- **管理面口令(ingress 2d 批 deploy-hardening,未运行验证)**:K8s 上 `admin.api-key` 由环境变量 `MMORPG_GATEWAY_ADMIN_API_KEY` 经 Secret `gateway-admin-api-key`
  注入(容器 env `ADMIN_APIKEY`),staging / prod 缺失、占位或短于 32 位即在任何写操作之前拒绝部署;dev 档回落占位值并告警。`X-Admin-Key` 在 K8s 上是注入值,
  不是 `application.yaml` 里的值。Spring 绑定优先级按文档核对、未经运行期验证,首次部署后需在集群内用一次 `/admin` 请求实测。
- **HTTP 接口不校验 access token**:`/api/server-list`、`/api/assign-gate`、`/api/queue-status`、可选 `/api/login`、`/api/refresh-token` 均不校验 access token;
  access token 在 gate TCP 的 Login 上出示(09-29 对抗核验订正了早先"HTTP 接口鉴权"的说法;以 `java/gateway_node/.../controller/` 与 `config/AdminApiKeyFilter.java`
  的代码为准,后者只管 `/admin`)。
