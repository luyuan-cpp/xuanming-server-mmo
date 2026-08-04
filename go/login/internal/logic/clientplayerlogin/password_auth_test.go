package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"login/internal/logic/pkg/auth"
	login_proto "proto/login"
	"shared/generated/pb/table"
)

type passwordAuthenticatorFunc func(context.Context, string, string) (*auth.AuthResult, error)

func (f passwordAuthenticatorFunc) ValidatePassword(ctx context.Context, account, password string) (*auth.AuthResult, error) {
	return f(ctx, account, password)
}

func TestResolvePasswordAccountFailsClosedWithoutProvider(t *testing.T) {
	request := &login_proto.LoginRequest{Account: "victim", Password: "anything"}
	if _, err := resolvePasswordAccount(context.Background(), request, nil); !errors.Is(err, auth.ErrPasswordAuthDisabled) {
		t.Fatalf("error = %v, want ErrPasswordAuthDisabled", err)
	}
}

func TestResolvePasswordAccountUsesBothCredentials(t *testing.T) {
	request := &login_proto.LoginRequest{Account: "robot_7", Password: "secret"}
	provider := passwordAuthenticatorFunc(func(_ context.Context, account, password string) (*auth.AuthResult, error) {
		if account != "robot_7" || password != "secret" {
			t.Fatalf("credentials not forwarded: account=%q password=%q", account, password)
		}
		return &auth.AuthResult{Account: account}, nil
	})

	account, err := resolvePasswordAccount(context.Background(), request, provider)
	if err != nil {
		t.Fatalf("resolvePasswordAccount: %v", err)
	}
	if account != "robot_7" {
		t.Fatalf("account = %q, want robot_7", account)
	}
}

func TestResolvePasswordAccountRejectsProviderFailureOrEmptyIdentity(t *testing.T) {
	tests := []struct {
		name     string
		provider passwordAuthenticatorFunc
	}{
		{
			name: "口令错误",
			provider: func(context.Context, string, string) (*auth.AuthResult, error) {
				return nil, auth.ErrInvalidCredentials
			},
		},
		{
			name: "认证器返回空结果",
			provider: func(context.Context, string, string) (*auth.AuthResult, error) {
				return nil, nil
			},
		},
		{
			name: "认证器返回空账号",
			provider: func(context.Context, string, string) (*auth.AuthResult, error) {
				return &auth.AuthResult{}, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePasswordAccount(context.Background(), &login_proto.LoginRequest{
				Account: "robot_7", Password: "wrong",
			}, tc.provider)
			if !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestLoginPasswordPathsReturnAuthErrorBeforeSideEffects(t *testing.T) {
	// 故意不传 ServiceContext：若认证失败后仍触碰锁、账号初始化或发 token，
	// 测试会直接 panic，因此两个 password 拼法都必须在这些副作用之前返回。
	for _, authType := range []string{"", "password"} {
		t.Run("auth_type="+authType, func(t *testing.T) {
			logic := NewLoginLogic(context.Background(), nil)
			resp, err := logic.Login(&login_proto.LoginRequest{
				Account:  "victim",
				Password: "anything",
				AuthType: authType,
			})
			if err != nil {
				t.Fatalf("Login returned transport error: %v", err)
			}
			if resp.ErrorMessage == nil || resp.ErrorMessage.Id != uint32(table.LoginError_kLoginAccountNotFound) {
				t.Fatalf("error tip = %v, want kLoginAccountNotFound", resp.ErrorMessage)
			}
			if resp.AccessToken != "" || resp.RefreshToken != "" {
				t.Fatal("rejected password login must not issue tokens")
			}
		})
	}
}
