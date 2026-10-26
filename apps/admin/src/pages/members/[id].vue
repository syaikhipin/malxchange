<template>
  <section class="member-detail">
    <p class="crumb"><NuxtLink to="/members">Members</NuxtLink> / {{ id }}</p>
    <h1>Member detail</h1>

    <p v-if="error" class="error" data-test="member-detail-error">{{ error }}</p>

    <el-descriptions v-else-if="member" :column="2" border data-test="member-descriptions">
      <el-descriptions-item label="Email">{{ member.email }}</el-descriptions-item>
      <el-descriptions-item label="Status">
        <el-tag :type="member.status === 'locked' ? 'danger' : 'success'" size="small">
          {{ member.status }}
        </el-tag>
      </el-descriptions-item>
      <el-descriptions-item label="KYC state">
        <el-tag size="small">{{ member.kycStatus }}</el-tag>
      </el-descriptions-item>
      <el-descriptions-item label="Authenticator">
        {{ member.authenticatorBound ? 'Bound' : 'Not bound' }}
      </el-descriptions-item>
      <el-descriptions-item label="Code channels">
        <span class="channels" data-test="member-channels">
          Email
          <el-tag v-if="member.phone" size="small" class="channel">SMS</el-tag>
          <el-tag v-if="member.telegramBound" size="small" class="channel">Telegram</el-tag>
          <span v-if="!member.phone && !member.telegramBound" class="channel none">
            Email only
          </span>
        </span>
      </el-descriptions-item>
      <el-descriptions-item label="Registered">{{ member.createdAt }}</el-descriptions-item>
    </el-descriptions>

    <div v-if="member" class="actions">
      <el-button
        v-if="member.status === 'active'"
        type="danger"
        data-test="member-lock"
        :loading="acting"
        @click="lock"
      >
        Lock member
      </el-button>
      <el-button
        v-else
        type="warning"
        data-test="member-unlock"
        :loading="acting"
        @click="unlock"
      >
        Unlock member
      </el-button>
      <el-button
        type="danger"
        plain
        data-test="member-reset-open"
        :disabled="member.kycStatus !== 'verified'"
        @click="resetOpen = true"
      >
        Reset authenticator
      </el-button>
      <p v-if="member.kycStatus !== 'verified'" class="hint">
        Authenticator recovery is only possible for KYC-verified members.
      </p>
    </div>

    <el-dialog
      v-model="resetOpen"
      title="Reset authenticator"
      width="440px"
      data-test="member-reset-dialog"
    >
      <p class="dialog-copy">
        This deletes the member's Authenticator binding and notifies them on every active Code
        channel. Confirm with a fresh code from <strong>your own</strong> authenticator app.
      </p>
      <el-input
        v-model="resetCode"
        data-test="member-reset-code"
        placeholder="Authenticator code"
        inputmode="numeric"
      />
      <p v-if="resetError" class="error" data-test="member-reset-error">{{ resetError }}</p>
      <template #footer>
        <el-button @click="resetOpen = false">Cancel</el-button>
        <el-button
          type="danger"
          data-test="member-reset-confirm"
          :loading="resetting"
          @click="reset"
        >
          Reset authenticator
        </el-button>
      </template>
    </el-dialog>
  </section>
</template>

<script setup lang="ts">
// Member detail (issue #58): api's member view RPC via admin-api, with
// the support lock/unlock actions and the compliance Authenticator
// reset (Step-up-gated; api refuses anything but KYC-verified members
// and notifies the member's active Code channels).
definePageMeta({ middleware: 'employee', props: true })

const props = defineProps<{ id: string }>()

interface MemberDetail {
  id: string
  email: string
  status: string
  phone: string
  kycStatus: string
  authenticatorBound: boolean
  telegramBound: boolean
  createdAt: string
}

const { apiFetch } = useApi()

const member = ref<MemberDetail | null>(null)
const error = ref('')
const acting = ref(false)

const resetOpen = ref(false)
const resetCode = ref('')
const resetError = ref('')
const resetting = ref(false)

useHead({ title: 'Member detail' })

onMounted(load)

async function load(): Promise<void> {
  error.value = ''
  try {
    member.value = await apiFetch<MemberDetail>(`/api/members/${props.id}`)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'This member could not be loaded.'
  }
}

async function lock(): Promise<void> {
  await setStatus('/lock')
}

async function unlock(): Promise<void> {
  await setStatus('/unlock')
}

async function setStatus(action: string): Promise<void> {
  error.value = ''
  acting.value = true
  try {
    await apiFetch(`/api/members/${props.id}${action}`, { method: 'POST', body: {} })
    await load()
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'The action failed. Try again.'
  } finally {
    acting.value = false
  }
}

async function reset(): Promise<void> {
  resetError.value = ''
  if (resetCode.value.trim() === '') {
    resetError.value = 'A fresh authenticator code is required for this action.'
    return
  }
  resetting.value = true
  try {
    await apiFetch(`/api/members/${props.id}/totp/reset`, {
      method: 'POST',
      body: { code: resetCode.value.trim() },
    })
    resetOpen.value = false
    resetCode.value = ''
    await load()
  } catch (cause) {
    resetError.value = cause instanceof Error ? cause.message : 'The reset failed. Try again.'
  } finally {
    resetting.value = false
  }
}
</script>

<style scoped lang="scss">
.member-detail {
  h1 {
    font-size: 20px;
    font-weight: 600;
    margin: 0 0 16px;
  }

  .crumb {
    color: var(--text-secondary);
    font-size: 13px;
    margin: 0 0 4px;

    a {
      color: var(--accent);
      text-decoration: none;
    }
  }

  .error {
    color: var(--el-color-danger, #f56c6c);
    font-size: 13px;
    margin: 0 0 8px;
  }

  .hint {
    color: var(--text-muted);
    font-size: 12px;
    margin: 8px 0 0;
  }

  .channels .channel {
    margin-left: 6px;

    &.none {
      color: var(--text-muted);
      font-size: 12px;
    }
  }

  .actions {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-top: 20px;
    flex-wrap: wrap;
  }

  .dialog-copy {
    color: var(--text-secondary);
    font-size: 13px;
    margin: 0 0 12px;
  }
}
</style>
