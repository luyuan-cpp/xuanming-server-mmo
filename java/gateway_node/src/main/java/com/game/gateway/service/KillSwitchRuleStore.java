package com.game.gateway.service;

import java.util.Map;
import java.util.Set;

/**
 * 热关停规则所在的键值存储(生产是 etcd,{@code etcd.EtcdKillSwitchRuleStore};docs/design/rpc-kill-switch.md §3)。
 * 单独成接口只为让管理接口的校验与审计能在单测里用内存实现验证;失败一律抛 RuntimeException,管理接口回 503。
 */
public interface KillSwitchRuleStore {

    /** 前缀下的全部键值(完整键 → 原样的值),按键排序。 */
    Map<String, String> list(String prefix);

    /** 写一个键(不挂租约:规则要一直生效到运维删掉它)。 */
    void put(String key, String value);

    /** 删一个键,返回删掉的条数(0 = 本来就没有)。 */
    long delete(String key);

    /** 某一类节点当前在发现里登记的 node_uuid(读 {@code <节点类型>.rpc/} 前缀下的 NodeInfo)。 */
    Set<String> registeredNodeUuids(String nodeType);
}
