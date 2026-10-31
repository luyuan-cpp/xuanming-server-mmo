# 存盘直接提交规格 v1 对抗评审记录(2026-10-30)

评审对象:`docs/design/player-save-direct-commit.md` v1。全部为静态核对,未运行。缩写同 `survey.md`(PL = player_lifecycle,OE = owner_epoch,ES = enter_scene,RC = rpc_replies,PS = 迁移脚本 proto schema,PF = player_fence)。

**结论:暂不能开始 B1。** §4 有两格写错,而它正是 B1 的纯函数。改完致命 1、2 和 B1 相关三项(payload 归一、kind 白名单、建表)才能开工。处理状态见文末。

## 成立的部分

- 认领与存盘靠写权行锁串行。铸造到认领之间旧持有者的写必被新持有者读到,认领后被拒,已确认给对方服务的都已提交,未找到串档路径。
- 判定表其余格没有问题:实体已销毁后的同实例重进、release 后重进、接管。新号初行 E=1:SM 铸造值不小于毫秒,实际走 E>Er。
- 交接不 release、崩溃接管、疏散、结算放锁、收件箱 Ack、资产终结的替换都成立。
- 多行 ODKU 能过 `statement.go`;RC 下锁后再读的论证成立(`pool.go` 每语句新 TSO、leader 读)。
- 并发建角无问题:Save 和认领都不建写权行。
- 未知结果可重试:同 q 得 EQUAL,同 E 认领幂等。
- EQUAL 未找到同 q 异内容路径(`node.cpp:1109` 实例每进程换,GO-3 换进程必铸)。

## 致命问题

1. **合法 E>Er 被判 UNCLAIMED,实体再也存不进,下线后进度全丢。** 「正常不会发生」不成立,三条路径:
   - B5:A 持 N → SM 铸 M、推路由失败回滚到 M+1 → `PL:7044` 采纳,`PL:7048` 存盘,E=M+1、Er=N。
   - 复用差 1:墙钟落后时铸 E+1(`OE:198`),`IsDeposedOnReentry` 为假,`PL:2987` 抬 E,复用不调 RPC。
   - 载入在途被新路由覆盖:`PL:2879` 换成 E2,`PL:2876` 不重发,认领的是 E1。

   修:`E>Er` 且持有者=实例时,按 E=Er 继续往下判并抬 Er;其余仍拒。有中间持有者时持有者必不是自己,不会误放。

2. **release 是终态,实体却能复用。** 序列:断线 → `Save(release,q9)` 在途 → 30s 内重连,`ES:747` 不铸造,`PL:2953` 取消退出并复用 → q9 提交 released=1 → 下一笔 `NOT_HOLDER` → 销毁,重连后进度全丢。退出存盘在途时内存又变(`PL:2539` kResave)同样。另外 `PL:3455` 快路径不存盘:挂机下线最常见,released 永不为 1。

   修:`NOT_HOLDER` 只判持有者≠实例,同实例已释放放行并按请求重写 released,§8 对应用例改;快路径发空行 release。

3. **判死放弃可一物两得(阻塞 B6,不阻塞 B1)。** 序列:scene 应用 seq、与 go/db 断开或进程暂停 ≥1h → etcd 判死 → 放弃退款 → 恢复后 E=Er、持有者相同,账本提交。现有 `playerAbsent`(`abandon.go:557`)只认位置键不存在,判死实现只在 SM(`ES:1080`),写权行只存 uuid,其他服务判不了。

   修:删判死分支,由 SM 判死后调 go/db 同事务抬 E、置 released=1;放弃一条语句同读 kind 0、2。released=1 分支靠租约与 `LastSendMs` 安全。

## 必须改的

- **未决基准:** `STALE_OWNER` 后重存(`PL:4873`)、OLDER_SEQ 重播种时,上一笔 rows 可能已提交,要并入脏集,否则改回原值的行不写。
- **回调按 Guid 查实体(§5.1-5):** `PL:5940` 销毁重载后,旧 q9 晚回会把新实体的 confirmed 改旧。按加载代次绑定状态机。
- **全局完成:** 有 pending 时不得发布(`RC:1171`),否则 q9 落地即 `BeginTravelHandoff`(`PL:2425`),标记早于冻结快照,目标读旧档。
- **遗漏等待点 `RC:516`(存盘未落定时挂起加载):** 同 E 销毁且 q9 未决 → 新实体读 S8 → q9 晚到写入,库与内存分叉。要保留,或认领带新 q 写入 save_seq。`HasUnsettledPlayerSave`、`HasPendingLoad` 全部接新在途。
- **§6 两处写错:** 会话结束凭证(`PL:5859`)无实体,「最终存盘后写标记」不成立,要发空行 release。标记未发时解冻靠 `PL:5362` 看门狗,`ResolveTravelOutcome` 直接返回(`PL:6309`)。
- **拼接:** 每字段只在一行时等价。换组后两行各一份,消息字段合并、repeated 追加,背包翻倍。
  - 须逐行解析、清除不属本 kind 的字段再合并。
  - kLegacy1 单独解析(字段 2 与 transform 冲突)。
  - 顶层未知字段拒存。
  - confirmed 用库里原字节播种;否则 Uncarried 的 `merge_state` 永不清,加载时改过的字段也不写。
  - 探针打戳时机要定:先打则恒脏,后打则不落盘。
- **确定性 SQL 错误:** `driver.go:76` 只认 1062,其余 Unknown,scene 无限重试、退出卡死。`[]byte(nil)` 按驱动写 NULL 撞 NOT NULL(未实测),绑定前归一;kind 拒 0 与重复;§4 缺 `TOO_LARGE` 处置。
- **重试要重签:** `auth.go:138` ±300s,故障超 5 分钟后永久失败。
- **建表:** 基线已跑的库 `cmd/migrate` 建不出新表且只告警(`PS:406`),Save 1146 无限重试。切 `go/schemamigrate` 或写明清库。
- **开关:** 写明单向、全局单源、部署门禁、翻转即清库。混跑时跨 zone 到 redis 节点读旧父键,静默回档;data_service 与 scene 不一致则 NO_OWNER 或 NIL。
- **遗漏:**
  - 职业在 kHot(`player_class_backfill.go:67`),§9.2 只写 kProfile。
  - `schema.go:1640` 号段地板、`route_bootstrap.go:37` 查旧表。
  - `rollback_logic.go:776` GM 回档写 Redis 回成功。
  - `asset_op_ledger_verified.go:83` 仍读 Redis,放弃停摆。
  - `PF:95` 改比写权行。
  - merge_zone 按清单自动收 `player_store`,B7 前 fail-closed。
  - `stress_summarize` DB 段变空,go/db 要补指标,否则 AGENTS §6 无对比表。
  - verifier、`robot/data_stress` 依赖探针与 DBTask。
  - `player_centre_database` 只有存在性检查,不影响。

## 可接受的残余

- released=0、持有者活、无实体(`PL:6554`、载入放弃):放弃跳到进程退出或下次登录,不丢不复制。
- Redis 回退且时钟落后 E<Er:进场失败到时钟追上。不确定是否需要运维工具。
- 旧 scene 读新世代行被拒:滚动期活性,未上线。
- 写门禁 22+ 条、1205/1213 隔离连接的重连开销:压测定。
- EQUAL 排在持有者核对前:跨实例同 E 现不可达,建议对调。

## 涉及文件

根目录 `D:\luyuan\xuanming-server-mmo-cpp`:

- `go/shared/assetop/abandon.go`
- `go/shared/authoritysql/{statement,driver,pool}.go`
- `go/shared/serviceauth/auth.go`
- `go/data_service/internal/store/{schema,route_bootstrap}.go`
- `go/data_service/internal/logic/{rollback_logic,asset_op_ledger_verified}.go`
- `go/login/internal/logic/clientplayerlogin/player_class_backfill.go`
- `cpp/libs/engine/core/node/system/node/node.cpp`

## 处理状态

2026-10-30 已按本记录把规格修到 v2,逐条写进 `player-save-direct-commit.md`:致命 1、2 在 §4、§5.1;致命 3 在 §6,排在 B6;「必须改的」在 §1、§2、§5.1、§6、§7、§7.1、§8;遗漏项分配到 B2、B5、B6、B7。`schema.go` 的号段地板已随发号器批删除。「重试要重签」「确定性 SQL 错误」要在 B1、B3 落码时核对实际错误分类。
