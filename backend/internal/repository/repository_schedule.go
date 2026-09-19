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

// forUpdate 在支持行锁的数据库（MySQL）上对目标行加 FOR UPDATE 写锁。
// SQLite 靠数据库级写锁串行化写事务，不支持 FOR UPDATE 语法，因此跳过。
func forUpdate(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() == "sqlite" {
		return tx
	}
	return tx.Clauses(clause.Locking{Strength: "UPDATE"})
}

// FindForUpdate 必须在事务内调用：锁定排班行并预载班次，防止审批期间该行被其他事务改动。
func (r *ScheduleRepository) FindForUpdate(tx *gorm.DB, id uint) (model.Schedule, error) {
	var v model.Schedule
	e := forUpdate(tx).Preload("Shift").First(&v, id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, e
}

// ListByStaffIDs 返回指定人员的全部排班（含班次与人员信息），
// 供审批时在内存中模拟换班结果并做冲突检测。必须在同一事务内调用以保证读一致性。
func (r *ScheduleRepository) ListByStaffIDs(tx *gorm.DB, ids []uint) ([]model.Schedule, error) {
	var v []model.Schedule
	if len(ids) == 0 {
		return v, nil
	}
	e := forUpdate(tx).Preload("Staff").Preload("Shift").
		Where("staff_id IN ?", ids).Order("work_date ASC, id ASC").Find(&v).Error
	return v, e
}

// UpdateStaff 条件更新排班归属；只有当前归属仍为 wantOldStaff 时才生效，
// 用于事务内兜底并发改动。返回受影响行数。
func (r *ScheduleRepository) UpdateStaff(tx *gorm.DB, id, wantOldStaff, newStaff uint) (int64, error) {
	res := tx.Model(&model.Schedule{}).
		Where("id = ? AND staff_id = ?", id, wantOldStaff).
		Update("staff_id", newStaff)
	return res.RowsAffected, res.Error
}
