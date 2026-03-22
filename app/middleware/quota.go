package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"gin_base/app/config"
	"gin_base/app/helper/log_helper"
	"gin_base/app/identity"
	"gin_base/app/proxy"
	"gin_base/app/quota"
	"gin_base/app/success"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type compiledRegexMatcher struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// QuotaMiddleware 配额中间件
type QuotaMiddleware struct {
	identifier *identity.Identifier
	judge      success.Judge
	manager    *quota.Manager
	proxy      *proxy.ReverseProxy
	config     *config.Config
	matchers   map[string]compiledRuleMatcher
}

type compiledRuleMatcher struct {
	queryForm *compiledRegexMatcher
	jsonBody  *compiledRegexMatcher
	headers   *compiledRegexMatcher
}

// NewQuotaMiddleware 创建配额中间件
func NewQuotaMiddleware(cfg *config.Config) (*QuotaMiddleware, error) {
	identifier := identity.NewIdentifier(&cfg.Identity)
	judge := success.NewJudge(&cfg.SuccessRule)
	manager, err := quota.NewManager(cfg)
	if err != nil {
		return nil, err
	}
	p, err := proxy.NewReverseProxy(&cfg.Upstream)
	if err != nil {
		return nil, err
	}
	matchers, err := compileRuleMatchers(cfg.Quota.Rules)
	if err != nil {
		return nil, err
	}

	return &QuotaMiddleware{
		identifier: identifier,
		judge:      judge,
		manager:    manager,
		proxy:      p,
		config:     cfg,
		matchers:   matchers,
	}, nil
}

// Handler 返回中间件处理函数
func (m *QuotaMiddleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()

		if !m.manager.IsEnabled() {
			m.forwardRequest(c, startTime)
			return
		}

		rule := m.matchRule(c)
		if rule == nil {
			m.forwardRequest(c, startTime)
			return
		}

		ident := m.identifier.Identify(c)
		reserved, successCount, pendingCount, err := m.manager.TryReserve(rule, ident)
		if err != nil {
			if m.manager.IsFailOpen() {
				log_helper.Error("Redis error, fail-open mode", "error", err)
				m.forwardRequest(c, startTime)
				return
			}
			m.respondError(c, http.StatusServiceUnavailable, "服务暂时不可用")
			return
		}

		if !reserved {
			log_helper.Info("Quota exceeded",
				"rule", rule.Name,
				"identity", ident,
				"success", successCount,
				"pending", pendingCount)
			m.respondQuotaExceeded(c, rule)
			return
		}

		resp, err := m.proxy.Do(c)
		if err != nil {
			m.rollbackPending(rule, ident)
			m.respondError(c, http.StatusBadGateway, "上游服务不可用")
			return
		}

		if m.proxy.IsSSE(resp) {
			m.handleSSE(c, rule, ident, resp, startTime)
			return
		}

		m.handleBufferedResponse(c, rule, ident, resp, startTime)
	}
}

func (m *QuotaMiddleware) handleBufferedResponse(c *gin.Context, rule *config.QuotaRuleConfig, ident string, resp *http.Response, startTime time.Time) {
	result, err := m.proxy.ReadResponse(resp)
	if err != nil {
		m.rollbackPending(rule, ident)
		m.respondError(c, http.StatusBadGateway, "上游服务不可用")
		return
	}

	isSuccess := m.judge.IsSuccess(&http.Response{StatusCode: result.StatusCode}, result.Body)
	if isSuccess {
		m.confirmPending(rule, ident)
	} else {
		m.rollbackPending(rule, ident)
	}

	m.proxy.WriteResponse(c, result)

	elapsed := time.Since(startTime)
	log_helper.Info("Request completed",
		"rule", rule.Name,
		"path", c.Request.URL.Path,
		"method", c.Request.Method,
		"identity", ident,
		"status", result.StatusCode,
		"success", isSuccess,
		"elapsed", elapsed.String())
}

func (m *QuotaMiddleware) handleSSE(c *gin.Context, rule *config.QuotaRuleConfig, ident string, resp *http.Response, startTime time.Time) {
	if !m.isSSEStatusSuccess(resp.StatusCode) {
		m.rollbackPending(rule, ident)
		result, err := m.proxy.ReadResponse(resp)
		if err != nil {
			m.respondError(c, http.StatusBadGateway, "上游服务不可用")
			return
		}

		m.proxy.WriteResponse(c, result)

		elapsed := time.Since(startTime)
		log_helper.Info("SSE request rejected by status",
			"rule", rule.Name,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"identity", ident,
			"status", result.StatusCode,
			"elapsed", elapsed.String())
		return
	}

	confirmed, err := m.proxy.StreamSSE(c, resp, func() error {
		if err := m.manager.Confirm(rule, ident); err != nil {
			log_helper.Error("Failed to confirm SSE success", "error", err)
			return err
		}
		log_helper.Info("SSE first event confirmed",
			"rule", rule.Name,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"identity", ident,
			"status", resp.StatusCode)
		return nil
	})

	if err != nil {
		if !confirmed {
			m.rollbackPending(rule, ident)
			if !c.Writer.Written() {
				m.respondError(c, http.StatusBadGateway, "上游服务不可用")
			}
			log_helper.Error("SSE request rolled back before first event",
				"rule", rule.Name,
				"path", c.Request.URL.Path,
				"method", c.Request.Method,
				"identity", ident,
				"error", err)
			return
		}

		log_helper.Error("SSE stream ended after confirm",
			"rule", rule.Name,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"identity", ident,
			"error", err)
		return
	}

	if !confirmed {
		m.rollbackPending(rule, ident)
		log_helper.Error("SSE stream ended before first event",
			"rule", rule.Name,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"identity", ident)
		return
	}

	elapsed := time.Since(startTime)
	log_helper.Info("SSE request completed",
		"rule", rule.Name,
		"path", c.Request.URL.Path,
		"method", c.Request.Method,
		"identity", ident,
		"status", resp.StatusCode,
		"elapsed", elapsed.String())
}

func (m *QuotaMiddleware) confirmPending(rule *config.QuotaRuleConfig, ident string) {
	if err := m.manager.Confirm(rule, ident); err != nil {
		log_helper.Error("Failed to confirm success", "error", err)
	}
}

func (m *QuotaMiddleware) rollbackPending(rule *config.QuotaRuleConfig, ident string) {
	if err := m.manager.Rollback(rule, ident); err != nil {
		log_helper.Error("Failed to rollback pending", "error", err)
	}
}

func (m *QuotaMiddleware) isSSEStatusSuccess(statusCode int) bool {
	if m.config.SuccessRule.RequireHTTP2xx {
		return statusCode >= 200 && statusCode < 300
	}
	return statusCode < 400
}

// forwardRequest 直接转发请求（不进行配额检查）
func (m *QuotaMiddleware) forwardRequest(c *gin.Context, startTime time.Time) {
	resp, err := m.proxy.Do(c)
	if err != nil {
		m.respondError(c, http.StatusBadGateway, "上游服务不可用")
		return
	}

	if m.proxy.IsSSE(resp) {
		if _, err := m.proxy.StreamSSE(c, resp, nil); err != nil {
			if !c.Writer.Written() {
				m.respondError(c, http.StatusBadGateway, "上游服务不可用")
			}
			log_helper.Error("SSE request forward failed",
				"path", c.Request.URL.Path,
				"method", c.Request.Method,
				"error", err)
			return
		}

		elapsed := time.Since(startTime)
		log_helper.Info("SSE request forwarded",
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"status", resp.StatusCode,
			"elapsed", elapsed.String())
		return
	}

	result, err := m.proxy.ReadResponse(resp)
	if err != nil {
		m.respondError(c, http.StatusBadGateway, "上游服务不可用")
		return
	}

	m.proxy.WriteResponse(c, result)

	elapsed := time.Since(startTime)
	log_helper.Info("Request forwarded",
		"path", c.Request.URL.Path,
		"method", c.Request.Method,
		"status", result.StatusCode,
		"elapsed", elapsed.String())
}

func (m *QuotaMiddleware) matchRule(c *gin.Context) *config.QuotaRuleConfig {
	path := c.Request.URL.Path

	for _, excludePath := range m.config.Quota.ExcludePaths {
		if m.matchPath(path, excludePath) {
			return nil
		}
	}

	for i := range m.config.Quota.Rules {
		rule := &m.config.Quota.Rules[i]
		for _, includePath := range rule.IncludePaths {
			if m.matchPath(path, includePath) && m.matchRequest(rule, c) {
				return rule
			}
		}
	}

	return nil
}

func (m *QuotaMiddleware) matchRequest(rule *config.QuotaRuleConfig, c *gin.Context) bool {
	matcher := m.matchers[rule.Name]
	if matcher.queryForm == nil && matcher.jsonBody == nil && matcher.headers == nil {
		return true
	}

	if matcher.queryForm != nil {
		canonical, err := m.canonicalizeQueryForm(c)
		if err != nil || !matchRegexDomain(canonical, matcher.queryForm) {
			return false
		}
	}

	if matcher.jsonBody != nil {
		canonical, err := m.canonicalizeJSONBody(c)
		if err != nil || !matchRegexDomain(canonical, matcher.jsonBody) {
			return false
		}
	}

	if matcher.headers != nil {
		canonical := m.canonicalizeHeaders(c)
		if !matchRegexDomain(canonical, matcher.headers) {
			return false
		}
	}

	return true
}

func (m *QuotaMiddleware) readRequestBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}

	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

func (m *QuotaMiddleware) canonicalizeQueryForm(c *gin.Context) (string, error) {
	values := url.Values{}
	for key, vals := range c.Request.URL.Query() {
		for _, value := range vals {
			values.Add(key, value)
		}
	}

	body, err := m.readRequestBody(c)
	if err != nil {
		return "", err
	}

	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if len(body) > 0 && (strings.Contains(contentType, "application/x-www-form-urlencoded") || strings.Contains(contentType, "multipart/form-data")) {
		if err := c.Request.ParseMultipartForm(32 << 20); err != nil && err != http.ErrNotMultipart {
			if err := c.Request.ParseForm(); err != nil {
				return "", err
			}
		}
		defer func() {
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
		}()

		for key, vals := range c.Request.PostForm {
			for _, value := range vals {
				values.Add(key, value)
			}
		}
	}

	return values.Encode(), nil
}

func (m *QuotaMiddleware) canonicalizeJSONBody(c *gin.Context) (string, error) {
	body, err := m.readRequestBody(c)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", nil
	}

	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return "", nil
	}

	var payload interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil
	}

	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	return string(canonical), nil
}

func (m *QuotaMiddleware) canonicalizeHeaders(c *gin.Context) string {
	lines := make([]string, 0)
	for name, vals := range c.Request.Header {
		lowerName := strings.ToLower(name)
		sortedVals := append([]string(nil), vals...)
		sort.Strings(sortedVals)
		for _, value := range sortedVals {
			lines = append(lines, lowerName+":"+strings.TrimSpace(value))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func matchRegexDomain(canonical string, matcher *compiledRegexMatcher) bool {
	if matcher == nil {
		return true
	}

	if len(matcher.include) > 0 {
		matched := false
		for _, re := range matcher.include {
			if re.MatchString(canonical) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// exclude 具有最终否决权：即使已经命中 include，只要命中任一 exclude 仍然失败。
	for _, re := range matcher.exclude {
		if re.MatchString(canonical) {
			return false
		}
	}

	return true
}

func compileRuleMatchers(rules []config.QuotaRuleConfig) (map[string]compiledRuleMatcher, error) {
	matchers := make(map[string]compiledRuleMatcher, len(rules))
	for _, rule := range rules {
		matcher, err := compileRequestMatcher(&rule.RequestMatch)
		if err != nil {
			return nil, fmt.Errorf("compile matcher for rule %s: %w", rule.Name, err)
		}
		matchers[rule.Name] = matcher
	}
	return matchers, nil
}

func compileRequestMatcher(cfg *config.QuotaRuleRequestMatchConfig) (compiledRuleMatcher, error) {
	if cfg == nil {
		return compiledRuleMatcher{}, nil
	}

	queryForm, err := compileRegexMatcher(cfg.QueryForm)
	if err != nil {
		return compiledRuleMatcher{}, err
	}
	jsonBody, err := compileRegexMatcher(cfg.JSONBody)
	if err != nil {
		return compiledRuleMatcher{}, err
	}
	headers, err := compileRegexMatcher(cfg.Headers)
	if err != nil {
		return compiledRuleMatcher{}, err
	}

	return compiledRuleMatcher{
		queryForm: queryForm,
		jsonBody:  jsonBody,
		headers:   headers,
	}, nil
}

func compileRegexMatcher(cfg *config.RequestRegexMatchConfig) (*compiledRegexMatcher, error) {
	if cfg == nil {
		return nil, nil
	}

	matcher := &compiledRegexMatcher{}
	for _, pattern := range cfg.Include {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		matcher.include = append(matcher.include, re)
	}
	for _, pattern := range cfg.Exclude {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		matcher.exclude = append(matcher.exclude, re)
	}
	return matcher, nil
}

func (m *QuotaMiddleware) matchPath(path, pattern string) bool {
	if matched, _ := filepath.Match(pattern, path); matched {
		return true
	}
	if strings.HasSuffix(pattern, "**") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "**"))
	}
	return strings.HasPrefix(path, pattern)
}

// respondQuotaExceeded 返回配额超限响应
func (m *QuotaMiddleware) respondQuotaExceeded(c *gin.Context, rule *config.QuotaRuleConfig) {
	if rule.QuotaExceededBody != nil {
		body := *rule.QuotaExceededBody
		if body == "" {
			c.Status(http.StatusTooManyRequests)
			c.Writer.WriteHeaderNow()
			c.Abort()
			return
		}

		var jsonBody interface{}
		if json.Unmarshal([]byte(body), &jsonBody) == nil {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusTooManyRequests, jsonBody)
			c.Abort()
			return
		}

		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusTooManyRequests, body)
		c.Abort()
		return
	}

	c.JSON(http.StatusTooManyRequests, gin.H{
		"code":    42901,
		"message": "当前时间窗口内成功访问次数已达上限",
		"limit":   rule.SuccessLimit,
		"rule":    rule.Name,
	})
	c.Abort()
}

// respondError 返回错误响应
func (m *QuotaMiddleware) respondError(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, gin.H{
		"code":    statusCode,
		"message": message,
	})
	c.Abort()
}

// Close 关闭资源
func (m *QuotaMiddleware) Close() error {
	if m.manager != nil {
		return m.manager.Close()
	}
	return nil
}

// GetManager 获取配额管理器
func (m *QuotaMiddleware) GetManager() *quota.Manager {
	return m.manager
}
