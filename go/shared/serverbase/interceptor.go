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
// 它**不改动响应内容、不吞错、不把业务码翻成 gRPC status**
// —— 只观测。挂上它对客户端完全无感。
func UnaryInterceptor(opts Options) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
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
			verdict := (Options{}).Classify(ExtractBizCode(response))
			if resultErr != nil || verdict == VerdictFault || verdict == VerdictUnknown {
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
