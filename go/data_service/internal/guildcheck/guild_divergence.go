// Package guildcheck 是 data_service 回档闸问 guild 的只读适配器
// (docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.1、§7.5.3)。
//
// 它只回答一个问题:"这些玩家自各自快照时刻以来,帮会侧有没有已经终结为已应用的资产操作?"
// 为此做四件事:按 100 人分块、每块翻完所有页、保留期拒绝时钳位重查一次、按逐玩家 since 过滤。
// 裁决(拒绝 / 放行 / 日志 / 审计 / 指标)不在这里,在 internal/logic/rollback_logic.go。
//
// 为什么单独成包,而不是 07 写的 internal/logic/guild_divergence.go:svc.ServiceContext 要持有并装配
// 这个接缝(nil = 回档一律拒绝),而 logic import svc —— 接口与实现放在 logic 会成环。照 noderegistry
// 的先例单独成包,svc(装配)与 logic(裁决)都只依赖它。
package guildcheck

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	guildpb "proto/guild"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// MaxPlayersPerCall:一次 ListAppliedAssetOpsSince 最多带多少个 player_id。
	// = guild 侧上限 data.MaxAppliedOpsPlayerIDs(go/guild/internal/data,07 §7.4.2 沿用 X-14 的 IN 占位符上限);
	// 两个服务是不同 module,只能各写一份,改一边必须同改另一边。
	MaxPlayersPerCall = 100

	// PageLimit:单页行数,= guild 的上限 data.MaxAppliedOpsPageLimit。显式传而不是传 0 走 guild 的默认值:
	// guild 哪天改了默认值,这里的翻页行为不会悄悄跟着变。
	PageLimit uint32 = 500

	// MaxDivergenceRows:过滤后分歧行的上限(07 §7.5.4)。超过即 ErrTooManyDivergences,调用方按
	// CheckFailed 拒绝、放行也不管用。理由:上万行 ERROR 日志既进不全 Loki,也没人能照单补偿;
	// 缩小范围分批回档。先例:BatchRecallItems 超上限回 ErrCodeResultTruncated、零变更。
	MaxDivergenceRows = 10000

	// retentionRejectedMessagePrefix 是 guild 保留期拒绝(FailedPrecondition)的 message 前缀,后接十进制 cutoff_ms。
	// **跨服务契约**:权威定义在 go/guild/internal/server/guild_internal_server.go 的 RetentionRejectedMessagePrefix
	// (不同 module 的 internal 包,import 不了,只能各持一份)。两边的测试钉同一条样例串
	// "since_ms older than terminal retention; cutoff_ms=1697411600000"(guild G8 / 本包 D15)。
	// 格式漂移的方向是"解析失败 → 调用方 CheckFailed 拒绝",不会放过任何一次回档。
	retentionRejectedMessagePrefix = "since_ms older than terminal retention; cutoff_ms="
)

// ErrTooManyDivergences:过滤后的分歧行超过 MaxDivergenceRows。调用方据此把指标记成 truncated。
var ErrTooManyDivergences = errors.New("guild divergence rows exceed MaxDivergenceRows; narrow the rollback scope")

// GuildDivergence 是一条"快照之后终结为已应用"的帮会资产操作摘要(guildpb.GuildAssetOpBrief 去掉 stream)。
// Kind / Status 是 guild_db.proto 里 GuildAssetOpKind / GuildAssetOpStatus 的数值。
type GuildDivergence struct {
	OpID              uint64
	PlayerID          uint64
	GuildID           uint64
	Kind              uint32
	Status            uint32
	FundsDelta        uint64
	ContributionDelta uint64
	UpdatedMs         uint64
}

// GuildCheckResult 是一次完整检查(全部块、全部页)的结果。
type GuildCheckResult struct {
	// Divergences 已按逐玩家 since 过滤(保留 updated_ms > since[player_id] 的行),按 op_id 升序。
	Divergences []GuildDivergence
	// UnprovablePlayerIDs:since 早于 guild 终态流水保留期下界的玩家(升序)。对他们只查得到下界之后的行
	// (照样在 Divergences 里),更早的那段已被清理、无法证明。它**不是** error:调用方按"可放行的拒绝"处理(07 R2b)。
	UnprovablePlayerIDs []uint64
	// RetentionCutoffMs:guild 答复的保留期下界(多块时取最大值,即最晚的那个)。仅当 UnprovablePlayerIDs 非空时有意义;日志用。
	RetentionCutoffMs uint64
}

// GuildDivergenceChecker 是回档闸与 guild 之间的接缝(07 §7.5.1)。
//
// 契约:
//   - sinceMsByPlayer:player_id → since_ms(毫秒,> 0),键不得为 0。空 map 返回空结果,不发任何 RPC。
//   - 查不成必须返回 error —— 不可达、超时、ctx 预算耗尽、Unimplemented、任一页失败、保留期拒绝但解析不出下界、
//     钳位重查仍被拒、guild 答复自相矛盾(越界的 player_id、游标不前进)、过滤后超过 MaxDivergenceRows。
//     调用方据此拒绝回档且**不可放行**:不存在"问不到就当没有"。
//   - 实现自己翻完所有页、所有块;块与块串行(回档是运维操作,并发只会把 guild 的连接池打满)。
//   - 不重试:任何一次调用失败即整体失败,运维重发即可(07 §7.5.3-3)。
type GuildDivergenceChecker interface {
	ListDivergences(ctx context.Context, sinceMsByPlayer map[uint64]uint64) (GuildCheckResult, error)
}

// New 包一层 guild 内部 RPC 客户端。client 为 nil 返回 nil 接口(调用方据此拒绝回档)。
// 生产里 client 是 zrpc 连接上的 guildpb.GuildInternalClient;单次调用的超时由那条连接的 zrpc 超时拦截器给
// (GuildInternalRpc.Timeout),总预算由调用方的 ctx 给。
func New(client guildpb.GuildInternalClient) GuildDivergenceChecker {
	if client == nil {
		return nil
	}
	return &rpcChecker{client: client}
}

type rpcChecker struct {
	client guildpb.GuildInternalClient
}

func (c *rpcChecker) ListDivergences(ctx context.Context, sinceMsByPlayer map[uint64]uint64) (GuildCheckResult, error) {
	ids := make([]uint64, 0, len(sinceMsByPlayer))
	for id, since := range sinceMsByPlayer {
		if id == 0 {
			return GuildCheckResult{}, errors.New("guild check: player_id 0 in since map")
		}
		if since == 0 {
			// guild 把 since_ms == 0 当漏填拒掉;在这里就拦住,免得一个调用方 bug 表现成"guild 不可达"。
			return GuildCheckResult{}, fmt.Errorf("guild check: since_ms is 0 for player %d", id)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)

	var res GuildCheckResult
	for start := 0; start < len(ids); start += MaxPlayersPerCall {
		chunk := ids[start:min(start+MaxPlayersPerCall, len(ids))]
		if err := c.checkChunk(ctx, chunk, sinceMsByPlayer, &res); err != nil {
			return GuildCheckResult{}, err
		}
	}
	// 块按 player_id 切,各块内 op_id 升序,合并后重排一次。块之间玩家不重叠,一个 op 只属于一个玩家,不会重行。
	slices.SortFunc(res.Divergences, func(a, b GuildDivergence) int { return cmp.Compare(a.OpID, b.OpID) })
	return res, nil
}

// checkChunk 查一块(≤ MaxPlayersPerCall 个升序 id)的全部页,过滤后追加进 res。
//
// 该块的 since_ms = 块内最小的逐玩家 since(07 §7.2);返回后再按各自的 since 过滤,免得一个快照很老的玩家
// 把同块别人一个月内的操作全拉进分歧清单。
//
// 保留期钳位(07 §7.5.3-2b):首页收到 FailedPrecondition → 解析 cutoff_ms → 块内 since < cutoff 的玩家记为
// 不可证明 → 用 since_ms = cutoff 重查该块一次。这不是重试:换了参数,每块至多一次;重查再被拒即 error。
func (c *rpcChecker) checkChunk(ctx context.Context, ids []uint64, since map[uint64]uint64, res *GuildCheckResult) error {
	chunkSince := since[ids[0]]
	for _, id := range ids[1:] {
		chunkSince = min(chunkSince, since[id])
	}

	var after uint64
	clamped := false
	for {
		// 预算先查:ctx 已到点就不再发下一次调用,也让错误明确指向"预算耗尽"而不是某个 RPC 的超时。
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("guild check budget exhausted (players=%d since_ms=%d after_op_id=%d): %w",
				len(ids), chunkSince, after, err)
		}
		resp, err := c.client.ListAppliedAssetOpsSince(ctx, &guildpb.ListAppliedAssetOpsSinceRequest{
			ZoneId:    0, // 恒 0:按 guild.zone_id 收窄在合服 / 解散后会静默丢行(07 §7.4.3)
			PlayerIds: ids,
			SinceMs:   chunkSince,
			AfterOpId: after,
			Limit:     PageLimit,
		})
		if err != nil {
			st := status.Convert(err)
			if st.Code() == codes.FailedPrecondition && after == 0 && !clamped {
				cutoff, perr := ParseRetentionCutoffMs(st.Message())
				if perr != nil {
					return fmt.Errorf("guild retention rejection is unparsable (message=%q): %w", st.Message(), perr)
				}
				if cutoff <= chunkSince {
					// guild 说"早于保留期",给的下界却不晚于我们的 since:答复自相矛盾,按查不成处理。
					return fmt.Errorf("guild retention rejection is inconsistent: cutoff_ms=%d not after chunk since_ms=%d", cutoff, chunkSince)
				}
				for _, id := range ids {
					if since[id] < cutoff {
						res.UnprovablePlayerIDs = append(res.UnprovablePlayerIDs, id)
					}
				}
				res.RetentionCutoffMs = max(res.RetentionCutoffMs, cutoff)
				chunkSince = cutoff
				clamped = true
				continue
			}
			return fmt.Errorf("guild ListAppliedAssetOpsSince failed (players=%d since_ms=%d after_op_id=%d clamped=%t) code=%s: %w",
				len(ids), chunkSince, after, clamped, st.Code(), err)
		}

		last := after
		for _, op := range resp.GetOps() {
			pid := op.GetPlayerId()
			if _, found := slices.BinarySearch(ids, pid); !found {
				return fmt.Errorf("guild returned op_id=%d for player %d outside the requested chunk", op.GetOpId(), pid)
			}
			if op.GetOpId() <= last {
				return fmt.Errorf("guild returned op_id=%d not strictly after %d (rows must be op_id ascending past the cursor)", op.GetOpId(), last)
			}
			last = op.GetOpId()
			// 终态行 updated_ms ≥ next_attempt_ms(Finalize 同写 now,此后不再改),按 updated_ms 过滤只会多留、不会少留。
			if op.GetUpdatedMs() <= since[pid] {
				continue
			}
			res.Divergences = append(res.Divergences, GuildDivergence{
				OpID:              op.GetOpId(),
				PlayerID:          pid,
				GuildID:           op.GetGuildId(),
				Kind:              op.GetKind(),
				Status:            op.GetStatus(),
				FundsDelta:        op.GetFundsDelta(),
				ContributionDelta: op.GetContributionDelta(),
				UpdatedMs:         op.GetUpdatedMs(),
			})
			if len(res.Divergences) > MaxDivergenceRows {
				return fmt.Errorf("%w (more than %d rows)", ErrTooManyDivergences, MaxDivergenceRows)
			}
		}

		next := resp.GetNextAfterOpId()
		if next == 0 {
			return nil
		}
		if next <= after {
			// 游标不前进 = 死循环;guild 的 bug 不能让回档 RPC 挂到预算耗尽才失败。
			return fmt.Errorf("guild cursor did not advance: after_op_id=%d next_after_op_id=%d", after, next)
		}
		after = next
	}
}

// ParseRetentionCutoffMs 从 guild 保留期拒绝的 status message 里取出 cutoff_ms(本次可接受的最小 since_ms)。
// 格式必须逐字是 retentionRejectedMessagePrefix + 十进制正整数,前后不容任何多余字符;否则返回 error
// (调用方按 CheckFailed 拒绝:格式漂移的方向仍是拒绝)。
func ParseRetentionCutoffMs(message string) (uint64, error) {
	digits, ok := strings.CutPrefix(message, retentionRejectedMessagePrefix)
	if !ok {
		return 0, fmt.Errorf("message does not start with %q", retentionRejectedMessagePrefix)
	}
	cutoff, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cutoff_ms %q is not a decimal uint64: %w", digits, err)
	}
	if cutoff == 0 {
		return 0, errors.New("cutoff_ms is 0")
	}
	return cutoff, nil
}
