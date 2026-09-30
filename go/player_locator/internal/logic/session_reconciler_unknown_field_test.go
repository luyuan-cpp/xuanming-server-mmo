package logic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	base "proto/common/base"
)

const (
	reconcileGateUUIDA = "3f0c6a52-6d1e-4b7a-9a41-0c2f7e5b8d10"
	reconcileGateUUIDB = "b7e2d4c1-8a3f-4e6b-9c0d-1f2a3b4c5d6e"
)

// gateNodeInfoWithFutureFields 模拟新版本 gate 写进 etcd 的 NodeInfo:带 clientEndpoint(D76)
// 与一个任何版本都不存在的虚构字段,保证无论 proto 是否已重生,样本里都有真正的未知字段。
func gateNodeInfoWithFutureFields(uuid string) []byte {
	return []byte(`{"nodeId":3,"nodeType":4,"launchTime":"1767225600",` +
		`"endpoint":{"ip":"10.244.0.21","port":18000},"zoneId":101,` +
		`"nodeUuid":"` + uuid + `",` +
		`"clientEndpoint":{"ip":"127.0.0.1","port":30001},` +
		`"futureFieldNotInAnyVersion":{"k":"v"}}`)
}

// 回归(D77):带未知字段的 NodeInfo 必须解出 uuid 收进存活集,而不是把整段 JSON 当 uuid。
// 严格解析时真实 uuid 缺席,连续两轮后该 gate 上的 ONLINE 会话会被全部误打成 DISCONNECTING。
func TestCollectLiveGateInstances_UnknownFieldYieldsUUIDNotRawJSON(t *testing.T) {
	value := gateNodeInfoWithFutureFields(reconcileGateUUIDA)

	// 护栏:样本确实让默认严格解析失败,否则本用例证明不了宽松解析生效。
	require.Error(t, protojson.Unmarshal(value, &base.NodeInfo{}),
		"样本必须含默认严格 protojson 不认识的字段")

	live := collectLiveGateInstances([][]byte{value})

	assert.Contains(t, live, reconcileGateUUIDA)
	assert.NotContains(t, live, string(value), "整段 JSON 不得被当成 uuid 收进存活集")
	assert.Len(t, live, 1)
}

// allocated/ 占位 key 的值是裸 uuid,解析失败后仍按原语义把原始值收进存活集(多收不误杀);
// 与同一 gate 的 NodeInfo 同时出现时归并为同一个 uuid。
func TestCollectLiveGateInstances_MixedNodeInfoAndAllocatedPlaceholders(t *testing.T) {
	values := [][]byte{
		gateNodeInfoWithFutureFields(reconcileGateUUIDA),
		[]byte(reconcileGateUUIDA), // A 的 allocated/ 占位
		[]byte(reconcileGateUUIDB), // B 只剩占位(例如 NodeInfo 尚未写入)
	}

	live := collectLiveGateInstances(values)

	assert.Equal(t, map[string]struct{}{
		reconcileGateUUIDA: {},
		reconcileGateUUIDB: {},
	}, live)
}
