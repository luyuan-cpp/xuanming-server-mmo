package logic

// 帮会变更推送 NotifyGuildChanged(设计 docs/design/guild-phase2/02-management.md §13–§15)。
//
// 四条贯穿全文件的纪律:
//
//  1. **推送只是"提示客户端去拉一次"**,不是数据真相。载荷只有 GuildChangedS2C 的四个字段,
//     客户端收到后自己拉 GetPlayerGuild / 申请列表。不下发快照的理由:推送与响应的先后
//     不可控,一旦载荷里带状态,晚到的推送就会把客户端刷回旧数据,而这种不一致没法自愈。
//  2. **只在 MySQL 提交成功之后调用**,且**至多一次**。离线不推、gate 找不到会话即丢弃、
//     Kafka 写失败不重试 —— 错过的人打开帮会界面 / 手动刷新时照样拿到最新状态。
//  3. **绝不影响 RPC 结果**。推送失败只记指标与日志;把已经提交成功的写报成失败,
//     会让客户端以为操作没生效并重复操作,代价远大于少一条提示。
//  4. **不占 RPC 预算**。整批在 safego 协程里跑,自带独立超时,**不继承请求 ctx**
//     (请求返回即 cancel,继承等于必然写不出去)。

import (
	"context"
	"fmt"
	"shared/drainproof"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/protobuf/proto"

	"guild/generated/pb/game"
	"guild/internal/constants"
	"guild/internal/data"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/kafkautil"
	"shared/safego"
)

// guildPushBudget 是**一整批**推送的预算(读会话 MGET + 按网关分组写 Kafka)。
//
// 取 3s 而不是更大:与 svc/servicecontext.go 里 writer 的 WriteTimeout 同值,
// 也与 match/team 的推送口径一致(team-system.md G.3)。Kafka 写不出去时 3s 已足够判定
// 它不可用,再等下去只是让协程堆积;而正常路径上一次 MGET(单帮成员 ≤100,
// 见 constants.MaxGuildMembersCap)加少量分组消息远低于这个数。
const guildPushBudget = 3 * time.Second

// guildPushPoint 是 safego 的点位名。它会直接变成 safego_panic_total 的 label,
// 所以必须是**常量**,不能拼进任何 id。
const guildPushPoint = "guild.push"

// guildPushTotal 按**收件人**计数,不是按批次 —— 定位"谁没收到"时关心的是人数。
//
// label 只有 kind / outcome 两个有界枚举。**绝不放 player_id / guild_id**:
// 那是无上界的高基数维度,会把 Prometheus 的时间序列打爆(AGENTS.md §9)。
var guildPushTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild",
	Name:      "push_total",
	Help:      "NotifyGuildChanged 推送结果,按收件人计数。outcome: ok / offline / error / session_error。",
	Labels:    []string{"kind", "outcome"},
})

// GuildNotifier 是帮会变更推送的唯一接缝。
//
// 契约(实现必须全部满足):
//   - 只在 MySQL 提交成功之后调用;
//   - 至多一次、异步,Notify 立即返回,不阻塞 RPC;
//   - 失败 / 离线只记日志与指标,绝不影响 RPC 结果;
//   - 收件人去重、丢弃 0;
//   - 载荷只有 GuildChangedS2C 四个字段,客户端据此拉取,不下发快照。
type GuildNotifier interface {
	Notify(change *pb.GuildChangedS2C, recipients []uint64)
}

// NoopNotifier:未配 Kafka(Brokers 为空 → svcCtx.KafkaWriter 为 nil)或单测时使用。
// 客户端此时靠拉取刷新,功能降级但不出错。
type NoopNotifier struct{}

func (NoopNotifier) Notify(*pb.GuildChangedS2C, []uint64) {}

// KafkaGuildNotifier 走 Kafka gate-cmd → gate → TCP 这条既有下行链路(设计决策 D6),
// 与 match / friend 的推送共用 shared/kafkautil 的收口。
type KafkaGuildNotifier struct {
	admission *drainproof.Session
	writer    *kafkago.Writer
	builder   kafkautil.GateCommandBuilder
	sessions  *redis.Client // svcCtx.PlayerLocatorRedisClient,读 player:session:*
	// messageID 固定为 NotifyGuildChanged 的客户端消息号;存字段而不是每次现取,
	// 是为了让"推送用的是哪个号"在构造处一眼可见。
	messageID uint32
}

// NewGuildNotifier 是 nil 安全的唯一收口:任一依赖为 nil 即退回 NoopNotifier。
//
// 必须在这里判:kafkautil.PushToPlayer 不判 nil writer(go/shared/kafkautil/gate_push.go),
// 真拿 nil writer 去 WriteMessages 会 panic 在推送协程里 —— 那是配置缺失,
// 不该表现为运行期崩溃。打 Info 而不是 Error:本地开发不配 Kafka 是正常形态。
func NewGuildNotifier(w *kafkago.Writer, b kafkautil.GateCommandBuilder, sessions *redis.Client, admissions ...*drainproof.Session) GuildNotifier {
	var gate *drainproof.Session
	if len(admissions) > 1 {
		panic("公会通知角色重复")
	}
	if len(admissions) == 1 {
		gate = admissions[0]
		if gate == nil {
			panic("公会通知角色未装配")
		}
	}
	if w == nil || b == nil || sessions == nil {
		logx.Info("[guild] Kafka writer / gate builder / session Redis not configured: guild push disabled, client refreshes by polling")
		return NoopNotifier{}
	}
	return &KafkaGuildNotifier{
		admission: gate,
		writer:    w,
		builder:   b,
		sessions:  sessions,
		messageID: game.GuildServiceNotifyGuildChangedMessageId,
	}
}

func (n *KafkaGuildNotifier) Notify(change *pb.GuildChangedS2C, recipients []uint64) {
	ids := uniqueNonZero(recipients)
	if len(ids) == 0 {
		return
	}
	kind := changeKindLabel(change.GetKind())
	// 序列化与取字段都放在起协程**之前**:change 由调用方现场构造,
	// 放进闭包里再读等于把一个可能被复用的指针交给另一个 goroutine,-race 下必报。
	guildID := change.GetGuildId()
	body, err := proto.Marshal(change)
	if err != nil {
		guildPushTotal.Inc(kind, "error")
		logx.Errorf("[guild] marshal GuildChangedS2C kind=%s guild=%d: %v", kind, guildID, err)
		return
	}

	finish := func() {}
	if n.admission != nil {
		accepted, err := n.admission.Enter()
		if err != nil {
			n.admission.Hold()
			return
		}
		finish = accepted
	}
	safego.Go(guildPushPoint, func() {
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				if n.admission != nil {
					n.admission.Hold()
				}
				panic(recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), guildPushBudget)
		defer cancel()

		online, offline, err := loadGateInfos(ctx, n.sessions, ids)
		if err != nil {
			// 会话读不到就没法路由,整批不推。分开记 session_error 而不是并进 error:
			// 前者是会话 Redis 的问题,后者是 Kafka 的问题,排障时要能一眼分开。
			guildPushTotal.Add(float64(len(ids)), kind, "session_error")
			logx.Errorf("[guild] push kind=%s guild=%d: read sessions: %v", kind, guildID, err)
			return
		}
		if offline > 0 {
			guildPushTotal.Add(float64(offline), kind, "offline")
		}
		if len(online) == 0 {
			return
		}

		if len(online) == 1 {
			err = kafkautil.PushToPlayer(ctx, n.writer, n.builder, online[0], n.messageID, body)
		} else {
			err = kafkautil.BroadcastToPlayers(ctx, n.writer, n.builder, online, n.messageID, body)
		}
		if err != nil {
			if n.admission != nil {
				n.admission.Hold()
			}
			// BroadcastToPlayers 对个别坏条目会跳过其余照发,只返回首个错误,
			// 所以整批记 error 是**偏保守**的口径(实际可能大部分已送达);
			// 宁可指标偏悲观,明细在上面 kafkautil 的 Error 日志里。
			guildPushTotal.Add(float64(len(online)), kind, "error")
			logx.Errorf("[guild] push kind=%s guild=%d recipients=%d: %v", kind, guildID, len(online), err)
			return
		}
		guildPushTotal.Add(float64(len(online)), kind, "ok")
	})
}

// loadGateInfos:MGET player:session:*,返回在线者的网关路由信息与离线人数。
//
// "推不出去"一律按离线计(返回值的第二项),不单独分类:对调用方来说
// 离线、会话解不开、缺实例 id 的处置完全相同 —— 都是这一批不发给他。
// ids 必须已经去重且不含 0(由 uniqueNonZero 保证)。
func loadGateInfos(ctx context.Context, rdb *redis.Client, ids []uint64) ([]kafkautil.PlayerGateInfo, int, error) {
	if rdb == nil || len(ids) == 0 {
		return nil, 0, nil
	}

	keys := make([]string, 0, len(ids))
	for _, playerID := range ids {
		keys = append(keys, playerSessionKey(playerID))
	}
	values, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, 0, err
	}
	// MGET 承诺返回值与 keys 等长且一一对应。真出现长度不一致就是客户端库语义变了,
	// 此时按下标取 ids 会张冠李戴地把推送发给别人 —— 必须 fail-closed,不能猜。
	if len(values) != len(ids) {
		return nil, 0, fmt.Errorf("MGET returned %d values for %d keys", len(values), len(ids))
	}

	infos := make([]kafkautil.PlayerGateInfo, 0, len(ids))
	offline := 0
	for i, value := range values {
		session, online := onlineSessionFrom(value)
		if !online {
			offline++
			continue
		}
		if session.GetGateInstanceId() == "" {
			// 不变量 2(CLAUDE.md §7):没有实例 uuid,gate 侧按实例过滤的防僵尸校验就失效,
			// 所以 kafkautil.PushToPlayer / BroadcastToPlayers 会 fail-closed 拒绝这条。
			// 提前剔除是为了不让一个坏条目把整批记成 error;但它是真异常(会话写入侧漏填),
			// 必须留 Error 日志,不能静默吞掉。
			logx.Errorf("[guild] push skipped player %d: empty gate_instance_id (gate=%s)",
				ids[i], session.GetGateId())
			offline++
			continue
		}
		infos = append(infos, kafkautil.PlayerGateInfo{
			PlayerID:       ids[i],
			SessionID:      session.GetSessionId(),
			GateID:         session.GetGateId(),
			GateInstanceID: session.GetGateInstanceId(),
		})
	}
	return infos, offline, nil
}

// uniqueNonZero 去重并丢掉 0,保持首次出现的顺序。
//
// 丢 0 是必须的:收件人由"快照成员 - 操作者"之类的集合运算得来,
// 0 表示"没有这个人"(比如解散时 target=0),拿去拼 player:session:0 只会白读一次 Redis。
func uniqueNonZero(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// changeKindLabel 把枚举映射成**固定的**小写串。
//
// 不用 kind.String():那串由 proto 生成,枚举一旦改名,Prometheus 里的时间序列
// 就会断成两条,历史曲线对不上。这里一次写全 13 个值(含 B5/B6 的四个,见
// 90-consistency.md Y-03),后续批次只调用不再改本函数;未知值归 "other",
// 保证 label 取值集合有界且可枚举。
func changeKindLabel(kind pb.GuildChangeKind) string {
	switch kind {
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_JOINED:
		return "member_joined"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_LEFT:
		return "member_left"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_KICKED:
		return "member_kicked"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_ROLE_CHANGED:
		return "role_changed"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_LEADER_TRANSFERRED:
		return "leader_transferred"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_DISBANDED:
		return "disbanded"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_APPLICATION_RECEIVED:
		return "application_received"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_APPLICATION_REJECTED:
		return "application_rejected"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED:
		return "funds_changed"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_LEVEL_UP:
		return "level_up"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_ANNOUNCEMENT_CHANGED:
		return "announcement_changed"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_ACTIVITY_CHANGED:
		return "activity_changed"
	case pb.GuildChangeKind_GUILD_CHANGE_KIND_DELIVERY_DONE:
		return "delivery_done"
	default:
		return "other"
	}
}

// ── logic 侧助手(§13.5) ──────────────────────────────────────

// notify 组装 S2C 并交给 notifier。
//
// guildID 显式传参而不是从快照里取:解散之后已经没有快照可取了,而收件人恰恰需要知道
// 是哪个帮散了。actor / target 的取值见 §14 推送矩阵,**操作者一律不在收件人里**
// (他的响应已经带着最新状态,再推一条只会让客户端多拉一次)。
func (l *GuildLogic) notify(kind pb.GuildChangeKind, guildID, actor, target uint64, recipients []uint64) {
	if l.notifier == nil {
		// 正常路径不会发生(NewGuildLogic 的默认值是 NoopNotifier{})。
		// 这里宁可记一条 Error 也不让它 panic:notify 跑在提交**之后**,
		// 在这里 panic 等于把一次已经写成功的操作报成 RPC 失败。
		logx.Errorf("[guild] Notifier not wired; push dropped kind=%s guild=%d", changeKindLabel(kind), guildID)
		return
	}
	l.notifier.Notify(&pb.GuildChangedS2C{
		Kind:           kind,
		GuildId:        guildID,
		ActorPlayerId:  actor,
		TargetPlayerId: target,
	}, recipients)
}

// membersExcept 返回快照成员里去掉 excluded 之后的 player_id,顺序沿用快照
// (loadGuild 按 player_id 升序装配),便于日志与用例逐条比对。
//
// 只做集合减法、不做去重与丢 0:那两件事在 Notify 的 uniqueNonZero 里统一做,
// 免得同一条规则散落两处、日后改一处忘一处。
func membersExcept(g *data.GuildData, excluded ...uint64) []uint64 {
	if g == nil || len(g.Members) == 0 {
		return nil
	}
	skip := make(map[uint64]struct{}, len(excluded))
	for _, id := range excluded {
		skip[id] = struct{}{}
	}
	out := make([]uint64, 0, len(g.Members))
	for _, m := range g.Members {
		if _, ok := skip[m.PlayerID]; ok {
			continue
		}
		out = append(out, m.PlayerID)
	}
	return out
}

// ── 落地恢复摘要 bump(docs/design/landing-recovery-sync-digest.md §3.3 / §3.5) ──
//
// 推送是"在线时的提示",摘要是"落地后的提示":传送、断线、下线期间推送会丢且不补发,客户端落地后取摘要
// (核心 Redis 的 player:{id}:sync_digest),与本地缓存逐域比对、只拉变了的域。bump 与推送同形,
// 四条纪律:
//
//  1. **只在 MySQL 提交成功、repo 已失效缓存之后调用**(§3.5 第 1、2 条)。先 bump 后提交,客户端会把
//     旧数据记成新版本,下一次变化之前都看不到这次改动。repo 的写方法在返回前已做完 invalidateAfterCommit。
//  2. **收件人 = "拉取结果会变的所有玩家",含操作者本人**(§3.5 第 3 条)。这是与推送唯一的口径差:
//     推送刻意排除操作者(他手上有回包),而回包恰恰可能在断线时丢失。
//  3. **绝不影响业务结果**。失败只记日志与 playersync 的低基数指标 player_sync_bump_total{service,result};
//     漏掉的一次由打开面板时的拉取兜底(§3.5 第 5 条)。
//  4. **不占 RPC 预算**。整批在 safego 协程里跑,总预算 guildSyncBumpBudget,不继承请求 ctx
//     (全帮扇出最多 constants.MaxGuildMembersCap 人,§3.5 第 6 条)。
//
// 帮会级数值(资金、灯会进度)不**逐次**扇出:每笔捐献乘全帮人数的写放大不值得,由打开面板时的拉取覆盖(§3.3)。
// 这条豁免只针对"为了它专门扇出给他人":
//   - 本人那一次 bump 多带一个域号是零成本(同一条 Lua 多一个 ARGV),本人 GuildInfo 里的帮贡变了就带上 GUILD;
//   - 已经因为别的原因在扇出(升级、首次达阈值、计了资金的历练胜局)时,顺带的域号同样零成本,照带。

// guildSyncBumpBudget 是**一整批** bump 的总预算(设计 §3.5 第 6 条:2 秒)。
//
// 一次 bump 是核心 Redis 上的一条单键 Lua,全帮 100 人串行正常远低于这个数;Redis 不可用时 2s 已足够判定,
// 再等只是让协程堆积。比推送的 guildPushBudget 小:摘要丢一次的代价只是落地后多拉一次。
//
// 这个预算能封住**单条**命令,前提是底下的 go-redis 客户端开了 ContextTimeoutEnabled(svc.newPlayerSyncRedis):
// 不开时 go-redis v9 的套接字读写换成 context.Background(),一条卡住的命令要等满 ReadTimeout(默认 3s),
// ctx 只在逐人检查、取连接排队、拨号与重试退避处生效。
const guildSyncBumpBudget = 2 * time.Second

// guildSyncBumpPoint 是 safego 的点位名,会直接变成 safego_panic_total 的 label,必须是常量。
const guildSyncBumpPoint = "guild.sync_bump"

// PlayerSyncBumper 是"给一批玩家的同一组域各 +1"的能力,*playersync.Bumper 满足它(go/shared/playersync)。
//
// 契约由 playersync.Bumper.BumpAll 保证:nil 接收者安全;玩家去重、跳过 0;每个玩家发命令前先看 ctx,
// 预算到期后剩余玩家不再发;失败已记 WARN 日志并计入 player_sync_bump_total{service,result}(含预算耗尽时跳过的人数),
// 返回的 error 只是汇总。用窄接口而不是直接依赖 *playersync.Bumper:logic 只用这一个方法,单测的替身也只需实现这一个。
type PlayerSyncBumper interface {
	BumpAll(ctx context.Context, playerIDs []uint64, domains ...scenepb.SyncDomain) error
}

// GuildSyncBumper 是帮会侧摘要 bump 的唯一接缝(与 GuildNotifier 同形)。
//
// 契约(实现必须全部满足):
//   - 只在 MySQL 提交成功(repo 已失效缓存)之后调用;
//   - 异步、立即返回,不阻塞 RPC;整批共用一个预算,不继承请求 ctx;
//   - 失败只记日志与指标,绝不影响业务结果;
//   - 玩家去重、丢弃 0;domains 为空时什么都不做;
//   - 返回之后不再读、也从不改调用方传入的切片(调用方可以在返回后复用它们)。
type GuildSyncBumper interface {
	Bump(players []uint64, domains ...scenepb.SyncDomain)
}

// NoopSyncBumper:没接摘要 Redis(guild.go 未传 WithSyncBumper)或单测时使用。
// 退化形态 = 本功能上线前的现状:帮会各域在摘要里永不变化,客户端落地后不会因摘要去拉帮会,
// 只在打开帮会界面时拉取。不出错,只是传送窗口里丢的推送要等打开界面才对齐。
type NoopSyncBumper struct{}

func (NoopSyncBumper) Bump([]uint64, ...scenepb.SyncDomain) {}

// AsyncGuildSyncBumper 把一批 bump 放进 safego 协程,交给 PlayerSyncBumper.BumpAll 逐人执行(纪律 4)。
//
// 逐人发命令、预算耗尽即停、失败记 WARN 与指标,这些都由 playersync 统一做(friend / team 共用同一份口径),
// 本类型只负责"异步 + 不继承请求 ctx + 整批一个预算 + 不别名调用方切片"。
//
// 只经 NewGuildSyncBumper 构造(四个字段都必须非零);单测在同一包内按字段注入替身。
type AsyncGuildSyncBumper struct {
	admission *drainproof.Session
	inner     PlayerSyncBumper
	budget    time.Duration
	// withBudget 按 budget 给每一批建一个独立的预算上下文(不继承请求 ctx)。生产是 backgroundWithTimeout;
	// 抽成字段是为了单测注入截止时间固定的上下文,直接比对截止时间,不与真实墙钟比较(AGENTS §11.4)。
	withBudget func(time.Duration) (context.Context, context.CancelFunc)
	// launch 把一批交给另一个协程执行。生产是 launchSyncBump(safego.Go);
	// 单测注入记录器,同步地观察"起没起协程",并在调用方复用切片之后才执行交出去的那一批。
	launch func(func())
}

// backgroundWithTimeout 是 AsyncGuildSyncBumper.withBudget 的生产实现:挂在 Background 上,刻意不继承请求 ctx。
func backgroundWithTimeout(budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), budget)
}

// launchSyncBump 是 AsyncGuildSyncBumper.launch 的生产实现:panic 由 safego 兜住并计 safego_panic_total。
func launchSyncBump(fn func()) { safego.Go(guildSyncBumpPoint, fn) }

// NewGuildSyncBumper 是 nil 安全的唯一收口:inner 为 nil 接口即退回 NoopSyncBumper。
//
// nil 接口的坑:装着 nil *playersync.Bumper 的接口不等于 nil,走不到这里的退回分支;
// 那种形态(NewGoRedisEvaler / NewBumper 拿到 nil 句柄时返回 nil)由 playersync.Bumper 自己的
// nil 接收者兜住(BumpAll 直接返回 nil),不会 panic,只是多起一个空跑的协程。
func NewGuildSyncBumper(inner PlayerSyncBumper, admissions ...*drainproof.Session) GuildSyncBumper {
	var gate *drainproof.Session
	if len(admissions) > 1 {
		panic("公会摘要角色重复")
	}
	if len(admissions) == 1 {
		gate = admissions[0]
		if gate == nil {
			panic("公会摘要角色未装配")
		}
	}
	if inner == nil {
		logx.Info("[guild] sync digest bumper not configured: guild domains never change in the landing digest, client refreshes when opening the guild panel")
		return NoopSyncBumper{}
	}
	return &AsyncGuildSyncBumper{
		admission:  gate,
		inner:      inner,
		budget:     guildSyncBumpBudget,
		withBudget: backgroundWithTimeout,
		launch:     launchSyncBump,
	}
}

func (b *AsyncGuildSyncBumper) Bump(players []uint64, domains ...scenepb.SyncDomain) {
	ids := uniqueNonZero(players)
	if len(ids) == 0 || len(domains) == 0 {
		return
	}
	// 去重与复制都在起协程**之前**:players 常是事务快照的成员切片、domains 是可变参数的底层数组,
	// 交给另一个 goroutine 之后调用方若复用它们,-race 下必报。uniqueNonZero 已返回新切片。
	ds := slices.Clone(domains)
	finish := func() {}
	if b.admission != nil {
		accepted, err := b.admission.Enter()
		if err != nil {
			b.admission.Hold()
			return
		}
		finish = accepted
	}
	b.launch(func() {
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				if b.admission != nil {
					b.admission.Hold()
				}
				panic(recovered)
			}
		}()
		ctx, cancel := b.withBudget(b.budget)
		defer cancel()
		// 返回值只是汇总:每一个失败(含预算耗尽时没来得及发的人数)playersync 都已记 WARN 与指标,
		// 这里再记一行只会让同一次故障出现两遍。漏掉的由打开面板时的拉取兜底(纪律 3)。
		if err := b.inner.BumpAll(ctx, ids, ds...); err != nil && b.admission != nil {
			b.admission.Hold()
		}
	})
}

// bumpSync 是 logic 侧唯一的 bump 入口,与 notify 写在同一处(提交之后)。
//
// 与 notify 同一套防御:正常路径不会遇到 nil(NewGuildLogic 的默认值是 NoopSyncBumper{}),
// 真遇到了只记 Error 不 panic —— 它跑在提交之后,panic 等于把一次已经写成功的操作报成 RPC 失败。
func (l *GuildLogic) bumpSync(players []uint64, domains ...scenepb.SyncDomain) {
	if l.syncBumper == nil {
		logx.Errorf("[guild] sync bumper not wired; digest bump dropped domains=%v players=%d", domains, len(players))
		return
	}
	l.syncBumper.Bump(players, domains...)
}

// membersPlus 返回快照全体成员再追加 extra(已不在快照里的当事人:离帮者、被踢者)。
// 不去重、不丢 0:那两件事在 GuildSyncBumper.Bump 里统一做(同 membersExcept 的约定)。
func membersPlus(g *data.GuildData, extra ...uint64) []uint64 {
	return append(membersExcept(g), extra...)
}

// reviewersIn 返回快照里有审批权的人(长老与帮主)。待审数只显示在他们的 GuildInfo 里(guildInfoFor),
// 所以申请行的增删要给这组人 bump GUILD。职位判定与 guildInfoFor 同一个阈值(RankOfficer)。
// 快照通常来自事务内的权威读;撤回申请没有事务快照,用的是提交之后的缓存读(GetGuild,职位变更会失效缓存)。
func reviewersIn(g *data.GuildData) []uint64 {
	if g == nil {
		return nil
	}
	out := make([]uint64, 0, len(g.Members))
	for _, m := range g.Members {
		if constants.Rank(m.Role) >= constants.RankOfficer {
			out = append(out, m.PlayerID)
		}
	}
	return out
}

// assetFinalizedSyncDomains:一条资产指令终结之后,本人哪些域的拉取结果会变。
//
//   - DONATE → 捐献页(待结算 / 最近结果 / 今日次数);SHOP → 商店页(待发放 / 最近结果 / 帮贡 / 限购)。
//     两页同属 GUILD_ECONOMY(§3.3 表第 8 行)。
//     另带 GUILD:终结的对侧账会改本人 GetPlayerGuild 的结果 —— 捐献 APPLIED 给他记帮贡(GuildInfo.members[] 的
//     contribution_*)并入帮会资金,兑换 REJECTED / ABORTED 退回他的帮贡。不按终态细分:细分等于把
//     data.applyCounterparty 的对侧账表在 logic 里再抄一份,那张表一改这里就静默漏 bump;
//     本人多拉一次 GetPlayerGuild 无害,漏拉才违反 L-1。
//   - ACTIVITY_REWARD → 活动页的待发奖励(RewardStatuses → my_pending_reward_count),属 GUILD_ACTIVITY。
//     两个经济页都按 kind 过滤掉它,所以不 bump GUILD_ECONOMY(§3.5 第 3 条:只给拉取结果会变的域);
//     它的帮贡与资金在入队事务里已记完(afterActivityCommit 已 bump 过 GUILD),终结只 CAS,所以也不带 GUILD。
//   - 未知 kind → 三个域都 bump:多拉一次无害,漏拉才违反 L-1。
func assetFinalizedSyncDomains(kind pb.GuildAssetOpKind) []scenepb.SyncDomain {
	switch kind {
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP:
		return []scenepb.SyncDomain{
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD,
		}
	case pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD:
		return []scenepb.SyncDomain{scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ACTIVITY}
	default:
		return []scenepb.SyncDomain{
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ECONOMY,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD_ACTIVITY,
			scenepb.SyncDomain_SYNC_DOMAIN_GUILD,
		}
	}
}
