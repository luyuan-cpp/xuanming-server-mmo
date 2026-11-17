package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import org.apache.shiro.authz.UnauthorizedException;
import org.apache.shiro.subject.Subject;
import org.apache.shiro.util.ThreadContext;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

class GmControllerTest {
    private final ObjectMapper json = new ObjectMapper();
    private final Subject subject = mock(Subject.class);
    private final GameServices game = mock(GameServices.class);
    private final GmCommandJournal journal = mock(GmCommandJournal.class);
    private GmController controller;
    public static final class Principal { public String getUsername() { return "gm-operator"; } }
    @BeforeEach void setup() {
        ThreadContext.bind(subject);
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.getPrincipal()).thenReturn(new Principal());
        controller = new GmController(json, game, journal, "http://127.0.0.1:8081", "", "127.0.0.1:50900", "x".repeat(32));
    }
    @AfterEach void clear() { ThreadContext.unbindSubject(); }
    @Test void anonymousCannotReadOrWrite() {
        when(subject.isAuthenticated()).thenReturn(false);
        assertEquals(401, assertThrows(ResponseStatusException.class, () -> controller.player("123")).getStatusCode().value());
        assertEquals(401, assertThrows(ResponseStatusException.class, () -> controller.send("123")).getStatusCode().value());
        verifyNoInteractions(game, journal);
    }
    @Test void permissionIsCheckedBeforeGameCall() {
        doThrow(new UnauthorizedException()).when(subject).checkPermission("gm:mail:send");
        assertThrows(UnauthorizedException.class, () -> controller.send("123"));
        verifyNoInteractions(game, journal);
    }
    @Test void frozenDraftTakesActorFromSessionAndKeepsIntegerPrecision() throws Exception {
        when(game.player(anyString())).thenReturn(json.createObjectNode().put("found", true));
        when(journal.prepare(anyString(), anyString(), anyString(), anyString(), any())).thenReturn(json.createObjectNode().put("id", "42"));
        controller.prepare(draft());
        verify(journal).prepare(eq("gm-operator"), anyString(), eq("9223372036854775807"), eq("测试工单"), argThat(n ->
                n.path("attachments").path("currencies").get(0).path("amount").asText().equals("9223372036854775807")
                && !n.has("expireMs") && !n.has("requestId")));
    }
    @Test void duplicateFieldsAndSpoofedActorAreRejected() {
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"title\":", "\"title\":\"x\",\"title\":")));
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"body\":", "\"operator\":\"admin\",\"body\":")));
        verifyNoInteractions(game, journal);
    }
    @Test void unsafeNumbersAndUnknownFieldsAreRejected() {
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"currencyAmount\":\"9223372036854775807\"", "\"currencyAmount\":9223372036854775807")));
        assertThrows(ResponseStatusException.class, () -> controller.prepare(draft().replace("\"validDays\":7", "\"validDays\":31")));
        verifyNoInteractions(game, journal);
    }
    @Test void absentMailConfigurationFailsClosed() {
        var disabled = new GmController(json, game, journal, "http://127.0.0.1:8081", "", "", "");
        assertEquals(503, assertThrows(ResponseStatusException.class, () -> disabled.prepare(draft())).getStatusCode().value());
        verifyNoInteractions(game, journal);
    }
    private String draft() {
        return """
          {"clientRequestId":"05ee173b-67ed-4aef-836a-61180b26869a","playerId":"9223372036854775807",
           "title":"测试发放","body":"领取后到账","reason":"测试工单","currencyType":0,
           "currencyAmount":"9223372036854775807","validDays":7}
          """;
    }
}
