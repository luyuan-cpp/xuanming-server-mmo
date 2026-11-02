package com.game.gateway.entity;

import jakarta.persistence.*;
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

    public LocalDateTime getCreatedAt() { return createdAt; }
    public LocalDateTime getUpdatedAt() { return updatedAt; }
}
