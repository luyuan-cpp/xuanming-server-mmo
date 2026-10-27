package kafka

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 重试收据的归属(docs/design/no-single-node-horizontal-scaling-20261001.md §3)。
//
// 背景:重试任务从 ready 列表被「认领」到 processing 列表,做完才从 processing 里删掉;进程崩溃时留在
// processing 里的那份就是恢复 at-least-once 的唯一依据。以前 processing 是**全 zone 共用的一个列表**,
// 进程启动时把它整表搬回 ready —— 这只在「同一个 zone 只有一个 db 实例」时才对:第二个实例一启动,就把第一个
// 实例正在处理的收据全部搬走重做(结果是重复执行,不会写坏库,但每次有实例重启都会把全区在途的重试重放一遍,
// 而且「每条收据恰有一个认领者」不成立)。
//
// 现在:
//   - 每个实例认领到**自己的** processing 列表 kafka:retry:processing:{topic}:{instanceID};
//   - 实例在 ZSET kafka:retry:instances:{topic} 里登记租约(member = instanceID,score = 到期时刻的毫秒,
//     一律取 Redis 的 TIME,不用各进程自己的时钟),定期续租;
//   - 租约过期的实例,它名下的收据由任何一个活着的实例搬回 ready(孤儿回收);
//   - 旧版共享列表 kafka:retry:processing:{topic} 只回收、不再写入(滚动升级期间旧版实例还在用它)。
//
// 不变量:**一个实例的 processing 列表只在它持有未过期租约的那一刻才会增长**。认领脚本在同一个原子操作里
// 先续租再认领,所以不存在「已被摘出登记表、却还在往自己列表里认领」的实例 —— 那样的列表没人会来回收。
// 回收脚本只在列表搬空之后才把实例从登记表里摘掉,同样是原子的。
//
// 语义仍是 at-least-once:任一时刻每条未完成的收据至少在 ready、某个实例的 processing、死信三者之一。
// 误回收(实例其实活着,只是租约断了)的后果是重复执行,由写任务的 ordering lock + applied 游标挡住。
//
// 回退到旧版二进制时的注意事项:旧版看不到按实例的列表。正常停机(SIGTERM)时 release 已把收据还回 ready;
// 若新版是崩溃退出,回退前要先起一个新版实例让它回收,或手工把 kafka:retry:processing:{topic}:* 搬回 ready。
//
// 重试只交给分区属主(RetryRouting=owner,retry_routing.go)之后,ready 拆成共享「未分拣收件箱」
// kafka:retry:queue:{topic} 与按原始分区的 kafka:retry:queue:{topic}:p{N}。认领可以从任一列表来(claimFrom),
// 但回收、停机归还、旧共享列表仍一律搬回共享收件箱,由 owner 实例分拣到 p{origin}:回收与路由解耦,本文件的回收脚本
// 不认识 p 键,也不需要认识。回退到不认识 p* 的旧版之前,须先切 RetryRouting=shared 并等 p* 归零,或执行
// go/db/cmd/retry_requeue 把 p* 搬回共享收件箱(docs/design/db-retry-owner-routing.md §10)。
//
// 认领的幂等(R7 复核):主池客户端用 go-redis 缺省的 MaxRetries=3,读超时或 EOF 时会把 EVAL 原样重发,而第一次发出的
// 那份可能已经在服务端执行完(回包丢了;旧连接缓冲区里的命令在 Redis 恢复后照样执行)。认领脚本不幂等时,两次 RPOPLPUSH
// 会把第一次认领到的收据留在本实例 processing 里、无人派发,本实例一直续租、别人不回收,直到重启。所以每次认领带一个
// 随机 nonce:第一次执行把认领到的元素的「长度:SHA1」记进 kafka:retry:guard:claim:{processing}:{nonce}(retryGuardTTL),
// 同一个 nonce 再执行时不再 POP,只在 processing 里按它找回那一个元素返回。调用方拿到错误(结果未知)时把 nonce
// 记进 unresolved,下一拍认领之前用 retryClaimResolveScript 把可能已认领的那一个原样搬回原 ready(resolveUnresolvedClaims);
// 解决之后记录改写成 resolved,迟到执行的旧副本也不会再 POP。残余:Redis 卡顿或不可达超过 retryGuardTTL,记录过期后
// 迟到的旧副本会再 POP 一次,第一次认领到的那条照旧滞留到重启。
//
// 实例的生效模式(R7 复核):登记表只有实例名,回滚工具(go/db/cmd/retry_requeue)无从证明「没有 owner 实例活着」。
// 续租与认领脚本在 ZADD 的同一个原子操作里把本实例的生效模式写进 HASH kafka:retry:instance_mode:{topic}
// (routingMode 为空时不写,即直接拼结构体的单测);回收脚本摘掉登记时一并 HDEL。不写这个 HASH 的旧镜像一律视为
// 「模式未知」:retry_requeue 只能看见本版本起的 owner 实例。

const (
	// defaultRetryLeaseSeconds 是 Kafka.RetryLeaseSeconds 未配置(0)时的租约时长;
	// 续租与孤儿回收的周期取它的三分之一,所以崩溃实例的收据最迟约 lease + lease/3 之后回到 ready。
	defaultRetryLeaseSeconds = 30
	// minRetryLeaseSeconds:再短,一次 GC 停顿或 Redis 抖动就会被判成"实例已死"。
	minRetryLeaseSeconds = 10

	// retryReclaimBatch 是回收脚本一次调用最多搬的收据数(脚本执行期间 Redis 是阻塞的,不能一次搬完一个大列表)。
	retryReclaimBatch = 100
	// retryReclaimMaxBatchesPerInstance 限定一拍里替同一个实例最多调几次脚本;搬不完下一拍继续。
	retryReclaimMaxBatchesPerInstance = 100
	// retryLegacyDrainPerTick 限定一拍里从旧版共享列表最多搬多少条。
	retryLegacyDrainPerTick = 500
	// retryExpiredScanLimit 限定一拍里最多处理多少个过期实例。
	retryExpiredScanLimit = 32

	// retryOwnershipTickTimeout 是一拍(续租 + 回收)的时间预算。
	retryOwnershipTickTimeout = 5 * time.Second
	// retryReleaseTimeout 是停机时归还收据的时间预算;超时也无妨,租约过期后别的实例会回收。
	retryReleaseTimeout = 5 * time.Second

	// retryGuardTTL 是认领记录与收据 ack 防重记录(kafka:retry:guard:*)的存活时间:要盖住 go-redis 的内部重发
	// (MaxRetries=3,每次最多一个 ReadTimeout 3s,约 13s)、下一拍的 resolve,以及 Redis 卡顿期间旧连接里迟到执行的
	// 副本。记录很小(「长度:SHA1」或一个整数,连键名约 150 字节),最坏每实例约「每拍 200 次认领 + 同量 ack」× 300s 条
	// (满速排空 5 分钟约 12 万条、十几 MB);只有重试链路有流量时才产生。
	retryGuardTTL = 5 * time.Minute
	// retryClaimResolved 是认领记录被 resolve 之后的值:同一个 nonce 再执行(迟到的旧副本)不再 POP、也不再返回元素。
	// retryClaimScript / retryClaimResolveScript 里写的是同一个字面量,改一处要同改。
	retryClaimResolved = "resolved"
)

func retryInstancesKey(topic string) string { return "kafka:retry:instances:" + topic }

// retryInstanceModesKey 是实例 → 生效模式(owner / shared)的 HASH,与登记表同生共死(见文件头)。
// go/db/cmd/retry_requeue 经 RetryQueueDepths 读它判定「是否还有 owner 实例登记着」。
func retryInstanceModesKey(topic string) string { return "kafka:retry:instance_mode:" + topic }

// retryGuardKey 是一次认领(kind=claim)或一次收据 ack(kind=ack)的防重记录。前缀 kafka:retry:guard: 与队列键
// (kafka:retry:queue: / kafka:retry:processing:)不相交,按队列前缀 SCAN 的工具(tools/merge_zone、人工搬运)不会扫到它。
func retryGuardKey(kind, processingKey, nonce string) string {
	return "kafka:retry:guard:" + kind + ":" + processingKey + ":" + nonce
}

// newRetryGuardNonce 返回一次调用的随机 nonce(16 个十六进制字符)。不用进程内计数器:同名实例重启后计数器从头来,
// 会撞上上一个进程留下、尚未过期的记录。
func newRetryGuardNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("retry guard nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// retryLegacyProcessingKey 是旧版(单实例时代)全 zone 共用的 processing 列表。
func retryLegacyProcessingKey(topic string) string { return "kafka:retry:processing:" + topic }

func retryInstanceProcessingKey(topic, instanceID string) string {
	return retryLegacyProcessingKey(topic) + ":" + instanceID
}

// resolveRetryInstanceID 决定本实例在登记表里的名字。显式配置优先;否则取 <主机名>:<监听端口>:
// K8s 上主机名就是 Pod 名(Pod 重建即换名,同名只可能是同一个 Pod 的容器重启);本机多开时端口不同。
// 同名意味着"旧进程必然已死",所以启动时可以直接收回同名实例遗留的收据 —— 两个活着的实例配成同名
// 会互相收走对方的在途收据(后果是重复执行)。
func resolveRetryInstanceID(configured, hostname, listenOn string) (string, error) {
	if id := strings.TrimSpace(configured); id != "" {
		return id, nil
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", errors.New("retry instance id: hostname is unavailable and Kafka.RetryInstanceId is not configured")
	}
	port := strings.TrimSpace(listenOn)
	if _, p, err := net.SplitHostPort(port); err == nil {
		port = p
	}
	if port == "" {
		return "", errors.New("retry instance id: ListenOn is empty and Kafka.RetryInstanceId is not configured")
	}
	return hostname + ":" + port, nil
}

// retryLeaseDuration 把配置的秒数折算成租约时长:0(未配置)取默认,过小的值抬到下限。
func retryLeaseDuration(configuredSeconds int) time.Duration {
	seconds := configuredSeconds
	if seconds <= 0 {
		seconds = defaultRetryLeaseSeconds
	}
	if seconds < minRetryLeaseSeconds {
		seconds = minRetryLeaseSeconds
	}
	return time.Duration(seconds) * time.Second
}

// retryLeaseScript 续租(也用于首次登记)。到期时刻取 Redis 的 TIME。ARGV[3] 非空时同一原子操作里登记生效模式。
// KEYS[1] = 登记表,KEYS[2] = 模式 HASH;ARGV[1] = instanceID,ARGV[2] = 租约毫秒数,ARGV[3] = 生效模式(空串 = 不登记)。
const retryLeaseScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZADD', KEYS[1], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
if ARGV[3] ~= '' then
	redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
end
return 1`

// retryClaimScript 先续租(含登记生效模式)、再把一条收据从 ready 原子地认领到本实例的 processing 列表;ready 为空时
// 返回 nil。续租与认领必须在同一个脚本里(见文件头的不变量)。
//
// 按 nonce 幂等(文件头「认领的幂等」):KEYS[4] 不存在时 RPOPLPUSH,认领到元素就把「长度:SHA1」写进 KEYS[4];KEYS[4]
// 已存在(go-redis 的重发、迟到的旧副本)时不再 POP:值为 resolved 返回 nil,否则在 processing 里找回那个元素返回
// (先看队头 —— RPOPLPUSH 推在队头,重发紧跟在首发之后时它还在那里;再逐个比,长度不同的不算 SHA1),找不到返回 nil。
// KEYS[1] = ready,KEYS[2] = 本实例的 processing,KEYS[3] = 登记表,KEYS[4] = 本次认领的记录,KEYS[5] = 模式 HASH;
// ARGV[1] = instanceID,ARGV[2] = 租约毫秒数,ARGV[3] = 记录毫秒数,ARGV[4] = 生效模式(空串 = 不登记)。
const retryClaimScript = `
local function matches(v, rec)
	local sep = string.find(rec, ':', 1, true)
	if not sep or string.len(v) ~= tonumber(string.sub(rec, 1, sep - 1)) then
		return false
	end
	return redis.sha1hex(v) == string.sub(rec, sep + 1)
end
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZADD', KEYS[3], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
if ARGV[4] ~= '' then
	redis.call('HSET', KEYS[5], ARGV[1], ARGV[4])
end
local rec = redis.call('GET', KEYS[4])
if rec then
	if rec == 'resolved' then
		return false
	end
	local head = redis.call('LINDEX', KEYS[2], 0)
	if head and matches(head, rec) then
		return head
	end
	for _, v in ipairs(redis.call('LRANGE', KEYS[2], 0, -1)) do
		if matches(v, rec) then
			return v
		end
	end
	return false
end
local v = redis.call('RPOPLPUSH', KEYS[1], KEYS[2])
if v then
	redis.call('SET', KEYS[4], string.len(v) .. ':' .. redis.sha1hex(v), 'PX', ARGV[3])
end
return v`

// retryClaimResolveScript 处理一次结果未知的认领(retryOwnership.resolveUnresolvedClaims):记录不存在(那次认领没执行,
// 或执行了但 ready 是空的)或已是 resolved 时返回 0;否则在 processing 里按「长度:SHA1」找到那次认领到的元素,原字节
// RPUSH 回原 ready 的队尾(认领从队尾取,下一次就轮到它)再 LREM,返回 1;找不到(已被别的实例回收)返回 0。动手之前核对
// ready 的类型,类型不对整体报错、记录保持原样。处理完把记录改写成 resolved,迟到的旧副本与本脚本的重发都不再动任何东西。
// KEYS[1] = 认领记录,KEYS[2] = 本实例的 processing,KEYS[3] = 认领时的 ready;ARGV[1] = 记录毫秒数。
const retryClaimResolveScript = `
local function matches(v, rec)
	local sep = string.find(rec, ':', 1, true)
	if not sep or string.len(v) ~= tonumber(string.sub(rec, 1, sep - 1)) then
		return false
	end
	return redis.sha1hex(v) == string.sub(rec, sep + 1)
end
local rec = redis.call('GET', KEYS[1])
if not rec then return -1 end -- 防重记录缺失不能证明原命令未执行。
if rec == 'resolved' then
	return 0
end
local t = redis.call('TYPE', KEYS[3]).ok
if t ~= 'list' and t ~= 'none' then
	return redis.error_reply('WRONGTYPE retry claim resolve refused: ' .. KEYS[3] .. ' is a ' .. t)
end
local found = false
local head = redis.call('LINDEX', KEYS[2], 0)
if head and matches(head, rec) then
	found = head
else
	for _, v in ipairs(redis.call('LRANGE', KEYS[2], 0, -1)) do
		if matches(v, rec) then
			found = v
			break
		end
	end
end
redis.call('SET', KEYS[1], 'resolved', 'PX', ARGV[1])
if not found then
	return 0
end
redis.call('RPUSH', KEYS[3], found)
redis.call('LREM', KEYS[2], 1, found)
return 1`

// retryReclaimScript 把某个实例名下的收据搬回 ready(一次最多 ARGV[2] 条),列表搬空后把它从登记表里摘掉(连同模式 HASH
// 里的那一项)。ARGV[3] = '1' 时先复核租约:对方在此期间续上了就一条都不动,返回 -1。返回值否则是本次搬的条数。
// KEYS[1] = 登记表,KEYS[2] = 该实例的 processing,KEYS[3] = ready,KEYS[4] = 模式 HASH;ARGV[1] = 该实例的 instanceID。
const retryReclaimScript = `
if ARGV[3] == '1' then
	local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
	if score then
		local t = redis.call('TIME')
		local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
		if tonumber(score) > now then
			return -1
		end
	end
end
local moved = 0
for i = 1, tonumber(ARGV[2]) do
	local v = redis.call('RPOPLPUSH', KEYS[2], KEYS[3])
	if not v then
		break
	end
	moved = moved + 1
end
if redis.call('LLEN', KEYS[2]) == 0 then
	redis.call('ZREM', KEYS[1], ARGV[1])
	redis.call('HDEL', KEYS[4], ARGV[1])
end
return moved`

// retryOwnership 管本实例对重试收据的归属:认领、续租、回收孤儿、停机归还。
// 归属状态都在 Redis 里;进程内只有 unresolved(结果未知、待 resolve 的认领,mu 保护),所以可以被多个 goroutine 同时使用。
// routingMode 在构造之后、start 之前设定一次,之后只读。
type retryOwnership struct {
	rc         redis.Cmdable
	topic      string
	instanceID string
	lease      time.Duration
	// routingMode 是本实例的生效模式(metrics.RetryModeOwner / RetryModeShared),续租与认领时登记进模式 HASH;
	// 空串不登记(直接拼结构体的单测)。
	routingMode string

	readyKey      string
	processingKey string // 本实例的 processing 列表
	legacyKey     string // 旧版共享 processing 列表:只回收,不写入
	instancesKey  string
	modesKey      string // 实例 → 生效模式的 HASH

	mu         sync.Mutex
	unresolved []unresolvedClaim
}

// unresolvedClaim 是一次结果未知的认领:EVAL 返回了错误(超时、断连、ctx 取消),脚本可能已经执行、收据可能已经在
// processing 里却没有派发。由 resolveUnresolvedClaims 在之后的拍里处理。
type unresolvedClaim struct {
	readyKey  string
	recordKey string
}

func newRetryOwnership(rc redis.Cmdable, topic, readyKey, instanceID string, lease time.Duration) *retryOwnership {
	return &retryOwnership{
		rc:            rc,
		topic:         topic,
		instanceID:    instanceID,
		lease:         lease,
		readyKey:      readyKey,
		processingKey: retryInstanceProcessingKey(topic, instanceID),
		legacyKey:     retryLegacyProcessingKey(topic),
		instancesKey:  retryInstancesKey(topic),
		modesKey:      retryInstanceModesKey(topic),
	}
}

// renewLease 登记或续租(同时登记生效模式)。
func (o *retryOwnership) renewLease(ctx context.Context) error {
	return o.rc.Eval(ctx, retryLeaseScript, []string{o.instancesKey, o.modesKey},
		o.instanceID, o.lease.Milliseconds(), o.routingMode).Err()
}

// noteUnresolvedClaim 登记一次结果未知的认领(见 unresolvedClaim)。
func (o *retryOwnership) noteUnresolvedClaim(readyKey, recordKey string) {
	o.mu.Lock()
	o.unresolved = append(o.unresolved, unresolvedClaim{readyKey: readyKey, recordKey: recordKey})
	o.mu.Unlock()
}

// unresolvedClaimCount 是此刻待 resolve 的认领数(单测与日志用)。
func (o *retryOwnership) unresolvedClaimCount() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.unresolved)
}

// resolveUnresolvedClaims 逐条处理结果未知的认领:那次认领若已把收据搬进本实例 processing,就原字节搬回它原来的
// ready(retryClaimResolveScript),由下一次正常认领派发。重试协程每拍在认领之前调用(consumeRetryQueue)。
//
// 返回 false 表示还有没处理完的(Redis 仍然出错):调用方本拍不再分拣、认领,免得 Redis 不可用期间每拍再添一批结果未知的
// 认领。nil 接收者(直接拼结构体的单测没有归属登记)返回 true。脚本可重入(处理过的记录改写成 resolved),网络类错误时
// 出错的那条与其后的原样留到下一拍;Redis 的错误回复(确定性拒绝)只打 ERROR 并放弃那一条,不挡后面的认领。
func (o *retryOwnership) resolveUnresolvedClaims(ctx context.Context) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	pending := o.unresolved
	o.unresolved = nil
	o.mu.Unlock()
	if len(pending) == 0 {
		return true
	}
	for i, u := range pending {
		returned, err := o.rc.Eval(ctx, retryClaimResolveScript, []string{u.recordKey, o.processingKey, u.readyKey},
			retryGuardTTL.Milliseconds()).Int64()
		if err == nil && returned < 0 {
			err = errors.New("结果未知认领的原防重记录已缺失，不能以TTL当撤销")
		}
		if err != nil {
			o.mu.Lock()
			o.unresolved = append(append([]unresolvedClaim(nil), pending[i:]...), o.unresolved...)
			left := len(o.unresolved)
			o.mu.Unlock()
			if ctx.Err() == nil {
				logx.Errorf("resolve unknown-result retry claim failed; retry claims pause until it succeeds: topic=%s instance=%s ready=%s pending=%d err=%v",
					o.topic, o.instanceID, u.readyKey, left, err)
			}
			return false
		}
		if returned == 1 {
			// 那次认领确实执行了、回包丢了:收据已原样回到 ready,不丢、不耗重试次数。
			logx.Errorf("unknown-result retry claim had moved a receipt into processing; returned it untouched: topic=%s instance=%s ready=%s",
				o.topic, o.instanceID, u.readyKey)
		}
	}
	return true
}

// claim 从共享 ready(readyKey)认领一条重试收据。ready 为空时返回 redis.Nil。
// 与 claimFrom(retry_routing.go)同一个脚本,先续租再认领的不变量不变;owner 模式从 p{P} 认领时直接调 claimFrom。
func (o *retryOwnership) claim(ctx context.Context) ([]byte, error) {
	return o.claimFrom(ctx, o.readyKey)
}

// drainInstance 把某个实例名下的收据搬回 ready。requireExpired=true 时由脚本复核租约,
// 对方已续租就停手并返回 leased=true。
func (o *retryOwnership) drainInstance(ctx context.Context, instanceID string, requireExpired bool) (moved int, leased bool, err error) {
	checkLease := "0"
	if requireExpired {
		checkLease = "1"
	}
	keys := []string{o.instancesKey, retryInstanceProcessingKey(o.topic, instanceID), o.readyKey, o.modesKey}
	for batch := 0; batch < retryReclaimMaxBatchesPerInstance; batch++ {
		n, err := o.rc.Eval(ctx, retryReclaimScript, keys, instanceID, retryReclaimBatch, checkLease).Int64()
		if err != nil {
			return moved, false, err
		}
		if n < 0 {
			return moved, true, nil
		}
		moved += int(n)
		if n < retryReclaimBatch {
			return moved, false, nil
		}
	}
	return moved, false, errors.New("retry processing超过单次归还预算，不能证明已排空")
}

// drainLegacy 把旧版共享 processing 列表里的收据搬回 ready(一拍有上限)。
// 旧版实例还活着时,它的在途收据会因此被重复执行 —— 与以前每次滚动发布时的行为相同。
func (o *retryOwnership) drainLegacy(ctx context.Context) (int, error) {
	moved := 0
	for moved < retryLegacyDrainPerTick {
		_, err := o.rc.RPopLPush(ctx, o.legacyKey, o.readyKey).Result()
		if errors.Is(err, redis.Nil) {
			break
		}
		if err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// expiredInstances 列出租约已过期的实例(以 Redis 的时钟为准)。
func (o *retryOwnership) expiredInstances(ctx context.Context) ([]string, error) {
	now, err := o.rc.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	return o.rc.ZRangeByScore(ctx, o.instancesKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(now.UnixMilli(), 10),
		Count: retryExpiredScanLimit,
	}).Result()
}

// retryReclaimResult 是一次回收的结果,只用于日志与测试。
type retryReclaimResult struct {
	legacyMoved   int // 从旧版共享列表搬回的条数
	orphansMoved  int // 从过期实例名下搬回的条数
	instancesDone int // 处理完(未被续租打断)的过期实例数
}

// reclaimOnce 回收一遍:旧版共享列表 + 所有租约过期的其它实例。
// 自己的租约若已过期不在这里处理:下一次续租或认领会续上,自己回收自己只会制造重复执行。
func (o *retryOwnership) reclaimOnce(ctx context.Context) (retryReclaimResult, error) {
	var result retryReclaimResult
	legacy, err := o.drainLegacy(ctx)
	result.legacyMoved = legacy
	if err != nil {
		return result, fmt.Errorf("drain legacy processing list %s: %w", o.legacyKey, err)
	}

	expired, err := o.expiredInstances(ctx)
	if err != nil {
		return result, fmt.Errorf("list expired retry instances in %s: %w", o.instancesKey, err)
	}
	for _, instanceID := range expired {
		if instanceID == o.instanceID {
			continue
		}
		moved, leased, err := o.drainInstance(ctx, instanceID, true)
		result.orphansMoved += moved
		if err != nil {
			return result, fmt.Errorf("reclaim retry receipts of instance %s: %w", instanceID, err)
		}
		if !leased {
			result.instancesDone++
		}
	}
	return result, nil
}

// start 在开始消费之前调用:收回同名实例遗留的收据、登记租约、回收一遍孤儿。
// 任何一步失败都返回错误 —— 与以前"恢复 processing 失败即拒绝启动"同一口径:重试队列不可用时不该开始消费。
func (o *retryOwnership) start(ctx context.Context) error {
	own, _, err := o.drainInstance(ctx, o.instanceID, false)
	if err != nil {
		return fmt.Errorf("recover own retry receipts from %s: %w", o.processingKey, err)
	}
	if err := o.renewLease(ctx); err != nil {
		return fmt.Errorf("register retry instance lease in %s: %w", o.instancesKey, err)
	}
	reclaimed, err := o.reclaimOnce(ctx)
	if err != nil {
		return err
	}
	if own > 0 || reclaimed.legacyMoved > 0 || reclaimed.orphansMoved > 0 {
		logx.Errorf("retry receipts recovered at startup: topic=%s instance=%s own=%d legacy=%d orphans=%d (from %d expired instances)",
			o.topic, o.instanceID, own, reclaimed.legacyMoved, reclaimed.orphansMoved, reclaimed.instancesDone)
	}
	logx.Infof("retry receipt ownership ready: topic=%s instance=%s lease=%s processing=%s",
		o.topic, o.instanceID, o.lease, o.processingKey)
	return nil
}

// run 每收到一个 tick 续租并回收一遍;ctx 取消或 ticks 关闭时返回。
// tick 由调用方给出,测试可以逐拍驱动,不依赖墙钟(与 runPlacementCapabilityHeartbeat 同一做法)。
func (o *retryOwnership) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			o.tick(ctx)
		}
	}
}

func (o *retryOwnership) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, retryOwnershipTickTimeout)
	defer cancel()

	if err := o.renewLease(tickCtx); err != nil {
		if ctx.Err() == nil {
			logx.Errorf("renew retry instance lease failed (other instances will reclaim this instance's receipts once the lease expires): topic=%s instance=%s err=%v",
				o.topic, o.instanceID, err)
		}
		// 自己的租约都没续上,这一拍不去判别人死活。
		return
	}
	result, err := o.reclaimOnce(tickCtx)
	if result.legacyMoved > 0 || result.orphansMoved > 0 {
		logx.Errorf("reclaimed abandoned retry receipts: topic=%s by=%s legacy=%d orphans=%d (from %d expired instances)",
			o.topic, o.instanceID, result.legacyMoved, result.orphansMoved, result.instancesDone)
	}
	if err != nil && ctx.Err() == nil {
		logx.Errorf("reclaim abandoned retry receipts failed (will retry next tick): topic=%s instance=%s err=%v",
			o.topic, o.instanceID, err)
	}
}

// release 在停机时调用:把本实例还没做完的收据还回 ready,并从登记表里摘掉自己。
// 调用前必须已经停掉 worker 与 run 循环(否则它们会再认领 / 再续租)。失败不致命:租约过期后别的实例会回收。
func (o *retryOwnership) release(ctx context.Context) error {
	moved, _, err := o.drainInstance(ctx, o.instanceID, false)
	if err != nil {
		return fmt.Errorf("return retry receipts from %s: %w", o.processingKey, err)
	}
	remaining, err := o.rc.LLen(ctx, o.processingKey).Result()
	if err != nil {
		return err
	}
	if remaining != 0 {
		return errors.New("retry processing归还后仍有收据")
	}
	if _, err = o.rc.ZScore(ctx, o.instancesKey, o.instanceID).Result(); !errors.Is(err, redis.Nil) {
		if err != nil {
			return err
		}
		return errors.New("retry owner仍在registry")
	}
	if _, err = o.rc.HGet(ctx, o.modesKey, o.instanceID).Result(); !errors.Is(err, redis.Nil) {
		if err != nil {
			return err
		}
		return errors.New("retry owner仍在mode registry")
	}
	if moved > 0 {
		logx.Infof("returned %d unfinished retry receipts to the ready queue on shutdown: topic=%s instance=%s",
			moved, o.topic, o.instanceID)
	}
	return nil
}
