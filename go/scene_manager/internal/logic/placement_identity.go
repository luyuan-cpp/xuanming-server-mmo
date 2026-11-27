package logic

// 落点会话身份(docs/design/scene-enter-chain-residuals.md §1.2、§3、§5)。
//
// location 原本只记「在哪」与「哪一代归属」(owner_epoch),不记「哪次登录放行了它」。owner_epoch 只在持有节点
// 换了时铸造,同节点顶号不变,所以迟到的 LeaveScene(b)与陈旧登录 / 旧会话的同节点换图(d)都能照常改写
// location 与人数。本文件给 location 记上会话身份 (session_id, login_generation, gate_instance_id),并给出两张纯判定表:
//
//   - decidePlacementIdentity:EnterScene 入口,读完 location 与 owner_epoch 之后、任何预占 / 补种 / 写入 /
//     路由之前调用;拒绝回私有码 25(constants.ErrPlacementSessionSuperseded)。
//   - decideLeaveIdentity:LeaveScene 在 SceneId 比对之后、条件删除之前调用;判为 keep 时一个字节都不写。
//
// 比较口径(与 scene 入场栅栏 enter_fence::Decide 同源):
//   1. 以代次为主,只用 > 判新旧;唯一的相等判断是 G2 冲突检测(代次相等、会话都有效且不同)。
//   2. 任一侧代次为 0 表示不比较(旧版发送方 / 旧格式记录),退回旧行为并计 legacy,不当成「更旧」。
//   3. 同一会话(会话号相同且有效,两边 gate 实例都已知时实例也相同)一律放行,代次取 max,不因 location 记的代次落后而拒(G4)。
//      会话号由 gate 的 node_id 派生,同一 node_id 短暂有两个 gate 进程时同号可以出自两个进程,只比会话号会把老进程迟到的
//      请求当成当前会话(§14.8,口径同 scene 入场栅栏 enter_fence::SameGateInstance)。
//
// 两张表共用同一个 validSession,口径与 C++ player_exit::IsBoundSession 一致。
// 本文件只放纯函数:不读 Redis、不读时钟、不写日志与指标,调用方负责计数与日志。

import (
	smpb "proto/scene_manager"
)

// unboundSessionID 是 C++ 侧「未绑定会话」的哨兵值(player_exit::IsBoundSession 视为无效)。
const unboundSessionID uint32 = 0xFFFFFFFF

// validSession:会话号非 0 且非 0xFFFFFFFF。b、d 两张判定表共用,与 C++ player_exit::IsBoundSession 同一口径。
func validSession(sessionID uint32) bool {
	return sessionID != 0 && sessionID != unboundSessionID
}

// placementIdentity 是一次登录的会话身份:哪条会话(session_id)、第几次登录(login_generation)、
// 这条会话在哪个 gate 进程上(gate_instance_id,空 = 未知)。
// 零值 = 无身份(旧版 scene_manager 写入的 location,或不带会话的请求)。
type placementIdentity struct {
	sessionID       uint32
	loginGeneration uint64
	gateInstanceID  string
}

// absent:没有身份可比(sid 与代次都是 0)。
func (id placementIdentity) absent() bool {
	return id.sessionID == 0 && id.loginGeneration == 0
}

// identityOfLocation 取 location 里记的身份;loc 为 nil 时返回零值。
func identityOfLocation(loc *smpb.PlayerLocation) placementIdentity {
	return placementIdentity{sessionID: loc.GetSessionId(), loginGeneration: loc.GetLoginGeneration(),
		gateInstanceID: loc.GetGateInstanceId()}
}

// identityOfEnterRequest 取 EnterScene 请求代表的会话身份。托管落点不带 gate,实例为空(未知)。
func identityOfEnterRequest(in *smpb.EnterSceneRequest) placementIdentity {
	return placementIdentity{sessionID: in.GetSessionId(), loginGeneration: in.GetLoginGeneration(),
		gateInstanceID: in.GetGateInstanceId()}
}

// sameGateInstance:两个会话号相同的会话是否出自同一个 gate 进程。两边实例都已知且不同 = 另一个进程上的另一条会话;
// 任一边未知(空)不比较,按会话号认。与 C++ enter_fence::SameGateInstance 同一口径。
func sameGateInstance(cur, req string) bool {
	return cur == "" || req == "" || cur == req
}

// identityOfLeaveRequest 取 LeaveScene 请求带的被删会话身份(player_locator 从被删的会话记录里取原值)。
// 离场表以代次为主,不需要实例(LeaveSceneRequest 也不带)。
func identityOfLeaveRequest(in *smpb.LeaveSceneRequest) placementIdentity {
	return placementIdentity{sessionID: in.GetSessionId(), loginGeneration: in.GetLoginGeneration()}
}

// ---------------------------------------------------------------------------
// d:EnterScene 入口的落点身份裁决
// ---------------------------------------------------------------------------

// placementVerdict 是 decidePlacementIdentity 的结论。名表进 Prometheus label(site=enter)。
type placementVerdict uint8

const (
	// placementAdmitFresh:location 不存在或没有身份,写入请求身份。
	placementAdmitFresh placementVerdict = iota
	// placementAdmitSameSession:同一会话(或代次相等而 location 会话无效),代次取 max。
	placementAdmitSameSession
	// placementAdmitLegacy:请求无代次且会话不同。混版窗口的兼容放行,计数应在全部升级后趋近 0。
	placementAdmitLegacy
	// placementAdmitNewer:请求代次更新(或 location 无代次而请求有),写入请求身份。
	placementAdmitNewer
	// placementRejectStale:请求代次比 location 记的旧 —— 它所属的会话已被更新的登录取代。
	placementRejectStale
	// placementRejectConflict:代次相等、location 会话有效且与请求不同,违反 G2。
	placementRejectConflict
	// placementVerdictCount 只用作名表长度,不是结论。
	placementVerdictCount
)

// placementVerdictNames 按枚举顺序平铺;数组长度取 placementVerdictCount,多写一项编译失败,
// 少写一项留下空串,由 TestPlacementVerdictNamesCoverEveryValue 守住。
var placementVerdictNames = [placementVerdictCount]string{
	"fresh",
	"same_session",
	"legacy",
	"newer",
	"stale",
	"conflict",
}

func (v placementVerdict) String() string {
	if v < placementVerdictCount {
		return placementVerdictNames[v]
	}
	return "unknown"
}

// admits:结论是否放行。只有两种拒绝,其余都放行。
func (v placementVerdict) admits() bool {
	return v != placementRejectStale && v != placementRejectConflict
}

// decidePlacementIdentity 是 EnterScene 的落点会话身份裁决(scene-enter-chain-residuals.md §5.2 ① 判定表)。
//
//   - cur / curExists:本次 EnterScene **观察到的原始** location 里记的身份,以及 location 是否存在。
//     用 Gone / Dead 过滤之前的那一份:过滤只决定换手门放不放过,身份比较与它无关。
//   - req:请求的 (session_id, login_generation, gate_instance_id)。
//
// 返回结论与「放行时应写入 location 的身份」(拒绝时为零值)。按顺序判:
//
//	| 情形                                              | 结论                 | 应写入的身份                         |
//	| L 不存在(无 location,或 sid_L = 0 且 gen_L = 0)   | fresh                | R                                    |
//	| sid_R 有效且 sid_R == sid_L,实例相同或任一边未知   | same_session         | (sid, max(gen_L, gen_R), inst_R 非空取 inst_R 否则 inst_L) |
//	| gen_R == 0(会话不同)                             | legacy               | gen_L ≠ 0 时保持 L;gen_L == 0 时 (sid_R, 0, inst_R) |
//	| gen_L == 0,gen_R ≠ 0                             | newer                | R                                    |
//	| gen_R > gen_L                                     | newer                | R                                    |
//	| gen_R < gen_L                                     | stale(拒)           | —                                    |
//	| gen_R == gen_L,会话不同                          | sid_L 有效 → conflict(拒);sid_L 无效 → same_session | — / R |
//
// 「会话不同」含会话号相同而实例不同(另一个 gate 进程上的同号会话):按代次裁决,老进程迟到的请求代次更小 → stale,
// 相等 → conflict(G2),新进程上更新的登录 → newer。同一会话的请求不带实例(托管落点)时沿用 L 记的实例,不抹成未知。
//
// legacy 行**不降级**已有身份:location 已有非 0 代次时保持 L,落点照常写入,只是身份沿用 L(否则陈旧实体
// 一次无代次的换图就能把高代次身份抹成 (sid_A, 0),之后 A 的迟到 LeaveScene 判 legacy 删除、A 的换图判同会话
// 放行,b1 / d2 重新打开)。与 scene 栅栏只差这一行:「请求无代次、当前有代次、会话不同」在 scene 判
// kRejectUnversioned,这里放行 —— location 的寿命长于实体,login 混版或回滚到无代次版本期间拒绝会让玩家
// 一直进不来;scene 栅栏仍在入口兜底。
func decidePlacementIdentity(cur placementIdentity, curExists bool, req placementIdentity) (placementVerdict, placementIdentity) {
	if !curExists || cur.absent() {
		return placementAdmitFresh, req
	}
	if validSession(req.sessionID) && req.sessionID == cur.sessionID && sameGateInstance(cur.gateInstanceID, req.gateInstanceID) {
		generation := cur.loginGeneration
		if req.loginGeneration > generation {
			generation = req.loginGeneration
		}
		instance := cur.gateInstanceID
		if req.gateInstanceID != "" {
			instance = req.gateInstanceID
		}
		return placementAdmitSameSession, placementIdentity{sessionID: cur.sessionID, loginGeneration: generation,
			gateInstanceID: instance}
	}
	if req.loginGeneration == 0 {
		if cur.loginGeneration != 0 {
			return placementAdmitLegacy, cur
		}
		return placementAdmitLegacy, placementIdentity{sessionID: req.sessionID, gateInstanceID: req.gateInstanceID}
	}
	if cur.loginGeneration == 0 || req.loginGeneration > cur.loginGeneration {
		return placementAdmitNewer, req
	}
	if req.loginGeneration < cur.loginGeneration {
		return placementRejectStale, placementIdentity{}
	}
	// 代次相等、会话不同(或请求会话无效,或会话号相同而实例不同)。
	if validSession(cur.sessionID) {
		return placementRejectConflict, placementIdentity{}
	}
	return placementAdmitSameSession, req
}

// ---------------------------------------------------------------------------
// b:LeaveScene 的离场身份裁决
// ---------------------------------------------------------------------------

// leaveVerdict 是 decideLeaveIdentity 的结论。名表进 Prometheus label(site=leave)。
type leaveVerdict uint8

const (
	// leaveVerdictLegacy:任一侧代次为 0,旧行为(原文 CAS 删除并扣人数)。
	leaveVerdictLegacy leaveVerdict = iota
	// leaveVerdictKeepNewer:location 属于比离场会话更新的登录,不删、不扣人数。
	leaveVerdictKeepNewer
	// leaveVerdictKeepConflict:代次相等、会话都有效且不同(违反 G2),不删。
	leaveVerdictKeepConflict
	// leaveVerdictDeleted:location 的主人不比离场会话新,照旧原文 CAS 删除并扣人数。
	leaveVerdictDeleted
	// leaveVerdictCount 只用作名表长度,不是结论。
	leaveVerdictCount
)

// leaveVerdictNames 按枚举顺序平铺;长度守护同 placementVerdictNames,由 TestLeaveVerdictNamesCoverEveryValue 守住。
var leaveVerdictNames = [leaveVerdictCount]string{
	"legacy",
	"keep_newer",
	"keep_conflict",
	"deleted",
}

func (v leaveVerdict) String() string {
	if v < leaveVerdictCount {
		return leaveVerdictNames[v]
	}
	return "unknown"
}

// deletes:结论是否继续走原文 CAS 删除。
func (v leaveVerdict) deletes() bool {
	return v == leaveVerdictLegacy || v == leaveVerdictDeleted
}

// decideLeaveIdentity 是 LeaveScene 的离场身份裁决(scene-enter-chain-residuals.md §3.2 判定表)。
// loc = location 里记的身份(L),req = 请求带的被删会话身份(R)。按顺序判:
//
//	| gen_R == 0 或 gen_L == 0                                | legacy        | 旧行为:原文 CAS 删除 |
//	| gen_L > gen_R                                           | keep_newer    | 不删、不扣人数       |
//	| gen_L == gen_R、会话都有效且不同                         | keep_conflict | 不删(违反 G2)      |
//	| 其余(gen_L < gen_R、身份相同、代次相等而有一侧会话无效) | deleted       | 原文 CAS 删除        |
//
// 不按会话号精确匹配:同一连接登出后再登录,会话号不变而代次变大,精确匹配会误删这次新登录的 location。
// gen_L < gen_R 必须删:location 的主人比正在离场的会话还旧、已被它顶替,不删就泄漏(幽灵人数)。
// 本表相对旧行为只收窄一种情况:location 的主人比正在离场的会话新。
//
// keep_newer 不漏删的前提:被删会话记录里的代次不小于这条会话自己写进 location 的代次(§3.2;由发布顺序
// 「player_locator 先于 scene_manager」与 Reconnect 重放比代次守住)。
func decideLeaveIdentity(loc placementIdentity, req placementIdentity) leaveVerdict {
	if req.loginGeneration == 0 || loc.loginGeneration == 0 {
		return leaveVerdictLegacy
	}
	if loc.loginGeneration > req.loginGeneration {
		return leaveVerdictKeepNewer
	}
	if loc.loginGeneration == req.loginGeneration &&
		validSession(loc.sessionID) && validSession(req.sessionID) && loc.sessionID != req.sessionID {
		return leaveVerdictKeepConflict
	}
	return leaveVerdictDeleted
}
