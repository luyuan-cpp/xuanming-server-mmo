package svc

import (
	"github.com/stretchr/testify/require"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/rolefence"
	"strings"
	"testing"
)

func TestGlobalCallerSecretsAreRequiredAndIndependent(t *testing.T) {
	values := map[string]string{
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN":         strings.Repeat("l", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE":         strings.Repeat("t", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE":         strings.Repeat("s", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER": strings.Repeat("r", 32),
	}
	get := func(name string) string { return values[name] }
	require.NoError(t, validateGlobalCallerSecrets(get))
	values["MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER"] = " " + values["MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN"] + " "
	require.Error(t, validateGlobalCallerSecrets(get), "只读SM密钥不能拥有login写能力")
	values["MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER"] = ""
	require.Error(t, validateGlobalCallerSecrets(get))
}

// 角色护栏的密钥可以缺,缺的只关闭对应能力;配了就不能和别的调用方共用,也不能太短。
func TestRoleFenceSecretsAreOptionalButIndependent(t *testing.T) {
	values := map[string]string{
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN":         strings.Repeat("l", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE":         strings.Repeat("t", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE":         strings.Repeat("s", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER": strings.Repeat("r", 32),
	}
	get := func(name string) string { return values[name] }
	require.NoError(t, validateGlobalCallerSecrets(get))
	values[rolefence.CoordinatorSecretEnv] = strings.Repeat("c", 32)
	values[rolefence.SecretEnv("guild")] = strings.Repeat("g", 32)
	require.NoError(t, validateGlobalCallerSecrets(get))
	values[rolefence.SecretEnv("chat")] = values[rolefence.SecretEnv("guild")]
	require.Error(t, validateGlobalCallerSecrets(get), "读权威状态的密钥也要逐调用方独立")
	values[rolefence.SecretEnv("chat")] = values["MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN"]
	require.Error(t, validateGlobalCallerSecrets(get), "只读调用方不能拿到 login 的写能力")
	values[rolefence.SecretEnv("chat")] = "short"
	require.Error(t, validateGlobalCallerSecrets(get))
}

func TestRoleFenceAuthSkipsCallersWithoutSecret(t *testing.T) {
	values := map[string]string{rolefence.SecretEnv("guild"): strings.Repeat("g", 32)}
	auth, err := newRoleFenceAuth(func(name string) string { return values[name] })
	require.NoError(t, err)
	require.NotNil(t, auth)
	_, err = newRoleFenceAuth(func(string) string { return "" })
	require.Error(t, err, "一个密钥都没有时不装配,GetRoleFenceState 回 Unavailable")
}

// go/db 读落点的密钥和角色护栏同一规矩:可以缺,配了就不能和别的调用方共用。
func TestRouteCallerSecretIsOptionalButIndependent(t *testing.T) {
	values := map[string]string{
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN":         strings.Repeat("l", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE":         strings.Repeat("t", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE":         strings.Repeat("s", 32),
		"MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER": strings.Repeat("r", 32),
	}
	get := func(name string) string { return values[name] }
	values[accountdirectory.RouteSecretEnv] = strings.Repeat("d", 32)
	require.NoError(t, validateGlobalCallerSecrets(get))
	values[accountdirectory.RouteSecretEnv] = values["MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN"]
	require.Error(t, validateGlobalCallerSecrets(get), "go/db 不能拿到 login 的写能力")
	values[accountdirectory.RouteSecretEnv] = "short"
	require.Error(t, validateGlobalCallerSecrets(get))
}

// 写资格要恰好覆盖要写的逻辑库:少一份那个库打不开,多一份说明名册和配置不是同一次部署。
func TestCheckWriterSchemas(t *testing.T) {
	schemas := []string{"g_00", "g_01", "g_ledger", "zone_1_db"}
	writers := map[string]authoritysql.WriterConfig{}
	for _, schema := range schemas {
		writers[schema] = authoritysql.WriterConfig{}
	}
	require.NoError(t, checkWriterSchemas(writers, schemas))
	require.ErrorContains(t, checkWriterSchemas(writers, append([]string{"zone_2_db"}, schemas...)), "zone_2_db")
	require.Error(t, checkWriterSchemas(writers, schemas[1:]), "多一份也拒绝")
}
