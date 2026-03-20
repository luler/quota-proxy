package middleware

import (
	"fmt"
	"gin_base/app/helper/log_helper"
	"github.com/gin-gonic/gin"
	"net/http"
	"reflect"
)

// 异常捕获中间件
func Exception() gin.HandlerFunc {
	return func(context *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log_helper.Error("Panic recovered", "panic", r)
				if err, ok := r.(error); ok {
					context.JSON(http.StatusInternalServerError, gin.H{
						"code":    500,
						"message": err.Error(),
					})
				} else {
					context.JSON(http.StatusInternalServerError, gin.H{
						"code":    500,
						"message": fmt.Sprintf("%v", reflect.ValueOf(r)),
					})
				}
				context.Abort()
			}
		}()

		context.Next()
	}
}