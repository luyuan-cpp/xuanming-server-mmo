package _go

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

const unifiedStagingDir = "_unified"

// unifiedGoExcludedDirs 是不进 go/proto 统一 Go 绑定的 proto 目录(相对 proto/ 根的第一级目录名)。
//
// opentelemetry:官方 opentelemetry-proto 的拷贝,只给 C++ 的 span 导出器编码 OTLP 用
// (docs/design/distributed-tracing.md §10.3)。Go 一侧用 go-zero 带进来的官方模块 go.opentelemetry.io/proto/otlp;
// 这里再生成一份同名消息(opentelemetry.proto.*),任何 Go 程序只要同时链接两份,protobuf 的全局注册表在 init 时
// 就会因全名冲突 panic(google.golang.org/protobuf 的缺省冲突策略)。不生成就不存在这个隐患。
var unifiedGoExcludedDirs = []string{"opentelemetry"}

// pruneUnifiedGoExcludedDirs 从暂存目录里删掉 dirs 列出的目录,之后的 go_package 改写与 protoc 都看不到它们。
// 目录不存在不算错误。
func pruneUnifiedGoExcludedDirs(stagingRoot string, dirs []string) error {
	for _, dir := range dirs {
		if dir == "" || strings.ContainsAny(dir, `/\`) || dir == "." || dir == ".." {
			continue // 只认第一级目录名,不让配置写错删到暂存目录之外
		}
		if err := os.RemoveAll(filepath.Join(stagingRoot, dir)); err != nil {
			return err
		}
	}
	return nil
}

// BuildUnifiedGoProto generates all Go proto outputs into a single shared
// module directory (go/proto/) instead of duplicating into each service.
//
// Pipeline: stage → set go_package → protoc → go/proto/
func BuildUnifiedGoProto(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		stagingRoot := filepath.Join(
			_config.Global.Paths.GeneratorProtoDir,
			unifiedStagingDir,
			_config.Global.DirectoryNames.ProtoDirName,
		)

		// 1. Clean and recreate staging directory.
		if err := os.RemoveAll(filepath.Join(_config.Global.Paths.GeneratorProtoDir, unifiedStagingDir)); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to clean staging dir",
				zap.String("dir", stagingRoot),
				zap.Error(err),
			)
		}
		if err := os.MkdirAll(stagingRoot, 0755); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to create staging dir",
				zap.String("dir", stagingRoot),
				zap.Error(err),
			)
		}

		// 2. Copy all proto sources into the staging directory.
		if err := utils2.CopyLocalDir(_config.Global.Paths.ProtoDir, stagingRoot); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to copy proto sources",
				zap.String("src", _config.Global.Paths.ProtoDir),
				zap.String("dst", stagingRoot),
				zap.Error(err),
			)
		}

		// 2b. 去掉不进统一 Go 绑定的目录(见 unifiedGoExcludedDirs)。
		if err := pruneUnifiedGoExcludedDirs(stagingRoot, unifiedGoExcludedDirs); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to drop excluded proto directories",
				zap.String("staging_root", stagingRoot),
				zap.Error(err),
			)
		}

		// 3. Set go_package = "proto/{relative_path}" for every proto file.
		if err := setUnifiedGoPackage(stagingRoot); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to set go_package",
				zap.String("staging_root", stagingRoot),
				zap.Error(err),
			)
		}

		// 4. Collect all proto files.
		protoFiles, err := collectGoGrpcProtoFiles(stagingRoot)
		if err != nil {
			logger.Global.Fatal("Unified Go proto: failed to collect proto files",
				zap.String("staging_root", stagingRoot),
				zap.Error(err),
			)
		}
		if len(protoFiles) == 0 {
			logger.Global.Warn("Unified Go proto: no proto files found, skipping")
			return
		}

		// 5. Ensure output directory exists.
		nodeGoDir := _config.Global.Paths.NodeGoDir
		if err := utils2.EnsureDir(nodeGoDir); err != nil {
			logger.Global.Fatal("Unified Go proto: failed to create output dir",
				zap.String("dir", nodeGoDir),
				zap.Error(err),
			)
		}

		// 6. Run protoc. proto_path is the parent of stagingRoot so that
		//    import "proto/common/base/tip.proto" resolves correctly.
		protoRootPath := filepath.Dir(filepath.Clean(stagingRoot))
		if err := generateUnifiedOutput(nodeGoDir, filepath.Dir(stagingRoot), func(output string) error {
			return GenerateGoGrpc(protoFiles, output, protoRootPath)
		}); err != nil {
			logger.Global.Fatal("Unified Go proto: code generation failed",
				zap.String("staging_root", stagingRoot),
				zap.String("output_dir", nodeGoDir),
				zap.Error(err),
			)
		}

		logger.Global.Info("Unified Go proto: generation complete",
			zap.Int("proto_file_count", len(protoFiles)),
			zap.String("output_dir", nodeGoDir+"/proto/"),
		)
	}()
}

// protoc 先写全新的临时目录，成功后才发布变更。Windows 上重建未变化的既有文件
// 也可能因并行读者失败；复用统一的按内容写入规则，失败的 protoc 不污染绑定。
func generateUnifiedOutput(outputDir, stagingParent string, generate func(string) error) error {
	scratch, err := os.MkdirTemp(stagingParent, "go-output-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	if err = generate(scratch); err != nil {
		return err
	}
	return filepath.WalkDir(scratch, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		relative, err := filepath.Rel(scratch, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return utils2.WriteFileIfChanged(filepath.Join(outputDir, relative), content)
	})
}

// setUnifiedGoPackage sets go_package = "proto/{relative_dir}" for every
// .proto file under rootDir.
func setUnifiedGoPackage(rootDir string) error {
	return filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".proto" {
			return nil
		}

		relDir, err := filepath.Rel(rootDir, filepath.Dir(path))
		if err != nil {
			logger.Global.Fatal("Unified GoPackage: failed to compute relative path",
				zap.String("root", rootDir),
				zap.String("file", path),
				zap.Error(err),
			)
		}

		goPackage := "proto"
		if relDir != "." {
			goPackage = "proto/" + filepath.ToSlash(relDir)
		}

		if _, err := AddGoPackage(path, goPackage, false); err != nil {
			logger.Global.Fatal("Unified GoPackage: failed to set go_package",
				zap.String("file", path),
				zap.String("go_package", goPackage),
				zap.Error(err),
			)
		}
		return nil
	})
}
