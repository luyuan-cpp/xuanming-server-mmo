package data

import (
	"context"
	"errors"
	"strconv"

	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	pb "proto/friend"
)

// FillFriendProfiles 只填显示态，不写好友表或好友缓存。离线好友的档案可能已不在
// Redis，姓名按档案契约回源，等级/外观未知则留空，客户端不得伪造这些字段。
// 每批最多 64 人；失败返回已有可用资料，调用者保留原好友关系与在线状态。
func (d OnlineDirectory) FillFriendProfiles(ctx context.Context, friends []*pb.FriendEntry) error {
	if len(friends) == 0 {
		return nil
	}
	if d.Redis == nil {
		return errors.New("好友展示缓存未配置")
	}
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	for start := 0; start < len(friends); start += directoryScanCount {
		end := start + directoryScanCount
		if end > len(friends) {
			end = len(friends)
		}
		batch := friends[start:end]
		keys := make([]string, len(batch))
		ids := make([]uint64, len(batch))
		for i, entry := range batch {
			ids[i] = entry.GetFriendPlayerId()
			keys[i] = prefix + ":" + strconv.FormatUint(ids[i], 10)
		}
		blobs, err := d.Redis.MgetCtx(ctx, keys...)
		if err != nil {
			return err
		}
		if len(blobs) != len(batch) {
			return errors.New("好友展示缓存数量不符")
		}
		var missingNames []uint64
		for i, raw := range blobs {
			profile := &dbpb.PlayerAllData{}
			if raw != "" && proto.Unmarshal([]byte(raw), profile) == nil && profile.GetPlayerDatabaseData().GetPlayerId() == ids[i] {
				player := profile.GetPlayerDatabaseData()
				batch[i].Name = player.GetProfileComponent().GetName()
				batch[i].Level = player.GetLevelComponent().GetLevel()
				batch[i].ClassId = player.GetUint32PbComponent().GetClass()
				batch[i].Gender = player.GetProfileComponent().GetGender()
				batch[i].AppearanceId = player.GetProfileComponent().GetAppearanceId()
			}
			if batch[i].GetName() == "" {
				missingNames = append(missingNames, ids[i])
			}
		}
		if d.DataService == nil {
			continue
		}
		if len(missingNames) != 0 {
			names, err := d.DataService.BatchGetPlayerName(ctx, &dspb.BatchGetPlayerNameRequest{PlayerIds: missingNames})
			if err != nil {
				return err
			}
			for _, entry := range batch {
				if entry.GetName() == "" {
					entry.Name = names.GetNames()[entry.GetFriendPlayerId()]
				}
			}
		}
		zones, err := d.DataService.BatchGetPlayerHomeZone(ctx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: ids})
		if err != nil {
			return err
		}
		for _, entry := range batch {
			entry.ZoneId = zones.GetPlayerZoneMap()[entry.GetFriendPlayerId()]
		}
	}
	return nil
}
