package repository

import (
	"errors"
	"github.com/gbsched/hospital-scheduler/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ShiftRequestRepository struct{ db *gorm.DB }

func NewShiftRequestRepository(db *gorm.DB) *ShiftRequestRepository {
	return &ShiftRequestRepository{db}
}
func (r *ShiftRequestRepository) Create(v *model.ShiftRequest) error { return r.db.Create(v).Error }
func (r *ShiftRequestRepository) Save(v *model.ShiftRequest) error   { return r.db.Save(v).Error }
func (r *ShiftRequestRepository) Find(id uint) (model.ShiftRequest, error) {
	var v model.ShiftRequest
	e := r.db.Preload("Applicant").Preload("Substitute").First(&v, id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, e
}
func (r *ShiftRequestRepository) List() ([]model.ShiftRequest, error) {
	var v []model.ShiftRequest
	e := r.db.
		Preload("Applicant").Preload("Substitute").
		Preload("Schedule").Preload("Schedule.Shift").
		Preload("SubstituteSchedule").Preload("SubstituteSchedule.Shift").
		Order("id desc").Find(&v).Error
	return v, e
}

// FindForUpdate 在事务内锁定申请行并预载关联信息，保证并发审批中只有一个事务能拿到待审批状态。
func (r *ShiftRequestRepository) FindForUpdate(tx *gorm.DB, id uint) (model.ShiftRequest, error) {
	var v model.ShiftRequest
	q := tx.Preload("Applicant").Preload("Substitute")
	if tx.Dialector.Name() != "sqlite" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	e := q.First(&v, id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, e
}

// UpdateStatusIfPending 是“只成功一次”的兜底：仅当申请仍为 pending 时才更新审批结果，
// 返回受影响行数（1 表示本次审批生效，0 表示已被其他并发事务处理）。
func (r *ShiftRequestRepository) UpdateStatusIfPending(tx *gorm.DB, id, reviewer uint, status model.RequestStatus) (int64, error) {
	res := tx.Model(&model.ShiftRequest{}).
		Where("id = ? AND status = ?", id, model.RequestPending).
		Updates(map[string]any{
			"status":      status,
			"reviewer_id": reviewer,
			"reviewed_at": tx.NowFunc(),
		})
	return res.RowsAffected, res.Error
}
