package middleware

import (
	"gin_base/app/helper/log_helper"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 异常捕获中间件
func Exception() gin.HandlerFunc {
	return func(context *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log_helper.Error("Panic recovered", "panic", r)
				context.JSON(http.StatusInternalServerError, gin.H{
					"code":    500,
					"message": "服务器内部错误",
				})
				context.Abort()
			}
		}()

		context.Next()
	}
}
