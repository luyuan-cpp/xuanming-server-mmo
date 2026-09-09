package constants

const (
	ErrCodeOK              uint32 = 0
	ErrCodeRedis           uint32 = 1 // Redis operation failed
	ErrCodeLockConflict    uint32 = 2 // Another writer holds the player lock
	ErrCodeVersionMismatch uint32 = 3 // Optimistic lock version mismatch
	ErrCodeNotFound        uint32 = 4 // Player data or mapping not found

	// Snapshot / Rollback errors
	ErrCodeSnapshotNotFound uint32 = 10 // No snapshot found matching criteria
	ErrCodeSnapshotDBError  uint32 = 11 // MySQL error during snapshot operation
	ErrCodeRollbackFailed   uint32 = 12 // Rollback execution failed
	ErrCodePlayerOnline     uint32 = 13 // Player must be offline for rollback
	ErrCodeInvalidRequest   uint32 = 14 // Missing required fields
	ErrCodeZoneNotFound     uint32 = 15 // Zone has no players

	// ErrCodeNotImplemented 表示这条请求语义上合法,但服务端**没有真正执行**。
	// 存在的理由:宁可让调用方看到一次明确失败,也不能让"什么都没做"伪装成成功
	// —— 后者会让运营以为刷金资产已被回收、事故已处置完毕。
	ErrCodeNotImplemented uint32 = 16

	// ErrCodeResultTruncated 表示筛选命中数超过本次能安全返回/处理的上限。
	// 返回该码时本次操作必须为零变更，调用方应缩小时间或玩家范围后重试。
	ErrCodeResultTruncated uint32 = 17

	// ── AllocateIdSegment(号段发号,设计 §6.2)──────────────────────────────

	// ErrCodeIdSegmentDBError:id_segment 表读写失败,或号段 store 根本没配起来。
	// 与 ErrCodeSnapshotDBError 分开:两者虽同库,但告警要能一眼分清"回滚库坏了"
	// 与"发号源坏了"——后者会让 login 建角色 / 建公会 / 铸物品全部停摆。
	ErrCodeIdSegmentDBError uint32 = 18

	// ErrCodeIdSegmentExhausted:max_id + step 会越过 2^55 值域上限,fail-closed 拒绝
	// (号段值域与存量 snowflake 号不能相交,见设计 §6.3)。表里的 CHECK 是最后一道闸,
	// 这里在 UPDATE 之前就拒绝,表状态零变更。理论上万年内不会命中,命中即事故。
	ErrCodeIdSegmentExhausted uint32 = 19

	// ErrCodeIdSegmentUnknownTag:表里没有这个 biz_tag 的行,且 IdSegment.AllowAutoSeed=false
	// (生产形态),拒绝发号、不写任何行。
	//
	// 为什么不像以前那样运行期自动种一行从 1 起(设计 §7.5 第 7 条):行只会在全局库被
	// drop 重建、或从旧备份恢复时消失,而消费表(player_database / guild / 玩家 blob 里的物品)
	// 还留着已发出的号 —— 此时从 1 重发,login 的 INSERT ... ON DUPLICATE KEY UPDATE 会静默
	// 覆盖别人的角色行。缺行是 ID 安全事件:行必须由 `data_service -migrate`(或 dev 的
	// AutoMigrate)按 IdSegment.BootstrapTags 显式创建,迁移顺带把 max_id 抬到消费表最大号 +1。
	// 与 InvalidRequest 分开:调用方 tag 拼错是"业务拒绝",而合法 tag 缺行是必须告警的故障。
	ErrCodeIdSegmentUnknownTag uint32 = 20

	// ── 合服(RegisterPlayerZone / RemapHomeZoneForMerge)────────────────────

	// ErrCodeZoneMappingConflict:player:zone:{id} 已经存在**且值与请求不同**,
	// RegisterPlayerZone 拒绝覆盖(SETNX 语义),不写任何键。
	//
	// 为什么必须拒绝而不是覆盖:这个映射是玩家数据 / 公会 / 榜的归属权威。
	// 合服把它从源 zone 改写成目标 zone 之后,任何一次"按建角 zone 重新登记"
	// (login 重试建角、debug_import 重跑、旧版本回滚)都会把玩家送回已下线的
	// 源区,而且没有任何痕迹。覆盖是一次静默的数据事故,拒绝只是一次可见的失败。
	//
	// 值相同 = 幂等成功(不占这个码):login 的 CreatePlayer 允许重试。
	ErrCodeZoneMappingConflict uint32 = 21

	// ErrCodeZoneMergeInProgress:mapping Redis 里存在 merge:in_progress:{zone},
	// 该 zone 正处在合服维护窗口,写入类操作 fail-closed 拒绝。
	//
	// 契约刻意只有一条:**键存在即封锁**。值(JSON,含 started_at)与 TTL 都不解析
	// —— 解析就意味着多一种"值坏了怎么办"的分支,而这个闸门的唯一正确失败方向是
	// 拒绝。键由 tools/merge_zone 在写阶段之前写入、跑完删除,TTL 只是进程被杀时的兜底。
	//
	// 与 ErrCodeZoneMappingConflict 分开:那个是"这条映射本来就有主",这个是
	// "整个 zone 现在不接受新映射",运维看到后者要去查合服跑完没有。
	ErrCodeZoneMergeInProgress uint32 = 22

	// ErrCodeAdminAuthRequired:RemapHomeZoneForMerge 这类**改写全服归属**的运维 RPC
	// 缺少 / 带错 x-admin-token,或服务端根本没配 AdminToken(= 该 RPC 整体停用)。
	//
	// 没配 = 停用 而不是 免鉴权:一个默认敞开的 remap 接口意味着任何能连到
	// data_service 的进程都能把全服玩家的归属 zone 改掉,而 data_service 在集群内
	// 是无鉴权可达的。默认值必须是"不能用",开启是一次显式的运维动作。
	ErrCodeAdminAuthRequired uint32 = 23

	// ErrCodeMergeFenceMissing:RemapHomeZoneForMerge 找不到 merge:in_progress:{source},
	// 拒绝执行。
	//
	// 这是"线上误调用"的防线:合服工具必须先立标记再改写,标记同时也在挡住
	// RegisterPlayerZone(见 ErrCodeZoneMergeInProgress)。没有标记就改写,说明要么
	// 有人拿着 admin token 手工调了这个接口,要么工具的步骤被跳过了 —— 两种情况下
	// 源 zone 都还在线接受新映射,改写会与在线写入交错,产生一批指向源 zone 的漏网玩家。
	ErrCodeMergeFenceMissing uint32 = 24
)
