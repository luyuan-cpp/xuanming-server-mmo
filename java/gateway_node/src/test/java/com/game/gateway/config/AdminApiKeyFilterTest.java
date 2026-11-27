package com.game.gateway.config;

import org.junit.jupiter.api.Test;
import org.springframework.mock.web.MockFilterChain;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 管理面口令闸(热关停管理接口挂在它后面,docs/design/rpc-kill-switch.md §3;
 * 操作人与机器口令见 docs/design/admin-identity-and-dual-control.md §8 批 6)。
 */
class AdminApiKeyFilterTest {

    private static final String KEY = "0123456789abcdef0123456789abcdef";
    private static final String MACHINE = "fedcba9876543210fedcba9876543210";

    private static MockHttpServletRequest request(String method, String uri, String servletPath, String key, String... operators) {
        MockHttpServletRequest request = new MockHttpServletRequest(method, uri);
        request.setServletPath(servletPath);
        if (key != null) {
            request.addHeader("X-Admin-Key", key);
        }
        for (String operator : operators) {
            request.addHeader("X-Admin-Operator", operator);
        }
        return request;
    }

    private static MockHttpServletRequest request(String uri, String servletPath, String key) {
        return request("PUT", uri, servletPath, key, "ops.alice");
    }

    /** 放行返回 0(请求走到了链的下一环),拒绝时返回响应状态。 */
    private static int outcome(AdminApiKeyFilter filter, MockHttpServletRequest request) throws Exception {
        MockHttpServletResponse response = new MockHttpServletResponse();
        MockFilterChain chain = new MockFilterChain();
        filter.doFilter(request, response, chain);
        return chain.getRequest() != null ? 0 : response.getStatus();
    }

    /** 放行返回 true,拒绝时断言 401。 */
    private static boolean passes(AdminApiKeyFilter filter, MockHttpServletRequest request) throws Exception {
        int status = outcome(filter, request);
        if (status == 0) {
            return true;
        }
        assertEquals(401, status);
        return false;
    }

    @Test
    void adminPathsNeedTheConfiguredKey() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter(KEY);
        assertTrue(passes(filter, request("/admin/killswitch", "/admin/killswitch", KEY)));
        assertFalse(passes(filter, request("/admin/killswitch", "/admin/killswitch", null)));
        assertFalse(passes(filter, request("/admin/killswitch", "/admin/killswitch", KEY + "x")));
        assertFalse(passes(filter, request("/admin/killswitch", "/admin/killswitch", "")));
    }

    @Test
    void nonAdminPathsPassWithoutAKey() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter(KEY);
        assertTrue(passes(filter, request("/api/server-list", "/api/server-list", null)));
        assertTrue(passes(filter, request("/administrator", "/administrator", null)));
    }

    @Test
    void pathParametersAndDoubledSlashesDoNotBypassTheKey() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter(KEY);
        // 原始 URI 不以 /admin/ 开头,但容器与 Spring 都会把它当成 /admin/killswitch。
        assertFalse(passes(filter, request("/admin;x=1/killswitch", "/admin/killswitch", null)));
        assertFalse(passes(filter, request("//admin/killswitch", "/admin/killswitch", null)));
        assertFalse(passes(filter, request("/ADMIN/killswitch", "/ADMIN/killswitch", null)));
    }

    @Test
    void blankConfiguredKeyRejectsEveryAdminRequest() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter("  ");
        assertFalse(passes(filter, request("/admin/killswitch", "/admin/killswitch", "  ")));
        assertFalse(passes(filter, request("/admin/killswitch", "/admin/killswitch", "")));
        assertTrue(passes(filter, request("/api/login", "/api/login", null)));
    }

    @Test
    void writesNeedExactlyOneValidOperatorButReadsDoNot() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter(KEY);
        assertEquals(0, outcome(filter, request("GET", "/admin/zones", "/admin/zones", KEY)));
        for (String method : new String[] {"POST", "PUT", "PATCH", "DELETE"}) {
            assertEquals(400, outcome(filter, request(method, "/admin/zones/1", "/admin/zones/1", KEY)), method);
            assertEquals(0, outcome(filter, request(method, "/admin/zones/1", "/admin/zones/1", KEY, "alice@corp")), method);
        }
        assertEquals(400, outcome(filter, request("POST", "/admin/zones", "/admin/zones", KEY, "alice", "bob")), "两个操作人头");
        assertEquals(400, outcome(filter, request("POST", "/admin/zones", "/admin/zones", KEY, "alice smith")), "字符集之外");
        assertEquals(400, outcome(filter, request("POST", "/admin/zones", "/admin/zones", KEY, "a".repeat(65))), "超长");
        assertEquals(400, outcome(filter, request("POST", "/admin/tables/x", "/admin/tables/x", KEY, "table-bundle")),
                "人用的口令不能冒用机器操作人");
    }

    @Test
    void theMachineKeyOnlyReachesTableBundlesAsTableBundle() throws Exception {
        AdminApiKeyFilter filter = new AdminApiKeyFilter(KEY, MACHINE);
        assertEquals(0, outcome(filter, request("PUT", "/admin/tables/pointer", "/admin/tables/pointer", MACHINE, "table-bundle")));
        assertEquals(0, outcome(filter, request("GET", "/admin/tables/pointer", "/admin/tables/pointer", MACHINE)));
        assertEquals(400, outcome(filter, request("PUT", "/admin/tables/pointer", "/admin/tables/pointer", MACHINE, "alice")));
        assertEquals(400, outcome(filter, request("PUT", "/admin/tables/pointer", "/admin/tables/pointer", MACHINE)));
        assertEquals(403, outcome(filter, request("PUT", "/admin/killswitch/x", "/admin/killswitch/x", MACHINE, "table-bundle")));
        assertEquals(403, outcome(filter, request("GET", "/admin/zones", "/admin/zones", MACHINE)));
        assertEquals(403, outcome(filter, request("PUT", "/admin/tables/../killswitch/x", "/admin/killswitch/x", MACHINE, "table-bundle")),
                "原始 URI 与规范化路径都要落在 /admin/tables 下");
        assertEquals(403, outcome(filter, request("PUT", "/admin/tablesx", "/admin/tablesx", MACHINE, "table-bundle")));
        // 不配机器口令就没有这条路。
        assertEquals(401, outcome(new AdminApiKeyFilter(KEY), request("PUT", "/admin/tables/pointer", "/admin/tables/pointer", MACHINE, "table-bundle")));
    }

    @Test
    void theMachineKeyMustBeLongAndDifferentFromTheHumanKey() {
        assertThrows(IllegalStateException.class, () -> new AdminApiKeyFilter(KEY, KEY));
        assertThrows(IllegalStateException.class, () -> new AdminApiKeyFilter(KEY, "short-machine-key"));
        assertDoesNotThrow(() -> new AdminApiKeyFilter(KEY, ""));
    }
}
