//go:build integration

package logic

// 集成测试:玩家字段键族 player:{id}:<field> 的三段 Lua(saveFieldsScript / loadAllFieldsScript /
// deletePlayerFieldsScript)打**真 Redis**。
//
// 版本要跑两轮:K8s 清单(deploy/k8s/manifests/infra/redis*.yaml)钉 redis:7.2;本地 deploy/docker-compose.yml 的主 Redis
// 是 redis:latest(2026-10-19 本机镜像为 8.10.2),Java 版也钉 redis:8.10.2。用例本身不判版本,只在 -v 输出里记
// redis_version 作为证据,两轮都要全 PASS 才算覆盖。
//
// 为什么 miniredis 单测不够:miniredis 的 Lua 是 gopher-lua 仿真,下面三处与真 Redis 未必一致,而三段脚本恰好依赖它们:
//   - TYPE 的状态回复在 Lua 里是不是 {ok = '<type>'} 表 —— 脚本全靠 .ok 判类型;
//   - GET 缺失时 Lua 里是 false(占位)还是 nil(不占位,后面的名字 / 值错位),以及 false 回到客户端变成什么。
//     go-redis v9 缺省 RESP3;脚本内缺省 RESP2,Redis 按脚本侧协议把 false 转成 null,decodeLoadAllReply 只认 nil / string;
//   - INCR 对 "007" 这类前导零串:真 Redis 拒绝,miniredis 接受。守卫若晚于 SET,就会留下「字段已写、版本没涨」的半截写。
//
// 跑法(一次性容器,不借别的会话的容器;$img 先取 redis:7.2,再取 redis:8.10.2 各跑一轮):
//
//	docker run --rm -d -p 6390:6379 --name ds-it-fields $img --appendonly yes
//	docker run --rm -d -p 7001-7003:7001-7003 --name ds-it-cluster $img sh -c '<见 TestRealRedisClusterUndeclaredKeys>'
//	(集群用例自己等每个种子节点 cluster_state:ok,最多 30 秒;超时先看 docker logs ds-it-cluster)
//	$env:DATA_SERVICE_IT_REDIS_HOST='127.0.0.1:6390'
//	$env:DATA_SERVICE_IT_REDIS_CLUSTER_ADDRS='127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003'
//	go test -tags=integration ./internal/logic/ -run RealRedis -count=1 -v
//	docker stop ds-it-fields ds-it-cluster
//
// 单实例用例只读 DATA_SERVICE_IT_REDIS_HOST,Cluster 用例只读 DATA_SERVICE_IT_REDIS_CLUSTER_ADDRS:未设置就 Skip,
// **不回落**到 etc/data_service.yaml 的开发库;设置了却连不上(或集群没就绪)直接失败。
// 验收时加 $env:DATA_SERVICE_IT_REQUIRE='1':任何 SKIP 都判 FAIL(requireRealRedisIT)。
// 玩家 id 从 9900000000001 起,每个用例各占几个、互不重叠。跑前 SCAN player:{id}:* 非空或 lock:player:<id> 已存在就 Skip
// (不是本用例建的键,一个都不碰);跑后只删这些 id 的 player:{id}:* 与 lock:player:<id>,绝不 FLUSHDB。
// DevRedis 与 MappingRedis 都指向这台 Redis(同 newTestSvcCtx),两边都在 DB 0;玩家不登记映射,dev 模式照常可写,
// 不产生 player:zone:<id>。

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/routing"
	"data_service/internal/svc"

	"shared/assetop"
	"shared/savebarrier"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	envRealRedisHost = "DATA_SERVICE_IT_REDIS_HOST"
	// envRealRedisClusterAddrs 是逗号分隔的 Cluster 种子地址,只给 TestRealRedisClusterUndeclaredKeys 用。
	envRealRedisClusterAddrs = "DATA_SERVICE_IT_REDIS_CLUSTER_ADDRS"
	// envRealRedisRequire 设为 1 时,被跳过的用例判失败(与本模块其他集成测试同一个开关)。
	envRealRedisRequire = "DATA_SERVICE_IT_REQUIRE"
	// realRedisPlayerBase 往上依次分给各用例(见每个用例开头),新增用例接着往后排。
	realRedisPlayerBase uint64 = 9900000000001
	// realRedisLockToken 是直接调脚本时手工放进数据锁的 token:绕过 Router 拿锁,只为看脚本的原始返回值。
	realRedisLockToken = "it-real-redis-token"
)

// realRedisCtx 是接真 Redis 的 dev 模式服务上下文;raw 是同一台 Redis 的直连客户端,只用来造数据与断言。
type realRedisCtx struct {
	host string
	svc  *svc.ServiceContext
	raw  *goredis.Client
}

// requireRealRedisIT 必须是每个 TestRealRedis* 的第一句:它登记的清理最后一个执行,用例被跳过(环境变量没设、
// 键空间不干净)且设了 DATA_SERVICE_IT_REQUIRE=1 就判失败,杜绝把 SKIP 当成通过。与 routereconcile 的
// requireCacheIT、routing 的 requireRouteReadIT 同一写法:跳过的判定只在各自的 setup 一处,这里不再探测一遍。
func requireRealRedisIT(t *testing.T) {
	t.Helper()
	if os.Getenv(envRealRedisRequire) != "1" {
		return
	}
	t.Cleanup(func() {
		if t.Skipped() {
			t.Errorf("%s=1: this integration test was skipped (a dependency is missing or unusable, see the SKIP reason above); treated as FAIL",
				envRealRedisRequire)
		}
	})
}

// realRedisMappingLockKey 与 routing 包未导出的 mappingPlayerLockKey(router.go)同一拼法。
func realRedisMappingLockKey(pid uint64) string {
	return "lock:player:" + strconv.FormatUint(pid, 10)
}

func newRealRedisCtx(t *testing.T, pids ...uint64) *realRedisCtx {
	t.Helper()
	host := os.Getenv(envRealRedisHost)
	if host == "" {
		t.Skipf("%s 未设置:本用例只打显式指定的一次性 Redis,不回落到 yaml 里的开发库", envRealRedisHost)
	}
	raw := goredis.NewClient(&goredis.Options{Addr: host})
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()
	require.NoError(t, raw.Ping(ctx).Err(), "%s=%s 连不上:设置了就必须可达,不 Skip", envRealRedisHost, host)
	if info, err := raw.Info(ctx, "server").Result(); err == nil {
		t.Logf("real redis %s: %s", host, redisVersionLine(info))
	}

	rc := &realRedisCtx{host: host, raw: raw}
	for _, pid := range pids {
		keys, err := rc.scanKeys(pid)
		require.NoError(t, err)
		if len(keys) > 0 {
			t.Skipf("玩家 %d 已有 %d 个键(如 %s):不是本用例建的,不碰", pid, len(keys), keys[0])
		}
		if rc.exists(t, realRedisMappingLockKey(pid)) {
			t.Skipf("%s 已存在:不是本用例建的,不碰", realRedisMappingLockKey(pid))
		}
	}
	// 前置检查全部通过之后才登记清理,否则会删掉别人的键。清理里不用 require:一个 id 失败不影响其余 id。
	t.Cleanup(func() {
		for _, pid := range pids {
			keys, err := rc.scanKeys(pid)
			if err != nil {
				t.Errorf("cleanup player %d: scan: %v", pid, err)
				continue
			}
			keys = append(keys, realRedisMappingLockKey(pid))
			if err := raw.Del(context.Background(), keys...).Err(); err != nil {
				t.Errorf("cleanup player %d: del: %v", pid, err)
			}
		}
	})

	c := config.Config{
		MappingRedis:     redis.RedisConf{Host: host, Type: "node"},
		DevRedis:         config.DevRedisConfig{Host: host, DB: 0},
		PlayerLockTTLSec: 3,
	}
	r := routing.NewRouter(c)
	t.Cleanup(r.Close)
	rc.svc = &svc.ServiceContext{Config: c, Router: r}
	return rc
}

func redisVersionLine(info string) string {
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "redis_version:") {
			return strings.TrimSpace(line)
		}
	}
	return "redis_version unknown"
}

func (rc *realRedisCtx) scanKeys(pid uint64) ([]string, error) {
	return scanRealRedisPlayerKeys(rc.raw, pid)
}

// scanRealRedisPlayerKeys 枚举 player:{pid}:*(花括号在 SCAN 的 glob 里是字面量),排序去重。
// 用例自己的前置检查 / 清理 / 快照用它,不借被测的 scanPlayerKeys。Cluster 上 go-redis 按 pattern 的 hash tag 只发往该 slot 的主节点。
func scanRealRedisPlayerKeys(c goredis.Cmdable, pid uint64) ([]string, error) {
	seen := make(map[string]struct{})
	var cursor uint64
	for {
		batch, next, err := c.Scan(context.Background(), cursor, playerKeyPrefix(pid)+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range batch {
			seen[key] = struct{}{}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// keyTypes 是 player:{pid}:* 下每个键的类型快照,配合 diffKeyTypes 断言「删了哪些、留了哪些」。
func (rc *realRedisCtx) keyTypes(t *testing.T, pid uint64) map[string]string {
	t.Helper()
	keys, err := rc.scanKeys(pid)
	require.NoError(t, err)
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = rc.typeOf(t, key)
	}
	return out
}

// diffKeyTypes 返回 before 有、after 没有的键(排序),其中原本是 string 的个数,以及新出现或类型变了的键。
func diffKeyTypes(before, after map[string]string) (gone []string, goneStrings int, changed []string) {
	for key, typ := range before {
		if _, ok := after[key]; !ok {
			gone = append(gone, key)
			if typ == "string" {
				goneStrings++
			}
		}
	}
	for key, typ := range after {
		if before[key] != typ {
			changed = append(changed, key)
		}
	}
	sort.Strings(gone)
	sort.Strings(changed)
	return gone, goneStrings, changed
}

func (rc *realRedisCtx) get(t *testing.T, key string) (string, bool) {
	t.Helper()
	val, err := rc.raw.Get(context.Background(), key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false
	}
	require.NoError(t, err, key)
	return val, true
}

// requireValue 断言 key 是 string 且值为 want。
func (rc *realRedisCtx) requireValue(t *testing.T, key, want string) {
	t.Helper()
	got, ok := rc.get(t, key)
	require.True(t, ok, "%s 不存在", key)
	assert.Equal(t, want, got, key)
}

func (rc *realRedisCtx) hget(t *testing.T, key, field string) (string, bool) {
	t.Helper()
	val, err := rc.raw.HGet(context.Background(), key, field).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false
	}
	require.NoError(t, err, key)
	return val, true
}

func (rc *realRedisCtx) typeOf(t *testing.T, key string) string {
	t.Helper()
	typ, err := rc.raw.Type(context.Background(), key).Result()
	require.NoError(t, err, key)
	return typ
}

func (rc *realRedisCtx) exists(t *testing.T, key string) bool {
	t.Helper()
	n, err := rc.raw.Exists(context.Background(), key).Result()
	require.NoError(t, err, key)
	return n == 1
}

func (rc *realRedisCtx) set(t *testing.T, key, val string) {
	t.Helper()
	require.NoError(t, rc.raw.Set(context.Background(), key, val, 0).Err(), key)
}

func (rc *realRedisCtx) hset(t *testing.T, key, field, val string) {
	t.Helper()
	require.NoError(t, rc.raw.HSet(context.Background(), key, field, val).Err(), key)
}

func (rc *realRedisCtx) del(t *testing.T, keys ...string) {
	t.Helper()
	require.NoError(t, rc.raw.Del(context.Background(), keys...).Err())
}

// indexMembers 是字段索引的全部成员(含戳 __version),排序。
func (rc *realRedisCtx) indexMembers(t *testing.T, pid uint64) []string {
	t.Helper()
	members, err := rc.raw.HKeys(context.Background(), fieldIndexKey(pid)).Result()
	require.NoError(t, err)
	sort.Strings(members)
	return members
}

// makeLegacyPlayer 造一名存量老玩家:裸写字段键与 __version,没有字段索引(同 data_logic_test.go 的 miniredis 版)。
func (rc *realRedisCtx) makeLegacyPlayer(t *testing.T, pid uint64, fields map[string]string, version string) {
	t.Helper()
	for name, val := range fields {
		rc.set(t, playerField(pid, name), val)
	}
	rc.set(t, versionKey(pid), version)
}

func (rc *realRedisCtx) loadAll(t *testing.T, pid uint64) *LoadPlayerDataResp {
	t.Helper()
	resp, err := LoadPlayerData(context.Background(), rc.svc, &LoadPlayerDataReq{PlayerID: pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, resp.ErrorCode)
	return resp
}

// requireStampMatchesVersion:HGET(__fields, '__version') 与 GET(__version) 逐字节相等,且等于 want。
func (rc *realRedisCtx) requireStampMatchesVersion(t *testing.T, pid uint64, want string) {
	t.Helper()
	ver, ok := rc.get(t, versionKey(pid))
	require.True(t, ok, "版本键不存在")
	stamp, ok := rc.hget(t, fieldIndexKey(pid), fieldIndexStampField)
	require.True(t, ok, "索引没有戳")
	assert.Equal(t, want, ver)
	assert.Equal(t, []byte(ver), []byte(stamp), "戳必须与 GET(__version) 逐字节相等")
}

// requireAllThreeRefuse:读、写、删三个入口都回 ErrCodeRedis,错误里带 want(脚本 error_reply 的原文)。
func requireAllThreeRefuse(t *testing.T, rc *realRedisCtx, pid uint64, want string) {
	t.Helper()
	ctx := context.Background()
	loaded, err := LoadPlayerData(ctx, rc.svc, &LoadPlayerDataReq{PlayerID: pid})
	require.Error(t, err, "load")
	assert.Equal(t, constants.ErrCodeRedis, loaded.ErrorCode, "load")
	assert.Contains(t, err.Error(), want, "load")

	saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{PlayerID: pid, Data: map[string][]byte{"bag": []byte("refused")}})
	require.Error(t, err, "save")
	assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode, "save")
	assert.Contains(t, err.Error(), want, "save")

	del, err := DeletePlayerData(ctx, rc.svc, &DeletePlayerDataReq{PlayerID: pid})
	require.Error(t, err, "delete")
	assert.Equal(t, constants.ErrCodeRedis, del.ErrorCode, "delete")
	assert.Contains(t, err.Error(), want, "delete")
}

func bytesMap(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

// 1. TYPE 的 .ok。三段脚本都靠 redis.call('TYPE', k).ok 判类型:真 Redis 把状态回复转成 Lua 表 {ok = '<type>'};
// 若拿到的是裸字符串,.ok 恒为 nil,类型判断全部失灵。先用探针直接看取值,再看三段脚本的类型分支把类型名拼进
// error_reply —— 消息里出现 'string' / 'hash' 说明 .ok 取到了值(对 nil 做 .. 拼接是 Lua 运行时错,消息不同)。
func TestRealRedisTypeReplyOkField(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pid := realRedisPlayerBase
	rc := newRealRedisCtx(t, pid)
	mustSave(t, rc.svc, pid, map[string][]byte{"bag": []byte("v1")})
	rc.hset(t, savebarrier.Key(pid), "pending", "keep")

	const probe = `return redis.call('TYPE', KEYS[1]).ok`
	for key, want := range map[string]string{
		fieldIndexKey(pid):         "hash",   // 字段索引
		versionKey(pid):            "string", // 版本
		playerField(pid, "bag"):    "string", // 字段
		savebarrier.Key(pid):       "hash",   // 别家 HASH(C++ 存盘围栏)
		playerField(pid, "absent"): "none",
	} {
		got, err := rc.raw.Eval(ctx, probe, []string{key}).Result()
		require.NoError(t, err, key)
		assert.Equal(t, want, got, key)
	}

	// 索引被换成 string:三段脚本都在任何写之前 error_reply。
	rc.del(t, fieldIndexKey(pid))
	rc.set(t, fieldIndexKey(pid), "not-a-hash")
	requireAllThreeRefuse(t, rc, pid, "field index wrong type: string")
	rc.requireValue(t, playerField(pid, "bag"), "v1")
	rc.requireValue(t, versionKey(pid), "1")
	rc.requireValue(t, fieldIndexKey(pid), "not-a-hash")

	// 版本键被换成 HASH:同样三处都拒。
	rc.del(t, fieldIndexKey(pid), versionKey(pid))
	rc.hset(t, versionKey(pid), "f", "x")
	requireAllThreeRefuse(t, rc, pid, "version key wrong type: hash")
	rc.requireValue(t, playerField(pid, "bag"), "v1")
	assert.Equal(t, "hash", rc.typeOf(t, versionKey(pid)))
	assert.False(t, rc.exists(t, fieldIndexKey(pid)))
	assert.Equal(t, "hash", rc.typeOf(t, savebarrier.Key(pid)))
}

// 2. 全量读脚本里 GET 缺失回 false:false 是非 nil 值,在 Lua 表里占位,#out 照常增长,后面的名字 / 值不错位;
// 回到客户端时按脚本侧协议(缺省 RESP2)转成 null —— 对 RESP3 客户端(go-redis v9 缺省)也是 null,不是布尔 false。
// decodeLoadAllReply 只认 nil / string,回布尔会整体 fail-closed。两种协议的原始回复都看一遍,再走一次 LoadPlayerData。
func TestRealRedisLoadAllMissingValueKeepsPairs(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pid := realRedisPlayerBase + 1
	rc := newRealRedisCtx(t, pid)
	saved := map[string]string{"bag": "b", "attr": "a", "mail": "m"}
	ver := mustSave(t, rc.svc, pid, bytesMap(saved))

	// HKEYS 原序(脚本按同一顺序遍历),去掉戳。删第一个名字的字段键:它后面还有名字,截断或错位都看得出来。
	// 只删字段键、不动版本与戳,索引照旧可信,全量读仍走索引分支。
	members, err := rc.raw.HKeys(ctx, fieldIndexKey(pid)).Result()
	require.NoError(t, err)
	var names []string
	for _, name := range members {
		if name != fieldIndexStampField {
			names = append(names, name)
		}
	}
	require.Len(t, names, len(saved))
	gone := names[0]
	rc.del(t, playerField(pid, gone))
	want := make(map[string]string, len(saved))
	for name, val := range saved {
		if name != gone {
			want[name] = val
		}
	}

	for _, protocol := range []int{2, 3} {
		client := goredis.NewClient(&goredis.Options{Addr: rc.host, Protocol: protocol})
		res, err := client.Eval(ctx, loadAllFieldsScript, []string{versionKey(pid), fieldIndexKey(pid)}, playerKeyPrefix(pid)).Result()
		_ = client.Close()
		require.NoError(t, err, "RESP%d", protocol)
		arr, ok := res.([]interface{})
		require.True(t, ok, "RESP%d: %T %v", protocol, res, res)
		require.Len(t, arr, 2+2*len(names), "RESP%d: false 必须占位;被截断或错位时长度不对", protocol)
		assert.Equal(t, loadAllIndexed, arr[0], "RESP%d", protocol)
		assert.Equal(t, strconv.FormatUint(ver, 10), arr[1], "RESP%d", protocol)

		gonePos := -1
		got := make(map[string]string)
		for i := 2; i < len(arr); i += 2 {
			name, ok := arr[i].(string)
			require.True(t, ok, "RESP%d: 位置 %d 应是字段名,实际 %T %v", protocol, i, arr[i], arr[i])
			if name == gone {
				gonePos = i
				assert.Nil(t, arr[i+1], "RESP%d: 缺失值必须是 null(不是布尔 false)", protocol)
				continue
			}
			val, ok := arr[i+1].(string)
			require.True(t, ok, "RESP%d: 字段 %s 的值 %T %v", protocol, name, arr[i+1], arr[i+1])
			got[name] = val
		}
		require.NotEqual(t, -1, gonePos, "RESP%d: 索引里的名字即使字段键已删也要出现在回复里", protocol)
		assert.Less(t, gonePos, len(arr)-2, "RESP%d: 被删的名字后面必须还有名字,才证明没有截断", protocol)
		assert.Equal(t, want, got, "RESP%d", protocol)
	}

	rec := &cmdRecorder{}
	installHook(t, rc.svc, pid, rec)
	rec.reset()
	resp := rc.loadAll(t, pid)
	assert.Zero(t, rec.count("scan"), "索引仍可信,不应退回 SCAN")
	assert.Equal(t, ver, resp.Version)
	assert.Equal(t, bytesMap(want), resp.Data)
}

// 3. 戳与 GET(__version) 逐字节相等。saveFieldsScript 的戳取 INCR 之后 GET 出来的串,不用 Lua 数字转串:
// Redis 内嵌的 Lua 5.1 tostring 用 %.14g,15 位版本 123456789012346 会写成 "1.2345678901235e+14",戳与版本永不相等,
// 索引从此恒不可信(退化成每次 SCAN,不报错)。15 位仍在 2^53 以内,INCR 回到 Lua 的整数不丢精度,返回值可以精确断言。
// 守卫上界(15 位最大值 999999999999999)也走一遍:INCR 后是 16 位的 10^15,tostring 会写成 "1e+15"。
func TestRealRedisStampMatchesVersionBytes(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pidNew, pidBig, pidMax := realRedisPlayerBase+2, realRedisPlayerBase+3, realRedisPlayerBase+16
	rc := newRealRedisCtx(t, pidNew, pidBig, pidMax)

	mustSave(t, rc.svc, pidNew, map[string][]byte{"bag": []byte("b1")})
	rc.requireStampMatchesVersion(t, pidNew, "1")
	mustSave(t, rc.svc, pidNew, map[string][]byte{"attr": []byte("a1")})
	rc.requireStampMatchesVersion(t, pidNew, "2")
	assert.Equal(t, []string{fieldIndexStampField, "attr", "bag"}, rc.indexMembers(t, pidNew))

	// 15 位版本:手工造一名戳可信的已索引玩家,再走一次正常存盘(CAS 带同一个版本)。
	const bigVersion uint64 = 123456789012345
	bigStr := strconv.FormatUint(bigVersion, 10)
	rc.set(t, playerField(pidBig, "bag"), "b0")
	rc.set(t, versionKey(pidBig), bigStr)
	rc.hset(t, fieldIndexKey(pidBig), "bag", "")
	rc.hset(t, fieldIndexKey(pidBig), fieldIndexStampField, bigStr)

	saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
		PlayerID: pidBig, Data: map[string][]byte{"bag": []byte("b1")}, ExpectedVersion: bigVersion,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, saved.ErrorCode)
	assert.Equal(t, bigVersion+1, saved.NewVersion)
	rc.requireStampMatchesVersion(t, pidBig, strconv.FormatUint(bigVersion+1, 10))

	rec := &cmdRecorder{}
	installHook(t, rc.svc, pidBig, rec)
	rec.reset()
	resp := rc.loadAll(t, pidBig)
	assert.Zero(t, rec.count("scan"), "戳可信,全量读应走索引")
	assert.Equal(t, bigVersion+1, resp.Version)
	assert.Equal(t, map[string][]byte{"bag": []byte("b1")}, resp.Data)

	// 守卫上界:当前版本 999999999999999 照常存盘,NewVersion 精确为 10^15,戳逐字节相等,全量读仍走索引。
	// 16 位的 10^15 再存盘会被拒,见 TestRealRedisSaveRefusalsHaveNoSideEffects 的 version_1000000000000000。
	const maxVersion uint64 = 999999999999999
	maxStr := strconv.FormatUint(maxVersion, 10)
	rc.set(t, playerField(pidMax, "bag"), "b0")
	rc.set(t, versionKey(pidMax), maxStr)
	rc.hset(t, fieldIndexKey(pidMax), "bag", "")
	rc.hset(t, fieldIndexKey(pidMax), fieldIndexStampField, maxStr)

	saved, err = SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
		PlayerID: pidMax, Data: map[string][]byte{"bag": []byte("b1")}, ExpectedVersion: maxVersion,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, saved.ErrorCode)
	assert.Equal(t, maxVersion+1, saved.NewVersion)
	rc.requireStampMatchesVersion(t, pidMax, "1000000000000000")

	rec.reset()
	resp = rc.loadAll(t, pidMax)
	assert.Zero(t, rec.count("scan"), "戳可信,全量读应走索引")
	assert.Equal(t, maxVersion+1, resp.Version)
	assert.Equal(t, map[string][]byte{"bag": []byte("b1")}, resp.Data)
}

// 4. saveFieldsScript 的拒绝分支零副作用。Lua 出错不回滚已执行的写,所以可失败的检查必须全在第一条 SET 之前:
// 字段键是 HASH 回 {-2, cur};版本损坏 / 索引类型不对回 error_reply。拒绝之后字段未写、版本未变、索引未变。
func TestRealRedisSaveRefusalsHaveNoSideEffects(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pidConflict, pidIndexStr := realRedisPlayerBase+4, realRedisPlayerBase+5
	badVersions := []struct {
		pid     uint64
		version string
		// expected 是存盘带的 ExpectedVersion(0 = 跳过 CAS)。
		expected uint64
		// incrRejected:真 Redis 的 INCR 也拒绝它。其余几个 INCR 照常接受(都 < 2^63),守卫拒绝它们是因为超过 15 位:
		// 版本在 Lua 里是 double,只在 2^53 以内精确(见 saveFieldsScript 注释)。
		incrRejected bool
	}{
		{realRedisPlayerBase + 6, "abc", 0, true},
		{realRedisPlayerBase + 7, "007", 0, true}, // 重点:真 Redis 的 INCR 拒绝前导零串,miniredis 接受
		{realRedisPlayerBase + 8, "00", 0, true},
		{realRedisPlayerBase + 9, "1234567890123456789", 0, false},
		// 2^53+1 带过期一格的 ExpectedVersion=2^53:两者经 tonumber 都舍入成 2^53。旧守卫(≤18 位)放行它,CAS 误判相等、字段被写。
		{realRedisPlayerBase + 14, "9007199254740993", 9007199254740992, false},
		{realRedisPlayerBase + 15, "1000000000000000", 0, false}, // 10^15:超出 15 位上限的最小值
	}
	pids := []uint64{pidConflict, pidIndexStr}
	for _, c := range badVersions {
		pids = append(pids, c.pid)
	}
	rc := newRealRedisCtx(t, pids...)

	t.Run("field_key_is_hash", func(t *testing.T) {
		pid := pidConflict
		mustSave(t, rc.svc, pid, map[string][]byte{"bag": []byte("v1")})
		rc.hset(t, playerField(pid, "other"), "f", "keep")
		requireUntouched := func() {
			t.Helper()
			rc.requireValue(t, playerField(pid, "bag"), "v1")
			assert.False(t, rc.exists(t, playerField(pid, "mail")))
			rc.requireValue(t, versionKey(pid), "1")
			assert.Equal(t, []string{fieldIndexStampField, "bag"}, rc.indexMembers(t, pid))
			rc.requireStampMatchesVersion(t, pid, "1")
			assert.Equal(t, "hash", rc.typeOf(t, playerField(pid, "other")))
			got, _ := rc.hget(t, playerField(pid, "other"), "f")
			assert.Equal(t, "keep", got)
		}

		saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
			PlayerID: pid, Data: map[string][]byte{"bag": []byte("v2"), "mail": []byte("m"), "other": []byte("x")},
		})
		require.NoError(t, err)
		assert.Equal(t, constants.ErrCodeInvalidRequest, saved.ErrorCode)
		assert.Equal(t, uint64(1), saved.NewVersion)
		requireUntouched()

		// 直接调脚本看原始回复 {-2, 1}:手工放数据锁,绕过 Router。
		rc.set(t, routing.PlayerDataLockKey(pid), realRedisLockToken)
		status, ver, err := saveFieldsAtomic(ctx, rc.raw, pid, realRedisLockToken, 1,
			map[string][]byte{"bag": []byte("v2"), "other": []byte("x")})
		require.NoError(t, err)
		assert.Equal(t, saveFieldsTypeConflict, status)
		assert.Equal(t, uint64(1), ver)
		rc.del(t, routing.PlayerDataLockKey(pid))
		requireUntouched()
	})

	t.Run("field_index_is_string", func(t *testing.T) {
		pid := pidIndexStr
		mustSave(t, rc.svc, pid, map[string][]byte{"bag": []byte("v1")})
		rc.del(t, fieldIndexKey(pid))
		rc.set(t, fieldIndexKey(pid), "not-a-hash")

		saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
			PlayerID: pid, Data: map[string][]byte{"bag": []byte("v2"), "mail": []byte("m")},
		})
		require.Error(t, err)
		assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode)
		assert.Contains(t, err.Error(), "field index wrong type: string")
		rc.requireValue(t, playerField(pid, "bag"), "v1")
		assert.False(t, rc.exists(t, playerField(pid, "mail")))
		rc.requireValue(t, versionKey(pid), "1")
		rc.requireValue(t, fieldIndexKey(pid), "not-a-hash")
	})

	for _, c := range badVersions {
		pid := c.pid
		t.Run("version_"+c.version, func(t *testing.T) {
			rc.set(t, versionKey(pid), c.version)
			if c.incrRejected {
				// 前提:真 Redis 的 INCR 拒绝它(且失败不改值)。守卫若晚于 SET,存盘就会留下「字段已写、版本没涨」。
				require.Error(t, rc.raw.Incr(ctx, versionKey(pid)).Err(), "真 Redis 的 INCR 应拒绝 %q", c.version)
				rc.requireValue(t, versionKey(pid), c.version)
			}

			saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
				PlayerID: pid, Data: map[string][]byte{"bag": []byte("v")}, ExpectedVersion: c.expected,
			})
			require.Error(t, err)
			assert.Equal(t, constants.ErrCodeRedis, saved.ErrorCode)
			assert.Contains(t, err.Error(), "version value malformed")
			assert.False(t, rc.exists(t, playerField(pid, "bag")), "先 SET 后 INCR 失败会留下半截写")
			rc.requireValue(t, versionKey(pid), c.version)
			assert.False(t, rc.exists(t, fieldIndexKey(pid)))
		})
	}
}

// 5. deletePlayerFieldsScript:只删 TYPE 为 string 的自有键,别家 HASH 保留,返回值等于实际删掉的 string 键数(索引不计)。
// 索引模式按索引名字删,不 SCAN;未索引时脚本回 -2(零副作用),Go 侧 SCAN 收候选再以回退模式删。
func TestRealRedisDeleteScriptModes(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pidIndexed, pidLegacy, pidDirect := realRedisPlayerBase+10, realRedisPlayerBase+11, realRedisPlayerBase+12
	rc := newRealRedisCtx(t, pidIndexed, pidLegacy, pidDirect)
	rec := &cmdRecorder{}
	installHook(t, rc.svc, pidIndexed, rec)

	t.Run("indexed", func(t *testing.T) {
		pid := pidIndexed
		mustSave(t, rc.svc, pid, bytesMap(map[string]string{"bag": "b", "attr": "a", "mail": "m", "pet": "p"}))
		// 索引里多出的名字:mail 的字段键被带外删掉(不算删除数),pet 被带外换成 HASH(必须保留)。
		rc.del(t, playerField(pid, "mail"), playerField(pid, "pet"))
		rc.hset(t, playerField(pid, "pet"), "f", "keep")
		rc.hset(t, savebarrier.Key(pid), "pending", "keep")
		rc.hset(t, assetop.SQLCommitKey(pid), "data", "keep")
		before := rc.keyTypes(t, pid)
		rec.reset()

		del, err := DeletePlayerData(ctx, rc.svc, &DeletePlayerDataReq{PlayerID: pid})
		require.NoError(t, err)
		require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
		assert.Zero(t, rec.count("scan"), "索引可信,不应 SCAN")
		gone, goneStrings, changed := diffKeyTypes(before, rc.keyTypes(t, pid))
		assert.ElementsMatch(t, []string{
			playerField(pid, "bag"), playerField(pid, "attr"), versionKey(pid), fieldIndexKey(pid),
		}, gone)
		assert.Empty(t, changed)
		assert.Equal(t, uint32(goneStrings), del.KeysDeleted)
		assert.Equal(t, uint32(3), del.KeysDeleted)
		assert.Equal(t, "hash", rc.typeOf(t, playerField(pid, "pet")))
		assert.Equal(t, "hash", rc.typeOf(t, savebarrier.Key(pid)))
		assert.Equal(t, "hash", rc.typeOf(t, assetop.SQLCommitKey(pid)))
	})

	t.Run("legacy_fallback", func(t *testing.T) {
		pid := pidLegacy
		rc.makeLegacyPlayer(t, pid, map[string]string{"bag": "b", "attr": "a"}, "5")
		rc.hset(t, savebarrier.Key(pid), "pending", "keep")
		rc.hset(t, assetop.SQLCommitKey(pid), "data", "keep")
		rc.set(t, playerField(pid, "__old"), "z")          // 保留名:Go 侧候选过滤掉,保守不删
		rc.hset(t, playerField(pid, "other"), "f", "keep") // 未登记的同前缀 HASH:进候选,脚本按 TYPE 跳过
		before := rc.keyTypes(t, pid)
		rec.reset()

		del, err := DeletePlayerData(ctx, rc.svc, &DeletePlayerDataReq{PlayerID: pid})
		require.NoError(t, err)
		require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
		assert.Positive(t, rec.count("scan"), "未索引,应退回 SCAN")
		gone, goneStrings, changed := diffKeyTypes(before, rc.keyTypes(t, pid))
		assert.ElementsMatch(t, []string{playerField(pid, "bag"), playerField(pid, "attr"), versionKey(pid)}, gone)
		assert.Empty(t, changed)
		assert.Equal(t, uint32(goneStrings), del.KeysDeleted)
		assert.Equal(t, uint32(3), del.KeysDeleted)
		assert.Equal(t, "hash", rc.typeOf(t, savebarrier.Key(pid)))
		assert.Equal(t, "hash", rc.typeOf(t, assetop.SQLCommitKey(pid)))
		assert.Equal(t, "hash", rc.typeOf(t, playerField(pid, "other")))
		rc.requireValue(t, playerField(pid, "__old"), "z")
	})

	// 直接调脚本看原始返回值:手工放数据锁,候选不经 Go 侧过滤,脚本自己的防线单独成立。
	t.Run("script_direct", func(t *testing.T) {
		pid := pidDirect
		rc.makeLegacyPlayer(t, pid, map[string]string{"bag": "b", "attr": "a"}, "5")
		rc.hset(t, savebarrier.Key(pid), "pending", "keep")
		rc.set(t, routing.PlayerDataLockKey(pid), realRedisLockToken)
		before := rc.keyTypes(t, pid)

		// token 不对 → -1,零副作用。
		n, err := deletePlayerFieldsAtomic(ctx, rc.raw, pid, "not-the-holder", nil, false)
		require.NoError(t, err)
		assert.Equal(t, deleteFieldsLockLost, n)
		assert.Equal(t, before, rc.keyTypes(t, pid))

		// 索引模式遇上未索引玩家(有版本、无索引)→ -2,零副作用。
		n, err = deletePlayerFieldsAtomic(ctx, rc.raw, pid, realRedisLockToken, nil, false)
		require.NoError(t, err)
		assert.Equal(t, deleteFieldsNeedScan, n)
		assert.Equal(t, before, rc.keyTypes(t, pid))

		// 回退模式,候选是全部现存键:锁(KEYS[1])、版本(KEYS[2])、别家 HASH 都在里面。
		// 脚本跳过 KEYS[1..3] 与非 string,版本只由末尾的 DEL KEYS[2] 计一次。
		candidates := make([]string, 0, len(before))
		for key := range before {
			candidates = append(candidates, key)
		}
		sort.Strings(candidates)
		n, err = deletePlayerFieldsAtomic(ctx, rc.raw, pid, realRedisLockToken, candidates, true)
		require.NoError(t, err)
		gone, goneStrings, changed := diffKeyTypes(before, rc.keyTypes(t, pid))
		assert.ElementsMatch(t, []string{playerField(pid, "bag"), playerField(pid, "attr"), versionKey(pid)}, gone)
		assert.Empty(t, changed)
		assert.Equal(t, int64(goneStrings), n)
		assert.Equal(t, int64(3), n)
		assert.Equal(t, "hash", rc.typeOf(t, savebarrier.Key(pid)))
		rc.requireValue(t, routing.PlayerDataLockKey(pid), realRedisLockToken)
	})
}

// 6. 端到端:存量玩家(裸写字段 + 版本 "5",无索引)→ 全量读(SCAN)→ 新写(不建残缺索引)→ 删除(回退)
// → 重新存盘(新玩家,建索引)→ 全量读与删除都走索引。全程别家 HASH 不动,结束时不留锁。
func TestRealRedisLegacyPlayerEndToEnd(t *testing.T) {
	requireRealRedisIT(t)
	ctx := context.Background()
	pid := realRedisPlayerBase + 13
	rc := newRealRedisCtx(t, pid)
	rc.makeLegacyPlayer(t, pid, map[string]string{"bag": "b", "attr": "a"}, "5")
	rc.hset(t, savebarrier.Key(pid), "pending", "keep")
	rec := &cmdRecorder{}
	installHook(t, rc.svc, pid, rec)

	// 全量读:未索引 → SCAN;读路径不写索引。
	rec.reset()
	resp := rc.loadAll(t, pid)
	assert.Positive(t, rec.count("scan"))
	assert.Equal(t, bytesMap(map[string]string{"bag": "b", "attr": "a"}), resp.Data)
	assert.Equal(t, uint64(5), resp.Version)
	assert.False(t, rc.exists(t, fieldIndexKey(pid)), "读路径不收养")

	// 带读回的版本 CAS 新写:通过、版本 6;存量玩家不建残缺索引。
	saved, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
		PlayerID: pid, Data: map[string][]byte{"mail": []byte("m")}, ExpectedVersion: resp.Version,
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, saved.ErrorCode)
	assert.Equal(t, uint64(6), saved.NewVersion)
	rc.requireValue(t, versionKey(pid), "6")
	assert.False(t, rc.exists(t, fieldIndexKey(pid)), "存量玩家存盘不建残缺索引")

	// 旧版本再 CAS:版本冲突,零副作用。
	stale, err := SavePlayerData(ctx, rc.svc, &SavePlayerDataReq{
		PlayerID: pid, Data: map[string][]byte{"bag": []byte("stale")}, ExpectedVersion: 5,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeVersionMismatch, stale.ErrorCode)
	assert.Equal(t, uint64(6), stale.NewVersion)
	rc.requireValue(t, playerField(pid, "bag"), "b")

	rec.reset()
	resp = rc.loadAll(t, pid)
	assert.Positive(t, rec.count("scan"))
	assert.Equal(t, bytesMap(map[string]string{"bag": "b", "attr": "a", "mail": "m"}), resp.Data)
	assert.Equal(t, uint64(6), resp.Version)

	// 删除:未索引 → 脚本回 -2、Go 侧 SCAN 收候选、回退模式删;别家 HASH 保留。
	rec.reset()
	del, err := DeletePlayerData(ctx, rc.svc, &DeletePlayerDataReq{PlayerID: pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
	assert.Equal(t, uint32(4), del.KeysDeleted) // bag / attr / mail + 版本
	assert.Positive(t, rec.count("scan"))
	assert.Equal(t, map[string]string{savebarrier.Key(pid): "hash"}, rc.keyTypes(t, pid))

	// 删后:无数据,不 SCAN。
	rec.reset()
	resp = rc.loadAll(t, pid)
	assert.Empty(t, resp.Data)
	assert.Zero(t, resp.Version)
	assert.Zero(t, rec.count("scan"))

	// 重新存盘:按新玩家建索引,戳与版本逐字节相等;此后全量读与删除都走索引。
	assert.Equal(t, uint64(1), mustSave(t, rc.svc, pid, map[string][]byte{"bag": []byte("again")}))
	rc.requireStampMatchesVersion(t, pid, "1")
	assert.Equal(t, []string{fieldIndexStampField, "bag"}, rc.indexMembers(t, pid))

	rec.reset()
	resp = rc.loadAll(t, pid)
	assert.Zero(t, rec.count("scan"))
	assert.Equal(t, map[string][]byte{"bag": []byte("again")}, resp.Data)
	assert.Equal(t, uint64(1), resp.Version)

	rec.reset()
	del, err = DeletePlayerData(ctx, rc.svc, &DeletePlayerDataReq{PlayerID: pid})
	require.NoError(t, err)
	require.Equal(t, constants.ErrCodeOK, del.ErrorCode)
	assert.Equal(t, uint32(2), del.KeysDeleted) // bag + 版本
	assert.Zero(t, rec.count("scan"))

	// 全程没有残留的锁:token 校验释放脚本在真 Redis 上把两把锁都删了。
	assert.False(t, rc.exists(t, routing.PlayerDataLockKey(pid)))
	assert.False(t, rc.exists(t, realRedisMappingLockKey(pid)))
	assert.Equal(t, map[string]string{savebarrier.Key(pid): "hash"}, rc.keyTypes(t, pid))
}

// 7. Redis Cluster 上的未声明键。loadAllFieldsScript / deletePlayerFieldsScript 按索引里的名字拼出字段键,没在 KEYS 里声明;
// 它们与 KEYS 同 {id} slot,Redis 7 Cluster 放行。这里在三主集群上直接跑脚本层 helper(saveFieldsAtomic /
// loadAllPlayerFields / deletePlayerFieldsAtomic / scanPlayerKeys),不经 Router:Regions 模式还要 mapping Redis 与映射登记,
// 与本条要钉的 Redis 行为无关。SCAN 回退也在这里走一遍:go-redis 按 hash tag 把 SCAN 只发往该 slot 的主节点,
// 随后的 MGET 同 slot,不会 CROSSSLOT。
//
// 顺带钉住「跨 slot」的真实边界(script.c 的 scriptVerifyClusterState / scriptPrepareForRun,7.2 与 unstable 源码一致):无 shebang 的 EVAL
// 是兼容模式,带 SCRIPT_ALLOW_CROSS_SLOT —— 跨 slot 但仍在本节点的未声明键**静默放行**,只有落在别的节点才报
// "non local key"。所以键格式错拼成别的 slot 时报不报错,取决于那个 slot 恰好在哪个节点;一致性只能靠 hash tag 本身。
//
// 集群(一次性,单容器三进程,三主无副本;announce 127.0.0.1 让宿主机经端口映射直连每个节点):
//
//	docker run --rm -d -p 7001-7003:7001-7003 --name ds-it-cluster $img sh -c 'for p in 7001 7002 7003; do redis-server --port $p --cluster-enabled yes --cluster-config-file nodes-$p.conf --cluster-announce-ip 127.0.0.1 --protected-mode no --daemonize yes; done; sleep 2; redis-cli --cluster create 127.0.0.1:7001 127.0.0.1:7002 127.0.0.1:7003 --cluster-replicas 0 --cluster-yes; tail -f /dev/null'
func TestRealRedisClusterUndeclaredKeys(t *testing.T) {
	requireRealRedisIT(t)
	addrs := os.Getenv(envRealRedisClusterAddrs)
	if addrs == "" {
		t.Skipf("%s 未设置:本用例只打显式指定的一次性 Cluster", envRealRedisClusterAddrs)
	}
	ctx := context.Background()
	seeds := strings.Split(addrs, ",")
	for i := range seeds {
		seeds[i] = strings.TrimSpace(seeds[i])
	}
	// 就绪判断不经 ClusterClient:它把第一次 CLUSTER SLOTS 的结果缓存起来(ClusterStateReloadInterval 缺省 10s,到期也只异步重载),
	// 集群还没组好时缓存到的空表会让下面的 Eventually 白等到超时;缓存到半张表时 MasterForKey 对表外的 slot 随机挑节点,
	// 「同节点 / 别的节点」会分错,跨 slot 探针的假失败会被误读成 Redis 改了规则。所以先用单节点客户端逐个等到
	// cluster_state:ok(缺省 require-full-coverage 下即该节点看到 16384 个 slot 全覆盖),再建集群客户端。
	require.Eventually(t, func() bool {
		for _, addr := range seeds {
			node := goredis.NewClient(&goredis.Options{Addr: addr})
			info, err := node.ClusterInfo(ctx).Result()
			_ = node.Close()
			if err != nil || !strings.Contains(info, "cluster_state:ok") {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond, "%s=%s:集群没有就绪(有节点的 cluster_state 不是 ok)", envRealRedisClusterAddrs, addrs)
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: seeds})
	t.Cleanup(func() { _ = cluster.Close() })
	slots, err := cluster.ClusterSlots(ctx).Result()
	require.NoError(t, err)
	masters := make(map[string]struct{})
	covered := 0
	for _, s := range slots {
		covered += s.End - s.Start + 1
		if len(s.Nodes) > 0 {
			masters[s.Nodes[0].Addr] = struct{}{}
		}
	}
	require.Equal(t, 16384, covered, "CLUSTER SLOTS 不是全覆盖:客户端拿到的拓扑不完整,下面按节点挑 id 不可信")
	require.GreaterOrEqual(t, len(masters), 2, "至少要两个主节点,才分得出同节点跨 slot 与跨节点")
	if info, err := cluster.Info(ctx, "server").Result(); err == nil {
		t.Logf("real redis cluster %s (%d masters): %s", addrs, len(masters), redisVersionLine(info))
	}

	masterOf := func(pid uint64) string {
		t.Helper()
		node, err := cluster.MasterForKey(ctx, versionKey(pid))
		require.NoError(t, err)
		return node.Options().Addr
	}
	slotOf := func(pid uint64) int64 {
		t.Helper()
		slot, err := cluster.ClusterKeySlot(ctx, versionKey(pid)).Result()
		require.NoError(t, err)
		return slot
	}
	// pid 走已索引流程与跨 slot 探针,legacyPid 走 SCAN 回退;sameNodePid / otherNodePid 只当「别的 slot」:
	// 前者与 pid 同节点不同 slot,后者在别的节点。slot 分布由集群决定,从 base+22 起找第一个满足条件的 id。
	pid, legacyPid := realRedisPlayerBase+20, realRedisPlayerBase+21
	home := masterOf(pid)
	var sameNodePid, otherNodePid uint64
	for cand := realRedisPlayerBase + 22; cand < realRedisPlayerBase+2000 && (sameNodePid == 0 || otherNodePid == 0); cand++ {
		if masterOf(cand) != home {
			if otherNodePid == 0 {
				otherNodePid = cand
			}
		} else if sameNodePid == 0 && slotOf(cand) != slotOf(pid) {
			sameNodePid = cand
		}
	}
	require.NotZero(t, sameNodePid, "找不到与 pid 同节点、不同 slot 的 id")
	require.NotZero(t, otherNodePid, "找不到落在别的节点的 id")

	pids := []uint64{pid, legacyPid, sameNodePid, otherNodePid}
	for _, p := range pids {
		keys, err := scanRealRedisPlayerKeys(cluster, p)
		require.NoError(t, err)
		if len(keys) > 0 {
			t.Skipf("玩家 %d 已有 %d 个键(如 %s):不是本用例建的,不碰", p, len(keys), keys[0])
		}
	}
	// 前置检查通过之后才登记清理。一名玩家的键同 slot,一条 DEL 不会 CROSSSLOT。
	t.Cleanup(func() {
		for _, p := range pids {
			keys, err := scanRealRedisPlayerKeys(cluster, p)
			if err != nil {
				t.Errorf("cleanup player %d: scan: %v", p, err)
				continue
			}
			if len(keys) == 0 {
				continue
			}
			if err := cluster.Del(context.Background(), keys...).Err(); err != nil {
				t.Errorf("cleanup player %d: del: %v", p, err)
			}
		}
	})
	rec := &cmdRecorder{}
	cluster.AddHook(rec)

	// 已索引:存盘的键全部声明;全量读的 GET 与删除的 TYPE / DEL 打的是同 slot 的未声明键。
	require.NoError(t, cluster.Set(ctx, routing.PlayerDataLockKey(pid), realRedisLockToken, 0).Err())
	status, ver, err := saveFieldsAtomic(ctx, cluster, pid, realRedisLockToken, 0, bytesMap(map[string]string{"bag": "b", "attr": "a"}))
	require.NoError(t, err)
	require.Equal(t, saveFieldsOK, status)
	require.Equal(t, uint64(1), ver)
	rec.reset()
	data, ver, err := loadAllPlayerFields(ctx, cluster, pid)
	require.NoError(t, err, "同 slot 的未声明键必须放行")
	assert.Equal(t, bytesMap(map[string]string{"bag": "b", "attr": "a"}), data)
	assert.Equal(t, uint64(1), ver)
	assert.Zero(t, rec.count("scan"), "索引可信,不应 SCAN")

	// 跨 slot 探针:同一段全量读脚本,KEYS 仍是 pid 的,ARGV[1] 换成别的玩家的前缀。
	require.NoError(t, cluster.Set(ctx, playerField(sameNodePid, "bag"), "foreign", 0).Err())
	loadAllKeys := []string{versionKey(pid), fieldIndexKey(pid)}
	res, err := cluster.Eval(ctx, loadAllFieldsScript, loadAllKeys, playerKeyPrefix(sameNodePid)).Result()
	require.NoError(t, err, "同节点跨 slot:EVAL 兼容模式放行,不报错")
	reply, err := decodeLoadAllReply(pid, res)
	require.NoError(t, err)
	assert.Equal(t, loadAllIndexed, reply.kind)
	assert.Equal(t, bytesMap(map[string]string{"bag": "foreign"}), reply.data, "读到的是别的 slot 里别的玩家的键,且没有任何报错")
	_, err = cluster.Eval(ctx, loadAllFieldsScript, loadAllKeys, playerKeyPrefix(otherNodePid)).Result()
	require.Error(t, err, "跨节点的未声明键必须报错,不能静默")
	assert.Contains(t, err.Error(), "non local key")

	n, err := deletePlayerFieldsAtomic(ctx, cluster, pid, realRedisLockToken, nil, false)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n) // bag / attr + 版本
	keys, err := scanRealRedisPlayerKeys(cluster, pid)
	require.NoError(t, err)
	assert.Equal(t, []string{routing.PlayerDataLockKey(pid)}, keys, "只剩手工放的数据锁")

	// 未索引(存量玩家):脚本回退信号 → SCAN 收键(按 hash tag 路由)→ MGET 同 slot → 回退模式删。
	for name, val := range map[string]string{"bag": "b", "attr": "a"} {
		require.NoError(t, cluster.Set(ctx, playerField(legacyPid, name), val, 0).Err())
	}
	require.NoError(t, cluster.Set(ctx, versionKey(legacyPid), "5", 0).Err())
	require.NoError(t, cluster.HSet(ctx, savebarrier.Key(legacyPid), "pending", "keep").Err())
	require.NoError(t, cluster.Set(ctx, routing.PlayerDataLockKey(legacyPid), realRedisLockToken, 0).Err())
	rec.reset()
	data, ver, err = loadAllPlayerFields(ctx, cluster, legacyPid)
	require.NoError(t, err)
	assert.Equal(t, bytesMap(map[string]string{"bag": "b", "attr": "a"}), data)
	assert.Equal(t, uint64(5), ver)
	assert.Positive(t, rec.count("scan"), "未索引,应退回 SCAN")

	n, err = deletePlayerFieldsAtomic(ctx, cluster, legacyPid, realRedisLockToken, nil, false)
	require.NoError(t, err)
	assert.Equal(t, deleteFieldsNeedScan, n)
	candidates, err := scanPlayerKeys(ctx, cluster, legacyPid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		playerField(legacyPid, "bag"), playerField(legacyPid, "attr"), versionKey(legacyPid),
		savebarrier.Key(legacyPid), routing.PlayerDataLockKey(legacyPid),
	}, candidates, "Cluster 上的 SCAN 必须收全这名玩家的键")
	n, err = deletePlayerFieldsAtomic(ctx, cluster, legacyPid, realRedisLockToken, candidates, true)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n) // bag / attr + 版本;别家 HASH 与锁不删
	keys, err = scanRealRedisPlayerKeys(cluster, legacyPid)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{savebarrier.Key(legacyPid), routing.PlayerDataLockKey(legacyPid)}, keys)
}
