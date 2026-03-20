package identity

import (
	"gin_base/app/config"
	"strings"

	"github.com/gin-gonic/gin"
)

// Identifier 访问主体识别器
type Identifier struct {
	config *config.IdentityConfig
}

// NewIdentifier 创建身份识别器
func NewIdentifier(cfg *config.IdentityConfig) *Identifier {
	return &Identifier{
		config: cfg,
	}
}

// Identify 识别访问主体
// 按配置的 header 优先级检查，最后回退到 IP
func (i *Identifier) Identify(c *gin.Context) string {
	// 按 header 优先级检查
	for _, header := range i.config.Headers {
		value := c.GetHeader(header)
		if value != "" {
			return header + ":" + value
		}
	}

	// 回退到 IP
	if i.config.FallbackToIP {
		return "ip:" + c.ClientIP()
	}

	return "ip:" + c.ClientIP()
}

// GetIdentityType 获取主体类型
func (i *Identifier) GetIdentityType(identity string) string {
	parts := strings.SplitN(identity, ":", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return "unknown"
}

// GetIdentityValue 获取主体值
func (i *Identifier) GetIdentityValue(identity string) string {
	parts := strings.SplitN(identity, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return identity
}