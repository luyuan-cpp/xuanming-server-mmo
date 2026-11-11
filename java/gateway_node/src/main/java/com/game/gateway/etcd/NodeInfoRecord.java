package com.game.gateway.etcd;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * Mirrors the protobuf-JSON that C++/Go nodes write to etcd.
 * Fields match NodeInfo in proto/common/base/common.proto.
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public class NodeInfoRecord {

    @JsonProperty("nodeId")
    private long nodeId;

    @JsonProperty("nodeType")
    private int nodeType;

    @JsonProperty("launchTime")
    private long launchTime;

    @JsonProperty("sceneNodeType")
    private int sceneNodeType;

    @JsonProperty("endpoint")
    private Endpoint endpoint;

    @JsonProperty("zoneId")
    private long zoneId;

    @JsonProperty("protocolType")
    private int protocolType;

    @JsonProperty("nodeUuid")
    private String nodeUuid;

    @JsonProperty("playerCount")
    private long playerCount;

    /**
     * gate 自报的客户端连接上限(NodeInfo.max_connections = 13,即它的 GateMaxConnections)。
     * 0 = 没声明(dev 下不设上限、旧版 gate);protobuf JSON 不输出 0 值,读出来同样是 0。
     */
    @JsonProperty("maxConnections")
    private long maxConnections;

    public long getNodeId() { return nodeId; }
    public void setNodeId(long nodeId) { this.nodeId = nodeId; }

    public int getNodeType() { return nodeType; }
    public void setNodeType(int nodeType) { this.nodeType = nodeType; }

    public long getLaunchTime() { return launchTime; }
    public void setLaunchTime(long launchTime) { this.launchTime = launchTime; }

    public int getSceneNodeType() { return sceneNodeType; }
    public void setSceneNodeType(int sceneNodeType) { this.sceneNodeType = sceneNodeType; }

    public Endpoint getEndpoint() { return endpoint; }
    public void setEndpoint(Endpoint endpoint) { this.endpoint = endpoint; }

    public long getZoneId() { return zoneId; }
    public void setZoneId(long zoneId) { this.zoneId = zoneId; }

    public int getProtocolType() { return protocolType; }
    public void setProtocolType(int protocolType) { this.protocolType = protocolType; }

    public String getNodeUuid() { return nodeUuid; }
    public void setNodeUuid(String nodeUuid) { this.nodeUuid = nodeUuid; }

    public long getPlayerCount() { return playerCount; }
    public void setPlayerCount(long playerCount) { this.playerCount = playerCount; }

    public long getMaxConnections() { return maxConnections; }
    public void setMaxConnections(long maxConnections) { this.maxConnections = maxConnections; }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Endpoint {
        @JsonProperty("ip")
        private String ip;

        @JsonProperty("port")
        private int port;

        public String getIp() { return ip; }
        public void setIp(String ip) { this.ip = ip; }

        public int getPort() { return port; }
        public void setPort(int port) { this.port = port; }
    }
}
