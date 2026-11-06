package discovery

// parse 的解析语义(R6 起经 shared/nodeinfo 走 protojson):可分配状态 admission 按名字或数字都认,
// 缺省与不认识的名字都是 UNSPECIFIED;未知字段被忽略,整台节点不因此从镜像里消失;
// 分配占位键、缺 gRPC 端口的登记、已知字段类型不符照旧跳过。

import (
	"testing"

	"github.com/stretchr/testify/require"

	base "proto/common/base"

	"shared/releasetrack"
)

// battleRegistrationHead 是一条 battle 注册值去掉结尾的部分(字段名与 C++ MessageToJsonString 一致:
// lowerCamelCase,uint64 编成字符串,零值省略)。各用例在后面接上 admission 等字段再闭合。
const battleRegistrationHead = `{"nodeId":7,"nodeType":12,"launchTime":"1767225600","nodeUuid":"uuid-7","physicalRegion":3,` +
	`"endpoint":{"ip":"10.0.0.7","port":18000},"grpcEndpoint":{"ip":"10.0.0.7","port":48000},` +
	`"clientEndpoint":{"ip":"203.0.113.7","port":30007}`

const battleRegistrationKey = testBattlePrefix + "zone/0/node_type/12/node_id/7"

func TestParseReadsAdmissionByNameOrNumber(t *testing.T) {
	w := NewNodeWatcher("battle", testBattlePrefix, nil, nil)
	cases := []struct {
		name string
		tail string
		want base.NodeAdmission
	}{
		{name: "未上报(零值省略)", tail: `}`, want: base.NodeAdmission_NODE_ADMISSION_UNSPECIFIED},
		{name: "名字 OPEN", tail: `,"admission":"NODE_ADMISSION_OPEN"}`, want: base.NodeAdmission_NODE_ADMISSION_OPEN},
		{name: "名字 CLOSED", tail: `,"admission":"NODE_ADMISSION_CLOSED"}`, want: base.NodeAdmission_NODE_ADMISSION_CLOSED},
		{name: "数字 2", tail: `,"admission":2}`, want: base.NodeAdmission_NODE_ADMISSION_CLOSED},
		{name: "不认识的名字按未上报", tail: `,"admission":"NODE_ADMISSION_FROM_A_NEWER_VERSION"}`, want: base.NodeAdmission_NODE_ADMISSION_UNSPECIFIED},
		{name: "未知字段被忽略", tail: `,"admission":"NODE_ADMISSION_OPEN","futureFieldNotInAnyVersion":{"nested":[1,2,3]}}`, want: base.NodeAdmission_NODE_ADMISSION_OPEN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := w.parse(battleRegistrationKey, []byte(battleRegistrationHead+tc.tail))
			require.True(t, ok, "合法注册值必须能解析")
			require.Equal(t, NodeEntry{
				NodeId:         7,
				NodeUuid:       "uuid-7",
				PhysicalRegion: 3,
				ClientHost:     "203.0.113.7",
				ClientPort:     30007,
				Endpoint:       "10.0.0.7:48000",
				Admission:      tc.want,
			}, entry, "已知字段一个不丢,gRPC 地址只取 grpcEndpoint")
		})
	}
}

func TestParseSkipsNonRegistrationsAndUnusableValues(t *testing.T) {
	w := NewNodeWatcher("battle", testBattlePrefix, nil, nil)

	_, ok := w.parse(testBattlePrefix+"allocated/node_type/12/node_id/7", []byte("uuid-7"))
	require.False(t, ok, "分配占位键不是节点注册")

	_, ok = w.parse(testBattlePrefix+"zone/0/node_type/12/node_id/8",
		[]byte(`{"nodeId":8,"nodeUuid":"uuid-8","endpoint":{"ip":"10.0.0.8","port":18000},"admission":"NODE_ADMISSION_OPEN"}`))
	require.False(t, ok, "缺 gRPC 端口的节点不能当 gather 对端")

	_, ok = w.parse(testBattlePrefix+"zone/0/node_type/12/node_id/9", []byte(`{"nodeId":"not-a-number"}`))
	require.False(t, ok, "已知字段类型不符仍按解析失败处理")
}

// 发布轨道(docs/design/release-track-gray-canary.md §2.5):轨道照实解析;认不出的整条跳过;
// StableOnly 的镜像不收 canary,没写轨道的节点按 stable 收。
func TestParseReleaseTrackAndStableOnly(t *testing.T) {
	all := NewNodeWatcher("battle", testBattlePrefix, nil, nil)
	entry, ok := all.parse(battleRegistrationKey, []byte(battleRegistrationHead+`,"releaseTrack":"canary"}`))
	require.True(t, ok)
	require.Equal(t, releasetrack.Canary, entry.Track)

	_, ok = all.parse(battleRegistrationKey, []byte(battleRegistrationHead+`,"releaseTrack":"beta"}`))
	require.False(t, ok, "认不出的轨道既不能当 stable 也不能当 canary")

	stableOnly := NewNodeWatcher("battle", testBattlePrefix, nil, nil).StableOnly()
	_, ok = stableOnly.parse(battleRegistrationKey, []byte(battleRegistrationHead+`,"releaseTrack":"canary"}`))
	require.False(t, ok, "只收 stable 的镜像不能收 canary,否则匹配池归属和服务间调用会落到 canary 上")
	entry, ok = stableOnly.parse(battleRegistrationKey, []byte(battleRegistrationHead+`,"releaseTrack":"stable"}`))
	require.True(t, ok)
	require.Equal(t, releasetrack.Stable, entry.Track)
	_, ok = stableOnly.parse(battleRegistrationKey, []byte(battleRegistrationHead+`}`))
	require.True(t, ok, "没写轨道的节点按 stable 收")
}
