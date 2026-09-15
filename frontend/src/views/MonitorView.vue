<template>
  <div class="card-panel">
    <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:14px;gap:12px;flex-wrap:wrap">
      <div>
        <h2 style="margin:0 0 4px;font-size:18px">点位监控 · {{ deviceId }}</h2>
        <div class="sub" style="margin:0">Snapshot 定时刷新；写值前先由后端按真实写路径预演</div>
      </div>
      <div style="display:flex;gap:8px;align-items:center">
        <el-switch v-model="autoRefresh" active-text="自动刷新" />
        <el-button :loading="loading" @click="load">刷新</el-button>
        <el-button @click="$router.push('/devices')">返回</el-button>
      </div>
    </div>

    <el-table :data="points" v-loading="loading" empty-text="无点位">
      <el-table-column prop="name" label="点位" min-width="120" />
      <el-table-column prop="address" label="地址" width="80" />
      <el-table-column prop="type" label="类型" min-width="120" />
      <el-table-column label="可写" width="70">
        <template #default="{ row }">
          <el-tag :type="row.writable ? 'success' : 'info'" size="small">{{ row.writable ? '是' : '否' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="值" min-width="120">
        <template #default="{ row }">
          <span class="mono">{{ formatVal(row.value) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="Raw" min-width="120">
        <template #default="{ row }">
          <span class="mono">{{ (row.raw || []).join(',') }}</span>
        </template>
      </el-table-column>
      <el-table-column prop="quality" label="质量" width="90" />
      <el-table-column label="操作" width="100" fixed="right">
        <template #default="{ row }">
          <el-button
            v-if="row.writable"
            type="primary"
            link
            @click="openWrite(row)"
          >{{ auth.canWrite ? '写值' : '预演' }}</el-button>
          <span v-else class="sub">—</span>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="writeVisible" :title="auth.canWrite ? '写点位' : '预演点位（observer 只读）'" width="460px" destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitWrite">
        <el-form-item label="点位">
          <el-input :model-value="current?.name" disabled />
        </el-form-item>
        <el-form-item label="工程值">
          <el-input-number v-model="writeValue" :controls="true" style="width:100%" />
        </el-form-item>
      </el-form>

      <div v-if="previewLoading" class="sub">预演中…</div>
      <template v-else-if="preview">
        <el-alert
          v-if="!preview.ok"
          type="error"
          show-icon
          :closable="false"
          :title="(preview.violations || []).join('；')"
          style="margin-bottom:10px"
        />
        <el-descriptions :column="2" size="small" border>
          <el-descriptions-item label="Raw">{{ preview.raw }}</el-descriptions-item>
          <el-descriptions-item label="寄存器">{{ (preview.registers || []).join(', ') }}</el-descriptions-item>
          <template v-if="preview.mask">
            <el-descriptions-item label="位掩码">{{ hex4(preview.mask.mask) }} (bit {{ preview.mask.bit }})</el-descriptions-item>
            <el-descriptions-item label="当前寄存器">{{ hex4(preview.mask.existing) }}</el-descriptions-item>
            <el-descriptions-item label="保留其它位">{{ hex4(preview.mask.preserved) }}</el-descriptions-item>
            <el-descriptions-item label="合并写入">{{ hex4(preview.mask.merged) }}</el-descriptions-item>
          </template>
          <el-descriptions-item label="写函数">{{ preview.writeFunction === 'multiple' ? '0x10 多寄存器' : '0x06 单寄存器' }}</el-descriptions-item>
          <el-descriptions-item label="令牌过期">{{ expiryText }}</el-descriptions-item>
        </el-descriptions>
      </template>
      <p class="sub" style="margin-top:10px">
        预演与正式写同构：InvertScale → CheckMinMax → EncodeRegisters（bool_bit 先读后写），不落总线。
        提交时后端按同一套规则再校验；越界或 bool_bit 写必须携带未过期的一次性 previewToken。
      </p>
      <template #footer>
        <el-button @click="writeVisible = false">取消</el-button>
        <template v-if="auth.canWrite">
          <el-button type="primary" :disabled="!canSubmit" :loading="writing" @click="submitWrite">提交</el-button>
        </template>
        <span v-else class="sub">observer 仅可预演，不可提交</span>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import api from '../api/client'
import { useAuthStore } from '../stores/auth'

const route = useRoute()
const auth = useAuthStore()
const deviceId = computed(() => route.params.id)
const points = ref([])
const loading = ref(false)
const autoRefresh = ref(true)
const writeVisible = ref(false)
const current = ref(null)
const writeValue = ref(0)
const writing = ref(false)
const preview = ref(null)
const previewLoading = ref(false)
const previewFor = ref(null)
let timer = null
let previewTimer = null

function formatVal(v) {
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  if (typeof v === 'number') return Number.isInteger(v) ? String(v) : v.toFixed(4)
  return String(v ?? '')
}

function hex4(v) {
  if (typeof v !== 'number') return ''
  return '0x' + v.toString(16).toUpperCase().padStart(4, '0')
}

const expiryText = computed(() => {
  if (!preview.value?.expiresAt) return ''
  const d = new Date(preview.value.expiresAt)
  const text = Number.isNaN(d.getTime()) ? preview.value.expiresAt : d.toLocaleTimeString()
  return `${text}（${preview.value.ttlSeconds}s 内有效）`
})

// 提交条件：engineer + 预演通过 + 预演对应当前输入（值一变需重新预演）
const canSubmit = computed(() =>
  auth.canWrite &&
  !writing.value &&
  preview.value?.ok &&
  previewFor.value &&
  previewFor.value.name === current.value?.name &&
  previewFor.value.value === writeValue.value
)

async function load() {
  loading.value = true
  try {
    const { data } = await api.get(`/devices/${deviceId.value}/snapshot`)
    points.value = data.points || []
  } catch (e) {
    ElMessage.error(e.response?.data?.error || 'snapshot 失败')
  } finally {
    loading.value = false
  }
}

function openWrite(row) {
  current.value = row
  writeValue.value = typeof row.value === 'boolean' ? (row.value ? 1 : 0) : Number(row.value) || 0
  preview.value = null
  previewFor.value = null
  writeVisible.value = true
  runPreview()
}

// 输入即预演（防抖 300ms）
watch(writeValue, () => {
  if (!writeVisible.value) return
  preview.value = null
  previewFor.value = null
  if (previewTimer) clearTimeout(previewTimer)
  previewTimer = setTimeout(runPreview, 300)
})

watch(writeVisible, (v) => {
  if (!v && previewTimer) {
    clearTimeout(previewTimer)
    previewTimer = null
  }
})

async function runPreview() {
  if (!current.value || writeValue.value === null || writeValue.value === undefined) return
  previewLoading.value = true
  try {
    const { data } = await api.post(
      `/devices/${deviceId.value}/points/${current.value.name}/preview`,
      { value: writeValue.value }
    )
    preview.value = data
    previewFor.value = { name: current.value.name, value: writeValue.value }
  } catch (e) {
    preview.value = null
    previewFor.value = null
    ElMessage.error(e.response?.data?.error || '预演失败')
  } finally {
    previewLoading.value = false
  }
}

async function submitWrite() {
  if (!canSubmit.value) return
  writing.value = true
  try {
    await api.put(`/devices/${deviceId.value}/points/${current.value.name}`, {
      value: writeValue.value,
      previewToken: preview.value.previewToken
    })
    ElMessage.success('写值成功')
    writeVisible.value = false
    await load()
  } catch (e) {
    ElMessage.error(e.response?.data?.error || '写值失败')
    // 令牌可能已过期或被消费，重新预演取新令牌
    runPreview()
  } finally {
    writing.value = false
  }
}

function setupTimer() {
  if (timer) clearInterval(timer)
  timer = null
  if (autoRefresh.value) {
    timer = setInterval(load, 3000)
  }
}

watch(autoRefresh, setupTimer)
onMounted(() => {
  load()
  setupTimer()
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (previewTimer) clearTimeout(previewTimer)
})
</script>
