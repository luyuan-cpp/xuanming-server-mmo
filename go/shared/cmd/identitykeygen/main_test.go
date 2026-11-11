package main

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"shared/callerauth"
)

// RFC 8032 §7.1 TEST 1:用固定种子当随机源,输出必须是那一对。
const (
	testSeed      = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	testPublicKey = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
)

func TestGenerateWritesPairedKeys(t *testing.T) {
	seed, err := hex.DecodeString(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := generate(&out, bytes.NewReader(seed)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	want := callerauth.SigningKeyEnv + "=" + testSeed + "\n" + callerauth.PublicKeysEnv + "=" + testPublicKey + "\n"
	if out.String() != want {
		t.Fatalf("输出不对:\n got %q\nwant %q", out.String(), want)
	}
	// 输出的公钥能被验证方的解析器直接接受。
	if _, err := callerauth.ParsePublicKeys(testPublicKey); err != nil {
		t.Fatalf("公钥不能被 ParsePublicKeys 接受: %v", err)
	}
}

func TestGenerateFailsOnShortRandom(t *testing.T) {
	if err := generate(&bytes.Buffer{}, bytes.NewReader(make([]byte, 31))); err == nil {
		t.Fatal("随机源不足 32 字节时不应生成密钥")
	}
}

func TestDerivePublicKey(t *testing.T) {
	var out bytes.Buffer
	if err := derivePublicKey(&out, testSeed); err != nil {
		t.Fatalf("derive: %v", err)
	}
	if strings.TrimSpace(out.String()) != testPublicKey {
		t.Fatalf("推出的公钥不对: %s", out.String())
	}
	if err := derivePublicKey(&bytes.Buffer{}, "short"); err == nil {
		t.Fatal("格式不对的种子不应推出公钥")
	}
}
