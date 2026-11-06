package svc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"player_inbox/internal/config"

	base "proto/common/base"
	componentpb "proto/common/component"
	dspb "proto/data_service"

	"shared/accountdirectory"
	"shared/rolefence"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeDataService 只实现本服务用到的两个方法;其余方法走嵌入的 nil 接口(被调用即 panic,说明多调了)。
type fakeDataService struct {
	dspb.DataServiceClient
	homeZone    *dspb.GetPlayerHomeZoneResponse
	homeZoneErr error
	ledger      *dspb.GetPlayerInboxLedgerResponse
	ledgerErr   error
	block       *dspb.GetPlayerItemGainBlockResponse
	blockErr    error
	blockIn     *dspb.GetPlayerItemGainBlockRequest

	admissionIn    *dspb.VerifyRoleAdmissionRequest
	admissionValid bool
	admissionErr   error
}

func (f *fakeDataService) VerifyRoleAdmission(_ context.Context, in *dspb.VerifyRoleAdmissionRequest, _ ...grpc.CallOption) (*dspb.VerifyRoleAdmissionResponse, error) {
	f.admissionIn = in
	if f.admissionErr != nil {
		return nil, f.admissionErr
	}
	return &dspb.VerifyRoleAdmissionResponse{Valid: f.admissionValid}, nil
}

func (f *fakeDataService) GetPlayerHomeZone(context.Context, *dspb.GetPlayerHomeZoneRequest, ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	return f.homeZone, f.homeZoneErr
}

func (f *fakeDataService) GetPlayerInboxLedger(context.Context, *dspb.GetPlayerInboxLedgerRequest, ...grpc.CallOption) (*dspb.GetPlayerInboxLedgerResponse, error) {
	return f.ledger, f.ledgerErr
}

func (f *fakeDataService) GetPlayerItemGainBlock(_ context.Context, in *dspb.GetPlayerItemGainBlockRequest, _ ...grpc.CallOption) (*dspb.GetPlayerItemGainBlockResponse, error) {
	f.blockIn = in
	return f.block, f.blockErr
}

// TestDataServicePlayers 钉住玩家存在校验的映射(设计 §6.2 第 2 步):映射缺失 = 不存在;查询失败 = 故障(fail-closed)。
func TestDataServicePlayers(t *testing.T) {
	cases := []struct {
		name    string
		ds      *fakeDataService
		exists  bool
		wantErr bool
	}{
		{"有映射", &fakeDataService{homeZone: &dspb.GetPlayerHomeZoneResponse{HomeZoneId: 3}}, true, false},
		{"映射值为 0", &fakeDataService{homeZone: &dspb.GetPlayerHomeZoneResponse{}}, false, false},
		{"NotFound", &fakeDataService{homeZoneErr: status.Error(codes.NotFound, "no home zone mapping")}, false, false},
		{"旧版 Unknown + 文案", &fakeDataService{homeZoneErr: status.Error(codes.Unknown, "player 1: no home zone mapping")}, false, false},
		{"Unknown 其它原因", &fakeDataService{homeZoneErr: status.Error(codes.Unknown, "boom")}, false, true},
		{"Unavailable", &fakeDataService{homeZoneErr: status.Error(codes.Unavailable, "down")}, false, true},
		{"非 gRPC 错误", &fakeDataService{homeZoneErr: errors.New("dial")}, false, true},
	}
	for _, tc := range cases {
		p := &DataServicePlayers{Client: tc.ds}
		exists, err := p.PlayerExists(context.Background(), 42)
		if exists != tc.exists || (err != nil) != tc.wantErr {
			t.Errorf("%s: PlayerExists = (%v, %v), want (%v, err=%v)", tc.name, exists, err, tc.exists, tc.wantErr)
		}
	}
	if _, err := (&DataServicePlayers{}).PlayerExists(context.Background(), 1); err == nil {
		t.Error("未装配客户端必须报错(fail-closed),不能当成「玩家不存在」")
	}
}

// TestDataServiceLedgers 钉住账本读取的映射(与 assetop.DataServiceLedger 同口径)。
func TestDataServiceLedgers(t *testing.T) {
	l := &DataServiceLedgers{Client: &fakeDataService{ledger: &dspb.GetPlayerInboxLedgerResponse{Found: false}}}
	if ledger, found, err := l.ReadInboxLedger(context.Background(), 1); err != nil || found || ledger != nil {
		t.Errorf("found=false → (nil, false, nil),got (%v, %v, %v)", ledger, found, err)
	}
	l = &DataServiceLedgers{Client: &fakeDataService{ledger: &dspb.GetPlayerInboxLedgerResponse{Found: true}}}
	if ledger, found, err := l.ReadInboxLedger(context.Background(), 1); err != nil || !found || ledger == nil {
		t.Errorf("found=true 且账本缺席 → 空账本,got (%v, %v, %v)", ledger, found, err)
	}
	want := &componentpb.PlayerInboxLedgerComp{Cursors: []*componentpb.PlayerInboxDomainCursor{{Domain: 1, DecidedThroughSeq: 9}}}
	l = &DataServiceLedgers{Client: &fakeDataService{ledger: &dspb.GetPlayerInboxLedgerResponse{Found: true, Ledger: want}}}
	if ledger, found, err := l.ReadInboxLedger(context.Background(), 1); err != nil || !found || ledger != want {
		t.Errorf("found=true → 原账本,got (%v, %v, %v)", ledger, found, err)
	}
	l = &DataServiceLedgers{Client: &fakeDataService{ledgerErr: status.Error(codes.Unimplemented, "old data_service")}}
	if _, _, err := l.ReadInboxLedger(context.Background(), 1); status.Code(errors.Unwrap(err)) != codes.Unimplemented {
		t.Errorf("gRPC 错误必须原样(可取 code)返回,got %v", err)
	}
}

// TestDataServiceItemGainBlocks 钉住物品获取封禁表读取的映射(与 DataServiceLedgers 同口径,设计 §2.2)。
func TestDataServiceItemGainBlocks(t *testing.T) {
	ds := &fakeDataService{block: &dspb.GetPlayerItemGainBlockResponse{Found: false}}
	b := &DataServiceItemGainBlocks{Client: ds}
	if block, found, err := b.ReadItemGainBlock(context.Background(), 42); err != nil || found || block != nil {
		t.Errorf("found=false → (nil, false, nil),got (%v, %v, %v)", block, found, err)
	}
	if ds.blockIn.GetPlayerId() != 42 {
		t.Errorf("请求的 player_id = %d, want 42", ds.blockIn.GetPlayerId())
	}
	b = &DataServiceItemGainBlocks{Client: &fakeDataService{block: &dspb.GetPlayerItemGainBlockResponse{Found: true}}}
	if block, found, err := b.ReadItemGainBlock(context.Background(), 42); err != nil || !found || block == nil {
		t.Errorf("found=true 且封禁表缺席 → 空表,got (%v, %v, %v)", block, found, err)
	}
	want := &componentpb.PlayerItemGainBlockComp{ItemConfigIds: []uint32{30045}}
	b = &DataServiceItemGainBlocks{Client: &fakeDataService{block: &dspb.GetPlayerItemGainBlockResponse{Found: true, Block: want}}}
	if block, found, err := b.ReadItemGainBlock(context.Background(), 42); err != nil || !found || block != want {
		t.Errorf("found=true → 原封禁表,got (%v, %v, %v)", block, found, err)
	}
	b = &DataServiceItemGainBlocks{Client: &fakeDataService{blockErr: status.Error(codes.Unimplemented, "old data_service")}}
	if _, _, err := b.ReadItemGainBlock(context.Background(), 42); status.Code(errors.Unwrap(err)) != codes.Unimplemented {
		t.Errorf("gRPC 错误必须原样(可取 code)返回,got %v", err)
	}
	if _, _, err := (&DataServiceItemGainBlocks{}).ReadItemGainBlock(context.Background(), 42); err == nil {
		t.Error("没装配 data_service 客户端必须报错,不能回「没有存档」")
	}
}

func TestBuildDSNPinsSessionSettings(t *testing.T) {
	dsn := BuildDSN(configMySQL())
	for _, want := range []string{"transaction_isolation=%27READ-COMMITTED%27", "innodb_lock_wait_timeout=1", "STRICT_TRANS_TABLES", "/mmorpg_player_inbox?"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 缺 %q", want)
		}
	}
	if target := MySQLTarget(configMySQL()); strings.Contains(target, "secret-pass") {
		t.Errorf("MySQLTarget 不得含密码: %s", target)
	}
}

func configMySQL() config.MySQLConf {
	return config.MySQLConf{Host: "127.0.0.1:3306", User: "appuser", Password: "secret-pass", DBName: "mmorpg_player_inbox"}
}

// TestRoleAdmissionVerifier 钉住收件箱用的入场登记校验:请求带 player_inbox 的调用证明,false 不是错误,RPC 失败是错误(不能当成登记失效)。
func TestRoleAdmissionVerifier(t *testing.T) {
	secret := strings.Repeat("i", 32)
	admission := &base.RoleAdmissionIdentity{PlayerId: 11, OwnerRevision: 1}
	ds := &fakeDataService{admissionValid: true}
	a, err := rolefence.NewAdmissionVerifier(ds, RoleFenceCaller, secret)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := a.VerifyRoleAdmission(context.Background(), admission)
	if err != nil || !valid {
		t.Fatalf("valid=%v err=%v", valid, err)
	}
	auth, err := serviceauth.New(map[string]string{RoleFenceCaller: secret})
	if err != nil {
		t.Fatal(err)
	}
	if caller, err := auth.Verify(context.Background(), accountdirectory.MethodPrefix+"VerifyRoleAdmission", ds.admissionIn); err != nil || caller != RoleFenceCaller {
		t.Fatalf("调用证明 caller=%q err=%v", caller, err)
	}
	ds.admissionValid = false
	if valid, err = a.VerifyRoleAdmission(context.Background(), admission); err != nil || valid {
		t.Fatalf("登记失效是 false,不是错误: valid=%v err=%v", valid, err)
	}
	ds.admissionErr = status.Error(codes.Unavailable, "down")
	if _, err = a.VerifyRoleAdmission(context.Background(), admission); !errors.Is(err, rolefence.ErrUnavailable) {
		t.Fatalf("RPC 失败必须报不可用: %v", err)
	}
	if _, err = rolefence.NewAdmissionVerifier(nil, RoleFenceCaller, secret); err == nil {
		t.Fatal("未装配客户端必须报错")
	}
	if _, err = rolefence.NewAdmissionVerifier(ds, RoleFenceCaller, "short"); err == nil {
		t.Fatal("密钥不合法必须报错")
	}
}
