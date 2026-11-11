# 本机启动的 gate 会话身份密钥(docs/design/gate-online-identity.md §4、§10.1)。
# gate 用私钥签名(MMORPG_INTERNAL_AUTH_SIGNING_KEY,32 字节 Ed25519 种子的十六进制);
# login 与在线域服务只用公钥列表验签(MMORPG_INTERNAL_AUTH_PUBLIC_KEYS),gate 也读它核对配对。
# 已显式注入的值原样保留,只查形状;没注入时 dev/test 用下面的开发密钥对,非 dev/test 一律要求外部注入。
# 返回原环境供 finally 恢复,全程不输出值。

# 开发密钥对取 RFC 8032 §7.1 TEST 1 的公开测试向量:不是秘密,只给本机 dev/test 用。
# go/shared/callerauth 的 TestEd25519Rfc8032Vector 与 C++ 的 Ed25519MatchesRfc8032Vector 钉住这两个值配对。
$script:DevInternalIdentitySigningKey = '9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60'
$script:DevInternalIdentityPublicKeys = 'd75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'

# 与 go/shared/callerauth 的 ParseSeedHex / ParsePublicKeys、release_common.ps1 的 Assert-InternalIdentityKeyShape 同一规则。
function Test-LocalInternalIdentityKeyShape {
    param([AllowEmptyString()][string]$SigningKey, [AllowEmptyString()][string]$PublicKeys, [bool]$CheckSigningKey)
    if ($CheckSigningKey -and $SigningKey -cnotmatch '^[0-9a-fA-F]{64}$') { return $false }
    $seen = @{}
    foreach ($item in ($PublicKeys -split ',')) {
        if ($item -cnotmatch '^[0-9a-fA-F]{64}$') { return $false }
        $normalized = $item.ToLowerInvariant()
        if ($seen.ContainsKey($normalized)) { return $false }
        $seen[$normalized] = $true
    }
    return $true
}

function Enter-LocalGateIdentityEnvironment {
    param(
        [Parameter(Mandatory)][string]$RepoRoot,
        [bool]$AllowDevelopmentDefault = $true,
        # 只有 gate 进程要签名私钥;Go 服务只拿公钥列表,签不出名。
        [switch]$IncludeSigningKey
    )
    $signingName = 'MMORPG_INTERNAL_AUTH_SIGNING_KEY'
    $publicName = 'MMORPG_INTERNAL_AUTH_PUBLIC_KEYS'
    $saved = @{ $publicName = [Environment]::GetEnvironmentVariable($publicName, 'Process') }
    if ($IncludeSigningKey) { $saved[$signingName] = [Environment]::GetEnvironmentVariable($signingName, 'Process') }

    $publicKeys = $saved[$publicName]
    $signingKey = if ($IncludeSigningKey) { $saved[$signingName] } else { '' }
    $publicInjected = -not [string]::IsNullOrEmpty($publicKeys)
    $signingInjected = $IncludeSigningKey -and -not [string]::IsNullOrEmpty($signingKey)

    if ($publicInjected -or $signingInjected) {
        # 只注入了一半:拿外部的公钥配开发私钥(或反过来)必然不配对,不替运维猜。
        if ($IncludeSigningKey -and -not ($publicInjected -and $signingInjected)) {
            throw "Inject $signingName and $publicName together; a partial injection cannot pair with the development key."
        }
        if (-not (Test-LocalInternalIdentityKeyShape -SigningKey $signingKey -PublicKeys $publicKeys -CheckSigningKey $IncludeSigningKey)) {
            throw "Injected internal identity keys are malformed: $signingName must be 64 hex characters and $publicName a comma-separated list of 64-hex-character keys."
        }
        return $saved
    }

    if (-not $AllowDevelopmentDefault) { throw "Non-development runs must inject $publicName (and $signingName for gate)." }
    $yaml = [IO.File]::ReadAllText((Join-Path $RepoRoot 'go/login/etc/login.yaml'))
    if ($yaml -notmatch '(?m)^Mode:\s*(dev|test)\s*$') { throw 'The development internal identity key pair is only allowed when go/login/etc/login.yaml runs in dev or test mode.' }
    [Environment]::SetEnvironmentVariable($publicName, $script:DevInternalIdentityPublicKeys, 'Process')
    if ($IncludeSigningKey) { [Environment]::SetEnvironmentVariable($signingName, $script:DevInternalIdentitySigningKey, 'Process') }
    return $saved
}

function Restore-LocalGateIdentityEnvironment {
    param($Saved)
    if ($null -eq $Saved) { return }
    foreach ($key in $Saved.Keys) { [Environment]::SetEnvironmentVariable($key, $Saved[$key], 'Process') }
}
