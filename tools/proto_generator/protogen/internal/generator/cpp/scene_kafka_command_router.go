package cpp

import (
	"path/filepath"
	"strings"
	"sync"

	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

type sceneKafkaRouterCase struct {
	EventIdConstant string
	QualifiedName   string
	ClassName       string
	HandlerName     string
}

type sceneKafkaRouterTemplateData struct {
	Includes []string
	Cases    []sceneKafkaRouterCase
}

func buildSceneKafkaRouterData() sceneKafkaRouterTemplateData {
	data := sceneKafkaRouterTemplateData{}
	includeSet := make(map[string]struct{})
	for _, event := range globalProtoEventList {
		if event.Ingress != eventIngressScene {
			continue
		}
		baseName := strings.TrimSuffix(filepath.Base(event.ProtoInclude), _config.Global.FileExtensions.PbH)
		className := generateClassNameFromProtoName(baseName, _config.Global.Naming.HandlerFile)
		data.Includes = appendUniqueString(data.Includes, includeSet, baseName+_config.Global.FileExtensions.HandlerH)
		data.Includes = appendUniqueString(data.Includes, includeSet, "rpc/service_metadata/"+eventIdHeaderFileName(event.ProtoInclude))
		data.Cases = append(data.Cases, sceneKafkaRouterCase{
			EventIdConstant: event.IdName + _config.Global.Naming.EventId,
			QualifiedName:   event.QualifiedName,
			ClassName:       className,
			HandlerName:     buildEventHandlerMethodName(className, event.MessageName),
		})
	}
	return data
}

func GenerateSceneKafkaCommandRouter(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		outputPath := filepath.Join(_config.Global.Paths.SceneNodeEventHandlerDirectory, "scene_kafka_command_router.cpp")
		// 空白名单也覆写旧路由,避免撤掉入口后仍残留旧 handler 分支。
		if err := utils2.RenderTemplateToFile("internal/template/scene_kafka_command_router.cpp.tmpl", outputPath, buildSceneKafkaRouterData()); err != nil {
			logger.Global.Fatal("Failed to write the scene Kafka router", zap.String("output_file", outputPath), zap.Error(err))
		}
	}()
}
