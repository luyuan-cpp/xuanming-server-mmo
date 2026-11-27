package server

import (
	"context"
	"testing"
	"time"

	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeChannelIdentities struct {
	upserts int
	lastAcc string
	lastCh  string
	lastOID string
	reply   *dspb.GetPlayerChannelIdentityResponse
	err     error
	// 最近一次读是按哪个角色 / 哪个账号查的。
	byPlayer  uint64
	byAccount string
}

func (f *fakeChannelIdentities) UpsertChannelIdentity(_ context.Context, account, channel, openID string) error {
	f.upserts++
	f.lastAcc = account
	f.lastCh = channel
	f.lastOID = openID
	return f.err
}

func (f *fakeChannelIdentities) ChannelIdentityOfPlayer(_ context.Context, player uint64) (*dspb.GetPlayerChannelIdentityResponse, error) {
	f.byPlayer, f.byAccount = player, ""
	return f.read()
}

func (f *fakeChannelIdentities) ChannelIdentityOfAccount(_ context.Context, account string) (*dspb.GetPlayerChannelIdentityResponse, error) {
	f.byPlayer, f.byAccount = 0, account
	return f.read()
}

func (f *fakeChannelIdentities) read() (*dspb.GetPlayerChannelIdentityResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.reply != nil {
		return f.reply, nil
	}
	return &dspb.GetPlayerChannelIdentityResponse{Found: false}, nil
}

const (
	channelIdentityChatSecret  = "chat-secret-0123456789abcdef0123456789"
	channelIdentityGuardSecret = "guard-secret-0123456789abcdef012345678"
)

func channelIdentityServer(t *testing.T, ci *fakeChannelIdentities) *DataServiceServer {
	t.Helper()
	verifier, err := serviceauth.New(map[string]string{
		accountdirectory.ChatCaller:         channelIdentityChatSecret,
		accountdirectory.ContentGuardCaller: channelIdentityGuardSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewDataServiceServer(&svc.ServiceContext{
		AccountDirectory:     &serverDirectoryFake{},
		AccountDirectoryAuth: directoryVerifier(t),
		ChannelIdentities:    ci,
		ChannelIdentityAuth:  verifier,
	})
}

// guardLookup 是 content_guard 签好名的一次查询。
func guardLookup(t *testing.T, in *dspb.GetPlayerChannelIdentityRequest) *dspb.GetPlayerChannelIdentityRequest {
	t.Helper()
	if err := accountdirectory.SignChannelIdentityRequest(in, accountdirectory.ContentGuardCaller, channelIdentityGuardSecret); err != nil {
		t.Fatal(err)
	}
	return in
}

// 应答里有账号与平台 openid:没有证明、证明是别的调用方的、用错密钥的一律拒绝,只有 content_guard 与 chat 能读。
func TestGetPlayerChannelIdentityRequiresContentGuardOrChatProof(t *testing.T) {
	ci := &fakeChannelIdentities{reply: &dspb.GetPlayerChannelIdentityResponse{Found: true, Account: "wx_u1", OpenId: "oid_1"}}
	srv := channelIdentityServer(t, ci)
	unsigned := &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}
	byLogin := &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}
	signDirectoryRequest(t, "GetPlayerChannelIdentity", byLogin, "login", uint64(time.Now().UnixMilli()))
	wrongKey := &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}
	if err := accountdirectory.SignChannelIdentityRequest(wrongKey, accountdirectory.ChatCaller, channelIdentityGuardSecret); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]*dspb.GetPlayerChannelIdentityRequest{"unsigned": unsigned, "login": byLogin, "wrong key": wrongKey} {
		if _, err := srv.GetPlayerChannelIdentity(context.Background(), in); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: want PermissionDenied, got %v", name, err)
		}
	}
	byChat := &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}
	if err := accountdirectory.SignChannelIdentityRequest(byChat, accountdirectory.ChatCaller, channelIdentityChatSecret); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]*dspb.GetPlayerChannelIdentityRequest{"chat": byChat, "content_guard": guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42})} {
		if out, err := srv.GetPlayerChannelIdentity(context.Background(), in); err != nil || out.GetOpenId() != "oid_1" {
			t.Fatalf("%s: want the identity, got out=%v err=%v", name, out, err)
		}
	}
	// 两把密钥都没配:接口停用,不是免验签。
	closed := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: &serverDirectoryFake{}, AccountDirectoryAuth: directoryVerifier(t), ChannelIdentities: ci})
	if _, err := closed.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42})); status.Code(err) != codes.Unavailable {
		t.Fatalf("without a verifier the RPC must be unavailable, got %v", err)
	}
}

func TestUpsertAccountChannelIdentityRequiresLoginProof(t *testing.T) {
	for _, caller := range []string{"", "trade", "chat"} {
		ci := &fakeChannelIdentities{}
		in := &dspb.UpsertAccountChannelIdentityRequest{
			Account: "wx_u1", Channel: "wechat_minigame", OpenId: "oid_1",
		}
		if caller != "" {
			signDirectoryRequest(t, "UpsertAccountChannelIdentity", in, caller, uint64(time.Now().UnixMilli()))
		}
		_, err := channelIdentityServer(t, ci).UpsertAccountChannelIdentity(context.Background(), in)
		if status.Code(err) != codes.PermissionDenied || ci.upserts != 0 {
			t.Fatalf("caller %q: err=%v upserts=%d", caller, err, ci.upserts)
		}
	}

	ci := &fakeChannelIdentities{}
	in := &dspb.UpsertAccountChannelIdentityRequest{
		Account: "wx_u1", Channel: "wechat_minigame", OpenId: "oid_1",
	}
	signDirectoryRequest(t, "UpsertAccountChannelIdentity", in, "login", uint64(time.Now().UnixMilli()))
	if _, err := channelIdentityServer(t, ci).UpsertAccountChannelIdentity(context.Background(), in); err != nil {
		t.Fatalf("login proof failed: %v", err)
	}
	if ci.upserts != 1 || ci.lastAcc != "wx_u1" || ci.lastCh != "wechat_minigame" || ci.lastOID != "oid_1" {
		t.Fatalf("unexpected recorded identity: %+v", ci)
	}
}

// 建角送检时角色还没发号:player_id 为 0 时按账号查。两个都给时以 player_id 为准。
func TestGetPlayerChannelIdentityLooksUpByAccountWhenThereIsNoPlayer(t *testing.T) {
	ci := &fakeChannelIdentities{
		reply: &dspb.GetPlayerChannelIdentityResponse{Found: true, Account: "wx_u1", Channel: "wechat_minigame", OpenId: "oid_1"},
	}
	srv := channelIdentityServer(t, ci)

	out, err := srv.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{Account: "wx_u1"}))
	if err != nil || out.GetOpenId() != "oid_1" || ci.byAccount != "wx_u1" || ci.byPlayer != 0 {
		t.Fatalf("lookup by account: out=%v err=%v byPlayer=%d byAccount=%q", out, err, ci.byPlayer, ci.byAccount)
	}
	_, err = srv.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42, Account: "wx_other"}))
	if err != nil || ci.byPlayer != 42 || ci.byAccount != "" {
		t.Fatalf("player_id must win over account: err=%v byPlayer=%d byAccount=%q", err, ci.byPlayer, ci.byAccount)
	}
}

func TestGetPlayerChannelIdentityReturnsProfileAndRetryOnTransientError(t *testing.T) {
	ci := &fakeChannelIdentities{
		reply: &dspb.GetPlayerChannelIdentityResponse{
			Found:            true,
			Account:          "dy_u2",
			Channel:          "douyin_minigame",
			OpenId:           "dy_oid_2",
			AccountCreatedMs: 1700000000000,
			PlayerLevel:      25,
		},
	}
	srv := channelIdentityServer(t, ci)

	out, err := srv.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}))
	if err != nil || !out.GetFound() || out.GetChannel() != "douyin_minigame" || out.GetPlayerLevel() != 25 {
		t.Fatalf("unexpected response: out=%v err=%v", out, err)
	}

	_, err = srv.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 0}))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for zero player_id, got %v", err)
	}

	ci.err = accountdirectory.ErrUnavailable
	out, err = srv.GetPlayerChannelIdentity(context.Background(), guardLookup(t, &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42}))
	if err != nil || !out.GetRetry() {
		t.Fatalf("expected retry=true on transient error, got out=%v err=%v", out, err)
	}
}
