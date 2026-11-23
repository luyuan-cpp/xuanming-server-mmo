package main

import (
	"net/http"
	"time"
)

// 官方通行证(Casdoor OIDC,auth_type="passport")的本地离线沙箱(docs/design/official-passport-registration.md)。
//
// 提供与 Casdoor 同路径的公钥接口 GET /.well-known/jwks,以及离线签发接口:
//
//	GET /mock/passport/id_token?user=<种子>&aud=<ClientId>[&iss=<Issuer>]
//
//   - iss 留空时默认取 sandboxPassportIssuer("http://127.0.0.1:18090"),容器内联调可传 ?iss=http://sandbox-mock:18090;
//   - 默认有效期 1h(对齐 deploy/passport/init_data.json 的 expireInHours: 1);
//   - user=EXPIRED 签过期令牌;user=OVERSIZED_TTL 签 168h 令牌(用于测试 login 对 Casdoor 默认 168h 的防护拦截)。

const (
	sandboxPassportIssuer   = "http://127.0.0.1:18090"
	sandboxPassportTokenTTL = time.Hour
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
		switch user {
		case "EXPIRED":
			iat, exp = iat.Add(-2*time.Hour), iat.Add(-time.Hour)
		case "OVERSIZED_TTL":
			exp = iat.Add(168 * time.Hour)
		}
		token, err := i.sign(map[string]any{
			"iss": iss,
			"aud": aud,
			"sub": "passport-sandbox-" + hashTo12(user, "passport-sub"),
			"iat": iat.Unix(),
			"exp": exp.Unix(),
			"jti": randomHex(8),
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": "sign failed"})
			return
		}
		writeJSON(w, map[string]any{"id_token": token})
	}
}
