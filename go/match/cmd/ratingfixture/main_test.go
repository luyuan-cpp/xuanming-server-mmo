package main

import (
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
