import { defHttp } from '/@/utils/http/axios';

export type GmCommandStatus = 'DRAFT' | 'SENDING' | 'DELIVERED' | 'REJECTED' | 'UNKNOWN' | 'DECLINED';

export interface GmCurrencyOption {
  id: number;
  label: string;
}

export interface GmOverview {
  zones: Record<string, unknown>[];
  zonesAvailable?: boolean;
  mailEnabled: boolean;
  // 双人复核是否开启：开启时发起人不能批准自己的申请。
  dualControl?: boolean;
  currencies?: GmCurrencyOption[];
}

export interface GmPlayer {
  playerId: string;
  found: boolean;
  display: unknown;
  homeZoneId: string | number | null;
  sessionFound: boolean;
  session: unknown;
}

export interface MailCurrencyAttachment {
  currencyType: number;
  amount: string;
}

export interface MailItemAttachment {
  configId: number;
  count: number;
}

export interface MailCurrencyRow {
  currencyType: number;
  amount: string;
}

export interface MailItemRow {
  configId: string;
  count: string;
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
  currencies?: MailCurrencyAttachment[];
  items?: MailItemAttachment[];
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
  // 第一次放行的人与时刻；还没放行时为 null。
  approver?: string | null;
  approvedAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface GmCommandPage {
  records: GmCommand[];
  total: number;
  page?: number;
  pageSize?: number;
}

export interface GmCommandQuery {
  page?: number;
  pageSize?: number;
  playerId?: string;
  status?: string;
}

export interface GmAuditEntry {
  id: string;
  operator: string;
  event: string;
  attempt: number;
  result: Record<string, unknown> | null;
  createdAt: string;
}

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;
export const MAX_INT64 = '9223372036854775807';
export const MAX_MAIL_CURRENCIES = 4;
export const MAX_MAIL_ITEMS = 16;
export const MAX_MAIL_TOTAL_ITEM_COUNT = 4096;

export function currencyLabel(label: string): string {
  return (
    ({ GOLD: '银两', DIAMOND: '灵石', BIND_DIAMOND: '绑定灵石' } as Record<string, string>)[label.replace(/^CURRENCY_TYPE_/, '')] || label
  );
}

// 玩家永久 ID 和货币不经过 Number，避免超过 2^53 后被浏览器舍入。
export function normalizePlayerId(value: unknown): string {
  return positiveDecimal(value, '玩家 ID', MAX_INT64);
}

export function positiveDecimal(value: unknown, label: string, max: string): string {
  const text = typeof value === 'string' ? value.trim() : '';
  if (!/^[1-9]\d*$/.test(text) || text.length > max.length || (text.length === max.length && text > max)) {
    throw new Error(`${label}须为 1 至 ${max} 之间的十进制整数，不支持小数、科学计数法或前导零。`);
  }
  return text;
}

export function textField(value: unknown, label: string, max: number, required = true): string {
  if (value !== undefined && value !== null && typeof value !== 'string') throw new Error(`${label}必须是文本。`);
  const text = typeof value === 'string' ? value.trim() : '';
  if ((required && !text) || Array.from(text).length > max) {
    throw new Error(`${label}${required ? '不能为空，且' : ''}最多 ${max} 个字符。`);
  }
  return text;
}

export function validateMailAttachments(
  currencyRows: MailCurrencyRow[],
  itemRows: MailItemRow[],
  currencies?: GmCurrencyOption[]
): { currencies?: MailCurrencyAttachment[]; items?: MailItemAttachment[] } {
  const validCurrencies: MailCurrencyAttachment[] = [];
  const seenCurrencies = new Set<number>();
  for (const row of currencyRows) {
    if (typeof row.amount !== 'string') throw new Error('货币数量必须按整数文本输入。');
    const amount = row.amount.trim();
    if (!amount) continue;
    if (
      typeof row.currencyType !== 'number' ||
      !Number.isSafeInteger(row.currencyType) ||
      row.currencyType < 0 ||
      (currencies?.length && !currencies.some((currency) => currency.id === row.currencyType))
    ) {
      throw new Error('请选择服务端提供的有效币种。');
    }
    if (seenCurrencies.has(row.currencyType)) throw new Error('货币附件不能包含重复币种。');
    seenCurrencies.add(row.currencyType);
    validCurrencies.push({
      currencyType: row.currencyType,
      amount: positiveDecimal(amount, '货币数量', MAX_INT64),
    });
  }
  if (validCurrencies.length > MAX_MAIL_CURRENCIES) {
    throw new Error(`单封邮件最多包含 ${MAX_MAIL_CURRENCIES} 种货币。`);
  }

  const validItems: MailItemAttachment[] = [];
  const seenItems = new Set<number>();
  let totalItems = 0;
  for (const row of itemRows) {
    if (typeof row.configId !== 'string' || typeof row.count !== 'string') {
      throw new Error('物品配置 ID 与数量必须按整数文本输入。');
    }
    const configText = row.configId.trim();
    const countText = row.count.trim();
    if (!configText && !countText) continue;
    const configId = Number(positiveDecimal(configText, '物品配置 ID', '2147483647'));
    const count = Number(positiveDecimal(countText, '物品数量', String(MAX_MAIL_TOTAL_ITEM_COUNT)));
    if (seenItems.has(configId)) throw new Error(`物品配置 ID ${configId} 重复，请合并数量。`);
    seenItems.add(configId);
    totalItems += count;
    if (totalItems > MAX_MAIL_TOTAL_ITEM_COUNT) {
      throw new Error(`单封邮件物品总数不能超过 ${MAX_MAIL_TOTAL_ITEM_COUNT}。`);
    }
    validItems.push({ configId, count });
  }
  if (validItems.length > MAX_MAIL_ITEMS) {
    throw new Error(`单封邮件最多包含 ${MAX_MAIL_ITEMS} 种物品。`);
  }
  if (!validCurrencies.length && !validItems.length) {
    throw new Error('至少填写货币或物品中的一种附件。');
  }
  const result: { currencies?: MailCurrencyAttachment[]; items?: MailItemAttachment[] } = {};
  if (validCurrencies.length) result.currencies = validCurrencies;
  if (validItems.length) result.items = validItems;
  return result;
}

export function buildMailDraft(
  values: Record<string, unknown>,
  clientRequestId: string,
  currencies?: GmCurrencyOption[],
  rows?: { currencies: MailCurrencyRow[]; items: MailItemRow[] }
): GmMailDraft {
  const draft: GmMailDraft = {
    clientRequestId,
    playerId: normalizePlayerId(values.playerId),
    title: textField(values.title, '邮件标题', 128),
    body: textField(values.body, '邮件正文', 4096, false),
    reason: textField(values.reason, '操作原因', 191),
    validDays: Number(positiveDecimal(String(values.validDays ?? ''), '有效天数', '30')),
  };
  if (rows) {
    const attachments = validateMailAttachments(rows.currencies, rows.items, currencies);
    if (attachments.currencies) draft.currencies = attachments.currencies;
    if (attachments.items) draft.items = attachments.items;
    return draft;
  }
  for (const field of ['currencyAmount', 'itemConfigId', 'itemCount']) {
    if (values[field] !== undefined && values[field] !== null && typeof values[field] !== 'string')
      throw new Error('附件数量与配置 ID 必须按整数文本输入。');
  }
  const amount = typeof values.currencyAmount === 'string' ? values.currencyAmount.trim() : '';
  if (amount) {
    const currencyType = values.currencyType === undefined || values.currencyType === null ? 0 : values.currencyType;
    if (
      typeof currencyType !== 'number' ||
      !Number.isSafeInteger(currencyType) ||
      currencyType < 0 ||
      (currencies?.length && !currencies.some((currency) => currency.id === currencyType))
    ) {
      throw new Error('请选择服务端提供的有效币种。');
    }
    draft.currencyType = currencyType;
    draft.currencyAmount = positiveDecimal(amount, '货币数量', MAX_INT64);
  }
  const configId = typeof values.itemConfigId === 'string' ? values.itemConfigId.trim() : '';
  const itemCount = typeof values.itemCount === 'string' ? values.itemCount.trim() : '';
  if (configId || itemCount) {
    draft.itemConfigId = Number(positiveDecimal(configId, '物品配置 ID', '2147483647'));
    draft.itemCount = Number(positiveDecimal(itemCount, '物品数量', '4096'));
  }
  if (!amount && !configId) throw new Error('至少填写货币或物品中的一种附件。');
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

export function buildAuditQueryParams(query?: GmCommandQuery): Record<string, string | number> {
  const params: Record<string, string | number> = {
    page: query?.page ?? 1,
    pageSize: query?.pageSize ?? 50,
  };
  const playerId = query?.playerId?.trim();
  if (playerId) params.playerId = normalizePlayerId(playerId);
  const status = query?.status?.trim();
  if (status) params.status = status;
  return params;
}

export function checkedAuditEntries(payload: { records?: GmAuditEntry[] } | undefined): GmAuditEntry[] {
  if (!payload || !Array.isArray(payload.records)) throw new Error('审计轨迹响应无效。');
  return payload.records.map((entry) => {
    if (!entry || typeof entry.id !== 'string' || typeof entry.event !== 'string' || typeof entry.operator !== 'string') {
      throw new Error('审计轨迹条目格式无效。');
    }
    return entry;
  });
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
    !['DRAFT', 'SENDING', 'DELIVERED', 'REJECTED', 'UNKNOWN', 'DECLINED'].includes(command.status)
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

export async function getGmCommands(query?: GmCommandQuery): Promise<GmCommandPage> {
  const result = await defHttp.get<GmCommandPage>(
    { url: '/gm/mail/commands', params: buildAuditQueryParams(query) },
    requestOptions
  );
  if (!result || !Array.isArray(result.records)) throw new Error('邮件审计响应无效。');
  return { ...result, records: result.records.map(checkedCommand) };
}

export async function getGmCommand(commandId: string): Promise<GmCommand> {
  return checkedCommand(await defHttp.get<GmCommand>({ url: `/gm/mail/commands/${encodeURIComponent(commandId)}` }, requestOptions));
}

export async function getGmCommandAudit(commandId: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ commandId: string; records: GmAuditEntry[] }>(
    { url: `/gm/mail/commands/${encodeURIComponent(commandId)}/audit` },
    requestOptions
  );
  return checkedAuditEntries(result);
}
