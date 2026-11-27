import { ref } from 'vue';
import { defHttp } from '/@/utils/http/axios';

export type IdentityMode = 'oidc' | 'native';

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

/** 登录页该显示哪一种登录。问到后台之前是 unknown，此时不显示口令表单。 */
export const identityMode = ref<IdentityMode | 'unknown'>('unknown');

export async function loadIdentityMode(): Promise<IdentityMode> {
  const result = await defHttp.get<{ mode: IdentityMode }>({ url: '/gm/sso/config' }, requestOptions);
  if (result?.mode !== 'oidc' && result?.mode !== 'native') throw new Error('后台没有返回有效的登录模式');
  identityMode.value = result.mode;
  return result.mode;
}

/** 向后台要一个身份提供方的授权地址；state、nonce 与 PKCE 校验串都由后台生成并保存。 */
export async function beginEnterpriseLogin(): Promise<string> {
  const result = await defHttp.get<{ url: string }>({ url: '/gm/sso/authorize' }, requestOptions);
  if (typeof result?.url !== 'string' || !/^https?:\/\//.test(result.url)) throw new Error('后台没有返回有效的授权地址');
  return result.url;
}

/** 把身份提供方带回来的授权码交给后台换会话；身份提供方的令牌不经过浏览器。 */
export async function finishEnterpriseLogin(code: string, state: string): Promise<string> {
  const result = await defHttp.post<{ token: string }>({ url: '/gm/sso/token', data: { code, state } }, requestOptions);
  if (typeof result?.token !== 'string' || result.token.length === 0) throw new Error('后台没有签发会话');
  return result.token;
}
