package loginpregatelogic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"login/internal/logic/pkg/loginqueue"
	"login/internal/logic/pkg/token"
)

// AssignGate 的鉴权(assigngate_auth.go):身份只从 access token 解出;没带令牌只在旧路径开着时匿名放行;
// 令牌无效回 UNAUTHENTICATED,令牌存储故障回 ERROR(让客户端重试,不让它重新登录)。

type fakeAccessValidator struct {
	account string
	err     error
	calls   int
}

func (f *fakeAccessValidator) ValidateAccess(_ context.Context, _ string) (*token.TokenData, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &token.TokenData{Account: f.account}, nil
}

const assignGateTestToken = "SYNTHETIC_ACCESS_TOKEN_VALUE"

func TestAssignGateAuthUsesAccountFromToken(t *testing.T) {
	validator := &fakeAccessValidator{account: "wx_union_alice"}
	for _, legacy := range []bool{true, false} {
		account, denied := authenticateAssignGate(context.Background(), validator, assignGateTestToken, legacy)
		if denied != nil {
			t.Fatalf("legacy=%v: 有效令牌不应被拒: %+v", legacy, denied)
		}
		if account != "wx_union_alice" {
			t.Fatalf("legacy=%v: account = %q, want 令牌解出的账号", legacy, account)
		}
	}
}

func TestAssignGateAuthAnonymousOnlyWhileLegacyGateLoginEnabled(t *testing.T) {
	validator := &fakeAccessValidator{account: "should-not-be-read"}

	account, denied := authenticateAssignGate(context.Background(), validator, "", true)
	if denied != nil || account != "" {
		t.Fatalf("旧路径开着时没带令牌应匿名放行: account=%q denied=%+v", account, denied)
	}

	_, denied = authenticateAssignGate(context.Background(), validator, "", false)
	if denied == nil || denied.Status != uint32(loginqueue.StatusUnauthenticated) {
		t.Fatalf("旧路径关掉后没带令牌应回 UNAUTHENTICATED,实际 %+v", denied)
	}
	if validator.calls != 0 {
		t.Fatalf("没带令牌时不应查令牌存储(调用 %d 次)", validator.calls)
	}
}

func TestAssignGateAuthRejectedTokenIsUnauthenticated(t *testing.T) {
	validator := &fakeAccessValidator{err: fmt.Errorf("%w: token not found or expired", token.ErrAccessRejected)}
	for _, legacy := range []bool{true, false} {
		account, denied := authenticateAssignGate(context.Background(), validator, assignGateTestToken, legacy)
		if denied == nil || denied.Status != uint32(loginqueue.StatusUnauthenticated) {
			t.Fatalf("legacy=%v: 无效令牌应回 UNAUTHENTICATED(不降级成匿名),实际 account=%q denied=%+v", legacy, account, denied)
		}
		if strings.Contains(denied.Error, assignGateTestToken) {
			t.Fatal("拒绝回包不得回显令牌")
		}
	}
}

func TestAssignGateAuthStoreFailureIsRetryableError(t *testing.T) {
	validator := &fakeAccessValidator{err: errors.New("Redis lookup: connection refused")}
	_, denied := authenticateAssignGate(context.Background(), validator, assignGateTestToken, true)
	if denied == nil || denied.Status != uint32(loginqueue.StatusError) {
		t.Fatalf("令牌存储故障应回 ERROR(客户端稍后重试),实际 %+v", denied)
	}
	if strings.Contains(denied.Error, assignGateTestToken) {
		t.Fatal("故障回包不得回显令牌")
	}
}
