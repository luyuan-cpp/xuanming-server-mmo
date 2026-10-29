package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	smpb "proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/kafkacmd"
	"shared/ownerepoch"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

func getPlayerLocationKey(playerId uint64) string {
	return fmt.Sprintf("player:%d:location", playerId)
}

// GateTopicName returns the Kafka topic name for gate control-plane commands.
//
// 控制面已经从"一个 gate 一个 topic"改成"一个类型一个 topic + 按 node_id % P 定分区"
// (docs/design/control-plane-topic-partitioning-20260908.md),所以 topic 名不再依赖
// gateId —— 参数留着只为调用点可读,落点由 GateCommandMessageFor 一起算。
func GateTopicName(gateId string) string {
	return kafkacmd.GateCommandTopic()
}

// GateCommandMessageFor 组一条发往指定 gate 的命令消息(topic + 显式分区一起给)。
// topic 与分区必须成对产生:分开拼一定会漂移,而漂移的表现是消息落到没人 assign
// 的分区上、Kafka 一个错都不报。
func GateCommandMessageFor(gateId string, key string, value []byte) (kafkago.Message, error) {
	return kafkacmd.GateCommandMessage(gateId, key, value)
}

// GetPlayerLocation retrieves the current scene and node for a player
func GetPlayerLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64) (*smpb.PlayerLocation, error) {
	loc, _, err := getPlayerLocationWithRaw(svcCtx, playerId)
	return loc, err
}

func getPlayerLocationWithRaw(svcCtx *svc.ServiceContext, playerId uint64) (*smpb.PlayerLocation, string, error) {
	loc, raw, _, err := getPlayerLocationSnapshot(svcCtx, playerId)
	return loc, raw, err
}

// UpdatePlayerLocation 把玩家落到 (zone, scene, node),同时铸造一个新的归属 epoch。
//
// 这是 player:{id}:location 的**唯一**写入口(EnterScene 之外只有测试夹具会调它):
// location 与 owner_epoch 必须在同一段 Lua 里一起推进,分开写就会出现「epoch 已经
// 是 N+1、location 还写着 N」的窗口,C++ 侧的存盘 CAS 会据此把正常持有者当成
// 已废黜。细节见 placePlayerLocation。
//
// guard 的 identity 与 expectLoc 都留零值(无身份、原文期望 any),行为与加会话身份之前相同 ——
// 它只供测试夹具摆初始状态,不经过 EnterScene 的身份裁决。
func UpdatePlayerLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32) error {
	observed, err := currentOwnerEpoch(svcCtx, playerId)
	if err != nil {
		return err
	}
	_, err = placePlayerLocationCtx(ctx, svcCtx, playerId, sceneId, nodeId, zoneId, placementGuard{observedEpoch: observed, mint: true})
	return err
}

// placedLocation 是一次成功落点留下的精确值,供路由失败时做 exact-value 回滚(回滚 Lua 以「location 仍是
// raw、epoch 仍是 epoch」为前提,见 owner_epoch.go luaRollbackPlayerPlacement)。
type placedLocation struct {
	revision    uint64
	tag         string
	previousTag string
	// epoch 是写进 location、并随 RoutePlayerEvent 下发的 owner_epoch:
	// 铸造时 = 观察值 + 1,不铸造时 = 观察值本身。
	epoch uint64
	// minted 记录本次是否推进了 epoch,回滚据此选择 bump(再 INCR 一格,epoch 只进不退)或 keep
	// (epoch 键不动,只退 location);见 planRouteRollback。
	minted bool
	// raw 是写进 player:{id}:location 的精确 protobuf 字节。
	raw string
}

// placementGuard 是一次落点的并发前提,全部来自本次 EnterScene 开头的同一次观察。
type placementGuard struct {
	ownerRevision uint64
	observedTag   string
	// observedEpoch:落点 Lua 的 CAS 期望值(键不存在按 0)。
	observedEpoch uint64
	// mint:持有者换了才铸造新 epoch(见 placePlayerLocation)。
	mint bool
	// requiredMarker:非空 = 本次放行凭的是这份 handoff 标记原文,落点 Lua 里它必须
	// 原样还在(见 luaMintEpochAndSetLocation)。只对 mint=true 有意义。
	requiredMarker string
	// pendingSceneConfID 不是并发前提,是随「等待落点」一起写进 location 的目标地图
	// (PlayerLocation.pending_scene_conf_id);放在这里只为不再给 placePlayerLocation 加位置参数。
	// 常规落点(nodeId 非空)必须留 0。
	pendingSceneConfID uint64
	// identity 也不是并发前提,是本次落点要写进 location 的会话身份(decidePlacementIdentity 给出的
	// 「应写入的身份」,scene-enter-chain-residuals.md §5.2 ②)。零值 = 无身份,只有测试夹具会这样写。
	identity placementIdentity
	// expectLoc:location 原文期望(落点 Lua 的 ARGV[4] / ARGV[5],见 luaLocationExpectCheck)。
	// 零值 mode 即 any(不检查),**只供测试夹具与 UpdatePlayerLocation 使用**;生产构造点(跨区 guard、
	// 常规 guard、samePlacement 的身份写)必须用 expectObservedLocation 显式填 missing / present ——
	// 漏填时原文 CAS 静默失效,由 enterscene_identity_test.go 里每处构造点的「观察之后 location 变化 → 19」用例守住。
	expectLoc locationExpectation
}

// locationExpectMode 是落点 Lua 对 location 原文的期望模式(ARGV[4])。零值 = any。
type locationExpectMode uint8

const (
	// locationExpectAny:不检查(ARGV[4] 传空串)。只供测试夹具与 UpdatePlayerLocation。
	locationExpectAny locationExpectMode = iota
	// locationExpectMissing:观察时没有 location,提交时也必须没有。
	locationExpectMissing
	// locationExpectPresent:提交时 location 必须逐字节等于观察原文。
	locationExpectPresent
)

// luaArg 是 Lua 实参里的写法,与 luaLocationExpectCheck 的字面量一一对应。
func (m locationExpectMode) luaArg() string {
	switch m {
	case locationExpectMissing:
		return "missing"
	case locationExpectPresent:
		return "present"
	default:
		return ""
	}
}

// locationExpectation 是一次落点对 location 原文的期望:模式 + present 时的期望原文。
type locationExpectation struct {
	mode locationExpectMode
	raw  string
}

// expectObservedLocation 按本次 EnterScene 开头观察到的 location 原文给出期望:没有 location = missing,
// 否则 present + 观察原文。observedRaw 必须是 Gone / Dead 过滤**之前**的原文 —— 过滤只决定换手门放不放过,
// 提交时 Redis 里的 location 仍是观察到的那一份才说明观察之后没人改过它。
func expectObservedLocation(observedRaw string) locationExpectation {
	if observedRaw == "" {
		return locationExpectation{mode: locationExpectMissing}
	}
	return locationExpectation{mode: locationExpectPresent, raw: observedRaw}
}

// placePlayerLocation 写入 location,并按 mint 决定是否同时铸造新的归属 epoch;
// 「epoch 仍是 observedEpoch」的比对与写入在同一段 Lua 里原子完成。
//
// observedEpoch 是调用方在本次 EnterScene 开头读到的值(键不存在按 0),换手门的
// 标记比对用的也是它。这里**不重新 GET**:本次决策(源是否已落盘、要不要铸造)
// 全部建立在那一次观察上,观察之后只要有并发 EnterScene 推进过 epoch,CAS 就必须
// 失败 —— 返回 errOwnerEpochConflict,Redis 里**什么都没改**,调用方拒绝本次请求、
// 不发路由,由先到者决定归属。
//
// 同一段 Lua 还比 location 原文(guard.expectLoc,scene-enter-chain-residuals.md §5.2 ③):本次的会话身份判定
// 也建立在那一次观察上,观察之后 location 被改写或删除(另一次登录写了身份、LeaveScene 删了它)时同样返回
// errOwnerEpochConflict。owner_epoch CAS 只挡得住「有人铸造」,同节点不铸造的落点与 samePlacement 改写
// location 不动 epoch,只有原文期望挡得住。写入的 location 带 guard.identity。
//
// mint=true(持有者换了:首次落点 / 过了标记门的交接 / 跨 zone 放行 / 等待落点
// 的第二条腿)→ luaMintEpochAndSetLocation,location 里记 observedEpoch+1。
// mint=false(持有者没换:同节点换图;或 dev 旁路下无标记的跨节点交接,保持旧的
// 竞态语义让旧节点的释放存盘照常落地)→ luaSetLocationIfEpoch,epoch 不动。
//
// 为什么不是「先 INCR 得 N,再 CAS-SET location」:那样两条命令之间进程一旦崩溃,
// 侧车键会永久领先 location 一格,而当前持有者手里还是旧 epoch,它之后每一次存盘
// 都会被 C++ 的 epoch CAS 拒掉、最终销毁实体 —— 一次 scene_manager 崩溃变成玩家
// 被踢 + 最终态丢失。把 INCR 放进 Lua 与 SET 同一原子域,这个窗口就不存在;
// 「CAS 失败 = 有并发请求抢先铸造」的语义完全一样。
//
// nodeId 为空、sceneId 为 0 表示「跨 zone 交接已放行、等待目标 zone 落点」:
// 此时没有任何节点持有该玩家,只有 zone 与 epoch 有意义(cross-zone-scene-travel.md CZ-4)。
//
// 本函数不带 ctx,铸造的 WAITAOF 按配置值等、不受请求截止约束 —— 只给单测直接调用;生产入口(EnterScene 的两处落点、
// UpdatePlayerLocation)一律走 placePlayerLocationCtx。
func placePlayerLocation(svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32,
	guard placementGuard) (placedLocation, error) {
	return placePlayerLocationCtx(context.Background(), svcCtx, playerId, sceneId, nodeId, zoneId, guard)
}

// placePlayerLocationCtx 是 placePlayerLocation 的本体。ctx 是请求 ctx:mint=true 时铸造的截止时间由它推出
// (ownerEpochMintContext,给铸造之后的同步推路由留足时间)。
//
// mint=true 的铸造经 svcCtx.OwnerEpochMint 执行:铸造 EVAL 与紧随的 WAITAOF 在同一条连接、同一个 pipeline 里
// (docs/design/zone-redis-regression-guard-20261022.md §4.3.2 G3b)。返回值在原有之外多两种:
//   - errOwnerEpochMintNotDurable(包装):铸造已写进 Redis 但没确认落盘,**placed 有效**,调用方按推路由失败回滚;
//   - svc.ErrOwnerEpochMintNotSent(errors.Is 可判):剩余预算不够发一次 WAITAOF,一条都没发,Redis 一个字节都没改;
//     调用方按通用落点错误回 3。
//
// 铸造 EVAL 结果未知(传输错误重发耗尽)时返回普通错误:首发可能已生效,与以前 go-zero 自动重发耗尽同一语义。
// mint=false 的 CAS 写不铸造新 epoch,不发 WAITAOF,仍走 svcCtx.Redis。
func placePlayerLocationCtx(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32,
	guard placementGuard) (placedLocation, error) {
	observedEpoch, mint := guard.observedEpoch, guard.mint
	placedEpoch := observedEpoch
	script := luaSetLocationIfEpoch
	if mint {
		placedEpoch = observedEpoch + 1
		script = luaMintEpochAndSetLocation
	}

	loc := &smpb.PlayerLocation{
		SceneId:    sceneId,
		NodeId:     nodeId,
		UpdateTime: uint64(time.Now().Unix()),
		ZoneId:     zoneId,
		OwnerEpoch: placedEpoch,

		PendingSceneConfId: guard.pendingSceneConfID,

		SessionId:       guard.identity.sessionID,
		LoginGeneration: guard.identity.loginGeneration,
	}
	data, err := proto.Marshal(loc)
	if err != nil {
		return placedLocation{}, err
	}
	raw := string(data)

	keys := []string{ownerepoch.OwnerEpochKey(playerId), getPlayerLocationKey(playerId), ownerepoch.HandoffKey(playerId), locationRevisionKey(playerId), locationRevisionFloorKey(playerId)}
	args := []any{strconv.FormatUint(observedEpoch, 10), raw, guard.requiredMarker,
		guard.expectLoc.mode.luaArg(), guard.expectLoc.raw, optionalRevisionText(guard.ownerRevision), locationRevisionTag(guard.ownerRevision, raw), guard.observedTag}
	var result any
	var minted svc.MintEvalResult
	if mint {
		if svcCtx.OwnerEpochMint == nil {
			return placedLocation{}, errOwnerEpochMinterMissing
		}
		mintCtx, cancel := ownerEpochMintContext(ctx, &svcCtx.Config)
		minted, err = svcCtx.OwnerEpochMint.EvalAndConfirm(mintCtx, script, keys, args...)
		cancel()
		if err != nil {
			return placedLocation{}, err
		}
		result = minted.Reply
	} else {
		result, err = svcCtx.Redis.Eval(script, keys, args...)
		if err != nil {
			return placedLocation{}, err
		}
	}
	signed, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return placedLocation{}, fmt.Errorf("owner_epoch CAS 返回值非法: %v: %w", result, parseErr)
	}
	replayed := false
	if mint {
		signed, replayed = decodeMintReply(signed)
	}
	if signed == -1 {
		return placedLocation{}, errHandoffWithdrawn
	}
	if signed <= 0 {
		if mint && minted.Attempts > 1 {
			// 首发结果未知、在新连接上重发后回 0。凭标记的铸造会被重放识别认出来,走不到这里;不凭标记的铸造(首次落点、
			// 等待落点的第二条腿、同节点 epoch 0)认不出自己的写,首发可能已生效 —— 已登记的残余(owner_epoch.go
			// luaMintEpochAndSetLocation「不带标记的铸造不做识别」),照旧按并发冲突回 19、不发路由、不回滚。
			logx.Errorf("[OwnerEpochMint] mint EVAL resent after an unknown first result and the CAS now fails; the first attempt may have applied: player=%d observed_epoch=%d attempts=%d marker=%v",
				playerId, observedEpoch, minted.Attempts, guard.requiredMarker != "")
		}
		return placedLocation{}, errOwnerEpochConflict
	}
	returned := uint64(signed)
	if mint && returned != placedEpoch {
		// INCR 的结果与我们序列化进 location 的值不一致,只可能是 Lua 与这里的
		// 约定被改劈叉了。location 里的 epoch 已经是错的,必须当失败处理。
		return placedLocation{}, fmt.Errorf("owner_epoch 铸造结果 %d 与预期 %d 不一致", returned, placedEpoch)
	}
	if replayed {
		// 铸造 EVAL 首发已生效、应答丢了,铸造通道在新连接上原样重发,被脚本认成「本请求已生效」(脚本在这一支把 epoch
		// 原值写回一次,所以紧随的 WAITAOF 覆盖首发那次写)。
		// 以前这里回 19、既不发重定向也不回滚;应恒近 0,持续出现说明 zone Redis 读超时频繁。
		metrics.ObserveEnterSceneMintReplayRecognized()
		logx.Infof("[MintReplay] player=%d epoch=%d zone=%d attempts=%d: owner_epoch mint EVAL was resent after an unknown result; recognised as this request's own write",
			playerId, placedEpoch, zoneId, minted.Attempts)
	}
	placed := placedLocation{epoch: placedEpoch, minted: mint, raw: raw, revision: guard.ownerRevision, tag: locationRevisionTag(guard.ownerRevision, raw), previousTag: guard.observedTag}
	if mint {
		// 写进 Redis 的铸造才判落盘(上面 CAS 落败 / 标记撤回什么都没写,已经返回)。没确认时 placed 原样交回,供回滚。
		if err := observeMintDurability(minted); err != nil {
			return placed, err
		}
	}
	return placed, nil
}

// decodeMintReply 把 luaMintEpochAndSetLocation 的返回值拆成 (epoch 或状态码, 是否重放识别命中)。
// 只对铸造脚本调用:重放识别命中时脚本返回 -新epoch(≤ -2),其余返回值(-1 / 0 / 新 epoch)原样透传。
func decodeMintReply(signed int64) (int64, bool) {
	if signed < -1 {
		return -signed, true
	}
	return signed, false
}

// setLocationIdentityIfUnchanged 是 samePlacement(同一落点重连)的身份写(scene-enter-chain-residuals.md §5.2 ②):
// 克隆观察到的 location,保留 owner_epoch、rollback_receipt、update_time、pending_scene_conf_id 等全部字段,
// 只把会话身份改成 identity,用不铸造的 luaSetLocationIfEpoch 写回(present 模式,期望 = 观察原文,
// epoch = observedEpoch)。
//
//   - observed / observedRaw:本次 EnterScene 开头观察到的 location 与原文,必须成对且非空。
//   - 返回 errOwnerEpochConflict:epoch 被并发推进,或 location 在观察之后被改写 / 删除(例如 LeaveScene 删了它),
//     Redis 一个字节都没改;调用方回 19、不发路由。
//   - 只改身份,不动人数,也不动 owner_epoch 键:归属没变,持有节点的缓存必须继续合法。
//
// 调用方只在「应写入的身份 ≠ location 记的身份」时调用。写入之后若推路由失败,**不回滚身份**(§5.2 ② v0.2):
// kafka-go 报错但 broker 其实已投递时,回滚会把身份退回旧会话,重开迟到 LeaveScene 误删(b1)与陈旧换图放行(d2)。
func setLocationIdentityIfUnchanged(svcCtx *svc.ServiceContext, playerId uint64, observed *smpb.PlayerLocation,
	observedRaw string, identity placementIdentity, observedEpoch, ownerRevision uint64, observedTag string) error {
	if observed == nil || observedRaw == "" {
		return fmt.Errorf("同落点身份写缺少观察到的 location: observed_nil=%v raw_len=%d", observed == nil, len(observedRaw))
	}
	updated := proto.Clone(observed).(*smpb.PlayerLocation)
	updated.SessionId = identity.sessionID
	updated.LoginGeneration = identity.loginGeneration
	data, err := proto.Marshal(updated)
	if err != nil {
		return err
	}
	expect := expectObservedLocation(observedRaw)
	newTag := ""
	if observedTag != "" {
		newTag = locationRevisionTag(ownerRevision, string(data))
	}
	result, err := svcCtx.Redis.Eval(luaSetLocationIfEpoch,
		[]string{ownerepoch.OwnerEpochKey(playerId), getPlayerLocationKey(playerId), ownerepoch.HandoffKey(playerId), locationRevisionKey(playerId), locationRevisionFloorKey(playerId)},
		strconv.FormatUint(observedEpoch, 10), string(data), "", expect.mode.luaArg(), expect.raw, optionalRevisionText(ownerRevision), newTag, observedTag)
	if err != nil {
		return err
	}
	signed, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return fmt.Errorf("同落点身份写返回值非法: %v: %w", result, parseErr)
	}
	if signed <= 0 {
		return errOwnerEpochConflict
	}
	return nil
}

// DeletePlayerLocation removes the player's location from Redis.
//
// 刻意**不删** player:{id}:owner_epoch:epoch 是单调递增的归属代际,删掉再从 1
// 开始铸造会让一个旧节点手里的旧值重新"合法";下一次 EnterScene 在它之上继续 INCR。
func DeletePlayerLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64) error {
	key := getPlayerLocationKey(playerId)
	_, err := svcCtx.Redis.Del(key)
	return err
}

// GetSceneZone looks up the zone_id for a scene from Redis (scene:{id}:zone).
// Returns 0 if not found —— 读出错与值损坏也返回 0。0 会被调用方当作「没记录 zone」放行的
// 判定(例如 resolveScene 的跨 zone 守卫)要改用 lookupSceneZone。
func GetSceneZone(svcCtx *svc.ServiceContext, sceneId uint64) uint32 {
	z, _ := lookupSceneZone(svcCtx, sceneId)
	return z
}

// lookupSceneZone 读 scene:{id}:zone,区分「没记录」(0, nil)与「读出错 / 值损坏」(0, err)。
func lookupSceneZone(svcCtx *svc.ServiceContext, sceneId uint64) (uint32, error) {
	// go-zero 的 Get 把 redis.Nil 吞成 ("", nil):键不存在就是 val 为空这一支。
	val, err := svcCtx.Redis.Get(sceneZoneKey(sceneId))
	if err != nil {
		return 0, err
	}
	if val == "" {
		return 0, nil
	}
	z, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q: %w", sceneZoneKey(sceneId), val, err)
	}
	return uint32(z), nil
}
