package repository

import (
	"errors"
	"github.com/gbsched/hospital-scheduler/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type ScheduleRepository struct{ db *gorm.DB }

func NewScheduleRepository(db *gorm.DB) *ScheduleRepository  { return &ScheduleRepository{db} }
func (r *ScheduleRepository) Create(v *model.Schedule) error { return r.db.Create(v).Error }
func (r *ScheduleRepository) Save(v *model.Schedule) error   { return r.db.Save(v).Error }
func (r *ScheduleRepository) List(dept, staff uint, from, to time.Time) ([]model.Schedule, error) {
	var v []model.Schedule
	q := r.db.Preload("Staff").Preload("Shift").Order("work_date,staff_id")
	if dept > 0 {
		q = q.Where("department_id=?", dept)
	}
	if staff > 0 {
		q = q.Where("staff_id=?", staff)
	}
	if !from.IsZero() {
		q = q.Where("work_date>=?", from)
	}
	if !to.IsZero() {
		q = q.Where("work_date<=?", to)
	}
	return v, q.Find(&v).Error
}
func (r *ScheduleRepository) Find(id uint) (model.Schedule, error) {
	var v model.Schedule
	e := r.db.First(&v, id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, e
}
func (r *ScheduleRepository) DeleteRange(dept uint, from, to time.Time) error {
	return r.db.Where("department_id=? AND work_date>=? AND work_date<=?", dept, from, to).Delete(&model.Schedule{}).Error
}

// FindForUpdateTx loads a schedule with its shift inside a transaction and takes
// a row lock on MySQL so concurrent approvals cannot swap the same pair twice.
func (r *ScheduleRepository) FindForUpdateTx(tx *gorm.DB, id uint) (model.Schedule, error) {
	var v model.Schedule
	q := tx.Preload("Staff").Preload("Shift").Where("id = ?", id)
	if tx.Dialector.Name() == "mysql" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if e := q.First(&v).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return v, ErrNotFound
		}
		return v, e
	}
	return v, nil
}

// StaffRangeTx lists every schedule of the given staff within [from,to] inside a
// transaction, with shifts preloaded for rule evaluation.
func (r *ScheduleRepository) StaffRangeTx(tx *gorm.DB, staffID uint, from, to time.Time) ([]model.Schedule, error) {
	return r.StaffSetRangeTx(tx, []uint{staffID}, from, to)
}

// StaffSetRangeTx lists the schedules of several staff within [from,to]. The
// union is required when simulating a swap: the incoming schedule belongs to
// the other party in the database but must be evaluated for the receiver.
func (r *ScheduleRepository) StaffSetRangeTx(tx *gorm.DB, staffIDs []uint, from, to time.Time) ([]model.Schedule, error) {
	var v []model.Schedule
	e := tx.Preload("Staff").Preload("Shift").
		Where("staff_id IN ? AND work_date >= ? AND work_date <= ?", staffIDs, from, to).
		Order("work_date, id").
		Find(&v).Error
	return v, e
}

// SwapStaffTx exchanges the staff of two schedules in one UPDATE, so the board
// can never observe a half-applied swap.
func (r *ScheduleRepository) SwapStaffTx(tx *gorm.DB, scheduleA, scheduleB model.Schedule) error {
	return tx.Model(&model.Schedule{}).Where("id IN ?", []uint{scheduleA.ID, scheduleB.ID}).
		UpdateColumn("staff_id", gorm.Expr(
			"CASE id WHEN ? THEN ? WHEN ? THEN ? END",
			scheduleA.ID, scheduleB.StaffID, scheduleB.ID, scheduleA.StaffID,
		)).Error
}
