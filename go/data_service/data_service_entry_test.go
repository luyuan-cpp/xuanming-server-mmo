package main

import "testing"

// TestSelectEntry 钉住 main 的入口选择(映射持久化第 1b-1 批):-route-tool 必须带子命令、不能与 -migrate 同时给;
// 没有 -route-tool 却带位置参数一律拒绝 —— 以前这些参数被静默忽略,`data_service -f x.yaml freeze` 会起成一个普通服务。
func TestSelectEntry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migrate    bool
		routeTool  bool
		issuerInit bool
		args       []string
		want       entryMode
		wantErr    bool
	}{
		{name: "serve", want: entryServe},
		{name: "migrate", migrate: true, want: entryMigrate},
		{name: "route-tool with a subcommand", routeTool: true, args: []string{"freeze", "-reason=x"}, want: entryRouteTool},
		{name: "route-tool without a subcommand", routeTool: true, wantErr: true},
		{name: "migrate and route-tool", migrate: true, routeTool: true, args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments without route-tool", args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments with migrate", migrate: true, args: []string{"freeze"}, wantErr: true},
		{name: "id issuer init", issuerInit: true, want: entryIdIssuerInit},
		{name: "id issuer init and migrate", issuerInit: true, migrate: true, wantErr: true},
		{name: "id issuer init and route-tool", issuerInit: true, routeTool: true, args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments with id issuer init", issuerInit: true, args: []string{"freeze"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectEntry(tc.migrate, tc.routeTool, tc.issuerInit, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectEntry(%t, %t, %t, %q) = %d, want an error", tc.migrate, tc.routeTool, tc.issuerInit, tc.args, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("selectEntry(%t, %t, %t, %q) = (%d, %v), want (%d, nil)", tc.migrate, tc.routeTool, tc.issuerInit, tc.args, got, err, tc.want)
			}
		})
	}
}
