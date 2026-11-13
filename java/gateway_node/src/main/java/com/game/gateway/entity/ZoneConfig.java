package com.game.gateway.entity;

import com.fasterxml.jackson.annotation.JsonIgnore;
import jakarta.persistence.*;
import org.hibernate.annotations.ColumnDefault;

import java.time.LocalDateTime;

@Entity
@Table(name = "zone_config")
public class ZoneConfig {

    @Id
    @Column(name = "zone_id")
    private Long zoneId;

    @Column(name = "name", nullable = false, length = 64)
    private String name;

    /**
     * 0=OPEN, 1=MAINTENANCE, 2=CLOSED, 3=PREVIEW
     */
    @Column(name = "manual_status", nullable = false)
    private int manualStatus;

    @Column(name = "capacity")
    private int capacity = 45000;

    @Column(name = "maintenance_msg", length = 256)
    private String maintenanceMsg = "";

    @Column(name = "open_time")
    private LocalDateTime openTime;

    @Column(name = "recommended")
    private boolean recommended;

    @Column(name = "sort_order")
    private int sortOrder;

    /**
     * 本区暂停创建新角色(2026-11-10,区服满员处理):已有角色照常登录进场,login 只拦 CreatePlayer。运维开关,只经
     * {@code POST /admin/zones/{id}/create-role/close|open} 改;POST 新建区服时取库缺省(开着),PUT 整行替换不碰它。
     * 本区网关的 ZonePolicyPublisher 把它与 capacity 一起发布给本区 login。
     *
     * <p>{@code @ColumnDefault}:原生 INSERT(ZoneConfigRepository.INSERT_IF_ABSENT_SQL)不写这一列,靠库缺省;
     * 测试库由 Hibernate 按实体建表(create-drop),不写缺省就是 NOT NULL 无缺省,插入会失败。生产 DDL 见 schema.sql。
     */
    @Column(name = "create_role_closed", nullable = false)
    @ColumnDefault("false")
    private boolean createRoleClosed;

    /**
     * 本区角色上限(2026-11-12,区服满员处理 §6):本区一共建过的角色数(data_service 按合服之后的最终区数,删角不减)
     * 到了这个数,本区网关的 RoleCapacityGuard 就把 {@link #createRoleClosed} 关上。0 = 不设上限。软上限:到线后
     * 至多一个检查周期加一次策略发布内建出的角色会超出,见 docs/design/zone-full-handling.md §6。
     * 运维开关,只经 {@code PUT /admin/zones/{id}/role-capacity} 改;POST 新建区服时取库缺省(0),PUT 整行替换不碰它。
     * {@code @ColumnDefault} 的理由同 {@link #createRoleClosed}。
     */
    @Column(name = "role_capacity", nullable = false)
    @ColumnDefault("0")
    private int roleCapacity;

    /**
     * 行版本(2026-11-12,区服目录乐观并发):这一行每被写一次加一,{@code GET /admin/zones/{id}} 拿它作 ETag,整行替换的
     * {@code PUT} 必须带 If-Match,比较与写入在同一条条件 UPDATE 里(ZoneConfigRepository#updateSettings)。新行的起始值
     * 见 AdminZoneController#create;种子行不写这一列,取库缺省 1。
     *
     * <ul>
     *   <li>只经 ZoneConfigRepository 的写语句改。不用 JPA 的 {@code @Version}:写入全是显式语句,{@code @Version} 只在实体
     *       flush 时生效,这里一次也用不上,还会改掉仓库类注释里「merge 判不出新旧」那段论证的前提。</li>
     *   <li>不进 JSON:版本只经 ETag / If-Match 两个头往返。放进响应体会诱导调用方自己拼 If-Match,或以为 PUT 体里带的
     *       version 会被采用。</li>
     *   <li>{@code @ColumnDefault} 与 schema.sql 一致,测试库按实体建表时也有这个缺省。</li>
     * </ul>
     */
    @JsonIgnore
    @Column(name = "version", nullable = false)
    @ColumnDefault("1")
    private long version = 1;

    @Column(name = "created_at", updatable = false)
    private LocalDateTime createdAt;

    @Column(name = "updated_at")
    private LocalDateTime updatedAt;

    @PrePersist
    protected void onCreate() {
        createdAt = LocalDateTime.now();
        updatedAt = createdAt;
    }

    @PreUpdate
    protected void onUpdate() {
        updatedAt = LocalDateTime.now();
    }

    // --- getters/setters ---

    public Long getZoneId() { return zoneId; }
    public void setZoneId(Long zoneId) { this.zoneId = zoneId; }

    public String getName() { return name; }
    public void setName(String name) { this.name = name; }

    public int getManualStatus() { return manualStatus; }
    public void setManualStatus(int manualStatus) { this.manualStatus = manualStatus; }

    public int getCapacity() { return capacity; }
    public void setCapacity(int capacity) { this.capacity = capacity; }

    public String getMaintenanceMsg() { return maintenanceMsg; }
    public void setMaintenanceMsg(String maintenanceMsg) { this.maintenanceMsg = maintenanceMsg; }

    public LocalDateTime getOpenTime() { return openTime; }
    public void setOpenTime(LocalDateTime openTime) { this.openTime = openTime; }

    public boolean isRecommended() { return recommended; }
    public void setRecommended(boolean recommended) { this.recommended = recommended; }

    public int getSortOrder() { return sortOrder; }
    public void setSortOrder(int sortOrder) { this.sortOrder = sortOrder; }

    public boolean isCreateRoleClosed() { return createRoleClosed; }
    public void setCreateRoleClosed(boolean createRoleClosed) { this.createRoleClosed = createRoleClosed; }

    public int getRoleCapacity() { return roleCapacity; }
    public void setRoleCapacity(int roleCapacity) { this.roleCapacity = roleCapacity; }

    public long getVersion() { return version; }
    public void setVersion(long version) { this.version = version; }

    public LocalDateTime getCreatedAt() { return createdAt; }
    public LocalDateTime getUpdatedAt() { return updatedAt; }
}
