#requires -Version 7
<#
.SYNOPSIS
	Pod 间透明加密(WireGuard)在集群上的只读核对项(docs/design/gate-online-identity.md §10.3)。

.DESCRIPTION
	部署前检(k8s_deploy.ps1 Assert-PodNetworkEncryptionPreflight)与集群核验脚本(k8s_netpol_probe.ps1)共用这一份,
	两边核对的是同一组对象、同一个判据。本文件只有纯函数与常量:不调 kubectl,调用方拿 kubectl 的输出来判。

	两层核对,缺一不可:
	  1. 开关:网络插件的配置里打开了 WireGuard(Get-PodNetworkEncryptionProbe)。
	  2. 每个节点都真的启用了:Calico 在启用了 WireGuard 的节点上把公钥写进 Node 的注解 projectcalico.org/WireguardPublicKey,
	     Cilium 写进 CiliumNode 的注解 network.cilium.io/wg-pub-key(Get-PodNetworkEncryptionNodeGaps)。
	开关打开、某个节点的内核却不支持 WireGuard 时,Calico 不报错,只是这个节点进出的流量不加密;只看开关会把它当成已加密。
	只认 IPv4 的公钥注解:集群是单栈 IPv4。

	不设 StrictMode、不定义脚本级变量:k8s_deploy.ps1 会 dot-source 本文件,这里的设置会带进它的整个脚本作用域。
#>

# 开关的只读核对项:kubectl 参数、期望值、出错时给人看的说明。
function Get-PodNetworkEncryptionProbe {
	param([Parameter(Mandatory = $true)][string]$Mode)
	switch ($Mode) {
		'cilium-wireguard' {
			return [pscustomobject]@{ Args = @('get', 'configmap', 'cilium-config', '-n', 'kube-system', '-o', 'jsonpath={.data.enable-wireguard}')
				Expected = 'true'; Description = 'kube-system/cilium-config enable-wireguard' }
		}
		'calico-wireguard' {
			return [pscustomobject]@{ Args = @('get', 'felixconfiguration', 'default', '-o', 'jsonpath={.spec.wireguardEnabled}')
				Expected = 'true'; Description = 'FelixConfiguration default spec.wireguardEnabled' }
		}
	}
	throw "internal error: no cluster probe for PodNetworkEncryption '$Mode'"
}

# 每个节点启用了 WireGuard 的证据在哪:取节点全集的 kubectl 参数、取证据对象的 kubectl 参数、证据注解名。
# Calico 的证据就在 Node 上(两个参数相同,调用方可以只取一次);Cilium 的在同名的 CiliumNode 上。
function Get-PodNetworkEncryptionNodeEvidenceSpec {
	param([Parameter(Mandatory = $true)][string]$Mode)
	switch ($Mode) {
		'cilium-wireguard' {
			return [pscustomobject]@{
				NodesArgs     = @('get', 'nodes', '-o', 'json')
				EvidenceArgs  = @('get', 'ciliumnodes.cilium.io', '-o', 'json')
				Annotation    = 'network.cilium.io/wg-pub-key'
				Description   = 'CiliumNode annotation network.cilium.io/wg-pub-key'
			}
		}
		'calico-wireguard' {
			return [pscustomobject]@{
				NodesArgs     = @('get', 'nodes', '-o', 'json')
				EvidenceArgs  = @('get', 'nodes', '-o', 'json')
				Annotation    = 'projectcalico.org/WireguardPublicKey'
				Description   = 'Node annotation projectcalico.org/WireguardPublicKey'
			}
		}
	}
	throw "internal error: no node evidence for PodNetworkEncryption '$Mode'"
}

# 取一个 Kubernetes 对象 JSON(ConvertFrom-Json 之后)上的注解;没有为空串。注解名里有 '.' 与 '/',不能当属性路径用。
function Get-KubeObjectAnnotation {
	param(
		[Parameter(Mandatory = $true)]$Object,
		[Parameter(Mandatory = $true)][string]$Name
	)
	$metadata = $Object.PSObject.Properties['metadata']
	if ($null -eq $metadata -or $null -eq $metadata.Value) { return '' }
	$annotations = $metadata.Value.PSObject.Properties['annotations']
	if ($null -eq $annotations -or $null -eq $annotations.Value) { return '' }
	$value = $annotations.Value.PSObject.Properties[$Name]
	if ($null -eq $value -or $null -eq $value.Value) { return '' }
	return ([string]$value.Value).Trim()
}

# 判每个节点有没有 WireGuard 公钥。NodesJson 是 `kubectl get nodes -o json` 的原文,EvidenceJson 是证据对象列表的原文
# (Calico 与 NodesJson 相同)。返回 { Nodes = 节点全集; Missing = 没有公钥的节点 },都按名字排序。
# 解析不了、节点全集为空都 throw:判不了不能当成通过。
function Get-PodNetworkEncryptionNodeGaps {
	param(
		[Parameter(Mandatory = $true)][string]$Mode,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$NodesJson,
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$EvidenceJson
	)
	$spec = Get-PodNetworkEncryptionNodeEvidenceSpec -Mode $Mode
	$nodes = ConvertFrom-KubeListJson -Json $NodesJson -What 'node list'
	$evidence = ConvertFrom-KubeListJson -Json $EvidenceJson -What "$($spec.Description) list"
	$names = @($nodes | ForEach-Object { [string]$_.metadata.name } | Where-Object { $_ } | Sort-Object -Unique)
	if ($names.Count -eq 0) {
		throw "PodNetworkEncryption=${Mode}: the cluster returned no nodes; cannot tell whether WireGuard is active"
	}
	$keyed = @{}
	foreach ($item in $evidence) {
		$name = [string]$item.metadata.name
		if ($name -and (Get-KubeObjectAnnotation -Object $item -Name $spec.Annotation)) { $keyed[$name] = $true }
	}
	$missing = @($names | Where-Object { -not $keyed.ContainsKey($_) })
	return [pscustomobject]@{ Nodes = [string[]]$names; Missing = [string[]]$missing; Description = $spec.Description }
}

# 在集群上核对声明的 Pod 间加密:先看开关,再看每个节点的公钥。只读。
# InvokeKubectl 由调用方给(部署脚本与核验脚本各有各的 kubectl 出口):收 kubectl 参数数组,回 { ExitCode; Stdout }。
# 回发现的问题(英文,一条一句);空数组 = 核对通过。kubectl 失败、输出解析不了都算问题:判不了不能当成通过。
function Get-PodNetworkEncryptionClusterProblems {
	param(
		[Parameter(Mandatory = $true)][string]$Mode,
		[Parameter(Mandatory = $true)][scriptblock]$InvokeKubectl
	)
	$probe = Get-PodNetworkEncryptionProbe -Mode $Mode
	$flag = & $InvokeKubectl $probe.Args
	$observed = if ($flag.ExitCode -eq 0) { ([string]$flag.Stdout).Trim() } else { '' }
	if ($observed -cne $probe.Expected) {
		# 开关没开,逐节点的公钥无从谈起,不再往下查。
		return [string[]]@("$($probe.Description) is '$observed', expected '$($probe.Expected)' (kubectl exit $($flag.ExitCode))")
	}

	$spec = Get-PodNetworkEncryptionNodeEvidenceSpec -Mode $Mode
	$nodes = & $InvokeKubectl $spec.NodesArgs
	if ($nodes.ExitCode -ne 0) {
		return [string[]]@("could not list the cluster nodes (kubectl exit $($nodes.ExitCode))")
	}
	$evidence = $nodes
	if (($spec.EvidenceArgs -join ' ') -cne ($spec.NodesArgs -join ' ')) {
		$evidence = & $InvokeKubectl $spec.EvidenceArgs
		if ($evidence.ExitCode -ne 0) {
			return [string[]]@("could not read the $($spec.Description) objects (kubectl exit $($evidence.ExitCode))")
		}
	}
	try {
		$gaps = Get-PodNetworkEncryptionNodeGaps -Mode $Mode -NodesJson ([string]$nodes.Stdout) -EvidenceJson ([string]$evidence.Stdout)
	}
	catch {
		return [string[]]@($_.Exception.Message)
	}
	if ($gaps.Missing.Count -gt 0) {
		return [string[]]@("$($gaps.Missing.Count) of $($gaps.Nodes.Count) nodes have no $($gaps.Description), so WireGuard is not active there and their traffic is not encrypted: $($gaps.Missing -join ', ')")
	}
	return [string[]]@()
}

# `kubectl get <kind> -o json` 的原文 → items 数组。不是 List 形状就 throw。
function ConvertFrom-KubeListJson {
	param(
		[Parameter(Mandatory = $true)][AllowEmptyString()][string]$Json,
		[Parameter(Mandatory = $true)][string]$What
	)
	if ([string]::IsNullOrWhiteSpace($Json)) { throw "kubectl returned an empty $What" }
	try { $parsed = $Json | ConvertFrom-Json -Depth 64 }
	catch { throw "kubectl returned an unparsable ${What}: $($_.Exception.Message)" }
	$items = $parsed.PSObject.Properties['items']
	if ($null -eq $items) { throw "kubectl returned a $What without items" }
	return @($items.Value)
}
