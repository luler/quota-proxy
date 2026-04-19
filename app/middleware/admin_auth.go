package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func AdminAuth(store *RuntimeStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "运行时未初始化"})
			c.Abort()
			return
		}

		runtime := store.Current()
		if runtime == nil || runtime.Config == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "运行时未初始化"})
			c.Abort()
			return
		}

		apiKey := strings.TrimSpace(runtime.Config.Admin.APIKey)
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "管理鉴权未启用，请先在配置文件中设置 admin.api_key"})
			c.Abort()
			return
		}

		provided := strings.TrimSpace(c.GetHeader("X-API-Key"))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "管理密钥无效"})
			c.Abort()
			return
		}

		c.Next()
	}
}
