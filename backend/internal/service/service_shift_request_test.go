package service

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbsched/hospital-scheduler/internal/model"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var memDBSequence int

type requestFixture struct {
	db        *gorm.DB
	svc       *ShiftRequestService
	app, sub  model.Staff
	third     model.Staff
	day       model.Shift
	evening   model.Shift
	night     model.Shift
	rest      model.Shift
}

func newRequestFixture(t *testing.T) requestFixture {
	t.Helper()
	// mode=memory + cache=shared 让同一 DSN 的多个连接共享同一个内存库；
	// 每个夹具使用唯一名，避免跨用例串数据。
	memDBSequence++
	db, e := gorm.Open(sqlite.Open(fmt.Sprintf("file:memreq%d?mode=memory&cache=shared", memDBSequence)), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	// 事务内还会在另一查询中读取科室规则，连接池至少保留 2 条连接。
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if e = db.AutoMigrate(&model.Staff{}, &model.Shift{}, &model.Schedule{}, &model.ShiftRequest{}, &model.ScheduleRule{}, &model.AuditLog{}); e != nil {
		t.Fatal(e)
	}
	fx := requestFixture{db: db}
	for _, st := range []*model.Staff{
		{Name: "王医生", Username: "wang_doc", Active: true, DepartmentID: 1},
		{Name: "张护士", Username: "zhang_nurse", Active: true, DepartmentID: 1},
		{Name: "李护工", Username: "li_aide", Active: true, DepartmentID: 1},
	} {
		if e = db.Create(st).Error; e != nil {
			t.Fatal(e)
		}
	}
	fx.app = model.Staff{BaseModel: model.BaseModel{ID: 1}}
	fx.sub = model.Staff{BaseModel: model.BaseModel{ID: 2}}
	fx.third = model.Staff{BaseModel: model.BaseModel{ID: 3}}
	for _, sh := range []*model.Shift{
		{Name: "白班", Kind: model.ShiftDay},
		{Name: "中班", Kind: model.ShiftEvening},
		{Name: "夜班", Kind: model.ShiftNight},
		{Name: "休息", Kind: model.ShiftRest},
	} {
		if e = db.Create(sh).Error; e != nil {
			t.Fatal(e)
		}
		switch sh.Kind {
		case model.ShiftDay:
			fx.day = *sh
		case model.ShiftEvening:
			fx.evening = *sh
		case model.ShiftNight:
			fx.night = *sh
		case model.ShiftRest:
			fx.rest = *sh
		}
	}
	if e = db.Create(&model.ScheduleRule{DepartmentID: 1, MaxConsecutiveDays: 5, ForbidNightToDay: true}).Error; e != nil {
		t.Fatal(e)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fx.svc = NewShiftRequestService(
		repository.NewShiftRequestRepository(db),
		repository.NewScheduleRepository(db),
		repository.NewScheduleRuleRepository(db),
		repository.NewAuditLogRepository(db),
		db, logger,
	)
	return fx
}

func parseDay(s string) time.Time {
	d, e := time.ParseInLocation(dateLayout, s, time.Local)
	if e != nil {
		panic(e)
	}
	return d
}

func (fx requestFixture) addSchedule(t *testing.T, staff uint, shift model.Shift, date string) model.Schedule {
	t.Helper()
	sc := model.Schedule{DepartmentID: 1, StaffID: staff, ShiftID: shift.ID, WorkDate: parseDay(date)}
	if e := fx.db.Create(&sc).Error; e != nil {
		t.Fatal(e)
	}
	return sc
}

func (fx requestFixture) submitRequest(t *testing.T, a, b model.Schedule) uint {
	t.Helper()
	v := model.ShiftRequest{
		ApplicantID:          fx.app.ID,
		SubstituteID:         fx.sub.ID,
		ScheduleID:           a.ID,
		SubstituteScheduleID: b.ID,
		Reason:               "家中有事需要调班",
	}
	if e := fx.svc.Create(v, fx.app.ID); e != nil {
		t.Fatalf("create request: %v", e)
	}
	var req model.ShiftRequest
	if e := fx.db.Order("id desc").First(&req).Error; e != nil {
		t.Fatal(e)
	}
	return req.ID
}

func (fx requestFixture) staffOf(t *testing.T, id uint) uint {
	t.Helper()
	var sc model.Schedule
	if e := fx.db.First(&sc, id).Error; e != nil {
		t.Fatal(e)
	}
	return sc.StaffID
}

func (fx requestFixture) requestStatus(t *testing.T, id uint) model.RequestStatus {
	t.Helper()
	var req model.ShiftRequest
	if e := fx.db.First(&req, id).Error; e != nil {
		t.Fatal(e)
	}
	return req.Status
}

// 合规场景下审批通过：双方排班一次交换，申请置为 approved。
func TestReviewApprovedSwapsAtomically(t *testing.T) {
	fx := newRequestFixture(t)
	// 申请人 8/1 白班；替班人 7/31 休息、8/2 夜班。交换后双方各持有一条，无任何冲突。
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.night, "2026-08-02")
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-07-31")
	id := fx.submitRequest(t, a, b)

	if e := fx.svc.Review(id, 99, true); e != nil {
		t.Fatalf("review: %v", e)
	}
	if got := fx.staffOf(t, a.ID); got != fx.sub.ID {
		t.Fatalf("applicant schedule staff = %d, want %d", got, fx.sub.ID)
	}
	if got := fx.staffOf(t, b.ID); got != fx.app.ID {
		t.Fatalf("substitute schedule staff = %d, want %d", got, fx.app.ID)
	}
	if status := fx.requestStatus(t, id); status != model.RequestApproved {
		t.Fatalf("status = %s, want approved", status)
	}
}

// 交换后替班人出现“夜班后接白班”，整次拒绝，冲突信息需明确到人到日期，
// 且申请仍为 pending、排班不变。
func TestReviewRejectsNightFollowedByDay(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.night, "2026-08-02")
	fx.addSchedule(t, fx.sub.ID, fx.night, "2026-07-31") // 换入 8/1 白班后形成 7/31 夜 -> 8/1 白
	fx.addSchedule(t, fx.app.ID, fx.rest, "2026-08-03")
	id := fx.submitRequest(t, a, b)

	e := fx.svc.Review(id, 99, true)
	if !IsConflict(e) {
		t.Fatalf("want SwapConflictError, got %v", e)
	}
	if msg := e.Error(); !strings.Contains(msg, "张护士") || !strings.Contains(msg, "夜班") || !strings.Contains(msg, "2026-08-01") {
		t.Fatalf("conflict message not specific enough: %s", msg)
	}
	if fx.requestStatus(t, id) != model.RequestPending {
		t.Fatal("rejected request must stay pending")
	}
	if fx.staffOf(t, a.ID) != fx.app.ID || fx.staffOf(t, b.ID) != fx.sub.ID {
		t.Fatal("schedules must remain unchanged after rejection")
	}
}

// 交换后申请人连续工作 6 天，超过科室上限 5 天，整次拒绝。
func TestReviewRejectsConsecutiveDaysOverflow(t *testing.T) {
	fx := newRequestFixture(t)
	for _, d := range []string{"2026-08-01", "2026-08-02", "2026-08-03", "2026-08-04", "2026-08-05"} {
		fx.addSchedule(t, fx.app.ID, fx.day, d)
	}
	fx.addSchedule(t, fx.app.ID, fx.rest, "2026-07-31")
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-10") // 申请人的远端班次
	b := fx.addSchedule(t, fx.sub.ID, fx.day, "2026-08-06") // 换入后申请人 8/1-8/6 连做 6 天
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-08-05")
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-08-07")
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-08-11")
	id := fx.submitRequest(t, a, b)

	e := fx.svc.Review(id, 99, true)
	if !IsConflict(e) {
		t.Fatalf("want SwapConflictError, got %v", e)
	}
	if msg := e.Error(); !strings.Contains(msg, "连续工作") || !strings.Contains(msg, "6 天") {
		t.Fatalf("unexpected conflict message: %s", msg)
	}
	if fx.staffOf(t, a.ID) != fx.app.ID || fx.staffOf(t, b.ID) != fx.sub.ID {
		t.Fatal("schedules must remain unchanged after rejection")
	}
}

// 交换后替班人同日存在两条排班（换入白班 + 本人中班），整次拒绝。
func TestReviewRejectsSameDayDuplicate(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.day, "2026-08-01")
	fx.addSchedule(t, fx.sub.ID, fx.evening, "2026-08-01")
	fx.addSchedule(t, fx.app.ID, fx.rest, "2026-07-31")
	fx.addSchedule(t, fx.app.ID, fx.rest, "2026-08-02")
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-08-02")
	id := fx.submitRequest(t, a, b)

	e := fx.svc.Review(id, 99, true)
	if !IsConflict(e) {
		t.Fatalf("want SwapConflictError, got %v", e)
	}
	if msg := e.Error(); !strings.Contains(msg, "同日重复排班") {
		t.Fatalf("unexpected conflict message: %s", msg)
	}
	if fx.staffOf(t, a.ID) != fx.app.ID || fx.staffOf(t, b.ID) != fx.sub.ID {
		t.Fatal("schedules must remain unchanged after rejection")
	}
}

// 申请校验：申请人只能换本人班次，替班人必须对应其本人班次。
func TestCreateOwnershipValidation(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.day, "2026-08-02")

	// 申请人提交了不属于自己的班次
	stolen := model.ShiftRequest{SubstituteID: fx.sub.ID, ScheduleID: b.ID, SubstituteScheduleID: a.ID, Reason: "测试调班原因"}
	if e := fx.svc.Create(stolen, fx.app.ID); e == nil || !strings.Contains(e.Error(), "本人的班次") {
		t.Fatalf("want ownership error, got %v", e)
	}
	// 替班人与其指定班次不对应
	mismatch := model.ShiftRequest{SubstituteID: fx.third.ID, ScheduleID: a.ID, SubstituteScheduleID: b.ID, Reason: "测试调班原因"}
	if e := fx.svc.Create(mismatch, fx.app.ID); e == nil || !strings.Contains(e.Error(), "替班人") {
		t.Fatalf("want substitute mismatch error, got %v", e)
	}
	// 申请人与替班人相同
	self := model.ShiftRequest{SubstituteID: fx.app.ID, ScheduleID: a.ID, SubstituteScheduleID: b.ID, Reason: "测试调班原因"}
	if e := fx.svc.Create(self, fx.app.ID); e == nil {
		t.Fatal("want error when applicant equals substitute")
	}
}

// 提交申请后、审批前排班被调走：审批需识别归属变化并拒绝，状态保持 pending。
func TestReviewRejectsWhenScheduleMovedAfterSubmission(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.day, "2026-08-02")
	id := fx.submitRequest(t, a, b)

	if e := fx.db.Model(&model.Schedule{}).Where("id = ?", a.ID).Update("staff_id", fx.third.ID).Error; e != nil {
		t.Fatal(e)
	}
	e := fx.svc.Review(id, 99, true)
	if !IsConflict(e) || !strings.Contains(e.Error(), "已不属于本人") {
		t.Fatalf("want structural conflict, got %v", e)
	}
	if fx.requestStatus(t, id) != model.RequestPending {
		t.Fatal("request must stay pending when validation fails")
	}
}

// 重复审批只能成功一次：通过后再次审批返回 ErrRequestAlreadyReviewed，排班只交换一次。
func TestReviewIsIdempotent(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.night, "2026-08-02")
	fx.addSchedule(t, fx.app.ID, fx.rest, "2026-08-03")
	fx.addSchedule(t, fx.sub.ID, fx.rest, "2026-07-31")
	id := fx.submitRequest(t, a, b)

	if e := fx.svc.Review(id, 99, true); e != nil {
		t.Fatalf("first review: %v", e)
	}
	if e := fx.svc.Review(id, 99, true); !errors.Is(e, ErrRequestAlreadyReviewed) {
		t.Fatalf("second review: want ErrRequestAlreadyReviewed, got %v", e)
	}
	// 再驳回也不能改变已定稿的状态
	if e := fx.svc.Review(id, 99, false); !errors.Is(e, ErrRequestAlreadyReviewed) {
		t.Fatalf("review after approve: want ErrRequestAlreadyReviewed, got %v", e)
	}
	if fx.requestStatus(t, id) != model.RequestApproved {
		t.Fatal("status must remain approved")
	}
	if fx.staffOf(t, a.ID) != fx.sub.ID || fx.staffOf(t, b.ID) != fx.app.ID {
		t.Fatal("schedules swapped exactly once")
	}
}

// 驳回只更新申请状态，不动排班；驳回后不能再次审批。
func TestReviewRejectDoesNotTouchSchedules(t *testing.T) {
	fx := newRequestFixture(t)
	a := fx.addSchedule(t, fx.app.ID, fx.day, "2026-08-01")
	b := fx.addSchedule(t, fx.sub.ID, fx.night, "2026-08-02")
	id := fx.submitRequest(t, a, b)

	if e := fx.svc.Review(id, 99, false); e != nil {
		t.Fatalf("reject: %v", e)
	}
	if fx.requestStatus(t, id) != model.RequestRejected {
		t.Fatal("status must be rejected")
	}
	if fx.staffOf(t, a.ID) != fx.app.ID || fx.staffOf(t, b.ID) != fx.sub.ID {
		t.Fatal("rejection must not swap schedules")
	}
	if e := fx.svc.Review(id, 99, true); !errors.Is(e, ErrRequestAlreadyReviewed) {
		t.Fatalf("review after reject: want ErrRequestAlreadyReviewed, got %v", e)
	}
}

// 并发审批：无论多少个请求同时到达，只有一个事务生效，其余全部得到已审批错误。
func TestConcurrentReviewSucceedsOnce(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "concurrency.db")+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(&model.Staff{}, &model.Shift{}, &model.Schedule{}, &model.ShiftRequest{}, &model.ScheduleRule{}, &model.AuditLog{}); e != nil {
		t.Fatal(e)
	}
	app := model.Staff{Name: "王医生", Username: "c_app", Active: true, DepartmentID: 1}
	sub := model.Staff{Name: "张护士", Username: "c_sub", Active: true, DepartmentID: 1}
	db.Create(&app)
	db.Create(&sub)
	var day, night, rest model.Shift
	for _, sh := range []model.Shift{{Name: "白班", Kind: model.ShiftDay}, {Name: "夜班", Kind: model.ShiftNight}, {Name: "休息", Kind: model.ShiftRest}} {
		db.Create(&sh)
		if sh.Kind == model.ShiftDay {
			day = sh
		}
		if sh.Kind == model.ShiftNight {
			night = sh
		}
		if sh.Kind == model.ShiftRest {
			rest = sh
		}
	}
	db.Create(&model.ScheduleRule{DepartmentID: 1, MaxConsecutiveDays: 5, ForbidNightToDay: true})
	a := model.Schedule{DepartmentID: 1, StaffID: app.ID, ShiftID: day.ID, WorkDate: parseDay("2026-08-01")}
	b := model.Schedule{DepartmentID: 1, StaffID: sub.ID, ShiftID: night.ID, WorkDate: parseDay("2026-08-02")}
	db.Create(&a)
	db.Create(&b)
	db.Create(&model.Schedule{DepartmentID: 1, StaffID: app.ID, ShiftID: rest.ID, WorkDate: parseDay("2026-08-03")})
	db.Create(&model.Schedule{DepartmentID: 1, StaffID: sub.ID, ShiftID: rest.ID, WorkDate: parseDay("2026-07-31")})
	req := model.ShiftRequest{ApplicantID: app.ID, SubstituteID: sub.ID, ScheduleID: a.ID, SubstituteScheduleID: b.ID, Reason: "并发审批测试", Status: model.RequestPending}
	db.Create(&req)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewShiftRequestService(repository.NewShiftRequestRepository(db), repository.NewScheduleRepository(db), repository.NewScheduleRuleRepository(db), repository.NewAuditLogRepository(db), db, logger)

	const n = 8
	var wg sync.WaitGroup
	var success, alreadyReviewed int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			err := svc.Review(req.ID, 99, true)
			mu.Lock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrRequestAlreadyReviewed):
				alreadyReviewed++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("success count = %d, want exactly 1", success)
	}
	if success+alreadyReviewed != n {
		t.Fatalf("unexpected errors: success=%d alreadyReviewed=%d total=%d", success, alreadyReviewed, n)
	}
	var gotA, gotB model.Schedule
	db.First(&gotA, a.ID)
	db.First(&gotB, b.ID)
	if gotA.StaffID != sub.ID || gotB.StaffID != app.ID {
		t.Fatal("schedules not swapped exactly once")
	}
}

