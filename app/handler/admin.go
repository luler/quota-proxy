package handler

import (
	"gin_base/app/quota"

	"github.com/gin-gonic/gin"
)

// AdminHandler 管理接口处理器
type AdminHandler struct {
	manager *quota.Manager
}

// NewAdminHandler 创建管理接口处理器
func NewAdminHandler(manager *quota.Manager) *AdminHandler {
	return &AdminHandler{
		manager: manager,
	}
}

// Health 健康检查
func (h *AdminHandler) Health(c *gin.Context) {
	c.JSON(200, gin.H{
		"status": "ok",
	})
}

// GetQuota 查看配额使用情况
func (h *AdminHandler) GetQuota(c *gin.Context) {
	identity := c.Query("identity")
	if identity == "" {
		c.JSON(400, gin.H{
			"code":    400,
			"message": "identity 参数必填",
		})
		return
	}

	ruleName := c.DefaultQuery("rule", "")
	if ruleName == "" {
		statuses, err := h.manager.GetAllStatus(identity)
		if err != nil {
			c.JSON(500, gin.H{
				"code":    500,
				"message": "查询配额失败",
				"error":   err.Error(),
			})
			return
		}

		c.JSON(200, gin.H{
			"identity": identity,
			"quotas":   statuses,
			"rules":    h.manager.ListRuleNames(),
		})
		return
	}

	status, err := h.manager.GetStatus(ruleName, identity)
	if err != nil {
		c.JSON(500, gin.H{
			"code":    500,
			"message": "查询配额失败",
			"error":   err.Error(),
		})
		return
	}

	c.JSON(200, gin.H{
		"identity":      identity,
		"rule":          status.RuleName,
		"window":        status.Window,
		"period_key":    status.PeriodKey,
		"success_count": status.Success,
		"pending_count": status.Pending,
		"limit":         status.Limit,
		"remaining":     status.Remaining,
		"rules":         h.manager.ListRuleNames(),
	})
}

// ResetQuota 重置配额
func (h *AdminHandler) ResetQuota(c *gin.Context) {
	var req struct {
		Identity string `json:"identity" binding:"required"`
		Rule     string `json:"rule"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{
			"code":    400,
			"message": "identity 参数必填",
		})
		return
	}

	if req.Rule == "" {
		if err := h.manager.ResetAll(req.Identity); err != nil {
			c.JSON(500, gin.H{
				"code":    500,
				"message": "重置配额失败",
				"error":   err.Error(),
			})
			return
		}

		c.JSON(200, gin.H{
			"code":     200,
			"message":  "所有配额已重置",
			"identity": req.Identity,
			"rules":    h.manager.ListRuleNames(),
		})
		return
	}

	if err := h.manager.Reset(req.Rule, req.Identity); err != nil {
		c.JSON(500, gin.H{
			"code":    500,
			"message": "重置配额失败",
			"error":   err.Error(),
		})
		return
	}

	c.JSON(200, gin.H{
		"code":     200,
		"message":  "配额已重置",
		"identity": req.Identity,
		"rule":     req.Rule,
	})
}
