package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"login/internal/logic/pkg/auth"
)

// mock 的 Apple 换票与撤销桩和 login 的 AppleRevoker 对得上(docs/design/account-deletion.md §8.2):
// 签发接口给的授权码能换票,换出的令牌能撤销,同一授权码第二次换票回 invalid_grant(login 映射成「需要重新授权」)。
func TestSandboxAppleRevokeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mock/apple/id_token?user=alice&aud=com.example.game")
	if err != nil {
		t.Fatal(err)
	}
	var minted struct {
		IdToken string `json:"id_token"`
		Code    string `json:"authorization_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil || minted.Code == "" || minted.IdToken == "" {
		t.Fatalf("签发应同时给令牌与授权码: %+v %v", minted, err)
	}
	resp.Body.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	revoker, err := auth.NewAppleRevoker("com.example.game", "TEAM123456", "KEY1234567", keyPEM, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := revoker.RevokeWithCode(context.Background(), minted.Code); err != nil {
		t.Fatalf("换票并撤销: %v", err)
	}
	if err := revoker.RevokeWithCode(context.Background(), minted.Code); !errors.Is(err, auth.ErrAppleCodeInvalid) {
		t.Fatalf("授权码一次性,第二次应回 ErrAppleCodeInvalid: %v", err)
	}

	resp, err = http.Get(srv.URL + "/mock/apple/revocations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var count struct {
		Revoked int `json:"revoked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&count); err != nil || count.Revoked != 1 {
		t.Fatalf("应撤销一次: %+v %v", count, err)
	}
}

// 两次签发同一种子的令牌也各不相同(jti),否则一次性消费会把第二次登录当成重放。
func TestSandboxIdTokensAreUnique(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	mint := func() string {
		resp, err := http.Get(srv.URL + "/mock/google/id_token?user=bob&aud=1-x.apps.googleusercontent.com")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			IdToken string `json:"id_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.IdToken
	}
	if mint() == mint() {
		t.Fatal("同一种子两次签发得到了同一枚令牌")
	}
}
