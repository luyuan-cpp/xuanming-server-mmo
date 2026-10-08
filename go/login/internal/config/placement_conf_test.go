package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// StorageIDForNewPlayer 的三档语义(player-storage-placement.md §8.3):
// 关 → 0(不钉);开且未填 → 本 login 的 zone;开且填了 → 原样。
func TestPlacementStorageIDForNewPlayer(t *testing.T) {
	const loginZone = uint32(7)
	cases := map[string]struct {
		c    PlacementConf
		want uint32
	}{
		"zero value is off":         {c: PlacementConf{}, want: 0},
		"off ignores storage id":    {c: PlacementConf{NewPlayerStorageId: 1000000}, want: 0},
		"on with 0 uses login zone": {c: PlacementConf{PinOnCreate: true}, want: loginZone},
		"on with global storage":    {c: PlacementConf{PinOnCreate: true, NewPlayerStorageId: 1000000}, want: 1000000},
		"on with another zone's db": {c: PlacementConf{PinOnCreate: true, NewPlayerStorageId: 102}, want: 102},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.c.StorageIDForNewPlayer(loginZone); got != tc.want {
				t.Fatalf("StorageIDForNewPlayer(%d) = %d, want %d", loginZone, got, tc.want)
			}
		})
	}
}

// 块整个不写 = 关:go-zero 不给缺失的 optional 块填内层 default,零值必须就是「不钉」。
// 反过来如果有人给 PinOnCreate 加 default=true,仓外没写这块的老配置会静默开始钉落点,
// 而那时 go/db 可能还是旧版(按进程 zone 选库)—— §13 明令禁止的顺序。
func TestPlacementBlockMissingMeansOff(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\nListenOn: 127.0.0.1:1\n"), &c); err != nil {
		// 其余必填块缺失会报错;这里只关心 Placement 的零值语义,所以忽略加载错误。
		_ = err
	}
	if c.Placement.PinOnCreate {
		t.Fatal("a missing Placement block must leave PinOnCreate=false")
	}
	if got := c.Placement.StorageIDForNewPlayer(7); got != 0 {
		t.Fatalf("StorageIDForNewPlayer on a missing block = %d, want 0", got)
	}
}

// placementKeysProbe 只用来核对样例 yaml 的段名 / 键名拼写。
//
// 字段**故意不加 optional**:go-zero 的必填语义下,键缺失时 conf.Load 报 "field ... is not set"。
// 为什么需要它:go-zero 不校验未知键,`Placment:` / `PinOnCreat:` 这类拼写错误会被静默忽略,
// 而 Config.Placement 的零值恰好就是「关」—— 只断言零值抓不到拼写错误(§8.3 显式写关闭的
// 初衷正是让段名写错能被测试发现)。json tag 必须与 Config / PlacementConf 上的保持一致。
type placementKeysProbe struct {
	Placement struct {
		PinOnCreate        bool   `json:"PinOnCreate"`
		NewPlayerStorageId uint32 `json:"NewPlayerStorageId"`
	} `json:"Placement"`
}

// 两份样例 yaml(仓内 etc 与 deploy/login-stack.linux)必须显式写出 Placement 的两个键,
// 键名拼写由 placementKeysProbe 的必填字段保证。
func TestLoginYamlSpellsOutPlacementKeys(t *testing.T) {
	for _, path := range []string{
		"../../etc/login.yaml",
		"../../../../deploy/login-stack.linux/login.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			var p placementKeysProbe
			if err := conf.Load(path, &p); err != nil {
				t.Fatalf("%s must spell out Placement.PinOnCreate / Placement.NewPlayerStorageId explicitly "+
					"(a typo is silently ignored by go-zero): %v", path, err)
			}
		})
	}
}

// 探针自身的反例:段名 / 键名写错时必须加载失败,否则上面那条测试是空转。
func TestPlacementKeysProbeRejectsTypos(t *testing.T) {
	cases := map[string]string{
		"section typo": "Placment:\n  PinOnCreate: false\n  NewPlayerStorageId: 0\n",
		"key typo":     "Placement:\n  PinOnCreat: false\n  NewPlayerStorageId: 0\n",
		"id key typo":  "Placement:\n  PinOnCreate: false\n  NewPlayerStorageID2: 0\n",
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
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  PinOnCreate: false\n  NewPlayerStorageId: 0\n"), &ok); err != nil {
		t.Fatalf("probe rejected a correctly spelled Placement block: %v", err)
	}
}

// etc/login.yaml 里的 Placement 示例块必须是关着的(§13:go/db / data_service 新版就绪前不许钉)。
// 这里的零值断言**只负责「当前是关着的」**:块缺失或拼错时零值同样成立,
// 键名拼写由 TestLoginYamlSpellsOutPlacementKeys 的必填探针保证。
func TestEtcYamlPlacementBlockIsOff(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if c.Placement.PinOnCreate {
		t.Fatal("Placement.PinOnCreate must be false in etc/login.yaml until go/db placement routing is rolled out (§13)")
	}
	if c.Placement.StorageIDForNewPlayer(c.Node.ZoneId) != 0 {
		t.Fatal("etc/login.yaml must not pin a storage id by default")
	}
}
