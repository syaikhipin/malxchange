<template>
  <section class="withdrawal-detail">
    <div class="crumbs">
      <NuxtLink to="/finance/withdrawals">Withdrawals</NuxtLink>
      <span class="sep">&gt;</span>
      <span class="mono">{{ id }}</span>
    </div>

    <p v-if="loading" class="state">Loading this withdrawal…</p>
    <el-alert v-else-if="error" class="error" type="error" :closable="false" :title="error" />
    <template v-else-if="withdrawal">
      <h1>Withdrawal <span class="mono">{{ withdrawal.id }}</span></h1>
      <p class="state-line">
        <el-tag :type="stateTag[withdrawal.state] ?? 'info'">{{ withdrawal.state }}</el-tag>
        <span v-if="withdrawal.error" class="fail-reason">{{ withdrawal.error }}</span>
      </p>

      <el-descriptions :column="2" border>
        <el-descriptions-item label="Member">{{ withdrawal.member }}</el-descriptions-item>
        <el-descriptions-item label="Coin">{{ withdrawal.coin }}</el-descriptions-item>
        <el-descriptions-item label="Address">
          <span class="mono">{{ withdrawal.address }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="Amount">{{ withdrawal.amount }}</el-descriptions-item>
        <el-descriptions-item label="Fee">{{ withdrawal.fee }}</el-descriptions-item>
        <el-descriptions-item label="Net">{{ withdrawal.net }}</el-descriptions-item>
        <el-descriptions-item label="Transaction ID">
          <span class="mono">{{ withdrawal.txid !== '' ? withdrawal.txid : '—' }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="Created">{{ withdrawal.createdAt }}</el-descriptions-item>
        <el-descriptions-item label="Updated">{{ withdrawal.updatedAt }}</el-descriptions-item>
      </el-descriptions>

      <div class="actions">
        <el-button
          v-if="withdrawal.state === 'audit'"
          type="success"
          :loading="acting !== ''"
          @click="openConfirm('approve')"
        >
          Approve
        </el-button>
        <el-button
          v-if="withdrawal.state === 'audit'"
          type="warning"
          :loading="acting !== ''"
          @click="rejectWithdrawal"
        >
          Reject
        </el-button>
        <el-button
          v-if="withdrawal.state === 'failed'"
          type="primary"
          :loading="acting !== ''"
          @click="openConfirm('retry')"
        >
          Retry broadcast
        </el-button>
        <el-button
          v-if="withdrawal.state === 'failed'"
          type="warning"
          :loading="acting !== ''"
          @click="refundWithdrawal"
        >
          Refund member
        </el-button>
        <span v-if="withdrawal.state !== 'audit' && withdrawal.state !== 'failed'" class="hint">
          No decision is open: this withdrawal has left the audit state.
        </span>
      </div>

      <el-alert v-if="actionError" class="error" type="error" :closable="false" :title="actionError" />

      <!-- Step-up (ADR-0012): approve and retry demand a fresh
           authenticator code of the acting Employee. -->
      <el-dialog
        v-model="confirmVisible"
        :title="confirmVerb === 'approve' ? 'Approve withdrawal' : 'Retry broadcast'"
        width="420px"
      >
        <p class="dialog-hint">
          Enter a fresh code from your authenticator app to confirm. The withdrawal
          {{ confirmVerb === 'approve' ? 'is sent to the signer' : 're-enters the signing queue' }}.
        </p>
        <el-input
          v-model="stepUpCode"
          name="code"
          inputmode="numeric"
          placeholder="6-digit code"
          maxlength="6"
        />
        <template #footer>
          <el-button @click="confirmVisible = false">Cancel</el-button>
          <el-button type="primary" :loading="acting !== ''" @click="submitWithStepUp">Confirm</el-button>
        </template>
      </el-dialog>
    </template>
  </section>
</template>

<script setup lang="ts">
// The withdrawal detail (issue #59): the full record plus the finance
// decisions — approve / reject from audit, retry / refund from failed.
// Approve and retry demand Step-up (ADR-0012); every decision is
// audit-logged server-side with the acting Employee as the actor.
definePageMeta({ middleware: 'employee', props: true })

const props = defineProps<{ id: string }>()

interface WithdrawalDetail {
  id: string
  coin: string
  member: string
  address: string
  amount: string
  fee: string
  net: string
  state: string
  txid: string
  error: string
  createdAt: string
  updatedAt: string
}

const stateTag: Record<string, string> = {
  audit: 'warning',
  approved: 'primary',
  signed: 'primary',
  broadcast: 'primary',
  confirmed: 'success',
  rejected: 'info',
  failed: 'danger',
  refunded: 'info',
}

const { apiFetch } = useApi()

const withdrawal = ref<WithdrawalDetail | null>(null)
const loading = ref(false)
const error = ref('')
const actionError = ref('')
const acting = ref('')

const confirmVisible = ref(false)
const confirmVerb = ref<'approve' | 'retry'>('approve')
const stepUpCode = ref('')

useHead({ title: 'Withdrawal detail' })

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    withdrawal.value = await apiFetch<WithdrawalDetail>(`/api/withdrawals/${props.id}`)
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'This withdrawal could not be loaded.'
  } finally {
    loading.value = false
  }
}

function openConfirm(verb: 'approve' | 'retry'): void {
  confirmVerb.value = verb
  stepUpCode.value = ''
  confirmVisible.value = true
}

async function act(path: string, body: unknown): Promise<boolean> {
  acting.value = path
  actionError.value = ''
  try {
    await apiFetch(`/api/withdrawals/${props.id}/${path}`, { method: 'POST', body })
    await load()
    return true
  } catch (e) {
    actionError.value = e instanceof Error ? e.message : 'The action failed. Try again.'
    return false
  } finally {
    acting.value = ''
  }
}

async function submitWithStepUp(): Promise<void> {
  if (stepUpCode.value.trim() === '') {
    actionError.value = 'Enter a fresh authenticator code.'
    return
  }
  const done = await act(confirmVerb.value, { code: stepUpCode.value.trim() })
  if (done) {
    confirmVisible.value = false
  }
}

async function rejectWithdrawal(): Promise<void> {
  await act('reject', {})
}

async function refundWithdrawal(): Promise<void> {
  await act('refund', {})
}

onMounted(load)
</script>

<style scoped lang="scss">
.withdrawal-detail {
  .crumbs {
    display: flex;
    gap: 6px;
    align-items: center;
    font-size: 13px;
    margin: 0 0 12px;

    .sep {
      color: var(--text-muted);
    }

    .mono {
      font-family: var(--mono-font, monospace);
    }
  }

  h1 {
    font-size: 20px;
    font-weight: 600;
    margin: 0 0 8px;

    .mono {
      font-family: var(--mono-font, monospace);
      font-size: 14px;
      font-weight: 400;
    }
  }

  .state-line {
    display: flex;
    gap: 10px;
    align-items: center;
    margin: 0 0 16px;

    .fail-reason {
      color: var(--text-secondary);
      font-size: 13px;
    }
  }

  .actions {
    display: flex;
    gap: 10px;
    align-items: center;
    margin: 16px 0 8px;

    .hint {
      color: var(--text-secondary);
      font-size: 13px;
    }
  }

  .error {
    margin: 12px 0 0;
  }

  .dialog-hint {
    color: var(--text-secondary);
    font-size: 13px;
    margin: 0 0 12px;
  }

  .mono {
    font-family: var(--mono-font, monospace);
    font-size: 12px;
  }
}
</style>
