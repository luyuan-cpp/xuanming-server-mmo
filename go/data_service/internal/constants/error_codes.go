package constants

const (
	ErrCodeOK       uint32 = 0
	ErrCodeNotFound uint32 = 4 // GetPlayerHomeZone:映射缺席

	// 1 是 Redis 读写故障用过的号,2、3、10、12、13、15 是字段 KV 和回档用过的号,都已删除,不再分配。
	ErrCodeSnapshotDBError uint32 = 11 // MySQL error during snapshot operation
	ErrCodeInvalidRequest  uint32 = 14 // Missing required fields

	// ErrCodeNotImplemented 表示这条请求语义上合法,但服务端**没有真正执行**。
	// 存在的理由:宁可让调用方看到一次明确失败,也不能让"什么都没做"伪装成成功
	// —— 后者会让运营以为刷金资产已被回收、事故已处置完毕。
	ErrCodeNotImplemented uint32 = 16

	// ErrCodeResultTruncated 表示筛选命中数超过本次能安全返回/处理的上限。
	// 返回该码时本次操作必须为零变更，调用方应缩小时间或玩家范围后重试。
	ErrCodeResultTruncated uint32 = 17

	// ── AllocateIdSegment(号段发号,设计 §6.2)──────────────────────────────

	// ErrCodeIdSegmentSourceError:发号器读写预留记录失败,或者根本没配起来。
	// 告警要能一眼分清,这个码意味着 login 建角色 / 建公会 / 铸物品全部停摆。
	ErrCodeIdSegmentSourceError uint32 = 18

	// ErrCodeIdSegmentExhausted:本地域的值域见底,fail-closed 拒绝。理论上万年内不会命中,命中即事故。
	ErrCodeIdSegmentExhausted uint32 = 19

	// ErrCodeIdSegmentUnknownTag:发号器确定地拒绝发号,不写任何记录。两种原因:
	//   - biz_tag 没登记进 IdSegment.Tags:配置漏了;
	//   - 两份预留记录都不存在,且 IdSegment.AllowAutoSeed=false(生产形态)。首次部署要显式跑
	//     `data_service -id-issuer-init`;如果不是首次部署,就是两套 etcd 同时丢了数据,这是 ID 安全事件,
	//     按 docs/ops/id-issuer-runbook.md 处理。
	// 与 InvalidRequest 分开:调用方 tag 拼错是"业务拒绝",而这里是必须告警的故障。
	ErrCodeIdSegmentUnknownTag uint32 = 20

	// ── 归属登记(RegisterPlayerZone)────────

	// ErrCodeZoneMappingConflict:player_route 里已有归属**且值与请求不同**,
	// RegisterPlayerZone 拒绝覆盖,不写。
	//
	// 为什么必须拒绝而不是覆盖:home_zone 建号后不再改(合服只写 zone_merged_into 别名,
	// docs/design/player-data-tidb-scaling-plan.md §7.1),它还决定 go/db 选库。一次覆盖会让玩家的
	// 数据找不到、归属跟着错,而且没有任何痕迹。覆盖是一次静默的数据事故,拒绝只是一次可见的失败。
	//
	// 值相同 = 幂等成功(不占这个码):login 的 CreatePlayer 允许重试。
	ErrCodeZoneMappingConflict uint32 = 21

	// 22 已删除,号不复用:曾是 ErrCodeZoneMergeInProgress(合服围栏拒绝登记)。
	// 别名合服之后登记不再看围栏(docs/design/player-data-tidb-scaling-plan.md §7.1),旧日志里还可能出现这个号。

	// ErrCodeAdminAuthRequired:运维 RPC 缺少 / 带错 x-admin-token,或服务端根本没配 AdminToken
	// (= 该 RPC 整体停用)。
	//
	// 现在守的是 ReleasePlayerName 的运维路径(不限登记时间的释放)。
	//
	// 没配 = 停用 而不是 免鉴权:一个默认敞开的运维接口意味着任何能连到
	// data_service 的进程都能释放任意玩家的名字,而 data_service 在集群内
	// 是无鉴权可达的。默认值必须是"不能用",开启是一次显式的运维动作。
	ErrCodeAdminAuthRequired uint32 = 23

	// 24 已删除,号不复用:曾是 ErrCodeMergeFenceMissing(RemapHomeZoneForMerge 找不到合服标记)。
	// 该 RPC 已随别名合服删除,旧日志与告警规则里还可能出现这个号。

	// ── 玩家名字注册表(docs/design/guild-phase2/03-names.md §3.1)────────────

	// ErrCodePlayerNameDBError:player_name 表读写失败,或名字 store 根本没装配起来
	// (建表失败 / 连不上全局库 / PlayerName 段配置非法)。
	//
	// 与 ErrCodeSnapshotDBError 分开:两者同库,但告警必须
	// 一眼分清坏的是哪条路 —— 名字挂了的症状是**建角全线拒绝**(名字登记是 CreatePlayer
	// 的同步前置),而审计库挂了只影响 GM 工具。混成一个码,值班只能挨个试。
	//
	// 方向是 fail-closed:读不到名字可以降级成空名展示,但**登记**结果不确定时绝不能
	// 放行 —— 两个角色拿到同一个名字之后没有任何事后修复手段(v1 不支持改名)。
	ErrCodePlayerNameDBError uint32 = 25

	// ErrCodePlayerNameConflict:这个 player_id 名下已经登记了**另一个**名字。
	//
	// v1 没有改名功能,所以它只可能来自 player_id 复用(号段行被重置 / 全局库从旧备份
	// 恢复,见 ErrCodeIdSegmentUnknownTag)——也就是说请求里的这个 id 在库里已经属于
	// 另一个在役角色。绝不能"顺手把旧的删了再插新的":那会抹掉一个活人的名字,且不可
	// 回滚。返回本码 + ERROR 日志,交给人去查发号水位。
	//
	// 与 ErrCodePlayerNameDBError 分开:那个是"库坏了,重试可能好";这个是数据已经
	// 互相矛盾,重试一万次都是同一个答案。
	ErrCodePlayerNameConflict uint32 = 26

	// ── 玩家归属映射(docs/design/player-route-mapping-durability-proposal.md §15)────────

	// ErrCodeRouteStoreError:归属映射读写的库这一段出了故障 —— route store 没装配起来、route_meta 缺行或 writable≠1、
	// 锁等待超时(1205)、1213/9007 重试用尽、其它 SQL 错误;归属解析时合服映射缓存没装配。
	//
	// 只出现在 gRPC status 的 message 里(`error_code=30`,RegisterPlayerZone 的响应体是 emptypb.Empty),gRPC code 是 Unavailable,
	// **绝不**回 NotFound:一次库故障不能伪装成「这名玩家没有归属」。
	ErrCodeRouteStoreError uint32 = 30
)
