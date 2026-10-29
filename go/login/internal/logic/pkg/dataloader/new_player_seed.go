package dataloader

import (
	"context"
	"fmt"
	login_proto_database "proto/common/database"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strconv"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// committedPlayerSeedLua 只发布本次 SQL 严格新建的完整提交；所有检查先于任何写入。
// 较高 owner、既有非初始父键或已推进屏障一律拒写，不以缓存优化失败补偿删除已提交角色。
const committedPlayerSeedLua = `
local owner = redis.call('GET', KEYS[2])
local parent = redis.call('GET', KEYS[1])
local b = redis.call('HMGET', KEYS[3], 'pending', 'player_database', 'player_database_1', 'outbox_version')
local complete = owner == ARGV[4] and parent == ARGV[1] and b[1] == ARGV[3]
    and b[2] == ARGV[3] and b[3] == ARGV[3] and b[4] == false
    and redis.call('HGET', KEYS[3], 'schema_gen') == ARGV[5]
if complete then
    local allowed = {pending=true, player_database=true, player_database_1=true, schema_gen=true,
        ['` + savebarrier.VerifiedIdentityField + `']=true}
    for _, field in ipairs(redis.call('HKEYS', KEYS[3])) do
        if not allowed[field] then return redis.error_reply('initial SQL seed has extra barrier state') end
    end
    return 2
end
if owner ~= false then return -1 end
if parent ~= false then return 0 end
if redis.call('EXISTS', KEYS[3]) ~= 0 then
    return redis.error_reply('initial SQL seed barrier already exists')
end
for i = 1, 4 do
    if b[i] ~= false then return redis.error_reply('initial SQL seed barrier already exists') end
end
local identity, role = instance_identity()
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('SET', KEYS[2], ARGV[4])
redis.call('HSET', KEYS[3], 'pending', ARGV[3], 'player_database', ARGV[3],
    'player_database_1', ARGV[3], 'schema_gen', ARGV[5])
if identity ~= '' and role == 'master' then
    redis.call('HSET', KEYS[3], '` + savebarrier.VerifiedIdentityField + `', identity)
end
return 1
`

// SeedCommittedNewPlayerAllData 只接受正式 Create 成功回执的完整初始快照；不得自行按 ID 构造调用。
func SeedCommittedNewPlayerAllData(ctx context.Context, rc *redis.Client, parent *login_proto_database.PlayerAllData) (SeedResult, error) {
	return seedCommittedNewPlayerAllData(ctx, rc, defaultParentLoadScripts, parent)
}

func seedCommittedNewPlayerAllData(ctx context.Context, rc *redis.Client, scripts parentLoadScripts, parent *login_proto_database.PlayerAllData) (SeedResult, error) {
	if rc == nil || !savebarrier.IsInitialPlayerData(parent) {
		return SeedError, fmt.Errorf("缺少本次已提交的玩家 SQL 初始原件")
	}
	data, err := proto.Marshal(parent)
	if err != nil {
		return SeedError, err
	}
	id := parent.GetPlayerDatabaseData().GetPlayerId()
	res, err := scripts.committedSeed.Run(ctx, rc, parentBarrierKeys(buildParentKey(parent, id), id), data,
		playerAllDataParentTTL.Milliseconds(), savebarrier.Version(savebarrier.InitialOwnerEpoch, savebarrier.InitialSaveSeq),
		strconv.FormatUint(savebarrier.InitialOwnerEpoch, 10), parent.GetPlayerDatabaseData().GetPersistSchemaGen()).Int()
	if err != nil {
		return SeedError, err
	}
	switch res {
	case 1:
		return SeedWritten, nil
	case 2:
		return SeedIdempotent, nil
	case -1:
		return SeedOwnerEpochExists, nil
	case 0:
		return SeedParentExists, nil
	default:
		return SeedError, fmt.Errorf("SQL 初始种子回复非法")
	}
}

// newPlayerAllDataSkeleton 仅是两行 SQL 解码的目标容器，不证明任一行存在。
// 正式 Ensure 必须收到两份非空 Data 后才能发布；旧缓存夹具仍用它表达零序号种子。
// 此处不加身份或 registration_timestamp，身份仍由 backfill 和 scene 正式首次注册负责。
func newPlayerAllDataSkeleton(playerId uint64) *login_proto_database.PlayerAllData {
	return &login_proto_database.PlayerAllData{
		PlayerDatabaseData:   &login_proto_database.PlayerDatabase{PlayerId: playerId},
		PlayerDatabase_1Data: &login_proto_database.PlayerDatabase_1{PlayerId: playerId},
	}
}

// SeedResult 是 SeedNewPlayerAllData 的结论。零值 SeedError:漏判的分支落在最保守的「不知道写没写」上
// (写了也无害,见 seedNewPlayerLua)。
type SeedResult uint8

const (
	SeedError            SeedResult = iota // Redis 出错 / 回复不认识:种子是否落地未知
	SeedWritten                            // 父键原本不存在,已写入种子
	SeedIdempotent                         // 父键已是这份种子:同一条 EVAL 被 go-redis 重发,或这个 id 以前写过种子(成因见 seedNewPlayerLua)
	SeedParentExists                       // 父键已存在且不是这份种子:未写
	SeedOwnerEpochExists                   // player:{id}:owner_epoch 已存在(玩家被派入过场景):未写
)

// String 返回指标 label 值(login_create_player_seed_total{result}),集合封闭。
func (r SeedResult) String() string {
	switch r {
	case SeedWritten:
		return "written"
	case SeedIdempotent:
		return "idempotent"
	case SeedParentExists:
		return "parent_exists"
	case SeedOwnerEpochExists:
		return "owner_epoch_exists"
	default:
		return "error"
	}
}

// seedNewPlayerLua 写新号种子(parentLoadScripts.seed,前面拼实例身份片段)。
// KEYS = {PlayerAllData 父键, player:{id}:owner_epoch, 提交屏障 hash};ARGV = {种子字节, TTL 毫秒}。
//
// 只在两件事同时成立时写:
//   - owner_epoch 键不存在。scene_manager 每次把玩家派入场景都会 INCR 它、从不删除(shared/ownerepoch 包注释;C++ redis_client.h kSaveIfGuardLuaScript 的注释)。
//     它在 = 这名玩家进过场景,scene 的存档才是真相,种子绝不能落。这一条兜住「种子请求在网络里滞留,
//     落地时玩家早已入场并存过盘、父键又恰好被删(合服失效缓存)」这类交错;
//   - 父键不存在。存在就不覆盖;若它恰好就是这份种子,回 2 算成功。回 2 有两种成因:go-redis 把已在服务端执行过的
//     EVAL 又重发了一次;或者这个 id 以前铸出过、当时写的种子还在(id 复用)。建角调用点套了 1s 的 ctx 预算,
//     读超时(3s)之后的重发到不了,只剩连接层快速失败(EOF / 连接被重置)后的重发,所以 idempotent 稳态
//     应接近 0,持续出现要按 id 复用排查。
//
// 写入带 PX,与冷读组装的父键同一 TTL(playerAllDataParentTTL);scene 首次存盘的裸 SET 会去掉 TTL。
// 三把键必须在同一实例:C++ 存盘的严格屏障脚本本来就同时访问它们(redis_client.h kSaveWithCommitBarrierLuaScript)。
//
// 写种子的同时给屏障写上本实例的身份戳(savebarrier.VerifiedIdentityField):刚铸出的 id 在任何库里都没有行，
// 种子就是真值，等于已经核验过。首次 EnterGame 只要实例身份没变就走快路径，不因身份戳缺失去读 SQL
// (docs/design/zone-redis-regression-guard-20261022.md §4.1)。屏障 hash 已存在(id 复用)、身份未知或不是主时不写戳，
// 首次登录照常核验。
//
// 返回:1 已写;2 已是这份种子;0 父键已存在且不同;-1 owner_epoch 已存在。
const seedNewPlayerLua = `
if redis.call("EXISTS", KEYS[2]) == 1 then
    return -1
end
local cur = redis.call("GET", KEYS[1])
if cur == false then
    redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
    local identity, role = instance_identity()
    if identity ~= '' and role == 'master' and redis.call("EXISTS", KEYS[3]) == 0 then
        redis.call("HSET", KEYS[3], "` + savebarrier.VerifiedIdentityField + `", identity)
    end
    return 1
end
if cur == ARGV[1] then
    return 2
end
return 0
`

// SeedNewPlayerAllData 保留旧缓存格式以验证兼容与历史缺口，正式 Create 不再调用。
// 该零序号缓存不能走新可信快路，Ensure 必须强读两张 SQL 行；建角使用提交后能力 SeedCommittedNewPlayerAllData。
func SeedNewPlayerAllData(ctx context.Context, rc *redis.Client, playerId uint64) (SeedResult, error) {
	return seedNewPlayerAllData(ctx, rc, defaultParentLoadScripts, playerId)
}

// seedNewPlayerAllData 与 SeedNewPlayerAllData 相同，脚本显式传入(单测换实例身份替身)。
func seedNewPlayerAllData(ctx context.Context, rc *redis.Client, scripts parentLoadScripts, playerId uint64) (SeedResult, error) {
	if rc == nil {
		return SeedError, fmt.Errorf("seed new player %d: redis client not wired", playerId)
	}
	if playerId == 0 {
		return SeedError, fmt.Errorf("seed new player: player_id is 0")
	}
	parent := newPlayerAllDataSkeleton(playerId)
	data, err := proto.Marshal(parent)
	if err != nil {
		return SeedError, fmt.Errorf("marshal seed for player %d: %w", playerId, err)
	}
	// 屏障 hash 只是写身份戳的目标，不参与「已有状态」判定，所以不进 newPlayerStateKeys(探测与种子共用那两把)。
	keys := append(newPlayerStateKeys(playerId), savebarrier.Key(playerId))
	res, err := scripts.seed.Run(ctx, rc, keys, data, playerAllDataParentTTL.Milliseconds()).Int()
	if err != nil {
		return SeedError, fmt.Errorf("seed new player %d: %w", playerId, err)
	}
	switch res {
	case 1:
		return SeedWritten, nil
	case 2:
		return SeedIdempotent, nil
	case 0:
		return SeedParentExists, nil
	case -1:
		return SeedOwnerEpochExists, nil
	default:
		return SeedError, fmt.Errorf("seed new player %d: unexpected script result %d", playerId, res)
	}
}

// newPlayerStateKeys 返回判定「这个 id 上已有既有状态」的两把键:{PlayerAllData 父键, player:{id}:owner_epoch}。
// 种子脚本(写时的原子复核)与建角的只读探测(ProbeMintedPlayerState)共用它:两边判的必须是同一组键,
// 否则探测放行的 id 可能在种子那一步才被拦下,或者反过来。顺序即 seedNewPlayerLua 的 KEYS 顺序。
func newPlayerStateKeys(playerId uint64) []string {
	return []string{buildParentKey(&login_proto_database.PlayerAllData{}, playerId), ownerepoch.OwnerEpochKey(playerId)}
}

// MintedIDProbe 是 ProbeMintedPlayerState 的结论。零值 MintedIDProbeError:漏判的分支落在「探不清」上,
// 调用方一律按拒绝处理(fail-closed)。
type MintedIDProbe uint8

const (
	MintedIDProbeError       MintedIDProbe = iota // Redis 出错 / 入参不合法:探不清
	MintedIDClear                                 // 两把键都不在
	MintedIDParentExists                          // PlayerAllData 父键已存在
	MintedIDOwnerEpochExists                      // player:{id}:owner_epoch 已存在(两把都在时也归这一类,与种子脚本的判定顺序相同)
)

// String 返回指标 label 值(login_create_player_id_probe_total{result}),集合封闭。
// parent_exists / owner_epoch_exists 与 SeedResult 的同名取值含义相同,告警表达式可以共用一个正则。
func (r MintedIDProbe) String() string {
	switch r {
	case MintedIDClear:
		return "clear"
	case MintedIDParentExists:
		return "parent_exists"
	case MintedIDOwnerEpochExists:
		return "owner_epoch_exists"
	default:
		return "error"
	}
}

// ProbeMintedPlayerState 只读探测刚铸出的 playerId 上是否已有既有状态(newPlayerStateKeys 的两把键),一次往返。
//
// 建角在登记名字之前调用它(docs/design/new-player-first-enter-no-cold-read.md §10 P7,负责人 2026-10-15 定):
// 号段 id 不复用(I1),新铸 id 上本不该有任何状态;有,说明发号器把在役 id 又发了一次,或 id_segment 表被重置
// 而 Redis 没清,这个 id 不能交给新账号。与种子开关无关:关着种子时,id 复用同样会让新账号在首次入场时读到那份
// 旧缓存(父键在),或让旧玩家的进度被当成新号的(owner_epoch 在)。
//
// 它只是写之前的预检,不是提交点:探测与种子写之间仍有窗口,seedNewPlayerLua 的条件判断照旧保留,
// 作为写时的原子复核(建角侧对它报的 parent_exists / owner_epoch_exists 同样拒绝,设计 §10 P2)。
// 种子只在 NewPlayerSeed.Enabled 时写;提交点(建角第 7 步)不复核这两把键,残余窗口见设计 §13.2 2026-10-16 补注。
//
// 两条 EXISTS 走一个 pipeline:一次往返,并且分得清是哪把键在。两条之间不需要原子:任何一把在都拒绝。
// rc 必须与预加载、scene 读写父键是同一个 Redis 实例(同 SeedNewPlayerAllData)。本函数不写任何键。
//
// err 只在结论为 MintedIDProbeError 时非 nil。
func ProbeMintedPlayerState(ctx context.Context, rc *redis.Client, playerId uint64) (MintedIDProbe, error) {
	if rc == nil {
		return MintedIDProbeError, fmt.Errorf("probe minted player %d: redis client not wired", playerId)
	}
	if playerId == 0 {
		return MintedIDProbeError, fmt.Errorf("probe minted player: player_id is 0")
	}
	keys := newPlayerStateKeys(playerId)
	pipe := rc.Pipeline()
	parent := pipe.Exists(ctx, keys[0])
	ownerEpoch := pipe.Exists(ctx, keys[1])
	if _, err := pipe.Exec(ctx); err != nil {
		return MintedIDProbeError, fmt.Errorf("probe minted player %d: %w", playerId, err)
	}
	switch {
	case ownerEpoch.Val() > 0:
		return MintedIDOwnerEpochExists, nil
	case parent.Val() > 0:
		return MintedIDParentExists, nil
	default:
		return MintedIDClear, nil
	}
}
