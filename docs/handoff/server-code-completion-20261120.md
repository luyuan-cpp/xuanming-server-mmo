# 服务器代码完成度核对与决定(2026-11-20)

范围:服务器(Go、C++)与客户端代码。Java 不动,留到 todo(见 Java 仓 `PARITY.md`)。协议生成、编译、测试、冒烟交 Codex。

## 1. 结论

- 按缺口清单([server-gap-audit-20261101.md](server-gap-audit-20261101.md))与 `docs/PROGRESS.md` 的记录,**服务器代码可做的部分已经全部落码,但全部未编译、未运行测试**。
- **还没有全部做完。** 剩下三类:需要负责人决定的(§3.1)、依赖后台身份体系的(§3.2)、其它会话正在写的(§3.3)。Java 按要求留到 todo(§3.4)。
- 本轮没有新增代码改动。做了的事见 §2。
- 本轮没有逐条重读全部代码。结论依据是缺口清单与 PROGRESS 记录,加上 §2.2 的抽查。

## 2. 本轮做了什么

### 2.1 提交

- 提交 `55eae6be3`(本地 main,**未 push**),588 个文件,+33726 / −5948。
- 提交前复扫密钥,命中 18 行。逐条确认是测试夹具、占位符、示例 DSN 或 PEM 校验正则,没有真实凭证。
- 提交是提交瞬间的快照:含其它会话未完成的改动。9 个在暂存时已被其它会话删除的文件,以删除的形式进入提交,与它们的删除意图一致。

### 2.2 抽查

抽查 19 项,18 项在 HEAD 中存在:

- 符号:`moveWorldChannelOffDeadNode`、`IsRetryableTxError`、`luaRegisterInstance`、`luaResolveCasReceipt`、`luaBeginDrainWorldChannel`、`RevisionTracker`、`NewAdmitSigner`、`workReadyTicket`。
- 测试文件:`scene_registration_test.go`、`world_channel_commit_order_test.go`、`dead_node_reregister_test.go`、`scene_claim_atomic_test.go`。
- 新包与告警:`go/shared/noderegistry`、`go/shared/gatepick`、`go/shared/liveview`、`go/content_guard`、`cpp-node-dependency-alerts.yaml`、`noderegistry-alerts.yaml`。

1 项过时:缺口清单第 1 条写的 `luaRollbackInstanceAllocation` 已删除(PROGRESS 有记录),回滚现在由 `atomicRollbackInstanceAllocation`(`go/scene_manager/internal/logic/scene_atomic.go`)完成。缺口清单已改正。

### 2.3 PROGRESS

提交里的 `docs/PROGRESS.md` 只新增 2159 行,没有删改旧条目,符合 AGENTS §5。本轮在文末追加一条。

## 3. 还剩什么,以及按标准做的决定

### 3.1 需要负责人定(本轮不擅自做)

- **支付回调入口**:微信 / 支付宝 / App Store / Google Play 的到账通知需要公网 HTTPS 域名与证书。用哪个域名、证书怎么签,要负责人定。K8s 清单与契约测试本身不缺决定。
- **App Store / Google Play 客户端桥**:工程里没有 StoreKit 2 / Play Billing 插件。装哪一个,要先授权(AGENTS §10.2:装依赖要授权)。

### 3.2 依赖后台身份体系:决定先不做,不造假审批

- **交易人工判定的审批流**(缺口第 2 条残余):标准做法是双人复核(maker-checker),审批人身份必须是管理后台的真实登录。仓库现在只有服务间口令(serviceauth,调用方 `ops-commerce`)。拿它当审批人就等于没有审批,所以不做。后台身份体系落地后,按双人复核实现;`TradeAdmin.ListManualContracts` / `ResolveManualContract` 作为入口保留。
- **官方商城与人民币寄售的人工处理**(同条残余):PROGRESS 里本轮没找到落码记录,列为待核对。

### 3.3 其它会话正在做(本轮不重复做)

据会话名判断范围(ListAgents 显示这些会话仍在运行或空闲):

- 改名与内容安全(`go/content_guard` 已在 HEAD):「玩家名字重名检测与屏蔽词过滤」。
- Unity 登录排队持票(缺口 §11.5 第 6 条:客户端断网几秒仍丢排位):「玩家登录排队」。建议的标准做法是:排位以服务端票据为准,票据在宽限期内(超时才释放)允许断线重连续排。落实前该会话要核对契约,本文只记建议。
- 挂机月卡开通入口:「挂机月卡开通入口与托管跟随冒烟」。
- 剧本审核流水保留期清理:「剧本审核流水加保留期清理」。
- 账号注销客户端、Apple / Google 登录:「海外上架」(空闲)。
- 全球账号库与第三方登录:「全球账号库与第三方登录」(空闲)。

### 3.4 Java(按要求留到 todo)

缺口清单里标为「只剩 Java」的条目:第 7、16、18、19、21、27 条,以及各条的 Java 残余。本轮不改 Java。

### 3.5 已定案、不做的项(沿用缺口清单的记录)

- 第 13 条运行期字段集合指纹:设计 §13 第 11 条已定不做。
- 第 15 条 `snapshot_guid` 分桶与按天分区:前提不成立,不做(11-11)。
- 第 30 条场景亲和与命令 topic 换代:不做(11-11)。
- 第 33 条住户预算:保留 200(11-11)。
- 第 3 条审计 topic 的 `retention`:prod 4GiB(11-11)。

## 4. 交 Codex 的验证

- 按 PROGRESS 各条目的命令执行,重点是 2026-11-19 与 2026-11-20 的条目。
- 提交 `55eae6be3` 含其它会话的 C++ 改动。建议 Codex 先跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`(AGENTS §10.1)。本轮自己没有 C++ 改动。
- 提交含 proto 改动(如 `proto/common`、`proto/match`)。Codex 需先按 AGENTS §4 执行 `proto-gen-build` 与 `proto-gen-run`,再编译受影响的服务。

## 5. 风险与未验证项

- 全部未编译、未运行测试。
- 提交是快照,含其它会话未完成的改动。
- 提交后工作区仍有其它会话在改的文件(如 `docs/design` 下的几份文档)。
- 本文的结论依据记录与抽查,不是逐条代码复核。
