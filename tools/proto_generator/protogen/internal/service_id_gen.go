package internal

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	_config "protogen/internal/config"
	"protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 模板与工具同批编译,运行时不依赖工作目录定位模板(同 grpc_handler_gen.go)。
//
//go:embed template/service_metadata.h.tmpl
var serviceMetadataHeaderTemplate string

// nodeRpcMethodTagSuffix 与 <KeyName>MessageId 同一命名族;不用 <KeyName>Method,那是现有宏
// (bag_test 在用),也不按数字特化 Traits<Id>(docs/design/muduo-rpc-channel.md §10)。
const nodeRpcMethodTagSuffix = "Rpc"

// serviceMetadataHeaderData 是 <file>_service_metadata.h 的模板数据。
type serviceMetadataHeaderData struct {
	IncludeName   string
	ServiceInfo   []*RPCServiceInfo
	MessageIdName string
	// 只在文件里有节点间 RPC0 服务(hasNodeRpcMethodTags)时非空;其余文件的输出与旧版逐字节相同。
	ForwardDecls []cppForwardDeclGroup
	MethodTags   []nodeRpcMethodTag
}

// cppForwardDeclGroup 是同一 C++ 命名空间下要前向声明的消息类。
type cppForwardDeclGroup struct {
	Namespace string // 空串 = 全局命名空间
	Classes   []string
}

// nodeRpcMethodTag 是一个节点间 RPC0 方法的类型安全调用面。
type nodeRpcMethodTag struct {
	TagName        string // <KeyName>Rpc
	MessageIdConst string // <KeyName>MessageId,即同一头文件里的消息号常量
	Request        string // 全限定 C++ 类名,如 ::NodeHandshakeRequest
	Response       string
}

// cppMessageRef 是 proto 消息在 C++ 里的位置。
type cppMessageRef struct {
	Namespace string // proto package 的 :: 形式;空串 = 全局命名空间
	ClassName string // 嵌套消息按 protoc 规则拼成 Outer_Inner:只有这个名字能在命名空间作用域前向声明
}

func (ref cppMessageRef) QualifiedName() string {
	if ref.Namespace == "" {
		return "::" + ref.ClassName
	}
	return "::" + ref.Namespace + "::" + ref.ClassName
}

// hasNodeRpcMethodTags 判定服务的方法是否由节点间 RPC0(RpcChannel)直接收发:cc_generic_services 文件里、
// 既不是玩家服务也不是客户端协议服务的那些(今天是 Gate、Scene、SceneScene)。
// 玩家服务与客户端协议服务的消息只作为内层载荷经 Scene.ProcessClientPlayerMessage 等派发,不生成 tag。
func hasNodeRpcMethodTags(info *RPCServiceInfo) bool {
	return info.CcGenericServices() &&
		!IsPlayerService(info.ServiceDescriptorProto) &&
		!IsClientProtocolService(info.ServiceDescriptorProto)
}

// GenServiceIdHeader generates the service ID header file content.
// 请求 / 响应类型按全局描述符集合 FdSet 拆出命名空间与类名(GenerateAllInOneDescriptor 在此之前已加载完)。
func GenServiceIdHeader(serviceInfo []*RPCServiceInfo) (string, error) {
	return renderServiceMetadataHeader(serviceInfo, FdSet.GetFile())
}

// renderServiceMetadataHeader 是 GenServiceIdHeader 的实现;描述符文件显式传入,便于测试。
func renderServiceMetadataHeader(serviceInfo []*RPCServiceInfo, files []*descriptorpb.FileDescriptorProto) (string, error) {
	if len(serviceInfo) == 0 {
		return "", nil
	}

	messageIdName := _config.Global.Naming.MessageId
	forwardDecls, methodTags, err := buildNodeRpcMethodTags(serviceInfo, files, messageIdName)
	if err != nil {
		return "", err
	}

	data := serviceMetadataHeaderData{
		IncludeName:   serviceInfo[0].IncludeName(),
		ServiceInfo:   serviceInfo,
		MessageIdName: messageIdName,
		ForwardDecls:  forwardDecls,
		MethodTags:    methodTags,
	}

	return utils.GlobalEngine.Render("serviceMetadataHeader", serviceMetadataHeaderTemplate, data)
}

// buildNodeRpcMethodTags 按方法在文件里的顺序产出 tag,并汇总去重、排序后的前向声明(输出稳定,便于 diff)。
func buildNodeRpcMethodTags(serviceInfo []*RPCServiceInfo, files []*descriptorpb.FileDescriptorProto, messageIdName string) ([]cppForwardDeclGroup, []nodeRpcMethodTag, error) {
	classesByNamespace := map[string]map[string]struct{}{}
	var tags []nodeRpcMethodTag

	for _, service := range serviceInfo {
		if !hasNodeRpcMethodTags(service) {
			continue
		}
		for _, method := range service.Methods {
			request, err := resolveCppMessageRef(files, method.MethodDescriptorProto.GetInputType())
			if err != nil {
				return nil, nil, fmt.Errorf("method %s request type: %w", method.KeyName(), err)
			}
			response, err := resolveCppMessageRef(files, method.MethodDescriptorProto.GetOutputType())
			if err != nil {
				return nil, nil, fmt.Errorf("method %s response type: %w", method.KeyName(), err)
			}

			for _, ref := range []cppMessageRef{request, response} {
				if classesByNamespace[ref.Namespace] == nil {
					classesByNamespace[ref.Namespace] = map[string]struct{}{}
				}
				classesByNamespace[ref.Namespace][ref.ClassName] = struct{}{}
			}

			tags = append(tags, nodeRpcMethodTag{
				TagName:        method.KeyName() + nodeRpcMethodTagSuffix,
				MessageIdConst: method.KeyName() + messageIdName,
				Request:        request.QualifiedName(),
				Response:       response.QualifiedName(),
			})
		}
	}

	return sortedForwardDeclGroups(classesByNamespace), tags, nil
}

// sortedForwardDeclGroups 把 命名空间 → 类名集合 展开成有序列表:全局命名空间(空串)排最前。
func sortedForwardDeclGroups(classesByNamespace map[string]map[string]struct{}) []cppForwardDeclGroup {
	namespaces := make([]string, 0, len(classesByNamespace))
	for namespace := range classesByNamespace {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)

	groups := make([]cppForwardDeclGroup, 0, len(namespaces))
	for _, namespace := range namespaces {
		classes := make([]string, 0, len(classesByNamespace[namespace]))
		for className := range classesByNamespace[namespace] {
			classes = append(classes, className)
		}
		sort.Strings(classes)
		groups = append(groups, cppForwardDeclGroup{Namespace: namespace, Classes: classes})
	}
	return groups
}

// resolveCppMessageRef 按 proto 全名(".pkg.Outer.Inner")在描述符集合里找到声明它的文件,
// 用该文件的 package 区分「命名空间段」与「嵌套消息段」——只看全名字符串分不出这两者。
// protoc 已校验类型存在,找不到只能是描述符集合不完整,按错误返回而不是猜一个名字。
func resolveCppMessageRef(files []*descriptorpb.FileDescriptorProto, protoFullName string) (cppMessageRef, error) {
	fullName := strings.TrimPrefix(protoFullName, ".")
	if fullName == "" {
		return cppMessageRef{}, fmt.Errorf("empty message type name")
	}

	for _, file := range files {
		packageName := file.GetPackage()
		relativeName := fullName
		if packageName != "" {
			if !strings.HasPrefix(fullName, packageName+".") {
				continue
			}
			relativeName = strings.TrimPrefix(fullName, packageName+".")
		}
		if !declaresMessage(file.GetMessageType(), strings.Split(relativeName, ".")) {
			continue
		}
		return cppMessageRef{
			Namespace: strings.ReplaceAll(packageName, ".", "::"),
			ClassName: strings.ReplaceAll(relativeName, ".", "_"),
		}, nil
	}

	return cppMessageRef{}, fmt.Errorf("message %q not found in descriptor set", protoFullName)
}

// declaresMessage 判断消息列表里是否沿 path(外层到内层的短名)声明了该消息。
func declaresMessage(messages []*descriptorpb.DescriptorProto, path []string) bool {
	for _, message := range messages {
		if message.GetName() != path[0] {
			continue
		}
		if len(path) == 1 {
			return true
		}
		return declaresMessage(message.GetNestedType(), path[1:])
	}
	return false
}

// writeServiceIdHeadFile writes the service ID header file.
func writeServiceIdHeadFile(serviceInfo []*RPCServiceInfo) {
	if len(serviceInfo) == 0 {
		return
	}

	fileName := serviceInfo[0].ServiceInfoHeadInclude()
	outputPath := _config.Global.Paths.ServiceInfoDir + fileName
	content, err := GenServiceIdHeader(serviceInfo)
	if err != nil {
		logger.Global.Fatal("Failed to generate service ID header file",
			zap.String("file_path", outputPath),
			zap.Error(err),
		)
	}
	if err := utils.WriteFileIfChanged(outputPath, []byte(content)); err != nil {
		logger.Global.Fatal("Failed to write the service message ID header file", zap.String("file_path", outputPath), zap.Error(err))
		return
	}

	logger.Global.Info("Service ID header file generated",
		zap.String("file_path", outputPath),
		zap.Int("service_count", len(serviceInfo)),
	)
}
