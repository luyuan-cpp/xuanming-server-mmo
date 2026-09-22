package admin

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	"login/internal/constants"
	"login/internal/svc"
	pbbase "proto/common/base"
	dbpb "proto/common/database"
)

func TestRemovePlayerPreservesRemainingAppearanceWithoutExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	key := constants.GetAccountDataKey("admin-retention")
	kept := &pbbase.AccountSimplePlayer{PlayerId: 2, AppearanceId: "06_thunder_caster_boy"}
	raw, err := proto.Marshal(&dbpb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{Players: []*pbbase.AccountSimplePlayer{
		{PlayerId: 1, AppearanceId: "04_mountain_guardian_boy"}, kept,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, key, raw, 12*time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if removed, err := removePlayersFromAccount(ctx, &svc.ServiceContext{RedisClient: rdb}, "admin-retention", []uint64{1}); err != nil || removed != 1 {
		t.Fatalf("移除角色失败: removed=%d err=%v", removed, err)
	}
	mr.FastForward(48 * time.Hour)
	raw, err = rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	account := &dbpb.UserAccounts{}
	if err := proto.Unmarshal(raw, account); err != nil {
		t.Fatal(err)
	}
	players := account.GetSimplePlayers().GetPlayers()
	if len(players) != 1 || !proto.Equal(players[0], kept) || mr.TTL(key) != 0 {
		t.Fatalf("剩余角色身份或保留策略错误: %v", account)
	}
}
