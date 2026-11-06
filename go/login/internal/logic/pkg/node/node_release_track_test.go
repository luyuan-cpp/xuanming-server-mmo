package node

import (
	"testing"

	login_proto "proto/common/base"
	"shared/releasetrack"
)

// 注册值带上本进程的轨道。失租重注册重放的是分配时写下的同一份值,所以核对构造点就够了。
//
// 拒绝非法轨道的那一步在 NewNode 开头、连 etcd 之前。NewNode 出错只回 nil,单测分不出是哪一步失败,
// 这条由冒烟负例覆盖:MMORPG_RELEASE_TRACK=beta 起 login,应拒启并打出 invalid release track。
func TestNewNodeInfoCarriesReleaseTrack(t *testing.T) {
	nodeType := uint32(login_proto.ENodeType_LoginNodeService)
	for _, track := range []releasetrack.Track{releasetrack.Stable, releasetrack.Canary} {
		info := newNodeInfo(nodeType, 3, "10.0.0.7", 50000, track)
		if got := info.GetReleaseTrack(); got != track.String() {
			t.Errorf("newNodeInfo(track=%s).ReleaseTrack = %q, want %q", track, got, track.String())
		}
		if info.GetNodeType() != nodeType || info.GetZoneId() != 3 ||
			info.GetGrpcEndpoint().GetPort() != 50000 || info.GetNodeUuid() == "" {
			t.Errorf("newNodeInfo(track=%s) lost other fields: %v", track, info)
		}
	}
}
