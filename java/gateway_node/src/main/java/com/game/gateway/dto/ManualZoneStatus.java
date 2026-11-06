package com.game.gateway.dto;

/**
 * Manual zone status set by operations.
 * Ordinal values match the DB column (zone_config.manual_status).
 */
public enum ManualZoneStatus {
    OPEN(0),
    MAINTENANCE(1),
    CLOSED(2),
    PREVIEW(3);

    private final int code;

    ManualZoneStatus(int code) { this.code = code; }

    public int getCode() { return code; }

    /**
     * code 是否是上面定义过的状态。管理接口写库前用它拒绝未知值:{@link #fromCode} 对未知值回 OPEN,
     * 不能拿来做校验,否则写进去的 7 会在选服列表里显示成开放。
     */
    public static boolean isDefined(int code) {
        for (ManualZoneStatus s : values()) {
            if (s.code == code) return true;
        }
        return false;
    }

    public static ManualZoneStatus fromCode(int code) {
        for (ManualZoneStatus s : values()) {
            if (s.code == code) return s;
        }
        return OPEN;
    }
}
