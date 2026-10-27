package saveoutbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"shared/kafkautil"
	"shared/placement"
	"shared/savebarrier"
)

// Publisher 只有 broker acks=all 确认才可回 nil；结果未知也可晚到，调用者仍保留原意图重投。
type Publisher interface {
	Publish(context.Context, string, string, []byte) error
}

// StoreHealth 是落点库注册表的只读能力：本 zone 主库在冷却期内时补投整拍跳过(与重试循环同口径，L-04 §3.5)。
// 生产由 *proto_sql.StoreRegistry 实现；nil 表示不判冷却。
type StoreHealth interface {
	CoolingDown(storageID uint32) bool
}

// RouteGate 是本实例 go/db 读方就绪闸的只读视图(kafka/route_ready_gate.go,D1 第 3 批)。闸关着(mapping Redis 副本不可信:
// 重启、切主、就绪标记被对账 leader 撤掉、读脚本出错)时消费循环不从 Kafka 取新消息、重试循环不认领,补投出去的任务只会
// 堆在 Kafka 里;补投器据此整拍跳过、不认领、不计次(r2-rr#5),与重试循环同口径。
// 生产由 *kafka.KeyOrderedKafkaConsumer 实现:Placement.RequireRouteReady=false 时没有闸,恒放行。实现必须并发安全。
// 退场:负责人 2026-10-23 已定映射改为直读 TiDB(docs/handoff/d1-review-fix-handoff-20261023.md),届时 go/db 就绪闸作废。
// 本接缝随闸同批删除(本接口、KeyOrderedKafkaConsumer.RouteGateOpen、db.go 的注入、测试夹具 routeGateStub 与
// replay_route_gate_test.go 的闸用例),不要留一个恒放行的空实现;按消费积压暂停补投是另一条后续任务,不复用本接缝。
type RouteGate interface {
	RouteGateOpen() bool
}

type Config struct {
	ZoneID          uint32
	TopicGeneration uint32
	// 每拍只读一个确定的桶；最多Batch个玩家，总预算Budget，不全量SCAN玩家键。
	Batch    int
	Interval time.Duration
	Budget   time.Duration
	// FirstReplayAfter：首次看到某版本意图只登记，过了这段时间才第一次补投(正常路径的 Kafka 投递优先)。
	FirstReplayAfter time.Duration
	// RetryAfter：第 1 次补投后的等待；之后每次翻倍，封顶 MaxRetryAfter。
	RetryAfter    time.Duration
	MaxRetryAfter time.Duration
	// MaxAttempts：同一版本最多补投的次数；达到后只复查、不再补投，计 exhausted 并告警。
	MaxAttempts int
	// RecheckAfter：终态、耗尽、损坏的意图的复查间隔(只读状态，不补投)。
	RecheckAfter time.Duration
	// InflightWindow：go/db 最近一次把该玩家的写延后进重试之后，这段时间内视为仍有在途重试，补投只推迟。
	InflightWindow time.Duration
}

func (c Config) normalized() (Config, error) {
	if !placement.IsZoneStorage(c.ZoneID) {
		return c, errors.New("save outbox zone 不合法")
	}
	if c.TopicGeneration == 0 {
		c.TopicGeneration = 1
	}
	if c.Batch == 0 {
		c.Batch = 16
	}
	if c.Interval == 0 {
		c.Interval = 100 * time.Millisecond
	}
	if c.Budget == 0 {
		c.Budget = 2 * time.Second
	}
	if c.FirstReplayAfter == 0 {
		c.FirstReplayAfter = 30 * time.Second
	}
	if c.RetryAfter == 0 {
		c.RetryAfter = 10 * time.Second
	}
	if c.MaxRetryAfter == 0 {
		c.MaxRetryAfter = 10 * time.Minute
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 8
	}
	if c.RecheckAfter == 0 {
		c.RecheckAfter = 10 * time.Minute
	}
	if c.InflightWindow == 0 {
		c.InflightWindow = 2 * time.Minute
	}
	if c.Batch < 1 || c.Batch > 64 || c.Interval < 10*time.Millisecond || c.Interval > time.Second ||
		c.Budget < time.Millisecond || c.Budget > 10*time.Second ||
		c.FirstReplayAfter <= c.Budget || c.FirstReplayAfter > 10*time.Minute ||
		c.RetryAfter <= c.Budget || c.MaxRetryAfter < c.RetryAfter || c.MaxRetryAfter > time.Hour ||
		c.MaxAttempts < 1 || c.MaxAttempts > 64 ||
		c.RecheckAfter <= c.Budget || c.RecheckAfter > time.Hour ||
		c.InflightWindow < time.Second || c.InflightWindow > time.Hour {
		return c, errors.New("save outbox 有界轮询参数不合法")
	}
	return c, nil
}

// Replay 的桶游标仅由 Run/Tick 的单个调用者拥有；不能并发调用 Tick。
// 进程/副本间通过Redis短租期分工，所有投递仍由既有SQL围栏幂等。
type Replay struct {
	rc      redis.Cmdable
	mapping redis.Cmdable
	pub     Publisher
	health  StoreHealth
	route   RouteGate
	cfg     Config
	now     func() time.Time
	// jitter 返回本次排程附加的抖动；同一故障里大量意图同时到期时错开重投。
	jitter func() time.Duration
	bucket uint64
	// publishPausedUntil：上一次 broker 发布失败后整个补投器暂停到这一刻(r2-db#1)。只由 Tick 的调用者读写。
	publishPausedUntil time.Time
	results            *prometheus.CounterVec
}

// 结果 label 是封闭集合，启动时以 0 建出全部序列。
var replayResults = []string{"published", "completed", "migrated", "conflict", "invalid", "redis_error", "publish_error",
	"observed", "inflight_deferred", "terminal", "exhausted", "store_cooldown", "legacy_alive", "publish_paused", "route_not_ready"}

// New 装配补投器。health 为 nil 表示不判主库冷却；route 不得为 nil：漏传就绪闸时补投会在消费停摆期间照常重投并计次(r2-rr#5)，
// 装配错误按依赖缺失拒启，不静默当作恒放行。
func New(rc, mapping redis.Cmdable, pub Publisher, health StoreHealth, route RouteGate, cfg Config, reg prometheus.Registerer) (*Replay, error) {
	if rc == nil || mapping == nil || pub == nil {
		return nil, errors.New("save outbox Redis/publisher 依赖缺失")
	}
	if route == nil {
		return nil, errors.New("save outbox 读方就绪闸依赖缺失")
	}
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "db_save_outbox_total",
		Help: "玩家持久存盘意图补投结果；published仅指broker确认，completed要求两表SQL屏障ACK；terminal/exhausted为不再补投的意图(按复查间隔重复计数)。"}, []string{"result"})
	if err := reg.Register(counter); err != nil {
		if existing, ok := err.(prometheus.AlreadyRegisteredError); ok {
			var same bool
			counter, same = existing.ExistingCollector.(*prometheus.CounterVec)
			if !same {
				return nil, errors.New("save outbox 指标类型冲突")
			}
		} else {
			return nil, err
		}
	}
	for _, label := range replayResults {
		counter.WithLabelValues(label)
	}
	retryJitter := cfg.RetryAfter / 2
	return &Replay{rc: rc, mapping: mapping, pub: pub, health: health, route: route, cfg: cfg, now: time.Now, results: counter,
		jitter: func() time.Duration { return rand.N(retryJitter + 1) }}, nil
}

func (r *Replay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			logx.Errorf("[SaveOutbox] 本拍补投未完成，原意图保留: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick 返回本拍成功broker确认的DBTask条数。任何失败都保留意图，不根据Kafka ACK清理。
func (r *Replay) Tick(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Budget)
	defer cancel()
	bucket := r.bucket
	r.bucket = (r.bucket + 1) % BucketCount
	// broker 刚发布失败:整拍跳过 RetryAfter,不在 Kafka 故障期间逐拍认领、推迟别的意图并刷 ERROR(r2-db#1)。
	if r.now().Before(r.publishPausedUntil) {
		r.results.WithLabelValues("publish_paused").Inc()
		return 0, nil
	}
	// 本实例读方就绪闸关着(r2-rr#5)：消费循环不取新消息，投出去的任务只会堆在 Kafka 里挤占 retention.bytes 的保留缓冲，
	// broker 确认后却照常计次，停摆久了意图被耗到 exhausted，而原任务与补投副本可能已被裁掉。整拍跳过、不认领、不计次；
	// 闸重开后从各意图原有的到期时刻接着补投。
	if r.routeGateClosed() {
		return 0, nil
	}
	// 主库冷却期间投出去的任务只会在选库处被挡回、各自开一条重试链；整拍跳过，不消耗尝试次数。
	if r.health != nil && r.health.CoolingDown(r.cfg.ZoneID) {
		r.results.WithLabelValues("store_cooldown").Inc()
		return 0, nil
	}
	// 本 zone 仍有 legacy 实例在消费时，补投的旧意图可能被它无条件写进 MySQL(go-db-pipeline#2)；等它按世代门禁退出。
	alive, err := legacyAlive(ctx, r.rc, r.cfg.ZoneID)
	if err != nil {
		r.results.WithLabelValues("redis_error").Inc()
		return 0, errors.New("save outbox 读取legacy存活标记失败")
	}
	if alive {
		r.results.WithLabelValues("legacy_alive").Inc()
		return 0, nil
	}
	index := pendingIndexKey(r.cfg.ZoneID, bucket)
	now := r.now().UnixMilli()
	if now <= 0 {
		return 0, errors.New("save outbox 时钟不合法")
	}
	ids, err := r.rc.ZRangeByScore(ctx, index, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now, 10), Count: int64(r.cfg.Batch)}).Result()
	if err != nil {
		r.results.WithLabelValues("redis_error").Inc()
		return 0, errors.New("save outbox 读取桶索引失败")
	}
	published := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return published, err
		}
		// 闸在本拍中途关上：剩下的玩家留到闸重开(与重试循环每认领一条前看一次闸同口径)，当拍至多多投正在处理的那一名。
		if r.routeGateClosed() {
			return published, nil
		}
		playerID, err := strconv.ParseUint(id, 10, 64)
		if err != nil || playerID == 0 || kafkautil.DBTaskKey(playerID) != id || playerID%BucketCount != bucket {
			r.results.WithLabelValues("invalid").Inc()
			// 坏member也推迟，不能让一页坏数据永远饿死后续玩家；不删除证据。
			if err := postponeInvalidMemberScript.Run(ctx, r.rc, []string{index}, id, now, now+r.cfg.RecheckAfter.Milliseconds()).Err(); err != nil {
				return published, errors.New("save outbox 推迟坏索引失败")
			}
			continue
		}
		reply, err := claimScript.Run(ctx, r.rc, []string{savebarrier.Key(playerID), index}, id, now,
			r.cfg.FirstReplayAfter.Milliseconds(), r.cfg.RetryAfter.Milliseconds(), r.cfg.MaxRetryAfter.Milliseconds(),
			r.cfg.MaxAttempts, r.jitter().Milliseconds(), r.cfg.InflightWindow.Milliseconds(), r.cfg.RecheckAfter.Milliseconds()).Slice()
		if err != nil {
			r.results.WithLabelValues("redis_error").Inc()
			return published, errors.New("save outbox 原子认领失败")
		}
		if len(reply) == 0 {
			return published, errors.New("save outbox 认领返回为空")
		}
		code, ok := reply[0].(int64)
		if !ok {
			return published, errors.New("save outbox 认领返回非法")
		}
		switch code {
		case 0:
			continue
		case 2:
			r.results.WithLabelValues("completed").Inc()
			continue
		case -1:
			r.results.WithLabelValues("invalid").Inc()
			logx.Errorf("[SaveOutbox] 存盘意图缺失或损坏，保留待人工核对 player=%d", playerID)
			continue
		case 3:
			r.results.WithLabelValues("terminal").Inc()
			reason := ""
			if len(reply) > 1 {
				reason, _ = reply[1].(string)
			}
			logx.Errorf("[SaveOutbox] 存盘意图已被go/db判为确定性拒写，停止补投、保留原字节与死信 player=%d reason=%s", playerID, reason)
			continue
		case 4:
			r.results.WithLabelValues("exhausted").Inc()
			logx.Errorf("[SaveOutbox] 存盘意图补投已达上限仍未取得两表SQL ACK，停止补投、保留原字节待人工处置 player=%d attempts=%v", playerID, reply[1:])
			continue
		case 5:
			r.results.WithLabelValues("observed").Inc()
			continue
		case 6:
			r.results.WithLabelValues("inflight_deferred").Inc()
			continue
		case 1:
		default:
			return published, fmt.Errorf("save outbox 未知认领结局 %d", code)
		}
		in, attempts, err := decodeIntent(reply)
		if err != nil {
			r.results.WithLabelValues("invalid").Inc()
			continue
		}
		// 意图本身不合法时重投也不会变好:照样计一次,按次数走到 exhausted 并告警,而不是每个退避周期报一次错直到永远。
		zone, err := validateIntent(playerID, in)
		if err != nil {
			r.results.WithLabelValues("invalid").Inc()
			logx.Errorf("[SaveOutbox] 存盘意图身份不合法，保留待核对: %v", err)
			if err := r.consumeAttempt(ctx, playerID, in.version, attempts); err != nil {
				return published, err
			}
			continue
		}
		if in.topic != topicFor(zone, r.cfg.TopicGeneration) {
			r.results.WithLabelValues("invalid").Inc()
			logx.Errorf("[SaveOutbox] 原topic世代未在当前配置放行，保留意图；切换世代前必须排空")
			if err := r.consumeAttempt(ctx, playerID, in.version, attempts); err != nil {
				return published, err
			}
			continue
		}
		if zone != r.cfg.ZoneID {
			// 映射改为直读 TiDB 后(docs/handoff/d1-review-fix-handoff-20261023.md §4)，这里改读 TiDB 真源，归新方向会话。
			home, err := r.mapping.Get(ctx, placement.HomeZoneKey(playerID)).Result()
			switch {
			case err == nil:
			case errors.Is(err, redis.Nil), redis.HasErrorPrefix(err, "WRONGTYPE"):
				// 键不存在，或被写成别的类型(GET 稳定回 WRONGTYPE)：都是该玩家映射数据本身的确定性问题，重读不会变好。
				// 按不匹配处理并计次，次数用完进 exhausted、触发 DbSaveOutboxExhausted；当成故障不计次的话，
				// 意图会永远停在基数退避上，返场一直被提交屏障拒绝而没有任何告警。
				if !errors.Is(err, redis.Nil) {
					logx.Errorf("[SaveOutbox] authoritative home_zone key holds a non-string value, counting the attempt player=%d: %v", playerID, err)
				}
				home = ""
			default:
				// mapping Redis 不可用(连接、超时，或重启切主期间的 LOADING / MASTERDOWN 等回复)、权限配置错误(NOPERM)
				// 与意图本身无关，且会同时波及所有跨 zone 意图：不计次，与发布失败同口径(r2-rr#5)，修好后自动恢复，不必逐人清计数。
				// 认领时已按本次退避推迟了该意图，下一次到期再读；持续出现时看 db_save_outbox_total{result="redis_error"} 的增量。
				r.results.WithLabelValues("redis_error").Inc()
				return published, fmt.Errorf("save outbox 读取权威home_zone失败，原意图保留: %w", err)
			}
			if home != strconv.FormatUint(uint64(zone), 10) {
				r.results.WithLabelValues("invalid").Inc()
				logx.Errorf("[SaveOutbox] 跨zone索引迁移缺少匹配的权威home_zone，原意图保留")
				if err := r.consumeAttempt(ctx, playerID, in.version, attempts); err != nil {
					return published, err
				}
				continue
			}
			moved, err := migrateScript.Run(ctx, r.rc, []string{savebarrier.Key(playerID), index, pendingIndexKey(zone, bucket)}, id, in.version, in.topic).Int64()
			if err != nil {
				r.results.WithLabelValues("redis_error").Inc()
				return published, errors.New("save outbox 迁移旧zone索引失败")
			}
			label := "conflict"
			if moved == 1 {
				label = "migrated"
			}
			r.results.WithLabelValues(label).Inc()
			continue
		}
		for _, payload := range []string{in.first, in.second} {
			if err := r.pub.Publish(ctx, in.topic, id, []byte(payload)); err != nil {
				// 不计次(r2-db#1):broker 故障与意图本身无关,恢复后必须还能投。认领时已按本次退避推迟了该意图。
				r.results.WithLabelValues("publish_error").Inc()
				r.publishPausedUntil = r.now().Add(r.cfg.RetryAfter)
				return published, errors.New("save outbox Kafka确认失败，原任务保留，补投暂停一个退避周期")
			}
			published++
			r.results.WithLabelValues("published").Inc()
		}
		if err := r.consumeAttempt(ctx, playerID, in.version, attempts); err != nil {
			return published, err
		}
	}
	return published, nil
}

// routeGateClosed 报告本实例的读方就绪闸此刻是否关着，关着时计一次 route_not_ready。
// 闸由消费者每个刷新间隔(RouteReadyGateIntervalMs，缺省 1s)刷新一次，单次刷新最长约一个 Redis 读超时：mapping Redis 刚出事、
// 闸还没刷新的那几拍仍可能认领并补投。认领会把意图推迟至少 RetryAfter(缺省 10s)，所以每次出事每个意图多计的次数有界
// (缺省配置下至多一次)，与停摆时长无关，不会因停摆累计到 exhausted。
func (r *Replay) routeGateClosed() bool {
	if r.route.RouteGateOpen() {
		return false
	}
	r.results.WithLabelValues("route_not_ready").Inc()
	return true
}

// consumeAttempt 在两条任务都经 broker 确认(或意图本身不合法)后计一次。版本或计数已变(并发认领、新版本存盘)时落空，不算错误。
func (r *Replay) consumeAttempt(ctx context.Context, playerID uint64, version string, attempts int64) error {
	if err := confirmAttemptScript.Run(ctx, r.rc, []string{savebarrier.Key(playerID)}, version, strconv.FormatInt(attempts, 10)).Err(); err != nil {
		r.results.WithLabelValues("redis_error").Inc()
		return errors.New("save outbox 补投计次失败；本次至多多投一次")
	}
	return nil
}

// decodeIntent 解析认领脚本结局 1 的 {1, 版本, topic, 两条任务, 认领时的已计次数}；次数只用于 consumeAttempt 的比较，不参与投递。
func decodeIntent(reply []interface{}) (intent, int64, error) {
	if len(reply) != 6 {
		return intent{}, 0, errors.New("存盘意图返回字段不完整")
	}
	fields := make([]string, 4)
	for i := range fields {
		var ok bool
		fields[i], ok = reply[i+1].(string)
		if !ok {
			return intent{}, 0, errors.New("存盘意图返回字段类型不合法")
		}
	}
	attempts, ok := reply[5].(int64)
	if !ok || attempts < 0 {
		return intent{}, 0, errors.New("存盘意图返回次数不合法")
	}
	return intent{version: fields[0], topic: fields[1], first: fields[2], second: fields[3]}, attempts, nil
}

// DrainHistorical在普通Run已join后由唯一owner调用。它完整读64桶及未索引barrier，
// 原topic/两条原body按审定全集投递；只有既有双ACK脚本可回收已完成意图，不清理dead或未完成pending。
// 返回值是本次实际发现仍待双ACK的意图数；0也必须再经portable verifier的独立全量闭包。
func (r *Replay) DrainHistorical(ctx context.Context, topics []string) (int, error) {
	if len(topics) == 0 || len(topics) > 128 {
		return 0, errors.New("历史drain缺审定topic全集")
	}
	allowed := map[string]bool{}
	for _, topic := range topics {
		zone, err := topicZone(topic)
		if err != nil || zone != r.cfg.ZoneID || allowed[topic] {
			return 0, errors.New("历史drain topic不属于本zone或重复")
		}
		allowed[topic] = true
	}
	budget, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	indexed := map[uint64]float64{}
	for b := uint64(0); b < BucketCount; b++ {
		rows, err := r.rc.ZRangeWithScores(budget, pendingIndexKey(r.cfg.ZoneID, b), 0, -1).Result()
		if err != nil {
			return 0, err
		}
		if len(indexed)+len(rows) > 16384 {
			return 0, errors.New("历史drain索引超过16K预算")
		}
		for _, row := range rows {
			id, ok := row.Member.(string)
			player, err := strconv.ParseUint(id, 10, 64)
			if !ok || err != nil || player == 0 || kafkautil.DBTaskKey(player) != id || player%BucketCount != b || math.IsNaN(row.Score) || math.IsInf(row.Score, 0) {
				return 0, errors.New("历史drain索引损坏")
			}
			if _, exists := indexed[player]; exists {
				return 0, errors.New("历史drain重复玩家索引")
			}
			indexed[player] = row.Score
		}
	}
	barriers := map[uint64]map[string]string{}
	cursor := uint64(0)
	for {
		keys, next, err := r.rc.Scan(budget, cursor, "player:{*}:save_barrier", 256).Result()
		if err != nil {
			return 0, err
		}
		for _, key := range keys {
			raw := strings.TrimSuffix(strings.TrimPrefix(key, "player:{"), "}:save_barrier")
			id, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || id == 0 || savebarrier.Key(id) != key {
				return 0, errors.New("历史drain遇到未知barrier键")
			}
			if _, seen := barriers[id]; seen {
				continue
			}
			if len(barriers) >= 16384 {
				return 0, errors.New("历史drain屏障超过16K预算")
			}
			fields, err := r.rc.HGetAll(budget, key).Result()
			if err != nil {
				return 0, err
			}
			barriers[id] = fields
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	for id := range indexed {
		if _, exists := barriers[id]; !exists {
			return 0, errors.New("历史drain索引没有原屏障")
		}
	}
	remaining := 0
	for id, fields := range barriers {
		if err := budget.Err(); err != nil {
			return remaining, err
		}
		in := intent{version: fields[fieldVersion], topic: fields[fieldTopic], first: fields[fieldFirst], second: fields[fieldSecond]}
		hasOutbox := false
		for name := range fields {
			if strings.HasPrefix(name, "outbox_") {
				hasOutbox = true
			}
		}
		pending := fields["pending"]
		if !hasOutbox {
			if pending != "" || fields["player_database"] != "" || fields["player_database_1"] != "" {
				epoch, seq, err := savebarrier.Parse(pending)
				if err != nil || epoch == 0 || seq == 0 || fields["player_database"] != pending || fields["player_database_1"] != pending {
					return remaining, errors.New("历史drain未索引屏障未完成")
				}
			}
			if _, exists := indexed[id]; exists {
				return remaining, errors.New("历史drain索引缺意图，保持原件")
			}
			continue
		}
		zone, err := validateIntent(id, in)
		if err != nil {
			return remaining, err
		}
		if zone != r.cfg.ZoneID {
			continue
		} // 同实例其它zone由其具名relay负责；最终verifier仍检查全体。
		if !allowed[in.topic] || pending != in.version {
			return remaining, errors.New("历史drain原topic未审定或版本漂移")
		}
		score, indexedHere := indexed[id]
		if !indexedHere {
			restored, err := restoreHistoricalIndexScript.Run(budget, r.rc, []string{savebarrier.Key(id), pendingIndexKey(r.cfg.ZoneID, id%BucketCount)}, kafkautil.DBTaskKey(id), in.version, in.topic, in.first, in.second).Int64()
			if err != nil || restored != 1 {
				return remaining, errors.New("历史drain未索引outbox的原子索引修复未确认")
			}
			score = 0
			indexed[id] = score
		}
		if fields["player_database"] == pending && fields["player_database_1"] == pending {
			now := r.now().UnixMilli()
			if score >= float64(now) {
				if score > float64(1<<52) {
					return remaining, errors.New("历史drain索引时间超界")
				}
				now = int64(score) + 1
			}
			reply, err := claimScript.Run(budget, r.rc, []string{savebarrier.Key(id), pendingIndexKey(r.cfg.ZoneID, id%BucketCount)}, kafkautil.DBTaskKey(id), now, r.cfg.FirstReplayAfter.Milliseconds(), r.cfg.RetryAfter.Milliseconds(), r.cfg.MaxRetryAfter.Milliseconds(), r.cfg.MaxAttempts, 0, r.cfg.InflightWindow.Milliseconds(), r.cfg.RecheckAfter.Milliseconds()).Slice()
			if err != nil || len(reply) == 0 || reply[0] != int64(2) {
				return remaining, errors.New("历史drain双ACK回收未确认")
			}
			continue
		}
		remaining++
		if fields[fieldTerminal] != "" {
			return remaining, errors.New("历史drain存在terminal意图，保持原字节")
		}
		for _, payload := range []string{in.first, in.second} {
			if err := r.pub.Publish(budget, in.topic, kafkautil.DBTaskKey(id), []byte(payload)); err != nil {
				return remaining, err
			}
		}
	}
	return remaining, nil
}

// 未索引意图是完整扫描发现的原事实；只在原version/topic/两body逐字节仍相同时补回NX索引。
// 不重写意图、版本、次数或terminal，不删除任何未完成pending。
var restoreHistoricalIndexScript = redis.NewScript(`
if redis.call('TYPE',KEYS[1]).ok ~= 'hash' then return redis.error_reply('historical barrier type') end
local index_type=redis.call('TYPE',KEYS[2]).ok
if index_type ~= 'none' and index_type ~= 'zset' then return redis.error_reply('historical index type') end
if redis.call('HGET',KEYS[1],'pending') ~= ARGV[2]
 or redis.call('HGET',KEYS[1],'outbox_version') ~= ARGV[2]
 or redis.call('HGET',KEYS[1],'outbox_topic') ~= ARGV[3]
 or redis.call('HGET',KEYS[1],'outbox_player_database') ~= ARGV[4]
 or redis.call('HGET',KEYS[1],'outbox_player_database_1') ~= ARGV[5] then return 0 end
redis.call('ZADD',KEYS[2],'NX',0,ARGV[1])
return 1
`)
