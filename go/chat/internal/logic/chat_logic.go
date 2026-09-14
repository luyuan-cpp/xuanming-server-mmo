// Package logic 是 chat v1 的业务逻辑:SendChat / PullChatHistory(契约 zone_contract_v1 §9)。
//
// 错误语义:一律 in-band —— `return &Resp{ErrorMessage: &TipInfoMessage{Id: code}}, nil`。
// 为什么不走 gRPC error:路由服 / gate 的回包桥接只认成功响应,gRPC 错误在 gate 侧无法路由回
// 会话,客户端只会看到超时;真故障(Redis 挂了)用 fault 码 kServiceUnavailable 表达,
// 由 serverbase 拦截器记成 rpc_inband_fault 并告警。
//
// 存储(全部 ChatRedis、全部单 key 操作,Redis Cluster 安全):
//
//	chat:{world}:log            LIST  全服唯一世界频道(与 scene_manager world_channels:zone:{z} 无关)
//	chat:{p:<小id>:<大id>}:log  LIST  私聊会话,双方同一把 key
//	chat:{req:<pid>}:<rid>      STRING SendChat 幂等键,SET NX EX
//	chat:{rl:<pid>}             STRING 每秒发言计数
//
// 历史是 7 天 / 200 条的尽力而为窗口,不是权威账本:Redis 重启即清空(契约 §9)。
package logic

import (
	"context"
	"crypto/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"chat/internal/constants"
	"chat/internal/session"
	"chat/internal/svc"

	chatpb "proto/chat"
	base "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const (
	// worldLogKey 世界频道历史。hash tag {world} 让它在集群里固定落一个 slot(单 key,本无必要,
	// 统一写法便于日后给同频道追加辅助 key 时直接同 slot)。
	worldLogKey = "chat:{world}:log"

	// rateLimitWindowSeconds 限速窗口(秒)。契约写死"每秒 N 条",所以不进配置。
	rateLimitWindowSeconds = 1

	// maxRequestIdBytes request_id 的长度上限。request_id 是客户端可控字符串且直接拼进 Redis key,
	// 不设上限等于让客户端决定 key 大小(每条 60s 常驻内存)。64 足够放 UUID(36)与带前缀的 nonce。
	maxRequestIdBytes = 64

	// claimReleaseTimeout 释放 / 完结幂等键的独立超时:两者都发生在原请求 ctx 可能已到期的时刻
	// (失败路径、或写入 Lua 刚好耗尽预算),沿用它会让操作必然失败。
	claimReleaseTimeout = time.Second

	// 幂等键的两态值。为什么要两态而不是只 SET NX 一个 "1":
	// 首发占住键后还要跑限速与写入,这段时间里同 request_id 的重试若一见键存在就回成功,而首发随后
	// 失败并释放了键,客户端就以为发出去了、消息却没落库,也不会再重试 —— 静默丢失。
	// 所以只有读到 done(首发确已写入)才按成功回;读到 pending 回业务拒绝码让客户端稍后重试。
	claimPending = "pending"
	claimDone    = "done"
)

// releaseClaimScript 只释放本次占用的 pending:token。旧请求可能在 TTL 过期后才失败,
// 此时同 key 已经属于新请求;仅比较 pending 状态或无条件 DEL 都会误删新请求的状态。
const releaseClaimScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`

// markClaimDoneScript:仅当值仍等于本次 pending:token 时改成 done 并重置 TTL。
// token 每次成功占用都不同,旧请求不能完结过期后新建的 pending。
// 与历史写入分开调用:两把 key 的 hash tag 不同,Redis Cluster 禁止跨 slot 原子操作。
// 因而 token 只保证清理和完结的归属,不能阻止过期请求迟到写历史;这类偶发重复与写入超时
// 但实际已落库的情况一样,仍按契约 §9.3 的尽力幂等处理。
const markClaimDoneScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then
	redis.call("SET", KEYS[1], ARGV[2], "EX", ARGV[3])
	return 1
end
return 0`

// rateLimitScript:INCR 后首次(=1)设 1 秒过期,返回当前计数(契约 §3 两层限速的第二层)。
// 必须一条 Lua:INCR 与 EXPIRE 分两次调用时,中间进程崩溃 / 超时会留下永不过期的计数器,
// 该玩家从此永久被限速。额外判 TTL == -1 是对存量无 TTL 计数器的自愈(例如被人手工 SET 过)。
const rateLimitScript = `local n = redis.call("INCR", KEYS[1])
if n == 1 or redis.call("TTL", KEYS[1]) == -1 then
	redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return n`

// appendLogScript:LPUSH 新消息 + LTRIM 保留最近 ARGV[2] 条 + 续期 ARGV[3] 秒,一条 Lua 原子完成。
// 分开调用的话,LPUSH 成功而 LTRIM 失败会让 LIST 无上限增长;EXPIRE 失败会让冷私聊永不过期。
const appendLogScript = `redis.call("LPUSH", KEYS[1], ARGV[1])
redis.call("LTRIM", KEYS[1], 0, tonumber(ARGV[2]) - 1)
redis.call("EXPIRE", KEYS[1], ARGV[3])
return 1`

// privateLogKey 私聊历史 key:两个 id 排序后拼,保证 A→B 与 B→A 写同一把 key。
func privateLogKey(a, b uint64) string {
	if a > b {
		a, b = b, a
	}
	return "chat:{p:" + strconv.FormatUint(a, 10) + ":" + strconv.FormatUint(b, 10) + "}:log"
}

// requestIdKey 幂等键:带 pid,不同玩家恰好用了同一个 request_id 互不影响。
func requestIdKey(playerId uint64, requestId string) string {
	return "chat:{req:" + strconv.FormatUint(playerId, 10) + "}:" + requestId
}

// rateLimitKey 限速计数 key。
func rateLimitKey(playerId uint64) string {
	return "chat:{rl:" + strconv.FormatUint(playerId, 10) + "}"
}

// rejection 是一次业务拒绝:给客户端的 tip 码 + 指标 outcome,两者总是成对出现。
type rejection struct {
	code    uint32
	outcome string
}

var (
	rejectNoSession          = rejection{code: constants.ErrInvalidParameter, outcome: svc.OutcomeNoSession}
	rejectBadRequest         = rejection{code: constants.ErrInvalidParameter, outcome: svc.OutcomeBadRequest}
	rejectChannelUnavailable = rejection{code: constants.ErrChannelUnavailable, outcome: svc.OutcomeChannelUnavailable}
	rejectTooLong            = rejection{code: constants.ErrMessageTooLong, outcome: svc.OutcomeTooLong}
	rejectRateLimited        = rejection{code: constants.ErrRateLimited, outcome: svc.OutcomeRateLimited}
	rejectStorage            = rejection{code: constants.ErrStorage, outcome: svc.OutcomeStorageError}
)

// ChatLogic 是一次请求的逻辑上下文(照 go-zero logic 惯例,每请求一个)。
type ChatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
	// now 是时间源,生产恒为 time.Now;单测注入固定时钟验证"按 send_time_ms 排序"。
	now func() time.Time
}

func NewChatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ChatLogic {
	return &ChatLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
		now:    time.Now,
	}
}

// SendChat 发一条聊天。
//
// 顺序是刻意的:先做全部纯校验(身份 → 频道 → 内容 → request_id → 序列化),再按
// 幂等 → 限速 → 写入 的顺序碰 Redis。
//   - 纯校验在前:被拒的请求不消耗限速额度、不占幂等键;
//   - 幂等在限速之前:首发已写入(键为 done)的重发(gate / 客户端超时重试)无论当前是否超速,
//     都回"成功"—— 同一请求得到同一结果,客户端不会对已经发出去的消息弹"发送太快";
//   - 幂等键先占成 pending,写入成功后才改 done;占住之后任何一步失败(限速 / Redis 故障)都
//     **释放**幂等键,让客户端能用同一个 request_id 重试。代价:写入 Lua 超时但实际已落库时,
//     重试会产生一条重复消息 —— 对聊天而言"偶发重复"比"静默丢失"更可接受;
//   - 并发的同 request_id 请求:后到者读到 pending(先到者还没写完)回 kRateLimitExceeded 让它稍后重试,
//     而**不是**回成功 —— 否则先到者随后失败并释放键时,客户端以为发出去了、消息却没落库(静默丢失)。
func (l *ChatLogic) SendChat(in *chatpb.SendChatRequest) (*chatpb.SendChatResponse, error) {
	msg := in.GetMessage()
	channelLabel := svc.ChannelLabel(msg.GetChannel())
	reject := func(r rejection) (*chatpb.SendChatResponse, error) {
		svc.ObserveSend(channelLabel, r.outcome)
		return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: r.code}}, nil
	}
	cfg := l.svcCtx.Config
	rds := l.svcCtx.ChatRedis

	// ① 身份:发言人只认会话,请求体里的 sender_player_id 下面会被覆盖。
	senderId, ok := session.PlayerID(l.ctx)
	if !ok {
		return reject(rejectNoSession)
	}
	if msg == nil {
		return reject(rejectBadRequest)
	}

	// ② 频道 → 存储 key。WORLD 的 target 没有意义,落库前清零,免得客户端塞的值被别人拉到。
	logKey, rej, ok := resolveLogKey(msg.GetChannel(), senderId, msg.GetTargetPlayerId())
	if !ok {
		return reject(rej)
	}
	targetId := uint64(0)
	if msg.GetChannel() == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE {
		targetId = msg.GetTargetPlayerId()
	}

	// ③ 内容:按原始字节数限长(不是 rune 数 —— 存储与带宽都按字节算),trim 后不能为空。
	//    proto3 string 反序列化时已保证是合法 UTF-8,这里不必再校验编码。
	if len(msg.GetContent()) > cfg.MaxContentBytes {
		return reject(rejectTooLong)
	}
	content := strings.TrimSpace(msg.GetContent())
	if content == "" {
		return reject(rejectTooLong)
	}

	requestId := in.GetRequestId()
	if len(requestId) > maxRequestIdBytes {
		return reject(rejectBadRequest)
	}

	// 服务端盖时间戳、覆盖发言人:历史里的每个字段都由服务端决定,客户端伪造不了。
	stored, err := proto.Marshal(&chatpb.ChatMessage{
		SenderPlayerId: senderId,
		TargetPlayerId: targetId,
		Channel:        msg.GetChannel(),
		Content:        content,
		SendTimeMs:     l.now().UnixMilli(),
	})
	if err != nil {
		l.Errorf("[chat] ChatMessage 序列化失败 sender=%d: %v", senderId, err)
		return reject(rejectStorage)
	}

	// ④ 幂等:request_id 为空则不做(契约 §9)。先占成 pending,写入成功后才改 done(见 claimPending 注释)。
	claimKey, claimValue := "", ""
	if requestId != "" {
		claimKey = requestIdKey(senderId, requestId)
		claimValue = claimPending + ":" + rand.Text()
		claimed, err := rds.SetnxExCtx(l.ctx, claimKey, claimValue, cfg.RequestIdTTLSeconds)
		if err != nil {
			l.Errorf("[chat] 幂等键 SET NX 失败 sender=%d: %v", senderId, err)
			return reject(rejectStorage)
		}
		if !claimed {
			return l.duplicateResponse(claimKey, senderId, channelLabel)
		}
	}

	// ⑤ 限速。
	res, err := rds.EvalCtx(l.ctx, rateLimitScript, []string{rateLimitKey(senderId)}, strconv.Itoa(rateLimitWindowSeconds))
	if err != nil {
		l.releaseClaim(claimKey, claimValue)
		l.Errorf("[chat] 限速脚本失败 sender=%d: %v", senderId, err)
		return reject(rejectStorage)
	}
	count, isInt := res.(int64)
	if !isInt {
		l.releaseClaim(claimKey, claimValue)
		l.Errorf("[chat] 限速脚本返回形态异常 sender=%d: %T %v", senderId, res, res)
		return reject(rejectStorage)
	}
	if count > int64(cfg.RateLimitPerSecond) {
		l.releaseClaim(claimKey, claimValue)
		return reject(rejectRateLimited)
	}

	// ⑥ 写入:LPUSH + LTRIM + EXPIRE 一条 Lua。
	if _, err := rds.EvalCtx(l.ctx, appendLogScript, []string{logKey}, string(stored),
		strconv.Itoa(cfg.HistoryMaxEntries), strconv.Itoa(cfg.HistoryTTLSeconds)); err != nil {
		l.releaseClaim(claimKey, claimValue)
		l.Errorf("[chat] 写历史失败 sender=%d key=%s: %v", senderId, logKey, err)
		return reject(rejectStorage)
	}

	// ⑦ 幂等键 pending → done。消息已落库,这一步失败也照样回成功(见 markClaimDone)。
	l.markClaimDone(claimKey, claimValue, senderId)

	svc.ObserveSend(channelLabel, svc.OutcomeOK)
	return &chatpb.SendChatResponse{}, nil
}

// PullChatHistory 拉某频道最近 N 条(快照式,新在前)。
//
// v1 无游标:v1.1 在 PullChatHistoryRequest 追加 since 游标(append-only)后再做增量拉取(契约 §5)。
func (l *ChatLogic) PullChatHistory(in *chatpb.PullChatHistoryRequest) (*chatpb.PullChatHistoryResponse, error) {
	channelLabel := svc.ChannelLabel(in.GetChannel())
	reject := func(r rejection) (*chatpb.PullChatHistoryResponse, error) {
		svc.ObservePull(channelLabel, r.outcome)
		return &chatpb.PullChatHistoryResponse{ErrorMessage: &base.TipInfoMessage{Id: r.code}}, nil
	}
	cfg := l.svcCtx.Config

	selfId, ok := session.PlayerID(l.ctx)
	if !ok {
		return reject(rejectNoSession)
	}
	// 私聊 key 由 (会话里的自己, peer) 决定:玩家只能拉到自己参与的私聊,拿别人的 id 当 peer
	// 拉到的是自己和对方的会话,看不到第三者之间的记录。
	logKey, rej, ok := resolveLogKey(in.GetChannel(), selfId, in.GetPeerPlayerId())
	if !ok {
		return reject(rej)
	}

	limit := clampHistoryLimit(in.GetLimit(), cfg.HistoryDefaultLimit, cfg.HistoryMaxLimit)
	raws, err := l.svcCtx.ChatRedis.LrangeCtx(l.ctx, logKey, 0, limit-1)
	if err != nil {
		l.Errorf("[chat] 读历史失败 player=%d key=%s: %v", selfId, logKey, err)
		return reject(rejectStorage)
	}

	messages := make([]*chatpb.ChatMessage, 0, len(raws))
	for _, raw := range raws {
		m := &chatpb.ChatMessage{}
		if err := proto.Unmarshal([]byte(raw), m); err != nil {
			// 单条坏数据跳过而不是整体失败:一条损坏记录不该让整个频道对所有人不可读。
			l.Errorf("[chat] 历史条目反序列化失败,已跳过 key=%s: %v", logKey, err)
			continue
		}
		messages = append(messages, m)
	}
	// LPUSH 顺序在单实例下已是新在前,但多实例时钟偏差会让"写入顺序"与"send_time_ms"不一致;
	// 以服务端时间戳为准排一次,客户端展示顺序与消息上的时间一致。稳定排序:同毫秒保持写入顺序。
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].GetSendTimeMs() > messages[j].GetSendTimeMs()
	})

	svc.ObservePull(channelLabel, svc.OutcomeOK)
	return &chatpb.PullChatHistoryResponse{Messages: messages}, nil
}

// resolveLogKey 按频道决定历史 key;SendChat(peer = target)与 PullChatHistory(peer = peer_player_id)共用,
// 保证"写到哪"与"从哪读"只有一处定义。ok=false 时 rej 给出拒绝原因。
func resolveLogKey(channel chatpb.ChatChannelType, selfId, peerId uint64) (logKey string, rej rejection, ok bool) {
	switch channel {
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD:
		return worldLogKey, rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE:
		if peerId == 0 || peerId == selfId {
			return "", rejectBadRequest, false
		}
		return privateLogKey(selfId, peerId), rejection{}, true
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM,
		chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM,
		chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED:
		// TEAM 要队伍服务给成员关系、SYSTEM 只能服务端发 —— v1 都不开放(契约 §9)。
		return "", rejectChannelUnavailable, false
	default:
		// 客户端传了本版本不认识的枚举数值。
		return "", rejectBadRequest, false
	}
}

// clampHistoryLimit:0 取默认值,超上限钳到上限。返回值 ≥1(Validate 保证 default/max 为正)。
func clampHistoryLimit(requested uint32, defaultLimit, maxLimit int) int {
	if requested == 0 {
		return defaultLimit
	}
	if uint64(requested) > uint64(maxLimit) {
		return maxLimit
	}
	return int(requested)
}

// releaseClaim 失败路径上删除幂等键,让客户端能用同一 request_id 重试。尽力而为:
// 删失败只记日志,最坏情况是键停在 pending,该 request_id 在 RequestIdTTLSeconds 内重发回限速码(不会被当成功吞掉)。
func (l *ChatLogic) releaseClaim(claimKey, claimValue string) {
	if claimKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), claimReleaseTimeout)
	defer cancel()
	if _, err := l.svcCtx.ChatRedis.EvalCtx(ctx, releaseClaimScript, []string{claimKey}, claimValue); err != nil {
		l.Errorf("[chat] 释放幂等键失败 key=%s: %v(TTL 内同 request_id 重发会回 kRateLimitExceeded,过期后可重试)", claimKey, err)
	}
}

// markClaimDone 写入成功后把幂等键从 pending 改成 done,之后的同 request_id 重发才按成功回。
//
// 失败只记日志、不改变本次响应:消息已经落库,回失败会诱发客户端换新 request_id 重发 → 重复消息。
// 代价是键停在 pending 直到 TTL 过期:这段时间重发回 kRateLimitExceeded,过期后重发会再写一条 ——
// 与"写入 Lua 超时但实际已落库"同一类偶发重复,聊天可接受(重复比丢失可接受)。
func (l *ChatLogic) markClaimDone(claimKey, claimValue string, senderId uint64) {
	if claimKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), claimReleaseTimeout)
	defer cancel()
	if _, err := l.svcCtx.ChatRedis.EvalCtx(ctx, markClaimDoneScript, []string{claimKey},
		claimValue, claimDone, strconv.Itoa(l.svcCtx.Config.RequestIdTTLSeconds)); err != nil {
		l.Errorf("[chat] 幂等键置 done 失败 sender=%d key=%s: %v(消息已写入;TTL 内同 request_id 重发会回限速码)",
			senderId, claimKey, err)
	}
}

// duplicateResponse 处理"幂等键已被占"的重发:按首发的状态决定回什么。
//   - done:首发已写入 → 回成功,不重复写(同一请求同一结果);
//   - pending:首发还在跑限速 / 写入,或首发进程崩在半路 → 回 kRateLimitExceeded(业务拒绝码,不是 fault 码),
//     客户端稍后用同一 request_id 重试;首发若最终失败会释放键,重试就能真正写入 —— 不会静默丢消息;
//   - 键恰在 SET NX 与本次 GET 之间过期 / 被释放(读到空):同样按 pending 处理,让客户端重试一次即可,
//     不在这里递归重新占键,免得把一次重发变成无界循环。
//
// 读失败是真故障,回 kServiceUnavailable。
func (l *ChatLogic) duplicateResponse(claimKey string, senderId uint64, channelLabel string) (*chatpb.SendChatResponse, error) {
	state, err := l.svcCtx.ChatRedis.GetCtx(l.ctx, claimKey)
	if err != nil {
		l.Errorf("[chat] 读幂等键状态失败 sender=%d: %v", senderId, err)
		svc.ObserveSend(channelLabel, rejectStorage.outcome)
		return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: rejectStorage.code}}, nil
	}
	if state == claimDone {
		svc.ObserveSend(channelLabel, svc.OutcomeDuplicate)
		return &chatpb.SendChatResponse{}, nil
	}
	svc.ObserveSend(channelLabel, svc.OutcomeInFlight)
	return &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: rejectRateLimited.code}}, nil
}
