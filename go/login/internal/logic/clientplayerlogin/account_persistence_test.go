package clientplayerloginlogic

import (
	"context"
	"fmt"
	"strings"
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
	"shared/redisslot"
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

func TestAccountInitializationIsIndependentOfRedisAndPreservesRoleBytes(t *testing.T) {
	d := newTestDirectory()
	ctx := context.Background()
	roles, err := d.Read(ctx, "persistent-init", true)
	if err != nil || len(roles.Players) != 0 {
		t.Fatal(roles, err)
	}
	role := &pbbase.AccountSimplePlayer{PlayerId: 42, AppearanceId: "04_mountain_guardian_boy"}
	role.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 91, protowire.BytesType), []byte("future-field")))
	if _, err = d.Create(ctx, "persistent-init", role); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := d.Read(ctx, "persistent-init", true)
		if err != nil || len(got.Players) != 1 || !proto.Equal(got.Players[0], role) {
			t.Fatal(got, err)
		}
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
				ctx = ctxkeys.WithSessionDetails(ctx, &pbbase.SessionDetails{SessionId: 5152, GateInstanceId: "gate-persistence", SessionIncarnation: 1})
			}
			// 独立名称避免改动生产认证器或其他测试的默认认证状态。
			authType := fmt.Sprintf("account-persistence-%d", accountPersistenceAuthSequence.Add(1))
			auth.Register(authType, accountPersistenceAuth{account: accountName})
			logic := NewLoginLogic(ctx, &svc.ServiceContext{RedisClient: rdb, LoginRedis: rdb, AccountDirectory: fixtureDirectory(t, rdb),
				TokenManager: token.NewManager(rdb, token.Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: 30 * 24 * time.Hour})})
			loginSessionKey := redisslot.LoginSessionKey(redisslot.LoginConnectionID("gate-persistence", 1))
			for attempt := 0; attempt < 2; attempt++ {
				resp, err := logic.Login(&loginpb.LoginRequest{AuthType: authType})
				if err != nil || resp.GetErrorMessage().GetId() != 0 || len(resp.GetPlayers()) != 1 || !proto.Equal(resp.GetPlayers()[0].GetPlayer(), wanted) {
					t.Fatalf("第 %d 次登录人物身份变化: response=%v err=%v", attempt+1, resp, err)
				}

				if attempt == 0 {
					if resp.AccessToken == "" {
						t.Fatal("首次登录必须签发 access token")
					}
					mr.FastForward(48 * time.Hour)
					for _, key := range mr.Keys() {
						if strings.HasPrefix(key, "access_token:") || key == loginSessionKey {
							t.Fatalf("临时 token/会话仍必须正常过期: %s", key)
						}
					}
				}
			}
			roles := fixtureRoles(t, rdb, accountName)
			if len(roles.Players) != 1 || !proto.Equal(roles.Players[0], wanted) {
				t.Fatal("Redis旧TTL到期不得影响SQL目录")
			}

		})
	}
}
