package internalrpcauth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/metadata"

	"shared/clusterredis"
)

const (
	testSecret     = "internal-rpc-auth-test-secret-0123456789abcdef"
	teamTestSecret = "internal-rpc-auth-test-secret-team-9876543210fedcba"
	testMethod     = "/mmorpg.trade.v1.TradeService/ResolveInternalOrder"
	testAudience   = "trade:global"
)

type memoryReplayStore struct {
	seen map[string]struct{}
	err  error
}

func (s *memoryReplayStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if _, ok := s.seen[key]; ok {
		return false, nil
	}
	s.seen[key] = struct{}{}
	return true, nil
}

func signedIncoming(t *testing.T, signer *Signer, method string, subject uint64) context.Context {
	t.Helper()
	out, err := signer.SignContext(context.Background(), method, subject)
	if err != nil {
		t.Fatalf("SignContext: %v", err)
	}
	md, ok := metadata.FromOutgoingContext(out)
	if !ok {
		t.Fatal("signed context has no outgoing metadata")
	}
	return metadata.NewIncomingContext(context.Background(), md.Copy())
}

func signedIncomingWithPayload(t *testing.T, signer *Signer, method string, subject uint64, payload []byte) context.Context {
	t.Helper()
	out, err := signer.SignContextWithPayload(context.Background(), method, subject, payload)
	if err != nil {
		t.Fatalf("SignContextWithPayload: %v", err)
	}
	md, ok := metadata.FromOutgoingContext(out)
	if !ok {
		t.Fatal("payload-signed context has no outgoing metadata")
	}
	return metadata.NewIncomingContext(context.Background(), md.Copy())
}

func newAuthPair(t *testing.T) (*Signer, *Verifier, *memoryReplayStore) {
	t.Helper()
	store := &memoryReplayStore{seen: map[string]struct{}{}}
	signer, err := NewSigner(testSecret, "login", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(testSecret, "login", testAudience, 30*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_800_000_000, 123_000_000).UTC()
	signer.now = func() time.Time { return fixed }
	signer.random = bytes.NewReader(bytes.Repeat([]byte{0x5a}, nonceBytes))
	verifier.now = func() time.Time { return fixed }
	return signer, verifier, store
}

func TestVerifyConsumesNonceAndRejectsReplay(t *testing.T) {
	signer, verifier, _ := newAuthPair(t)
	ctx := signedIncoming(t, signer, testMethod, 42)
	if err := verifier.Verify(ctx, testMethod, 42); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if err := verifier.Verify(ctx, testMethod, 42); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay error=%v, want ErrReplay", err)
	}
}

func TestVerifyBindsMethodSubjectAudienceBeforeReplayWrite(t *testing.T) {
	signer, verifier, store := newAuthPair(t)
	ctx := signedIncoming(t, signer, testMethod, 42)
	if err := verifier.Verify(ctx, "/other.Service/Read", 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("different method err=%v, want ErrUnauthorized", err)
	}
	if err := verifier.Verify(ctx, testMethod, 43); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("different subject err=%v, want ErrUnauthorized", err)
	}
	otherAud, err := NewVerifier(testSecret, "login", "guild:global", 30*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherAud.Verify(ctx, testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("different audience err=%v, want ErrUnauthorized", err)
	}
	if len(store.seen) != 0 {
		t.Fatalf("unauthenticated requests consumed %d replay nonce(s)", len(store.seen))
	}
}

func TestSignerStripsPlayerAndGateCredentials(t *testing.T) {
	signer, _, _ := newAuthPair(t)
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer player-token",
		"x-session-detail-bin", "raw-session-bytes",
		"x-caller-id", "gate",
		"x-caller-sig", "deadbeef",
	))
	signed, err := signer.SignContext(base, testMethod, 42)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(signed)
	for _, stripped := range []string{"authorization", "x-session-detail-bin", "x-caller-id", "x-caller-sig"} {
		if len(md.Get(stripped)) != 0 {
			t.Fatalf("header %q leaked into internal RPC metadata", stripped)
		}
	}
}

func TestPayloadBindingPreventsTamperingDowngradeAndUpgrade(t *testing.T) {
	signer, verifier, store := newAuthPair(t)
	payload := []byte("canonical-op-payload")
	ctx := signedIncomingWithPayload(t, signer, testMethod, 42, payload)

	if err := verifier.Verify(ctx, testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("downgrade err=%v, want ErrUnauthorized", err)
	}
	if err := verifier.VerifyWithPayload(ctx, testMethod, 42, []byte("tampered")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered payload err=%v, want ErrUnauthorized", err)
	}
	if len(store.seen) != 0 {
		t.Fatal("failed payload checks must not consume nonce")
	}
	if err := verifier.VerifyWithPayload(ctx, testMethod, 42, payload); err != nil {
		t.Fatalf("valid payload VerifyWithPayload: %v", err)
	}
	if err := verifier.VerifyWithPayload(ctx, testMethod, 42, payload); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed payload err=%v, want ErrReplay", err)
	}

	plainSigner, plainVerifier, _ := newAuthPair(t)
	plainCtx := signedIncoming(t, plainSigner, testMethod, 42)
	if err := plainVerifier.VerifyWithPayload(plainCtx, testMethod, 42, payload); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("upgrade err=%v, want ErrUnauthorized", err)
	}
}

func TestMultiCallerVerifierIsolatesKeysAndNonces(t *testing.T) {
	store := &memoryReplayStore{seen: map[string]struct{}{}}
	fixed := time.Unix(1_800_000_000, 0).UTC()

	mkSigner := func(secret, caller string) *Signer {
		s, err := NewSigner(secret, caller, testAudience)
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return fixed }
		s.random = bytes.NewReader(bytes.Repeat([]byte{0x5a}, nonceBytes))
		return s
	}
	mkVerifier := func(secret, caller string) *Verifier {
		v, err := NewVerifier(secret, caller, testAudience, 30*time.Second, store)
		if err != nil {
			t.Fatal(err)
		}
		v.now = func() time.Time { return fixed }
		return v
	}

	loginSigner := mkSigner(testSecret, "login")
	teamSigner := mkSigner(teamTestSecret, "team")
	mv, err := NewMultiCallerVerifier(mkVerifier(testSecret, "login"), mkVerifier(teamTestSecret, "team"))
	if err != nil {
		t.Fatal(err)
	}

	loginCtx := signedIncoming(t, loginSigner, testMethod, 42)
	teamCtx := signedIncoming(t, teamSigner, testMethod, 42)
	if err := mv.Verify(loginCtx, testMethod, 42); err != nil {
		t.Fatalf("login Verify: %v", err)
	}
	// Same nonce bytes for another caller must not collide.
	if err := mv.Verify(teamCtx, testMethod, 42); err != nil {
		t.Fatalf("team Verify: %v", err)
	}
	if err := mv.Verify(teamCtx, testMethod, 42); !errors.Is(err, ErrReplay) {
		t.Fatalf("team replay err=%v, want ErrReplay", err)
	}
	// Cross-caller impersonation must fail closed without touching replay store.
	forged := mkSigner(testSecret, "team")
	if err := mv.Verify(signedIncoming(t, forged, testMethod, 42), testMethod, 42); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("impersonation err=%v, want ErrUnauthorized", err)
	}
}

func TestRedisReplayStore_ClusterDomainPoliciesAndCrossReplicaReplay(t *testing.T) {
	mr := miniredis.RunT(t)

	// 1. Player-domain cluster policy (tagged keys required).
	playerRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = playerRDB.Close() }()
	playerRDB.AddHook(clusterredis.GuardHook{Policy: clusterredis.PlayerDomainKeys})

	playerStore, err := NewRedisReplayStore(playerRDB, "")
	if err != nil {
		t.Fatal(err)
	}
	// Two independent verifiers (simulating two replicas behind a load balancer) sharing Redis.
	signer, err := NewSigner(testSecret, "login", testAudience)
	if err != nil {
		t.Fatal(err)
	}
	replicaA, err := NewVerifier(testSecret, "login", testAudience, 30*time.Second, playerStore)
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := NewVerifier(testSecret, "login", testAudience, 30*time.Second, playerStore)
	if err != nil {
		t.Fatal(err)
	}

	ctx := signedIncoming(t, signer, testMethod, 77)
	if err := replicaA.Verify(ctx, testMethod, 77); err != nil {
		t.Fatalf("replicaA Verify: %v", err)
	}
	if err := replicaB.Verify(ctx, testMethod, 77); !errors.Is(err, ErrReplay) {
		t.Fatalf("cross-replica replay err=%v, want ErrReplay", err)
	}

	// 2. Control-cluster policy (untagged single-key SETNX).
	ctrlRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = ctrlRDB.Close() }()
	ctrlRDB.AddHook(clusterredis.GuardHook{Policy: clusterredis.ControlClusterKeys})

	ctrlStore, err := NewControlClusterReplayStore(ctrlRDB, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := ctrlStore.Consume(context.Background(), "login:abc", time.Minute)
	if err != nil || !ok {
		t.Fatalf("control cluster first consume = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = ctrlStore.Consume(context.Background(), "login:abc", time.Minute)
	if err != nil || ok {
		t.Fatalf("control cluster replay consume = (%v, %v), want (false, nil)", ok, err)
	}
}
