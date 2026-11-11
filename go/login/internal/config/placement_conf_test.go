package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"

	"shared/placement"
)

// 存档库和区脱钩(docs/design/player-data-tidb-scaling-plan.md §7.2):新号在本地域的 K 个存档库里按 player_id 取模选库,
// 区(Node.ZoneId)不是选库的输入。
func TestPlacementStorageIDForNewPlayerUsesRegionStorage(t *testing.T) {
	c := PlacementConf{Region: 1, StorageShards: 64}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, playerID := range []uint64{1, 63, 64, 1<<40 + 9} {
		got, err := c.StorageIDForNewPlayer(playerID)
		if err != nil {
			t.Fatalf("player %d: %v", playerID, err)
		}
		want, _ := placement.RegionStorageID(1, uint32(playerID%64))
		if got != want {
			t.Fatalf("player %d: storage %d, want %d", playerID, got, want)
		}
		if got <= placement.MaxZoneID {
			t.Fatalf("player %d landed in the zone id range (%d)", playerID, got)
		}
	}
	// 同一个号算两次落点相同:落点只由地域、K 和 player_id 决定。
	a, _ := c.StorageIDForNewPlayer(12345)
	b, _ := c.StorageIDForNewPlayer(12345)
	if a != b {
		t.Fatalf("storage is not a function of the player id: %d vs %d", a, b)
	}
}

// dev、test:地域 0、K=1,所有新号落在 player_store_1000000_db。
func TestPlacementDevRegionStorageIsTheDefaultGlobalStore(t *testing.T) {
	c := PlacementConf{Region: 0, StorageShards: 1}
	got, err := c.StorageIDForNewPlayer(987654321)
	if err != nil || got != placement.DefaultGlobalStorageID {
		t.Fatalf("dev storage = (%d,%v), want %d", got, err, placement.DefaultGlobalStorageID)
	}
}

// 不合法的配置在启动时拒绝,不建出落到错误库的角色。没配 StorageShards(包括整块缺席)也是不合法:
// 没有回落到本 zone 库的旧语义(批次 A17b 删除),报错要点名缺的键。
func TestPlacementValidateRejectsBadCombinations(t *testing.T) {
	for name, c := range map[string]PlacementConf{
		"block absent":          {},
		"region without shards": {Region: 1},
		"region out of range":   {Region: placement.MaxRegionCode + 1, StorageShards: 1},
		"too many shards":       {StorageShards: placement.RegionStorageStride + 1},
	} {
		err := c.Validate()
		if err == nil {
			t.Fatalf("%s: Validate accepted %+v", name, c)
		}
		if c.StorageShards == 0 && !strings.Contains(err.Error(), "Placement.StorageShards is required") {
			t.Fatalf("%s: error must name the missing key, got %v", name, err)
		}
	}
	// 建号时玩家号为 0 同样拒绝。
	if id, err := (PlacementConf{StorageShards: 1}).StorageIDForNewPlayer(0); err == nil {
		t.Fatalf("player 0 must be refused, got %d", id)
	}
}

// 配置文件里没有 Placement 块:加载不报缺键(块是 optional),由 Validate 在任何外部连接之前拒绝启动(login.go)。
func TestPlacementBlockMissingIsRefusedAtStartup(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\nListenOn: 127.0.0.1:1\n"), &c); err != nil {
		// 其余必填块缺失会报错;这里只关心 Placement 的零值,所以忽略加载错误。
		_ = err
	}
	if err := c.Placement.Validate(); err == nil {
		t.Fatalf("a missing Placement block must be refused, got %+v", c.Placement)
	}
}

// placementKeysProbe 只用来核对样例 yaml 的段名 / 键名拼写。
//
// 字段**故意不加 optional**:go-zero 的必填语义下,键缺失时 conf.Load 报 "field ... is not set"。
// 为什么需要它:go-zero 不校验未知键,`Placment:` 这类拼写错误会被静默忽略,login 就会因缺 StorageShards 拒启,
// 而报错里看不出是拼错了。json tag 必须与 Config / PlacementConf 上的保持一致。
type placementKeysProbe struct {
	Placement struct {
		Region        uint32 `json:"Region"`
		StorageShards uint32 `json:"StorageShards"`
	} `json:"Placement"`
}

// 两份样例 yaml(仓内 etc 与 deploy/login-stack.linux)必须显式写出地域与存档库数,
// 键名拼写由 placementKeysProbe 的必填字段保证。K8s 的 login ConfigMap 不镜像这一段,由 k8s_deploy.ps1 按 -Region / -PlayerStoreShards 渲染。
func TestLoginYamlSpellsOutPlacementKeys(t *testing.T) {
	for _, path := range []string{
		"../../etc/login.yaml",
		"../../../../deploy/login-stack.linux/login.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			var p placementKeysProbe
			if err := conf.Load(path, &p); err != nil {
				t.Fatalf("%s must spell out Placement.Region and Placement.StorageShards explicitly "+
					"(a typo is silently ignored by go-zero): %v", path, err)
			}
		})
	}
}

// 探针自身的反例:段名 / 键名写错时必须加载失败,否则上面那条测试是空转。
func TestPlacementKeysProbeRejectsTypos(t *testing.T) {
	cases := map[string]string{
		"section typo": "Placment:\n  Region: 0\n  StorageShards: 1\n",
		"shards typo":  "Placement:\n  Region: 0\n  StorageShard: 1\n",
		"region typo":  "Placement:\n  Regoin: 0\n  StorageShards: 1\n",
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
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  Region: 0\n  StorageShards: 1\n"), &ok); err != nil {
		t.Fatalf("probe rejected a correctly spelled Placement block: %v", err)
	}
}

// etc/login.yaml 是本机和 docker 调试栈的配置:地域 0、一个存档库(player_store_1000000_db),不跟着 Node.ZoneId 走。
func TestEtcYamlPlacementIsRegionStorage(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if err := c.Placement.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Placement.Region != 0 || c.Placement.StorageShards != 1 {
		t.Fatalf("etc/login.yaml Placement = %+v, want Region 0, StorageShards 1", c.Placement)
	}
	got, err := c.Placement.StorageIDForNewPlayer(1<<40 + 7)
	if err != nil || got != placement.DefaultGlobalStorageID {
		t.Fatalf("etc/login.yaml new player storage = (%d,%v), want %d", got, err, placement.DefaultGlobalStorageID)
	}
}
