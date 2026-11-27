package org.jeecg.modules.gm;

import java.nio.charset.StandardCharsets;
import org.apache.shiro.SecurityUtils;
import org.apache.shiro.subject.Subject;
import org.springframework.beans.BeanWrapperImpl;
import org.springframework.http.HttpStatus;
import org.springframework.web.server.ResponseStatusException;

/**
 * GM 接口的身份与权限检查:必须是已认证的管理员,持有指定权限,操作人取认证主体的用户名(不接受请求里带的操作人)。
 * 与 GmController / GmPlayerCommandController 里的同名私有方法同一口径,新写的控制器用这一份。
 */
final class GmAccess {
    private GmAccess() {}

    static Subject authenticated() {
        Subject subject = SecurityUtils.getSubject();
        if (!subject.isAuthenticated() || subject.getPrincipal() == null)
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "请先登录管理员账号");
        return subject;
    }

    /** 检查权限并返回操作人;没有权限抛 Shiro 的 AuthorizationException,由控制器映射成 403。 */
    static String operator(String permission) {
        Subject subject = authenticated();
        subject.checkPermission(permission);
        // LoginUser 属于上游,不复制其类型;读取其稳定的 username Bean 属性。
        Object value = new BeanWrapperImpl(subject.getPrincipal()).getPropertyValue("username");
        if (!(value instanceof String name) || name.isBlank() || name.getBytes(StandardCharsets.UTF_8).length > 64
                || name.contains("|") || name.codePoints().anyMatch(Character::isISOControl))
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "管理员身份无效或名称过长");
        return name;
    }
}
