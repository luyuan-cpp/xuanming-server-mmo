package cpp

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	messageoption "github.com/luyuancpp/protooption"

	"go.uber.org/zap"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"
)

func appendUniqueString(dst []string, seen map[string]struct{}, value string) []string {
	if value == "" {
		return dst
	}
	if _, ok := seen[value]; ok {
		return dst
	}
	seen[value] = struct{}{}
	return append(dst, value)
}

// ReadProtoFileService reads service information from a protobuf descriptor file.
func ReadProtoFileService() error {
	serviceIndex := uint32(0)

	// Iterate through each file in the descriptor set
	for _, file := range internal.FdSet.File {
		for _, service := range file.Service {
			// Create an RPCServiceInfo object for each service
			rpcServiceInfo := internal.RPCServiceInfo{
				ProtoFileInfo: internal.ProtoFileInfo{
					Fd:                     file,
					ServiceDescriptorProto: service,
				},
				ServiceIndex: serviceIndex,
			}

			// Iterate through each method in the service
			for index, method := range service.Method {
				// Create an MethodInfo object for each method
				rpcMethodInfo := internal.MethodInfo{
					ProtoFileInfo: internal.ProtoFileInfo{
						Fd:                     file,
						ServiceDescriptorProto: service,
					},
					Id:                    math.MaxUint64,
					Index:                 uint64(index),
					MethodDescriptorProto: method,
				}

				// Append the method info to the service info
				rpcServiceInfo.Methods = append(rpcServiceInfo.Methods, &rpcMethodInfo)
			}

			internal.GlobalRPCServiceList = append(internal.GlobalRPCServiceList, &rpcServiceInfo)

			result, ok := internal.FileServiceMap.Load(file.GetName())
			if ok {
				serviceList := result.([]*internal.RPCServiceInfo)
				serviceList = append(serviceList, &rpcServiceInfo)
				internal.FileServiceMap.Store(file.GetName(), serviceList)
			} else {
				var serviceList []*internal.RPCServiceInfo
				serviceList = append(serviceList, &rpcServiceInfo)
				internal.FileServiceMap.Store(file.GetName(), serviceList)
			}
			serviceIndex++
		}
	}

	// Return nil on successful completion
	return nil
}

// ReadAllProtoFileServices reads all service information from protobuf files in configured directories.
func ReadAllProtoFileServices(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ReadProtoFileService()
	}()
}

// message_id.txt 的墓碑行 `N=reserved:<原 KeyName>` 与 event_id.txt 同一格式(行解析与重号校验见 id_file.go):
// RPC 方法从 proto 里删掉(或改名、挪服务)后,它的号永久保留、不再发给别的方法。滚动升级窗口里旧节点
// 按旧号发来的请求 / 应答会被新节点按新方法的类型解析,而 protobuf 往往还能"解析成功"。墓碑号在
// gRpcMethodRegistry 里留空槽,IsDispatchableMessageId 判为不可派发。

const (
	// maxNewMessageTombstonesPerRun 是一轮生成里允许新转墓碑的 RPC 方法数上限(与事件号的
	// maxNewTombstonesPerRun 分开设)。墓碑永久且不可逆;一次冒出一大批,几乎总是本轮 proto 输入不完整
	// (proto 目录配空、指错,方法多的域整域没进描述符集),照写会让大批活号永久作废、同名方法全部换号。
	// 超限即中止,确实要批量删时显式设 allowMassMessageTombstoneEnv=1 放行。
	// 第 7 批删 Route 一族恰好 4 个(41/74/78/81),卡线通过。
	//
	// 本上限按条数设防,拦不住 RPC 方法不超过 4 个的小域(2026-10 有 chat 2 个、db 1 个、client_rpc_router 1 个)。
	// 小域整域漏读改由输入收集按域设防(prototools.collectUniqueProtoFiles,2026-10-16 起):任一 domain_meta 域的
	// source 为空、不存在、不是目录、遍历出错或走下来 0 个 .proto 即中止,此时 message_id.txt 还没写。
	// 仍拦不住的情形(剩余风险):①配置里删掉或漏掉整个 domain_meta 块,该域的活号会和正常删除一样转墓碑,
	// 方法不超过 4 个时毫无拦截;②域目录在但个别 .proto 被删或改名,与正常删方法无从区分。
	// regen 后仍须核对 "RPC method no longer in proto" WARN 与 message_id.txt 的 diff 都在预期内再提交。
	maxNewMessageTombstonesPerRun = 4

	// maxMessageIdFileHoles 是 message_id.txt 允许的空洞数上限(空洞 = 0..最大号之间既非活号也非墓碑的号)。
	// 本生成器删方法转墓碑、新方法升序填空洞、写回不删行,正常文件没有空洞(2026-10-16 为 0);空洞只来自
	// 墓碑机制之前的旧删除或手改。手改把号多打几位会把最大号推到远处:kMaxRpcMethodCount 与以它定长的
	// std::array 静默膨胀、该方法换号,极端值还会让 assignMessageIds 找空洞的循环耗尽内存。超限即中止。
	maxMessageIdFileHoles = 64

	// allowMassMessageTombstoneEnv 只认 "1";放行后仍逐条打 WARN,提交 message_id.txt 前要人工核对。
	allowMassMessageTombstoneEnv = "PROTOGEN_ALLOW_MASS_MESSAGE_TOMBSTONE"
)

var (
	// message_id.txt 的内存视图,由 ReadMessageIdFile 填。
	loadedMessageIdFile = newIdFileState()
	// 全部墓碑(文件原有 + 本轮新转,号 → 原 KeyName),由 InitMessageId 填、WriteMessageIdFile 写回。
	// 本轮活方法在 internal.RpcIdMethodMap,发号后的最大号(含墓碑)在 internal.FileMaxMessageId。
	reservedMessageIdMap = map[uint64]string{}
)

// parseMessageIdFile 把 message_id.txt 读成内存视图(除读 r 外无副作用)。比 event_id.txt 严:
// 空行跳过;其余任何不是 `N=<KeyName>` / `N=reserved:<原 KeyName>` 形状的行、空名、非数字号、重号
// 一律返回错误 —— 跳过一行就等于把那个号当成空洞,下一轮会被发给别的方法。号的范围也要校验:
// 不小于 math.MaxUint32 的号、以及空洞超过 maxMessageIdFileHoles 的最大号同样返回错误。
func parseMessageIdFile(r io.Reader) (*idFileState, error) {
	state := newIdFileState()
	scanner := bufio.NewScanner(r)
	lineNo := 0
	maxIdLineNo, maxIdLine := 0, ""
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, ok, err := parseIdFileLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d %q: message ID is not a decimal integer: %w", lineNo, line, err)
		}
		if !ok || entry.Name == "" {
			return nil, fmt.Errorf("line %d %q: expected N=<KeyName> or N=%s<KeyName>", lineNo, line, internal.ReservedEventIdPrefix)
		}
		// 线上 message_id 是 uint32,kMaxRpcMethodCount(= 最大号 + 1)是 uint32_t,所以号必须小于 MaxUint32;
		// 这同时挡住 math.MaxUint64 —— 它是 assignMessageIds 的"未定号"标记,找空洞的循环到它也停不下来。
		if entry.Id >= math.MaxUint32 {
			return nil, fmt.Errorf("line %d %q: message ID out of range, must be below %d", lineNo, line, uint64(math.MaxUint32))
		}
		if err := state.add(entry); err != nil {
			return nil, fmt.Errorf("line %d %q: %w", lineNo, line, err)
		}
		if entry.Id == state.MaxId {
			maxIdLineNo, maxIdLine = lineNo, line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// add 已拒绝重号,所以条目数 = 不同号的个数,空洞数 = 最大号 + 1 - 条目数。
	if entries := uint64(len(state.Active) + len(state.Reserved)); entries > 0 {
		if holes := state.MaxId + 1 - entries; holes > maxMessageIdFileHoles {
			return nil, fmt.Errorf("line %d %q: max message ID %d leaves %d unassigned IDs below it, limit is %d; is the ID mistyped?",
				maxIdLineNo, maxIdLine, state.MaxId, holes, maxMessageIdFileHoles)
		}
	}
	return state, nil
}

// ReadMessageIdFile 异步把 message_id.txt 读进 loadedMessageIdFile;文件打不开或任何一行有问题都中止生成。
func ReadMessageIdFile(wg *sync.WaitGroup) {
	wg.Add(1)

	go func() {
		defer wg.Done() // Fix: defer wg.Done() to ensure it runs regardless of error

		f, err := os.Open(_config.Global.Paths.MessageIdFile)
		if err != nil {
			logger.Global.Fatal("Failed to open message ID file",
				zap.String("file_path", _config.Global.Paths.MessageIdFile),
				zap.Error(err),
			)
		}
		defer f.Close()

		state, err := parseMessageIdFile(f)
		if err != nil {
			logger.Global.Fatal("Corrupted message ID file",
				zap.String("file_path", _config.Global.Paths.MessageIdFile),
				zap.Error(err),
			)
		}
		loadedMessageIdFile = state
	}()
}

// messageIdAssignment 是 assignMessageIds 的结果。
type messageIdAssignment struct {
	Infos                map[uint64]*internal.MethodInfo // 本轮活方法:号 → 方法
	Reserved             map[uint64]string               // 全部墓碑(文件原有 + 本轮新转):号 → 原 KeyName
	MaxId                uint64                          // 发号后的最大号(含墓碑)
	NewlyReserved        []uint64                        // 本轮新转墓碑的号,升序,供自检与日志
	ReaddedOverTombstone []uint64                        // 与已有墓碑同名、因此拿了新号的方法的新号,升序,供日志
}

// assignMessageIds 按 message_id.txt 给本轮 proto 里的 RPC 方法定号。不读写包级状态、不打日志;
// 成功时唯一的副作用是写 services 里每个方法的 Id,返回错误时不改任何方法。
//
//  1. 文件里已登记的方法(按 KeyName = 服务名 + 方法名)沿用原号;
//  2. 文件里的活号在本轮 proto 里找不到对应方法(方法被删,或改名、挪服务 —— 等同删旧加新)
//     → 转墓碑,号永久保留;
//  3. 新方法按 services 的顺序(即 GlobalRPCServiceList 的顺序)先升序填 0..file.MaxId 中
//     既非活号也非墓碑的空洞,填完再从 MaxId+1 往后发。墓碑号无论新旧一律跳过;与墓碑同名的方法
//     重新出现时同样按新方法发新号,墓碑不动(复活旧号只能人工改行并评审)。
//
// 本轮两个方法的 KeyName 相同(例如服务 `Ab` 的方法 `C` 与服务 `A` 的方法 `bC`)会争同一个号,返回错误。
func assignMessageIds(services internal.RPCServiceInfoList, file *idFileState) (messageIdAssignment, error) {
	methods := make([]*internal.MethodInfo, 0)
	liveNames := make(map[string]struct{})
	for _, service := range services {
		for _, method := range service.Methods {
			name := method.KeyName()
			if _, dup := liveNames[name]; dup {
				return messageIdAssignment{}, fmt.Errorf("two RPC methods share the message ID key %q (service name + method name); rename one of them", name)
			}
			liveNames[name] = struct{}{}
			methods = append(methods, method)
		}
	}

	result := messageIdAssignment{
		Infos:    make(map[uint64]*internal.MethodInfo, len(methods)),
		Reserved: make(map[uint64]string, len(file.Reserved)),
		MaxId:    file.MaxId,
	}
	tombstonedNames := make(map[string]struct{}, len(file.Reserved))
	for id, originalName := range file.Reserved {
		result.Reserved[id] = originalName
		tombstonedNames[originalName] = struct{}{}
	}

	for _, method := range methods {
		if id, known := file.Active[method.KeyName()]; known {
			method.Id = id
			result.Infos[id] = method
		} else {
			method.Id = math.MaxUint64
		}
	}

	for name, id := range file.Active {
		if _, live := liveNames[name]; live {
			continue
		}
		result.Reserved[id] = name
		result.NewlyReserved = append(result.NewlyReserved, id)
	}
	sort.Slice(result.NewlyReserved, func(i, j int) bool { return result.NewlyReserved[i] < result.NewlyReserved[j] })

	holes := make([]uint64, 0)
	for id := uint64(0); id <= file.MaxId; id++ {
		if _, used := result.Infos[id]; used {
			continue
		}
		if _, reserved := result.Reserved[id]; reserved {
			continue
		}
		holes = append(holes, id)
	}

	// 空洞升序且都不大于 file.MaxId,所以新号随 methods 顺序单调递增,ReaddedOverTombstone 天然升序。
	nextHole := 0
	for _, method := range methods {
		if method.Id != math.MaxUint64 {
			continue
		}
		if nextHole < len(holes) {
			method.Id = holes[nextHole]
			nextHole++
		} else {
			result.MaxId++
			method.Id = result.MaxId
		}
		result.Infos[method.Id] = method
		if _, readded := tombstonedNames[method.KeyName()]; readded {
			result.ReaddedOverTombstone = append(result.ReaddedOverTombstone, method.Id)
		}
	}
	return result, nil
}

// checkMessageIdInputComplete 判定本轮 proto 输入是否完整到可以落墓碑(纯函数),在 assignMessageIds 之后、
// WriteMessageIdFile 之前调用。墓碑写进 message_id.txt 就永久生效,输入可疑时宁可中止也不猜(fail-closed):
//  1. 本轮一个 RPC 方法都没读到,而 message_id.txt 里还有活号 → proto 目录配空或描述符集没生成,
//     不受 allowMassTombstone 放行;
//  2. 本轮新转墓碑超过 maxNewMessageTombstonesPerRun 且未显式放行。文件里原有的墓碑不计入。
//
// 返回 nil 表示可以继续;错误信息列出涉及的 `号=名`,供排障。
func checkMessageIdInputComplete(file *idFileState, result messageIdAssignment, allowMassTombstone bool) error {
	if len(result.Infos) == 0 && len(file.Active) > 0 {
		return fmt.Errorf("no RPC method was read from proto in this run, but the message ID file still lists %d live methods; "+
			"they would all be tombstoned", len(file.Active))
	}

	if len(result.NewlyReserved) > maxNewMessageTombstonesPerRun && !allowMassTombstone {
		return fmt.Errorf("%d RPC methods would be tombstoned in one run, limit is %d (%s); "+
			"check that every proto service was read, or set %s=1 if they were really removed",
			len(result.NewlyReserved), maxNewMessageTombstonesPerRun, formatIdNames(result.NewlyReserved, result.Reserved), allowMassMessageTombstoneEnv)
	}
	return nil
}

// reservedIdsNamed 返回原名为 name 的全部墓碑号,升序,供日志。
func reservedIdsNamed(reserved map[uint64]string, name string) []uint64 {
	ids := make([]uint64, 0)
	for id, originalName := range reserved {
		if originalName == name {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// InitMessageId 给本轮全部 RPC 方法定号(规则见 assignMessageIds),结果写 internal.RpcIdMethodMap、
// reservedMessageIdMap 与 internal.FileMaxMessageId。发号或自检失败即中止:此时 message_id.txt 还没写
// (pipeline 里 WriteMessageIdFile 在其后),中止即不落任何墓碑、不改任何号。
func InitMessageId() {
	result, err := assignMessageIds(internal.GlobalRPCServiceList, loadedMessageIdFile)
	if err == nil {
		allowMassTombstone := os.Getenv(allowMassMessageTombstoneEnv) == "1"
		err = checkMessageIdInputComplete(loadedMessageIdFile, result, allowMassTombstone)
	}
	if err != nil {
		logger.Global.Fatal("Refusing to assign message IDs; message ID file left unchanged",
			zap.String("message_id_file", _config.Global.Paths.MessageIdFile),
			zap.Error(err),
		)
	}

	internal.RpcIdMethodMap = result.Infos
	reservedMessageIdMap = result.Reserved
	internal.FileMaxMessageId = result.MaxId

	// WARN 而不是 INFO:删方法时这是预期输出,要在提交 message_id.txt 之前被看见;
	// 批量的情况已由 checkMessageIdInputComplete 拦下或经显式放行。
	for _, id := range result.NewlyReserved {
		logger.Global.Warn("RPC method no longer in proto; its message ID is now reserved and will never be reused",
			zap.Uint64("message_id", id),
			zap.String("method_key", result.Reserved[id]),
		)
	}
	for _, id := range result.ReaddedOverTombstone {
		methodKey := result.Infos[id].KeyName()
		logger.Global.Warn("RPC method re-added under a tombstoned name; it got a new message ID and the old ID stays reserved",
			zap.Uint64("message_id", id),
			zap.String("method_key", methodKey),
			zap.Uint64s("reserved_message_ids", reservedIdsNamed(result.Reserved, methodKey)),
		)
	}
}

// renderMessageIdFile 按号升序输出活方法 `N=<KeyName>` 与墓碑 `N=reserved:<原 KeyName>`(纯函数);
// 读回再写出逐字节不变,下一轮生成仍能认出墓碑。
func renderMessageIdFile(infos map[uint64]*internal.MethodInfo, reserved map[uint64]string) string {
	ids := make([]uint64, 0, len(infos)+len(reserved))
	for id := range infos {
		ids = append(ids, id)
	}
	for id := range reserved {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var builder strings.Builder
	for _, id := range ids {
		if method, live := infos[id]; live {
			fmt.Fprintf(&builder, "%d=%s\n", id, method.KeyName())
			continue
		}
		fmt.Fprintf(&builder, "%d=%s%s\n", id, internal.ReservedEventIdPrefix, reserved[id])
	}
	return builder.String()
}

func WriteMessageIdFile() {
	if err := utils2.WriteFileIfChanged(_config.Global.Paths.MessageIdFile, []byte(renderMessageIdFile(internal.RpcIdMethodMap, reservedMessageIdMap))); err != nil {
		logger.Global.Fatal("Failed to write the message ID file", zap.String("message_id_file", _config.Global.Paths.MessageIdFile), zap.Error(err))
		return
	}
}

func GetProtocol(dirName string) uint32 {
	if utils2.HasGrpcService(dirName) {
		return _config.Global.PathLists.NodeTypes.GrpcNode
	}
	return _config.Global.PathLists.NodeTypes.TcpNode
}

func GetProtocolByEnum(enumName string) uint32 {
	nodeName := strings.ReplaceAll(strings.ToLower(enumName), _config.Global.Naming.NodeServiceSuffix, "")
	for _, v := range _config.Global.PathLists.ProtoDirectories {
		if !strings.Contains(v, nodeName) || !strings.Contains(v, _config.Global.Naming.GrpcName) {
			continue
		}
		return _config.Global.PathLists.NodeTypes.GrpcNode
	}
	return _config.Global.PathLists.NodeTypes.TcpNode
}

func IsTcpNodeByEnum(dirName string) bool {
	return GetProtocolByEnum(dirName) == _config.Global.PathLists.NodeTypes.TcpNode
}

// rpcMethodMetaInit 是一条 gRpcMethodRegistry 初始化所需的全部值,按 RpcMethodMeta 成员顺序排列。
// RpcMethodMeta 不再有 handler 成员:服务端派发走 RpcChannel 注册的服务对象,玩家消息走 gPlayerService,
// 注册表里的空 XxxImpl 实例全仓无读者(批 7 删除)。
type rpcMethodMetaInit struct {
	MessageIdConst string // <KeyName>MessageId
	ServiceName    string
	MethodName     string
	CppRequest     string
	CppResponse    string
	Protocol       uint32
	NodeType       string // eNodeType 的全限定枚举值
	SenderName     string // 空 = 不带 sender(cc_generic_services 的 muduo 方法)
	GrpcFullMethod string // proto 服务全名；供 gate 直连与路由转发使用同一签名目标
}

// rpcMethodMetaInitLines 生成一条 RpcMethodMeta 初始化(纯函数),按字段角色分行:
//
//	line 1: assignment + opening brace
//	line 2: serviceName, methodName
//	line 3: requestProto
//	line 4: responseProto
//	line 5: protocol, targetNodeType [, sender] + closing brace
func rpcMethodMetaInitLines(meta rpcMethodMetaInit) []string {
	lines := []string{
		fmt.Sprintf("gRpcMethodRegistry[%s] = RpcMethodMeta{", meta.MessageIdConst),
		fmt.Sprintf(`    "%s", "%s",`, meta.ServiceName, meta.MethodName),
		fmt.Sprintf("    std::make_unique<%s>(),", meta.CppRequest),
		fmt.Sprintf("    std::make_unique<%s>(),", meta.CppResponse),
	}
	if meta.SenderName == "" {
		return append(lines, fmt.Sprintf("    %d, %s};", meta.Protocol, meta.NodeType))
	}
	if meta.GrpcFullMethod != "" {
		return append(lines, fmt.Sprintf("    %d, %s, %s, %q};", meta.Protocol, meta.NodeType, meta.SenderName, meta.GrpcFullMethod))
	}
	return append(lines, fmt.Sprintf("    %d, %s, %s};", meta.Protocol, meta.NodeType, meta.SenderName))
}

// grpcSenderFor 只给 unary gRPC 方法生成带显式完成回调的通用发送器。
func grpcSenderFor(method *internal.MethodInfo) (declaration, sender string, ok bool) {
	if method.CcGenericServices() || method.ClientStreaming() || method.ServerStreaming() {
		return "", "", false
	}
	function := "Send" + method.Service() + method.Method() + "Erased"
	declaration = "void " + function + "(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );"
	if method.CppPackage() != "" {
		declaration = "namespace " + method.CppPackage() + "{" + declaration + "}"
	}
	return declaration, method.CppPackage() + "::" + function, true
}

// inRpcRegistry 报告服务的方法是否写进 C++ RPC 注册表(rpc_event_registry.cpp 的 gRpcMethodRegistry):
// 没有方法的、属于 db 节点的、对不上节点类型枚举的都不进(方法元数据要写目标节点类型,见 RPCServiceInfo.HasNodeType)。
// 收头文件、写方法元数据、收 gate 上行白名单三处共用这一个判定。依赖 GenNodeUtil 填好的 NodeEnumValueSet。
func inRpcRegistry(service *internal.RPCServiceInfo) bool {
	return len(service.Methods) > 0 &&
		!internal.IsFileBelongToNode(service.Fd, messageoption.NodeType_NODE_DB) &&
		service.HasNodeType()
}

// clientAllowlistCases 生成 gate 上行白名单 IsClientMessageId 的 case 行(docs/design/client-rpc-callback.md §7 R4)。
//
// 输入:services 生产传 GlobalRPCServiceList(消息号已分配);isSceneTCP 生产传 internal.IsSceneTCPMethod,单测注入;
// messageIdSuffix 是消息号常量名后缀(_config.Global.Naming.MessageId)。
// 输出:进 RPC 注册表(inRpcRegistry)的客户端协议服务里客户端能发起的方法(ClientRpcKind.AcceptsClientUpstream:
// REQUEST / ONE_WAY),按服务与方法的声明顺序,去重。PUSH 只下行,SERVER_ONLY 由服务端或 gate 代发(Disconnect 由 gate 合成),都不收。
// 方法形态取 internal.ClassifyClientRpcServices 的分类,与 Unity、robot 生成器同一份,本函数不自行读 option。
// 错误:分类校验有任一违例(含 option 读不出来、Notify 前缀没标推送)即返回该错误,不按 REQUEST 兜底放行(fail-closed);
// 调用方 Fatal。只读 services 与描述符,不读配置。
func clientAllowlistCases(services internal.RPCServiceInfoList, isSceneTCP internal.SceneTCPPredicate, messageIdSuffix string) ([]string, error) {
	classified, err := internal.ClassifyClientRpcServices(services, isSceneTCP)
	if err != nil {
		return nil, err
	}

	var (
		cases []string
		seen  = make(map[string]struct{})
	)
	for _, service := range classified {
		if !inRpcRegistry(service.Service) {
			continue
		}
		for _, method := range service.Methods {
			if !method.Kind.AcceptsClientUpstream() {
				continue
			}
			cases = appendUniqueString(cases, seen, fmt.Sprintf("case %s%s:", method.Method.KeyName(), messageIdSuffix))
		}
	}
	return cases, nil
}

// writeServiceInfoCppFile generates C++ code that initializes gRPC service metadata.
func writeServiceInfoCppFile(wg *sync.WaitGroup) {
	defer wg.Done()

	type ServiceInfoCppData struct {
		Includes             []string
		ServiceInfoIncludes  []string
		EventIncludesBlock   string
		InitLines            []string
		ClientMessageIdCases []string
		MessageIdArraySize   int
		EventIdArraySize     int
		SenderDeclarations   []string
	}

	var (
		includes             []string
		serviceInfoIncludes  []string
		initLines            []string
		senderDeclarations   []string
		includeSet           = make(map[string]struct{})
		serviceIncludeSet    = make(map[string]struct{})
		senderDeclarationSet = make(map[string]struct{})
	)

	// Step 1: Collect headers
	for _, service := range internal.GlobalRPCServiceList {
		if !inRpcRegistry(service) {
			continue
		}

		firstMethod := service.Methods[0]

		pbInclude := _config.Global.Naming.IncludeBegin +
			strings.Replace(firstMethod.Path(), _config.Global.Paths.ProtoDir, _config.Global.DirectoryNames.ProtoDirName, 1) +
			firstMethod.PbcHeadName() + "\"\n"
		includes = appendUniqueString(includes, includeSet, pbInclude)
		serviceInfoIncludes = appendUniqueString(serviceInfoIncludes, serviceIncludeSet, firstMethod.ServiceInfoIncludeName())
	}

	// Step 2: Generate init lines for RpcService
	for _, service := range internal.GlobalRPCServiceList {
		if !inRpcRegistry(service) {
			continue
		}

		// Add service group separator for readability
		if len(initLines) > 0 {
			initLines = append(initLines, "")
		}
		initLines = append(initLines, fmt.Sprintf("// --- %s ---", service.Service()))

		for _, method := range service.Methods {
			messageId := method.KeyName() + _config.Global.Naming.MessageId

			nodeType := fmt.Sprintf("%s::%sNodeService", internal.NodeEnumCppQualifiedType, service.NodeServiceForCpp())

			meta := rpcMethodMetaInit{
				MessageIdConst: messageId,
				ServiceName:    method.Service(),
				MethodName:     method.Method(),
				CppRequest:     method.CppRequest(),
				CppResponse:    method.CppResponse(),
				Protocol:       GetProtocol(method.Path()),
				NodeType:       nodeType,
			}
			if declaration, sender, ok := grpcSenderFor(method); ok {
				senderDeclarations = appendUniqueString(senderDeclarations, senderDeclarationSet, declaration)
				meta.SenderName = sender
				qualifiedService := method.Service()
				if method.Package() != "" {
					qualifiedService = method.Package() + "." + qualifiedService
				}
				meta.GrpcFullMethod = "/" + qualifiedService + "/" + method.Method()
			}
			initLines = append(initLines, rpcMethodMetaInitLines(meta)...)
		}
	}

	// Step 3: gate 上行白名单(docs/design/client-rpc-callback.md §7 R4)
	clientIdLines, err := clientAllowlistCases(internal.GlobalRPCServiceList, internal.IsSceneTCPMethod, _config.Global.Naming.MessageId)
	if err != nil {
		logger.Global.Fatal("Failed to build the gate client allowlist IsClientMessageId", zap.Error(err))
	}

	// Step 4: Fill template data and render
	tmplData := ServiceInfoCppData{
		Includes:             includes,
		ServiceInfoIncludes:  serviceInfoIncludes,
		EventIncludesBlock:   strings.Join(EventIdHeaderIncludes(), "\n") + "\n",
		InitLines:            initLines,
		ClientMessageIdCases: clientIdLines,
		MessageIdArraySize:   int(internal.MessageIdLen()),
		EventIdArraySize:     int(EventIdLen()),
		SenderDeclarations:   senderDeclarations,
	}

	if err := utils2.RenderTemplateToFile("internal/template/rpc_event_registry.cpp.tmpl", _config.Global.Paths.ServiceCppFile, tmplData); err != nil {
		logger.Global.Fatal("Failed to generate service info cpp file",
			zap.String("template_path", "internal/template/rpc_event_registry.cpp.tmpl"),
			zap.String("output_path", _config.Global.Paths.ServiceCppFile),
			zap.Error(err),
		)
	}
}

// writeServiceInfoHeadFile writes service information to a header file.
func writeServiceInfoHeadFile() {
	type HeaderTemplateData struct {
		MaxMessageLen uint64
		MaxEventLen   uint64
	}

	data := HeaderTemplateData{
		MaxMessageLen: internal.MessageIdLen(),
		MaxEventLen:   EventIdLen(),
	}

	err := utils2.RenderTemplateToFile("internal/template/service_header.tmpl", _config.Global.Paths.ServiceHeaderFile, data)
	if err != nil {
		logger.Global.Fatal("Failed to generate service info header file",
			zap.String("template_path", "internal/template/service_header.tmpl"),
			zap.String("output_path", _config.Global.Paths.ServiceHeaderFile),
			zap.Error(err),
		)
		return
	}
}

// Helper function to generate instance data for player services.
func generateInstanceData(serviceList []string, isPlayerHandlerFunc func(*internal.RPCMethods) bool, handlerDir string, serviceName string) string {
	type PlayerServiceInstanceData struct {
		SelfHeader     string   // e.g. login_player_service.h
		Includes       []string // list of #include "...Handler.h"
		HandlerClasses []string // e.g. class LoginServiceImpl : public LoginService {};
		InitLines      []string // e.g. g_player_service.emplace(...)
	}

	fileNameWithoutExt := serviceName[:len(serviceName)-len(filepath.Ext(serviceName))]

	var includes, handlerClasses, initLines []string

	for _, service := range internal.GlobalRPCServiceList {
		if !isPlayerHandlerFunc(&service.Methods) {
			continue
		}
		method := service.Methods[0]
		className := method.Service() + "Impl"

		includes = append(includes, fmt.Sprintf(`#include "%s%s"`, method.FileBaseNameNoEx(), _config.Global.FileExtensions.HandlerH))
		handlerClasses = append(handlerClasses, fmt.Sprintf("class %s : public %s {};", className, method.Service()))
		initLines = append(initLines, fmt.Sprintf(`gPlayerService.emplace("%s", std::make_unique<%s%s>(std::make_unique<%s>()));`,
			method.Service(), method.Service(), _config.Global.Naming.HandlerFile, className))
	}

	data := PlayerServiceInstanceData{
		SelfHeader:     _config.Global.DirectoryNames.GeneratedRpcName + fileNameWithoutExt + ".h",
		Includes:       includes,
		HandlerClasses: handlerClasses,
		InitLines:      initLines,
	}

	outputPath := handlerDir + serviceName
	if err := utils2.RenderTemplateToFile("internal/template/player_service_instance.cpp.tmpl", outputPath, data); err != nil {
		logger.Global.Fatal("Failed to generate player service instance",
			zap.String("template_path", "internal/template/player_service_instance.cpp.tmpl"),
			zap.String("output_path", outputPath),
			zap.Error(err),
		)
	}
	return ""
}

func writePlayerServiceInstanceFiles(wg *sync.WaitGroup, serviceType string, isPlayerHandlerFunc func(*internal.RPCMethods) bool, handlerDir, serviceName string) {
	defer wg.Done()
	ServiceList := make([]string, 0, len(internal.GlobalRPCServiceList))

	for _, service := range internal.GlobalRPCServiceList {
		ServiceList = append(ServiceList, service.Service())
	}

	var generatorFunc func([]string, func(*internal.RPCMethods) bool, string, string) string

	if serviceType == "instance" {
		generatorFunc = generateInstanceData
	} else {
		logger.Global.Warn("Unknown service instance type, skipping generation",
			zap.String("service_type", serviceType),
			zap.String("service_name", serviceName),
		)
		return // Handle error or unknown type
	}

	generatorFunc(ServiceList, isPlayerHandlerFunc, handlerDir, serviceName)
}

func WriteServiceRegisterInfoFile(wg *sync.WaitGroup) {
	writeEventIdHeaderFiles()
	if err := writeKafkaCommandEventWhitelistHeader(); err != nil {
		logger.Global.Fatal("Failed to generate the Kafka allowlist", zap.Error(err))
	}
	// 头文件(kMaxRpcMethodCount)必须先于 .cpp 同步落盘:若生成中途 Fatal 退出,
	// 只允许出现"头已扩容而 .cpp 未更新"的安全方向(数组偏大无害);反向会让
	// InitMessageInfo 越界写 std::array,节点启动即断言(2026-09-01 事故)。
	writeServiceInfoHeadFile()
	wg.Add(1)
	go writeServiceInfoCppFile(wg)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "instance", IsSceneNodeHostedPlayerProtocolHandler, _config.Global.PathLists.MethodHandlerDirectories.SceneNodePlayer, _config.Global.Naming.PlayerService)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "instance", IsNoOpHandler, _config.Global.PathLists.MethodHandlerDirectories.GateNodePlayer, _config.Global.Naming.PlayerService)
}
