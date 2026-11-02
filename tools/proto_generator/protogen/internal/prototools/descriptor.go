package prototools

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

// GenerateAllInOneDescriptor generates a merged Protobuf descriptor file.
func GenerateAllInOneDescriptor(wg *sync.WaitGroup) {
	wg.Add(1)

	go func() {
		defer wg.Done()
		if err := generateAllInOneDesc(); err != nil {
			logger.Global.Warn("Failed to generate descriptor",
				zap.Error(err),
			)
		}
	}()
}

// generateAllInOneDesc generates and parses the all-in-one descriptor file.
func generateAllInOneDesc() error {
	protoFiles, err := collectUniqueProtoFiles(_config.Global.DomainMeta)
	if err != nil {
		// 此时 message_id.txt / event_id.txt 都还没写(WriteMessageIdFile 在描述符解析之后),中止不留墓碑。
		logger.Global.Fatal("Failed to collect proto files, domain_meta input is incomplete",
			zap.Error(err),
		)
	}
	if len(protoFiles) == 0 {
		logger.Global.Info("Descriptor generation: no proto files found")
		return nil
	}
	logger.Global.Info("Descriptor generation: collected unique proto files",
		zap.Int("count", len(protoFiles)),
	)

	args, err := buildDescriptorArgs(protoFiles)
	if err != nil {
		logger.Global.Fatal("Failed to build command arguments",
			zap.Error(err),
		)
	}

	if err := executeDescriptorCommand(args); err != nil {
		logger.Global.Fatal("Failed to execute protoc",
			zap.Error(err),
		)
	}

	if err := parseDescriptorFile(); err != nil {
		logger.Global.Fatal("Failed to parse descriptor file",
			zap.Error(err),
		)
	}

	logger.Global.Info("Descriptor generation succeeded",
		zap.String("output_path", _config.Global.Paths.AllInOneDesc),
	)
	return nil
}

// collectUniqueProtoFiles collects all unique proto files across domains.
//
// 任一域读不全就返回错误、整轮作废,不跳过该域继续:描述符集合是 RPC 方法与事件的唯一输入,
// 整域漏读时它的方法会在 message_id.txt 里被永久转成墓碑(事件号同理)。生成器的
// maxNewMessageTombstonesPerRun 只按条数设防,拦不住方法数不超过上限的小域,所以这里按域设防。
// 退役的域要从 proto_gen*.yaml 删掉 domain_meta 块,不能留一个空目录或不存在的 source。
func collectUniqueProtoFiles(domains map[string]_config.DomainMeta) ([]string, error) {
	protoFileSet := make(map[string]struct{}) // deduplication set
	var allProtoFiles []string

	// map iteration order is random in Go; sort keys to stabilize generation output.
	domainKeys := make([]string, 0, len(domains))
	for key := range domains {
		domainKeys = append(domainKeys, key)
	}
	sort.Strings(domainKeys)

	for _, domain := range domainKeys {
		domainFiles, err := collectDomainProtoFiles(domain, domains[domain].Source)
		if err != nil {
			return nil, err
		}
		for _, absPath := range domainFiles {
			if _, exists := protoFileSet[absPath]; !exists {
				protoFileSet[absPath] = struct{}{}
				allProtoFiles = append(allProtoFiles, absPath)
			}
		}
	}

	// Keep protoc input order deterministic across machines/runs.
	sort.Strings(allProtoFiles)

	return allProtoFiles, nil
}

// collectDomainProtoFiles 返回一个域 source 目录下(含子目录)全部 .proto 的绝对路径,分隔符统一为 /。
// source 为空、不存在、不是目录、遍历中任一路径访问失败、走下来 0 个 .proto,都返回写明域名与路径的错误:
// 这些情形都意味着该域整域或部分漏读,不能当成"这个域没有协议"。0 个 .proto 按域单独计,不受跨域去重影响。
func collectDomainProtoFiles(domain, source string) ([]string, error) {
	if source == "" {
		return nil, fmt.Errorf("domain_meta.%s: source is empty", domain)
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("domain_meta.%s: source directory %s is not accessible (remove the domain_meta block if the domain was retired): %w",
			domain, source, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("domain_meta.%s: source %s is not a directory", domain, source)
	}

	var files []string
	err = filepath.WalkDir(source, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("access %s: %w", path, walkErr)
		}
		if !utils2.IsProtoFile(d) {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve absolute path of %s: %w", path, err)
		}
		files = append(files, filepath.ToSlash(absPath))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("domain_meta.%s: failed to walk source directory %s: %w", domain, source, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("domain_meta.%s: source directory %s contains no .proto files (remove the domain_meta block if the domain was retired)",
			domain, source)
	}
	return files, nil
}

// buildDescriptorArgs builds protoc arguments for descriptor generation.
func buildDescriptorArgs(protoFiles []string) ([]string, error) {
	descOutput := filepath.ToSlash(_config.Global.Paths.AllInOneDesc)

	args := []string{
		"--descriptor_set_out=" + descOutput,
		"--include_imports",     // include all dependency descriptors
		"--include_source_info", // include source info for debugging
	}

	importPaths := []string{
		_config.Global.Paths.OutputRoot,
		_config.Global.Paths.ProtobufDir,
	}
	for _, ip := range importPaths {
		if ip == "" {
			continue // skip empty path
		}
		args = append(args, "--proto_path="+filepath.ToSlash(ip))
	}

	args = append(args, protoFiles...)
	return args, nil
}

// executeDescriptorCommand runs protoc to generate the descriptor file.
func executeDescriptorCommand(args []string) error {
	protocPath, err := utils2.ProtocPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(protocPath, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logger.Global.Debug("Descriptor generation: executing protoc",
		zap.String("protoc_path", protocPath),
		zap.Strings("args", args),
	)
	if err := cmd.Run(); err != nil {
		logger.Global.Fatal("Command execution failed",
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)
	}

	if stdout.Len() > 0 {
		logger.Global.Info("Descriptor generation: command output",
			zap.String("stdout", stdout.String()),
		)
	}
	return nil
}
