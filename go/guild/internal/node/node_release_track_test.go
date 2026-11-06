package node

import (
	"strings"
	"testing"

	base "proto/common/base"
	"shared/releasetrack"
)

// 发布轨道的环境变量非法时,NewNode 在碰 etcd 之前就返回错误,进程随之拒启。
// 本用例不准备配置:校验若排到连 etcd 之后,返回的会是连接错误,错误里不会提到环境变量名。
func TestNewNodeRejectsInvalidReleaseTrackBeforeEtcd(t *testing.T) {
	t.Setenv(releasetrack.EnvVar, "beta")
	n, err := NewNode(uint32(base.ENodeType_GuildNodeService), "127.0.0.1", 9190)
	if err == nil {
		t.Fatalf("NewNode with %s=beta: want error, got node %+v", releasetrack.EnvVar, n)
	}
	if !strings.Contains(err.Error(), releasetrack.EnvVar) {
		t.Fatalf("NewNode with %s=beta: error should name the env var, got %v", releasetrack.EnvVar, err)
	}
}

// 注册值带上本进程的轨道。重注册沿用同一个 Info,所以核对构造点就够了。
func TestNewNodeInfoCarriesReleaseTrack(t *testing.T) {
	nodeType := uint32(base.ENodeType_GuildNodeService)
	for _, track := range []releasetrack.Track{releasetrack.Stable, releasetrack.Canary} {
		info := newNodeInfo(nodeType, 3, "10.0.0.7", 9190, track)
		if got := info.GetReleaseTrack(); got != track.String() {
			t.Errorf("newNodeInfo(track=%s).ReleaseTrack = %q, want %q", track, got, track.String())
		}
		if info.GetNodeType() != nodeType || info.GetZoneId() != 3 ||
			info.GetGrpcEndpoint().GetPort() != 9190 || info.GetNodeUuid() == "" {
			t.Errorf("newNodeInfo(track=%s) lost other fields: %v", track, info)
		}
	}
}
