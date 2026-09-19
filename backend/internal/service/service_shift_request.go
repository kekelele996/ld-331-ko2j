package service

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gbsched/hospital-scheduler/internal/model"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"gorm.io/gorm"
)

// ErrInvalidSwap is returned when an applicant tries to swap a schedule they do
// not own or points the substitute at the wrong schedule.
var ErrInvalidSwap = errors.New("invalid shift swap")

// SwapConflictError explains every rule violation that would appear after the
// swap; the whole request must be rejected when it is non-nil.
type SwapConflictError struct {
	Conflicts []string
}

func (e *SwapConflictError) Error() string {
	return "换班后存在排班冲突：" + strings.Join(e.Conflicts, "；")
}

type ShiftRequestService struct {
	repo         *repository.ShiftRequestRepository
	scheduleRepo *repository.ScheduleRepository
	ruleRepo     *repository.ScheduleRuleRepository
	db           *gorm.DB
	logger       *slog.Logger
}

func NewShiftRequestService(r *repository.ShiftRequestRepository, s *repository.ScheduleRepository, rr *repository.ScheduleRuleRepository, db *gorm.DB, l *slog.Logger) *ShiftRequestService {
	return &ShiftRequestService{r, s, rr, db, l}
}

func (s *ShiftRequestService) List() ([]model.ShiftRequest, error) {
	v, e := s.repo.List()
	if e != nil {
		return nil, fmt.Errorf("list shift requests: %w", e)
	}
	return v, nil
}

func (s *ShiftRequestService) Create(v model.ShiftRequest, actor uint) (model.ShiftRequest, error) {
	a, e := s.scheduleRepo.Find(v.ScheduleID)
	if e != nil {
		return v, fmt.Errorf("find applicant schedule: %w", e)
	}
	b, e := s.scheduleRepo.Find(v.SubstituteScheduleID)
	if e != nil {
		return v, fmt.Errorf("find substitute schedule: %w", e)
	}
	if a.ID == b.ID {
		return v, fmt.Errorf("%w: 申请人班次与替班班次不能是同一条排班", ErrInvalidSwap)
	}
	if actor == v.SubstituteID {
		return v, fmt.Errorf("%w: 申请人与替班人不能为同一人", ErrInvalidSwap)
	}
	// The applicant may only offer one of their own schedules, and the
	// substitute must currently own the specified counter schedule.
	if a.StaffID != actor {
		return v, fmt.Errorf("%w: 只能申请调换本人的班次", ErrInvalidSwap)
	}
	if b.StaffID != v.SubstituteID {
		return v, fmt.Errorf("%w: 替班人必须对应指定班次", ErrInvalidSwap)
	}
	v.ApplicantID = actor
	v.Status = model.RequestPending
	if e = s.repo.Create(&v); e != nil {
		return v, fmt.Errorf("create shift request: %w", e)
	}
	return v, nil
}

func (s *ShiftRequestService) Review(id, reviewer uint, approved bool) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Lock the pending request row. On MySQL this serializes concurrent
		// reviewers (a duplicate/concurrent attempt blocks, then sees a
		// non-pending status and fails with ErrAlreadyReviewed); on the
		// single-connection SQLite pool transactions are serialized the same way.
		req, e := s.repo.LockPendingTx(tx, id)
		if e != nil {
			return fmt.Errorf("lock shift request: %w", e)
		}

		// Lock both schedule rows so a concurrent approval touching the same
		// schedules blocks until this transaction finishes. Always acquire the
		// locks in ascending ID order to avoid cross-transaction deadlocks.
		var applicantSchedule, substituteSchedule model.Schedule
		if req.ScheduleID < req.SubstituteScheduleID {
			applicantSchedule, e = s.scheduleRepo.FindForUpdateTx(tx, req.ScheduleID)
			if e != nil {
				return fmt.Errorf("find applicant schedule: %w", e)
			}
			substituteSchedule, e = s.scheduleRepo.FindForUpdateTx(tx, req.SubstituteScheduleID)
			if e != nil {
				return fmt.Errorf("find substitute schedule: %w", e)
			}
		} else {
			substituteSchedule, e = s.scheduleRepo.FindForUpdateTx(tx, req.SubstituteScheduleID)
			if e != nil {
				return fmt.Errorf("find substitute schedule: %w", e)
			}
			applicantSchedule, e = s.scheduleRepo.FindForUpdateTx(tx, req.ScheduleID)
			if e != nil {
				return fmt.Errorf("find applicant schedule: %w", e)
			}
		}

		if !approved {
			// Nothing else changes; the rejection is finalized atomically on return.
			if e = s.repo.FinalizeTx(tx, id, reviewer, model.RequestRejected); e != nil {
				return fmt.Errorf("finalize rejection: %w", e)
			}
			s.logger.Info("shift request rejected", "request_id", id, "reviewer_id", reviewer)
			return nil
		}

		// Ownership is re-checked at approval time because the board may have been
		// hand-edited while the request was pending.
		if applicantSchedule.StaffID != req.ApplicantID || substituteSchedule.StaffID != req.SubstituteID {
			return fmt.Errorf("%w: 排班归属已变化，申请人或替班人不再对应指定班次", ErrInvalidSwap)
		}
		if applicantSchedule.ID == substituteSchedule.ID {
			return fmt.Errorf("%w: 申请人班次与替班班次不能是同一条排班", ErrInvalidSwap)
		}

		rule, _ := s.ruleRepo.ByDepartmentTx(tx, applicantSchedule.DepartmentID)
		// Any rule violation aborts the whole transaction: the request stays
		// pending and neither schedule is modified.
		if _, ce := s.detectConflicts(tx, applicantSchedule, substituteSchedule, rule); ce != nil {
			return ce
		}

		if e = s.scheduleRepo.SwapStaffTx(tx, applicantSchedule, substituteSchedule); e != nil {
			return fmt.Errorf("swap schedules: %w", e)
		}
		// Flip the request to approved in the same transaction so the board
		// exchange and the request status become visible together exactly once.
		if e = s.repo.FinalizeTx(tx, id, reviewer, model.RequestApproved); e != nil {
			return fmt.Errorf("finalize approval: %w", e)
		}
		s.logger.Info("shift request approved", "request_id", id, "reviewer_id", reviewer,
			"schedule_id", req.ScheduleID, "substitute_schedule_id", req.SubstituteScheduleID)
		return nil
	})
}

// detectConflicts simulates the swap for both staff and returns a
// SwapConflictError describing every violation, or nil when the swap is safe.
func (s *ShiftRequestService) detectConflicts(tx *gorm.DB, applicant, substitute model.Schedule, rule model.ScheduleRule) ([]string, error) {
	maxConsecutive := rule.MaxConsecutiveDays
	if maxConsecutive <= 0 {
		maxConsecutive = 5
	}

	// Widen the window around both anchor dates so neighbour-shift and
	// consecutive-day runs are evaluated against the complete surroundings.
	from := minTime(applicant.WorkDate, substitute.WorkDate).AddDate(0, 0, -maxConsecutive-2)
	to := maxTime(applicant.WorkDate, substitute.WorkDate).AddDate(0, 0, maxConsecutive+2)

	// Load the union of both staff's schedules once, then redistribute the two
	// anchor rows according to the swap before evaluating the rules per person.
	items, e := s.scheduleRepo.StaffSetRangeTx(tx, []uint{applicant.StaffID, substitute.StaffID}, from, to)
	if e != nil {
		return nil, fmt.Errorf("load staff schedules: %w", e)
	}
	staffSchedules := buildSwappedViews(items, applicant, substitute)
	names := pairNames(items, applicant, substitute)

	var conflicts []string
	conflicts = append(conflicts, checkDuplicateDuty(staffSchedules, names, applicant, substitute)...)
	conflicts = append(conflicts, checkNightToDay(staffSchedules, names, applicant, substitute)...)
	conflicts = append(conflicts, checkConsecutiveDays(staffSchedules, names, maxConsecutive, applicant, substitute)...)
	if len(conflicts) > 0 {
		return conflicts, &SwapConflictError{Conflicts: conflicts}
	}
	return nil, nil
}

// buildSwappedViews redistributes the two anchor schedules between the two
// staff as the swap would, then returns each staff's post-swap schedule list.
func buildSwappedViews(items []model.Schedule, applicant, substitute model.Schedule) map[uint][]model.Schedule {
	applicantView := make([]model.Schedule, 0, len(items))
	substituteView := make([]model.Schedule, 0, len(items))
	for _, item := range items {
		switch item.ID {
		case applicant.ID:
			// Applicant's old shift goes to the substitute.
			item.StaffID = substitute.StaffID
		case substitute.ID:
			// Substitute's old shift goes to the applicant.
			item.StaffID = applicant.StaffID
		}
		switch item.StaffID {
		case applicant.StaffID:
			applicantView = append(applicantView, item)
		case substitute.StaffID:
			substituteView = append(substituteView, item)
		}
	}
	return map[uint][]model.Schedule{
		applicant.StaffID:  applicantView,
		substitute.StaffID: substituteView,
	}
}

// pairNames resolves staff display names from the preloaded rows, since anchor
// rows passed into the checkers carry their pre-swap owner name.
func pairNames(items []model.Schedule, applicant, substitute model.Schedule) map[uint]string {
	names := map[uint]string{
		applicant.StaffID:  "",
		substitute.StaffID: "",
	}
	for _, item := range items {
		if item.Staff.Name != "" {
			names[item.StaffID] = item.Staff.Name
		}
	}
	return names
}

// checkDuplicateDuty rejects when either staff ends up with more than one
// non-rest shift on any anchor date (e.g. swapping onto a date they already work).
func checkDuplicateDuty(byStaff map[uint][]model.Schedule, names map[uint]string, applicant, substitute model.Schedule) []string {
	anchorDates := map[string]time.Time{
		dateKey(applicant.WorkDate):  applicant.WorkDate,
		dateKey(substitute.WorkDate): substitute.WorkDate,
	}
	var conflicts []string
	for staffID, items := range byStaff {
		counts := map[string]int{}
		for _, item := range items {
			if item.Shift.Kind != model.ShiftRest {
				counts[dateKey(item.WorkDate)]++
			}
		}
		for key, day := range anchorDates {
			if counts[key] > 1 {
				conflicts = append(conflicts, fmt.Sprintf("%s在%s同日重复排班", staffLabel(names[staffID], staffID), day.Format("2006-01-02")))
			}
		}
	}
	return conflicts
}

// checkNightToDay rejects a night shift immediately followed by a day shift on
// the next calendar date (in either direction of the adjacency).
func checkNightToDay(byStaff map[uint][]model.Schedule, names map[uint]string, applicant, substitute model.Schedule) []string {
	anchorIDs := map[uint]bool{applicant.ID: true, substitute.ID: true}
	var conflicts []string
	seen := map[string]bool{}
	add := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			conflicts = append(conflicts, msg)
		}
	}
	for staffID, items := range byStaff {
		byDate := map[string]model.Schedule{}
		for _, item := range items {
			byDate[dateKey(item.WorkDate)] = item
		}
		for _, item := range items {
			if !anchorIDs[item.ID] {
				continue
			}
			label := staffLabel(names[staffID], staffID)
			next, hasNext := byDate[dateKey(item.WorkDate.AddDate(0, 0, 1))]
			if item.Shift.Kind == model.ShiftNight && hasNext && next.Shift.Kind == model.ShiftDay {
				add(fmt.Sprintf("%s在%s夜班后次日接白班", label, item.WorkDate.Format("2006-01-02")))
			}
			prev, hasPrev := byDate[dateKey(item.WorkDate.AddDate(0, 0, -1))]
			if item.Shift.Kind == model.ShiftDay && hasPrev && prev.Shift.Kind == model.ShiftNight {
				add(fmt.Sprintf("%s在%s夜班后次日接白班", label, prev.WorkDate.Format("2006-01-02")))
			}
		}
	}
	return conflicts
}

// checkConsecutiveDays rejects any working run that exceeds the department
// limit; only runs touching an anchor date are attributable to the swap.
func checkConsecutiveDays(byStaff map[uint][]model.Schedule, names map[uint]string, maxDays int, applicant, substitute model.Schedule) []string {
	anchorDates := map[string]bool{
		dateKey(applicant.WorkDate):  true,
		dateKey(substitute.WorkDate): true,
	}
	var conflicts []string
	for staffID, items := range byStaff {
		if len(items) == 0 {
			continue
		}
		label := staffLabel(names[staffID], staffID)
		runStart := -1
		runLen := 0
		flush := func(endIdx int) {
			if runLen > maxDays {
				for i := runStart; i <= endIdx; i++ {
					if anchorDates[dateKey(items[i].WorkDate)] {
						conflicts = append(conflicts, fmt.Sprintf("%s连续工作%d天，超过上限%d天（%s至%s）",
							label, runLen, maxDays,
							items[runStart].WorkDate.Format("2006-01-02"), items[endIdx].WorkDate.Format("2006-01-02")))
						return
					}
				}
			}
		}
		for i, item := range items {
			working := item.Shift.Kind != model.ShiftRest
			contiguous := false
			if i > 0 {
				expected := items[i-1].WorkDate.AddDate(0, 0, 1)
				contiguous = sameDate(item.WorkDate, expected)
			}
			if working && (runLen == 0 || contiguous) {
				if runLen == 0 {
					runStart = i
				}
				runLen++
			} else {
				if runLen > 0 {
					flush(i - 1)
				}
				if working {
					runStart, runLen = i, 1
				} else {
					runStart, runLen = -1, 0
				}
			}
		}
		if runLen > 0 {
			flush(len(items) - 1)
		}
	}
	return conflicts
}

func dateKey(t time.Time) string { return t.Format("2006-01-02") }
func sameDate(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func staffLabel(name string, fallback uint) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fmt.Sprintf("员工#%d", fallback)
}
