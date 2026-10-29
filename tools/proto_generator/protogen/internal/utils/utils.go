package utils

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"protogen/logger"

	"go.uber.org/zap"
)

// protocCommands 统一限制序列化编译子进程，避免各阶段的 goroutine 同时启动大量 protoc。
// 进程退出并收完输出后才交出名额；现有命令、输出与失败语义保持。
var protocCommands = newProtocExecutionGate()

type protocExecutionGate struct {
	active chan struct{}
}

func newProtocExecutionGate() *protocExecutionGate {
	return &protocExecutionGate{active: make(chan struct{}, 1)}
}

func (g *protocExecutionGate) run(command func() error) error {
	g.active <- struct{}{}
	defer func() { <-g.active }()
	return command()
}

// EnsureDir ensures the directory exists, creating it with 0755 permissions if needed.
func EnsureDir(dirPath string) error {
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		logger.Global.Fatal("Failed to create directory",
			zap.String("dir_path", dirPath),
			zap.Error(err),
		)
	}
	return nil
}

// CollectProtoFiles collects all .proto files under the given directory (returns absolute paths).
func CollectProtoFiles(dirPath string) ([]string, error) {
	var protoFiles []string

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".proto") {
			absPath, err := filepath.Abs(path)
			if err != nil {
				logger.Global.Warn("CollectProtoFiles: failed to get absolute path, skipping",
					zap.String("file_path", path),
					zap.Error(err),
				)
				return nil
			}
			protoFiles = append(protoFiles, absPath)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return protoFiles, nil
}

// RunProtoc runs protoc using the system PATH.
func RunProtoc(args []string, actionDesc string) error {
	cmd := exec.Command("protoc", args...)
	return ExecuteProtocCmd(cmd, actionDesc)
}

// RunProtocWithPath runs protoc with a specified path.
func RunProtocWithPath(protocPath string, args []string, actionDesc string) error {
	cmd := exec.Command(protocPath, args...)
	return ExecuteProtocCmd(cmd, actionDesc)
}

// ExecuteProtocCmd executes a protoc command and handles output and errors.
func ExecuteProtocCmd(cmd *exec.Cmd, actionDesc string) error {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logger.Global.Info("Running protoc",
		zap.String("action", actionDesc),
		zap.String("command", cmd.String()),
	)

	if err := protocCommands.run(cmd.Run); err != nil {
		logger.Global.Fatal("protoc execution failed",
			zap.String("action", actionDesc),
			zap.Error(err),
			zap.String("stderr", stderr.String()),
			zap.String("command", cmd.String()),
		)
	}

	if stdout.Len() > 0 {
		logger.Global.Info("protoc execution succeeded",
			zap.String("action", actionDesc),
			zap.String("stdout", stdout.String()),
		)
	}
	return nil
}
