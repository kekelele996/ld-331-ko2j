<template>
  <div>
    <div class="page-title">
      <div>
        <h1>调班与替班申请</h1>
        <p>主管审批通过后，系统在一个事务内交换双方排班；存在夜班接白班、连续工作超限或同日重复排班时整次拒绝。</p>
      </div>
      <el-button v-if="auth.role==='staff'" type="primary" @click="openCreate">发起调班申请</el-button>
    </div>

    <el-card>
      <el-table v-loading="loading" :data="rows">
        <el-table-column prop="id" label="#" width="64" />
        <el-table-column label="申请人班次" min-width="180">
          <template #default="scope">
            <div>{{ scope.row.applicant?.name }}：{{ formatSlot(scope.row.schedule) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="替班人班次" min-width="180">
          <template #default="scope">
            <div>{{ scope.row.substitute?.name }}：{{ formatSlot(scope.row.substitute_schedule) }}</div>
          </template>
        </el-table-column>
        <el-table-column prop="reason" label="调班原因" min-width="180" show-overflow-tooltip />
        <el-table-column label="状态" width="110">
          <template #default="scope">
            <el-tag :type="statusType(scope.row.status)">{{ statusText(scope.row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column v-if="auth.role!=='staff'" label="操作" width="170">
          <template #default="scope">
            <el-button size="small" type="success" :disabled="scope.row.status!=='pending'"
                       @click="review(scope.row, true)">同意</el-button>
            <el-button size="small" type="danger" :disabled="scope.row.status!=='pending'"
                       @click="review(scope.row, false)">驳回</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="creating" title="发起调班申请" width="520px">
      <el-form label-width="110px">
        <el-form-item label="我的班次" required>
          <el-select v-model="form.schedule_id" placeholder="选择要换出的本人班次" style="width:100%">
            <el-option v-for="s in mySchedules" :key="s.id"
                       :value="s.id" :label="formatOption(s)" />
          </el-select>
        </el-form-item>
        <el-form-item label="替班人" required>
          <el-select v-model="form.substitute_id" placeholder="选择替班同事" style="width:100%"
                     @change="onSubstituteChange">
            <el-option v-for="p in colleagues" :key="p.id" :value="p.id" :label="p.name" />
          </el-select>
        </el-form-item>
        <el-form-item label="替班人班次" required>
          <el-select v-model="form.substitute_schedule_id" placeholder="选择替班人名下被调换的班次"
                     style="width:100%">
            <el-option v-for="s in substituteSchedules" :key="s.id"
                       :value="s.id" :label="formatOption(s)" />
          </el-select>
        </el-form-item>
        <el-form-item label="调班原因" required>
          <el-input v-model="form.reason" type="textarea" :rows="3" maxlength="300" show-word-limit
                    placeholder="请说明调班原因（2-300 字）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="creating=false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submitCreate">提交申请</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import {onMounted, ref} from 'vue'
import {ElMessage, ElMessageBox} from 'element-plus'
import api from '../api/http'
import {auth} from '../stores/auth'

const rows = ref<any[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    rows.value = await api.get('/shift-requests')
  } finally {
    loading.value = false
  }
}

function statusText(s: string) {
  return s === 'approved' ? '已通过' : s === 'rejected' ? '已驳回' : '待审批'
}
function statusType(s: string) {
  return s === 'approved' ? 'success' : s === 'rejected' ? 'info' : 'warning'
}
function formatSlot(s: any) {
  if (!s || !s.work_date) return '（排班不存在）'
  return `${s.work_date.slice(0, 10)} ${s.shift?.name || '未知班次'}`
}
function formatOption(s: any) {
  return `${s.work_date.slice(0, 10)} ${s.shift?.name || ''}班`
}

async function review(row: any, approved: boolean) {
  try {
    await api.put(`/shift-requests/${row.id}/review`, {approved})
    ElMessage.success(approved ? '审批通过，双方排班已交换' : '已驳回该申请')
    await load()
  } catch (error: any) {
    // 合规冲突 / 重复审批：后端返回具体冲突说明，弹窗逐条展示并刷新到最新班表状态。
    ElMessageBox.alert(error.message, approved ? '审批未通过' : '操作失败', {
      type: 'error',
      confirmButtonText: '我知道了',
    }).catch(() => undefined)
    await load()
  }
}

// ---------------- 发起申请 ----------------
const creating = ref(false)
const submitting = ref(false)
const colleagues = ref<any[]>([])
const mySchedules = ref<any[]>([])
const substituteSchedules = ref<any[]>([])
const form = ref({schedule_id: 0 as number, substitute_id: 0 as number, substitute_schedule_id: 0 as number, reason: ''})

async function openCreate() {
  form.value = {schedule_id: 0, substitute_id: 0, substitute_schedule_id: 0, reason: ''}
  substituteSchedules.value = []
  creating.value = true
  const [staff, mine]: any = await Promise.all([
    api.get('/staff'),
    api.get('/schedules', {params: {staff_id: auth.staffId}}),
  ])
  colleagues.value = staff.filter((p: any) => p.id !== auth.staffId)
  mySchedules.value = mine
}

async function onSubstituteChange(id: number) {
  form.value.substitute_schedule_id = 0
  if (!id) {
    substituteSchedules.value = []
    return
  }
  const list: any = await api.get('/schedules', {params: {staff_id: id}})
  substituteSchedules.value = list
}

async function submitCreate() {
  if (!form.value.schedule_id || !form.value.substitute_id || !form.value.substitute_schedule_id) {
    ElMessage.warning('请完整选择双方班次')
    return
  }
  if (form.value.reason.trim().length < 2) {
    ElMessage.warning('请填写至少 2 个字的调班原因')
    return
  }
  submitting.value = true
  try {
    await api.post('/shift-requests', form.value)
    ElMessage.success('调班申请已提交，等待主管审批')
    creating.value = false
    await load()
  } catch (error: any) {
    ElMessage.error(error.message)
  } finally {
    submitting.value = false
  }
}

onMounted(load)
</script>
