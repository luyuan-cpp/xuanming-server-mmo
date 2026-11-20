package com.game.gateway.grpc;

import com.google.protobuf.CodedOutputStream;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.IOException;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 账号注销三条消息与 LoginResponse 字段 7~9 的手写 wire 编解码(docs/design/account-deletion.md §5.1)。
 * 同 {@code LoginRpcClientParseTest}:字段号写错没有编译期报错,只能靠 {@link CodedOutputStream} 手编字节守住。
 *
 * <p>字段号来自 {@code proto/login/login.proto}:AccountDeletionRequest account = 1 … authorization_code = 5;
 * CancelAccountDeletionRequest access_token = 1;AccountDeletionResponse error_message = 1、state = 2、execute_after = 3、
 * blockers = 4;AccountDeletionBlocker player_id = 1、reason = 2;LoginResponse account_deletion_state = 7、
 * account_deletion_execute_after = 8、previous_account_deleted_at = 9;TipInfoMessage id = 1。
 */
class LoginRpcClientAccountDeletionCodecTest {

    @FunctionalInterface
    private interface Fields {
        void writeTo(CodedOutputStream out) throws IOException;
    }

    @Test
    void deletionRequestWritesFields1To5AndOmitsEmptyOnes() throws IOException {
        byte[] want = encode(out -> {
            out.writeString(1, "alice");
            out.writeString(2, "pw");
            out.writeString(3, "apple");
            out.writeString(4, "identity-token");
            out.writeString(5, "authorization-code");
        });

        assertArrayEquals(want, new LoginRpcClient.AccountDeletionRequestProto(
                "alice", "pw", "apple", "identity-token", "authorization-code").toBytes());
        assertArrayEquals(new byte[0],
                new LoginRpcClient.AccountDeletionRequestProto(null, null, null, null, "").toBytes(),
                "proto3: empty strings are not written");
    }

    @Test
    void cancelRequestWritesAccessTokenAsField1() throws IOException {
        assertArrayEquals(encode(out -> out.writeString(1, "access-token")),
                new LoginRpcClient.CancelAccountDeletionRequestProto("access-token").toBytes());
    }

    @Test
    void parsesTipStateExecuteAfterAndBlockersSkippingUnknownFields() throws IOException {
        byte[] bytes = encode(out -> {
            out.writeByteArray(1, encode(tip -> tip.writeUInt32(1, 2039)));
            out.writeUInt32(2, 2);
            out.writeInt64(3, 1_800_000_000L);
            // 未知字段夹在 player_id 与 reason 之间:跳过时多读 / 少读一个字节,reason 就会错位。
            out.writeByteArray(4, encode(b -> {
                b.writeUInt64(1, 7_000_000_000_000_000_000L);
                b.writeString(9, "future field");
                b.writeString(2, "guild_leader");
            }));
            // 第二名角色没写 player_id:必须是 0,不能沿用上一条。
            out.writeByteArray(4, encode(b -> b.writeString(2, "role_listed")));
            out.writeString(15, "future top-level field");
        });

        LoginRpcClient.AccountDeletionResponseProto resp = LoginRpcClient.parseAccountDeletionResponse(bytes);

        assertTrue(resp.hasError());
        assertEquals(2039, resp.errorCode);
        assertEquals(2, resp.state);
        assertEquals(1_800_000_000L, resp.executeAfter);
        assertEquals(2, resp.blockers.size());
        assertEquals(7_000_000_000_000_000_000L, resp.blockers.get(0).getPlayerId());
        assertEquals("guild_leader", resp.blockers.get(0).getReason());
        assertEquals(0L, resp.blockers.get(1).getPlayerId());
        assertEquals("role_listed", resp.blockers.get(1).getReason());
    }

    @Test
    void emptyDeletionResponseIsSuccessWithStateZero() throws IOException {
        LoginRpcClient.AccountDeletionResponseProto resp = LoginRpcClient.parseAccountDeletionResponse(new byte[0]);

        assertFalse(resp.hasError());
        assertEquals(0, resp.state);
        assertEquals(0L, resp.executeAfter);
        assertTrue(resp.blockers.isEmpty());
    }

    @Test
    void loginResponseParsesDeletionFields7To9() throws IOException {
        byte[] bytes = encode(out -> {
            out.writeString(3, "access-token");
            out.writeUInt32(7, 1);
            out.writeInt64(8, 1_800_000_000L);
            out.writeInt64(9, 1_700_000_000L);
        });

        LoginRpcClient.LoginResponseProto resp = LoginRpcClient.parseLoginResponse(bytes);

        assertEquals("access-token", resp.accessToken);
        assertEquals(1, resp.accountDeletionState);
        assertEquals(1_800_000_000L, resp.accountDeletionExecuteAfter);
        assertEquals(1_700_000_000L, resp.previousAccountDeletedAt);
    }

    private static byte[] encode(Fields fields) throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        fields.writeTo(out);
        out.flush();
        return baos.toByteArray();
    }
}
