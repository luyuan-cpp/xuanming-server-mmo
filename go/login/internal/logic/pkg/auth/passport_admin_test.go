package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 本文件钉住通行证用户删除(passport_admin.go)对 Casdoor 管理接口的用法:
//   - 按 sub 现查用户名,带 Basic 凭据;查到才删,删除请求只带 owner / name;删完再查一次,查不到才算删掉;
//   - 用户已经不在按删完(可以重做);status=error、删除没影响到行、删完还查得到、应答里的 id 对不上都回错误,不当成删完;
//   - 失败都标 ErrProviderUnavailable。

const (
	testPassportSub          = "550e8400-e29b-41d4-a716-446655440000"
	testPassportAdminID      = "xuanming-game-server"
	testPassportAdminSecret  = "5d1c0e7a9b3f4a628c7d2e1f0a9b8c7d"
	testPassportOrganization = "xuanming"
)

// casdoorCalls 是 casdoorStub 收到的请求的记录。
type casdoorCalls struct {
	gets      int
	deletes   int
	lastQuery string
	lastBody  map[string]string
	badAuth   bool
}

// casdoorStub 模拟 Casdoor 的 get-user / delete-user。getData / deleteData 是两个接口应答里 data 的原文;
// delete-user 回 "Affected" 之后 get-user 改回 getDataAfterDelete(空 = null,即真删掉了)。
type casdoorStub struct {
	status             string // 应答的 status,空 = "ok"
	httpStatus         int    // 非 0 时直接回这个状态码
	getData            string
	deleteData         string
	getDataAfterDelete string

	mu    sync.Mutex
	calls casdoorCalls
}

func (s *casdoorStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, secret, ok := r.BasicAuth(); !ok || id != testPassportAdminID || secret != testPassportAdminSecret {
		s.calls.badAuth = true
	}
	if s.httpStatus != 0 {
		w.WriteHeader(s.httpStatus)
		return
	}
	status := s.status
	if status == "" {
		status = "ok"
	}
	var data string
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/get-user":
		s.calls.gets++
		s.calls.lastQuery = r.URL.RawQuery
		data = s.getData
	case r.Method == http.MethodPost && r.URL.Path == "/api/delete-user":
		s.calls.deletes++
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.calls.lastBody)
		data = s.deleteData
		if data == `"Affected"` {
			s.getData = s.getDataAfterDelete
			if s.getData == "" {
				s.getData = "null"
			}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write([]byte(`{"status":"` + status + `","msg":"Unauthorized operation","data":` + data + `}`))
}

// seen 回目前为止收到的请求记录。
func (s *casdoorStub) seen() casdoorCalls {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newPassportDeleter(t *testing.T, stub *casdoorStub) *PassportUserDeleter {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	d, err := NewPassportUserDeleter(testPassportIssuer, srv.URL, testPassportAdminID, testPassportAdminSecret, srv.Client())
	if err != nil {
		t.Fatalf("NewPassportUserDeleter: %v", err)
	}
	return d
}

func foundUser(id string) string {
	return `{"owner":"` + testPassportOrganization + `","name":"alice","id":"` + id + `","email":"alice@example.com"}`
}

func TestPassportUserDeleterLooksUpBySubThenDeletesByName(t *testing.T) {
	stub := &casdoorStub{getData: foundUser(testPassportSub), deleteData: `"Affected"`}
	if err := newPassportDeleter(t, stub).DeleteUser(context.Background(), testPassportSub); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	calls := stub.seen()
	if calls.badAuth {
		t.Fatal("every call must carry the admin client credentials as HTTP Basic")
	}
	if calls.gets != 2 || calls.lastQuery != "userId="+testPassportSub {
		t.Fatalf("get-user should be called by userId before and after the delete: gets=%d query=%q", calls.gets, calls.lastQuery)
	}
	if calls.deletes != 1 || len(calls.lastBody) != 2 || calls.lastBody["owner"] != testPassportOrganization || calls.lastBody["name"] != "alice" {
		t.Fatalf("delete-user should carry only owner and name: deletes=%d body=%v", calls.deletes, calls.lastBody)
	}
}

func TestPassportUserDeleterTreatsMissingUserAsDone(t *testing.T) {
	stub := &casdoorStub{getData: "null"}
	if err := newPassportDeleter(t, stub).DeleteUser(context.Background(), testPassportSub); err != nil {
		t.Fatalf("a user that is already gone must count as deleted: %v", err)
	}
	if stub.seen().deletes != 0 {
		t.Fatal("nothing to delete when the user is gone")
	}
}

func TestPassportUserDeleterNeverReportsSuccessWithoutProof(t *testing.T) {
	found := foundUser(testPassportSub)
	for name, stub := range map[string]*casdoorStub{
		"get-user refused (wrong credentials)": {status: "error", getData: "null"},
		"delete-user removed nothing":          {getData: found, deleteData: `"Unaffected"`},
		"get-user returned another user":       {getData: foundUser("another-id"), deleteData: `"Affected"`},
		"get-user returned a nameless user":    {getData: `{"owner":"xuanming","name":"","id":"` + testPassportSub + `"}`, deleteData: `"Affected"`},
		"passport is down":                     {httpStatus: http.StatusBadGateway},
		// 组织开了软删除:delete-user 回 Affected 但用户还查得到,不能当成删完。
		"user still present after delete": {getData: found, deleteData: `"Affected"`, getDataAfterDelete: found},
	} {
		err := newPassportDeleter(t, stub).DeleteUser(context.Background(), testPassportSub)
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Errorf("%s: want ErrProviderUnavailable, got %v", name, err)
		}
	}

	// 查到的不是这名用户时,连删除请求都不能发。
	other := &casdoorStub{getData: foundUser("another-id"), deleteData: `"Affected"`}
	_ = newPassportDeleter(t, other).DeleteUser(context.Background(), testPassportSub)
	if other.seen().deletes != 0 {
		t.Fatal("must not send delete-user for a user whose id does not match the subject")
	}
}

func TestNewPassportUserDeleterRequiresAllSettings(t *testing.T) {
	for name, args := range map[string][3]string{
		"issuer":        {"", testPassportAdminID, testPassportAdminSecret},
		"client id":     {testPassportIssuer, "", testPassportAdminSecret},
		"client secret": {testPassportIssuer, testPassportAdminID, ""},
	} {
		if _, err := NewPassportUserDeleter(args[0], "", args[1], args[2], nil); err == nil {
			t.Errorf("missing %s should be rejected", name)
		}
	}
}

func TestPassportSubject(t *testing.T) {
	if sub, ok := PassportSubject("passport_" + testPassportSub); !ok || sub != testPassportSub {
		t.Fatalf("PassportSubject = %q, %v", sub, ok)
	}
	for _, account := range []string{"apple_001234.abcdef", "passport_", "robot_1", ""} {
		if sub, ok := PassportSubject(account); ok {
			t.Errorf("%q is not a passport account, got sub %q", account, sub)
		}
	}
}
