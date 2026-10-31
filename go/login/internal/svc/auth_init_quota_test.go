package svc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
	"shared/playerredis"
)

func TestPasswordQuotaFactoryRejectsBadConfigBeforeOpeningDependencies(t *testing.T) {
	_, _, err := productionPasswordProviderFromConfig(config.PasswordAuthConf{}, playerredis.Config{}, func(string) (string, bool) { t.Fatal("无效配额不得读DSN或开连接"); return "", false })
	if err == nil {
		t.Fatal("无效配置被接受")
	}
}

func TestPasswordQuotaPoolHasHardBoundAndNoRetries(t *testing.T) {
	loginRedis := playerredis.Config{Addrs: []string{"redis-a:6379", "redis-b:6379"}, Cluster: true, PasswordEnv: "LOGIN_REDIS_PASSWORD", PoolSizePerNode: 20, ReadTimeoutMs: 3000, DisableContextTimeout: true}
	got := passwordQuotaRedisConfig(loginRedis)
	budgetMs := int(auth.PasswordQuotaBudget.Milliseconds())
	if got.PoolSizePerNode != 4 || got.DisableContextTimeout || got.DialTimeoutMs != budgetMs || got.ReadTimeoutMs != budgetMs || got.WriteTimeoutMs != budgetMs || got.PoolTimeoutMs != budgetMs {
		t.Fatalf("生产专用池缺少硬边界: %+v", got)
	}
	if !got.Cluster || len(got.Addrs) != 2 || got.PasswordEnv != "LOGIN_REDIS_PASSWORD" {
		t.Fatalf("地址、口令、集群形态必须随登录域: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("专用池配置必须合法: %v", err)
	}
}

func TestInitAuthProvidersRealSQLCanonicalQuotaAcrossReplicas(t *testing.T) {
	if os.Getenv("LOGIN_TEST_LOCAL_AUTH_FIXTURES") != "1" {
		t.Skip("显式开启本机13316/13379隔离夹具")
	}
	// 只允许固定 loopback 专用端口；不接收任意 DSN/Redis 端点作为可写夹具。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := sql.Open("mysql", "root@tcp(127.0.0.1:13316)/?charset=utf8mb4&timeout=2s&readTimeout=2s&writeTimeout=2s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "login_quota_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanup, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error(err)
		}
	})
	dsn := "root@tcp(127.0.0.1:13316)/" + name + "?charset=utf8mb4&timeout=2s&readTimeout=2s&writeTimeout=2s"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, "CREATE TABLE user_accounts (account VARCHAR(191) PRIMARY KEY, password VARCHAR(255)) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal(err)
	}
	canonical := "Álice" + strings.TrimPrefix(name, "login_quota_")
	other := "Other" + strings.TrimPrefix(name, "login_quota_")
	encoded, err := auth.HashPassword("correct-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{canonical, other} {
		if _, err := db.ExecContext(ctx, "INSERT INTO user_accounts(account,password) VALUES(?,?)", account, encoded); err != nil {
			t.Fatal(err)
		}
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:13379", DB: 14, MaxRetries: -1, DialTimeout: auth.PasswordQuotaBudget, ReadTimeout: auth.PasswordQuotaBudget, WriteTimeout: auth.PasswordQuotaBudget, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	var keys []string
	for _, account := range []string{canonical, other} {
		sum := sha256.Sum256([]byte(account))
		keys = append(keys, "login:password:quota:v1:{known:"+hex.EncodeToString(sum[:])+"}")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := rdb.Del(cleanup, keys...).Err(); err != nil {
			t.Error(err)
		}
	})
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig = config.Config{}
	config.AppConfig.PasswordAuth = config.PasswordAuthConf{Enabled: true, DSNEnv: "LOGIN_TEST_FACTORY_DSN", MaxOpenConns: 2, MaxIdleConns: 1, KDFConcurrency: 2, KDFWaitTimeout: 500 * time.Millisecond, FailureLimit: 5, FailureWindow: 15 * time.Minute, FailureLock: 5 * time.Minute}
	config.AppConfig.Node.RedisClient = config.RedisConf{Host: "127.0.0.1:13379", DB: 14}
	t.Setenv("LOGIN_TEST_FACTORY_DSN", dsn)
	closeFirst := InitAuthProviders()
	t.Cleanup(closeFirst)
	first := auth.GetPassword()
	if first == nil {
		t.Fatal("生产工厂未注册")
	}
	second, closeSecond, err := productionPasswordProviderFromConfig(config.AppConfig.PasswordAuth, config.AppConfig.EffectiveLoginRedis(), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeSecond)
	aliases := []string{strings.Replace(canonical, "Álice", "Alice", 1), strings.ToUpper(canonical), strings.ToLower(canonical)}
	// 真 SQL 接缝先核三种 CI/重音等价别名确实回同一权威原值。
	store, err := auth.NewSQLPasswordHashStore(dsn, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		actual, hash, found, err := store.LookupPasswordHash(ctx, alias)
		if err != nil || !found || actual != canonical || hash != encoded {
			t.Fatalf("权威SQL别名未汇聚: found=%v err=%v", found, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	providers := []auth.PasswordAuthenticator{first, second}
	for index := 0; index < 5; index++ {
		if result, err := providers[index%2].ValidatePassword(ctx, aliases[index%len(aliases)], "wrong-fixture-password"); result != nil || !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("错密结算%d=%v,%v", index, result, err)
		}
	}
	for _, provider := range providers {
		if result, err := provider.ValidatePassword(ctx, canonical, "correct-fixture-password"); result != nil || !errors.Is(err, auth.ErrPasswordQuotaDenied) {
			t.Fatalf("五错后正确口令必须锁住=%v,%v", result, err)
		}
	}
	if result, err := second.ValidatePassword(ctx, other, "correct-fixture-password"); err != nil || result == nil || result.Account != other {
		t.Fatalf("不同账号不受锁影响=%v,%v", result, err)
	}
	// cleanup 必须幂等，并同时关闭只读 SQL 与专用 Redis；关闭后不能认证。
	closeFirst()
	closeFirst()
	if result, err := first.ValidatePassword(ctx, other, "correct-fixture-password"); result != nil || err == nil {
		t.Fatal("已关闭生产资源仍可认证")
	}
	t.Log("真实InitAuthProviders/双工厂+MySQL unicode_ci：三种别名汇聚同canonical，5错锁住正确口令，不同账号成功；随机库与两条自有Redis键清理")
}
