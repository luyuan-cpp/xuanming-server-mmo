package _go

import (
	"os"
	"path/filepath"
	"sync"

	_config "protogen/internal/config"
	"protogen/internal/prototools"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

// GenerateRobotGoProto recursively processes proto files in a directory to generate Go gRPC code.
func GenerateRobotGoProto(rootDir string, protoRootPath string) error {
	protoFiles, err := collectGoGrpcProtoFiles(rootDir)
	if err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to collect proto files",
			zap.String("root_dir", rootDir),
			zap.Error(err),
		)
	}
	if len(protoFiles) == 0 {
		logger.Global.Info("Go gRPC generation: no proto files to process, skipping directory",
			zap.String("root_dir", rootDir),
		)
		return nil
	}

	nodeGoDir := _config.Global.Paths.ToolsDir
	if err := utils2.EnsureDir(nodeGoDir); err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to create output directory",
			zap.String("output_dir", nodeGoDir),
			zap.Error(err),
		)
	}

	if err := GenerateGoGrpc(protoFiles, nodeGoDir, protoRootPath); err != nil {
		logger.Global.Fatal("Go gRPC generation: code generation failed",
			zap.String("root_dir", rootDir),
			zap.String("output_dir", nodeGoDir),
			zap.Error(err),
		)
	}

	logger.Global.Info("Robot Go gRPC generation: directory processed",
		zap.String("root_dir", rootDir),
		zap.Int("proto_file_count", len(protoFiles)),
	)
	return nil
}

// collectGoGrpcProtoFiles recursively collects all non-database proto files in a directory.
func collectGoGrpcProtoFiles(rootDir string) ([]string, error) {
	var protoFiles []string
	err := filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			logger.Global.Fatal("Go gRPC collect: failed to access path",
				zap.String("path", path),
				zap.Error(err),
			)
		}

		if d.IsDir() || !utils2.IsProtoFile(d) || d.Name() == _config.Global.Naming.DbProtoFile {
			return nil
		}

		absPath, err := filepath.Abs(path)
		if err != nil {
			logger.Global.Warn("Go gRPC collect: failed to get absolute path, skipping file",
				zap.String("path", path),
				zap.Error(err),
			)
			return nil
		}
		protoFiles = append(protoFiles, filepath.ToSlash(absPath))
		return nil
	})

	logger.Global.Debug("Go gRPC collect: file scan completed",
		zap.String("root_dir", rootDir),
		zap.Int("collected_count", len(protoFiles)),
	)
	return protoFiles, err
}

// GenerateGoGrpc generates Go gRPC code.
func GenerateGoGrpc(protoFiles []string, outputDir string, protoRootPath string) error {
	if len(protoFiles) == 0 {
		logger.Global.Fatal("Go gRPC generation: no proto files to process",
			zap.String("output_dir", outputDir),
		)
	}

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to resolve output directory absolute path",
			zap.String("output_dir", outputDir),
			zap.Error(err),
		)
	}
	absProtoRootDir, err := filepath.Abs(protoRootPath)
	if err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to resolve proto root directory absolute path",
			zap.String("proto_root", protoRootPath),
			zap.Error(err),
		)
	}
	absProtobufDir, err := filepath.Abs(_config.Global.Paths.ProtobufDir)
	if err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to resolve protobuf directory absolute path",
			zap.String("protobuf_dir", _config.Global.Paths.ProtobufDir),
			zap.Error(err),
		)
	}

	protocPath, err := utils2.ProtocPath()
	if err != nil {
		logger.Global.Fatal("Go gRPC generation: failed to resolve protoc path",
			zap.Error(err),
		)
	}

	args := []string{
		"--go_out=" + absOutputDir,
		"--go-grpc_out=" + absOutputDir,
		"--proto_path=" + absProtoRootDir,
		"--proto_path=" + absProtobufDir,
	}

	for _, file := range protoFiles {
		args = append(args, file)
	}

	logger.Global.Debug("Go gRPC generation: executing protoc command",
		zap.String("protoc_path", protocPath),
		zap.Strings("args", args),
		zap.Int("proto_count", len(protoFiles)),
	)

	return utils2.RunProtocWithPath(protocPath, args, "generating Go gRPC code")
}

// AddGoPackageToProtoDir adds go_package declarations to proto files.
// Only db and login need per-service go_package (for goctl); all other Go
// services use the unified go/proto module.
func AddGoPackageToProtoDir(wg *sync.WaitGroup) {
	wg.Add(1) // Increment wg counter to ensure the main flow waits for this goroutine.
	go func() {
		defer wg.Done()

		for _, dirName := range prototools.GoctlServices() {
			destDir := filepath.ToSlash(_config.Global.Paths.GeneratorProtoDir + dirName + "/" + _config.Global.DirectoryNames.ProtoDirName)
			baseGoPackage := filepath.ToSlash(dirName)

			if err := addDynamicGoPackage(destDir, baseGoPackage, destDir, false); err != nil {
				logger.Global.Warn("GoPackage setup: failed to process directory",
					zap.String("dest_dir", destDir),
					zap.String("base_go_package", baseGoPackage),
					zap.Error(err),
				)
			} else {
				logger.Global.Info("GoPackage setup: directory processed",
					zap.String("dest_dir", destDir),
					zap.String("base_go_package", baseGoPackage),
				)
			}
		}

		destDir := _config.Global.Paths.RobotGeneratedProto
		baseGoPackage := filepath.ToSlash(_config.Global.Naming.GoRobotPackage)

		if err := addDynamicGoPackage(_config.Global.Paths.RobotGeneratedProto, baseGoPackage, destDir, false); err != nil {
			logger.Global.Warn("GoPackage setup: failed to process directory",
				zap.String("dest_dir", destDir),
				zap.String("base_go_package", baseGoPackage),
				zap.Error(err),
			)
		} else {
			logger.Global.Info("GoPackage setup: directory processed",
				zap.String("dest_dir", destDir),
				zap.String("base_go_package", baseGoPackage),
			)
		}
	}()
}

// addDynamicGoPackage generates relative-path-based go_package for all files in a directory.
func addDynamicGoPackage(rootDir, baseGoPackage, currentDir string, isMulti bool) error {
	entries, err := os.ReadDir(currentDir)
	if err != nil {
		logger.Global.Fatal("GoPackage setup: failed to read directory",
			zap.String("current_dir", currentDir),
			zap.Error(err),
		)
	}

	for _, entry := range entries {
		fullPath := filepath.Join(currentDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			logger.Global.Fatal("GoPackage setup: failed to get file info",
				zap.String("file_path", fullPath),
				zap.Error(err),
			)
		}

		if info.IsDir() {
			if err := addDynamicGoPackage(rootDir, baseGoPackage, fullPath, isMulti); err != nil {
				return err
			}
		} else if filepath.Ext(fullPath) == ".proto" {
			if err := processProtoFileForGoPackage(rootDir, baseGoPackage, fullPath, isMulti, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// processProtoFileForGoPackage sets go_package for a single proto file.
func processProtoFileForGoPackage(rootDir, baseGoPackage, filePath string, isMulti, isGoZero bool) error {
	relativePath, err := filepath.Rel(rootDir, filepath.Dir(filePath))
	if err != nil {
		logger.Global.Fatal("GoPackage setup: failed to compute relative path",
			zap.String("root_dir", rootDir),
			zap.String("file_path", filePath),
			zap.Error(err),
		)
	}

	var goPackagePath string
	if relativePath == "." {
		if isGoZero {
			goPackagePath = baseGoPackage
		} else {
			goPackagePath = filepath.Join(baseGoPackage, _config.Global.DirectoryNames.ProtoDirName)
		}
	} else {
		if isGoZero {
			goPackagePath = filepath.Join(baseGoPackage, filepath.ToSlash(relativePath))
		} else {
			goPackagePath = filepath.Join(
				baseGoPackage,
				_config.Global.DirectoryNames.ProtoDirName,
				filepath.ToSlash(relativePath),
			)
		}
	}
	goPackagePath = filepath.ToSlash(goPackagePath)

	added, err := AddGoPackage(filePath, goPackagePath, isMulti)
	if err != nil {
		logger.Global.Fatal("GoPackage setup: failed to set go_package",
			zap.String("file_path", filePath),
			zap.String("go_package", goPackagePath),
			zap.Error(err),
		)
	}

	if added {
		logger.Global.Info("GoPackage setup: go_package added",
			zap.String("file_path", filePath),
			zap.String("go_package", goPackagePath),
		)
	} else {
		logger.Global.Debug("GoPackage setup: go_package already exists, skipping",
			zap.String("file_path", filePath),
			zap.String("go_package", goPackagePath),
		)
	}
	return nil
}
