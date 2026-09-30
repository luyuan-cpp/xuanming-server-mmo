package nodeinfo

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	base "proto/common/base"
)

const sampleGateUUID = "8c1f0f3e-2b7a-4c55-9d7e-0a6f5b1c2d3e"

// newGateNodeInfoJSON 模拟新版本 gate 写进 etcd 的注册值(字段名与 C++ MessageToJsonString /
// Go protojson.Marshal 一致,lowerCamelCase,uint64 编成字符串):带 clientEndpoint(D76,
// NodeInfo.client_endpoint=11),外加一个任何版本都不存在的虚构字段 —— 保证样本里始终有
// 至少一个真正的未知字段,否则"能解析"证明不了任何事(见 TestStrictProtojsonRejectsSample)。
const newGateNodeInfoJSON = `{"nodeId":7,"nodeType":4,"launchTime":"1767225600",` +
	`"endpoint":{"ip":"10.244.0.12","port":18000},"zoneId":101,` +
	`"nodeUuid":"` + sampleGateUUID + `",` +
	`"clientEndpoint":{"ip":"127.0.0.1","port":30000},` +
	`"futureFieldNotInAnyVersion":{"nested":[1,2,3],"flag":true}}`

// 带未知字段的 NodeInfo 必须能解析,且已知字段一个不丢。
func TestUnmarshalDiscardsUnknownFields(t *testing.T) {
	var info base.NodeInfo
	if err := Unmarshal([]byte(newGateNodeInfoJSON), &info); err != nil {
		t.Fatalf("带未知字段的 NodeInfo 必须能解析: %v", err)
	}
	if got := info.GetNodeUuid(); got != sampleGateUUID {
		t.Fatalf("nodeUuid = %q, want %q", got, sampleGateUUID)
	}
	if info.GetNodeId() != 7 || info.GetNodeType() != 4 || info.GetZoneId() != 101 {
		t.Fatalf("nodeId/nodeType/zoneId = %d/%d/%d, want 7/4/101",
			info.GetNodeId(), info.GetNodeType(), info.GetZoneId())
	}
	if got := info.GetLaunchTime(); got != 1767225600 {
		t.Fatalf("launchTime = %d, want 1767225600", got)
	}
	if ep := info.GetEndpoint(); ep.GetIp() != "10.244.0.12" || ep.GetPort() != 18000 {
		t.Fatalf("endpoint = %s:%d, want 10.244.0.12:18000", ep.GetIp(), ep.GetPort())
	}
}

// clientEndpoint 是已知字段(NodeInfo.client_endpoint=11),必须被保留,不能跟虚构的未知字段
// 一起被丢掉。
//
// 这里刻意按字段名反射取值,不调生成的 GetClientEndpoint():本包其余用例只验证放宽语义,
// 不该因 proto 尚未重生而整包编译失败。字段缺失时用 t.Fatal 明确报红(不 t.Skip),
// 未重生时只有本用例失败,不会静默跳过。
func TestUnmarshalKeepsClientEndpoint(t *testing.T) {
	var info base.NodeInfo
	if err := Unmarshal([]byte(newGateNodeInfoJSON), &info); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	msg := info.ProtoReflect()
	fd := msg.Descriptor().Fields().ByName("client_endpoint")
	if fd == nil {
		t.Fatal("NodeInfo 缺 client_endpoint(字段 11):先执行 cd go && build.bat 重生 proto")
	}
	if fd.Number() != 11 || fd.Message() == nil {
		t.Fatalf("NodeInfo.client_endpoint 应为字段 11 的子消息, 实际 number=%d kind=%s", fd.Number(), fd.Kind())
	}
	if !msg.Has(fd) {
		t.Fatal("clientEndpoint 没有被解析进 NodeInfo(被当成未知字段丢弃了)")
	}
	ce := msg.Get(fd).Message()
	ip := mustField(t, ce, "ip").String()
	port := mustField(t, ce, "port").Uint()
	if ip != "127.0.0.1" || port != 30000 {
		t.Fatalf("clientEndpoint = %s:%d, want 127.0.0.1:30000", ip, port)
	}
}

// mustField 按字段名取 m 的值;字段不存在直接 t.Fatal,避免对 nil 描述符取值 panic。
func mustField(t *testing.T, m protoreflect.Message, name protoreflect.Name) protoreflect.Value {
	t.Helper()
	fd := m.Descriptor().Fields().ByName(name)
	if fd == nil {
		t.Fatalf("%s 缺字段 %s", m.Descriptor().FullName(), name)
	}
	return m.Get(fd)
}

// 反向护栏:同一份样本用默认严格模式必须失败。若它成功了,说明样本里已经没有真正的未知字段,
// 上面两个用例就退化成空测试,需要换一个虚构字段名。
func TestStrictProtojsonRejectsSample(t *testing.T) {
	var info base.NodeInfo
	if err := protojson.Unmarshal([]byte(newGateNodeInfoJSON), &info); err == nil {
		t.Fatal("默认严格 protojson 应拒绝带未知字段的样本,样本已失去护栏作用")
	}
}

// 只放宽未知字段名(及未知枚举名):语法错误、已知字段类型不符等仍必须报错,调用方的失败分支才有意义
// (例如 player_locator 对账把解析失败的值当 allocated/ 占位的裸 uuid)。
func TestUnmarshalStillRejectsMalformedJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "空值", input: ``},
		{name: "截断的 JSON", input: `{"nodeId":7,"nodeUuid":"` + sampleGateUUID + `"`},
		{name: "allocated 占位的裸 uuid", input: sampleGateUUID},
		{name: "顶层不是对象", input: `[1,2,3]`},
		{name: "已知字段类型不符", input: `{"nodeId":"not-a-number"}`},
		{name: "已知子消息类型不符", input: `{"endpoint":"10.244.0.12:18000"}`},
		{name: "未知字段值本身是非法 JSON", input: `{"nodeId":7,"futureField":{]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var info base.NodeInfo
			if err := Unmarshal([]byte(tc.input), &info); err == nil {
				t.Fatalf("输入 %q 应解析失败", tc.input)
			}
		})
	}
}
