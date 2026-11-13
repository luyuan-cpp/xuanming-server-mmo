package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	_config "protogen/internal/config"
	cppGen "protogen/internal/generator/cpp"
	"protogen/internal/persistschema"
	"protogen/logger"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

func makeProjectDir() {
	os.MkdirAll(_config.Global.Paths.GeneratedDir, os.FileMode(0777))
	os.MkdirAll(_config.Global.Paths.ServiceInfoDir, os.FileMode(0777))
	os.MkdirAll(_config.Global.Paths.CppGenGrpcDir, os.FileMode(0777))
	os.MkdirAll(_config.Global.Paths.TempFileGenDir, os.FileMode(0775))
	os.MkdirAll(_config.Global.Paths.ProtoBufCTempDir, os.FileMode(0777))
	os.MkdirAll(_config.Global.Paths.TableGeneratorDir, os.FileMode(0777))
}

func init() {
	if err := logger.Init(); err != nil {
		panic(fmt.Sprintf("Failed to initialize global logger: %v", err))
	}
}

func startPprofServer() {
	go func() {
		logger.Global.Info("Starting pprof server", zap.String("address", "localhost:11111"))
		if err := http.ListenAndServe("localhost:11111", nil); err != nil {
			logger.Global.Error("pprof server exited with error", zap.Error(err))
		}
	}()
}

func waitForShutdownSignal() {
	logger.Global.Info("Waiting for shutdown. Visit localhost:11111/debug/pprof for profiling. Press Ctrl+C to exit.")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	logger.Global.Info("Graceful shutdown started...")
}

func isEnabledEnv(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func main() {
	defer logger.Sync()
	grpcHandler := flag.String("grpc-handler", "", "仅从指定源 proto 重生成内部 C++ gRPC handler，不执行全量或客户端生成")
	persistSchemaSeal := flag.Bool("persist-schema-seal", false,
		"只把持久 schema 台账封存到当前最大世代(发布前运行),不执行生成;见 docs/ops/release-checklist.md §2.1 ①")
	persistSchemaVerify := flag.Bool("persist-schema-verify", false,
		"只核对持久 schema 台账:全部封存成立且已封存到最大世代,不写任何文件;发布预检调用")
	flag.Parse()
	exclusiveModes := 0
	for _, on := range []bool{*persistSchemaSeal, *persistSchemaVerify, strings.TrimSpace(*grpcHandler) != ""} {
		if on {
			exclusiveModes++
		}
	}
	if exclusiveModes > 1 {
		logger.Global.Fatal("--persist-schema-seal, --persist-schema-verify and --grpc-handler are mutually exclusive")
	}
	flag.Visit(func(option *flag.Flag) {
		if option.Name == "grpc-handler" && strings.TrimSpace(*grpcHandler) == "" {
			logger.Global.Fatal("--grpc-handler requires a source proto; an empty value must not fall through to full generation")
		}
	})
	if flag.NArg() != 0 {
		logger.Global.Fatal("Unsupported command arguments", zap.Strings("arguments", flag.Args()))
	}

	start := time.Now()
	logger.Global.Info("Program started", zap.Duration("elapsed", time.Since(start)))
	enablePprof := isEnabledEnv("PROTOGEN_ENABLE_PPROF") || isEnabledEnv("PBGEN_ENABLE_PPROF")
	if enablePprof && *grpcHandler == "" && !*persistSchemaSeal && !*persistSchemaVerify {
		startPprofServer()
	}

	if err := _config.Load(); err != nil {
		logger.Global.Fatal("Failed to load config", zap.Error(err))
	}

	logger.Global.Info("Config loaded", zap.String("output_root", _config.Global.Paths.OutputRoot))
	if *persistSchemaVerify {
		seal, err := persistschema.VerifyLedgerFile(_config.Global.Paths.PersistSchemaLedgerFile)
		if err != nil {
			logger.Global.Fatal("Persist schema ledger verification failed", zap.Error(err))
		}
		logger.Global.Info("Persist schema ledger verified", zap.Uint32("sealed_gen", seal.Gen), zap.Int("rows", seal.Rows),
			zap.String("ledger", _config.Global.Paths.PersistSchemaLedgerFile))
		return
	}
	if *persistSchemaSeal {
		seal, changed, err := persistschema.SealLedgerFile(_config.Global.Paths.PersistSchemaLedgerFile)
		if err != nil {
			logger.Global.Fatal("Persist schema ledger seal failed", zap.Error(err))
		}
		logger.Global.Info("Persist schema ledger sealed", zap.Uint32("gen", seal.Gen), zap.Int("rows", seal.Rows),
			zap.Bool("changed", changed), zap.String("ledger", _config.Global.Paths.PersistSchemaLedgerFile))
		return
	}
	if *grpcHandler != "" {
		if err := cppGen.GenerateGrpcHandler(*grpcHandler); err != nil {
			logger.Global.Fatal("Targeted gRPC handler generation failed", zap.Error(err))
		}
		logger.Global.Info("Targeted gRPC handler generation finished", zap.String("proto", *grpcHandler))
		return
	}

	dir, err := os.Getwd()
	if err != nil {
		logger.Global.Fatal("Failed to get current working directory", zap.Error(err))
	}

	fmt.Println("Current working directory:", dir)

	runner := NewExecutionRunner()
	runGenerationPipeline(runner)
	runner.PrintStats()

	logger.Global.Info("Total execution time", zap.Duration("elapsed", time.Since(start)))
	if enablePprof {
		waitForShutdownSignal()
		return
	}

	logger.Global.Info("Generation complete. To enable profiling, set PROTOGEN_ENABLE_PPROF=1 (legacy: PBGEN_ENABLE_PPROF=1)")
}
