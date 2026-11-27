package server

import (
	"context"
	"testing"
	"time"

	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"

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

func channelIdentityServer(t *testing.T, ci *fakeChannelIdentities) *DataServiceServer {
	t.Helper()
	return NewDataServiceServer(&svc.ServiceContext{
		AccountDirectory:     &serverDirectoryFake{},
		AccountDirectoryAuth: directoryVerifier(t),
		ChannelIdentities:    ci,
	})
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

	out, err := srv.GetPlayerChannelIdentity(context.Background(), &dspb.GetPlayerChannelIdentityRequest{Account: "wx_u1"})
	if err != nil || out.GetOpenId() != "oid_1" || ci.byAccount != "wx_u1" || ci.byPlayer != 0 {
		t.Fatalf("lookup by account: out=%v err=%v byPlayer=%d byAccount=%q", out, err, ci.byPlayer, ci.byAccount)
	}
	_, err = srv.GetPlayerChannelIdentity(context.Background(), &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42, Account: "wx_other"})
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

	out, err := srv.GetPlayerChannelIdentity(context.Background(), &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42})
	if err != nil || !out.GetFound() || out.GetChannel() != "douyin_minigame" || out.GetPlayerLevel() != 25 {
		t.Fatalf("unexpected response: out=%v err=%v", out, err)
	}

	_, err = srv.GetPlayerChannelIdentity(context.Background(), &dspb.GetPlayerChannelIdentityRequest{PlayerId: 0})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for zero player_id, got %v", err)
	}

	ci.err = accountdirectory.ErrUnavailable
	out, err = srv.GetPlayerChannelIdentity(context.Background(), &dspb.GetPlayerChannelIdentityRequest{PlayerId: 42})
	if err != nil || !out.GetRetry() {
		t.Fatalf("expected retry=true on transient error, got out=%v err=%v", out, err)
	}
}
