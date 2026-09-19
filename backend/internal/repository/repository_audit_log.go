package repository

import (
	"github.com/gbsched/hospital-scheduler/internal/model"
	"gorm.io/gorm"
)

type AuditLogRepository struct{ db *gorm.DB }

func NewAuditLogRepository(db *gorm.DB) *AuditLogRepository  { return &AuditLogRepository{db} }
func (r *AuditLogRepository) Create(v *model.AuditLog) error { return r.db.Create(v).Error }

// CreateOn 在指定句柄（审批事务）上写审计记录，与排班交换、申请状态更新同生共死。
func (r *AuditLogRepository) CreateOn(tx *gorm.DB, v *model.AuditLog) error {
	return tx.Create(v).Error
}
func (r *AuditLogRepository) List() ([]model.AuditLog, error) {
	var v []model.AuditLog
	return v, r.db.Order("id desc").Limit(100).Find(&v).Error
}
