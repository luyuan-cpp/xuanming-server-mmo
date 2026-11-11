package com.game.gateway.service;

import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;
import com.game.gateway.entity.ZoneConfig;

/**
 * 发布给本区 login 的区服策略:etcd 里 {@link ZonePolicyPublisher#KEY_PREFIX}{@code <zoneId>} 的值(JSON)。
 *
 * <p>字段名是与 Go 的契约:go/login/internal/logic/pkg/zonepolicy 的 Policy。金样
 * go/login/internal/logic/pkg/zonepolicy/testdata/policy.golden.json 两边的单测都核对。只做加法演进,不改名、不改类型。
 *
 * @param capacity         区服目录的 capacity 列:本区同时在线的准入上限;库里出现负数(手改)时按 0(目录没给)发布,
 *                         免得 login 解析失败、一直用旧值
 * @param createRoleClosed 区服目录的 create_role_closed 列:本区暂停创建新角色
 */
@JsonPropertyOrder({"capacity", "createRoleClosed"})
public record ZonePolicy(@JsonProperty("capacity") long capacity,
                         @JsonProperty("createRoleClosed") boolean createRoleClosed) {

    static ZonePolicy of(ZoneConfig zone) {
        return new ZonePolicy(Math.max(zone.getCapacity(), 0), zone.isCreateRoleClosed());
    }
}
