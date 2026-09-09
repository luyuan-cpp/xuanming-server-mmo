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

// buildGuildIDMinter 的真值表:IdSegment 三种接法 × 号段成败,输出必须与设计稿 §6.4 一致。
func TestBuildGuildIDMinter_Policy(t *testing.T) {
	segDown := errors.New("segment unavailable")
	snowflakeOK := func() (uint64, error) { return 67000000000000000, nil }

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
			wantID:  67000000000000000,
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
			wantID:  67000000000000000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := buildGuildIDMinter(tc.cfg, tc.segment, snowflakeOK)
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

// NewGuildIDMinter 收到 nil *Client(号段关闭)时不能把 typed-nil 塞进 Source 接口,
// 否则 Minter 会去调 nil 客户端的 Next。
func TestNewGuildIDMinter_NilClientMeansSnowflake(t *testing.T) {
	m := NewGuildIDMinter(idsegment.Conf{Enabled: true}, nil, func() (uint64, error) { return 9, nil })
	id, err := m.Mint(context.Background())
	if err != nil || id != 9 {
		t.Fatalf("id=%d err=%v, want snowflake id 9", id, err)
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
