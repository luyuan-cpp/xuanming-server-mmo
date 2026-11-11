// identitykeygen 生成 gate 会话身份签名用的 Ed25519 密钥对(docs/design/gate-online-identity.md §4、§10.1)。
//
// 用法(在 go/shared 下):
//
//	go run ./cmd/identitykeygen            新生成一对,两行 KEY=VALUE 写到标准输出
//	go run ./cmd/identitykeygen -derive    读环境变量 MMORPG_INTERNAL_AUTH_SIGNING_KEY,只输出与它配对的公钥
//
// 私钥(种子)只交给密钥管理、只注入 gate;公钥列表注入 gate、login 与在线域服务,轮换期新旧公钥逗号并列。
// 工具不写文件;标准错误只打英文提示,不含任何密钥值。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"

	"shared/callerauth"
)

func main() {
	derive := flag.Bool("derive", false, "print the public key paired with "+callerauth.SigningKeyEnv+" instead of generating a new pair")
	flag.Parse()

	var err error
	if *derive {
		err = derivePublicKey(os.Stdout, os.Getenv(callerauth.SigningKeyEnv))
	} else {
		err = generate(os.Stdout, rand.Reader)
		if err == nil {
			fmt.Fprintln(os.Stderr, "Store the signing key in the gate-identity-auth Secret only; never commit it or paste it into logs.")
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "identitykeygen:", err)
		os.Exit(1)
	}
}

// generate 从 random 取 32 字节种子,输出种子与配对公钥两行。
func generate(out io.Writer, random io.Reader) error {
	seed := make([]byte, 32)
	if _, err := io.ReadFull(random, seed); err != nil {
		return fmt.Errorf("read random seed: %w", err)
	}
	signer, err := callerauth.NewEd25519Signer(seed)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s=%s\n%s=%s\n",
		callerauth.SigningKeyEnv, hex.EncodeToString(seed),
		callerauth.PublicKeysEnv, hex.EncodeToString(signer.PublicKey()))
	return err
}

// derivePublicKey 由已有的种子十六进制推出公钥,用于核对或补发公钥列表。
func derivePublicKey(out io.Writer, seedHex string) error {
	seed, err := callerauth.ParseSeedHex(seedHex)
	if err != nil {
		return fmt.Errorf("%s must be exactly 64 hex characters", callerauth.SigningKeyEnv)
	}
	signer, err := callerauth.NewEd25519Signer(seed)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, hex.EncodeToString(signer.PublicKey()))
	return err
}
