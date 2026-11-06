package main

import (
	"strings"
	"testing"

	"shared/releasetrack"

	"github.com/zeromicro/go-zero/core/discov"
)

// TestValidateReleaseTrackRegistration:canary 副本登记 go-zero 服务键时必须带 .canary 后缀,否则 guild 的内部调用会落到它上面
// (docs/design/release-track-gray-canary.md §2.5)。stable、不登记 go-zero 键的进程不受这条约束。
func TestValidateReleaseTrackRegistration(t *testing.T) {
	hosts := []string{"127.0.0.1:2379"}
	cases := []struct {
		name    string
		track   releasetrack.Track
		etcd    discov.EtcdConf
		wantErr bool
	}{
		{name: "stable registers the shared key", track: releasetrack.Stable, etcd: discov.EtcdConf{Hosts: hosts, Key: "matchservice.rpc"}},
		{name: "canary on the stable key is refused", track: releasetrack.Canary, etcd: discov.EtcdConf{Hosts: hosts, Key: "matchservice.rpc"}, wantErr: true},
		{name: "canary on a zone key without the suffix is refused", track: releasetrack.Canary, etcd: discov.EtcdConf{Hosts: hosts, Key: "matchservice.rpc.z1"}, wantErr: true},
		{name: "canary with an empty key is refused", track: releasetrack.Canary, etcd: discov.EtcdConf{Hosts: hosts}, wantErr: true},
		{name: "canary on the canary key starts", track: releasetrack.Canary, etcd: discov.EtcdConf{Hosts: hosts, Key: "matchservice.rpc.canary"}},
		{name: "canary on a zone canary key starts", track: releasetrack.Canary, etcd: discov.EtcdConf{Hosts: hosts, Key: "matchservice.rpc.z1.canary"}},
		{name: "canary without go-zero registration has nothing to check", track: releasetrack.Canary, etcd: discov.EtcdConf{Key: "matchservice.rpc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReleaseTrackRegistration(tc.track, tc.etcd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("key %q on track %s must be refused", tc.etcd.Key, tc.track)
				}
				if !strings.Contains(err.Error(), canaryRegistrationKeySuffix) {
					t.Fatalf("the error must name the required suffix %q, got %v", canaryRegistrationKeySuffix, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("key %q on track %s must be accepted, got %v", tc.etcd.Key, tc.track, err)
			}
		})
	}
}
