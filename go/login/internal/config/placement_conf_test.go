package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// StorageIDForNewPlayer(player-storage-placement.md §8.3):未填 → 本 login 的 zone;填了 → 原样。
func TestPlacementStorageIDForNewPlayer(t *testing.T) {
	const loginZone = uint32(7)
	cases := map[string]struct {
		c    PlacementConf
		want uint32
	}{
		"0 uses login zone": {c: PlacementConf{}, want: loginZone},
		"global storage":    {c: PlacementConf{NewPlayerStorageId: 1000000}, want: 1000000},
		"another zone's db": {c: PlacementConf{NewPlayerStorageId: 102}, want: 102},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.c.StorageIDForNewPlayer(loginZone); got != tc.want {
				t.Fatalf("StorageIDForNewPlayer(%d) = %d, want %d", loginZone, got, tc.want)
			}
		})
	}
}

// 缺 Placement 块等于没有指定落点:新角色落在本 login 的 zone,不会出现不写落点的角色。
func TestPlacementBlockMissingUsesLoginZone(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\nListenOn: 127.0.0.1:1\n"), &c); err != nil {
		// 其余必填块缺失会报错;这里只关心 Placement 的零值语义,所以忽略加载错误。
		_ = err
	}
	if got := c.Placement.StorageIDForNewPlayer(7); got != 7 {
		t.Fatalf("StorageIDForNewPlayer on a missing block = %d, want 7", got)
	}
}

// placementKeysProbe 只用来核对样例 yaml 的段名 / 键名拼写。
//
// 字段**故意不加 optional**:go-zero 的必填语义下,键缺失时 conf.Load 报 "field ... is not set"。
// 为什么需要它:go-zero 不校验未知键,`Placment:` 这类拼写错误会被静默忽略,新角色就落到意料之外的库。
// json tag 必须与 Config / PlacementConf 上的保持一致。
type placementKeysProbe struct {
	Placement struct {
		NewPlayerStorageId uint32 `json:"NewPlayerStorageId"`
	} `json:"Placement"`
}

// 两份样例 yaml(仓内 etc 与 deploy/login-stack.linux)必须显式写出落点,
// 键名拼写由 placementKeysProbe 的必填字段保证。
func TestLoginYamlSpellsOutPlacementKeys(t *testing.T) {
	for _, path := range []string{
		"../../etc/login.yaml",
		"../../../../deploy/login-stack.linux/login.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			var p placementKeysProbe
			if err := conf.Load(path, &p); err != nil {
				t.Fatalf("%s must spell out Placement.NewPlayerStorageId explicitly "+
					"(a typo is silently ignored by go-zero): %v", path, err)
			}
		})
	}
}

// 探针自身的反例:段名 / 键名写错时必须加载失败,否则上面那条测试是空转。
func TestPlacementKeysProbeRejectsTypos(t *testing.T) {
	cases := map[string]string{
		"section typo": "Placment:\n  NewPlayerStorageId: 0\n",
		"id key typo":  "Placement:\n  NewPlayerStorageID2: 0\n",
		"block absent": "Name: login.rpc\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			var p placementKeysProbe
			if err := conf.LoadFromYamlBytes([]byte(yaml), &p); err == nil {
				t.Fatalf("probe accepted a misspelled / missing Placement block:\n%s", yaml)
			}
		})
	}
	var ok placementKeysProbe
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  NewPlayerStorageId: 0\n"), &ok); err != nil {
		t.Fatalf("probe rejected a correctly spelled Placement block: %v", err)
	}
}

// etc/login.yaml 的默认落点是本 login 的 zone。
// 键名拼写由 TestLoginYamlSpellsOutPlacementKeys 的必填探针保证。
func TestEtcYamlPlacementBlockIsGlobal(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if c.Placement.NewPlayerStorageId != 0 || c.Placement.StorageIDForNewPlayer(c.Node.ZoneId) != c.Node.ZoneId {
		t.Fatal("默认新号必须沿正规Node.ZoneId钉落点，不能凭空指定全局落点")
	}
}
