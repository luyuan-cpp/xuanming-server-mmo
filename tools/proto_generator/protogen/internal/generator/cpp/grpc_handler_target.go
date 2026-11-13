package cpp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// GenerateGrpcHandler 从单个源契约生成最新描述，再复用常规 handler 路由；不运行其他语言、客户端、注册表或发号步骤。
func GenerateGrpcHandler(protoPath string) error {
	sourcePath := protoPath
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(_config.Global.Paths.OutputRoot, sourcePath)
	}
	relativePath, err := filepath.Rel(_config.Global.Paths.OutputRoot, sourcePath)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || filepath.Ext(relativePath) != ".proto" {
		return fmt.Errorf("gRPC handler 源契约必须是当前仓库内的 proto 文件: %s", protoPath)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("读取 gRPC handler 源契约失败: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(_config.Global.Paths.OutputRoot)
	if err != nil {
		return err
	}
	resolvedSource, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return err
	}
	resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedSource)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("gRPC handler 源契约解析到了当前仓库之外: %s", protoPath)
	}
	protoPath = filepath.ToSlash(relativePath)
	tempDir, err := os.MkdirTemp("", "proto-gen-grpc-handler-")
	if err != nil {
		return fmt.Errorf("创建临时描述目录失败: %w", err)
	}
	defer func() {
		if err := removeGrpcHandlerTempDir(tempDir); err != nil {
			logger.Global.Warn("Failed to clean up the temporary gRPC descriptor directory", zap.String("path", tempDir), zap.Error(err))
		}
	}()
	descriptorPath := filepath.Join(tempDir, "handler.desc")
	args := []string{"--descriptor_set_out=" + filepath.ToSlash(descriptorPath), "--include_imports"}
	for _, includePath := range []string{_config.Global.Paths.OutputRoot, _config.Global.Paths.ProtobufDir} {
		if includePath != "" {
			args = append(args, "--proto_path="+filepath.ToSlash(includePath))
		}
	}
	args = append(args, protoPath)
	if err := utils.RunProtoc(args, "生成单个 gRPC handler 的描述"); err != nil {
		return err
	}
	data, err := os.ReadFile(descriptorPath)
	if err != nil {
		return fmt.Errorf("读取新生成的 gRPC 描述失败: %w", err)
	}
	return generateGrpcHandlerFromDescriptor(protoPath, data)
}

func generateGrpcHandlerFromDescriptor(protoPath string, data []byte) error {
	var descriptorSet descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &descriptorSet); err != nil {
		return fmt.Errorf("解析 gRPC 描述失败: %w", err)
	}
	var targetMethods internal.RPCMethods
	for _, file := range descriptorSet.GetFile() {
		if file.GetName() != protoPath {
			continue
		}
		for _, service := range file.GetService() {
			if internal.IsClientProtocolService(service) || internal.IsPlayerService(service) {
				continue
			}
			var methods internal.RPCMethods
			for index, method := range service.GetMethod() {
				methods = append(methods, &internal.MethodInfo{
					ProtoFileInfo: internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service},
					Index:         uint64(index), MethodDescriptorProto: method,
				})
			}
			if len(methods) == 0 || (!IsBattleNodeGrpcHandler(&methods) && !IsSceneNodeGrpcHandler(&methods)) {
				continue
			}
			if targetMethods != nil {
				return fmt.Errorf("同一文件包含多个可生成的 gRPC 服务，handler 输出会冲突: %s", protoPath)
			}
			targetMethods = methods
		}
	}
	if len(targetMethods) == 0 {
		return fmt.Errorf("源契约没有配置可生成的内部 gRPC handler: %s", protoPath)
	}
	directory := _config.Global.PathLists.MethodHandlerDirectories.SceneNodeGrpc
	if IsBattleNodeGrpcHandler(&targetMethods) {
		directory = _config.Global.PathLists.MethodHandlerDirectories.BattleNodeGrpc
	}
	if directory == "" {
		return fmt.Errorf("内部 gRPC handler 输出目录未配置: %s", protoPath)
	}
	// 并行写入之前先完整渲染两份输入；任一守护段非法时不能先覆盖另一份文件。
	first := targetMethods[0]
	headerPath := directory + first.FileBaseNameNoEx() + _config.Global.FileExtensions.GrpcHandlerH
	cppPath := directory + strings.ToLower(first.FileBaseNameNoEx()) + _config.Global.FileExtensions.GrpcHandlerCpp
	if _, err := internal.GetGrpcServiceHandlerHeadFromFileStr(headerPath, targetMethods); err != nil {
		return err
	}
	_ = internal.GetGrpcServiceHandlerCppStr(cppPath, targetMethods, "", "")
	var wg sync.WaitGroup
	ProcessAllHandlers(&wg, targetMethods)
	wg.Wait()
	return nil
}

func removeGrpcHandlerTempDir(tempDir string) error {
	tempRoot, err := filepath.Abs(os.TempDir())
	if err != nil {
		return err
	}
	target, err := filepath.Abs(tempDir)
	if err != nil {
		return err
	}
	tempRoot, err = filepath.EvalSymlinks(tempRoot)
	if err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	relativePath, err := filepath.Rel(tempRoot, target)
	if err != nil || filepath.Dir(relativePath) != "." || !strings.HasPrefix(relativePath, "proto-gen-grpc-handler-") {
		return fmt.Errorf("拒绝清理临时目录之外的路径: %s", tempDir)
	}
	return os.RemoveAll(target)
}
