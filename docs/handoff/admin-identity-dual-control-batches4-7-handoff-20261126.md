# 后台管理员身份与双人复核:批 4~7 交接(2026-11-26)

设计:[docs/design/admin-identity-and-dual-control.md](../design/admin-identity-and-dual-control.md)。§8 表格的「状态」列是总账,做完一批就改那一格。
负责人在原会话里的原话是「7 批全做」。父会话转来的话只批了 1~3 批,两边不一致;按负责人在原会话的原话做全部 7 批,收尾时把这处不一致报给负责人。

批 1~3 已落码,全部未编译,待 Gemini 验证;命令见 `docs/PROGRESS.md` 里标题带「后台管理员身份与双人复核·批」的三条。

**2026-11-26 晚进度**:批 4、5、6 已落码(未生成、未编译),批 7 的告警与契约测试已落码;做法与取舍见设计稿 §11,命令见 PROGRESS 标题带「后台管理员身份与双人复核·批 4」「批 5」「批 6」「批 7」的各条。**还没做的只有 §5 末尾「批 7 剩余」**,由新会话接着做。

## 1. 已有的公共件(批 4~7 直接用,别另写一套)

| 件 | 位置 | 怎么用 |
|---|---|---|
| 批准记录协议 | `proto/common/base/admin_approval.proto` | 甲级请求加一个 `common.base.AdminApproval approval` 字段。它在请求体里,所以被请求自带的 ServiceCallAuth 签名覆盖 |
| 资源服务核对 | `go/shared/dualcontrol.Require(approval, operator)` | 先验调用证明,再调它;每个服务一个开关,缺省要求带。Java(`mall_game`)照它的四条判断写十几行即可 |
| 流水 | `java/gm_admin/.../GmCommandJournal.java` | 新类别:`prepareInternal` 加一支校验与冻结字段;发送用 `sendInternal(..., BiFunction<request, approval, result>, kind)`。下游按批准号(或请求号)判重的类别加进 `retryable`。批准记录由 `approvalOf(row)` 生成,批准号就是流水号 |
| 权限检查 | `GmAccess.operator(permission)` | 新控制器都用它;操作人只取认证主体 |
| 批准面板 | `frontend/src/views/gm/console/ApprovalPanel.vue`、`frontend/src/api/gm/approval.ts` | 新类别在 `DeclinableKind` 与 `declineCommand` 里加一个分支 |
| 标准角色 | `GmInstallation` 迁移 7(`gm_operator` / `gm_approver` / `gm_auditor`) | 新权限照迁移 8 的写法:建菜单行、授给 admin,再用 `grant(...)` 授给标准角色 |
| 参照实现 | 交易人工处置:`GameTradeAdmin`、`GmTradeController`、`TradeManual.vue`;trade 的 `manual_contract_ops.go` `admitApproval`、`commerce/manual.go` `alreadyExecuted` | 新接入照这一套抄 |

## 2. 批 4:商城人工单与 SKU 迁进后台

- **现状**:
  - `java/gateway_node` 的 `MallAdminController`、`MallAdminClient` 与 `mall-admin.{html,js,css}`,鉴权是共享 `X-Admin-Key`。转到 `java/mall_game` 的 `/internal/admin/*` 时用 HMAC,主体是 `admin`(`HmacAuthentication.java` 里按路径对应主体)。
  - `ManualAdminStore` 的幂等表是 `commerce_admin_command(actor, request_id)`,actor 就是 `admin`。
  - `manual/incomes/dispute` 在 `mall_game`,但不在网关的 `COMMANDS` 里。
- **要做**:
  - 后台新增商城客户端,签名写法照 `MallAdminClient.signature`。
  - 流水新增类别:人工订单 / 退款 / 寄售 / 收入争议,以及 SKU 新建 / 改价 / 调库存。`request_id` 就用批准号。
  - `mall_game` 的处置请求带批准记录,核对开关缺省开;`commerce_admin_command` 加发起人、批准人两列。
  - 把漏掉的收入争议接上。
  - 删网关的商城代理与页面,同时删掉它们的测试、配置键 `mall-admin.*` 和契约测试里的引用(先 grep `mall-admin`、`MallAdmin`)。
- **注意**:商城建表与列变更走它自己的 `SchemaVerifier` / 合同戳(见 `docs/design/mall-administration.md`、PROGRESS 11-21「商城/寄售人工单处置」)。项目没上线,可以清库,但合同戳要同步改。

## 3. 批 5:邮件、回收、收件箱、账号类

- **邮件**:`proto/mail/mail_admin.proto` 的 `SendMailRequest` 现在没有操作人字段。
  - 加 `approval`,并落进邮件行(看 `mail_table.proto`)。
  - 只有调用方 `admin`(后台与 `mailadmin` 工具)要求带批准记录;系统生产方(战斗掉落、榜单奖励等别的调用方)不要求。按调用方配置,别一刀切。
  - 邮件按 `(sender_kind, request_id)` 加全文摘要判重。重试时批准记录原样复用,摘要不变。
- **共享口令 `x-admin-token` 的入口**:data_service 回收五个入口、player_inbox 的 `GmSanctionPlayer` / `VoidItem`、login 的 `LoginAdmin` 三个方法。请求加 `approval` 并调 `dualcontrol.Require`。
  - 口令只发给后台工作负载;拿到口令的人能伪造批准记录,和 HMAC 密钥同一个信任边界,设计 §6 第 3 条已写。
  - data_service 回收请求已有自由文本 `Operator` 与 `RequestId`:改为 operator 必须等于 proposer,幂等键用批准号。
- **login.proto 是客户端可见协议**(客户端有 `Login.cs`)。往它里面 import `admin_approval.proto`,会把这条消息带进客户端的生成闭包。
  - 标准做法:把 `LoginAdmin` 服务和它的请求移到新文件 `proto/login/login_admin.proto`(只给服务端)。
  - 移完核对 C++ / Go 的引用路径。客户端可见的 `login.proto` 少了服务端管理消息,要按 AGENTS §4 让 Gemini 跑客户端三个脚本。
- **rank**(`RemoveRankEntry`、`DeleteRankGeneration`)与 **match**(`ResolveStrandedFormation`)同样接入。
- 后台为每类加页面,或者在一个「运营处置」页里分区。

## 4. 批 6:网关 `/admin/**` 收口

- 写操作一律经后台发起,后台从会话里填 `X-Admin-Operator`。网关的写操作要求这个头合规(与身份提供方主体同一字符集),并且每个控制器都打 `AUDIT` 行;现在只有热关停和配置表两个控制器打。
- `table-bundle publish / rollback` 是机器调用:给它一把单独的机器口令(与人用的后台路径分开),操作人记成 `table-bundle`。
- 查部署清单:公网入口(Ingress / Gateway)不路由 `/admin`。先 grep `deploy/k8s` 里的网关入口。
- 乙级动作按设计 §3 不要求双人,只要求个人身份与留痕。

## 5. 批 7:后台上 K8s、告警、本机转发

- **清单**:Deployment、Service、NetworkPolicy(只许后台访问各服务管理端口)、Secret 键(OIDC 客户端密钥、交易 / 邮件 / 商城管理密钥),以及 `gm.identity.mode=oidc`。契约测试照 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的写法。
- **告警**:`trade_manual_resolution_unapproved_execute_total` 在 5 分钟内有增长就告警(告警名 `TradeManualResolutionUnapprovedExecute`,与 trade 配置注释一致)。批 5 各服务的「不带批准记录被接受」计数也同样接上。
- **本机转发**:`deploy/gm-admin/rpc-bridge.py` 现在只转 data_service(2356)与 player_locator(2357)。加一路到 trade,做法照现有两路(就绪凭据、容器身份核对),并让 `run_local.py` 在验收通过后放行 `GM_TRADE_TARGET` / `GM_TRADE_ADMIN_SECRET`;批 3 已经让它在没有验收时一律不放行。
- 支付人工面(`payment_admin.proto`,设计在 `payment-recharge.md` §5.4)不在这 7 批里。落码时直接按本设计接入后台流水。

### 批 7 剩余(2026-11-26 晚登记,交新会话)

**2026-11-27 进度**:第 1 项(后台上 K8s 及各管理入口的密钥接线、准入、应急开关守卫、契约测试)与第 3 项(文档、PROGRESS)已落码,未运行;做法与取舍在设计稿 §11.4,命令在 PROGRESS 标题带「双人复核·批 7 剩余」的条目。
第 2 项定为这次不做(2026-11-27):本机 Docker 调试栈里没有 trade 容器,下面写的「本机 trade 的 `go/trade/etc/trade.yaml`」前提不成立。理由(详见设计稿 §11.4 末节):
1. trade 只在宿主机 `start_game.ps1` 那套里跑,和后台连的 Docker 世界不是同一套库,后台直连它会把两个世界的数据混在一个页面上;
2. 另起独立 trade 节点(照 `mail-*`)还要合同引擎(`AssetOp.Enabled`)与资产通道的上下游,单起一个没有可处置的合同,验不出东西;
3. 交易这条路靠 trade 的 `TestManualContractOps*`、真库 `TestRealDBManual*`、后台 `GameTradeAdmin` 的回环 gRPC 用例,以及 K8s 部署后的集群冒烟;
4. `run_local.py` 维持现状,`GM_TRADE_*` 一律不放行;
5. 以后要本机全链路联调时走独立 trade 节点,同时起资产通道的依赖。

批 7 到此没有待写的代码。下面原文保留作当时的清单。

已做:`deploy/k8s/dual-control-alerts.yaml`(8 条「不带批准记录被接受」告警,trade 的叫 `TradeManualResolutionUnapprovedExecute`)与 `tools/scripts/tests/dual_control_alerts_contract.tests.ps1`。

没做的(按这个顺序):

1. **后台上 K8s**。现在 `tools/scripts/java_svc_image.ps1` 的镜像目录只有 gateway 与 mall,`k8s_deploy.ps1` 里没有 gm_admin;后台的构建靠 `java/gm_admin/prepare.py` 先拉起 JeecgBoot 上游再打包(见 `docs/design/jeecg-gm-administration.md`)。要补:
   - 镜像构建与 Deployment / Service(只在集群内可达,不建 Ingress);`gm.identity.mode=oidc`;OIDC 客户端密钥走 Secret。
   - 后台要的密钥都走工作负载密钥目录(照 `$WorkloadSecretSpecs` 与 `Resolve-InjectedSecret`):`GM_TRADE_ADMIN_SECRET`(与 trade 的 `Commerce.ManualResolutionSecretEnv` 指名的同值)、`GM_MAIL_ADMIN_SECRET`(= mail 的 `MAIL_PRODUCER_SECRET_ADMIN`)、`GM_MALL_ADMIN_SECRET`(= 商城 `COMMERCE_ADMIN_SECRET`)、`GM_LOGIN_ADMIN_TOKEN`(= `MMORPG_LOGIN_ADMIN_TOKEN`)、`GM_DATA_ADMIN_TOKEN`(= data_service `AdminToken`,**它在 K8s 配置模板里现在没有渲染,回收入口在集群里是关的,要一起接上**)、`GM_INBOX_ADMIN_TOKEN`(= `MMORPG_PLAYER_INBOX_ADMIN_TOKEN`)、`GM_RANK_ADMIN_SECRET`(= rank 的 `RANK_PRODUCER_SECRET_ADMIN`)、`GM_MATCH_FORMATION_SECRET`(match 的 `GlobalMatch.ManualResolutionSecretEnv` 现在也没进清单,要一起接)、`GM_GATEWAY_ADMIN_KEY`(= 网关 `admin.api-key`)。各把互不相同;这些管理口令只挂给后台(设计 §6 第 3 条),契约测试守住「消费者只有 gm_admin 与资源服务本身」。
   - 网关的机器口令 `admin.machine-key`(批 6 新增,环境变量 `ADMIN_MACHINEKEY`)走同样的 Secret 注入,与 `admin.api-key` 不同值;`table-bundle` 那一侧拿机器口令、带 `-operator table-bundle`。
   - NetworkPolicy:只许后台访问各服务的管理端口(照 `$script:GrpcAllowLists` / `New-GrpcAllowListNetworkPolicyYaml`)。
   - 部署守卫:mail / rank 的 ConfigMap 是整份复制 `etc/*.yaml` 再替换,match 的 `GlobalMatch` 段也是照抄;有人把应急开关(`AdminAllowUnapproved`、`ManualResolutionAllowUnapproved`、商城的 `COMMERCE_ADMIN_ALLOW_UNAPPROVED`)提交成 true 就会上集群。非 dev 档在任何写操作之前拒绝渲染出来的开关为 true,契约测试守住。
   - 契约测试照 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 与 `k8s_mall_payment_deploy_contract.tests.ps1` 的写法。
2. **本机后台到 trade 的转发**。`deploy/gm-admin/rpc-bridge.py` 与 `rpc-bridge/main.go`、`rpc-bridge.compose.yml`、`rpc_bridge_test.py` 加第三路(照现有两路:就绪回执、容器身份与 etcd 注册核对、端口只绑回环);`tools/scripts/gm_scene_promotion.py` 的 `READ_TARGETS` 要求回执与它逐字相等,要一起改。本机 trade 的 `go/trade/etc/trade.yaml` 现在 `ManualResolutionSecretEnv` 为空(入口关着),本机调试栈要能打开它才有意义。三处一起验收后,`java/gm_admin/run_local.py` 才从回执放行 `GM_TRADE_TARGET` / `GM_TRADE_ADMIN_SECRET`(现在一律不放行)。商城与运营处置各下游的本机转发同理,先不做。
3. 做完在 PROGRESS 追加一条,设计稿 §8 批 7 的状态格与 §11.4 改到最新。

## 6. 规矩(与批 1~3 相同)

- 只在 main 工作区改,不开分支、不提交、不 push;别的会话正在改的文件不碰,改之前先 `git diff HEAD -- <文件>` 看清哪些是自己的。
- 协议生成、编译、测试、冒烟交 Gemini;每批写完在 PROGRESS 追加一条,写明「未编译,待 Gemini 验证」和可直接执行的命令。
- 日志英文,注释与文档中文。新增文档后跑 `python tools/scripts/gen_docs_index.py --check`,绿了再生成,diff 只许加行。
- Java 独立仓只在 `PARITY.md` 登记待做;批 3 已登记总条目,后面各批只在那一条下补充。
- 分析、方案与解决办法写进设计稿;负责人没想到的好做法一定要记下(例如批准号排在版本比较之前、锁外锁内各认一次)。
