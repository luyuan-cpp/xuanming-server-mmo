# 区服列表：现行 Gateway 接口与静态发布提案

> 原提案：2026-05-23；现状核对：2026-11-09。
> **现行入口是 Java Gateway 的 `GET /api/server-list`。** 对象存储与 CDN 静态发布仍是未实施的方案；本文的候选设计不是现行接口、上线门禁或运维步骤。

## 1. 当前实现

| 环节 | 当前行为 |
|---|---|
| 请求入口 | [ServerListController](../../java/gateway_node/src/main/java/com/game/gateway/controller/ServerListController.java) 直接返回列表 DTO，不经过登录和分配 Gate 的前置限流器；控制器未实现 ETag、条件请求 304 或 CDN 缓存策略。 |
| 运营目录 | [ServerListService](../../java/gateway_node/src/main/java/com/game/gateway/service/ServerListService.java) 从 SQL `zone_config` 读行，按 `sort_order` 升序。每个 Gateway 副本缓存成功读取的行 1 秒，从查库完成计时，不缓存最终 JSON，也不因管理接口更新主动失效。过期并发请求合并为一次查询。 |
| 失败与超时 | 查库失败不缓存，也不返回过期目录作为成功。等待方最多等 3000ms；超时不取消正在查库的请求。查库方另受 JDBC 配置约束，`socketTimeout=10000` 是 socket 读超时，不是整个 HTTP 请求的硬截止。 |
| 展示状态 | 组装响应时读取 [区服探测快照](../../java/gateway_node/src/main/java/com/game/gateway/service/ZoneHealthProbeService.java)，并重新判断新服。手工关闭、维护、预告优先；手工 OPEN 遇探测 DOWN 显示维护，探测 UNKNOWN 时仍显示 OPEN、但不附负载。展示为 OPEN 不代表一定可进入。 |
| 机器人 | 通用登录在 `zone_id=0` 时调用 Gateway 动态接口，取首个推荐区或第一项；显式指定区号则不读取列表。读取失败时机器人仍尝试传 0，但当前 Gateway 无法据此分配 Gate。没有本地文件或 CDN 读取模式，见 [robot 选区说明](../../robot/README.md#网关与选区)。 |

响应为 `{"zones":[...]}`，按 [ZoneInfoDto](../../java/gateway_node/src/main/java/com/game/gateway/dto/ZoneInfoDto.java) 输出
`zone_id`、`name`、`status`、`recommended`、`is_new`，以及有值时的
`load_level`、`maintenance_msg`、`open_time`。`sort_order` 用于服务端排序，不是当前响应字段；
旧提案的 `schema_version`、`published_at`、`publisher`、`etag` 和 `fallback_gateway` 也不属于当前接口。

区服列表只提供展示信息。对显式区号，[AssignGateService](../../java/gateway_node/src/main/java/com/game/gateway/service/AssignGateService.java)
在分配和持非空排队令牌的查询路径按请求中的区号检查手工状态；这一步不验证令牌所绑定的区。
未知区返回业务码 404，维护、关闭、预告返回业务码 503，
准入查询失败返回业务码 500。非正区号虽跳过这一步目录检查，但 [LoginRpcClient](../../java/gateway_node/src/main/java/com/game/gateway/grpc/LoginRpcClient.java)
要求这两个 RPC 的目标区号为正，发出 RPC 前即拒绝，由 Gateway 映射为业务码 500（`internal_error`）。
探测态不替代准入，实际 Gate 由目标区的 login 选择。
即使将来接入 CDN，静态列表也不能替代准入检查或签发 Gate 票据。登录限流与排队各自见[削峰设计](./open-server-rate-limit-design.md)
及[登录排队设计](./login-queue-2026-05.md)。

现有短缓存降低重复查库，Gateway 仍承担 HTTP 连接、响应组装及缓存未命中的 SQL 读取。仓库已有缓存、失败和合并查询的
[测试源码](../../java/gateway_node/src/test/java/com/game/gateway/service/ServerListServiceZoneRowsCacheTest.java)；
本次未运行，不能据此给出冷启动容量、源站请求量或上线结论。

## 2. 2026-05 静态发布方向（未实施）

原提案希望把**公开区服目录的读取**从业务 Gateway 分离：运营仍修改 Gateway 所属 SQL 目录，不把区服状态迁到策划表；
由发布任务生成公共快照，上传对象存储并经 CDN 分发。最初考虑阿里云 OSS、主备 CDN、本地对象存储夹具，
以及运维手动发布和目录变更后自动发布。保留这些作为原设计选择；云厂商、SDK、发布配置和管理接口尚未实现或验收。

候选读取顺序为主 CDN、备用 CDN、动态 Gateway；本地文件只用于开发。Gateway 仍为现有调用方提供动态入口，
不能提前切成“仅兜底”。本仓未接入 `ServerListPublisher`、发布端点、`serverlist_mode` 或提案的 30 秒响应缓存；
现有 `/api/cdn-sign` 是另一条签名链路，不能当成区服列表发布能力。独立客户端及独立 Java 服务端本次未核对。

静态文档的 schema 与切换条件需在实施任务中定稿。应保留现有状态值与缺省字段语义，明确版本、
生成时间、过期与回滚；ETag 和 304 要与实际 HTTP 缓存行为一起验收，不能把 JSON 中的摘要字段当成 HTTP ETag。
公开文档只携带必要的区服展示字段，操作者身份及审计详情留在管理面；回退地址只能来自可信配置，
不能仅凭下载文档内的 `fallback_gateway` 引导客户端访问任意地址。

## 3. 重启实施前要验证什么

1. **发布一致性**：从已提交目录生成并校验快照，定义并发发布的版本顺序；新快照成功后再切当前版本。
   保留可回滚版本与内部审计，分别处理上传、切换和 CDN 刷新失败。涉及探测态、负载和新服标记时，
   需先决定是否发布、由什么触发更新以及陈旧时如何展示；目录变更触发本身不覆盖探测变化。
2. **旧快照和回退**：规定客户端接收的版本与最大陈旧时间、各级超时及总预算。验证主备同时不可用或对象过期时，
   Gateway 能承受回流；区服已关闭但静态缓存仍显示开放时，以动态准入查询的手工状态为准，并验证准入短缓存的生效窗口。
3. **运维与容量**：用冷启动与回流场景测 Gateway、SQL、缓存命中和故障恢复，再确定是否需要静态化、
   缓存周期和回退限流。原稿的“开服必崩”、命中率、固定传播时间和工期估算没有验收证据，不作为容量承诺。
4. **交付边界**：先确定公开字段及管理端权限，再单独实施发布端、机器人与独立客户端适配，
   验证本地和指定测试环境。本文不创建云资源、发布静态文件或修改线上入口。
