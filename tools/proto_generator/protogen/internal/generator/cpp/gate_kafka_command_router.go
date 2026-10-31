package cpp

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"

	"go.uber.org/zap"
)

type gateKafkaRouterCase struct {
	EventIdConstant  string
	EventMessageName string
	ClassName        string
	HandlerName      string
	Assignments      []string

	// PayloadRequired:事件有 GateCommand 信封装不下的字段(见 eventRequiresPayload)。空载荷的这类命令
	// 只可能来自坏生产者,模板把信封分支渲染成「计数 + 采样日志 + 拒绝」,不构造缺字段的事件下发;
	// 为 true 时 Assignments 为空。
	PayloadRequired bool
}

type gateKafkaRouterTemplateData struct {
	Cases []gateKafkaRouterCase
}

// AnyPayloadRequired 供模板决定是否渲染拒绝空载荷的辅助函数:没有需要载荷的事件时不生成它,
// 免得生成物里留一个没人调用的函数。
func (d gateKafkaRouterTemplateData) AnyPayloadRequired() bool {
	for _, c := range d.Cases {
		if c.PayloadRequired {
			return true
		}
	}
	return false
}

// gateCommandPayloadField 是 GateCommand 上承载事件序列化字节的字段。信封分支只在它为空时才走,所以它不算能映射的字段:
// 事件里同名的字段拿到的恒为空。
const gateCommandPayloadField = "payload"

// envelopeCarriesField 是信封分支映射规则的唯一实现:GateCommand 上有同名、同型、双方都不是 repeated 的标量字段,
// 这个事件字段才能从信封赋值。message / group 永远装不下(信封只有标量)。同名不同型多半是 proto 写错了,留一行 Warn。
func envelopeCarriesField(eventName string, commandFields map[string]*descriptorpb.FieldDescriptorProto,
	eventField *descriptorpb.FieldDescriptorProto) bool {
	commandField, exists := commandFields[eventField.GetName()]
	if !exists || eventField.GetName() == gateCommandPayloadField {
		return false
	}
	if commandField.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED ||
		eventField.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		return false
	}
	switch eventField.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, descriptorpb.FieldDescriptorProto_TYPE_GROUP:
		return false
	}
	if commandField.GetType() != eventField.GetType() {
		logger.Global.Warn("GateCommand and Event field type mismatch, the event now requires a payload",
			zap.String("event", eventName),
			zap.String("field", eventField.GetName()),
		)
		return false
	}
	return true
}

// eventRequiresPayload 判断事件是否只能经 payload 到 gate:只要有一个字段信封装不下(envelopeCarriesField 为假)就是。
// 信封分支对装不下的字段只能留默认值,这样构造出来的事件缺字段却照常执行:PushToPlayer 给客户端推一个空 MessageContent,
// 三种广播静默变成空操作,RedirectToGate 把客户端重定向到空地址,踢线和进场丢掉登录代次与所有权栅栏
// (docs/design/battle-spectate-tiered-fanout.md §14 顺带发现)。所以判据看全部字段,不只看 message / repeated。
func eventRequiresPayload(event *descriptorpb.DescriptorProto, commandFields map[string]*descriptorpb.FieldDescriptorProto) bool {
	for _, field := range event.GetField() {
		if !envelopeCarriesField(event.GetName(), commandFields, field) {
			return true
		}
	}
	return false
}

func findProtoFileBySuffix(pkgName string, nameSuffix string) *descriptorpb.FileDescriptorProto {
	for _, fd := range internal.FdSet.File {
		if fd.GetPackage() != pkgName {
			continue
		}
		if strings.HasSuffix(fd.GetName(), nameSuffix) {
			return fd
		}
	}
	return nil
}

func findMessageByName(fd *descriptorpb.FileDescriptorProto, messageName string) *descriptorpb.DescriptorProto {
	if fd == nil {
		return nil
	}
	for _, message := range fd.GetMessageType() {
		if message.GetName() == messageName {
			return message
		}
	}
	return nil
}

func collectFieldsByName(message *descriptorpb.DescriptorProto) map[string]*descriptorpb.FieldDescriptorProto {
	fields := make(map[string]*descriptorpb.FieldDescriptorProto)
	if message == nil {
		return fields
	}
	for _, field := range message.GetField() {
		fields[field.GetName()] = field
	}
	return fields
}

func buildGateKafkaRouterCases() ([]gateKafkaRouterCase, error) {
	gateCommandFd := findProtoFileBySuffix("contracts.kafka", "gate_command.proto")
	if gateCommandFd == nil {
		return nil, fmt.Errorf("缺少 contracts.kafka gate_command.proto 描述符")
	}

	gateEventFd := findProtoFileBySuffix("contracts.kafka", "gate_event.proto")
	if gateEventFd == nil {
		return nil, fmt.Errorf("缺少 contracts.kafka gate_event.proto 描述符")
	}

	gateCommandMsg := findMessageByName(gateCommandFd, "GateCommand")
	if gateCommandMsg == nil {
		return nil, fmt.Errorf("缺少 GateCommand 消息描述符")
	}

	commandFieldMap := collectFieldsByName(gateCommandMsg)

	cases := make([]gateKafkaRouterCase, 0)
	for _, eventMsg := range gateEventFd.GetMessageType() {
		enabled, err := kafkaIngressEnabled(eventMsg)
		if err != nil {
			return nil, fmt.Errorf("网关事件 %s 入口标注损坏: %w", eventMsg.GetName(), err)
		}
		if !enabled {
			continue
		}
		eventMessageName := eventMsg.GetName()

		// Compute the C++ event_id constant name: e.g. "ContractsKafkaKickPlayerEventEventId"
		eventIdConstant := buildEventIdName("contracts.kafka", eventMessageName) + "EventId"

		caseData := gateKafkaRouterCase{
			EventIdConstant:  eventIdConstant,
			EventMessageName: eventMessageName,
			ClassName:        "GateEventHandler",
			HandlerName:      buildEventHandlerMethodName("GateEventHandler", eventMessageName),
			Assignments:      make([]string, 0),
			PayloadRequired:  eventRequiresPayload(eventMsg, commandFieldMap),
		}

		// 信封装不下的事件不做同名映射:空载荷时模板渲染拒绝分支,映射出来也用不上。
		if caseData.PayloadRequired {
			cases = append(cases, caseData)
			continue
		}

		// 走到这里每个字段都能从信封映射(envelopeCarriesField),按同名赋值。
		for _, eventField := range eventMsg.GetField() {
			fieldName := eventField.GetName()
			caseData.Assignments = append(caseData.Assignments,
				"event.set_"+fieldName+"(command."+fieldName+"());")
		}

		cases = append(cases, caseData)
	}

	return cases, nil
}

func GenerateGateKafkaCommandRouter(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		cases, err := buildGateKafkaRouterCases()
		if err != nil {
			logger.Global.Fatal("生成网关 Kafka 路由失败", zap.Error(err))
		}

		// 撤销全部标注也必须覆写旧输出,使空白名单确实拒绝所有事件。
		outputPath := filepath.Join(_config.Global.Paths.GateNodeEventHandlerDirectory, "gate_kafka_command_router.cpp")
		data := gateKafkaRouterTemplateData{Cases: cases}

		if err := utils2.RenderTemplateToFile("internal/template/gate_kafka_command_router.cpp.tmpl", outputPath, data); err != nil {
			logger.Global.Fatal("写入网关 Kafka 路由失败",
				zap.String("output_file", outputPath),
				zap.Error(err),
			)
			return
		}

		logger.Global.Info("Gate Kafka router file generated successfully",
			zap.String("output_file", outputPath),
			zap.Int("case_count", len(cases)),
		)
	}()
}
