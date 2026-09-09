package svc

import (
	"context"
	"errors"
	"testing"

	"github.com/zeromicro/go-zero/zrpc"

	"shared/idsegment"
)

type fakeSegment struct {
	id  uint64
	err error
}

func (f *fakeSegment) Next(context.Context) (uint64, error) { return f.id, f.err }

// buildPlayerIDMinter 的真值表:IdSegment 三种接法 × 号段成败,输出必须与设计稿 §6.4 一致。
func TestBuildPlayerIDMinter_Policy(t *testing.T) {
	segDown := errors.New("segment unavailable")
	snowflakeOK := func() (uint64, error) { return 280000000000000000, nil }

	cases := []struct {
		name    string
		cfg     idsegment.Conf
		segment idsegment.Source
		wantID  uint64
		wantErr error
	}{
		{
			name:    "disabled → snowflake even though a segment client exists",
			cfg:     idsegment.Conf{Enabled: false},
			segment: &fakeSegment{id: 5},
			wantID:  280000000000000000,
		},
		{
			name:    "enabled, segment ok → segment id",
			cfg:     idsegment.Conf{Enabled: true},
			segment: &fakeSegment{id: 5},
			wantID:  5,
		},
		{
			name:    "enabled, segment down, fallback off (default) → fail closed",
			cfg:     idsegment.Conf{Enabled: true, FallbackToSnowflake: false},
			segment: &fakeSegment{err: segDown},
			wantErr: segDown,
		},
		{
			name:    "enabled, segment down, fallback on → snowflake id",
			cfg:     idsegment.Conf{Enabled: true, FallbackToSnowflake: true},
			segment: &fakeSegment{err: segDown},
			wantID:  280000000000000000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := buildPlayerIDMinter(tc.cfg, tc.segment, snowflakeOK)
			id, err := m.Mint(context.Background())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if id != 0 {
					t.Fatalf("id = %d on failure, must be 0", id)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("id=%d err=%v, want id=%d", id, err, tc.wantID)
			}
		})
	}
}

// SnowFlake 还没被 SetNodeId 装上时,回退必须报错而不是解引用 nil。
func TestSnowflakePlayerID_NotInitialised(t *testing.T) {
	s := &ServiceContext{}
	if _, err := s.snowflakePlayerID(); err == nil {
		t.Fatal("want an error when SnowFlake is nil")
	}
}

func TestHasRpcTarget(t *testing.T) {
	if hasRpcTarget(zrpc.RpcClientConf{}) {
		t.Fatal("empty conf must not count as a target")
	}
	c := zrpc.RpcClientConf{}
	c.Etcd.Key = "dataservice.rpc"
	if !hasRpcTarget(c) {
		t.Fatal("etcd key is a target")
	}
	if !hasRpcTarget(zrpc.RpcClientConf{Endpoints: []string{"127.0.0.1:9000"}}) {
		t.Fatal("endpoints are a target")
	}
	if !hasRpcTarget(zrpc.RpcClientConf{Target: "dns:///data-service:9000"}) {
		t.Fatal("target is a target")
	}
}
