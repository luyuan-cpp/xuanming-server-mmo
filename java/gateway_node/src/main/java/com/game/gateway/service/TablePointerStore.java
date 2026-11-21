package com.game.gateway.service;

import java.util.Map;
import java.util.Optional;

/**
 * 配置表热更轨道指针与实例回报的键值存储(生产是 etcd,{@code etcd.EtcdTablePointerStore};
 * docs/design/config-table-hot-reload.md §6.1、§6.4)。
 * 写操作基于 etcd {@code mod_revision} 做比较并写(CAS),防止两个运维同时发布时互相覆盖。
 */
public interface TablePointerStore {

    /** 单条 etcd 键值及其 mod_revision(不存在时用 0 表示)。 */
    record Entry(String value, long modRevision) {
    }

    /** 读单个键;不存在返回 empty。 */
    Optional<Entry> get(String key);

    /** 前缀下的全部键值(完整键 -> Entry),按键排序。 */
    Map<String, Entry> list(String prefix);

    /**
     * 比较并写:仅当 key 的当前 mod_revision == expectedModRevision(0 表示键必须不存在)时写入 value 并返回 true;
     * 并发写入导致版本漂移时返回 false,不改写原值。
     */
    boolean compareAndPut(String key, long expectedModRevision, String value);

    /**
     * 比较并删:仅当 key 的当前 mod_revision == expectedModRevision 时删除并返回 true;
     * 并发修改导致版本漂移时返回 false。
     */
    boolean compareAndDelete(String key, long expectedModRevision);
}
