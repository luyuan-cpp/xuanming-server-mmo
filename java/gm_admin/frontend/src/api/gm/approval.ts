import { ref } from 'vue';
import { useUserStore } from '/@/store/modules/user';
import { defHttp } from '/@/utils/http/axios';

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

/**
 * 双人复核是否开启。控制台首页与玩家命令页在加载总览时写入。
 * 它只影响按钮文案与提示；「发起人不能批准自己的申请」由后台流水判定，页面绕不过去。
 */
export const dualControl = ref(false);

/** 三类命令视图里与批准有关的公共字段。 */
export interface ApprovalView {
  id: string;
  status: string;
  operator: string;
  approver?: string | null;
  approvedAt?: string | null;
}

/** 当前登录的管理员。OIDC 模式下就是身份提供方的主体，与命令上记的发起人、批准人同一口径。 */
export function currentOperator(): string | undefined {
  const username = (useUserStore().getUserInfo as { username?: unknown } | undefined)?.username;
  return typeof username === 'string' ? username : undefined;
}

/** 开着双人复核、还没有人放行，而且申请是自己发起的：自己批不了，要等另一名管理员。 */
export function awaitsAnotherApprover(command: ApprovalView | null | undefined): boolean {
  return dualControl.value && !!command && command.status === 'DRAFT' && !command.approver && command.operator === currentOperator();
}

/** 命令列表与审计轨迹里新增的两种状态。 */
export const APPROVAL_STATUS_LABELS: Record<string, string> = { DECLINED: '已驳回', APPROVED: '已批准' };
export const APPROVAL_STATUS_COLORS: Record<string, string> = { DECLINED: 'default', APPROVED: 'success' };

export type DeclinableKind = 'mail' | 'system-mail' | 'mail-campaigns' | 'player-commands' | 'trade-manual';

/** 驳回一条还没放行的申请；发起人用它撤回自己的申请。返回驳回后的命令视图。 */
export function declineCommand<T extends ApprovalView>(kind: DeclinableKind, commandId: string, reason: string): Promise<T> {
  const id = encodeURIComponent(commandId);
  const base =
    kind === 'player-commands'
      ? `/gm/player-commands/${id}`
      : kind === 'trade-manual'
        ? `/gm/trade/manual/commands/${id}`
        : `/gm/${kind}/commands/${id}`;
  return defHttp.post<T>({ url: `${base}/decline`, data: { reason } }, requestOptions);
}
