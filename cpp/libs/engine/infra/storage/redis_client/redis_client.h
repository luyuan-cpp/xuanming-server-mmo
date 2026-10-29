#pragma once

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <functional>
#include <string>
#include <string_view>
#include <memory>
#include <unordered_map>
#include <vector>
#include <limits>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/contrib/hiredis/Hiredis.h"

#include "deps/hiredis/hiredis.h"

#include "engine/core/type_define/type_define.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"

// 普通存盘(无归属校验)。KEYS[1]=数据键, ARGV[1]=payload。
// 仍走脚本并恒返回 1,而不是裸 SET:三条存盘共用 ScriptSlot 的 EVALSHA / NOSCRIPT 回退
// 通道,OnSaved 也只把整数 1 当作提交证据(严格屏障另有 2 = warn 模式下 C3 判定倒退但已照写)。
// 约束(对本文件全部存盘脚本成立):脚本只访问 KEYS[] 声明的键,不得写任何写死的全局键。
// 原因:全服共享 Redis 上的单键会随累计存盘玩家数无界增长、所有 scene 都打同一个键,
// 未声明的键也挡住 Redis Cluster(见 docs/design/capacity-model-hok-scale.md A0-4)。
// 回归用例:currency_test 的 RedisSaveScriptsTouchOnlyDeclaredKeys。
static constexpr const char* kSaveLuaScript = R"(
    redis.call('SET', KEYS[1], ARGV[1])
    return 1
)";

// 带归属校验的存盘变体(scene-owner-reentry-barrier.md §6.2 方案 a)。
// KEYS[1]=数据键, KEYS[2]=guard 键(如 player:{123}:owner_epoch,player_ownership::OwnerEpochRedisKey),
// ARGV[1]=payload, ARGV[2]=调用方缓存的期望值。
// 比对与写入在同一段脚本里完成,没有 GET→SET 之间的 TOCTOU。
//
// guard 键**不存在**时放行,并把它补种成调用方的期望值(Redis Lua 里 GET 缺键
// 返回 false)。guard 键只由铸造方 INCR,从不删除,缺键只可能是 Redis 被清空 /
// 重启丢数据 —— 此刻数据键本身也没了。若按"不相等"拒绝,一次 Redis 清空会让
// 全服在线玩家在下一个周期存盘被逐个判成"已废黜"、不存盘销毁,内存里那份唯一
// 完好的状态反而被丢掉。补种而不是只放行:铸造方之后在它之上继续 INCR,代际
// 保持单调,不会从 1 重铸出一个与旧持有者相同的值。
// 残余风险(接受并记录):Redis 清空的同时恰好还有一个被废黜的僵尸节点,且它
// 抢在合法持有者之前存盘,它会补种自己的旧值、合法持有者随后被拒。两件小概率
// 事件叠加,且僵尸本身由再入屏障先行兜住。
// 键存在但不相等 → 返回 0(已被废黜)。兼容窗口(epoch==0 未铸造)由调用方走
// 无 guard 的 Save 跳过校验,不在脚本里放宽。
// 与 kSaveLuaScript 同一约束:只访问 KEYS[] 声明的键。
static constexpr const char* kSaveIfGuardLuaScript = R"(
    local cur = redis.call('GET', KEYS[2])
    if cur == false then
        redis.call('SET', KEYS[2], ARGV[2])
    elseif cur ~= ARGV[2] then
        return 0
    end
    redis.call('SET', KEYS[1], ARGV[1])
    return 1
)";

// 严格提交屏障:KEYS = 父缓存 / owner 键 player:{pid}:owner_epoch / save_barrier hash / 分桶待投 ZSET。
// ARGV = 父 bytes / owner / 41 字节版本 / topic / 两条完整 DBTask bytes / 玩家十进制
//        / 输出世代 / 加载世代 / 模式(warn | enforce)/ 写方令牌。
// owner 缺失必须由恢复流程处置,禁止沿用旧脚本的自动补种。全部校验先于 SET / HSET:Lua 错误不会回滚写入。
// 版本按字节比较,不转 double。父键 <全名>:{pid}、owner 键、屏障键都带 {pid} hash tag;补投索引键第 2c 批前仍不同槽,
// 部署前提是同一 Redis 实例(docs/design/redis-player-store-cluster.md §5.3、§13)。
//
// owner 判定(docs/design/zone-redis-regression-guard-20261022.md §4.4,G4):Redis 里的 owner 与 ARGV[2] 相等才继续。
//   - owner 缺失:error_reply(见上一段,不补种)。
//   - owner > ARGV[2]:返回 0。铸造方已把玩家派给更新的持有者,本实体已被废黜。
//   - owner < ARGV[2]:返回二元数组 {4, owner},一个键都不写。计数器只由铸造方抬高,持有者缓存的值来自一次成功的铸造,
//     Redis 里反而更小,只可能是 Redis 回退(崩溃丢 AOF 尾、异步复制切主、恢复旧备份)丢掉了那次铸造。存储值随回包
//     带回只为日志与计数。
//   - 只检测、不修复:不把计数器前推到 ARGV[2] 再照写。持有者在本地分不清自己载入的父键是新是旧 —— 回退丢掉源端冻结
//     存盘、却留下铸造时,目标节点载入的就是旧父键;前推会让这份旧内容以更高的 epoch 写穿 go/db 的 SQL 行围栏(旧盖新)。
//     所以 C++ 按废黜处置(HandlePlayerSaveRejected,原因 owner_counter_behind),只丢不盖;回退由 login 加载侧核验兜底。
//   - 两值都已过 is_owner(1..20 位、无前导零的十进制),num_gt 先比长度再按字节比,不经 Lua double。
//
// 覆盖世代校验 C3(docs/design/mixed-version-persist-field-protection.md §5.2):
//   - hash 的 schema_gen 是"这名玩家最近一次经屏障写入的覆盖世代",只升不降;写入成功时同一个 HSET 里写
//     max(存储值, ARGV[8]),存储值损坏时取 ARGV[8]。同一个 HSET 里把 schema_writer 写成 ARGV[11]。
//   - 写方令牌 ARGV[11] 由 C++ 每个加载实体生成一次(全局唯一,实体销毁即作废)。只有"上一次屏障写入不是本实体
//     写的"(schema_writer ≠ ARGV[11])时才比较:那正是接手别人写的数据、截断可能被引入的时刻。本实体的后续写入
//     (包括它换了 owner 代次之后、首存回包丢失后被排队值顶替时)基底都是它自己的上一次写入,跳过。不用 owner
//     代次判"是不是自己写的":同一实体会换代次(E→E+1 复用、B5 采纳),同一代次也会先后加载出两个实体(同落点重连)。
//   - 同版本重放(pending == ARGV[3])显式跳过:同版本分支不提前返回,会落到写入。
//   - 比较放在 owner / 版本判定之后:已被废黜的旧持有者必须拿到 0(DestroyDeposedPlayer),不能先拿到 3。
//   - 加载世代 ARGV[9] < 存储值,或存储值不是合法十进制 uint32 = 倒退证据:warn 照写、返回 2,并把写方令牌记进
//     schema_regressed;enforce 不写任何键、返回 3(下一次仍会比较)。hash 或 schema_gen 字段不存在 = 不比较。
//   - schema_regressed 等于 ARGV[11] 时(这个写方先前在 warn 下被判过倒退)照写并继续返回 2:判定那次的回包可能丢了,
//     重放同一元素或改发同令牌的后继都跳过 C3,只靠这个字段把证据带回来。别的写方写成功也不删它(只在等于本令牌时
//     起作用,令牌全局唯一;只有 save_barrier_restore 以 SQL 为准重建时清掉)。
//   - 参数非法(世代不是 1..4294967295、ARGV[9] > ARGV[8]、模式不是两个字面量、令牌为空 / 超过 64 字节 /
//     含字母数字与 ':' '-' 以外的字符)返回 error_reply:那是 C++ 的编程错误,SaveWithCommitBarrier 入队前已校验。
// 旧二进制自带 7 参旧脚本(ScriptSlot 按脚本文本算 SHA),不受影响;它不写 schema_gen / schema_writer,
// 不会把 hash 拉低;它留下的旧令牌只属于已被取代的实体,新实体的令牌不会与之相等。
static constexpr const char* kSaveWithCommitBarrierLuaScript = R"(
    local function gt(a, b)
        for i = 1, #a do
            local x, y = string.byte(a, i), string.byte(b, i)
            if x ~= y then return x > y end
        end
        return false
    end
    -- 无前导零的十进制数比较:先比长度,再按字节比。gt 只适用于等长串。
    local function num_gt(a, b)
        if #a ~= #b then return #a > #b end
        return gt(a, b)
    end
    local function is_version(s)
        if type(s) ~= 'string' or #s ~= 41 or string.sub(s, 21, 21) ~= ':'
            or not string.match(s, '^%d+:%d+$') then return false end
        local max = '18446744073709551615'
        return not gt(string.sub(s, 1, 20), max) and not gt(string.sub(s, 22, 41), max)
    end
    local function is_owner(s)
        if type(s) ~= 'string' or #s == 0 or #s > 20 or string.sub(s, 1, 1) == '0'
            or not string.match(s, '^%d+$') then return false end
        return #s < 20 or not gt(s, '18446744073709551615')
    end
    local function is_u32(s)
        return is_owner(s) and (#s < 10 or (#s == 10 and not gt(s, '4294967295')))
    end
    local function is_zone(s)
        return is_owner(s) and #s <= 6
    end
    local function topic_zone(topic)
        local zone = string.match(topic, '^db_task_zone_([1-9]%d*)$')
        if zone then return is_zone(zone) and zone or false end
        local z, generation = string.match(topic, '^db_task_zone_([1-9]%d*)_g([1-9]%d*)$')
        if not is_zone(z) or not is_u32(generation) or generation == '1' then return false end
        return z
    end
    if #KEYS ~= 4 or #ARGV ~= 11 then
        return redis.error_reply('SAVE_BARRIER invalid argument count')
    end
    for i = 1, 4 do
        if #KEYS[i] == 0 then return redis.error_reply('SAVE_BARRIER empty key') end
        for j = i + 1, 4 do
            if KEYS[i] == KEYS[j] then return redis.error_reply('SAVE_BARRIER keys must differ') end
        end
    end
    local parent_type = redis.call('TYPE', KEYS[1]).ok
    local owner_type = redis.call('TYPE', KEYS[2]).ok
    local barrier_type = redis.call('TYPE', KEYS[3]).ok
    local index_type = redis.call('TYPE', KEYS[4]).ok
    if (parent_type ~= 'none' and parent_type ~= 'string') or owner_type ~= 'string'
        or (barrier_type ~= 'none' and barrier_type ~= 'hash')
        or (index_type ~= 'none' and index_type ~= 'zset') then
        return redis.error_reply('SAVE_BARRIER invalid key type or missing owner')
    end
    if not is_owner(ARGV[2]) or not is_version(ARGV[3])
        or string.sub(ARGV[3], 1, 20) ~= string.rep('0', 20 - #ARGV[2]) .. ARGV[2]
        or string.sub(ARGV[3], 22, 41) == string.rep('0', 20) then
        return redis.error_reply('SAVE_BARRIER invalid version')
    end
    local zone, bucket = string.match(KEYS[4], '^player:save_pending:([1-9]%d*):(%d+)$')
    if not is_owner(ARGV[7]) or not is_zone(zone) or topic_zone(ARGV[4]) ~= zone
        or #ARGV[1] == 0 or #ARGV[5] == 0 or #ARGV[6] == 0 then
        return redis.error_reply('SAVE_BARRIER invalid outbox')
    end
    if KEYS[2] ~= 'player:{' .. ARGV[7] .. '}:owner_epoch'
        or KEYS[3] ~= 'player:{' .. ARGV[7] .. '}:save_barrier'
        or #KEYS[1] <= #ARGV[7] + 3 or string.sub(KEYS[1], -#ARGV[7] - 3) ~= ':{' .. ARGV[7] .. '}' then
        return redis.error_reply('SAVE_BARRIER player key mismatch')
    end
    -- GUID 仅逐位求余,中间值小于 640,不把 uint64 转成 double。
    local expected_bucket = 0
    for i = 1, #ARGV[7] do
        expected_bucket = (expected_bucket * 10 + string.byte(ARGV[7], i) - 48) % 64
    end
    if bucket ~= tostring(expected_bucket) then return redis.error_reply('SAVE_BARRIER invalid bucket') end
    if not is_u32(ARGV[8]) or not is_u32(ARGV[9]) or num_gt(ARGV[9], ARGV[8])
        or (ARGV[10] ~= 'warn' and ARGV[10] ~= 'enforce')
        or #ARGV[11] == 0 or #ARGV[11] > 64 or not string.match(ARGV[11], '^[%w:%-]+$') then
        return redis.error_reply('SAVE_BARRIER invalid schema argument')
    end
    local owner = redis.call('GET', KEYS[2])
    local pending = redis.call('HGET', KEYS[3], 'pending')
    if not is_owner(owner) or (pending and not is_version(pending)) then
        return redis.error_reply('SAVE_BARRIER invalid stored version')
    end
    local outbox_version = redis.call('HGET', KEYS[3], 'outbox_version')
    local outbox_topic = redis.call('HGET', KEYS[3], 'outbox_topic')
    local outbox_first = redis.call('HGET', KEYS[3], 'outbox_player_database')
    local outbox_second = redis.call('HGET', KEYS[3], 'outbox_player_database_1')
    if outbox_version or outbox_topic or outbox_first or outbox_second then
        if not is_version(outbox_version) or outbox_version ~= pending or not outbox_topic
            or not topic_zone(outbox_topic) or not outbox_first or #outbox_first == 0
            or not outbox_second or #outbox_second == 0 then
            return redis.error_reply('SAVE_BARRIER invalid stored outbox')
        end
    end
    -- G4:计数器比缓存值小 = Redis 回退丢了铸造。只检测:回 {4, 存储值},不写任何键,也不前推计数器(见头注释)。
    if num_gt(ARGV[2], owner) then return {4, owner} end
    if owner ~= ARGV[2] then return 0 end
    if pending and gt(pending, ARGV[3]) then
        return redis.error_reply('SAVE_BARRIER version regression')
    end
    if pending == ARGV[3] then
        if redis.call('GET', KEYS[1]) ~= ARGV[1]
            or (outbox_version and (outbox_topic ~= ARGV[4] or outbox_first ~= ARGV[5] or outbox_second ~= ARGV[6])) then
            return redis.error_reply('SAVE_BARRIER same version conflict')
        end
    end
    -- C3 放在 owner / 版本 / 同版本冲突判定之后(见头注释)。同版本重放(上面那支不提前返回)显式跳过;
    -- 上一次屏障写入就是本实体写的(写方令牌相同)同样跳过:基底是它自己的上一次写入。
    local stored_gen = redis.call('HGET', KEYS[3], 'schema_gen')
    local stored_gen_valid = stored_gen and is_u32(stored_gen)
    local stored_writer = redis.call('HGET', KEYS[3], 'schema_writer')
    local stored_regressed = redis.call('HGET', KEYS[3], 'schema_regressed')
    local regression = false
    if stored_gen and pending ~= ARGV[3] and stored_writer ~= ARGV[11] then
        regression = not stored_gen_valid or num_gt(stored_gen, ARGV[9])
    end
    if regression and ARGV[10] == 'enforce' then return 3 end
    -- warn 判过倒退的写方,之后的写入(回包丢失后的同版本重放、同令牌的后继)继续返回 2:判定那次的回包可能丢了,
    -- 证据不能跟着消失。证据记在 schema_regressed(= 被判倒退的写方令牌),别的写方写成功也不清:它只在等于
    -- ARGV[11] 时起作用而令牌全局唯一,保留对 C3 没有副作用;清掉的话"判倒退后回包丢失、随即被废黜"的那次判定就永远
    -- 不计数。值班巡检这个字段的存量(deploy/k8s/README.md)作为切 enforce 门禁的第三项。
    local warned = regression or stored_regressed == ARGV[11]
    local schema_gen = ARGV[8]
    if stored_gen_valid and num_gt(stored_gen, schema_gen) then schema_gen = stored_gen end
    redis.call('SET', KEYS[1], ARGV[1])
    redis.call('HSET', KEYS[3], 'pending', ARGV[3], 'outbox_version', ARGV[3],
        'outbox_topic', ARGV[4], 'outbox_player_database', ARGV[5], 'outbox_player_database_1', ARGV[6],
        'schema_gen', schema_gen, 'schema_writer', ARGV[11])
    if regression then
        redis.call('HSET', KEYS[3], 'schema_regressed', ARGV[11])
    end
    redis.call('ZADD', KEYS[4], 0, ARGV[7])
    if warned then return 2 end
    return 1
)";


namespace google
{
    namespace protobuf
    {
        class Message;
    }//namespace protobuf
}// namespace google


using MessageCachedArray = std::vector<uint8_t>;

// 只承载既有 DBTask 的不可变线格式,不新增资产真相。与父快照同生同版,重试及后台补投复用原字节。
struct RedisSaveCommitIntent
{
    static constexpr uint32_t kIndexBuckets = 64;
    std::string index_key;
    std::string topic;
    std::string player_database_task;
    std::string player_database_1_task;
};

// 严格屏障的覆盖世代参数(docs/design/mixed-version-persist-field-protection.md §5.2),对应 Lua 的 ARGV[8..11]。
// 本类不解释世代含义,只在入队前校验形状,保证非法值不会让脚本返回 error_reply 而无限重试。
struct RedisSaveSchemaGen
{
    uint32_t output_gen = 0; // ARGV[8]:本次写入盖的覆盖世代(两张表的戳),必须 >= 1
    uint32_t load_gen = 0;   // ARGV[9]:写方实体的加载世代,1 <= load_gen <= output_gen
    std::string mode;        // ARGV[10]:只认 "warn" / "enforce"
    std::string writer;      // ARGV[11]:写方令牌,每个加载实体一个、全局唯一;1..64 字节,只含字母数字与 ':' '-'
};

// 写方令牌的形状校验,与严格 Lua 的 ARGV[11] 校验同口径。
[[nodiscard]] inline bool IsValidRedisSaveSchemaWriter(const std::string& writer)
{
    if (writer.empty() || writer.size() > 64)
    {
        return false;
    }
    for (const char c : writer)
    {
        const bool alnum = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z');
        if (!alnum && c != ':' && c != '-')
        {
            return false;
        }
    }
    return true;
}

// owner 归属校验拒绝一笔存盘的原因,随 SaveRejectedCallback 交给调用方。两种都是"这份期望值已作废"、都不重试,
// 区别只在成因:调用方据此分开计数与日志,不能把 Redis 回退混进"已被改派"的双主证据里。
enum class RedisSaveOwnerRejectReason : uint8_t
{
    // Lua 返回 0:Redis 的 owner 与期望值不等。严格屏障里只剩"Redis 比期望值大"(已被改派、本实体被废黜);
    // 旧 guard 脚本(kSaveIfGuardLuaScript)不分大小。
    kOwnerMismatch,
    // 严格屏障返回 {4, 存储值}:Redis 的 owner 比期望值小 = Redis 回退丢了铸造(zone-redis-regression-guard-20261022.md §4.4)。
    kOwnerCounterBehind,
};

struct RedisSaveOwnerRejection
{
    RedisSaveOwnerRejectReason reason = RedisSaveOwnerRejectReason::kOwnerMismatch;
    std::string stored_owner; // 仅 kOwnerCounterBehind 有值:Redis 里 owner 计数器的十进制原文;返回 0 时脚本不带存储值,为空
};

// 严格屏障"owner 计数器比期望值小"的回包码,与 kSaveWithCommitBarrierLuaScript 里 return {4, owner} 同值。
inline constexpr long long kRedisSaveOwnerCounterBehindCode = 4;

// 解析严格屏障的 {4, 存储值} 回包。形状完全相符才返回 true 并写出存储值;任何偏差都不是这个结论,
// 交给调用方按坏回包处理(重试),不能猜成"被拒"而丢掉这次存盘。
[[nodiscard]] inline bool ParseRedisOwnerCounterBehindReply(const redisReply* reply, std::string& storedOwner)
{
    if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 2 || reply->element == nullptr)
    {
        return false;
    }
    const redisReply* code = reply->element[0];
    const redisReply* stored = reply->element[1];
    if (code == nullptr || code->type != REDIS_REPLY_INTEGER || code->integer != kRedisSaveOwnerCounterBehindCode ||
        stored == nullptr || stored->type != REDIS_REPLY_STRING || stored->str == nullptr || stored->len == 0)
    {
        return false;
    }
    storedOwner.assign(stored->str, stored->len);
    return true;
}

// 仅供单测在不连接真实 Redis 的情况下驱动异步回包；生产代码不定义或使用。
template <class MessageKey, class MessageValue>
struct MessageAsyncClientTestPeer;

class SyncRedisContext_Deleter
{
public:
    void operator()(redisContext* res);
};

class MessageSyncRedisClient
{
public:
    using ContextPtr = std::unique_ptr<redisContext, SyncRedisContext_Deleter>;
    void Connect(const std::string& redis_server_addr, int32_t port, int32_t sec, int32_t usec);

    void Save(const google::protobuf::Message& message);
    void Save(const google::protobuf::Message& message, Guid guid);
    void Save(const google::protobuf::Message& message, const std::string& key);

    void Load(google::protobuf::Message& message);
    void Load(google::protobuf::Message& message, Guid guid);
    void Load(google::protobuf::Message& message, const std::string& key);
private:
    void OnDisconnect();

    ContextPtr context_;
};

using PbSyncRedisClientPtr = std::shared_ptr<MessageSyncRedisClient>;

template <class MessageKey, class MessageValue>
class MessageAsyncClient
{
public:
	using MessageValuePtr = std::shared_ptr<MessageValue>;
	using CommittedCallback = std::function<void()>;

	// Max NIL retries. Combined with exponential backoff (2,4,8,16,32,60s) this
	// gives ~2 minutes for the DB write-back path (Kafka -> db service -> Redis)
	// to land the row before we permanently fail the load.
	static constexpr int kMaxLoadRetries = 6;

	struct Element
	{
		MessageKey message_key;
		std::string redis_key;
		MessageValuePtr message_value;
		// Pre-serialized payload (Save path only). Populated by Save() so the
		// retry path doesn't have to re-serialize the protobuf message.
		std::vector<uint8_t> serialized_payload;
		int retry_count = 0;
		// Save retries continue after the alert threshold. This flag ensures the
		// owner receives one high-severity notification per pending value instead
		// of one notification every capped-backoff tick.
		bool save_failure_notified = false;
		// Earliest time this element is allowed to be retried (steady_clock).
		// Set when the element is enqueued into pending_retry_queue_ /
		// pending_save_queue_.
		std::chrono::steady_clock::time_point next_retry_at{};
		// 归属校验(仅 Save 路径)。guard_key 为空 = 无校验,行为与旧 Save 完全
		// 一致;非空时只有 Redis 里 guard_key 的当前值与 guard_expected 逐字节
		// 相等才允许写入。两者随 element 走,重试 / NOSCRIPT 重发都用发起时的
		// 期望值,不回头问调用方 —— 一次存盘绑定的是"发起那一刻我自认的归属"。
		std::string guard_key;
		std::string guard_expected;
		std::string barrier_key;
		std::string barrier_version;
		std::shared_ptr<const RedisSaveCommitIntent> commit_intent;
		// 严格屏障的 ARGV[8..11](覆盖世代、加载世代的十进制文本,模式,写方令牌),发起时定好。与 guard 同理随
		// element 走:重试、NOSCRIPT 重发、重连回收都发同一组参数,不回头问调用方。非屏障路径为空。
		std::string schema_output_gen;
		std::string schema_load_gen;
		std::string schema_mode;
		std::string schema_writer;
		// 每份真正写入 Redis 的快照成功时执行一次;未下发就被合并的值不执行。
		// 即使有更新值排队也先登记本次 SQL 票据;全局回调仍只发布最新值。
		// 钩子与存盘回包同线程,允许重入 Save:本次在途登记保留到钩子结束,同 key 新值只排队。
		CommittedCallback committed_callback;
	};

	using ElementPtr = std::shared_ptr<Element>;
	using LoadingQueue = std::unordered_map<std::string, ElementPtr>;
	using EventCallback = std::function<void(MessageKey, MessageValue&)>;

	// DataNotFound 只表示 NIL 重试耗尽,不证明新号或 SQL 无行。调用方必须 fail-closed,不能补空存档。
	// RedisError 表示连接、解析或回包类型故障。
	enum class LoadFailureReason
	{
		DataNotFound,
		RedisError,
	};
	using FailedCallback = std::function<void(MessageKey, LoadFailureReason)>;

	// 存盘连续失败达到告警阈值的通知。通知后仍保留最新值并继续重试。
	//
	// 为什么必须有:异步存盘的调用方普遍依赖"成功回调迟早会来"来收尾
	// (销毁实体、清 session、解锁)。旧实现在重试耗尽时只打一条 LOG_ERROR
	// 就 return,没有任何人被通知 —— 退出流程于是永久悬挂,而且数据只留在
	// 内存里,盘上还是上一次成功存盘的旧值。加载路径早就有
	// load_failed_callback_,存盘路径缺这一半是接口级的不对称。
	using SaveFailedCallback = std::function<void(MessageKey, const std::string& redisKey, int retryCount)>;

	// 带 guard 的存盘被 Redis 原子拒绝(Lua 返回 0,或严格屏障返回 {4, 存储值})的通知。
	//
	// 这不是故障,是"这份期望值已作废":发出这次存盘时缓存的归属值与 Redis 不等。
	// 所以它既不走 save_failed_callback_(那一条的含义是"Redis 不可用,仍在重试"),
	// 也不原样重试 —— 同一个期望值再存只会再被拒。
	//
	// guardExpected 是**被拒的那一份**期望值,调用方必须拿它与自己当前缓存的值比:
	// 相等 = 这份归属确实已作废(销毁本地实体、停止一切存盘、不得发 relocate,
	// reentry-barrier §3.3 / §6.2);不相等 = 被拒的只是一笔旧代际的在途写,自己
	// 已经拿到了更新的归属,应当用新值重新存盘而不是自毁。
	// rejection 说明成因(见 RedisSaveOwnerRejectReason):kOwnerMismatch = 已被改派;kOwnerCounterBehind = Redis 的计数器
	// 比期望值还小(Redis 回退丢了铸造),stored_owner 带回存储值。两者处置相同,调用方只用它分开计数与日志,
	// 不得据此前推计数器或改写期望值重存(理由见严格 Lua 头注释)。
	// 同 key 排队中的值:期望值与被拒的相同 → 一并丢弃(发出去只会再被拒一次);
	// 期望值不同 → 它带着更新的归属,照常发出,此时不回调(那笔旧写已被它取代)。
	using SaveRejectedCallback = std::function<void(MessageKey, const std::string& redisKey, const std::string& guardExpected,
													const RedisSaveOwnerRejection& rejection)>;

	// 严格屏障覆盖世代校验 C3 的两种结局(docs/design/mixed-version-persist-field-protection.md §5.2)。
	//
	// 返回 2(warn 模式判定倒退但照写):Redis 已提交,是提交证据,照常执行提交钩子、发布 save_callback_。
	// SaveSchemaWarnedCallback 只用于计数与日志,在提交钩子之前同步调用,不得重入 Save。writer 是这份写入的写方令牌:
	// 同一写方被判过倒退之后每次写入都会再返回 2(证据留在 Redis,防回包丢失),调用方按令牌去重计数。
	//
	// 返回 3(enforce 模式拒写,一个键都没动):与 owner 拒绝(0)一样不重试、不执行提交钩子、不发布 save_callback_,
	// 同 key 排队值的处置也同 0 分支(期望值相同丢弃;不同则照发、不回调)。语义却不同于"已被废黜",
	// 所以不复用 SaveRejectedCallback(那条会按 epoch 重存而无限循环,或按废黜销毁),由调用方按实体状态分派。
	using SaveSchemaWarnedCallback = std::function<void(MessageKey, const std::string& redisKey, const std::string& writer)>;
	using SaveSchemaRejectedCallback = std::function<void(MessageKey, const std::string& redisKey, const std::string& guardExpected)>;

	using HiredisPtr = std::shared_ptr<hiredis::Hiredis>;

	// 回调绑定约定:本类所有 hiredis 回调都以裸 this 经 std::bind 绑定,而不是
	// AGENTS §11.7 要求的 weak_ptr。这是 §11.7 允许的"与被绑对象同寿命"例外:
	// 本类由 RedisSystem 以 thread_local 独占持有,hiredis_ 由同线程的
	// RedisManager 持有,两者寿命都覆盖整个 EventLoop;回调与 loop 同线程,
	// 且 RedisSystem::Shutdown 先摘定时器与重连回调再释放本类。若将来本类改为
	// 可在 loop 存活期间随意销毁(例如按玩家创建),必须改成
	// enable_shared_from_this + weak_ptr 捕获,不能沿用这里的写法。
	explicit MessageAsyncClient(HiredisPtr& hiredis)
		: hiredis_(hiredis)
	{
	}

	inline const std::string full_name() const { return std::string(MessageValue::GetDescriptor()->full_name()); }

	// 玩家域父键 <消息全名>:{<key>}(docs/design/redis-player-store-cluster.md §5.2):key 以 hash tag 包住,与同一玩家的
	// owner_epoch、屏障、补投索引同槽。与 go/shared/savebarrier.ParentKey 逐字一致;严格存盘 Lua 按 ":{<pid>}" 尾缀核对。
	// 队列的键、发往 Redis 的键都只经这里构造。
	std::string RedisKeyFor(const MessageKey& key) const { return full_name() + ":" + redis_slot::PlayerTag(key); }

	void SetSaveCallback(const EventCallback& cb) { save_callback_ = cb; }
	void SetLoadCallback(const EventCallback& cb) { load_callback_ = cb; }
	void SetLoadFailedCallback(const FailedCallback& cb) { load_failed_callback_ = cb; }
	void SetSaveFailedCallback(const SaveFailedCallback& cb) { save_failed_callback_ = cb; }
	void SetSaveRejectedCallback(const SaveRejectedCallback& cb) { save_rejected_callback_ = cb; }
	void SetSaveSchemaWarnedCallback(const SaveSchemaWarnedCallback& cb) { save_schema_warned_callback_ = cb; }
	void SetSaveSchemaRejectedCallback(const SaveSchemaRejectedCallback& cb) { save_schema_rejected_callback_ = cb; }


	void Save(const MessageValuePtr& message, const MessageKey& key, CommittedCallback committed = {})
	{
		EnqueueSave(message, key, std::string(), std::string(), {}, {}, {}, nullptr, std::move(committed));
	}

	// 带归属校验的存盘:只有 Redis 里 guardKey 的当前值 == guardExpected 时才
	// 原子地写入,否则整条写入被丢弃并回调 save_rejected_callback_
	// (见 kSaveIfGuardLuaScript)。guardKey 为空退化为无校验的 Save。
	// 期望值按调用方与铸造方约定的文本格式传入(owner_epoch 是纯十进制整数
	// 字符串),本类不解释其含义,只做逐字节比对。
	void Save(const MessageValuePtr& message, const MessageKey& key,
			  const std::string& guardKey, const std::string& guardExpected, CommittedCallback committed = {})
	{
		EnqueueSave(message, key, guardKey, guardExpected, {}, {}, {}, nullptr, std::move(committed));
	}

	// 严格路径必须显式携带完整持久意图,不能降级成普通 Save。参数 / 序列化失败返回 false;
	// 已排队的 Redis 错误保留最新快照重试,owner 不等才通知废黜,两者均不执行提交钩子。
	// schemaGen 是覆盖世代校验 C3 的参数(见 RedisSaveSchemaGen 与严格 Lua 头注释):形状不合法同样返回 false,
	// 不能让脚本因参数错误返回 error_reply —— OnSaved 对 ERROR 无限重试,这名玩家就永远存不进盘。
	bool SaveWithCommitBarrier(const MessageValuePtr& message, const MessageKey& key,
		const std::string& guardKey, const std::string& guardExpected,
		const std::string& barrierKey, const std::string& version,
		std::shared_ptr<const RedisSaveCommitIntent> intent, const RedisSaveSchemaGen& schemaGen,
		CommittedCallback committed = {})
	{
		if (guardKey.empty() || barrierKey.empty() || !intent || intent->index_key.empty() || intent->topic.empty() ||
			intent->player_database_task.empty() || intent->player_database_1_task.empty())
		{
			LOG_ERROR << "SaveWithCommitBarrier requires owner, barrier and complete outbox for player " << key;
			return false;
		}
		if (schemaGen.output_gen == 0 || schemaGen.load_gen == 0 || schemaGen.load_gen > schemaGen.output_gen ||
			(schemaGen.mode != "warn" && schemaGen.mode != "enforce") || !IsValidRedisSaveSchemaWriter(schemaGen.writer))
		{
			LOG_ERROR << "SaveWithCommitBarrier rejects invalid persist schema arguments for player " << key
					  << ": output_gen=" << schemaGen.output_gen << " load_gen=" << schemaGen.load_gen
					  << " mode='" << schemaGen.mode << "' writer_bytes=" << schemaGen.writer.size();
			return false;
		}
		return EnqueueSave(message, key, guardKey, guardExpected, barrierKey, version, std::move(intent), &schemaGen,
						   std::move(committed));
	}

	// 同 key 还有本客户端未落定的存盘(在途或排队)时,加载先挂起,等这些存盘落定(成功、被拒或被更新值取代后落定)
	// 再发 GET(ReleaseLoadsAwaitingSaves,由 RetryDuePending 的周期节拍驱动)。
	//
	// 为什么(docs/design/mixed-version-persist-field-protection.md §13 第 9 条):前任实体被不存盘销毁时,它的存盘可能还在
	// 排队重试;同落点重连在同一 owner 代次下加载出的新实体若先读,读到的是那笔存盘之前的父快照,随后那笔存盘落地,
	// 新实体的下一次存盘就会把它盖回去(回档),升世代窗口里还会被 C3 判倒退而永久拒存。挂起加载不改存盘语义:
	// 前任那份照常落地或被拒,新实体读到的就是落定之后的状态。不同 key 互不影响;同一连接上已在途的 GET 不受影响。
	void AsyncLoad(const MessageKey& key, int retry_count = 0)
	{
		std::string redis_key = RedisKeyFor(key);

		if (HasUnsettledSave(key) && loading_queue_.find(redis_key) == loading_queue_.end())
		{
			auto& parked = loads_awaiting_saves_[redis_key];
			if (!parked)
			{
				parked = std::make_shared<Element>();
				parked->message_key = key;
				parked->redis_key = redis_key;
				LOG_INFO << "AsyncLoad deferred until the pending save of the same key settles: " << redis_key;
			}
			parked->retry_count = std::max(parked->retry_count, retry_count);
			// 同 key 已在等重试的那份并进挂起项,免得两条路各发一次 GET。
			if (auto pendingIt = pending_retry_queue_.find(redis_key); pendingIt != pending_retry_queue_.end())
			{
				parked->retry_count = std::max(parked->retry_count, pendingIt->second->retry_count);
				pending_retry_queue_.erase(pendingIt);
			}
			return;
		}

		LOG_DEBUG << "AsyncLoad: key=" << redis_key
				  << " connected=" << (hiredis_ ? hiredis_->connected() : false)
				  << " retry=" << retry_count
				  << " loading_queue=" << loading_queue_.size()
				  << " pending_retry=" << pending_retry_queue_.size();

		if (!hiredis_ || !hiredis_->connected())
		{
			LOG_WARN << "Redis not connected, queueing AsyncLoad for retry: " << redis_key;
			auto element = std::make_shared<Element>();
			element->message_key = key;
			element->redis_key = redis_key;
			element->retry_count = retry_count;
			// next_retry_at default-constructed (epoch); periodic timer will pick it up
			// as soon as Redis reconnects.
			pending_retry_queue_[redis_key] = element;
			return;
		}

		// Already in-flight
		if (loading_queue_.find(redis_key) != loading_queue_.end())
		{
			return;
		}

		// If the same key is already waiting in pending_retry_queue_, preserve its
		// retry_count so an external re-trigger does not reset NIL backoff.
		if (auto pendingIt = pending_retry_queue_.find(redis_key); pendingIt != pending_retry_queue_.end())
		{
			if (pendingIt->second->retry_count > retry_count)
			{
				retry_count = pendingIt->second->retry_count;
			}
			pending_retry_queue_.erase(pendingIt);
		}

		ElementPtr element = std::make_shared<Element>();
		element->message_key = key;
		element->redis_key = redis_key;
		element->retry_count = retry_count;

		loading_queue_.emplace(redis_key, element);

		const std::string format = "GET " + redis_key;
		int ret = hiredis_->command(std::bind(&MessageAsyncClient::OnLoaded, this, std::placeholders::_1, std::placeholders::_2, element),
			format.c_str());
		if (ret != REDIS_OK)
		{
			LOG_ERROR << "Redis command failed (ret=" << ret << ") for key: " << redis_key
					  << ", connected=" << hiredis_->connected();
			loading_queue_.erase(redis_key);
			pending_retry_queue_[redis_key] = element;
		}
	}

	// Call ONLY from the Redis reconnect callback. Re-issues every pending load
	// immediately and re-flushes pending saves.
	//
	// 断线时 hiredis 会先把旧连接上所有在途命令的回调以空应答调用一次(连接断开 / 被释放都是如此),
	// 所以到这里时,在途 GET 通常已由 OnLoaded 放回 pending_retry_queue_,在途存盘已由 OnSaved 放回
	// pending_save_queue_。下面把 loading_queue_ / saving_queue_ 里剩下的条目也挪过去,只是兜底
	// (回调确实没来的情形);旧连接迟到的回调会被 OnLoaded / OnSaved 开头的在途比对当作陈旧回调丢弃。
	// (2026-10-16 前这里的注释写的是「断线后在途回调不会再来」,与 hiredis 的实际行为不符。)
	void OnReconnected()
	{
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}

		// Move each in-flight save back to pending, unless Save() already placed a
		// newer full snapshot for the same key there. Full snapshots are
		// superseding, so retaining the newest pending value is sufficient.
		for (auto &[k, v] : saving_queue_)
		{
			if (pending_save_queue_.find(k) == pending_save_queue_.end())
			{
				v->next_retry_at = std::chrono::steady_clock::now();
				pending_save_queue_[k] = v;
			}
		}
		saving_queue_.clear();

		// Cached EVALSHA hash is bound to the previous server connection;
		// drop it so we re-SCRIPT LOAD on the new connection. A SCRIPT LOAD that
		// was in flight on the dead connection will never clear this flag.
		// 三个脚本槽位都要清:普通、旧 guard 兼容与严格提交屏障各有一份 SHA。
		for (ScriptSlot* slot : {&save_script_, &guard_script_, &barrier_script_})
		{
			slot->sha1.clear();
			slot->load_in_flight = false;
			EnsureScriptLoaded(*slot);
		}

		const auto now = std::chrono::steady_clock::now();
		for (auto &[k, v] : loading_queue_)
		{
			v->next_retry_at = now;
			pending_retry_queue_[k] = v;
		}
		loading_queue_.clear();

		const size_t pendingLoads = pending_retry_queue_.size();
		const size_t pendingSaves = pending_save_queue_.size();
		if (pendingLoads + pendingSaves == 0)
		{
			return;
		}

		LOG_INFO << "OnReconnected: re-issuing " << pendingLoads << " loads, "
				 << pendingSaves << " saves";
		FlushDuePending(now, /*force=*/true);
	}

	// Periodic timer entry point. ONLY drains entries in pending_retry_queue_
	// and pending_save_queue_ whose backoff has elapsed. MUST NOT touch
	// loading_queue_, otherwise in-flight GETs get re-issued and load_callback_
	// fires twice for the same key.
	void RetryDuePending()
	{
		// 先放行存盘已落定的挂起加载(AsyncLoad 自己处理未连接:放回 pending_retry_queue_ 等重连)。
		ReleaseLoadsAwaitingSaves();
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}
		if (pending_retry_queue_.empty() && pending_save_queue_.empty())
		{
			return;
		}
		FlushDuePending(std::chrono::steady_clock::now(), /*force=*/false);
	}

	// Backwards-compatible alias.
	void RetryDuePendingLoads() { RetryDuePending(); }

	MessageValuePtr CreateMessage() { return std::make_shared<MessageValue>(); }

	// --- Observability ---
	// Number of GETs whose reply has not yet been received.
	size_t in_flight_load_count() const { return loading_queue_.size(); }
	// Number of loads waiting in the NIL/disconnect retry queue.
	size_t pending_load_count() const { return pending_retry_queue_.size(); }
	// 按key核实真实GET及重试均已排空；交易封存不能用待入场上下文消失替代它。
	bool HasPendingLoad(const MessageKey& key) const
	{
		const std::string redisKey = RedisKeyFor(key);
		return loading_queue_.contains(redisKey) || pending_retry_queue_.contains(redisKey) ||
			   loads_awaiting_saves_.contains(redisKey);
	}
	// 因同 key 存盘未落定而挂起的加载数(见 AsyncLoad)。
	size_t awaiting_save_load_count() const { return loads_awaiting_saves_.size(); }
	// Number of saves waiting in the failure/disconnect retry queue.
	size_t pending_save_count() const { return pending_save_queue_.size(); }
	// Number of SETs whose reply has not yet been received.
	size_t in_flight_save_count() const { return saving_queue_.size(); }

	// 该 key 是否还有未落地的存盘:在途(已发出、等回包),或排队(等在途完成 / 等退避重试 / 等重连)。
	// 只读,不改队列。给"销毁实体前确认内存态已全部落地"这类判断用
	// (docs/design/cross-zone-scene-travel.md §12.6 Z1 / M4;docs/design/guild-phase2/08-save-owner-fence.md §8.2.2)。
	// 两条使用约束:
	//   - 在 save_callback_ 里调用它恒为 false:OnSaved 成功路径先摘在途,有排队值就直接发新值且**不**回调,
	//     只有排队为空才回调。所以它不能在回调里当"还有没有更新值"的信号;它的用处在回调**之外**,
	//     例如快路径退出时,内存虽等于上次落地的快照,但还有一笔内容不同的存盘在途或排队 ——
	//     此刻销毁实体,那笔写落地后会把盘改回去。
	//   - 只看存盘的两个队列(saving_queue_ / pending_save_queue_),不看 pending_retry_queue_(那是载入的)。
	bool HasUnsettledSave(const MessageKey& key) const
	{
		const std::string redisKey = RedisKeyFor(key);
		return saving_queue_.find(redisKey) != saving_queue_.end() ||
			   pending_save_queue_.find(redisKey) != pending_save_queue_.end();
	}

	// Convenience: log a one-shot snapshot of all three queue lengths. Useful
	// when bolting MessageAsyncClient onto a periodic reporter.
	void LogQueueSnapshot(const char *tag) const
	{
		if (in_flight_load_count() == 0 && in_flight_save_count() == 0 &&
			pending_load_count() == 0 && pending_save_count() == 0 && awaiting_save_load_count() == 0)
		{
			return;
		}
		LOG_INFO << "[" << tag << "] " << full_name()
				 << " in_flight_loads=" << in_flight_load_count()
				 << " in_flight_saves=" << in_flight_save_count()
				 << " pending_loads=" << pending_load_count()
				 << " pending_saves=" << pending_save_count()
				 << " loads_awaiting_saves=" << awaiting_save_load_count();
	}

private:
	static std::chrono::milliseconds BackoffForRetry(int next_retry_count)
	{
		// 1->500ms, 2->1s, 3->2s, 4->4s, 5->8s, 6+->16s  (cumulative ~31.5s)
		// Tuned for the typical Login -> Kafka -> DB -> Redis warm path P99 (~1-3s).
		// First retry hits fast so a healthy preload barely impacts player-perceived
		// login latency; later retries widen to shed load if the backend is stuck.
		switch (next_retry_count)
		{
		case 1:
			return std::chrono::milliseconds(500);
		case 2:
			return std::chrono::milliseconds(1000);
		case 3:
			return std::chrono::milliseconds(2000);
		case 4:
			return std::chrono::milliseconds(4000);
		case 5:
			return std::chrono::milliseconds(8000);
		default:
			return std::chrono::milliseconds(16000);
		}
	}

	// 放行同 key 存盘都已落定的挂起加载(见 AsyncLoad)。先收集再发:AsyncLoad 会改本表。
	void ReleaseLoadsAwaitingSaves()
	{
		if (loads_awaiting_saves_.empty())
		{
			return;
		}
		std::vector<ElementPtr> released;
		for (auto it = loads_awaiting_saves_.begin(); it != loads_awaiting_saves_.end();)
		{
			if (!HasUnsettledSave(it->second->message_key))
			{
				released.push_back(it->second);
				it = loads_awaiting_saves_.erase(it);
			}
			else
			{
				++it;
			}
		}
		for (const auto& element : released)
		{
			LOG_INFO << "AsyncLoad released after the pending save settled: " << element->redis_key;
			AsyncLoad(element->message_key, element->retry_count);
		}
	}

	void FlushDuePending(std::chrono::steady_clock::time_point now, bool force)
	{
		// Drain due loads
		std::vector<ElementPtr> dueLoads;
		dueLoads.reserve(pending_retry_queue_.size());
		for (auto it = pending_retry_queue_.begin(); it != pending_retry_queue_.end();)
		{
			if (force || it->second->next_retry_at <= now)
			{
				dueLoads.push_back(it->second);
				it = pending_retry_queue_.erase(it);
			}
			else
			{
				++it;
			}
		}

		// Drain due saves
		std::vector<ElementPtr> dueSaves;
		dueSaves.reserve(pending_save_queue_.size());
		for (auto it = pending_save_queue_.begin(); it != pending_save_queue_.end();)
		{
			if (force || it->second->next_retry_at <= now)
			{
				dueSaves.push_back(it->second);
				it = pending_save_queue_.erase(it);
			}
			else
			{
				++it;
			}
		}

		if (dueLoads.empty() && dueSaves.empty())
		{
			return;
		}
		LOG_INFO << "RetryDuePending: re-issuing " << dueLoads.size() << " loads, "
				 << dueSaves.size() << " saves "
				 << "(deferred loads=" << pending_retry_queue_.size()
				 << " saves=" << pending_save_queue_.size() << ")";
		for (auto &element : dueLoads)
		{
			AsyncLoad(element->message_key, element->retry_count);
		}
		for (auto &element : dueSaves)
		{
			IssueSave(element);
		}
	}

	// 一段 Lua 脚本在当前连接上的缓存状态。sha1 为空 = 尚未加载(发命令时走
	// EVAL 兜底),load_in_flight 挡住重复 SCRIPT LOAD。普通、旧 guard 与严格屏障
	// 各一份,因为 SHA 是按脚本文本算的。
	struct ScriptSlot
	{
		// 仅引用下方初始化的静态、以零结尾 Lua 字面量，生命周期长于连接。
		std::string_view source;
		std::string sha1;
		bool load_in_flight = false;
	};

	// Save 重载与严格屏障共用的入队逻辑;guardKey 与 barrierKey 都为空即无校验。
	//
	// 同 key 写入的不变量:在途(saving_queue_)至多一份,排队(pending_save_queue_)至多一份,
	// 且**排队的那份永远比在途的新**。成功回调(发排队值并压住本次回调)、ERROR 重试
	// (发排队值并丢掉失败的那份)、重连回收(排队值在就丢掉在途那份)三条路径都把"排队的"
	// 当成更新的值处理,正确性全靠这一条。写入排队的路径分两类:
	//   - 有在途时写入的新值(本函数首支、IssueSave 的在途防御分支放回):它就是最新快照;
	//   - 无在途时写入的值(本函数的顶替分支 / 未连接分支、IssueSave 的未连接分支、QueueSaveForRetry
	//     与 OnReconnected 各自在当时无排队值时):写入那一刻没有在途,不变量天然成立。
	// 新增入队路径时必须守住它 —— 历史上唯一破坏它的就是本函数"无在途但有排队旧值时直接发新值"
	// 那一支,见下方顶替分支与 docs/design/guild-phase2/08-save-owner-fence.md §8.2.2。
	// schemaGen 只在严格屏障路径非空(SaveWithCommitBarrier 已校验形状),其余路径传 nullptr。
	bool EnqueueSave(const MessageValuePtr& message, const MessageKey& key,
					 const std::string& guardKey, const std::string& guardExpected,
					 const std::string& barrierKey, const std::string& barrierVersion,
					 std::shared_ptr<const RedisSaveCommitIntent> intent, const RedisSaveSchemaGen* schemaGen,
					 CommittedCallback committed)
	{
		ElementPtr element = std::make_shared<Element>();
		element->message_key = key;
		element->redis_key = RedisKeyFor(key);
		element->message_value = message;
		element->guard_key = guardKey;
		element->guard_expected = guardExpected;
		element->barrier_key = barrierKey;
		element->barrier_version = barrierVersion;
		element->commit_intent = std::move(intent);
		if (schemaGen != nullptr)
		{
			element->schema_output_gen = std::to_string(schemaGen->output_gen);
			element->schema_load_gen = std::to_string(schemaGen->load_gen);
			element->schema_mode = schemaGen->mode;
			element->schema_writer = schemaGen->writer;
		}
		element->committed_callback = std::move(committed);

		// Serialize once; retry path re-uses this buffer.
		const size_t size = message->ByteSizeLong();
		element->serialized_payload.resize(size);
		if (size > 0 && !message->SerializeToArray(element->serialized_payload.data(), static_cast<int>(size)))
		{
			LOG_ERROR << "SerializeToArray failed for key " << key;
			return false;
		}

		// A key may have only one write in flight. If a periodic save is still
		// waiting for its reply when logout produces a newer full snapshot, keep
		// only that newer value and do not let the older completion finish logout.
		// This also prevents an old failed retry from overwriting a later success.
		if (saving_queue_.find(element->redis_key) != saving_queue_.end())
		{
			pending_save_queue_[element->redis_key] = element;
			return true;
		}

		// 没有在途,但排队里还有一份旧值(上次写失败后在等退避,或断线时停放):新值顶替它,并继承它的退避进度,
		// 由定时器(RetryDuePending)按原节奏发出,这里**不**直接发。
		// 直接发会让排队里留着更旧的值:新值落地后成功路径会把它当"更新的值"再发一遍、压住新值的
		// 回调,新值失败时 ERROR 路径会发它并丢掉新值,断线时重连回收会留它丢新值 —— 旧值盖掉新值
		// (同 key 新旧颠倒)。继承而不是重置退避:那份旧值刚失败,Redis 此刻多半仍不可写,立即发只会
		// 在同一个故障上再失败一次;save_failure_notified 一并继承,同一个 key 的同一段故障不重复告警。
		// 无论是否已连接都走这里:未连接时效果与下面那支相同(新值进排队),只是不丢退避进度。
		if (auto pending = pending_save_queue_.find(element->redis_key); pending != pending_save_queue_.end())
		{
			element->retry_count = pending->second->retry_count;
			element->next_retry_at = pending->second->next_retry_at;
			element->save_failure_notified = pending->second->save_failure_notified;
			pending->second = element;
			return true;
		}

		if (!hiredis_ || !hiredis_->connected())
		{
			LOG_WARN << "Redis not connected, queueing Save for retry: " << element->redis_key;
			// next_retry_at default-constructed (epoch); periodic timer will pick it up
			// as soon as Redis reconnects.
			pending_save_queue_[element->redis_key] = element;
			return true;
		}

		IssueSave(element);
		return true;
	}

	ScriptSlot& ScriptSlotFor(const Element& element)
	{
		if (!element.barrier_key.empty()) return barrier_script_;
		return element.guard_key.empty() ? save_script_ : guard_script_;
	}

	// Lazily load a Lua script and cache its SHA1 for EVALSHA.
	// Called eagerly from OnReconnected and lazily from IssueSave when sha is empty.
	void EnsureScriptLoaded(ScriptSlot& slot)
	{
		if (!slot.sha1.empty() || slot.load_in_flight)
		{
			return;
		}
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}
		slot.load_in_flight = true;
		hiredis_->command(std::bind(&MessageAsyncClient::OnScriptLoaded, this, std::placeholders::_1, std::placeholders::_2, std::ref(slot)),
						  "SCRIPT LOAD %s", slot.source.data());
	}

	void OnScriptLoaded(hiredis::Hiredis * /*c*/, redisReply *reply, ScriptSlot& slot)
	{
		slot.load_in_flight = false;
		if (!reply || reply->type != REDIS_REPLY_STRING)
		{
			LOG_ERROR << "SCRIPT LOAD failed for " << full_name()
					  << " (reply " << (reply ? std::to_string(reply->type) : std::string("null")) << "); will retry on next save";
			return;
		}
		slot.sha1.assign(reply->str, reply->len);
		LOG_INFO << "SCRIPT LOAD ok for " << full_name() << " sha1=" << slot.sha1;
	}

	// 按 element 的普通 / 旧 guard / 严格屏障路径选脚本与参数形状发出写命令。
	// useEvalSha=false 用于 SCRIPT LOAD 尚未完成、或刚收到 NOSCRIPT 的场合,
	// 此时把脚本全文随命令发出,保证不因缓存缺失而丢一次存盘。
	// 参数顺序是与 Lua 脚本的契约:普通版 KEYS[1]=数据键 / ARGV[1]=payload;
	// guard 版 KEYS[1]=数据键, KEYS[2]=guard 键 / ARGV[1]=payload, ARGV[2]=期望值。
	// 屏障版另加 KEYS[3]=屏障 hash、KEYS[4]=分桶索引;十一个 ARGV 顺序见严格 Lua 头注释。
	int SendSaveCommand(const ElementPtr &element, bool useEvalSha)
	{
		const hiredis::Hiredis::CommandCallback onSaved =
			std::bind(&MessageAsyncClient::OnSaved, this, std::placeholders::_1, std::placeholders::_2, element);
		const ScriptSlot& slot = ScriptSlotFor(*element);
		if (!element->barrier_key.empty())
		{
			const auto& intent = *element->commit_intent;
			const std::string member = std::to_string(element->message_key);
			if (useEvalSha)
			{
				return hiredis_->command(onSaved, "EVALSHA %b 4 %b %b %b %b %b %b %b %b %b %b %b %b %b %b %b",
					slot.sha1.data(), slot.sha1.size(),
					element->redis_key.data(), element->redis_key.size(),
					element->guard_key.data(), element->guard_key.size(),
					element->barrier_key.data(), element->barrier_key.size(),
					intent.index_key.data(), intent.index_key.size(),
					element->serialized_payload.data(), element->serialized_payload.size(),
					element->guard_expected.data(), element->guard_expected.size(),
					element->barrier_version.data(), element->barrier_version.size(),
					intent.topic.data(), intent.topic.size(),
					intent.player_database_task.data(), intent.player_database_task.size(),
					intent.player_database_1_task.data(), intent.player_database_1_task.size(),
					member.data(), member.size(),
					element->schema_output_gen.data(), element->schema_output_gen.size(),
					element->schema_load_gen.data(), element->schema_load_gen.size(),
					element->schema_mode.data(), element->schema_mode.size(),
					element->schema_writer.data(), element->schema_writer.size());
			}
			return hiredis_->command(onSaved, "EVAL %s 4 %b %b %b %b %b %b %b %b %b %b %b %b %b %b %b", slot.source.data(),
				element->redis_key.data(), element->redis_key.size(),
				element->guard_key.data(), element->guard_key.size(),
				element->barrier_key.data(), element->barrier_key.size(),
				intent.index_key.data(), intent.index_key.size(),
				element->serialized_payload.data(), element->serialized_payload.size(),
				element->guard_expected.data(), element->guard_expected.size(),
				element->barrier_version.data(), element->barrier_version.size(),
				intent.topic.data(), intent.topic.size(),
				intent.player_database_task.data(), intent.player_database_task.size(),
				intent.player_database_1_task.data(), intent.player_database_1_task.size(),
				member.data(), member.size(),
				element->schema_output_gen.data(), element->schema_output_gen.size(),
				element->schema_load_gen.data(), element->schema_load_gen.size(),
				element->schema_mode.data(), element->schema_mode.size(),
				element->schema_writer.data(), element->schema_writer.size());
		}
		if (element->guard_key.empty())
		{
			if (useEvalSha)
			{
				return hiredis_->command(onSaved,
										 "EVALSHA %b 1 %b %b",
										 slot.sha1.data(), slot.sha1.size(),
										 element->redis_key.c_str(), element->redis_key.length(),
										 element->serialized_payload.data(), element->serialized_payload.size());
			}
			return hiredis_->command(onSaved,
									 "EVAL %s 1 %b %b",
									 slot.source.data(),
									 element->redis_key.c_str(), element->redis_key.length(),
									 element->serialized_payload.data(), element->serialized_payload.size());
		}
		if (useEvalSha)
		{
			return hiredis_->command(onSaved,
									 "EVALSHA %b 2 %b %b %b %b",
									 slot.sha1.data(), slot.sha1.size(),
									 element->redis_key.c_str(), element->redis_key.length(),
									 element->guard_key.c_str(), element->guard_key.length(),
									 element->serialized_payload.data(), element->serialized_payload.size(),
									 element->guard_expected.c_str(), element->guard_expected.length());
		}
		return hiredis_->command(onSaved,
								 "EVAL %s 2 %b %b %b %b",
								 slot.source.data(),
								 element->redis_key.c_str(), element->redis_key.length(),
								 element->guard_key.c_str(), element->guard_key.length(),
								 element->serialized_payload.data(), element->serialized_payload.size(),
								 element->guard_expected.c_str(), element->guard_expected.length());
	}

	void IssueSave(const ElementPtr &element)
	{
		// Save() and the completion path serialize writes per key. Treat a second
		// issue defensively as a coalesced successor instead of allowing two SETs
		// whose retry order could roll Redis backwards.
		if (saving_queue_.find(element->redis_key) != saving_queue_.end())
		{
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		if (!hiredis_ || !hiredis_->connected())
		{
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		ScriptSlot& slot = ScriptSlotFor(*element);
		EnsureScriptLoaded(slot);

		saving_queue_[element->redis_key] = element;
		// Fall back to EVAL while SCRIPT LOAD is in flight or after a failure.
		const int ret = SendSaveCommand(element, /*useEvalSha=*/!slot.sha1.empty());

		if (ret != REDIS_OK)
		{
			LOG_ERROR << "Redis Save command failed (ret=" << ret << ") for key: " << element->redis_key;
			saving_queue_.erase(element->redis_key);
			QueueSaveForRetry(element);
		}
	}

	static constexpr int kMaxSaveRetries = 6;

	void QueueSaveForRetry(const ElementPtr &element)
	{
		// A later full snapshot supersedes this failed value. Issue the newer one
		// immediately; retrying the older payload first would both waste work and
		// create a stale-overwrite window.
		if (auto newer = pending_save_queue_.find(element->redis_key);
			newer != pending_save_queue_.end() && newer->second != element)
		{
			auto next = newer->second;
			pending_save_queue_.erase(newer);
			IssueSave(next);
			return;
		}

		if (element->retry_count >= kMaxSaveRetries && !element->save_failure_notified)
		{
			// 注意:这里**不能**指望 db 服务把缓存修回来。
			// scene 存的是 "<PlayerAllData full_name>:{id}",而 db 服务写回的是
			// login 读的分表 key("player_database:{id}" 等),两组 key 互不覆盖。
			// 所以不能在阈值处丢掉最新 payload 或让调用方销毁唯一内存态。
			// 这里只通知一次,随后以封顶退避继续重试,直到 Redis 恢复或进程
			// 进入有界停机流程。
			LOG_ERROR << "Save exhausted " << kMaxSaveRetries << " retries for key: " << element->redis_key
					  << " -- retaining the latest payload and retrying indefinitely with capped backoff. "
					  << "This key is NOT repaired by dbservice (different key space).";
			element->save_failure_notified = true;
			if (save_failed_callback_)
			{
				save_failed_callback_(element->message_key, element->redis_key, element->retry_count);
			}
		}
		const int nextRetry = std::min(element->retry_count + 1, kMaxSaveRetries);
		const auto backoff = BackoffForRetry(nextRetry);
		element->retry_count = nextRetry;
		element->next_retry_at = std::chrono::steady_clock::now() + backoff;
		// Latest pending value per key wins (Save with same key replaces older retry).
		pending_save_queue_[element->redis_key] = element;
		LOG_WARN << "Queued Save retry " << nextRetry << "/" << kMaxSaveRetries
				 << " for key: " << element->redis_key << " in " << backoff.count() << "ms";
	}

	// 在途写被原子拒绝(owner guard 返回 0 或严格屏障 {4, 存储值},或严格屏障 C3 enforce 返回 3)之后,同 key 排队值的处置:
	// 排队值的 guard 期望值与被拒的不同 → 调用方在这笔写在途期间拿到了更新的归属,照常发出它并返回 true,
	// 调用方此时不回调(那笔旧写已被它取代,由它的结果决定后续);相同 → 同一份已作废的期望值,一并丢弃,返回 false。
	// sameWriterOnly(C3 用):排队值的写方令牌与被拒的不同时同样照发 —— 它来自另一个加载实体,C3 的判定只看
	// 写方自己的加载世代,被拒的那份不能替它下结论;owner 拒绝只看 guard,不传。
	// 调用前在途记录必须已摘除。
	bool IssuePendingWithNewerGuard(const ElementPtr& element, const char* rejectedBy, bool sameWriterOnly = false)
	{
		auto newer = pending_save_queue_.find(element->redis_key);
		if (newer == pending_save_queue_.end())
		{
			return false;
		}
		auto next = newer->second;
		pending_save_queue_.erase(newer);
		const bool otherWriter = sameWriterOnly && next->schema_writer != element->schema_writer;
		if (next->guard_expected == element->guard_expected && !otherWriter)
		{
			return false;
		}
		LOG_WARN << "Redis Save rejected by " << rejectedBy << " for key: " << element->redis_key
				 << " expected=" << element->guard_expected
				 << " -- superseded by a pending value with newer guard=" << next->guard_expected
				 << (otherWriter ? " from another loaded entity" : "") << ", issuing it";
		IssueSave(next);
		return true;
	}

	// owner 归属校验拒绝(Lua 返回 0,或严格屏障返回 {4, 存储值})的共同处置。调用前在途记录必须已摘除。
	// 两种回包的处置完全相同,只有 rejection 里的成因与存储值不同:同 guard 的排队值一并丢弃后回调一次;
	// 排队值带着更新的 guard 就照发、不回调(见 IssuePendingWithNewerGuard)。
	void RejectSaveByOwner(const ElementPtr& element, const RedisSaveOwnerRejection& rejection)
	{
		const bool counterBehind = rejection.reason == RedisSaveOwnerRejectReason::kOwnerCounterBehind;
		if (IssuePendingWithNewerGuard(element, counterBehind ? "owner guard (counter behind)" : "owner guard"))
		{
			return;
		}
		LOG_WARN << "Redis Save rejected by owner guard for key: " << element->redis_key
				 << " guard=" << element->guard_key << " expected=" << element->guard_expected
				 << (counterBehind ? " stored=" + rejection.stored_owner + " (owner counter behind the expected value)"
								   : std::string())
				 << " -- dropping in-flight and same-guard pending values";
		if (save_rejected_callback_)
		{
			save_rejected_callback_(element->message_key, element->redis_key, element->guard_expected, rejection);
		}
	}

	void OnSaved(hiredis::Hiredis * /*c*/, redisReply *reply, ElementPtr element)
	{
		// Ignore a callback from a connection that was superseded during
		// reconnect. The current in-flight/pending value owns this key now.
		auto inFlight = saving_queue_.find(element->redis_key);
		if (inFlight == saving_queue_.end() || inFlight->second != element)
		{
			LOG_WARN << "Ignoring stale Redis Save callback for key: " << element->redis_key;
			return;
		}

		if (!reply)
		{
			LOG_ERROR << "Redis Save: null reply for key: " << element->redis_key;
			saving_queue_.erase(inFlight);
			QueueSaveForRetry(element);
			return;
		}
		if (reply->type == REDIS_REPLY_ERROR)
		{
			const std::string err = reply->str ? reply->str : "";
			// NOSCRIPT: server flushed scripts (FLUSHALL/SCRIPT FLUSH/restart) -> reload and retry as EVAL once.
			// 只清 element 自己用的那个槽位:三份脚本独立缓存,其余未必也丢了。
			if (err.compare(0, 8, "NOSCRIPT") == 0)
			{
				LOG_WARN << "EVALSHA NOSCRIPT for key: " << element->redis_key << " -- reloading script and retrying";
				ScriptSlot& slot = ScriptSlotFor(*element);
				slot.sha1.clear();
				EnsureScriptLoaded(slot);
				// Immediate retry as EVAL (do not increment retry_count for this case).
				const int ret = SendSaveCommand(element, /*useEvalSha=*/false);
				if (ret != REDIS_OK)
				{
					saving_queue_.erase(element->redis_key);
					QueueSaveForRetry(element);
				}
				return;
			}
			LOG_ERROR << "Redis Save error for key: " << element->redis_key << " err=" << err;
			saving_queue_.erase(inFlight);
			QueueSaveForRetry(element);
			return;
		}

		// 带 guard 的脚本返回整数 0 = 这份期望值的归属校验不通过。
		// 不原样重试(再存只会再被拒),不走 save_failed_callback_(那条语义是
		// "Redis 不可用,仍在重试"),也不发布 save_callback_(调用方会把它当成功并
		// 继续退出链)。
		// 同 key 排队中的值按期望值分两种:与被拒的相同 → 同一份已作废的归属,丢弃
		// (发出去只会再收一个 0,却多开一次覆盖窗口);不同 → 调用方在这笔写在途期间
		// 拿到了更新的归属,那份更新的快照照常发出,由它的结果决定后续,这里不回调。
		// 只对带 guard 的 element 解释这个 0:普通脚本恒返回 1,其它回包必须按故障重试。
		if (!element->guard_key.empty() && reply->type == REDIS_REPLY_INTEGER && reply->integer == 0)
		{
			saving_queue_.erase(inFlight);
			RejectSaveByOwner(element, RedisSaveOwnerRejection{});
			return;
		}

		// 严格屏障返回 {4, 存储值} = Redis 里的 owner 计数器比这份期望值还小(Redis 回退丢了铸造,
		// docs/design/zone-redis-regression-guard-20261022.md §4.4),一个键都没动。处置与上面的 0 完全相同
		// (不重试、不执行提交钩子、不发布 save_callback_,同 guard 排队值一并丢弃),只是把成因与存储值带给调用方。
		// 本类不前推计数器、不换期望值重发,理由见严格 Lua 头注释。只对屏障元素解释这种回包;形状不符按坏回包重试。
		if (RedisSaveOwnerRejection behind{RedisSaveOwnerRejectReason::kOwnerCounterBehind, {}};
			!element->barrier_key.empty() && ParseRedisOwnerCounterBehindReply(reply, behind.stored_owner))
		{
			saving_queue_.erase(inFlight);
			RejectSaveByOwner(element, behind);
			return;
		}

		// 严格屏障返回 3 = enforce 模式下覆盖世代校验 C3 判定倒退、拒写(一个键都没动)。只对屏障元素解释 3。
		// 处置同上面的 0:不重试(同一份数据再发只会再被拒)、不执行提交钩子、不发布 save_callback_;
		// 同 key 排队值 guard 与写方令牌都相同才一并丢弃(同一实体、同一归属,再发只会再被拒),
		// 否则照发、不回调(它带着更新的归属,或来自另一个加载实体,由它自己的结果决定后续)。
		if (!element->barrier_key.empty() && reply->type == REDIS_REPLY_INTEGER && reply->integer == 3)
		{
			saving_queue_.erase(inFlight);
			if (IssuePendingWithNewerGuard(element, "persist schema regression check", /*sameWriterOnly=*/true))
			{
				return;
			}
			LOG_ERROR << "Redis Save refused by persist schema regression check (enforce) for key: " << element->redis_key
					  << " expected=" << element->guard_expected << " output_gen=" << element->schema_output_gen
					  << " load_gen=" << element->schema_load_gen
					  << " -- nothing written, dropping in-flight and same-guard pending values";
			if (save_schema_rejected_callback_)
			{
				save_schema_rejected_callback_(element->message_key, element->redis_key, element->guard_expected);
			}
			return;
		}

		// 只有整数 1 是提交证据,不能把损坏 / 未知回包送进 SQL 准入钩子。唯一的例外是严格屏障的 2:
		// warn 模式下 C3 判定倒退但已照写,同样是提交证据,另外通知一次用于计数与日志。
		const bool schemaRegressionWarned =
			!element->barrier_key.empty() && reply->type == REDIS_REPLY_INTEGER && reply->integer == 2;
		if (reply->type != REDIS_REPLY_INTEGER || (reply->integer != 1 && !schemaRegressionWarned))
		{
			LOG_ERROR << "Redis Save received no commit acknowledgement for key: " << element->redis_key;
			saving_queue_.erase(inFlight);
			QueueSaveForRetry(element);
			return;
		}
		if (schemaRegressionWarned)
		{
			if (save_schema_warned_callback_)
			{
				save_schema_warned_callback_(element->message_key, element->redis_key, element->schema_writer);
			}
			else
			{
				LOG_WARN << "Redis Save committed despite persist schema regression (warn) for key: " << element->redis_key
						 << " output_gen=" << element->schema_output_gen << " load_gen=" << element->schema_load_gen;
			}
		}
		// 已提交的父快照必须有对应 SQL 任务;更新值尚未成功,不能拿它代替本次恢复证据。
		// 只丢弃未下发就被合并的快照钩子。先移动确保本次钩子至多执行一次。
		if (element->committed_callback)
		{
			auto committed = std::move(element->committed_callback);
			committed();
		}
		// 钩子可能重入 Save,甚至触发 map 扩容;不能跨钩子使用旧 iterator。
		// 若钩子使连接切换并转移了本次所有权,由替代请求决定后续,不删它也不发布旧完成。
		auto committedInFlight = saving_queue_.find(element->redis_key);
		if (committedInFlight == saving_queue_.end() || committedInFlight->second != element)
		{
			return;
		}
		saving_queue_.erase(committedInFlight);
		// Do not publish completion for an older full snapshot when a newer one
		// is waiting. In the Scene lifecycle that callback may destroy an exiting
		// entity, so only the newest successfully persisted snapshot may complete
		// the chain.
		if (auto newer = pending_save_queue_.find(element->redis_key);
			newer != pending_save_queue_.end())
		{
			auto next = newer->second;
			pending_save_queue_.erase(newer);
			IssueSave(next);
			return;
		}
		if (save_callback_)
		{
			save_callback_(element->message_key, *element->message_value);
		}
	}

	void OnLoaded(hiredis::Hiredis* /*c*/, redisReply* reply, ElementPtr element)
	{
		// 重连可能淘汰旧 GET，并为同一 key 发出新请求；旧连接的迟到回包
		// 不能删除或完成这个替代请求。
		auto inFlight = loading_queue_.find(element->redis_key);
		if (inFlight == loading_queue_.end() || inFlight->second != element)
		{
			LOG_WARN << "Ignoring stale Redis Load callback for key: " << element->redis_key;
			return;
		}
		loading_queue_.erase(inFlight);

		if (!reply)
		{
			// 空应答 = 连接断开 / 被释放时 hiredis 对在途命令的一次性回调(主切换掐连接、存活探测摘连接、停机):
			// GET 可能执行了也可能没执行,这不是「读失败」的结论。GET 只读、可以重发,所以放回重试队列,
			// 不计 NIL 重试次数;重连后 OnReconnected 立刻重发,之后由周期定时器兜底 —— 与 AsyncLoad 在未连接时
			// 排队是同一语义。以前这里直接 FailLoad(RedisError),玩家被踢回登录界面,只能手动重登(只影响活性)。
			// 同 key 已有排队项(断线期间又调过 AsyncLoad)时保留那一项,只把重试次数抬到两者较大者,
			// 免得 NIL 退避预算被外部重触发重置(与 AsyncLoad 合并排队项的规则一致)。
			LOG_WARN << "Redis GET got no reply (connection lost) for key: " << element->redis_key
					 << ", requeued for retry after reconnect";
			element->next_retry_at = std::chrono::steady_clock::now();
			const auto [queued, inserted] = pending_retry_queue_.try_emplace(element->redis_key, element);
			if (!inserted && queued->second->retry_count < element->retry_count)
			{
				queued->second->retry_count = element->retry_count;
			}
			return;
		}
		if (reply->type == REDIS_REPLY_ERROR)
		{
			// ERROR 是服务端的答复(WRONGTYPE 等),重发也是同一个答复:照旧 fail-closed。
			const std::string errorDetail = std::string(" err=") + (reply->str != nullptr
				? std::string(reply->str, reply->len)
				: std::string("<missing error text>"));
			LOG_ERROR << "Redis GET error for key: " << element->redis_key
					  << errorDetail;
			FailLoad(element, LoadFailureReason::RedisError);
			return;
		}

		if (reply->type == REDIS_REPLY_NIL)
		{
			if (element->retry_count < kMaxLoadRetries)
			{
				const int nextRetry = element->retry_count + 1;
				const auto backoff = BackoffForRetry(nextRetry);
				LOG_WARN << "Redis GET returned NIL for key: " << element->redis_key
						 << ", retry " << nextRetry << "/" << kMaxLoadRetries
						 << " in " << backoff.count() << "ms";
				element->retry_count = nextRetry;
				element->next_retry_at = std::chrono::steady_clock::now() + backoff;
				pending_retry_queue_[element->redis_key] = element;
				return;
			}

			LOG_ERROR << "Redis GET returned NIL for key: " << element->redis_key
					  << ", exhausted all " << kMaxLoadRetries << " retries";
			FailLoad(element, LoadFailureReason::DataNotFound);
			return;
		}

		if (reply->type == REDIS_REPLY_STRING)
		{
			if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
			{
				LOG_ERROR << "Redis payload too large for ParseFromArray, key: " << element->redis_key
						  << ", len=" << reply->len;
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}
			if (reply->len == 0 || reply->str == nullptr)
			{
				// Scene 保存的 PlayerAllData 必含 player_id；已存在的零字节值是损坏，
				// 不是“新玩家”信号。只有有界重试后的 REDIS_REPLY_NIL 才表示不存在。
				LOG_ERROR << "Redis payload is empty/invalid for key: " << element->redis_key;
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}

			element->message_value = CreateMessage();
			if (!element->message_value->ParseFromArray(reply->str, static_cast<int>(reply->len)))
			{
				LOG_ERROR << "ParseFromArray failed for key: " << element->redis_key;
				// 默认或部分解析的 proto 绝不能作为成功结果发布。Scene 会把
				// player_id=0 当作新角色，随后可能用默认值覆盖权威玩家状态。
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}
		}
		else
		{
			LOG_ERROR << "Redis GET unexpected reply type=" << reply->type << " for key: " << element->redis_key;
			FailLoad(element, LoadFailureReason::RedisError);
			return;
		}

		if (load_callback_)
		{
			load_callback_(element->message_key, *element->message_value);
		}
	}

	void FailLoad(const ElementPtr &element, LoadFailureReason reason)
	{
		// 终态回包不应再持有重试项；回调前防御性清理，保证回调若重入重试，
		// 也是从干净状态开始。
		pending_retry_queue_.erase(element->redis_key);
		element->message_value.reset();
		if (load_failed_callback_)
		{
			load_failed_callback_(element->message_key, reason);
		}
	}

private:
	template <class, class>
	friend struct MessageAsyncClientTestPeer;

	HiredisPtr& hiredis_;
	LoadingQueue loading_queue_;
	LoadingQueue pending_retry_queue_;
	// 因同 key 存盘未落定而挂起的加载(见 AsyncLoad);RetryDuePending 每拍检查,落定即发 GET。
	LoadingQueue loads_awaiting_saves_;
	LoadingQueue saving_queue_;
	LoadingQueue pending_save_queue_;
	ScriptSlot save_script_{kSaveLuaScript};
	ScriptSlot guard_script_{kSaveIfGuardLuaScript};
	ScriptSlot barrier_script_{kSaveWithCommitBarrierLuaScript};
	EventCallback save_callback_;
	SaveFailedCallback save_failed_callback_;
	SaveRejectedCallback save_rejected_callback_;
	SaveSchemaWarnedCallback save_schema_warned_callback_;
	SaveSchemaRejectedCallback save_schema_rejected_callback_;
	EventCallback load_callback_;
	FailedCallback load_failed_callback_;
};
