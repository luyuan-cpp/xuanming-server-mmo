package logic

// expected_guild_id 栅栏的不连库单测(docs/design/client-request-ordering.md O14 / §6.4,批次 E2)。
//
// 这里钉两件与存储无关的事:哪些错误算「不在预期帮」(expectedGuildLost 的真值表),以及会话 / 归属区这些前置
// 拒绝发生在绑定帮会之前(不计数、不碰仓储 —— repo 传 nil,顺序错了当场 nil panic,与 guild_manage_logic_test.go 同一套路)。
// 「复核与写入在同一个事务里」「复核与写入之间换帮也不会写错帮」要真 InnoDB 才有意义,见 expected_guild_integration_test.go
// (build tag integration)。
//
// watchExpectedGuild 也给那个文件用:它没有 build tag,带 -tags integration 时同包一起编译。

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	"guild/internal/data"
	pb "proto/guild"
)

// expectedGuildCounts 记下 expectedGuildObserved 的每一次调用(method, outcome)。
// 加锁:端到端用例里被测 RPC 跑在后台协程上。
type expectedGuildCounts struct {
	mu     sync.Mutex
	counts map[[2]string]int
}

// watchExpectedGuild 把包级计数钩子换成记录器,用例结束还原。必须在起任何并发之前、在主协程里调用
// (与 data 包 watchGuildTxDeadlocks 同一惯例;本包用例不 t.Parallel,替换与读写不竞争)。
func watchExpectedGuild(t *testing.T) *expectedGuildCounts {
	t.Helper()
	c := &expectedGuildCounts{counts: map[[2]string]int{}}
	original := expectedGuildObserved
	expectedGuildObserved = func(method, outcome string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.counts[[2]string{method, outcome}]++
	}
	t.Cleanup(func() { expectedGuildObserved = original })
	return c
}

func (c *expectedGuildCounts) of(method, outcome string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[[2]string{method, outcome}]
}

func (c *expectedGuildCounts) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.counts {
		n += v
	}
	return n
}

// TestExpectedGuildLost:expected = 0 时恒为 false(旧路径的错误映射一个字节都不变);expected ≠ 0 时只有
// 「预检确认不在」「事务锁不到本人成员行」「帮会行已不存在」三种算不在预期帮,职位不足、目标不在、写冲突照旧映射。
func TestExpectedGuildLost(t *testing.T) {
	const expected uint64 = 5101
	cases := []struct {
		name     string
		expected uint64
		err      error
		want     bool
	}{
		{"旧客户端:事务说不是成员,仍走旧映射", 0, data.ErrNotGuildMember, false},
		{"旧客户端:帮会已解散,仍走旧映射", 0, data.ErrGuildGone, false},
		{"旧客户端:预检哨兵不会出现,出现也不截", 0, errExpectedGuildMismatch, false},
		{"没有错误", expected, nil, false},
		{"预检确认不在预期帮", expected, errExpectedGuildMismatch, true},
		{"事务锁不到本人成员行", expected, data.ErrNotGuildMember, true},
		{"预期帮会已不存在", expected, data.ErrGuildGone, true},
		{"包装过的哨兵也认", expected, fmt.Errorf("leave: %w", data.ErrNotGuildMember), true},
		{"职位不足:人在预期帮里", expected, data.ErrRankTooLow, false},
		{"目标不在帮:操作者在预期帮里", expected, data.ErrTargetNotMember, false},
		{"帮主不能退:操作者在预期帮里", expected, data.ErrLeaderCantLeave, false},
		{"写冲突:让客户端重试", expected, data.ErrWriteConflict, false},
		{"别区帮会:不是「不在预期帮」", expected, data.ErrGuildZoneMismatch, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, expectedGuildLost(tc.expected, tc.err))
		})
	}
}

// expectedGuildWrite 把带 expected_guild_id 的五个写 RPC 抹平成同一个形状(同 managementCall 的理由:逐个手写等于给漏掉一个入口留位置)。
type expectedGuildWrite struct {
	method string // expected_guild 指标的 method 取值
	run    func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error)
}

func expectedGuildWrites() []expectedGuildWrite {
	return []expectedGuildWrite{
		{expectedGuildMethodLeave, func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error) {
			resp, err := l.LeaveGuild(ctx, &pb.LeaveGuildRequest{ExpectedGuildId: expected})
			return resp.GetErrorMessage().GetId(), err
		}},
		{expectedGuildMethodDisband, func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error) {
			resp, err := l.DisbandGuild(ctx, &pb.DisbandGuildRequest{ExpectedGuildId: expected})
			return resp.GetErrorMessage().GetId(), err
		}},
		{expectedGuildMethodKick, func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error) {
			resp, err := l.KickGuildMember(ctx, &pb.KickGuildMemberRequest{TargetPlayerId: 7, ExpectedGuildId: expected})
			return resp.GetErrorMessage().GetId(), err
		}},
		{expectedGuildMethodTransfer, func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error) {
			resp, err := l.TransferGuildLeader(ctx, &pb.TransferGuildLeaderRequest{TargetPlayerId: 7, ExpectedGuildId: expected})
			return resp.GetErrorMessage().GetId(), err
		}},
		{expectedGuildMethodSetRole, func(l *GuildLogic, ctx context.Context, expected uint64) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{
				TargetPlayerId: 7, Role: constants.RoleOfficer, ExpectedGuildId: expected,
			})
			return resp.GetErrorMessage().GetId(), err
		}},
	}
}

// TestExpectedGuildBoundAfterClientPrechecks:归属区未知时,无论带不带 expected_guild_id,
// 都在绑定帮会之前就拒绝 —— 不计 absent / mismatch(那两个数衡量的是「走到绑定的请求」),也不碰仓储(repo 为 nil)。
func TestExpectedGuildBoundAfterClientPrechecks(t *testing.T) {
	for _, w := range expectedGuildWrites() {
		for _, expected := range []uint64{0, 5101} {
			t.Run(fmt.Sprintf("%s/expected=%d", w.method, expected), func(t *testing.T) {
				counts := watchExpectedGuild(t)

				unknownZone := NewGuildLogic(nil, nil, nil, &fakeHomeZones{zone: 0})
				id, err := w.run(unknownZone, clientCtx(42), expected)
				require.NoError(t, err)
				assert.Equal(t, constants.ErrHomeZoneUnknown, id)

				assert.Zero(t, counts.total(), "前置拒绝不进 expected_guild 计数")
			})
		}
	}
}
