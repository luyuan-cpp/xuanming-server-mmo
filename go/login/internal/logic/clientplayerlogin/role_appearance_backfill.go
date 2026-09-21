package clientplayerloginlogic

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	pbbase "proto/common/base"
	dbpb "proto/common/database"
	loginpb "proto/login"
)

// 空账号资料可能来自 self-heal；选角前从已有存档补展示副本，首次入场再用 CAS
// 修复账号。这里只读当前缓存，失败或未入场的新角色保持空，不臆造新人物身份。
func fillMissingRoleAppearances(ctx context.Context, rdb *redis.Client, roles []*loginpb.AccountSimplePlayerWrapper) {
	if rdb == nil {
		return
	}
	var pending []*loginpb.AccountSimplePlayerWrapper
	var keys []string
	prefix := (&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName()
	for _, role := range roles {
		if role.GetPlayer().GetPlayerId() == 0 || role.GetPlayer().GetAppearanceId() != "" {
			continue
		}
		pending = append(pending, role)
		keys = append(keys, fmt.Sprintf("%s:%d", prefix, role.GetPlayer().GetPlayerId()))
	}
	if len(keys) == 0 {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	values, err := rdb.MGet(lookupCtx, keys...).Result()
	if err != nil {
		return
	}
	for i, value := range values {
		raw, ok := value.(string)
		if !ok {
			continue
		}
		data := &dbpb.PlayerAllData{}
		if proto.Unmarshal([]byte(raw), data) != nil {
			continue
		}
		player := data.GetPlayerDatabaseData()
		role := pending[i]
		if player.GetPlayerId() != role.GetPlayer().GetPlayerId() || player.GetProfileComponent().GetAppearanceId() == "" {
			continue
		}
		copy := proto.Clone(role.Player).(*pbbase.AccountSimplePlayer)
		copy.AppearanceId = player.GetProfileComponent().GetAppearanceId()
		if copy.ClassId == 0 {
			copy.ClassId = player.GetUint32PbComponent().GetClass()
		}
		if copy.Gender == 0 {
			copy.Gender = player.GetProfileComponent().GetGender()
		}
		role.Player = copy
	}
}
