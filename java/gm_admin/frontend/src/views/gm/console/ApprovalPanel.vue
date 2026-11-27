<template>
  <div class="approval-panel">
    <a-descriptions :column="1" bordered size="small">
      <a-descriptions-item label="发起人">{{ command.operator }}</a-descriptions-item>
      <a-descriptions-item label="批准人">{{ command.approver || (dualControl ? '尚未批准' : '未开启双人复核') }}</a-descriptions-item>
      <a-descriptions-item v-if="command.approvedAt" label="批准时间">{{ command.approvedAt }}</a-descriptions-item>
    </a-descriptions>
    <a-alert
      v-if="ownPending"
      class="approval-space"
      type="info"
      show-icon
      message="这是你发起的申请，需要另一名管理员批准后才会执行。"
    />
    <div v-if="canDecline" class="approval-decline">
      <a-input
        v-model:value="reason"
        :maxlength="191"
        :disabled="busy || declining"
        :placeholder="own ? '撤回原因（必填）' : '驳回原因（必填）'"
        :aria-label="own ? '撤回原因' : '驳回原因'"
      />
      <a-button danger :loading="declining" :disabled="busy || !reason.trim()" @click="decline">
        {{ own ? '撤回申请' : '驳回申请' }}
      </a-button>
    </div>
    <a-alert v-if="failure" class="approval-space" type="warning" show-icon :message="failure" />
  </div>
</template>

<script lang="ts" setup name="gm-approval-panel">
  import { computed, ref, watch } from 'vue';
  import { awaitsAnotherApprover, currentOperator, declineCommand, dualControl, type ApprovalView, type DeclinableKind } from '/@/api/gm/approval';

  const props = defineProps<{
    command: ApprovalView;
    kind: DeclinableKind;
    // 持有这类命令的放行权限：可以驳回任何人的待批申请。
    canRelease: boolean;
    // 只持有发起权限：只能撤回自己发起的申请。
    canPropose: boolean;
    busy: boolean;
  }>();
  const emit = defineEmits<{ (event: 'declined', command: ApprovalView): void }>();

  const own = computed(() => props.command.operator === currentOperator());
  const ownPending = computed(() => awaitsAnotherApprover(props.command));
  const canDecline = computed(() => props.command.status === 'DRAFT' && (props.canRelease || (props.canPropose && own.value)));
  const reason = ref('');
  const declining = ref(false);
  const failure = ref('');

  // 换了一条命令，上一条填到一半的原因与报错不能带过来。
  watch(
    () => props.command.id,
    () => {
      reason.value = '';
      failure.value = '';
    }
  );

  async function decline() {
    const text = reason.value.trim();
    if (!text || declining.value) return;
    declining.value = true;
    failure.value = '';
    try {
      emit('declined', await declineCommand(props.kind, props.command.id, text));
      reason.value = '';
    } catch (error) {
      const message = (error as { response?: { data?: { message?: unknown } } })?.response?.data?.message ?? (error as Error)?.message;
      failure.value = typeof message === 'string' && message.length > 0 ? message : '操作未完成，请刷新后核对这条申请的状态';
    } finally {
      declining.value = false;
    }
  }
</script>

<style lang="less" scoped>
  .approval-panel {
    margin-top: 12px;
  }

  .approval-space {
    margin-top: 12px;
  }

  .approval-decline {
    display: flex;
    gap: 8px;
    margin-top: 12px;
  }
</style>
