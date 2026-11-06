package noderegistry

import (
	"strings"
	"testing"

	base "proto/common/base"
	"shared/releasetrack"
)

// 发布轨道的环境变量非法时,Register 在碰 etcd 之前就返回错误,进程随之拒启。
// 故意传 nil client:校验若排到任何 etcd 调用之后,这里会空指针 panic,而不是返回错误。
func TestRegisterRejectsInvalidReleaseTrackBeforeEtcd(t *testing.T) {
	t.Setenv(releasetrack.EnvVar, "beta")
	reg, err := Register(nil, uint32(base.ENodeType_SceneManagerNodeService), 1, "127.0.0.1", 9150, 30)
	if err == nil {
		t.Fatalf("Register with %s=beta: want error, got registration %+v", releasetrack.EnvVar, reg)
	}
	if !strings.Contains(err.Error(), releasetrack.EnvVar) {
		t.Fatalf("Register with %s=beta: error should name the env var, got %v", releasetrack.EnvVar, err)
	}
}

// 注册值带上本进程的轨道。换号重注册沿用同一个 Info,所以核对构造点就够了。
func TestNewNodeInfoCarriesReleaseTrack(t *testing.T) {
	nodeType := uint32(base.ENodeType_SceneManagerNodeService)
	for _, track := range []releasetrack.Track{releasetrack.Stable, releasetrack.Canary} {
		info := newNodeInfo(nodeType, 3, "10.0.0.7", 9150, track)
		if got := info.GetReleaseTrack(); got != track.String() {
			t.Errorf("newNodeInfo(track=%s).ReleaseTrack = %q, want %q", track, got, track.String())
		}
		if info.GetNodeType() != nodeType || info.GetZoneId() != 3 ||
			info.GetGrpcEndpoint().GetPort() != 9150 || info.GetNodeUuid() == "" {
			t.Errorf("newNodeInfo(track=%s) lost other fields: %v", track, info)
		}
	}
}
