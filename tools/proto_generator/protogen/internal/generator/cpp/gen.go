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
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// BuildProtocCpp 只生成 Protobuf 消息；标准 gRPC 协议由 muduo 适配器承载，不调用官方插件。
// 将所有域的去重 .proto 按文件大小均衡切分到不超过 protoc 并发上限的批次，
// 各批次写独立临时子目录并行调用 protoc，消除按 26 域重复解析 import 与同目录写入竞争。
func BuildProtocCpp(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		protoFiles, err := collectDomainCppProtoFiles(_config.Global.DomainMeta)
		if err != nil {
			logger.Global.Warn("C++ batch build failed during proto collection",
				zap.Error(err),
			)
			return
		}
		if len(protoFiles) == 0 {
			logger.Global.Info("C++ batch generation skipped: no proto files found")
			return
		}

		cppOutputDir := _config.Global.Paths.ProtoBufCProtoOutputDir
		if err := utils2.EnsureDir(cppOutputDir); err != nil {
			logger.Global.Fatal("C++ generation: failed to create output directory",
				zap.String("dir", cppOutputDir),
				zap.Error(err),
			)
		}
		cppTempDir := _config.Global.Paths.ProtoBufCTempDir
		if err := utils2.EnsureDir(cppTempDir); err != nil {
			logger.Global.Fatal("C++ generation: failed to create temp directory",
				zap.String("dir", cppTempDir),
				zap.Error(err),
			)
		}

		shards := shardProtoFilesBySize(protoFiles, utils2.ProtocConcurrency(), fileSizeOrZero)
		for _, shard := range shards {
			shardFiles := shard
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := buildProtoCppShard(shardFiles, cppTempDir, cppOutputDir); err != nil {
					logger.Global.Warn("C++ shard build failed",
						zap.Int("file_count", len(shardFiles)),
						zap.Error(err),
					)
				}
			}()
		}
	}()
}

// buildProtoCppShard 在独占临时子目录运行 protoc 并在全部拷贝完成后清理临时目录。
func buildProtoCppShard(protoFiles []string, tempParentDir, destDir string) error {
	shardTempDir, err := os.MkdirTemp(tempParentDir, "cpp-shard-*")
	if err != nil {
		logger.Global.Fatal("C++ generation: failed to create shard temp directory",
			zap.String("parent_dir", tempParentDir),
			zap.Error(err),
		)
	}
	defer os.RemoveAll(shardTempDir)

	if err := GenerateCpp(protoFiles, shardTempDir); err != nil {
		return err
	}

	var copyWg sync.WaitGroup
	if err := CopyCppOutputs(&copyWg, protoFiles, shardTempDir, destDir); err != nil {
		copyWg.Wait()
		return err
	}
	copyWg.Wait()
	return nil
}

// collectDomainCppProtoFiles 按域名排序收集全部域下的 .proto 并去重。
// 与历史行为一致，单个域目录缺失时记 Warn 跳过，其他 I/O 错误返回。
func collectDomainCppProtoFiles(domains map[string]_config.DomainMeta) ([]string, error) {
	domainKeys := make([]string, 0, len(domains))
	for domain := range domains {
		domainKeys = append(domainKeys, domain)
	}
	sort.Strings(domainKeys)

	seen := make(map[string]struct{})
	var allFiles []string
	for _, domain := range domainKeys {
		protoDir := domains[domain].Source
		if _, statErr := os.Stat(protoDir); statErr != nil {
			if os.IsNotExist(statErr) {
				logger.Global.Warn("C++ batch generation skipped: proto directory not found",
					zap.String("dir", protoDir),
				)
				continue
			}
			return nil, statErr
		}
		files, err := utils2.CollectProtoFiles(protoDir)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			dedupKey := strings.ToLower(filepath.ToSlash(filepath.Clean(file)))
			if _, ok := seen[dedupKey]; ok {
				continue
			}
			seen[dedupKey] = struct{}{}
			allFiles = append(allFiles, file)
		}
	}
	sort.Strings(allFiles)
	return allFiles, nil
}

func fileSizeOrZero(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// shardProtoFilesBySize 使用按文件大小降序的 LPT 贪心均衡切分，缩短并行 protoc 长尾。
func shardProtoFilesBySize(protoFiles []string, maxShards int, sizeOf func(string) int64) [][]string {
	if len(protoFiles) == 0 {
		return nil
	}
	if maxShards < 1 {
		maxShards = 1
	}
	if maxShards > len(protoFiles) {
		maxShards = len(protoFiles)
	}
	if maxShards == 1 {
		single := make([]string, len(protoFiles))
		copy(single, protoFiles)
		sort.Strings(single)
		return [][]string{single}
	}

	type weightedFile struct {
		path string
		size int64
	}
	weighted := make([]weightedFile, len(protoFiles))
	for i, path := range protoFiles {
		weighted[i] = weightedFile{path: path, size: sizeOf(path)}
	}
	sort.Slice(weighted, func(i, j int) bool {
		if weighted[i].size != weighted[j].size {
			return weighted[i].size > weighted[j].size
		}
		return weighted[i].path < weighted[j].path
	})

	shards := make([][]string, maxShards)
	weights := make([]int64, maxShards)
	for _, item := range weighted {
		minIdx := 0
		for i := 1; i < maxShards; i++ {
			if weights[i] < weights[minIdx] {
				minIdx = i
			}
		}
		shards[minIdx] = append(shards[minIdx], item.path)
		weights[minIdx] += item.size
	}
	for _, shard := range shards {
		sort.Strings(shard)
	}
	return shards
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
