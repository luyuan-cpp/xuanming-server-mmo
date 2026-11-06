// Package loginqueue implements a Redis ZSET-backed FIFO queue for AssignGate.
//
// Why this lives here (not in Java Gateway):
//   - Out-of-queue admission requires reading EnterGame state + reconnect
//     decision from sessionmanager + player_locator, both of which are
//     in-process for go-zero login.
//   - Java Gateway's existing RateLimitDecision.QUEUE is a Bucket4j retry
//     hint, not a real ordered queue. The two layers compose: Bucket4j
//     filters obvious spam first, the surviving traffic enters this queue.
//
// Redis layout (per zone):
//
//	queue:zone:{zoneId}        ZSET   member=queueId, score=enqueue unix ms
//	admit:{queueId}            STRING JSON AdmitSlot{node_id, ip, port, zone_id},TTL = AdmitTTL
//	                           (只记放行时选好的 gate,不含签名;PopAdmit 把它与在途占位排进同一个 MULTI 写入,
//	                           gate 票在取走时由 consumeAdmitAndSign 现签。R17 R1(2026-06-04)之前存的是
//	                           已签好的 {ip, port, payload, signature, deadline},票在 Redis 里就开始耗有效期)
//	admitted:zset:zone:{zoneId} ZSET member=queueId | fast:<uuid>, score=占位到期 unix ms
//	                           (在途占位:成员逐个到期,计数前清掉;整键 TTL 只做垃圾回收、只增不减。
//	                           G4b-G 之前是 SET admitted:zone:{zoneId} + 整键滑动 TTL,新版本不再读写旧键)
//	queue:meta:{queueId}       HASH   {account, device, zone_id, createdAt, admitted_at}
//	                           (admitted_at:放行时刻 unix 毫秒,PopAdmit 与放行槽同一个 MULTI 写入,
//	                           入队时没有;Lookup 靠它区分「放行窗口已过」与「在途窗口」,2026-10-15 起)
//	queue:token:{queueToken}   STRING queueId  (token → queueId reverse index)
//
// Lifecycle:
//
//	Enqueue   → ZADD + meta + token mapping (one Redis round-trip via pipeline)
//	Rank      → ZRANK + ZCARD               (cheap, hot path for QueryQueueStatus)
//	Admit     → ZPOPMIN N → write admit:{queueId} + meta.admitted_at + 占位(到期 = 现在 + AdmitTTL) (dispatcher)
//	Consume   → GETDEL admit:{queueId}, 签 gate 票, 占位改为「现在 + handoffGrace」到期(不撤)
//	FastPath  → 一段 Lua:清过期 + 计数 < budget + 占位(到期 = 现在 + handoffGrace)
//
// 放行之后客户端拿到的是确定的终态:放行槽被取走一次即 ADMITTED;之后再查(重复轮询、回包丢失后重试),
// 或放行槽没人来取、按 AdmitTTL 过期之后再查,Lookup 看到 meta.admitted_at 而放行槽已不在,回 EXPIRED
// (gateway 映射 410),客户端丢掉 token 重走 /assign-gate。2026-10-15 之前这里写的是「dropped admit
// 只是让客户端重新入队」,实际 Lookup 回的是「排队中、名次 0」,客户端一直轮询到 meta 按 QueueEntryTTL
// (1h)过期。PopAdmit 在写放行槽之前丢掉的条目(写放行槽的 MULTI 失败等)没有 admitted_at,仍回名次 0
// 直到 meta 过期,这是既有边角(login-queue-2026-05.md §14 第 7 条)。queue:meta TTL is the global ceiling.
//
// (2026-10-15 更正)上文「PopAdmit 把它与在途占位排进同一个 MULTI」「admitted_at 与放行槽同一个 MULTI 写入」
// 已不成立:放行槽、admitted_at、在途占位改由 admitEntryScript 一段脚本写,先判 meta 在不在、admitted_at
// 有没有。meta 不在(条目在队里待满 QueueEntryTTL,token 索引与排队令牌同时失效,查询本来就回 EXPIRED)
// 的条目整条丢弃,不写放行槽、不占名额、不计入放行数。ZPOPMIN 之后的写入也不再随调用方 ctx 取消(停服、
// 丢租约),见 popAdmitWriteBudget。仍会丢条目、停在名次 0 的路径只剩:取消之后写入超出这个预算(预算自
// PopAdmit 看到取消时起算,没有取消时不截断)、Redis 出错(含 noeviction 写满)、进程被强杀,以及 pickFn
// 回 (nil, nil)、Marshal 失败;写入出错每批汇总一行 ERROR。
//
// (2026-10-18 更正)上面的 Redis 布局与 Lifecycle 有几处已不成立:
//   - Admit 不再 ZPOPMIN:ZRANGE 读出前 N 个候选(只读,条目仍在队列),逐条挑 gate,再对每条跑一段 admitEntryScript;
//     脚本先判成员仍在队列,再在同一段里写 admitted_at、放行槽、在途占位并 ZREM 成员。(2026-11-06 起第一条单发探路,
//     之后按块放进 pipeline,一块一次往返,见 PopAdmit。)
//   - Rank 不再是 ZRANK + ZCARD:(2026-11-06 起)一次轮询只跑一段 lookupScript,核对 token 索引、取放行槽、读 meta、
//     名次与总数,条目仍在排队时顺带续空闲 TTL;此前是 GET token 索引、GETDEL 放行槽、读状态脚本三次往返。
//     这两处不改键名与值的格式,新旧版本混跑时互不影响。
//   - meta、token 索引与队列键的 QueueEntryTTL 改为空闲 TTL:自最后一次成功的排队中查询(或入队)起算,至少
//     QueueEntryTTL、至多 1.25 倍(见 entryRenewTTL)。排队令牌的 HMAC 过期固定为 24h(queueTokenMaxAge),
//     上文「queue:meta TTL is the global ceiling」改读:单张票的绝对上限是令牌的 24h,meta 只管空闲。
//   - 上一段(2026-10-15 更正)末尾列出的「仍会丢条目、停在名次 0 的路径」已全部消除:条目在放行脚本写成之前
//     从不离开队列,任何 Redis 错误(含 noeviction 写满)、取消、进程退出之后,它要么仍在队列,要么已放行可取;
//     popAdmitWriteBudget 随之删除。「在途窗口」不复存在:Lookup 看到 meta 在、没有 admitted_at、又不在队列时
//     回 EXPIRED(queue_gone)。
//   - 发布约束:本版本以及从它回滚,都必须在 Queue.Enabled=false 下滚完。排队开着混跑时,旧版 leader 已 ZPOPMIN、
//     尚未写完放行的条目会被新版判成 queue_gone、打回队尾。K8s 部署依赖 tools/scripts/k8s_deploy.ps1 的
//     $script:LoginQueueMergeGateCleared 拦截(未清门槛时各档位排队都关)。
//
// 残余风险逐条写在 admitEntryScript、lookupScript、PopAdmit、Lookup 与 queueTokenMaxAge 的注释里。
package loginqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// Status mirrors AssignGateResponse.status / QueryQueueStatusResponse.status.
type Status uint32

const (
	StatusAdmitted Status = 0
	StatusQueueing Status = 1
	StatusError    Status = 2
	StatusExpired  Status = 3
)

// AdmitToken is what the consume-time gate-token signer produces and what
// the client retrieves on its next AssignGate / QueryQueueStatus call. The
// fields mirror AssignGateResponse so handlers can copy directly.
//
// IMPORTANT: AdmitToken is **never written to Redis** in the queue path.
// What goes into admit:{queueId} is an AdmitSlot (gate-pick only) — the
// HMAC + expire is computed at consume time so the 5-min token TTL starts
// when the client redeems the admit, not when the dispatcher issued it.
// See `consumeAdmitAndSign` for the rationale (R17 R1 root cause).
//
// The fast-path /assign-gate handler (no queue) builds AdmitToken inline
// via PickAndSignGateToken because there is no Redis caching between sign
// and use; the field shape is shared for handler-side simplicity.
type AdmitToken struct {
	IP             string `json:"ip"`
	Port           uint32 `json:"port"`
	TokenPayload   []byte `json:"token_payload"`
	TokenSignature []byte `json:"token_signature"`
	TokenDeadline  int64  `json:"token_deadline"`
}

// AdmitSlot is what dispatcher writes to admit:{queueId} after picking a
// gate but BEFORE signing the gate token. It carries everything needed to
// produce a fresh AdmitToken at consume time — the gate endpoint identity
// plus the zone it belongs to. No HMAC, no expire.
//
// Why split this out from AdmitToken (R17 R1 fix, 2026-06-04):
//   - dispatcher.PopAdmit may run minutes ahead of the client's polling
//     consume. If we put a signed AdmitToken in Redis at PopAdmit time,
//     its expire_timestamp starts counting immediately and the client
//     can receive a token with only a few seconds of life left.
//   - Splitting "decide which gate" (admit-time) from "issue the HMAC"
//     (consume-time) means the 5-min token TTL window always starts
//     when the client is about to use it.
//
// Memory cost: AdmitSlot is smaller than AdmitToken (no payload bytes,
// no 64-byte hex signature), so this is also strictly cheaper for Redis.
type AdmitSlot struct {
	NodeID uint32 `json:"node_id"`
	IP     string `json:"ip"`
	Port   uint32 `json:"port"`
	ZoneID uint32 `json:"zone_id"`
}

// EnqueueResult bundles everything the AssignGate handler needs to build a
// queueing response in one place.
type EnqueueResult struct {
	QueueID    string
	QueueToken string
	Rank       uint32 // 0-based
	Total      uint32
}

// QueueState is the snapshot returned by Lookup, used by QueryQueueStatus.
type QueueState struct {
	Status Status
	Admit  *AdmitToken // populated when Status == StatusAdmitted
	Rank   uint32      // populated when Status == StatusQueueing
	Total  uint32      // populated when Status == StatusQueueing
}

// defaultHandoffGrace:gate 票交到客户端之后,在途占位再保留多久。
//
// 票一交出去(排队路径:客户端轮询取走放行槽;快速通道:assign-gate 当场回票),这个人还要
// 「连上 gate → gate 把人数报进 etcd」才出现在 online 里,这段时间只有占位替他占着名额。
// C++ gate 每 10 秒上报一次 player_count(cpp/nodes/gate/main.cpp 的 playerCountReportTimer),
// login 每次算容量都直接读 etcd(pkg/node/watcher.go FetchAllNodes,无缓存),最坏滞后 =
// 建连耗时 + 10s。20s = 两个上报周期:一个周期的上报滞后,再留 10s 建连与传播余量。
// 改 gate 的上报周期必须同步改这里。
// (2026-10-18 更正)容量改读 gate 目录的进程内 list-watch 镜像(NodeWatcher.Nodes):watch 健康时只多毫秒级传播;
// 镜像按每台 gate 计新鲜度,有一台超过 node.cacheStaleAfter(15s)没刷新就回落直读,所以镜像里一台 gate 的人数
// 最多比 etcd 晚约 5s,最坏滞后约 15s + 建连耗时,仍在 20s 之内,建连余量从 10s 缩到约 5s。
// 改 cacheStaleAfter 时要同步核对这里。
//
// 取小了:占位先于人数到期,空档里多放人(旧实现取走即撤占位,相当于 0s,开区时每拍都按
// 「在线 0」再放一满批)。取大了:人已进 online、占位还在,同一个人算两遍,少放人(20s 时平均约 14s)。
// Java 版 LoginQueue.TAKEN_GRACE 是 15s、Java gate 每 5s 发布,两版的建连余量同为 10s。
// 2026-10-15 负责人定为 20s(spec_G4b-G §11 #1),待压测确认(L-14)。
const defaultHandoffGrace = 20 * time.Second

// admittedKeyGCSlack:在途占位 ZSET 的整键 TTL 比最长成员寿命多留的时间。
// 成员是否有效只看分数;整键 TTL 只负责在本 zone 没有任何 login 再来清理时(下线 / 合服)
// 把键收走。多留一分钟,免得 login 时钟(分数)与 Redis 时钟(TTL)的偏差让整键先于成员消失。
const admittedKeyGCSlack = time.Minute

// queueTokenMaxAge:排队令牌 HMAC 里的绝对过期(2026-10-18 起;此前等于 QueueEntryTTL)。
//
// 令牌只回答「这张票还能不能用」;条目空闲多久算过期,由 Redis 侧 meta、token 索引与队列键的 TTL 管(空闲 TTL,
// 见 entryRenewTTL)。24h 远长于开服日任何预期的等待,只给单张票一个绝对上限。令牌离开 Redis 里的条目就没用,
// 实际可用期 = min(24h, 条目存活),所以把它从 1h 延长到 24h 不扩大被截获令牌的危害面。
// 不做成配置(YAGNI)。改它要同步改 ValidateTTLs 的上界与 docs/design/login-queue-2026-05.md §6。
//
// 残余:续期让 meta 可能活得比令牌久。持续轮询超过 24h 的人,令牌按 ttl 拒绝、客户端重排;他原来的成员与 meta
// 仍可能再活至多 1.25×QueueEntryTTL 并被放行,白占一个 AdmitTTL 的在途名额。不改代码的理由:触发要持续轮询超过
// 24h,实际到不了;「旧版令牌只有 +1h」的窗口被发布约束排除(排队关着滚,开排队时不存在旧令牌);若要改,得给
// ParseAndVerifyQueueTokenMulti 加返回值、续期改成 PEXPIREAT min(...),代价与收益不成比例。
//
// 密钥轮换(2026-10-18 补注):令牌只在 Enqueue 时签一次,排队期间不换签(handler 回包里放的仍是原令牌)。此前令牌与
// 条目都在 QueueEntryTTL 内失效,QueueToken 三段式轮换(config/secrets.go 包注释)的第 ③ 步(摘掉旧密钥)在第 ② 步
// (提升新密钥开始签)全量生效后等一个 QueueEntryTTL 即可;现在持续轮询的人可以拿着旧密钥签的令牌排到 24h。第 ③ 步
// 须在第 ② 步全量生效后至少等 queueTokenMaxAge;要提前,须确认各 zone 的 `ZRANGE queue:zone:<z> 0 0 WITHSCORES`
// (分数 = 入队毫秒)都晚于第 ② 步全量生效时刻,且自该时刻起已过一个 AdmitTTL(已放行、未取走放行槽的人也持旧令牌)。
// 否则这些人下一次查询按 bad_signature(zone=unknown)回 EXPIRED、被打回队尾,指标上与伪造流量混在一起。K8s 档位
// 只写单把 Value、不走 Additional,轮换本来就让在排玩家重新入队(open-server-rate-limit-design.md §11.3 第 7 条),
// 不受此条影响。
const queueTokenMaxAge = 24 * time.Hour

// Queue wraps Redis ZSET operations behind a small typed surface. The zero
// value is unusable; construct via New.
type Queue struct {
	rdb        *redis.Client
	entryTTL   time.Duration
	admitTTL   time.Duration
	hmacSecret []byte // signs queue tokens; same secret as gate tokens is fine

	// handoffGrace:gate 票交到客户端之后占位再留多久。生产恒为 defaultHandoffGrace;
	// 只有同包测试会改它(New 不收这个参数)。
	handoffGrace time.Duration
	// now 是在途占位记账用的时钟:占位的到期分数、清过期成员时的「现在」都读它。
	// 生产是 time.Now;同包测试换成可拨的假时钟。排队令牌、入队分数等其它时间不走它。
	now func() time.Time
}

// New constructs a Queue. hmacSecret is used to sign opaque queue tokens —
// reusing the gate token secret is acceptable because the queue token is
// useless to a gate (different message shape) and useless to login without
// a matching queueId entry, so the threat model is just "client tampering",
// which HMAC over (queueId|expire) defeats.
//
// entryTTL、admitTTL 必须为正,New 本身不校验;生产路径应先调 ValidateTTLs(2026-10-15)。
func New(rdb *redis.Client, entryTTL, admitTTL time.Duration, hmacSecret []byte) *Queue {
	return &Queue{
		rdb:          rdb,
		entryTTL:     entryTTL,
		admitTTL:     admitTTL,
		hmacSecret:   hmacSecret,
		handoffGrace: defaultHandoffGrace,
		now:          time.Now,
	}
}

// ValidateTTLs 校验 New 的两个时长参数(2026-10-15)。应由起服装配在 New 之前调用、失败即起服失败,
// 与 NewDispatcher 对 interval / lockTTL 的校验同级;配置的 default 只在键缺省时生效,显式写 0s 不回落。
// 两者都必须为正:
//   - admitTTL <= 0:放行脚本的 SET PX 报错,弹出的条目会全部丢失(PopAdmit 因此在弹出之前也调本函数,
//     不出队);旧写法(go-redis 的 Set(..., 0))则是写出永不过期的放行槽。在途占位一写即到期,FreeSlots 看不到
//     已放行、未取走的人,超发。
//   - entryTTL <= 0:go-redis 的 Expire(..., <=0) 当场删键,每次 Enqueue 都把整个 zone 的队列与 meta 删掉,
//     token 索引却不带 TTL、永久残留;排队令牌签发即过期。
//
// 不要求 admitTTL < entryTTL:放行槽写在放行时刻、自带 TTL,排队久的条目本来就可能比 meta 晚到期,
// 没有不变量依赖这个大小关系;Java 版 QueueSettings 同样只校验为正。
//
// (2026-10-15 补注)admitTTL 还须不小于 1ms:admitEntryScript 以 admitTTL.Milliseconds() 作 SET PX,
// 不足 1ms 取整为 0,Redis 拒绝 SET PX 0,弹出的条目全部丢失、停在名次 0 直到 meta 过期;旧写法经 go-redis
// 的 formatMs 截为 1ms,不受影响。entryTTL 不加这条下界:Enqueue 走 go-redis 的 Expire / Set,不足 1s / 1ms
// 会被截成 1s / 1ms。报错串仍含 "ttl must be positive",起服装配与用例按这个子串匹配。
//
// (2026-10-18 补注)entryTTL 收紧为 ∈ [1s, queueTokenMaxAge],上一段「entryTTL 不加这条下界」作废。1s 下界与
// Enqueue 走 go-redis 秒级 EXPIRE 对齐,也保证 entryRenewTTL 的毫秒值为正(读状态脚本的 PEXPIRE 不会写出 0、
// 当场删键);上界的理由是排队令牌 24h 先到期,再大的空闲 TTL 没有意义。Lookup 在碰 Redis 之前也调本函数。
// 第一条里「弹出的条目会全部丢失」「在弹出之前也调本函数,不出队」改读:PopAdmit 在 ZRANGE 之前调本函数,一个都不
// 放行;放行脚本出错时条目本来就留在队列,不再丢失(见 PopAdmit)。
func ValidateTTLs(entryTTL, admitTTL time.Duration) error {
	if entryTTL < time.Second || entryTTL > queueTokenMaxAge {
		return fmt.Errorf("loginqueue: queue entry ttl must be positive, at least 1s and at most %v, got %v", queueTokenMaxAge, entryTTL)
	}
	if admitTTL < time.Millisecond {
		return fmt.Errorf("loginqueue: admit ttl must be positive and at least 1ms, got %v", admitTTL)
	}
	return nil
}

// ── Key helpers ────────────────────────────────────────────────────────

func zoneZSetKey(zoneID uint32) string     { return fmt.Sprintf("queue:zone:%d", zoneID) }
func admittedZSetKey(zoneID uint32) string { return fmt.Sprintf("admitted:zset:zone:%d", zoneID) }
func admitKey(queueID string) string       { return "admit:" + queueID }
func metaKey(queueID string) string        { return "queue:meta:" + queueID }
func tokenIndexKey(token string) string    { return "queue:token:" + token }

// metaFieldAdmittedAt 是 meta 里的放行时刻字段(unix 毫秒)。PopAdmit 写、Lookup 读,只看有没有,不看值。
// (2026-10-18 补注)放行脚本的 REPLAYED 判定会看值:成员已不在队列、admitted_at 恰等于本次的放行时刻,即 go-redis
// 重发了本次已执行成功的同一条命令(见 admitEntryScript)。Lookup 仍只看有没有。
const metaFieldAdmittedAt = "admitted_at"

// metaFieldZoneID 是 meta 里的 zone 字段(十进制)。Enqueue 写;consumeAdmitAndSign 读来记占位与等待时长;
// lookupScript 拿它与令牌里的 zone 对账(2026-10-18 起统一用这个常量)。
const metaFieldZoneID = "zone_id"

// entryRenewTTL 是读状态脚本续空闲 TTL 时写入的值:QueueEntryTTL 的 1.25 倍(2026-10-18)。
//
// 滞后续期:只有剩余 PTTL 已不足 QueueEntryTTL(记作 E)时才续到 E + E/4。于是任何空闲不足 E 的条目都不会过期,
// 空闲 TTL 落在 [E, 1.25E];每个排队条目约每 E/4 才续一次,而不是每次轮询都写。新值 > E > 触发时的 PTTL,天然只增
// 不减,不需要 GT。不用「PTTL < E/2 才续到 E」:那会让空闲 30min 的条目也可能过期,违背「QueueEntryTTL 是最短空闲
// 时长」的语义。E ≥ 1s(ValidateTTLs)保证它的毫秒值为正。
func (q *Queue) entryRenewTTL() time.Duration { return q.entryTTL + q.entryTTL/4 }

// ── Enqueue ────────────────────────────────────────────────────────────

// Enqueue adds the caller to the zone queue and returns rank+token.
//
// NOT idempotent on (zoneId, account, deviceID): every call mints a fresh
// queueId. A client that loses its queueToken (e.g. due to a network blip
// before the response arrives) and re-calls AssignGate without the token
// WILL be appended at the tail again — the previous entry sits orphaned
// until its 1h TTL expires. The robot client mitigates this by saving
// queueToken from the FIRST AssignGate response and threading it through
// every subsequent retry; the server's reentry path (handleReentry) then
// resolves the saved token instead of creating a duplicate.
//
// We DON'T enqueue under a distributed lock; ZADD is atomic. Duplicates
// from missing-token retries are bounded (1 per retry round-trip) and
// the orphan TTLs out cheaply, so the cost of a (deviceID → queueId)
// reverse index would not pay back. If clients start spamming
// AssignGate-without-token under network failure, revisit.
//
// (2026-10-18 补注)上文「orphaned until its 1h TTL expires」改读:孤儿不再被轮询,它的 meta 与 token 索引自入队起
// QueueEntryTTL 后过期(空闲 TTL 只在排队中的查询里续);它的队列成员在被出队时由放行脚本的 META_GONE 分支 ZREM 掉。
// 排队令牌现按 queueTokenMaxAge(24h)签,不再等于 QueueEntryTTL。
func (q *Queue) Enqueue(ctx context.Context, zoneID uint32, account, deviceID string) (*EnqueueResult, error) {
	queueID := uuid.NewString()
	token, err := signQueueToken(q.hmacSecret, queueID, zoneID, time.Now().Add(queueTokenMaxAge).Unix())
	if err != nil {
		return nil, fmt.Errorf("sign queue token: %w", err)
	}

	scoreMs := time.Now().UnixMilli()

	pipe := q.rdb.TxPipeline()
	pipe.ZAdd(ctx, zoneZSetKey(zoneID), redis.Z{Score: float64(scoreMs), Member: queueID})
	// 队列键的 TTL 只增不减(2026-10-18 起;此前无条件 EXPIRE E,会把读状态脚本续上的 1.25E、或 QueueEntryTTL 较大的
	// 副本写的 TTL 压回本副本的 E,整条队列可能先于仍在轮询的条目过期)。NX 与 GT 缺一不可:NX 保证新建的键(或没有
	// TTL 的键)一定设上 TTL;GT 只延长、不缩短。只用 GT 的话,没有 TTL 的键按「无限」处理、永远设不上(与
	// admittedKeyTTL 注释里的陷阱一相同)。ZADD 在前,两条 EXPIRE 执行时键一定存在。
	// 版本下限:EXPIRE 的 NX / GT 需要 Redis ≥ 7.0(此前排队路径只要求 6.2 的 GETDEL)。低于 7.0 时这两条在 MULTI 入列
	// 阶段报参数错误,EXEC 回 EXECABORT,每次 Enqueue 都失败、新玩家全部进不了队;生产与 docker-compose 均为 7.2。
	pipe.ExpireNX(ctx, zoneZSetKey(zoneID), q.entryTTL)
	pipe.ExpireGT(ctx, zoneZSetKey(zoneID), q.entryTTL)
	pipe.HSet(ctx, metaKey(queueID), map[string]any{
		"account":       account,
		"device":        deviceID,
		metaFieldZoneID: zoneID,
		"createdAt":     scoreMs,
	})
	// meta 与 token 索引是新键,照旧无条件设 TTL。go-redis 的 EXPIRE 按秒截断,与脚本里续期用的毫秒不一致,
	// 但 E ≥ 1s(ValidateTTLs)时无害。
	pipe.Expire(ctx, metaKey(queueID), q.entryTTL)
	pipe.Set(ctx, tokenIndexKey(token), queueID, q.entryTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("enqueue pipeline: %w", err)
	}

	rank, err := q.rdb.ZRank(ctx, zoneZSetKey(zoneID), queueID).Result()
	if err != nil {
		// Race: ZADD succeeded but member was popped by the dispatcher between
		// the pipeline and ZRANK. Treat as rank 0 — the client will see it as
		// "next" and likely receive an admit on its first poll.
		// Since 2026-10-18 a miss here means the admit script has just ZREM'd
		// the member in the same script that wrote its admit slot, so rank 0 is
		// harmless: the next poll takes the admit.
		if errors.Is(err, redis.Nil) {
			rank = 0
		} else {
			return nil, fmt.Errorf("zrank just-added member: %w", err)
		}
	}
	total, _ := q.rdb.ZCard(ctx, zoneZSetKey(zoneID)).Result()

	return &EnqueueResult{
		QueueID:    queueID,
		QueueToken: token,
		Rank:       uint32(rank),
		Total:      uint32(total),
	}, nil
}

// ── Lookup (QueryQueueStatus) ─────────────────────────────────────────

// SignAdmitFn is the consume-time gate-token signer. Lookup invokes it
// after GetDel admit:{queueId} succeeds, so the resulting AdmitToken's
// HMAC + expire begin at consume time rather than admit time. This is
// the heart of the R17 R1 fix: a 5-min token TTL must mean "5 min after
// the client receives it", not "5 min after the dispatcher picked the
// gate".
//
// The slot argument is whatever AdmitSlot the dispatcher stored at admit
// time (gate IP/Port/NodeID/ZoneID). Returning (nil, err) makes Lookup
// surface the error to the caller; returning (nil, nil) is not allowed —
// either we sign successfully or we have an error.
//
// (2026-11-05)生产入口一律用 NewAdmitSigner:签之前复核 slot 里的 gate,它已停写、满载或不在此刻的候选里就改签另一台;
// 复核读不到候选时照签 slot 里的 gate,复核本身不产生错误(见 admit_recheck.go)。
type SignAdmitFn func(slot *AdmitSlot) (*AdmitToken, error)

// Lookup translates a queue token to its current state. signFn 只在取到放行槽时调用,调用方必须传非 nil
// (queuestatuslogic / assigngatelogic 传 NewAdmitSigner)。
//
// token 必须是调用方已验签的排队令牌,zoneID 取自同一次验签(ParseAndVerifyQueueTokenMulti 的第二个返回值),0 视为损坏
// (2026-10-18 起;此前 zone 从 meta 读)。queueId 从令牌正文取(queueIDOfVerifiedToken),令牌与条目的绑定由 lookupScript
// 拿 token 索引核对(2026-11-06 起;此前 queueId 是 GET token 索引读出来的)。lookupScript 用 zoneID 定位队列键,并与 meta
// 的 zone_id 对账,不符回 EXPIRED(meta_gone)。EXPIRED 的 zone 标签一律取这个 zone。
//
// 判定顺序(2026-11-06 起全在一段 lookupScript 里,一次往返;此前是 GET token 索引、GETDEL 放行槽、读状态脚本三次往返,
// 读状态脚本看到 admitted_at 时还要再 GETDEL 一次。缺口清单第 14 条):
//  1. TTL 不合法(ValidateTTLs)或 signFn 为 nil:回错误,不碰 Redis。前者是纵深防御,挡住绕过起服装配直接 New 的调用方,
//     免得续期写出 PEXPIRE 0、当场删键。
//  2. zoneID == 0,或令牌拆不出 queueId:EXPIRED(meta_gone),不碰 Redis。不去查 queue:zone:0(dispatcher 永远不出它)。
//  3. lookupScript(与放行脚本互斥原子):
//     - token 索引不在,或不指向这个 queueId:EXPIRED(meta_gone)。空闲超过 QueueEntryTTL 的条目走这里;
//     - 取到放行槽(GETDEL,只成功一次):现签 gate 票(consumeAdmitAndSign),ADMITTED;
//     - meta 不在、不是 hash,或 zone 不符:EXPIRED(meta_gone);
//     - meta 有 admitted_at 而放行槽不在:放行过,放行槽已被取走或已按 AdmitTTL 过期,EXPIRED(admit_window_passed)。
//       放行脚本在同一段里写 admitted_at 与放行槽,本脚本在同一段里读两者,不会是「放行还没落地」,不用再取一次;
//       不改占位,重复轮询不给它续期;
//     - 仍在队列:QUEUEING + 名次 + 总数,按需续空闲 TTL;
//     - meta 在、没有 admitted_at、又不在队列:EXPIRED(queue_gone),来源见 lookupScript;它依赖发布约束
//       (排队开着时不得混跑新旧版本)。
//
// 往返:排队中一次轮询 = 一次 EVALSHA(进程内首次调用多一次 NOSCRIPT 回退的 EVAL);取到放行槽的那一次另有签票之后的
// 读 meta 与改占位(各一次往返,每个条目只有一次,见 consumeAdmitAndSign)。轮询从纯读变为「偶尔带写」(续期),
// 写放大量级见 lookupScript。
//
// 残余:
//   - 空闲 TTL ∈ [E, 1.25E](E = QueueEntryTTL)。整个 zone 空闲时队列键与各条 meta 谁先到期不定,queue_gone 与
//     meta_gone 都可能出现,都是 EXPIRED,客户端照常重排。
//   - 客户端空闲接近 E 时恰好被放行:meta 或 token 索引先到期,回 meta_gone,放行槽白占至多一个 AdmitTTL。
//   - 持续轮询超过 24h:见 queueTokenMaxAge。
func (q *Queue) Lookup(ctx context.Context, token string, zoneID uint32, signFn SignAdmitFn) (*QueueState, error) {
	if err := ValidateTTLs(q.entryTTL, q.admitTTL); err != nil {
		return nil, err
	}
	if signFn == nil {
		return nil, errors.New("loginqueue: Lookup needs a non-nil signFn (caller bug)")
	}
	if zoneID == 0 {
		recordExpired(0, ExpireReasonMetaGone)
		return &QueueState{Status: StatusExpired}, nil
	}
	queueID, ok := queueIDOfVerifiedToken(token)
	if !ok {
		recordExpired(zoneID, ExpireReasonMetaGone)
		return &QueueState{Status: StatusExpired}, nil
	}

	res, err := q.lookupEntry(ctx, token, queueID, zoneID)
	if err != nil {
		return nil, fmt.Errorf("lookup state: %w", err)
	}
	switch res.code {
	case lookupAdmitTaken:
		// 条目的生命到此为止。meta 与 token 索引留到 TTL(见 consumeAdmitAndSign 末尾);
		// 在途占位不撤,consumeAdmitAndSign 把它改成再留 handoffGrace。
		admit, err := q.consumeAdmitAndSign(ctx, queueID, res.slotJSON, signFn)
		if err != nil {
			return nil, err
		}
		return &QueueState{Status: StatusAdmitted, Admit: admit}, nil
	case lookupQueueing:
		return &QueueState{Status: StatusQueueing, Rank: res.rank, Total: res.total}, nil
	case lookupAdmitWindowPassed:
		recordExpired(zoneID, ExpireReasonAdmitWindowPassed)
		return &QueueState{Status: StatusExpired}, nil
	case lookupQueueGone:
		recordExpired(zoneID, ExpireReasonQueueGone)
		return &QueueState{Status: StatusExpired}, nil
	case lookupMetaGone:
		recordExpired(zoneID, ExpireReasonMetaGone)
		return &QueueState{Status: StatusExpired}, nil
	default:
		// lookupEntry 已拒绝未知返回码,走不到这里;防的是两处常量改得不同步。
		return nil, fmt.Errorf("lookup state: unexpected code %d", res.code)
	}
}

// lookupScript 是一次轮询的全部 Redis 操作(2026-11-06 起,合并了此前的 GET token 索引、GETDEL 放行槽与读状态脚本
// lookupStateScript;缺口清单第 14 条):核对令牌、取走放行槽或读出排队状态,条目仍在排队时在同一次执行里续空闲 TTL。
//
//	KEYS[1] = queue:meta:{queueId}   KEYS[2] = queue:zone:{zone}(zone 取自令牌)   KEYS[3] = queue:token:{token}
//	KEYS[4] = admit:{queueId}
//	ARGV[1] = member(queueId,取自令牌)   ARGV[2] = zone(十进制)   ARGV[3] = metaFieldZoneID   ARGV[4] = metaFieldAdmittedAt
//	ARGV[5] = E 毫秒(续期阈值,QueueEntryTTL)   ARGV[6] = entryRenewTTL 毫秒(续期写入值)
//	返回 {code, rank, total, slot}:code 见 lookup* 常量;rank / total 只在 QUEUEING 时有意义;slot 只在取到放行槽时是放行槽
//	JSON,其余为空串。四个元素的类型固定(整数、整数、整数、字符串)。
//
// 与放行脚本互斥原子:放行脚本在同一段里写 admitted_at、放行槽并把成员移出队列,本脚本在同一段里读这三样,
// 「GETDEL 落空之后放行才落地」的交错不复存在,此前为它准备的二次 GETDEL 与测试注入点(afterAdmitMiss)随之删除。
// 令牌核对:改写过正文的令牌本来就没有索引(索引按令牌全文建,见 queueIDOfVerifiedToken),取不到别人的放行槽;
// 比对索引的值而不只看它在不在,是纵深防御,防索引值与令牌正文对不上的坏数据把键拼到别的条目上。
//
// 纯新版下 queue_gone(meta 在、没有 admitted_at、又不在队列)只有三个来源:
//  1. 整个 zone 空闲 ≥ E 后,队列键先于该条目的 meta 过期(该条目空闲 ∈ [E, 1.25E],本来就该 EXPIRED,只是原因标签不同);
//  2. 人工 ZREM 成员或 DEL 队列键;
//  3. 排队开着时混跑旧版 leader(旧版 ZPOPMIN 之后、写放行之前的条目)——发布约束禁止这种情况。
//
// 续期规则:meta、队列键、token 索引三把键各自判 PTTL,只有 PTTL 不是 -2 且 < E 时才 PEXPIRE 到 1.25E(滞后续期,理由见
// entryRenewTTL)。PTTL 为 -1(没有 TTL)时顺手设上 TTL;不存在的键不碰,PEXPIRE 也不会建键,本脚本不造出没有 TTL 的键。
// 放行之后(成员已移出,或 admitted_at 已在)不再续期,meta 与 token 索引按剩余 TTL 到期。
//
// 写满:脚本里的写只有 GETDEL 与 PEXPIRE,都不是 DENYOOM 命令,Redis 写满时照常执行,排队中的轮询、续期与取走放行槽
// 都不受影响。
//
// 写放大估算(未压测):每个排队条目只在首次轮询与之后约每 E/4 产生一次写(meta 与 token 各一条 PEXPIRE),队列键
// 每 zone 约每 E/4 一次。排队 4.5 万人、2s 轮询、E=1h 时,稳态续期约 100 条 PEXPIRE/s,另加入队洪峰期间约 2 × 入队
// 速率。GETDEL 落空不改数据;没触发续期的执行不进复制流与 AOF。每次 EVALSHA 的 Redis CPU 未测,压测时看
// stress_summarize 的 Redis 负载段(R1 / R2)。
//
// 只返回整数与字符串:缺失判定一律写 `not x` 或比较值,返回表里不放 nil / false(原因见 admitEntryScript (f))。
// 排队族的键分属不同 hash slot,不支持 Cluster;KEYS 全部声明是脚本契约本身。KEYS[1] 是 meta 而不是放行槽:测试按
// 「第一个键以 admit: 开头」认放行脚本(admitScriptKey),两者不能混。
var lookupScript = redis.NewScript(`
if redis.call('GET', KEYS[3]) ~= ARGV[1] then
	return {1, 0, 0, ''}
end
local slot = redis.call('GETDEL', KEYS[4])
if slot then
	return {2, 0, 0, slot}
end
if redis.call('TYPE', KEYS[1]).ok ~= 'hash' then
	return {1, 0, 0, ''}
end
local f = redis.call('HMGET', KEYS[1], ARGV[3], ARGV[4])
if f[1] ~= ARGV[2] then
	return {1, 0, 0, ''}
end
if f[2] then
	return {5, 0, 0, ''}
end
local rank = redis.call('ZRANK', KEYS[2], ARGV[1])
if not rank then
	return {4, 0, 0, ''}
end
local idle = tonumber(ARGV[5])
for i = 1, 3 do
	local ttl = redis.call('PTTL', KEYS[i])
	if ttl ~= -2 and ttl < idle then
		redis.call('PEXPIRE', KEYS[i], ARGV[6])
	end
end
return {3, rank, redis.call('ZCARD', KEYS[2]), ''}
`)

// lookupScript 返回数组的第一个元素(2026-11-06;1、3、4 与此前 lookupStateScript 同义,2 改为「取到放行槽」,
// 5 是从此前的 2「meta 有 admitted_at」拆出来的「放行槽已不在」)。
const (
	lookupMetaGone          = 1 // token 索引不在或不指向这个条目,meta 不在或不是 hash,或 meta 的 zone 与令牌不符
	lookupAdmitTaken        = 2 // 取到放行槽(已 GETDEL),第四个元素是放行槽 JSON
	lookupQueueing          = 3 // 仍在队列,带名次与总数,已按需续期
	lookupQueueGone         = 4 // meta 在、没有 admitted_at、又不在队列
	lookupAdmitWindowPassed = 5 // meta 有 admitted_at,放行槽已被取走或已按 AdmitTTL 过期
)

// lookupResult 是一次 lookupScript 的结果。slotJSON 只在 code 为 lookupAdmitTaken 时非空。
type lookupResult struct {
	code        int64
	rank, total uint32
	slotJSON    []byte
}

// lookupEntry 对一个条目跑一次 lookupScript。只由 Lookup 调用。用 Script.Run(先 EVALSHA,NOSCRIPT 时回退 EVAL:
// EVALSHA 撞上 NOSCRIPT 时脚本没有执行,回退不会重复取走放行槽)。返回值不是 4 个元素、类型不对或 code 未知时回错误。
func (q *Queue) lookupEntry(ctx context.Context, token, queueID string, zoneID uint32) (lookupResult, error) {
	vals, err := lookupScript.Run(ctx, q.rdb,
		[]string{metaKey(queueID), zoneZSetKey(zoneID), tokenIndexKey(token), admitKey(queueID)},
		queueID, zoneID, metaFieldZoneID, metaFieldAdmittedAt,
		q.entryTTL.Milliseconds(), q.entryRenewTTL().Milliseconds(),
	).Slice()
	if err != nil {
		return lookupResult{}, err
	}
	if len(vals) != 4 {
		return lookupResult{}, fmt.Errorf("script returned %d values, want 4", len(vals))
	}
	code, okCode := vals[0].(int64)
	rank, okRank := vals[1].(int64)
	total, okTotal := vals[2].(int64)
	slot, okSlot := vals[3].(string)
	if !okCode || !okRank || !okTotal || !okSlot {
		return lookupResult{}, fmt.Errorf("script returned values of types %T %T %T %T, want three integers and a string",
			vals[0], vals[1], vals[2], vals[3])
	}
	switch code {
	case lookupMetaGone, lookupAdmitTaken, lookupQueueing, lookupQueueGone, lookupAdmitWindowPassed:
	default:
		return lookupResult{}, fmt.Errorf("script returned unknown code %d", code)
	}
	res := lookupResult{code: code, rank: uint32(rank), total: uint32(total)}
	if code == lookupAdmitTaken {
		res.slotJSON = []byte(slot)
	}
	return res, nil
}

// consumeAdmitAndSign 把 lookupScript 已经取走(GETDEL)的放行槽签成 gate 票:解析放行槽 → signFn 现签 → 读 meta →
// 在途占位改为「现在 + handoffGrace」到期 → 记等待时长。(2026-11-06 起 GETDEL 挪进 lookupScript,本函数从取到的放行槽
// 开始;此前本函数自己 GETDEL,取不到回 (nil, nil),由 Lookup 再读状态。)
//
// The signFn argument is what makes the R17 R1 fix work: the gate token's
// HMAC + expire are computed HERE, at consume time, so the token TTL window
// begins when the client redeems the admit — not when the dispatcher picked
// the gate. Before this split (pre-2026-06-04) signing happened inside
// PopAdmit and the token could sit in Redis for 4-5 min before the client
// GetDel'd it, burning most of its TTL. See
// stress-1zone-45k-2026-06-03-round17.md §4 R1 for the diagnosis.
func (q *Queue) consumeAdmitAndSign(ctx context.Context, queueID string, slotJSON []byte, signFn SignAdmitFn) (*AdmitToken, error) {
	var slot AdmitSlot
	if err := json.Unmarshal(slotJSON, &slot); err != nil {
		return nil, fmt.Errorf("unmarshal admit slot: %w", err)
	}

	// Sign NOW so the token's expire starts ticking from this moment.
	admit, err := signFn(&slot)
	if err != nil {
		// The slot has already been taken (GETDEL in lookupScript) — the entry
		// is gone from Redis. Surface the signing error; the client will see
		// code=500 and can restart from /assign-gate to get re-enqueued. We
		// could try to put the slot back, but a signing failure usually means
		// HMAC-secret misconfig or crypto/rand failing, neither of which is
		// fixed by retrying with the same slot.
		return nil, fmt.Errorf("sign admit token: %w", err)
	}

	// 在途占位与等待时长:best-effort,失败不让这次取走作废(票已经签好)。
	//
	// We do HMGET (not HGET) to grab createdAt and zone_id in one RTT so the
	// wait-time histogram observation below pays no extra Redis cost. Missing
	// createdAt is non-fatal — the wait observation is skipped.
	fields, _ := q.rdb.HMGet(ctx, metaKey(queueID), metaFieldZoneID, "createdAt").Result()
	var (
		zoneID    uint32
		createdAt int64
	)
	if len(fields) >= 1 {
		if s, ok := fields[0].(string); ok {
			_, _ = fmt.Sscanf(s, "%d", &zoneID)
		}
	}
	if len(fields) >= 2 {
		if s, ok := fields[1].(string); ok {
			_, _ = fmt.Sscanf(s, "%d", &createdAt)
		}
	}
	if zoneID != 0 {
		// 不撤占位,改成再留 handoffGrace(G4b-G):票已交给客户端,但他连上 gate、gate 把人数
		// 报进 etcd 之前不在任何 gate 的 online 里。此刻撤掉,下一拍会按「在线没变、在途为 0」
		// 再放一批(旧实现在这里 SREM,开区时能放进几倍容量)。
		// 用普通 ZADD 而不是 XX:占位若已按分数清掉(login 与 Redis 的时钟差),这个人照样要占名额。
		// zone 读不到(meta 被淘汰)时走不到这里:占位保持放行时的分数,最迟 AdmitTTL 到期。
		if err := q.holdAdmitted(ctx, q.rdb, zoneID, queueID, q.handoffGrace).Err(); err != nil {
			logx.Errorf("[loginqueue] hold admitted placeholder after consume zone=%d queueId=%s err=%v",
				zoneID, queueID, err)
		}
		if createdAt > 0 {
			waitMs := time.Now().UnixMilli() - createdAt
			recordWait(zoneID, time.Duration(waitMs)*time.Millisecond)
		}
	}
	// meta 与 token 索引不删,留到各自的 TTL(QueueEntryTTL)。之后同一令牌再查(回包丢失后重试、go-redis
	// 超时重发),lookupScript 靠 meta 里的 admitted_at 认出「放行过、放行槽已不在」,回 StatusExpired
	// (原因 admit_window_passed),客户端按 410 丢掉 token 重走 /assign-gate。
	return admit, nil
}

// ── Dispatcher-facing (admit a batch) ─────────────────────────────────

// PickAdmitSlotFn is the dispatcher-side gate selector used by PopAdmit.
// It chooses which gate the queue entry will be assigned to without
// signing the gate token — the HMAC + expire happen later, at consume
// time, in consumeAdmitAndSign.
//
// Returning (nil, err) makes PopAdmit re-add the entry to the front of
// the ZSET so the next dispatcher tick retries; returning (nil, nil) is
// treated as a no-gate-available outcome and skips the entry entirely.
//
// (2026-10-18 更正)上面两句已不成立:返回错误或 (nil, nil) 时 PopAdmit 都只跳过该条——条目从未离开队列、分数不变
// (不再以分数 0 回填队首),下一拍再试;首个错误随 PopAdmit 的返回值交给 dispatcher。
type PickAdmitSlotFn func(zoneID uint32, queueID string) (*AdmitSlot, error)

// PopAdmit admits up to n entries from the front of zoneId's queue and
// writes admit:{queueId} = AdmitSlot for each. Caller (the dispatcher)
// supplies pickFn that selects a gate without signing — sign happens
// later at consume time so the token's 5-min TTL starts when the
// client redeems it (R17 R1 fix, see consumeAdmitAndSign).
//
// 返回 (已放行数, err);err != nil 时已放行数仍然有效,调用方照样计入(2026-10-18 起)。
//
// 出队方式(2026-10-18 起不再 ZPOPMIN;2026-11-06 起按块流水线发脚本):
//   - 先 ZRANGE 读出前 n 个候选(只读,条目仍在队列),逐条 pickFn 挑好 gate,再对每条跑一段 admitEntryScript;
//     脚本先判成员仍在队列,再在同一段里写 admitted_at、放行槽、在途占位并 ZREM 成员。每条各自原子,互不依赖。
//   - 脚本按块发:第一块只有一条(探路),之后每块至多 popAdmitPipelineChunk 条、放进一个 pipeline 一次往返
//     (见 popAdmitPipelineChunk)。此前每条等回包再发下一条,放行速率约等于 1 / Redis 往返(缺口清单第 14 条)。
//     探路那一条单发:写满、占位键或队列键 WRONGTYPE、连不上这类持续性错误只在它身上出一次,本批随即停下,
//     与逐条执行时一样只尝试一条,部分写入(admitEntryScript 注释 (d))也最多一条。
//   - 停批规则:一块里有脚本出错(写满、连接 / 超时、WRONGTYPE、取消)就在这一块之后停下,后面的块不再发,原样留在
//     队列;同一块里出错条目之外的各条照样执行了(成功的已放行、计数,出错的原样留在队列)。错误带上首个出错的 queueId、
//     已放行数、跳过数、出错数与未尝试数。挑 gate 失败、回 (nil, nil) 或 Marshal 失败只跳过该条(它从未离开队列、分数
//     不变,下一拍再试),首个跳过原因在批末随返回值交出。调用方 ctx 取消(停服 / 丢租约)时在发下一块之前停下。
//     与条目内容有关的坏数据(meta 不在或类型损坏、空 member)由脚本就地清理,不停批。
//   - 不丢条目:任何错误、写满、取消、进程退出之后,条目要么仍在队列,要么已放行可取。
//   - 结果不确定时(go-redis 重试用尽的读超时,整块的结果都不确定),条目要么已写成并移出(客户端可取),要么仍在队列,
//     下一拍不会重复放行;写成的那些不计数,只影响指标。go-redis 对整块重发时,已执行过的那些走 REPLAYED、照常计数。
//   - Stop 最多多等一块在途 pipeline 的 ReadTimeout:客户端没开 ContextTimeoutEnabled,正在等回包的命令不随取消返回;
//     取消之后 go-redis 的重试退避直接返回,不再重发。
//   - 重叠 leader 或多副本时两边可能 ZRANGE 到同一批,脚本第一步保证每条只写一次(后到的拿到 GONE),在途上界
//     仍 ≤ 2×(capacity−online)。
//   - 快速通道看到的队列,在本批每条都原子地「移出 + 写占位」之前不会变空;此前「批弹空后、占位写全前快速通道
//     可插队」的窗口随之消失。
//   - 写满(noeviction)时放行停止(fail-closed),dispatcher 每拍一行含 OOM 的 ERROR;排队中的轮询与续期照常;
//     新入队整体失败(StatusError)。需要 Redis 内存水位告警。
//
// 残余:
//   - zone 满(free==0)时 dispatchOnce 不调用本函数,队首的 META_GONE 孤儿要等有空位那一拍才清理(既有性质)。
//   - pickFn 若对特定条目确定性失败,该条会永久占一个批位、让排在它后面的人名次多 1;生产 pickFn 与条目无关,走不到。
//   - 哨兵切主时异步复制可能丢掉放行效果,条目可能再次放行、签出第二张 gate 票(既有问题,不在本函数范围)。
//   - 部分写入与 REPLAYED 的计数偏差见 admitEntryScript。
func (q *Queue) PopAdmit(
	ctx context.Context,
	zoneID uint32,
	n int,
	pickFn PickAdmitSlotFn,
) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	// TTL 不合法时一个都不放行,条目原样留在队列,由 dispatcher 每拍记 ERROR。起服装配(initLoginQueue)已在 New
	// 之前调 ValidateTTLs、失败即起服失败,这里是纵深防御,挡住绕过装配直接 New 的调用方(否则放行脚本的 SET PX 0
	// 每条都报错)。
	if err := ValidateTTLs(q.entryTTL, q.admitTTL); err != nil {
		return 0, err
	}
	members, err := q.rdb.ZRange(ctx, zoneZSetKey(zoneID), 0, int64(n-1)).Result()
	if err != nil {
		return 0, fmt.Errorf("zrange: %w", err)
	}

	var (
		admitted, gone, metaGone, already, skipped int
		firstSkippedID                             string
		firstSkipErr                               error
	)
	defer func() {
		// 每批至多一行 INFO,不逐条打。脚本出错时不在这里打 ERROR:错误带着上下文交给 dispatcher,每拍只打一行。
		if gone+metaGone+already > 0 {
			logx.Infof("[loginqueue] PopAdmit entries not admitted zone=%d metaGone=%d alreadyAdmitted=%d gone=%d",
				zoneID, metaGone, already, gone)
		}
	}()
	// 空串 member 不特殊处理:交给脚本,由 META_GONE 分支 ZREM 掉。在 ZRANGE 之下对它 continue 会让它永久卡在队首。
	// 先给每条挑好 gate,再分块执行脚本;批内记账只看挑了几次,与脚本结果无关(本拍之后就丢掉)。
	entries := make([]admitEntryInput, 0, len(members))
	for _, queueID := range members {
		slotJSON, err := pickAdmitSlot(pickFn, zoneID, queueID)
		if err != nil {
			// 条目从未离开队列,分数不变,下一拍再试。
			skipped++
			if firstSkipErr == nil {
				firstSkippedID, firstSkipErr = queueID, err
			}
			continue
		}
		entries = append(entries, admitEntryInput{queueID: queueID, slotJSON: slotJSON})
	}
	for start := 0; start < len(entries); {
		// 停服 / 丢租约:剩余条目原样留在队列。pickFn 期间发生的取消在第一块之前就生效。
		if err := ctx.Err(); err != nil {
			return admitted, err
		}
		end := start + popAdmitPipelineChunk
		if start == 0 {
			end = start + 1 // 第一块只放一条探路,见本函数注释
		}
		end = min(end, len(entries))
		chunk := entries[start:end]
		var (
			failed     int
			firstErrID string
			firstErr   error
		)
		for i, out := range q.runAdmitScripts(ctx, zoneID, chunk) {
			err := out.err
			if err == nil {
				switch out.result {
				case admitEntryWritten, admitEntryReplayed:
					admitted++
				case admitEntryGone:
					gone++
				case admitEntryMetaGone:
					metaGone++
				case admitEntryAlreadyAdmitted:
					already++
				default:
					err = fmt.Errorf("unexpected result %d", out.result)
				}
			}
			if err != nil {
				failed++
				if firstErr == nil {
					firstErrID, firstErr = chunk[i].queueID, err
				}
			}
		}
		if firstErr != nil {
			return admitted, fmt.Errorf("admit script queueId=%s admitted=%d skipped=%d failed=%d untried=%d: %w",
				firstErrID, admitted, skipped, failed, len(entries)-end, firstErr)
		}
		start = end
	}
	if firstSkipErr != nil {
		return admitted, fmt.Errorf("skipped %d entries without an admit slot, first queueId=%s: %w",
			skipped, firstSkippedID, firstSkipErr)
	}
	return admitted, nil
}

// admittedKeyTTL 是本副本每次写在途占位之后,整键至少还要剩的 TTL(记作 W)。只做垃圾回收,
// 见 admittedKeyGCSlack。
//
// 整键 TTL 只增不减:三条写路径(放行、取走、快速通道)都是「先 ZADD,剩余 PTTL < W 才 PEXPIRE W」。
// W 由各副本按自己的 AdmitTTL 算,排队开着时滚动调整 AdmitTTL 就有新旧两种 W 并存;若无条件
// PEXPIRE,小 W 副本的写入会把整键 TTL 压到大 W 副本写的占位之前,该 zone 随后一段时间没有写入时,
// 整键连同未到期的占位一起消失,下一拍最多多放一批。只增不减之后,整键到期时刻不早于任何一次写入的
// 「写入时刻 + 写入方的 W」,也就晚于那次写下的成员。代价:调小 AdmitTTL 之后,已续上的较大 TTL
// 要自然走完才收敛,只影响 zone 下线后键多留多久。
//
// 改脚本时的两个陷阱。其一,不能用 PEXPIRE … GT:没有 TTL 的键按「无限 TTL」处理,GT 永不成立,
// 刚由 ZADD 建出的键就永不过期。其二,必须先 ZADD 再读 PTTL:键不存在时 PTTL 回 -2,此时的
// PEXPIRE 打空(回 0),随后 ZADD 建出的键没有 TTL。先 ZADD 之后键一定存在,PTTL 只会是 -1(无 TTL)
// 或剩余毫秒,-1 < W 恒成立,新键一定被设上 TTL。
func (q *Queue) admittedKeyTTL() time.Duration {
	return max(q.admitTTL, q.handoffGrace) + admittedKeyGCSlack
}

// admittedHoldScript 把一个成员的在途占位记到给定的到期毫秒(成员已在则覆盖分数),并保证整键剩余
// TTL 不少于 W(只增不减,见 admittedKeyTTL)。放行与取走共用,写法与 reserveFastPathSlotScript 的
// 后两步相同,改一处要同步改另一处。
// (2026-10-15 更正)放行改由 admitEntryScript 写占位,本脚本只剩取走一处在用;admitEntryScript 的后两步
// 也是同一写法,现在是三处同步改。
//
//	KEYS[1] = admitted:zset:zone:{zone}
//	ARGV[1] = 占位到期毫秒   ARGV[2] = member(queueId)   ARGV[3] = W 毫秒
//	返回 1。不能不返回:脚本回 nil 时 go-redis 记为 redis.Nil,排在 MULTI 里会让 Exec 回 error。
var admittedHoldScript = redis.NewScript(`
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[3]) then
	redis.call('PEXPIRE', KEYS[1], ARGV[3])
end
return 1
`)

// holdAdmitted 把 member 在 zoneID 的在途占位记到「现在 + hold」到期(成员已在则覆盖分数),
// 并按只增不减续上整键的垃圾回收 TTL,两步在一段脚本里。
//
// c 传 q.rdb 时当场执行;传 MULTI 的 pipeline 时只是入队,结果由调用方的 Exec 给出。用 EVAL 而不是
// Script.Run:Run 先发 EVALSHA,遇到 NOSCRIPT 才回退 EVAL,而排在 pipeline 里的命令要到 Exec 才知道
// 结果,回退来不及;MULTI 里的 EVALSHA 若撞上 NOSCRIPT(Redis 重启、SCRIPT FLUSH 之后),放行槽照样
// 写进去、占位却没写。脚本只有几行,每次带上全文的开销可以忽略。
// (2026-10-15 补注)放行改用 admitEntryScript 之后,只剩取走路径调用,传的是 q.rdb(当场执行)。
func (q *Queue) holdAdmitted(ctx context.Context, c redis.Scripter, zoneID uint32, member string, hold time.Duration) *redis.Cmd {
	return admittedHoldScript.Eval(ctx, c,
		[]string{admittedZSetKey(zoneID)},
		q.now().Add(hold).UnixMilli(), member, q.admittedKeyTTL().Milliseconds(),
	)
}

// admitEntryScript 的返回值,全部非零(2026-10-18 起;此前是 0/1/2 三种)。各分支的含义与写入见该脚本的注释。
const (
	admitEntryGone            = 1 // 成员已不在队列,什么都没写,不计数
	admitEntryMetaGone        = 2 // meta 不在或不是 hash,已 ZREM 成员,不计数
	admitEntryAlreadyAdmitted = 3 // 成员仍在队列但 meta 已有 admitted_at(上一次部分写入),已 ZREM,不计数
	admitEntryWritten         = 4 // 已写 admitted_at、放行槽、在途占位并 ZREM,计数
	admitEntryReplayed        = 5 // go-redis 重发了本次已执行成功的同一条命令,什么都没写,计数
)

// admitEntryScript 放行一个条目:先判它仍在队列,再在同一段里写 meta 的 admitted_at、放行槽与在途占位,最后把它
// 移出队列,对其它客户端同时可见(2026-10-18 起,替代 2026-10-15 版的「先判 meta 在、admitted_at 不在,再写
// SET → HSET → ZADD」;那一版由 ZPOPMIN 之后调用,出错即丢条目)。
//
//	KEYS[1] = admit:{queueId}   KEYS[2] = queue:meta:{queueId}   KEYS[3] = admitted:zset:zone:{zone}
//	KEYS[4] = queue:zone:{zone}
//	ARGV[1] = AdmitSlot JSON    ARGV[2] = AdmitTTL 毫秒          ARGV[3] = 字段名(metaFieldAdmittedAt)
//	ARGV[4] = 放行时刻 unix 毫秒(time.Now,与 meta 的 createdAt 同源)
//	ARGV[5] = 占位到期毫秒(Queue.now + AdmitTTL)   ARGV[6] = member(queueId)   ARGV[7] = W 毫秒(admittedKeyTTL)
//	返回 admitEntry* 常量之一(1~5),从不回 nil,也不回 Lua false。
//
// 分支:
//   - GONE(1):成员已不在队列(另一副本或重叠 leader 已放行或已清理),什么都没写,不计数。
//   - REPLAYED(5):成员已不在队列,且 admitted_at 等于本次的 ARGV[4],即 go-redis 重发了本次已执行成功的同一条
//     命令。什么都不写,计入放行数。
//   - META_GONE(2):meta 不在或不是 hash,ZREM 成员,不写放行、不占名额、不计数。meta 不在时 token 索引同样不在
//     (同一个 MULTI、同一个空闲 TTL),再没有人能取到放行槽;也不能直接 HSET,会建出没有 TTL 的 meta。
//   - ALREADY(3):成员仍在队列,但 meta 已有 admitted_at,只可能是上一次非写满原因造成的部分写入(见下)。ZREM,
//     不计数。
//   - WRITTEN(4):已写 admitted_at、放行槽、在途占位并 ZREM 成员,计数。
//
// 为什么这样写:
//
// (a) 第一步 ZSCORE 让每个条目只会被一个副本写一次:两个 leader ZRANGE 到同一批时,后到的拿到 GONE。go-redis 对
// 单条命令在读超时 / EOF 之后会自动重发(MaxRetries 默认 3),第一次已执行时重发会看到成员已不在,所以有 REPLAYED
// 分支,把这次放行照样计入。残余:另一副本恰在同一毫秒放行同一条目时会重复计数,只影响指标。
//
// (b) 判 meta 用 TYPE 而不是 EXISTS:meta 被写成别的类型时,HEXISTS / HSET 会报 WRONGTYPE、整批停在这一条,
// 成了卡死整个 zone 的毒丸;判成 META_GONE 就地清掉。TYPE 在 Lua 里返回 {ok=...} 状态表,miniredis 与 Redis 一致。
//
// (c) 写满(maxmemory + noeviction)语义:Redis 6/7 兼容模式(无 shebang)下,EVAL / EVALSHA 本身不带 DENYOOM,
// 写满时脚本照常开始执行。脚本里只有「脚本开跑前已判定 OOM,且尚未执行过任何写命令」时,才拒绝那条 DENYOOM 写
// (scriptVerifyOOM:pre_command_oom_state && !SCRIPT_WRITE_DIRTY && CMD_DENYOOM);一旦执行过任何带 WRITE 标志的
// 命令(包括没删到东西的 ZREM、PEXPIRE),后续命令不再受 maxmemory 约束。所以写段的第一条写必须是 HSET(DENYOOM),
// 它之前只能有只读命令(ZSCORE、TYPE、HEXISTS、HGET):写满时整段被拒,什么都没写,条目原样留在队列;不写满时
// 整段写完,中途越线也不停。不得把 ZREM、PEXPIRE 等非 DENYOOM 写挪到 HSET 前面,否则写满时会静默写穿 maxmemory,
// fail-closed 变成 fail-open。META_GONE / ALREADY 分支的 ZREM 不是 DENYOOM 命令,写满时照常清理,这是有意的。
// 不用 #!lua:miniredis 不解析 shebang;而且带 shebang 不带 flags 时,Redis 会在写满时整段拒绝,连上述清理也做不了。
//
// (d) 部分写入只来自非写满错误。实际可达的只有一种:在途占位键被改成别的类型,ZADD 报 WRONGTYPE。此时 HSET 与 SET
// 已写,成员仍在队列。客户端仍能取走放行槽(ADMITTED),或在 AdmitTTL 后得到 EXPIRED(admit_window_passed),不会
// 双签。同一个键也会让 FreeSlots 的 AdmittedCount 报错,所以修复之前不会再放行;修好之后的下一拍走 ALREADY,ZREM
// 掉成员。这个人没有占位,至多多放 1 人、持续一个 AdmitTTL。SET 只会因 PX ≤ 0 失败,已被 ValidateTTLs 挡住;
// 理论上若失败,Lookup 会回 EXPIRED(admit_window_passed),客户端重排,下一拍走 ALREADY 清理。
//
// (e) 占位两步「先 ZADD、PTTL < W 才 PEXPIRE W」与 admittedHoldScript、reserveFastPathSlotScript 同一写法,改一处要
// 三处同步(两个陷阱见 admittedKeyTTL)。HSET 只在 meta 已是 hash 时执行,不改 meta 的 TTL;SET 带 PX;ZREM 不建键。
// 本脚本不会造出没有 TTL 的键。
//
// (f) 只返回整数:go-redis v9 默认 RESP3,真实 Redis 会把 Lua false 回成布尔值,miniredis 回成 null,两边行为不同。
//
// (g) 排队族的键分属不同 hash slot,不支持 Cluster。KEYS 全部声明是脚本契约本身,与是否兼容 Cluster 无关。
var admitEntryScript = redis.NewScript(`
if not redis.call('ZSCORE', KEYS[4], ARGV[6]) then
	if redis.call('TYPE', KEYS[2]).ok == 'hash' and redis.call('HGET', KEYS[2], ARGV[3]) == ARGV[4] then
		return 5
	end
	return 1
end
if redis.call('TYPE', KEYS[2]).ok ~= 'hash' then
	redis.call('ZREM', KEYS[4], ARGV[6])
	return 2
end
if redis.call('HEXISTS', KEYS[2], ARGV[3]) == 1 then
	redis.call('ZREM', KEYS[4], ARGV[6])
	return 3
end
redis.call('HSET', KEYS[2], ARGV[3], ARGV[4])
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZADD', KEYS[3], ARGV[5], ARGV[6])
if redis.call('PTTL', KEYS[3]) < tonumber(ARGV[7]) then
	redis.call('PEXPIRE', KEYS[3], ARGV[7])
end
redis.call('ZREM', KEYS[4], ARGV[6])
return 4
`)

// popAdmitPipelineChunk:PopAdmit 探路之后,一个 pipeline 里至多放多少条放行脚本(2026-11-06,缺口清单第 14 条)。
//
// 一块一次往返,放行速率从约 1 / 往返升到约 128 / 往返。块大小按 Redis 那一侧定:一条放行脚本十来条命令、约十几微秒,
// 一块约 2ms,单次占住 Redis 的时间仍短,别的客户端(轮询、快速通道)排在块与块之间;一块的请求约 128 × 1KB
// (每条带脚本全文,见 runAdmitScripts)。没有压测基线,值是按上面的量级定的,压测后可调。
const popAdmitPipelineChunk = 128

// admitEntryInput 是一条待放行的条目:queueId 与挑好 gate 的放行槽 JSON。
type admitEntryInput struct {
	queueID  string
	slotJSON []byte
}

// admitEntryOutcome 是一段放行脚本的结果:err 为 nil 时 result 取 admitEntry* 常量之一;err 非 nil 时这条没有确定的
// 结果(写满、WRONGTYPE、连接 / 超时、取消),处理见 PopAdmit 的停批规则。
type admitEntryOutcome struct {
	result int64
	err    error
}

// pickAdmitSlot 为一个条目挑 gate 并编好放行槽 JSON。挑失败、回 (nil, nil) 或 Marshal 失败都回错误,调用方跳过该条。
func pickAdmitSlot(pickFn PickAdmitSlotFn, zoneID uint32, queueID string) ([]byte, error) {
	slot, err := pickFn(zoneID, queueID)
	if err != nil {
		return nil, fmt.Errorf("pick gate: %w", err)
	}
	if slot == nil {
		return nil, errors.New("pick returned no admit slot")
	}
	slotJSON, err := json.Marshal(slot)
	if err != nil {
		return nil, fmt.Errorf("marshal admit slot: %w", err)
	}
	return slotJSON, nil
}

// runAdmitScripts 对一块条目各跑一段 admitEntryScript,结果与 entries 同下标。只由 PopAdmit 调用。
//   - 一条时单发(admitEntry,Script.Run:先 EVALSHA,NOSCRIPT 时当场回退 EVAL)。
//   - 多条时放进一个 pipeline 一次往返,每条发 EVAL 全文:pipeline 里的 EVALSHA 撞上 NOSCRIPT(Redis 重启、SCRIPT FLUSH
//     之后)要等整块回来才知道,那几条没执行、还得另发一遍;脚本约 1KB,带全文的开销可以接受(与 holdAdmitted 同一取舍)。
//     各条仍是独立的一段脚本,一条出错不影响同块别的条目执行。
//
// 每条的结果与错误看它自己的 Cmd:连接 / 超时出错时 go-redis 给整块的 Cmd 都设上错误;Exec 的返回值只是其中第一条的
// 错误,不另看。
func (q *Queue) runAdmitScripts(ctx context.Context, zoneID uint32, entries []admitEntryInput) []admitEntryOutcome {
	out := make([]admitEntryOutcome, len(entries))
	if len(entries) == 1 {
		out[0].result, out[0].err = q.admitEntry(ctx, zoneID, entries[0].queueID, entries[0].slotJSON)
		return out
	}
	pipe := q.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(entries))
	for i, e := range entries {
		cmds[i] = admitEntryScript.Eval(ctx, pipe, admitEntryKeys(zoneID, e.queueID), q.admitEntryArgs(e.queueID, e.slotJSON)...)
	}
	_, _ = pipe.Exec(ctx)
	for i, cmd := range cmds {
		out[i].result, out[i].err = cmd.Int64()
	}
	return out
}

// admitEntry 对 queueID 跑一次 admitEntryScript,返回值见该脚本。由 runAdmitScripts 在一块只有一条时调用。
// 不在 pipeline 里,用 Script.Run(先 EVALSHA,NOSCRIPT 时回退 EVAL):EVALSHA 撞上 NOSCRIPT 时脚本没有执行,
// 当场就能回退,不存在 holdAdmitted 注释里「排在 MULTI 里来不及回退」的问题。
// (2026-10-18 起多传队列键 KEYS[4];KEYS[1..3] 与全部 ARGV 同位不变。四个键都在排队族同一个实例上。)
func (q *Queue) admitEntry(ctx context.Context, zoneID uint32, queueID string, slotJSON []byte) (int64, error) {
	return admitEntryScript.Run(ctx, q.rdb, admitEntryKeys(zoneID, queueID), q.admitEntryArgs(queueID, slotJSON)...).Int64()
}

// admitEntryKeys 与 admitEntryArgs 是 admitEntryScript 的 KEYS 与 ARGV(次序见该脚本注释),单发与 pipeline 共用一份。
// ARGV[4] 的放行时刻在这里取墙钟:go-redis 重发同一条命令时带的是同一个值,脚本据此认出 REPLAYED。
func admitEntryKeys(zoneID uint32, queueID string) []string {
	return []string{admitKey(queueID), metaKey(queueID), admittedZSetKey(zoneID), zoneZSetKey(zoneID)}
}

func (q *Queue) admitEntryArgs(queueID string, slotJSON []byte) []any {
	return []any{
		slotJSON, q.admitTTL.Milliseconds(), metaFieldAdmittedAt, time.Now().UnixMilli(),
		q.now().Add(q.admitTTL).UnixMilli(), queueID, q.admittedKeyTTL().Milliseconds(),
	}
}

// admittedCountScript 清掉已到期的占位再计数,两步在同一段脚本里。
//
//	KEYS[1] = admitted:zset:zone:{zone}
//	ARGV[1] = now_ms
//	返回未到期的成员数
var admittedCountScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
return redis.call('ZCARD', KEYS[1])
`)

// AdmittedCount 返回 zone 当前的在途占位数:已放行还没来取的,加上票已交出去、还在
// handoffGrace 之内的(排队取走与快速通道)。FreeSlots 用它扣容量,避免上一批还没出现在
// gate 的人数里就再放一批。先清过期成员再数,所以它是一条**写命令**。
func (q *Queue) AdmittedCount(ctx context.Context, zoneID uint32) (uint32, error) {
	n, err := admittedCountScript.Run(ctx, q.rdb,
		[]string{admittedZSetKey(zoneID)}, q.now().UnixMilli()).Int64()
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}

// reserveFastPathSlotScript 原子占位:先清掉已到期的占位,未到期的不足 budget 才记一枚
// 一次性占位成员。budget = cap - online,即「在当前在线数之上还允许多少在途准入」。
// 清理、判定、占位在同一段脚本里,并发的快速通道调用彼此可见,不会各读同一个 free>0
// 快照后全部旁路队列签票。
//
// 占位之后的整键续期与 admittedHoldScript 相同:先 ZADD,剩余 PTTL < W 才 PEXPIRE W(只增不减,
// 两个陷阱见 admittedKeyTTL)。
//
//	KEYS[1] = admitted:zset:zone:{zone}
//	ARGV[1] = now_ms   ARGV[2] = budget   ARGV[3] = member(fast:<uuid>)
//	ARGV[4] = 占位到期毫秒   ARGV[5] = W 毫秒(整键 TTL 下限,只做垃圾回收)
//	返回 1 = 占到,0 = 已满
var reserveFastPathSlotScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[3])
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[5]) then
	redis.call('PEXPIRE', KEYS[1], ARGV[5])
end
return 1
`)

// TryReserveFastPathSlot 尝试为快速通道原子占用一个准入名额。budget 传 cap-online。
// 返回 true 表示占用成功(调用方可直接签发 gate token);false 表示已满(应入队)。
// 快速通道当场交票,等同排队路径的「已取走」,所以占位只留 handoffGrace,不是 AdmitTTL;
// 之后它按自己的分数到期,不需要、也没有回收步骤。
func (q *Queue) TryReserveFastPathSlot(ctx context.Context, zoneID uint32, budget uint32) (bool, error) {
	if budget == 0 {
		return false, nil
	}
	now := q.now()
	member := "fast:" + uuid.NewString()
	res, err := reserveFastPathSlotScript.Run(
		ctx, q.rdb,
		[]string{admittedZSetKey(zoneID)},
		now.UnixMilli(), budget, member,
		now.Add(q.handoffGrace).UnixMilli(), q.admittedKeyTTL().Milliseconds(),
	).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// QueueLen is a thin wrapper over ZCARD for capacity decisions.
func (q *Queue) QueueLen(ctx context.Context, zoneID uint32) (uint32, error) {
	n, err := q.rdb.ZCard(ctx, zoneZSetKey(zoneID)).Result()
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}
