package dataloader

import (
	"context"
	"errors"
	"fmt"
	"login/internal/config"
	componentpb "proto/common/component"
	databasepb "proto/common/database"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strconv"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// 父键、owner 与屏障必须位于同一 Redis 实例；既有键名不支持 Cluster 多键脚本。
// 读到的状态只约束本次组装，提交时还要原子复核，不能把预检当提交点。
//
// 加载分三条路(docs/design/zone-redis-regression-guard-20261022.md §4.1，由 route 决定):
//   - 快路径:父键在、屏障已提交、身份戳等于本实例身份 → 直接用 Redis 父键;
//   - 等待:身份可信而屏障在途 → 返回可重试错误，等补投追平;
//   - 核验:父键缺失(冷加载)、身份变了、ACK 高于 pending、历史缺口 → 读两张 SQL 行，
//     按 savebarrier.Reconcile 判定后由 commit 脚本原子提交。
//
// Redis 是加载真源，但它会因崩溃丢尾、切主丢尾、从备份恢复而回退；回退后的副本内容旧而 epoch 低，
// 新持有者合法地铸出更大的 epoch 后，第一次存盘会越过 SQL 行围栏把更新的 SQL 行整行覆盖。
// 铸造地板防不住这一条，只能在这里拦:实例身份变过的副本必须先和已提交的 SQL 对齐才能用。
type parentLoadSnapshot struct {
	owner     string
	pending   string
	ackFirst  string // 屏障 hash 的 player_database 字段(该表的 SQL ACK)
	ackSecond string // 屏障 hash 的 player_database_1 字段
	outbox    string // 屏障 hash 的 outbox_version，只用于提交时原子复核
	raw       string
	state     savebarrier.BarrierState
	// schemaGen 是屏障 hash 里 scene 记录的持久 schema 世代原文(C4 用),hasSchemaGen 表示该字段是否存在。
	// 两者分开:字段存在但值为空串是损坏(与 scene C3、go/db save_barrier_restore 同一口径),不能当成"不存在"放过。
	// 只是索引不是权威(设计 §3.3),提交时不复核它。
	schemaGen    string
	hasSchemaGen bool
	// stamp 是屏障里的身份戳(savebarrier.VerifiedIdentityField);identity / role 是本次脚本所在实例的身份，
	// 与 stamp 在同一段 Lua 里读出，中间没有窗口。identity 为空 = 身份未知，按未核验处理。
	stamp    string
	identity string
	role     string
	// historyGap 非空:Redis 没有提交屏障，却有存档历史的迹象(owner 已铸造，或父快照不是建角种子)。
	// 回退越过首次存盘、屏障整键丢失都会是这个样子。只有 SQL 两行可采纳时放行，否则原样返回它。
	historyGap error
	// unsavedNewPlayer 仅保留首存前失败的候选形状供诊断，不能证明玩家从未存盘。
	// 缺少持久 birth 原件时，两张 SQL 行也缺失仍可能是历史丢失，必须继续拒绝 historyGap。
	unsavedNewPlayer bool
}

// errParentBarrierNotCommitted:最近一次存盘还没有两张表都落 SQL。可重试，补投会追平。
var errParentBarrierNotCommitted = errors.New("parent SQL barrier not committed")

// parentLoadScripts 是父快照加载链上要读实例身份的脚本。身份的 Lua 片段作为参数注入:生产固定用
// savebarrier.LuaInstanceIdentity(defaultParentLoadScripts);miniredis 不支持 INFO server / replication，
// 单测经 newParentLoadScripts 换成从普通键读身份的替身。
type parentLoadScripts struct {
	begin         *redis.Script
	commit        *redis.Script
	seed          *redis.Script // 建角写种子并写身份戳(new_player_seed.go)
	committedSeed *redis.Script // 只接收正式 SQL 初始提交回执。
}

var defaultParentLoadScripts = newParentLoadScripts(savebarrier.LuaInstanceIdentity)

func newParentLoadScripts(instanceIdentityLua string) parentLoadScripts {
	return parentLoadScripts{
		begin:         redis.NewScript(instanceIdentityLua + parentLoadBeginLua),
		commit:        redis.NewScript(parentLoadCommitLua),
		seed:          redis.NewScript(instanceIdentityLua + seedNewPlayerLua),
		committedSeed: redis.NewScript(instanceIdentityLua + committedPlayerSeedLua),
	}
}

// parentLoadBeginLua 只读，一次取回加载判定要用的全部状态。版本串是否规范由 Go 侧判(savebarrier.ClassifyBarrier)。
//
//	KEYS = 父键 / owner / 屏障 hash
//	返回 11 个字符串:父键原文、owner、pending、两表 ACK、outbox_version、schema_gen 原文与存在标记('1' / '0')、
//	身份戳、本实例 identity、role。缺失的键或字段一律为空串。
//
// 身份与身份戳在同一段脚本里读，检查与使用之间没有窗口(实例换了，戳与身份必然对不上)。
// schema_gen 的存在标记把「字段存在但值为空串」(损坏)与「字段不存在」(不比较)分开，供 C4 用。
const parentLoadBeginLua = `
local function key_type(key, expected)
    local t = redis.call('TYPE', key).ok
    return t == 'none' or t == expected
end
if not key_type(KEYS[1], 'string') or not key_type(KEYS[2], 'string')
    or not key_type(KEYS[3], 'hash') then
    return redis.error_reply('parent load key type mismatch')
end
local identity, role = instance_identity()
local b = redis.call('HMGET', KEYS[3], 'pending', 'player_database', 'player_database_1',
    'outbox_version', 'schema_gen', '` + savebarrier.VerifiedIdentityField + `')
return {redis.call('GET', KEYS[1]) or '', redis.call('GET', KEYS[2]) or '',
    b[1] or '', b[2] or '', b[3] or '', b[4] or '', b[5] or '', b[5] and '1' or '0',
    b[6] or '', identity, role}
`

// parentLoadCommitLua 是核验路径唯一的提交点。先原子复核核验时读到的 owner、pending、两表 ACK、outbox_version
// 原文都没变(变了说明期间有存盘或 go/db 发布，回错误、一个键都不写)，再按模式写入:
//
//	stamp:SQL 与 Redis 一致。父键缺失时补上 SQL 组装的父键(冷加载);写身份戳。
//	adopt:SQL 严格更新(Redis 回退过)。用 SQL 组装的父键覆盖;pending 与两表 ACK 改为 SQL 版本;
//	       清掉更旧版本的补投意图;owner 只升不降地抬到 SQL 版本的 epoch(加载要求 owner ≥ pending 的代次)。
//	       只抬到这个值、不加余量:仍缓存着更高 epoch 的持有节点不会因为这次采纳被额外废黜。
//
//	KEYS = 父键 / owner / 屏障 hash
//	ARGV = 模式 / 核验时的 owner、pending、ACK1、ACK2、outbox_version 原文 / 身份戳(空串 = 不写)
//	       / 父快照字节(空串 = 不写) / 父键 TTL 毫秒 / 采纳版本 / 采纳版本的 owner_epoch(十进制规范串)
//	返回:stamp 模式 1 = 补写了父键、0 = 父键已在;adopt 模式 1。
//
// 身份戳写的是核验时读到的身份。提交时实例若已经换了(又一次回退)，写进去的旧身份与新实例对不上，
// 下次登录照样重新核验，所以这里不必再读一次 INFO。没有存档历史的玩家写戳会建出只有这一个字段的屏障 hash:
// 所有读者都把缺失的 pending / ACK 当作没有历史，与不建 hash 等价。
// 清掉的 9 个补投字段与 go/db 判「两表已提交」时清的是同一组(go/db/internal/saveoutbox/contract.go);
// 不清的话 C++ 严格屏障 Lua 会因 outbox_version ≠ pending 拒绝下一次存盘。补投索引 ZSET 的成员留给
// 补投认领脚本，它见到两表 ACK 等于 pending 会自己清理。
const parentLoadCommitLua = `
local function key_type(key, expected)
    local t = redis.call('TYPE', key).ok
    return t == 'none' or t == expected
end
local function dec_lt(a, b)
    if #a ~= #b then return #a < #b end
    for i = 1, #a do
        local x, y = string.byte(a, i), string.byte(b, i)
        if x ~= y then return x < y end
    end
    return false
end
local mode = ARGV[1]
if (mode ~= 'stamp' and mode ~= 'adopt') or not tonumber(ARGV[9]) or tonumber(ARGV[9]) <= 0
    or (mode == 'adopt' and (#ARGV[8] == 0 or #ARGV[10] ~= 41 or not string.match(ARGV[11], '^[1-9]%d*$'))) then
    return redis.error_reply('parent load commit argument')
end
if not key_type(KEYS[1], 'string') or not key_type(KEYS[2], 'string') or not key_type(KEYS[3], 'hash') then
    return redis.error_reply('parent load key type mismatch')
end
local b = redis.call('HMGET', KEYS[3], 'pending', 'player_database', 'player_database_1', 'outbox_version')
if (redis.call('GET', KEYS[2]) or '') ~= ARGV[2] or (b[1] or '') ~= ARGV[3] or (b[2] or '') ~= ARGV[4]
    or (b[3] or '') ~= ARGV[5] or (b[4] or '') ~= ARGV[6] then
    return redis.error_reply('parent ownership or save barrier changed during SQL verification')
end
local written = 0
if mode == 'adopt' then
    redis.call('SET', KEYS[1], ARGV[8], 'PX', ARGV[9])
    redis.call('HSET', KEYS[3], 'pending', ARGV[10], 'player_database', ARGV[10], 'player_database_1', ARGV[10])
    redis.call('HDEL', KEYS[3], 'outbox_version', 'outbox_topic', 'outbox_player_database', 'outbox_player_database_1',
        'outbox_attempt_version', 'outbox_attempts', 'outbox_terminal', 'outbox_terminal_reason', 'outbox_retry_ms')
    local owner = redis.call('GET', KEYS[2])
    if not owner or dec_lt(owner, ARGV[11]) then redis.call('SET', KEYS[2], ARGV[11]) end
    written = 1
elseif ARGV[8] ~= '' and redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], ARGV[8], 'PX', ARGV[9])
    written = 1
end
if ARGV[7] ~= '' then redis.call('HSET', KEYS[3], '` + savebarrier.VerifiedIdentityField + `', ARGV[7]) end
return written
`

func parentBarrierKeys(parentKey string, playerID uint64) []string {
	return []string{parentKey, ownerepoch.OwnerEpochKey(playerID), savebarrier.Key(playerID)}
}

// beginParentLoad 读父快照、提交屏障与实例身份，并做全部只读校验。regressionMode 必须是
// config.ParsePersistSchemaRegressionMode 归一化之后的值,只决定 C4 判定倒退时放行还是返回错误。
// 屏障在途、ACK 高于 pending、历史缺口都不在这里报错:它们记在快照上，由 route 决定等待还是核验。
func beginParentLoad(ctx context.Context, rc redis.Cmdable, scripts parentLoadScripts, parentKey string, playerID uint64,
	regressionMode config.PersistSchemaRegressionMode) (parentLoadSnapshot, error) {
	var snapshot parentLoadSnapshot
	values, err := scripts.begin.Run(ctx, rc, parentBarrierKeys(parentKey, playerID)).StringSlice()
	if err != nil {
		return snapshot, fmt.Errorf("读取父快照提交屏障: %w", err)
	}
	if len(values) != 11 || (values[7] != "0" && values[7] != "1") {
		return snapshot, fmt.Errorf("父快照屏障回复长度错误")
	}
	snapshot = parentLoadSnapshot{raw: values[0], owner: values[1], pending: values[2], ackFirst: values[3],
		ackSecond: values[4], outbox: values[5], schemaGen: values[6], hasSchemaGen: values[7] == "1",
		stamp: values[8], identity: values[9], role: values[10]}
	if snapshot.state, err = savebarrier.ClassifyBarrier(snapshot.pending, snapshot.ackFirst, snapshot.ackSecond); err != nil {
		return snapshot, fmt.Errorf("父快照提交屏障畸形: %w", err)
	}
	ownerEpoch, err := ownerepoch.ParseEpoch(snapshot.owner)
	if err != nil || (snapshot.owner != "" && strconv.FormatUint(ownerEpoch, 10) != snapshot.owner) {
		return snapshot, fmt.Errorf("父快照归属计数器不规范")
	}
	if snapshot.pending != "" {
		epoch, seq, err := savebarrier.Parse(snapshot.pending)
		if err != nil || epoch == 0 || seq == 0 || ownerEpoch < epoch {
			return snapshot, fmt.Errorf("父快照归属计数器缺失或落后于提交屏障")
		}
	}
	if snapshot.raw != "" {
		parent := new(databasepb.PlayerAllData)
		if err := proto.Unmarshal([]byte(snapshot.raw), parent); err != nil {
			return snapshot, fmt.Errorf("父快照解析失败: %w", err)
		}
		if parent.GetPlayerDatabaseData().GetPlayerId() != playerID || parent.GetPlayerDatabase_1Data().GetPlayerId() != playerID {
			return snapshot, fmt.Errorf("父快照玩家身份不符")
		}
		if snapshot.pending == "" {
			// 首次身份补齐与铸造owner之间可断线；只允许身份字段，不猜测任何历史资产的提交上界。
			// 不是种子就是历史缺口:只有 SQL 两行可采纳时放行(settle),C4 留给采纳出来的 SQL 父快照。
			if seed := isUnownedLoginSeed(parent, playerID); ownerEpoch != 0 || !seed {
				snapshot.historyGap = errors.New("历史父快照缺少提交屏障，需要停写恢复或迁移")
				snapshot.unsavedNewPlayer = seed
				return snapshot, nil
			}
		} else {
			_, seq, _ := savebarrier.Parse(snapshot.pending)
			if parent.GetLastSaveSeq() != seq {
				return snapshot, fmt.Errorf("父快照水位与提交屏障不符")
			}
		}
		// C4 放在身份、水位、种子校验之后,只判快路径:冷加载要求两表 SQL ACK 等于 pending,
		// 行戳必然 ≥ 屏障记录的世代(设计 §5.3、§5.4),不需要也不应该在这里判。
		if err := checkParentSchemaRegression(parent, snapshot.schemaGen, snapshot.hasSchemaGen, playerID, regressionMode); err != nil {
			return snapshot, err
		}
	} else if snapshot.pending == "" && ownerEpoch != 0 {
		snapshot.historyGap = errors.New("历史玩家父快照与提交屏障缺失，需要停写恢复")
		snapshot.unsavedNewPlayer = true
	}
	return snapshot, nil
}

type parentLoadRoute uint8

const (
	parentLoadFast   parentLoadRoute = iota + 1 // 直接用 Redis 父键
	parentLoadWait                              // 返回 errParentBarrierNotCommitted，等补投追平
	parentLoadVerify                            // 读两张 SQL 行，按 settle 判定并提交
)

// 进核验路径的原因，供日志与排查用(指标只按结论分，见 countParentLoadVerify)。
const (
	verifyTriggerCold       = "cold"        // 父键缺失
	verifyTriggerIdentity   = "identity"    // 身份戳不等于本实例身份(含身份未知)
	verifyTriggerAckAhead   = "ack_ahead"   // ACK 高于 pending:Redis 回退的证据
	verifyTriggerHistoryGap = "history_gap" // 屏障缺失而有存档历史的迹象
)

// identityVerified:本实例身份已知、是主，且屏障里的身份戳就是它 —— 自上次核验以来这份副本没有丢过已确认的写。
func (snapshot parentLoadSnapshot) identityVerified() bool {
	return snapshot.identity != "" && snapshot.role == "master" && snapshot.stamp == snapshot.identity
}

// stampToWrite 是核验提交时要写的身份戳。身份未知或不是主时不写，下次登录再核验。
func (snapshot parentLoadSnapshot) stampToWrite() string {
	if snapshot.identity == "" || snapshot.role != "master" {
		return ""
	}
	return snapshot.identity
}

// route 决定走哪条路，返回值的第二项是核验的原因(其余路为空串)。
// verifyOnIdentityChange 关闭时身份一律视为可信，只用于冷读风暴时止血(设计 §6);
// ACK 高于 pending 与历史缺口不受开关影响:它们本身就是回退证据，不核验就只能永久拒登。
func (snapshot parentLoadSnapshot) route(verifyOnIdentityChange bool) (parentLoadRoute, string) {
	switch {
	case snapshot.historyGap != nil:
		return parentLoadVerify, verifyTriggerHistoryGap
	case snapshot.pending == "":
		// 旧 LastSeq0 缓存种子不是持久基底；即使身份戳相同或止血开关关闭也必须强读 SQL。
		return parentLoadVerify, verifyTriggerCold
	case snapshot.state == savebarrier.BarrierAckAhead:
		return parentLoadVerify, verifyTriggerAckAhead
	case verifyOnIdentityChange && !snapshot.identityVerified():
		// 身份变了时在途的屏障也要核验:回退后的意图版本可能低于 SQL，补投只会被判旧、永远追不平。
		return parentLoadVerify, verifyTriggerIdentity
	case snapshot.state == savebarrier.BarrierInflight:
		return parentLoadWait, ""
	case snapshot.raw == "":
		return parentLoadVerify, verifyTriggerCold
	default:
		return parentLoadFast, ""
	}
}

// checkParentSchemaRegression 是 C4(设计 §5.1):父快照的加载世代低于屏障记录的 schema_gen,
// 或 schema_gen 存在但不是规范的十进制 uint32,都算倒退证据(数据可能已被不认识新字段的旧写方截断)。
// 屏障里没有 schema_gen 字段(机制前的存量玩家、merge_zone 删过 hash、新号种子)一律不比较(设计 §3.3);
// 字段存在但为空串属于损坏,照常判定。
// 两种模式都计数并打日志;只有 enforce 返回错误,走 EnterGame 现有失败路径(固定 kEnterSceneFailed,
// 原因不外发给客户端,不新增 tip)。
func checkParentSchemaRegression(parent *databasepb.PlayerAllData, barrierGen string, hasBarrierGen bool,
	playerID uint64, mode config.PersistSchemaRegressionMode) error {
	if !hasBarrierGen {
		return nil
	}
	loaded := loadedSchemaGen(parent)
	recorded, ok := parseBarrierSchemaGen(barrierGen)
	var reason string
	switch {
	case !ok:
		reason = schemaRegressionReasonMalformed
	case loaded < recorded:
		reason = schemaRegressionReasonRegression
	default:
		return nil
	}
	enforce := mode == config.PersistSchemaRegressionEnforce
	// label 只取两个字面量:调用方传入的是归一化后的模式,这里再收口一次,保证序列集合封闭。
	modeLabel, action := string(config.PersistSchemaRegressionWarn), "admitted"
	if enforce {
		modeLabel, action = string(config.PersistSchemaRegressionEnforce), "rejected"
	}
	persistSchemaRegressionTotal.Inc(reason, modeLabel)
	// go-zero logx 没有 WARN 级别,沿用本服务 "WARNING" 前缀 + Errorf 的写法。
	// barrierGen 可能是损坏的任意字节,%.32q 截断并转义,不让脏值撑爆日志行。
	logx.Errorf("WARNING persist schema regression on parent snapshot fast path: playerId=%d reason=%s "+
		"loadedGen=%d barrierGen=%.32q mode=%s action=%s", playerID, reason, loaded, barrierGen, modeLabel, action)
	if !enforce {
		return nil
	}
	return fmt.Errorf("父快照持久 schema 世代倒退(%s):加载世代 %d,屏障记录 %.32q", reason, loaded, barrierGen)
}

// loadedSchemaGen 是父快照的加载世代:两张表戳取最小值,未盖戳的 0 按 1(设计 §3.3)。
// 取最小值是为了不让一张表被截断的事实被另一张表的高戳洗白。
func loadedSchemaGen(parent *databasepb.PlayerAllData) uint32 {
	return min(effectiveSchemaGen(parent.GetPlayerDatabaseData().GetPersistSchemaGen()),
		effectiveSchemaGen(parent.GetPlayerDatabase_1Data().GetPersistSchemaGen()))
}

func effectiveSchemaGen(raw uint32) uint32 {
	if raw == 0 {
		return 1
	}
	return raw
}

// parseBarrierSchemaGen 只认 scene 写入的规范形式:十进制、>= 1、无符号、无前导零、不超过 uint32。
// 与 go/db save_barrier_restore 脚本的 is_schema_gen 同一口径;不规范即视为损坏。
func parseBarrierSchemaGen(raw string) (uint32, bool) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != raw {
		return 0, false
	}
	return uint32(v), true
}

// backfillPlayerIdentity在首次owner之前只补这四个身份字段。清除白名单后必须逐字段等于
// 建角种子，未知字段也参与比较；不得把注册记录、货币或其他旧档数据当成可重建的新号。
// clone只用于只读校验，不能把清除后的副本回写覆盖真实身份。
func isUnownedLoginSeed(parent *databasepb.PlayerAllData, playerID uint64) bool {
	if parent == nil {
		return false
	}
	seed := proto.Clone(parent).(*databasepb.PlayerAllData)
	row := seed.GetPlayerDatabaseData()
	if row == nil {
		return false
	}
	if class := row.GetUint32PbComponent(); class != nil {
		class.Class = 0
		if proto.Equal(class, &componentpb.PlayerUint32Comp{}) {
			row.Uint32PbComponent = nil
		}
	}
	if profile := row.GetProfileComponent(); profile != nil {
		profile.Name, profile.AppearanceId, profile.Gender = "", "", 0
		if proto.Equal(profile, &componentpb.PlayerProfileComp{}) {
			row.ProfileComponent = nil
		}
	}
	return proto.Equal(seed, newPlayerAllDataSkeleton(playerID))
}

// fencedSQLRow 是两张玩家表共有的读接口。
type fencedSQLRow interface {
	GetPlayerId() uint64
	GetFenceOwnerEpoch() uint64
	GetFenceSaveSeq() uint64
}

// validateSQLRowIdentity 只接受真正的 SQL 回包；核验不采信可能落后一代的 Redis 子表缓存。
// 版本关系不在这里判，统一交给 settle 里的 savebarrier.Reconcile。
func validateSQLRowIdentity(sub proto.Message, playerID uint64) error {
	row, ok := sub.(fencedSQLRow)
	if !ok || row.GetPlayerId() != playerID {
		return fmt.Errorf("冷恢复 SQL 子表身份或类型不符")
	}
	return nil
}

func sqlRowVersion(row fencedSQLRow) string {
	return savebarrier.RowVersion(row.GetFenceOwnerEpoch(), row.GetFenceSaveSeq())
}

// requireNoSQLHistory:Redis 一侧没有存盘历史时，SQL 两行除 player_id 外必须为空。
// 零围栏却有数据的行是围栏之前的历史资产，不能当成新号。
func requireNoSQLHistory(sqlParent *databasepb.PlayerAllData, playerID uint64) error {
	for _, sub := range []proto.Message{sqlParent.GetPlayerDatabaseData(), sqlParent.GetPlayerDatabase_1Data()} {
		empty := sub.ProtoReflect().Type().New()
		empty.Set(empty.Descriptor().Fields().ByName("player_id"), protoreflect.ValueOfUint64(playerID))
		if !proto.Equal(sub, empty.Interface()) {
			return fmt.Errorf("SQL 存在历史数据但缺少提交屏障，需要停写迁移")
		}
	}
	return nil
}

// settle 是核验路径的收尾:把两张 SQL 行(已组装进 sqlParent)与核验时读到的屏障按 savebarrier.Reconcile 判定，
// 再经 commit 脚本原子提交。返回 nil 表示父键已就绪(沿用 Redis 父键，或已写入 SQL 组装的父键);
// 返回的 Verdict 只用于计数，判定之前就失败时为 0。
func (snapshot parentLoadSnapshot) settle(ctx context.Context, rc redis.Cmdable, scripts parentLoadScripts, parentKey string,
	playerID uint64, sqlParent *databasepb.PlayerAllData, regressionMode config.PersistSchemaRegressionMode) (verdict savebarrier.Verdict, err error) {
	defer func() { countParentLoadVerify(verdict, err) }()
	sqlVersion := sqlRowVersion(sqlParent.GetPlayerDatabaseData())
	verdict, err = savebarrier.Reconcile(savebarrier.Observation{
		Pending:   snapshot.pending,
		AckFirst:  snapshot.ackFirst,
		AckSecond: snapshot.ackSecond,
		SQLFirst:  sqlVersion,
		SQLSecond: sqlRowVersion(sqlParent.GetPlayerDatabase_1Data()),
	})
	if err != nil {
		return 0, err
	}
	switch verdict {
	case savebarrier.VerdictConsistent:
		if snapshot.pending == "" {
			if err := requireNoSQLHistory(sqlParent, playerID); err != nil {
				return verdict, err
			}
		}
		// 空 SQL 不能证明从未存盘；没有持久 birth 原件，首存候选形状仍必须按历史缺口拒绝。
		if snapshot.historyGap != nil {
			return verdict, snapshot.historyGap
		}
		var coldParent []byte
		if snapshot.raw == "" {
			if coldParent, err = marshalParentAt(sqlParent, snapshot.pending); err != nil {
				return verdict, err
			}
		}
		return verdict, snapshot.commit(ctx, rc, scripts, parentKey, playerID, commitStamp, coldParent, "")
	case savebarrier.VerdictAdoptSQL:
		// 冷加载之所以不判 C4,是因为它要求两表 ACK 等于 pending;采纳的 SQL 新于屏障，要照快路径判一次。
		if err := checkParentSchemaRegression(sqlParent, snapshot.schemaGen, snapshot.hasSchemaGen, playerID, regressionMode); err != nil {
			return verdict, err
		}
		adopted, err := marshalParentAt(sqlParent, sqlVersion)
		if err != nil {
			return verdict, err
		}
		// go-zero logx 没有 WARNING 级别,沿用本服务 "WARNING" 前缀 + Errorf 的写法。采纳本身是自愈，
		// 但每一次都说明 Redis 丢过已确认的写，要能按玩家追查。
		logx.Errorf("WARNING PARENT-LOAD-ADOPT-SQL playerId=%d pending=%q ack=%q/%q sql=%s",
			playerID, snapshot.pending, snapshot.ackFirst, snapshot.ackSecond, sqlVersion)
		return verdict, snapshot.commit(ctx, rc, scripts, parentKey, playerID, commitAdopt, adopted, sqlVersion)
	case savebarrier.VerdictInflight:
		return verdict, errParentBarrierNotCommitted
	case savebarrier.VerdictReconciling:
		return verdict, fmt.Errorf("SQL 两张表停在不同版本，等落后的那张表追平")
	default:
		logx.Errorf("PARENT-LOAD-SQL-BEHIND playerId=%d pending=%q ack=%q/%q sql=%q/%q",
			playerID, snapshot.pending, snapshot.ackFirst, snapshot.ackSecond, sqlVersion,
			sqlRowVersion(sqlParent.GetPlayerDatabase_1Data()))
		return verdict, fmt.Errorf("SQL 行低于提交屏障的 ACK，疑似 SQL 回档，需要人工处置")
	}
}

// marshalParentAt 把父快照的 last_save_seq 对齐到 version 的 save_seq 后序列化;version 为空(没有历史)时不动。
func marshalParentAt(parent *databasepb.PlayerAllData, version string) ([]byte, error) {
	if version != "" {
		_, seq, err := savebarrier.Parse(version)
		if err != nil {
			return nil, err
		}
		parent.LastSaveSeq = seq
	}
	return proto.Marshal(parent)
}

type commitMode string

const (
	commitStamp commitMode = "stamp"
	commitAdopt commitMode = "adopt"
)

// commit 调 parentLoadCommitLua。parent 为空时不写父键;version 只在 adopt 模式下有值。
func (snapshot parentLoadSnapshot) commit(ctx context.Context, rc redis.Cmdable, scripts parentLoadScripts, parentKey string,
	playerID uint64, mode commitMode, parent []byte, version string) error {
	adoptEpoch := ""
	if mode == commitAdopt {
		epoch, _, err := savebarrier.Parse(version)
		if err != nil {
			return err
		}
		adoptEpoch = strconv.FormatUint(epoch, 10)
	}
	_, err := scripts.commit.Run(ctx, rc, parentBarrierKeys(parentKey, playerID), string(mode),
		snapshot.owner, snapshot.pending, snapshot.ackFirst, snapshot.ackSecond, snapshot.outbox,
		snapshot.stampToWrite(), parent, playerAllDataParentTTL.Milliseconds(), version, adoptEpoch).Int()
	return err
}
