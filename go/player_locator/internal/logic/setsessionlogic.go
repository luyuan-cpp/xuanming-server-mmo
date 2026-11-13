package logic

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	common "proto/common/base"
	pb "proto/player_locator"
)

// sessionChangedError 构造「会话记录已不是调用方读到的那条」的应答:gRPC ABORTED。
//
// ABORTED 的通行语义就是读改写序列被并发打断,调用方应从读开始重做。回它有两个前提,缺一不可:
//  1. 本次写入确定没有发生(不是结果未知),记录一个字节都没改;
//  2. 被拒的原因只是「记录在调用方读取之后变了」,重读、重新决策之后再写是合法的。
//
// 典型来源是 gate 排空迁移与跨 zone 重定向:客户端先关旧连接、再在新连接上 EnterGame,旧 gate 的断线上报
// (SetDisconnecting 推进了版本)落在 login 那条登录链的 GetSession 与 SetSession 之间。login 收到 ABORTED 后重读,
// 这次看到 DISCONNECTING,改走短线重连(go/login/internal/logic/pkg/sessionmanager 的 ErrSessionChanged;
// docs/design/gate-drain-session-migration.md §5)。
// 代次不前进、清理未完成等拒绝不满足第 2 条,仍按普通错误返回,调用方按登录失败处理。
func sessionChangedError(format string, args ...any) error {
	return status.Errorf(codes.Aborted, format, args...)
}

type SetSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetSessionLogic {
	return &SetSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *SetSessionLogic) SetSession(in *pb.SetSessionRequest) (*common.Empty, error) {
	incoming := in.GetSession()
	if incoming == nil {
		return nil, fmt.Errorf("session is required")
	}
	if incoming.PlayerId == 0 {
		return nil, fmt.Errorf("session player_id is required")
	}

	// clone 后再补继承字段，避免修改 gRPC request 自身。
	s := proto.Clone(incoming).(*pb.PlayerSession)
	key := sessionKey(s.PlayerId)
	expected, err := l.svcCtx.PlayerRedis.Get(l.ctx, key).Bytes()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	// previous 是 expected 字节解出的旧会话,只用来推导要摘除的 gate 索引成员;首次写入时为 nil。
	var previous *pb.PlayerSession
	if err == redis.Nil {
		expected = nil
	} else {
		current := &pb.PlayerSession{}
		if err := proto.Unmarshal(expected, current); err != nil {
			return nil, fmt.Errorf("decode current session: %w", err)
		}
		previous = current

		// ReplaceLogin 的调用方当前不会把旧 SceneID/SceneNodeID 拷进新会话。
		// 在现有 RPC 契约下保留旧值，至少避免顶号登录把已知场景位置清成 0。
		if s.SceneId == 0 {
			s.SceneId = current.SceneId
		}
		if s.SceneNodeId == "" {
			s.SceneNodeId = current.SceneNodeId
		}
		// LoginGeneration 刻意不继承:请求为 0(旧版 login)时如实写 0,表示这条会话没有代次;
		// 继承旧值会让无代次的会话冒用上一次登录的代次,scene 栅栏会把它当成已放行的那次登录。

		// 重放判断必须排在代次栅栏之前:重放请求的代次与记录相等,先判栅栏会把丢包重试误拒成 stale。
		if proto.Equal(s, current) {
			// RPC 响应丢失后的同请求重放：允许幂等地清掉残留 lease 状态。
		} else if s.SessionVersion != current.SessionVersion+1 {
			// 调用方按它读到的版本构造了这次写入,而记录已被别的写者推进过:回 ABORTED,调用方重读重判。
			return nil, sessionChangedError(
				"stale session update for player %d: current version=%d incoming=%d",
				s.PlayerId, current.SessionVersion, s.SessionVersion,
			)
		} else if !loginGenerationAdvances(current.LoginGeneration, s.LoginGeneration) {
			// 版本对得上、代次却不前进:不是记录变了,是调用方带来的代次不合法(失锁的旧登录链)。
			// 不回 ABORTED:调用方按登录失败处理,不重放读改写。
			return nil, fmt.Errorf(
				"stale session update for player %d: current login_generation=%d incoming=%d",
				s.PlayerId, current.LoginGeneration, s.LoginGeneration,
			)
		}
	}

	data, err := proto.Marshal(s)
	if err != nil {
		return nil, err
	}

	// session 写入与旧 ready/processing lease 的撤销在同一 Lua 中完成。
	// 完整 protobuf CAS 防止两个基于同一旧版本构造的替换登录互相覆盖。
	// gate 索引(旧 gate 摘除、新 gate 加入)也在这一段 Lua 里,与会话写入原子。
	result, err := replaceSessionAndCancelLease(l.ctx, l.svcCtx, s.PlayerId, expected, data, 0, previous, s)
	if err != nil {
		return nil, err
	}
	if result == replaceSessionCleanupPending {
		return nil, fmt.Errorf("%w for player %d", ErrSessionCleanupPending, s.PlayerId)
	}
	if result != replaceSessionApplied {
		// 上面 GET 之后、这段 Lua 之前记录又被改写(整字节 CAS 没对上):同样是「记录变了」,回 ABORTED。
		return nil, sessionChangedError("session changed concurrently for player %d", s.PlayerId)
	}

	return &common.Empty{}, nil
}

// loginGenerationAdvances 是会话提交点的登录代次栅栏(docs/design/scene-enter-session-fence.md §3.4)。
//
// 记录与请求都非 0 时要求严格递增:失锁后两条并发登录链里只有一条能提交,记录代次不回退(G1),
// 两条会话也不会共用同一个非 0 代次(G2)。任一方为 0 不校验:0 是旧版 login 写入或发出的无代次会话,
// 混版窗口内照旧放行,由 scene 栅栏的判定表兜底(§6)。
// 只比较请求值与记录值,不读铸造键(G3):铸了没提交的号直接作废,不影响这里的判定。
// 判定读的是 CAS 的 expected 字节,与写入同属一次整字节 CAS,不存在先判后写的窗口。
func loginGenerationAdvances(current, incoming uint64) bool {
	return current == 0 || incoming == 0 || incoming > current
}
