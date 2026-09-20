package com.game.gateway.grpc;

import com.game.gateway.dto.LoginResponse;
import com.google.protobuf.CodedOutputStream;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.IOException;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;

/**
 * {@code LoginRpcClient.parseLoginResponse} 是手写的 wire 解析(网关不引入 protoc 生成类),
 * 字段号写错不会有任何编译期报错,只能靠这里用 {@link CodedOutputStream} 手编字节守住。
 *
 * <p>字段号来自 proto 源:
 * <ul>
 *   <li>{@code proto/login/login.proto} LoginResponse.players = 2、access_token = 3;
 *       AccountSimplePlayerWrapper.player = 1</li>
 *   <li>{@code proto/common/base/user_accounts.proto} AccountSimplePlayer:
 *       player_id = 1、class_id = 2、gender = 3、zone_id = 4、name = 5</li>
 * </ul>
 */
class LoginRpcClientParseTest {

    private static final long PLAYER_ID = 42L;
    private static final String PLAYER_NAME = "云中君";

    /** 往 AccountSimplePlayer 消息体里写字段;用回调而不是一堆布尔参数,三种响应各写各的。 */
    @FunctionalInterface
    private interface PlayerFields {
        void writeTo(CodedOutputStream out) throws IOException;
    }

    @Test
    void parsesPlayerNameFromField5() throws IOException {
        byte[] bytes = loginResponse(playerEntry(out -> {
            out.writeUInt64(1, PLAYER_ID);
            out.writeString(5, PLAYER_NAME);
        }));

        LoginRpcClient.LoginResponseProto resp = LoginRpcClient.parseLoginResponse(bytes);

        assertEquals(1, resp.players.size());
        LoginResponse.PlayerInfo p = resp.players.get(0);
        assertEquals(PLAYER_ID, p.getPlayerId());
        assertEquals(PLAYER_NAME, p.getName());
    }

    @Test
    void nameStaysNullWhenOldLoginOmitsField5() throws IOException {
        // 老版本 login 的 AccountSimplePlayer 只有 1–4;2/3/4 网关不读,必须被跳过而不影响 player_id。
        byte[] bytes = loginResponse(playerEntry(out -> {
            out.writeUInt64(1, PLAYER_ID);
            out.writeUInt32(2, 3);
            out.writeUInt32(3, 1);
            out.writeUInt32(4, 7);
        }));

        LoginRpcClient.LoginResponseProto resp = LoginRpcClient.parseLoginResponse(bytes);

        assertEquals(1, resp.players.size());
        assertEquals(PLAYER_ID, resp.players.get(0).getPlayerId());
        assertNull(resp.players.get(0).getName());
    }

    @Test
    void unknownField6IsSkippedWithoutDisturbingKnownFields() throws IOException {
        // 未知字段 6 故意夹在 player_id 与 name 之间:跳过时若多读/少读一个字节,后面的 name 就会错位。
        // 末尾再跟一个顶层 access_token,验证嵌套 limit 正确弹出、外层解析不受影响。
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeByteArray(2, playerEntry(p -> {
            p.writeUInt64(1, PLAYER_ID);
            p.writeString(6, "未来字段");
            p.writeString(5, PLAYER_NAME);
        }));
        out.writeString(3, "access-token");
        out.flush();

        LoginRpcClient.LoginResponseProto resp = LoginRpcClient.parseLoginResponse(baos.toByteArray());

        assertEquals(1, resp.players.size());
        assertEquals(PLAYER_ID, resp.players.get(0).getPlayerId());
        assertEquals(PLAYER_NAME, resp.players.get(0).getName());
        assertEquals("access-token", resp.accessToken);
    }

    @Test
    void nameDoesNotLeakIntoNextPlayerEntry() throws IOException {
        // 同一账号下新老角色混排:前一个有名字,后一个没有 —— 后者必须是 null,不能沿用前者。
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeByteArray(2, playerEntry(p -> {
            p.writeUInt64(1, PLAYER_ID);
            p.writeString(5, PLAYER_NAME);
        }));
        out.writeByteArray(2, playerEntry(p -> p.writeUInt64(1, PLAYER_ID + 1)));
        out.flush();

        LoginRpcClient.LoginResponseProto resp = LoginRpcClient.parseLoginResponse(baos.toByteArray());

        assertEquals(2, resp.players.size());
        assertEquals(PLAYER_NAME, resp.players.get(0).getName());
        assertEquals(PLAYER_ID + 1, resp.players.get(1).getPlayerId());
        assertNull(resp.players.get(1).getName());
    }

    /** 只含一个 players 条目的 LoginResponse。 */
    private static byte[] loginResponse(byte[] wrapperBytes) throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeByteArray(2, wrapperBytes);
        out.flush();
        return baos.toByteArray();
    }

    /** AccountSimplePlayerWrapper{ player = 1 } 的字节;嵌套消息在 wire 上就是 length-delimited 字节串。 */
    private static byte[] playerEntry(PlayerFields fields) throws IOException {
        ByteArrayOutputStream playerBytes = new ByteArrayOutputStream();
        CodedOutputStream playerOut = CodedOutputStream.newInstance(playerBytes);
        fields.writeTo(playerOut);
        playerOut.flush();

        ByteArrayOutputStream wrapperBytes = new ByteArrayOutputStream();
        CodedOutputStream wrapperOut = CodedOutputStream.newInstance(wrapperBytes);
        wrapperOut.writeByteArray(1, playerBytes.toByteArray());
        wrapperOut.flush();
        return wrapperBytes.toByteArray();
    }
}
