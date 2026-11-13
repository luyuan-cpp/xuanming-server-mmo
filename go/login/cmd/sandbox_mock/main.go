// Sandbox mock for WeChat Open Platform / QQ Connect, the WeChat / Douyin
// mini game code2Session, and Sign in with Apple / Google identity tokens
// (signing keys plus a token minting stand-in for the client SDK, idtoken.go).
//
// What this does:
//
//	Stands up a single HTTP server that mimics the two exact endpoints
//	the WeChatProvider / QQProvider call:
//
//	  GET /sns/oauth2/access_token   (WeChat)
//	  GET /oauth2.0/me               (QQ)
//
//	The response shape is byte-for-byte the same as api.weixin.qq.com /
//	graph.qq.com would return. The data is deterministic, derived from
//	the `code` / `access_token` query parameter, so the same input always
//	maps to the same openid / unionid — useful for verifying that the
//	provider correctly resolves "wx_<unionid>" account keys and that the
//	account-reuse path works without flipping accounts between runs.
//
// Why we need it (and why the existing httptest in providers_test.go
// doesn't suffice):
//   - httptest is per-test-function: it can't be reached by a separate
//     binary running `Validate` against a real go-zero login.rpc.
//   - The real e2e validation requires:
//     client -> Java Gateway POST /api/login {auth_type:"wechat"}
//     -> gRPC login -> WeChatProvider.Validate -> HTTP GET
//     -> some mocked api.weixin.qq.com -> openid/unionid
//     -> account key derived -> tokens issued -> JSON response.
//     The single missing link in network-restricted environments is the
//     "some mocked api.weixin.qq.com" hop. This binary fills it.
//
// Pointing the provider at this mock:
//
//	In go/login/etc/login.yaml (Mode: dev — inline AppSecret and http://
//	endpoints are refused in any other Mode):
//	  AuthProviders:
//	    WeChat:
//	      AppId: "wx_sandbox_test"
//	      AppSecret: "any_string_ok_here"
//	      Endpoint: "http://127.0.0.1:18090"
//	    QQ:
//	      AppId: "100000001"            # must equal sandboxQQClientID
//	      Endpoint: "http://127.0.0.1:18090"
//	RequireUnionId defaults to true: only WeChat codes starting with "U"
//	get a unionid here, so other codes are refused unless the WeChat block
//	sets RequireUnionId: false.
//
// Crafting deterministic codes:
//
//	For WeChat, the `code` query parameter is passed straight through to
//	build the openid: `openid = "openid_" + sha256(code)[:12]`. unionid
//	is derived the same way but with a different suffix, AND only emitted
//	when the code starts with "U" (mirrors the real "no unionid until the
//	app is bound to an Open Platform subject" quirk).
//
//	For QQ, the `access_token` query parameter drives both openid and
//	unionid via the same hash trick. The mock always sets unionid since
//	we passed `unionid=1` on the wire.
//
// Running:
//
//	go run ./cmd/sandbox_mock                    # default :18090
//	go run ./cmd/sandbox_mock -addr :19091       # override
//
// Failure-mode testing:
//
//	The mock returns errcode=40029 when code == "EXPIRED" so ops can
//	exercise the "expired code" branch end-to-end without waiting 5 min.
//	QQ error mode: pass access_token == "INVALID" -> error response.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":18090", "listen address; defaults to :18090 to match docs/ops/wechat-qq-sandbox-runbook.md examples")
	flag.Parse()

	log.Printf("sandbox mock listening on %s — point WeChat/QQ providers' Endpoint at http://127.0.0.1%s", *addr, *addr)
	log.Printf("  /sns/oauth2/access_token  (WeChat) — code starting with 'U' returns unionid, code 'EXPIRED' returns errcode 40029")
	log.Printf("  /oauth2.0/me              (QQ)     — access_token 'INVALID' returns error response")
	log.Printf("  /sns/jscode2session       (WeChat mini game) — js_code starting with 'U' returns unionid, js_code 'EXPIRED' returns errcode 40029")
	log.Printf("  /mgplatform/api/apps/jscode2session (Douyin mini game) — code 'EXPIRED' returns an error response")
	log.Printf("  /auth/keys, /oauth2/v3/certs (Apple / Google signing keys)")
	log.Printf("  /mock/apple/id_token, /mock/google/id_token?user=<seed>&aud=<ClientId> — mint an identity token; user 'EXPIRED' mints an expired one")

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("sandbox mock: %v", err)
	}
}

// newMux 注册 mock 的全部路由。单独成函数,测试用同一张路由表起 httptest 服务,不会和 main 各写一份。
//
// 第三、四条是小游戏登录(2026-11-12 起,auth_type 为 wechat_minigame / douyin_minigame)。login.yaml 里加
// AuthProviders.WeChatMiniGame / DouyinMiniGame,Endpoint 同样指向本 mock,WeChatMiniGame.AppId 必须与
// WeChat.AppId 不同。两个接口的应答都带 session_key,与真实平台一致;login 不解析它(auth/minigame_providers.go)。
// 再往后是 Apple / Google 登录的公钥与签发接口(idtoken.go)。签名密钥每个 mux 一把,在这里生成。
func newMux() *http.ServeMux {
	issuer := newIdTokenIssuer()
	mux := http.NewServeMux()
	mux.HandleFunc("/sns/oauth2/access_token", handleWeChatTokenExchange)
	mux.HandleFunc("/oauth2.0/me", handleQQTokenVerify)
	mux.HandleFunc("/sns/jscode2session", handleWeChatMiniGameCode2Session)
	mux.HandleFunc("/mgplatform/api/apps/jscode2session", handleDouyinMiniGameCode2Session)
	mux.HandleFunc("/auth/keys", issuer.handleJWKS)
	mux.HandleFunc("/oauth2/v3/certs", issuer.handleJWKS)
	mux.HandleFunc("/mock/apple/id_token", issuer.mintHandler(sandboxAppleIssuer, sandboxAppleTokenTTL, "apple-sub"))
	mux.HandleFunc("/mock/google/id_token", issuer.mintHandler(sandboxGoogleIssuer, sandboxGoogleTokenTTL, "google-sub"))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// sandboxQQClientID 是 mock 对每个 QQ access_token 固定返回的 client_id。QQProvider 要求它等于配置的
// AuthProviders.QQ.AppId,所以 runbook §G.2 与 deploy/login-stack.linux/login.yaml 必须写同一个值
// (main_test.go 钉住 linux 栈那份;两边不一致时每次 QQ 登录都是 client_id mismatch)。
const sandboxQQClientID = "100000001"

// hashTo12 derives a stable 12-char hex string from input. Used so the
// same `code` always maps to the same openid — important for the
// "account reuse on second OAuth" verification in
// docs/ops/wechat-qq-sandbox-runbook.md §C.4.
func hashTo12(seed, salt string) string {
	sum := sha256.Sum256([]byte(seed + "|" + salt))
	return hex.EncodeToString(sum[:6]) // 12 chars
}

func handleWeChatTokenExchange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	appid := q.Get("appid")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Empty / missing required fields -> mimic real errcodes.
	switch {
	case code == "":
		writeJSON(w, map[string]any{"errcode": 40029, "errmsg": "invalid code"})
		return
	case code == "EXPIRED":
		writeJSON(w, map[string]any{"errcode": 40029, "errmsg": "invalid code"})
		return
	case appid == "":
		writeJSON(w, map[string]any{"errcode": 40013, "errmsg": "invalid appid"})
		return
	}

	resp := map[string]any{
		"access_token":  "MOCK_ACCESS_" + hashTo12(code, "access"),
		"expires_in":    7200,
		"refresh_token": "MOCK_REFRESH_" + hashTo12(code, "refresh"),
		"openid":        "openid_" + hashTo12(code, "openid"),
		"scope":         "snsapi_userinfo",
	}
	// Mirror the "unionid only when app is bound to an Open Platform
	// subject" quirk: emit unionid only when code starts with "U".
	if strings.HasPrefix(code, "U") {
		resp["unionid"] = "unionid_" + hashTo12(code, "unionid")
	}
	writeJSON(w, resp)
}

func handleQQTokenVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accessToken := q.Get("access_token")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case accessToken == "":
		writeJSON(w, map[string]any{"error": 100002, "error_description": "missing access_token"})
		return
	case accessToken == "INVALID":
		writeJSON(w, map[string]any{"error": 100007, "error_description": "wrong access_token"})
		return
	}

	resp := map[string]any{
		"client_id": sandboxQQClientID,
		"openid":    "qq_openid_" + hashTo12(accessToken, "openid"),
		"unionid":   "qq_unionid_" + hashTo12(accessToken, "unionid"),
	}
	writeJSON(w, resp)
}

// handleWeChatMiniGameCode2Session 模拟微信小游戏的 code2Session(GET /sns/jscode2session)。
// 身份的派生规则与 handleWeChatTokenExchange 相同,所以同一个字符串当 App 的 code 和当小游戏的 js_code
// 得到同一个 unionid:用它验证「同一主体下 App 与小游戏是同一个账号」。openid 按应用区分,这里换了盐。
func handleWeChatMiniGameCode2Session(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	jsCode := q.Get("js_code")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case jsCode == "" || jsCode == "EXPIRED":
		writeJSON(w, map[string]any{"errcode": 40029, "errmsg": "invalid code"})
		return
	case q.Get("appid") == "":
		writeJSON(w, map[string]any{"errcode": 40013, "errmsg": "invalid appid"})
		return
	}

	resp := map[string]any{
		"openid":      "mg_openid_" + hashTo12(jsCode, "minigame-openid"),
		"session_key": "MOCK_SESSION_" + hashTo12(jsCode, "session"),
	}
	// 与 App 形态同一个怪癖:小游戏没绑定开放平台账号就没有 unionid。js_code 以 "U" 开头才下发。
	if strings.HasPrefix(jsCode, "U") {
		resp["unionid"] = "unionid_" + hashTo12(jsCode, "unionid")
	}
	writeJSON(w, resp)
}

// handleDouyinMiniGameCode2Session 模拟抖音小游戏的 code2Session(GET /mgplatform/api/apps/jscode2session)。
// 成功应答 error=0;失败应答 error 非 0,另带 errcode / errmsg / message。抖音文档没有给出错误码表,
// 这里的 errcode 数值只是示例,不要拿它当平台的真实取值写断言。
func handleDouyinMiniGameCode2Session(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case code == "" || code == "EXPIRED":
		writeJSON(w, map[string]any{"errcode": 40018, "errmsg": "bad code", "error": 1, "message": "bad code"})
		return
	case q.Get("appid") == "":
		writeJSON(w, map[string]any{"errcode": 40015, "errmsg": "bad appid", "error": 1, "message": "bad appid"})
		return
	}

	writeJSON(w, map[string]any{
		"error":            0,
		"session_key":      "MOCK_SESSION_" + hashTo12(code, "douyin-session"),
		"openid":           "openid_" + hashTo12(code, "douyin-openid"),
		"anonymous_openid": "",
		"unionid":          "unionid_" + hashTo12(code, "douyin-unionid"),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("sandbox mock: encode: %v", err)
	}
}

// Compile-time sanity: keep the imports honest if someone later trims them.
var _ = fmt.Sprint
