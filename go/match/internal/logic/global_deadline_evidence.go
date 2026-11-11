package logic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"

	"match/internal/authority"
	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	pb "proto/match"
	"shared/tracing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// 截止作废的取证(2026-11-06,cross-zone-matchmaking.md §13.3「2026-11-06 修复」;时间判据与证据取值在
// authority/terminal_abort.go)。到期恢复与帮会取消写截止 ABORT 之前,先向成局的原执行实例确认它没有冻结待提交的终局:
// 截止前已经打完、battle 正在重放 CommitBattleOutcome 的局,不能因为 match 或它的库不可用跨过了截止加宽限就被作废。
// 与提前作废(global_no_room_abort.go 的 tombstoneAndConfirmAborted)共用同一次交互 askOriginalInstance,不另写探测:
// DestroyBattle(global_match_abort_if_absent)只对尚无房间的原操作立拒收墓碑、有房间时忽略,截止之后问一次不会伤到
// 还在等提交的房间;再 QueryBattleCreation 读结果。
//
// 评审修正(2026-11-06):「原地址连不上」不再从 gRPC 错误与 ctx 推断。最初 askOriginalInstance 走按 endpoint 缓存的
// grpc.ClientConn,一次建连失败之后它停在 TRANSIENT_FAILURE,不带 WaitForReady 的 RPC 当场回 Unavailable:黑洞端点
// (主机与 match、etcd 一起分区,battle 还活着、终局冻结着在重放)只有第一次取证会等到期限,之后每次都像「当场连不上」,
// 租约到期、镜像摘掉它之后就会被判成原进程已不在、提前作废,分区恢复后 battle 用同一个实例 UUID 重注册,重放的 COMPLETE
// 只收到 ABORT。现在只认另起一次 TCP 拨号被原地址当场拒绝(RST,originalAddressRefuses),超时、不可达一律判不了,
// 交给硬上限。「原 node_id 已登记成另一个实例」也不再单独算证据:分区中的原进程恢复后要在重注册失败时才停机,
// 停机前还会排空、补提交一轮(battle main.cpp 的排空门,至多 15 秒)。
// 评审补(2026-11-10):取证不再经 discovery 的进程级连接缓存,每次单独建连、用完即关(dialOriginalInstance,原因见那里)。
// 判据不变,仍只认 TCP 拒连:一次性连接上的 RPC 照样可能在期限之内失败(对端一连上就关、地址上不是 battle),
// gRPC 错误说明不了原地址是什么状态。

// originalInstanceAnswer 是一次「立墓碑再查询」的原始结果。queryErr 为 nil 时 query 非 nil。
type originalInstanceAnswer struct {
	query      *battlepb.QueryBattleCreationResponse
	destroyErr error // DestroyBattle 的错误;拨号失败时与 queryErr 相同
	queryErr   error // QueryBattleCreation 的错误;拨号失败也记在这里
}

// askOriginalInstance 向成局的原执行实例(f.TargetEndpoint 上的 f.TargetInstanceId)发 DestroyBattle(原 operation,
// reason=global_match_abort_if_absent),再查询同一 operation。DestroyBattle 失败时照样查询:battle 对不符的实例 UUID 拒绝
// DestroyBattle(placement_operation_mismatch)、查询回空包,截止作废据查询回包认出「原地址上已是另一个实例」;提前作废
// 只认 DestroyBattle 成功之后的 ABORTED(tombstoneAndConfirmAborted)。只发 RPC,不碰 SQL,不在任何事务或行锁里调用;
// ctx 由调用方给期限。两次 RPC 走这一次单独建的连接,返回前关掉,不进进程级缓存(dialOriginalInstance);
// RPC 的失败(含当场回 Unavailable)说明不了原地址是什么状态,见文件头的评审修正。
func askOriginalInstance(ctx context.Context, f *pb.MatchFormationRecord) originalInstanceAnswer {
	conn, err := dialOriginalInstance(f.TargetEndpoint)
	if err != nil {
		return originalInstanceAnswer{destroyErr: err, queryErr: err}
	}
	defer func() { _ = conn.Close() }()
	client := battlepb.NewBattleNodeClient(conn)
	_, destroyErr := client.DestroyBattle(ctx, &battlepb.DestroyBattleRequest{BattleId: f.BattleId, Reason: "global_match_abort_if_absent", PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	query, queryErr := client.QueryBattleCreation(ctx, &battlepb.QueryBattleCreationRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId})
	return originalInstanceAnswer{query: query, destroyErr: destroyErr, queryErr: queryErr}
}

// dialOriginalInstance 为一次「立墓碑再查询」单独建一条 gRPC 连接,调用方用完即关(评审补 2026-11-10)。
// 不用 discovery.DialEndpoint:取证与提前作废问的常常是已经摘册的原实例,经它建的连接会重新放回进程级缓存,而缓存只在镜像删除
// 那一刻经 onRemove 清一次,之后没人再清 —— 每个问过的死端点都留下一条连接,它在死地址上反复重连失败,退避一路涨到 gRPC 上限
// (约 120 秒);同一 ip:port 上来新 battle 时镜像里是新条目、不清旧连接,撮合与观战拿到的就是这条停在 TRANSIENT_FAILURE 的
// 旧连接,不带 WaitForReady 的调用当场回 Unavailable,新节点最长约两分钟不可用。取证每局每 30 秒才问一次,会把这变成常态。
// 拨号选项与 DialEndpoint 相同(内网 insecure,traceparent 由 shared/tracing 的拦截器写)。grpc.NewClient 不连网,第一次 RPC
// 才建连:端口关闭时当场失败,黑洞时等满调用方给的期限,与原先第一次取证同一语义;多一次握手的代价可以接受。
func dialOriginalInstance(endpoint string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(tracing.UnaryClientInterceptor()))
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", endpoint, err)
	}
	return conn, nil
}

// battleRegistryView 是 match 的 battle 注册中心镜像(stable 与 canary 两份)对原执行实例的只读事实。
// 原 node_id 是否已登记成另一个实例不在这里:它不单独算证据(见文件头的评审修正)。
type battleRegistryView struct {
	synced bool // 两份镜像都已从 etcd 全量同步过至少一次(NodeWatcher.Synced)
	listed bool // 原实例 UUID 还登记在某一份镜像里
}

// forgotten:镜像已同步、原实例已不在册。只是拒连作证据的前提,单凭它不算(etcd 抖动、租约短暂失效时也查不到)。
// battle 正常停机时先停听、排空并补提交一轮,最后才撤销租约(node.cpp CloseTransportOwnersInLoop 的 ReleaseNodeId),
// 所以停听后的排空窗口里它仍在册,拒连不会在这时被当成原进程已不在。
func (v battleRegistryView) forgotten() bool { return v.synced && !v.listed }

// battleRegistryOf 读 match 的 battle 镜像。没装配镜像(只在单测里)时什么都不知道:不算同步过、当作仍在册,不给任何证据。
func battleRegistryOf(s *svc.ServiceContext, f *pb.MatchFormationRecord) battleRegistryView {
	if s == nil || s.BattleNodes == nil {
		return battleRegistryView{listed: true}
	}
	view := battleRegistryView{
		synced: s.BattleNodes.Synced() && (s.CanaryBattleNodes == nil || s.CanaryBattleNodes.Synced()),
	}
	for _, watcher := range []*discovery.NodeWatcher{s.BattleNodes, s.CanaryBattleNodes} {
		if watcher == nil {
			continue
		}
		for _, entry := range watcher.Snapshot() {
			if entry.NodeUuid != "" && entry.NodeUuid == f.GetTargetInstanceId() {
				view.listed = true
			}
		}
	}
	return view
}

// classifyDeadlineEvidence 把一次「立墓碑再查询」的结果、原地址是否当场拒连与注册中心镜像归成截止作废证据,纯函数;
// 各取值的含义见 authority.DeadlineAbortEvidence。拿不准一律 Undetermined,由硬上限兜底。refused:查询没成之后另起的一次
// TCP 拨号被原地址当场拒绝(originalAddressRefuses)。
//   - 查询回显的就是原实例:ABORTED 且本次立墓碑成功 → NoRoom;PRESENT → BattleStillHolds;其余(UNKNOWN:同一 battle_id
//     记着别的 operation;立墓碑没成)→ Undetermined。原实例亲口作答时,注册中心说什么都不作数;
//   - 查询成功却没有回显实例:原地址上应答的是另一个 battle 进程(实例 UUID 每个进程生成一次,不符时 battle 回空包)
//     → InstanceGone;回显了别的实例(battle 不会这样回)→ Undetermined;
//   - 查询失败:原地址的 TCP 端口当场拒连、且镜像已同步、原实例已不在册 → InstanceGone;其余一律 Undetermined —— 超时、黑洞、
//     主机或网络不可达、连得上却不是 battle、gRPC 当场回 Unavailable(对端一连上就关;修复前的缓存连接停在 TRANSIENT_FAILURE
//     时也是这样)、镜像没同步过、原实例仍在册。
//     只凭「注册中心查不到」或「原 node_id 已登记成另一个实例」都不算(分区中的原进程还活着,恢复后还会补提交)。
func classifyDeadlineEvidence(f *pb.MatchFormationRecord, answer originalInstanceAnswer, refused bool, registry battleRegistryView) authority.DeadlineAbortEvidence {
	instance := f.GetTargetInstanceId()
	if answer.queryErr == nil && instance != "" && answer.query.GetTargetInstanceId() == instance {
		switch answer.query.GetState() {
		case battlepb.BattleCreationState_BATTLE_CREATION_ABORTED:
			if answer.destroyErr == nil {
				return authority.DeadlineEvidenceNoRoom
			}
		case battlepb.BattleCreationState_BATTLE_CREATION_PRESENT:
			return authority.DeadlineEvidenceBattleStillHolds
		}
		return authority.DeadlineEvidenceUndetermined
	}
	if answer.queryErr == nil {
		if answer.query.GetTargetInstanceId() == "" {
			return authority.DeadlineEvidenceInstanceGone
		}
		return authority.DeadlineEvidenceUndetermined
	}
	if refused && registry.forgotten() {
		return authority.DeadlineEvidenceInstanceGone
	}
	return authority.DeadlineEvidenceUndetermined
}

// originalAddressRefuses 另起一次 TCP 拨号(不走 gRPC,不发任何字节,连上就立刻关),只在对端当场回 RST
// (ECONNREFUSED)时为真:原地址的主机在,端口上已没有进程在听。超时、黑洞、主机或网络不可达、ctx 已到期、连上了
// (不论对端是谁)一律为假。用调用方给的期限:查询已等满期限时它立即失败,不再多等。
// Windows 本机开发环境里回环拒连要经过 SYN 重试(约 2 秒)才报,可能超出取证期限,那就按判不了处理(只是等到硬上限,不会误判)。
func originalAddressRefuses(ctx context.Context, endpoint string) bool {
	if endpoint == "" {
		return false
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return connectionRefused(err)
}

// wsaeConnRefused 是 Winsock 的 WSAECONNREFUSED:Go 在 Windows 上把拒连报成这个错误码,syscall.ECONNREFUSED 在 Windows 上
// 只是 Go 自造的占位值,errors.Is 认不出它。只在 Windows 上比对。
const wsaeConnRefused syscall.Errno = 10061

// connectionRefused 判拨号错误是不是对端当场拒连。
func connectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return runtime.GOOS == "windows" && errors.Is(err, wsaeConnRefused)
}

// deadlineEvidenceProbe 组装截止作废取证:ask 是与原实例的那一次交互;registry 读注册中心镜像(在交互之后读,反映最新登记);
// 查询没成、且镜像已同步而原实例已不在册时,refuses 另起一次 TCP 拨号看原地址是否当场拒连(其余情况拒连也不算证据,不拨,
// 免得在黑洞端点上白等期限)。可被多个 goroutine 同时调用(到期恢复一页并行取证):三者都只读共享状态。
func deadlineEvidenceProbe(ask func(context.Context, *pb.MatchFormationRecord) originalInstanceAnswer, refuses func(context.Context, string) bool, registry func(*pb.MatchFormationRecord) battleRegistryView) authority.DeadlineAbortProbe {
	return func(ctx context.Context, f *pb.MatchFormationRecord) authority.DeadlineAbortEvidence {
		answer := ask(ctx, f)
		view := registry(f)
		refused := answer.queryErr != nil && view.forgotten() && refuses(ctx, f.GetTargetEndpoint())
		return classifyDeadlineEvidence(f, answer, refused, view)
	}
}

// BindDeadlineAbortEvidence 把生产取证接到成局权威上(authority.Store.BindDeadlineAbortProbe)。match_service 在起任何后台循环、
// 注册任何 RPC 之前调用一次;stable 与 canary 副本都要接(帮会取消是 RPC,两条轨道都可能收到)。
// 没接时一律判不了:截止 ABORT 只在硬上限之后才写(fail-closed,但全员要多占约 10 分钟)。
func BindDeadlineAbortEvidence(s *svc.ServiceContext) {
	s.Authority.BindDeadlineAbortProbe(deadlineEvidenceProbe(askOriginalInstance, originalAddressRefuses, func(f *pb.MatchFormationRecord) battleRegistryView {
		return battleRegistryOf(s, f)
	}))
}
