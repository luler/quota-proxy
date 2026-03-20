package middleware

import (
	"bytes"
	"encoding/json"
	"gin_base/app/config"
	"gin_base/app/helper/log_helper"
	"gin_base/app/identity"
	"gin_base/app/proxy"
	"gin_base/app/quota"
	"gin_base/app/success"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// QuotaMiddleware 配额中间件
type QuotaMiddleware struct {
	identifier *identity.Identifier
	judge      success.Judge
	manager    *quota.Manager
	proxy      *proxy.ReverseProxy
	config     *config.Config
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

	return &QuotaMiddleware{
		identifier: identifier,
		judge:      judge,
		manager:    manager,
		proxy:      p,
		config:     cfg,
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
	matcher := rule.RequestMatch
	if matcher.QueryFormContains == "" && matcher.JSONBodyContains == "" && matcher.HeaderContains == "" {
		return true
	}

	if matcher.QueryFormContains != "" && !m.matchQueryAndFormContains(c, matcher.QueryFormContains) {
		return false
	}

	if matcher.JSONBodyContains != "" && !m.matchJSONBodyContains(c, matcher.JSONBodyContains) {
		return false
	}

	if matcher.HeaderContains != "" && !m.matchHeaderContains(c, matcher.HeaderContains) {
		return false
	}

	return true
}

func (m *QuotaMiddleware) matchQueryAndFormContains(c *gin.Context, target string) bool {
	for _, values := range c.Request.URL.Query() {
		if containsAny(values, target) {
			return true
		}
	}

	body, err := m.readRequestBody(c)
	if err != nil {
		return false
	}

	if len(body) == 0 {
		return false
	}

	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if !(strings.Contains(contentType, "application/x-www-form-urlencoded") || strings.Contains(contentType, "multipart/form-data")) {
		return false
	}

	if err := c.Request.ParseForm(); err != nil {
		return false
	}
	defer func() {
		c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	}()

	for key, values := range c.Request.PostForm {
		if _, exists := c.Request.URL.Query()[key]; exists {
			continue
		}
		if containsAny(values, target) {
			return true
		}
	}

	return false
}

func (m *QuotaMiddleware) matchJSONBodyContains(c *gin.Context, target string) bool {
	body, err := m.readRequestBody(c)
	if err != nil || len(body) == 0 {
		return false
	}

	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return false
	}

	return strings.Contains(string(body), target)
}

func (m *QuotaMiddleware) matchHeaderContains(c *gin.Context, target string) bool {
	for _, values := range c.Request.Header {
		if containsAny(values, target) {
			return true
		}
	}
	return false
}

func (m *QuotaMiddleware) readRequestBody(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}

	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

func containsAny(values []string, target string) bool {
	for _, value := range values {
		if strings.Contains(value, target) {
			return true
		}
	}
	return false
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
