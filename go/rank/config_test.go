package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"rank/internal/data"
)

// loadRankYaml 按实际加载的那份读 etc/rank.yaml。源配置刻意不写 go-zero 的发现键 Etcd.Key(D-13),而 discov.EtcdConf.Key
// 是必填,原文件直接 conf.Load 会报「Etcd.Key is not set」;部署与本机启动都先在根 Etcd 段补 Key: ""
// (tools/scripts/k8s_deploy.ps1 的 Add-RootEtcdEmptyKey、tools/scripts/lib/sql_boot_catalog_local.ps1),这里照同一规则补,
// 再走 conf.LoadFromYamlBytes(同样调 config.Validate)。
func loadRankYaml(t *testing.T) config {
	t.Helper()
	raw, err := os.ReadFile("etc/rank.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rootEtcd := regexp.MustCompile(`(?m)^(Etcd:\r?\n  Hosts: [^\r\n]+)(\r?)$`)
	if n := len(rootEtcd.FindAllIndex(raw, -1)); n != 1 {
		t.Fatalf("etc/rank.yaml 应恰有一个单行 Hosts 的根 Etcd 段(部署脚本按同一形状补 Key), 实有 %d 个", n)
	}
	var rank config
	if err := conf.LoadFromYamlBytes(rootEtcd.ReplaceAll(raw, []byte("${1}${2}\n  Key: \"\"${2}")), &rank); err != nil {
		t.Fatalf("rank.yaml 不能通过启动校验: %v", err)
	}
	return rank
}

// rank 读不到邮件配置,只能在 rank.yaml 显式写同值。两份源配置必须一致,且落在邮件允许的范围内;
// K8s 的两份 ConfigMap 由这两份源配置生成(tools/scripts/k8s_deploy.ps1 不改这两个键),同样受本用例约束。
func TestRewardMailTtlMatchesMailService(t *testing.T) {
	rank := loadRankYaml(t)
	var mail struct {
		Mail struct {
			// 与 go/mail/internal/data/policy.go 的 Limits 同一缺省:不写即 30 天。
			MaxPersonalMailTtlDays uint32 `json:",default=30"`
		} `json:"Mail"`
	}
	if err := conf.Load("../mail/etc/mail.yaml", &mail); err != nil {
		t.Fatal(err)
	}
	if rank.MailMaxPersonalMailTtlDays != mail.Mail.MaxPersonalMailTtlDays {
		t.Fatalf("rank.yaml MailMaxPersonalMailTtlDays=%d 必须等于 mail.yaml Mail.MaxPersonalMailTtlDays=%d",
			rank.MailMaxPersonalMailTtlDays, mail.Mail.MaxPersonalMailTtlDays)
	}
	if _, err := data.RewardMailWindow(rank.MailMaxPersonalMailTtlDays); err != nil {
		t.Fatal(err)
	}
}

// 越出邮件允许范围的值在加载配置时就拒绝,进程起不来;1 天也拒:发件要剩 24 小时可领,截止点就是封榜时刻,一封都发不出。
func TestConfigRejectsOutOfRangeRewardMailTtl(t *testing.T) {
	rank := loadRankYaml(t)
	for _, days := range []uint32{0, 1, 366} {
		rank.MailMaxPersonalMailTtlDays = days
		if err := rank.Validate(); err == nil {
			t.Fatalf("MailMaxPersonalMailTtlDays=%d 必须拒启", days)
		}
	}
}
