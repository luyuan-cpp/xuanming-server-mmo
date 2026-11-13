package data

// battle_sender_crosslang_test.go —— C++ scene 以 battle 身份发个人邮件(两包都放不下的战斗掉落改投邮件,
// docs/design/battle-overflow-mail.md)。身份由四个字面量拼成,两种语言各写一份:
// 发件角色号、签名的调用方名、发件密钥的环境变量名、签名里的方法路径。任何一个漂移,表现都是 scene 的每一封都被拒
// (Unauthenticated 或 ErrDenied),待寄条目只会一直留在玩家存档里。纯函数,不连库。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mailpb "proto/mail"
)

// 路径相对仓库根。go test 的工作目录是本包目录(go/mail/internal/data),仓库根在上四级。
const (
	cppMailOutboxSource = "cpp/libs/services/scene/player/system/player_mail_outbox.cpp"
	cppMailAdminClient  = "cpp/generated/grpc_client/mail/mail_admin_grpc_client.h"
	mailServiceYaml     = "go/mail/etc/mail.yaml"
)

func readRepoFile(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(relative)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("找不到 %s(只拷了 go/ 的构建环境),跳过逐字比对", relative)
	}
	if err != nil {
		t.Fatalf("读 %s: %v", relative, err)
	}
	return string(data)
}

func TestBattleSenderIdentityMatchesSceneOutbox(t *testing.T) {
	if got := SenderKind("battle"); got != 5 {
		t.Fatalf("SenderKind(\"battle\") = %d,要与 C++ 的 kBattleSenderKind 同为 5", got)
	}
	source := readRepoFile(t, cppMailOutboxSource)
	for _, literal := range []string{
		"kBattleSenderKind = 5;",
		`kBattleCaller = "battle";`,
		`kBattleProducerSecretEnv[] = "MAIL_PRODUCER_SECRET_BATTLE";`,
	} {
		if !strings.Contains(source, literal) {
			t.Fatalf("%s 里找不到 %q:C++ 侧的 battle 发件身份变了,Go 侧(SenderKind、mail.yaml 的 Producers)要同批改", cppMailOutboxSource, literal)
		}
	}
	// mail 只认配置里列出的调用方;battle 一行缺了,scene 的签名找不到验签密钥。
	if yaml := readRepoFile(t, mailServiceYaml); !strings.Contains(yaml, "battle: MAIL_PRODUCER_SECRET_BATTLE") {
		t.Fatalf("%s 的 Producers 里没有 battle: MAIL_PRODUCER_SECRET_BATTLE", mailServiceYaml)
	}
	// 签名把方法路径算在内:C++ 用生成客户端里的路径常量,必须与 Go 的 FullMethodName 逐字相同。
	client := readRepoFile(t, cppMailAdminClient)
	if want := `"` + mailpb.MailAdmin_SendPersonalMail_FullMethodName + `"`; !strings.Contains(client, want) {
		t.Fatalf("%s 里找不到方法路径 %s", cppMailAdminClient, want)
	}
}
