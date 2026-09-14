package logic

// chat v1 逻辑层的 miniredis 集成测试(miniredis 内置 gopher-lua,限速 / 写入两条 Lua 真实执行)。
// 直接构造 ServiceContext:NewServiceContext 需要 etcd。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"chat/internal/config"
	"chat/internal/constants"
	"chat/internal/session"
	"chat/internal/svc"

	chatpb "proto/chat"
	base "proto/common/base"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	world   = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD
	private = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE

	playerA = uint64(1001)
	playerB = uint64(1002)
	playerC = uint64(1003)
)

// testConfig 与 etc/chat.yaml 的默认阈值一致。
func testConfig() config.Config {
	return config.Config{
		ZoneId:              1,
		LeaseTTL:            60,
		MaxContentBytes:     512,
		RateLimitPerSecond:  5,
		HistoryMaxEntries:   200,
		HistoryTTLSeconds:   604800,
		HistoryDefaultLimit: 20,
		HistoryMaxLimit:     50,
		RequestIdTTLSeconds: 60,
	}
}

// newTestSvcCtx:ChatRedis / SharedRedis 指向同一个 miniredis = 本地单库回落形态。
func newTestSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	return &svc.ServiceContext{Config: testConfig(), ChatRedis: rds, SharedRedis: rds}, mr
}

func playerCtx(playerId uint64) context.Context {
	return session.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: playerId})
}

// sendAs 以 sender 的会话发一条,返回 tip 码(0 = 成功)。
func sendAs(t *testing.T, svcCtx *svc.ServiceContext, sender uint64, channel chatpb.ChatChannelType,
	target uint64, content, requestId string) uint32 {
	t.Helper()
	resp, err := NewChatLogic(playerCtx(sender), svcCtx).SendChat(&chatpb.SendChatRequest{
		Message:   &chatpb.ChatMessage{TargetPlayerId: target, Channel: channel, Content: content},
		RequestId: requestId,
	})
	require.NoError(t, err, "业务失败必须 in-band,不许走 gRPC error")
	require.NotNil(t, resp)
	return resp.GetErrorMessage().GetId()
}

// pullAs 以 self 的会话拉历史,返回 tip 码与消息。
func pullAs(t *testing.T, svcCtx *svc.ServiceContext, self uint64, channel chatpb.ChatChannelType,
	peer uint64, limit uint32) (uint32, []*chatpb.ChatMessage) {
	t.Helper()
	resp, err := NewChatLogic(playerCtx(self), svcCtx).PullChatHistory(&chatpb.PullChatHistoryRequest{
		Channel: channel, PeerPlayerId: peer, Limit: limit,
	})
	require.NoError(t, err, "业务失败必须 in-band,不许走 gRPC error")
	require.NotNil(t, resp)
	return resp.GetErrorMessage().GetId(), resp.GetMessages()
}

// 无会话(或会话 player_id=0)一律 kInvalidParameter,且不碰存储。
func TestNoSessionRejected(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	req := &chatpb.SendChatRequest{
		Message: &chatpb.ChatMessage{SenderPlayerId: playerA, Channel: world, Content: "hi"},
	}

	for name, ctx := range map[string]context.Context{
		"缺会话":            context.Background(),
		"会话 player_id=0": session.WithSessionDetails(context.Background(), &base.SessionDetails{}),
	} {
		t.Run(name, func(t *testing.T) {
			sendResp, err := NewChatLogic(ctx, svcCtx).SendChat(req)
			require.NoError(t, err)
			require.Equal(t, constants.ErrInvalidParameter, sendResp.GetErrorMessage().GetId())

			pullResp, err := NewChatLogic(ctx, svcCtx).PullChatHistory(&chatpb.PullChatHistoryRequest{Channel: world})
			require.NoError(t, err)
			require.Equal(t, constants.ErrInvalidParameter, pullResp.GetErrorMessage().GetId())
		})
	}
	require.False(t, mr.Exists(worldLogKey), "无会话请求不许写历史")
}

// WORLD:发 → 别人拉得到;sender 被会话覆盖、target 清零、内容 trim、服务端盖时间、LIST 带 7 天 TTL。
func TestWorldSendThenPullVisibleWithSenderOverridden(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	resp, err := NewChatLogic(playerCtx(playerA), svcCtx).SendChat(&chatpb.SendChatRequest{
		Message: &chatpb.ChatMessage{
			SenderPlayerId: 9999, // 伪造的发言人,必须被会话覆盖
			TargetPlayerId: 7,
			Channel:        world,
			Content:        "  world hello  ",
			SendTimeMs:     1, // 伪造的时间,必须被服务端覆盖
		},
		RequestId: "w-1",
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())

	code, msgs := pullAs(t, svcCtx, playerB, world, 0, 20)
	require.Zero(t, code)
	require.Len(t, msgs, 1)
	got := msgs[0]
	require.Equal(t, playerA, got.GetSenderPlayerId())
	require.Zero(t, got.GetTargetPlayerId())
	require.Equal(t, world, got.GetChannel())
	require.Equal(t, "world hello", got.GetContent())
	require.Greater(t, got.GetSendTimeMs(), int64(1))

	require.Equal(t, 604800*time.Second, mr.TTL(worldLogKey))
}

// PRIVATE:A→B 与 B→A 写同一把排序 key,双方都能拉到全部两条;第三者拉不到。
func TestPrivateBothDirectionsShareOneKey(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	require.Zero(t, sendAs(t, svcCtx, playerA, private, playerB, "a to b", ""))
	require.Zero(t, sendAs(t, svcCtx, playerB, private, playerA, "b to a", ""))
	require.True(t, mr.Exists("chat:{p:1001:1002}:log"))
	require.False(t, mr.Exists("chat:{p:1002:1001}:log"))

	for _, viewer := range []struct{ self, peer uint64 }{{playerA, playerB}, {playerB, playerA}} {
		code, msgs := pullAs(t, svcCtx, viewer.self, private, viewer.peer, 0)
		require.Zero(t, code)
		require.Len(t, msgs, 2, "player %d 应看到私聊双方的两条", viewer.self)
		for _, m := range msgs {
			require.Equal(t, private, m.GetChannel())
		}
	}

	// C 拿 A 当 peer 拉到的是 C↔A 的会话(空),看不到 A↔B。
	code, msgs := pullAs(t, svcCtx, playerC, private, playerA, 0)
	require.Zero(t, code)
	require.Empty(t, msgs)

	// 缺对端 / 对端是自己。
	require.Equal(t, constants.ErrInvalidParameter, sendAs(t, svcCtx, playerA, private, 0, "x", ""))
	require.Equal(t, constants.ErrInvalidParameter, sendAs(t, svcCtx, playerA, private, playerA, "x", ""))
	code, _ = pullAs(t, svcCtx, playerA, private, 0, 0)
	require.Equal(t, constants.ErrInvalidParameter, code)
}

// 超长(按字节)/ 空 / 纯空白 → kMessageSizeExceeded;恰好 512 字节放行。
func TestContentLengthLimits(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	require.Equal(t, constants.ErrMessageTooLong, sendAs(t, svcCtx, playerA, world, 0, strings.Repeat("a", 513), ""))
	// 171 个汉字 = 513 字节:限的是字节不是字符。
	require.Equal(t, constants.ErrMessageTooLong, sendAs(t, svcCtx, playerA, world, 0, strings.Repeat("汉", 171), ""))
	require.Equal(t, constants.ErrMessageTooLong, sendAs(t, svcCtx, playerA, world, 0, "", ""))
	require.Equal(t, constants.ErrMessageTooLong, sendAs(t, svcCtx, playerA, world, 0, " \t\n ", ""))
	require.False(t, mr.Exists(worldLogKey), "被拒的内容不许写历史")
	require.False(t, mr.Exists(rateLimitKey(playerA)), "纯校验失败不许消耗限速额度")

	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, strings.Repeat("a", 512), ""))
}

// 限速:同一秒第 6 条拒;限速按玩家隔离;窗口过去后同 request_id 可重试(被限速时幂等键已释放)。
func TestRateLimitSixthRejected(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	for i := 1; i <= 5; i++ {
		require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, fmt.Sprintf("m%d", i), fmt.Sprintf("rl-%d", i)), "第 %d 条应放行", i)
	}
	require.Equal(t, constants.ErrRateLimited, sendAs(t, svcCtx, playerA, world, 0, "m6", "rl-6"))

	require.Zero(t, sendAs(t, svcCtx, playerB, world, 0, "b1", ""), "限速是每玩家的")

	mr.FastForward(time.Second)
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, "m6", "rl-6"),
		"被限速的那次没写入,幂等键必须已释放,同 request_id 重试应成功")

	_, msgs := pullAs(t, svcCtx, playerC, world, 0, 50)
	require.Len(t, msgs, 7)
}

// 幂等:同 request_id 重发只落一条;键带 pid,不同玩家同 id 互不影响;超长 request_id 拒。
func TestSameRequestIdStoredOnce(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, "dup", "req-1"))
	got, err := mr.Get("chat:{req:1001}:req-1")
	require.NoError(t, err)
	require.Equal(t, claimDone, got, "写入成功后幂等键必须是 done,重发才按成功回")
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, "dup", "req-1"), "首发已写入的重复请求按成功回")
	_, msgs := pullAs(t, svcCtx, playerB, world, 0, 20)
	require.Len(t, msgs, 1)
	require.Equal(t, 60*time.Second, mr.TTL("chat:{req:1001}:req-1"))

	require.Zero(t, sendAs(t, svcCtx, playerB, world, 0, "dup", "req-1"))
	_, msgs = pullAs(t, svcCtx, playerC, world, 0, 20)
	require.Len(t, msgs, 2)

	require.Equal(t, constants.ErrInvalidParameter,
		sendAs(t, svcCtx, playerA, world, 0, "x", strings.Repeat("r", maxRequestIdBytes+1)))
}

// 幂等两态:首发还在途(键为 pending)时同 request_id 重发不许回成功 —— 否则首发随后失败释放键,
// 客户端以为发出去了、消息却没落库。回 kRateLimitExceeded(业务拒绝)且不写历史;首发释放键后重试能真正写入。
func TestSameRequestIdWhileFirstInFlightNotReportedAsSuccess(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	// 模拟首发已占键、尚未写完(或首发进程崩在半路)。
	claimKey := requestIdKey(playerA, "req-inflight")
	require.NoError(t, mr.Set(claimKey, claimPending))
	mr.SetTTL(claimKey, 60*time.Second)

	require.Equal(t, constants.ErrRateLimited, sendAs(t, svcCtx, playerA, world, 0, "inflight", "req-inflight"),
		"首发未完成时重发必须回限速码,不能回成功")
	require.False(t, mr.Exists(worldLogKey), "在途重发不许写历史")
	got, err := mr.Get(claimKey)
	require.NoError(t, err)
	require.Equal(t, claimPending, got, "在途重发不许改动首发的幂等键")

	// 首发失败 → 释放键;客户端用同一 request_id 重试,这次真正写入并置 done。
	mr.Del(claimKey)
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, "inflight", "req-inflight"))
	_, msgs := pullAs(t, svcCtx, playerB, world, 0, 20)
	require.Len(t, msgs, 1)
	got, err = mr.Get(claimKey)
	require.NoError(t, err)
	require.Equal(t, claimDone, got)
}

// pausedChatRequest 只标记待交错的请求,同一连接池里的其它请求仍能正常前进。
type pausedChatRequest struct{}

type requestIOPause struct {
	entered        chan struct{}
	resume         chan struct{}
	once           sync.Once
	resumeOnce     sync.Once
	scriptContains string
	fail           bool
}

func newRequestIOPause(t *testing.T, scriptContains string, fail bool) *requestIOPause {
	t.Helper()
	p := &requestIOPause{entered: make(chan struct{}), resume: make(chan struct{}), scriptContains: scriptContains, fail: fail}
	t.Cleanup(p.release)
	return p
}

func (p *requestIOPause) release() { p.resumeOnce.Do(func() { close(p.resume) }) }

func (p *requestIOPause) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("请求没有到达指定 I/O 暂停点")
	}
}

// claimExpiryHook 在 Redis 命令送出前暂停指定请求。测试通过 FastForward 推进键 TTL,
// 再完成新请求后恢复旧请求,不依赖 sleep 或真实网络超时。
type claimExpiryHook map[string]*requestIOPause

func (h claimExpiryHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h claimExpiryHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h claimExpiryHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		name, _ := ctx.Value(pausedChatRequest{}).(string)
		pause := h[name]
		if pause == nil || cmd.Name() != "eval" || len(cmd.Args()) < 2 {
			return next(ctx, cmd)
		}
		script, _ := cmd.Args()[1].(string)
		if !strings.Contains(script, pause.scriptContains) {
			return next(ctx, cmd)
		}
		paused := false
		pause.once.Do(func() {
			paused = true
			close(pause.entered)
			<-pause.resume
		})
		if paused && pause.fail {
			return errors.New("单测注入:旧请求占键后的存储失败")
		}
		return next(ctx, cmd)
	}
}

type chatSendResult struct {
	resp *chatpb.SendChatResponse
	err  error
}

func startPausedSend(svcCtx *svc.ServiceContext, name string, req *chatpb.SendChatRequest) <-chan chatSendResult {
	done := make(chan chatSendResult, 1)
	go func() {
		ctx := context.WithValue(playerCtx(playerA), pausedChatRequest{}, name)
		resp, err := NewChatLogic(ctx, svcCtx).SendChat(req)
		done <- chatSendResult{resp: resp, err: err}
	}()
	return done
}

func awaitSend(t *testing.T, done <-chan chatSendResult) uint32 {
	t.Helper()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.NotNil(t, result.resp)
		return result.resp.GetErrorMessage().GetId()
	case <-time.After(5 * time.Second):
		t.Fatal("恢复后的请求没有结束")
		return 0
	}
}

func newClaimExpirySvc(t *testing.T, hook claimExpiryHook) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"}, redis.WithHook(hook))
	return &svc.ServiceContext{Config: testConfig(), ChatRedis: rds, SharedRedis: rds}, mr
}

func TestExpiredRequestFailureCannotReleaseSuccessfulRetry(t *testing.T) {
	pause := newRequestIOPause(t, `redis.call("INCR"`, true)
	svcCtx, mr := newClaimExpirySvc(t, claimExpiryHook{"old": pause})
	req := &chatpb.SendChatRequest{
		Message: &chatpb.ChatMessage{Channel: world, Content: "只允许写入一次"}, RequestId: "expired-release",
	}
	old := startPausedSend(svcCtx, "old", req)
	pause.wait(t)

	mr.FastForward(time.Duration(svcCtx.Config.RequestIdTTLSeconds) * time.Second)
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, req.Message.Content, req.RequestId))
	pause.release()
	require.Equal(t, constants.ErrStorage, awaitSend(t, old))

	// 新请求的 TTL 仍有效,旧请求的迟到清理不得导致下一次重试重新落库。
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, req.Message.Content, req.RequestId))
	code, messages := pullAs(t, svcCtx, playerB, world, 0, 50)
	require.Zero(t, code)
	require.Len(t, messages, 1, "旧 owner 失败不得删除新请求已经完成的幂等状态")
}

func TestExpiredRequestSuccessCannotCompleteNewPendingRequest(t *testing.T) {
	oldPause := newRequestIOPause(t, `redis.call("GET"`, false)
	newPause := newRequestIOPause(t, `redis.call("INCR"`, false)
	svcCtx, mr := newClaimExpirySvc(t, claimExpiryHook{"old": oldPause, "new": newPause})
	req := &chatpb.SendChatRequest{
		Message: &chatpb.ChatMessage{Channel: world, Content: "完结只认占用者"}, RequestId: "expired-complete",
	}
	old := startPausedSend(svcCtx, "old", req)
	oldPause.wait(t) // 旧请求已写入,尚未将自己的 pending 改为 done。
	mr.FastForward(time.Duration(svcCtx.Config.RequestIdTTLSeconds) * time.Second)
	newer := startPausedSend(svcCtx, "new", req)
	newPause.wait(t) // 新请求已经占到过期后重新创建的 pending。

	oldPause.release()
	require.Zero(t, awaitSend(t, old))
	require.Equal(t, constants.ErrRateLimited,
		sendAs(t, svcCtx, playerA, world, 0, req.Message.Content, req.RequestId),
		"旧 owner 成功不得把新 owner 的在途状态提前改成 done")

	newPause.release()
	require.Zero(t, awaitSend(t, newer))
	require.Zero(t, sendAs(t, svcCtx, playerA, world, 0, req.Message.Content, req.RequestId))
}

// TEAM / SYSTEM / UNSPECIFIED → kFeatureUnavailable;未知枚举值 → kInvalidParameter;均不碰存储。
func TestNonOpenChannelsRejected(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)

	for _, channel := range []chatpb.ChatChannelType{
		chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM,
		chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM,
		chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED,
	} {
		require.Equal(t, constants.ErrChannelUnavailable, sendAs(t, svcCtx, playerA, channel, playerB, "x", ""), channel.String())
		code, _ := pullAs(t, svcCtx, playerA, channel, playerB, 0)
		require.Equal(t, constants.ErrChannelUnavailable, code, channel.String())
	}
	require.Equal(t, constants.ErrInvalidParameter, sendAs(t, svcCtx, playerA, chatpb.ChatChannelType(99), 0, "x", ""))
	require.False(t, mr.Exists(rateLimitKey(playerA)), "频道被拒不许消耗限速额度")
}

// limit:0 → 默认 20;超上限 → 钳到 50;中间值原样。
func TestPullLimitClamped(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)

	// 12 个发送者各 5 条 = 60 条,每人恰好用满限速,不需要拨时钟。
	for sender := uint64(0); sender < 12; sender++ {
		for i := 0; i < 5; i++ {
			require.Zero(t, sendAs(t, svcCtx, 2000+sender, world, 0, fmt.Sprintf("m-%d-%d", sender, i), ""))
		}
	}

	_, msgs := pullAs(t, svcCtx, playerA, world, 0, 1000)
	require.Len(t, msgs, 50)
	_, msgs = pullAs(t, svcCtx, playerA, world, 0, 0)
	require.Len(t, msgs, 20)
	_, msgs = pullAs(t, svcCtx, playerA, world, 0, 7)
	require.Len(t, msgs, 7)
}

// 排序以 send_time_ms 为准(新在前),而不是 LIST 写入顺序(多实例时钟偏差时二者不一致)。
func TestPullSortedBySendTimeDesc(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)

	for _, ms := range []int64{1000, 3000, 2000} {
		l := NewChatLogic(playerCtx(playerA), svcCtx)
		l.now = func() time.Time { return time.UnixMilli(ms) }
		resp, err := l.SendChat(&chatpb.SendChatRequest{
			Message: &chatpb.ChatMessage{Channel: world, Content: fmt.Sprintf("t%d", ms)},
		})
		require.NoError(t, err)
		require.Zero(t, resp.GetErrorMessage().GetId())
	}

	_, msgs := pullAs(t, svcCtx, playerB, world, 0, 0)
	require.Len(t, msgs, 3)
	got := []int64{msgs[0].GetSendTimeMs(), msgs[1].GetSendTimeMs(), msgs[2].GetSendTimeMs()}
	require.Equal(t, []int64{3000, 2000, 1000}, got)
}

// Redis 故障 → kServiceUnavailable(fault 码),仍然 in-band。
func TestRedisFailureMapsToServiceUnavailable(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	svcCtx := &svc.ServiceContext{Config: testConfig(), ChatRedis: rds, SharedRedis: rds}
	mr.Close()

	require.Equal(t, constants.ErrStorage, sendAs(t, svcCtx, playerA, world, 0, "x", "req-down"))
	require.Equal(t, constants.ErrStorage, sendAs(t, svcCtx, playerA, world, 0, "x", ""))
	code, _ := pullAs(t, svcCtx, playerA, world, 0, 0)
	require.Equal(t, constants.ErrStorage, code)
}
