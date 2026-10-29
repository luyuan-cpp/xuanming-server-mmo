package logic

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/routing"
	"data_service/internal/svc"

	"shared/assetop"
	"shared/ownerepoch"
	"shared/redisslot"
	"shared/savebarrier"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── 字段索引、保留名、写门禁(data_service_role_and_scope.md「玩家路由 ClientForPlayer 的分布式审计」)──────────

// testCtx 是一名已登记映射(zone 1)的玩家与 dev 模式的服务上下文。
type testCtx struct {
	svc  *svc.ServiceContext
	mr   *miniredis.Miniredis
	pid  uint64
	zone uint32
}

func newTestCtx(t *testing.T, pid uint64) *testCtx {
	t.Helper()
	svcCtx, mr := newTestSvcCtx(t)
	setupPlayer(t, svcCtx, pid, 1)
	return &testCtx{svc: svcCtx, mr: mr, pid: pid, zone: 1}
}

// cmdRecorder 记下 dev 模式数据客户端上发出的命令名,用来断言「这一步没有 SCAN」。
type cmdRecorder struct{ names []string }

func (r *cmdRecorder) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (r *cmdRecorder) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func (r *cmdRecorder) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		r.names = append(r.names, cmd.Name())
		return next(ctx, cmd)
	}
}

func (r *cmdRecorder) reset() { r.names = nil }

func (r *cmdRecorder) count(name string) int {
	n := 0
	for _, got := range r.names {
		if got == name {
			n++
		}
	}
	return n
}

func installRecorder(t *testing.T, ctx *testCtx) *cmdRecorder {
	t.Helper()
	rec := &cmdRecorder{}
	installHook(t, ctx.svc, ctx.pid, rec)
	return rec
}

func loadAll(t *testing.T, tc *testCtx) *LoadPlayerDataResp {
	t.Helper()
	resp, err := LoadPlayerData(context.Background(), tc.svc, &LoadPlayerDataReq{PlayerID: tc.pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	return resp
}

func dataKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// 已索引的全量读:一段脚本读完,没有 SCAN、没有 MGET;返回的版本与数据同一瞬间,可直接 CAS。
func TestLoadAllFields_IndexedSingleScript(t *testing.T) {
	tc := newTestCtx(t, 9301)
	saved := mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("b"), "attr": []byte("a")})
	rec := installRecorder(t, tc)
	rec.reset()

	resp := loadAll(t, tc)
	assert.Zero(t, rec.count("scan"))
	assert.Zero(t, rec.count("mget"))
	assert.Equal(t, 1, rec.count("eval"))
	assert.Equal(t, map[string][]byte{"bag": []byte("b"), "attr": []byte("a")}, resp.Data)
	assert.Equal(t, saved, resp.Version)

	cas, err := SavePlayerData(context.Background(), tc.svc, &SavePlayerDataReq{
		PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("b2")}, ExpectedVersion: resp.Version,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, cas.ErrorCode)
}

// 没有任何数据的玩家:不 SCAN,直接回空、版本 0。
func TestLoadAllFields_NoDataDoesNotScan(t *testing.T) {
	tc := newTestCtx(t, 9302)
	rec := installRecorder(t, tc)
	rec.reset()

	resp := loadAll(t, tc)
	assert.Empty(t, resp.Data)
	assert.Zero(t, resp.Version)
	assert.Zero(t, rec.count("scan"))
}

// 存量老玩家(有字段与版本、没有索引)退回 SCAN:别的模块同前缀的 HASH 与保留名不进结果;读路径不写索引。
// owner_epoch 是同前缀的 string 键(玩家域集群化后带 tag),MGET 读得出值,只能靠保留后缀表挡住。
func TestLoadAllFields_LegacyPlayerScanFallback(t *testing.T) {
	tc := newTestCtx(t, 9303)
	makeLegacyPlayer(t, tc.mr, tc.pid, map[string]string{"bag": "b", "attr": "a"}, "5")
	tc.mr.HSet(savebarrier.Key(tc.pid), "pending", "x")
	tc.mr.HSet(assetop.SQLCommitKey(tc.pid), "version", "x")
	require.NoError(t, tc.mr.Set(ownerepoch.OwnerEpochKey(tc.pid), "7"))
	require.NoError(t, tc.mr.Set(playerField(tc.pid, "__old"), "z"))
	rec := installRecorder(t, tc)
	rec.reset()

	resp := loadAll(t, tc)
	assert.Equal(t, []string{"attr", "bag"}, dataKeys(resp.Data))
	assert.Equal(t, uint64(5), resp.Version)
	assert.Positive(t, rec.count("scan"))
	assert.False(t, tc.mr.Exists(fieldIndexKey(tc.pid)), "读路径不收养、不写索引")
}

// 滚动升级中的旧副本只 INCR、不维护戳:索引自动失效,全量读退回 SCAN,不漏读旧副本写的字段;
// 新代码下一次存盘把失效索引删掉。
func TestLoadAllFields_StaleStampFromOldWriter(t *testing.T) {
	tc := newTestCtx(t, 9304)
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("b")})
	require.NoError(t, tc.mr.Set(playerField(tc.pid, "mail"), "m"))
	_, err := tc.mr.Incr(versionKey(tc.pid), 1)
	require.NoError(t, err)

	resp := loadAll(t, tc)
	assert.Equal(t, []string{"bag", "mail"}, dataKeys(resp.Data))

	mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("b2")})
	assert.False(t, tc.mr.Exists(fieldIndexKey(tc.pid)), "戳失效时存盘必须删掉索引")
	resp = loadAll(t, tc)
	assert.Equal(t, []string{"bag", "mail"}, dataKeys(resp.Data))
	assert.Equal(t, []byte("b2"), resp.Data["bag"])
}

// 带外写者把版本重置成更小值、又没删索引之后,旧戳可能与之后某个版本巧合相等。这里旧戳 "2" ≠ 重置后的 "1",
// saveFieldsScript「不维护就删索引」保证下一次存盘之后带外写进来的字段不会被藏掉。重置的当下戳就等于新版本的情形
// 它兜不住,所以带外写者必须自己删索引(cmd/debug_import 的 writeLegacyPlayerFields,有自己的回归测试)。
func TestSave_StaleIndexDeletedSoStampCannotCoincide(t *testing.T) {
	tc := newTestCtx(t, 9305)
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"a": []byte("1")})
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"a": []byte("2")}) // 版本 2,戳 "2"
	require.NoError(t, tc.mr.Set(playerField(tc.pid, "x"), "out-of-band"))
	require.NoError(t, tc.mr.Set(versionKey(tc.pid), "1"))

	ver := mustSave(t, tc.svc, tc.pid, map[string][]byte{"y": []byte("new")})
	require.Equal(t, uint64(2), ver, "版本又回到 2:不删索引的话,旧戳 \"2\" 会被当成可信")
	resp := loadAll(t, tc)
	assert.Equal(t, []string{"a", "x", "y"}, dataKeys(resp.Data))
}

// 版本或索引键损坏:读写都 fail-closed(ErrCodeRedis),且写入零副作用(所有可失败检查都在第一条 SET 之前)。
func TestPlayerFields_CorruptKeysFailClosed(t *testing.T) {
	ctx := context.Background()

	tc := newTestCtx(t, 9306)
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("v1")})
	tc.mr.Del(fieldIndexKey(tc.pid))
	require.NoError(t, tc.mr.Set(fieldIndexKey(tc.pid), "not-a-hash"))
	resp, err := LoadPlayerData(ctx, tc.svc, &LoadPlayerDataReq{PlayerID: tc.pid})
	assert.Error(t, err)
	assert.Equal(t, constants.ErrCodeRedis, resp.ErrorCode)
	saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("v2")}})
	assert.Error(t, err)
	assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode)
	bag, _ := tc.mr.Get(playerField(tc.pid, "bag"))
	assert.Equal(t, "v1", bag)
	ver, _ := tc.mr.Get(versionKey(tc.pid))
	assert.Equal(t, "1", ver)

	// "007" / "00":真 Redis 的 INCR 拒绝前导零串(miniredis 接受),守卫必须在第一条 SET 之前就拦下。
	for i, badVersion := range []string{"abc", "1234567890123456789", "007", "00"} {
		tc := newTestCtx(t, uint64(9307+i))
		require.NoError(t, tc.mr.Set(versionKey(tc.pid), badVersion))
		saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("v")}})
		assert.Error(t, err, "version=%q", badVersion)
		assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode, "version=%q", badVersion)
		assert.False(t, tc.mr.Exists(playerField(tc.pid, "bag")), "version=%q: 先 SET 后 INCR 失败会留下半截写", badVersion)
		got, _ := tc.mr.Get(versionKey(tc.pid))
		assert.Equal(t, badVersion, got)
	}
}

// 版本位数上限(≤15 位)。版本经 tonumber 与 INCR 进入 Lua 都是 double,只在 2^53 以内精确:上限内的最大值照常存盘,
// 新版本与戳都精确;16 位起按损坏拒写、零副作用。2^53+1 那一组在旧守卫(≤18 位)下是红的:它与 ExpectedVersion=2^53
// 经 tonumber 都舍入成 2^53,CAS 误放行并写入。gopher-lua 的 tonumber 同样落到 float64,miniredis 上复现得出来。
func TestSave_VersionPrecisionCap(t *testing.T) {
	ctx := context.Background()
	const maxVersion uint64 = 999999999999999 // 15 位最大值
	maxStr := strconv.FormatUint(maxVersion, 10)
	nextStr := strconv.FormatUint(maxVersion+1, 10) // 10^15,16 位

	// 上界:已索引玩家,当前版本是 15 位最大值。INCR 后的 10^15 仍在 2^53 以内,NewVersion 精确;戳取 GET 出来的串,
	// 不是 Lua 数字转串("1e+15")。
	tc := newTestCtx(t, 9320)
	require.NoError(t, tc.mr.Set(playerField(tc.pid, "bag"), "b0"))
	require.NoError(t, tc.mr.Set(versionKey(tc.pid), maxStr))
	tc.mr.HSet(fieldIndexKey(tc.pid), "bag", "", fieldIndexStampField, maxStr)
	saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{
		PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("b1")}, ExpectedVersion: maxVersion,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, saved.ErrorCode)
	assert.Equal(t, maxVersion+1, saved.NewVersion)
	ver, _ := tc.mr.Get(versionKey(tc.pid))
	assert.Equal(t, nextStr, ver)
	assert.Equal(t, nextStr, tc.mr.HGet(fieldIndexKey(tc.pid), fieldIndexStampField))

	// 再存一次:当前版本已是 16 位,按损坏拒写,字段、版本、戳都不动。
	saved, err = SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{
		PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("b2")}, ExpectedVersion: maxVersion + 1,
	})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode)
	assert.Contains(t, err.Error(), "version value malformed")
	bag, _ := tc.mr.Get(playerField(tc.pid, "bag"))
	assert.Equal(t, "b1", bag)
	ver, _ = tc.mr.Get(versionKey(tc.pid))
	assert.Equal(t, nextStr, ver)
	assert.Equal(t, nextStr, tc.mr.HGet(fieldIndexKey(tc.pid), fieldIndexStampField))

	// 2^53+1 带过期一格的 ExpectedVersion=2^53:必须在比较之前按位数拒绝,不能让舍入后的「相等」放行。
	tc = newTestCtx(t, 9321)
	makeLegacyPlayer(t, tc.mr, tc.pid, map[string]string{"bag": "b0"}, "9007199254740993")
	saved, err = SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{
		PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("stale")}, ExpectedVersion: 9007199254740992,
	})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode)
	assert.Contains(t, err.Error(), "version value malformed")
	bag, _ = tc.mr.Get(playerField(tc.pid, "bag"))
	assert.Equal(t, "b0", bag, "过期的 ExpectedVersion 不能因舍入相等而写入")
	ver, _ = tc.mr.Get(versionKey(tc.pid))
	assert.Equal(t, "9007199254740993", ver)
}

// 保留名(内部键前缀 __、shared/redisslot 登记的别家后缀,如 save_barrier / asset_sql_commit / owner_epoch):
// 四个入口都拒绝,别家的键原样不动。
func TestReservedNamesRejected(t *testing.T) {
	ctx := context.Background()
	tc := newTestCtx(t, 9310)
	tc.mr.HSet(savebarrier.Key(tc.pid), "pending", "keep")
	require.NoError(t, tc.mr.Set(ownerepoch.OwnerEpochKey(tc.pid), "7"))

	names := append([]string{"__fields", "__foo"}, redisslot.ReservedPlayerKeySuffixes()...)
	for _, name := range names {
		saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{PlayerID: tc.pid, Data: map[string][]byte{name: []byte("x")}})
		require.NoError(t, err, name)
		assert.Equal(t, constants.ErrCodeInvalidRequest, saved.ErrorCode, name)
		set, err := SetPlayerField(ctx, tc.svc, tc.pid, name, []byte("x"), 0)
		require.NoError(t, err, name)
		assert.Equal(t, constants.ErrCodeInvalidRequest, set.ErrorCode, name)
		loaded, err := LoadPlayerData(ctx, tc.svc, &LoadPlayerDataReq{PlayerID: tc.pid, Fields: []string{name}})
		require.NoError(t, err, name)
		assert.Equal(t, constants.ErrCodeInvalidRequest, loaded.ErrorCode, name)
		_, err = GetPlayerField(ctx, tc.svc, tc.pid, name)
		assert.Error(t, err, name)
	}
	assert.Equal(t, "hash", tc.mr.Type(savebarrier.Key(tc.pid)))
	assert.Equal(t, "keep", tc.mr.HGet(savebarrier.Key(tc.pid), "pending"))
	owner, err := tc.mr.Get(ownerepoch.OwnerEpochKey(tc.pid))
	require.NoError(t, err)
	assert.Equal(t, "7", owner)
	assert.False(t, tc.mr.Exists(versionKey(tc.pid)))
}

// 未登记的同前缀 HASH:写入零副作用拒绝(InvalidRequest),整批一个字段都不写。
func TestSave_TypeConflictZeroSideEffect(t *testing.T) {
	ctx := context.Background()
	tc := newTestCtx(t, 9311)
	tc.mr.HSet(playerField(tc.pid, "other"), "f", "v")

	saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{
		PlayerID: tc.pid, Data: map[string][]byte{"other": []byte("x"), "bag": []byte("b")},
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, saved.ErrorCode)
	assert.False(t, tc.mr.Exists(playerField(tc.pid, "bag")))
	assert.False(t, tc.mr.Exists(versionKey(tc.pid)))
	assert.False(t, tc.mr.Exists(fieldIndexKey(tc.pid)))
	assert.Equal(t, "hash", tc.mr.Type(playerField(tc.pid, "other")))

	set, err := SetPlayerField(ctx, tc.svc, tc.pid, "other", []byte("x"), 0)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, set.ErrorCode)
}

// 存量老玩家存盘不建「不完整的索引」:否则老字段会从全量读里永久消失。
func TestSave_LegacyPlayerNeverGetsPartialIndex(t *testing.T) {
	tc := newTestCtx(t, 9312)
	makeLegacyPlayer(t, tc.mr, tc.pid, map[string]string{"bag": "b"}, "5")

	ver := mustSave(t, tc.svc, tc.pid, map[string][]byte{"mail": []byte("m")})
	assert.Equal(t, uint64(6), ver)
	assert.False(t, tc.mr.Exists(fieldIndexKey(tc.pid)))
	resp := loadAll(t, tc)
	assert.Equal(t, []string{"bag", "mail"}, dataKeys(resp.Data))
	assert.Equal(t, uint64(6), resp.Version)
}

// 新玩家存一次空数据:索引只有戳;全量读回空、版本 1,不 SCAN。
func TestSave_EmptyDataStampsIndex(t *testing.T) {
	tc := newTestCtx(t, 9313)
	ver := mustSave(t, tc.svc, tc.pid, map[string][]byte{})
	require.Equal(t, uint64(1), ver)
	members, err := tc.mr.HKeys(fieldIndexKey(tc.pid))
	require.NoError(t, err)
	assert.Equal(t, []string{fieldIndexStampField}, members)
	assert.Equal(t, "1", tc.mr.HGet(fieldIndexKey(tc.pid), fieldIndexStampField))

	rec := installRecorder(t, tc)
	rec.reset()
	resp := loadAll(t, tc)
	assert.Empty(t, resp.Data)
	assert.Equal(t, uint64(1), resp.Version)
	assert.Zero(t, rec.count("scan"))
}

// 已索引的删除:只删自有 string 键(字段 + 版本)与索引,不删别的模块的 HASH,不 SCAN;KeysDeleted 是实际删除数。
func TestDelete_IndexedDeletesOnlyOwnKeys(t *testing.T) {
	ctx := context.Background()
	tc := newTestCtx(t, 9314)
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("b"), "attr": []byte("a")})
	tc.mr.HSet(savebarrier.Key(tc.pid), "pending", "keep")
	tc.mr.HSet(assetop.SQLCommitKey(tc.pid), "data", "keep")
	rec := installRecorder(t, tc)
	rec.reset()

	del, err := DeletePlayerData(ctx, tc.svc, &DeletePlayerDataReq{PlayerID: tc.pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
	assert.Equal(t, uint32(3), del.KeysDeleted)
	assert.Zero(t, rec.count("scan"))
	for _, key := range []string{playerField(tc.pid, "bag"), playerField(tc.pid, "attr"), versionKey(tc.pid), fieldIndexKey(tc.pid)} {
		assert.False(t, tc.mr.Exists(key), key)
	}
	assert.Equal(t, "hash", tc.mr.Type(savebarrier.Key(tc.pid)))
	assert.Equal(t, "hash", tc.mr.Type(assetop.SQLCommitKey(tc.pid)))

	rec.reset()
	resp := loadAll(t, tc)
	assert.Empty(t, resp.Data)
	assert.Zero(t, resp.Version)
	assert.Zero(t, rec.count("scan"))
	ver := mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("again")})
	assert.Equal(t, uint64(1), ver)
	assert.Equal(t, "1", tc.mr.HGet(fieldIndexKey(tc.pid), fieldIndexStampField))
}

// 存量老玩家的删除退回 SCAN:只删字段 string 键与版本,别的模块的 HASH 与保留名(如 __old)保守不删。
func TestDelete_LegacyFallbackSkipsForeignAndReserved(t *testing.T) {
	ctx := context.Background()
	tc := newTestCtx(t, 9315)
	makeLegacyPlayer(t, tc.mr, tc.pid, map[string]string{"bag": "b", "attr": "a"}, "5")
	tc.mr.HSet(savebarrier.Key(tc.pid), "pending", "keep")
	tc.mr.HSet(assetop.SQLCommitKey(tc.pid), "data", "keep")
	require.NoError(t, tc.mr.Set(ownerepoch.OwnerEpochKey(tc.pid), "7"))
	require.NoError(t, tc.mr.Set(playerField(tc.pid, "__old"), "z"))
	rec := installRecorder(t, tc)
	rec.reset()

	del, err := DeletePlayerData(ctx, tc.svc, &DeletePlayerDataReq{PlayerID: tc.pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
	assert.Equal(t, uint32(3), del.KeysDeleted)
	assert.Positive(t, rec.count("scan"))
	assert.False(t, tc.mr.Exists(playerField(tc.pid, "bag")))
	assert.False(t, tc.mr.Exists(playerField(tc.pid, "attr")))
	assert.False(t, tc.mr.Exists(versionKey(tc.pid)))
	assert.True(t, tc.mr.Exists(savebarrier.Key(tc.pid)))
	assert.True(t, tc.mr.Exists(assetop.SQLCommitKey(tc.pid)))
	assert.True(t, tc.mr.Exists(ownerepoch.OwnerEpochKey(tc.pid)), "owner_epoch 是 string 键,只靠保留后缀表免删")
	assert.True(t, tc.mr.Exists(playerField(tc.pid, "__old")))
}

// 字段索引键名是跨模块契约:tools/merge_zone 是独立 module,引不到本包,只能在 player_blob_migrate.go::playerFieldIndexKey
// 镜像一份字面量,由那边的 TestPlayerFieldIndexKeyMatchesDataService 钉住。两边钉同一个字面量,改这里必须同批改那边,
// 否则合服拷贝 DEL 的是旧键,目标端残留索引可能把拷过去的字段从全量读里藏掉。
func TestFieldIndexKeyLiteralPinnedForMergeZone(t *testing.T) {
	assert.Equal(t, "player:{42}:__fields", fieldIndexKey(42))
	assert.Equal(t, "player:{42}:", playerKeyPrefix(42))
}

// 非规范十进制的版本(前导零、空串)读路径同样 fail-closed,与写侧守卫同一口径。
// 版本键不存在走 parseVersionReply 的 nil 分支,不经过这里。
func TestParseVersionValueRejectsNonCanonical(t *testing.T) {
	for _, val := range []string{"", "007", "00", "+1", " 1"} {
		_, err := parseVersionValue(1, val)
		assert.Error(t, err, "val=%q", val)
	}
	for val, want := range map[string]uint64{"0": 0, "7": 7, "18446744073709551615": 18446744073709551615} {
		got, err := parseVersionValue(1, val)
		require.NoError(t, err, "val=%q", val)
		assert.Equal(t, want, got, "val=%q", val)
	}
}

func TestDecodeLoadAllReply(t *testing.T) {
	const pid = 1
	ok := []struct {
		name    string
		reply   interface{}
		kind    int64
		version uint64
		data    map[string][]byte
	}{
		{"no data", []interface{}{int64(0)}, loadAllNoData, 0, map[string][]byte{}},
		{"indexed with a missing value", []interface{}{int64(1), "3", "a", "x", "b", nil}, loadAllIndexed, 3, map[string][]byte{"a": []byte("x")}},
		{"unindexed without version", []interface{}{int64(2), nil}, loadAllUnindexed, 0, nil},
		{"unindexed with version", []interface{}{int64(2), "7"}, loadAllUnindexed, 7, nil},
	}
	for _, c := range ok {
		got, err := decodeLoadAllReply(pid, c.reply)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.kind, got.kind, c.name)
		assert.Equal(t, c.version, got.version, c.name)
		if c.data != nil {
			assert.Equal(t, c.data, got.data, c.name)
		}
	}

	bad := map[string]interface{}{
		"not an array":         "oops",
		"empty array":          []interface{}{},
		"kind not int":         []interface{}{"1"},
		"unknown kind":         []interface{}{int64(9)},
		"no-data with extras":  []interface{}{int64(0), "1"},
		"indexed odd tail":     []interface{}{int64(1), "3", "a"},
		"indexed nil version":  []interface{}{int64(1), nil},
		"indexed name not str": []interface{}{int64(1), "3", int64(5), "x"},
		"corrupt version":      []interface{}{int64(1), "abc"},
		"unindexed too long":   []interface{}{int64(2), "1", "2"},
	}
	for name, reply := range bad {
		_, err := decodeLoadAllReply(pid, reply)
		assert.Error(t, err, name)
	}
}

// ── 写门禁:合服窗口内写入零副作用拒绝(22),读不拦 ──────────────────────────────

func requireNoWriteLocks(t *testing.T, tc *testCtx) {
	t.Helper()
	// "lock:player:<id>" 是 routing 包未导出的 mapping 锁键(router.go mappingPlayerLockKey)。
	assert.False(t, tc.mr.Exists("lock:player:"+strconv.FormatUint(tc.pid, 10)))
	assert.False(t, tc.mr.Exists(routing.PlayerDataLockKey(tc.pid)))
}

func TestWrites_RefusedInsideMergeFence(t *testing.T) {
	ctx := context.Background()
	tc := newTestCtx(t, 9320)
	mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("v1")})
	require.NoError(t, tc.mr.Set(routing.MergeFenceKey(tc.zone), "{}"))

	saved, err := SavePlayerData(ctx, tc.svc, &SavePlayerDataReq{PlayerID: tc.pid, Data: map[string][]byte{"bag": []byte("v2")}})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeZoneMergeInProgress, saved.ErrorCode)
	set, err := SetPlayerField(ctx, tc.svc, tc.pid, "bag", []byte("v2"), 0)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeZoneMergeInProgress, set.ErrorCode)
	del, err := DeletePlayerData(ctx, tc.svc, &DeletePlayerDataReq{PlayerID: tc.pid})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeZoneMergeInProgress, del.ErrorCode)
	assert.Zero(t, del.KeysDeleted)

	bag, _ := tc.mr.Get(playerField(tc.pid, "bag"))
	assert.Equal(t, "v1", bag)
	ver, _ := tc.mr.Get(versionKey(tc.pid))
	assert.Equal(t, "1", ver)
	requireNoWriteLocks(t, tc)

	// 读不拦:写门禁保证拷贝之后源端冻结,换路由前后读到的是同一份数据。
	loaded, err := LoadPlayerData(ctx, tc.svc, &LoadPlayerDataReq{PlayerID: tc.pid})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, loaded.ErrorCode)
	assert.Equal(t, []byte("v1"), loaded.Data["bag"])
	field, err := GetPlayerField(ctx, tc.svc, tc.pid, "bag")
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), field)

	// 门禁不粘滞:撤掉围栏立即可写。
	tc.mr.Del(routing.MergeFenceKey(tc.zone))
	assert.Equal(t, uint64(2), mustSave(t, tc.svc, tc.pid, map[string][]byte{"bag": []byte("v2")}))
}

// dev 模式下没有映射的玩家照常可写:数据位置不依赖映射,与引入写门禁之前一致。
func TestWrites_UnmappedPlayerDevModeStillWrites(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	saved, err := SavePlayerData(context.Background(), svcCtx, &SavePlayerDataReq{PlayerID: 9321, Data: map[string][]byte{"bag": []byte("b")}})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeOK, saved.ErrorCode)
}
