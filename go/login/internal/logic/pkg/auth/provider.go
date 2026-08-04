package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// AuthResult holds the resolved account from authentication.
type AuthResult struct {
	Account string // Resolved account identifier
}

// Provider validates an auth token and returns the resolved account.
type Provider interface {
	Validate(ctx context.Context, token string) (*AuthResult, error)
}

// PasswordAuthenticator 与第三方 token Provider 分开：口令认证同时需要
// account + password，不能再把 account 冒充成 token 塞进通用接口。
type PasswordAuthenticator interface {
	ValidatePassword(ctx context.Context, account, password string) (*AuthResult, error)
}

var (
	ErrPasswordAuthDisabled = errors.New("password authentication is disabled")
	ErrInvalidCredentials   = errors.New("invalid credentials")
)

var (
	mu        sync.RWMutex
	providers = map[string]Provider{}
	password  PasswordAuthenticator
)

// Register adds a named auth provider. Panics on duplicate names.
func Register(name string, p Provider) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := providers[name]; exists {
		panic(fmt.Sprintf("auth: provider %q already registered", name))
	}
	providers[name] = p
}

// RegisterPassword 注册显式配置的口令认证器。生产默认不注册，因此
// auth_type="password" 与空 auth_type 都 fail-closed。
func RegisterPassword(p PasswordAuthenticator) {
	if p == nil {
		panic("auth: nil password authenticator")
	}
	mu.Lock()
	defer mu.Unlock()
	if password != nil {
		panic("auth: password authenticator already registered")
	}
	password = p
}

// GetPassword 返回当前口令认证器；nil 表示口令路径被禁用。
func GetPassword() PasswordAuthenticator {
	mu.RLock()
	defer mu.RUnlock()
	return password
}

// Get returns the provider for the given auth type, or nil if not registered.
func Get(name string) Provider {
	mu.RLock()
	defer mu.RUnlock()
	return providers[name]
}
