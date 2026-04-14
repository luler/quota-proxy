package route

import (
	"gin_base/app/config"
	"gin_base/app/handler"
	"gin_base/app/middleware"

	"github.com/gin-gonic/gin"
)

// InitRouter 初始化路由
func InitRouter(e *gin.Engine, cfg *config.Config) error {
	store, err := middleware.NewRuntimeStore(cfg)
	if err != nil {
		return err
	}

	// 全局中间件
	e.Use(middleware.Exception())

	// 健康检查（不受配额限制）
	e.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 管理接口
	adminHandler := handler.NewAdminHandler(store)
	admin := e.Group("/__admin")
	{
		admin.GET("/ui", adminHandler.AdminUI)
		admin.POST("/login", adminHandler.Login)
	}

	adminProtected := e.Group("/__admin")
	adminProtected.Use(middleware.AdminAuth(store))
	{
		adminProtected.GET("/summary", adminHandler.GetSummary)
		adminProtected.GET("/quotas", adminHandler.ListQuotas)
		adminProtected.GET("/config", adminHandler.GetConfig)
		adminProtected.POST("/config/validate", adminHandler.ValidateConfig)
		adminProtected.POST("/config/save", adminHandler.SaveConfig)
		adminProtected.POST("/config/reload", adminHandler.ReloadConfig)
		adminProtected.GET("/quota", adminHandler.GetQuota)
		adminProtected.POST("/quota/reset", adminHandler.ResetQuota)
	}

	// 配额中间件处理所有其他请求
	e.NoRoute(store.Handler())

	return nil
}

