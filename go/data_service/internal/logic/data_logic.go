package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/routing"
	"data_service/internal/svc"

	"shared/redisslot"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	playerDataPrefix    = "player:{%d}:" // hash-tag ensures same slot
	playerVersionKey    = "player:{%d}:__version"
	playerFieldIndexKey = "player:{%d}:__fields"
	// fieldIndexStampField 是字段索引里记「索引在哪个版本上完整」的保留成员,与三段 Lua 里的字面量 '__version' 同步。
	fieldIndexStampField = "__version"
	// reservedFieldPrefix 开头的字段名一律保留给 data_service 内部键(__version / __lock / __fields 与将来新增的)。
	reservedFieldPrefix = "__"
)

func playerField(playerID uint64, field string) string {
	return fmt.Sprintf("player:{%d}:%s", playerID, field)
}

func playerKeyPrefix(playerID uint64) string {
	return fmt.Sprintf(playerDataPrefix, playerID)
}

func versionKey(playerID uint64) string {
	return fmt.Sprintf(playerVersionKey, playerID)
}

func fieldIndexKey(playerID uint64) string {
	return fmt.Sprintf(playerFieldIndexKey, playerID)
}

// isReservedPlayerField 报告字段名能不能当 data_service 的玩家字段用。
//
// player:{id}:<field> 与别的模块共用同一个花括号前缀:C++ 存盘围栏 player:{id}:save_barrier(HASH)、go/db 的
// SQL 提交证据 player:{id}:asset_sql_commit(HASH),以及玩家域集群化后带 tag 的 owner_epoch / handoff / location 等
// (docs/design/redis-player-store-cluster.md §5.2)。用这些名字 Save / SetField 会覆盖别家的键,SCAN 退路还会把
// 它们当字段读出或删掉 —— 例如覆盖 save_barrier 后 C++ 存盘脚本见到类型不对就回 error_reply,该玩家此后每一次
// 游戏存盘都失败;删掉 owner_epoch 等于抹掉归属代次。保留后缀只认 shared/redisslot 的登记表,这里不另抄名字;
// 今后任何模块新增 player:{<id>}: 键,必须登记到那张表(redisslot 的契约测试枚举核对)。
// playerID 不参与判定(保留名与玩家无关),留着是为了调用点不变。
func isReservedPlayerField(_ uint64, field string) bool {
	return strings.HasPrefix(field, reservedFieldPrefix) || redisslot.IsReservedPlayerKeySuffix(field)
}

// ── 玩家数据路由 ───────────────────────────────────────────────

// routeFailureLogTag 是玩家数据路由失败日志的固定前缀,值班按它检索;改它等于改排障口径。
const routeFailureLogTag = "[DataRoute]"

// logRouteFailure 记一次玩家数据路由失败(解析不出 home_zone 或数据 Redis)的原因。
//
// in-band 入口把路由失败一律折成 ErrCodeRedis(故障;不另分码的理由见 constants.ErrCodeRedis 与
// docs/design/data_service_role_and_scope.md「已澄清」),server 层只回码、丢掉错误文本,serverbase 的
// rpc_inband_fault 只带 method/code —— 原因只能在这里留下。错误文本本身已能区分:no home zone mapping(映射缺席)、
// invalid zone mapping(值畸形)、mapping lookup failed(mapping Redis 故障或熔断)、route read unavailable(全局库回源失败)、
// no Redis cluster configured for zone N(Regions 漏配);ctx_err 非空表示调用方取消或超时。只记日志,不改返回值。
func logRouteFailure(ctx context.Context, op string, playerID uint64, err error) {
	logx.WithContext(ctx).Errorf("%s %s player=%d: player data routing failed, answered ErrCodeRedis: %v (ctx_err=%v)",
		routeFailureLogTag, op, playerID, err, ctx.Err())
}

// clientForPlayer 是 in-band 读入口解析玩家数据 Redis 的唯一入口:失败时先记原因,再把原错误原样交回
// (errors.Is 仍可判 routing.ErrHomeZoneNotMapped / routing.ErrRouteReadUnavailable),调用方照旧折成 ErrCodeRedis。
// 写入口走 acquirePlayerLock(写门禁);账本读(persisted_player_database.go、asset_sql_commit.go)回 gRPC status、
// 由调用方记日志,不走这里。
func clientForPlayer(ctx context.Context, svcCtx *svc.ServiceContext, op string, playerID uint64) (goredis.Cmdable, error) {
	client, err := svcCtx.Router.ClientForPlayer(ctx, playerID)
	if err != nil {
		logRouteFailure(ctx, op, playerID, err)
		return nil, err
	}
	return client, nil
}

// ── LoadPlayerData ─────────────────────────────────────────────

type LoadPlayerDataReq struct {
	PlayerID uint64
	Fields   []string
}

type LoadPlayerDataResp struct {
	ErrorCode uint32
	Data      map[string][]byte
	Version   uint64
}

// LoadPlayerData 读玩家字段与乐观锁版本。
//
// 契约:返回的 Version 永远不比 Data 新。调用方拿它(非 0)做 CAS 存盘时,只要读之后有别的写落地,
// 存盘必然被拒;最坏只是多一次伪冲突,不会覆盖别人的写。版本键不存在回 0 —— 按 saveFieldsScript
// 的既有约定 expected_version=0 跳过校验,首次存盘前的并发不受本契约保护。任何读失败
// (含版本读失败、版本值损坏、字段索引类型损坏)一律 fail-closed 回 ErrCodeRedis —— 回一个 Version=0 的"成功"
// 会让调用方以为可以跳过版本校验,等于关掉乐观锁。请求保留名(见 isReservedPlayerField)回 ErrCodeInvalidRequest。
func LoadPlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *LoadPlayerDataReq) (*LoadPlayerDataResp, error) {
	for _, field := range req.Fields {
		if isReservedPlayerField(req.PlayerID, field) {
			return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
		}
	}
	client, err := clientForPlayer(ctx, svcCtx, "LoadPlayerData", req.PlayerID)
	if err != nil {
		return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}

	var data map[string][]byte
	var ver uint64
	if len(req.Fields) == 0 {
		data, ver, err = loadAllPlayerFields(ctx, client, req.PlayerID)
	} else {
		data, ver, err = loadSelectedPlayerFields(ctx, client, req.PlayerID, req.Fields)
	}
	if err != nil {
		return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	return &LoadPlayerDataResp{Data: data, Version: ver}, nil
}

// loadSelectedPlayerFields 把版本键与字段键放进同一条 MGET:所有 player:{id}:* 共享 {id} hash-tag,
// 同一 slot,单条命令原子,读到的版本与字段属于同一瞬间。
//
// 旧写法先 MGET 字段、再单独 GET 版本,两步之间落地的 saveFieldsScript 会让返回值变成
// 「旧数据 + 新版本」,调用方据此 CAS 存盘能通过,覆盖掉中间那次写。
func loadSelectedPlayerFields(ctx context.Context, client goredis.Cmdable, playerID uint64, fields []string) (map[string][]byte, uint64, error) {
	keys := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		keys = append(keys, playerField(playerID, f))
	}
	keys = append(keys, versionKey(playerID))
	vals, err := mget(ctx, client, keys...)
	if err != nil {
		return nil, 0, err
	}
	ver, err := parseVersionReply(playerID, vals[len(fields)])
	if err != nil {
		return nil, 0, err
	}
	data := make(map[string][]byte, len(fields))
	for i, f := range fields {
		if vals[i] == nil {
			continue
		}
		data[f] = []byte(vals[i].(string))
	}
	return data, ver, nil
}

// ── 字段索引 ───────────────────────────────────────────────────
//
// player:{id}:__fields 是 data_service 自有字段键的索引(HASH,与 __version / __lock / 字段键同 {id} slot):
// 成员 <字段名> = "",保留成员 __version = 戳,记「索引在哪个版本上完整」。
// 可信判据只有一条:TYPE 为 hash、__version 存在、且 HGET(__fields, '__version') 与 GET(__version) 逐字节相等。
//   - 版本、索引都不存在 → 无数据,不 SCAN;
//   - 可信 → 全量读 / 删除按索引一段 Lua 完成,不 SCAN;
//   - 其余(存量老玩家、戳失效、有索引无版本)→ 未索引,退回 SCAN(今天的行为)。
// 不变量:可信时,该版本下所有 data_service 字段键都在索引里(可以多,不能少)。只有 saveFieldsScript 维护它;
// 任何只 INCR 不维护戳的写者(滚动升级中的旧副本)都会让索引自动失效,读方退回 SCAN,不会漏读。
// 把版本**重置**成更小值的带外写者(cmd/debug_import 的 player 模式写 "1")必须在同一个 MULTI 里 DEL 索引:
// 否则旧戳可能恰好等于重置后的版本,带外写进来的字段被当成「不在索引里」藏掉,下一次存盘还会把它固化。
// 读路径不「收养」存量玩家(不在读路径写元数据):这族键没有生产写者,存量集合不再增长;收敛靠
// player_fields_enum_total{path="legacy_scan"} 观察。

// loadAllFieldsScript 是全量读(只读)。KEYS[1] = 版本键,KEYS[2] = 字段索引,ARGV[1] = 前缀 player:{id}:。
// 返回 {0}(无数据)、{1, ver, 名1, 值1|false, ...}(已索引:字段集合、值、版本同一瞬间)或 {2, ver|false}(未索引,需回退)。
// GET 缺失回 false,在表里占位、不截断返回数组;名字对应的键不是 string 时 redis.call 报错,整体 fail-closed。
// 按名字拼出的字段键没有在 KEYS 里声明,但与 KEYS 同 {id} slot:单实例没有限制,Cluster 放行同 slot 的未声明键。
// 不能指望 Redis 替拼错的键报错:本仓脚本无 shebang,是 EVAL 兼容模式(带 SCRIPT_ALLOW_CROSS_SLOT),跨 slot 但仍在
// 本节点的未声明键**静默放行**,只有落在别的节点才报 "non local key"(两种情形由 TestRealRedisClusterUndeclaredKeys 钉住)。
// 正确性只靠拼出的键与 KEYS 共用 {id} hash tag;改键格式要同步这里。
const loadAllFieldsScript = `
local vtype = redis.call('TYPE', KEYS[1]).ok
if vtype ~= 'none' and vtype ~= 'string' then
	return redis.error_reply('PLAYER_FIELDS version key wrong type: ' .. vtype)
end
local itype = redis.call('TYPE', KEYS[2]).ok
if itype ~= 'none' and itype ~= 'hash' then
	return redis.error_reply('PLAYER_FIELDS field index wrong type: ' .. itype)
end
local ver = redis.call('GET', KEYS[1])
if itype == 'none' then
	if ver == false then
		return {0}
	end
	return {2, ver}
end
if ver == false or redis.call('HGET', KEYS[2], '__version') ~= ver then
	return {2, ver}
end
local out = {1, ver}
for _, name in ipairs(redis.call('HKEYS', KEYS[2])) do
	if name ~= '__version' then
		out[#out + 1] = name
		out[#out + 1] = redis.call('GET', ARGV[1] .. name)
	end
end
return out`

const (
	loadAllNoData    int64 = 0
	loadAllIndexed   int64 = 1
	loadAllUnindexed int64 = 2
)

type loadAllReply struct {
	kind    int64
	version uint64
	data    map[string][]byte
}

// decodeLoadAllReply 解读 loadAllFieldsScript 的回复;形状不对一律报错(fail-closed)。纯函数,单测直接测。
func decodeLoadAllReply(playerID uint64, res interface{}) (loadAllReply, error) {
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		return loadAllReply{}, fmt.Errorf("unexpected load-all reply for player %d: %T %v", playerID, res, res)
	}
	kind, ok := arr[0].(int64)
	if !ok {
		return loadAllReply{}, fmt.Errorf("unexpected load-all reply kind for player %d: %T %v", playerID, arr[0], arr[0])
	}
	switch kind {
	case loadAllNoData:
		if len(arr) != 1 {
			return loadAllReply{}, fmt.Errorf("unexpected load-all no-data reply for player %d: %d elements", playerID, len(arr))
		}
		return loadAllReply{kind: kind, data: map[string][]byte{}}, nil
	case loadAllUnindexed:
		if len(arr) > 2 {
			return loadAllReply{}, fmt.Errorf("unexpected load-all unindexed reply for player %d: %d elements", playerID, len(arr))
		}
		var raw interface{}
		if len(arr) == 2 {
			raw = arr[1]
		}
		ver, err := parseVersionReply(playerID, raw)
		if err != nil {
			return loadAllReply{}, err
		}
		return loadAllReply{kind: kind, version: ver}, nil
	case loadAllIndexed:
		if len(arr) < 2 || (len(arr)-2)%2 != 0 {
			return loadAllReply{}, fmt.Errorf("unexpected load-all indexed reply for player %d: %d elements", playerID, len(arr))
		}
		rawVer, ok := arr[1].(string)
		if !ok {
			return loadAllReply{}, fmt.Errorf("unexpected load-all indexed version for player %d: %T %v", playerID, arr[1], arr[1])
		}
		ver, err := parseVersionValue(playerID, rawVer)
		if err != nil {
			return loadAllReply{}, err
		}
		data := make(map[string][]byte, (len(arr)-2)/2)
		for i := 2; i < len(arr); i += 2 {
			name, ok := arr[i].(string)
			if !ok {
				return loadAllReply{}, fmt.Errorf("unexpected load-all field name for player %d: %T %v", playerID, arr[i], arr[i])
			}
			switch val := arr[i+1].(type) {
			case nil:
				// 索引可以多出成员(字段键被带外删掉);读不到值就跳过。
			case string:
				data[name] = []byte(val)
			default:
				return loadAllReply{}, fmt.Errorf("unexpected load-all value for player %d field %q: %T", playerID, name, val)
			}
		}
		return loadAllReply{kind: kind, version: ver, data: data}, nil
	default:
		return loadAllReply{}, fmt.Errorf("unexpected load-all reply kind %d for player %d", kind, playerID)
	}
}

// loadAllPlayerFields 读玩家全部字段:已索引一段 Lua 读完(版本与数据同一瞬间),无数据直接回空,
// 只有未索引的存量玩家才退回 SCAN(loadAllPlayerFieldsByScan)。
func loadAllPlayerFields(ctx context.Context, client goredis.Cmdable, playerID uint64) (map[string][]byte, uint64, error) {
	evalCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	res, err := client.Eval(evalCtx, loadAllFieldsScript, []string{versionKey(playerID), fieldIndexKey(playerID)},
		playerKeyPrefix(playerID)).Result()
	cancel()
	if err != nil {
		logx.Errorf("[LoadPlayerData] player %d: load-all script failed: %v", playerID, err)
		return nil, 0, err
	}
	reply, err := decodeLoadAllReply(playerID, res)
	if err != nil {
		logx.Errorf("[LoadPlayerData] player %d: load-all script reply rejected: %v", playerID, err)
		return nil, 0, err
	}
	switch reply.kind {
	case loadAllNoData:
		metrics.ObservePlayerFieldsEnum(metrics.PlayerFieldsOpLoadAll, metrics.PlayerFieldsPathEmpty)
		return reply.data, 0, nil
	case loadAllIndexed:
		metrics.ObservePlayerFieldsEnum(metrics.PlayerFieldsOpLoadAll, metrics.PlayerFieldsPathIndexed)
		return reply.data, reply.version, nil
	default:
		metrics.ObservePlayerFieldsEnum(metrics.PlayerFieldsOpLoadAll, metrics.PlayerFieldsPathLegacyScan)
		logx.Infof("[LoadPlayerData] player %d: field index absent or stale (version %d), enumerating with SCAN",
			playerID, reply.version)
		return loadAllPlayerFieldsByScan(ctx, client, playerID, reply.version)
	}
}

// loadAllPlayerFieldsByScan 是未索引玩家的全量读:SCAN 枚举字段键,再一条 MGET 连同版本键一起读。
// verBeforeScan 是判定「未索引」那一刻读出的版本(先读版本、再读数据)。
//
// MGET 本身原子,但 SCAN 与 MGET 是两步:窗口内落地的存盘可能新建了 SCAN 没扫到的字段键,
// 这时 MGET 里的版本对应的数据并不完整。拿它做 CAS 会通过,调用方若据此「新建」那个它没看到的字段,
// 就覆盖了窗口里的那次写。所以:
//   - MGET 里的版本与 verBeforeScan 相同 → 窗口内没有存盘(saveFieldsScript 成功必 INCR),SCAN 的键集完整,
//     数据与版本属于同一瞬间;
//   - 不同 → 返回 verBeforeScan。它比数据旧,调用方据此 CAS 必然被拒,只多一次伪冲突。
//
// 保留名(内部键、save_barrier、asset_sql_commit)不进 MGET;MGET 对非 string 键回 nil,按缺失跳过。
func loadAllPlayerFieldsByScan(ctx context.Context, client goredis.Cmdable, playerID, verBeforeScan uint64) (map[string][]byte, uint64, error) {
	scanned, err := scanPlayerKeys(ctx, client, playerID)
	if err != nil {
		return nil, 0, err
	}
	prefix := playerKeyPrefix(playerID)
	keys := make([]string, 0, len(scanned)+1)
	for _, key := range scanned {
		if !strings.HasPrefix(key, prefix) || isReservedPlayerField(playerID, key[len(prefix):]) {
			continue
		}
		keys = append(keys, key)
	}
	// 版本键固定放在末尾。
	keys = append(keys, versionKey(playerID))
	vals, err := mget(ctx, client, keys...)
	if err != nil {
		return nil, 0, err
	}
	verAtRead, err := parseVersionReply(playerID, vals[len(vals)-1])
	if err != nil {
		return nil, 0, err
	}

	data := make(map[string][]byte)
	for i, key := range keys[:len(keys)-1] {
		if vals[i] == nil {
			continue
		}
		data[key[len(prefix):]] = []byte(vals[i].(string))
	}

	if verAtRead != verBeforeScan {
		logx.Infof("[LoadPlayerData] player %d: save landed during full load (version %d -> %d), returning the pre-scan version",
			playerID, verBeforeScan, verAtRead)
		return data, verBeforeScan, nil
	}
	return data, verAtRead, nil
}

// scanPlayerKeys 用 SCAN player:{id}:* 枚举一名玩家的键。只有未索引的存量玩家才会走到这里:
// 单机上 hash tag 不收窄,要扫整个库(轮数约 = 全库键数 / 100);Cluster 上 go-redis 按 pattern 的 slot
// 只发往该 slot 的主节点(Scan 对带 hash tag 的 pattern 调 SetFirstKeyPos),迁槽窗口内会漏掉已迁走的键(既有问题)。
func scanPlayerKeys(ctx context.Context, client goredis.Cmdable, playerID uint64) ([]string, error) {
	pattern := playerKeyPrefix(playerID) + "*"
	var keys []string
	var cursor uint64
	for {
		scanCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		batch, next, err := client.Scan(scanCtx, cursor, pattern, 100).Result()
		cancel()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if next == 0 {
			return keys, nil
		}
		cursor = next
	}
}

// ── SavePlayerData ─────────────────────────────────────────────

type SavePlayerDataReq struct {
	PlayerID        uint64
	Data            map[string][]byte
	ExpectedVersion uint64
}

type SavePlayerDataResp struct {
	ErrorCode  uint32
	NewVersion uint64
}

// acquirePlayerLock 走 Router.AcquirePlayerWriteLock(预检 home 与合服围栏 → 拿两把锁 → 数据锁之后复核),
// 把结果翻成 in-band 码。成功时返回玩家的数据客户端与持锁 token,调用方必须
// defer svcCtx.Router.ReleasePlayerLock(ctx, client, id, token)(直接传请求 ctx,释放自带不随请求取消的短超时)。
// op 只进日志。
//
// 码(server 层 err≠nil 时只回码、不留文本,所以每个失败分支都打日志):
//   - 合服围栏 / 拿锁后发现 home 已被改写 → ErrCodeZoneMergeInProgress(22),err=nil:规则拒绝、零写入,合服结束后重试;
//   - 锁被占                              → ErrCodeLockConflict(2),err=nil;
//   - 其余(Regions 下映射缺席、Redis 故障、就绪读回源失败、zone 未配集群、映射值畸形)→ ErrCodeRedis(1),带 err:
//     与读路径同一口径,路由失败算故障(理由见 constants.ErrCodeRedis)。
//
// 22 / 2 / 1 都发生在 saveFieldsScript / 删除脚本之前,保证零写入。
func acquirePlayerLock(ctx context.Context, svcCtx *svc.ServiceContext, op string, playerID uint64) (goredis.Cmdable, string, uint32, error) {
	client, token, locked, err := svcCtx.Router.AcquirePlayerWriteLock(ctx, playerID)
	switch {
	case errors.Is(err, routing.ErrZoneMergeInProgress):
		metrics.ObservePlayerLock("merge_fenced")
		logx.WithContext(ctx).Errorf("[PlayerWriteLock] %s player %d: write refused by merge fence: %v", op, playerID, err)
		return nil, "", constants.ErrCodeZoneMergeInProgress, nil
	case err != nil:
		metrics.ObservePlayerLock("error")
		logx.WithContext(ctx).Errorf("%s %s player=%d: write route or lock failed, answered ErrCodeRedis: %v (ctx_err=%v)",
			routeFailureLogTag, op, playerID, err, ctx.Err())
		return nil, "", constants.ErrCodeRedis, err
	case !locked:
		metrics.ObservePlayerLock("conflict")
		logx.Errorf("player %d data is being written by another process", playerID)
		return nil, "", constants.ErrCodeLockConflict, nil
	}
	metrics.ObservePlayerLock("acquired")
	return client, token, 0, nil
}

// saveFieldsScript 原子完成「校验 → 字段写入 → 版本自增 → 维护字段索引」。
//
// 拆开做(先 GET 比版本、再 Pipeline 写)会留一条竞态:两个写者在对方写入前
// 都读到相同版本、双双通过检查,随后交错写入 —— 乐观锁失效,玩家数据出现
// 字段级撕裂。玩家锁挡不住这条路:它带 TTL,慢写者超时后锁会被下一个写者
// 合法拿走。所有 player:{id}:* key 共享 {id} hash-tag(见 playerDataPrefix),
// 同一 slot,Cluster 下单脚本可覆盖。
//
// Lua 出错不会回滚已执行的写,所以所有可失败的检查都在第一条 SET 之前:版本键类型与格式(规范十进制:无前导零、≤15 位)、
// 锁 token、期望版本、索引类型、每个字段键的类型(不是 none / string 就零副作用拒写,防止把别家的 HASH 覆盖成 string)。
// 「写成功必 INCR」是全量读回退兜底的前提。
//
// 版本格式守卫的两条理由:
//   - 规范十进制:真 Redis 的 INCR 拒绝 "007" 这类前导零串,miniredis 却接受,所以守卫不能只查「全是数字」;
//   - ≤15 位是精度上限,不是格式:版本经 tonumber 与 INCR 进入 Lua 都是 double,只在 2^53 以内精确。超过之后相邻整数
//     舍入成同一个数,过期的 expected_version 会被当成相等放行(2^53+1 与 2^53 都变成 2^53),返回的 new_version 也不准。
//     ≤15 位时当前版本 < 10^15、INCR 后 ≤ 10^15,都在 2^53 以内;expected_version 超过 2^53 时 tonumber 也会舍入,
//     但舍入后仍 ≥ 2^53,不会与当前版本相等。更大的版本只可能是带外写坏的,按损坏拒写(单玩家存盘次数到不了 10^15)。
//     读路径不经 Lua 数字(版本以字符串回到 Go,ParseUint 精确),所以 parseVersionValue 只查规范十进制、不设位数上限。
//
// 字段索引(见上文「字段索引」):
//   - 新玩家(版本、索引都不存在)或索引可信 → 把本次字段名加进索引并把戳盖成新版本;
//   - 否则(存量老玩家、戳失效)→ 不往索引里加名字:往不完整的索引里补名字再盖新戳,会把漏掉的老字段永久藏起来;
//     索引存在则 DEL —— 带外写者把版本重置成更小值之后,旧戳可能与之后某个版本巧合相等。
//   - 戳取 INCR 之后 GET 出来的串:Lua 数字转串在大值时可能变成科学计数法。名字经 ARGV 传入,不在 Lua 里从键名反切前缀。
//
// KEYS[1] = data lock key,KEYS[2] = version key,KEYS[3] = 字段索引,KEYS[4..3+n] = field keys;
// ARGV[1] = lock token,ARGV[2] = expected_version(0 = 跳过检查),
// ARGV[3..2+n] = 字段值,ARGV[3+n..2+2n] = 字段名,两者与字段键同序对应。
// 返回 {1, new_version}、{0, current_version}(版本冲突)、{-1, current_version}(锁已失效)、
// {-2, current_version}(有字段键已存在且不是 string,零副作用),或 error_reply(版本 / 索引类型或格式损坏,零副作用)。
const saveFieldsScript = `
local vtype = redis.call('TYPE', KEYS[2]).ok
if vtype ~= 'none' and vtype ~= 'string' then
	return redis.error_reply('PLAYER_FIELDS version key wrong type: ' .. vtype)
end
local ver = redis.call('GET', KEYS[2])
if ver and (not (ver == '0' or string.match(ver, '^[1-9]%d*$')) or #ver > 15) then
	return redis.error_reply('PLAYER_FIELDS version value malformed')
end
local cur = tonumber(ver or '0')
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return {-1, cur}
end
local expected = tonumber(ARGV[2])
if expected ~= 0 and cur ~= expected then
	return {0, cur}
end
local itype = redis.call('TYPE', KEYS[3]).ok
if itype ~= 'none' and itype ~= 'hash' then
	return redis.error_reply('PLAYER_FIELDS field index wrong type: ' .. itype)
end
for i = 4, #KEYS do
	local ftype = redis.call('TYPE', KEYS[i]).ok
	if ftype ~= 'none' and ftype ~= 'string' then
		return {-2, cur}
	end
end
local maintain
if itype == 'none' then
	maintain = (ver == false)
else
	maintain = (ver ~= false) and (redis.call('HGET', KEYS[3], '__version') == ver)
end
local n = #KEYS - 3
for i = 1, n do
	redis.call('SET', KEYS[3 + i], ARGV[2 + i])
end
local newver = redis.call('INCR', KEYS[2])
if maintain then
	for i = 1, n do
		redis.call('HSET', KEYS[3], ARGV[2 + n + i], '')
	end
	redis.call('HSET', KEYS[3], '__version', redis.call('GET', KEYS[2]))
elseif itype ~= 'none' then
	redis.call('DEL', KEYS[3])
end
return {1, newver}`

type saveFieldsStatus int64

const (
	saveFieldsTypeConflict    saveFieldsStatus = -2
	saveFieldsLockLost        saveFieldsStatus = -1
	saveFieldsVersionMismatch saveFieldsStatus = 0
	saveFieldsOK              saveFieldsStatus = 1
)

// saveFieldsAtomic runs saveFieldsScript. status 精确区分版本冲突、锁失效与类型冲突;
// 锁失效意味着当前调用已经是 TTL 后恢复的旧持锁者，绝不能继续写。
func saveFieldsAtomic(ctx context.Context, client goredis.Cmdable, playerID uint64, token string, expectedVersion uint64, data map[string][]byte) (saveFieldsStatus, uint64, error) {
	keys := make([]string, 0, len(data)+3)
	values := make([]interface{}, 0, len(data))
	names := make([]interface{}, 0, len(data))
	keys = append(keys, routing.PlayerDataLockKey(playerID), versionKey(playerID), fieldIndexKey(playerID))
	for field, val := range data {
		keys = append(keys, playerField(playerID, field))
		values = append(values, val)
		names = append(names, field)
	}
	argv := make([]interface{}, 0, 2+2*len(data))
	argv = append(argv, token, expectedVersion)
	argv = append(argv, values...)
	argv = append(argv, names...)

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := client.Eval(ctx, saveFieldsScript, keys, argv...).Result()
	if err != nil {
		return saveFieldsLockLost, 0, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript reply: %T %v", res, res)
	}
	status, statusOK := arr[0].(int64)
	ver, versionOK := arr[1].(int64)
	if !statusOK || !versionOK || ver < 0 {
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript values: %T/%v %T/%v",
			arr[0], arr[0], arr[1], arr[1])
	}
	switch saveFieldsStatus(status) {
	case saveFieldsTypeConflict, saveFieldsLockLost, saveFieldsVersionMismatch, saveFieldsOK:
		return saveFieldsStatus(status), uint64(ver), nil
	default:
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript status: %d", status)
	}
}

// sortedFieldNames 只给日志用:字段名排序后输出,不带值。
func sortedFieldNames(data map[string][]byte) []string {
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func SavePlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *SavePlayerDataReq) (*SavePlayerDataResp, error) {
	startTime := time.Now()
	for field := range req.Data {
		if isReservedPlayerField(req.PlayerID, field) {
			logx.Errorf("[SavePlayerData] player %d: field %q is reserved", req.PlayerID, field)
			metrics.ObserveSavePlayerData("invalid_request", startTime)
			return &SavePlayerDataResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
		}
	}
	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, "SavePlayerData", req.PlayerID)
	if errCode != 0 {
		outcome := "redis_error"
		switch errCode {
		case constants.ErrCodeLockConflict:
			outcome = "lock_conflict"
		case constants.ErrCodeZoneMergeInProgress:
			outcome = "merge_fenced"
		}
		metrics.ObserveSavePlayerData(outcome, startTime)
		return &SavePlayerDataResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, req.PlayerID, token)

	status, ver, err := saveFieldsAtomic(ctx, client, req.PlayerID, token, req.ExpectedVersion, req.Data)
	if err != nil {
		metrics.ObserveSavePlayerData("redis_error", startTime)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	switch status {
	case saveFieldsLockLost:
		metrics.ObserveSavePlayerData("lock_conflict", startTime)
		logx.Errorf("player %d lock ownership expired before atomic save; stale writer fenced", req.PlayerID)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeLockConflict, NewVersion: ver}, nil
	case saveFieldsVersionMismatch:
		logx.Errorf("version mismatch for player %d: expected %d, got %d", req.PlayerID, req.ExpectedVersion, ver)
		metrics.ObserveVersionMismatch("save_player_data")
		metrics.ObserveSavePlayerData("version_mismatch", startTime)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeVersionMismatch, NewVersion: ver}, nil
	case saveFieldsTypeConflict:
		metrics.ObserveSavePlayerData("type_conflict", startTime)
		logx.Errorf("[SavePlayerData] player %d: refused before any write: a requested field key exists with a non-string type (fields=%v)",
			req.PlayerID, sortedFieldNames(req.Data))
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeInvalidRequest, NewVersion: ver}, nil
	}

	metrics.ObserveSavePlayerData("ok", startTime)
	return &SavePlayerDataResp{NewVersion: ver}, nil
}

// ── GetPlayerField ─────────────────────────────────────────────

func GetPlayerField(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, field string) ([]byte, error) {
	if isReservedPlayerField(playerID, field) {
		return nil, fmt.Errorf("field %q is reserved", field)
	}
	client, err := clientForPlayer(ctx, svcCtx, "GetPlayerField", playerID)
	if err != nil {
		return nil, err
	}
	val, err := getString(ctx, client, playerField(playerID, field))
	if err == goredis.Nil {
		return nil, nil
	}
	return []byte(val), err
}

// ── SetPlayerField ─────────────────────────────────────────────

type SetPlayerFieldResp struct {
	ErrorCode  uint32
	NewVersion uint64
}

func SetPlayerField(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, field string, value []byte, expectedVersion uint64) (*SetPlayerFieldResp, error) {
	if isReservedPlayerField(playerID, field) {
		logx.Errorf("[SetPlayerField] player %d: field %q is reserved", playerID, field)
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, "SetPlayerField", playerID)
	if errCode != 0 {
		return &SetPlayerFieldResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, playerID, token)

	status, ver, err := saveFieldsAtomic(ctx, client, playerID, token, expectedVersion, map[string][]byte{field: value})
	if err != nil {
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	switch status {
	case saveFieldsLockLost:
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeLockConflict, NewVersion: ver}, nil
	case saveFieldsVersionMismatch:
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeVersionMismatch, NewVersion: ver}, nil
	case saveFieldsTypeConflict:
		logx.Errorf("[SetPlayerField] player %d: refused before any write: field %q exists with a non-string type", playerID, field)
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeInvalidRequest, NewVersion: ver}, nil
	}

	return &SetPlayerFieldResp{NewVersion: ver}, nil
}

// ── Helpers ────────────────────────────────────────────────────

// parseVersionReply 解读回复里版本键那一格:nil = 键不存在 = 0。
//
// 必须把「版本 key 不存在」和「读失败 / 值损坏」分开:saveFieldsScript 里 expected_version==0 的语义是
// **跳过版本校验**。把读错误或损坏值降级成 0 并当成功返回,调用方会拿着 version=0 去存盘 ——
// 乐观锁被静默关掉,变成无条件覆盖,表现为随机回档。
func parseVersionReply(playerID uint64, raw interface{}) (uint64, error) {
	if raw == nil {
		return 0, nil
	}
	val, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("unexpected version reply for player %d: %T %v", playerID, raw, raw)
	}
	return parseVersionValue(playerID, val)
}

// parseVersionValue 解读版本键**存在**时的值;「键不存在」只由 parseVersionReply 的 nil 分支表达。
// 空串同样按损坏处理:saveFieldsScript 只经 INCR 写版本、守卫也拒绝空串,空串只可能是带外写坏的。
// 读成 0 并回成功违反 LoadPlayerData 的 fail-closed 契约:调用方看到「无版本」,下一次存盘才被写侧守卫拒掉,
// 故障被推迟到写路径,原因也被掩盖。
func parseVersionValue(playerID uint64, val string) (uint64, error) {
	v, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("corrupt version value %q for player %d: %w", val, playerID, err)
	}
	// 非规范十进制(前导零等)按损坏处理,与 saveFieldsScript 的守卫同一口径:真 Redis 的 INCR 不接受它,
	// 读路径若照常回 Version,调用方据此存盘只会撞上写侧的 fail-closed。
	// 写侧的 ≤15 位上限这里不跟:那是 Lua double 的精度限制,读路径不经 Lua 数字,ParseUint 对整个 uint64 都精确。
	if strconv.FormatUint(v, 10) != val {
		return 0, fmt.Errorf("corrupt version value %q for player %d: not canonical decimal", val, playerID)
	}
	return v, nil
}

func getString(ctx context.Context, c goredis.Cmdable, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Get(ctx, key).Result()
}

func mget(ctx context.Context, c goredis.Cmdable, keys ...string) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.MGet(ctx, keys...).Result()
}

// ── DeletePlayerData ───────────────────────────────────────────

type DeletePlayerDataReq struct {
	PlayerID          uint64
	DeleteZoneMapping bool
}

type DeletePlayerDataResp struct {
	ErrorCode   uint32
	KeysDeleted uint32
}

// ErrZoneMappingDeletionDisabled:DeletePlayerData 带 DeleteZoneMapping=true 的请求一律拒绝(W3,2026-10-12,
// docs/design/player-route-mapping-durability-proposal.md §4.3)。拒绝发生在**任何副作用之前**:
// 先删数据键、再拒删映射,会留下"数据没了、映射还在"的半截操作。
// 为什么停用:锁 token 在 mapping Redis、映射真源要落到全局库 player_route,删映射与持锁校验无法原子;
// 删号与落点的关系本来就待定(player-storage-placement.md)。仓库内没有生产调用方。
// server 层把它翻成 gRPC FailedPrecondition(DeletePlayerData 唯一不走 in-band 码的失败)。
var ErrZoneMappingDeletionDisabled = errors.New("DeletePlayerData: delete_zone_mapping is disabled (proposal §4.3, W3)")

// DeletePlayerData 删除 data_service 自有的字段键与版本键(连同字段索引),不删别的模块同前缀的键
// (save_barrier / asset_sql_commit 等非 string 键与保留名)。KeysDeleted 是脚本实际删掉的字段 string 键数加版本键,
// 索引不计。索引可信时一段 Lua 按索引删,不 SCAN;否则退回 SCAN 收集候选,再一段 Lua 只删其中的 string 键。
func DeletePlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *DeletePlayerDataReq) (*DeletePlayerDataResp, error) {
	if req.DeleteZoneMapping {
		logx.Errorf("[DeletePlayerData] player %d: refused before any side effect: delete_zone_mapping is disabled (proposal §4.3, W3)", req.PlayerID)
		return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeNotImplemented},
			fmt.Errorf("%w: player %d", ErrZoneMappingDeletionDisabled, req.PlayerID)
	}

	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, "DeletePlayerData", req.PlayerID)
	if errCode != 0 {
		return &DeletePlayerDataResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, req.PlayerID, token)

	n, err := deletePlayerFieldsAtomic(ctx, client, req.PlayerID, token, nil, false)
	path := metrics.PlayerFieldsPathIndexed
	if err == nil && n == deleteFieldsNeedScan {
		path = metrics.PlayerFieldsPathLegacyScan
		logx.Infof("[DeletePlayerData] player %d: field index absent or stale, collecting candidate keys with SCAN", req.PlayerID)
		keys, scanErr := scanPlayerKeys(ctx, client, req.PlayerID)
		if scanErr != nil {
			logx.Errorf("[DeletePlayerData] player %d: SCAN for candidate keys failed: %v", req.PlayerID, scanErr)
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, scanErr
		}
		prefix := playerKeyPrefix(req.PlayerID)
		candidates := make([]string, 0, len(keys))
		for _, key := range keys {
			// 锁键由 defer 的 token 校验释放;保留名(内部键与别的模块的键)保守不删。
			if !strings.HasPrefix(key, prefix) || isReservedPlayerField(req.PlayerID, key[len(prefix):]) {
				continue
			}
			candidates = append(candidates, key)
		}
		n, err = deletePlayerFieldsAtomic(ctx, client, req.PlayerID, token, candidates, true)
	}
	if err != nil {
		logx.Errorf("[DeletePlayerData] player %d: delete script failed: %v", req.PlayerID, err)
		return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if n == deleteFieldsLockLost {
		logx.Errorf("[DeletePlayerData] player %d: lock expired before delete; stale deleter fenced", req.PlayerID)
		return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeLockConflict}, nil
	}
	if n < 0 {
		return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, fmt.Errorf("unexpected delete script result %d for player %d", n, req.PlayerID)
	}
	metrics.ObservePlayerFieldsEnum(metrics.PlayerFieldsOpDelete, path)
	deleted := uint32(n)

	// 映射删除分支已停用(入口处就拒绝了 DeleteZoneMapping=true,见 ErrZoneMappingDeletionDisabled),这里只删数据键。
	logx.Infof("[DeletePlayerData] player %d: deleted=%d path=%s (zone mapping kept)", req.PlayerID, deleted, path)
	return &DeletePlayerDataResp{KeysDeleted: deleted}, nil
}

// deletePlayerFieldsScript 删除一名玩家 data_service 自有的键。
//
// KEYS[1] = 数据锁,KEYS[2] = 版本键,KEYS[3] = 字段索引,KEYS[4..] = SCAN 收来的候选键(只在回退模式下有);
// ARGV[1] = token,ARGV[2] = '0' 只按可信索引删 / '1' 回退模式,ARGV[3] = 前缀 player:{id}:。
// 返回 -1(锁已失效)、-2(需要回退,零副作用)或 ≥0(实际删掉的自有 string 键数:字段键加版本键;索引不计)。
// 只删 TYPE 为 string 的键:别的模块同前缀的 HASH 不会被误删。「是否需要回退」在脚本里判定,Go 侧不做非原子预检。
// 按索引名字拼出的字段键没有在 KEYS 里声明,同 slot 约束同 loadAllFieldsScript。
const deletePlayerFieldsScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
local vtype = redis.call('TYPE', KEYS[2]).ok
if vtype ~= 'none' and vtype ~= 'string' then
	return redis.error_reply('PLAYER_FIELDS version key wrong type: ' .. vtype)
end
local itype = redis.call('TYPE', KEYS[3]).ok
if itype ~= 'none' and itype ~= 'hash' then
	return redis.error_reply('PLAYER_FIELDS field index wrong type: ' .. itype)
end
local ver = redis.call('GET', KEYS[2])
if ARGV[2] == '0' then
	if itype == 'none' then
		if ver ~= false then
			return -2
		end
	elseif ver == false or redis.call('HGET', KEYS[3], '__version') ~= ver then
		return -2
	end
end
local deleted = 0
local function del_own_string(key)
	if key == KEYS[1] or key == KEYS[2] or key == KEYS[3] then
		return
	end
	if redis.call('TYPE', key).ok == 'string' then
		deleted = deleted + redis.call('DEL', key)
	end
end
if itype == 'hash' then
	for _, name in ipairs(redis.call('HKEYS', KEYS[3])) do
		if name ~= '__version' then
			del_own_string(ARGV[3] .. name)
		end
	end
end
for i = 4, #KEYS do
	del_own_string(KEYS[i])
end
deleted = deleted + redis.call('DEL', KEYS[2])
redis.call('DEL', KEYS[3])
return deleted`

const (
	deleteFieldsLockLost int64 = -1
	deleteFieldsNeedScan int64 = -2
)

func deletePlayerFieldsAtomic(ctx context.Context, c goredis.Cmdable, playerID uint64, token string, candidates []string, legacy bool) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	keys := make([]string, 0, len(candidates)+3)
	keys = append(keys, routing.PlayerDataLockKey(playerID), versionKey(playerID), fieldIndexKey(playerID))
	keys = append(keys, candidates...)
	mode := "0"
	if legacy {
		mode = "1"
	}
	return c.Eval(ctx, deletePlayerFieldsScript, keys, token, mode, playerKeyPrefix(playerID)).Int64()
}
