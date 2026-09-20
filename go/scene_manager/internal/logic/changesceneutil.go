package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	smpb "proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/kafkacmd"
	"shared/ownerepoch"

	kafkago "github.com/segmentio/kafka-go"
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
	key := getPlayerLocationKey(playerId)
	val, err := svcCtx.Redis.Get(key)
	if err != nil {
		return nil, "", err
	}
	if val == "" {
		return nil, "", nil
	}

	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal([]byte(val), loc); err != nil {
		return nil, "", err
	}
	return loc, val, nil
}

// UpdatePlayerLocation 把玩家落到 (zone, scene, node),同时铸造一个新的归属 epoch。
//
// 这是 player:{id}:location 的**唯一**写入口(EnterScene 之外只有测试夹具会调它):
// location 与 owner_epoch 必须在同一段 Lua 里一起推进,分开写就会出现「epoch 已经
// 是 N+1、location 还写着 N」的窗口,C++ 侧的存盘 CAS 会据此把正常持有者当成
// 已废黜。细节见 placePlayerLocation。
func UpdatePlayerLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32) error {
	observed, err := currentOwnerEpoch(svcCtx, playerId)
	if err != nil {
		return err
	}
	_, err = placePlayerLocation(svcCtx, playerId, sceneId, nodeId, zoneId, placementGuard{observedEpoch: observed, mint: true})
	return err
}

// placedLocation 是一次成功落点留下的精确值,供路由失败时做 exact-value 回滚。
type placedLocation struct {
	// epoch 是写进 location、并随 RoutePlayerEvent 下发的 owner_epoch:
	// 铸造时 = 观察值 + 1,不铸造时 = 观察值本身。
	epoch uint64
	// minted 记录本次是否推进了 epoch,回滚据此决定要不要把 epoch 键退回去。
	minted bool
	// raw 是写进 player:{id}:location 的精确 protobuf 字节。
	raw string
}

// placementGuard 是一次落点的并发前提,全部来自本次 EnterScene 开头的同一次观察。
type placementGuard struct {
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
func placePlayerLocation(svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32,
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
	}
	data, err := proto.Marshal(loc)
	if err != nil {
		return placedLocation{}, err
	}
	raw := string(data)

	result, err := svcCtx.Redis.Eval(script,
		[]string{ownerepoch.OwnerEpochKey(playerId), getPlayerLocationKey(playerId), ownerepoch.HandoffKey(playerId)},
		strconv.FormatUint(observedEpoch, 10), raw, guard.requiredMarker)
	if err != nil {
		return placedLocation{}, err
	}
	signed, parseErr := strconv.ParseInt(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return placedLocation{}, fmt.Errorf("owner_epoch CAS 返回值非法: %v: %w", result, parseErr)
	}
	if signed == -1 {
		return placedLocation{}, errHandoffWithdrawn
	}
	if signed <= 0 {
		return placedLocation{}, errOwnerEpochConflict
	}
	returned := uint64(signed)
	if mint && returned != placedEpoch {
		// INCR 的结果与我们序列化进 location 的值不一致,只可能是 Lua 与这里的
		// 约定被改劈叉了。location 里的 epoch 已经是错的,必须当失败处理。
		return placedLocation{}, fmt.Errorf("owner_epoch 铸造结果 %d 与预期 %d 不一致", returned, placedEpoch)
	}
	return placedLocation{epoch: placedEpoch, minted: mint, raw: raw}, nil
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
// Returns 0 if not found.
func GetSceneZone(svcCtx *svc.ServiceContext, sceneId uint64) uint32 {
	val, err := svcCtx.Redis.Get(fmt.Sprintf("scene:%d:zone", sceneId))
	if err != nil || val == "" {
		return 0
	}
	z, _ := strconv.ParseUint(val, 10, 32)
	return uint32(z)
}
