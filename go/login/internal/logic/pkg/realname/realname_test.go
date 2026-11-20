package realname

import (
	"context"
	"errors"
	"testing"
	"time"

	"login/internal/logic/pkg/nppa"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const (
	testAccount = "wx_union"
	testPI      = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"
	otherPI     = "1hpfml00000000000000000000000000000000"
	validID     = "11010519491231002X"
)

type fakeVerifier struct {
	checks    int
	lastAI    string
	check     func() (nppa.AuthResult, error)
	query     func(ai string) (nppa.AuthResult, error)
	queriedAI string
}

func (f *fakeVerifier) Check(_ context.Context, ai, _, _ string) (nppa.AuthResult, error) {
	f.checks++
	f.lastAI = ai
	return f.check()
}

func (f *fakeVerifier) Query(_ context.Context, ai string) (nppa.AuthResult, error) {
	f.queriedAI = ai
	return f.query(ai)
}

type fakeDirectory struct {
	stored string
	err    error
}

func (d *fakeDirectory) SetRealName(_ context.Context, _ string, pi string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	if d.stored == "" {
		d.stored = pi
	}
	return d.stored, nil
}

func newService(t *testing.T, v *fakeVerifier, d *fakeDirectory, limit int) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	return &Service{Redis: rdb, Verifier: v, Directory: d, DailyLimit: limit, Now: func() time.Time { return now }}, mr
}

func ok(pi string) func() (nppa.AuthResult, error) {
	return func() (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthOK, PI: pi}, nil }
}

func TestSubmitRejectsMalformedInputWithoutCallingTheSystem(t *testing.T) {
	v := &fakeVerifier{check: ok(testPI)}
	s, _ := newService(t, v, &fakeDirectory{}, 5)
	for _, in := range [][2]string{{"", validID}, {"张三", "110105194912310021"}} {
		if _, tip := s.Submit(context.Background(), testAccount, in[0], in[1]); tip != uint32(table.LoginError_kLoginRealNameInvalid) {
			t.Fatalf("Submit(%q, %q) tip = %d, want invalid", in[0], in[1], tip)
		}
	}
	if v.checks != 0 {
		t.Fatalf("malformed input must not reach the NPPA system, checks = %d", v.checks)
	}
}

func TestSubmitBindsTheVerifiedPI(t *testing.T) {
	d := &fakeDirectory{}
	s, _ := newService(t, &fakeVerifier{check: ok(testPI)}, d, 5)
	pi, tip := s.Submit(context.Background(), testAccount, " 张三 ", "11010519491231002x")
	if tip != 0 || pi != testPI || d.stored != testPI {
		t.Fatalf("Submit = %q, %d; stored %q", pi, tip, d.stored)
	}
}

func TestSubmitReportsAnAccountAlreadyBoundToAnotherIdentity(t *testing.T) {
	s, _ := newService(t, &fakeVerifier{check: ok(testPI)}, &fakeDirectory{stored: otherPI}, 5)
	pi, tip := s.Submit(context.Background(), testAccount, "张三", validID)
	if tip != uint32(table.LoginError_kLoginRealNameAlreadyBound) || pi != otherPI {
		t.Fatalf("Submit = %q, %d; want the stored pi and already-bound", pi, tip)
	}
}

func outcome(r nppa.AuthResult, err error) func() (nppa.AuthResult, error) {
	return func() (nppa.AuthResult, error) { return r, err }
}

func TestSubmitClassifiesSystemOutcomes(t *testing.T) {
	invalid := uint32(table.LoginError_kLoginRealNameInvalid)
	unavailable := uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	cases := []struct {
		name  string
		check func() (nppa.AuthResult, error)
		want  uint32
	}{
		{"identity mismatch", outcome(nppa.AuthResult{Status: nppa.AuthFailed}, nil), invalid},
		{"format rejected", outcome(nppa.AuthResult{}, nppa.ErrInvalidIdentity), invalid},
		{"system down", outcome(nppa.AuthResult{}, nppa.ErrUnavailable), unavailable},
	}
	for _, c := range cases {
		s, _ := newService(t, &fakeVerifier{check: c.check}, &fakeDirectory{}, 5)
		if _, tip := s.Submit(context.Background(), testAccount, "张三", validID); tip != c.want {
			t.Errorf("%s: tip = %d, want %d", c.name, tip, c.want)
		}
	}
}

func TestSubmitEnforcesTheDailyLimit(t *testing.T) {
	v := &fakeVerifier{check: func() (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthFailed}, nil }}
	s, mr := newService(t, v, &fakeDirectory{}, 2)
	for i := 0; i < 2; i++ {
		s.Submit(context.Background(), testAccount, "张三", validID)
	}
	if _, tip := s.Submit(context.Background(), testAccount, "张三", validID); tip != uint32(table.LoginError_kLoginRealNameTooManyAttempts) {
		t.Fatalf("third submit tip = %d, want too many attempts", tip)
	}
	if v.checks != 2 {
		t.Fatalf("the over-limit submit must not reach the NPPA system, checks = %d", v.checks)
	}
	if ttl := mr.TTL(redisslot.RealNameAttemptsKey(testAccount, "2026-11-19")); ttl != attemptsTTL {
		t.Fatalf("attempt counter TTL = %v, want %v (keyed by the Beijing date)", ttl, attemptsTTL)
	}
}

func TestPendingSubmitIsResolvedOnALaterLogin(t *testing.T) {
	v := &fakeVerifier{check: func() (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthPending}, nil }}
	d := &fakeDirectory{}
	s, mr := newService(t, v, d, 5)
	if _, tip := s.Submit(context.Background(), testAccount, "张三", validID); tip != uint32(table.LoginError_kLoginRealNamePending) {
		t.Fatalf("pending submit tip = %d", tip)
	}
	pendingKey := redisslot.RealNamePendingKey(testAccount)
	if got, _ := mr.Get(pendingKey); got != v.lastAI || mr.TTL(pendingKey) != pendingTTL {
		t.Fatalf("pending ai = %q (ttl %v), want the submitted ai kept 48h", got, mr.TTL(pendingKey))
	}

	v.query = func(string) (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthPending}, nil }
	if pi := s.ResolvePending(context.Background(), testAccount); pi != "" || !mr.Exists(pendingKey) {
		t.Fatal("still pending: nothing bound, ai kept")
	}
	v.query = func(string) (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthOK, PI: testPI}, nil }
	if pi := s.ResolvePending(context.Background(), testAccount); pi != testPI || d.stored != testPI || mr.Exists(pendingKey) {
		t.Fatalf("resolved = %q, stored %q, pending kept %v", pi, d.stored, mr.Exists(pendingKey))
	}
	if v.queriedAI != v.lastAI {
		t.Fatalf("queried ai %q, want the submitted %q", v.queriedAI, v.lastAI)
	}
}

func TestResolvePendingKeepsTheAiWhenBindingFails(t *testing.T) {
	v := &fakeVerifier{query: func(string) (nppa.AuthResult, error) { return nppa.AuthResult{Status: nppa.AuthOK, PI: testPI}, nil }}
	s, mr := newService(t, v, &fakeDirectory{err: errors.New("data_service down")}, 5)
	pendingKey := redisslot.RealNamePendingKey(testAccount)
	_ = mr.Set(pendingKey, "ai-1")
	if pi := s.ResolvePending(context.Background(), testAccount); pi != "" || !mr.Exists(pendingKey) {
		t.Fatal("a failed bind must keep the ai for the next login")
	}
	v.query = func(string) (nppa.AuthResult, error) { return nppa.AuthResult{}, nppa.ErrNoRecord }
	if pi := s.ResolvePending(context.Background(), testAccount); pi != "" || mr.Exists(pendingKey) {
		t.Fatal("no record left at the system: the ai is forgotten and the player resubmits")
	}
}

func TestResolvePendingWithoutPendingDoesNotCallTheSystem(t *testing.T) {
	v := &fakeVerifier{}
	s, _ := newService(t, v, &fakeDirectory{}, 5)
	if pi := s.ResolvePending(context.Background(), testAccount); pi != "" || v.queriedAI != "" {
		t.Fatal("no pending ai: no query")
	}
}
