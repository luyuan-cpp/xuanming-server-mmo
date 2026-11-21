import { defHttp } from '/@/utils/http/axios';
import {
  buildAuditQueryParams,
  checkedAuditEntries,
  normalizePlayerId,
  type GmAuditEntry,
  type GmCommandQuery,
} from './console';

export const PLAYER_OPERATIONS = [
  { code: 'currency.add', label: '增加货币', permission: 'gm:currency:add' },
  { code: 'currency.deduct', label: '扣除货币', permission: 'gm:currency:deduct' },
  { code: 'currency.block', label: '封禁货币获取', permission: 'gm:currency:block' },
  { code: 'currency.unblock', label: '解除货币封禁', permission: 'gm:currency:unblock' },
  { code: 'player.level', label: '设置等级', permission: 'gm:player:level' },
  { code: 'pet.grant', label: '发放宝宝', permission: 'gm:pet:grant' },
  { code: 'item.grant', label: '发放物品', permission: 'gm:item:grant' },
] as const;

export type PlayerOperation = (typeof PLAYER_OPERATIONS)[number]['code'];
export type PlayerCommandStatus = 'DRAFT' | 'SENDING' | 'EXECUTED' | 'UNKNOWN' | 'REJECTED';
export interface PlayerCommandCatalog {
  enabled: boolean;
  currencies: { id: number; label: string }[];
  limits?: { maxLevel?: number; maxItemCount?: number };
  operations: { code: PlayerOperation; label: string; permission: string; enabled: boolean; description: string }[];
}
export interface PlayerCommandDraft {
  clientRequestId: string;
  playerId: string;
  operation: PlayerOperation;
  arguments: Record<string, string | number>;
  reason: string;
}
export interface PlayerCommand {
  id: string;
  commandType: 'PLAYER';
  operation: PlayerOperation;
  playerId: string;
  operator: string;
  reason: string;
  request: Record<string, unknown>;
  status: PlayerCommandStatus;
  result: Record<string, unknown> | null;
  createdAt: string;
  updatedAt: string;
}

const options = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

export function playerCommandExpiresAt(command: PlayerCommand): number | undefined {
  const issuedAt = command.request.issuedAtMs;
  if (typeof issuedAt !== 'string' || !/^[1-9]\d{0,14}$/.test(issuedAt)) return undefined;
  const time = Number(issuedAt);
  return Number.isSafeInteger(time) ? time + 5 * 60 * 1000 : undefined;
}

function positiveInteger(value: unknown, label: string, max: string): string {
  const text = typeof value === 'string' ? value.trim() : '';
  if (!/^[1-9]\d*$/.test(text) || text.length > max.length || (text.length === max.length && text > max)) {
    throw new Error(`${label}须为 1 至 ${max} 之间的整数文本，不支持科学计数法或前导零。`);
  }
  return text;
}

export function buildPlayerCommandDraft(
  values: Record<string, unknown>,
  operation: PlayerOperation,
  clientRequestId: string,
  catalog?: PlayerCommandCatalog
): PlayerCommandDraft {
  if (!PLAYER_OPERATIONS.some((entry) => entry.code === operation)) throw new Error('不支持此玩家 GM 操作。');
  const reason = typeof values.reason === 'string' ? values.reason.trim() : '';
  if (!reason || Array.from(reason).length > 191) throw new Error('操作原因不能为空，且最多 191 个字符。');
  if (typeof values.reason === 'string' && /[\u0000-\u001f\u007f-\u009f\u2028\u2029]/.test(values.reason))
    throw new Error('操作原因须为单行文本，不能包含换行或控制字符。');
  const args: Record<string, string | number> = {};
  if (operation.startsWith('currency.')) {
    if (
      typeof values.currencyType !== 'number' ||
      !Number.isSafeInteger(values.currencyType) ||
      !catalog?.currencies.some((currency) => currency.id === values.currencyType)
    ) {
      throw new Error('请选择服务端提供的有效币种。');
    }
    args.currencyType = values.currencyType;
  }
  if (operation === 'currency.add' || operation === 'currency.deduct') {
    args.amount = positiveInteger(values.amount, '货币数量', '9223372036854775807');
  } else if (operation === 'player.level') {
    const maxLevel = catalog?.limits?.maxLevel;
    const limit = Number.isSafeInteger(maxLevel) && maxLevel! > 0 ? Math.min(maxLevel!, 85) : 85;
    args.level = Number(positiveInteger(values.level, '玩家等级', String(limit)));
  } else if (operation === 'pet.grant') {
    args.petTableId = Number(positiveInteger(values.petTableId, '宠物配置 ID', '2147483647'));
  } else if (operation === 'item.grant') {
    args.configId = Number(positiveInteger(values.configId, '物品配置 ID', '2147483647'));
    const maxItemCount = catalog?.limits?.maxItemCount;
    const limit = Number.isSafeInteger(maxItemCount) && maxItemCount! > 0 ? Math.min(maxItemCount!, 99) : 99;
    args.count = Number(positiveInteger(values.count, '物品数量', String(limit)));
  }
  return { clientRequestId, playerId: normalizePlayerId(values.playerId), operation, arguments: args, reason };
}

function checkedPlayerCommand(command: PlayerCommand): PlayerCommand {
  if (
    !command ||
    typeof command.id !== 'string' ||
    !command.id ||
    typeof command.playerId !== 'string' ||
    command.commandType !== 'PLAYER' ||
    !PLAYER_OPERATIONS.some((entry) => entry.code === command.operation) ||
    !['DRAFT', 'SENDING', 'EXECUTED', 'UNKNOWN', 'REJECTED'].includes(command.status) ||
    !command.request ||
    typeof command.request !== 'object' ||
    Array.isArray(command.request)
  )
    throw new Error('玩家 GM 命令响应无效，请刷新审计核对。');
  return command;
}

export async function getPlayerCommandCatalog(): Promise<PlayerCommandCatalog> {
  const catalog = await defHttp.get<PlayerCommandCatalog>({ url: '/gm/player-commands/catalog' }, options);
  if (
    !catalog ||
    typeof catalog.enabled !== 'boolean' ||
    !Array.isArray(catalog.operations) ||
    catalog.operations.some(
      (operation) => !operation || typeof operation.code !== 'string' || typeof operation.enabled !== 'boolean' || typeof operation.label !== 'string'
    ) ||
    !Array.isArray(catalog.currencies) ||
    catalog.currencies.some((currency) => !currency || !Number.isSafeInteger(currency.id) || currency.id < 0 || typeof currency.label !== 'string')
  )
    throw new Error('玩家 GM 服务状态响应无效。');
  return catalog;
}
export async function preparePlayerCommand(draft: PlayerCommandDraft): Promise<PlayerCommand> {
  return checkedPlayerCommand(await defHttp.post<PlayerCommand>({ url: '/gm/player-commands/drafts', data: draft }, options));
}
export async function sendPlayerCommand(id: string): Promise<PlayerCommand> {
  return checkedPlayerCommand(await defHttp.post<PlayerCommand>({ url: `/gm/player-commands/${encodeURIComponent(id)}/send` }, options));
}
export async function getPlayerCommands(
  query?: GmCommandQuery
): Promise<{ records: PlayerCommand[]; total: number; page?: number; pageSize?: number }> {
  const page = await defHttp.get<{ records: PlayerCommand[]; total: number; page?: number; pageSize?: number }>(
    { url: '/gm/player-commands', params: buildAuditQueryParams(query) },
    options
  );
  if (!page || !Array.isArray(page.records)) throw new Error('玩家 GM 审计响应无效。');
  return { ...page, records: page.records.map(checkedPlayerCommand) };
}
export async function getPlayerCommandAudit(id: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ commandId: string; records: GmAuditEntry[] }>(
    { url: `/gm/player-commands/${encodeURIComponent(id)}/audit` },
    options
  );
  return checkedAuditEntries(result);
}
