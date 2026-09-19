package service

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbsched/hospital-scheduler/internal/model"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var fixtureSeq int64

type swapFixture struct {
	db     *gorm.DB
	svc    *ShiftRequestService
	dept   uint
	staff  map[string]uint // "applicant"/"substitute"/"other"
	shifts map[model.ShiftKind]model.Shift
	scheds map[string]model.Schedule
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	seq := atomic.AddInt64(&fixtureSeq, 1)
	dsn := filepath.Join(t.TempDir(), fmt.Sprintf("swap-%d.db", seq))
	db, e := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if e != nil {
		t.Fatalf("open db: %v", e)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if e = db.AutoMigrate(
		&model.Department{}, &model.Position{}, &model.Staff{},
		&model.Shift{}, &model.Schedule{}, &model.ShiftRequest{},
		&model.ScheduleRule{}, &model.Holiday{}, &model.AuditLog{},
	); e != nil {
		t.Fatalf("migrate: %v", e)
	}

	dept := model.Department{Name: "内科"}
	if e = db.Create(&dept).Error; e != nil {
		t.Fatalf("create dept: %v", e)
	}
	people := map[string]model.Staff{
		"applicant":  {Name: "王医生", Username: "applicant_user", Role: model.RoleStaff, DepartmentID: dept.ID, Active: true},
		"substitute": {Name: "张护士", Username: "substitute_user", Role: model.RoleStaff, DepartmentID: dept.ID, Active: true},
		"other":      {Name: "李医生", Username: "other_user", Role: model.RoleStaff, DepartmentID: dept.ID, Active: true},
	}
	staff := map[string]uint{}
	for key, p := range people {
		if e = db.Create(&p).Error; e != nil {
			t.Fatalf("create staff %s: %v", key, e)
		}
		staff[key] = p.ID
	}
	shifts := map[model.ShiftKind]model.Shift{}
	for _, sh := range []model.Shift{
		{Name: "白班", Kind: model.ShiftDay},
		{Name: "中班", Kind: model.ShiftEvening},
		{Name: "夜班", Kind: model.ShiftNight},
		{Name: "休息", Kind: model.ShiftRest},
	} {
		if e = db.Create(&sh).Error; e != nil {
			t.Fatalf("create shift %s: %v", sh.Kind, e)
		}
		shifts[sh.Kind] = sh
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewShiftRequestService(
		repository.NewShiftRequestRepository(db),
		repository.NewScheduleRepository(db),
		repository.NewScheduleRuleRepository(db),
		db, logger,
	)
	return &swapFixture{db: db, svc: svc, dept: dept.ID, staff: staff, shifts: shifts, scheds: map[string]model.Schedule{}}
}

func (f *swapFixture) addSchedule(t *testing.T, key string, staffKey string, date time.Time, kind model.ShiftKind) model.Schedule {
	t.Helper()
	sc := model.Schedule{
		DepartmentID: f.dept,
		StaffID:      f.staff[staffKey],
		ShiftID:      f.shifts[kind].ID,
		WorkDate:     date,
	}
	if e := f.db.Create(&sc).Error; e != nil {
		t.Fatalf("create schedule %s: %v", key, e)
	}
	f.scheds[key] = sc
	return sc
}

func (f *swapFixture) createRequest(t *testing.T) model.ShiftRequest {
	t.Helper()
	req := model.ShiftRequest{
		ApplicantID:          f.staff["applicant"],
		SubstituteID:         f.staff["substitute"],
		ScheduleID:           f.scheds["a"].ID,
		SubstituteScheduleID: f.scheds["b"].ID,
		Reason:               "家中有事需要调班",
		Status:               model.RequestPending,
	}
	if e := f.db.Create(&req).Error; e != nil {
		t.Fatalf("create request: %v", e)
	}
	return req
}

func date(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.Local) }

func (f *swapFixture) reloadSchedules(t *testing.T, keys ...string) map[string]model.Schedule {
	t.Helper()
	out := map[string]model.Schedule{}
	for _, key := range keys {
		var sc model.Schedule
		if e := f.db.First(&sc, f.scheds[key].ID).Error; e != nil {
			t.Fatalf("reload schedule %s: %v", key, e)
		}
		out[key] = sc
	}
	return out
}

// 申请人只能换本人班次，替班人必须对应指定班次。
func TestCreateRejectsOwnershipMismatch(t *testing.T) {
	f := newSwapFixture(t)
	d := date(5)
	f.addSchedule(t, "a", "applicant", d, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d.AddDate(0, 0, 7), model.ShiftRest)

	_, e := f.svc.Create(model.ShiftRequest{
		SubstituteID:         f.staff["substitute"],
		ScheduleID:           f.scheds["a"].ID,
		SubstituteScheduleID: f.scheds["b"].ID,
		Reason:               "想休息",
	}, f.staff["other"])
	if !errors.Is(e, ErrInvalidSwap) {
		t.Fatalf("expected ErrInvalidSwap for non-own schedule, got %v", e)
	}

	// Applicant's own schedule, but the counter schedule belongs to someone
	// else rather than the named substitute.
	f.addSchedule(t, "otherShift", "other", d.AddDate(0, 0, 8), model.ShiftRest)
	_, e = f.svc.Create(model.ShiftRequest{
		SubstituteID:         f.staff["substitute"],
		ScheduleID:           f.scheds["a"].ID,
		SubstituteScheduleID: f.scheds["otherShift"].ID,
		Reason:               "想休息",
	}, f.staff["applicant"])
	if !errors.Is(e, ErrInvalidSwap) {
		t.Fatalf("expected ErrInvalidSwap for substitute schedule mismatch, got %v", e)
	}
}

// 合规换班：双方排班原子交换，申请一次变为 approved。
func TestReviewApprovedSwapsAtomically(t *testing.T) {
	f := newSwapFixture(t)
	d := date(1)
	f.addSchedule(t, "a", "applicant", d, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d.AddDate(0, 0, 14), model.ShiftRest)
	req := f.createRequest(t)

	if e := f.svc.Review(req.ID, 999, true); e != nil {
		t.Fatalf("review: %v", e)
	}
	got := f.reloadSchedules(t, "a", "b")
	if got["a"].StaffID != f.staff["substitute"] || got["b"].StaffID != f.staff["applicant"] {
		t.Fatalf("schedules not swapped: a->%d b->%d", got["a"].StaffID, got["b"].StaffID)
	}
	var saved model.ShiftRequest
	if e := f.db.First(&saved, req.ID).Error; e != nil {
		t.Fatalf("reload request: %v", e)
	}
	if saved.Status != model.RequestApproved || saved.ReviewerID == nil || *saved.ReviewerID != 999 {
		t.Fatalf("unexpected approved request: %+v", saved)
	}
}

// 夜班后接白班：交换后申请人出现 night->day，整次拒绝且排班不变。
func TestReviewRejectsNightToDay(t *testing.T) {
	f := newSwapFixture(t)
	d1, d2, d3 := date(1), date(2), date(3)
	// Applicant works d1 (offered) and d3; after the swap the applicant takes
	// the substitute's d2 night, which is immediately followed by their own d3 day.
	f.addSchedule(t, "a", "applicant", d1, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d2, model.ShiftNight)
	f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["applicant"], ShiftID: f.shifts[model.ShiftDay].ID, WorkDate: d3})
	// Substitute surroundings stay neutral: rest on d1 and d3.
	f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["substitute"], ShiftID: f.shifts[model.ShiftRest].ID, WorkDate: d1})
	f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["substitute"], ShiftID: f.shifts[model.ShiftRest].ID, WorkDate: d3})
	req := f.createRequest(t)

	e := f.svc.Review(req.ID, 999, true)
	var conflict *SwapConflictError
	if !errors.As(e, &conflict) {
		t.Fatalf("expected SwapConflictError, got %v", e)
	}
	if !contains(conflict.Conflicts, "夜班后次日接白班") {
		t.Fatalf("expected night-to-day conflict, got %v", conflict.Conflicts)
	}
	got := f.reloadSchedules(t, "a", "b")
	if got["a"].StaffID != f.staff["applicant"] || got["b"].StaffID != f.staff["substitute"] {
		t.Fatalf("schedules changed despite rejection")
	}
	var saved model.ShiftRequest
	f.db.First(&saved, req.ID)
	if saved.Status != model.RequestPending {
		t.Fatalf("rejected-on-conflict request must stay pending, got %s", saved.Status)
	}
}

// 连续工作天数超限（默认上限 5）：换入一个工作日使连续 6 天时拒绝。
func TestReviewRejectsConsecutiveDays(t *testing.T) {
	f := newSwapFixture(t)
	d1, d2, d3, d4, d5, d6 := date(1), date(2), date(3), date(4), date(5), date(6)
	// applicant works d1..d5, offers the rest on d6 (b is substitute's day shift).
	f.addSchedule(t, "b", "substitute", d6, model.ShiftDay)
	f.addSchedule(t, "a", "applicant", d6, model.ShiftRest)
	for _, dd := range []time.Time{d1, d2, d3, d4, d5} {
		f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["applicant"], ShiftID: f.shifts[model.ShiftDay].ID, WorkDate: dd})
	}
	// substitute works d1..d5 too; handing d6 to applicant means substitute
	// rests that day, which is safe.
	for _, dd := range []time.Time{d1, d2, d3, d4, d5} {
		f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["substitute"], ShiftID: f.shifts[model.ShiftDay].ID, WorkDate: dd})
	}
	req := f.createRequest(t)

	e := f.svc.Review(req.ID, 999, true)
	var conflict *SwapConflictError
	if !errors.As(e, &conflict) {
		t.Fatalf("expected SwapConflictError, got %v", e)
	}
	if !contains(conflict.Conflicts, "连续工作") || !contains(conflict.Conflicts, "超过上限") {
		t.Fatalf("expected consecutive-days conflict, got %v", conflict.Conflicts)
	}
}

// 同日重复排班：换入的日期本人已有一个非休息班次时拒绝。
func TestReviewRejectsDuplicateDuty(t *testing.T) {
	f := newSwapFixture(t)
	d1, d2 := date(1), date(2)
	f.addSchedule(t, "a", "applicant", d1, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d2, model.ShiftDay)
	// applicant already works d2 (evening), so taking b doubles up d2.
	f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["applicant"], ShiftID: f.shifts[model.ShiftEvening].ID, WorkDate: d2})
	// substitute rests d1, so taking a there stays single duty.
	f.db.Create(&model.Schedule{DepartmentID: f.dept, StaffID: f.staff["substitute"], ShiftID: f.shifts[model.ShiftRest].ID, WorkDate: d1})
	req := f.createRequest(t)

	e := f.svc.Review(req.ID, 999, true)
	var conflict *SwapConflictError
	if !errors.As(e, &conflict) {
		t.Fatalf("expected SwapConflictError, got %v", e)
	}
	if !contains(conflict.Conflicts, "同日重复排班") {
		t.Fatalf("expected duplicate-duty conflict, got %v", conflict.Conflicts)
	}
	if !contains(conflict.Conflicts, "王医生") {
		t.Fatalf("conflict must name the staff, got %v", conflict.Conflicts)
	}
}

// 并发/重复审批只有一次成功：一个通过后，其余审批拿到 ErrAlreadyReviewed。
func TestConcurrentReviewSucceedsOnce(t *testing.T) {
	f := newSwapFixture(t)
	d := date(20)
	f.addSchedule(t, "a", "applicant", d, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d.AddDate(0, 0, 14), model.ShiftRest)
	req := f.createRequest(t)

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx] = f.svc.Review(req.ID, uint(900+idx), true)
		}(i)
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, e := range results {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, repository.ErrAlreadyReviewed):
			conflict++
		default:
			t.Fatalf("unexpected review error: %v", e)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("expected exactly 1 success, got ok=%d conflict=%d", ok, conflict)
	}
	got := f.reloadSchedules(t, "a", "b")
	if got["a"].StaffID != f.staff["substitute"] || got["b"].StaffID != f.staff["applicant"] {
		t.Fatalf("schedules not swapped after concurrent approval")
	}
	var saved model.ShiftRequest
	f.db.First(&saved, req.ID)
	if saved.Status != model.RequestApproved {
		t.Fatalf("expected approved, got %s", saved.Status)
	}
}

// 驳回不交换排班，且重复驳回同样只能成功一次。
func TestRejectIsIdempotent(t *testing.T) {
	f := newSwapFixture(t)
	d := date(21)
	f.addSchedule(t, "a", "applicant", d, model.ShiftDay)
	f.addSchedule(t, "b", "substitute", d.AddDate(0, 0, 14), model.ShiftRest)
	req := f.createRequest(t)

	if e := f.svc.Review(req.ID, 1, false); e != nil {
		t.Fatalf("reject: %v", e)
	}
	if e := f.svc.Review(req.ID, 2, false); !errors.Is(e, repository.ErrAlreadyReviewed) {
		t.Fatalf("second reject should fail with ErrAlreadyReviewed, got %v", e)
	}
	got := f.reloadSchedules(t, "a", "b")
	if got["a"].StaffID != f.staff["applicant"] || got["b"].StaffID != f.staff["substitute"] {
		t.Fatalf("rejection must not swap schedules")
	}
	var saved model.ShiftRequest
	f.db.First(&saved, req.ID)
	if saved.Status != model.RequestRejected {
		t.Fatalf("expected rejected, got %s", saved.Status)
	}
}

func contains(list []string, sub string) bool {
	for _, v := range list {
		if len(v) >= len(sub) {
			for i := 0; i+len(sub) <= len(v); i++ {
				if v[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
