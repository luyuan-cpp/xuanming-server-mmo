package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// 钉死 etc/login.yaml 里的号段块与 data_service 客户端块确实能被解析成设计稿 §6.4 的默认值。
//
// 为什么要有这条:go-zero 对 optional 结构体块缺失时不填内层 default(见 KillSwitchConf 注释),
// 段名写错(IdSegments / DataServiceRPC)会静默保持零值 —— 号段悄悄关掉、PlayerId 继续走
// snowflake,启动日志之外没有任何地方能看出来。
func TestEtcYamlIdSegmentBlock(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if !c.IdSegment.Enabled {
		t.Fatal("IdSegment.Enabled must be true in etc/login.yaml (segment minting is the Phase 4 default)")
	}
	if c.IdSegment.StepOrDefault() != 100 {
		t.Fatalf("IdSegment.Step = %d, want 100", c.IdSegment.Step)
	}
	if c.IdSegment.FallbackToSnowflake {
		t.Fatal("IdSegment.FallbackToSnowflake must default to false (plan §6.4)")
	}
	if c.DataServiceRpc.Etcd.Key != "dataservice.rpc" {
		t.Fatalf("DataServiceRpc.Etcd.Key = %q, want dataservice.rpc (data_service registers under that key)", c.DataServiceRpc.Etcd.Key)
	}
	if len(c.DataServiceRpc.Etcd.Hosts) == 0 {
		t.Fatal("DataServiceRpc.Etcd.Hosts is empty")
	}
	if !c.DataServiceRpc.NonBlock {
		t.Fatal("DataServiceRpc.NonBlock must be true: the segment source is a weak dependency, login must start without data_service")
	}
}

// 块整个不写 = 关(回滚开关),而不是半开。
func TestIdSegmentBlockMissingMeansDisabled(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\nListenOn: 127.0.0.1:1\n"), &c); err != nil {
		// 其余必填块缺失会报错;这里只关心 IdSegment 的零值语义,所以忽略加载错误。
		_ = err
	}
	if c.IdSegment.Enabled {
		t.Fatal("a missing IdSegment block must leave Enabled=false")
	}
	if c.IdSegment.StepOrDefault() != 100 {
		t.Fatalf("StepOrDefault on a missing block = %d, want 100", c.IdSegment.StepOrDefault())
	}
}
