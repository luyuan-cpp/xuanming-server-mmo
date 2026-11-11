package idissuer

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// LoadRegistryTLS 读登记处双向 TLS 的三个 PEM 文件,返回给登记处 etcd 客户端用的 tls.Config。
//
//   - 服务端证书只认 caFile 里的世界 CA。连到别的环境误起的登记处(它没有世界 CA 签的证书)握手就失败,
//     这同时钉住了「连的是哪一套登记处」:地域配置里只有地址,地址写错或被改过,靠这一步拦。
//   - 客户端证书每次握手都从 certFile、keyFile 重读。K8s 更新 Secret 卷里的文件之后,下一次重连就用新证书,
//     不用重启进程;重读失败只让这次握手失败,下次重连再读。
//   - 启动时先把三个文件都读一遍:路径或内容错了在装配时就报,不等第一次预留。
//   - CA 只在这里读一次。换世界 CA 是全世界一起做的变更,要按手册重启 data-service。
func LoadRegistryTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("id issuer: read the id registry CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("id issuer: no PEM certificate in the id registry CA %s", caFile)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, fmt.Errorf("id issuer: load the id registry client certificate %s: %w", certFile, err)
	}
	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("id issuer: reload the id registry client certificate %s: %w", certFile, err)
			}
			return &cert, nil
		},
	}, nil
}
