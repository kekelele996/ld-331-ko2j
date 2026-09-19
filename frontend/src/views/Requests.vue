<template>
  <div>
    <div class="page-title">
      <div>
        <h1>调班与替班申请</h1>
        <p>主管审批后，系统会在一个事务内同时交换双方排班并更新申请状态。</p>
      </div>
      <el-button v-if="auth.role === 'staff'" type="primary" @click="openCreate">发起调班</el-button>
    </div>
    <el-card>
      <el-table :data="rows" v-loading="loading" row-key="id">
        <el-table-column prop="id" label="#" width="70" />
        <el-table-column label="申请人" width="110">
          <template #default="scope">{{ scope.row.applicant?.name }}</template>
        </el-table-column>
        <el-table-column label="申请人班次" width="180">
          <template #default="scope">
            <span v-if="scope.row.schedule">
              {{ scope.row.schedule.work_date?.slice(0, 10) }}
              {{ scope.row.schedule.shift?.name }}
            </span>
            <span v-else class="muted">排班已不存在</span>
          </template>
        </el-table-column>
        <el-table-column label="替班人" width="110">
          <template #default="scope">{{ scope.row.substitute?.name }}</template>
        </el-table-column>
        <el-table-column label="替班人班次" width="180">
          <template #default="scope">
            <span v-if="scope.row.substitute_schedule">
              {{ scope.row.substitute_schedule.work_date?.slice(0, 10) }}
              {{ scope.row.substitute_schedule.shift?.name }}
            </span>
            <span v-else class="muted">排班已不存在</span>
          </template>
        </el-table-column>
        <el-table-column prop="reason" label="调班原因" />
        <el-table-column label="状态" width="100">
          <template #default="scope">
            <el-tag :type="statusType(scope.row.status)" size="small">
              {{ statusText(scope.row.status) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column v-if="auth.role !== 'staff'" label="操作" width="160">
          <template #default="scope">
            <el-button size="small" type="success"
                       :disabled="scope.row.status !== 'pending'"
                       @click="review(scope.row, true)">同意</el-button>
            <el-button size="small" type="danger" plain
                       :disabled="scope.row.status !== 'pending'"
                       @click="review(scope.row, false)">驳回</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="createVisible" title="发起调班" width="520px">
      <el-form :model="form" label-width="110px">
        <el-form-item label="我的班次" required>
          <el-select v-model="form.schedule_id" placeholder="只能选择本人班次" filterable style="width: 100%">
            <el-option v-for="s in mySchedules" :key="s.id"
                       :value="s.id"
                       :label="`${s.work_date.slice(0, 10)} ${s.shift?.name || ''}`" />
          </el-select>
        </el-form-item>
        <el-form-item label="替班人" required>
          <el-select v-model="form.substitute_id" placeholder="选择替班人" filterable style="width: 100%"
                     @change="onSubstituteChange">
            <el-option v-for="p in substituteOptions" :key="p.id" :value="p.id" :label="p.name" />
          </el-select>
        </el-form-item>
        <el-form-item label="替班班次" required>
          <el-select v-model="form.substitute_schedule_id" placeholder="替班人必须对应指定班次" filterable style="width: 100%">
            <el-option v-for="s in substituteSchedules" :key="s.id"
                       :value="s.id"
                       :label="`${s.work_date.slice(0, 10)} ${s.shift?.name || ''}`" />
          </el-select>
        </el-form-item>
        <el-form-item label="调班原因" required>
          <el-input v-model="form.reason" type="textarea" :rows="3" maxlength="300" show-word-limit />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submitCreate">提交申请</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { ElMessage } from 'element-plus';
import api from '../api/http';
import { auth } from '../stores/auth';

const rows = ref<any[]>([]);
const loading = ref(false);

async function load() {
  loading.value = true;
  try {
    rows.value = await api.get('/shift-requests');
  } finally {
    loading.value = false;
  }
}

async function review(row: any, approved: boolean) {
  try {
    await api.put(`/shift-requests/${row.id}/review`, { approved });
    ElMessage.success(approved ? '审批通过，双方排班已交换' : '已驳回该申请');
  } catch (error: any) {
    // 并发审批可能已被另一端抢先处理：同样重新拉取，保证页面与班表一致。
    ElMessage.error(error.message);
  }
  await load();
}

function statusText(status: string) {
  return { pending: '待审批', approved: '已通过', rejected: '已驳回' }[status] || status;
}
function statusType(status: string) {
  return { pending: 'warning', approved: 'success', rejected: 'info' }[status] || '';
}

// ---- 发起调班 ----
const createVisible = ref(false);
const submitting = ref(false);
const mySchedules = ref<any[]>([]);
const substituteSchedules = ref<any[]>([]);
const substituteOptions = ref<any[]>([]);
const form = ref({ schedule_id: 0 as number, substitute_id: 0 as number, substitute_schedule_id: 0 as number, reason: '' });

async function openCreate() {
  form.value = { schedule_id: 0, substitute_id: 0, substitute_schedule_id: 0, reason: '' };
  substituteSchedules.value = [];
  createVisible.value = true;
  const [mine, staff]: any[] = await Promise.all([
    api.get('/schedules', { params: { staff_id: auth.staffId } }),
    api.get('/staff'),
  ]);
  mySchedules.value = mine.filter((s: any) => s.shift?.kind !== 'rest');
  substituteOptions.value = staff.filter((p: any) => p.id !== auth.staffId);
}

async function onSubstituteChange() {
  form.value.substitute_schedule_id = 0;
  if (!form.value.substitute_id) {
    substituteSchedules.value = [];
    return;
  }
  const list: any = await api.get('/schedules', { params: { staff_id: form.value.substitute_id } });
  substituteSchedules.value = list.filter((s: any) => s.shift?.kind !== 'rest');
}

async function submitCreate() {
  if (!form.value.schedule_id || !form.value.substitute_id || !form.value.substitute_schedule_id) {
    ElMessage.warning('请完整选择本人班次、替班人和替班班次');
    return;
  }
  if (form.value.schedule_id === form.value.substitute_schedule_id) {
    ElMessage.warning('本人班次与替班班次不能是同一条排班');
    return;
  }
  if (form.value.reason.trim().length < 2) {
    ElMessage.warning('请填写至少 2 个字的调班原因');
    return;
  }
  submitting.value = true;
  try {
    await api.post('/shift-requests', form.value);
    ElMessage.success('调班申请已提交，等待主管审批');
    createVisible.value = false;
    await load();
  } catch (error: any) {
    ElMessage.error(error.message);
  } finally {
    submitting.value = false;
  }
}

onMounted(load);
</script>

<style scoped>
.muted {
  color: var(--el-text-color-placeholder);
}
</style>
