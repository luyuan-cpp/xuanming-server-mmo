package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	smpb "proto/scene_manager"
	"scene_manager/internal/svc"

	"google.golang.org/protobuf/proto"
)

func getPlayerLocationKey(playerId uint64) string {
	return fmt.Sprintf("player:%d:location", playerId)
}

// GateTopicName returns the Kafka topic name for a gate node.
func GateTopicName(gateId string) string {
	return fmt.Sprintf("gate-%s", gateId)
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
