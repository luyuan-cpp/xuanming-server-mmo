//go:build etcd_integration

package idissuer

// 登记处双向 TLS 的真 etcd 回归,步骤见 docs/handoff/global-id-issuer-registry-20261105.md §9。
//
// 先跑 TestEtcdIntegration_WriteRegistryTLSFixture 生成证书(MMORPG_ID_ISSUER_IT_TLS_FIXTURE_OUT 给目录),
// 执行者用其中的 server.crt、server.key、ca.crt 起一个只收双向 TLS 的单节点 etcd(--client-cert-auth),
// 再设 MMORPG_ID_ISSUER_IT_TLS_REGISTRY(host:port)和 MMORPG_ID_ISSUER_IT_TLS_DIR(同一个目录)跑
// TestEtcdIntegration_RegistryOverMutualTLS。没给这些变量就跳过。

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// 证书夹具:世界 CA 签的服务端证书(SAN 127.0.0.1、localhost)和客户端证书,另一个 CA 和它签的客户端证书。
// 只是测试用的一次性证书,文件权限放宽到容器里的 etcd 读得到。
func TestEtcdIntegration_WriteRegistryTLSFixture(t *testing.T) {
	out := strings.TrimSpace(os.Getenv("MMORPG_ID_ISSUER_IT_TLS_FIXTURE_OUT"))
	if out == "" {
		t.Skip("MMORPG_ID_ISSUER_IT_TLS_FIXTURE_OUT is not set")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	world := newTestPKI(t, "test world id registry CA")
	other := newTestPKI(t, "test CA of another world")
	serverCert, serverKey := world.issue("etcd-id-registry", true)
	clientCert, clientKey := world.issue("region-test", false)
	strayCert, strayKey := other.issue("region-stray", false)
	for name, data := range map[string][]byte{
		"ca.crt":       world.caPEM,
		"server.crt":   serverCert,
		"server.key":   serverKey,
		"client.crt":   clientCert,
		"client.key":   clientKey,
		"other-ca.crt": other.caPEM,
		"stray.crt":    strayCert,
		"stray.key":    strayKey,
	} {
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote the mutual TLS fixture to %s", out)
}

// 登记处 etcd 只收双向 TLS:世界 CA 签的客户端照常开启、认领、读回;不带 TLS、不带客户端证书、
// 客户端证书是别的 CA 签的、认的是别的 CA(服务端证书对不上),都连不上。
func TestEtcdIntegration_RegistryOverMutualTLS(t *testing.T) {
	endpoint := itEndpoints(t, "MMORPG_ID_ISSUER_IT_TLS_REGISTRY")
	dir := strings.TrimSpace(os.Getenv("MMORPG_ID_ISSUER_IT_TLS_DIR"))
	if dir == "" {
		t.Skip("MMORPG_ID_ISSUER_IT_TLS_DIR is not set")
	}
	file := func(name string) string { return filepath.Join(dir, name) }
	root := itRoot(t) + "registry/"

	good, err := LoadRegistryTLS(file("ca.crt"), file("client.crt"), file("client.key"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newEtcdRegistryAt(endpoint, root, good)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if _, err := registry.cli.Delete(context.Background(), root, clientv3.WithPrefix()); err != nil {
			t.Errorf("clean up the isolated registry keys: %v", err)
		}
	}()
	if opened, err := registry.Open(ctx, 1); err != nil || !opened {
		t.Fatalf("open over mutual TLS: opened=%v err=%v", opened, err)
	}
	claim := Claim{State: ClaimActive, Authority: testAuthority, Deployment: "k8s:tls-test", PrimaryCluster: 1, SecondaryCluster: 2, HighWater: 1}
	if ok, err := registry.ClaimNew(ctx, 1, claim); err != nil || !ok {
		t.Fatalf("claim over mutual TLS: ok=%v err=%v", ok, err)
	}
	got, err := registry.Read(ctx, 1)
	if err != nil || !got.Exists || got.Claim != claim || got.ClusterID == 0 {
		t.Fatalf("read back over mutual TLS: %+v %v", got, err)
	}

	strayClient, err := LoadRegistryTLS(file("ca.crt"), file("stray.crt"), file("stray.key"))
	if err != nil {
		t.Fatal(err)
	}
	otherWorld, err := LoadRegistryTLS(file("other-ca.crt"), file("client.crt"), file("client.key"))
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*tls.Config{
		"plain connection":                 nil,
		"no client certificate":            {RootCAs: good.RootCAs, MinVersion: tls.VersionTLS12},
		"client certificate of another CA": strayClient,
		"server certificate of another CA": otherWorld,
	} {
		refused, err := newEtcdRegistryAt(endpoint, root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if _, err := refused.Read(readCtx, 1); err == nil {
			t.Errorf("%s: reading the registry must fail", name)
		}
		readCancel()
		_ = refused.Close()
	}
}
