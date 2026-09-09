package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	smpb "proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/kafkacmd"

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

// UpdatePlayerLocation updates the player's location using protobuf
func UpdatePlayerLocation(ctx context.Context, svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32) error {
	_, err := updatePlayerLocationWithRaw(svcCtx, playerId, sceneId, nodeId, zoneId)
	return err
}

func updatePlayerLocationWithRaw(svcCtx *svc.ServiceContext, playerId uint64, sceneId uint64, nodeId string, zoneId uint32) (string, error) {
	key := getPlayerLocationKey(playerId)

	loc := &smpb.PlayerLocation{
		SceneId:    sceneId,
		NodeId:     nodeId,
		UpdateTime: uint64(time.Now().Unix()),
		ZoneId:     zoneId,
	}

	data, err := proto.Marshal(loc)
	if err != nil {
		return "", err
	}

	raw := string(data)
	return raw, svcCtx.Redis.Set(key, raw)
}

// DeletePlayerLocation removes the player's location from Redis
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
