package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Sign in with Apple 的换票与撤销桩(docs/design/account-deletion.md §8.2),给账号注销的沙箱联调用:
//
//	POST /auth/token   表单 client_id、client_secret、code、grant_type=authorization_code
//	    授权码一次性:只认 /mock/apple/id_token 发过、没用过的 mock-code-*;用过或不认识回 400 {"error":"invalid_grant"}。
//	POST /auth/revoke  表单 client_id、client_secret、token、token_type_hint
//	    token 是 /auth/token 换出的 mock-refresh-* / mock-access-* 回 200,否则 400。
//	GET  /mock/apple/revocations
//	    回 {"revoked": N},沙箱手册据此核对注销时确实撤销了授权。
//
// client_secret 只核形状(三段、头部 alg=ES256 且有 kid、声明里 iss / sub / aud 非空),不验签名:mock 没有运维的 .p8 公钥。
// 签名本身由 login 的单测(auth/apple_revoke_test.go)用对应公钥验。

type appleTokenStub struct {
	mu      sync.Mutex
	used    map[string]bool // 已经换过票的授权码
	tokens  map[string]bool // 换出、还能撤销的令牌
	revoked int
}

func newAppleTokenStub() *appleTokenStub {
	return &appleTokenStub{used: map[string]bool{}, tokens: map[string]bool{}}
}

func (s *appleTokenStub) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil || !wellFormedClientSecret(r.PostForm.Get("client_secret")) ||
		r.PostForm.Get("client_id") == "" || r.PostForm.Get("grant_type") != "authorization_code" {
		appleError(w, "invalid_client")
		return
	}
	code := r.PostForm.Get("code")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(code, "mock-code-") || s.used[code] {
		appleError(w, "invalid_grant")
		return
	}
	s.used[code] = true
	refresh, access := "mock-refresh-"+randomHex(8), "mock-access-"+randomHex(8)
	s.tokens[refresh], s.tokens[access] = true, true
	writeJSON(w, map[string]any{"access_token": access, "token_type": "bearer", "expires_in": 3600, "refresh_token": refresh, "id_token": "mock"})
}

func (s *appleTokenStub) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil || !wellFormedClientSecret(r.PostForm.Get("client_secret")) || r.PostForm.Get("client_id") == "" {
		appleError(w, "invalid_client")
		return
	}
	token := r.PostForm.Get("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokens[token] {
		appleError(w, "invalid_request")
		return
	}
	delete(s.tokens, token)
	s.revoked++
	w.WriteHeader(http.StatusOK)
}

func (s *appleTokenStub) handleRevocations(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{"revoked": s.revoked})
}

func appleError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]any{"error": code})
}

// wellFormedClientSecret 只核 client_secret 的形状,见文件头。
func wellFormedClientSecret(secret string) bool {
	parts := strings.Split(secret, ".")
	if len(parts) != 3 {
		return false
	}
	var header map[string]string
	var claims map[string]any
	if decodeSegment(parts[0], &header) != nil || decodeSegment(parts[1], &claims) != nil {
		return false
	}
	return header["alg"] == "ES256" && header["kid"] != "" && claims["iss"] != nil && claims["sub"] != nil && claims["aud"] != nil
}

func decodeSegment(segment string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败只可能是运行环境坏了
	}
	return hex.EncodeToString(b)
}
