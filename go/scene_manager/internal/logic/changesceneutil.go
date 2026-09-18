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
	_, err := placePlayerLocation(svcCtx, playerId, sceneId, nodeId, zoneId)
	return err
}

// placedLocation 是一次成功落点留下的精确值,供路由失败时做 exact-value 回滚。
type placedLocation struct {
	// epoch 是本次铸造的 owner_epoch(= 写入前的值 + 1),随 RoutePlayerEvent 下发。
	epoch uint64
	// raw 是写进 player:{id}:location 的精确 protobuf 字节。
	raw string
}

// placePlayerLocation 铸造新的归属 epoch 并写入 location,两步在同一段 Lua 里原子完成。
//
// 流程:先 GET 当前 epoch E(键不存在按 0),用 E+1 序列化 location,再执行
// luaMintEpochAndSetLocation:「GET owner_epoch == E 才 INCR 并 SET location」。
// 并发的 EnterScene 若在 GET 与 Lua 之间先把 epoch 推到 E+1,本次 CAS 失败,
// 返回 errOwnerEpochConflict —— Redis 里**什么都没改**,调用方拒绝本次请求、
// 不发路由,由后到者决定归属。
//
// 为什么不是「先 INCR 得 N,再 CAS-SET location」:那样两条命令之间进程一旦崩溃,
// 侧车键会永久领先 location 一格,而当前持有者手里还是旧 epoch,它之后每一次存盘
// 都会被 C++ 的 epoch CAS 拒掉、最终销毁实体 —— 一次 scene_manager 崩溃变成玩家
// 被踢 + 最终态丢失。把 INCR 放进 Lua 与 SET 同一原子域,这个窗口就不存在;
// 「CAS 失败 = 有并发请求抢先铸造」的语义完全一样。
//
// nodeId 为空、sceneId 为 0 表示「跨 zone 交接已放行、等待目标 zone 落点」:
// 此时没有任何节点持有该玩家,只有 zone 与 epoch 有意义(cross-zone-scene-travel.md CZ-4)。
func placePlayerLocation(svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32) (placedLocation, error) {
	current, err := currentOwnerEpoch(svcCtx, playerId)
	if err != nil {
		return placedLocation{}, err
	}
	next := current + 1

	loc := &smpb.PlayerLocation{
		SceneId:    sceneId,
		NodeId:     nodeId,
		UpdateTime: uint64(time.Now().Unix()),
		ZoneId:     zoneId,
		OwnerEpoch: next,
	}
	data, err := proto.Marshal(loc)
	if err != nil {
		return placedLocation{}, err
	}
	raw := string(data)

	result, err := svcCtx.Redis.Eval(luaMintEpochAndSetLocation,
		[]string{ownerepoch.OwnerEpochKey(playerId), getPlayerLocationKey(playerId)},
		strconv.FormatUint(current, 10), raw)
	if err != nil {
		return placedLocation{}, err
	}
	minted, parseErr := strconv.ParseUint(fmt.Sprint(result), 10, 64)
	if parseErr != nil {
		return placedLocation{}, fmt.Errorf("owner_epoch CAS 返回值非法: %v: %w", result, parseErr)
	}
	if minted == 0 {
		return placedLocation{}, errOwnerEpochConflict
	}
	if minted != next {
		// INCR 的结果与我们序列化进 location 的值不一致,只可能是 Lua 与这里的
		// 约定被改劈叉了。location 里的 epoch 已经是错的,必须当失败处理。
		return placedLocation{}, fmt.Errorf("owner_epoch 铸造结果 %d 与预期 %d 不一致", minted, next)
	}
	return placedLocation{epoch: minted, raw: raw}, nil
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
