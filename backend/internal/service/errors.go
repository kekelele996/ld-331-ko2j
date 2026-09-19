package service

import "errors"

// ErrRequestAlreadyReviewed 表示申请已被审批，重复/并发审批不会二次生效。
var ErrRequestAlreadyReviewed = errors.New("该调班申请已审批，请勿重复操作")

// SwapConflictError 表示换班结果违反排班合规规则，审批整体拒绝。
// 其信息面向审批人，逐条说明具体冲突。
type SwapConflictError struct {
	Conflicts []string
}

func (e *SwapConflictError) Error() string {
	msg := "审批拒绝，换班后存在排班冲突："
	for i, c := range e.Conflicts {
		if i > 0 {
			msg += "；"
		}
		msg += c
	}
	return msg
}

// IsConflict 判断错误是否为换班合规冲突。
func IsConflict(e error) bool {
	var c *SwapConflictError
	return errors.As(e, &c)
}
