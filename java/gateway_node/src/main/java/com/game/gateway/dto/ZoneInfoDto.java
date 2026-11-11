package com.game.gateway.dto;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class ZoneInfoDto {

    private long zoneId;
    private String name;
    private ZoneDisplayStatus status;
    private LoadLevel loadLevel;
    private String maintenanceMsg;
    private Long openTime;       // epoch seconds, null if not PREVIEW
    private boolean isNew;
    private boolean recommended;
    /**
     * 本区暂停创建新角色(2026-11-10,JSON {@code create_role_closed}):已有角色照常进,新角色只能建在别的区。
     * 客户端在区服列表上标「暂停创角」;旧客户端不认识这个字段,照旧可选,建角时由 login 拒绝(tip kLoginZoneCreateRoleClosed)。
     */
    private boolean createRoleClosed;

    public long getZoneId() { return zoneId; }
    public void setZoneId(long zoneId) { this.zoneId = zoneId; }

    public String getName() { return name; }
    public void setName(String name) { this.name = name; }

    public ZoneDisplayStatus getStatus() { return status; }
    public void setStatus(ZoneDisplayStatus status) { this.status = status; }

    public LoadLevel getLoadLevel() { return loadLevel; }
    public void setLoadLevel(LoadLevel loadLevel) { this.loadLevel = loadLevel; }

    public String getMaintenanceMsg() { return maintenanceMsg; }
    public void setMaintenanceMsg(String maintenanceMsg) { this.maintenanceMsg = maintenanceMsg; }

    public Long getOpenTime() { return openTime; }
    public void setOpenTime(Long openTime) { this.openTime = openTime; }

    // Jackson 会把 boolean getter isNew() 的属性名剥成 "new",SNAKE_CASE 后
    // 仍是 "new";客户端字段只能叫 is_new(new 是 C# 关键字),必须显式标注。
    @JsonProperty("is_new")
    public boolean isNew() { return isNew; }
    public void setNew(boolean isNew) { this.isNew = isNew; }

    public boolean isRecommended() { return recommended; }
    public void setRecommended(boolean recommended) { this.recommended = recommended; }

    public boolean isCreateRoleClosed() { return createRoleClosed; }
    public void setCreateRoleClosed(boolean createRoleClosed) { this.createRoleClosed = createRoleClosed; }
}
