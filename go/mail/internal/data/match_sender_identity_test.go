package data

// match_sender_identity_test.go —— go/match 以 match 身份发个人邮件(群仙问道赛领奖,docs/planning/season-team-tournament-v1.md 第七节)。
// go/match 与 go/mail 是两个模块,身份各写一份:发件角色号、签名的调用方名、发件密钥的环境变量名。任何一个漂移,
// 表现都是 match 的每一封都被拒,带附件的奖励一直停在可领取(不会丢,但玩家领不到)。按源码逐字比对,不连库;
// readRepoFile 与 battle 那份共用(battle_sender_crosslang_test.go)。

import (
	"regexp"
	"strings"
	"testing"
)

const (
	matchTournamentClaimSource = "go/match/internal/logic/tournamentlogic.go"
	matchServiceYaml           = "go/match/etc/match_service.yaml"
)

func TestMatchSenderIdentityMatchesTournamentClaim(t *testing.T) {
	if got := SenderKind("match"); got != 6 {
		t.Fatalf("SenderKind(\"match\") = %d,要与 go/match 的 tournamentMailSenderKind 同为 6", got)
	}
	source := readRepoFile(t, matchTournamentClaimSource)
	for _, pattern := range []string{
		`tournamentMailSenderKind\s+uint32\s*=\s*6\b`,
		`tournamentMailCaller\s*=\s*"match"`,
	} {
		if !regexp.MustCompile(pattern).MatchString(source) {
			t.Fatalf("%s 里找不到 %s:match 侧的发件身份变了,mail 的 SenderKind 与 Producers 要同批改", matchTournamentClaimSource, pattern)
		}
	}
	// mail 只认配置里列出的调用方;match 一行缺了,match 的签名找不到验签密钥。两边的变量名必须是同一个。
	if yaml := readRepoFile(t, matchServiceYaml); !regexp.MustCompile(`(?m)^MailSecretEnv:\s*MAIL_PRODUCER_SECRET_MATCH\s*$`).MatchString(yaml) {
		t.Fatalf("%s 的 MailSecretEnv 不是 MAIL_PRODUCER_SECRET_MATCH", matchServiceYaml)
	}
	if yaml := readRepoFile(t, mailServiceYaml); !strings.Contains(yaml, "match: MAIL_PRODUCER_SECRET_MATCH") {
		t.Fatalf("%s 的 Producers 里没有 match: MAIL_PRODUCER_SECRET_MATCH", mailServiceYaml)
	}
}
