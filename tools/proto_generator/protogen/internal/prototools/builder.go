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

// CopyProtoToGenDir copies proto files to the robot generation staging directory.
// All Go services (including db and login) consume the unified go/proto module directly.
func CopyProtoToGenDir(wg *sync.WaitGroup) {
	wg.Add(1)

	go func() {
		defer wg.Done()
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
