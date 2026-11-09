package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"login/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const sqlHealPlayerID = uint64(7001)

func sqlHealState() enterGameSessionState {
	return enterGameSessionState{playerID: sqlHealPlayerID, account: "sql-heal-account", ownerRevision: 1}
}

func sqlHealRole() *base.AccountSimplePlayer {
	role := &base.AccountSimplePlayer{
		PlayerId: sqlHealPlayerID, Name: "SQL原角色名", ClassId: 3,
		AppearanceId: "04_mountain_guardian_boy", Gender: 1,
	}
	role.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 91, protowire.BytesType), []byte("未来人物字段")))
	return role
}

func snapshotHealOwners(d *testDirectory) map[uint64]*dspb.AccountRoleOwnership {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[uint64]*dspb.AccountRoleOwnership, len(d.owners))
	for id, owned := range d.owners {
		out[id] = proto.Clone(owned).(*dspb.AccountRoleOwnership)
	}
	return out
}

func assertHealOwnersUnchanged(t *testing.T, d *testDirectory, before map[uint64]*dspb.AccountRoleOwnership) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.owners) != len(before) {
		t.Fatalf("角色目录数量被改变: got=%d want=%d", len(d.owners), len(before))
	}
	for id, owned := range before {
		if !proto.Equal(d.owners[id], owned) {
			t.Fatalf("角色原身份、未知字段或归属被覆盖: player=%d got=%v want=%v", id, d.owners[id], owned)
		}
	}
}

func TestHealAccountRole_SQLReadPreservesIdentityUnknownFieldsAndOtherRoles(t *testing.T) {
	directory := newTestDirectory()
	state := sqlHealState()
	role := sqlHealRole()
	directory.seed(state.account, role, &base.AccountSimplePlayer{PlayerId: 7002, Name: "并发建角", AppearanceId: "08_alchemy_prodigy_boy"})
	before := snapshotHealOwners(directory)
	// 不装配 Redis，SQL 原归属是唯一授权来源；传入的注册表名不能覆盖已存名字。
	logic := NewEnterGameLogic(context.Background(), &svc.ServiceContext{AccountDirectory: directory})
	got := logic.healAccountRole(context.Background(), state, "陈旧注册表名")
	if !proto.Equal(got, role) {
		t.Fatalf("自愈只读应返回完整 SQL 人物身份: got=%v want=%v", got, role)
	}
	assertHealOwnersUnchanged(t, directory, before)
}

func TestHealAccountRole_RejectsMissingChangedOrUnprovenSQLOwner(t *testing.T) {
	for _, rejected := range []string{"missing", "other-account", "changed-revision", "unproven", "dependency", "unready"} {
		t.Run(rejected, func(t *testing.T) {
			directory := newTestDirectory()
			state := sqlHealState()
			directory.seed(state.account, sqlHealRole())
			switch rejected {
			case "missing":
				delete(directory.owners, state.playerID)
			case "other-account":
				directory.owners[state.playerID].Account = "买家新账号"
			case "changed-revision":
				directory.owners[state.playerID].OwnerRevision++
			case "unproven":
				state.ownerRevision = 0
			case "dependency":
				directory.lookupErr = errors.New("SQL目录读取失败")
			}
			before := snapshotHealOwners(directory)
			sc := &svc.ServiceContext{AccountDirectory: directory}
			if rejected == "unready" {
				sc.AccountDirectory = nil
			}
			logic := NewEnterGameLogic(context.Background(), sc)
			if got := logic.healAccountRole(context.Background(), state, "注册表有名字也不能回填"); got != nil {
				t.Fatalf("缺失或变化的 SQL 归属不得恢复角色: %v", got)
			}
			assertHealOwnersUnchanged(t, directory, before)
		})
	}
}
