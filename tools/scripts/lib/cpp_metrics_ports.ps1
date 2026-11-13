# 本机多进程 C++ 节点的 Prometheus 出口端口(环境变量 MMORPG_METRICS_PORT)分配规则。
#
# cpp_nodes.ps1 起进程时按它给每个实例设端口,stress_snap.ps1 按同一规则从 PID 文件推出抓取地址;规则只写在这里一份。
# C++ 侧没设 MMORPG_METRICS_PORT 就不开出口,同一主机上的多个进程必须用不同端口
# (cpp/libs/engine/core/metrics/metrics_http_exporter.h;端口被占用时进程在启动期退出)。K8s 上每个 Pod 一个 IP,
# 部署脚本按角色固定端口,不走这里。
#
# 实例键 = run/pids/cpp_nodes.pid.json 的键(cpp_nodes.ps1 的 Invoke-Start 生成):
#   可选的 z<zone>_ 前缀(cpp_nodes.ps1 -Zone N 时才有)+ 节点名(gate / scene / battle)+ 可选的 _<序号>;
#   只起一个实例时没有序号,按 1 算。例:gate、scene_2、z1_scene_1、z3_battle。
# 端口 = 基准 + 1000 × zone(没有前缀算 0) + 节点偏移(gate 0、scene 100、battle 200) + 序号。
#   例:基准 19400 时 gate → 19401,scene_2 → 19502,z1_scene_1 → 20501,z2_battle → 21601。
# 基准建议 19400:避开本机 Go 服务的 91xx / 92xx 指标端口。序号只许 1~99,zone 只许 0~9,超出就报错(端口段会互相重叠)。

$CppMetricsNodeOffsets = [ordered]@{ gate = 0; scene = 100; battle = 200 }

# 把实例键拆成 zone、节点名与序号;认不出的键抛错(不猜)。
function ConvertFrom-CppInstanceKey {
    param([Parameter(Mandatory = $true)][string]$Key)
    $m = [regex]::Match($Key, '^(?:z(?<zone>[0-9]+)_)?(?<node>[a-z]+)(?:_(?<index>[0-9]+))?$')
    if (-not $m.Success -or -not $CppMetricsNodeOffsets.Contains($m.Groups['node'].Value)) {
        throw "C++ instance key '$Key' is not <[z<zone>_]gate|scene|battle[_<index>]>"
    }
    $zone = if ($m.Groups['zone'].Success) { [int]$m.Groups['zone'].Value } else { 0 }
    $index = if ($m.Groups['index'].Success) { [int]$m.Groups['index'].Value } else { 1 }
    if ($zone -gt 9) { throw "C++ instance key '$Key': zone $zone is outside 0..9" }
    if ($index -lt 1 -or $index -gt 99) { throw "C++ instance key '$Key': index $index is outside 1..99" }
    return [pscustomobject]@{ Key = $Key; Zone = $zone; Node = $m.Groups['node'].Value; Index = $index }
}

# 一个实例的出口端口。BasePort 只许 1024~55000,算出来的端口不会越过 65535。
function Get-CppMetricsPort {
    param(
        [Parameter(Mandatory = $true)][int]$BasePort,
        [Parameter(Mandatory = $true)][string]$Key
    )
    if ($BasePort -lt 1024 -or $BasePort -gt 55000) { throw "C++ metrics base port $BasePort is outside 1024..55000" }
    $instance = ConvertFrom-CppInstanceKey -Key $Key
    return $BasePort + 1000 * $instance.Zone + $CppMetricsNodeOffsets[$instance.Node] + $instance.Index
}
