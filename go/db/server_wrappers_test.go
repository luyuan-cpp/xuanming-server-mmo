package main

import (
	"os"
	"regexp"
	"testing"

	dbpb "proto/db"
)

// 核对 internal/server/db/dbserver.go 实现了 proto/db.Db 的全部方法，防止新增 RPC 退化成内嵌 Unimplemented 桩。
func TestDbServerImplementsEveryRPC(t *testing.T) {
	const file = "internal/server/db/dbserver.go"
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	names := make([]string, 0, len(dbpb.Db_ServiceDesc.Methods)+len(dbpb.Db_ServiceDesc.Streams))
	for _, m := range dbpb.Db_ServiceDesc.Methods {
		names = append(names, m.MethodName)
	}
	for _, s := range dbpb.Db_ServiceDesc.Streams {
		names = append(names, s.StreamName)
	}
	if len(names) == 0 {
		t.Fatalf("%s declares no RPC", dbpb.Db_ServiceDesc.ServiceName)
	}
	for _, name := range names {
		declared := regexp.MustCompile(`(?m)^func \(\w+ \*DbServer\) ` + regexp.QuoteMeta(name) + `\(`)
		if !declared.Match(source) {
			t.Errorf("%s does not implement %s.%s: add the forwarding method to %s",
				file, dbpb.Db_ServiceDesc.ServiceName, name, file)
		}
	}
}
