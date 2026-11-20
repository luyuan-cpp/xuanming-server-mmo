package com.game.gateway.dto;

import com.game.gateway.grpc.LoginRpcClient;

/**
 * 防沉迷状态(docs/design/real-name-anti-addiction.md §5.3),原样转 login 的 AntiAddictionStatus。
 * JSON 字段按全局 SNAKE_CASE:real_name_required、minor、play_deadline_ms。
 */
public class AntiAddictionStatus {
    /** 本次登录的渠道要游戏方实名,而账号还没实名:先调 /api/real-name。 */
    private boolean realNameRequired;
    /** 已实名且未满 18 周岁。 */
    private boolean minor;
    /** 未成年人本次能玩到的时刻(Unix 毫秒,到点服务端踢线);0 = 不限,或现在不在可玩时段。 */
    private long playDeadlineMs;

    /** login 没带这一段(部署没开防沉迷)时返回 null,JSON 里省略。 */
    public static AntiAddictionStatus from(LoginRpcClient.AntiAddictionProto proto) {
        if (proto == null) {
            return null;
        }
        AntiAddictionStatus status = new AntiAddictionStatus();
        status.realNameRequired = proto.realNameRequired;
        status.minor = proto.minor;
        status.playDeadlineMs = proto.playDeadlineMs;
        return status;
    }

    public boolean isRealNameRequired() { return realNameRequired; }
    public void setRealNameRequired(boolean realNameRequired) { this.realNameRequired = realNameRequired; }
    public boolean isMinor() { return minor; }
    public void setMinor(boolean minor) { this.minor = minor; }
    public long getPlayDeadlineMs() { return playDeadlineMs; }
    public void setPlayDeadlineMs(long playDeadlineMs) { this.playDeadlineMs = playDeadlineMs; }
}
