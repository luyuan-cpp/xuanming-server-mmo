import { defHttp } from '/@/utils/http/axios';
import {
  buildAuditQueryParams,
  checkedAuditEntries,
  positiveDecimal,
  textField,
  validateMailAttachments,
  type GmAuditEntry,
  type GmCommand,
  type GmCommandPage,
  type GmCommandQuery,
  type GmCurrencyOption,
  type MailCurrencyAttachment,
  type MailCurrencyRow,
  type MailItemAttachment,
  type MailItemRow,
} from './console';

export const CAMPAIGN_SHARD_SIZE = 1000;
export const CAMPAIGN_MAX_RECIPIENTS = 10000;
const MAX_UINT64 = '18446744073709551615';
const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

export interface SystemMailDraft {
  clientRequestId: string;
  title: string;
  body: string;
  reason: string;
  validDays: number;
}

export interface MailCampaignDraft {
  clientRequestId: string;
  title: string;
  body: string;
  reason: string;
  validDays: number;
  currencies?: MailCurrencyAttachment[];
  items?: MailItemAttachment[];
  recipientPlayerIds: string[];
}

export interface ParsedRecipientList {
  ids: string[];
  duplicateCount: number;
  shardCount: number;
}

// 严格按十进制字符串比较无符号 64 位整数，不经 Number，避免精度丢失。
function compareUint64Decimal(left: string, right: string): number {
  if (left.length !== right.length) return left.length - right.length;
  return left < right ? -1 : left > right ? 1 : 0;
}

export function parseRecipientPlayerIds(rawText: string): ParsedRecipientList {
  const tokens = rawText
    .split(/[\s,，;；]+/)
    .map((token) => token.trim())
    .filter((token) => token.length > 0);
  if (!tokens.length) throw new Error('收件人玩家 ID 名单不能为空。');
  if (tokens.length > CAMPAIGN_MAX_RECIPIENTS * 2) {
    throw new Error(`单次输入的收件人条目过多，请将去重后人数控制在 ${CAMPAIGN_MAX_RECIPIENTS} 人以内。`);
  }
  const unique = new Set<string>();
  let duplicateCount = 0;
  for (const token of tokens) {
    const id = positiveDecimal(token, '收件人玩家 ID', MAX_UINT64);
    if (unique.has(id)) {
      duplicateCount += 1;
    } else {
      unique.add(id);
      if (unique.size > CAMPAIGN_MAX_RECIPIENTS) {
        throw new Error(`单次网页活动邮件收件人去重后不能超过 ${CAMPAIGN_MAX_RECIPIENTS} 人。`);
      }
    }
  }
  const ids = Array.from(unique).sort(compareUint64Decimal);
  return {
    ids,
    duplicateCount,
    shardCount: Math.ceil(ids.length / CAMPAIGN_SHARD_SIZE),
  };
}

export interface MailFormFields {
  title?: unknown;
  body?: unknown;
  reason?: unknown;
  validDays?: unknown;
}

export function buildSystemMailDraft(values: MailFormFields, clientRequestId: string): SystemMailDraft {
  return {
    clientRequestId,
    title: textField(values.title, '邮件标题', 128),
    body: textField(values.body, '邮件正文', 4096, false),
    reason: textField(values.reason, '操作原因', 191),
    validDays: Number(positiveDecimal(String(values.validDays ?? ''), '有效天数', '30')),
  };
}

export function buildMailCampaignDraft(
  values: MailFormFields,
  currencyRows: MailCurrencyRow[],
  itemRows: MailItemRow[],
  recipientText: string,
  clientRequestId: string,
  currencies?: GmCurrencyOption[]
): MailCampaignDraft {
  const parsed = parseRecipientPlayerIds(recipientText);
  const attachments = validateMailAttachments(currencyRows, itemRows, currencies);
  const draft: MailCampaignDraft = {
    clientRequestId,
    title: textField(values.title, '邮件标题', 128),
    body: textField(values.body, '邮件正文', 4096, false),
    reason: textField(values.reason, '操作原因', 191),
    validDays: Number(positiveDecimal(String(values.validDays ?? ''), '有效天数', '30')),
    recipientPlayerIds: parsed.ids,
  };
  if (attachments.currencies) draft.currencies = attachments.currencies;
  if (attachments.items) draft.items = attachments.items;
  return draft;
}

function checkedMailCommand(command: GmCommand): GmCommand {
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
    throw new Error('系统邮件命令响应无效，请刷新审计核对。');
  }
  return command;
}

export async function prepareSystemMail(draft: SystemMailDraft): Promise<GmCommand> {
  return checkedMailCommand(await defHttp.post<GmCommand>({ url: '/gm/system-mail/drafts', data: draft }, requestOptions));
}

export async function sendSystemMail(commandId: string): Promise<GmCommand> {
  return checkedMailCommand(
    await defHttp.post<GmCommand>({ url: `/gm/system-mail/commands/${encodeURIComponent(commandId)}/send` }, requestOptions)
  );
}

export async function getSystemMails(query?: Omit<GmCommandQuery, 'playerId'>): Promise<GmCommandPage> {
  const result = await defHttp.get<GmCommandPage>(
    { url: '/gm/system-mail/commands', params: buildAuditQueryParams({ page: query?.page, pageSize: query?.pageSize, status: query?.status }) },
    requestOptions
  );
  if (!result || !Array.isArray(result.records)) throw new Error('系统邮件审计响应无效。');
  return { ...result, records: result.records.map(checkedMailCommand) };
}

export async function getSystemMail(commandId: string): Promise<GmCommand> {
  return checkedMailCommand(
    await defHttp.get<GmCommand>({ url: `/gm/system-mail/commands/${encodeURIComponent(commandId)}` }, requestOptions)
  );
}

export async function getSystemMailAudit(commandId: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ commandId: string; records: GmAuditEntry[] }>(
    { url: `/gm/system-mail/commands/${encodeURIComponent(commandId)}/audit` },
    requestOptions
  );
  return checkedAuditEntries(result);
}

export async function prepareMailCampaign(draft: MailCampaignDraft): Promise<GmCommand> {
  return checkedMailCommand(await defHttp.post<GmCommand>({ url: '/gm/mail-campaigns/drafts', data: draft }, requestOptions));
}

export async function sendMailCampaign(commandId: string): Promise<GmCommand> {
  return checkedMailCommand(
    await defHttp.post<GmCommand>({ url: `/gm/mail-campaigns/commands/${encodeURIComponent(commandId)}/send` }, requestOptions)
  );
}

export async function getMailCampaigns(query?: Omit<GmCommandQuery, 'playerId'>): Promise<GmCommandPage> {
  const result = await defHttp.get<GmCommandPage>(
    {
      url: '/gm/mail-campaigns/commands',
      params: buildAuditQueryParams({ page: query?.page, pageSize: query?.pageSize, status: query?.status }),
    },
    requestOptions
  );
  if (!result || !Array.isArray(result.records)) throw new Error('活动邮件审计响应无效。');
  return { ...result, records: result.records.map(checkedMailCommand) };
}

export async function getMailCampaign(commandId: string): Promise<GmCommand> {
  return checkedMailCommand(
    await defHttp.get<GmCommand>({ url: `/gm/mail-campaigns/commands/${encodeURIComponent(commandId)}` }, requestOptions)
  );
}

export async function getMailCampaignAudit(commandId: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ commandId: string; records: GmAuditEntry[] }>(
    { url: `/gm/mail-campaigns/commands/${encodeURIComponent(commandId)}/audit` },
    requestOptions
  );
  return checkedAuditEntries(result);
}
