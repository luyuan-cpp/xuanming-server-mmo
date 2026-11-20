package nppa

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 技术规范 V1.9 第四、五节的样例。
const (
	specSecretKey  = "2836e95fcd10e04b0069bb1ee659955b"
	specPlain      = `{"ai":"test-accountId","name":"用户姓名","idNum":"371321199012310912"}`
	specCiphertext = "CqT/33f3jyoiYqT8MtxEFk3x2rlfhmgzhxpHqWosSj4d3hq2EbrtVyx2aLj565ZQNTcPrcDipnvpq/D/vQDaLKW70O83Q42zvR0//OfnYLcIjTPMnqa+SOhsjQrSdu66ySSORCAo"
	specSign       = "386c03b776a28c06b8032a958fbd89337424ef45c62d0422706cca633d8ad5fd"
	validPI        = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"
)

// decrypt 是 encrypt 的逆:Base64 解码后前 12 字节是 nonce,其余是密文加认证标签。
func decrypt(t *testing.T, secretKey, data string) string {
	t.Helper()
	key, err := hex.DecodeString(secretKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return string(plain)
}

// 规范的样例密文能被同样的布局解开,说明 nonce 在前、标签在后的布局与系统一致。
func TestSpecCiphertextUsesNoncePrefixLayout(t *testing.T) {
	if got := decrypt(t, specSecretKey, specCiphertext); got != specPlain {
		t.Fatalf("decrypted spec sample = %q, want %q", got, specPlain)
	}
}

func TestSignMatchesSpecExample(t *testing.T) {
	params := map[string]string{
		"appId": "test-appId", "bizId": "test-bizId", "timestamps": "1584949895758",
		"id": "test-id", "name": "test-name",
	}
	if got := sign(specSecretKey, params, `{"data":"`+specCiphertext+`"}`); got != specSign {
		t.Fatalf("sign = %s, want %s", got, specSign)
	}
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(Config{
		AppID: "app", BizID: "biz", SecretKey: specSecretKey,
		CheckURL: server.URL + "/check", QueryURL: server.URL + "/query", LoginoutURL: server.URL + "/loginout",
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }
	return client
}

// verifyRequest 按规范在服务端重算签名并解开报文体,返回明文。
func verifyRequest(t *testing.T, r *http.Request) string {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	params := map[string]string{
		"appId": r.Header.Get("appId"), "bizId": r.Header.Get("bizId"), "timestamps": r.Header.Get("timestamps"),
	}
	for k, v := range r.URL.Query() {
		params[k] = v[0]
	}
	if params["appId"] != "app" || params["bizId"] != "biz" || params["timestamps"] != "1700000000000" {
		t.Errorf("system headers = %v", params)
	}
	if got := r.Header.Get("sign"); got != sign(specSecretKey, params, string(body)) {
		t.Errorf("sign header does not verify")
	}
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("body is not {\"data\": ...}: %v", err)
	}
	return decrypt(t, specSecretKey, envelope.Data)
}

func TestCheckSendsEncryptedSignedRequest(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/check" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := verifyRequest(t, r); got != `{"ai":"a1","name":"张三","idNum":"11010519491231002X"}` {
			t.Errorf("plaintext = %s", got)
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok","data":{"result":{"status":0,"pi":"`+validPI+`"}}}`)
	})
	got, err := client.Check(context.Background(), "a1", "张三", "11010519491231002X")
	if err != nil || got != (AuthResult{Status: AuthOK, PI: validPI}) {
		t.Fatalf("Check = %+v, %v", got, err)
	}
}

func TestCheckClassifiesResults(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		want    AuthResult
		wantErr error
	}{
		{"pending", `{"errcode":0,"data":{"result":{"status":1}}}`, 200, AuthResult{Status: AuthPending}, nil},
		{"failed", `{"errcode":0,"data":{"result":{"status":2}}}`, 200, AuthResult{Status: AuthFailed}, nil},
		{"id format", `{"errcode":2001,"errmsg":"BUS AUTH IDNUM ILLEGAL"}`, 200, AuthResult{}, ErrInvalidIdentity},
		{"name illegal", `{"errcode":2005}`, 200, AuthResult{}, ErrInvalidIdentity},
		{"no record", `{"errcode":2003}`, 200, AuthResult{}, ErrNoRecord},
		{"bad sign", `{"errcode":1011,"errmsg":"SYS REQ PARTNER AUTH ERROR"}`, 200, AuthResult{}, ErrUnavailable},
		{"ok without pi", `{"errcode":0,"data":{"result":{"status":0}}}`, 200, AuthResult{}, ErrUnavailable},
		{"http 500", `oops`, 500, AuthResult{}, ErrUnavailable},
		{"not json", `<html>`, 200, AuthResult{}, ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			got, err := client.Check(context.Background(), "a1", "张三", "11010519491231002X")
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Check = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func TestQuerySignsTheAiParameter(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/query" || r.URL.Query().Get("ai") != "a2" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		verifyRequest(t, r)
		_, _ = io.WriteString(w, `{"errcode":0,"data":{"result":{"status":0,"pi":"`+validPI+`"}}}`)
	})
	if got, err := client.Query(context.Background(), "a2"); err != nil || got.PI != validPI {
		t.Fatalf("Query = %+v, %v", got, err)
	}
}

func TestLoginoutNumbersTheBatchAndReportsPartialRejections(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var plain struct {
			Collections []Behavior `json:"collections"`
		}
		if err := json.Unmarshal([]byte(verifyRequest(t, r)), &plain); err != nil {
			t.Fatal(err)
		}
		if len(plain.Collections) != 2 || plain.Collections[0].No != 1 || plain.Collections[1].No != 2 {
			t.Errorf("collections = %+v", plain.Collections)
		}
		_, _ = io.WriteString(w, `{"errcode":3001,"data":{"results":[{"no":2,"errcode":3010,"errmsg":"BUS COLL PLAYERID INVALID"}]}}`)
	})
	rejected, err := client.Loginout(context.Background(), []Behavior{
		{SI: "s1", BT: BehaviorLogin, OT: 1_700_000_000, PI: validPI},
		{SI: "s1", BT: BehaviorLogout, OT: 1_700_000_001, PI: validPI},
	})
	if err != nil || len(rejected) != 1 || rejected[0] != 2 {
		t.Fatalf("Loginout = %v, %v; want rejected [2]", rejected, err)
	}
}

func TestNewClientRejectsMalformedSecret(t *testing.T) {
	for _, secret := range []string{"", "2836e95f", "zz36e95fcd10e04b0069bb1ee659955b"} {
		if _, err := NewClient(Config{AppID: "a", BizID: "b", SecretKey: secret}, nil); err == nil {
			t.Errorf("NewClient accepted secret %q", secret)
		}
	}
	if _, err := NewClient(Config{SecretKey: specSecretKey}, nil); err == nil {
		t.Error("NewClient accepted an empty AppId / BizId")
	}
}
