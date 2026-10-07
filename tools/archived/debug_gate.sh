#!/bin/bash
set -e
apt-get update > /dev/null 2>&1
apt-get install -y gdb > /dev/null 2>&1

cd /app/bin
mkdir -p etc logs

cat > etc/base_deploy_config.yaml << 'EOF'
Etcd:
  Hosts:
    - "etcd:2379"
TableDataDirectory: "../generated/generated_tables/"
DataRootDirectory: "/app/"
LogLevel: 1
HealthCheckInterval: 1
Kafka:
  Brokers:
    - "kafka:9092"
  Topics:
    - "game-events"
  GroupID: "game-consumer-group"
  EnableAutoCommit: true
  AutoOffsetReset: "earliest"
EOF

cat > etc/game_config.yaml << 'EOF'
SceneNodeType: 0
ZoneId: 1
zoneredis:
  host: "127.0.0.1"
  port: 6379
EOF

gdb -batch -ex 'set confirm off' -ex run -ex 'bt full' ./gate 2>&1 | tail -80
