package service

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/gbsched/hospital-scheduler/internal/model"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const dateLayout = "2006-01-02"

type ShiftRequestService struct {
	repo         *repository.ShiftRequestRepository
	scheduleRepo *repository.ScheduleRepository
	ruleRepo     *repository.ScheduleRuleRepository
	auditRepo    *repository.AuditLogRepository
	db           *gorm.DB
	logger       *slog.Logger
}

func NewShiftRequestService(r *repository.ShiftRequestRepository, s *repository.ScheduleRepository, rr *repository.ScheduleRuleRepository, ar *repository.AuditLogRepository, db *gorm.DB, l *slog.Logger) *ShiftRequestService {
	return &ShiftRequestService{r, s, rr, ar, db, l}
}

// writeAudit 在审批事务内留痕；若写审计失败则审批一并回滚，避免“排班已换但无记录”。
func (s *ShiftRequestService) writeAudit(tx *gorm.DB, actor uint, action, detail string) error {
	if s.auditRepo == nil {
		return nil
	}
	if e := s.auditRepo.CreateOn(tx, &model.AuditLog{ActorID: actor, Action: action, Detail: detail}); e != nil {
		return fmt.Errorf("write audit log: %w", e)
	}
	return nil
}

func (s *ShiftRequestService) List() ([]model.ShiftRequest, error) {
	v, e := s.repo.List()
	if e != nil {
		return nil, fmt.Errorf("list shift requests: %w", e)
	}
	return v, nil
}

// Create 校验申请人只能提交本人班次、替班人必须对应其本人被指定的班次，
// 申请初始状态一律为 pending，等待主管审批。
func (s *ShiftRequestService) Create(v model.ShiftRequest, actor uint) error {
	a, e := s.scheduleRepo.Find(v.ScheduleID)
	if e != nil {
		return fmt.Errorf("find applicant schedule: %w", e)
	}
	b, e := s.scheduleRepo.Find(v.SubstituteScheduleID)
	if e != nil {
		return fmt.Errorf("find substitute schedule: %w", e)
	}
	switch {
	case v.ScheduleID == v.SubstituteScheduleID:
		return fmt.Errorf("申请人班次与替班人班次不能是同一条排班")
	case actor == v.SubstituteID:
		return fmt.Errorf("申请人与替班人不能是同一人")
	case a.StaffID != actor:
		return fmt.Errorf("申请人只能申请调换本人的班次")
	case b.StaffID != v.SubstituteID:
		return fmt.Errorf("替班人必须对应其本人被指定的班次")
	}
	v.ApplicantID = actor
	v.Status = model.RequestPending
	if e = s.repo.Create(&v); e != nil {
		return fmt.Errorf("create shift request: %w", e)
	}
	return nil
}

// Review 在单个数据库事务内完成审批：
//   - 驳回：仅原子更新申请状态，重复/并发审批只会有一次生效；
//   - 通过：锁定申请行与双方排班行 -> 复核归属 -> 在内存中模拟交换并做合规检测
//     （夜班后接白班、连续工作天数超限、同日重复排班）-> 交换双方排班 -> 更新申请状态。
//
// 任一环节失败整事务回滚，排班与申请状态不会出现部分生效。
// MySQL 下多个审批事务可能以不同顺序锁定同一人员的排班行而触发 InnoDB 死锁，
// 此时安全重试（条件更新保证最终最多一次生效）。
func (s *ShiftRequestService) Review(id, reviewer uint, approved bool) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = s.reviewOnce(id, reviewer, approved)
		if lastErr == nil {
			return nil
		}
		if !isDeadlockRetryable(lastErr) {
			return lastErr
		}
		s.logger.Warn("review transaction hit lock conflict, retrying",
			"request_id", id, "attempt", attempt+1, "error", lastErr)
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return lastErr
}

func (s *ShiftRequestService) reviewOnce(id, reviewer uint, approved bool) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		r, e := s.repo.FindForUpdate(tx, id)
		if e != nil {
			return fmt.Errorf("find shift request: %w", e)
		}
		if r.Status != model.RequestPending {
			return ErrRequestAlreadyReviewed
		}

		if !approved {
			n, e := s.repo.UpdateStatusIfPending(tx, id, reviewer, model.RequestRejected)
			if e != nil {
				return fmt.Errorf("reject shift request: %w", e)
			}
			if n == 0 {
				return ErrRequestAlreadyReviewed
			}
			return s.writeAudit(tx, reviewer, "shift_request.reject",
				fmt.Sprintf("调班申请 #%d 被驳回（申请人=%d，替班人=%d）", id, r.ApplicantID, r.SubstituteID))
		}

		// 按排班主键升序加锁，避免并发事务交叉持锁导致死锁。
		firstID, secondID := r.ScheduleID, r.SubstituteScheduleID
		if firstID > secondID {
			firstID, secondID = secondID, firstID
		}
		first, e := s.scheduleRepo.FindForUpdate(tx, firstID)
		if e != nil {
			return fmt.Errorf("lock first schedule: %w", e)
		}
		second, e := s.scheduleRepo.FindForUpdate(tx, secondID)
		if e != nil {
			return fmt.Errorf("lock second schedule: %w", e)
		}
		a, b := first, second
		if a.ID != r.ScheduleID {
			a, b = second, first
		}

		var structural []string
		if a.ID == b.ID {
			structural = append(structural, "申请人班次与替班人班次是同一条排班，无法调换")
		}
		if r.ApplicantID == r.SubstituteID {
			structural = append(structural, "申请人与替班人是同一人，无法调换")
		}
		if a.StaffID != r.ApplicantID {
			structural = append(structural, fmt.Sprintf("申请人 %s 的班次（排班 #%d，%s）当前已不属于本人，申请提交后排班发生过变动", r.Applicant.Name, a.ID, dateKey(a.WorkDate)))
		}
		if b.StaffID != r.SubstituteID {
			structural = append(structural, fmt.Sprintf("替班人 %s 的班次（排班 #%d，%s）当前已不属于其本人，申请提交后排班发生过变动", r.Substitute.Name, b.ID, dateKey(b.WorkDate)))
		}
		if len(structural) > 0 {
			return &SwapConflictError{Conflicts: structural}
		}

		all, e := s.scheduleRepo.ListByStaffIDs(tx, []uint{r.ApplicantID, r.SubstituteID})
		if e != nil {
			return fmt.Errorf("list staff schedules for validation: %w", e)
		}
		conflicts := s.detectSwapConflicts(tx, all, r, a, b)
		if len(conflicts) > 0 {
			return &SwapConflictError{Conflicts: conflicts}
		}

		// 条件更新兜底：归属与加锁时不一致则放弃，避免与其他写事务互相覆盖。
		if n, e := s.scheduleRepo.UpdateStaff(tx, a.ID, a.StaffID, b.StaffID); e != nil {
			return fmt.Errorf("swap applicant schedule: %w", e)
		} else if n != 1 {
			return &SwapConflictError{Conflicts: []string{fmt.Sprintf("排班 #%d 在审批期间被其他操作修改，请刷新后重试", a.ID)}}
		}
		if n, e := s.scheduleRepo.UpdateStaff(tx, b.ID, b.StaffID, a.StaffID); e != nil {
			return fmt.Errorf("swap substitute schedule: %w", e)
		} else if n != 1 {
			return &SwapConflictError{Conflicts: []string{fmt.Sprintf("排班 #%d 在审批期间被其他操作修改，请刷新后重试", b.ID)}}
		}

		n, e := s.repo.UpdateStatusIfPending(tx, id, reviewer, model.RequestApproved)
		if e != nil {
			return fmt.Errorf("approve shift request: %w", e)
		}
		if n == 0 {
			return ErrRequestAlreadyReviewed
		}
		return s.writeAudit(tx, reviewer, "shift_request.approve",
			fmt.Sprintf("调班申请 #%d 通过：排班 #%d 与 #%d 完成交换（申请人=%d，替班人=%d）", id, a.ID, b.ID, r.ApplicantID, r.SubstituteID))
	})
}

// detectSwapConflicts 在内存中模拟交换双方排班，然后逐人检测三类合规冲突。
// 只报告会因本次交换而产生的冲突（涉及两个被交换日期），历史遗留且与本次交换无关的问题不误报。
func (s *ShiftRequestService) detectSwapConflicts(tx *gorm.DB, all []model.Schedule, r model.ShiftRequest, a, b model.Schedule) []string {
	applicantKey := dateKey(a.WorkDate)  // 交换后替班人接管该日期
	substituteKey := dateKey(b.WorkDate) // 交换后申请人接管该日期
	swappedDates := map[string]bool{applicantKey: true, substituteKey: true}
	placement := map[uint]map[string]bool{
		r.ApplicantID:  {substituteKey: true},
		r.SubstituteID: {applicantKey: true},
	}
	names := map[uint]string{
		r.ApplicantID:  r.Applicant.Name,
		r.SubstituteID: r.Substitute.Name,
	}
	rules := map[uint]model.ScheduleRule{
		r.ApplicantID:  s.ruleFor(tx, r.Applicant.DepartmentID),
		r.SubstituteID: s.ruleFor(tx, r.Substitute.DepartmentID),
	}

	// 按“人员 -> 日期 -> 排班列表”归集。
	groups := map[uint]map[string][]model.Schedule{}
	for _, sc := range all {
		if groups[sc.StaffID] == nil {
			groups[sc.StaffID] = map[string][]model.Schedule{}
		}
		k := dateKey(sc.WorkDate)
		groups[sc.StaffID][k] = append(groups[sc.StaffID][k], sc)
	}
	if groups[r.ApplicantID] == nil {
		groups[r.ApplicantID] = map[string][]model.Schedule{}
	}
	if groups[r.SubstituteID] == nil {
		groups[r.SubstituteID] = map[string][]model.Schedule{}
	}
	moveSlot(groups, a.ID, a.StaffID, b.StaffID)
	moveSlot(groups, b.ID, b.StaffID, a.StaffID)

	var conflicts []string
	for _, staffID := range []uint{r.ApplicantID, r.SubstituteID} {
		days := groups[staffID]
		dates := make([]string, 0, len(days))
		for d := range days {
			dates = append(dates, d)
		}
		sort.Strings(dates)

		kindAt := func(d string) (model.ShiftKind, bool) {
			list := days[d]
			if len(list) != 1 {
				return "", false // 无排班或同日多条（多条已单独报重复）
			}
			return list[0].Shift.Kind, true
		}
		rule := rules[staffID]
		name := names[staffID]
		if name == "" {
			name = fmt.Sprintf("人员 #%d", staffID)
		}

		// 1) 同日重复排班：只有被交换的两个日期可能因交换新增重复。
		for _, d := range dates {
			if len(days[d]) > 1 && swappedDates[d] {
				conflicts = append(conflicts, fmt.Sprintf("%s 在 %s 同日重复排班（%d 条排班记录）", name, d, len(days[d])))
			}
		}

		// 2) 夜班后接白班：检查“夜班当日 -> 次日白班”，且该相邻对涉及本次新换入的日期。
		if rule.ForbidNightToDay {
			for _, d := range dates {
				kind, ok := kindAt(d)
				if !ok || kind != model.ShiftNight {
					continue
				}
				next := nextDateKey(d)
				nextKind, okNext := kindAt(next)
				if okNext && nextKind == model.ShiftDay &&
					(placement[staffID][d] || placement[staffID][next]) {
					conflicts = append(conflicts, fmt.Sprintf("%s 在 %s 上夜班后，次日 %s 接白班，违反夜班后不得接白班的规则", name, d, next))
				}
			}
		}

		// 3) 连续工作天数超限：只检查包含本次换入日期的连续工作段，
		//    休息班次与无排班日均中断连续工作段。
		if rule.MaxConsecutiveDays > 0 {
			workDates := map[string]bool{}
			for _, d := range dates {
				if kind, ok := kindAt(d); ok && kind != model.ShiftRest {
					workDates[d] = true
				}
			}
			for _, target := range []string{substituteKey, applicantKey} {
				if !placement[staffID][target] || !workDates[target] {
					continue
				}
				start, end := target, target
				for k := prevDateKey(start); workDates[k]; k = prevDateKey(k) {
					start = k
				}
				for k := nextDateKey(end); workDates[k]; k = nextDateKey(k) {
					end = k
				}
				run := daysBetween(start, end)
				if run > rule.MaxConsecutiveDays {
					conflicts = append(conflicts, fmt.Sprintf("%s 自 %s 至 %s 将连续工作 %d 天，超过科室连续工作天数上限 %d 天", name, start, end, run, rule.MaxConsecutiveDays))
				}
			}
		}
	}
	return dedupStrings(conflicts)
}

func (s *ShiftRequestService) ruleFor(tx *gorm.DB, deptID uint) model.ScheduleRule {
	rule, e := s.ruleRepo.ByDepartmentOn(tx, deptID)
	if e != nil || rule.MaxConsecutiveDays <= 0 {
		rule.MaxConsecutiveDays = 5
	}
	return rule
}

// moveSlot 将一条排班从原人员的日期分组移动到新人员的日期分组（模拟交换）。
func moveSlot(groups map[uint]map[string][]model.Schedule, slotID, fromStaff, toStaff uint) {
	for d, list := range groups[fromStaff] {
		for i, sc := range list {
			if sc.ID != slotID {
				continue
			}
			groups[fromStaff][d] = append(list[:i:i], list[i+1:]...)
			sc.StaffID = toStaff
			groups[toStaff][d] = append(groups[toStaff][d], sc)
			return
		}
	}
}

func dateKey(t time.Time) string { return t.Format(dateLayout) }

func nextDateKey(k string) string { return shiftDateKey(k, 1) }
func prevDateKey(k string) string { return shiftDateKey(k, -1) }

func shiftDateKey(k string, delta int) string {
	d, e := time.Parse(dateLayout, k)
	if e != nil {
		return k
	}
	return d.AddDate(0, 0, delta).Format(dateLayout)
}

func daysBetween(start, end string) int {
	a, e1 := time.Parse(dateLayout, start)
	b, e2 := time.Parse(dateLayout, end)
	if e1 != nil || e2 != nil {
		return 1
	}
	return int(b.Sub(a).Hours()/24) + 1
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// isDeadlockRetryable 判断错误是否为 InnoDB 死锁(1213)或锁等待超时(1205)，
// 这类失败会整体回滚事务，重试不会产生重复写入。
func isDeadlockRetryable(e error) bool {
	var mySQLErr *mysql.MySQLError
	if errors.As(e, &mySQLErr) {
		return mySQLErr.Number == 1213 || mySQLErr.Number == 1205
	}
	return false
}
