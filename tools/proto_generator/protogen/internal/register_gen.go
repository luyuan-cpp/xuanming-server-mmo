package internal

import (
	"sort"
	"sync"

	_config "protogen/internal/config"
	"protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

// checkRepliedCb is the callback type for checking replied methods.
type checkRepliedCb func(methods *RPCMethods) bool

// RegisterFileData holds data for generating the service register file.
type RegisterFileData struct {
	Includes  []string
	InitLines []string
}

// GenRegisterFile generates the service registration file.
func GenRegisterFile(wg *sync.WaitGroup, dst string, cb checkRepliedCb) {
	const tmplStr = `
#include <unordered_map>
#include <memory>
#include <google/protobuf/service.h>
{{- range .Includes }}
{{ . }}
{{ end }}

extern std::unordered_map<std::string, std::unique_ptr<::google::protobuf::Service>> gNodeService;

void InitServiceHandler()
{
{{- range .InitLines }}
{{ . }}
{{ end }}
}
`

	defer wg.Done()

	var includes []string
	var initLines []string

	logger.Global.Info("Generating service register file",
		zap.String("output_path", dst),
	)

	for _, service := range GlobalRPCServiceList {
		if !cb(&service.Methods) {
			continue
		}
		first := service.Methods[0]
		includes = append(includes, first.CppHandlerIncludeName())
		initLine := " gNodeService.emplace(\"" + first.Service() + "\", std::make_unique<" + first.Service() + _config.Global.Naming.HandlerFile + ">());"
		initLines = append(initLines, initLine)
	}

	data := RegisterFileData{
		Includes:  includes,
		InitLines: initLines,
	}

	content := utils.RenderTemplateOrExit("registerFile", tmplStr, data)
	if err := utils.WriteFileIfChanged(dst, []byte(content)); err != nil {
		logger.Global.Fatal("Failed to write the service register file", zap.String("output_path", dst), zap.Error(err))
		return
	}

	logger.Global.Info("Service register file generated",
		zap.String("output_path", dst),
		zap.Int("include_count", len(includes)),
	)
}

// GenerateServiceConstants generates service constant header files.
func GenerateServiceConstants(wg *sync.WaitGroup) {
	logger.Global.Info("Generating service constants",
		zap.String("service_info_dir", _config.Global.Paths.ServiceInfoDir),
	)

	FileServiceMap.Range(func(k, v interface{}) bool {
		protoFile := k.(string)
		serviceList := v.([]*RPCServiceInfo)
		wg.Add(1)
		go func(protoFile string, serviceInfo []*RPCServiceInfo) {
			defer wg.Done()

			if len(serviceInfo) == 0 {
				return
			}

			sort.Slice(serviceInfo, func(i, j int) bool {
				return serviceInfo[i].ServiceIndex < serviceInfo[j].ServiceIndex
			})

			writeServiceIdHeadFile(serviceInfo)
		}(protoFile, serviceList)

		return true
	})
}
