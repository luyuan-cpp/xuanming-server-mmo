package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"match/internal/config"

	"github.com/zeromicro/go-zero/core/service"

	"shared/dbplacement"
)

func TestParseFixtureAcceptsRatedRows(t *testing.T) {
	parsed, err := parseFixture(strings.NewReader(`{"mode":7,"ratings":[{"account":"robot_8722","player_id":11,"rating_centi":120000},{"account":"robot_8723","player_id":12,"rating_centi":180000}]}`))
	if err != nil {
		t.Fatalf("合格的夹具应能读出: %v", err)
	}
	if parsed.Mode != 7 || len(parsed.Ratings) != 2 || parsed.Ratings[1].PlayerID != 12 || parsed.Ratings[1].RatingCenti != 180000 {
		t.Fatalf("读出的内容不对: %+v", parsed)
	}
}

// robot 的 prepare 写出 applied_ms = 0;重复写库时读到的是上次写回的时刻。两种都要能读,旧值不影响本次写库。
func TestParseFixtureAcceptsAppliedMs(t *testing.T) {
	for _, applied := range []string{"0", "1762790400123"} {
		body := `{"mode":7,"ratings":[{"account":"robot_8701","player_id":11,"rating_centi":150000}],"applied_ms":` + applied + `}`
		if _, err := parseFixture(strings.NewReader(body)); err != nil {
			t.Fatalf("applied_ms=%s 应能读出: %v", applied, err)
		}
	}
}

// 写回的夹具能被本工具与 robot 原样读回;覆盖已有文件,不留临时文件。
func TestWriteFixtureRecordsAppliedMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(`{"mode":7,"ratings":[{"account":"robot_8701","player_id":11,"rating_centi":150000}],"applied_ms":0}`), 0o644); err != nil {
		t.Fatalf("准备夹具: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开夹具: %v", err)
	}
	parsed, err := parseFixture(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("读夹具: %v", err)
	}
	parsed.AppliedMs = 1762790400123
	if err := writeFixture(path, parsed); err != nil {
		t.Fatalf("写回夹具: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回夹具: %v", err)
	}
	again, err := parseFixture(bytes.NewReader(body))
	if err != nil || !reflect.DeepEqual(again, parsed) {
		t.Fatalf("写回后读出 = %+v, %v; want %+v", again, err, parsed)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("临时文件应已改名: %v", err)
	}
}

func TestParseFixtureRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"不计分的模式":        `{"mode":5,"ratings":[{"player_id":11,"rating_centi":120000}]}`,
		"没有评分":          `{"mode":7,"ratings":[]}`,
		"player_id 为 0": `{"mode":7,"ratings":[{"player_id":0,"rating_centi":120000}]}`,
		"负分":            `{"mode":7,"ratings":[{"player_id":11,"rating_centi":-1}]}`,
		"同一人两行":         `{"mode":7,"ratings":[{"player_id":11,"rating_centi":1},{"player_id":11,"rating_centi":2}]}`,
		"不认识的字段":        `{"mode":7,"ratings":[{"player_id":11,"rating_centi":1,"games":3}]}`,
		"不是 JSON":       `mode=7`,
	} {
		if _, err := parseFixture(strings.NewReader(body)); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
}

func TestCheckTargetOnlyAllowsDirectDevOrTestDatabases(t *testing.T) {
	dev := config.Config{GlobalMatch: config.GlobalMatchConf{Host: "127.0.0.1:4000", User: "root", Database: "mmorpg_match"}}
	dev.Mode = service.DevMode
	if err := checkTarget(dev); err != nil {
		t.Fatalf("dev 直连应放行: %v", err)
	}
	test := dev
	test.Mode = service.TestMode
	if err := checkTarget(test); err != nil {
		t.Fatalf("test 直连应放行: %v", err)
	}
	production := dev
	production.Mode = service.ProMode
	if err := checkTarget(production); err == nil {
		t.Fatal("生产配置必须拒绝")
	}
	placed := dev
	placed.GlobalMatch.DbPlacement = dbplacement.Config{DefaultCluster: "a", SqlClusters: map[string]dbplacement.SqlCluster{"a": {}}}
	if err := checkTarget(placed); err == nil {
		t.Fatal("按映射拨号时必须拒绝")
	}
	missing := dev
	missing.GlobalMatch.Host = ""
	if err := checkTarget(missing); err == nil {
		t.Fatal("缺数据库地址必须拒绝")
	}
}
