# java/ —— Spring Boot 服务

| 目录 | 端口 | 职责 | 状态 |
|------|------|------|------|
| `gateway_node/` | HTTP 8081 | 客户端的第一个入口:区服列表、分配 gate、公告、管理接口 | 本地一键启动与 Kubernetes 部署都会用到 |
| `mall_game/` | HTTP 18120（缺省仅本机） | mall 官方固定价 SKU 订单适配：幂等下单、库存预占、可信支付、取消与待交付意图 | 首批独立进程；尚未接 gate、游戏资产、真实支付或原 mall 管理后台，不随现有一键脚本自动启动 |
| `config_node/` | HTTP 8090 | 配置表查询服务,导表器 Java 产物的落点 | 只在导表器的 CI 里编译,日常不启动 |
| `springboot_satoken_auth_starter/` | — | Sa-Token 认证的示例应用 | 仅 `dev.bat start-satoken` 与 robot 的 satoken 登录方式使用 |

## gateway_node

客户端登录前只和它说话:

1. `GET /api/server-list` —— 拿区服列表与各区状态
2. `POST /api/assign-gate` —— 选区后要一台 gate;网关经 gRPC 调 `login`,返回 gate 地址与 token
3. 客户端用拿到的地址与 token 直连 gate,之后不再经过网关

区服状态有两个来源:管理接口写进 MySQL 的人工状态,以及对 etcd、Redis 的自动探测;人工状态优先。
管理接口(`/admin/**`)需要 `X-Admin-Key` 请求头。

区服管理接口 `/admin/zones` 的并发与错误约定(2026-11-12):

- **改整行要带版本**:先 `GET /admin/zones/{id}`,响应头 `ETag` 是这一行的当前版本;改完整行 `PUT` 回去,请求头 `If-Match` 原样带上那个 ETag(连双引号)。没带或带 `*` 回 428,这期间有人改过(版本不符)回 412,都不写;412 时重新 GET,在新的整行上重做修改再 PUT。比较与写入在同一条条件 UPDATE 里,两个运维先后 GET 再 PUT 时,后到的那个拿 412,不会覆盖前一个人的修改。`If-None-Match` 带上次的 ETag 且没变时 GET 回 304。
- **命令不带版本**:`/maintenance`、`/open`、`/create-role/close|open`、`/role-capacity` 写的是这几列的目标值,不要求 If-Match(带了也不看);它们照样让版本加一,基于更早 GET 的整行 PUT 因此回 412,不会把刚置的维护状态改回开放。
- **错误体**:400、412、428 回 problem details(`application/problem+json`):`reason` 是原因码(`missing`、`invalid`、`too_long`、`mismatch`、`if_match_invalid`、`if_match_required`、`version_mismatch`),`field` 是出问题的 JSON 字段名或 `If-Match`(问题在整个请求体时没有这一项),`detail` 是英文说明;都不含请求里的值。`/admin/whitelist` 的 400 同形。之前 400 不带响应体,原因只在网关日志里。代码在 `AdminProblems`、`AdminZoneController`。
- **库表**:`zone_config` 加了 `version` 列(`BIGINT NOT NULL DEFAULT 1`)。网关 `ddl-auto: validate`,缺列拒启;项目未上线,已有的 `mmorpg` 库重建,或手工 `ALTER TABLE zone_config ADD COLUMN version BIGINT NOT NULL DEFAULT 1 AFTER role_capacity;`。

```powershell
cd java/gateway_node
mvn package        # 打包
mvn test           # 测试
```

需要 JDK 23。编码约定与各接口的代码位置见 [AGENTS.md](AGENTS.md)。

## mall_game

本模块使用 JDK 21、Spring Boot 3.5.16；选取官方 mall 固定版本的模型与库存订单结构，来源、修改和 SHA 在 `UPSTREAM.properties`，第三方许可证为 `LICENSE`，两文件随 jar 放入 `META-INF/`。当前只完成官方 SKU 订单到 `DELIVERY_PENDING`，不提供伪造支付、完成发货或直接修改玩家资产的入口。后续聚宝斋、拍卖和后台接入见 [移植决定 D-17](../docs/design/xuanming-port-decisions-20260910.md) 与 [聚宝斋设计](../docs/design/jubaozhai-market.md)。

```powershell
cd java/mall_game
mvn test
mvn package
java -jar target/mall-game-0.1.0-SNAPSHOT.jar
```

启动前由运维在本地/开发环境的**独立商城库**安装 `src/main/resources/db/schema.sql`（只用于全新 v1，不能向原 mall 实物电商库导入），应用不自动建表。数据库连接通过 `COMMERCE_JDBC_URL`、`COMMERCE_DB_USER`、`COMMERCE_DB_PASSWORD` 注入；`COMMERCE_GATEWAY_SECRET` 与 `COMMERCE_PAYMENT_SECRET` 是两把不同且至少 32 字节的服务间密钥，必须从环境注入，不能放入客户端或日志。缺省要求 TiDB，缺表、错误存储合同或关键唯一索引缺失拒启；测试采用 H2 显式初始化，不是生产存储方案。

`COMMERCE_RUN_MODE` 缺省 `prod`，只接受 `prod/dev/test`；生产模式不能关闭 `COMMERCE_REQUIRE_TIDB`。每个物理连接设置 RC、悲观事务、leader 读取、`tidb_read_staleness=0`、`tidb_low_resolution_tso=OFF`、严格 SQL 模式及有界等待，schema 校验在 HTTP 接客前完成。

`POST /api/orders`、`GET /api/orders/{orderId}`、`POST /api/orders/{orderId}/cancel` 只接受可信网关代会话主体发起的签名请求；`POST /internal/payments` 使用独立支付调用方密钥，付款控制器再次复核角色。canonical v1 在 `HmacAuthentication`，包含角色、完整 uint64 十进制主体、时间、nonce、方法、完整原始 URI 和正文 SHA-256；部署 context-path/servlet-path 时仍须签入全部前缀，角色按 servlet 内业务路径判定。同业务重试保留 requestId、重新生成 nonce。这里的网关调用方和真实支付适配器尚未接线，签名接口通过测试不代表玩家已经能购买。

所有实例须指向同一逻辑 TiDB 权威库；不同区域不建独立可成交副本。价格以最小单位整数保存，当前订单上限见 `CommerceStore.MAX_AMOUNT_MINOR`，不是上游的元单位 decimal。每买家缺省最多 5 条未付单（可配 1–100），支付窗口 15 分钟；未核实渠道关单前不自动释放预占。收到的每笔付款都按 `(channel, transaction_id)` 在 `commerce_payment_receipt` 记一行，一笔交易全局只记一次，金额币种记渠道实收值：取消后迟到的付款绑定原单并转 `REFUND_PENDING`；异常付款不绑定订单（`order_sn` 为 NULL），另记一行 `commerce_abnormal_payment` 写明冲的是哪张订单和原因，订单、库存与交付都不动。异常付款有两类：已付订单又收到的另一笔（`DUPLICATE_PAYMENT`，2026-11-05，第一轮数据审计 §4 第 19 条），金额或币种与订单冻结值不符的一笔（`AMOUNT_MISMATCH` / `CURRENCY_MISMATCH`，2026-11-10，第二轮数据审计评审 R3；之前回 409、什么都不落，钱停在渠道里）。

`POST /internal/payments` 回 200 即表示这笔付款已落库，重放同一笔也回 200、不重复记账；应答体为 `{order, refundReason}`（`CommerceApi.PaymentOutcome`）。`refundReason` 为 null 表示这笔是订单本身的付款；非 null 表示商城已为这笔钱建好退款待办，值即原因，适配器据此区分取消后迟到、重复付款、金额不符与币种不符。同一交易号报给别的订单、或改了金额币种回 409，适配器可当终局；商城存储不变量被破坏时回 503，这笔没有记下，适配器必须保留、退避重试并告警。上面几种都生成按被退付款交易键识别的退款待办（`commerce_refund_intent.source`，取值见 `CommerceApi.RefundReason`），`/internal/refunds/poll` 每条列出要退的那一笔、实收金额币种与原因；适配器以这笔付款的交易号作渠道退款的幂等键（渠道限制长度时确定性派生），不能用订单号。`/internal/refunds/receipt` 必须带 `paymentTransactionId` 指明退的是哪一笔，只有订单本身的退款把订单推进到 `REFUNDED`；一条渠道退款流水只能了结一笔付款，复用回 409。真实渠道适配器未完成，当前不得开放真钱。已付单只记可靠交付意图；持久资产结果仍待接通。

2026-11-05 起退款两表改键、付款回执的订单列可空；2026-11-10 起 `commerce_duplicate_payment` 改名 `commerce_abnormal_payment` 并加 `reason` 列，退款待办的来源 CHECK 加了两种原因，存储合同戳随之换新（`SchemaVerifier.STORAGE_CONTRACT`）。之前安装的开发库不能就地升级（没有 V6 迁移），`SchemaVerifier` 会拒启，须按上文用 `schema.sql` 新建独立商城库重装，不要在旧库上重跑（项目未上线，库里只有测试数据）。

真实 TiDB 集成只接受本机 `4000` 端口，不接受已有库名；每例新建随机测试库，结束后只清理自己的库。测试凭证从 `COMMERCE_IT_TIDB_USER` / `COMMERCE_IT_TIDB_PASSWORD` 读取，不放进命令行：

```powershell
mvn '-Dtest=CommerceStoreTest,TiDbEnvironmentIT' '-Dcommerce.it.tidb=jdbc:mysql://127.0.0.1:4000/' test
mvn '-Dtest=CommerceTiDBHttpIT' '-Dcommerce.it.tidb=jdbc:mysql://127.0.0.1:4000/' test
```

2026-10-15 最终源码：默认测试 21/21（含两种部署前缀）、真实 TiDB 8.5.2 事务/环境 16/16、真实 TiDB＋Tomcat HTTP 1/1 均通过且无跳过。每个新物理连接和重建连接均固定 RC、悲观事务、strict、leader，清空历史 snapshot、关闭陈旧读和低精度 TSO；启动时同一连接复核。四条同时持有连接及 weak/snapshot 拒启已有真库回归。真库首跑发现的 lazy BEGIN 零 TSO 已修为 nonce 独立短事务先主键点读初始化；前缀下误接受网关签名付款也已修复，红绿证据保留。最终产物及日志见 [移植决定 D-17.5](../docs/design/xuanming-port-decisions-20260910.md#d-175-权威读连接收尾2026-10-15覆盖上一节的产物指纹)。没有压测或跨地域故障验证。
