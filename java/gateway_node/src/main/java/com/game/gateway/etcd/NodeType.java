package com.game.gateway.etcd;

/**
 * Node type constants matching proto/common/base/node.proto eNodeType.
 */
public final class NodeType {

    private NodeType() {}

    public static final int GATE_NODE_SERVICE  = 4;
    public static final int SCENE_NODE_SERVICE = 3;
    public static final int LOGIN_NODE_SERVICE = 5;

    /** etcd key prefix for gate nodes (must match Go gateway's GateWatcher). */
    public static final String GATE_PREFIX  = "GateNodeService.rpc/";
    /** etcd key prefix for scene nodes. */
    public static final String SCENE_PREFIX = "SceneNodeService.rpc/";
    /**
     * etcd key prefix for go-zero login nodes. Written by go/login's node
     * registry: {@code LoginNodeService.rpc/zone/{zoneId}/node_type/5/node_id/{id}}
     * (go/login/internal/logic/pkg/node/node.go GetRpcPrefix/BuildRpcPath).
     */
    public static final String LOGIN_PREFIX = "LoginNodeService.rpc/";
}
