package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	base "proto/common/base"
	dbpb "proto/common/database"
)

func recoveryCLIFixture(t *testing.T) (recoveryOptions, *dbpb.AccountRoleAdmissionRecovery_Request) {
	t.Helper()
	id := bytes.Repeat([]byte{'a'}, 32)
	in := &dbpb.AccountRoleAdmissionRecovery_Request{RequestId: id, SellerAccount: "owned-seller", Seal: &base.RoleTradeSeal{PlayerId: 7, ListingId: 9, OwnerRevision: 1, SceneInstanceId: "11111111-2222-3333-4444-555555555555", ThroughGeneration: 2, SceneNodeId: "3", ZoneId: 1}, MaintenanceReceiptId: bytes.Repeat([]byte{'b'}, 32), MaintenanceRequestDigest: bytes.Repeat([]byte{2}, 32)}
	body, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(in)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(path, body, 0600))
	sum := sha256.Sum256(body)
	return recoveryOptions{requestFile: path, requestSHA: hex.EncodeToString(sum[:]), dsnEnv: "OWNED_RECOVERY_DSN", schema: "mmorpg_global", stores: "1", maintenance: true, mode: "plan", planFile: filepath.Join(t.TempDir(), "plan.json"), localTarget: strings.Repeat("a", 32), sqlTarget: "tidb.maintenance.example:4000"}, in
}

func TestAdmissionRecoveryCLIPinnedRequestAndPermanentReceipt(t *testing.T) {
	o, in := recoveryCLIFixture(t)
	var stdout, stderr bytes.Buffer
	calls := 0
	code := runRecovery(context.Background(), o, &stdout, &stderr, func(_ context.Context, actual *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
		calls++
		require.Equal(t, in.RequestId, actual.RequestId)
		return &dbpb.AccountRoleAdmissionRecovery{RequestId: actual.RequestId, RequiredOwnerEpoch: 21, RequiredSaveSeq: 1, SqlProofDigest: bytes.Repeat([]byte{3}, 32)}, nil
	})
	require.Equal(t, 0, code)
	require.Equal(t, 1, calls)
	require.Empty(t, stderr.String())
	require.Contains(t, stdout.String(), "完整SQL校验已只读回滚")
	body, err := os.ReadFile(o.planFile)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	o.mode = "apply"
	o.planSHA = hex.EncodeToString(sum[:])
	stdout.Reset()
	code = runRecovery(context.Background(), o, &stdout, &stderr, func(_ context.Context, actual *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
		return &dbpb.AccountRoleAdmissionRecovery{RequestId: actual.RequestId, RequiredOwnerEpoch: 21, RequiredSaveSeq: 1, SqlProofDigest: bytes.Repeat([]byte{3}, 32)}, nil
	})
	require.Equal(t, 0, code)
	require.Contains(t, stdout.String(), "required_owner_epoch")
}

func TestAdmissionRecoveryCLIRejectsBadPlanBeforeStore(t *testing.T) {
	for _, kind := range []string{"sha", "no-maintenance", "unknown-field", "duplicate-storage", "bad-schema", "missing-target-address", "credential-in-address", "wrong-nonce"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := recoveryCLIFixture(t)
			switch kind {
			case "sha":
				o.requestSHA = strings.Repeat("0", 64)
			case "no-maintenance":
				o.maintenance = false
			case "unknown-field":
				body, err := os.ReadFile(o.requestFile)
				require.NoError(t, err)
				body = append([]byte(`{"unreviewed":true,`), body[1:]...)
				require.NoError(t, os.WriteFile(o.requestFile, body, 0600))
				sum := sha256.Sum256(body)
				o.requestSHA = hex.EncodeToString(sum[:])
			case "duplicate-storage":
				o.stores = "1,1"
			case "bad-schema":
				o.schema = "global;bad"
			case "missing-target-address":
				o.sqlTarget = ""
			case "credential-in-address":
				o.sqlTarget = "root:password@tidb.maintenance.example:4000"
			case "wrong-nonce":
				o.localTarget = strings.Repeat("0", 32)
			}
			var out, errOut bytes.Buffer
			called := false
			code := runRecovery(context.Background(), o, &out, &errOut, func(context.Context, *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
				called = true
				return nil, nil
			})
			require.Equal(t, 2, code)
			require.False(t, called)
			require.Empty(t, out.String())
		})
	}
}

func TestAdmissionRecoveryCLICommitUnknownNeverReportsSuccessOrSecret(t *testing.T) {
	o, _ := recoveryCLIFixture(t)
	var out, errOut bytes.Buffer
	code := runRecovery(context.Background(), o, &out, &errOut, func(context.Context, *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
		return nil, errors.New("private-password-must-not-leak")
	})
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), "提交未知")
	require.NotContains(t, errOut.String(), "private-password")
}

func TestAdmissionRecoveryCLIRejectsChangedSQLInstanceBeforeApply(t *testing.T) {
	o, _ := recoveryCLIFixture(t)
	var out, errOut bytes.Buffer
	code := runRecovery(context.Background(), o, &out, &errOut, func(_ context.Context, in *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
		return &dbpb.AccountRoleAdmissionRecovery{RequestId: in.RequestId, RequiredOwnerEpoch: 21, RequiredSaveSeq: 1, SqlProofDigest: bytes.Repeat([]byte{3}, 32)}, nil
	})
	require.Equal(t, 0, code)
	body, err := os.ReadFile(o.planFile)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	o.mode = "apply"
	o.planSHA = hex.EncodeToString(sum[:])
	o.localTarget = strings.Repeat("b", 32)
	called := false
	out.Reset()
	errOut.Reset()
	code = runRecovery(context.Background(), o, &out, &errOut, func(context.Context, *dbpb.AccountRoleAdmissionRecovery_Request) (*dbpb.AccountRoleAdmissionRecovery, error) {
		called = true
		return nil, nil
	})
	require.Equal(t, 2, code)
	require.False(t, called)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), "SQL实例不符")
}
