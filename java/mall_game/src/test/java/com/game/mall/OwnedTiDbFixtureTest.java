package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;

import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.NullSource;
import org.junit.jupiter.params.provider.ValueSource;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import java.nio.file.Path;
import java.io.IOException;
import java.util.Map;
import com.fasterxml.jackson.databind.ObjectMapper;

/** 只解析端点，不连接SQL；真实运行本轮只允许root已核归属的127.0.0.1:24000。 */
class OwnedTiDbFixtureTest {
    @ParameterizedTest
    @ValueSource(strings = {"jdbc:mysql://127.0.0.1:24000", "jdbc:mysql://127.0.0.1:24000/", "jdbc:mysql://[::1]:24000/"})
    void acceptsNumericLoopbackWithExplicitPort(String endpoint) {
        assertTrue(OwnedTiDbFixture.endpoint(endpoint).endsWith("/"));
    }

    @ParameterizedTest
    @NullSource
    @ValueSource(strings = {"", "jdbc:mysql://localhost:24000/", "jdbc:mysql://10.0.0.1:24000/",
        "jdbc:mysql://127.0.0.1:0/", "jdbc:mysql://127.0.0.1:65536/", "jdbc:mysql://127.0.0.1/",
        "jdbc:mysql://127.0.0.1:24000/existing", "jdbc:mysql://127.0.0.1:24000/?tidb_snapshot=x",
        "jdbc:mysql://user:password@127.0.0.1:24000/", "jdbc:mysql://127.0.0.1:24000/#fragment"})
    void rejectsForeignOrImplicitTargets(String endpoint) {
        assertThrows(IllegalArgumentException.class, () -> OwnedTiDbFixture.endpoint(endpoint));
    }

    @Test void privateReferencesCannotEscapeThisNewOwner(@TempDir Path root) throws Exception {
        assertEquals(root.resolve("boot/input/request.json"), OwnedTiDbFixture.privatePath(root, root.resolve("boot/input/request.json").toString()));
        assertThrows(IOException.class, () -> OwnedTiDbFixture.privatePath(root, root.toString()));
        assertThrows(IOException.class, () -> OwnedTiDbFixture.privatePath(root, root.resolve("../other-owner/owner.json").toString()));
        assertThrows(IOException.class, () -> OwnedTiDbFixture.privatePath(root, root.resolveSibling(root.getFileName() + "-other").resolve("owner.json").toString()));
    }

    @Test void secondPoolCanOnlyInjectSessionVariablesIntoTheSameOwnedSchema() throws Exception {
        String url = "jdbc:mysql://127.0.0.1:24000/mall_it_0123456789abcdef0123456789abcdef";
        OwnedTiDbFixture.requireSameSchemaUrl(url, url);
        OwnedTiDbFixture.requireSameSchemaUrl(url, url + "?sessionVariables=tidb_read_consistency%3D%27weak%27");
        for (String candidate : new String[]{url + "_other", url.replace(":24000/", ":3306/"),
                url + "?user=root", url + "?allowMultiQueries=true", url + "?sessionVariables=x&user=root"}) {
            assertThrows(IOException.class, () -> OwnedTiDbFixture.requireSameSchemaUrl(url, candidate));
        }
    }

    @Test void normalContextCannotInjectSchemaCredentialsOrReplaceTheTrustedHelper() throws Exception {
        var environment = new java.util.LinkedHashMap<>(Map.of(
                "MMORPG_SQL_WRITER_GRANT_FILE", "new-owner/grant.json",
                "MMORPG_SQL_WRITER_GRANT_SHA256", "a".repeat(64),
                "MMORPG_SQL_WRITER_CONSUME_DIR", "new-owner/consume",
                "MMORPG_SQL_DEPLOYMENT_ID", "fixture-only-deployment",
                "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256", "b".repeat(64)));
        var mapper = new ObjectMapper();
        assertEquals(5, OwnedTiDbFixture.normalEnvironment(mapper.valueToTree(Map.of("environment", environment))).size());
        for (String forbidden : new String[]{"MMORPG_SQL_SCHEMA_DSN", "MMORPG_SQL_REGISTRAR_DSN", "MMORPG_SQL_PRIVATE_FILE_HELPER"}) {
            environment.put(forbidden, "test-only-no-credential");
            assertThrows(IOException.class, () -> OwnedTiDbFixture.normalEnvironment(mapper.valueToTree(Map.of("environment", environment))));
            environment.remove(forbidden);
        }
        environment.remove("MMORPG_SQL_WRITER_GRANT_SHA256");
        assertThrows(IOException.class, () -> OwnedTiDbFixture.normalEnvironment(mapper.valueToTree(Map.of("environment", environment))));
    }

    @Test void defaultProductionFactoryDoesNotConsumeAGrantWhileBeingWired() {
        // 只构造工厂，不创建连接；缺资格的正常入口仍留给WriterAuthority统一拒绝。
        assertNotNull(new DatabaseConfiguration().normalDataSourceFactory());
    }
}
