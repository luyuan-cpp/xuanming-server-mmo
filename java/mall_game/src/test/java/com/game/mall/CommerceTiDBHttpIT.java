package com.game.mall;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Import;
import org.springframework.context.annotation.Primary;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/** 真TiDB继承全部HTTP业务断言；没有正规Schema/physical/I3/NORMAL前置即失败，不降级H2。 */
@DirtiesContext(classMode = DirtiesContext.ClassMode.AFTER_CLASS)
@Import(CommerceTiDBHttpIT.NormalConfiguration.class)
class CommerceTiDBHttpIT extends CommerceHttpFlowTest {
    @DynamicPropertySource
    static synchronized void realDatabase(DynamicPropertyRegistry registry) throws java.io.IOException {
        OwnedTiDbFixture.Owner owner = OwnedTiDbFixture.prepareHttp();
        String url = owner.jdbcUrl(), user = owner.username(), password = owner.password();
        registry.add("test.mall.datasource", () -> "owned-normal");
        registry.add("spring.datasource.url", () -> url);
        registry.add("spring.datasource.username", () -> user);
        registry.add("spring.datasource.password", () -> password);
        // provider已按原schema.sql安装；Spring的NORMAL连接没有DDL权限。
        registry.add("spring.sql.init.mode", () -> "never");
        registry.add("commerce.run-mode", () -> "prod");
        registry.add("commerce.require-tidb", () -> "true");
        registry.add("commerce.fixture.normal-context-file", owner::contextFile);
        registry.add("commerce.fixture.normal-context-sha256", owner::contextSha256);
    }

    @TestConfiguration(proxyBeanMethods = false)
    @ConditionalOnProperty(name = "test.mall.datasource", havingValue = "owned-normal")
    static class NormalConfiguration {
        @Bean @Primary
        DatabaseConfiguration.DataSourceFactory ownedNormalDataSourceFactory(
                @Value("${commerce.fixture.normal-context-file}") String contextFile,
                @Value("${commerce.fixture.normal-context-sha256}") String contextSha) {
            return (source, props) -> OwnedTiDbFixture.openHttpDataSource(source, props, contextFile, contextSha);
        }
    }
}