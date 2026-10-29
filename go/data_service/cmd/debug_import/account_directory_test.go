package main

import (
	"context"
	"data_service/cmd/debugutil"
	"errors"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dbpb "proto/common/database"
	"shared/accountdirectory"
	"shared/savebarrier"
	"testing"
)

func TestImportedAccountRolePreservesSQLRoleUnknownFields(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 17, ClassId: 3, Name: "导入角色", AppearanceId: "04_mountain_guardian_boy"}
	role.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 91, protowire.BytesType), []byte("future-role")))
	bytes, err := proto.Marshal(role)
	require.NoError(t, err)
	raw, err := proto.Marshal(&dbpb.AccountRole{PlayerId: 17, Account: "原账号", RoleProto: bytes, OwnerRevision: 9})
	require.NoError(t, err)
	value := debugutil.RenderProto(raw, func() proto.Message { return &dbpb.AccountRole{} }, nil)
	bundle := &debugutil.Bundle{Global: &debugutil.BundleGlobal{AccountRole: &value}}
	out, err := importedAccountRole(bundle, &importPlan{playerID: 17})
	require.NoError(t, err)
	require.True(t, proto.Equal(role, out))
	_, err = importedAccountRole(bundle, &importPlan{playerID: 18})
	require.Error(t, err, "不能把别人的SQL目录原件导入到本角色")
	bundle.Global.AccountRole = &debugutil.Value{}
	_, err = importedAccountRole(bundle, &importPlan{playerID: 17})
	require.Error(t, err, "有目录原件字段却无字节不能静默猜测")
}

func TestLocalImportStableRequestTermsAndResumeDoNotChangeIdentity(t *testing.T) {
	b := &debugutil.Bundle{PlayerID: 17, ExportedAtUTC: "2026-10-21T00:00:00Z", Format: debugutil.BundleFormat, FormatVersion: debugutil.BundleFormatVersion}
	one, err := localImportRequestID(b, "sql", 1, "")
	require.NoError(t, err)
	again, err := localImportRequestID(b, "sql", 1, "")
	require.NoError(t, err)
	require.Equal(t, one, again)
	for _, terms := range []struct {
		source  string
		zone    uint32
		account string
	}{{"redis-parent", 1, ""}, {"sql", 2, ""}, {"sql", 1, "new-account"}} {
		changed, err := localImportRequestID(b, terms.source, terms.zone, terms.account)
		require.NoError(t, err)
		require.NotEqual(t, one, changed)
	}
}

func TestLocalImportAuthorizationAndSQLFailureKeepPendingWithoutACK(t *testing.T) {
	for _, mode := range []string{"open-admission", "sql-failure", "success"} {
		t.Run(mode, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			ctx := context.Background()
			version := savebarrier.Version(17, 3)
			require.NoError(t, stageLocalRedis(ctx, client, 17, 1, "", 17, version))
			var steps []string
			err := applyAuthorizedLocalImport(func() error {
				steps = append(steps, "authorize")
				if mode == "open-admission" {
					return accountdirectory.ErrConflict
				}
				return nil
			}, func() error {
				steps = append(steps, "sql")
				if mode == "sql-failure" {
					return errors.New("SQL unavailable")
				}
				return nil
			}, func() error {
				steps = append(steps, "ack")
				return commitLocalRedis(ctx, client, 17, version)
			})
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, []string{"authorize", "sql", "ack"}, steps)
				require.Equal(t, version, mr.HGet(savebarrier.Key(17), "player_database"))
			} else {
				require.Error(t, err)
				require.Equal(t, version, mr.HGet(savebarrier.Key(17), "pending"))
				require.Empty(t, mr.HGet(savebarrier.Key(17), "player_database"))
				require.Empty(t, mr.HGet(savebarrier.Key(17), "player_database_1"))
				if mode == "open-admission" {
					require.Equal(t, []string{"authorize"}, steps)
				} else {
					require.Equal(t, []string{"authorize", "sql"}, steps)
				}
			}
		})
	}
}

func TestLegacyBundleRoleUsesPlayerIdentityWithoutRedisOwnership(t *testing.T) {
	player := &dbpb.PlayerDatabase{PlayerId: 17}
	plan := &importPlan{playerID: 17, rows: []planRow{{msg: player}}}
	role, err := importedAccountRole(&debugutil.Bundle{}, plan)
	require.NoError(t, err)
	require.EqualValues(t, 17, role.PlayerId)
	_, err = importedAccountRole(&debugutil.Bundle{}, &importPlan{playerID: 17})
	require.Error(t, err)
}
