package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/dbplacement"

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

// 闸门冻结:这个工具不过闸门,store 同事务查到冻结就拒绝,这里要报清楚,不说成结局未知。
func TestRecoveryRefusesFrozenGate(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runRecovery(context.Background(), recoveryFixture("apply"), &out, &errOut, func(context.Context, uint64, string, bool) (uint64, error) {
		return 0, fmt.Errorf("release holder: %w", authoritysql.ErrWriterFrozen)
	})
	require.Equal(t, 1, code)
	require.Contains(t, errOut.String(), "frozen")
	require.Empty(t, out.String())
}

// 没有映射就不知道库在哪:拒绝,不连连接串里的地址。
func TestRecoveryRequiresDbPlacement(t *testing.T) {
	t.Setenv(authoritysql.BackendEnv, "mysql")
	t.Setenv("MMORPG_SQL_OPERATOR_PRINCIPAL", "maintenance@%")
	t.Setenv("ADMISSION_RECOVER_TEST_DSN", "u:p@tcp(127.0.0.1:1)/mmorpg_global")
	t.Setenv(dbplacement.EnvConfig, "")
	o := recoveryFixture("plan")
	o.dsnEnv, o.schema, o.stores = "ADMISSION_RECOVER_TEST_DSN", "mmorpg_global", "1"
	var out, errOut bytes.Buffer
	code := runRecovery(context.Background(), o, &out, &errOut, nil)
	require.Equal(t, 2, code)
	require.Contains(t, errOut.String(), dbplacement.EnvConfig)
	require.Empty(t, out.String())
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
