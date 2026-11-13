package com.game.gateway.config;

import org.junit.jupiter.api.Test;
import org.springframework.mock.web.MockFilterChain;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;

import static org.junit.jupiter.api.Assertions.*;

/** 管理面口令闸(热关停管理接口挂在它后面,docs/design/rpc-kill-switch.md §3)。 */
class AdminApiKeyFilterTest {

    private static final String KEY = "0123456789abcdef0123456789abcdef";

    private static MockHttpServletRequest request(String uri, String servletPath, String key) {
        MockHttpServletRequest request = new MockHttpServletRequest("PUT", uri);
        request.setServletPath(servletPath);
        if (key != null) {
            request.addHeader("X-Admin-Key", key);
        }
        return request;
    }

    /** 放行返回 true(请求走到了链的下一环),拒绝时断言 401。 */
    private static boolean passes(AdminApiKeyFilter filter, MockHttpServletRequest request) throws Exception {
        MockHttpServletResponse response = new MockHttpServletResponse();
        MockFilterChain chain = new MockFilterChain();
        filter.doFilter(request, response, chain);
        if (chain.getRequest() != null) {
            return true;
        }
        assertEquals(401, response.getStatus());
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
}
