package secretenv

import (
	"strings"
	"testing"
)

// lookupOf 把一张表包装成 os.LookupEnv 的形状,单测不读真实环境变量。
func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestFillWithoutEnvNameKeepsLiterals(t *testing.T) {
	// 本机 etc/*.yaml 不写指名键:照用字面量,同名环境变量在也不读。
	pass := "local-literal"
	if err := Fill("MySQLPasswordEnv", "", lookupOf(map[string]string{"MMORPG_MYSQL_PASSWORD": "from-env"}),
		Target{Field: "MySQL.Password", Value: &pass}); err != nil {
		t.Fatalf("Fill with an empty env name must be a no-op, got %v", err)
	}
	if pass != "local-literal" {
		t.Fatalf("literal must stay untouched, got %q", pass)
	}
}

func TestFillWritesEveryTargetVerbatim(t *testing.T) {
	// 值原样写入,不去空白、不展开 $:服务端与其它语言的进程拿到的都是 Secret 里的原值。
	const raw = " p@%^:,~#$!&*()=+.${HOME} "
	first, second := "", ""
	err := Fill("Spectate.TicketSecretEnv", "MMORPG_SPECTATE_TOKEN_SECRET", lookupOf(map[string]string{"MMORPG_SPECTATE_TOKEN_SECRET": raw}),
		Target{Field: "Spectate.TicketSecret", Value: &first},
		Target{Field: "Other.Secret", Value: &second})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if first != raw || second != raw {
		t.Fatalf("every target must get the variable's value verbatim, got %q / %q", first, second)
	}
}

func TestFillRejectsUnsetOrEmptyVariable(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"unset", map[string]string{}},
		{"empty", map[string]string{"MMORPG_MYSQL_PASSWORD": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass := ""
			err := Fill("MySQLPasswordEnv", "MMORPG_MYSQL_PASSWORD", lookupOf(tc.env), Target{Field: "MySQL.Password", Value: &pass})
			if err == nil {
				t.Fatal("a named but missing variable must fail closed")
			}
			if !strings.Contains(err.Error(), "MySQLPasswordEnv") || !strings.Contains(err.Error(), "MMORPG_MYSQL_PASSWORD") {
				t.Fatalf("error must name both the config key and the variable, got %v", err)
			}
			if pass != "" {
				t.Fatalf("target must stay untouched on error, got %q", pass)
			}
		})
	}
}

func TestFillRejectsLiteralAlongsideEnv(t *testing.T) {
	const secret = "the-real-secret"
	first, second := "", "stale-literal"
	err := Fill("MySQLPasswordEnv", "MMORPG_MYSQL_PASSWORD", lookupOf(map[string]string{"MMORPG_MYSQL_PASSWORD": secret}),
		Target{Field: "MySQL.Password", Value: &first},
		Target{Field: "Database.Passwd", Value: &second})
	if err == nil {
		t.Fatal("a literal secret next to the env key must be rejected")
	}
	if !strings.Contains(err.Error(), "Database.Passwd") {
		t.Fatalf("error must name the conflicting key, got %v", err)
	}
	if strings.Contains(err.Error(), "stale-literal") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error must not echo any secret, got %v", err)
	}
	if first != "" || second != "stale-literal" {
		t.Fatalf("no target may be written when the call fails, got first=%q second=%q", first, second)
	}
}

func TestFillRejectsWiringMistakes(t *testing.T) {
	env := lookupOf(map[string]string{"MMORPG_MYSQL_PASSWORD": "x"})
	if err := Fill("MySQLPasswordEnv", "MMORPG_MYSQL_PASSWORD", env); err == nil || !strings.Contains(err.Error(), "MySQLPasswordEnv") {
		t.Fatalf("a named variable with no target is a wiring bug and must be reported, got %v", err)
	}
	if err := Fill("MySQLPasswordEnv", "MMORPG_MYSQL_PASSWORD", env, Target{Field: "MySQL.Password"}); err == nil || !strings.Contains(err.Error(), "MySQL.Password") {
		t.Fatalf("a target without a field pointer must be reported by key, got %v", err)
	}
	pass := ""
	if err := Fill("MySQLPasswordEnv", "MMORPG_MYSQL_PASSWORD", nil, Target{Field: "MySQL.Password", Value: &pass}); err == nil {
		t.Fatal("a nil lookup must be rejected instead of panicking")
	}
}
