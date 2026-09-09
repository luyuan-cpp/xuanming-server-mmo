package homezone

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pbbase "proto/common/base"
	dspb "proto/data_service"
)

// fakeDataService 只实现映射相关的三条 RPC;其余方法由内嵌的 nil 接口兜底(调到即 panic,
// 正好暴露「测试路径碰了不该碰的 RPC」)。
type fakeDataService struct {
	dspb.DataServiceClient
	zones      map[uint64]uint32
	err        error // 所有读 RPC 返回的错误
	getErr     error // 仅 GetPlayerHomeZone 返回的错误(模拟老版「未映射 = 错误」契约)
	regErr     error
	batchCalls int
	getCalls   int
	regCalls   int
	lastBatch  []uint64
	lastReg    *dspb.RegisterPlayerZoneRequest
}

func (f *fakeDataService) GetPlayerHomeZone(_ context.Context, in *dspb.GetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	f.getCalls++
	if f.err != nil {
		return nil, f.err
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &dspb.GetPlayerHomeZoneResponse{HomeZoneId: f.zones[in.GetPlayerId()]}, nil
}

func (f *fakeDataService) BatchGetPlayerHomeZone(_ context.Context, in *dspb.BatchGetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerHomeZoneResponse, error) {
	f.batchCalls++
	f.lastBatch = in.GetPlayerIds()
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

func (f *fakeDataService) RegisterPlayerZone(_ context.Context, in *dspb.RegisterPlayerZoneRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.regCalls++
	f.lastReg = in
	if f.regErr != nil {
		return nil, f.regErr
	}
	return &emptypb.Empty{}, nil
}

func roles(ids ...uint64) []*pbbase.AccountSimplePlayer {
	out := make([]*pbbase.AccountSimplePlayer, 0, len(ids))
	for _, id := range ids {
		out = append(out, &pbbase.AccountSimplePlayer{PlayerId: id, ClassId: 3, ZoneId: 1})
	}
	return out
}

func newResolver(f *fakeDataService) *Resolver { return New(f, 0, 0, 0) }

// (a) 映射里有的角色 zone_id 被覆盖;一次登录只发一条 Batch RPC;原切片不被改动。
func TestRefreshRoleZones_OverwritesFromBatch(t *testing.T) {
	fake := &fakeDataService{zones: map[uint64]uint32{10: 2, 11: 2}}
	r := newResolver(fake)
	stored := roles(10, 11)

	got := r.RefreshRoleZones(context.Background(), stored)

	if fake.batchCalls != 1 || len(fake.lastBatch) != 2 {
		t.Fatalf("batch calls=%d ids=%v, want exactly one call with both ids", fake.batchCalls, fake.lastBatch)
	}
	for _, p := range got {
		if p.ZoneId != 2 {
			t.Fatalf("player %d zone=%d, want 2 (resolved home zone)", p.PlayerId, p.ZoneId)
		}
		if p.ClassId != 3 {
			t.Fatalf("player %d class_id lost in clone", p.PlayerId)
		}
	}
	// 不回写账号 blob:调用方持有的原对象必须保持建角 zone。
	for _, p := range stored {
		if p.ZoneId != 1 {
			t.Fatalf("stored player %d mutated to zone %d; refresh must not touch the account blob", p.PlayerId, p.ZoneId)
		}
	}
}

// (b1) 映射里缺席 / 值为 0 的 id 保留建角 zone,其余仍被覆盖。
func TestRefreshRoleZones_MissingIdsKeepStoredZone(t *testing.T) {
	fake := &fakeDataService{zones: map[uint64]uint32{10: 5, 12: 0}}
	got := newResolver(fake).RefreshRoleZones(context.Background(), roles(10, 11, 12))

	want := map[uint64]uint32{10: 5, 11: 1, 12: 1}
	for _, p := range got {
		if p.ZoneId != want[p.PlayerId] {
			t.Fatalf("player %d zone=%d, want %d", p.PlayerId, p.ZoneId, want[p.PlayerId])
		}
	}
}

// (b2) RPC 失败 / 客户端未配置 → 全部保留建角 zone,且不 panic、不返回空列表。
func TestRefreshRoleZones_RPCErrorKeepsStoredZone(t *testing.T) {
	cases := map[string]*Resolver{
		"rpc error":    newResolver(&fakeDataService{err: errors.New("unavailable")}),
		"nil client":   New(nil, 0, 0, 0),
		"nil resolver": nil,
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			got := r.RefreshRoleZones(context.Background(), roles(10, 11))
			if len(got) != 2 {
				t.Fatalf("len=%d, want 2 (login must still return the role list)", len(got))
			}
			for _, p := range got {
				if p.ZoneId != 1 {
					t.Fatalf("player %d zone=%d, want stored 1", p.PlayerId, p.ZoneId)
				}
			}
		})
	}
}

// (c) 归属 zone 与本 zone 不同 → 返回归属 zone 并标记重定向。
func TestResolveEnterZone_OverridesWhenHomeDiffers(t *testing.T) {
	fake := &fakeDataService{zones: map[uint64]uint32{10: 7}}
	zone, redirected := ResolveEnterZone(context.Background(), newResolver(fake), 10, 1)
	if zone != 7 || !redirected {
		t.Fatalf("zone=%d redirected=%v, want 7/true", zone, redirected)
	}
	if fake.getCalls != 1 {
		t.Fatalf("GetPlayerHomeZone calls=%d, want 1", fake.getCalls)
	}
}

// (d) 归属 == 本 zone / 映射为 0 / 查询失败 / 未配置 → 维持本 zone,不重定向。
func TestResolveEnterZone_NoOverride(t *testing.T) {
	cases := map[string]Lookup{
		"equal":      newResolver(&fakeDataService{zones: map[uint64]uint32{10: 1}}),
		"unmapped":   newResolver(&fakeDataService{zones: map[uint64]uint32{}}),
		"rpc error":  newResolver(&fakeDataService{err: errors.New("unavailable")}),
		"nil client": New(nil, 0, 0, 0),
		"nil lookup": nil,
	}
	for name, lk := range cases {
		t.Run(name, func(t *testing.T) {
			zone, redirected := ResolveEnterZone(context.Background(), lk, 10, 1)
			if zone != 1 || redirected {
				t.Fatalf("zone=%d redirected=%v, want 1/false", zone, redirected)
			}
		})
	}
}

// (e) 「未映射」的两种服务端契约都归一成 (0, nil):老版 Unknown+"no home zone mapping"、
// gRPC NotFound、新版 HomeZoneId=0。只有真正的传输层错误才原样返回。
func TestHomeZone_UnmappedContracts(t *testing.T) {
	cases := map[string]struct {
		fake    *fakeDataService
		wantErr bool
	}{
		"legacy unknown + text": {fake: &fakeDataService{getErr: status.Error(codes.Unknown, "no home zone mapping for player 10")}},
		"legacy plain error":    {fake: &fakeDataService{getErr: errors.New("no home zone mapping for player 10")}},
		"grpc not found":        {fake: &fakeDataService{getErr: status.Error(codes.NotFound, "player 10")}},
		"new contract zero":     {fake: &fakeDataService{zones: map[uint64]uint32{}}},
		"unavailable":           {fake: &fakeDataService{getErr: status.Error(codes.Unavailable, "connection refused")}, wantErr: true},
		"deadline":              {fake: &fakeDataService{getErr: context.DeadlineExceeded}, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			zone, err := newResolver(tc.fake).HomeZone(context.Background(), 10)
			if zone != 0 {
				t.Fatalf("zone=%d, want 0", zone)
			}
			if tc.wantErr && err == nil {
				t.Fatal("transport error must surface as err, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unmapped must be (0, nil), got err=%v", err)
			}
		})
	}
}

// (f) RegisterPlayerZone:成功透传参数;RPC 失败 / 未配置 / 参数为 0 一律返回错误
// (建角侧据此 fail-closed)。
func TestRegisterPlayerZone(t *testing.T) {
	ok := &fakeDataService{}
	if err := newResolver(ok).RegisterPlayerZone(context.Background(), 42, 3); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok.regCalls != 1 || ok.lastReg.GetPlayerId() != 42 || ok.lastReg.GetHomeZoneId() != 3 {
		t.Fatalf("register call = %d/%v, want 1 call with player 42 → zone 3", ok.regCalls, ok.lastReg)
	}

	failing := map[string]struct {
		r          *Resolver
		player     uint64
		zone       uint32
		wantNoCall bool
	}{
		"rpc error":   {r: newResolver(&fakeDataService{regErr: status.Error(codes.Unavailable, "down")}), player: 1, zone: 1},
		"nil client":  {r: New(nil, 0, 0, 0), player: 1, zone: 1, wantNoCall: true},
		"nil resolver": {r: nil, player: 1, zone: 1, wantNoCall: true},
		"zero zone":   {r: newResolver(&fakeDataService{}), player: 1, zone: 0, wantNoCall: true},
		"zero player": {r: newResolver(&fakeDataService{}), player: 0, zone: 1, wantNoCall: true},
	}
	for name, tc := range failing {
		t.Run(name, func(t *testing.T) {
			if err := tc.r.RegisterPlayerZone(context.Background(), tc.player, tc.zone); err == nil {
				t.Fatal("want error")
			}
			if tc.wantNoCall && tc.r != nil && tc.r.Client != nil && tc.r.Client.(*fakeDataService).regCalls != 0 {
				t.Fatal("invalid arguments must be rejected before hitting the RPC")
			}
		})
	}
}

// (g) 三个超时各自独立,<=0 落到各自默认值。
func TestNewTimeouts(t *testing.T) {
	r := New(nil, 0, 0, 0)
	if r.RoleListTimeout != DefaultRoleListLookupTimeout || r.EnterTimeout != DefaultEnterLookupTimeout || r.RegisterTimeout != DefaultRegisterTimeout {
		t.Fatalf("defaults = %s/%s/%s", r.RoleListTimeout, r.EnterTimeout, r.RegisterTimeout)
	}
	r = New(nil, 1, 2, 3)
	if r.RoleListTimeout != 1 || r.EnterTimeout != 2 || r.RegisterTimeout != 3 {
		t.Fatalf("explicit = %s/%s/%s", r.RoleListTimeout, r.EnterTimeout, r.RegisterTimeout)
	}
}
