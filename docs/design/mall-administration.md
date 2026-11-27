# 商城后台的受限经济命令

状态：2026-10-21 实施中，尚未编译或运行。本设计沿用 D-17 的商城订单权威与现有 PMS/OMS，不新增游戏钱包。

## 职责与接口

后台目录模块负责 SKU 条款、发布状态和库存调整；已有订单的金额、游戏映射、付款及交付记录不可被后台改写。管理查询返回包含未发布 SKU 的有界主键页，以及只读原订单视图。经济命令只接受创建 SKU、修改新订单条款和按差额调整库存，不提供任意 SQL、REPLACE、直接置已付或置已发货接口。

内部接口使用独立的 admin 角色、主体和至少 32 字节的签名密钥，不能复用 gateway/payment/delivery 密钥。未配置管理密钥时管理接口拒绝，但现有商城入口照常工作；控制器再次核角色。浏览器不持有商城签名密钥，管理界面通过既有运维鉴权代理这些固定命令。

## 提交点与幂等

每项写命令携带不透明 requestId；同一已认证管理主体与请求号只提交一次。请求摘要覆盖命令种类与完整类型化参数，同请求换条款拒绝。成功响应快照、原请求摘要和业务写入在同一 TiDB 事务提交；提交后回包丢失重放返回原响应，不能再调整一次库存。

事务按管理请求主键、SKU 主键的固定顺序取锁。新增 catalog_revision 只用于管理条款的乐观并发控制；普通下单预占、付款扣库存不增加此修订，但管理写入必须在持 SKU 行锁后使用当时真实 stock/lock_stock 复核。修改条款保留 stock/lock_stock；库存调整使用差额且调整后 stock 不小于 lock_stock，不允许绝对库存覆盖。修订号溢出拒绝。

SKU 的新价格、标题及发物映射只影响未来订单。已有订单继续读取 oms_order 的冻结价格与 commerce_order_grant 的冻结映射。成功命令日志只追加，不向普通玩家开放；认证 nonce 仍按原协议独立消耗。失败或未知提交不伪装成功，重试使用原请求号。

## 存储与部署

第三方商城的 SQL 迁移例外继续适用。新安装 schema 与 V3__catalog_admin.sql 同批增加 catalog_revision 及 commerce_admin_command；启动形状守卫核对必要列、二进制请求键、唯一约束和金额类型。V3 只给既有 V2 库升级，不在已含此结构的新库重复运行。充值稿原先预留的 V3 尚未实现，实际后续充值迁移必须使用新的版本号，不复用这个版本。

两版独立部署、独立存储，管理内部接口形状同步；真实渠道按负责人本轮决定留待以后，本批管理面不能绕过该决定开放渠道或凭空写支付事实。

## 运维入口与页面

两版内部提供 `POST /internal/admin/skus/{list,create,update,stock}`、`/internal/admin/order-view` 与人工单处置(`manual/list`、`manual/{orders,refunds,consignments}/resolve`、`manual/incomes/dispute`,见 `ManualAdminApi`);order-view 复用原订单只读视图,admin 不具有 payment 权限。Java 版管理面在 game-data,复用其 X-Game-Admin-Token、X-Game-Operator 与既有审计,方法内核认证标记,不把运维入口挪到 game-gateway。

**主仓的人用入口是后台(`java/gm_admin`)的「运营处置」页**(2026-11-26 起,`docs/design/admin-identity-and-dual-control.md` §8 批 4):

- 只读查询(人工待办、SKU 目录、原订单)直接调,要 `gm:mall:read`。
- 七条写命令(SKU 新建 / 改条款含改价 / 调库存,与四条人工处置)先在后台流水冻结成申请(`gm:mall:prepare`),由另一名管理员批准后才发出(`gm:mall:approve`)。请求号就是流水号,也就是批准号;正文带批准记录 `approval{approvalId, proposer, approver, approvedAtMs}`,被 HMAC 签名覆盖。人工处置的原因由后台填成申请上的原因,发起人不能另填。
- 商城核对批准记录(`AdminApproval.refusal`,与 `go/shared/dualcontrol.Require` 同一口径,另加「请求号就是批准号」:一次批准只能执行一次),缺省必须带。`commerce.admin-allow-unapproved` 只给本机开发:打开时启动和每次放行都打 ERROR,并计入 `mall_admin_unapproved_command_total`;带了记录就必须合格,打开时也一样。
- 发起人、批准人写进 `commerce_admin_command` 的 `proposer` / `approver` 两列,与处置同一事务;请求摘要覆盖整份请求(含批准记录),后台重试原样复用批准记录,摘要不变。
- 后台的客户端 `GameMallAdmin`:只收 HTTPS 或本机回环 HTTP;连接 3 秒、请求 8 秒、响应最多 256 KiB,不跟随重定向。确定的拒绝(400 / 401 / 403 / 404 / 409 / 413)记 REJECTED;其余(503、5xx、超时、断连)都可能已经提交,记结果未知,用原申请重试由商城按 (主体, 请求号) 判重。
- 原网关代理 `POST /admin/mall/*` 与 `/mall-admin` 页随批 4 删除。它只认一把共享的 X-Admin-Key,分不出是谁在操作,也漏了收入争议接口。

主仓配置:后台 `GM_MALL_URL`、`GM_MALL_ADMIN_SECRET`、`GM_MALL_ADMIN_SUBJECT`(缺省 mall-admin),与商城 `COMMERCE_ADMIN_SECRET`、`COMMERCE_ADMIN_SUBJECT` 同值;商城管理密钥只挂给后台工作负载。Java 配置 `GAME_DATA_MALL_ADMIN_URL`、`GAME_MALL_ADMIN_SECRET`、`GAME_MALL_ADMIN_SUBJECT`,使用原 GAME_ADMIN_TOKEN(双人复核待做,见 Java 仓 PARITY.md)。所有配置只写环境变量名,不把实际秘密写入跟踪文件。

## 验收

必须验证同请求并发和提交后重放只改一次、换载荷拒绝、两个修订竞争者仅一个提交、当前预占不能被价格或库存命令抹掉、已有订单金额与映射不变、库存下界与整数溢出、越权及未知路由拒绝。H2 与 HTTP 用例只验证相应接缝，另跑真实 TiDB，不能冒充完整后台或真实渠道验收。

本批另覆盖缺发物映射的历史SKU可查询／修复、修复前库存命令409且不留journal；新响应与重放都核合法完整条款，坏响应日志（包括null、空对象、尾随垃圾与第二根JSON）503，不接管重做。V2→V3回归执行同一升级SQL并核旧订单／预占保留。后台到商城的客户端测试(`GameMallAdminTest`)使用本例随机 loopback 服务,不连接他人服务;批准记录的拒绝路径由商城 `CatalogAdminHttpTest` 覆盖。真实鉴权、商城写入和联机页面仍待执行验收。

### 当前本机验收接线（2026-10-21）

主仓网关使用JDK23，商城使用JDK21；页面是classpath静态资源，没有npm工程。后台运营处置只发上述管理命令,不能代替 payment 角色发付款或交付回执。`start_game.ps1`当前没有自动拉起商城，验收由root单独批准当前商城JAR与子进程环境；不为冒烟打开全局默认开关。

商城真TiDB夹具显式接受数字loopback合法非零端口，且不得带库名、凭据或查询参数；本任务root只使用已核归属的`127.0.0.1:24000`。建随机库前先核TiDB及强读会话；四物理连接和弱读／历史快照拒绝断言保留。普通H2与loopback HTTP测试仍不等同真库或游戏到账证据。

`robot`的`mall-arrival-smoke`已改用`POST /internal/mock/pay`，正文只有原`orderId`和由该订单派生的稳定`requestId`。商城读取冻结金额并先提交mock提供方原件，robot不再向旧`/internal/payments`提交`mock-dev`五元组，也不增加渠道白名单。先验证额外金额字段400拒绝、原单仍待付款，再以两个原字段付款并重放，核原买家、金额、币种、SKU、份数；后续原到账、三拍不重发、重登存盘及遗留原单清理保持。`COMMERCE_RUN_MODE=dev`与`COMMERCE_MOCK_PAYMENT_ENABLED=true`必须同时显式设置，`COMMERCE_ALLOWED_PAYMENT_CHANNELS`为空。未知付款提交不换请求号。

本段是源码接线与待执行口径。本批未运行Maven、Go测试、mock接口、商城进程或游戏到账链；完整本地命令、环境名和各层通过标准置于`build/verification/current-completion-20261021/mall-admin-mock-current-acceptance-20261021/`，不把已有静态页面预览作为真实管理链路通过。
