package logic

// 帮会管理与审批的 logic 层单测(设计 docs/design/guild-phase2/02-management.md §21.4)。
//
// 本文件的全部用例都把 repo 传 nil:一旦某条前置的顺序被改错、提前走到了仓储调用,
// 就会当场 nil panic。用"误触即崩"代替"断言没调用过",是因为 *data.GuildRepo 是具体类型
// 而不是接口,没法塞一个记录调用的替身进去 —— 顺序正确是这里唯一能机械守住的东西。
//
// 与本包其它测试文件共用的替身:fakeHomeZones / clientCtx 在 client_zone_test.go,
// fakeFence 在 merge_fence_test.go。本文件只新建它们覆盖不到的那两个(见下)。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	"guild/internal/data"
	pb "proto/guild"
	plpb "proto/player_locator"
	tablepb "shared/generated/pb/table"
	"shared/kafkautil"
)

// ── 测试替身 ──────────────────────────────────────────────────

// perPlayerZones 是按玩家分别作答的归属 zone 替身。
//
// 为什么不复用 client_zone_test.go 的 fakeHomeZones:审批通过时 logic 会**并行**查
// 审批人与申请人的归属 zone(§12.2.1),两个 goroutine 同时进来。fakeHomeZones 只有单个
// zone 字段且 calls++ 无锁,-race 下必报数据竞争,而且也答不出"这两个人 zone 不同"。
type perPlayerZones struct {
	mu    sync.Mutex
	zones map[uint64]uint32
	errs  map[uint64]error
	calls map[uint64]int
}

func (f *perPlayerZones) HomeZone(_ context.Context, playerID uint64) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[uint64]int)
	}
	f.calls[playerID]++
	if err := f.errs[playerID]; err != nil {
		return 0, err
	}
	return f.zones[playerID], nil
}

// callCount 给用例读调用次数。必须同样加锁:被查的那个 goroutine 可能还在跑。
func (f *perPlayerZones) callCount(playerID uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[playerID]
}

// fakeGateCommandBuilder 只为满足 kafkautil.GateCommandBuilder 的类型约束存在:
// 用它构造出来的 notifier 在本文件里从不真正发消息(每个用例都缺至少一个依赖 → NoopNotifier)。
type fakeGateCommandBuilder struct{}

func (fakeGateCommandBuilder) BuildPushCommand(uint32, uint32, string, uint32, []byte) ([]byte, error) {
	return nil, nil
}

func (fakeGateCommandBuilder) BuildBroadcastCommand(uint32, []uint32, string, uint32, []byte) ([]byte, error) {
	return nil, nil
}

func (fakeGateCommandBuilder) BuildBroadcastToSceneCommand(uint32, uint64, string, uint32, []byte) ([]byte, error) {
	return nil, nil
}

func (fakeGateCommandBuilder) BuildBroadcastToAllCommand(uint32, string, uint32, []byte) ([]byte, error) {
	return nil, nil
}

// managementCall 把签名各不相同的 RPC 抹平成同一个形状,好让"会话""闸门"这类
// 公共前置用一张表覆盖全部入口 —— 逐个手写等于给漏掉一个入口留了位置。
//
// 返回值取 tip 的 id 而不是整个响应:全部响应都有 error_message 字段,而生成的 getter
// 对 nil 接收者安全,所以故障分支(resp == nil)也能走同一条路。
type managementCall struct {
	name string
	run  func(l *GuildLogic, ctx context.Context) (uint32, error)
}

// managementRPCs 是 B2s 新增的 8 个 RPC(协议里都没有 player_id 字段,只接受会话调用)。
func managementRPCs() []managementCall {
	return []managementCall{
		{"SetGuildMemberRole", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{TargetPlayerId: 7, Role: constants.RoleOfficer})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"KickGuildMember", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.KickGuildMember(ctx, &pb.KickGuildMemberRequest{TargetPlayerId: 7})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"TransferGuildLeader", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.TransferGuildLeader(ctx, &pb.TransferGuildLeaderRequest{TargetPlayerId: 7})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"ApplyJoinGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ApplyJoinGuild(ctx, &pb.ApplyJoinGuildRequest{GuildId: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"CancelGuildApplication", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.CancelGuildApplication(ctx, &pb.CancelGuildApplicationRequest{GuildId: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"ListMyGuildApplications", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ListMyGuildApplications(ctx, &pb.ListMyGuildApplicationsRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"ListGuildApplications", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ListGuildApplications(ctx, &pb.ListGuildApplicationsRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"ReviewGuildApplication", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ReviewGuildApplication(ctx, &pb.ReviewGuildApplicationRequest{ApplicantPlayerId: 7, Approve: false})
			return resp.GetErrorMessage().GetId(), err
		}},
	}
}

// fenceGuardedWrites 是所有要过合服闸门的写 RPC(§29 第 3 条:"所有写 RPC"取字面,
// 存量的 LeaveGuild / DisbandGuild / SetAnnouncement / CreateGuild 也在内)。
//
// 两个读 RPC(ListMyGuildApplications / ListGuildApplications)**刻意不在这张表里**:
// 合服窗口里读一眼自己申请了哪些帮会是安全的,拦掉只会让界面变成空白。
//
// 审批统一用 approve:false:通过分支会额外起一个协程去查申请人的 zone,
// 而本表的用例用的是单值替身 fakeHomeZones —— 那条路由 TestReviewApproveChecksApplicantZoneBeforeRepo 覆盖。
func fenceGuardedWrites() []managementCall {
	readOnly := map[string]bool{"ListMyGuildApplications": true, "ListGuildApplications": true}
	writes := make([]managementCall, 0, 10)
	for _, rpc := range managementRPCs() {
		if readOnly[rpc.name] {
			continue
		}
		writes = append(writes, rpc)
	}
	writes = append(writes,
		managementCall{"LeaveGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.LeaveGuild(ctx, &pb.LeaveGuildRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		managementCall{"DisbandGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.DisbandGuild(ctx, &pb.DisbandGuildRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		managementCall{"SetAnnouncement", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetAnnouncement(ctx, &pb.SetAnnouncementRequest{GuildId: 1, Announcement: "今晚八点集合"})
			return resp.GetErrorMessage().GetId(), err
		}},
		managementCall{"CreateGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.CreateGuild(ctx, &pb.CreateGuildRequest{Name: "青云门"})
			return resp.GetErrorMessage().GetId(), err
		}},
	)
	return writes
}

// ── §11.2 会话前置 ───────────────────────────────────────────

// TestManagementRPCsRequireClientSession 钉住 §29 第 5 条:管理 / 申请 RPC 的请求体里
// **没有** player_id,内部调用因此拿不出可信的操作者身份。放行只会让操作者变成 0 号玩家。
// GM / 运维代操作要另设带身份的内部 RPC,不走这几个口。
func TestManagementRPCsRequireClientSession(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, &fakeFence{}, &fakeHomeZones{zone: 2})

	for _, tc := range managementRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.run(l, context.Background())

			require.Error(t, err)
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
		})
	}
}

// ── §11.1 合服闸门 ───────────────────────────────────────────

// TestWriteRPCsRefusedWhileMerging:闸门命中时回 tip 而不是 gRPC 错误(§11.1),
// 且查的是**请求者归属 zone**(2),不是客户端选的任何值。
func TestWriteRPCsRefusedWhileMerging(t *testing.T) {
	for _, tc := range fenceGuardedWrites() {
		t.Run(tc.name, func(t *testing.T) {
			fence := &fakeFence{merging: true}
			l := NewGuildLogic(nil, nil, nil, fence, &fakeHomeZones{zone: 2})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err, "合服是可预期的运维窗口,回 gRPC 错误会让客户端进重连隔离")
			assert.Equal(t, constants.ErrZoneMerging, id)
			assert.Equal(t, uint32(2), fence.lastZone, "闸门查的必须是请求者的归属 zone")
		})
	}
}

// TestFenceUnreadableReturnsZoneMergingTip:查不到闸门状态 ≠ 没有闸门。
// 合服窗口里写出来的帮会数据会落在一个正在搬迁的 zone 上,那是要手工修的数据事故;
// 拒绝一次只是玩家重试。
func TestFenceUnreadableReturnsZoneMergingTip(t *testing.T) {
	for _, tc := range fenceGuardedWrites() {
		t.Run(tc.name, func(t *testing.T) {
			fence := &fakeFence{err: errors.New("dial tcp: connection refused")}
			l := NewGuildLogic(nil, nil, nil, fence, &fakeHomeZones{zone: 2})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			assert.Equal(t, constants.ErrZoneMerging, id)
		})
	}
}

// TestHomeZoneUnknownPrecedesFence:没有归属 zone 就不知道该查哪个区的闸门,
// 顺序反过来会对着一个猜出来的 zone 做判定。fence.calls == 0 是这条顺序的证明。
func TestHomeZoneUnknownPrecedesFence(t *testing.T) {
	for _, tc := range fenceGuardedWrites() {
		t.Run(tc.name, func(t *testing.T) {
			fence := &fakeFence{merging: true}
			l := NewGuildLogic(nil, nil, nil, fence, &fakeHomeZones{zone: 0})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			assert.Equal(t, constants.ErrHomeZoneUnknown, id)
			assert.Zero(t, fence.calls, "归属 zone 未知时不该产生一次闸门查询")
		})
	}
}

// ── §11.4 / §12 业务前置 ─────────────────────────────────────

// TestTargetValidationBeforeRepo:参数级的拒绝全部发生在碰库之前(repo 为 nil,走到就崩)。
// 这些不是"锦上添花的早退":target=0 / target=自己 一旦落进事务,锁序里就会出现
// 一把本不该加的行锁,而结果照样是拒绝。
func TestTargetValidationBeforeRepo(t *testing.T) {
	newLogic := func() *GuildLogic {
		return NewGuildLogic(nil, nil, nil, &fakeFence{}, &fakeHomeZones{zone: 2})
	}
	const actor = uint64(42)

	cases := []struct {
		name   string
		run    func(l *GuildLogic, ctx context.Context) (uint32, error)
		wantID uint32
	}{
		{"任免目标为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{TargetPlayerId: 0, Role: constants.RoleOfficer})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrTargetNotMember},
		{"任免目标是自己", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{TargetPlayerId: actor, Role: constants.RoleOfficer})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrCannotTargetSelf},
		// role=3 是帮主:放行等于开了"自己给自己升帮主"的后门,帮主只能经转让产生。
		{"任免成帮主", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{TargetPlayerId: 7, Role: constants.RoleLeader})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrNoPermission},
		// role=2 是持久化编码里刻意跳过的空号:透传进库会造出一个 Rank 判不出档位的成员。
		{"任免成空号职位", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.SetGuildMemberRole(ctx, &pb.SetGuildMemberRoleRequest{TargetPlayerId: 7, Role: 2})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrNoPermission},
		{"踢人目标为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.KickGuildMember(ctx, &pb.KickGuildMemberRequest{TargetPlayerId: 0})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrTargetNotMember},
		{"踢自己", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.KickGuildMember(ctx, &pb.KickGuildMemberRequest{TargetPlayerId: actor})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrCannotTargetSelf},
		{"转让目标为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.TransferGuildLeader(ctx, &pb.TransferGuildLeaderRequest{TargetPlayerId: 0})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrTargetNotMember},
		{"转让给自己", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.TransferGuildLeader(ctx, &pb.TransferGuildLeaderRequest{TargetPlayerId: actor})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrCannotTargetSelf},
		{"审批人为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ReviewGuildApplication(ctx, &pb.ReviewGuildApplicationRequest{ApplicantPlayerId: 0, Approve: true})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrApplicationNotFound},
		{"审批自己的申请", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ReviewGuildApplication(ctx, &pb.ReviewGuildApplicationRequest{ApplicantPlayerId: actor, Approve: true})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrCannotTargetSelf},
		{"申请的帮会 id 为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.ApplyJoinGuild(ctx, &pb.ApplyJoinGuildRequest{GuildId: 0})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrGuildNotFound},
		// 撤回没指定帮会 = 没有"哪一条申请"可言,与"这条申请已经不在了"同一答复。
		{"撤回的帮会 id 为 0", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.CancelGuildApplication(ctx, &pb.CancelGuildApplicationRequest{GuildId: 0})
			return resp.GetErrorMessage().GetId(), err
		}, constants.ErrApplicationNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.run(newLogic(), clientCtx(actor))

			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

// TestReviewApproveChecksApplicantZoneBeforeRepo 钉住 §29 第 8 条与 M1 的修法。
//
// 申请行是最长 72 小时前写的,这期间申请人可能被合服搬去了别的区。放进来就会造出一个
// "人在 A 区、帮在 B 区"的成员,而帮会的一切按 zone 隔离 —— 那要手工修数据。
// 所以归属 zone 查不到或查询报错一律不批(fail-closed),且这一步在事务之前。
func TestReviewApproveChecksApplicantZoneBeforeRepo(t *testing.T) {
	const reviewer, applicant = uint64(42), uint64(7)

	t.Run("申请人没有归属 zone 映射", func(t *testing.T) {
		zones := &perPlayerZones{zones: map[uint64]uint32{reviewer: 2}}
		l := NewGuildLogic(nil, nil, nil, &fakeFence{}, zones)

		resp, err := l.ReviewGuildApplication(clientCtx(reviewer),
			&pb.ReviewGuildApplicationRequest{ApplicantPlayerId: applicant, Approve: true})

		require.NoError(t, err)
		// 不单独发码:对审批者来说"这条申请已经不能批了"就是全部,分得更细只会泄露申请人状态。
		assert.Equal(t, constants.ErrApplicationNotFound, resp.GetErrorMessage().GetId())
		assert.Equal(t, 1, zones.callCount(applicant))
	})

	t.Run("查申请人 zone 报错是故障,不是放行", func(t *testing.T) {
		boom := errors.New("data_service unreachable")
		zones := &perPlayerZones{
			zones: map[uint64]uint32{reviewer: 2},
			errs:  map[uint64]error{applicant: boom},
		}
		l := NewGuildLogic(nil, nil, nil, &fakeFence{}, zones)

		resp, err := l.ReviewGuildApplication(clientCtx(reviewer),
			&pb.ReviewGuildApplicationRequest{ApplicantPlayerId: applicant, Approve: true})

		require.ErrorIs(t, err, boom)
		assert.Nil(t, resp)
	})

	t.Run("拒绝不查申请人 zone", func(t *testing.T) {
		zones := &perPlayerZones{zones: map[uint64]uint32{reviewer: 2}}
		l := NewGuildLogic(nil, nil, nil, &fakeFence{}, zones)

		// 删掉一条申请在任何 zone 下都是安全的,所以拒绝分支不该付这次 data_service 往返。
		// repo 为 nil,流程会一直走到 operatorGuild 才崩 —— 这个 panic 正是"确实走到了仓储"的证据。
		require.Panics(t, func() {
			_, _ = l.ReviewGuildApplication(clientCtx(reviewer),
				&pb.ReviewGuildApplicationRequest{ApplicantPlayerId: applicant, Approve: false})
		}, "拒绝分支应当一路走到仓储调用")

		assert.Zero(t, zones.callCount(applicant), "拒绝不该查申请人的归属 zone")
		assert.Equal(t, 1, zones.callCount(reviewer), "审批人自己的 zone 仍要查(闸门要用)")
	})
}

// TestWriteConflictMapsToBusyTip:死锁重试耗尽 / 锁等待超时回业务 tip,不回 gRPC 错误(D6)。
// 回 gRPC 错误会被 serverbase 记成服务端故障并让客户端进重连隔离,而两个人同时改同一个帮会
// 是正常玩法,玩家原地重试一次就能成功。
//
// 这一支刻意不碰 repo(映射表里只有它和几个前置分支不复核映射),所以 repo 传 nil 就是断言。
func TestWriteConflictMapsToBusyTip(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)

	tip, err := l.mapWriteErr(context.Background(), 42, 1, 1, data.ErrWriteConflict)

	require.NoError(t, err)
	require.NotNil(t, tip)
	assert.Equal(t, constants.ErrBusyRetry, tip.GetId())
}

// TestMapWriteErrMapping 把 §11.4 的整张映射表钉死。
//
// 这张表是仓储哨兵变成玩家可见文案的唯一出口:错一格没有任何征兆,
// 表现只是"某个操作偶尔提示得莫名其妙",线上极难发现。
// repo 传 nil 是安全的:会复核映射的几支都经 verifyMapping,而它对 nil repo 直接返回。
func TestMapWriteErrMapping(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	ctx := context.Background()

	tips := []struct {
		name   string
		err    error
		wantID uint32
	}{
		{"公会不存在", data.ErrGuildGone, constants.ErrGuildNotFound},
		// 别区的帮会与不存在的帮会同一答复,不向客户端透露"它在别的区"。
		{"公会在别的区", data.ErrGuildZoneMismatch, constants.ErrGuildNotFound},
		{"操作者不在帮", data.ErrNotGuildMember, constants.ErrNotInGuild},
		{"目标不在帮", data.ErrTargetNotMember, constants.ErrTargetNotMember},
		{"职位不足", data.ErrRankTooLow, constants.ErrRankTooLow},
		{"长老已满", data.ErrOfficerLimit, constants.ErrOfficerLimit},
		{"帮主不能退", data.ErrLeaderCantLeave, constants.ErrLeaderCantLeave},
		{"公会已满", data.ErrGuildFull, constants.ErrGuildFull},
		{"已在别的帮", data.ErrPlayerAlreadyInGuild, constants.ErrAlreadyInGuild},
		{"申请已失效", data.ErrApplicationNotFound, constants.ErrApplicationNotFound},
		{"本人申请数已满", data.ErrApplicationLimit, constants.ErrApplicationLimit},
		{"帮会待审队列已满", data.ErrApplicationQueueFull, constants.ErrApplicationQueueFull},
		{"写冲突", data.ErrWriteConflict, constants.ErrBusyRetry},
	}
	for _, tc := range tips {
		t.Run(tc.name, func(t *testing.T) {
			tip, err := l.mapWriteErr(ctx, 42, 1, 1, tc.err)

			require.NoError(t, err, "业务拒绝必须回 tip + nil error")
			require.NotNil(t, tip)
			assert.Equal(t, tc.wantID, tip.GetId())
		})
	}

	// 双存储互相矛盾 / 配表缺行不是玩家能修的,必须以故障形式暴露出来让人看见。
	faults := []struct {
		name string
		err  error
	}{
		{"leader_id 与成员表矛盾", data.ErrLeaderMismatch},
		{"GuildLevel 缺行", data.ErrGuildLevelConfigMissing},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			tip, mapped := l.mapWriteErr(ctx, 42, 1, 1, tc.err)

			assert.Nil(t, tip)
			assert.Equal(t, codes.Internal, status.Code(mapped))
		})
	}

	t.Run("入参为 nil 时不产生 tip", func(t *testing.T) {
		tip, err := l.mapWriteErr(ctx, 42, 1, 1, nil)
		assert.Nil(t, tip)
		assert.NoError(t, err)
	})

	t.Run("未知错误原样返回", func(t *testing.T) {
		boom := errors.New("some unexpected failure")
		tip, err := l.mapWriteErr(ctx, 42, 1, 1, boom)

		assert.Nil(t, tip, "认不出的错误不能被翻成任何业务码")
		assert.ErrorIs(t, err, boom)
	})
}

// ── §13.5 推送助手 ───────────────────────────────────────────

// TestMembersExcept:收件人 = 快照成员 - 操作者。顺序沿用快照(loadGuild 按 player_id 升序),
// 便于日志与用例逐条比对。
func TestMembersExcept(t *testing.T) {
	snapshot := &data.GuildData{GuildID: 9, Members: []data.MemberData{
		{PlayerID: 7, Role: constants.RoleLeader},
		{PlayerID: 42, Role: constants.RoleOfficer},
		{PlayerID: 108, Role: constants.RoleMember},
	}}

	assert.Equal(t, []uint64{7, 108}, membersExcept(snapshot, 42))
	assert.Equal(t, []uint64{7, 42, 108}, membersExcept(snapshot, 999), "排除不存在的 id 不影响其余人")
	assert.Equal(t, []uint64{7, 42, 108}, membersExcept(snapshot))
	// 解散之后已经没有快照可取,membersExcept 必须能安全地回 nil。
	assert.Nil(t, membersExcept(nil, 42))
	assert.Nil(t, membersExcept(&data.GuildData{GuildID: 9}, 42))
}

// TestNewGuildNotifierNilSafe:任一依赖缺失都退回 NoopNotifier。
//
// 必须在构造处判:kafkautil.PushToPlayer 不判 nil writer,真拿 nil 去 WriteMessages
// 会 panic 在推送协程里 —— 那是配置缺失,不该表现为运行期崩溃。
func TestNewGuildNotifierNilSafe(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	writer := &kafkago.Writer{}
	builder := fakeGateCommandBuilder{}

	cases := []struct {
		name     string
		notifier GuildNotifier
	}{
		{"没配 Kafka writer", NewGuildNotifier(nil, builder, rdb)},
		{"没配 gate 命令构造器", NewGuildNotifier(writer, nil, rdb)},
		{"没配会话 Redis", NewGuildNotifier(writer, builder, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.IsType(t, NoopNotifier{}, tc.notifier)
			assert.NotPanics(t, func() {
				tc.notifier.Notify(&pb.GuildChangedS2C{
					Kind:    pb.GuildChangeKind_GUILD_CHANGE_KIND_DISBANDED,
					GuildId: 9,
				}, []uint64{1, 2})
			})
		})
	}
}

// pushSessionIDs 是下面两个用例共用的固定样本,每个 id 对应一种会话形态。
var pushSessionIDs = []uint64{1, 2, 3, 4, 5}

// seedPushSessions 铺 player:session:* 的五种形态:在线 / 离线 / 在线但缺实例 id /
// 解不开 / 键不存在。返回的 Redis 客户端由 t.Cleanup 负责关。
func seedPushSessions(t *testing.T) *goredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	write := func(playerID uint64, state plpb.PlayerSessionState, gateID, instanceID string) {
		raw, err := proto.Marshal(&plpb.PlayerSession{
			PlayerId:       playerID,
			SessionId:      uint32(playerID) + 10,
			GateId:         gateID,
			GateInstanceId: instanceID,
			State:          state,
		})
		require.NoError(t, err)
		require.NoError(t, mr.Set(playerSessionKey(playerID), string(raw)))
	}

	write(1, plpb.PlayerSessionState_SESSION_STATE_ONLINE, "1", "inst-1")
	write(2, plpb.PlayerSessionState_SESSION_STATE_OFFLINE, "1", "inst-1")
	write(3, plpb.PlayerSessionState_SESSION_STATE_ONLINE, "1", "")
	// 0x08 = 字段 1 的 varint 标签后面什么都没有,proto.Unmarshal 必定失败。
	require.NoError(t, mr.Set(playerSessionKey(4), string([]byte{0x08})))
	// 5 号刻意不写:MGET 对不存在的键回 nil,那一格也必须按离线处理。

	return rdb
}

// TestLoadGateInfosFiltersOffline:"推不出去"一律按离线计。
//
// 缺 gate_instance_id 的那条(3 号)同样剔除:没有实例 uuid,gate 侧按实例过滤的
// 防僵尸校验就失效,kafkautil 那层会 fail-closed 拒绝 —— 提前剔除只是为了不让
// 一个坏条目把整批记成 error。
func TestLoadGateInfosFiltersOffline(t *testing.T) {
	rdb := seedPushSessions(t)

	infos, offline, err := loadGateInfos(context.Background(), rdb, pushSessionIDs)

	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, kafkautil.PlayerGateInfo{
		PlayerID:       1,
		SessionID:      11,
		GateID:         "1",
		GateInstanceID: "inst-1",
	}, infos[0])
	assert.Equal(t, 4, offline)
}

// TestOnlineSessionFromMatchesResolver:推送路由与成员列表的在线标记必须出自同一个判据。
// 两处各写一遍解码,迟早会出现"成员列表说他在线、推送却不发给他"这种自相矛盾。
func TestOnlineSessionFromMatchesResolver(t *testing.T) {
	rdb := seedPushSessions(t)
	ctx := context.Background()

	onlineMap := NewOnlineStatusResolver(rdb).BatchResolve(ctx, pushSessionIDs)

	for _, playerID := range pushSessionIDs {
		raw, err := rdb.Get(ctx, playerSessionKey(playerID)).Result()
		var value any
		if err == nil {
			value = raw
		}
		_, want := onlineSessionFrom(value)

		assert.Equal(t, want, onlineMap[playerID], "player %d 的在线判定两处必须一致", playerID)
	}
	// 3 号在线但缺实例 id:在线判定说在线(它确实连着),推送另行剔除 —— 两件事分开。
	assert.True(t, onlineMap[3])
}

// TestChangeKindLabelIsBounded:label 的取值集合必须有界且可枚举,否则 Prometheus 的
// 时间序列会随枚举无限增长(AGENTS.md §9)。同时不能用 kind.String():枚举改名会把
// 历史曲线断成两条。
func TestChangeKindLabelIsBounded(t *testing.T) {
	allowed := map[string]struct{}{
		"member_joined": {}, "member_left": {}, "member_kicked": {},
		"role_changed": {}, "leader_transferred": {}, "disbanded": {},
		"application_received": {}, "application_rejected": {},
		"funds_changed": {}, "level_up": {}, "announcement_changed": {},
		"activity_changed": {}, "delivery_done": {}, "other": {},
	}

	named := make(map[string]struct{})
	for kind := 0; kind <= 13; kind++ {
		label := changeKindLabel(pb.GuildChangeKind(kind))
		_, ok := allowed[label]
		require.True(t, ok, "kind=%d 的 label %q 不在固定集合里", kind, label)
		if kind > 0 {
			named[label] = struct{}{}
		}
	}
	// 1–13 必须各有自己的 label:两个 kind 塌成同一个串就等于排障时分不开。
	assert.Len(t, named, 13)

	assert.Equal(t, "other", changeKindLabel(pb.GuildChangeKind_GUILD_CHANGE_KIND_UNSPECIFIED))
	assert.Equal(t, "other", changeKindLabel(pb.GuildChangeKind(99)), "越界值必须归 other,不能拼出新 label")
}

// ── §3.3 配表启动校验 ────────────────────────────────────────

// legalGuildRule 是 §3.1 的默认行(GuildRule.xlsx 第 6 行)。
// 第 5–7 列(asset_op_* / reunion_*)由 B5/B6 读,validateGuildTables 不校验它们,
// 这里填上只是为了让样例与真实配表逐格一致。
func legalGuildRule() *tablepb.GuildRuleTable {
	return &tablepb.GuildRuleTable{
		Id:                              1,
		ApplicationExpireHours:          72,
		MaxPendingApplicationsPerPlayer: 3,
		MaxPendingApplicationsPerGuild:  50,
		AssetOpDeadlineSeconds:          600,
		AssetOpRetryBaseMs:              1000,
		ReunionMinOnlineMembers:         3,
	}
}

// legalGuildLevels 是 §3.2 的默认 10 级(GuildLevel.xlsx 第 6–15 行)。
func legalGuildLevels() []*tablepb.GuildLevelTable {
	rows := [][4]uint64{
		{1, 30, 2, 20000},
		{2, 35, 2, 50000},
		{3, 40, 3, 100000},
		{4, 45, 3, 180000},
		{5, 50, 4, 300000},
		{6, 60, 4, 460000},
		{7, 70, 5, 680000},
		{8, 80, 5, 960000},
		{9, 90, 6, 1300000},
		{10, 100, 6, 0},
	}
	levels := make([]*tablepb.GuildLevelTable, 0, len(rows))
	for _, row := range rows {
		levels = append(levels, &tablepb.GuildLevelTable{
			Id:               uint32(row[0]),
			MaxMembers:       uint32(row[1]),
			MaxOfficers:      uint32(row[2]),
			UpgradeCostFunds: row[3],
		})
	}
	return levels
}

// brokenRule / brokenLevels 在合法样例上改一处。每次都重新构造,用例之间不共享行指针。
func brokenRule(mutate func(*tablepb.GuildRuleTable)) *tablepb.GuildRuleTable {
	rule := legalGuildRule()
	mutate(rule)
	return rule
}

func brokenLevels(mutate func([]*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
	return mutate(legalGuildLevels())
}

// TestValidateGuildTables:配表错了要在**启动时**拒绝,不能运行期兜底。
//
// 成员上限、长老上限、申请有效期全部来自这两张表,表错了会表现为"任免随机失败"
// "申请永不过期"这类静默错误,等玩家报上来时数据已经脏了;启动期拒绝的代价只是一次回滚。
func TestValidateGuildTables(t *testing.T) {
	t.Run("默认配表必须通过", func(t *testing.T) {
		require.NoError(t, validateGuildTables(legalGuildRule(), legalGuildLevels()))
	})

	cases := []struct {
		name         string
		rule         *tablepb.GuildRuleTable
		levels       []*tablepb.GuildLevelTable
		wantContains string
	}{
		{
			name:         "缺少规则行",
			rule:         nil,
			levels:       legalGuildLevels(),
			wantContains: "GuildRule",
		},
		{
			name:         "有效期为 0",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.ApplicationExpireHours = 0 }),
			levels:       legalGuildLevels(),
			wantContains: "application_expire_hours",
		},
		{
			name:         "有效期超过 30 天",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.ApplicationExpireHours = 721 }),
			levels:       legalGuildLevels(),
			wantContains: "application_expire_hours",
		},
		{
			name:         "每人待审上限为 0",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.MaxPendingApplicationsPerPlayer = 0 }),
			levels:       legalGuildLevels(),
			wantContains: "max_pending_applications_per_player",
		},
		{
			name:         "每人待审上限越界",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.MaxPendingApplicationsPerPlayer = 11 }),
			levels:       legalGuildLevels(),
			wantContains: "max_pending_applications_per_player",
		},
		{
			name:         "每帮待审上限为 0",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.MaxPendingApplicationsPerGuild = 0 }),
			levels:       legalGuildLevels(),
			wantContains: "max_pending_applications_per_guild",
		},
		{
			name:         "每帮待审上限越界",
			rule:         brokenRule(func(r *tablepb.GuildRuleTable) { r.MaxPendingApplicationsPerGuild = 501 }),
			levels:       legalGuildLevels(),
			wantContains: "max_pending_applications_per_guild",
		},
		{
			name:         "等级表为空",
			rule:         legalGuildRule(),
			levels:       nil,
			wantContains: "GuildLevel",
		},
		{
			name: "等级表有空行",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				return append(rows, nil)
			}),
			wantContains: "GuildLevel",
		},
		{
			// guild.level 直接当 id 查表,不从 1 开始等于第 1 级的帮会全部读不到配表。
			name: "等级 id 不从 1 开始",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				for _, row := range rows {
					row.Id++
				}
				return rows
			}),
			wantContains: "GuildLevel.id",
		},
		{
			name: "缺第 3 级",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				return append(rows[:2], rows[3:]...)
			}),
			wantContains: "GuildLevel.id",
		},
		{
			// 相等就意味着可以把全帮任命成长老,帮主除了唯一性之外失去全部区分度。
			name: "长老上限不小于成员上限",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				rows[0].MaxOfficers = rows[0].MaxMembers
				return rows
			}),
			wantContains: "max_officers",
		},
		{
			// 超过 MaxGuildMembersCap 就打破了推送批量与 GuildInfo 包体的预算前提。
			name: "成员上限超过校验上限",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				rows[len(rows)-1].MaxMembers = constants.MaxGuildMembersCap + 1
				return rows
			}),
			wantContains: "max_members",
		},
		{
			name: "成员上限随等级递减",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				rows[1].MaxMembers = rows[0].MaxMembers - 10
				return rows
			}),
			wantContains: "max_members",
		},
		{
			// 花费 0 的语义是"满级",出现在中间一行等于那一级永远升不上去,而且没有任何报错。
			name: "中间级升级花费为 0",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				rows[4].UpgradeCostFunds = 0
				return rows
			}),
			wantContains: "upgrade_cost_funds",
		},
		{
			name: "末级升级花费不为 0",
			rule: legalGuildRule(),
			levels: brokenLevels(func(rows []*tablepb.GuildLevelTable) []*tablepb.GuildLevelTable {
				rows[len(rows)-1].UpgradeCostFunds = 1
				return rows
			}),
			wantContains: "upgrade_cost_funds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGuildTables(tc.rule, tc.levels)

			require.Error(t, err)
			// 文案必须带表名 / 字段名:策划看到启动日志要能直接定位到格子。
			assert.Contains(t, err.Error(), tc.wantContains)
		})
	}
}
