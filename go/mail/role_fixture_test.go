package main

import (
	"context"

	rfpb "proto/role_fence"
)

// noGuilds:夹具里的玩家都没有帮。帮会目录在生产是 guild 服务的 RPC。
type noGuilds struct{}

func (noGuilds) GuildOf(context.Context, uint64) (uint64, error) { return 0, nil }

// fixedRoleOwner 是目录 GetRoleFenceState 的替身:所有角色都归同一个账号。
type fixedRoleOwner struct {
	account  string
	revision uint64
}

func (f fixedRoleOwner) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	return &rfpb.RoleFenceState{PlayerId: player, Account: f.account, OwnerRevision: f.revision, Version: 1, HomeZone: 1}, nil
}
