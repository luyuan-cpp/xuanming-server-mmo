package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"shared/accountdirectory"

	"github.com/stretchr/testify/require"
)

func recoveryFixture(mode string) recoveryOptions {
	return recoveryOptions{mode: mode, playerID: 11, instance: "11111111-2222-3333-4444-555555555555", confirmedDead: true}
}

func TestRecoveryRejectsBadArgumentsBeforeTouchingTheStore(t *testing.T) {
	cases := map[string]func(*recoveryOptions){
		"未知模式":        func(o *recoveryOptions) { o.mode = "force" },
		"缺玩家":         func(o *recoveryOptions) { o.playerID = 0 },
		"缺实例":         func(o *recoveryOptions) { o.instance = "" },
		"apply 未确认已死": func(o *recoveryOptions) { o.confirmedDead = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o := recoveryFixture("apply")
			mutate(&o)
			var out, errOut bytes.Buffer
			code := runRecovery(context.Background(), o, &out, &errOut, func(context.Context, uint64, string, bool) (uint64, error) {
				t.Fatal("参数错误不能碰库")
				return 0, nil
			})
			require.Equal(t, 2, code)
			require.NotEmpty(t, errOut.String())
		})
	}
}

func TestRecoveryPlanOnlyChecksAndApplyWrites(t *testing.T) {
	for mode, wantApply := range map[string]bool{"plan": false, "apply": true} {
		var out, errOut bytes.Buffer
		code := runRecovery(context.Background(), recoveryFixture(mode), &out, &errOut, func(_ context.Context, player uint64, instance string, apply bool) (uint64, error) {
			require.Equal(t, uint64(11), player)
			require.Equal(t, wantApply, apply)
			return 7, nil
		})
		require.Equal(t, 0, code, errOut.String())
		require.Contains(t, out.String(), "owner_epoch_before=7")
	}
}

// 错误只输出固定文案:底层错误可能带连接串。
func TestRecoveryErrorsDoNotLeakDetails(t *testing.T) {
	for _, cause := range []error{accountdirectory.ErrConflict, accountdirectory.ErrMissing, errors.New("private-password-must-not-leak")} {
		var out, errOut bytes.Buffer
		code := runRecovery(context.Background(), recoveryFixture("apply"), &out, &errOut, func(context.Context, uint64, string, bool) (uint64, error) {
			return 0, cause
		})
		require.Equal(t, 1, code)
		require.NotContains(t, errOut.String(), "private-password")
		require.Empty(t, out.String())
	}
}
