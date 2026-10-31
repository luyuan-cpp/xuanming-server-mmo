package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	pb "proto/player_locator"
	"shared/playerredis"
	"shared/redisslot"
)

// 会话按 gate 实例建的派生索引(docs/design/player-locator-gate-session-index.md),按会话 pid 的 slot 分桶。
//
// 写路径在 session_cas.go 的三段会话 Lua 里与会话写入原子维护;本文件放索引专用的纯函数与
// 对账 / 补建用的守卫脚本。索引只是**提示**:任何依据索引动会话或摘成员的动作,都先按会话原始字节校验,
// 再用整字节 CAS 或字节守卫脚本提交,所以脏成员、漏成员、两个副本短暂重叠都不会误伤会话。

// GateSessionIndexCapabilityPrefix 是「本 locator 的会话写入按 slot 维护 gate 索引」的能力键前缀,
// 键为前缀 + NodeUuid,与 NodeInfo 同一个注册事务、同一个租约(node.WithLeaseBoundMarkers)。
// 对账 leader 比对「已注册的 locator」与「有能力键的 locator」,差集就是不维护这个形状的旧版写者。
//
// 不放在 PlayerLocatorNodeService.rpc/ 下面:C++ 把该前缀当作 zone 级发现前缀解析(node_util.cpp),
// 混进非 NodeInfo 值有解析风险。v1 是全局索引,v2 是按 slot 分桶;再改索引形状时再换前缀。
const GateSessionIndexCapabilityPrefix = "/mmorpg/capability/player_locator/gate_session_index/v2/"

// indexedGate 返回会话应当所在的 gate 索引:ONLINE 且 gate_instance_id 非空时为该 uuid,否则为 ""。
// 这是不变量 I1 的唯一定义处,写路径与对账都经过它。
func indexedGate(s *pb.PlayerSession) string {
	if s == nil || s.State != pb.PlayerSessionState_SESSION_STATE_ONLINE {
		return ""
	}
	return s.GateInstanceId
}

// gateIndexKeyOrPlaceholder 给脚本「这一侧的索引键」那个位置填值:gate 非空时是索引键;为空(这一侧没有索引)时填
// 本 slot 的登记集,只占位,脚本凭 ARGV 里的 gate 判断,不会碰它。
func gateIndexKeyOrPlaceholder(slot uint16, gate string) string {
	if gate == "" {
		return gateRegistryKey(slot)
	}
	return gateSessionIndexKey(slot, gate)
}

// gateSlot 指一把索引键:slot 上的 gate。清扫的处理单位。
type gateSlot struct {
	slot uint16
	gate string
}

// indexedAction 是对一个索引成员的判定。
type indexedAction int

const (
	// indexedOnlineHere:会话 ONLINE 且就在这个 gate 上 —— 成员是真的。死 gate 上要转 DISCONNECTING,
	// 活 gate 上原样保留。
	indexedOnlineHere indexedAction = iota
	// indexedStale:会话存在但已不在这个 gate 上 ONLINE(换 gate、断线中、旧写者漏摘)。按字节守卫摘除。
	indexedStale
	// indexedMissing:会话键不存在。按「仍不存在」守卫摘除。
	indexedMissing
	// indexedMalformed:会话字节解不开,或载荷里的 player_id 与成员不符。按字节守卫摘成员,会话原样不动。
	indexedMalformed
	// indexedBadMember:成员不是规范的十进制非零 player_id,对应不到任何会话键。直接摘除。
	indexedBadMember
)

// parseIndexMember 把索引成员解析成 player_id。只接受写路径写出的规范形式(strconv.FormatUint),
// "007"、"+7"、"0" 都判为坏成员:它们对应不到写路径会写的会话键。
func parseIndexMember(member string) (uint64, bool) {
	pid, err := strconv.ParseUint(member, 10, 64)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != member {
		return 0, false
	}
	return pid, true
}

// classifyIndexedMember 判定 gate 索引里的一个成员(纯函数)。raw / found 是该成员会话键的 MGET 结果。
// 返回的 session 只在 indexedOnlineHere 与 indexedStale 时非 nil。
func classifyIndexedMember(member string, raw []byte, found bool, gate string) (uint64, *pb.PlayerSession, indexedAction) {
	pid, ok := parseIndexMember(member)
	if !ok {
		return 0, nil, indexedBadMember
	}
	if !found {
		return pid, nil, indexedMissing
	}
	session := &pb.PlayerSession{}
	if err := proto.Unmarshal(raw, session); err != nil || session.PlayerId != pid {
		return pid, nil, indexedMalformed
	}
	if indexedGate(session) == gate {
		return pid, session, indexedOnlineHere
	}
	return pid, session, indexedStale
}

// pruneGateIndexMemberScript 只在会话仍是判定时看到的那一版时摘除成员:
// mode=present 要求会话字节 == expected;mode=missing 要求会话键仍不存在。
// 判定之后会话被改写(玩家重登回到这个 gate)时脚本返回 0,成员保留,由下一次判定重来。
// KEYS[1]=会话键,KEYS[2]=索引键;ARGV[1]=mode,ARGV[2]=expected,ARGV[3]=成员。
var pruneGateIndexMemberScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if ARGV[1] == "missing" then
    if current then return 0 end
else
    if (not current) or current ~= ARGV[2] then return 0 end
end
return redis.call("SREM", KEYS[2], ARGV[3])
`)

// releaseGateIndexIfEmptyScript 在索引已空时删掉索引键并把 gate 移出本 slot 的登记集(守住 I2)。
// 与会话 Lua 同在 Redis 单线程上串行:并发的 SetSession 若在之后把玩家加回这个 gate,它会同时 SADD 登记集。
// KEYS[1]=索引键,KEYS[2]=登记集;ARGV[1]=gate uuid。返回 1 = 已释放。
var releaseGateIndexIfEmptyScript = redis.NewScript(`
if redis.call("SCARD", KEYS[1]) > 0 then return 0 end
redis.call("DEL", KEYS[1])
redis.call("SREM", KEYS[2], ARGV[1])
return 1
`)

// repairGateIndexMemberScript 给漏进索引的 ONLINE 会话补成员(只补索引,不转换会话)。
// 会话已不是扫描时那一版时返回 -1,不补:新版本的写入已经按新会话维护过索引。
// KEYS[1]=会话键,KEYS[2]=索引键,KEYS[3]=登记集;ARGV[1]=expected,ARGV[2]=成员,ARGV[3]=gate uuid。
// 返回 1 = 原先缺失、已补;0 = 本来就在。
var repairGateIndexMemberScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if (not current) or current ~= ARGV[1] then return -1 end
local added = redis.call("SADD", KEYS[2], ARGV[2])
redis.call("SADD", KEYS[3], ARGV[3])
return added
`)

// markLegacyWritersScript:leader 在一次成功的 etcd 读里看到旧写者时,递增代次并删就绪标记(同一段 Lua,控制域)。
// 在途的补建读到的是旧代次,结束时的「代次未变才设标记」因此失败。KEYS[1]=代次,KEYS[2]=就绪标记。
var markLegacyWritersScript = redis.NewScript(`
redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
return 1
`)

// renewGateIndexReadyScript:只给已经存在、且是本版本(ARGV[2] 前缀)的就绪标记续租,不新建。
// 新建只走补建结束时的 setGateIndexReadyIfGenScript,续期不可能把被删掉的标记救回来。
// KEYS[1]=就绪标记;ARGV[1]=租约毫秒,ARGV[2]=值前缀。返回 1 = 已续。
var renewGateIndexReadyScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if not v then return 0 end
if string.sub(v, 1, string.len(ARGV[2])) ~= ARGV[2] then return 0 end
redis.call("PEXPIRE", KEYS[1], ARGV[1])
return 1
`)

// setGateIndexReadyIfGenScript:补建结束时,代次仍是补建开始时读到的值才设就绪标记(带租约),
// 同时记下这次完整扫描的完成时刻(审计计时从这里起)。代次不存在按 "0" 比较。控制域。
// KEYS[1]=代次,KEYS[2]=就绪标记,KEYS[3]=上次审计时刻;
// ARGV[1]=开始时的代次,ARGV[2]=标记值,ARGV[3]=租约毫秒,ARGV[4]=完成时刻 unix 秒。返回 1 = 已设。
var setGateIndexReadyIfGenScript = redis.NewScript(`
local g = redis.call("GET", KEYS[1])
if not g then g = "0" end
if g ~= ARGV[1] then return 0 end
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
redis.call("SET", KEYS[3], ARGV[4])
return 1
`)

// 以下是玩家域索引脚本的 KEYS,生产与 script_slots_contract_test.go 共用同一份。

func pruneGateIndexMemberKeys(playerID uint64, gate string) []string {
	return []string{sessionKey(playerID), gateSessionIndexKey(redisslot.PlayerSlot(playerID), gate)}
}

func releaseGateIndexKeys(p gateSlot) []string {
	return []string{gateSessionIndexKey(p.slot, p.gate), gateRegistryKey(p.slot)}
}

func repairGateIndexMemberKeys(playerID uint64, gate string) []string {
	slot := redisslot.PlayerSlot(playerID)
	return []string{sessionKey(playerID), gateSessionIndexKey(slot, gate), gateRegistryKey(slot)}
}

// readGateIndexGen 读代次;键不存在时为 "0"(与 setGateIndexReadyIfGenScript 的比较口径一致)。
func readGateIndexGen(ctx context.Context, rdb *redis.Client) (string, error) {
	gen, err := rdb.Get(ctx, gateIndexGenKey).Result()
	if err == redis.Nil {
		return "0", nil
	}
	return gen, err
}

// gateIndexReadyValue 是就绪标记的值:版本前缀 + 设置时刻(只给排障看,计时不用它)。
func gateIndexReadyValue(now time.Time) string {
	return gateIndexReadyValuePrefix + strconv.FormatInt(now.Unix(), 10)
}

// pruneGateIndexMember 按判定时看到的会话字节守卫摘除成员。found=false 表示判定时会话键不存在
// (不用 expected==nil 表达:存着空串的会话键 MGET 回来也是零长切片,两种情况必须分开)。
// 调用方保证 pid 就在这把索引的 slot 上(sweepIndexPage 先过滤)。
func pruneGateIndexMember(ctx context.Context, rdb redis.UniversalClient, gate string, member string, pid uint64, expected []byte, found bool) (bool, error) {
	mode := "present"
	if !found {
		mode = "missing"
	}
	removed, err := pruneGateIndexMemberScript.Run(
		ctx, rdb,
		pruneGateIndexMemberKeys(pid, gate),
		mode, expected, member,
	).Int()
	return removed == 1, err
}

// mgetSessions 取一页会话的原始字节,按 slot 分组读(一页会话散落在不同 slot);found[i]=false 表示该键不存在。
func mgetSessions(ctx context.Context, rdb redis.UniversalClient, pids []uint64) ([][]byte, []bool, error) {
	if len(pids) == 0 {
		return nil, nil, nil
	}
	keys := make([]string, len(pids))
	for i, pid := range pids {
		keys[i] = sessionKey(pid)
	}
	values, err := playerredis.MGetBySlot(ctx, rdb, keys)
	if err != nil {
		return nil, nil, err
	}
	raws := make([][]byte, len(values))
	found := make([]bool, len(values))
	for i, v := range values {
		switch v := v.(type) {
		case string:
			raws[i], found[i] = []byte(v), true
		case []byte:
			raws[i], found[i] = v, true
		}
	}
	return raws, found, nil
}

// gateRegistryBatchSlots 是读登记集时一个 pipeline 里的 slot 数。
const gateRegistryBatchSlots = 64

// readGateRegistries 读一批 slot 的登记集,展开成索引键列表(slot 升序,同 slot 内顺序不定)。
//
// 某个 slot 的登记集类型不对只跳过这个 slot(tally 计 gate_error,按上限);其余错误(连接、超时、实例级故障)整体返回。
func readGateRegistries(ctx context.Context, rdb redis.UniversalClient, slots []uint16, actions map[string]int, tally *keyErrorTally) ([]gateSlot, error) {
	var pairs []gateSlot
	for start := 0; start < len(slots); start += gateRegistryBatchSlots {
		chunk := slots[start:min(start+gateRegistryBatchSlots, len(slots))]
		cmds := make([]*redis.StringSliceCmd, len(chunk))
		// Pipelined 只返回第一条失败命令的错误;逐条看,才分得清「某个键类型不对」与「整条 pipeline 失败」。
		_, _ = rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
			for i, slot := range chunk {
				cmds[i] = p.SMembers(ctx, gateRegistryKey(slot))
			}
			return nil
		})
		for i, slot := range chunk {
			gates, err := cmds[i].Result()
			if err != nil {
				if aerr := tally.absorb(err, actions, reconcileActionGateError,
					fmt.Sprintf("gate registry of slot=%d", slot)); aerr != nil {
					return nil, fmt.Errorf("read gate registry of slot %d: %w", slot, aerr)
				}
				continue
			}
			for _, gate := range gates {
				if gate != "" {
					pairs = append(pairs, gateSlot{slot: slot, gate: gate})
				}
			}
		}
	}
	return pairs, nil
}
