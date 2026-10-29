package logic

import (
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// 删 location 绑定「持有者已释放」(docs/design/location-release-binding.md §3.1 R1、§3.2)。
//
// location 是换手门(CZ-4)的入口条件:它在,跨节点落点必须出示当前 owner_epoch 的 handoff 标记;它不在,下一次进场按首次落点
// 铸造新 epoch、不过门。所以删 location 之前必须证明没有任何节点还持有该玩家在当前代 E 下未落盘的状态,否则新节点会从旧快照起步,
// 持有节点之后的存盘被 owner CAS 拒绝、实体被当作已废黜销毁 —— 回档到上一次成功存盘(审计 §2.1)。

// leaveReleaseVerdict 是 LeaveScene 在离场身份判为可删(legacy / deleted)之后的释放判定。名字同时是
// scene_manager_leave_holder_release_total 的 verdict 标签。底层类型 uint8 数的是结论种类数(现 8 种)。
type leaveReleaseVerdict uint8

const (
	// 以下旧枚举保留指标兼容；配置旁路、epoch缺失与等待超时均不再授权删除。
	leaveReleaseDisabled leaveReleaseVerdict = iota
	// leaveReleaseLegacyEpochZero仅保留旧指标位置；缺epoch同样不能证明已释放。
	leaveReleaseLegacyEpochZero
	// leaveReleaseNoHolder:location 的 node_id 为空(跨 zone 传送的等待落点),此刻没有任何节点持有该玩家。
	leaveReleaseNoHolder
	// leaveReleaseOwnerDead:持有节点在 etcd 注册表里正面消失且再入屏障已过(playerLocationOwnerDead),或持有进程实例
	// 已被复用同一节点号的新进程顶替、同样已消失且屏障已过(playerLocationOwnerReplaced,GO-3)。不认"zone 负载集为空"
	// (playerLocationOwnerGone 修复前的判据):负载键丢失时它会把活节点判死(第二轮审计 r2-sm#3)。
	leaveReleaseOwnerDead
	// leaveReleaseReleased:handoff 标记的 epoch 等于当前代 E —— 持有者已把 E 代状态落盘并不再写(A1′ / 交接 / 改派 / 凭证)。
	// 删除 Lua 里原子复核(R2)。
	leaveReleaseReleased
	// leaveReleaseForced保留旧指标位置，生产判定不再返回它。
	leaveReleaseForced
	// leaveReleasePending:没有证据，离场身份可证明location属于离场会话：无论等多久都保留并请原holder释放。
	leaveReleasePending
	// leaveReleasePendingUnproven:同上,但离场身份是 legacy(任一侧无身份,混版窗口):location 可能属于一次更新的登录,
	// 退出它等于踢掉在线玩家,所以只等证据、不发释放(R3)。
	leaveReleasePendingUnproven

	leaveReleaseVerdictCount
)

var leaveReleaseVerdictNames = [...]string{
	leaveReleaseDisabled:        "disabled",
	leaveReleaseLegacyEpochZero: "legacy_epoch0",
	leaveReleaseNoHolder:        "no_holder",
	leaveReleaseOwnerDead:       "owner_dead",
	leaveReleaseReleased:        "released",
	leaveReleaseForced:          "forced",
	leaveReleasePending:         "pending",
	leaveReleasePendingUnproven: "pending_unproven",
}

// 名表长度与 leaveReleaseVerdictCount 不一致时编译失败(漏加 / 多加一行)。
var _ [leaveReleaseVerdictCount]struct{} = [len(leaveReleaseVerdictNames)]struct{}{}

func (v leaveReleaseVerdict) String() string {
	if v < leaveReleaseVerdictCount {
		return leaveReleaseVerdictNames[v]
	}
	return "unknown"
}

// deletes:按这个结论删 location。released 走释放模式(Lua 复核标记),其余删除类走原文 CAS 模式。
func (v leaveReleaseVerdict) deletes() bool {
	switch v {
	case leaveReleaseNoHolder, leaveReleaseOwnerDead, leaveReleaseReleased:
		return true
	default:
		return false
	}
}

// waits:没拿到证据、需要先看等待计时的结论。
func (v leaveReleaseVerdict) waits() bool {
	return v == leaveReleasePending || v == leaveReleasePendingUnproven || v == leaveReleaseDisabled || v == leaveReleaseLegacyEpochZero || v == leaveReleaseForced
}

// holderReleaseFacts 是 decideHolderRelease 的输入,全部由调用方采好。默认值取"等"的一侧:漏填只会多等,不会误删。
type holderReleaseFacts struct {
	enabled        bool   // 整项检查开着(EffectiveLeaveSceneHolderReleaseWait 的 enabled)
	epoch          uint64 // 当前代 E:owner_epoch 键,键为 0 时取 location 记的 epoch
	noHolder       bool   // location 的 node_id 为空
	ownerDead      bool   // 持有节点已死 / 持有进程已被顶替(只认 playerLocationOwnerDead / playerLocationOwnerReplaced 的正面证据)
	markerReleased bool   // handoff 标记的 epoch == E
	waitExceeded   bool   // 历史夹具保留；不参与授权删除
	identityProven bool   // 离场身份判定为 deleted(location 的登录身份 ≤ 离场会话)
}

// decideHolderRelease 是 docs/design/location-release-binding.md §3.2 判定表,按顺序判,零副作用。
func decideHolderRelease(f holderReleaseFacts) leaveReleaseVerdict {
	switch {
	case f.noHolder:
		return leaveReleaseNoHolder
	case f.ownerDead:
		return leaveReleaseOwnerDead
	case f.markerReleased && f.epoch != 0:
		return leaveReleaseReleased
	case f.epoch == 0:
		return leaveReleasePendingUnproven
	case f.identityProven:
		return leaveReleasePending
	default:
		return leaveReleasePendingUnproven
	}
}

// leaveReleaseEpoch 是判定用的当前代 E:owner_epoch 键非 0 时用它;键为 0(缺键 / 被单独删)时取 location 记的值 ——
// 与 EnterScene「持有节点缓存为 0 只从两处同时为 0 推出」同一口径(enterscenelogic.go 同节点补种段)。
func leaveReleaseEpoch(observedEpoch uint64, loc *scene_manager.PlayerLocation) uint64 {
	if observedEpoch != 0 {
		return observedEpoch
	}
	return loc.GetOwnerEpoch()
}

// markerReleasesEpoch:handoff 标记原文是否证明 epoch 这一代已释放。没有标记 / 写坏都不算(fail-closed)。
func markerReleasesEpoch(markerRaw string, epoch uint64) bool {
	marker, err := ownerepoch.ParseHandoff(markerRaw)
	return err == nil && epoch != 0 && marker.Epoch == epoch
}

// errHolderReleasePending 是 LeaveScene 拿不到释放证据时回给调用方的错误。用 FailedPrecondition:它不是故障,
// go-zero 的熔断不计它;player_locator 按这个码识别"等持有者释放",打 INFO 并按清理通道重试(leasemonitor.go)。
// 消息前缀是固定串,便于日志检索;玩家号等只进日志不进 code。
const holderReleasePendingMessage = "holder_release_pending"

func newHolderReleasePendingError(playerID, sceneID uint64, nodeID string, epoch uint64, verdict leaveReleaseVerdict,
	waited time.Duration) error {
	return status.Error(codes.FailedPrecondition, fmt.Sprintf("%s: player=%d scene=%d node=%s owner_epoch=%d verdict=%s waited=%s",
		holderReleasePendingMessage, playerID, sceneID, nodeID, epoch, verdict, waited.Truncate(time.Second)))
}

// ── 等待计时 ─────────────────────────────────────────────────────────────────
//
// player:{id}:leave_release_wait = "{E}:{node}:{sid}:{gen}:{first_ms}",EX = 上限 + leaveReleaseWaitKeyMargin。
// 第一次拿不到证据时写入;location 的 (E, 节点, 登录身份) 变了就当作一次新的等待重新计时。删 location 的 Lua 顺手删掉它。
// 读写失败一律按"刚开始等"处理:宁可多等一轮,也不因为计时读不出来而提前 forced。
//
// 时钟只用 Redis 服务端的(脚本里 TIME):first_ms 的写入与"已等多久"的计算都在同一段 Lua 里取同一个时钟。以前用各 scene_manager
// 副本自己的墙钟,副本 A 写下 first_ms、时钟快 Δ 的副本 B 来算,就多算 Δ、提前 forced;NTP 跳变同理(第二轮审计 r2-sm#2)。
// 现在剩下的只有 Redis 服务端自己的时钟跳变(单一时钟,不再有副本间偏差);往回跳时等待时长钳在 0,只会多等。
// 脚本里调 TIME:Redis 5+ 按效果复制脚本,非确定性命令合法;部署用的是 Redis 7。

const leaveReleaseWaitKeyMargin = 10 * time.Minute

func leaveReleaseWaitKey(playerID uint64) string {
	return fmt.Sprintf("player:%d:leave_release_wait", playerID)
}

// leaveReleaseWaitScope 是等待计时所属的那一份 location。
func leaveReleaseWaitScope(epoch uint64, loc *scene_manager.PlayerLocation) string {
	return fmt.Sprintf("%d:%s:%d:%d", epoch, loc.GetNodeId(), loc.GetSessionId(), loc.GetLoginGeneration())
}

// luaNoteLeaveReleaseWait:读或登记等待计时,返回这份 location 已经等了多少毫秒(第一次登记返回 0)。
// KEYS[1]=player:{id}:leave_release_wait  ARGV[1]=scope("{E}:{node}:{sid}:{gen}")  ARGV[2]=键的 TTL 秒数
// 值的格式 "{scope}:{first_ms}";scope 里也有冒号,所以按最后一个冒号切(贪婪匹配)。first_ms 用 '%.0f' 写:毫秒时间戳约 1.8e12,
// 在 double 里无损;不用 '%d'(Lua 实现可能截成 32 位)。
const luaNoteLeaveReleaseWait = `
local t = redis.call('TIME')
local nowMs = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local v = redis.call('GET', KEYS[1])
if v then
    local scope, first = string.match(v, '^(.*):(%d+)$')
    if scope ~= nil and scope == ARGV[1] then
        local waited = nowMs - tonumber(first)
        if waited < 0 then
            waited = 0
        end
        return waited
    end
end
redis.call('SET', KEYS[1], ARGV[1] .. ':' .. string.format('%.0f', nowMs), 'EX', ARGV[2])
return 0
`

// noteLeaveReleaseWait 返回这份 location 已经等了多久(第一次见到时登记并返回 0)。时钟取 Redis 服务端的(见上),
// 不读本进程墙钟;脚本出错按"刚开始等"返回 0(只会多等)。
func noteLeaveReleaseWait(svcCtx *svc.ServiceContext, playerID uint64, scope string, budget time.Duration) time.Duration {
	ttlSeconds := int64((budget + leaveReleaseWaitKeyMargin) / time.Second)
	result, err := svcCtx.Redis.Eval(luaNoteLeaveReleaseWait, []string{leaveReleaseWaitKey(playerID)}, scope,
		strconv.FormatInt(ttlSeconds, 10))
	if err != nil {
		logx.Errorf("[LeaveRelease] wait timer unavailable, treating as just started: player=%d scope=%s err=%v",
			playerID, scope, err)
		return 0
	}
	waitedMs, ok := result.(int64)
	if !ok || waitedMs < 0 {
		logx.Errorf("[LeaveRelease] wait timer returned unexpected value, treating as just started: player=%d scope=%s result=%v",
			playerID, scope, result)
		return 0
	}
	return time.Duration(waitedMs) * time.Millisecond
}

// ── 删除 ─────────────────────────────────────────────────────────────────────

// location 删除的两种模式,即删除 Lua 的 ARGV[2]。
const (
	// leaveDeleteModeAny只供已经正证实无持有者/节点死亡的原文CAS删除；超时不授予此模式。
	leaveDeleteModeAny = "any"
	// leaveDeleteModeReleased:另外原子复核 owner_epoch 仍为 E(缺键视作未变)且 handoff 标记 epoch == E(R2)。
	leaveDeleteModeReleased = "released"
)

// leaveOutcomeNotReleased:释放模式复核时证据已不在(标记被撤回 / 换代 / owner_epoch 变了),一个字节都没动。
const leaveOutcomeNotReleased = "not_released"
