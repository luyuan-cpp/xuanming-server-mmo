package data

import (
	"context"
	"errors"

	dspb "proto/data_service"
	pb "proto/friend"
)

// playerBrief 是列表和目录要显示的玩家资料。
type playerBrief struct {
	name         string
	level        uint32
	classID      uint32
	gender       uint32
	appearanceID string
}

// readBriefs 批量读展示资料,来自 data_service 的 player_store。读不到的玩家不在结果里。
func (d OnlineDirectory) readBriefs(ctx context.Context, ids []uint64) (map[uint64]playerBrief, error) {
	if d.DataService == nil {
		return nil, errors.New("好友展示依赖未配置")
	}
	resp, err := d.DataService.BatchGetPlayerDisplay(ctx, &dspb.BatchGetPlayerDisplayRequest{PlayerIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]playerBrief, len(ids))
	for id, display := range resp.GetPlayers() {
		out[id] = playerBrief{name: display.GetName(), level: display.GetLevel(), classID: display.GetClassId(),
			gender: display.GetGender(), appearanceID: display.GetAppearanceId()}
	}
	return out, nil
}

// FillFriendProfiles 只填显示态，不写好友表或好友缓存。没有存档的好友姓名按档案契约回源，
// 等级/外观未知则留空，客户端不得伪造这些字段。
// 每批最多 64 人；失败返回已有可用资料，调用者保留原好友关系与在线状态。
func (d OnlineDirectory) FillFriendProfiles(ctx context.Context, friends []*pb.FriendEntry) error {
	if len(friends) == 0 {
		return nil
	}
	if d.DataService == nil {
		return errors.New("好友展示依赖未配置")
	}
	for start := 0; start < len(friends); start += directoryScanCount {
		end := start + directoryScanCount
		if end > len(friends) {
			end = len(friends)
		}
		batch := friends[start:end]
		ids := make([]uint64, len(batch))
		for i, entry := range batch {
			ids[i] = entry.GetFriendPlayerId()
		}
		briefs, err := d.readBriefs(ctx, ids)
		if err != nil {
			return err
		}
		var missingNames []uint64
		for i := range batch {
			if brief, ok := briefs[ids[i]]; ok {
				batch[i].Name = brief.name
				batch[i].Level = brief.level
				batch[i].ClassId = brief.classID
				batch[i].Gender = brief.gender
				batch[i].AppearanceId = brief.appearanceID
			}
			if batch[i].GetName() == "" {
				missingNames = append(missingNames, ids[i])
			}
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
