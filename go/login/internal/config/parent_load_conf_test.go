package config

import (
	"reflect"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

func TestParentLoadIdentityVerificationDefaultsAndExplicitOverride(t *testing.T) {
	f, ok := reflect.TypeOf(Config{}).FieldByName("ParentLoad")
	if !ok || f.Tag.Get("json") != "ParentLoad" {
		t.Fatal("整块缺失时必须仍加载 ParentLoad 内部默认值")
	}
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"缺省块", "Name: login.rpc\n", true},
		{"空块", "ParentLoad: {}\n", true},
		{"显式开启", "ParentLoad:\n  VerifyOnRedisIdentityChange: true\n", true},
		{"显式关闭", "ParentLoad:\n  VerifyOnRedisIdentityChange: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p struct {
				ParentLoad ParentLoadConf `json:"ParentLoad"`
			}
			if err := conf.LoadFromYamlBytes([]byte(tc.yaml), &p); err != nil {
				t.Fatal(err)
			}
			if p.ParentLoad.VerifyOnRedisIdentityChange != tc.want {
				t.Fatalf("身份核验=%v，期望=%v", p.ParentLoad.VerifyOnRedisIdentityChange, tc.want)
			}
		})
	}
}
