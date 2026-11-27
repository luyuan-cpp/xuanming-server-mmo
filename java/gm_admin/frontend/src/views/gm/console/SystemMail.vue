<template>
  <a-card title="系统邮件与批量活动发奖" class="gm-system-mail">
    <a-alert v-if="!mailEnabled" type="warning" show-icon message="邮件服务尚未启用，当前不能生成或发送系统邮件与活动发奖。" />

    <a-tabs v-model:activeKey="activeTab" class="space">
      <a-tab-pane key="notice" tab="全服公告邮件（纯文本）">
        <a-alert
          type="info"
          show-icon
          message="全服纯文本公告零玩家扩散写，所有区服玩家打开邮箱均可看到；此入口不允许携带任何货币或物品附件。"
        />

        <div v-if="hasPermission('gm:system-mail:prepare')" class="form-section space">
          <a-row :gutter="[16, 12]">
            <a-col :xs="24" :lg="16">
              <label class="field-label" for="gm-sys-title">邮件标题 *</label>
              <a-input
                id="gm-sys-title"
                v-model:value="noticeForm.title"
                placeholder="最多 128 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateNoticeDraft"
              />
            </a-col>
            <a-col :xs="24" :lg="8">
              <label class="field-label" for="gm-sys-days">有效天数 *</label>
              <a-input-number
                id="gm-sys-days"
                v-model:value="noticeForm.validDays"
                :min="1"
                :max="30"
                :precision="0"
                style="width: 100%"
                :disabled="blocked || !mailEnabled"
                @change="invalidateNoticeDraft"
              />
            </a-col>
            <a-col :span="24">
              <label class="field-label" for="gm-sys-body">邮件正文</label>
              <a-textarea
                id="gm-sys-body"
                v-model:value="noticeForm.body"
                :rows="3"
                placeholder="可留空，最多 4096 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateNoticeDraft"
              />
            </a-col>
            <a-col :span="24">
              <label class="field-label" for="gm-sys-reason">操作原因 *</label>
              <a-input
                id="gm-sys-reason"
                v-model:value="noticeForm.reason"
                placeholder="填写停服维护、版本公告等具体原因，最多 191 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateNoticeDraft"
              />
            </a-col>
          </a-row>

          <a-alert v-if="noticePrepareError" class="space" type="warning" show-icon :message="noticePrepareError" />
          <div class="actions">
            <a-button
              v-auth="'gm:system-mail:prepare'"
              type="primary"
              :loading="preparing && activeTab === 'notice'"
              :disabled="blocked || !mailEnabled"
              @click="prepareNotice"
            >
              {{ noticePrepared ? '查看已有草稿' : noticePrepareError ? '沿用请求号获取草稿' : '生成公告草稿并核对' }}
            </a-button>
            <a-button :disabled="blocked" @click="resetNotice">清空并开始新操作</a-button>
          </div>
          <p v-if="noticeRequestId" class="hint space">
            本次请求号：<span class="identifier">{{ noticeRequestId }}</span>
          </p>
        </div>
        <a-empty v-else class="space" description="当前账号没有准备系统邮件的权限" />

        <div v-if="hasPermission('gm:audit:read')" class="audit">
          <div class="audit-heading">
            <h3>全服公告记录</h3>
            <a-button v-auth="'gm:audit:read'" :loading="noticeAuditLoading" :disabled="blocked" @click="loadNoticeAudit">刷新记录</a-button>
          </div>
          <div class="filters">
            <a-select
              v-model:value="noticeFilterStatus"
              class="filter-select"
              allow-clear
              placeholder="全部状态"
              :options="mailStatusOptions"
              :disabled="blocked"
              @change="applyNoticeFilter"
            />
            <a-button :loading="noticeAuditLoading" :disabled="blocked" @click="applyNoticeFilter">筛选</a-button>
            <a-button :disabled="blocked" @click="resetNoticeFilter">重置</a-button>
          </div>
          <a-alert v-if="noticeAuditError" class="space" type="warning" show-icon :message="noticeAuditError" />
          <BasicTable @register="registerNoticeTable" :data-source="noticeRecords" :loading="noticeAuditLoading">
            <template #status="{ record }">
              <a-tag :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag>
            </template>
            <template #title="{ record }">{{ String(record.request?.title ?? '') }}</template>
            <template #action="{ record }">
              <a-button type="link" :disabled="blocked" @click="openReview('notice', record)">核对 / 详情</a-button>
            </template>
          </BasicTable>
          <div class="pagination space">
            <p class="hint">共 {{ noticeTotal }} 条，当前第 {{ noticePage }} 页（本页 {{ noticeRecords.length }} 条）。</p>
            <a-pagination
              :current="noticePage"
              :page-size="noticePageSize"
              :total="noticeTotal"
              :page-size-options="['20', '50', '100']"
              show-size-changer
              :disabled="blocked || noticeAuditLoading"
              @change="onNoticePageChange"
            />
          </div>
        </div>
      </a-tab-pane>

      <a-tab-pane key="campaign" tab="批量活动发奖（冻结名单 + 附件）">
        <a-alert
          type="info"
          show-icon
          message="附件批量发奖走三段式幂等协议：建活动 → 按 1000 人分片导入冻结名单 → 校验 SHA-256 摘要并封存发布。网页单次上限 10,000 人；玩家领取附件后资产才会到账。"
        />

        <div v-if="hasPermission('gm:system-mail:prepare')" class="form-section space">
          <a-row :gutter="[16, 12]">
            <a-col :xs="24" :lg="16">
              <label class="field-label" for="gm-camp-title">活动邮件标题 *</label>
              <a-input
                id="gm-camp-title"
                v-model:value="campaignForm.title"
                placeholder="最多 128 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateCampaignDraft"
              />
            </a-col>
            <a-col :xs="24" :lg="8">
              <label class="field-label" for="gm-camp-days">有效天数 *</label>
              <a-input-number
                id="gm-camp-days"
                v-model:value="campaignForm.validDays"
                :min="1"
                :max="30"
                :precision="0"
                style="width: 100%"
                :disabled="blocked || !mailEnabled"
                @change="invalidateCampaignDraft"
              />
            </a-col>
            <a-col :span="24">
              <label class="field-label" for="gm-camp-body">邮件正文</label>
              <a-textarea
                id="gm-camp-body"
                v-model:value="campaignForm.body"
                :rows="3"
                placeholder="可留空，最多 4096 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateCampaignDraft"
              />
            </a-col>

            <a-col :xs="24" :lg="12">
              <div class="sub-header">
                <span class="field-label">货币附件（最多 {{ MAX_MAIL_CURRENCIES }} 种，不可重复币种）</span>
                <a-button
                  size="small"
                  :disabled="blocked || !mailEnabled || campaignCurrencies.length >= MAX_MAIL_CURRENCIES"
                  @click="addCampaignCurrency"
                >
                  + 添加货币
                </a-button>
              </div>
              <div v-for="(row, index) in campaignCurrencies" :key="`cur-${index}`" class="attachment-row">
                <a-select
                  v-model:value="row.currencyType"
                  class="currency-select"
                  :options="currencyOptions"
                  :disabled="blocked || !mailEnabled"
                  @change="invalidateCampaignDraft"
                />
                <a-input
                  v-model:value="row.amount"
                  inputmode="numeric"
                  placeholder="正整数数量，留空忽略"
                  :disabled="blocked || !mailEnabled"
                  @change="invalidateCampaignDraft"
                />
                <a-button size="small" :disabled="blocked || !mailEnabled" @click="removeCampaignCurrency(index)">移除</a-button>
              </div>
              <p v-if="!campaignCurrencies.length" class="hint">未添加货币附件。</p>
            </a-col>

            <a-col :xs="24" :lg="12">
              <div class="sub-header">
                <span class="field-label">物品附件（最多 {{ MAX_MAIL_ITEMS }} 种，总数 ≤ {{ MAX_MAIL_TOTAL_ITEM_COUNT }}）</span>
                <a-button
                  size="small"
                  :disabled="blocked || !mailEnabled || campaignItems.length >= MAX_MAIL_ITEMS"
                  @click="addCampaignItem"
                >
                  + 添加物品
                </a-button>
              </div>
              <div v-for="(row, index) in campaignItems" :key="`item-${index}`" class="attachment-row">
                <a-input
                  v-model:value="row.configId"
                  inputmode="numeric"
                  placeholder="物品配置 ID"
                  :disabled="blocked || !mailEnabled"
                  @change="invalidateCampaignDraft"
                />
                <a-input
                  v-model:value="row.count"
                  inputmode="numeric"
                  placeholder="数量 (1~4096)"
                  :disabled="blocked || !mailEnabled"
                  @change="invalidateCampaignDraft"
                />
                <a-button size="small" :disabled="blocked || !mailEnabled" @click="removeCampaignItem(index)">移除</a-button>
              </div>
              <p v-if="!campaignItems.length" class="hint">未添加物品附件。</p>
            </a-col>

            <a-col :span="24">
              <label class="field-label" for="gm-camp-recipients">收件人玩家 ID 名单 *（支持换行、空格、逗号或分号分隔，单次最多 10,000 人）</label>
              <a-textarea
                id="gm-camp-recipients"
                v-model:value="campaignRecipientText"
                :rows="4"
                placeholder="每行一个玩家 ID，或用逗号/空格分隔"
                :disabled="blocked || !mailEnabled"
                @change="invalidateCampaignDraft"
              />
              <p v-if="recipientPreview.summary" class="hint space">{{ recipientPreview.summary }}</p>
              <a-alert v-if="recipientPreview.error" class="space" type="error" show-icon :message="recipientPreview.error" />
            </a-col>

            <a-col :span="24">
              <label class="field-label" for="gm-camp-reason">操作原因 *</label>
              <a-input
                id="gm-camp-reason"
                v-model:value="campaignForm.reason"
                placeholder="填写活动发奖、故障补偿单号等具体原因，最多 191 个字符"
                :disabled="blocked || !mailEnabled"
                @change="invalidateCampaignDraft"
              />
            </a-col>
          </a-row>

          <a-alert v-if="campaignPrepareError" class="space" type="warning" show-icon :message="campaignPrepareError" />
          <div class="actions">
            <a-button
              v-auth="'gm:system-mail:prepare'"
              type="primary"
              :loading="preparing && activeTab === 'campaign'"
              :disabled="blocked || !mailEnabled"
              @click="prepareCampaign"
            >
              {{ campaignPrepared ? '查看已有活动草稿' : campaignPrepareError ? '沿用请求号获取草稿' : '生成冻结活动草稿并核对' }}
            </a-button>
            <a-button :disabled="blocked" @click="resetCampaign">清空并开始新活动</a-button>
          </div>
          <p v-if="campaignRequestId" class="hint space">
            本次请求号：<span class="identifier">{{ campaignRequestId }}</span>
          </p>
        </div>
        <a-empty v-else class="space" description="当前账号没有准备活动发奖的权限" />

        <div v-if="hasPermission('gm:audit:read')" class="audit">
          <div class="audit-heading">
            <h3>批量活动发奖记录</h3>
            <a-button v-auth="'gm:audit:read'" :loading="campaignAuditLoading" :disabled="blocked" @click="loadCampaignAudit">刷新记录</a-button>
          </div>
          <div class="filters">
            <a-select
              v-model:value="campaignFilterStatus"
              class="filter-select"
              allow-clear
              placeholder="全部状态"
              :options="mailStatusOptions"
              :disabled="blocked"
              @change="applyCampaignFilter"
            />
            <a-button :loading="campaignAuditLoading" :disabled="blocked" @click="applyCampaignFilter">筛选</a-button>
            <a-button :disabled="blocked" @click="resetCampaignFilter">重置</a-button>
          </div>
          <a-alert v-if="campaignAuditError" class="space" type="warning" show-icon :message="campaignAuditError" />
          <BasicTable @register="registerCampaignTable" :data-source="campaignRecords" :loading="campaignAuditLoading">
            <template #status="{ record }">
              <a-tag :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag>
            </template>
            <template #title="{ record }">{{ campaignMailField(record, 'title') }}</template>
            <template #recipients="{ record }">{{ String(record.request?.totalRecipients ?? 0) }} 人 / {{ String(record.request?.shardCount ?? 0) }} 片</template>
            <template #action="{ record }">
              <a-button type="link" :disabled="blocked" @click="openReview('campaign', record)">核对 / 详情</a-button>
            </template>
          </BasicTable>
          <div class="pagination space">
            <p class="hint">共 {{ campaignTotal }} 条，当前第 {{ campaignPage }} 页（本页 {{ campaignRecords.length }} 条）。</p>
            <a-pagination
              :current="campaignPage"
              :page-size="campaignPageSize"
              :total="campaignTotal"
              :page-size-options="['20', '50', '100']"
              show-size-changer
              :disabled="blocked || campaignAuditLoading"
              @change="onCampaignPageChange"
            />
          </div>
        </div>
      </a-tab-pane>
    </a-tabs>

    <BasicModal
      @register="registerModal"
      :title="reviewKind === 'campaign' ? '核对批量活动发奖' : '核对全服公告邮件'"
      :width="820"
      :mask-closable="false"
      :closable="!blocked"
      :keyboard="!blocked"
      :can-fullscreen="false"
    >
      <template v-if="reviewed">
        <a-alert
          type="warning"
          show-icon
          :message="
            reviewKind === 'campaign'
              ? '确认发布后将按三段式幂等协议导入冻结名单并封存发布；仅名单内玩家可领取附件，领取后资产到账。'
              : '全服系统公告发送后，全服所有玩家均可在邮箱中看到此纯文本通知。请仔细核对标题、正文与有效期。'
          "
        />
        <a-descriptions class="space" :column="1" bordered size="small">
          <a-descriptions-item label="命令号">
            <span class="identifier">{{ reviewed.id }}</span>
          </a-descriptions-item>
          <a-descriptions-item label="状态">
            <a-tag :color="statusColor(reviewed.status)">{{ statusLabel(reviewed.status) }}</a-tag>
          </a-descriptions-item>
          <a-descriptions-item label="操作人">{{ reviewed.operator }}</a-descriptions-item>
          <a-descriptions-item label="操作原因">{{ reviewed.reason }}</a-descriptions-item>
          <a-descriptions-item label="邮件标题">{{ reviewTitle }}</a-descriptions-item>
          <a-descriptions-item label="邮件正文">
            <span class="value-text">{{ reviewBody }}</span>
          </a-descriptions-item>
          <template v-if="reviewKind === 'campaign'">
            <a-descriptions-item label="活动 ID">
              <span class="identifier">{{ reviewCampaignField('campaignId') }}</span>
            </a-descriptions-item>
            <a-descriptions-item label="冻结名单规模">
              {{ String(reviewed.request.totalRecipients ?? 0) }} 人（共 {{ String(reviewed.request.shardCount ?? 0) }} 个分片）
            </a-descriptions-item>
            <a-descriptions-item label="清单摘要 (SHA-256)">
              <span class="identifier">{{ reviewCampaignField('manifestHash') }}</span>
            </a-descriptions-item>
            <a-descriptions-item label="收件人样例（前 20 位）">
              <span class="identifier">{{ reviewRecipientSample }}</span>
            </a-descriptions-item>
            <a-descriptions-item label="货币附件">
              <div v-for="(currency, index) in reviewCurrencies" :key="index" class="identifier">
                {{ mailCurrencyName(currency.currencyType) }}：{{ currency.amount }}
              </div>
              <span v-if="!reviewCurrencies.length">无</span>
            </a-descriptions-item>
            <a-descriptions-item label="物品附件">
              <div v-for="(item, index) in reviewItems" :key="index">配置 ID {{ item.configId }} × {{ item.count }}</div>
              <span v-if="!reviewItems.length">无</span>
            </a-descriptions-item>
          </template>
          <a-descriptions-item label="邮件到期时间">{{ expireTime(reviewExpireMs) }}</a-descriptions-item>
          <a-descriptions-item label="创建时间">{{ reviewed.createdAt }}</a-descriptions-item>
          <a-descriptions-item label="更新时间">{{ reviewed.updatedAt }}</a-descriptions-item>
        </a-descriptions>
        <ApprovalPanel
          :command="reviewed"
          :kind="reviewKind === 'campaign' ? 'mail-campaigns' : 'system-mail'"
          :can-release="hasPermission('gm:system-mail:send')"
          :can-propose="hasPermission('gm:system-mail:prepare')"
          :busy="blocked"
          @declined="onReviewedDeclined"
        />

        <a-collapse class="space">
          <a-collapse-panel key="request" header="冻结请求详情">
            <pre class="json">{{ pretty(reviewed.request) }}</pre>
          </a-collapse-panel>
          <a-collapse-panel v-if="hasPermission('gm:audit:read')" key="audit" header="状态变更轨迹">
            <a-spin :spinning="auditHistoryLoading">
              <a-alert v-if="auditHistoryError" type="warning" show-icon :message="auditHistoryError" />
              <a-empty v-else-if="!auditHistory.length" description="暂无审计轨迹" />
              <div v-else class="timeline">
                <div v-for="entry in auditHistory" :key="entry.id" class="timeline-item">
                  <div class="timeline-meta">
                    <a-tag :color="statusColor(entry.event)">{{ statusLabel(entry.event) }}</a-tag>
                    <span>第 {{ entry.attempt }} 次</span>
                    <span>操作人：{{ entry.operator }}</span>
                    <span class="hint">{{ entry.createdAt }}</span>
                  </div>
                  <pre v-if="entry.result" class="json">{{ pretty(entry.result) }}</pre>
                </div>
              </div>
            </a-spin>
          </a-collapse-panel>
        </a-collapse>

        <template v-if="reviewed.result">
          <h3 class="space">服务端执行结果</h3>
          <pre class="json">{{ pretty(reviewed.result) }}</pre>
        </template>

        <a-alert v-if="sendError" class="space" type="warning" show-icon :message="sendError" />
        <a-alert
          v-else-if="reviewed.status === 'UNKNOWN'"
          class="space"
          type="warning"
          show-icon
          message="上次发布结果未知。请先刷新原命令状态；重试将沿用同一冻结命令号与清单摘要幂等续传，不会重复发布。"
        />
        <a-alert
          v-else-if="reviewed.status === 'SENDING'"
          class="space"
          type="info"
          show-icon
          message="命令正在发布中，请刷新原命令状态核对结果。"
        />
        <a-alert
          v-else-if="reviewed.status === 'DELIVERED'"
          class="space"
          type="success"
          show-icon
          :message="reviewKind === 'campaign' ? '活动邮件已完成三段式导入并封存发布。' : '全服公告邮件已成功投递。'"
        />

        <a-checkbox v-if="canSendReviewed" v-model:checked="reviewConfirmed" :disabled="blocked || !mailEnabled" class="space">
          我已核对以上内容与原因，确认{{ retryingReviewed ? '沿用原冻结命令幂等重试' : reviewKind === 'campaign' ? '导入名单并封存发布此活动邮件' : '向全服发送此公告邮件' }}。
        </a-checkbox>
      </template>

      <template #footer>
        <a-button :disabled="blocked" @click="closeModal">关闭</a-button>
        <a-button v-auth="'gm:audit:read'" :loading="refreshingReview || auditHistoryLoading" :disabled="blocked" @click="refreshReviewed">
          刷新原命令状态
        </a-button>
        <a-button
          v-if="canSendReviewed"
          v-auth="'gm:system-mail:send'"
          type="primary"
          :loading="sending"
          :disabled="blocked || !mailEnabled || !reviewConfirmed"
          @click="sendReviewed"
        >
          {{ retryingReviewed ? '沿用原命令重试' : dualControl ? '批准并发布' : '确认发布' }}
        </a-button>
      </template>
    </BasicModal>
  </a-card>
</template>

<script lang="ts" setup name="gm-system-mail">
  import { computed, onMounted, reactive, ref } from 'vue';
  import { BasicTable, useTable, type BasicColumn } from '/@/components/Table';
  import { BasicModal, useModal } from '/@/components/Modal';
  import { usePermission } from '/@/hooks/web/usePermission';
  import { useMessage } from '/@/hooks/web/useMessage';
  import ApprovalPanel from './ApprovalPanel.vue';
  import { APPROVAL_STATUS_COLORS, APPROVAL_STATUS_LABELS, awaitsAnotherApprover, dualControl, type ApprovalView } from '/@/api/gm/approval';
  import {
    currencyLabel,
    MAX_MAIL_CURRENCIES,
    MAX_MAIL_ITEMS,
    MAX_MAIL_TOTAL_ITEM_COUNT,
    newClientRequestId,
    type GmAuditEntry,
    type GmCommand,
    type GmCurrencyOption,
    type MailCurrencyRow,
    type MailItemRow,
  } from '/@/api/gm/console';
  import {
    buildMailCampaignDraft,
    buildSystemMailDraft,
    getMailCampaign,
    getMailCampaignAudit,
    getMailCampaigns,
    getSystemMail,
    getSystemMailAudit,
    getSystemMails,
    parseRecipientPlayerIds,
    prepareMailCampaign,
    prepareSystemMail,
    sendMailCampaign,
    sendSystemMail,
  } from '/@/api/gm/systemMail';

  type ReviewKind = 'notice' | 'campaign';

  const props = defineProps<{ busy: boolean; mailEnabled: boolean; currencies: GmCurrencyOption[] }>();
  const emit = defineEmits<{ (event: 'busy-change', busy: boolean): void }>();
  const { hasPermission } = usePermission();
  const { createMessage } = useMessage();

  const activeTab = ref<ReviewKind>('notice');
  const preparing = ref(false);
  const sending = ref(false);
  const blocked = computed(() => props.busy || preparing.value || sending.value);
  const currencyOptions = computed(() => {
    if (props.currencies.length) return props.currencies.map((currency) => ({ value: currency.id, label: currencyLabel(currency.label) }));
    return [{ value: 0, label: '银两' }];
  });
  const mailStatusOptions = [
    { value: 'DRAFT', label: '待确认草稿' },
    { value: 'SENDING', label: '发布中' },
    { value: 'DELIVERED', label: '已发布' },
    { value: 'REJECTED', label: '已拒绝' },
    { value: 'UNKNOWN', label: '结果未知' },
    { value: 'DECLINED', label: '已驳回' },
  ];

  // 全服纯文本公告表单与审计状态
  const noticeForm = reactive({ title: '', body: '', validDays: 7, reason: '' });
  const noticeRequestId = ref('');
  const noticePrepared = ref<GmCommand>();
  const noticePrepareError = ref('');
  const noticeRecords = ref<GmCommand[]>([]);
  const noticeTotal = ref(0);
  const noticePage = ref(1);
  const noticePageSize = ref(20);
  const noticeFilterStatus = ref<string | undefined>(undefined);
  const noticeAuditLoading = ref(false);
  const noticeAuditError = ref('');

  // 批量活动发奖表单与审计状态
  const campaignForm = reactive({ title: '', body: '', validDays: 7, reason: '' });
  const campaignCurrencies = ref<MailCurrencyRow[]>([{ currencyType: 0, amount: '' }]);
  const campaignItems = ref<MailItemRow[]>([]);
  const campaignRecipientText = ref('');
  const campaignRequestId = ref('');
  const campaignPrepared = ref<GmCommand>();
  const campaignPrepareError = ref('');
  const campaignRecords = ref<GmCommand[]>([]);
  const campaignTotal = ref(0);
  const campaignPage = ref(1);
  const campaignPageSize = ref(20);
  const campaignFilterStatus = ref<string | undefined>(undefined);
  const campaignAuditLoading = ref(false);
  const campaignAuditError = ref('');

  // 共享核对弹窗状态
  const reviewKind = ref<ReviewKind>('notice');
  const reviewed = ref<GmCommand>();
  const reviewConfirmed = ref(false);
  const sendError = ref('');
  const unconfirmedCommandId = ref('');
  const refreshingReview = ref(false);
  const auditHistory = ref<GmAuditEntry[]>([]);
  const auditHistoryLoading = ref(false);
  const auditHistoryError = ref('');
  let auditHistoryVersion = 0;
  const [registerModal, { openModal, closeModal }] = useModal();

  const recipientPreview = computed(() => {
    if (!campaignRecipientText.value.trim()) return { summary: '', error: '' };
    try {
      const parsed = parseRecipientPlayerIds(campaignRecipientText.value);
      const dupText = parsed.duplicateCount > 0 ? `（已自动去重 ${parsed.duplicateCount} 条）` : '';
      return {
        summary: `有效收件人 ${parsed.ids.length} 人${dupText}，将切分为 ${parsed.shardCount} 个分片并计算 SHA-256 清单摘要。`,
        error: '',
      };
    } catch (error) {
      return { summary: '', error: errorText(error) };
    }
  });

  const retryingReviewed = computed(
    () => reviewed.value?.status === 'UNKNOWN' || reviewed.value?.id === unconfirmedCommandId.value
  );
  const canSendReviewed = computed(() => {
    const command = reviewed.value;
    return !!command && hasPermission('gm:system-mail:send') && ['DRAFT', 'UNKNOWN'].includes(command.status) && !awaitsAnotherApprover(command);
  });

  function reviewedMailNode(): Record<string, unknown> {
    const request = reviewed.value?.request;
    if (!request) return {};
    if (reviewKind.value === 'campaign') {
      const mail = request.mail;
      return mail && typeof mail === 'object' && !Array.isArray(mail) ? (mail as Record<string, unknown>) : {};
    }
    return request;
  }

  const reviewTitle = computed(() => String(reviewedMailNode().title ?? ''));
  const reviewBody = computed(() => String(reviewedMailNode().body ?? ''));
  const reviewExpireMs = computed(() => reviewedMailNode().expireMs);
  const reviewCurrencies = computed(() => reviewAttachments('currencies'));
  const reviewItems = computed(() => reviewAttachments('items'));
  const reviewRecipientSample = computed(() => {
    const sample = reviewed.value?.request.recipientSample;
    if (!Array.isArray(sample) || !sample.length) return '无';
    const total = Number(reviewed.value?.request.totalRecipients ?? sample.length);
    const suffix = total > sample.length ? ` …（共 ${total} 人）` : '';
    return sample.map(String).join(', ') + suffix;
  });

  function reviewCampaignField(key: string): string {
    return String(reviewedMailNode()[key] ?? '未提供');
  }

  function reviewAttachments(kind: 'currencies' | 'items'): Record<string, unknown>[] {
    const attachments = reviewedMailNode().attachments;
    if (!attachments || typeof attachments !== 'object') return [];
    const entries = (attachments as Record<string, unknown>)[kind];
    return Array.isArray(entries) ? entries.filter((entry) => entry && typeof entry === 'object') : [];
  }

  const noticeColumns: BasicColumn[] = [
    { title: '命令号', dataIndex: 'id', width: 220 },
    { title: '状态', dataIndex: 'status', width: 110, slots: { customRender: 'status' } },
    { title: '公告标题', dataIndex: 'title', width: 220, slots: { customRender: 'title' } },
    { title: '操作人', dataIndex: 'operator', width: 120 },
    { title: '原因', dataIndex: 'reason', width: 200 },
    { title: '创建时间', dataIndex: 'createdAt', width: 190 },
  ];
  const [registerNoticeTable] = useTable({
    columns: noticeColumns,
    rowKey: 'id',
    pagination: false,
    showIndexColumn: false,
    showTableSetting: false,
    useSearchForm: false,
    canResize: false,
    bordered: true,
    actionColumn: { title: '操作', dataIndex: 'action', width: 120, slots: { customRender: 'action' } },
    scroll: { x: 1180 },
  });

  const campaignColumns: BasicColumn[] = [
    { title: '命令号 / 活动 ID', dataIndex: 'id', width: 220 },
    { title: '状态', dataIndex: 'status', width: 110, slots: { customRender: 'status' } },
    { title: '活动标题', dataIndex: 'title', width: 200, slots: { customRender: 'title' } },
    { title: '名单规模', dataIndex: 'recipients', width: 150, slots: { customRender: 'recipients' } },
    { title: '操作人', dataIndex: 'operator', width: 120 },
    { title: '原因', dataIndex: 'reason', width: 190 },
    { title: '创建时间', dataIndex: 'createdAt', width: 190 },
  ];
  const [registerCampaignTable] = useTable({
    columns: campaignColumns,
    rowKey: 'id',
    pagination: false,
    showIndexColumn: false,
    showTableSetting: false,
    useSearchForm: false,
    canResize: false,
    bordered: true,
    actionColumn: { title: '操作', dataIndex: 'action', width: 120, slots: { customRender: 'action' } },
    scroll: { x: 1300 },
  });

  function errorText(error: unknown): string {
    return error instanceof Error && error.message ? error.message : '请求失败，请稍后重试。';
  }
  function pretty(value: unknown): string {
    return JSON.stringify(value ?? null, null, 2);
  }
  function expireTime(value: unknown): string {
    if (typeof value !== 'string' || !/^\d{1,15}$/.test(value)) return '未提供';
    const time = new Date(Number(value));
    return Number.isNaN(time.getTime()) ? '未提供' : time.toLocaleString('zh-CN', { hour12: false });
  }
  function campaignMailField(record: GmCommand, key: string): string {
    const mail = record.request?.mail;
    if (!mail || typeof mail !== 'object') return '';
    return String((mail as Record<string, unknown>)[key] ?? '');
  }
  function mailCurrencyName(type: unknown): string {
    const id = typeof type === 'number' ? type : Number(type);
    const found = props.currencies.find((currency) => currency.id === id);
    if (found) return currencyLabel(found.label);
    return id === 0 ? '银两' : `货币 ${String(type ?? '')}`;
  }
  function statusLabel(status: string): string {
    return (
      ({ DRAFT: '待确认草稿', SENDING: '发布中', DELIVERED: '已发布', REJECTED: '已拒绝', UNKNOWN: '结果未知', ...APPROVAL_STATUS_LABELS } as Record<string, string>)[status] ||
      status
    );
  }
  function statusColor(status: string): string {
    return (
      ({ DRAFT: 'blue', SENDING: 'processing', DELIVERED: 'success', REJECTED: 'error', UNKNOWN: 'warning', ...APPROVAL_STATUS_COLORS } as Record<string, string>)[status] ||
      'default'
    );
  }

  function invalidateNoticeDraft() {
    noticeRequestId.value = '';
    noticePrepared.value = undefined;
    noticePrepareError.value = '';
  }
  function resetNotice() {
    if (blocked.value) return;
    noticeForm.title = '';
    noticeForm.body = '';
    noticeForm.validDays = 7;
    noticeForm.reason = '';
    invalidateNoticeDraft();
  }

  function invalidateCampaignDraft() {
    campaignRequestId.value = '';
    campaignPrepared.value = undefined;
    campaignPrepareError.value = '';
  }
  function addCampaignCurrency() {
    if (blocked.value || campaignCurrencies.value.length >= MAX_MAIL_CURRENCIES) return;
    const used = new Set(campaignCurrencies.value.map((row) => row.currencyType));
    const nextOption = currencyOptions.value.find((option) => !used.has(option.value)) || currencyOptions.value[0];
    campaignCurrencies.value.push({ currencyType: nextOption?.value ?? 0, amount: '' });
    invalidateCampaignDraft();
  }
  function removeCampaignCurrency(index: number) {
    if (blocked.value) return;
    campaignCurrencies.value.splice(index, 1);
    invalidateCampaignDraft();
  }
  function addCampaignItem() {
    if (blocked.value || campaignItems.value.length >= MAX_MAIL_ITEMS) return;
    campaignItems.value.push({ configId: '', count: '' });
    invalidateCampaignDraft();
  }
  function removeCampaignItem(index: number) {
    if (blocked.value) return;
    campaignItems.value.splice(index, 1);
    invalidateCampaignDraft();
  }
  function resetCampaign() {
    if (blocked.value) return;
    campaignForm.title = '';
    campaignForm.body = '';
    campaignForm.validDays = 7;
    campaignForm.reason = '';
    campaignCurrencies.value = [{ currencyType: currencyOptions.value[0]?.value ?? 0, amount: '' }];
    campaignItems.value = [];
    campaignRecipientText.value = '';
    invalidateCampaignDraft();
  }

  function openReview(kind: ReviewKind, command: GmCommand) {
    if (blocked.value) return;
    reviewKind.value = kind;
    reviewed.value = command;
    reviewConfirmed.value = false;
    sendError.value =
      command.id === unconfirmedCommandId.value ? '上次发布未获确认，不会自动重发。请先刷新状态；重试将沿用同一冻结命令号。' : '';
    openModal(true);
    void loadAuditHistory(kind, command.id);
  }

  async function loadAuditHistory(kind: ReviewKind, commandId: string) {
    if (!hasPermission('gm:audit:read')) return;
    const version = ++auditHistoryVersion;
    auditHistoryLoading.value = true;
    auditHistoryError.value = '';
    try {
      const entries = kind === 'campaign' ? await getMailCampaignAudit(commandId) : await getSystemMailAudit(commandId);
      if (version === auditHistoryVersion) auditHistory.value = entries;
    } catch (error) {
      if (version === auditHistoryVersion) {
        auditHistory.value = [];
        auditHistoryError.value = `状态轨迹加载失败。${errorText(error)}`;
      }
    } finally {
      if (version === auditHistoryVersion) auditHistoryLoading.value = false;
    }
  }

  /** 驳回或撤回之后：换上后台返回的最新状态，并刷新轨迹与列表。 */
  async function onReviewedDeclined(command: ApprovalView) {
    if (reviewed.value?.id !== command.id) return;
    const kind = reviewKind.value;
    reviewed.value = command as GmCommand;
    if (kind === 'notice' && noticePrepared.value?.id === command.id) noticePrepared.value = command as GmCommand;
    if (kind === 'campaign' && campaignPrepared.value?.id === command.id) campaignPrepared.value = command as GmCommand;
    reviewConfirmed.value = false;
    await loadAuditHistory(kind, command.id);
    if (kind === 'campaign') await loadCampaignAudit();
    else await loadNoticeAudit();
  }

  async function refreshReviewed() {
    const current = reviewed.value;
    if (!current || !hasPermission('gm:audit:read')) return;
    const kind = reviewKind.value;
    refreshingReview.value = true;
    try {
      const updated = kind === 'campaign' ? await getMailCampaign(current.id) : await getSystemMail(current.id);
      if (reviewed.value?.id === updated.id) {
        if (updated.status !== current.status) reviewConfirmed.value = false;
        reviewed.value = updated;
        if (kind === 'notice' && noticePrepared.value?.id === updated.id) noticePrepared.value = updated;
        if (kind === 'campaign' && campaignPrepared.value?.id === updated.id) campaignPrepared.value = updated;
        if (updated.status !== 'UNKNOWN') {
          if (unconfirmedCommandId.value === updated.id) unconfirmedCommandId.value = '';
          sendError.value = '';
        }
      }
    } catch (error) {
      sendError.value = `刷新原命令状态失败。${errorText(error)}`;
    } finally {
      refreshingReview.value = false;
    }
    await loadAuditHistory(kind, current.id);
    if (kind === 'campaign') await loadCampaignAudit();
    else await loadNoticeAudit();
  }

  async function prepareNotice() {
    if (blocked.value || !props.mailEnabled || !hasPermission('gm:system-mail:prepare')) return;
    if (noticePrepared.value) {
      openReview('notice', noticePrepared.value);
      return;
    }
    preparing.value = true;
    emit('busy-change', true);
    let command: GmCommand | undefined;
    try {
      const draft = buildSystemMailDraft(noticeForm, noticeRequestId.value);
      if (!noticeRequestId.value) noticeRequestId.value = newClientRequestId();
      draft.clientRequestId = noticeRequestId.value;
      command = await prepareSystemMail(draft);
      noticePrepared.value = command;
      noticePrepareError.value = '';
    } catch (error) {
      noticePrepareError.value = noticeRequestId.value
        ? `未确认草稿结果，请求号已保留。再次点击只会获取同一草稿，不会发送邮件。${errorText(error)}`
        : errorText(error);
    } finally {
      preparing.value = false;
      emit('busy-change', false);
    }
    if (command) {
      openReview('notice', command);
      await loadNoticeAudit();
    }
  }

  async function prepareCampaign() {
    if (blocked.value || !props.mailEnabled || !hasPermission('gm:system-mail:prepare')) return;
    if (campaignPrepared.value) {
      openReview('campaign', campaignPrepared.value);
      return;
    }
    preparing.value = true;
    emit('busy-change', true);
    let command: GmCommand | undefined;
    try {
      const draft = buildMailCampaignDraft(
        campaignForm,
        campaignCurrencies.value,
        campaignItems.value,
        campaignRecipientText.value,
        campaignRequestId.value,
        props.currencies
      );
      if (!campaignRequestId.value) campaignRequestId.value = newClientRequestId();
      draft.clientRequestId = campaignRequestId.value;
      command = await prepareMailCampaign(draft);
      campaignPrepared.value = command;
      campaignPrepareError.value = '';
    } catch (error) {
      campaignPrepareError.value = campaignRequestId.value
        ? `未确认活动草稿结果，请求号已保留。再次点击只会获取同一草稿，不会发布活动。${errorText(error)}`
        : errorText(error);
    } finally {
      preparing.value = false;
      emit('busy-change', false);
    }
    if (command) {
      openReview('campaign', command);
      await loadCampaignAudit();
    }
  }

  async function sendReviewed() {
    const command = reviewed.value;
    if (!command || blocked.value || !props.mailEnabled || !reviewConfirmed.value || !canSendReviewed.value) return;
    const kind = reviewKind.value;
    sending.value = true;
    emit('busy-change', true);
    reviewConfirmed.value = false;
    sendError.value = '';
    try {
      const result = kind === 'campaign' ? await sendMailCampaign(command.id) : await sendSystemMail(command.id);
      reviewed.value = result;
      if (kind === 'notice' && noticePrepared.value?.id === result.id) noticePrepared.value = result;
      if (kind === 'campaign' && campaignPrepared.value?.id === result.id) campaignPrepared.value = result;
      unconfirmedCommandId.value = result.status === 'UNKNOWN' ? result.id : '';
      if (result.status === 'DELIVERED') {
        createMessage.success(kind === 'campaign' ? '批量活动发奖已封存发布，名单内玩家领取后到账。' : '全服公告邮件已投递。');
      }
    } catch (error) {
      unconfirmedCommandId.value = command.id;
      sendError.value = `服务器未确认发布结果，不会自动重发。请刷新原命令状态核对。${errorText(error)}`;
    } finally {
      sending.value = false;
      emit('busy-change', false);
    }
    if (kind === 'campaign') await loadCampaignAudit();
    else await loadNoticeAudit();
    if (reviewed.value) await loadAuditHistory(kind, reviewed.value.id);
  }

  function applyNoticeFilter() {
    noticePage.value = 1;
    void loadNoticeAudit();
  }
  function resetNoticeFilter() {
    noticeFilterStatus.value = undefined;
    noticePage.value = 1;
    void loadNoticeAudit();
  }
  function onNoticePageChange(page: number, pageSize: number) {
    noticePage.value = page;
    noticePageSize.value = pageSize;
    void loadNoticeAudit();
  }
  async function loadNoticeAudit() {
    if (noticeAuditLoading.value || !hasPermission('gm:audit:read')) return;
    noticeAuditLoading.value = true;
    try {
      const page = await getSystemMails({ page: noticePage.value, pageSize: noticePageSize.value, status: noticeFilterStatus.value });
      noticeRecords.value = page.records;
      noticeTotal.value = page.total;
      noticeAuditError.value = '';
    } catch (error) {
      noticeAuditError.value = `公告记录刷新失败。${errorText(error)}`;
    } finally {
      noticeAuditLoading.value = false;
    }
  }

  function applyCampaignFilter() {
    campaignPage.value = 1;
    void loadCampaignAudit();
  }
  function resetCampaignFilter() {
    campaignFilterStatus.value = undefined;
    campaignPage.value = 1;
    void loadCampaignAudit();
  }
  function onCampaignPageChange(page: number, pageSize: number) {
    campaignPage.value = page;
    campaignPageSize.value = pageSize;
    void loadCampaignAudit();
  }
  async function loadCampaignAudit() {
    if (campaignAuditLoading.value || !hasPermission('gm:audit:read')) return;
    campaignAuditLoading.value = true;
    try {
      const page = await getMailCampaigns({ page: campaignPage.value, pageSize: campaignPageSize.value, status: campaignFilterStatus.value });
      campaignRecords.value = page.records;
      campaignTotal.value = page.total;
      campaignAuditError.value = '';
    } catch (error) {
      campaignAuditError.value = `活动发奖记录刷新失败。${errorText(error)}`;
    } finally {
      campaignAuditLoading.value = false;
    }
  }

  onMounted(() => {
    void loadNoticeAudit();
    void loadCampaignAudit();
  });
</script>

<style lang="less" scoped>
  .gm-system-mail {
    margin-bottom: 16px;
  }
  .space {
    margin-top: 12px;
  }
  .field-label {
    display: block;
    margin-bottom: 4px;
    font-weight: 500;
  }
  .sub-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 6px;
  }
  .attachment-row {
    display: flex;
    gap: 8px;
    align-items: center;
    margin-bottom: 8px;
  }
  .currency-select {
    min-width: 120px;
  }
  .actions,
  .filters,
  .pagination,
  .timeline-meta {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .actions {
    margin-top: 12px;
  }
  .audit {
    margin-top: 20px;
  }
  .audit-heading {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 10px;
  }
  .audit-heading h3 {
    margin: 0;
  }
  .filters {
    margin-bottom: 10px;
  }
  .filter-select {
    width: 150px;
  }
  .pagination {
    justify-content: space-between;
  }
  .hint {
    color: #64748b;
    font-size: 12px;
    margin: 0;
  }
  .identifier {
    font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
    overflow-wrap: anywhere;
  }
  .value-text {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .json {
    max-height: 300px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: 12px;
    background: #f1f5f9;
    color: #1e293b;
    border-radius: 6px;
  }
  .timeline {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .timeline-item {
    border-left: 2px solid #cbd5e1;
    padding-left: 10px;
  }
</style>
