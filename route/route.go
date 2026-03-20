package route

import (
	"gin_base/app/config"
	"gin_base/app/handler"
	"gin_base/app/middleware"

	"github.com/gin-gonic/gin"
)

var quotaMiddleware *middleware.QuotaMiddleware

// InitRouter 初始化路由
func InitRouter(e *gin.Engine, cfg *config.Config) error {
	// 创建配额中间件
	qm, err := middleware.NewQuotaMiddleware(cfg)
	if err != nil {
		return err
	}
	quotaMiddleware = qm

	// 全局中间件
	e.Use(middleware.Exception())

	// 健康检查（不受配额限制）
	e.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 管理接口
	adminHandler := handler.NewAdminHandler(quotaMiddleware.GetManager())
	admin := e.Group("/__admin")
	{
		admin.GET("/quota", adminHandler.GetQuota)
		admin.POST("/quota/reset", adminHandler.ResetQuota)
	}

	// 配额中间件处理所有其他请求
	e.NoRoute(quotaMiddleware.Handler())

	return nil
}

