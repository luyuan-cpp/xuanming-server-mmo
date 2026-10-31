package logic

import (
	"context"
	"errors"
	"hash/fnv"
	"sort"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/redisslot"
	"shared/safego"
)

// 「成员登记 + 段租约」分片(docs/design/redis-player-store-cluster.md §5.4、§5.9):把 16384 个 slot 的后台扫描分给
// 存活副本,副本多了各扫一份,副本全没了下一个启动的副本接管全部。
//
//   - 成员登记:每个副本每周期在成员 ZSET 上续心跳(分数是到期毫秒)并清掉过期成员,存活成员数 N 取这里的计数,
//     没有持段的副本也算在内,所以新副本能拿到份额。
//   - 段租约:256 段,每段 64 个连续 slot。段 c 的租约键值为实例身份,SET NX PX 取得,按令牌续约。
//   - 公平份额 share = ceil(256 / N):持有不足 share 时接管无主段,超过时释放多出来的。所有存活成员都持满 share 时
//     总量不小于 256,所以只要有一个副本活着,无主段都会在一个周期内被接管(N=1 时 share=256)。
//   - 一周期先读一遍全部 256 把租约键。读失败的段(主节点挂了或这个副本到不了它)这一周期不算持有:不续、不扫,
//     到期后归能连上它的副本。扫不到的段握在手里没有意义。
//   - 有段读失败的副本这一周期**不算成员**(撤销登记),但照常持有读得到的段。否则它占着一份份额却接不了那些段,
//     别的副本又持满了自己的份额,那些段就一直没人扫。这样只要有一个副本读得全,它的份额就够接下全部无主段;
//     所有副本都读不全(主节点真挂了)时成员数归零,按 1 算,各自接管读得到的无主段,故障转移后恢复。
//
// 段租约只是分担负载,**不是正确性 fence**:两个副本短暂扫同一段无害,互斥由认领脚本和整字节 CAS 保证。
// 所以成员到期按各副本的时钟算,时钟偏差只会让份额短暂算偏一点。
//
// 同一套分片有两个实例(role):LeaseMonitor 只依赖 Redis,死 gate 清扫还依赖 etcd。分开之后 etcd 不可达只让清扫让出段,
// 断线租约照常到期。

const (
	slotSegmentCount = 256
	slotsPerSegment  = redisslot.SlotCount / slotSegmentCount

	shardCycleInterval = 10 * time.Second
	// shardLeaseTTL / shardMemberTTL 是 3 个周期:一次抖动漏一拍不至于丢段或被算成已退出。
	shardLeaseTTL  = 30 * time.Second
	shardMemberTTL = 30 * time.Second

	shardRoleLeaseMonitor = "lease_monitor"
	shardRoleGateSweep    = "gate_sweep"
)

// slotOwner 是后台扫描对分片的全部依赖:当前分给本副本的 slot。返回的切片升序、只读。
type slotOwner interface {
	OwnedSlots() []uint16
}

// shardHeartbeatScript 清掉过期成员、登记或撤销自己、返回存活成员数。KEYS[1]=成员 ZSET;
// ARGV[1]=当前毫秒,ARGV[2]=本成员到期毫秒,ARGV[3]=实例身份,ARGV[4]=ZSET 自身的过期毫秒(全部副本退出后回收),
// ARGV[5]="1" 登记 / "0" 撤销。
var shardHeartbeatScript = redis.NewScript(`
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[1])
if ARGV[5] == "1" then
    redis.call("ZADD", KEYS[1], ARGV[2], ARGV[3])
    redis.call("PEXPIRE", KEYS[1], ARGV[4])
else
    redis.call("ZREM", KEYS[1], ARGV[3])
end
return redis.call("ZCARD", KEYS[1])
`)

// shardRenewScript 续约自己持有的段。KEYS[1]=段租约;ARGV[1]=实例身份,ARGV[2]=租约毫秒。返回 1 = 已续。
var shardRenewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
redis.call("PEXPIRE", KEYS[1], ARGV[2])
return 1
`)

// shardReleaseScript 释放自己持有的段。KEYS[1]=段租约;ARGV[1]=实例身份。
var shardReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
return redis.call("DEL", KEYS[1])
`)

type slotShard struct {
	rdb  redis.UniversalClient
	role string
	self string
	// healthy 为 false 时本副本退出分片:释放全部段、撤销成员登记,份额归其余副本,恢复后自动回来。
	// 这是 §5.4 的「续约前提」:扫描停了就不该继续握着段。nil = 总是健康。
	healthy func() bool
	now     func() time.Time

	// lastMembers 是最近一次心跳读到的成员数;0 = 还没读到过,这时不调整持有量。只由跑 cycle 的 goroutine 访问。
	lastMembers int64
	// segments / slots 是已发布的持有结果,跨 goroutine 读:存不可变切片。
	segments atomic.Pointer[[]int]
	slots    atomic.Pointer[[]uint16]
	stopped  atomic.Bool
}

func newSlotShard(rdb redis.UniversalClient, role, self string, healthy func() bool) *slotShard {
	return &slotShard{rdb: rdb, role: role, self: self, healthy: healthy, now: time.Now}
}

// OwnedSlots 返回当前持有的段里的全部 slot(升序)。
func (s *slotShard) OwnedSlots() []uint16 {
	if p := s.slots.Load(); p != nil {
		return *p
	}
	return nil
}

// Run 按周期调整持有的段,直到 ctx 结束。第一周期立即执行,N=1 时启动后马上持有全部段。
func (s *slotShard) Run(ctx context.Context) {
	ticker := time.NewTicker(shardCycleInterval)
	defer ticker.Stop()
	for {
		// recover 收在一周期:分片循环停摆的后果是这个副本从此不扫、段也不放,要等租约过期才归别人。
		safego.Run("player_locator.slot_shard.cycle", func() {
			cycleCtx, cancel := context.WithTimeout(ctx, shardCycleInterval)
			defer cancel()
			s.cycle(cycleCtx)
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop 优雅退出时主动让出:释放全部段、撤销成员登记,不把它们留到 TTL 自然过期。幂等,跨 goroutine 安全。
// go-zero 的 SIGTERM 走 os.Exit,main 的 defer 不执行,所以挂到 proc.AddShutdownListener。
func (s *slotShard) Stop() {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.leave(ctx)
}

// cycle 是一周期:核对健康 → 读全部段的持有者 → 心跳取成员数 → 续约 → 按公平份额释放或接管 → 发布结果。
func (s *slotShard) cycle(ctx context.Context) {
	if s.stopped.Load() {
		return
	}
	if s.healthy != nil && !s.healthy() {
		s.leave(ctx)
		return
	}

	held, free, complete := s.readSegments(ctx)
	members, err := s.heartbeat(ctx, complete)
	if err != nil {
		// 成员 ZSET 所在的主节点不可用:沿用上一次的成员数,持有的段照常续约,故障转移之后恢复。
		logx.Errorf("SlotShard: role=%s heartbeat failed, keeping the last member count %d: %v", s.role, s.lastMembers, err)
	} else {
		s.lastMembers = max(members, 1) // 自己没登记、别人也都不在时按 1 算
	}

	kept := s.renew(ctx, held)

	acquired, released := 0, 0
	if s.lastMembers > 0 {
		share := fairShare(s.lastMembers)
		switch {
		case len(kept) > share:
			released = s.release(ctx, kept[share:])
			kept = kept[:share]
		case len(kept) < share:
			got := s.acquire(ctx, free, share-len(kept))
			acquired = len(got)
			kept = append(kept, got...)
		}
	}
	sort.Ints(kept)
	s.publish(kept)
	if acquired > 0 || released > 0 {
		logx.Infof("SlotShard: role=%s self=%s members=%d segments=%d acquired=%d released=%d",
			s.role, s.self, s.lastMembers, len(kept), acquired, released)
	}
}

// fairShare 是每个存活成员的段配额:ceil(256 / members)。
func fairShare(members int64) int {
	return int((slotSegmentCount + members - 1) / members)
}

// heartbeat 取存活成员数。register=false 时撤销自己的登记(有段读不到,见文件头)。
func (s *slotShard) heartbeat(ctx context.Context, register bool) (int64, error) {
	now := s.now()
	registerFlag := "0"
	if register {
		registerFlag = "1"
	}
	return shardHeartbeatScript.Run(ctx, s.rdb, []string{shardMembersKey(s.role)},
		now.UnixMilli(), now.Add(shardMemberTTL).UnixMilli(), s.self, (2 * shardMemberTTL).Milliseconds(), registerFlag,
	).Int64()
}

// readSegments 读全部段的持有者,一条 pipeline(Cluster 下按节点拆)。held = 持有者是自己,free = 无主。
// 读失败的段两边都不算(见文件头)。complete = 没有因为连不上而读不到的段;键类型不对只是那一把键坏了,不算。
func (s *slotShard) readSegments(ctx context.Context) (held, free []int, complete bool) {
	cmds := make([]*redis.StringCmd, slotSegmentCount)
	// 无主的段 GET 回 redis.Nil,Pipelined 会把它当第一个错误返回,所以不看返回值,逐条判。
	_, _ = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for c := range cmds {
			cmds[c] = p.Get(ctx, shardSegmentKey(s.role, c))
		}
		return nil
	})
	complete = true
	failed := 0
	var firstErr error
	for c, cmd := range cmds {
		owner, err := cmd.Result()
		switch {
		case err == nil:
			if owner == s.self {
				held = append(held, c)
			}
		case errors.Is(err, redis.Nil):
			free = append(free, c)
		default:
			failed++
			if firstErr == nil {
				firstErr = err
			}
			if !isKeyTypeError(err) {
				complete = false
			}
		}
	}
	if failed > 0 {
		logx.Errorf("SlotShard: role=%s could not read %d segment lease(s), skipping them this cycle: %v", s.role, failed, firstErr)
	}
	return held, free, complete
}

// renew 给 held 里的段续约,返回确实续上的段。续失败或已被别人持有的不再算持有。
func (s *slotShard) renew(ctx context.Context, held []int) []int {
	kept := make([]int, 0, len(held))
	for _, c := range held {
		renewed, err := shardRenewScript.Run(ctx, s.rdb, []string{shardSegmentKey(s.role, c)},
			s.self, shardLeaseTTL.Milliseconds()).Int()
		if err != nil {
			logx.Errorf("SlotShard: role=%s renew segment %d failed: %v", s.role, c, err)
			continue
		}
		if renewed == 1 {
			kept = append(kept, c)
		}
	}
	return kept
}

// release 释放 segments,返回确实释放的段数。释放失败的段不再扫,租约按 TTL 过期;
// 这期间下一周期又读到它归自己,照常续约再释放一次。
func (s *slotShard) release(ctx context.Context, segments []int) int {
	released := 0
	for _, c := range segments {
		n, err := shardReleaseScript.Run(ctx, s.rdb, []string{shardSegmentKey(s.role, c)}, s.self).Int()
		if err != nil {
			logx.Errorf("SlotShard: role=%s release segment %d failed: %v", s.role, c, err)
			continue
		}
		if n == 1 {
			released++
		}
	}
	return released
}

// acquire 从 free 里最多接管 need 个段。起点按实例身份散开,副本们不总在抢同一段;抢输了换下一段。
func (s *slotShard) acquire(ctx context.Context, free []int, need int) []int {
	if len(free) == 0 || need <= 0 {
		return nil
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(s.self))
	start := int(hash.Sum32() % uint32(len(free)))
	got := make([]int, 0, need)
	for i := range free {
		if len(got) == need {
			break
		}
		c := free[(start+i)%len(free)]
		ok, err := s.rdb.SetNX(ctx, shardSegmentKey(s.role, c), s.self, shardLeaseTTL).Result()
		if err != nil {
			logx.Errorf("SlotShard: role=%s acquire segment %d failed: %v", s.role, c, err)
			continue
		}
		if ok {
			got = append(got, c)
		}
	}
	return got
}

// leave 退出分片:释放已发布的全部段、撤销成员登记、清空持有结果。先清结果,扫描立刻停在这些段上。
func (s *slotShard) leave(ctx context.Context) {
	var segments []int
	if p := s.segments.Load(); p != nil {
		segments = *p
	}
	s.publish(nil)
	if released := s.release(ctx, segments); released > 0 {
		logx.Infof("SlotShard: role=%s self=%s left the shard, released %d segment(s)", s.role, s.self, released)
	}
	if err := s.rdb.ZRem(ctx, shardMembersKey(s.role), s.self).Err(); err != nil {
		// 撤销失败只是份额多算一个成员,最迟一个成员 TTL 后过期。
		logx.Errorf("SlotShard: role=%s remove member failed: %v", s.role, err)
	}
}

// publish 发布持有结果。segments 须升序。
func (s *slotShard) publish(segments []int) {
	slots := make([]uint16, 0, len(segments)*slotsPerSegment)
	for _, c := range segments {
		for offset := 0; offset < slotsPerSegment; offset++ {
			slots = append(slots, uint16(c*slotsPerSegment+offset))
		}
	}
	s.segments.Store(&segments)
	s.slots.Store(&slots)
	reconcileSlotShardSegments.Set(float64(len(segments)), s.role)
}
