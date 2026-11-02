#requires -Version 7
# 只读部署契约与最小权限模板；不启动服务、不执行DDL、不访问集群。
$ErrorActionPreference='Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/../lib/release_common.ps1"
$chatRepo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
function ChatSource([string]$Path) { Get-Content -LiteralPath (Join-Path $chatRepo $Path) -Raw }

Test-Case 'Chat部署先显式迁移，两类DSN只通过独立Secret key交给对应进程' {
    $runtime=ChatSource 'deploy/k8s/manifests/go-svc/chat.yaml'
    $migration=ChatSource 'deploy/k8s/manifests/go-svc/chat-migrate.yaml'
    # 常驻账号的DSN不再由Secret直接注入主容器：只有init里的registrar读运行输入Secret，把本次boot选中的DSN写进Pod私有目录，
    # 主容器启动时才从私有目录读出。主容器环境里不能再出现这把DSN的Secret引用，否则绕过写者闸门。
    Assert-Match $runtime '"-runtime-env-output","/run/sql-writer/runtime/private/env"' 'registrar把常驻DSN写进私有目录'
    Assert-Match $runtime 'CHAT_AUTHORITY_DSN=\\"\$\(cat /app/etc/private/env/CHAT_AUTHORITY_DSN\)\\"; export CHAT_AUTHORITY_DSN;' '常驻账号只取registrar写出的私有DSN'
    Assert-NotMatch $runtime 'name: CHAT_AUTHORITY_DSN' '主容器不再经环境变量直接拿到DSN'
    Assert-NotMatch $runtime 'chat-authority-secrets' '常驻Deployment不引用迁移账号所在的Secret'
    Assert-Match $migration 'name: CHAT_AUTHORITY_DSN\s+valueFrom:\s+secretKeyRef: \{name: chat-authority-secrets, key: migration-dsn, optional: false\}' '迁移账号只取migration key'
    Assert-NotMatch $runtime 'key: migration-dsn' '运行进程不取得DDL能力'
    Assert-NotMatch $migration 'name: LOGIN_REDIS_PASSWORD' '显式-migrate不读取登录Redis'
    Assert-Match $migration 'args: \["-f", "/app/etc/chat.yaml", "-migrate"\]' '使用服务正式迁移入口'
    Assert-Match (ChatSource 'tools/scripts/k8s_deploy.ps1') 'chat\s*=\s*@\{[^\r\n]+MigrateJob = "chat-migrate.yaml"' '现有迁移门禁先于常驻实例'
}

Test-Case 'Chat渲染必须携原会话与同域目录，账号值不进ConfigMap' {
    $source=ChatSource 'tools/scripts/k8s_deploy.ps1'
    $start=$source.IndexOf('Name: ${chatName}')
    $stop=$source.IndexOf('"client-rpc-router" {',$start)
    Assert-True ($start-ge 0-and $stop-gt $start) '能够定位正式Chat渲染分支'
    $chat=$source.Substring($start,$stop-$start)
    foreach($pattern in @('DSNEnv: \$\{chatSqlDsnEnv\}','Backend: \$\{DatabaseBackend\}','MaxOpenConns: \$\{chatSqlMaxOpen\}','GlobalLoginSessions: true','\$\{loginRedisBlock\}','(?m)^DataServiceRpc:','(?m)^\$\{dbPlacementTop\}')) {
        Assert-Match $chat $pattern '不能遗漏正式SQL或原登录态字段'
    }
    # 登录态段由公共准备段渲染(New-PlayerRedisConfigYaml):不建玩家域集群时指向 login 的实例与 DB,口令只写环境变量名。
    Assert-Match $source "'chat' \{ \`$loginRedisArgs\.StandalonePasswordEnv = 'LOGIN_REDIS_PASSWORD' \}" '单实例时口令走环境变量'
    Assert-Match $source "loginRedisArgs\.StandaloneDb = \[int\]\(Get-AuthoritativeScalar -RelativePath 'go/login/etc/login\.yaml' -KeyPath 'Node\.RedisClient\.DB'\)" '库号与 login 相同'
    Assert-NotMatch $chat '(?m)^ChatRedis:|^\s+(?:Pass|Password):' '旧缓存不能恢复成聊天权威，也不在兼容配置内写密码值'
    Assert-NotMatch $chat 'AccountRoleSchema' '身份校验走目录 RPC，聊天不再跨库读角色目录'
    $config=Join-Path $chatRepo 'go/chat/etc/chat.yaml'
    Assert-Equal 'CHAT_AUTHORITY_DSN' (Get-YamlScalar -Path $config -KeyPath 'DSNEnv').Value '镜像正规环境名'
    Assert-Equal 'tidb' (Get-YamlScalar -Path $config -KeyPath 'Backend').Value '源配置缺省 tidb 档,部署按 -DatabaseBackend 覆写'
}

Test-Case 'Chat业务角色无DDL或聊天记录改删权，迁移角色没有角色目录写权' {
    $runtime=ChatSource 'deploy/sql/chat_runtime_role.sql'
    $migration=ChatSource 'deploy/sql/chat_migration_role.sql'
    $grants=@(($runtime-split "`n")|Where-Object {$_-match '^GRANT '})
    Assert-Equal 2 $grants.Count '逐表具名授权，只有聊天库两张表'
    Assert-NotMatch ($grants-join "`n") '`mmorpg_global`' '身份校验走目录RPC，运行角色不碰全局库'
    Assert-Match $runtime 'GRANT SELECT, INSERT ON `mmorpg_chat`.`chat_send_authority`' '永久消息原件只允许读和追加'
    Assert-NotMatch ($grants-join "`n") '\b(?:ALL|CREATE|ALTER|DROP|DELETE|INDEX|GRANT OPTION)\b' '常驻账号没有迁移、删除或转授权能力'
    Assert-NotMatch $migration '`mmorpg_global`' '迁移角色无全局归属写权限'
    Assert-NotMatch (ChatSource 'deploy/mysql-init/00_init_zone_dbs.sql') 'GRANT ALL PRIVILEGES ON `mmorpg_chat`' '不把旧appuser的全权复用给Chat'
}
exit (Complete-TestRun -SuiteName 'Chat SQL权威部署与最小权限')
