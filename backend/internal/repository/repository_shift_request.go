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

// requestPreloads eagerly loads the people and the two schedule/shift records so
// list views stay consistent with the current schedule board after an approval.
var requestPreloads = []string{"Applicant", "Substitute", "Schedule", "Schedule.Shift", "SubstituteSchedule", "SubstituteSchedule.Shift"}

func (r *ShiftRequestRepository) Find(id uint) (model.ShiftRequest, error) {
	var v model.ShiftRequest
	q := r.db
	for _, p := range requestPreloads {
		q = q.Preload(p)
	}
	e := q.First(&v, id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, e
}
func (r *ShiftRequestRepository) List() ([]model.ShiftRequest, error) {
	var v []model.ShiftRequest
	q := r.db
	for _, p := range requestPreloads {
		q = q.Preload(p)
	}
	return v, q.Order("id desc").Find(&v).Error
}

// LockPendingTx locks the request row inside tx and verifies it is still
// pending. A concurrent reviewer blocks here until the holder commits or rolls
// back; a non-pending row yields ErrAlreadyReviewed so a request is only
// processed once.
func (r *ShiftRequestRepository) LockPendingTx(tx *gorm.DB, id uint) (model.ShiftRequest, error) {
	var v model.ShiftRequest
	q := tx.Where("id = ?", id)
	if tx.Dialector.Name() == "mysql" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if e := q.First(&v).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return v, ErrNotFound
		}
		return v, e
	}
	if v.Status != model.RequestPending {
		return v, ErrAlreadyReviewed
	}
	return v, nil
}

// FinalizeTx transitions the pending request to nextStatus in one conditional
// UPDATE. The row lock normally guarantees success; the pending predicate is
// retained as defence in depth. Zero affected rows means the claim was lost.
func (r *ShiftRequestRepository) FinalizeTx(tx *gorm.DB, id, reviewer uint, nextStatus model.RequestStatus) error {
	res := tx.Model(&model.ShiftRequest{}).
		Where("id = ? AND status = ?", id, model.RequestPending).
		Updates(map[string]any{
			"status":      nextStatus,
			"reviewer_id": reviewer,
			"reviewed_at": gorm.Expr("CURRENT_TIMESTAMP"),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrAlreadyReviewed
	}
	return nil
}
