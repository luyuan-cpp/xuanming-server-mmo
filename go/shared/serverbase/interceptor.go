package serverbase

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	databasepb "proto/common/database"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/grpcstats"
)

// EventInbandFault 是 in-band 故障的**稳定事件名**。
// 日志检索、告警规则、压测判据都认这个字符串,改它等于改对外契约。
const EventInbandFault = "rpc_inband_fault"

// EventInbandUnknownCode 是"业务码超出已知码表"的稳定事件名。
const EventInbandUnknownCode = "rpc_inband_unknown_code"

// Options 配置 in-band 故障拦截器。零值可用(只是 error_code 分支不判故障)。
type Options struct {
	// ErrorCodeClassifier 定性 `error_code` 字段里的码。
	//
	// **没有安全的默认值**:data_service 与 scene_manager 各有一套私有码表,
	// 数值区间还和 tip 码表重叠(tip 的 1 是 kSuccess,data_service 的 1 是
	// Redis 失败)。留 nil 时,error_code 分支的非 0 码一律只记成
	// VerdictBizReject —— 少报,但绝不误报。
	// 用 FaultCodeSet 三行就能给出本服务的定性函数。
	ErrorCodeClassifier Classifier

	// TipClassifier 定性 TipInfoMessage.id。留 nil 用 TipVerdict。
	//
	// 「哪个码算故障」来自 data/tip/Tip.xlsx 的 fault 列(生成到 shared/generated/tip),
	// **不要**为了给某个码换定性而在服务里包一层本地 map 传进来 —— 那是把
	// 「分类与码定义分家」重新制造出来;要改分类,改表。这个接缝只留给测试注入,
	// 以及 TipInfoMessage.id 根本不在 tip 码轴上的服务(目前没有)。
	TipClassifier Classifier

	// SuppressFaultLog 为 true 时只记指标不打日志。
	// 压测期某条方法已知会大量返回故障码、日志已成噪音时用,平时别开。
	SuppressFaultLog bool

	// MethodName 把 gRPC 全方法名压成 Prometheus label 用的短名。
	// 留 nil 用 grpcstats.ShortMethod("/pkg.Svc/M" → "Svc/M")。
	MethodName func(fullMethod string) string
}

// Classify 按业务码的**出处**选对应码表的定性函数。
// 单独导出是为了可测:这是整个拦截器里唯一有分支逻辑的部分。
func (o Options) Classify(bc BizCode) Verdict {
	switch bc.Source {
	case SourceTipInfo:
		if o.TipClassifier != nil {
			return o.TipClassifier(bc.Code)
		}
		return TipVerdict(bc.Code)

	case SourceErrorCode:
		if o.ErrorCodeClassifier != nil {
			return o.ErrorCodeClassifier(bc.Code)
		}
		// 码表未知:0 是成功,非 0 只敢说"失败了",不敢说"是故障"。
		if bc.Code == 0 {
			return VerdictOK
		}
		return VerdictBizReject

	default:
		// 响应体里压根没有业务码字段,只要 handler 没返错就是成功。
		return VerdictOK
	}
}

func (o Options) methodName(fullMethod string) string {
	if o.MethodName != nil {
		return o.MethodName(fullMethod)
	}
	return grpcstats.ShortMethod(fullMethod)
}

// UnaryInterceptor 返回一个把 in-band 业务失败翻成日志 + 指标的拦截器。
//
// 除了「写者闸门冻结」和「自愈读不到目录」这两种确定没写的失败(见 retryableWriterFailure),它**不改动响应内容、不吞错、不把业务码翻成 gRPC status**
// —— 只观测。挂上它对客户端完全无感。
func UnaryInterceptor(opts Options) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		ctx, frozen := authoritysql.ObserveWriterFrozen(ctx)
		resp, err := handler(ctx, req)
		resp, err = retryableWriterFailure(ctx, frozen, resp, err)
		elapsed := time.Since(start)

		method := opts.methodName(info.FullMethod)

		// handler 真的返回了 error:gRPC status 已经不是 OK,
		// go-zero 自带的那套会记它,这里只补一条耗时,不重复告警。
		if err != nil {
			observeTransportError(method, elapsed)
			return resp, err
		}

		bc := ExtractBizCode(resp)
		verdict := opts.Classify(bc)
		observe(method, bc, verdict, elapsed)

		if !opts.SuppressFaultLog {
			switch verdict {
			case VerdictFault:
				logx.Errorw(EventInbandFault,
					logx.Field("method", info.FullMethod),
					logx.Field("code", bc.Code),
					logx.Field("code_source", bc.Source.String()),
					logx.Field("code_domain", codeDomainLabel(bc)),
					logx.Field("latency_ms", elapsed.Milliseconds()),
				)
			case VerdictUnknown:
				logx.Errorw(EventInbandUnknownCode,
					logx.Field("method", info.FullMethod),
					logx.Field("code", bc.Code),
					logx.Field("code_source", bc.Source.String()),
				)
			}
		}

		return resp, nil
	}
}

// errWriterFrozen 是写者闸门冻结时回给调用方的状态:搬库切换窗口里这笔没写进去,原样重试就能成功。
// 这个值本身就是结论「结局已知」,PublicationAdmission 按值认它,不 Hold(definiteRejection)。
var errWriterFrozen = status.Error(codes.Unavailable, "sql writer gate frozen, retry")

// errWriterFrozenOutcomeUnknown 同样让调用方重试,但这次请求之前有过结局未知的 SQL,不能说确定没写,排空证明要 Hold。
var errWriterFrozenOutcomeUnknown = status.Error(codes.Unavailable, "sql writer gate frozen after an unknown outcome, retry")

// errNoWriteRetry 是自愈读不到目录、handler 又报了故障时回给调用方的状态:事务已回滚,原样重试就行。按值认它,不 Hold(definiteRejection)。
var errNoWriteRetry = status.Error(codes.Unavailable, "role fence authority unavailable, retry")

// writerFrozenStatus 选冻结时回哪个状态。ctx 必须是 ObserveWriterFrozen 返回的那条。
func writerFrozenStatus(ctx context.Context) error {
	if authoritysql.WriterOutcomeUnknownSeen(ctx) {
		return errWriterFrozenOutcomeUnknown
	}
	return errWriterFrozen
}

// retryableWriterFailure 把「确定没写、重试就行」的失败改写成可重试的状态,其余原样回。ctx 必须是 ObserveWriterFrozen 返回的那条。
func retryableWriterFailure(ctx context.Context, frozen func() bool, resp any, err error) (any, error) {
	switch {
	case writerFrozenFailure(frozen, resp, err):
		return nil, writerFrozenStatus(ctx)
	case noWriteRetryFailure(ctx, resp, err):
		return nil, errNoWriteRetry
	default:
		return resp, err
	}
}

// noWriteRetryFailure 判 handler 是不是把「自愈读不到目录」报成了会 Hold 的失败。调用方自己映射成非故障 tip 的不动:比如 guild、player_inbox,客户端要的是 tip。
// 这次请求有过结局未知的 SQL 就不改,照旧 Hold。
func noWriteRetryFailure(ctx context.Context, resp any, err error) bool {
	if !authoritysql.NoWriteRetrySeen(ctx) || authoritysql.WriterOutcomeUnknownSeen(ctx) {
		return false
	}
	return publicationOutcomeUnknown(resp, err)
}

// writerFrozenFailure 判断这次请求是不是因为写者闸门冻结而失败。
//
// 各服务的 handler 常把存储错误翻成带内错误码或 Internal,哨兵错误到不了这里,所以看连接层置的观察位。
// 只有 handler 报告了失败才算:handler 吞掉冻结错误、照常回成功(尽力而为的旁路写)时不改它的结果。
//
// Aborted 也不改:这是 handler 自己下的结论「提交点之前就失败了」,比「请重试」确切。建角撞上冻结的闸门回的就是它,
// 改成 Unavailable 的话 login 只能按结局未知处理,名字释放不掉。
func writerFrozenFailure(frozen func() bool, resp any, err error) bool {
	if err != nil {
		if status.Code(err) == codes.Aborted {
			return false
		}
		return frozen() || authoritysql.IsWriterFrozen(err)
	}
	return frozen() && ExtractBizCode(resp).Code != 0
}

// WriterFrozenInterceptor 给没挂 UnaryInterceptor 的服务单独提供「闸门冻结回 Unavailable」。
func WriterFrozenInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, frozen := authoritysql.ObserveWriterFrozen(ctx)
		resp, err := handler(ctx, req)
		return retryableWriterFailure(ctx, frozen, resp, err)
	}
}

// codeDomainLabel 只对 tip 码给得出域名;私有码表没有域的概念。
func codeDomainLabel(bc BizCode) string {
	if bc.Source == SourceTipInfo {
		return TipDomain(bc.Code)
	}
	return "service_private"
}

// RootAdmission 覆盖同步根调用；异步工作必须在实际发布 owner 另计 RELAY。
func RootAdmission(gate *drainproof.Session) grpc.UnaryServerInterceptor {
	return PublicationAdmission(gate, nil, nil)
}

// publicationOutcomeUnknown 判这次请求的结局是不是未知。未知就要 Hold,排空证明从此过不去,要人工处理。
// 带内错误码按码表定性,gRPC 错误见 definiteRejection。
func publicationOutcomeUnknown(response any, err error) bool {
	if err != nil {
		return !definiteRejection(err)
	}
	verdict := (Options{}).Classify(ExtractBizCode(response))
	return verdict == VerdictFault || verdict == VerdictUnknown
}

// definiteRejection 判这个错误是不是确定的拒绝:服务端明确没有写入,结局已知,不用 Hold。
// 这是排空证明的安全边界,全仓核对过这些码的来源,结论在 docs/design/horizontal-scale-logical-db.md §3.6。handler 新回这些码之前先看那一节。
//
// 拿不准的都算未知:
//   - Unavailable、DeadlineExceeded、Canceled、Internal、Unknown:提交可能已经成了。闸门冻结、自愈读不到目录也回 Unavailable,但内层拦截器看过观察位,
//     确定没有结局未知才回 errWriterFrozen、errNoWriteRetry,这里按值认它们。这一次请求之前提交成功的写结局是已知的,不影响排空。
//   - Unimplemented、DataLoss:没有逐个证明过。
//   - 非 gRPC 错误(SQL、context):handler 没下结论。
//   - 包了一层的 gRPC 错误(fmt.Errorf("...: %w", rpcErr)):转发的是下游的结论,说不准这边写没写。所以这里只认错误本身就是 status。
//
// Aborted 在表里:语义是这次请求没有结局未知的写。目录的 accountdirectory.ErrNotCommitted(建角提交点之前失败)、ErrRetryLater(结局已知、同参数重放)回它,
// friend 的身份检查(只读)也回它。handler 回这个码之前要能证明这一点,结局未知的提交不能走到这里。
// ResourceExhausted 在表里:全仓只有目录的「账号角色数已满」回它,建角和过户都在提交点之前拒绝。别的 handler 不要拿它表示过载。
func definiteRejection(err error) bool {
	if err == errWriterFrozen || err == errNoWriteRetry {
		return true
	}
	direct, ok := err.(interface{ GRPCStatus() *status.Status })
	if !ok {
		return false
	}
	switch direct.GRPCStatus().Code() {
	case codes.NotFound, codes.InvalidArgument, codes.FailedPrecondition, codes.AlreadyExists,
		codes.PermissionDenied, codes.Unauthenticated, codes.OutOfRange, codes.Aborted, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

// PublicationAdmission 按实际内部方法白名单区分派生工作；关闭 SOURCE 不会阻断已接收工作的 RELAY。
func PublicationAdmission(root, relay *drainproof.Session, relayMethods map[string]bool, relayIngress ...*drainproof.Session) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (response any, resultErr error) {
		gate := root
		if relayMethods[info.FullMethod] {
			gate = relay
			if len(relayIngress) > 1 {
				return nil, status.Error(codes.Unavailable, "派生入口配置无效")
			}
			if len(relayIngress) == 1 {
				if relayIngress[0] == nil {
					return nil, status.Error(codes.Unavailable, "派生入口未装配")
				}
				finishIngress, err := relayIngress[0].Enter()
				if err != nil {
					return nil, status.Error(codes.Unavailable, "派生外部入口已关闭")
				}
				defer finishIngress()
			}
		}
		if gate == nil {
			return nil, status.Error(codes.Unavailable, "发行者未装配")
		}
		finish, err := gate.Enter()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "发行者尚未开放或已永久关闭")
		}
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				gate.Hold()
				panic(recovered)
			}
			if publicationOutcomeUnknown(response, resultErr) {
				gate.Hold()
			}
		}()
		return next(ctx, req)
	}
}

// DriveDrainRuntime 只编排既有 Runtime：真实 owner 的停止、join 或准备失败一律保留 HOLD。
// closeSource 不得关闭 RELAY/SQL；closeRelay 只有在所有实际发布终局已知时才能 Seal。
// prepared 必须提供真实 consumer 准备前沿；没有该 owner 时传 nil，绝不以空队列冒充准备成功。
// PreparedPublication 带实际 consumer 原件与 open 时的活代次复核。
// NoConsumers 只用于已审计没有 Kafka consumer 的源；存在 consumer 必须给 Raw 和 WithReady。
type PreparedPublication struct {
	Digest      []byte
	Raw         []byte
	NoConsumers bool
	WithReady   func(context.Context, func() ([]byte, error)) ([]byte, error)
}

func DriveDrainRuntime(ctx context.Context, runtime *drainproof.Runtime, controlDirectory string,
	root, relay *drainproof.Session, bind func(*drainproof.Runtime) error,
	closeSource, closeRelay func(context.Context) error, prepared func(context.Context) (*PreparedPublication, error)) (resultErr error) {
	if root == nil || bind == nil || closeSource == nil {
		return drainproof.ErrHold
	}
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, context.Canceled) {
			root.CloseAdmission()
		}
	}()
	if runtime == nil {
		if controlDirectory == "" {
			return nil
		}
		var err error
		runtime, err = drainproof.WaitRuntime(ctx, controlDirectory)
		if err != nil {
			return err
		}
	}
	if err := bind(runtime); err != nil {
		return err
	}
	if runtime.Request.RunPhase == databasepb.SqlWriterDrainRun_SQL_RELEASED {
		if prepared == nil {
			return fmt.Errorf("%w: 新 boot 缺少真实 consumer 准备 owner", drainproof.ErrHold)
		}
		prepareCtx, cancel := context.WithTimeout(ctx, time.Minute)
		observation, err := prepared(prepareCtx)
		cancel()
		if err != nil {
			return err
		}
		if observation == nil {
			return drainproof.ErrHold
		}
		var ready *databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness
		if observation.NoConsumers {
			if len(observation.Raw) != 0 {
				return drainproof.ErrHold
			}
			raw, marshalErr := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(&databasepb.SqlWriterMaintenanceReceipt_ConsumerReadiness{})
			if marshalErr != nil {
				return marshalErr
			}
			digest := sha256.Sum256(raw)
			ready, err = runtime.ReadinessWithEvidence("SOURCE", digest[:], raw)
		} else {
			if len(observation.Raw) == 0 || observation.WithReady == nil {
				return drainproof.ErrHold
			}
			ready, err = runtime.ReadinessWithEvidence("SOURCE", observation.Digest, observation.Raw)
		}
		if err != nil {
			return err
		}
		return runtime.AwaitOpen(ctx, ready, func(openCtx context.Context) ([]byte, error) {
			commit := func() ([]byte, error) {
				if err := root.OpenPrepared(); err != nil {
					return nil, err
				}
				return root.SnapshotDigest(), nil
			}
			if observation.WithReady != nil {
				return observation.WithReady(openCtx, commit)
			}
			return commit()
		})
	}
	closedSource := runtime.Seed("SOURCE") == nil
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if !closedSource {
			requested, err := runtime.Requested("SOURCE")
			if err != nil {
				return err
			}
			if requested {
				closeCtx, cancel := context.WithTimeout(ctx, time.Minute)
				err = closeSource(closeCtx)
				cancel()
				if err != nil {
					return err
				}
				if err = runtime.Write("SOURCE", root); err != nil {
					return err
				}
				closedSource = true
			}
		}
		if !closedSource {
			continue
		}
		if runtime.Seed("RELAY") == nil {
			return nil
		}
		requested, err := runtime.Requested("RELAY")
		if err != nil {
			return err
		}
		if !requested {
			continue
		}
		if relay == nil || closeRelay == nil {
			return drainproof.ErrHold
		}
		closeCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err = closeRelay(closeCtx)
		cancel()
		if err != nil {
			return err
		}
		return runtime.Write("RELAY", relay)
	}
}

// LoadPublicationRuntime 在创建 SQL/worker 之前确定新 boot 的 prepared 根门禁。
func LoadPublicationRuntime(plan, directory, digest string) (*drainproof.Runtime, *drainproof.Session, *drainproof.Session, error) {
	var runtime *drainproof.Runtime
	if plan != "" {
		var err error
		runtime, err = drainproof.LoadRuntime(plan, directory, digest)
		if err != nil {
			return nil, nil, nil, err
		}
	} else if directory != "" || digest != "" {
		return nil, nil, nil, drainproof.ErrHold
	}
	root := drainproof.New()
	if runtime != nil && runtime.Request.RunPhase == databasepb.SqlWriterDrainRun_SQL_RELEASED {
		root = drainproof.NewPrepared()
	}
	return runtime, root, drainproof.New(), nil
}
