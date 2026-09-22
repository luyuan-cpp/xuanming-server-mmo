package clientplayerloginlogic

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	pbbase "proto/common/base"
	dbpb "proto/common/database"
	loginpb "proto/login"
)

// 这些用例验证当前 Login + Redis 命令路径，不替代真实 Redis AOF/数据库重启验收。
func mustUnmarshalAccount(t *testing.T, raw []byte) *dbpb.UserAccounts {
	t.Helper()
	account := &dbpb.UserAccounts{}
	if err := proto.Unmarshal(raw, account); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestAccountInitializationIsPersistentAndDoesNotOverwriteExistingBytes(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	key := constants.GetAccountDataKey("persistent-init")
	account, err := GetOrInitUserAccount(ctx, rdb, "persistent-init")
	if err != nil || len(account.GetSimplePlayers().GetPlayers()) != 0 {
		t.Fatalf("初始化失败: account=%v err=%v", account, err)
	}
	mr.FastForward(48 * time.Hour)
	if !mr.Exists(key) || mr.TTL(key) != 0 {
		t.Fatal("新账号目录不能自动过期")
	}
	// 模拟另一条写路径已经建角，同时保留未来协议字段。
	account.SimplePlayers = &pbbase.AccountSimplePlayerList{Players: []*pbbase.AccountSimplePlayer{
		{PlayerId: 42, AppearanceId: "04_mountain_guardian_boy"},
	}}
	raw, err := proto.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	raw = protowire.AppendBytes(protowire.AppendTag(raw, 91, protowire.BytesType), []byte("future-field"))
	if err := rdb.Set(ctx, key, raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := GetOrInitUserAccount(ctx, rdb, "persistent-init"); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := rdb.Get(ctx, key).Bytes()
	if err != nil || !bytes.Equal(raw, stored) || mr.TTL(key) != 0 {
		t.Fatalf("旧目录保留不得改字节或重设 TTL: err=%v ttl=%v", err, mr.TTL(key))
	}
}

type accountPersistenceAuth struct{ account string }

func (p accountPersistenceAuth) Validate(context.Context, string) (*auth.AuthResult, error) {
	return &auth.AuthResult{Account: p.account}, nil
}

var accountPersistenceAuthSequence atomic.Uint64

func TestLoginPersistsLegacyAccountAndKeepsAppearanceAcrossOldExpiry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			previousConfig := config.AppConfig
			t.Cleanup(func() { config.AppConfig = previousConfig })
			config.AppConfig.LegacyGateLoginEnabled = true
			config.AppConfig.Locker.AccountLockTTL = 10
			config.AppConfig.Account.MaxDevicesPerAccount = 3
			config.AppConfig.Account.CacheExpire = 12 * time.Hour
			config.AppConfig.Node.SessionExpireMin = 30
			config.AppConfig.HomeZone.RefreshRoleListDisabled = true
			accountName := "account-persistence"
			key := constants.GetAccountDataKey(accountName)
			wanted := &pbbase.AccountSimplePlayer{PlayerId: 42, ClassId: 3, Gender: 1,
				Name: "山岳守卫", AppearanceId: "04_mountain_guardian_boy"}
			raw, err := proto.Marshal(&dbpb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{Players: []*pbbase.AccountSimplePlayer{wanted}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := rdb.Set(ctx, key, raw, 12*time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			if legacy {
				ctx = ctxkeys.WithSessionDetails(ctx, &pbbase.SessionDetails{SessionId: 5152})
			}
			// 独立名称避免改动生产认证器或其他测试的默认认证状态。
			authType := fmt.Sprintf("account-persistence-%d", accountPersistenceAuthSequence.Add(1))
			auth.Register(authType, accountPersistenceAuth{account: accountName})
			logic := NewLoginLogic(ctx, &svc.ServiceContext{RedisClient: rdb,
				TokenManager: token.NewManager(rdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 30 * 24 * time.Hour})})
			var firstAccessToken string
			for attempt := 0; attempt < 2; attempt++ {
				resp, err := logic.Login(&loginpb.LoginRequest{AuthType: authType})
				if err != nil || resp.GetErrorMessage().GetId() != 0 || len(resp.GetPlayers()) != 1 || !proto.Equal(resp.GetPlayers()[0].GetPlayer(), wanted) {
					t.Fatalf("第 %d 次登录人物身份变化: response=%v err=%v", attempt+1, resp, err)
				}
				if ttl := mr.TTL(key); ttl != 0 {
					t.Fatalf("登录未移除旧账号 TTL: %v", ttl)
				}
				if attempt == 0 {
					firstAccessToken = resp.AccessToken
					mr.FastForward(48 * time.Hour)
					if mr.Exists("access_token:"+firstAccessToken) || (legacy && mr.Exists("login_session:5152")) {
						t.Fatal("临时 token/会话仍必须正常过期")
					}
				}
			}
			stored, err := rdb.Get(ctx, key).Bytes()
			if err != nil || !bytes.Equal(stored, raw) {
				t.Fatalf("重登不应改写账号存档: %v", err)
			}
		})
	}
}
