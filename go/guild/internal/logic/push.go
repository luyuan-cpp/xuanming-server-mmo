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
	"time"

	"github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/protobuf/proto"

	"guild/generated/pb/game"
	"guild/internal/data"
	pb "proto/guild"
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
	writer   *kafkago.Writer
	builder  kafkautil.GateCommandBuilder
	sessions *redis.Client // svcCtx.PlayerLocatorRedisClient,读 player:session:*
	// messageID 固定为 NotifyGuildChanged 的客户端消息号;存字段而不是每次现取,
	// 是为了让"推送用的是哪个号"在构造处一眼可见。
	messageID uint32
}

// NewGuildNotifier 是 nil 安全的唯一收口:任一依赖为 nil 即退回 NoopNotifier。
//
// 必须在这里判:kafkautil.PushToPlayer 不判 nil writer(go/shared/kafkautil/gate_push.go),
// 真拿 nil writer 去 WriteMessages 会 panic 在推送协程里 —— 那是配置缺失,
// 不该表现为运行期崩溃。打 Info 而不是 Error:本地开发不配 Kafka 是正常形态。
func NewGuildNotifier(w *kafkago.Writer, b kafkautil.GateCommandBuilder, sessions *redis.Client) GuildNotifier {
	if w == nil || b == nil || sessions == nil {
		logx.Info("[guild] Kafka writer / gate builder / session Redis 未配置:帮会推送关闭,客户端靠拉取刷新")
		return NoopNotifier{}
	}
	return &KafkaGuildNotifier{
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

	safego.Go(guildPushPoint, func() {
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
		logx.Errorf("[guild] notifier 未接线,kind=%s guild=%d 的推送被丢弃", changeKindLabel(kind), guildID)
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
//(loadGuild 按 player_id 升序装配),便于日志与用例逐条比对。
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
