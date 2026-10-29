package cpp

import (
	"fmt"
	"os"
	"path/filepath"
	"protogen/internal"
	_config "protogen/internal/config"
	_proto "protogen/internal/prototools"
	utils2 "protogen/internal/utils"
	"protogen/logger"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// BuildProtocCpp 只生成 Protobuf 消息；标准 gRPC 协议由 muduo 适配器承载，不调用官方插件。
func BuildProtocCpp(wg *sync.WaitGroup) {
	for _, meta := range _config.Global.DomainMeta {
		wg.Add(1)
		go func(meta _config.DomainMeta) {
			defer wg.Done()
			dir := meta.Source
			if err := BuildProtoCpp(wg, dir); err != nil {
				logger.Global.Warn("C++ batch build failed",
					zap.String("dir", dir),
					zap.Error(err),
				)
			}
		}(meta)
	}
}

// BuildProtoCpp generates C++ serialization code for all proto files in the given directory.
func BuildProtoCpp(wg *sync.WaitGroup, protoDir string) error {
	if _, statErr := os.Stat(protoDir); statErr != nil {
		if os.IsNotExist(statErr) {
			logger.Global.Warn("C++ batch generation skipped: proto directory not found",
				zap.String("dir", protoDir),
			)
			return nil
		}
		return statErr
	}

	protoFiles, err := utils2.CollectProtoFiles(protoDir)
	if err != nil {
		return err
	}
	if len(protoFiles) == 0 {
		logger.Global.Info("C++ batch generation skipped: no proto files found",
			zap.String("dir", protoDir),
		)
		return nil
	}

	cppOutputDir := _config.Global.Paths.ProtoBufCProtoOutputDir
	if err := utils2.EnsureDir(cppOutputDir); err != nil {
		return err
	}

	cppTempDir := _config.Global.Paths.ProtoBufCTempDir

	if err := GenerateCpp(protoFiles, cppTempDir); err != nil {
		return err
	}
	if err := CopyCppOutputs(wg, protoFiles, cppTempDir, cppOutputDir); err != nil {
		return err
	}

	return nil
}

// GenerateCpp invokes protoc to generate C++ serialization code.
func GenerateCpp(protoFiles []string, outputDir string) error {
	if err := utils2.EnsureDir(outputDir); err != nil {
		logger.Global.Fatal("C++ generation: failed to create output directory",
			zap.Error(err),
		)
	}

	cppOutputDir := outputDir
	protoRootDir := _config.Global.Paths.OutputRoot
	protoBufferDir := _config.Global.Paths.ProtobufDir

	args := []string{
		fmt.Sprintf("--cpp_out=%s", cppOutputDir),
		fmt.Sprintf("--proto_path=%s", protoRootDir),
		fmt.Sprintf("--proto_path=%s", protoBufferDir),
	}

	for _, file := range protoFiles {
		args = append(args, file)
	}

	return utils2.RunProtoc(args, "generate C++ serialization code")
}

// CopyCppOutputs copies generated C++ files from the temp directory to the destination.
func CopyCppOutputs(wg *sync.WaitGroup, protoFiles []string, tempDir, destDir string) error {
	cppGenTempDir := tempDir
	cppDestDir := destDir
	protoRootDir := _config.Global.Paths.OutputRoot

	for _, protoFile := range protoFiles {
		protoRelPath, err := filepath.Rel(protoRootDir, protoFile)
		if err != nil {
			logger.Global.Warn("C++ copy: failed to compute relative path, skipping",
				zap.String("proto_file", protoFile),
				zap.Error(err),
			)
			continue
		}
		protoRelDir := filepath.Dir(protoRelPath)

		protoFileName := filepath.Base(protoFile)
		headerFile := strings.Replace(protoFileName, _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbH, 1)
		cppFile := strings.Replace(protoFileName, _config.Global.FileExtensions.Proto, _config.Global.FileExtensions.PbCc, 1)

		tempHeaderPath := filepath.Join(cppGenTempDir, protoRelDir, headerFile)
		tempCppPath := filepath.Join(cppGenTempDir, protoRelDir, cppFile)
		destHeaderPath := filepath.Join(cppDestDir, protoRelDir, headerFile)
		destCppPath := filepath.Join(cppDestDir, protoRelDir, cppFile)

		if err := utils2.EnsureDir(filepath.Dir(destHeaderPath)); err != nil {
			logger.Global.Warn("C++ copy: failed to create destination directory, skipping",
				zap.String("dest_dir", filepath.Dir(destHeaderPath)),
				zap.Error(err),
			)
			continue
		}

		utils2.CopyFileIfChangedAsync(wg, tempHeaderPath, destHeaderPath)
		utils2.CopyFileIfChangedAsync(wg, tempCppPath, destCppPath)
	}
	return nil
}

// generateGameGrpcCpp generates game gRPC C++ code.
func generateGameGrpcCpp(wg *sync.WaitGroup, protoFiles []string) error {
	wg.Add(1)
	go func() {
		defer wg.Done()

		cppOutputDir := _config.Global.Paths.ProtoBufCProtoOutputDir
		if err := utils2.EnsureDir(cppOutputDir); err != nil {
			logger.Global.Fatal("Failed to create C++ output directory",
				zap.Error(err),
			)
		}

		cppTempDir := _config.Global.Paths.ProtoBufCTempDir

		if err := GenerateCpp(protoFiles, cppTempDir); err != nil {
			logger.Global.Fatal("Failed to generate C++ code",
				zap.Error(err),
			)
		}

		cppDestDir := _config.Global.Paths.GrpcOutputDir

		if err := CopyCppOutputs(wg, protoFiles, cppTempDir, cppDestDir); err != nil {
			logger.Global.Fatal("Failed to copy C++ code",
				zap.Error(err),
			)
		}

	}()

	return nil
}

// generateGameGrpcImpl contains the core logic for game gRPC generation.
func generateGameGrpcImpl(wg *sync.WaitGroup) error {
	gameProtoPath, err := _proto.ResolveGameProtoPath()
	if err != nil {
		logger.Global.Fatal("Failed to resolve proto path",
			zap.Error(err),
		)
	}
	protoFiles := []string{gameProtoPath}

	if err := generateGameGrpcCpp(wg, protoFiles); err != nil {
		logger.Global.Fatal("C++ code generation failed",
			zap.Error(err),
		)
	}

	return nil
}

// GenerateGameGrpc generates game gRPC code (C++ serialization + Go node code).
func GenerateGameGrpc(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done() // Fix: defer wg.Done() to ensure it runs regardless of error
		if err := generateGameGrpcImpl(wg); err != nil {
			logger.Global.Warn("Game gRPC generation failed",
				zap.Error(err),
			)
		}
	}()
}

func GeneratorHandler(wg *sync.WaitGroup) {
	for _, service := range internal.GlobalRPCServiceList {
		ProcessAllHandlers(wg, service.Methods)
	}
}

func WriteMethodFile(wg *sync.WaitGroup) {
	// Concurrent operations for game and gate registers
	// 节点间 RPC0 的全局回包表已退场(docs/design/muduo-rpc-channel.md §9.2),不再生成 rpc_replies/register_response_handler.cpp 的 InitReply。
	wg.Add(1)
	go internal.GenRegisterFile(wg, _config.Global.PathLists.MethodHandlerDirectories.SceneNode+_config.Global.FileExtensions.RegisterHandlerCppExtension, IsSceneNodeHostedProtocolHandler)

	wg.Add(1)
	go internal.GenRegisterFile(wg, _config.Global.PathLists.MethodHandlerDirectories.GateNodeReplied+_config.Global.FileExtensions.RegisterHandlerCppExtension, IsNoOpHandler)
}
