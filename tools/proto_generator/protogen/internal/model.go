package internal

import (
	"path"
	"path/filepath"
	_config "protogen/internal/config"
	"strings"
	"sync"

	"github.com/iancoleman/strcase"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type EmptyStruct struct{}

// ProtoFileInfo holds common proto file and service descriptor fields.
type ProtoFileInfo struct {
	Fd                     *descriptorpb.FileDescriptorProto
	ServiceDescriptorProto *descriptorpb.ServiceDescriptorProto
}

func (info *ProtoFileInfo) FileName() string {
	return *info.Fd.Name
}

func (info *ProtoFileInfo) FileBaseName() string {
	return filepath.Base(*info.Fd.Name)
}

func (info *ProtoFileInfo) FileBaseNameNoEx() string {
	return strings.Replace(info.FileBaseName(), _config.Global.FileExtensions.Proto, "", 1)
}

func (info *ProtoFileInfo) FileBaseNameCamel() string {
	base := strings.Replace(info.FileBaseName(), _config.Global.FileExtensions.Proto, "", 1)
	base = strings.ReplaceAll(base, "_", " ")
	base = cases.Title(language.English).String(base)
	base = strings.ReplaceAll(base, " ", "")
	return base
}

func (info *ProtoFileInfo) Path() string {
	return strings.Replace(filepath.Dir(*info.Fd.Name), "\\", "/", -1) + "/"
}

func (info *ProtoFileInfo) PbcHeadName() string {
	return strings.Replace(info.FileBaseName(), _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbH, 1)
}

func (info *ProtoFileInfo) GrpcHeadName() string {
	return strings.Replace(info.FileBaseName(), _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.GrpcPbH, 1)
}

func (info *ProtoFileInfo) GrpcIncludeHeadName() string {
	return _config.Global.Naming.IncludeBegin + strings.Replace(info.Path(), _config.Global.Paths.ProtoDir, _config.Global.DirectoryNames.ProtoDirName, 1) + info.GrpcHeadName() + "\"\n"
}

func (info *ProtoFileInfo) Package() string {
	if nil == info.Fd.Package {
		return ""
	}
	return *info.Fd.Package
}

// CppPackage returns the proto package name with dots converted to :: for use as C++ namespace.
func (info *ProtoFileInfo) CppPackage() string {
	return strings.ReplaceAll(info.Package(), ".", "::")
}

func (info *ProtoFileInfo) Service() string {
	return info.ServiceDescriptorProto.GetName()
}

func (info *ProtoFileInfo) CcGenericServices() bool {
	if info.Fd.Options == nil {
		return false
	}
	if info.Fd.Options.CcGenericServices == nil {
		return false
	}
	return *info.Fd.Options.CcGenericServices
}

func (info *ProtoFileInfo) GetServiceFullNameWithNoColon() string {
	if len(info.Package()) <= 0 {
		return info.Service()
	}
	return info.CppPackage() + "::" + info.Service()
}

func (info *ProtoFileInfo) GeneratorGrpcFileName() string {
	return info.FileBaseNameNoEx() + _config.Global.FileExtensions.GrpcClient
}

func (info *ProtoFileInfo) ServiceInfoIncludeName() string {
	return _config.Global.Naming.IncludeBegin + _config.Global.DirectoryNames.GeneratedRpcName + _config.Global.DirectoryNames.ServiceInfoName + info.FileBaseNameNoEx() + _config.Global.FileExtensions.ServiceInfoExtension + _config.Global.FileExtensions.Header + "\"\n"
}

func (info *ProtoFileInfo) ServiceInfoHeadInclude() string {
	return info.FileBaseNameNoEx() + _config.Global.FileExtensions.ServiceInfoExtension + _config.Global.FileExtensions.Header
}

// MethodInfo defines an RPC method.
type MethodInfo struct {
	ProtoFileInfo
	Id                    uint64
	Index                 uint64
	MethodDescriptorProto *descriptorpb.MethodDescriptorProto
}

// RPCMethods is a slice of MethodInfo pointers.
type RPCMethods []*MethodInfo

// RPCServiceInfo defines an RPC service with its methods.
type RPCServiceInfo struct {
	ProtoFileInfo
	Methods      RPCMethods
	ServiceIndex uint32
}

var FdSet = &descriptorpb.FileDescriptorSet{}

// RPCServiceInfoList supports sorting by ServiceIndex.
type RPCServiceInfoList []*RPCServiceInfo

func (s RPCServiceInfoList) Len() int           { return len(s) }
func (s RPCServiceInfoList) Less(i, j int) bool { return s[i].ServiceIndex < s[j].ServiceIndex }
func (s RPCServiceInfoList) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

var GlobalRPCServiceList RPCServiceInfoList
var FileServiceMap sync.Map

var RpcIdMethodMap = map[uint64]*MethodInfo{}
var MessageIdMap = map[string]uint64{}
var MaxMessageId = uint64(0)
var MessageIdFileMaxId = uint64(0)
var FileMaxMessageId = uint64(0)

// NodeEnumCppQualifiedType holds the C++ fully qualified eNodeType name (e.g. "common::base::eNodeType").
// Set during GenNodeUtil when the enum is discovered.
var NodeEnumCppQualifiedType string

// NodeEnumValueSet holds the set of all eNodeType enum value names (e.g. "SceneNodeService").
// Populated during GenNodeUtil.
var NodeEnumValueSet = map[string]bool{}

var (
	ActiveMsgDescCache = make(map[protoreflect.FullName]protoreflect.MessageDescriptor)
	FileDescCache      = make(map[string]protoreflect.FileDescriptor)
)

func (info *RPCServiceInfo) IncludeName() string {
	return "#include \"" + strings.Replace(info.Path(), _config.Global.Paths.ProtoDir, "", 1) + info.PbcHeadName() + "\"\n"
}

func (info *RPCServiceInfo) BasePathForCpp() string {
	return strcase.ToCamel(path.Base(info.Path()))
}

// NodeServiceForCpp resolves the eNodeType enum prefix for this service.
// It first checks if {ServiceName}NodeService exists in the enum; if so, uses the service name.
// Then tries {PackageName}NodeService (CamelCase) from the proto package declaration.
// Otherwise, it falls back to the directory-based derivation.
func (info *RPCServiceInfo) NodeServiceForCpp() string {
	// 1. Try service name directly: e.g. SceneManager -> SceneManagerNodeService
	candidate := info.Service() + "NodeService"
	if NodeEnumValueSet[candidate] {
		return info.Service()
	}
	// 2. Try proto package name: e.g. scene_node -> SceneNode -> SceneNodeNodeService
	pkgCamel := strcase.ToCamel(info.Package())
	if pkgCamel != "" {
		candidate = pkgCamel + "NodeService"
		if NodeEnumValueSet[candidate] {
			return pkgCamel
		}
	}
	// 3. Fallback to directory name
	return strcase.ToCamel(path.Base(info.Path()))
}

func (info *RPCServiceInfo) ProtoPathWithFileBaseName() string {
	return info.Path() + info.FileBaseNameNoEx()
}

func (info *RPCServiceInfo) LogicalPath() string {
	return strings.TrimPrefix(info.ProtoPathWithFileBaseName(), "proto/")
}

func (info *MethodInfo) KeyName() string {
	return info.Service() + info.Method()
}

func (info *MethodInfo) Method() string {
	return info.MethodDescriptorProto.GetName()
}

func (info *MethodInfo) IncludeName() string {
	if info.CcGenericServices() {
		return _config.Global.Naming.IncludeBegin + strings.Replace(info.Path(), _config.Global.Paths.ProtoDir, _config.Global.DirectoryNames.ProtoDirName, 1) + info.PbcHeadName() + "\"\n"
	}
	return info.GrpcIncludeHeadName()
}

func (info *MethodInfo) CppHandlerIncludeName() string {
	return _config.Global.Naming.IncludeBegin + info.FileBaseNameNoEx() + _config.Global.FileExtensions.HandlerH + _config.Global.Naming.IncludeEndLine
}

func (info *MethodInfo) CppRepliedHandlerIncludeName() string {
	return _config.Global.Naming.IncludeBegin + info.FileBaseNameNoEx() + _config.Global.FileExtensions.RepliedHandlerHeaderExtension + _config.Global.Naming.IncludeEndLine
}

func (info *MethodInfo) CppHandlerClassName() string {
	return info.Service() + _config.Global.Naming.HandlerFile
}

func (info *MethodInfo) CppRepliedHandlerClassName() string {
	return info.Service() + _config.Global.Naming.RepliedHandlerFile
}

func (info *MethodInfo) GetServiceFullNameWithColon() string {
	if len(info.Package()) <= 0 {
		return info.Service()
	}
	return info.CppPackage() + "::" + info.Service()
}

func (info *MethodInfo) CppRequest() string {
	inputType := info.MethodDescriptorProto.GetInputType()
	return strings.Replace(inputType, ".", "::", -1)
}

func (info *MethodInfo) CppResponse() string {
	outputType := info.MethodDescriptorProto.GetOutputType()
	return strings.Replace(outputType, ".", "::", -1)
}

func (info *MethodInfo) GoRequest() string {
	fullType := strings.TrimPrefix(info.MethodDescriptorProto.GetInputType(), ".")
	typeName := fullType
	if idx := strings.LastIndex(fullType, "."); idx >= 0 {
		typeName = fullType[idx+1:]
	}
	var fileDir string
	if activeMsgDesc, ok := ActiveMsgDescCache[protoreflect.FullName(fullType)]; ok {
		filePath := activeMsgDesc.ParentFile().Path()
		fileDir = filepath.Base(filepath.Dir(filePath)) + "."
	}
	return fileDir + typeName
}

func (info *MethodInfo) GoResponse() string {
	fullType := strings.TrimPrefix(info.MethodDescriptorProto.GetOutputType(), ".")
	typeName := fullType
	if idx := strings.LastIndex(fullType, "."); idx >= 0 {
		typeName = fullType[idx+1:]
	}
	var fileDir string
	if activeMsgDesc, ok := ActiveMsgDescCache[protoreflect.FullName(fullType)]; ok {
		filePath := activeMsgDesc.ParentFile().Path()
		fileDir = filepath.Base(filepath.Dir(filePath)) + "."
	}
	return fileDir + typeName
}

// goProtoImportDir 把 descriptor 路径转成 proto 模块内的包目录。
// ParentFile().Path() 取决于 protoc 的 -I 根:以仓库根加载时带 "proto/"
// 前缀,以 proto 根加载时不带。消费模板统一硬拼 "proto/<dir>",这里必须
// 剥掉可能存在的前缀,否则产出 "proto/proto/battle" 双前缀 import
// (2026-09-01 五个 robot 观战 stub 因此编译失败,且 stub 存在即跳过、
// 重跑生成器也不会自愈)。
func goProtoImportDir(filePath string) string {
	dir := filepath.ToSlash(filepath.Dir(filePath))
	if dir == "proto" {
		return ""
	}
	return strings.TrimPrefix(dir, "proto/")
}

// GoResponseImportDir returns the proto file's relative directory path for the
// response type (e.g. "chat", "common/base", "scene"). Used to construct the
// correct Go import path: "robot/proto/" + GoResponseImportDir().
func (info *MethodInfo) GoResponseImportDir() string {
	fullType := strings.TrimPrefix(info.MethodDescriptorProto.GetOutputType(), ".")
	if activeMsgDesc, ok := ActiveMsgDescCache[protoreflect.FullName(fullType)]; ok {
		return goProtoImportDir(activeMsgDesc.ParentFile().Path())
	}
	return ""
}

// GoRequestImportDir returns the proto file's relative directory path for the
// request type.
func (info *MethodInfo) GoRequestImportDir() string {
	fullType := strings.TrimPrefix(info.MethodDescriptorProto.GetInputType(), ".")
	if activeMsgDesc, ok := ActiveMsgDescCache[protoreflect.FullName(fullType)]; ok {
		return goProtoImportDir(activeMsgDesc.ParentFile().Path())
	}
	return ""
}

func (info *MethodInfo) RequestName() string {
	return GetTypeName(info.MethodDescriptorProto.GetInputType())
}

func (info *MethodInfo) ClientStreaming() bool {
	if nil == info.MethodDescriptorProto.ClientStreaming {
		return false
	}
	return *info.MethodDescriptorProto.ClientStreaming
}

func (info *MethodInfo) ServerStreaming() bool {
	if nil == info.MethodDescriptorProto.ServerStreaming {
		return false
	}
	return *info.MethodDescriptorProto.ServerStreaming
}
func (s RPCMethods) Len() int { return len(s) }
func (s RPCMethods) Less(i, j int) bool {
	if s[i].Service() < s[j].Service() {
		return true
	}
	return s[i].Index < s[j].Index
}
func (s RPCMethods) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func GetTypeName(fullTypeName string) string {
	if idx := strings.LastIndex(fullTypeName, "."); idx != -1 {
		return fullTypeName[idx+1:]
	}
	return fullTypeName
}

func MessageIdLen() uint64 {
	return FileMaxMessageId + 1
}
