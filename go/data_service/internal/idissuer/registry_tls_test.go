package idissuer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 测试证书的有效期固定覆盖 2000~2099 年,不依赖当前时间。
var (
	pkiNotBefore = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	pkiNotAfter  = time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// testPKI 是测试用的一个 CA,签服务端和客户端证书。真 etcd 的双向 TLS 回归也用它生成证书。
type testPKI struct {
	t      *testing.T
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	serial int64
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestPKI(t *testing.T, name string) *testPKI {
	t.Helper()
	key := newTestKey(t)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             pkiNotBefore,
		NotAfter:              pkiNotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{t: t, caCert: cert, caKey: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), serial: 1}
}

// issue 签一张叶子证书,返回 PEM 的证书和私钥。server=true 时带 127.0.0.1、localhost 的 SAN,用途同时有服务端和客户端:
// etcd 开了 --client-cert-auth 时,它自己的网关也拿服务端证书当客户端证书用。
func (p *testPKI) issue(commonName string, server bool) (certPEM, keyPEM []byte) {
	p.t.Helper()
	p.serial++
	key := newTestKey(p.t)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    pkiNotBefore,
		NotAfter:     pkiNotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		template.ExtKeyUsage = append(template.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		p.t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		p.t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clientFiles 把世界 CA 和一张客户端证书写成 Secret 卷里的三个文件。
func clientFiles(t *testing.T, ca *testPKI, signer *testPKI, commonName string) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM := signer.issue(commonName, false)
	return writeTestFile(t, dir, "ca.crt", ca.caPEM), writeTestFile(t, dir, "tls.crt", certPEM), writeTestFile(t, dir, "tls.key", keyPEM)
}

// newMutualTLSServer 是只收 ca 签的客户端证书的 TLS 服务,响应体是客户端证书的 CN。
func newMutualTLSServer(t *testing.T, ca *testPKI) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := ca.issue("etcd-id-registry", true)
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.caCert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: pool, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// fetch 用 cfg 新建一条连接(不复用),返回服务端看到的客户端证书 CN。
func fetch(srv *httptest.Server, cfg *tls.Config) (string, error) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}, Timeout: 10 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// 正常握手:登记处只收世界 CA 签的客户端证书,发号器只认世界 CA 签的服务端证书。
func TestLoadRegistryTLS_MutualHandshake(t *testing.T) {
	world := newTestPKI(t, "world id registry CA")
	srv := newMutualTLSServer(t, world)
	cfg, err := LoadRegistryTLS(clientFiles(t, world, world, "region-1"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("min version %x is below TLS 1.2", cfg.MinVersion)
	}
	got, err := fetch(srv, cfg)
	if err != nil || got != "region-1" {
		t.Fatalf("handshake: client=%q err=%v", got, err)
	}
}

// 别的环境误起的登记处:它的证书不是世界 CA 签的,握手失败,发号器连不上它。这就是「连的是哪一套登记处」的钉子。
func TestLoadRegistryTLS_RefusesARegistryOutsideTheWorldCA(t *testing.T) {
	world := newTestPKI(t, "world id registry CA")
	stray := newTestPKI(t, "a registry somebody started by mistake")
	srv := newMutualTLSServer(t, stray)
	cfg, err := LoadRegistryTLS(clientFiles(t, world, world, "region-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fetch(srv, cfg); err == nil {
		t.Fatalf("connected to a registry outside the world CA as %q", got)
	}
}

// 客户端证书每次握手都重读:Secret 卷里的文件换成新证书之后,下一条连接就用新证书,不用重启。
func TestLoadRegistryTLS_ReloadsTheClientCertificateOnEachHandshake(t *testing.T) {
	world := newTestPKI(t, "world id registry CA")
	srv := newMutualTLSServer(t, world)
	caFile, certFile, keyFile := clientFiles(t, world, world, "region-1")
	cfg, err := LoadRegistryTLS(caFile, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fetch(srv, cfg); err != nil || got != "region-1" {
		t.Fatalf("before rotation: client=%q err=%v", got, err)
	}
	certPEM, keyPEM := world.issue("region-1-rotated", false)
	writeTestFile(t, filepath.Dir(certFile), filepath.Base(certFile), certPEM)
	writeTestFile(t, filepath.Dir(keyFile), filepath.Base(keyFile), keyPEM)
	if got, err := fetch(srv, cfg); err != nil || got != "region-1-rotated" {
		t.Fatalf("after rotation: client=%q err=%v", got, err)
	}
}

// 文件缺失或内容不对:装配时就报错,不等第一次预留。
func TestLoadRegistryTLS_RefusesBadFilesAtStartup(t *testing.T) {
	world := newTestPKI(t, "world id registry CA")
	caFile, certFile, keyFile := clientFiles(t, world, world, "region-1")
	dir := t.TempDir()
	notPEM := writeTestFile(t, dir, "not-a-cert", []byte("not a certificate"))
	_, otherKey := world.issue("another", false)
	mismatchedKey := writeTestFile(t, dir, "mismatched.key", otherKey)
	for name, files := range map[string][3]string{
		"missing CA":            {filepath.Join(dir, "missing.crt"), certFile, keyFile},
		"CA without a PEM cert": {notPEM, certFile, keyFile},
		"missing client cert":   {caFile, filepath.Join(dir, "missing.crt"), keyFile},
		"key of another cert":   {caFile, certFile, mismatchedKey},
	} {
		if _, err := LoadRegistryTLS(files[0], files[1], files[2]); err == nil {
			t.Errorf("%s: must be refused at startup", name)
		}
	}
}
