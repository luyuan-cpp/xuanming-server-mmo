package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 队伍补位匹配的票据域实现(设计文档 docs/design/team-backfill-matching.md)。
//
// 一句话:队列里仍然是一个人一个十进制 player_id 元素,只是同一支补位单的成员票据上带同一个 party_id 与名单;
// matcher 按 party_id 把他们当成一个不可拆的单位(party_match.go)。票据是「是否在排」的唯一事实来源,
// 队伍记录里不存匹配态。
//
// 本文件放与 matcher 选人无关的部分:名单编解码、整批入队 / 回队的 Lua、撤单、team 端口的三个方法、
// 以及 JoinQueue / 整队开战预检遇到补位票时的自愈。
//
// 不变量(改这里的任何一步都要对照):
//   - 一支补位单进出 list 永远是一条 Lua(整批 RPUSH / 整批 LPUSH / 整批摘出),所以 list 里不存在
//     「只有一部分成员」的合法瞬间;matcher 看到不完整的单就可以判它失效;
//   - 成员出现在 list 里时他的票据必然已是 queued(入队:先建票后 RPUSH;回队:先全员 CAS 后 LPUSH);
//   - 撤单:先逐人删票(queued 才删)再摘 list —— 与 CancelQueue 同序,并发弹组方靠票据缺失识别;
//   - 一支补位单进 gather 的充要条件是全体成员的票都被 CAS 成了 matched(matchQueueOnce + requeueFront)。

const (
	// partyCheckTimeout 一次 team 复核(CheckParty)的超时。自愈路径单次用它;matcher 一轮弹组里的全部
	// 复核另受 partyCheckRoundBudget 约束(凑单锁默认 10s 不续期)。
	partyCheckTimeout = 2 * time.Second
	// battleLockReadTimeout 回队首前读一次战斗锁的上限(dropMembersInOtherBattle)。这一步夹在「票据已回 queued」与
	// 「压回队列」之间,不能让 SharedRedis 抖动时的重试把这个窗口拉长;超时按读锁失败处理(照常回队)。
	battleLockReadTimeout = 500 * time.Millisecond
	// partyCheckRoundBudget matcher 一次 popGroup 里花在 team 复核上的总预算。
	partyCheckRoundBudget = 3 * time.Second
	// partyEnqueueGrace 补位票「queued 但不在 list」被当成崩溃残留之前的宽限。EnqueueParty 是先逐人建票、
	// 最后一条 Lua 入队,两步之间成员自己的 JoinQueue / 队长重复点开战会读到「有票、不在队列」;
	// 没有宽限的话那次读会把正在入队的整单当孤儿撤掉。回队首路径同样有这个窗口(毫秒级),
	// 但回队票据的入队时间早已超过宽限,那里被误判的后果只是整单提前退出匹配并收到提示。
	partyEnqueueGrace = 15 * time.Second
	// partyEndMarkerTTLSeconds 撤单原因 / 已通知两个标记的存活时间:只需盖住「同一单被两条路径先后收尾」的窗口
	// (取消落在 matcher 两次票据 CAS 之间时,要等那一局的 matcher 把票据推进完、回队首时才收尾,
	// 上限是一轮弹组的耗时),远小于票据 TTL;party_id 每次入队新发,不会复用。
	partyEndMarkerTTLSeconds = 120
)

// formatPartyMembers / parsePartyMembers 补位单名单在票据里的编码:逗号分隔的十进制 player_id,顺序即站位顺序。
func formatPartyMembers(ids []uint64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return strings.Join(parts, ",")
}

// parsePartyMembers 空串返回 nil。任何一段不是非零十进制 uint64、或有重复 → 返回 nil:名单损坏,
// 调用方拿不到完整名单就无法把这一单凑齐,matcher 会按「不完整」把它撤掉。
func parsePartyMembers(raw string) []uint64 {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]uint64, 0, len(parts))
	seen := make(map[uint64]bool, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseUint(part, 10, 64)
		if err != nil || id == 0 || seen[id] {
			return nil
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// pushMembersScript 把一批成员一次性挂进队列并保证队列在注册集里
// (KEYS=[index, queue, rank, versions, sequence],ARGV=[queueKey, front("1"=队首/"0"=队尾), member1, rating1, member2, rating2...])。
// 队首:倒序 LPUSH,members[0] 最终在最前,保持原相对顺序;队尾:顺序 RPUSH。
// 一条 Lua 完成是补位单「不可拆」在存储层的落点:同一单的成员要么都在 list 里,要么都不在。
// 所有 key 同 {mq} slot;与 enqueueScript / requeueScript 同样维护评分镜像和每次实际写入的新版本。
// INCR 后 GET 使用整数的原始字符串,不把超过 Lua 精确整数范围的序列转成浮点版本。
const pushMembersScript = `redis.call("INCR", KEYS[5])
local version = redis.call("GET", KEYS[5])
redis.call("SADD", KEYS[1], ARGV[1])
local n = (#ARGV - 2) / 2
if ARGV[2] == "1" then
	for i = n, 1, -1 do
		redis.call("HSET", KEYS[4], ARGV[1 + 2 * i], version)
		redis.call("ZADD", KEYS[3], ARGV[2 + 2 * i], ARGV[1 + 2 * i])
		redis.call("LREM", KEYS[2], 0, ARGV[1 + 2 * i])
		redis.call("LPUSH", KEYS[2], ARGV[1 + 2 * i])
	end
else
	for i = 1, n do
		redis.call("HSET", KEYS[4], ARGV[1 + 2 * i], version)
		redis.call("ZADD", KEYS[3], ARGV[2 + 2 * i], ARGV[1 + 2 * i])
		redis.call("LREM", KEYS[2], 0, ARGV[1 + 2 * i])
		redis.call("RPUSH", KEYS[2], ARGV[1 + 2 * i])
	end
end
return n`

// queuePush 整批入队 / 回队的一个成员:rating 写回评分镜像(非评分队列不看它,但镜像不变量照样维护)。
type queuePush struct {
	playerId uint64
	rating   float64
}

// pushMembersRegistered 整批入队(front=false)或整批回队首(front=true),见 pushMembersScript。
func pushMembersRegistered(ctx context.Context, svcCtx *svc.ServiceContext, queueKey string, members []queuePush, front bool) error {
	if len(members) == 0 {
		return nil
	}
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		return err
	}
	flag := "0"
	if front {
		flag = "1"
	}
	args := make([]any, 0, 2+2*len(members))
	args = append(args, queueKey, flag)
	for _, m := range members {
		args = append(args, strconv.FormatUint(m.playerId, 10), formatRating(m.rating))
	}
	_, err = svcCtx.MatchRedis.EvalCtx(ctx, pushMembersScript, []string{matchQueueIndexKey, queueKey, rankKey, queueVersionsKey(queueKey), matchQueueVersionSequenceKey}, args...)
	return err
}

// notifyPartyEnded 通知 team 一支补位单已被撤出队列。端口未注入(没有组队服务的进程 / 部分测试)时不通知;
// reason = PartyEndNone 是静默撤单(入队回滚:队员还没收到过「开始匹配」),同样不通知。
//
// 同一单可能被两条路径先后收尾(取消落在 matcher 两次票据 CAS 之间;撤单进行中被 matcher 扫到),规则:
//   - 原因以最先登记的为准(claimPartyEndReason):主动取消方在撤其余人之前就登记「某某取消」,
//     之后 matcher 侧收尾时沿用它,不会把一次主动取消说成「队员未就绪」;
//   - 全队只收到一条通知(markPartyEndNotified),并且由**把这一单收干净的那一方**发 ——
//     cancelParty 发现有人「太迟」(票已 matched)时不通知,免得推送里带着半截状态的视图。
func notifyPartyEnded(svcCtx *svc.ServiceContext, seed *queueTicket, reason uint32, culprit uint64) {
	if reason == constants.PartyEndNone || seed == nil || seed.TeamId == 0 || len(seed.PartyMembers) == 0 {
		return
	}
	port := svcCtx.PartyTeam()
	if port == nil {
		// 撤单已经做了,但没人能推「匹配已取消」:队员靠客户端的排队轮询发现。生产装配不该走到这里。
		logx.Errorf("[match] Party left backfill queue but the team port is not wired; members are not notified team=%d party=%s reason=%d",
			seed.TeamId, seed.PartyId, reason)
		return
	}
	reason, culprit = claimPartyEndReason(svcCtx, seed.PartyId, reason, culprit)
	if !markPartyEndNotified(svcCtx, seed.PartyId) {
		return
	}
	port.PartyEnded(seed.TeamId, append([]uint64(nil), seed.PartyMembers...), reason, culprit)
}

// notifyPartiesStarted gather 成功出口:members 里若有队伍补位单(票据带 party_id),每单通知 team 一次
// (svc.PartyTeamPort.PartyStarted)。此刻票据已是 ready;读票失败只记日志 —— 这条通知只影响队伍面板的显示,
// 客户端在收到开战推送时自己也会把这一次排队记为结束。端口未注入时不读票。
func notifyPartiesStarted(svcCtx *svc.ServiceContext, members []uint64) {
	port := svcCtx.PartyTeam()
	if port == nil {
		return
	}
	notified := make(map[string]bool)
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		if err != nil {
			logx.Errorf("[match] Failed to read ticket for party start notification player=%d: %v", pid, err)
			continue
		}
		if ticket == nil || ticket.PartyId == "" || ticket.TeamId == 0 || notified[ticket.PartyId] {
			continue
		}
		notified[ticket.PartyId] = true
		port.PartyStarted(ticket.TeamId, append([]uint64(nil), ticket.PartyMembers...))
	}
}

// claimPartyEndReason 登记这一单的撤单原因(SET NX):已经有人登记过就返回先登记的那个,否则返回自己的。
// 读写出错时返回自己的原因(只影响提示文案,不影响撤单本身)。
func claimPartyEndReason(svcCtx *svc.ServiceContext, partyId string, reason uint32, culprit uint64) (uint32, uint64) {
	if partyId == "" {
		return reason, culprit
	}
	key := matchPartyEndReasonKey(partyId)
	value := strconv.FormatUint(uint64(reason), 10) + ":" + strconv.FormatUint(culprit, 10)
	acquired, err := svcCtx.MatchRedis.SetnxEx(key, value, partyEndMarkerTTLSeconds)
	if err != nil || acquired {
		if err != nil {
			logx.Errorf("[match] Failed to record party end reason party=%s: %v", partyId, err)
		}
		return reason, culprit
	}
	stored, err := svcCtx.MatchRedis.Get(key)
	if err != nil {
		logx.Errorf("[match] Failed to read party end reason party=%s: %v", partyId, err)
		return reason, culprit
	}
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return reason, culprit
	}
	storedReason, err1 := strconv.ParseUint(parts[0], 10, 32)
	storedCulprit, err2 := strconv.ParseUint(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return reason, culprit
	}
	return uint32(storedReason), storedCulprit
}

// markPartyEndNotified 抢这一单的通知权(SET NX):true = 由调用方通知。
// Redis 出错时返回 true:宁可多推一条,也不要让全队收不到「匹配已取消」。
func markPartyEndNotified(svcCtx *svc.ServiceContext, partyId string) bool {
	acquired, err := svcCtx.MatchRedis.SetnxEx(matchPartyEndNotifiedKey(partyId), "1", partyEndMarkerTTLSeconds)
	if err != nil {
		logx.Errorf("[match] Failed to mark party end notification; notifying anyway party=%s: %v", partyId, err)
		return true
	}
	return acquired
}

// battleLockKind 排队成员身上战斗锁的归类(classifyBattleLock)。
type battleLockKind uint8

const (
	// battleLockNone 没有锁:他不在任何战斗里。
	battleLockNone battleLockKind = iota
	// battleLockResidual 上一局开局失败没清掉的备战锁:锁值就是票据记下的 last_failed_battle_id。
	battleLockResidual
	// battleLockOther 别的战斗的锁:他排队期间真的进了别的战斗(切磋等入口)。
	battleLockOther
)

// classifyBattleLock 按锁值归类排队成员身上的战斗锁。lockValue 是 battle:lock 的值(scene 写的 battle_id,
// 没有锁为空串,见 playerBattleLockValue):
//   - 空 → battleLockNone;
//   - 等于票据记下的 last_failed_battle_id(非 0)→ battleLockResidual:上一局开局失败,scene 侧的解冻丢了,
//     锁会留到自己过期;
//   - 其余(别的 battle_id、不是数字、票据没经历过开局失败)→ battleLockOther。
//
// 「在不在」和「是哪一局」必须出自同一次读:分两次读(先 EXISTS 再 GET)时,锁在中间消失会被当成
// 「在别的战斗里」而删票 —— 一个明明可以成组的人被踢出队列,补位单更是连带整队。
func classifyBattleLock(lockValue string, ticket *queueTicket) battleLockKind {
	if lockValue == "" {
		return battleLockNone
	}
	if ticket == nil || ticket.LastFailedBattleId == 0 {
		return battleLockOther
	}
	lockBattleId, err := strconv.ParseUint(lockValue, 10, 64)
	if err != nil || lockBattleId != ticket.LastFailedBattleId {
		return battleLockOther
	}
	return battleLockResidual
}

// firstMissingPartyTicket 逐人复读 members 的票据,返回第一个「名下已不是 expected 里那一张票」的人
// (票据缺失,或已换成别的票);都还在返回 (0, false)。不看票据状态 —— 刚入队 / 刚回队的单随时可能被别的实例弹出推进。
// 读不到的人跳过、不当成缺失:这是一道补充检查,漏掉的由 matcher 的完整性规则与票据自愈接住。
func firstMissingPartyTicket(ctx context.Context, svcCtx *svc.ServiceContext, members []uint64, expected map[uint64]string) (uint64, bool) {
	for _, pid := range members {
		ticket, err := loadTicketCtx(ctx, svcCtx, pid)
		if err != nil {
			logx.WithContext(ctx).Errorf("[match] Failed to re-read party ticket; skipping this member in the post-push check player=%d: %v", pid, err)
			continue
		}
		if ticket == nil || ticket.Ticket != expected[pid] {
			return pid, true
		}
	}
	return 0, false
}

// cancelParty 把一支补位单整体撤出队列并通知 team(team-backfill-matching.md §4.3)。seed 是这一单任一成员
// 的票据,只用它的 PartyId / PartyMembers / QueueKey / TeamId。返回这一单的名单。
//
// 逐人处理名单里的每一名成员(与 CancelQueue 同序:先删票、后摘 list):
//   - 票据属于这一单:仍是 queued 才删(ticketCancelScript);已被弹出推进 matched 的不动 —— 取消太迟,
//     那一侧的 matcher 发现同单有人票据缺失后自己收尾(见 requeueFront 的补位单完整性规则)。
//     此时不通知,收尾方通知时沿用这里已经登记的原因;
//   - 没有票据:只剩 list 里的残留项,摘掉;
//   - 持有别的票据:这个人已经在排别的单。那张票正指向这条队列且 queued → 他在 list 里的项现在属于新票,不摘;
//     否则是本队列的残留项,摘掉(与 groupPicker.validate 的「别的队列的残留成员」同口径)。
//
// 读票 / 删票出错的成员跳过(不摘 list):残留由 matcher 的票据校验与补位单完整性规则兜底。
// 不持凑单锁也可以调用(CancelQueue 一直如此);删票按 ticket id CAS,摘队列按读票前的写入版本 CAS。
func cancelParty(svcCtx *svc.ServiceContext, seed *queueTicket, reason uint32, culprit uint64) []uint64 {
	if seed == nil || seed.PartyId == "" || len(seed.PartyMembers) == 0 {
		return nil
	}
	// 先快照队列写入版本,再读/删票。成员旧票一旦删除,他的独立会话即可重排;
	// 最终摘项只认这里的版本,新 push 即使是旧命令的自动重试也会生成另一版本。
	var versions map[uint64]string
	if seed.QueueKey != "" {
		var err error
		versions, err = loadQueueMemberVersions(svcCtx, seed.QueueKey, seed.PartyMembers)
		if err != nil {
			logx.Errorf("[match] Party cancel failed to snapshot queue versions; leaving remnants to matcher validation team=%d queue=%s: %v",
				seed.TeamId, seed.QueueKey, err)
		}
	}
	// 撤单原因先登记、后删票:收尾方(matcher 判不完整 / 回队首的完整性规则)要看到某张票被删了才会去通知,
	// 先登记保证它沿用的是这里的原因 —— 否则名单变化会被它说成「某队员未就绪」,点名的还是个无关的人。
	// 静默撤单不登记。
	if reason != constants.PartyEndNone {
		claimPartyEndReason(svcCtx, seed.PartyId, reason, culprit)
	}
	// tooLate:名单里有人的票已经被推进 matched(那一侧的 matcher 正在弹这一单)。这一单此刻还没有真正收干净,
	// 现在通知的话,那个人的视图还是「集合中」。留给 matcher 侧收尾时通知(见 notifyPartyEnded)。
	tooLate := false
	remove := make([]uint64, 0, len(seed.PartyMembers))
	for _, pid := range seed.PartyMembers {
		ticket, err := loadTicket(svcCtx, pid)
		if err != nil {
			logx.Errorf("[match] Party cancel failed to read ticket; leaving remnants to matcher validation team=%d player=%d: %v",
				seed.TeamId, pid, err)
			continue
		}
		switch {
		case ticket == nil:
			remove = append(remove, pid)
		case ticket.PartyId == seed.PartyId:
			deleted, err := cancelTicketIfQueued(svcCtx, pid, ticket.Ticket)
			if err != nil {
				logx.Errorf("[match] Party cancel failed to delete ticket; leaving remnants to matcher validation team=%d player=%d: %v",
					seed.TeamId, pid, err)
				continue
			}
			if !deleted {
				// 没删到有两种可能,要分开:这张票已被推进 matched(真的太迟);或另一条撤单路径刚把它删了 / 换了
				// (两名队员同时取消、取消与 matcher 判不完整重叠)。后一种这一单其实已经收干净,
				// 若也当成太迟,两条路径会各自以为该由对方通知,结果谁都不通知。
				current, rerr := loadTicket(svcCtx, pid)
				switch {
				case rerr != nil:
					logx.Errorf("[match] Party cancel failed to re-read ticket; leaving remnants to matcher validation team=%d player=%d: %v",
						seed.TeamId, pid, rerr)
				case current == nil:
					remove = append(remove, pid)
				case current.PartyId != seed.PartyId:
					if !(current.State == ticketStateQueued && current.QueueKey == seed.QueueKey) {
						remove = append(remove, pid)
					}
				default:
					// 仍是本单的票。它此刻若是 queued:第一次去删时还是 matched,刚被回队首改回来
					// (requeueUnit 逐人 CAS 的中途)—— 再删一次,删到了就不算太迟。
					if current.State == ticketStateQueued {
						again, aerr := cancelTicketIfQueued(svcCtx, pid, current.Ticket)
						if aerr != nil {
							logx.Errorf("[match] Party cancel failed to delete ticket on retry; leaving remnants to matcher validation team=%d player=%d: %v",
								seed.TeamId, pid, aerr)
						}
						if again {
							remove = append(remove, pid)
							continue
						}
					}
					logx.Infof("[match] Party cancel too late for member; ticket already matched team=%d player=%d ticket=%s",
						seed.TeamId, pid, ticket.Ticket)
					tooLate = true
				}
				continue
			}
			remove = append(remove, pid)
		case ticket.State == ticketStateQueued && ticket.QueueKey == seed.QueueKey:
			// 已换成另一张票并且就排在这条队列里:list 项归新票所有。
		default:
			remove = append(remove, pid)
		}
	}
	if len(remove) > 0 && seed.QueueKey != "" && versions != nil {
		if _, err := removeQueueMembersVersionedCtx(context.Background(), svcCtx, seed.QueueKey, remove, versions); err != nil {
			logx.Errorf("[match] Party cancel failed to dequeue (matcher ticket validation will handle remnants) team=%d queue=%s members=%v: %v",
				seed.TeamId, seed.QueueKey, remove, err)
		}
	}
	logx.Infof("[match] Party left backfill queue team=%d party=%s queue=%s members=%v reason=%d culprit=%d too_late=%t",
		seed.TeamId, seed.PartyId, seed.QueueKey, seed.PartyMembers, reason, culprit, tooLate)
	if !tooLate {
		notifyPartyEnded(svcCtx, seed, reason, culprit)
	}
	return append([]uint64(nil), seed.PartyMembers...)
}

// leaveQueueForOtherBattle 玩家经不带票据的入口(切磋)开局成功时调用:他若还挂着 queued 的排队票据,立即退出排队。
//
// 「排队期间进了别的战斗 = 出局」本来就是规则(matcher 校验成员时见战斗锁就删票),但那是惰性的:
// 队列人数不够凑满时 matcher 不扫,票据会一直留到这场战斗打完之后。单排的人打完切磋被「已在排队」卡住;
// 补位单更糟 —— 他的客户端在开战时已把这一次排队记为结束,排队面板没了、取消不了,队友却还显示匹配中,
// 之后路人凑满,他被直接拉进副本。
//
//   - 单人票:queued CAS 删票 + 出队;
//   - 补位票:queued CAS 删掉自己的票,删到了再撤整单并通知全队(原因:该成员未就绪);
//   - 票据已是 matched / ready(凑单在途):不动 —— 那一局走到他时 PrepareBattle 会因他已在战斗中而失败,按肇事者收尾;
//     那一局若先败在别人身上,回队首时会看到他的战斗锁而不让他回队(dropMembersInOtherBattle)。
//
// 尽力而为:出错只记日志,残留由 matcher 的战斗锁校验与票据自愈兜底。
func leaveQueueForOtherBattle(svcCtx *svc.ServiceContext, playerId uint64) {
	ticket, err := loadTicket(svcCtx, playerId)
	if err != nil {
		logx.Errorf("[match] Failed to read ticket of a player who entered another battle (matcher validation will remove it) player=%d: %v",
			playerId, err)
		return
	}
	if ticket == nil || ticket.State != ticketStateQueued {
		return
	}
	deleted, err := cancelTicketIfQueued(svcCtx, playerId, ticket.Ticket)
	if err != nil {
		logx.Errorf("[match] Failed to delete ticket of a player who entered another battle (matcher validation will remove it) player=%d: %v",
			playerId, err)
		return
	}
	if !deleted {
		return
	}
	logx.Infof("[match] Queued player entered another battle; leaving queue player=%d queue=%s team=%d party=%s",
		playerId, ticket.QueueKey, ticket.TeamId, ticket.PartyId)
	if ticket.PartyId != "" {
		cancelParty(svcCtx, ticket, constants.PartyEndMemberNotReady, playerId)
		return
	}
	queueKey := ticketQueueKey(ticket)
	if err := dequeueMemberCtx(context.Background(), svcCtx, queueKey, playerId); err != nil {
		logx.Errorf("[match] Failed to dequeue a player who entered another battle (matcher ticket validation will handle remnants) player=%d queue=%s: %v",
			playerId, queueKey, err)
	}
}

// partyMissingMember 这一单的名单里第一个「已经不持有本单票据」的成员(票据缺失或已换成别的票);都持有返回 0。
// skip 是调用方自己(他的票就是 seed,不用再读)。读票出错返回 err,调用方不做失效判定。
func partyMissingMember(svcCtx *svc.ServiceContext, seed *queueTicket, skip uint64) (uint64, error) {
	for _, pid := range seed.PartyMembers {
		if pid == skip {
			continue
		}
		ticket, err := loadTicket(svcCtx, pid)
		if err != nil {
			return 0, err
		}
		if ticket == nil || ticket.PartyId != seed.PartyId {
			return pid, nil
		}
	}
	return 0, nil
}

// healPartyTicket healOrphanTicket 的补位票分支(JoinQueue 与整队开战预检共用)。existing 是 playerId 的
// queued 补位票,inQueue 是它在不在 list 里。返回 true = 这一单已失效并撤掉,调用方可以继续;
// false = 确实在排(或拿不准,按在排处理)。
//
//   - 不在 list:入队宽限内(partyEnqueueGrace)按「正在入队」处理;超过宽限 = 崩溃残留,撤整单;
//   - 在 list:先核对同单其他人是否还持有本单的票(半撤状态的兜底),再问 team 复核(队伍还在、成员集合与
//     名单相等、全员在线),任一项不过就撤整单。这是「主动撤单没成功」的兜底:否则离队的人会一直被
//     ErrAlreadyQueued 卡到这一单被弹组时才发现 —— 而队列人数不到凑满人数时 matcher 根本不扫它。
func healPartyTicket(svcCtx *svc.ServiceContext, log logx.Logger, playerId uint64, existing *queueTicket, inQueue bool) (bool, error) {
	reason, culprit := constants.PartyEndMemberNotReady, uint64(0)
	if inQueue {
		// 先看这一单是否还完整:撤单逐人删票时若在某个人身上遇到 Redis 抖动,会留下「有人已无票、有人还 queued
		// 且在 list 里」的半撤状态。队列人数不到凑满人数时 matcher 根本不扫这条队列,只能靠这里发现。
		missing, err := partyMissingMember(svcCtx, existing, playerId)
		if err != nil {
			return false, err
		}
		if missing != 0 {
			culprit = missing
		} else {
			port := svcCtx.PartyTeam()
			if port == nil {
				return false, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), partyCheckTimeout)
			defer cancel()
			reason, culprit, err = port.CheckParty(ctx, existing.TeamId, existing.PartyMembers)
			if err != nil {
				return false, err
			}
			if reason == constants.PartyEndNone {
				return false, nil
			}
		}
	} else if now := nowMs(); existing.EnqueuedAtMs+uint64(partyEnqueueGrace/time.Millisecond) > now {
		return false, nil
	}
	cancelParty(svcCtx, existing, reason, culprit)
	// 撤单按 ticket id 做 CAS:自己的票若恰好在这期间被弹出推进了 matched,它还在 —— 那就是确实在途。
	current, err := loadTicket(svcCtx, playerId)
	if err != nil {
		return false, err
	}
	if current != nil && current.Ticket == existing.Ticket {
		return false, nil
	}
	log.Errorf("[match] Cleared stale party backfill ticket player=%d team=%d ticket=%s queue=%q in_queue=%t reason=%d culprit=%d",
		playerId, existing.TeamId, existing.Ticket, existing.QueueKey, inQueue, reason, culprit)
	return true, nil
}

// ---- team.BattleStarter 的补位部分(方法签名只用内建类型,team 不 import logic) ----

// EnqueueParty 整队进 targetSize 人数档的队列等补位(team-backfill-matching.md §4.1 第 2 步)。
// roster 是 team 排好序的名单(队长在前、其余按 join_seq),原样记进票据,凑满后就是这一单在战斗里的站位顺序。
//
// 返回 (0, nil) = 已入队。否则本次写下的票据都已回滚:
//   - (pid, nil)  该成员没能持有本单的票:他已有别的票据(并发单排 / 重复开战的后来者),
//     或入队途中这一单被撤、他的票已经不在(其余人的票与队列项已一并撤掉);
//   - (pid, err)  给该成员建票时 Redis 出错(结果未知,按已写入回滚);
//   - (0, err)    参数不合法,或票都建好了但整批入队失败。
//
// 前置条件(team 已校验,这里只做防御性复核):len(roster) < targetSize ≤ 副本人数上限,roster 非空无重复。
// ctx 约束建票与入队;回滚用独立 ctx。
func (s *TeamBattleStarter) EnqueueParty(ctx context.Context, battleConfigId, targetSize uint32, teamId uint64,
	roster []uint64, zones map[uint64]uint32,
) (uint64, error) {
	return s.EnqueuePartyMode(ctx, uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), battleConfigId, targetSize, teamId, roster, zones)
}

// targetSize对PVP表示每侧人数，完整的满员队伍也必须等待另一方真人。
func (s *TeamBattleStarter) EnqueuePartyMode(ctx context.Context, rawMode, battleConfigId, targetSize uint32, teamId uint64,
	roster []uint64, zones map[uint64]uint32,
) (uint64, error) {
	log := logx.WithContext(ctx)
	limit := s.TeamSizeFor(rawMode, battleConfigId)
	mode := matchpb.MatchMode(rawMode)
	if mode != matchpb.MatchMode_MATCH_MODE_PVE_TEAM && mode != matchpb.MatchMode_MATCH_MODE_PVP_TEAM {
		return 0, fmt.Errorf("invalid party match mode: %d", rawMode)
	}
	fullPve := mode == matchpb.MatchMode_MATCH_MODE_PVE_TEAM && uint32(len(roster)) == targetSize
	if len(roster) == 0 || teamId == 0 || uint32(len(roster)) > targetSize || fullPve || targetSize > limit {
		return 0, fmt.Errorf("invalid party enqueue arguments: team=%d roster=%d target=%d limit=%d", teamId, len(roster), targetSize, limit)
	}
	required := targetSize
	if mode == matchpb.MatchMode_MATCH_MODE_PVP_TEAM {
		required *= 2
	}
	queueKey := queueKeyForJoin(mode, battleConfigId, required)
	partyId := uuid.New().String()
	enqueuedAt := nowMs()
	ttl := ticketTTLSeconds(s.svcCtx)

	tickets := make(map[uint64]string, len(roster))
	pushes := make([]queuePush, 0, len(roster))
	owned := make([]uint64, 0, len(roster))
	for _, pid := range roster {
		tickets[pid] = uuid.New().String()
		rating := loadRatingOrDefault(s.svcCtx, pid)
		created, err := createTeamTicketFn(ctx, s.svcCtx, pid, &queueTicket{
			Ticket:       tickets[pid],
			Mode:         int32(mode),
			Config:       battleConfigId,
			State:        ticketStateQueued,
			EnqueuedAtMs: enqueuedAt,
			ZoneId:       zones[pid],
			QueueKey:     queueKey,
			Rating:       rating,
			TeamId:       teamId,
			PartyId:      partyId,
			PartyMembers: roster,
		}, ttl)
		if err != nil {
			log.Errorf("[match] Failed to create party backfill ticket (unknown outcome; rolling back as written) team=%d player=%d: %v",
				teamId, pid, err)
			s.rollbackTickets(teamId, append(owned, pid), tickets)
			return pid, err
		}
		if !created {
			log.Infof("[match] Party backfill ticket creation conflict: member already has a ticket team=%d player=%d", teamId, pid)
			s.rollbackTickets(teamId, owned, tickets)
			metrics.ObserveJoinQueue(mode.String(), "party_conflict")
			return pid, nil
		}
		owned = append(owned, pid)
		pushes = append(pushes, queuePush{playerId: pid, rating: rating})
	}

	if err := pushMembersRegistered(ctx, s.svcCtx, queueKey, pushes, false); err != nil {
		// 结果未知(超时 / 断连时 Lua 可能已执行):票据回滚,list 里可能留下的项尽力摘一次,
		// 摘不掉的由 matcher 的票据校验丢弃。
		log.Errorf("[match] Failed to enqueue party for backfill; rolling back tickets team=%d queue=%s: %v", teamId, queueKey, err)
		s.rollbackTickets(teamId, owned, tickets)
		if _, rmErr := removeQueueMembers(s.svcCtx, queueKey, roster); rmErr != nil {
			logx.Errorf("[match] Failed to dequeue after party enqueue failure (matcher ticket validation will handle remnants) team=%d queue=%s: %v",
				teamId, queueKey, rmErr)
		}
		return 0, err
	}
	// 建票循环中途若有一次撤单落下来(名单变化的撤单在提交之后异步执行,可能迟到;它按 team_id 找票),
	// 已建好的票被删、还没建的照常建,入队的就成了半单。整批入队之后逐人复读:有人名下不是本次建的那张票,
	// 就静默撤掉整单并按「该成员没能入队」返回,队长可以重试。那次撤单自己已经按它的原因通知过全队,这里不再通知。
	if absent, withdrawn := firstMissingPartyTicket(ctx, s.svcCtx, roster, tickets); withdrawn {
		log.Infof("[match] Party withdrawn while it was being enqueued; removing the rest team=%d party=%s absent=%d",
			teamId, partyId, absent)
		cancelParty(s.svcCtx, &queueTicket{TeamId: teamId, PartyId: partyId, PartyMembers: roster, QueueKey: queueKey},
			constants.PartyEndNone, 0)
		metrics.ObserveJoinQueue(mode.String(), "party_withdrawn")
		return absent, nil
	}
	log.Infof("[match] Party enqueued for backfill team=%d party=%s config=%d target=%d queue=%s roster=%v",
		teamId, partyId, battleConfigId, targetSize, queueKey, roster)
	// 与单人入队同一条指标(join_queue_total),outcome 单列:一支补位单记一次,不按人头。
	metrics.ObserveJoinQueue(mode.String(), "party_ok")
	return 0, nil
}

// 视图模式来自本人当前票据；不在队伍记录里制造第二份匹配状态。
func (s *TeamBattleStarter) PartyQueueMode(ctx context.Context, playerId, teamId uint64) (uint32, error) {
	if s.svcCtx.Authority != nil {
		ticket, err := s.svcCtx.Authority.Ticket(ctx, playerId)
		if err != nil || ticket == nil || ticket.TeamId != teamId {
			return 0, err
		}
		return ticket.Mode, nil
	}
	ticket, err := loadTicketCtx(ctx, s.svcCtx, playerId)
	if err != nil || ticket == nil || ticket.TeamId != teamId || ticket.PartyId == "" {
		return 0, err
	}
	return uint32(ticket.Mode), nil
}

// CancelParty 撤掉 teamId 仍在排的补位单(team-backfill-matching.md §4.3:名单变化、入队后复核不过)。
// candidates 是可能持有这一单票据的成员(名单变化前的成员表),找到第一张属于本队的 queued 补位票就按它的
// 名单撤整单并返回该名单;一张都没有返回 (nil, nil)(队伍没在排,或已凑满弹出、取消太迟)。
// keepIfRoster 非空时:找到的那一单名单若恰好等于它(集合语义)就不撤 —— 名单变化的撤单是提交之后异步执行的,
// 传「提交后的成员集合」进来,迟到执行时就不会误杀队长已经按新名单重新入队的新单(它本来就是有效的)。
// 解散(提交后没有成员)与入队回滚传 nil,无条件撤。
// reason / culprit 原样带进给 team 的通知(constants.PartyEnd*;PartyEndNone = 静默,不通知)。读票出错返回 err,调用方只记日志:
// 没撤成的单由 healPartyTicket 与 matcher 弹组前复核兜底。
func (s *TeamBattleStarter) CancelParty(ctx context.Context, teamId uint64, candidates, keepIfRoster []uint64,
	reason uint32, culprit uint64,
) ([]uint64, error) {
	if s.svcCtx.Authority != nil {
		// 名册变化已在SQL同tx撤全unit；后台通知不能用当前成员补旧意图授权。
		return nil, nil
	}
	if teamId == 0 {
		return nil, errors.New("CancelParty: team_id is 0")
	}
	for _, pid := range candidates {
		ticket, err := loadTicketCtx(ctx, s.svcCtx, pid)
		if err != nil {
			return nil, err
		}
		if ticket == nil || ticket.PartyId == "" || ticket.TeamId != teamId || ticket.State != ticketStateQueued {
			continue
		}
		if len(keepIfRoster) > 0 && samePlayerSet(ticket.PartyMembers, keepIfRoster) {
			logx.WithContext(ctx).Infof("[match] Party roster already matches the committed roster; not withdrawing team=%d party=%s",
				teamId, ticket.PartyId)
			return nil, nil
		}
		return cancelParty(s.svcCtx, ticket, reason, culprit), nil
	}
	return nil, nil
}

// samePlayerSet 两份名单是同一个集合(顺序无关;入参都不含重复)。
func samePlayerSet(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uint64]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

// PartyQueueState playerId 是否正随 teamId 在补位(team-backfill-matching.md §4.5,team 用它装饰队伍视图)。
// 返回 (constants.PartyQueue*, 目标人数, 副本, 入队时刻毫秒, err);后三项只在 Queued 时有值。
// ready 票(已开战,短暂停留)按 None:战斗内状态不由队伍视图表达,不能让一场已打完的战斗把开战按钮锁住。
func (s *TeamBattleStarter) PartyQueueState(ctx context.Context, playerId, teamId uint64) (uint32, uint32, uint32, uint64, error) {
	if s.svcCtx.Authority != nil {
		return s.svcCtx.Authority.TeamQueueState(ctx, playerId, teamId)
	}
	ticket, err := loadTicketCtx(ctx, s.svcCtx, playerId)
	if err != nil {
		return constants.PartyQueueNone, 0, 0, 0, err
	}
	if ticket == nil || ticket.PartyId == "" || teamId == 0 || ticket.TeamId != teamId {
		return constants.PartyQueueNone, 0, 0, 0, nil
	}
	switch ticket.State {
	case ticketStateQueued:
		return constants.PartyQueueQueued, queueTeamSize(ticket.QueueKey), ticket.Config, ticket.EnqueuedAtMs, nil
	case ticketStateMatched:
		return constants.PartyQueueMatched, 0, 0, 0, nil
	}
	return constants.PartyQueueNone, 0, 0, 0, nil
}
