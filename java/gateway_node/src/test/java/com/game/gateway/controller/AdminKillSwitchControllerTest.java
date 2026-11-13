package com.game.gateway.controller;

import com.game.gateway.service.KillSwitchRuleStore;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.http.HttpStatus;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.web.server.ResponseStatusException;

import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 热关停管理接口的契约测试(docs/design/rpc-kill-switch.md §3、§6):键怎么拼、什么会被拒、列表怎么解读。
 * 存储用内存实现;etcd 客户端行为不在这里验。
 */
class AdminKillSwitchControllerTest {

    private static final String SCENE_UUID = "0f8fad5b-d9cb-469f-a165-70867728950e";

    /** 内存版存储:键值放 TreeMap,发现里的节点按类型给。 */
    static final class MemoryStore implements KillSwitchRuleStore {
        final Map<String, String> kv = new TreeMap<>();
        final Map<String, Set<String>> nodes = new HashMap<>();
        boolean failing;

        @Override
        public Map<String, String> list(String prefix) {
            check();
            Map<String, String> result = new TreeMap<>();
            kv.forEach((k, v) -> {
                if (k.startsWith(prefix)) {
                    result.put(k, v);
                }
            });
            return result;
        }

        @Override
        public void put(String key, String value) {
            check();
            kv.put(key, value);
        }

        @Override
        public long delete(String key) {
            check();
            return kv.remove(key) == null ? 0 : 1;
        }

        @Override
        public Set<String> registeredNodeUuids(String nodeType) {
            check();
            return nodes.getOrDefault(nodeType, Set.of());
        }

        private void check() {
            if (failing) {
                throw new IllegalStateException("etcd down");
            }
        }
    }

    private MemoryStore store;
    private AdminKillSwitchController controller;
    private MockHttpServletRequest request;

    @BeforeEach
    void setUp() {
        store = new MemoryStore();
        store.nodes.put("SceneNodeService", Set.of(SCENE_UUID));
        controller = new AdminKillSwitchController(store);
        request = new MockHttpServletRequest();
        request.addHeader(AdminKillSwitchController.OPERATOR_HEADER, "ops.alice");
    }

    private static AdminKillSwitchController.RuleRequest rule(String target, String nodeType, String instance,
                                                              Boolean deny, String reason, Integer code) {
        return new AdminKillSwitchController.RuleRequest(target, nodeType, instance, deny, reason, code);
    }

    private static HttpStatus statusOf(Runnable action) {
        ResponseStatusException e = assertThrows(ResponseStatusException.class, action::run);
        return HttpStatus.valueOf(e.getStatusCode().value());
    }

    @Test
    void globalMethodRuleIsWrittenUnderThePrefixWithCanonicalJson() {
        var view = controller.put(rule("ClientPlayerChat/SendChat", null, null, true, "chat storm", null), request);
        assertEquals("ClientPlayerChat/SendChat", view.key());
        assertEquals("all", view.scope());
        assertEquals("{\"deny\":true,\"reason\":\"chat storm\"}", store.kv.get("/mmorpg/killswitch/ClientPlayerChat/SendChat"));
    }

    @Test
    void nodeTypeScopeRequiresARegisteredNodeOfThatType() {
        var view = controller.put(rule("*", "SceneNodeService", null, true, null, 9), request);
        assertEquals("type/SceneNodeService/*", view.key());
        assertEquals("{\"deny\":true,\"code\":9}", store.kv.get("/mmorpg/killswitch/type/SceneNodeService/*"));

        // 类型名写错(或这一类节点根本没部署)时拒绝,避免规则静默不生效。
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("*", "SceenNodeService", null, true, null, null), request)));
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("*", "scene", null, true, null, null), request)));
    }

    @Test
    void instanceScopeIsCheckedAgainstDiscovery() {
        var view = controller.put(rule("SceneSkillClientPlayer/ReleaseSkill", "SceneNodeService",
                SCENE_UUID.toUpperCase(), true, null, null), request);
        assertEquals("instance/" + SCENE_UUID + "/SceneSkillClientPlayer/ReleaseSkill", view.key());
        assertTrue(store.kv.containsKey("/mmorpg/killswitch/instance/" + SCENE_UUID + "/SceneSkillClientPlayer/ReleaseSkill"));

        // 没给类型、uuid 不在线、uuid 形状不对:都拒绝。
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("*", null, SCENE_UUID, true, null, null), request)));
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(
                rule("*", "SceneNodeService", "11111111-2222-3333-4444-555555555555", true, null, null), request)));
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("*", "SceneNodeService", "node-3", true, null, null), request)));
    }

    @Test
    void malformedRequestsAreRejectedWithoutWriting() {
        for (String bad : List.of("", "Svc", "Svc/", "/", "a/b/c", "type/SceneNodeService/*", "Svc/Get*", "Svc/Method with space")) {
            assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule(bad, null, null, true, null, null), request)), bad);
        }
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("Svc/M", null, null, null, null, null), request)));
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("Svc/M", null, null, true, "line\nbreak", null), request)));
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.put(rule("Svc/M", null, null, true, null, 17), request)));
        assertTrue(store.kv.isEmpty(), "nothing is written for a rejected request");
    }

    @Test
    void listParsesScopesValuesAndMarksMalformedEntries() {
        store.kv.put("/mmorpg/killswitch/ClientPlayerFriend/*", "true");
        store.kv.put("/mmorpg/killswitch/type/SceneNodeService/Scene/CreateScene", "{\"deny\":false}");
        store.kv.put("/mmorpg/killswitch/instance/" + SCENE_UUID + "/*", "{\"deny\":true,\"code\":9,\"reason\":\"probe\"}");
        store.kv.put("/mmorpg/killswitch/ClientPlayerChat/SendChat", "{\"deny\":tru");
        store.kv.put("/mmorpg/killswitch/type/SceneNodeService", "true");

        Map<String, AdminKillSwitchController.RuleView> byKey = new HashMap<>();
        controller.list().forEach(v -> byKey.put(v.key(), v));

        assertEquals("all", byKey.get("ClientPlayerFriend/*").scope());
        assertTrue(byKey.get("ClientPlayerFriend/*").deny());
        var typeRule = byKey.get("type/SceneNodeService/Scene/CreateScene");
        assertEquals("type", typeRule.scope());
        assertEquals("SceneNodeService", typeRule.scopeValue());
        assertEquals("Scene/CreateScene", typeRule.target());
        assertFalse(typeRule.deny());
        var instanceRule = byKey.get("instance/" + SCENE_UUID + "/*");
        assertEquals(9, instanceRule.code());
        assertEquals("probe", instanceRule.reason());
        assertFalse(byKey.get("ClientPlayerChat/SendChat").valid(), "a malformed value is listed as invalid");
        assertEquals("malformed", byKey.get("type/SceneNodeService").scope());
    }

    @Test
    void deleteRemovesTheExactKeyAndIsIdempotent() {
        store.kv.put("/mmorpg/killswitch/type/SceneNodeService/*", "true");
        assertEquals(1, controller.delete("/type/SceneNodeService/*", request).deleted());
        assertEquals(0, controller.delete("type/SceneNodeService/*", request).deleted());
        assertEquals(HttpStatus.BAD_REQUEST, statusOf(() -> controller.delete("bad key", request)));
    }

    @Test
    void etcdFailureIsServiceUnavailable() {
        store.failing = true;
        assertEquals(HttpStatus.SERVICE_UNAVAILABLE, statusOf(() -> controller.list()));
        assertEquals(HttpStatus.SERVICE_UNAVAILABLE, statusOf(() -> controller.put(rule("*", null, null, true, null, null), request)));
    }

    @Test
    void ruleValueParsingMatchesTheNodes() {
        assertTrue(AdminKillSwitchController.parseRule("").isPresent());
        assertTrue(AdminKillSwitchController.parseRule("DENY").orElseThrow().deny());
        assertFalse(AdminKillSwitchController.parseRule("allow").orElseThrow().deny());
        assertTrue(AdminKillSwitchController.parseRule("maybe").isEmpty());
        assertTrue(AdminKillSwitchController.parseRule("{\"deny\":\"yes\"}").isEmpty());
        assertTrue(AdminKillSwitchController.parseRule("{\"code\":-1}").isEmpty());
        assertTrue(AdminKillSwitchController.parseRule("{\"deny\":true}}").isEmpty());
    }
}
