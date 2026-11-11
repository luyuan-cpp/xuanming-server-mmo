package svc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"data_service/internal/config"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/service"
)

// 地域码 0 只给 dev、test:Region 是可选字段,漏写就是 0,其他模式不能把漏写当成一个地域。
func TestNewIdIssuer_RegionZeroReservedForDevAndTest(t *testing.T) {
	for _, mode := range []string{service.ProMode, service.PreMode, service.RtMode, ""} {
		var c config.Config
		c.Mode = mode
		_, err := NewIdIssuer(c)
		require.ErrorContains(t, err, "reserved for dev and test", "mode=%q", mode)
	}
}

// staging、prod 的配置来自运维预置的 Secret:抄错的地域码不报错,只会和别的地域撞号。部署参数和配置要对得上。
func TestCheckExpectedRegion(t *testing.T) {
	require.NoError(t, checkExpectedRegion("", 5), "本机路径没有这个环境变量")
	require.NoError(t, checkExpectedRegion("5", 5))
	require.ErrorContains(t, checkExpectedRegion("6", 5), "fix the runtime config")
}

// productionIssuerConfig 是一份生产形态(AllowAutoSeed=false)的发号器配置:三组地址互不相交,登记处带双向 TLS 的三个文件。
func productionIssuerConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv(envExpectedIdRegion, "1")
	var c config.Config
	c.Mode = service.ProMode
	c.IdSegment.Issuer.Region = 1
	c.IdSegment.Issuer.PrimaryEtcd = []string{"127.0.0.1:1"}
	c.IdSegment.Issuer.SecondaryEtcd = []string{"127.0.0.1:2"}
	c.IdSegment.Issuer.RegistryEtcd = []string{"127.0.0.1:3"}
	c.IdSegment.Issuer.RegistryTLS = writeRegistryClientTLS(t)
	return c
}

// 生产形态必须接上部署身份的读取器和登记处的 TLS:漏接身份时 idissuer.New 拒绝,这里就会报错。
// 构造不读身份,也不等 etcd 连上(客户端不设 DialTimeout,拨号在后台)。
func TestNewIdIssuer_ProductionWiresTheDeploymentIdentity(t *testing.T) {
	issuer, err := NewIdIssuer(productionIssuerConfig(t))
	require.NoError(t, err)
	_ = issuer.Close()
}

// 生产形态没配登记处的 TLS,或者证书文件读不出来:装配时就拒绝,不建任何连接。
func TestNewIdIssuer_ProductionRequiresRegistryTLS(t *testing.T) {
	c := productionIssuerConfig(t)
	c.IdSegment.Issuer.RegistryTLS = config.RegistryTLSConfig{}
	_, err := NewIdIssuer(c)
	require.ErrorContains(t, err, "RegistryTLS needs CAFile, CertFile and KeyFile")

	c = productionIssuerConfig(t)
	c.IdSegment.Issuer.RegistryTLS.CertFile = filepath.Join(t.TempDir(), "missing.crt")
	_, err = NewIdIssuer(c)
	require.ErrorContains(t, err, "id registry client certificate")
}

// writeRegistryClientTLS 现场生成一套 CA 和它签的客户端证书,写成 Secret 卷里的三个文件。有效期固定,不依赖当前时间。
func writeRegistryClientTLS(t *testing.T) config.RegistryTLSConfig {
	t.Helper()
	notBefore := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test id registry CA"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "region-1"},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	require.NoError(t, err)
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	require.NoError(t, err)

	dir := t.TempDir()
	write := func(name, blockType string, der []byte) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600))
		return path
	}
	return config.RegistryTLSConfig{
		CAFile:   write("ca.crt", "CERTIFICATE", caDER),
		CertFile: write("tls.crt", "CERTIFICATE", clientDER),
		KeyFile:  write("tls.key", "EC PRIVATE KEY", clientKeyDER),
	}
}

// 这些配置过了地域码这一关。它们没给 etcd 地址,会停在地址校验,不连 etcd。
func TestNewIdIssuer_RegionAccepted(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		region uint32
	}{
		{service.DevMode, 0},
		{service.TestMode, 0},
		{service.DevMode, 1},
		{service.ProMode, 1},
		{service.ProMode, 63},
	} {
		var c config.Config
		c.Mode = tc.mode
		c.IdSegment.Issuer.Region = tc.region
		_, err := NewIdIssuer(c)
		require.ErrorContains(t, err, "PrimaryEtcd, SecondaryEtcd and RegistryEtcd are all required", "mode=%q region=%d", tc.mode, tc.region)
	}
}
