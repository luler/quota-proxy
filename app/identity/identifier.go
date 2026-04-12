package identity

import (
	"gin_base/app/config"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// Identifier 访问主体识别器
type Identifier struct {
	config     *config.IdentityConfig
	extractors []compiledExtractor
}

type compiledExtractor struct {
	header string
	group  int
	name   string
	regex  *regexp.Regexp
	direct bool
}

// NewIdentifier 创建身份识别器
func NewIdentifier(cfg *config.IdentityConfig) *Identifier {
	extractors := make([]compiledExtractor, 0, len(cfg.Extractors))
	for _, extractor := range cfg.Extractors {
		compiled := compiledExtractor{
			header: extractor.Header,
			group:  extractor.Group,
			name:   extractor.Name,
			direct: extractor.Regex == "",
		}
		if !compiled.direct {
			compiled.regex = regexp.MustCompile(extractor.Regex)
		}
		extractors = append(extractors, compiled)
	}

	return &Identifier{
		config:     cfg,
		extractors: extractors,
	}
}

// Identify 识别访问主体
// 按 extractors 顺序提取，命中第一个后返回，最后回退到 IP
func (i *Identifier) Identify(c *gin.Context) string {
	for _, extractor := range i.extractors {
		value := c.GetHeader(extractor.header)
		if value == "" {
			continue
		}

		if extractor.direct {
			return extractor.name + ":" + value
		}

		matches := extractor.regex.FindStringSubmatch(value)
		if len(matches) == 0 {
			continue
		}

		return extractor.name + ":" + matches[extractor.group]
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
