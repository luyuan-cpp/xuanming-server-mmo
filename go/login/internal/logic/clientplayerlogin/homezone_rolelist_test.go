package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"login/internal/logic/pkg/homezone"
	pbbase "proto/common/base"
	dspb "proto/data_service"
)

type fakeZoneClient struct {
	dspb.DataServiceClient
	zones map[uint64]uint32
	err   error
}

func (f *fakeZoneClient) BatchGetPlayerHomeZone(_ context.Context, in *dspb.BatchGetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerHomeZoneResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[uint64]uint32{}
	for _, id := range in.GetPlayerIds() {
		if z, ok := f.zones[id]; ok {
			out[id] = z
		}
	}
	return &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: out}, nil
}

func storedRoles() []*pbbase.AccountSimplePlayer {
	return []*pbbase.AccountSimplePlayer{
		{PlayerId: 100, ZoneId: 1, ClassId: 2},
		{PlayerId: 101, ZoneId: 1, ClassId: 4},
	}
}

// Login 响应的角色列表:zone_id 来自映射(合服后的真归属),建角 zone 只是兜底。
func TestBuildRoleList_ZoneFromMapping(t *testing.T) {
	r := homezone.New(&fakeZoneClient{zones: map[uint64]uint32{100: 3}}, 0, 0, 0)
	got := buildRoleList(context.Background(), r, false, storedRoles())
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Player.ZoneId != 3 {
		t.Fatalf("player 100 zone=%d, want 3 from mapping", got[0].Player.ZoneId)
	}
	if got[1].Player.ZoneId != 1 {
		t.Fatalf("player 101 (absent from mapping) zone=%d, want stored 1", got[1].Player.ZoneId)
	}
}

// RPC 失败 / 开关关闭 / 没有 resolver:列表照常返回,zone 是建角值。
func TestBuildRoleList_FallbackKeepsStoredZone(t *testing.T) {
	cases := map[string]struct {
		r        *homezone.Resolver
		disabled bool
	}{
		"rpc error":    {r: homezone.New(&fakeZoneClient{err: errors.New("down")}, 0, 0, 0)},
		"disabled":     {r: homezone.New(&fakeZoneClient{zones: map[uint64]uint32{100: 3}}, 0, 0, 0), disabled: true},
		"nil resolver": {r: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := buildRoleList(context.Background(), tc.r, tc.disabled, storedRoles())
			if len(got) != 2 {
				t.Fatalf("len=%d, want 2 (login must not fail)", len(got))
			}
			for _, w := range got {
				if w.Player.ZoneId != 1 {
					t.Fatalf("player %d zone=%d, want stored 1", w.Player.PlayerId, w.Player.ZoneId)
				}
			}
		})
	}
	if got := buildRoleList(context.Background(), nil, false, nil); got != nil {
		t.Fatalf("empty role list must stay nil, got %v", got)
	}
}
