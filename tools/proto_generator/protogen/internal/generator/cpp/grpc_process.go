package cpp

import (
	"os"
	"path"
	"sort"
	"sync"

	"go.uber.org/zap"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	messageoption "github.com/luyuancpp/protooption"
)

// GrpcServiceTemplateData holds data passed to gRPC templates.
type GrpcServiceTemplateData struct {
	ServiceInfo           []*internal.RPCServiceInfo
	GrpcIncludeHeadName   string
	GeneratorGrpcFileName string
	Package               string
	GrpcCompleteQueueName string
	FileBaseNameCamel     string
}

// grpcInitFileInfo represents one generated async gRPC client translation unit.
// A proto file can contain multiple services, so Methods contains the union of
// all RPCs whose completion tags are consumed by that generated client.
type grpcInitFileInfo struct {
	*internal.RPCServiceInfo
	Methods internal.RPCMethods
}

type grpcInitTemplateData struct {
	GrpcFiles       []*grpcInitFileInfo
	NodeEnumCppType string
	NodeInfoCppType string
}

func buildGrpcInitFileInfo(services []*internal.RPCServiceInfo) []*grpcInitFileInfo {
	filesByProtoPath := make(map[string]*grpcInitFileInfo)
	files := make([]*grpcInitFileInfo, 0)

	for _, service := range services {
		if service.CcGenericServices() {
			continue
		}

		if internal.IsFileBelongToNode(service.Fd, messageoption.NodeType_NODE_DB) {
			continue
		}

		protoPath := service.FileName()
		file, ok := filesByProtoPath[protoPath]
		if !ok {
			file = &grpcInitFileInfo{RPCServiceInfo: service}
			filesByProtoPath[protoPath] = file
			files = append(files, file)
		}
		file.Methods = append(file.Methods, service.Methods...)
	}

	// Stabilize namespace/branch emission order in grpc_init_total templates.
	sort.Slice(files, func(i, j int) bool {
		left := files[i].RPCServiceInfo
		right := files[j].RPCServiceInfo

		if left.BasePathForCpp() != right.BasePathForCpp() {
			return left.BasePathForCpp() < right.BasePathForCpp()
		}
		if left.Package() != right.Package() {
			return left.Package() < right.Package()
		}
		return left.FileName() < right.FileName()
	})

	for _, file := range files {
		sort.Slice(file.Methods, func(i, j int) bool {
			return file.Methods[i].Id < file.Methods[j].Id
		})
	}

	return files
}

// generateGrpcFile generates a gRPC file from a template file and avoids duplicate writes.
func generateGrpcFile(fileName string, grpcServices []*internal.RPCServiceInfo, tmplPath string) error {
	if len(grpcServices) == 0 {
		logger.Global.Fatal("Failed to generate gRPC file: service info must not be empty",
			zap.String("target_file", fileName),
		)
	}

	data := GrpcServiceTemplateData{
		ServiceInfo:           grpcServices,
		GrpcIncludeHeadName:   grpcServices[0].GrpcIncludeHeadName(),
		GeneratorGrpcFileName: grpcServices[0].GeneratorGrpcFileName(),
		Package:               grpcServices[0].CppPackage(),
		GrpcCompleteQueueName: grpcServices[0].FileBaseNameCamel() + _config.Global.Naming.CompleteQueue,
		FileBaseNameCamel:     grpcServices[0].FileBaseNameCamel(),
	}

	if err := utils2.RenderTemplateToFile(tmplPath, fileName, data); err != nil {
		logger.Global.Error("Failed to generate gRPC file",
			zap.String("template_path", tmplPath),
			zap.String("file_path", fileName),
			zap.String("directory", path.Dir(fileName)),
			zap.Error(err),
		)
		return err
	}

	return nil
}

func CppGrpcCallClient(wg *sync.WaitGroup) {
	internal.FileServiceMap.Range(func(k, v interface{}) bool {
		protoFile := k.(string)
		serviceList := v.([]*internal.RPCServiceInfo)
		wg.Add(1)
		go func(protoFile string, serviceInfo []*internal.RPCServiceInfo) {
			defer wg.Done()

			if len(serviceInfo) <= 0 {
				return
			}

			firstService := serviceInfo[0]
			_ = firstService.Path()

			// Generate per-file async grpc clients for proto files that use gRPC services.
			// CcGenericServices=true means legacy C++ generic RPC handlers, not gRPC stubs.
			if firstService.CcGenericServices() {
				return
			}

			// ...
			sort.Slice(serviceInfo, func(i, j int) bool {
				return serviceInfo[i].ServiceIndex < serviceInfo[j].ServiceIndex
			})

			targetDir := path.Dir(_config.Global.Paths.CppGenGrpcDir + firstService.LogicalPath())
			err := os.MkdirAll(targetDir, os.FileMode(0777))
			if err != nil {
				logger.Global.Fatal("Failed to generate gRPC client code: directory creation failed",
					zap.String("directory", targetDir),
					zap.String("proto_file", protoFile),
					zap.Error(err),
				)
				return
			}

			cppFileBaseName := firstService.LogicalPath()

			filePath := _config.Global.Paths.CppGenGrpcDir + cppFileBaseName + _config.Global.FileExtensions.GrpcClientH
			if err := generateGrpcFile(filePath, serviceInfo, "internal/template/grpc_async_client.h.tmpl"); err != nil {
				logger.Global.Fatal("Failed to generate gRPC client header file",
					zap.String("file_path", filePath),
					zap.String("proto_file", protoFile),
					zap.Error(err),
				)
			}

			filePathCpp := _config.Global.Paths.CppGenGrpcDir + cppFileBaseName + _config.Global.FileExtensions.GrpcClientCpp
			if err := generateGrpcFile(filePathCpp, serviceInfo, "internal/template/grpc_async_client.cpp.tmpl"); err != nil {
				logger.Global.Fatal("Failed to generate gRPC client cpp file",
					zap.String("file_path", filePathCpp),
					zap.String("proto_file", protoFile),
					zap.Error(err),
				)
			}
		}(protoFile, serviceList)

		return true
	})

	{
		wg.Add(1)

		go func() {
			defer wg.Done()
			grpcFiles := buildGrpcInitFileInfo(internal.GlobalRPCServiceList)

			err := os.MkdirAll(path.Dir(_config.Global.Paths.GrpcInitCppFile), os.FileMode(0777))
			if err != nil {
				logger.Global.Error("Failed to generate gRPC init file: directory creation failed",
					zap.String("directory", path.Dir(_config.Global.Paths.GrpcInitCppFile)),
					zap.Error(err),
				)
				return
			}

			// NodeInfo is defined in common.proto (no package), so it stays in the global namespace.
			nodeInfoCppType := "NodeInfo"

			cppData := grpcInitTemplateData{
				GrpcFiles:       grpcFiles,
				NodeEnumCppType: internal.NodeEnumCppQualifiedType,
				NodeInfoCppType: nodeInfoCppType,
			}

			if err := utils2.RenderTemplateToFile("internal/template/grpc_init_total.cpp.tmpl", _config.Global.Paths.GrpcInitCppFile, cppData); err != nil {
				logger.Global.Fatal("Failed to generate gRPC init cpp file",
					zap.String("file_path", _config.Global.Paths.GrpcInitCppFile),
					zap.Error(err),
				)
			}

			if err := utils2.RenderTemplateToFile("internal/template/grpc_init_total.h.tmpl", _config.Global.Paths.GrpcInitHeadFile, cppData); err != nil {
				logger.Global.Fatal("Failed to generate gRPC init header file",
					zap.String("file_path", _config.Global.Paths.GrpcInitHeadFile),
					zap.Error(err),
				)
			}
		}()
	}
}
