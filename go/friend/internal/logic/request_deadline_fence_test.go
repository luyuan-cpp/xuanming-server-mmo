package logic

// 请求截止之后不再提交(docs/design/client-request-ordering.md §6.1 / O11 / §14 第 4 条)的逻辑层回归。
//
// gate 对同一会话的路由类请求单飞:路由服在 ForwardTimeoutMs 回超时,gate 就放行下一条。friend 的 handler
// 若在截止之后仍把写发出去,这条写就会晚于下一条请求落地,例如「同意好友 → 删好友」执行成「删好友 → 同意好友」,
// 两条都回成功、最终仍是好友(§1.3)。
//
// data 包的 request_deadline_fence_test.go 只证明 repo 收到已截止的 ctx 时不发语句;它证明不了 logic 真的把
// 请求 ctx 交给了 repo。这里用**真** data.FriendRepo 装配:MySQL 换成只数「有没有人来取连接」的 connector,
// FriendRedis / PlayerRedis 是 miniredis,摘要 bump 是记录型假 Evaler。logic → repo → database/sql 这条链上
// 任何一处改用 context.Background() / WithoutCancel,截止之后就会有人来取连接或写 Redis,用例判红。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"friend/internal/constants"
	"friend/internal/data"

	pb "proto/friend"
)

var errDeadlineConnect = errors.New("截止栅栏:截止之后不该有人取 MySQL 连接")

// connectCountingConnector 只记「有没有人来取连接」。database/sql 发任何语句(含 BEGIN)之前都要先取连接,
// 池子初始是空的,所以 Connect / Open 一次都没被调用 ⟺ 一条语句都没发出。取连接一律报错:
// 截止之后本不该走到这里,真走到了要表现为失败,不能悄悄「成功」。
type connectCountingConnector struct{ attempts atomic.Int32 }

func (c *connectCountingConnector) Connect(context.Context) (driver.Conn, error) {
	c.attempts.Add(1)
	return nil, errDeadlineConnect
}

func (c *connectCountingConnector) Driver() driver.Driver { return c }

func (c *connectCountingConnector) Open(string) (driver.Conn, error) {
	c.attempts.Add(1)
	return nil, errDeadlineConnect
}

// 客户端请求路径上的全部写 RPC。ctx 在进入 handler 之前就已截止(go-zero 超时拦截器到期后 handler 手里的就是这种 ctx)。
func TestFriendWritesAfterRequestDeadlineReachNothing(t *testing.T) {
	const me, target, requestID uint64 = 7101, 7102, 4242
	cases := []struct {
		name string
		call func(ctx context.Context, l *FriendLogic) (uint32, error)
	}{
		{"AddFriend", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: target})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"AcceptFriend", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.AcceptFriend(ctx, &pb.AcceptFriendRequest{RequestId: requestID})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"RejectFriend", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.RejectFriend(ctx, &pb.RejectFriendRequest{RequestId: requestID})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"RemoveFriend", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.RemoveFriend(ctx, &pb.RemoveFriendRequest{TargetPlayerId: target})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"Block", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.Block(ctx, &pb.BlockRequest{TargetPlayerId: target})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"Unblock", func(ctx context.Context, l *FriendLogic) (uint32, error) {
			resp, err := l.Unblock(ctx, &pb.UnblockRequest{TargetPlayerId: target})
			return resp.GetErrorMessage().GetId(), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			digest := f.withSyncDigest()
			conns := &connectCountingConnector{}
			db := sql.OpenDB(conns)
			t.Cleanup(func() { _ = db.Close() })
			var allocated atomic.Int32
			f.deps.Repo = data.NewFriendRepo(f.svcCtx.FriendRedis, db, data.FriendRepoOptions{
				NextRequestID: func(context.Context) (uint64, error) {
					allocated.Add(1)
					return 1, nil
				},
				DefaultTTL:        time.Minute,
				ListReadHardLimit: f.svcCtx.Config.Friend.ListReadHardLimit,
			})
			ctx, cancel := context.WithCancel(playerCtx(me))
			cancel()

			code, err := tc.call(ctx, f.logic())

			require.NoError(t, err, "截止也必须 in-band")
			assert.Equal(t, constants.ErrStorage, code, "截止之后必须如实报故障,不能回成功")
			assert.Zero(t, conns.attempts.Load(), "截止之后不得取 MySQL 连接(一条语句都不发,含容量行补齐与 BEGIN)")
			assert.Zero(t, allocated.Load(), "截止之后不得向 data_service 领申请代次")
			assert.Empty(t, f.friendMr.Keys(), "截止之后不得消耗频控额度、不得失效缓存")
			assert.Empty(t, f.sharedMr.Keys(), "截止之后不得写共享库")
			assert.Empty(t, digest.calls, "截止之后不得 bump 落地摘要")
		})
	}
}
