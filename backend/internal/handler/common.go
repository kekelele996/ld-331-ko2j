package handler

import (
	"errors"
	"net/http"

	"github.com/gbsched/hospital-scheduler/internal/constants"
	"github.com/gbsched/hospital-scheduler/internal/repository"
	"github.com/gbsched/hospital-scheduler/internal/service"
	"github.com/gbsched/hospital-scheduler/pkg/response"
	"github.com/gin-gonic/gin"
)

func handleError(c *gin.Context, e error) {
	if errors.Is(e, repository.ErrNotFound) {
		response.Error(c, http.StatusNotFound, constants.CodeNotFound, "资源不存在")
		return
	}
	// 换班合规冲突（夜班后接白班、连续工作超限、同日重复等）整体拒绝，回传逐条冲突说明。
	if service.IsConflict(e) {
		response.Error(c, http.StatusConflict, constants.CodeConflict, e.Error())
		return
	}
	// 重复/并发审批：只有一个请求能生效，其余按冲突处理以便前端刷新到最新状态。
	if errors.Is(e, service.ErrRequestAlreadyReviewed) {
		response.Error(c, http.StatusConflict, constants.CodeConflict, e.Error())
		return
	}
	response.Error(c, http.StatusBadRequest, constants.CodeBadRequest, e.Error())
}
func parseID(c *gin.Context) (uint, bool) {
	var id uint
	if e := c.ShouldBindUri(&struct {
		ID *uint `uri:"id" binding:"required"`
	}{ID: &id}); e != nil {
		response.Error(c, 400, constants.CodeBadRequest, "无效资源编号")
		return 0, false
	}
	return id, true
}
