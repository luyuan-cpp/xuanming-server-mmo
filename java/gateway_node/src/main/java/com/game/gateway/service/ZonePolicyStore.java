package com.game.gateway.service;

import java.util.Optional;

/**
 * 区服策略所在的键值存储(生产是 etcd,{@code etcd.EtcdZonePolicyStore})。单独成接口只为让 {@link ZonePolicyPublisher}
 * 的「比较再写」能在单测里用内存实现验证;失败一律抛 RuntimeException,由发布器按区记账、下一拍重试。
 */
public interface ZonePolicyStore {

    /** 读一个键的当前值;键不存在回 empty。 */
    Optional<byte[]> get(String key);

    /** 写一个键(不挂租约:策略是持久配置,不随网关进程退出而消失)。 */
    void put(String key, byte[] value);
}
