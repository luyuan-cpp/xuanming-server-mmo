package data

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	base "proto/common/base"
	guildpb "proto/guild"
	table "shared/generated/pb/table"
)

type fakeGuildClient struct {
	guildpb.GuildServiceClient
	in  *guildpb.GetPlayerGuildRequest
	out *guildpb.GetPlayerGuildResponse
	err error
}

func (f *fakeGuildClient) GetPlayerGuild(_ context.Context, in *guildpb.GetPlayerGuildRequest, _ ...grpc.CallOption) (*guildpb.GetPlayerGuildResponse, error) {
	f.in = in
	return f.out, f.err
}

func TestGuildRPCDirectoryMapsReplies(t *testing.T) {
	boom := errors.New("unavailable")
	for _, tc := range []struct {
		name    string
		out     *guildpb.GetPlayerGuildResponse
		err     error
		want    uint64
		wantErr bool
	}{
		{name: "在帮", out: &guildpb.GetPlayerGuildResponse{Guild: &guildpb.GuildInfo{GuildId: 77}}, want: 77},
		{name: "不在帮是正常结果", out: &guildpb.GetPlayerGuildResponse{ErrorMessage: &base.TipInfoMessage{Id: uint32(table.GuildError_kGuildNotInGuild)}}},
		{name: "别的拒绝不能当成不在帮", out: &guildpb.GetPlayerGuildResponse{ErrorMessage: &base.TipInfoMessage{Id: uint32(table.CommonError_kServiceUnavailable)}}, wantErr: true},
		{name: "RPC 失败", err: boom, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeGuildClient{out: tc.out, err: tc.err}
			got, err := GuildRPCDirectory{Client: client}.GuildOf(context.Background(), 7)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %d err %v", got, err)
			}
			if client.in.GetPlayerId() != 7 {
				t.Fatalf("请求 %+v", client.in)
			}
		})
	}
}

type failingGuilds struct{}

func (failingGuilds) GuildOf(context.Context, uint64) (uint64, error) {
	return 0, errors.New("guild unavailable")
}

// 读不到按没有帮算:邮件不因为帮会服务抖动而失败。
func TestGuildOfTreatsLookupFailureAsNoGuild(t *testing.T) {
	r := &Repo{guilds: failingGuilds{}}
	if got := r.guildOf(context.Background(), 7); got != 0 {
		t.Fatalf("got %d", got)
	}
}
