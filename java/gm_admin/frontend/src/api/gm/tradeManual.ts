import { defHttp } from '/@/utils/http/axios';
import type { GmAuditEntry } from '/@/api/gm/console';

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

/** 交易人工处置能做的六种动作(go/trade/internal/commerce.ManualAction)。 */
export const TRADE_MANUAL_ACTIONS = [
  { value: 'resume', label: '恢复(回到转人工前的阶段重新判定)' },
  { value: 'reissue', label: '补发(没到账的最终发放腿各补一条)' },
  { value: 'refund', label: '退回(托管资产原样退给扣出它的人)' },
  { value: 'partial', label: '部分认定(逐腿写明缺口)' },
  { value: 'redirect', label: '改绑(把一方改到原账号名下另一角色)' },
  { value: 'void-auction', label: '作废拍卖(拍品与出价一起退回)' },
] as const;

export type TradeManualAction = (typeof TRADE_MANUAL_ACTIONS)[number]['value'];
export type TradeManualStatus = 'DRAFT' | 'SENDING' | 'EXECUTED' | 'UNKNOWN' | 'REJECTED' | 'DECLINED';

export interface TradeStatus {
  enabled: boolean;
  dualControl: boolean;
}

/** trade 回包按 protobuf JSON 原样转发;64 位整数一律是十进制字符串。 */
export interface ManualContractRecord {
  commerceId?: string;
  kind?: number;
  status?: number;
  version?: string;
  updatedMs?: string;
  [key: string]: unknown;
}

export interface ManualContractPage {
  contracts?: ManualContractRecord[];
  nextAfterUpdatedMs?: string;
  nextAfterCommerceId?: string;
  target?: string;
  retry?: boolean;
}

export interface ManualContractReply {
  verdict?: string;
  refusal?: string;
  target?: string;
  record?: ManualContractRecord;
  linked?: ManualContractRecord;
  legs?: Record<string, unknown>[];
  parties?: Record<string, unknown>[];
  actions?: { action?: string; allowed?: boolean; detail?: string }[];
  plan?: Record<string, unknown>;
  linkedPlan?: Record<string, unknown>;
  stage?: string;
  status?: number;
  auditLine?: string;
  alreadyExecuted?: boolean;
}

export interface TradeManualDraft {
  clientRequestId: string;
  commerceId: string;
  action: TradeManualAction;
  expectVersion: string;
  expectTarget: string;
  reason: string;
  parts?: string[];
  party?: 'seller' | 'buyer';
  toPlayerId?: string;
  linkedCommerceId?: string;
  linkedVersion?: string;
}

export interface TradeManualCommand {
  id: string;
  commandType: 'TRADE_MANUAL';
  status: TradeManualStatus;
  operator: string;
  approver?: string | null;
  approvedAt?: string | null;
  reason: string;
  request: Record<string, unknown>;
  result: Record<string, unknown> | null;
  createdAt: string;
  updatedAt: string;
}

export interface TradeManualCommandPage {
  records: TradeManualCommand[];
  total: number;
}

export function getTradeStatus(): Promise<TradeStatus> {
  return defHttp.get<TradeStatus>({ url: '/gm/trade/status' }, requestOptions);
}

export function listManualContracts(afterUpdatedMs = '0', afterCommerceId = '0', limit = 50): Promise<ManualContractPage> {
  return defHttp.get<ManualContractPage>({ url: '/gm/trade/manual-contracts', params: { afterUpdatedMs, afterCommerceId, limit } }, requestOptions);
}

export function showManualContract(commerceId: string): Promise<ManualContractReply> {
  return defHttp.get<ManualContractReply>({ url: `/gm/trade/manual-contracts/${encodeURIComponent(commerceId)}` }, requestOptions);
}

export function prepareTradeManual(draft: TradeManualDraft): Promise<{ command: TradeManualCommand; plan: ManualContractReply }> {
  return defHttp.post<{ command: TradeManualCommand; plan: ManualContractReply }>({ url: '/gm/trade/manual/drafts', data: draft }, requestOptions);
}

export function dryRunTradeManual(commandId: string): Promise<ManualContractReply> {
  return defHttp.post<ManualContractReply>({ url: `/gm/trade/manual/commands/${encodeURIComponent(commandId)}/dry-run` }, requestOptions);
}

export function sendTradeManual(commandId: string): Promise<TradeManualCommand> {
  return defHttp.post<TradeManualCommand>({ url: `/gm/trade/manual/commands/${encodeURIComponent(commandId)}/send` }, requestOptions);
}

export function listTradeManualCommands(page: number, pageSize: number, status?: string): Promise<TradeManualCommandPage> {
  return defHttp.get<TradeManualCommandPage>({ url: '/gm/trade/manual/commands', params: { page, pageSize, status } }, requestOptions);
}

export function getTradeManualCommand(commandId: string): Promise<TradeManualCommand> {
  return defHttp.get<TradeManualCommand>({ url: `/gm/trade/manual/commands/${encodeURIComponent(commandId)}` }, requestOptions);
}

export async function getTradeManualAudit(commandId: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ records: GmAuditEntry[] }>(
    { url: `/gm/trade/manual/commands/${encodeURIComponent(commandId)}/audit` },
    requestOptions
  );
  return Array.isArray(result?.records) ? result.records : [];
}
