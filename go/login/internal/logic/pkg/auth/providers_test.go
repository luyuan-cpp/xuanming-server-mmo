package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevelopmentPasswordProviderRequiresSecretAndPrefix(t *testing.T) {
	if _, err := NewDevelopmentPasswordProvider("", []string{"robot_"}); err == nil {
		t.Fatal("expected empty shared secret to be rejected")
	}
	if _, err := NewDevelopmentPasswordProvider("secret", nil); err == nil {
		t.Fatal("expected missing account prefixes to be rejected")
	}
	if _, err := NewDevelopmentPasswordProvider("secret", []string{""}); err == nil {
		t.Fatal("expected empty account prefix to be rejected")
	}
}

func TestDevelopmentPasswordProviderIsPrefixAndSecretRestricted(t *testing.T) {
	p, err := NewDevelopmentPasswordProvider("dev-secret", []string{"robot_", "dev_"})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	result, err := p.ValidatePassword(context.Background(), "robot_42", "dev-secret")
	if err != nil {
		t.Fatalf("expected valid development credentials: %v", err)
	}
	if result.Account != "robot_42" {
		t.Fatalf("account = %q, want robot_42", result.Account)
	}

	cases := []struct {
		name     string
		account  string
		password string
	}{
		{name: "real account prefix is forbidden", account: "alice", password: "dev-secret"},
		{name: "wrong shared secret", account: "robot_42", password: "wrong"},
		{name: "empty account", account: "", password: "dev-secret"},
		{name: "surrounding whitespace", account: " robot_42", password: "dev-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, gotErr := p.ValidatePassword(context.Background(), tc.account, tc.password); !errors.Is(gotErr, ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", gotErr)
			}
		})
	}
}

func newTestClient(handler http.Handler) (*http.Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	cli := &http.Client{
		Transport: &redirectTransport{base: srv.URL},
		Timeout:   srv.Config.ReadTimeout,
	}
	return cli, srv
}

// redirectTransport rewrites the request URL host/scheme to point at the test server,
// so we can keep production endpoint hostnames in the Provider while serving from httptest.
type redirectTransport struct {
	base string // e.g. "http://127.0.0.1:port"
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	idx := strings.Index(t.base, "://")
	scheme := t.base[:idx]
	host := t.base[idx+3:]
	req.URL.Scheme = scheme
	req.URL.Host = host
	req.Host = host
	return http.DefaultTransport.RoundTrip(req)
}

func TestWeChatProvider_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sns/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("code"); got != "abc" {
			t.Errorf("code mismatch: %q", got)
		}
		_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":7200,"refresh_token":"RT","openid":"OPEN1","scope":"snsapi_userinfo","unionid":"UNION1"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &WeChatProvider{AppId: "wxid", AppSecret: "secret", HTTP: cli}
	res, err := p.Validate(context.Background(), "abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "wx_UNION1" {
		t.Errorf("expected wx_UNION1, got %s", res.Account)
	}
}

func TestWeChatProvider_FallbackToOpenId(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sns/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"AT","openid":"OPEN1","scope":"snsapi_userinfo"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &WeChatProvider{AppId: "wxid", AppSecret: "secret", HTTP: cli}
	res, err := p.Validate(context.Background(), "abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "wx_OPEN1" {
		t.Errorf("expected wx_OPEN1, got %s", res.Account)
	}
}

func TestWeChatProvider_ApiError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sns/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":40029,"errmsg":"invalid code"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &WeChatProvider{AppId: "wxid", AppSecret: "secret", HTTP: cli}
	if _, err := p.Validate(context.Background(), "bad"); err == nil {
		t.Fatal("expected error")
	}
}

func TestWeChatProvider_EmptyCode(t *testing.T) {
	p := &WeChatProvider{AppId: "wxid", AppSecret: "secret"}
	if _, err := p.Validate(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty code")
	}
}

func TestQQProvider_SuccessJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2.0/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"client_id":"10001","openid":"QQOPEN","unionid":"QQUNION"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &QQProvider{AppId: "10001", HTTP: cli}
	res, err := p.Validate(context.Background(), "AT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "qq_QQUNION" {
		t.Errorf("expected qq_QQUNION, got %s", res.Account)
	}
}

func TestQQProvider_SuccessJSONP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2.0/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`callback( {"client_id":"10001","openid":"QQOPEN"} );`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &QQProvider{AppId: "10001", HTTP: cli}
	res, err := p.Validate(context.Background(), "AT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Account != "qq_QQOPEN" {
		t.Errorf("expected qq_QQOPEN, got %s", res.Account)
	}
}

func TestQQProvider_ClientIdMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2.0/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"client_id":"WRONG","openid":"QQOPEN"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &QQProvider{AppId: "10001", HTTP: cli}
	if _, err := p.Validate(context.Background(), "AT"); err == nil {
		t.Fatal("expected client_id mismatch error")
	}
}

func TestQQProvider_ApiError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2.0/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":100016,"error_description":"access_token check failed"}`))
	})
	cli, srv := newTestClient(mux)
	defer srv.Close()

	p := &QQProvider{AppId: "10001", HTTP: cli}
	if _, err := p.Validate(context.Background(), "bad"); err == nil {
		t.Fatal("expected error")
	}
}

func TestQQProvider_EmptyToken(t *testing.T) {
	p := &QQProvider{AppId: "10001"}
	if _, err := p.Validate(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty token")
	}
}
