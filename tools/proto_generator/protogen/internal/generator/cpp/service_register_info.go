package cpp

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

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

				// Increment the global message ID counter
				atomic.AddUint64(&internal.MaxMessageId, 1)
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

// ReadMessageIdFile reads message IDs from a file asynchronously.
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

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			splitList := strings.Split(line, "=")
			if len(splitList) != 2 {
				logger.Global.Warn("Invalid line in message ID file, skipping",
					zap.String("line", line),
					zap.String("file_path", _config.Global.Paths.MessageIdFile),
				)
				continue
			}
			id, err := strconv.ParseUint(splitList[0], 10, 64)
			if err != nil {
				logger.Global.Fatal("Failed to parse message ID",
					zap.String("line", line),
					zap.String("file_path", _config.Global.Paths.MessageIdFile),
					zap.Error(err),
				)
			}
			internal.MessageIdMap[splitList[1]] = id
		}

		if err := scanner.Err(); err != nil {
			logger.Global.Fatal("Failed to scan message ID file",
				zap.String("file_path", _config.Global.Paths.MessageIdFile),
				zap.Error(err),
			)
		}
	}()
}

func WriteMessageIdFile() {
	var data string
	var idList []uint64
	for k := range internal.RpcIdMethodMap {
		idList = append(idList, k)
	}
	sort.Slice(idList, func(i, j int) bool { return idList[i] < idList[j] })
	for i := 0; i < len(idList); i++ {
		rpcMethodInfo, ok := internal.RpcIdMethodMap[idList[i]]
		if !ok {
			logger.Global.Warn("Unused message ID",
				zap.Int("index", i),
				zap.Uint64("msg_id", idList[i]),
			)
			continue
		}
		data += strconv.FormatUint(rpcMethodInfo.Id, 10) + "=" + (*rpcMethodInfo).KeyName() + "\n"
	}
	utils2.WriteFileIfChanged(_config.Global.Paths.MessageIdFile, []byte(data))
}

// InitMessageId initializes message IDs based on the loaded service methods and ID mappings.
func InitMessageId() {
	var unUseMessageId = make(map[uint64]internal.EmptyStruct)
	var useMessageId = make(map[uint64]internal.EmptyStruct)

	for _, service := range internal.GlobalRPCServiceList {
		for _, mv := range service.Methods {
			id, ok := internal.MessageIdMap[mv.KeyName()]
			if !ok {
				//Id not found in file means a new message or renamed; new messages handled later
				continue
			}
			if internal.MessageIdFileMaxId < id {
				internal.MessageIdFileMaxId = id
			}
			useMessageId[id] = internal.EmptyStruct{}
			mv.Id = id
		}
	}

	for i := uint64(0); i < internal.MaxMessageId; i++ {
		if _, ok := useMessageId[i]; !ok {
			unUseMessageId[i] = internal.EmptyStruct{}
		}
	}

	for _, service := range internal.GlobalRPCServiceList {
		for _, mv := range service.Methods {
			if len(unUseMessageId) > 0 && mv.Id == math.MaxUint64 {
				for uk := range unUseMessageId {
					mv.Id = uk
					internal.RpcIdMethodMap[mv.Id] = mv
					delete(unUseMessageId, uk)
					break
				}
				continue
			}
			if mv.Id == math.MaxUint64 {
				internal.MessageIdFileMaxId++
				mv.Id = internal.MessageIdFileMaxId
			}
			internal.RpcIdMethodMap[mv.Id] = mv
			if internal.FileMaxMessageId < mv.Id && mv.Id != math.MaxUint64 {
				internal.FileMaxMessageId = mv.Id
			}
		}
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

// writeServiceInfoCppFile generates C++ code that initializes gRPC service metadata.
func writeServiceInfoCppFile(wg *sync.WaitGroup) {
	defer wg.Done()

	type ServiceInfoCppData struct {
		Includes             []string
		ServiceInfoIncludes  []string
		EventIncludesBlock   string
		HandlerClasses       []string
		EventDispatchCases   []string
		InitLines            []string
		ClientMessageIdCases []string
		MessageIdArraySize   int
		EventIdArraySize     int
		SenderFunctions      []string
	}

	var (
		includes            []string
		serviceInfoIncludes []string
		eventIncludes       []string
		handlerClasses      []string
		eventDispatchCases  []string
		initLines           []string
		clientIdLines       []string
		senderFunction      []string
		includeSet          = make(map[string]struct{})
		serviceIncludeSet   = make(map[string]struct{})
		eventIncludeSet     = make(map[string]struct{})
		handlerClassSet     = make(map[string]struct{})
		senderFunctionSet   = make(map[string]struct{})
		clientIDSet         = make(map[string]struct{})
	)

	// Step 1: Collect headers and handler classes
	for _, service := range internal.GlobalRPCServiceList {
		if len(service.Methods) == 0 {
			continue
		}

		if internal.IsFileBelongToNode(service.Fd, messageoption.NodeType_NODE_DB) {
			continue
		}

		firstMethod := service.Methods[0]

		pbInclude := _config.Global.Naming.IncludeBegin +
			strings.Replace(firstMethod.Path(), _config.Global.Paths.ProtoDir, _config.Global.DirectoryNames.ProtoDirName, 1) +
			firstMethod.PbcHeadName() + "\"\n"
		includes = appendUniqueString(includes, includeSet, pbInclude)
		serviceInfoIncludes = appendUniqueString(serviceInfoIncludes, serviceIncludeSet, firstMethod.ServiceInfoIncludeName())

		if firstMethod.CcGenericServices() {
			handlerClass := fmt.Sprintf(
				"class %sImpl final : public %s {};",
				service.Service(),
				service.Service())
			handlerClasses = appendUniqueString(handlerClasses, handlerClassSet, handlerClass)
		}
	}

	// Step 2: Generate init lines for RpcService and allowed client message IDs
	for _, service := range internal.GlobalRPCServiceList {
		if len(service.Methods) == 0 {
			continue
		}

		if internal.IsFileBelongToNode(service.Fd, messageoption.NodeType_NODE_DB) {
			continue
		}

		// Add service group separator for readability
		if len(initLines) > 0 {
			initLines = append(initLines, "")
		}
		initLines = append(initLines, fmt.Sprintf("// --- %s ---", service.Service()))

		for _, method := range service.Methods {
			messageId := method.KeyName() + _config.Global.Naming.MessageId

			isClientMessage := internal.IsClientProtocolService(service.ServiceDescriptorProto)
			nodeType := fmt.Sprintf("%s::%sNodeService", internal.NodeEnumCppQualifiedType, service.NodeServiceForCpp())

			// Emit multi-line RpcMethodMeta initialization grouped by field role:
			//   line 1: assignment + opening brace
			//   line 2: serviceName, methodName
			//   line 3: requestProto
			//   line 4: responseProto
			//   line 5: handler, protocol, targetNodeType [, sender] + closing brace
			initLines = append(initLines, fmt.Sprintf("gRpcMethodRegistry[%s] = RpcMethodMeta{", messageId))
			initLines = append(initLines, fmt.Sprintf(`    "%s", "%s",`, method.Service(), method.Method()))
			initLines = append(initLines, fmt.Sprintf("    std::make_unique<%s>(),", method.CppRequest()))

			if method.CcGenericServices() {
				handler := service.Service() + "Impl"
				initLines = append(initLines, fmt.Sprintf("    std::make_unique<%s>(),", method.CppResponse()))
				initLines = append(initLines, fmt.Sprintf(
					"    std::make_unique<%s>(), %d, %s};",
					handler, GetProtocol(method.Path()), nodeType))
			} else {
				// 空 package 时不能包 namespace:"namespace {...}" 是匿名命名空间,
				// 声明会与其他编译单元中的全局定义失配(LNK2019,先例:battle 域 proto 无 package)。
				// 空则直接全局声明,引用名退化为 ::SendXxx。
				sendSignature := "void Send" + service.Service() + method.Method() +
					"(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );"
				declareFunction := sendSignature
				if method.CppPackage() != "" {
					declareFunction = "namespace " + method.CppPackage() + "{" + sendSignature + "}"
				}
				senderFunction = appendUniqueString(senderFunction, senderFunctionSet, declareFunction)
				sendName := method.CppPackage() + "::" + "Send" + service.Service() + method.Method()
				initLines = append(initLines, fmt.Sprintf("    std::make_unique<%s>(),", method.CppResponse()))
				initLines = append(initLines, fmt.Sprintf(
					"    nullptr, %d, %s, %s};",
					GetProtocol(method.Path()), nodeType, sendName))
			}

			if isClientMessage {
				clientIdLines = appendUniqueString(clientIdLines, clientIDSet, fmt.Sprintf("case %s:", messageId))
			}
		}
	}

	for _, event := range globalProtoEventList {
		eventIncludes = appendUniqueString(eventIncludes, eventIncludeSet, fmt.Sprintf("#include \"%s\"", event.ProtoInclude))
		eventIdRef := event.IdName + _config.Global.Naming.EventId
		eventDispatchCases = append(eventDispatchCases,
			fmt.Sprintf("case %s: {", eventIdRef),
			fmt.Sprintf("\t%s event;", event.QualifiedName),
			"\tif (!event.ParseFromString(payload)) {",
			"\t\treturn false;",
			"\t}",
			"\ttlsEcs.dispatcher.trigger(event);",
			"\treturn true;",
			"}",
		)
	}

	// Step 3: Fill template data and render
	tmplData := ServiceInfoCppData{
		Includes:             includes,
		ServiceInfoIncludes:  serviceInfoIncludes,
		EventIncludesBlock:   strings.Join(eventIncludes, "\n") + "\n" + strings.Join(EventIdHeaderIncludes(), "\n") + "\n",
		HandlerClasses:       handlerClasses,
		EventDispatchCases:   eventDispatchCases,
		InitLines:            initLines,
		ClientMessageIdCases: clientIdLines,
		MessageIdArraySize:   int(internal.MessageIdLen()),
		EventIdArraySize:     int(EventIdLen()),
		SenderFunctions:      senderFunction,
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
func writeServiceInfoHeadFile(wg *sync.WaitGroup) {
	defer wg.Done()
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

// Helper function to generate instance data for player services.
func generateRepliedInstanceData(serviceList []string, isPlayerHandlerFunc func(*internal.RPCMethods) bool, handlerDir string, serviceName string) string {
	type PlayerServiceRepliedInstanceData struct {
		SelfHeader     string
		Includes       []string
		HandlerClasses []string
		InitLines      []string
	}

	fileNameWithoutExt := serviceName[:len(serviceName)-len(filepath.Ext(serviceName))]

	var includes, handlerClasses, initLines []string

	for _, service := range internal.GlobalRPCServiceList {
		if !isPlayerHandlerFunc(&service.Methods) {
			continue
		}

		method := service.Methods[0]
		className := method.Service() + "Impl"

		includes = append(includes, fmt.Sprintf(`#include "%s%s"`, method.FileBaseNameNoEx(), _config.Global.FileExtensions.RepliedHandlerH))
		handlerClasses = append(handlerClasses, fmt.Sprintf("class %s : public %s {};", className, method.Service()))
		initLines = append(initLines, fmt.Sprintf(`gPlayerServiceReplied.emplace("%s", std::make_unique<%s%s>(std::make_unique<%s>()));`,
			method.Service(), method.Service(), _config.Global.Naming.RepliedHandlerFile, className))
	}

	templateData := PlayerServiceRepliedInstanceData{
		SelfHeader:     _config.Global.DirectoryNames.GeneratedRpcName + fileNameWithoutExt + ".h",
		Includes:       includes,
		HandlerClasses: handlerClasses,
		InitLines:      initLines,
	}

	outputPath := handlerDir + serviceName
	if err := utils2.RenderTemplateToFile("internal/template/player_service_replied_instance.cpp.tmpl", outputPath, templateData); err != nil {
		logger.Global.Fatal("Failed to generate player replied service instance",
			zap.String("template_path", "internal/template/player_service_replied_instance.cpp.tmpl"),
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
	} else if serviceType == "repliedInstance" {
		generatorFunc = generateRepliedInstanceData
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
	wg.Add(1)
	go writeServiceInfoCppFile(wg)
	wg.Add(1)
	go writeServiceInfoHeadFile(wg)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "instance", IsSceneNodeHostedPlayerProtocolHandler, _config.Global.PathLists.MethodHandlerDirectories.SceneNodePlayer, _config.Global.Naming.PlayerService)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "repliedInstance", IsSceneNodeReceivedPlayerResponseHandler, _config.Global.PathLists.MethodHandlerDirectories.SceneNodePlayerReplied, _config.Global.Naming.PlayerRepliedService)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "instance", IsNoOpHandler, _config.Global.PathLists.MethodHandlerDirectories.GateNodePlayer, _config.Global.Naming.PlayerService)
	wg.Add(1)
	go writePlayerServiceInstanceFiles(wg, "repliedInstance", IsNoOpHandler, _config.Global.PathLists.MethodHandlerDirectories.GateNodePlayerReplied, _config.Global.Naming.PlayerRepliedService)
}
