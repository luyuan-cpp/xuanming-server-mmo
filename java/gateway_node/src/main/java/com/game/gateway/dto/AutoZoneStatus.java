package com.game.gateway.dto;

/**
 * Auto-detected health status from probing etcd/Redis.
 */
public enum AutoZoneStatus {
    /** 尚无成功探测、快照中没有该区，或快照已超过 TTL。 */
    UNKNOWN,
    HEALTHY,
    DEGRADED,
    DOWN
}
