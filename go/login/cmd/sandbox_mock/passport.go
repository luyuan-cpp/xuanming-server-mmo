package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// 官方通行证(Casdoor OIDC,auth_type="passport")的本地离线沙箱(docs/design/official-passport-registration.md)。
//
// 提供与 Casdoor 同路径的公钥接口 GET /.well-known/jwks,以及离线签发接口:
//
//	GET /mock/passport/id_token?user=<种子>&aud=<ClientId>[&iss=<Issuer>]
//
//   - iss 留空时默认取 sandboxPassportIssuer("http://127.0.0.1:18090"),容器内联调可传 ?iss=http://sandbox-mock:18090;
//   - 默认签身份令牌(tokenType="id-token"),有效期 1h(对齐 deploy/passport/init_data.json 的 expireInHours: 1);
//   - user=EXPIRED 签过期令牌;user=OVERSIZED_TTL 签 168h 的身份令牌(用于测试 login 对 Casdoor 默认 168h 的防护拦截);
//   - user=ACCESS_TOKEN / REFRESH_TOKEN 签访问令牌(1h)/ 刷新令牌(24h)。Casdoor 一次换票三枚都给,同一把钥匙、同 iss / aud / sub,
//     只有 tokenType 不同;login 只收身份令牌,这两个种子用来走「类型不对按凭证无效拒绝」的分支。应答的键仍叫 id_token。
//
// 账号注销要删通行证里的用户,mock 另有 Casdoor v4.27.1 的两个管理接口(路径、应答形状与真系统相同,成功与拒绝都回 HTTP 200;
// 凭据走 HTTP Basic,取 sandboxPassportAdminClientID / sandboxPassportAdminClientSecret):
//
//	GET  /api/get-user?userId=<sub>      用户在回 {"status":"ok","data":{owner,name,id}};已删掉回 data:null(不看凭据,与真系统一致)
//	POST /api/delete-user {owner,name}   删掉回 data:"Affected",本来就不在回 "Unaffected"
//	GET  /mock/passport/deleted_users    回 {"deleted":[sub...]},沙箱手册据此核对注销时确实删了通行证用户
//
// mock 里任何 sub 都算存在,直到被 delete-user 删掉,用户名固定是 "u_" + sub。删掉之后签发接口照样给这个种子签令牌
// (真系统里用户没了就登录不了);要验「注销后重新注册是新账号」得连真 Casdoor。

const (
	sandboxPassportIssuer   = "http://127.0.0.1:18090"
	sandboxPassportTokenTTL = time.Hour
	// sandboxPassportRefreshTTL 对齐 deploy/passport/init_data.json 的 refreshExpireInHours: 24。
	sandboxPassportRefreshTTL = 24 * time.Hour

	// 管理接口的凭据。deploy/login-stack.linux/login.yaml 的 Passport.AdminClientId / AdminClientSecret 必须写同一组值
	// (passport_test.go 钉住;不一致时每次删通行证用户都被 mock 拒绝)。
	sandboxPassportAdminClientID     = "xuanming-game-server"
	sandboxPassportAdminClientSecret = "sandbox_secret_keep_it_local"
	sandboxPassportOrganization      = "xuanming"
	sandboxPassportUserNamePrefix    = "u_"
)

func (i *idTokenIssuer) passportMintHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		user, aud := q.Get("user"), q.Get("aud")
		iss := q.Get("iss")
		if iss == "" {
			iss = sandboxPassportIssuer
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if user == "" || aud == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "query parameters user and aud are required"})
			return
		}
		iat := i.now()
		exp := iat.Add(sandboxPassportTokenTTL)
		tokenType := "id-token"
		switch user {
		case "EXPIRED":
			iat, exp = iat.Add(-2*time.Hour), iat.Add(-time.Hour)
		case "OVERSIZED_TTL":
			exp = iat.Add(168 * time.Hour)
		case "ACCESS_TOKEN":
			tokenType = "access-token"
		case "REFRESH_TOKEN":
			tokenType, exp = "refresh-token", iat.Add(sandboxPassportRefreshTTL)
		}
		token, err := i.sign(map[string]any{
			"iss":       iss,
			"aud":       aud,
			"sub":       "passport-sandbox-" + hashTo12(user, "passport-sub"),
			"iat":       iat.Unix(),
			"exp":       exp.Unix(),
			"jti":       randomHex(8),
			"tokenType": tokenType,
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": "sign failed"})
			return
		}
		writeJSON(w, map[string]any{"id_token": token})
	}
}

// passportAdminStub 模拟 Casdoor 的用户管理接口。deleted 记被 delete-user 删掉的 sub。
type passportAdminStub struct {
	mu      sync.Mutex
	deleted map[string]bool
}

func newPassportAdminStub() *passportAdminStub {
	return &passportAdminStub{deleted: map[string]bool{}}
}

// authorized 核对 Basic 凭据。Casdoor 把认不出的凭据当匿名,匿名调管理接口回 HTTP 200 + status=error,这里照做。
func (s *passportAdminStub) authorized(w http.ResponseWriter, r *http.Request) bool {
	id, secret, ok := r.BasicAuth()
	if ok && id == sandboxPassportAdminClientID && secret == sandboxPassportAdminClientSecret {
		return true
	}
	writeJSON(w, map[string]any{"status": "error", "msg": "Unauthorized operation", "data": nil})
	return false
}

func (s *passportAdminStub) handleGetUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sub := r.URL.Query().Get("userId")
	s.mu.Lock()
	gone := sub == "" || s.deleted[sub]
	s.mu.Unlock()
	if gone {
		writeJSON(w, map[string]any{"status": "ok", "msg": "", "data": nil})
		return
	}
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "msg": "", "data": map[string]string{
		"owner": sandboxPassportOrganization, "name": sandboxPassportUserNamePrefix + sub, "id": sub,
	}})
}

func (s *passportAdminStub) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var user struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&user) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !s.authorized(w, r) {
		return
	}
	sub, named := strings.CutPrefix(user.Name, sandboxPassportUserNamePrefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !named || sub == "" || user.Owner != sandboxPassportOrganization || s.deleted[sub] {
		writeJSON(w, map[string]any{"status": "ok", "msg": "", "data": "Unaffected"})
		return
	}
	s.deleted[sub] = true
	writeJSON(w, map[string]any{"status": "ok", "msg": "", "data": "Affected"})
}

func (s *passportAdminStub) handleDeletedUsers(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := make([]string, 0, len(s.deleted))
	for sub := range s.deleted {
		subs = append(subs, sub)
	}
	sort.Strings(subs)
	writeJSON(w, map[string]any{"deleted": subs})
}
