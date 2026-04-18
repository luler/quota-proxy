package identity

import (
	"gin_base/app/config"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// 策略常量
const (
	StrategyHeaderPriority = "header_priority"
	StrategyMergeAll       = "merge_all"
)

// merge_all 合并多个 extractor 结果时使用的分隔符
const mergeSeparator = "|"

// Identifier 访问主体识别器
type Identifier struct {
	config     *config.IdentityConfig
	strategy   string
	extractors []compiledExtractor
}

type compiledExtractor struct {
	source string // header / query / cookie
	key    string
	group  int
	name   string
	regex  *regexp.Regexp
	direct bool
}

// NewIdentifier 创建身份识别器
func NewIdentifier(cfg *config.IdentityConfig) *Identifier {
	strategy := strings.ToLower(strings.TrimSpace(cfg.Strategy))
	if strategy == "" {
		strategy = StrategyHeaderPriority
	}

	extractors := make([]compiledExtractor, 0, len(cfg.Extractors))
	for _, extractor := range cfg.Extractors {
		source := strings.ToLower(strings.TrimSpace(extractor.Source))
		if source == "" {
			source = "header"
		}
		key := strings.TrimSpace(extractor.Key)
		if key == "" && source == "header" {
			key = strings.TrimSpace(extractor.Header)
		}

		compiled := compiledExtractor{
			source: source,
			key:    key,
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
		strategy:   strategy,
		extractors: extractors,
	}
}

// Identify 识别访问主体
//   - header_priority（默认）：按 extractors 顺序命中第一个即返回
//   - merge_all：遍历所有 extractors，将命中结果按 name:value 拼接成单一标识
//     （未命中的 extractor 贡献空值，形如 app_id:X|user_id:）
//
// 所有 extractor 都未命中时回退到 IP
func (i *Identifier) Identify(c *gin.Context) string {
	if i.strategy == StrategyMergeAll {
		return i.identifyMerge(c)
	}
	return i.identifyPriority(c)
}

func (i *Identifier) identifyPriority(c *gin.Context) string {
	for idx := range i.extractors {
		if value, ok := i.extractValue(c, &i.extractors[idx]); ok {
			return i.extractors[idx].name + ":" + value
		}
	}
	return i.fallback(c)
}

func (i *Identifier) identifyMerge(c *gin.Context) string {
	if len(i.extractors) == 0 {
		return i.fallback(c)
	}

	parts := make([]string, 0, len(i.extractors))
	anyMatched := false
	for idx := range i.extractors {
		value, ok := i.extractValue(c, &i.extractors[idx])
		if ok {
			anyMatched = true
		}
		parts = append(parts, i.extractors[idx].name+":"+value)
	}

	if !anyMatched {
		return i.fallback(c)
	}
	return strings.Join(parts, mergeSeparator)
}

func (i *Identifier) extractValue(c *gin.Context, ex *compiledExtractor) (string, bool) {
	raw := i.readRaw(c, ex)
	if raw == "" {
		return "", false
	}
	if ex.direct {
		return raw, true
	}
	matches := ex.regex.FindStringSubmatch(raw)
	if len(matches) == 0 || ex.group >= len(matches) {
		return "", false
	}
	return matches[ex.group], true
}

func (i *Identifier) readRaw(c *gin.Context, ex *compiledExtractor) string {
	if c == nil || c.Request == nil || ex.key == "" {
		return ""
	}
	switch ex.source {
	case "header":
		return c.GetHeader(ex.key)
	case "query":
		return c.Query(ex.key)
	case "cookie":
		value, err := c.Cookie(ex.key)
		if err != nil {
			return ""
		}
		return value
	default:
		return ""
	}
}

func (i *Identifier) fallback(c *gin.Context) string {
	if i.config != nil && !i.config.FallbackToIP {
		return "ip:" + c.ClientIP()
	}
	return "ip:" + c.ClientIP()
}

// GetIdentityType 获取主体类型
func (i *Identifier) GetIdentityType(identity string) string {
	// merge_all 下 identity 形如 app_id:X|user_id:Y，类型取第一段
	head := identity
	if idx := strings.Index(identity, mergeSeparator); idx > 0 {
		head = identity[:idx]
	}
	parts := strings.SplitN(head, ":", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return "unknown"
}

// GetIdentityValue 获取主体值
func (i *Identifier) GetIdentityValue(identity string) string {
	head := identity
	if idx := strings.Index(identity, mergeSeparator); idx > 0 {
		head = identity[:idx]
	}
	parts := strings.SplitN(head, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return identity
}
