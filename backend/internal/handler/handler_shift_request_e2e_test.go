package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gbsched/hospital-scheduler/internal/handler"
	"github.com/gbsched/hospital-scheduler/internal/model"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"github.com/gbsched/hospital-scheduler/internal/router"
	"github.com/gbsched/hospital-scheduler/internal/service"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type e2eEnv struct {
	t       *testing.T
	router  http.Handler
	db      *gorm.DB
	tokens  map[string]string
	staffID map[string]uint
	shiftID map[model.ShiftKind]uint
	deptID  uint
}

func setupE2E(t *testing.T) *e2eEnv {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "e2e.db")
	db, e := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if e != nil {
		t.Fatalf("open db: %v", e)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if e = db.AutoMigrate(
		&model.Department{}, &model.Position{}, &model.Staff{}, &model.Shift{},
		&model.Schedule{}, &model.ShiftRequest{}, &model.ScheduleRule{},
		&model.Holiday{}, &model.AuditLog{},
	); e != nil {
		t.Fatalf("migrate: %v", e)
	}

	dept := model.Department{Name: "内科"}
	db.Create(&dept)
	pos := model.Position{DepartmentID: dept.ID, Name: "主治医师"}
	db.Create(&pos)
	hash, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
	people := []model.Staff{
		{Name: "主管", Username: "sup", PasswordHash: string(hash), Role: model.RoleSupervisor, DepartmentID: dept.ID, PositionID: pos.ID, Active: true},
		{Name: "王医生", Username: "doc", PasswordHash: string(hash), Role: model.RoleStaff, DepartmentID: dept.ID, PositionID: pos.ID, Active: true},
		{Name: "张护士", Username: "nur", PasswordHash: string(hash), Role: model.RoleStaff, DepartmentID: dept.ID, PositionID: pos.ID, Active: true},
	}
	staffID := map[string]uint{}
	for i := range people {
		db.Create(&people[i])
		staffID[people[i].Username] = people[i].ID
	}
	shiftID := map[model.ShiftKind]uint{}
	for _, sh := range []model.Shift{
		{Name: "白班", Kind: model.ShiftDay},
		{Name: "中班", Kind: model.ShiftEvening},
		{Name: "夜班", Kind: model.ShiftNight},
		{Name: "休息", Kind: model.ShiftRest},
	} {
		db.Create(&sh)
		shiftID[sh.Kind] = sh.ID
	}
	db.Create(&model.ScheduleRule{DepartmentID: dept.ID, MaxConsecutiveDays: 5, ForbidNightToDay: true})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	depRepo := repository.NewDepartmentRepository(db)
	posRepo := repository.NewPositionRepository(db)
	staffRepo := repository.NewStaffRepository(db)
	shiftRepo := repository.NewShiftRepository(db)
	schedRepo := repository.NewScheduleRepository(db)
	reqRepo := repository.NewShiftRequestRepository(db)
	ruleRepo := repository.NewScheduleRuleRepository(db)
	holidayRepo := repository.NewHolidayRepository(db)
	auditRepo := repository.NewAuditLogRepository(db)

	staffSvc := service.NewStaffService(staffRepo, logger)
	schedSvc := service.NewScheduleService(schedRepo, staffRepo, shiftRepo, ruleRepo, logger)
	reqSvc := service.NewShiftRequestService(reqRepo, schedRepo, ruleRepo, db, logger)

	secret := "e2e-secret"
	r := router.New(router.Handlers{
		Auth:        handler.NewAuthHandler(staffSvc, secret),
		Departments: handler.NewDepartmentHandler(service.NewDepartmentService(depRepo, logger)),
		Positions:   handler.NewPositionHandler(service.NewPositionService(posRepo, logger)),
		Staff:       handler.NewStaffHandler(staffSvc),
		Schedules:   handler.NewScheduleHandler(schedSvc),
		Requests:    handler.NewShiftRequestHandler(reqSvc),
		Rules:       handler.NewRuleHandler(service.NewRuleService(ruleRepo, logger)),
		Holidays:    handler.NewHolidayHandler(service.NewHolidayService(holidayRepo, logger)),
		Audit:       handler.NewAuditHandler(service.NewAuditService(auditRepo, logger)),
	}, secret, logger)

	env := &e2eEnv{t: t, router: r, db: db, staffID: staffID, shiftID: shiftID, deptID: dept.ID, tokens: map[string]string{}}
	for _, u := range []string{"sup", "doc", "nur"} {
		env.tokens[u] = env.login(u, "admin123")
	}
	return env
}

func (e *e2eEnv) login(username, password string) string {
	code, body := e.call(http.MethodPost, "/api/v1/auth/login", "", map[string]any{"username": username, "password": password})
	if code != http.StatusOK {
		e.t.Fatalf("login %s: code=%d body=%s", username, code, body)
	}
	var token string
	if jerr := json.Unmarshal(getJSON(body, "data", "token"), &token); jerr != nil {
		e.t.Fatalf("decode token: %v", jerr)
	}
	return token
}

// call performs a request and returns HTTP status and raw response body.
func (e *e2eEnv) call(method, path, token string, payload any) (int, []byte) {
	var reader *bytes.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func getJSON(body []byte, path ...string) json.RawMessage {
	var raw json.RawMessage = body
	for _, key := range path {
		var doc map[string]json.RawMessage
		if e := json.Unmarshal(raw, &doc); e != nil {
			return nil
		}
		raw = doc[key]
	}
	return raw
}

// scheduleRow is one item returned by GET /schedules.
type scheduleRow struct {
	ID       uint   `json:"id"`
	StaffID  uint   `json:"staff_id"`
	ShiftID  uint   `json:"shift_id"`
	WorkDate string `json:"work_date"`
	Shift    struct {
		Kind string `json:"kind"`
	} `json:"shift"`
}

func (e *e2eEnv) listSchedules(staff uint, from, to string) []scheduleRow {
	path := fmt.Sprintf("/api/v1/schedules?department_id=%d&staff_id=%d&from=%s&to=%s", e.deptID, staff, from, to)
	code, body := e.call(http.MethodGet, path, e.tokens["doc"], nil)
	if code != http.StatusOK {
		e.t.Fatalf("list schedules: %d %s", code, body)
	}
	var rows []scheduleRow
	if e2 := json.Unmarshal(getJSON(body, "data"), &rows); e2 != nil {
		e.t.Fatalf("decode schedules: %v", e2)
	}
	return rows
}

func (e *e2eEnv) mustSchedule(staff uint, date string, kind model.ShiftKind) scheduleRow {
	for _, r := range e.listSchedules(staff, date, date) {
		if r.WorkDate[:10] == date && r.Shift.Kind == string(kind) {
			return r
		}
	}
	e.t.Fatalf("schedule not found: staff=%d date=%s kind=%s", staff, date, kind)
	return scheduleRow{}
}

// 端到端：冲突换班返回 409 并带具体冲突说明，排班不变。
func TestE2EConflictRejectedWithDetails(t *testing.T) {
	e := setupE2E(t)
	d1, d2, d3 := "2026-11-01", "2026-11-02", "2026-11-03"
	doc, nur := e.staffID["doc"], e.staffID["nur"]
	day, night := e.shiftID[model.ShiftDay], e.shiftID[model.ShiftNight]
	e.db.Create(&model.Schedule{DepartmentID: e.deptID, StaffID: doc, ShiftID: day, WorkDate: parseDate(d1)})
	e.db.Create(&model.Schedule{DepartmentID: e.deptID, StaffID: doc, ShiftID: day, WorkDate: parseDate(d3)})
	e.db.Create(&model.Schedule{DepartmentID: e.deptID, StaffID: nur, ShiftID: night, WorkDate: parseDate(d2)})
	a := e.mustSchedule(doc, d1, model.ShiftDay)
	b := e.mustSchedule(nur, d2, model.ShiftNight)

	code, body := e.call(http.MethodPost, "/api/v1/shift-requests", e.tokens["doc"], map[string]any{
		"schedule_id": a.ID, "substitute_id": nur, "substitute_schedule_id": b.ID, "reason": "个人原因申请调班",
	})
	if code != http.StatusOK {
		t.Fatalf("create request: %d %s", code, body)
	}
	var idVal uint
	json.Unmarshal(getJSON(body, "data", "id"), &idVal)
	reqID := idVal

	// Approve: nurse's night moves to the doctor on d2, followed by the doctor's d3 day.
	code, body = e.call(http.MethodPut, fmt.Sprintf("/api/v1/shift-requests/%d/review", reqID), e.tokens["sup"], map[string]any{"approved": true})
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", code, body)
	}
	var msg string
	json.Unmarshal(getJSON(body, "message"), &msg)
	if !containsRaw(msg, "夜班后次日接白班") {
		t.Fatalf("expected night-to-day detail, got %s", msg)
	}

	// Request stays pending and schedules are untouched.
	code, body = e.call(http.MethodGet, "/api/v1/shift-requests", e.tokens["sup"], nil)
	if !containsRaw(string(body), `"status":"pending"`) {
		t.Fatalf("request should remain pending: %s", body)
	}
	if got := e.mustSchedule(doc, d1, model.ShiftDay); got.StaffID != doc {
		t.Fatalf("applicant schedule changed after rejected swap")
	}
}

// 端到端：非本人班次不能发起；合规换班原子生效；重复审批只有一次成功。
func TestE2EValidSwapAndDuplicateReview(t *testing.T) {
	e := setupE2E(t)
	d1, d2 := "2026-12-10", "2026-12-20"
	doc, nur := e.staffID["doc"], e.staffID["nur"]
	e.db.Create(&model.Schedule{DepartmentID: e.deptID, StaffID: doc, ShiftID: e.shiftID[model.ShiftDay], WorkDate: parseDate(d1)})
	e.db.Create(&model.Schedule{DepartmentID: e.deptID, StaffID: nur, ShiftID: e.shiftID[model.ShiftRest], WorkDate: parseDate(d2)})
	a := e.mustSchedule(doc, d1, model.ShiftDay)
	b := e.mustSchedule(nur, d2, model.ShiftRest)

	// The doctor must not be able to offer someone else's schedule.
	code, body := e.call(http.MethodPost, "/api/v1/shift-requests", e.tokens["doc"], map[string]any{
		"schedule_id": b.ID, "substitute_id": nur, "substitute_schedule_id": a.ID, "reason": "不符合归属的申请",
	})
	if code != http.StatusBadRequest || !containsRaw(string(body), "只能申请调换本人的班次") {
		t.Fatalf("expected ownership 400, got %d %s", code, body)
	}

	// Legitimate request.
	code, body = e.call(http.MethodPost, "/api/v1/shift-requests", e.tokens["doc"], map[string]any{
		"schedule_id": a.ID, "substitute_id": nur, "substitute_schedule_id": b.ID, "reason": "合规调班申请",
	})
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	var idVal2 uint
	json.Unmarshal(getJSON(body, "data", "id"), &idVal2)
	reqID := idVal2

	// First approval succeeds.
	code, body = e.call(http.MethodPut, fmt.Sprintf("/api/v1/shift-requests/%d/review", reqID), e.tokens["sup"], map[string]any{"approved": true})
	if code != http.StatusOK {
		t.Fatalf("first review: %d %s", code, body)
	}
	// Duplicate/concurrent approval fails with 409.
	code, body = e.call(http.MethodPut, fmt.Sprintf("/api/v1/shift-requests/%d/review", reqID), e.tokens["sup"], map[string]any{"approved": true})
	if code != http.StatusConflict || !containsRaw(string(body), "已完成审批") {
		t.Fatalf("duplicate review expected 409, got %d %s", code, body)
	}

	// Refresh-equivalent: the board and the request agree.
	if got := e.mustSchedule(nur, d1, model.ShiftDay); got.StaffID != nur {
		t.Fatalf("applicant schedule not swapped to substitute")
	}
	if got := e.mustSchedule(doc, d2, model.ShiftRest); got.StaffID != doc {
		t.Fatalf("substitute schedule not swapped to applicant")
	}
	code, body = e.call(http.MethodGet, "/api/v1/shift-requests", e.tokens["sup"], nil)
	if !containsRaw(string(body), `"status":"approved"`) {
		t.Fatalf("request should be approved after refresh: %s", body)
	}
}

func parseDate(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}
func containsRaw(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
