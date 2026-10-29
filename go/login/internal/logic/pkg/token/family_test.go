package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func familyData(t *testing.T, rdb *redis.Client, token string) TokenData {
	t.Helper()
	raw, err := rdb.Get(context.Background(), refreshTokenPrefix+token).Bytes()
	if err != nil {
		t.Fatal("读取本用例 refresh 失败", err)
	}
	var data TokenData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// 独立连接不携带并发注入 hook；hash 字段也进入原子性比较，不能只比较键类型。
func familySnapshot(t *testing.T, mr *miniredis.Miniredis) map[string]string {
	t.Helper()
	out := snapshot(t, mr)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	for _, key := range mr.Keys() {
		if mr.Type(key) != "hash" {
			continue
		}
		fields, err := rdb.HGetAll(context.Background(), key).Result()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		out[key] = string(raw) + " ttl=" + mr.TTL(key).String()
	}
	return out
}

func TestTokenFamily_IssueRotationAndAccountRevocation(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	old := issueSeed(t, m)
	data := familyData(t, rdb, old.RefreshToken)
	rawID, err := base64.RawURLEncoding.DecodeString(data.FamilyID)
	if err != nil || len(data.FamilyID) != 22 || len(rawID) != 16 {
		t.Fatal("family 必须为 16 随机字节的规范 base64url")
	}
	key := refreshFamilyPrefix + data.FamilyID
	fields, err := rdb.HGetAll(ctx, key).Result()
	if err != nil || fields["account"] != testAccount || fields["head"] != old.RefreshToken || mr.TTL(key) != testConfig.RefreshTokenTTL {
		t.Fatal("新认证必须同时提交 family 归属、head 与 TTL")
	}
	other, err := m.Issue(ctx, "other-account", testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Issue(ctx, testAccount, testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	if familyData(t, rdb, second.RefreshToken).FamilyID == data.FamilyID {
		t.Fatal("同账号再次认证必须建立独立随机 family")
	}
	got, err := m.Refresh(ctx, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if familyData(t, rdb, got.RefreshToken).FamilyID != data.FamilyID {
		t.Fatal("轮换不得建立另一条 family")
	}
	if _, err := m.ValidateAccess(ctx, old.AccessToken); err != nil {
		t.Fatal("正常轮换不得撤销同 family 未过期 access", err)
	}
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{old.AccessToken, got.AccessToken, second.AccessToken} {
		if _, err := m.ValidateAccess(ctx, access); err == nil {
			t.Fatal("撤销 family 后旧新 access 均须拒绝")
		}
	}
	if _, err := m.Refresh(ctx, got.RefreshToken); err == nil {
		t.Fatal("撤销后不能刷新复活")
	}
	if _, err := m.ValidateAccess(ctx, other.AccessToken); err != nil {
		t.Fatal("不能误撤其它账号", err)
	}
}

func TestTokenFamily_LegacyCompatibilityAndEnrollment(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	access, _ := generateToken()
	refresh, _ := generateToken()
	raw, err := json.Marshal(TokenData{Account: testAccount, AuthType: testAuthType, DeviceID: testDevice, CreatedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, accessTokenPrefix+access, raw, testConfig.AccessTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, refreshTokenPrefix+refresh, raw, testConfig.RefreshTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.ZAdd(ctx, accountRefreshKey+testAccount, redis.Z{Score: float64(time.Now().Add(testConfig.RefreshTokenTTL).Unix()), Member: refresh}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ValidateAccess(ctx, access); err != nil {
		t.Fatal("旧 access 按原 TTL 兼容", err)
	}
	got, err := m.Refresh(ctx, refresh)
	if err != nil {
		t.Fatal(err)
	}
	if !validFamilyID(familyData(t, rdb, got.RefreshToken).FamilyID) {
		t.Fatal("旧 refresh 首次轮换必须收编为真实随机 family")
	}
	if _, err := m.Refresh(ctx, refresh); err == nil {
		t.Fatal("旧 refresh 收编仍只准一次")
	}
	if len(keysWithPrefix(mr, refreshFamilyPrefix)) != 1 {
		t.Fatal("不能伪造或重复收编 family")
	}
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ValidateAccess(ctx, got.AccessToken); err == nil {
		t.Fatal("收编后的 access 必须可撤销")
	}
	if _, err := m.ValidateAccess(ctx, access); err != nil {
		t.Fatal("不追认旧无 family access 已被撤销", err)
	}
}

func TestTokenFamily_InvalidFamilyFailsClosedWithoutRotation(t *testing.T) {
	for _, kind := range []string{"missing", "other-account", "stale-head", "wrong-type", "missing-head-token", "missing-index", "invalid-id", "noncanonical-id", "empty-id", "null-id", "persistent-family", "persistent-refresh"} {
		t.Run(kind, func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			ctx := context.Background()
			pair := issueSeed(t, m)
			data := familyData(t, rdb, pair.RefreshToken)
			key := refreshFamilyPrefix + data.FamilyID
			switch kind {
			case "missing":
				mr.Del(key)
			case "other-account":
				rdb.HSet(ctx, key, "account", "different-account")
			case "stale-head":
				rdb.HSet(ctx, key, "head", "another-head")
			case "wrong-type":
				mr.Del(key)
				mr.Set(key, "broken")
			case "missing-head-token":
				mr.Del(refreshTokenPrefix + pair.RefreshToken)
			case "missing-index":
				mr.Del(accountRefreshKey + testAccount)
			case "persistent-family", "persistent-refresh":
				persistentKey := key
				if kind == "persistent-refresh" {
					persistentKey = refreshTokenPrefix + pair.RefreshToken
				}
				if removed, err := rdb.Persist(ctx, persistentKey).Result(); err != nil || !removed {
					t.Fatal("夹具必须移除原正 TTL", err)
				}
			case "invalid-id", "noncanonical-id":
				data.FamilyID = strings.Repeat("A", 21) + "!"
				if kind == "noncanonical-id" {
					data.FamilyID = strings.Repeat("A", 21) + "B"
				}
				raw, _ := json.Marshal(data)
				rdb.Set(ctx, accessTokenPrefix+pair.AccessToken, raw, testConfig.AccessTokenTTL)
				rdb.Set(ctx, refreshTokenPrefix+pair.RefreshToken, raw, testConfig.RefreshTokenTTL)
			case "empty-id", "null-id":
				fields := map[string]any{"account": data.Account, "auth_type": data.AuthType, "created_at": data.CreatedAt, "family_id": ""}
				if kind == "null-id" {
					fields["family_id"] = nil
				}
				raw, _ := json.Marshal(fields)
				rdb.Set(ctx, accessTokenPrefix+pair.AccessToken, raw, testConfig.AccessTokenTTL)
				rdb.Set(ctx, refreshTokenPrefix+pair.RefreshToken, raw, testConfig.RefreshTokenTTL)
			}
			before := familySnapshot(t, mr)
			if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
				t.Fatal("损坏 family 不得认证")
			}
			if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
				t.Fatal("损坏 family 不得签出后继")
			}
			if !maps.Equal(before, familySnapshot(t, mr)) {
				t.Fatal("拒绝必须不修改原 token/family/索引")
			}
		})
	}
}

func TestTokenFamily_ExpiredFamilyRejectsUnexpiredAccess(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	m.cfg = Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Minute}
	pair := issueSeed(t, m)
	mr.FastForward(time.Minute + time.Millisecond)
	if !mr.Exists(accessTokenPrefix + pair.AccessToken) {
		t.Fatal("夹具须保留尚未过期 access")
	}
	if _, err := m.ValidateAccess(context.Background(), pair.AccessToken); err == nil {
		t.Fatal("family 到期不能只凭 access 继续认证")
	}
	_ = rdb
	t.Run("persistent-access", func(t *testing.T) {
		m, rdb, mr := newTestManager(t)
		ctx := context.Background()
		pair := issueSeed(t, m)
		if removed, err := rdb.Persist(ctx, accessTokenPrefix+pair.AccessToken).Result(); err != nil || !removed {
			t.Fatal("夹具必须移除 access 的原正 TTL", err)
		}
		before := familySnapshot(t, mr)
		if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
			t.Fatal("新 family 的永久 access 必须拒绝认证")
		}
		if !maps.Equal(before, familySnapshot(t, mr)) {
			t.Fatal("拒绝永久 access 不能修改 token/family/索引")
		}
	})
}

func TestTokenFamily_ConcurrentRefreshHasOneWinner(t *testing.T) {
	m, rdb, _ := newTestManager(t)
	old := issueSeed(t, m)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := m.Refresh(context.Background(), old.RefreshToken)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("同一 refresh 只能有一个赢家，实际 %d", winners)
	}
	if n := rdb.ZCard(context.Background(), accountRefreshKey+testAccount).Val(); n != 1 {
		t.Fatal("不能重复推进 head")
	}
}

func TestTokenFamily_TrimStaleMemberCannotDeleteRotatedFamily(t *testing.T) {
	m, rdb, _ := newTestManager(t)
	ctx := context.Background()
	old := issueSeed(t, m)
	oldData := familyData(t, rdb, old.RefreshToken)
	got, err := m.Refresh(ctx, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧候选残留；当前 head 已经推进，淘汰此旧成员不能删除新 head 的 family。
	raw, _ := json.Marshal(oldData)
	rdb.Set(ctx, refreshTokenPrefix+old.RefreshToken, raw, testConfig.RefreshTokenTTL)
	rdb.ZAdd(ctx, accountRefreshKey+testAccount, redis.Z{Score: -1, Member: old.RefreshToken})
	legacy := oldData
	legacy.FamilyID = ""
	raw, _ = json.Marshal(legacy)
	for i := 0; i < maxRefreshTokensPerAccount-1; i++ {
		token, err := generateToken()
		if err != nil {
			t.Fatal(err)
		}
		rdb.Set(ctx, refreshTokenPrefix+token, raw, testConfig.RefreshTokenTTL)
		rdb.ZAdd(ctx, accountRefreshKey+testAccount, redis.Z{Score: float64(i + 1), Member: token})
	}
	m.trimRefreshSet(ctx, testAccount)
	if n := rdb.ZCard(ctx, accountRefreshKey+testAccount).Val(); n != maxRefreshTokensPerAccount {
		t.Fatal("容量整理应在一个提交中完成")
	}
	if _, err := m.ValidateAccess(ctx, got.AccessToken); err != nil {
		t.Fatal("旧候选不能误删已轮换的新 family", err)
	}
}

func TestTokenFamily_RevokeDoesNotDeleteAuthenticationIssuedAfterCommit(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	issueSeed(t, m)
	if err := retireRefreshScript.Load(ctx, rdb).Err(); err != nil {
		t.Fatal(err)
	}
	otherClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer otherClient.Close()
	otherManager := NewManager(otherClient, testConfig)
	var next *TokenPair
	rdb.AddHook(&afterFirstCommand{do: func() {
		var err error
		next, err = otherManager.Issue(ctx, testAccount, testAuthType, testDevice)
		if err != nil {
			t.Fatal(err)
		}
	}})
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("夹具必须在撤销提交后创建新认证")
	}
	if _, err := otherManager.ValidateAccess(ctx, next.AccessToken); err != nil {
		t.Fatal("旧撤销不得误删提交后的新 family", err)
	}
}

func TestTokenFamily_CrossAccountIndexCorruptionCannotDeleteForeignFamily(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	issueSeed(t, m)
	other, err := m.Issue(ctx, "foreign-account", testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	rdb.ZAdd(ctx, accountRefreshKey+testAccount, redis.Z{Score: 0, Member: other.RefreshToken})
	before := familySnapshot(t, mr)
	if err := m.RevokeAll(ctx, testAccount); err == nil {
		t.Fatal("跨账号索引损坏必须拒绝")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("所有候选核验完前不得删除部分 family")
	}
	if _, err := m.ValidateAccess(ctx, other.AccessToken); err != nil {
		t.Fatal("不得误删其它账号 family", err)
	}
}

func TestTokenFamily_WrongIndexTypeWritesNothing(t *testing.T) {
	m, _, mr := newTestManager(t)
	ctx := context.Background()
	pair := issueSeed(t, m)
	mr.Del(accountRefreshKey + testAccount)
	if err := mr.Set(accountRefreshKey+testAccount, "broken-index"); err != nil {
		t.Fatal(err)
	}
	before := familySnapshot(t, mr)
	if _, err := m.Issue(ctx, testAccount, testAuthType, testDevice); err == nil {
		t.Fatal("损坏 ZSET 不得签出部分新 pair")
	}
	if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("损坏 ZSET 不得轮换")
	}
	if err := m.RevokeAll(ctx, testAccount); err == nil {
		t.Fatal("损坏 ZSET 不得冒撤销成功")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("WRONGTYPE 路径必须在任何写入前拒绝")
	}
}

func TestTokenFamily_DamagedRevocationCandidateWritesNothing(t *testing.T) {
	for _, kind := range []string{"foreign-head", "wrong-type", "missing-head", "malformed-payload", "empty-family", "persistent-refresh", "persistent-family"} {
		t.Run(kind, func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			ctx := context.Background()
			issueSeed(t, m)
			bad := issueSeed(t, m)
			data := familyData(t, rdb, bad.RefreshToken)
			key := refreshFamilyPrefix + data.FamilyID
			// 保证损坏候选排在正常候选之后，仍不能先删除正常候选。
			rdb.ZAdd(ctx, accountRefreshKey+testAccount, redis.Z{Score: float64(time.Now().Add(testConfig.RefreshTokenTTL).Unix() + 100), Member: bad.RefreshToken})
			switch kind {
			case "foreign-head":
				foreign, err := m.Issue(ctx, "foreign-head-account", testAuthType, testDevice)
				if err != nil {
					t.Fatal(err)
				}
				rdb.HSet(ctx, key, "head", foreign.RefreshToken)
			case "wrong-type":
				mr.Del(key)
				if err := mr.Set(key, "broken-family"); err != nil {
					t.Fatal(err)
				}
			case "missing-head":
				rdb.HDel(ctx, key, "head")
			case "malformed-payload":
				rdb.Set(ctx, refreshTokenPrefix+bad.RefreshToken, "{", testConfig.RefreshTokenTTL)
			case "persistent-refresh", "persistent-family":
				persistentKey := key
				if kind == "persistent-refresh" {
					persistentKey = refreshTokenPrefix + bad.RefreshToken
				}
				if removed, err := rdb.Persist(ctx, persistentKey).Result(); err != nil || !removed {
					t.Fatal("夹具必须移除撤销候选的原正 TTL", err)
				}
			case "empty-family":
				fields := map[string]any{"account": data.Account, "auth_type": data.AuthType, "device_id": data.DeviceID, "created_at": data.CreatedAt, "family_id": ""}
				payload, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				rdb.Set(ctx, refreshTokenPrefix+bad.RefreshToken, payload, testConfig.RefreshTokenTTL)
			}
			before := familySnapshot(t, mr)
			if err := m.RevokeAll(ctx, testAccount); err == nil {
				t.Fatal("损坏候选必须拒绝撤销")
			}
			if !maps.Equal(before, familySnapshot(t, mr)) {
				t.Fatal("Lua 错误没有回滚，必须先核全部候选再写")
			}
		})
	}
}

func TestTokenFamily_ReplacedRawValueCannotRotate(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	pair := issueSeed(t, m)
	data := familyData(t, rdb, pair.RefreshToken)
	original, err := rdb.Get(ctx, refreshTokenPrefix+pair.RefreshToken).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	data.DeviceID = "concurrent-replacement"
	replacement, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	other := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer other.Close()
	if err := other.Set(ctx, refreshTokenPrefix+pair.RefreshToken, replacement, testConfig.RefreshTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	expected := familySnapshot(t, mr)
	if err := other.Set(ctx, refreshTokenPrefix+pair.RefreshToken, original, testConfig.RefreshTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	rdb.AddHook(&afterFirstCommand{do: func() {
		if err := other.Set(ctx, refreshTokenPrefix+pair.RefreshToken, replacement, testConfig.RefreshTokenTTL).Err(); err != nil {
			t.Fatal(err)
		}
	}})
	if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("head 未变也不能消费已被替换的原始值")
	}
	if !maps.Equal(expected, familySnapshot(t, mr)) {
		t.Fatal("旧 raw CAS 失败不得更新 family 或签出 token")
	}
}

// 仅具名本机 Redis 窗口执行；所有凭证与键均随机生成，清理仅删除本用例登记的原键，不 FLUSH/SCAN。
func TestTokenFamily_RealRedis(t *testing.T) {
	if os.Getenv("LOGIN_TOKEN_REQUIRE_REAL_REDIS") != "1" {
		t.Skip("待具名真实 Redis 验证窗口")
	}
	addr := os.Getenv("LOGIN_TOKEN_TEST_REDIS_ADDR")
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("只允许明确的数值 loopback Redis")
	}
	db, err := strconv.Atoi(os.Getenv("LOGIN_TOKEN_TEST_REDIS_DB"))
	if err != nil || db < 1 || db > 15 {
		t.Fatal("须显式指定本任务 Redis DB 1..15")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: db, Password: os.Getenv("LOGIN_TOKEN_TEST_REDIS_PASSWORD"), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal("本次 Redis 协议不可用", err)
	}
	suffix, err := generateOpaque(familyByteLen)
	if err != nil {
		t.Fatal(err)
	}
	account := "codex-token-family-it-" + suffix
	m := NewManager(rdb, Config{AccessTokenTTL: 2 * time.Minute, RefreshTokenTTL: 5 * time.Minute})
	keys := []string{accountRefreshKey + account}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		if err := rdb.Del(cleanup, keys...).Err(); err != nil {
			t.Error("本用例精确键清理失败", err)
		}
	})
	old, err := m.Issue(ctx, account, testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	data := familyData(t, rdb, old.RefreshToken)
	keys = append(keys, accessTokenPrefix+old.AccessToken, refreshTokenPrefix+old.RefreshToken, refreshFamilyPrefix+data.FamilyID)
	got, err := m.Refresh(ctx, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	keys = append(keys, accessTokenPrefix+got.AccessToken, refreshTokenPrefix+got.RefreshToken)
	if _, err := m.Refresh(ctx, old.RefreshToken); err == nil {
		t.Fatal("真实 Redis 不得二次消费旧 refresh")
	}
	if _, err := m.ValidateAccess(ctx, old.AccessToken); err != nil {
		t.Fatal("正常轮换不撤销同 family 旧 access", err)
	}
	if err := m.RevokeAll(ctx, account); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{old.AccessToken, got.AccessToken} {
		if _, err := m.ValidateAccess(ctx, token); err == nil {
			t.Fatal("真实 Redis family 撤销未生效")
		}
	}
	// miniredis 的 JSON 解码把 null 变成 nil，无法替代 Redis cjson 的 null 原件。
	// 这项必须在真实后端运行，且不打印随机键、令牌或 DUMP 内容。
	t.Run("null-retire-preserves-all-candidates", func(t *testing.T) {
		nullAccount := account + "-null"
		setKey := accountRefreshKey + nullAccount
		keys = append(keys, setKey)
		localKeys := []string{setKey}
		var bad *TokenPair
		for range 2 {
			pair, err := m.Issue(ctx, nullAccount, testAuthType, testDevice)
			if err != nil {
				t.Fatal(err)
			}
			data := familyData(t, rdb, pair.RefreshToken)
			owned := []string{accessTokenPrefix + pair.AccessToken, refreshTokenPrefix + pair.RefreshToken, refreshFamilyPrefix + data.FamilyID}
			keys = append(keys, owned...)
			localKeys = append(localKeys, owned...)
			bad = pair
		}
		data := familyData(t, rdb, bad.RefreshToken)
		payload, err := json.Marshal(map[string]any{"account": data.Account, "auth_type": data.AuthType, "device_id": data.DeviceID, "created_at": data.CreatedAt, "family_id": nil})
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.Set(ctx, refreshTokenPrefix+bad.RefreshToken, payload, m.cfg.RefreshTokenTTL).Err(); err != nil {
			t.Fatal(err)
		}
		if err := rdb.ZAdd(ctx, setKey, redis.Z{Score: float64(time.Now().Add(m.cfg.RefreshTokenTTL).Unix() + 100), Member: bad.RefreshToken}).Err(); err != nil {
			t.Fatal(err)
		}
		snapshot := func() map[string]string {
			values := map[string]string{}
			for _, key := range localKeys {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil {
					t.Fatal("读取本例精确状态失败", err)
				}
				values[key] = value
			}
			return values
		}
		before := snapshot()
		if err := m.RevokeAll(ctx, nullAccount); err == nil {
			t.Fatal("显式 null 不得作为旧 JSON 撤销")
		}
		if !maps.Equal(before, snapshot()) {
			t.Fatal("拒绝 null 候选必须保留全部原候选、索引和 family")
		}
	})
	for _, kind := range []string{"persistent-access", "persistent-head-refresh", "persistent-family"} {
		t.Run(kind, func(t *testing.T) {
			caseAccount := account + "-" + kind
			keys = append(keys, accountRefreshKey+caseAccount)
			pair, err := m.Issue(ctx, caseAccount, testAuthType, testDevice)
			if err != nil {
				t.Fatal(err)
			}
			data := familyData(t, rdb, pair.RefreshToken)
			owned := []string{accessTokenPrefix + pair.AccessToken, refreshTokenPrefix + pair.RefreshToken, refreshFamilyPrefix + data.FamilyID, accountRefreshKey + caseAccount}
			keys = append(keys, owned[:3]...)
			corrupted := owned[0]
			if kind == "persistent-head-refresh" {
				corrupted = owned[1]
			} else if kind == "persistent-family" {
				corrupted = owned[2]
			}
			if removed, err := rdb.Persist(ctx, corrupted).Result(); err != nil || !removed {
				t.Fatal("真实 Redis 须实际移除本例正 TTL", err)
			}
			before := map[string]string{}
			for _, key := range owned {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil {
					t.Fatal(err)
				}
				before[key] = value
			}
			if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
				t.Fatal("永久凭证不能通过真实认证")
			}
			if kind != "persistent-access" {
				if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
					t.Fatal("永久 refresh/family 不能签出后继")
				}
				if err := m.RevokeAll(ctx, caseAccount); err == nil {
					t.Fatal("永久撤销候选必须先拒绝且不写")
				}
			}
			for _, key := range owned {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil || value != before[key] {
					t.Fatal("真实拒绝不得改写本例任意原件", err)
				}
			}
		})
	}
}
