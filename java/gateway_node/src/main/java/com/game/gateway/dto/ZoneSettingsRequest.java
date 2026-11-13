package com.game.gateway.dto;

import com.game.gateway.entity.ZoneConfig;

import java.time.LocalDateTime;

/**
 * {@code POST /admin/zones} 与 {@code PUT /admin/zones/{zoneId}} 的请求体:区服目录一行的<b>完整</b>业务字段。
 *
 * <h2>为什么不再直接绑实体(2026-11-05,第一轮数据审计 §4 第 18 条)</h2>
 * {@link ZoneConfig} 的字段是基本类型并带初值(manualStatus=0 即 OPEN、capacity=45000、recommended=false、
 * sortOrder=0、maintenanceMsg=""),请求体漏一个字段,绑出来的就是这个初值:整行写回时把维护中的区服改成开放,
 * 容量、推荐、排序、开服时间、维护文案一并被清掉,全程没有报错。这里一律用包装类型,null 就是「没给」,由
 * {@link #violation} 拒绝;openTime 本身允许为 null(没有开服时间),所以另记它「给没给」—— Jackson 只对 JSON
 * 里出现的属性调 setter,显式 {@code "open_time": null} 也会调,缺这个属性则不调。
 *
 * <p>JSON 字段名按 application.yaml 的 {@code spring.jackson.property-naming-strategy: SNAKE_CASE}:zone_id、name、
 * manual_status、capacity、maintenance_msg、open_time、recommended、sort_order。键名写错(例如驼峰)是未知字段,
 * 按 Spring Boot 缺省(FAIL_ON_UNKNOWN_PROPERTIES=false)被忽略,等同没给 → 400,不会落成初值。
 *
 * <p>只改状态走 {@code /maintenance}、{@code /open};改其它字段先 GET 整行、改完整行 PUT 回去,并把 GET 响应头里的
 * ETag 原样放进 If-Match(见 AdminZoneController#update)。GET 回来的 created_at / updated_at 等非业务字段同样被忽略。
 */
public final class ZoneSettingsRequest {

    /** 与 schema.sql 的列宽一致:name VARCHAR(64)、maintenance_msg VARCHAR(256),按字符(码点)计。 */
    static final int NAME_MAX_CHARS = 64;
    static final int MAINTENANCE_MSG_MAX_CHARS = 256;

    private Long zoneId;
    private String name;
    private Integer manualStatus;
    private Integer capacity;
    private String maintenanceMsg;
    private LocalDateTime openTime;
    private boolean openTimeGiven;
    private Boolean recommended;
    private Integer sortOrder;

    public Long getZoneId() { return zoneId; }
    public void setZoneId(Long zoneId) { this.zoneId = zoneId; }

    public void setName(String name) { this.name = name; }
    public void setManualStatus(Integer manualStatus) { this.manualStatus = manualStatus; }
    public void setCapacity(Integer capacity) { this.capacity = capacity; }
    public void setMaintenanceMsg(String maintenanceMsg) { this.maintenanceMsg = maintenanceMsg; }
    public void setRecommended(Boolean recommended) { this.recommended = recommended; }
    public void setSortOrder(Integer sortOrder) { this.sortOrder = sortOrder; }

    /** 调用即表示「给了」openTime,值为 null 表示没有开服时间。 */
    public void setOpenTime(LocalDateTime openTime) {
        this.openTime = openTime;
        this.openTimeGiven = true;
    }

    /**
     * 第一处缺失或不合法的字段(JSON 字段名 + 原因码 + 英文说明,见 {@link FieldViolation});全部合法返回 null。
     * 说明只含字段名与取值约束,不含请求里的值:它会原样进 400 的错误体和运行日志。
     *
     * @param pathZoneId PUT 传路径上的区服号:请求体里的 zone_id 可以不给,给了就必须与路径一致 —— 否则把别的区服
     *                   GET 回来的整行误 PUT 到这里,会拿别的区服的设置整行覆盖本区服。POST 传 null:zone_id 必填且为正。
     */
    public FieldViolation violation(Long pathZoneId) {
        if (pathZoneId == null) {
            if (zoneId == null) return FieldViolation.missing("zone_id", "zone_id is required");
            if (zoneId <= 0) return FieldViolation.invalid("zone_id", "zone_id must be a positive number");
        } else if (zoneId != null && !zoneId.equals(pathZoneId)) {
            return FieldViolation.mismatch("zone_id", "zone_id in body differs from path");
        }
        if (name == null) return FieldViolation.missing("name", "name is required");
        if (name.isBlank()) return FieldViolation.invalid("name", "name must not be blank");
        if (chars(name) > NAME_MAX_CHARS) {
            return FieldViolation.tooLong("name", "name exceeds " + NAME_MAX_CHARS + " characters");
        }
        if (manualStatus == null) return FieldViolation.missing("manual_status", "manual_status is required");
        if (!ManualZoneStatus.isDefined(manualStatus)) {
            return FieldViolation.invalid("manual_status", "manual_status must be a defined status code");
        }
        if (capacity == null) return FieldViolation.missing("capacity", "capacity is required");
        if (capacity <= 0) return FieldViolation.invalid("capacity", "capacity must be positive");
        if (maintenanceMsg == null) {
            return FieldViolation.missing("maintenance_msg", "maintenance_msg is required (use \"\" for none)");
        }
        if (chars(maintenanceMsg) > MAINTENANCE_MSG_MAX_CHARS) {
            return FieldViolation.tooLong("maintenance_msg",
                    "maintenance_msg exceeds " + MAINTENANCE_MSG_MAX_CHARS + " characters");
        }
        // open_time 是 DATETIME(秒):带小数秒的值落库会被舍入,回读就与请求对不上,create 的逐列比较会误判 409。
        if (!openTimeGiven) return FieldViolation.missing("open_time", "open_time is required (use null for none)");
        if (openTime != null && openTime.getNano() != 0) {
            return FieldViolation.invalid("open_time", "open_time must be whole seconds");
        }
        if (recommended == null) return FieldViolation.missing("recommended", "recommended is required");
        if (sortOrder == null) return FieldViolation.missing("sort_order", "sort_order is required");
        return null;
    }

    /**
     * 校验通过的请求体落成仓库绑定用的那一行(createdAt / updatedAt 由仓库按调用方时钟写)。
     *
     * @throws IllegalArgumentException 请求体不完整或不合法 —— 调用方必须先看 {@link #violation},这里只是兜底,
     *                                  免得某个字段为 null 时被拆箱成异常,或者更糟,被实体初值顶上。
     */
    public ZoneConfig toZoneConfig(long id) {
        FieldViolation violation = violation(id);
        if (violation != null) {
            throw new IllegalArgumentException("zone settings incomplete: " + violation.detail());
        }
        ZoneConfig z = new ZoneConfig();
        z.setZoneId(id);
        z.setName(name);
        z.setManualStatus(manualStatus);
        z.setCapacity(capacity);
        z.setMaintenanceMsg(maintenanceMsg);
        z.setOpenTime(openTime);
        z.setRecommended(recommended);
        z.setSortOrder(sortOrder);
        return z;
    }

    private static int chars(String s) {
        return s.codePointCount(0, s.length());
    }
}
