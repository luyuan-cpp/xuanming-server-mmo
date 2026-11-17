import { defHttp } from '/@/utils/http/axios';

export type GmCommandStatus = 'DRAFT' | 'SENDING' | 'DELIVERED' | 'REJECTED' | 'UNKNOWN';

export interface GmOverview {
  zones: Record<string, unknown>[];
  zonesAvailable?: boolean;
  mailEnabled: boolean;
}

export interface GmPlayer {
  playerId: string;
  found: boolean;
  display: unknown;
  homeZoneId: string | number | null;
  sessionFound: boolean;
  session: unknown;
}

export interface GmMailDraft {
  clientRequestId: string;
  playerId: string;
  title: string;
  body: string;
  reason: string;
  currencyType?: number;
  currencyAmount?: string;
  itemConfigId?: number;
  itemCount?: number;
  validDays: number;
}

export interface GmCommand {
  id: string;
  status: GmCommandStatus;
  operator: string;
  playerId: string;
  reason: string;
  request: Record<string, unknown>;
  result: Record<string, unknown> | null;
  createdAt: string;
  updatedAt: string;
}

export interface GmCommandPage {
  records: GmCommand[];
  total: number;
}

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;
const MAX_INT64 = '9223372036854775807';

// 玩家永久 ID 和货币不经过 Number，避免超过 2^53 后被浏览器舍入。
export function normalizePlayerId(value: unknown): string {
  return positiveDecimal(value, '玩家 ID', MAX_INT64);
}

function positiveDecimal(value: unknown, label: string, max: string): string {
  const text = typeof value === 'string' ? value.trim() : '';
  if (!/^[1-9]\d*$/.test(text) || text.length > max.length || (text.length === max.length && text > max)) {
    throw new Error(`${label}须为 1 至 ${max} 之间的十进制整数，不支持小数、科学计数法或前导零。`);
  }
  return text;
}

function textField(value: unknown, label: string, max: number, required = true): string {
  if (value !== undefined && value !== null && typeof value !== 'string') throw new Error(`${label}必须是文本。`);
  const text = typeof value === 'string' ? value.trim() : '';
  if ((required && !text) || Array.from(text).length > max) {
    throw new Error(`${label}${required ? '不能为空，且' : ''}最多 ${max} 个字符。`);
  }
  return text;
}

export function buildMailDraft(values: Record<string, unknown>, clientRequestId: string): GmMailDraft {
  const draft: GmMailDraft = {
    clientRequestId,
    playerId: normalizePlayerId(values.playerId),
    title: textField(values.title, '邮件标题', 128),
    body: textField(values.body, '邮件正文', 4096, false),
    reason: textField(values.reason, '操作原因', 191),
    validDays: Number(positiveDecimal(String(values.validDays ?? ''), '有效天数', '30')),
  };
  for (const field of ['currencyAmount', 'itemConfigId', 'itemCount']) {
    if (values[field] !== undefined && values[field] !== null && typeof values[field] !== 'string')
      throw new Error('附件数量与配置 ID 必须按整数文本输入。');
  }
  const amount = typeof values.currencyAmount === 'string' ? values.currencyAmount.trim() : '';
  if (amount) {
    draft.currencyType = 0;
    draft.currencyAmount = positiveDecimal(amount, '银两数量', MAX_INT64);
  }
  const configId = typeof values.itemConfigId === 'string' ? values.itemConfigId.trim() : '';
  const itemCount = typeof values.itemCount === 'string' ? values.itemCount.trim() : '';
  if (configId || itemCount) {
    draft.itemConfigId = Number(positiveDecimal(configId, '物品配置 ID', '2147483647'));
    draft.itemCount = Number(positiveDecimal(itemCount, '物品数量', '4096'));
  }
  if (!amount && !configId) throw new Error('至少填写银两或物品中的一种附件。');
  return draft;
}

export function newClientRequestId(): string {
  // getRandomValues 兼容本机 HTTP 开发页，不回退到可能碰撞的时间戳或 Math.random。
  if (!globalThis.crypto?.getRandomValues) throw new Error('浏览器不支持安全请求号，请使用当前版本的浏览器。');
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function checkedCommand(command: GmCommand): GmCommand {
  if (
    !command ||
    typeof command.id !== 'string' ||
    !command.id ||
    typeof command.playerId !== 'string' ||
    !command.request ||
    typeof command.request !== 'object' ||
    Array.isArray(command.request) ||
    !['DRAFT', 'SENDING', 'DELIVERED', 'REJECTED', 'UNKNOWN'].includes(command.status)
  ) {
    throw new Error('命令响应缺少字符串 ID，请刷新审计记录核对。');
  }
  return command;
}

export async function getGmOverview(): Promise<GmOverview> {
  const result = await defHttp.get<GmOverview>({ url: '/gm/overview' }, requestOptions);
  if (!result || !Array.isArray(result.zones) || typeof result.mailEnabled !== 'boolean') throw new Error('服务状态响应无效。');
  return result;
}

export async function getGmPlayer(playerId: string): Promise<GmPlayer> {
  const result = await defHttp.get<GmPlayer>({ url: `/gm/players/${encodeURIComponent(normalizePlayerId(playerId))}` }, requestOptions);
  if (!result || typeof result.playerId !== 'string') throw new Error('玩家响应缺少字符串 ID。');
  return result;
}

export async function prepareGmMail(draft: GmMailDraft): Promise<GmCommand> {
  return checkedCommand(await defHttp.post<GmCommand>({ url: '/gm/mail/drafts', data: draft }, requestOptions));
}

export async function sendGmMail(commandId: string): Promise<GmCommand> {
  return checkedCommand(await defHttp.post<GmCommand>({ url: `/gm/mail/commands/${encodeURIComponent(commandId)}/send` }, requestOptions));
}

export async function getGmCommands(): Promise<GmCommandPage> {
  const result = await defHttp.get<GmCommandPage>({ url: '/gm/mail/commands' }, requestOptions);
  return { ...result, records: result.records.map(checkedCommand) };
}
