package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePasswordHashStore struct {
	canonical string
	hash      string
	found     bool
	err       error
	lookups   atomic.Int32
}

func (s *fakePasswordHashStore) LookupPasswordHash(_ context.Context, _ string) (string, string, bool, error) {
	s.lookups.Add(1)
	return s.canonical, s.hash, s.found, s.err
}

func TestProductionPasswordProviderAcceptsOnlyMatchingArgon2idHash(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	store := &fakePasswordHashStore{canonical: "Alice", hash: encoded, found: true}
	provider, err := NewProductionPasswordProvider(store)
	if err != nil {
		t.Fatalf("NewProductionPasswordProvider: %v", err)
	}

	result, err := provider.ValidatePassword(context.Background(), "alice", "correct horse battery staple")
	if err != nil {
		t.Fatalf("ValidatePassword: %v", err)
	}
	if result.Account != "Alice" {
		t.Fatalf("account = %q, want canonical MySQL account Alice", result.Account)
	}

	if _, err := provider.ValidatePassword(context.Background(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", err)
	}
}

func TestProductionPasswordProviderFailsClosedForUnknownEmptyAndLegacyPlaintext(t *testing.T) {
	providerWith := func(store *fakePasswordHashStore) *ProductionPasswordProvider {
		provider, err := NewProductionPasswordProvider(store)
		if err != nil {
			t.Fatalf("NewProductionPasswordProvider: %v", err)
		}
		return provider
	}

	unknown := &fakePasswordHashStore{}
	if _, err := providerWith(unknown).ValidatePassword(context.Background(), "unknown", "password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown account error = %v, want ErrInvalidCredentials", err)
	}
	if unknown.lookups.Load() != 1 {
		t.Fatalf("unknown account lookups = %d, want 1", unknown.lookups.Load())
	}

	empty := &fakePasswordHashStore{canonical: "alice", found: true}
	if _, err := providerWith(empty).ValidatePassword(context.Background(), "alice", "password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("empty hash error = %v, want ErrInvalidCredentials", err)
	}

	legacy := &fakePasswordHashStore{canonical: "alice", hash: "old-plaintext-password", found: true}
	if _, err := providerWith(legacy).ValidatePassword(context.Background(), "alice", "old-plaintext-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("legacy plaintext error = %v, want ErrInvalidCredentials", err)
	}

	invalidInput := &fakePasswordHashStore{canonical: "alice", found: true}
	if _, err := providerWith(invalidInput).ValidatePassword(context.Background(), " alice", "password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("invalid account shape error = %v, want ErrInvalidCredentials", err)
	}
	if invalidInput.lookups.Load() != 0 {
		t.Fatalf("invalid account shape must not query MySQL, lookups=%d", invalidInput.lookups.Load())
	}
}

func TestProductionPasswordProviderPropagatesStoreOutageWithoutAuthenticating(t *testing.T) {
	store := &fakePasswordHashStore{err: errors.New("db unavailable")}
	provider, err := NewProductionPasswordProvider(store)
	if err != nil {
		t.Fatalf("NewProductionPasswordProvider: %v", err)
	}
	if _, err := provider.ValidatePassword(context.Background(), "alice", "password"); err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("store outage error = %v, want fail-closed backend error", err)
	}
}

func TestParseArgon2idRejectsWeakOrOversizedParameters(t *testing.T) {
	cases := []string{
		"$argon2id$v=19$m=1024,t=1,p=1$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZg",
		"$argon2id$v=19$m=999999,t=3,p=2$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZg",
		"plaintext",
	}
	for _, encoded := range cases {
		if _, _, _, err := parseArgon2id(encoded); err == nil {
			t.Fatalf("parseArgon2id(%q) unexpectedly succeeded", encoded)
		}
	}
}

func TestProductionPasswordProviderBoundsConcurrentKDFWork(t *testing.T) {
	store := &fakePasswordHashStore{canonical: "alice", hash: "test-hash", found: true}
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var active atomic.Int32
	var maximum atomic.Int32
	provider := &ProductionPasswordProvider{
		store:          store,
		dummyHash:      "dummy",
		kdfSlots:       make(chan struct{}, 2),
		kdfWaitTimeout: 2 * time.Second,
		verifyKDF: func(_, _ string) (bool, error) {
			now := active.Add(1)
			for {
				old := maximum.Load()
				if now <= old || maximum.CompareAndSwap(old, now) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return true, nil
		},
	}
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := provider.ValidatePassword(context.Background(), "alice", "password")
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			closeRelease()
			wg.Wait()
			t.Fatal("timed out waiting for bounded KDF workers")
		}
	}
	if got := active.Load(); got != 2 {
		closeRelease()
		wg.Wait()
		t.Fatalf("active KDF calls = %d, want 2", got)
	}
	closeRelease()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ValidatePassword: %v", err)
		}
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent KDF calls = %d, want 2", got)
	}
}

func TestProductionPasswordProviderKDFWaitIsContextBoundAndFailsClosed(t *testing.T) {
	provider := &ProductionPasswordProvider{
		store:          &fakePasswordHashStore{},
		dummyHash:      "dummy",
		kdfSlots:       make(chan struct{}, 1),
		kdfWaitTimeout: 20 * time.Millisecond,
		verifyKDF: func(_, _ string) (bool, error) {
			t.Fatal("KDF must not start while its only slot is occupied")
			return false, nil
		},
	}
	provider.kdfSlots <- struct{}{}
	defer func() { <-provider.kdfSlots }()

	started := time.Now()
	if _, err := provider.ValidatePassword(context.Background(), "unknown", "password"); !errors.Is(err, ErrPasswordKDFCapacity) {
		t.Fatalf("saturated KDF error = %v, want ErrPasswordKDFCapacity", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("KDF saturation waited too long: %s", elapsed)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	started = time.Now()
	if _, err := provider.ValidatePassword(cancelled, "unknown", "password"); !errors.Is(err, ErrPasswordKDFCapacity) {
		t.Fatalf("cancelled KDF wait error = %v, want ErrPasswordKDFCapacity", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled KDF wait was not prompt: %s", elapsed)
	}
}

func TestProductionPasswordProviderRejectsUnsafeKDFLimits(t *testing.T) {
	store := &fakePasswordHashStore{}
	if _, err := NewProductionPasswordProviderWithLimits(store, MaxPasswordKDFConcurrency+1, time.Second); err == nil {
		t.Fatal("KDF concurrency above hard limit was accepted")
	}
	if _, err := NewProductionPasswordProviderWithLimits(store, 1, MaxPasswordKDFWaitTimeout+time.Nanosecond); err == nil {
		t.Fatal("KDF wait timeout above hard limit was accepted")
	}
}

func TestValidatePasswordAccountMatchesStorageBoundary(t *testing.T) {
	valid := []string{"alice", "玩家一号", strings.Repeat("界", 191)}
	for _, account := range valid {
		if err := ValidatePasswordAccount(account); err != nil {
			t.Fatalf("ValidatePasswordAccount(%q): %v", account, err)
		}
	}
	invalidUTF8 := string([]byte{0xff})
	invalid := []string{"", " alice", "alice ", strings.Repeat("a", 192), invalidUTF8}
	for _, account := range invalid {
		if err := ValidatePasswordAccount(account); err == nil {
			t.Fatalf("ValidatePasswordAccount(%q) unexpectedly succeeded", account)
		}
	}
}

// 真实 MySQL 只读烟测：CI/运维显式提供三个环境变量时才运行，不创建、
// 更新或删除任何数据。账号应已由 password_admin 写入 Argon2id PHC。
func TestSQLPasswordHashStoreIntegrationReadOnly(t *testing.T) {
	dsn := os.Getenv("LOGIN_TEST_PASSWORD_DSN")
	account := os.Getenv("LOGIN_TEST_PASSWORD_ACCOUNT")
	if dsn == "" || account == "" {
		t.Skip("set LOGIN_TEST_PASSWORD_DSN and LOGIN_TEST_PASSWORD_ACCOUNT for the read-only MySQL integration test")
	}
	store, err := NewSQLPasswordHashStore(dsn, 1, 0)
	if err != nil {
		t.Fatalf("NewSQLPasswordHashStore: %v", err)
	}
	defer store.Close()
	canonical, encoded, found, err := store.LookupPasswordHash(context.Background(), account)
	if err != nil {
		t.Fatalf("LookupPasswordHash: %v", err)
	}
	if !found || canonical == "" || !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("authoritative row is not migrated: found=%v account=%q hash_prefix=%q",
			found, canonical, fmt.Sprintf("%.10s", encoded))
	}
}
