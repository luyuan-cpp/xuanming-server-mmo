package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"

	"login/internal/config"
	"login/internal/logic/pkg/nppa"
	"shared/antiaddiction"
)

// 国家防沉迷系统的沙箱端到端:login 真的 nppa.Client 去打 mock。报文的签名、加密、路径与应答的形状,
// 任何一边和 login 对不上,这里就会红。

// 合成的身份证号:地区码 999999 不存在,校验码是对的。
const (
	sandboxAdultID = "999999199001010016" // 1990-01-01 出生
	sandboxMinorID = "99999920120601001X" // 2012-06-01 出生
)

// nppaPath 取真系统地址里的路径。mock 的路由必须与它相同:login 只把三个地址的主机换成 mock。
func nppaPath(t *testing.T, official string) string {
	t.Helper()
	u, err := url.Parse(official)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func newSandboxNppaClient(t *testing.T, base, secretKey string) *nppa.Client {
	t.Helper()
	client, err := nppa.NewClient(nppa.Config{
		AppID: "sandbox-app", BizID: "sandbox-biz", SecretKey: secretKey,
		CheckURL:    base + nppaPath(t, nppa.DefaultCheckURL),
		QueryURL:    base + nppaPath(t, nppa.DefaultQueryURL),
		LoginoutURL: base + nppaPath(t, nppa.DefaultLoginoutURL),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func requireBirthDate(t *testing.T, pi, want string) {
	t.Helper()
	if birth, ok := antiaddiction.BirthDate(pi); !ok || birth.Format(time.DateOnly) != want {
		t.Fatalf("pi %q decodes to %v, %v; want birth date %s", pi, birth, ok, want)
	}
}

func TestNppaCheckAgainstSandboxMock(t *testing.T) {
	client := newSandboxNppaClient(t, newSandbox(t), sandboxNppaSecretKey)
	ctx := context.Background()

	adult, err := client.Check(ctx, "ai-adult", "张三", sandboxAdultID)
	if err != nil || adult.Status != nppa.AuthOK {
		t.Fatalf("adult Check = %+v, %v", adult, err)
	}
	requireBirthDate(t, adult.PI, "1990-01-01")
	if again, err := client.Check(ctx, "ai-adult-again", "张三", sandboxAdultID); err != nil || again.PI != adult.PI {
		t.Fatalf("the same person must get the same pi: %+v, %v", again, err)
	}

	// 未成年人靠身份证号里的出生日期区分:pi 的前 6 位和真系统一样带着它。
	minor, err := client.Check(ctx, "ai-minor", "李四", sandboxMinorID)
	if err != nil || minor.Status != nppa.AuthOK || minor.PI == adult.PI {
		t.Fatalf("minor Check = %+v, %v", minor, err)
	}
	requireBirthDate(t, minor.PI, "2012-06-01")

	if got, err := client.Check(ctx, "ai-failed", "不匹配的人", sandboxAdultID); err != nil || got.Status != nppa.AuthFailed {
		t.Fatalf("mismatch Check = %+v, %v; want AuthFailed", got, err)
	}
	// 校验码不对的号码:真系统回 2001。
	if _, err := client.Check(ctx, "ai-bad-id", "张三", "999999199001010010"); !errors.Is(err, nppa.ErrInvalidIdentity) {
		t.Fatalf("Check with a bad check code = %v, want ErrInvalidIdentity", err)
	}
}

func TestNppaPendingResultIsFetchedByQuery(t *testing.T) {
	client := newSandboxNppaClient(t, newSandbox(t), sandboxNppaSecretKey)
	ctx := context.Background()

	if got, err := client.Check(ctx, "ai-pending", "认证中的人", sandboxMinorID); err != nil || got.Status != nppa.AuthPending {
		t.Fatalf("pending Check = %+v, %v; want AuthPending", got, err)
	}
	resolved, err := client.Query(ctx, "ai-pending")
	if err != nil || resolved.Status != nppa.AuthOK {
		t.Fatalf("Query = %+v, %v; want the resolved result", resolved, err)
	}
	requireBirthDate(t, resolved.PI, "2012-06-01")
	// login 取到结果却没写进账号目录时会再查一次:结果还在。
	if again, err := client.Query(ctx, "ai-pending"); err != nil || again.PI != resolved.PI {
		t.Fatalf("second Query = %+v, %v", again, err)
	}
	if _, err := client.Query(ctx, "ai-never-submitted"); !errors.Is(err, nppa.ErrNoRecord) {
		t.Fatalf("Query of an unknown ai = %v, want ErrNoRecord", err)
	}
}

// login 配错了 secretKey:每个请求都验签失败,和真系统一样按系统错误回,不会悄悄放行。
func TestNppaSandboxRefusesAWrongSecretKey(t *testing.T) {
	client := newSandboxNppaClient(t, newSandbox(t), "00112233445566778899aabbccddeeff")
	if _, err := client.Check(context.Background(), "ai-1", "张三", sandboxAdultID); !errors.Is(err, nppa.ErrUnavailable) {
		t.Fatalf("Check with a wrong secret key = %v, want ErrUnavailable", err)
	}
}

func TestNppaLoginoutIsRecordedAndStaleBehaviorsAreRefused(t *testing.T) {
	base := newSandbox(t)
	client := newSandboxNppaClient(t, base, sandboxNppaSecretKey)
	ctx := context.Background()
	person, err := client.Check(ctx, "ai-1", "张三", sandboxAdultID)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	rejected, err := client.Loginout(ctx, []nppa.Behavior{
		{SI: "session-1", BT: nppa.BehaviorLogin, OT: now - 2, PI: person.PI},
		{SI: "session-1", BT: nppa.BehaviorLogout, OT: now - 1, PI: person.PI},
		{SI: "session-old", BT: nppa.BehaviorLogout, OT: now - 3600, PI: person.PI}, // 一小时前的:真系统只收 180 秒内的
	})
	if err != nil || len(rejected) != 1 || rejected[0] != 3 {
		t.Fatalf("Loginout = %v, %v; want only the stale third behavior rejected", rejected, err)
	}

	resp, err := http.Get(base + "/mock/nppa/behaviors")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Behaviors []nppa.Behavior `json:"behaviors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	got := listed.Behaviors
	if len(got) != 2 || got[0].SI != "session-1" || got[0].BT != nppa.BehaviorLogin || got[1].SI != "session-1" || got[1].BT != nppa.BehaviorLogout {
		t.Fatalf("recorded behaviors = %+v, want the login then the logout of session-1", got)
	}
}

// linux 登录栈(deploy/login-stack.linux)的 login 把防沉迷的三个地址指向本 mock:主机是 sandbox-mock,路径是真系统的路径。
// 缺省关着:robot 与口令冒烟不带实名,开着会在 gate 的 Login 被要求先实名。怎么打开见沙盒手册 §J。
func TestLinuxLoginStackAntiAddictionPointsAtSandboxMock(t *testing.T) {
	raw, err := os.ReadFile("../../../../deploy/login-stack.linux/login.yaml")
	if err != nil {
		t.Fatalf("read linux login stack config: %v", err)
	}
	var probe struct {
		AntiAddiction config.AntiAddictionConf `json:"AntiAddiction,optional"`
	}
	if err := conf.LoadFromYamlBytes(raw, &probe); err != nil {
		t.Fatalf("load linux login stack config: %v", err)
	}
	aa := probe.AntiAddiction
	if aa.Enabled {
		t.Fatal("AntiAddiction must stay off by default in the linux login stack: robots log in without a real name")
	}
	if aa.Nppa.AppId == "" || aa.Nppa.BizId == "" || aa.Nppa.SecretKeyEnv == "" {
		t.Fatalf("AntiAddiction.Nppa must be filled in so that flipping Enabled is enough: %+v", aa.Nppa)
	}
	for name, urls := range map[string][2]string{
		"CheckUrl":    {aa.Nppa.CheckUrl, nppa.DefaultCheckURL},
		"QueryUrl":    {aa.Nppa.QueryUrl, nppa.DefaultQueryURL},
		"LoginoutUrl": {aa.Nppa.LoginoutUrl, nppa.DefaultLoginoutURL},
	} {
		got, err := url.Parse(urls[0])
		if err != nil || got.Host != "sandbox-mock:18090" || got.Path != nppaPath(t, urls[1]) {
			t.Errorf("AntiAddiction.Nppa.%s = %q, want http://sandbox-mock:18090%s", name, urls[0], nppaPath(t, urls[1]))
		}
	}
}
