package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// 本文件钉住 GateDrain 块「缺省即默认值」的语义(集群外入口 D87)。
//
// 为什么要钉:K8s 的 login ConfigMap 由 k8s_deploy.ps1 另写模板、不含 GateDrain 块。
// 块缺失时如果拿到零值,Interval=0 = 排空判定循环不起,gate:{id}:drained 永远不出现,
// k8s_gate_drain.ps1 只能超时 —— 而起服日志之外没有任何报错。

var wantGateDrainDefaults = GateDrainConf{
	Interval:            5 * time.Second,
	DrainedBelowPlayers: 0,
	Deadline:            25 * time.Minute,
}

// Config.GateDrain 的 tag 不能带 optional:go-zero 对整块缺失的 optional 结构体不填内层 default。
// 下面的加载测试用的是同 tag 的探针结构体(整份 Config 缺必填块会先报错、轮不到 GateDrain),
// 所以这里要单独确认 Config 上的 tag 与探针一致。
func TestConfigGateDrainTagIsNotOptional(t *testing.T) {
	f, ok := reflect.TypeOf(Config{}).FieldByName("GateDrain")
	if !ok {
		t.Fatal("Config.GateDrain is missing")
	}
	if got := f.Tag.Get("json"); got != "GateDrain" {
		t.Fatalf("Config.GateDrain json tag = %q, want %q (optional would zero the whole block when it is absent)",
			got, "GateDrain")
	}
}

type gateDrainProbe struct {
	GateDrain GateDrainConf `json:"GateDrain"`
}

func TestGateDrainBlockMissingMeansDefaults(t *testing.T) {
	var p gateDrainProbe
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\n"), &p); err != nil {
		t.Fatalf("a missing GateDrain block must load with defaults, got error: %v", err)
	}
	if p.GateDrain != wantGateDrainDefaults {
		t.Fatalf("GateDrain on a missing block = %+v, want %+v", p.GateDrain, wantGateDrainDefaults)
	}
}

// 块写了但只写一部分:没写的字段同样取默认值。
func TestGateDrainPartialBlockFillsDefaults(t *testing.T) {
	var p gateDrainProbe
	if err := conf.LoadFromYamlBytes([]byte("GateDrain:\n  Deadline: 0s\n"), &p); err != nil {
		t.Fatalf("load partial GateDrain block: %v", err)
	}
	want := wantGateDrainDefaults
	want.Deadline = 0
	if p.GateDrain != want {
		t.Fatalf("GateDrain on a partial block = %+v, want %+v", p.GateDrain, want)
	}
}

// 仓内 etc/login.yaml 显式写出的值与默认值一致:本地开发与集群(靠默认值)行为相同。
func TestEtcYamlGateDrainBlock(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if c.GateDrain != wantGateDrainDefaults {
		t.Fatalf("etc/login.yaml GateDrain = %+v, want %+v", c.GateDrain, wantGateDrainDefaults)
	}
}
