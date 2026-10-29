package internal

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"protogen/internal/utils"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 客户端协议方法的调用形态(docs/design/client-rpc-callback.md §5.1–§5.2)。
//
// 形态由方法级 option OptionClientRpcKind(proto/db/proto_option.proto,字段号 410001)显式声明,未标注即 REQUEST。
// 本文件是 Unity 与 robot 两个客户端生成器共用的唯一判定:读 option、筛客户端协议服务、做生成期校验。
// 生成器不得再按服务名、Notify 前缀或响应类型自行推断形态 —— 那些约定只在这里作为校验出现。

// ClientRpcKind 与 proto 枚举 ClientRpcKind 的数值一一对应。
type ClientRpcKind int32

const (
	// ClientRpcRequest:客户端发起,服务端恰好回一个包(默认,proto 上不标)。
	ClientRpcRequest ClientRpcKind = 0
	// ClientRpcOneWay:客户端发起,成功不回包,以 id = 0 发送。只用于 scene TCP 路径上响应恰好是 Empty 的方法。
	ClientRpcOneWay ClientRpcKind = 1
	// ClientRpcPush:服务端推送,客户端不发起;载荷为方法输入类型。
	ClientRpcPush ClientRpcKind = 2
	// ClientRpcServerOnly:服务端内部或 gate 代发,客户端不得发起;客户端生成器不为它生成任何成员。
	ClientRpcServerOnly ClientRpcKind = 3
)

const (
	// OptionClientRpcKindNumber 是 MethodOptions 扩展 OptionClientRpcKind 的字段号。
	// 外部模块 github.com/luyuancpp/protooption 的绑定里没有这个扩展,描述符反序列化后它留在未知字段里,
	// 所以按字段号读(同 OptionPersistFrozen 的约定)。
	OptionClientRpcKindNumber protowire.Number = 410001

	// optionClientRpcKindName 是扩展全名;proto_option.proto 没有 package,全名即扩展名。
	optionClientRpcKindName protoreflect.FullName = "OptionClientRpcKind"

	// clientRpcEmptyType 是 gate 判定单向时比较的类型:C++ 的 ::Empty(proto/common/base/empty.proto,无 package)。
	// 必须全名精确比较:LoginEmptyResponse 一类名字含 Empty 的响应照常回包。
	clientRpcEmptyType = ".Empty"

	clientRpcNotifyPrefix = "Notify"
)

// pushWithoutNotifyPrefix 是「推送必须以 Notify 开头」的唯一例外(§5.2 第 1 条),键为 服务名.方法名。
// 这三个方法早于命名约定上线,改名会换消息号;新推送一律以 Notify 开头,不要往这里加。
var pushWithoutNotifyPrefix = map[string]struct{}{
	"SceneClientPlayerCommon.SendTipToClient": {},
	"SceneClientPlayerCommon.KickPlayer":      {},
	"SceneClientPlayerCommon.RedirectToGate":  {},
}

// String 返回与 proto 枚举值同名的文本,用于日志与错误信息。
func (k ClientRpcKind) String() string {
	switch k {
	case ClientRpcRequest:
		return "CLIENT_RPC_REQUEST"
	case ClientRpcOneWay:
		return "CLIENT_RPC_ONE_WAY"
	case ClientRpcPush:
		return "CLIENT_RPC_PUSH"
	case ClientRpcServerOnly:
		return "CLIENT_RPC_SERVER_ONLY"
	default:
		return fmt.Sprintf("ClientRpcKind(%d)", int32(k))
	}
}

// ReadClientRpcKind 读取方法上的 OptionClientRpcKind。
//
// 输入:任意 MethodDescriptorProto;nil 或没有 options 视为未标注。
// 返回:未标注 → (ClientRpcRequest, false, nil);标注 → (该值, true, nil),显式标 CLIENT_RPC_REQUEST 也算标注。
// 错误:字段号 410001 被另一个扩展占用、wire type 不是 varint、字节损坏,或值不在 ClientRpcKind 枚举范围内。
// 读取顺序同 ReadBoolExtension(internal/prototools/option/value.go;目录名 option,包名 prototools,调用方按
// prototools.ReadBoolExtension 引用):先看已注册扩展(将来绑定里有了它也能读),再看未知字段;
// 同一字段在未知字段里出现多次时按 protobuf 语义取最后一次。只读描述符,可并发调用。
func ReadClientRpcKind(method *descriptorpb.MethodDescriptorProto) (ClientRpcKind, bool, error) {
	opts := method.GetOptions()
	if opts == nil {
		return ClientRpcRequest, false, nil
	}
	message := opts.ProtoReflect()
	if !message.IsValid() {
		return ClientRpcRequest, false, nil
	}

	var (
		raw      int64
		found    bool
		rangeErr error
	)
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if !field.IsExtension() || protowire.Number(field.Number()) != OptionClientRpcKindNumber {
			return true
		}
		if field.FullName() != optionClientRpcKindName {
			rangeErr = fmt.Errorf("method option number %d is registered as %s, expected %s",
				OptionClientRpcKindNumber, field.FullName(), optionClientRpcKindName)
			return false
		}
		if field.Kind() != protoreflect.EnumKind || field.IsList() {
			rangeErr = fmt.Errorf("method option %s has kind %s, expected a singular enum",
				optionClientRpcKindName, field.Kind())
			return false
		}
		raw, found = int64(value.Enum()), true
		return false
	})
	if rangeErr != nil {
		return ClientRpcRequest, false, rangeErr
	}
	if !found {
		var err error
		raw, found, err = lastVarintField(message.GetUnknown(), OptionClientRpcKindNumber)
		if err != nil {
			return ClientRpcRequest, false, fmt.Errorf("method option %s: %w", optionClientRpcKindName, err)
		}
	}
	if !found {
		return ClientRpcRequest, false, nil
	}
	if raw < int64(ClientRpcRequest) || raw > int64(ClientRpcServerOnly) {
		return ClientRpcRequest, true, fmt.Errorf("method option %s = %d is outside the ClientRpcKind enum (0..3)",
			optionClientRpcKindName, raw)
	}
	return ClientRpcKind(raw), true, nil
}

// lastVarintField 在未知字段字节里找字段号 number 的最后一次出现,按 varint 解码。
// enum 以 int32 符号扩展成 64 位 varint 编码,所以按 int64 解释:负值与超大值都会落到范围检查之外。
func lastVarintField(raw []byte, number protowire.Number) (int64, bool, error) {
	var (
		value int64
		found bool
	)
	for len(raw) > 0 {
		num, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			return 0, false, protowire.ParseError(tagLength)
		}
		raw = raw[tagLength:]
		if num == number {
			if wireType != protowire.VarintType {
				return 0, false, fmt.Errorf("wire type is %d, expected varint", wireType)
			}
			v, valueLength := protowire.ConsumeVarint(raw)
			if valueLength < 0 {
				return 0, false, protowire.ParseError(valueLength)
			}
			value, found = int64(v), true
			raw = raw[valueLength:]
			continue
		}
		fieldLength := protowire.ConsumeFieldValue(num, wireType, raw)
		if fieldLength < 0 {
			return 0, false, protowire.ParseError(fieldLength)
		}
		raw = raw[fieldLength:]
	}
	return value, found, nil
}

// ClientRpcMethod 是一个客户端协议方法与它的调用形态。
type ClientRpcMethod struct {
	Method *MethodInfo
	Kind   ClientRpcKind
}

// ClientRpcService 是一个标了 OptionIsClientProtocolService 的服务。
// Methods 按 proto 声明顺序,含 SERVER_ONLY(是否为它生成成员由各生成器决定;两个客户端生成器都不生成)。
type ClientRpcService struct {
	Service *RPCServiceInfo
	Methods []ClientRpcMethod
}

// SceneTCPPredicate 回答「gate 是否经 scene TCP 路径(注册表 protocol = PROTOCOL_TCP)转发这个方法」。
type SceneTCPPredicate func(method *MethodInfo) bool

// IsSceneTCPMethod 是生产用判定,与 gate 注册表的 protocol 字段同源(cppGen.GetProtocol):
// 方法所在 proto 目录的 domain_meta rpc.type 不是 grpc 即 PROTOCOL_TCP。
// gate 对这条路径上响应恰好为 Empty 的方法单向发送、不回包(client_session_handler.cpp HandleTcpNodeMessage)。
// 依赖已加载的 _config.Global,只在生成流水线里调用;单测改为注入 SceneTCPPredicate。
func IsSceneTCPMethod(method *MethodInfo) bool {
	return !utils.HasGrpcService(method.Path())
}

// ClientRpcViolation 是一条生成期校验违例。
type ClientRpcViolation struct {
	Service string // proto service 名原样
	Method  string // proto 方法名
	File    string // 所在 proto 文件
	Reason  string
}

func (v ClientRpcViolation) String() string {
	return fmt.Sprintf("%s.%s (%s): %s", v.Service, v.Method, v.File, v.Reason)
}

// ClientRpcValidationError 汇总一次分类的全部违例:不在第一条就返回,一次生成就能看全要改的地方。
type ClientRpcValidationError struct {
	Violations []ClientRpcViolation
}

func (e *ClientRpcValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d client RPC method(s) violate docs/design/client-rpc-callback.md §5.2:", len(e.Violations))
	for _, v := range e.Violations {
		b.WriteString("\n  ")
		b.WriteString(v.String())
	}
	return b.String()
}

// ClassifyClientRpcServices 筛出客户端协议服务、给每个方法定形态,并执行 §5.2 的全部生成期校验。
//
// 调用时机:方法号已分配之后(流水线里 cppGen.InitMessageId 之后),通常传 GlobalRPCServiceList。
// 输入:isSceneTCP 判定 gate 的转发路径,生产传 IsSceneTCPMethod,单测注入;不得为 nil。
// 输出:按 services 顺序的客户端协议服务,方法按 proto 声明顺序。客户端协议服务只看 OptionIsClientProtocolService,
// 不看服务名(GuildService / MatchService 名字不含 ClientPlayer 也算;SceneRollbackClientPlayer 没标就不算)。
// 校验:
//  1. 名字以 Notify 开头 ⇔ CLIENT_RPC_PUSH,例外只有 SceneClientPlayerCommon 的三个推送;
//  2. CLIENT_RPC_ONE_WAY 的响应必须恰好是 .Empty 且走 scene TCP;反之 scene TCP 上响应恰好是 .Empty 的
//     REQUEST 必须改标 ONE_WAY(gate 不回包,回调只能超时);
//  3. CLIENT_RPC_PUSH 的响应必须是 .Empty 或与输入类型相同;
//  4. option 读不出来(越界值、wire type 错)即违例;没标 OptionIsClientProtocolService 的服务却标了
//     OptionClientRpcKind 也是违例(该标注不会生效,多半是标错了服务);
//  5. 消息号为 math.MaxUint64(未分配)即违例;0 是合法号。
//
// 错误:有任一违例即返回 *ClientRpcValidationError(含全部违例),结果为 nil;调用方按生成器习惯 Fatal。
// 只读 services 与描述符,不改全局状态,可被多个生成器并发调用。
func ClassifyClientRpcServices(services RPCServiceInfoList, isSceneTCP SceneTCPPredicate) ([]ClientRpcService, error) {
	if isSceneTCP == nil {
		return nil, errors.New("ClassifyClientRpcServices: isSceneTCP must not be nil")
	}

	var (
		result     []ClientRpcService
		violations []ClientRpcViolation
	)
	for _, service := range services {
		if service == nil {
			continue
		}
		clientService := IsClientProtocolService(service.ServiceDescriptorProto)
		classified := ClientRpcService{Service: service}
		for _, method := range service.Methods {
			kind, found, err := ReadClientRpcKind(method.MethodDescriptorProto)
			if err != nil {
				violations = append(violations, newClientRpcViolation(method, err.Error()))
				continue
			}
			if !clientService {
				if found {
					violations = append(violations, newClientRpcViolation(method,
						"OptionClientRpcKind is set but the service is not marked OptionIsClientProtocolService, so it has no effect"))
				}
				continue
			}
			for _, reason := range clientRpcMethodViolations(method, kind, isSceneTCP(method)) {
				violations = append(violations, newClientRpcViolation(method, reason))
			}
			classified.Methods = append(classified.Methods, ClientRpcMethod{Method: method, Kind: kind})
		}
		if clientService {
			result = append(result, classified)
		}
	}

	if len(violations) > 0 {
		return nil, &ClientRpcValidationError{Violations: violations}
	}
	return result, nil
}

func newClientRpcViolation(method *MethodInfo, reason string) ClientRpcViolation {
	return ClientRpcViolation{
		Service: method.Service(),
		Method:  method.Method(),
		File:    method.Fd.GetName(),
		Reason:  reason,
	}
}

// clientRpcMethodViolations 对一个客户端协议方法做 §5.2 第 1、2、3、5 条校验(纯函数),返回全部违例原因。
func clientRpcMethodViolations(method *MethodInfo, kind ClientRpcKind, sceneTCP bool) []string {
	var reasons []string
	name := method.Method()
	input := method.MethodDescriptorProto.GetInputType()
	output := method.MethodDescriptorProto.GetOutputType()

	if method.Id == math.MaxUint64 {
		reasons = append(reasons, "message id is not assigned (math.MaxUint64); check message_id.txt and that InitMessageId ran first")
	}

	hasNotifyPrefix := strings.HasPrefix(name, clientRpcNotifyPrefix)
	_, pushExempt := pushWithoutNotifyPrefix[method.Service()+"."+name]
	switch {
	case hasNotifyPrefix && kind != ClientRpcPush:
		reasons = append(reasons, fmt.Sprintf("name starts with %q but OptionClientRpcKind is %s; mark it CLIENT_RPC_PUSH",
			clientRpcNotifyPrefix, kind))
	case !hasNotifyPrefix && kind == ClientRpcPush && !pushExempt:
		reasons = append(reasons, fmt.Sprintf("CLIENT_RPC_PUSH requires the %q name prefix "+
			"(only SceneClientPlayerCommon.SendTipToClient / KickPlayer / RedirectToGate are exempt)", clientRpcNotifyPrefix))
	}

	switch kind {
	case ClientRpcOneWay:
		if output != clientRpcEmptyType {
			reasons = append(reasons, fmt.Sprintf("CLIENT_RPC_ONE_WAY requires response type exactly %s, got %s",
				clientRpcEmptyType, output))
		}
		if !sceneTCP {
			reasons = append(reasons, "CLIENT_RPC_ONE_WAY is only valid on the scene TCP path; "+
				"gRPC-routed methods always get a reply, mark it CLIENT_RPC_REQUEST")
		}
	case ClientRpcRequest:
		if sceneTCP && output == clientRpcEmptyType {
			reasons = append(reasons, fmt.Sprintf("on the scene TCP path the gate forwards methods returning %s one-way and never replies; "+
				"mark it CLIENT_RPC_ONE_WAY (or CLIENT_RPC_PUSH if the server sends it)", clientRpcEmptyType))
		}
	case ClientRpcPush:
		if output != clientRpcEmptyType && output != input {
			reasons = append(reasons, fmt.Sprintf("CLIENT_RPC_PUSH response type must be %s or the input type %s, got %s",
				clientRpcEmptyType, input, output))
		}
	}
	return reasons
}
