// route_tool 是路由表的运维工具(docs/design/player-route-mapping-durability-proposal.md §15)。
//
// 这里只是一个薄包装:逻辑在 internal/routetoolcli(参数、装配、退出码)与 internal/routetool
// (各子命令),并且并进了 data_service 主程序 —— K8s 镜像只构建单一入口 /app/service(deploy/k8s/Dockerfile.go-svc),
// 生产上用
//
//	data_service -f <data_service.yaml> -route-tool <子命令> [子命令参数]
//
// 本 cmd 只为 dev 的 `go run ./cmd/route_tool …` 保留,命令行与输出逐字相同:
//
//	route_tool -f <data_service.yaml> <tidb-prepare | freeze | unfreeze> [参数]
//
// 子命令、参数与退出码见 routetoolcli 的包注释与 usage。
package main

import (
	"context"
	"os"

	"data_service/internal/routetoolcli"
)

func main() {
	os.Exit(routetoolcli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
