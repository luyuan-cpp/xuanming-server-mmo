package main

import "testing"

// TestSelectEntry 钉住 main 的入口选择(映射持久化第 1b-1 批):-route-tool 必须带子命令、不能与 -migrate 同时给;
// 没有 -route-tool 却带位置参数一律拒绝 —— 以前这些参数被静默忽略,`data_service -f x.yaml freeze` 会起成一个普通服务。
// 发号器的运维入口(初始化、恢复、退役、开启登记处)彼此互斥,-id-issuer-rebind 只能跟着 -id-issuer-recover,
// -id-issuer-retire-unbound 只能跟着 -id-issuer-retire。
func TestSelectEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   entryFlags
		args    []string
		want    entryMode
		wantErr bool
	}{
		{name: "serve", want: entryServe},
		{name: "migrate", flags: entryFlags{migrate: true}, want: entryMigrate},
		{name: "route-tool with a subcommand", flags: entryFlags{routeTool: true}, args: []string{"freeze", "-reason=x"}, want: entryRouteTool},
		{name: "route-tool without a subcommand", flags: entryFlags{routeTool: true}, wantErr: true},
		{name: "migrate and route-tool", flags: entryFlags{migrate: true, routeTool: true}, args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments without route-tool", args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments with migrate", flags: entryFlags{migrate: true}, args: []string{"freeze"}, wantErr: true},
		{name: "id issuer init", flags: entryFlags{issuerInit: true}, want: entryIdIssuerInit},
		{name: "id issuer init and migrate", flags: entryFlags{issuerInit: true, migrate: true}, wantErr: true},
		{name: "id issuer init and route-tool", flags: entryFlags{issuerInit: true, routeTool: true}, args: []string{"freeze"}, wantErr: true},
		{name: "positional arguments with id issuer init", flags: entryFlags{issuerInit: true}, args: []string{"freeze"}, wantErr: true},
		{name: "id issuer recover", flags: entryFlags{issuerRecover: true}, want: entryIdIssuerRecover},
		{name: "id issuer recover with rebind", flags: entryFlags{issuerRecover: true, issuerRebind: true}, want: entryIdIssuerRecover},
		{name: "rebind alone", flags: entryFlags{issuerRebind: true}, wantErr: true},
		{name: "rebind with init", flags: entryFlags{issuerInit: true, issuerRebind: true}, wantErr: true},
		{name: "init and recover", flags: entryFlags{issuerInit: true, issuerRecover: true}, wantErr: true},
		{name: "id issuer retire", flags: entryFlags{issuerRetire: true}, want: entryIdIssuerRetire},
		{name: "retire and recover", flags: entryFlags{issuerRetire: true, issuerRecover: true}, wantErr: true},
		{name: "id issuer retire unbound", flags: entryFlags{issuerRetire: true, issuerRetireUnbound: true}, want: entryIdIssuerRetire},
		{name: "retire-unbound alone", flags: entryFlags{issuerRetireUnbound: true}, wantErr: true},
		{name: "retire-unbound with recover", flags: entryFlags{issuerRecover: true, issuerRetireUnbound: true}, wantErr: true},
		{name: "id issuer retire another code", flags: entryFlags{issuerRetire: true, issuerRetireCode: true}, want: entryIdIssuerRetire},
		{name: "retire-code alone", flags: entryFlags{issuerRetireCode: true}, wantErr: true},
		{name: "retire-code with init", flags: entryFlags{issuerInit: true, issuerRetireCode: true}, wantErr: true},
		{name: "id registry open", flags: entryFlags{registryOpen: true}, want: entryIdRegistryOpen},
		{name: "registry open and init", flags: entryFlags{registryOpen: true, issuerInit: true}, wantErr: true},
		{name: "positional arguments with recover", flags: entryFlags{issuerRecover: true}, args: []string{"x"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectEntry(tc.flags, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectEntry(%+v, %q) = %d, want an error", tc.flags, tc.args, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("selectEntry(%+v, %q) = (%d, %v), want (%d, nil)", tc.flags, tc.args, got, err, tc.want)
			}
		})
	}
}

// -id-issuer-retire-code:没给时退役配置里的码;显式给的值必须是 1..63。不合法的值一律报错,不回落到配置里的码:
// 那是本地域正在用的码,在本地域退役它会直接通过。
func TestRetireIssuerFactory(t *testing.T) {
	if factory, err := retireIssuerFactory(false, -1); err != nil || factory == nil {
		t.Fatalf("without the flag: factory=%v err=%v", factory != nil, err)
	}
	if factory, err := retireIssuerFactory(true, 7); err != nil || factory == nil {
		t.Fatalf("code 7: factory=%v err=%v", factory != nil, err)
	}
	for _, code := range []int{-5, -1, 0, 64} {
		if factory, err := retireIssuerFactory(true, code); err == nil || factory != nil {
			t.Errorf("code %d given explicitly must be refused, got factory=%v err=%v", code, factory != nil, err)
		}
	}
}
