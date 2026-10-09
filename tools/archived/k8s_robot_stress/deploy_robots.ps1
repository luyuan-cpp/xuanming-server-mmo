# Deploy 100K robots: 10 zones x 10 batches x 1000 robots
# Uses hostNetwork + ConfigMap for config

param(
    [int]$StartZone = 1,
    [int]$Zones = 10,
    [int]$Batches = 10,
    [int]$RobotsPerBatch = 1000,
    [string]$Image = "ghcr.io/luyuancpp/mmorpg-robot:latest"
)

$ErrorActionPreference = "Stop"
$tmpDir = Join-Path $env:TEMP "robot-deploy"
if (Test-Path $tmpDir) { Remove-Item $tmpDir -Recurse -Force }
New-Item -ItemType Directory -Path $tmpDir | Out-Null

$totalJobs = 0

foreach ($z in $StartZone..$Zones) {
    $ns = "mmorpg-zone-zone-$z"
    $loginAddr = "login.${ns}.svc.cluster.local:50000"

    foreach ($b in 1..$Batches) {
        $name = "robot-z${z}-b${b}"
        $cmName = "robot-cfg-z${z}-b${b}"
        $acctFmt = "robot_z${z}_b${b}_%04d"

        # robot.yaml content
        $cfgContent = @"
login_addr: "$loginAddr"
robot_count: $RobotsPerBatch
account_fmt: "$acctFmt"
password: "123456"
skill_ids: [1001]
action_interval: 5
report_interval: 10
mode: "stress"
profile: "stress"
"@

        # Write config to temp file
        $cfgFile = Join-Path $tmpDir "${name}-config.yaml"
        [System.IO.File]::WriteAllText($cfgFile, $cfgContent)

        # Create ConfigMap
        kubectl create configmap $cmName -n $ns --from-file="robot.yaml=$cfgFile" 2>&1 | Out-Null
        kubectl label configmap $cmName -n $ns app=robot 2>&1 | Out-Null

        # Job YAML
        $jobContent = @"
apiVersion: batch/v1
kind: Job
metadata:
  name: $name
  labels:
    app: robot
    zone: "zone-$z"
    batch: "b$b"
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 1800
  template:
    metadata:
      labels:
        app: robot
        zone: "zone-$z"
        batch: "b$b"
    spec:
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      restartPolicy: Never
      volumes:
      - name: config
        configMap:
          name: $cmName
      containers:
      - name: robot
        image: $Image
        imagePullPolicy: Never
        args: ["-c", "/app/etc/robot.yaml"]
        volumeMounts:
        - name: config
          mountPath: /app/etc
        resources:
          requests:
            memory: "32Mi"
            cpu: "10m"
          limits:
            memory: "256Mi"
            cpu: "200m"
"@

        $jobFile = Join-Path $tmpDir "${name}-job.yaml"
        [System.IO.File]::WriteAllText($jobFile, $jobContent)

        kubectl apply -n $ns -f $jobFile 2>&1 | Out-Null
        $totalJobs++
    }
    Write-Host "Zone ${z}: deployed ${Batches} batches"
}

# Cleanup temp files
Remove-Item $tmpDir -Recurse -Force
$deployedZones = $Zones - $StartZone + 1
Write-Host "=== Deployed $totalJobs robot jobs ($deployedZones zones x $Batches batches x $RobotsPerBatch robots) ==="
