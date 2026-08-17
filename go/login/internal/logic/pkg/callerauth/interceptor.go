package callerauth

import (
	"context"
	"encoding/base64"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"login/internal/logic/pkg/ctxkeys"
	login_proto "proto/common/base"
)

// 稳定事件名:日志检索与告警规则认这些字符串,改它们等于改对外契约。
const (
	// EventRejected 强制档下拒绝了一条身份声明。
	EventRejected = "caller_auth_rejected"
	// EventBypassed 宽松档(dev/test)下放行了一条验不过的身份声明。
	// **生产日志里出现这条 = 有人把 Mode 配错了**,应当直接告警。
	EventBypassed = "caller_auth_bypassed"
)

// sensitiveMetadataKeys 是交给 handler 之前必须从入站 metadata 里摘掉的 key。
//
// # 为什么要摘(confused deputy)
//
// login 自己会以客户端身份去调 player_locator / scene_manager。只要入站凭据
// 还留在 context 里,任何一处「顺手把入站 metadata 抄进出站请求」的写法
// (框架中间件、日志透传、将来某个人图省事)都会让 login 变成攻击者的代理人:
// 攻击者递进来一条伪造的 authorization,login 拿着它去替他敲下游的门。
// 身份在这一层验完就**只**通过 ctxkeys 这条已验证通道往下传,
// 原始 metadata 一律不留 —— 想伪造就没有可搬运的原料。
var sensitiveMetadataKeys = []string{
	MetaSessionDetail,
	MetaCaller,
	MetaTimestamp,
	MetaNonce,
	MetaSignature,
	"authorization",
	"proxy-authorization",
	"grpcgateway-authorization",
	"cookie",
	"x-api-key",
}

// UnaryServerInterceptor 是 login 的身份声明闸门,替代原来那个「解出来就信」
// 的 SessionInterceptor。
//
// 三条不变量:
//
//  1. **没带身份声明 = 没有身份**。不带 x-session-detail-bin 的调用(Java
//     Gateway 的 POST /api/login 新链路就是这样)原样放行,ctx 里不会凭空
//     出现 SessionDetails,handler 走「无会话」分支 —— 与改造前一致。
//  2. **带了身份声明就必须验签**。强制档下验不过直接 Unauthenticated;
//     宽松档(dev/test)打 WARN 后放行,方便上游分批接签名。
//  3. **只有验过的身份能进 ctx**。原始 metadata 在交给 handler 前被摘干净。
func UnaryServerInterceptor(v *Verifier) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, hasMD := metadata.FromIncomingContext(ctx)

		if hasMD {
			raw := mdFirst(md, MetaSessionDetail)
			if raw != "" {
				detail, err := authenticate(v, md, info.FullMethod, raw)
				if err != nil {
					reason := Reason(err)
					recordAuth(reason, v.Enforcing())
					if v.Enforcing() {
						logx.Errorw(EventRejected,
							logx.Field("method", info.FullMethod),
							logx.Field("reason", reason),
							logx.Field("detail", err.Error()),
						)
						return nil, status.Errorf(codes.Unauthenticated,
							"内部调用方身份声明验签失败(%s):调用方必须按 callerauth 规范附 %s/%s/%s/%s",
							reason, MetaCaller, MetaTimestamp, MetaNonce, MetaSignature)
					}
					logx.Errorw(EventBypassed,
						logx.Field("method", info.FullMethod),
						logx.Field("reason", reason),
						logx.Field("detail", err.Error()),
						logx.Field("hint", "dev/test 宽松档放行;生产模式下这条请求会被拒绝"),
					)
					// 宽松档:仍然把声明写进 ctx,保持本地链路可用。
					if detail != nil {
						ctx = ctxkeys.WithSessionDetails(ctx, detail)
					}
				} else {
					recordAuth("ok", v.Enforcing())
					ctx = ctxkeys.WithSessionDetails(ctx, detail)
				}
			}

			// 无论有没有身份声明都要摘:未验证的 authorization 之类同样不能
			// 落到 handler 手里。
			ctx = metadata.NewIncomingContext(ctx, stripSensitive(md))
		}

		resp, err := handler(ctx, req)

		// ---- 把会话详情回显到响应头(cpp gate 依赖这条,行为保持不变) ----
		if detail, ok := ctxkeys.GetSessionDetails(ctx); ok {
			if bin, merr := proto.Marshal(detail); merr == nil {
				header := metadata.Pairs(MetaSessionDetail, base64.StdEncoding.EncodeToString(bin))
				if serr := grpc.SendHeader(ctx, header); serr != nil {
					logx.Errorf("回写 %s 响应头失败: %v", MetaSessionDetail, serr)
				}
			} else {
				logx.Errorf("序列化 SessionDetails 失败: %v", merr)
			}
		}

		return resp, err
	}
}

// authenticate 解码 + 验签 + 反序列化。
//
// 返回的 *SessionDetails 在**失败时也可能非 nil**(签名不过但 proto 能解),
// 这是给宽松档用的:dev 下要能继续把声明放进 ctx,否则本地链路直接断。
// 强制档的调用方必须只看 error。
func authenticate(v *Verifier, md metadata.MD, fullMethod, raw string) (*login_proto.SessionDetails, error) {
	bin, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrBadPayload
	}

	// 先验签再反序列化:proto.Unmarshal 是攻击面(未认证输入喂解析器),
	// 能挡在签名后面就挡在签名后面。
	verr := v.verifyOnly(md, fullMethod, bin)

	detail := &login_proto.SessionDetails{}
	if uerr := proto.Unmarshal(bin, detail); uerr != nil {
		if verr != nil {
			return nil, verr
		}
		return nil, ErrBadPayload
	}
	if verr != nil {
		return detail, verr
	}
	return detail, nil
}

// stripSensitive 复制一份去掉敏感 key 的 metadata。
// 用 Copy 而不是原地删:入站 md 由 gRPC 运行时持有,原地改会影响到
// 同一条流上的其它读取方。
func stripSensitive(md metadata.MD) metadata.MD {
	out := md.Copy()
	for _, k := range sensitiveMetadataKeys {
		out.Delete(k)
	}
	return out
}
