package roleidentity

import (
	"context"
	"testing"

	base "proto/common/base"
	"shared/callerauth/callerauthtest"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const onlineMethod = "/test.Online/Act"

func TestSessionAccountReturnsSignedOriginalAccountWithoutAnyStore(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	ctx := callerauthtest.Incoming(t, onlineMethod, in)

	account, err := SessionAccount(ctx, in)
	require.NoError(t, err, "已签名的在线身份不读任何登录存储")
	require.Equal(t, "原账号", account)

	bound, err := CaptureSession(ctx, in)
	require.NoError(t, err)
	captured, ok := Original(bound)
	require.True(t, ok)
	require.Equal(t, "原账号", captured.Account)
	require.EqualValues(t, 3, captured.OwnerRevision)
	require.EqualValues(t, 11, captured.Role.GetPlayerId())
}

func TestSessionAccountRejectsClaimsThatDifferFromTheSignedOriginal(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 11, 3)
	ctx := callerauthtest.Incoming(t, onlineMethod, in)
	for name, mutate := range map[string]func(*base.SessionDetails){
		"新修订":     func(d *base.SessionDetails) { d.OwnerRevision++ },
		"换角色":     func(d *base.SessionDetails) { d.PlayerId++ },
		"换账号":     func(d *base.SessionDetails) { d.AuthenticatedAccount = "别人" },
		"重启后的gate": func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" },
		"新连接代次":   func(d *base.SessionDetails) { d.SessionIncarnation++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.Clone(in).(*base.SessionDetails)
			mutate(changed)
			_, err := SessionAccount(ctx, changed)
			require.ErrorIs(t, err, ErrUnverifiedSession, "业务层不能把原身份改写成别的连接、账号或归属")
			_, err = CaptureSession(ctx, changed)
			require.ErrorIs(t, err, ErrUnverifiedSession)
		})
	}
}

func TestSessionAccountRejectsUnsignedOrIncompleteOnlineIdentity(t *testing.T) {
	complete := callerauthtest.OnlineSession("原账号", 11, 3)
	_, err := SessionAccount(callerauthtest.Unverified(t, complete), complete)
	require.ErrorIs(t, err, ErrUnverifiedSession, "没有验签原件的自报身份一律拒绝")
	_, err = SessionAccount(context.Background(), complete)
	require.ErrorIs(t, err, ErrUnverifiedSession)
	_, err = SessionAccount(callerauthtest.Incoming(t, onlineMethod, complete), nil)
	require.ErrorIs(t, err, ErrUnverifiedSession)

	for name, mutate := range map[string]func(*base.SessionDetails){
		"未选角":  func(d *base.SessionDetails) { d.PlayerId = 0 },
		"缺修订":  func(d *base.SessionDetails) { d.OwnerRevision = 0 },
		"未绑账号": func(d *base.SessionDetails) { d.AuthenticatedAccount = "" },
	} {
		t.Run(name, func(t *testing.T) {
			// gate 即使签了名也不能凭不完整的会话授予在线角色身份(例如离场后账号已清空)。
			in := proto.Clone(complete).(*base.SessionDetails)
			mutate(in)
			_, err := SessionAccount(callerauthtest.Incoming(t, onlineMethod, in), in)
			require.ErrorIs(t, err, ErrUnverifiedSession)
		})
	}
}
