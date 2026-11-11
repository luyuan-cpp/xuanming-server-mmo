package clusteridentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testUID = "5b1e6c4a-0d2f-4c3b-9a8e-7f6d5c4b3a21"

// newReader 指向 srv,凭据写在临时目录里,和 Pod 里的挂载同形;集群 CA 是 caPEM。
func newReader(t *testing.T, srv *httptest.Server, caPEM []byte) Reader {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return Reader{Host: host, Port: port, TokenPath: tokenPath, CAPath: caPath}
}

// fakeAPIServer 起一个 TLS 的假 API server,集群 CA 就是它自己的证书。
func fakeAPIServer(t *testing.T, handler http.HandlerFunc) Reader {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return newReader(t, srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func namespaceBody(name, uid string) string {
	return `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"` + name + `","uid":"` + uid + `"}}`
}

// 正常路径:带上 ServiceAccount 令牌 GET kube-system,返回 Prefix + UID。
func TestRead_ReturnsTheKubeSystemUID(t *testing.T) {
	seen := make(chan [2]string, 1)
	r := fakeAPIServer(t, func(w http.ResponseWriter, req *http.Request) {
		seen <- [2]string{req.URL.Path, req.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(namespaceBody("kube-system", testUID)))
	})
	got, err := r.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != Prefix+testUID {
		t.Fatalf("identity=%q, want %q", got, Prefix+testUID)
	}
	request := <-seen
	if request[0] != "/api/v1/namespaces/kube-system" || request[1] != "Bearer test-token" {
		t.Fatalf("request path=%q authorization=%q", request[0], request[1])
	}
}

// 不在 Kubernetes 里:明确报不在集群里,不猜身份。
func TestRead_OutsideKubernetesIsRefused(t *testing.T) {
	if got, err := (Reader{}).Read(context.Background()); !errors.Is(err, ErrNotInCluster) {
		t.Fatalf("got %q err=%v, want ErrNotInCluster", got, err)
	}
}

// ServiceAccount 没有权限:错误里点名缺的是哪条权限,运维照着补 RBAC。
func TestRead_ForbiddenNamesTheMissingPermission(t *testing.T) {
	r := fakeAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden","message":"namespaces \"kube-system\" is forbidden"}`))
	})
	_, err := r.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "get on namespaces/kube-system") {
		t.Fatalf("err=%v, want an HTTP 403 error naming the missing permission", err)
	}
}

// 读回来的不是 kube-system,或者 UID 不像 UID:拒绝,不把它当身份。
func TestRead_RejectsAnUnexpectedObject(t *testing.T) {
	for _, body := range []string{
		namespaceBody("default", testUID),
		namespaceBody("kube-system", "not-a-uid"),
		`{"metadata":{"name":"kube-system"}}`,
		`not json`,
	} {
		r := fakeAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		if got, err := r.Read(context.Background()); err == nil {
			t.Errorf("body %s: got %q, want an error", body, got)
		}
	}
}

// 证书不是集群 CA 签的:TLS 校验失败,不读身份。拦的是把请求导到别处的假 API server。
func TestRead_RefusesAServerTheClusterCADoesNotVouchFor(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(namespaceBody("kube-system", testUID)))
	}))
	t.Cleanup(srv.Close)
	r := newReader(t, srv, otherCAPEM(t))
	if got, err := r.Read(context.Background()); err == nil {
		t.Fatalf("got %q from a server outside the cluster CA", got)
	}
}

// otherCAPEM 是一张和测试服务器无关的自签 CA 证书。
func otherCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "another cluster CA"},
		NotBefore:             time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
