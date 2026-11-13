package prototools

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

// ResolveGameProtoPath resolves the path to the core game proto file.
func ResolveGameProtoPath() (string, error) {
	gameProtoPath := filepath.Join(_config.Global.Paths.GameRpcProtoPath, _config.Global.Naming.GameRpcProto)
	if _, err := os.Stat(gameProtoPath); err != nil {
		logger.Global.Error("Game proto file does not exist",
			zap.String("path", gameProtoPath),
			zap.Error(err),
		)
		return "", errors.Join(errors.New("game proto file does not exist"), err)
	}
	return gameProtoPath, nil
}

func BuildGeneratorProtoPath(dir string) string {
	// dir is an absolute path; append the proto subdirectory name directly
	return filepath.Join(dir, filepath.ToSlash(_config.Global.DirectoryNames.NormalGoProto))
}

// goctlServices lists services that need per-service proto staging directories
// for goctl code generation (used by go/build.ps1).
// All other Go services consume the unified go/proto module directly.
var goctlServices = []string{"db", "login"}

// GoctlServices returns the list of services that need per-service proto staging.
func GoctlServices() []string {
	return goctlServices
}

// CopyProtoToGenDir copies proto files to the generation output directories.
// Only db and login need per-service copies (for goctl); all other Go services
// use the unified go/proto module.
func CopyProtoToGenDir(wg *sync.WaitGroup) {
	wg.Add(1)

	go func() {
		defer wg.Done()
		for _, dir := range goctlServices {
			destDir := _config.Global.Paths.GeneratorProtoDir + BuildGeneratorProtoPath(dir)
			if err := copyProtoToDir(_config.Global.Paths.ProtoDir, destDir); err != nil {
				logger.Global.Warn("Failed to copy proto files",
					zap.String("copy_type", "normal generation dir"),
					zap.String("dir", dir),
					zap.Error(err),
				)
			}
		}
		// Robot 的活协议先跟随源刷新，后续串行阶段再改写 go_package。
		if _config.Global.Generators.EnableRobotProto {
			destDir := _config.Global.Paths.RobotGeneratedProto
			if err := copyProtoToDir(_config.Global.Paths.ProtoDir, destDir); err != nil {
				logger.Global.Warn("Failed to copy robot proto sources",
					zap.String("copy_type", "robot generation dir"),
					zap.String("dir", destDir),
					zap.Error(err),
				)
			}
		}
	}()
}

// copyProtoToDir copies proto files from a single source directory to the destination.
func copyProtoToDir(srcDir, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return errors.Join(errors.New("failed to create directory"), err)
	}

	if err := utils2.CopyLocalDir(srcDir, destDir); err != nil {
		return errors.Join(errors.New("failed to copy files"), err)
	}
	return nil
}
