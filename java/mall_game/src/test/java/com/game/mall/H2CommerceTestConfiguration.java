package com.game.mall;

import com.zaxxer.hikari.HikariDataSource;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Primary;

/**
 * HTTP行为合同显式替换外部SQL适配；只在测试类路径存在，不给生产增加raw/H2旁路。
 * 真TiDB HTTP用例必须选择owned-normal，继承本配置也不会得到H2回退。
 */
@TestConfiguration(proxyBeanMethods = false)
@ConditionalOnProperty(name = "test.mall.datasource", havingValue = "h2", matchIfMissing = true)
class H2CommerceTestConfiguration {
    @Bean
    @Primary
    DatabaseConfiguration.DataSourceFactory h2CommerceDataSourceFactory() {
        return (source, props) -> {
            if (!"test".equals(props.getRunMode()) || props.isRequireTidb()
                    || source.getUrl() == null || !source.getUrl().startsWith("jdbc:h2:mem:")) {
                throw new IllegalStateException("H2 HTTP夹具只接受test模式的显式内存库，不能代替真实TiDB NORMAL");
            }
            // 池大小、事务隔离与超时仍取同一配置事实源；这里只替换physical适配。
            return new HikariDataSource(new DatabaseConfiguration().poolConfig(source, props));
        };
    }
}
