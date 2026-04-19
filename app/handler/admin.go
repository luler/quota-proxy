package handler

import (
	"embed"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gin_base/app/config"
	"gin_base/app/middleware"
	"gin_base/app/quota"

	"github.com/gin-gonic/gin"
)

//go:embed adminui/index.html
var adminUIFS embed.FS

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	sec := d.Seconds()
	if sec == float64(int(sec)) {
		return fmt.Sprintf("%ds", int(sec))
	}
	return d.String()
}

// AdminHandler 管理接口处理器
type AdminHandler struct {
	store *middleware.RuntimeStore
}

// NewAdminHandler 创建管理接口处理器
func NewAdminHandler(store *middleware.RuntimeStore) *AdminHandler {
	return &AdminHandler{store: store}
}

// AdminUI 管理页面
func (h *AdminHandler) AdminUI(c *gin.Context) {
	content, err := adminUIFS.ReadFile("adminui/index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load admin ui")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", content)
}

// Login 校验管理密钥
func (h *AdminHandler) Login(c *gin.Context) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "api_key 参数必填"})
		return
	}

	apiKey, ok := h.currentAdminAPIKey(c)
	if !ok {
		return
	}
	if apiKey == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "管理鉴权未启用，请先在配置文件中设置 admin.api_key"})
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "api_key 参数必填"})
		return
	}
	if strings.TrimSpace(req.APIKey) != apiKey {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "管理密钥无效"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "验证通过"})
}

// GetSummary 返回系统概览
func (h *AdminHandler) GetSummary(c *gin.Context) {
	runtime := h.currentRuntime(c)
	if runtime == nil {
		return
	}
	cfg := runtime.Config
	c.JSON(http.StatusOK, gin.H{
		"server": gin.H{
			"port":         cfg.Server.Port,
			"read_timeout": formatDuration(cfg.Server.ReadTimeout),
			"idle_timeout": formatDuration(cfg.Server.IdleTimeout),
		},
		"upstream": gin.H{
			"target":           cfg.Upstream.Target,
			"response_timeout": formatDuration(cfg.Upstream.ResponseTimeout),
		},
		"redis": gin.H{
			"addr":             cfg.Redis.Addr,
			"db":               cfg.Redis.DB,
			"password_present": cfg.Redis.Password != "",
		},
		"identity": gin.H{
			"strategy":       cfg.Identity.Strategy,
			"fallback_to_ip": cfg.Identity.FallbackToIP,
			"extractors":     cfg.Identity.Extractors,
		},
		"quota": gin.H{
			"enabled":       cfg.Quota.Enabled,
			"timezone":      cfg.Quota.Timezone,
			"exclude_paths": cfg.Quota.ExcludePaths,
			"fail_open":     cfg.Quota.FailOpen,
			"rules":         cfg.Quota.Rules,
		},
		"success_rule": cfg.SuccessRule,
		"logging":      cfg.Logging,
		"config_path":  config.ConfigPath(),
		"rule_names":   runtime.QuotaMiddleware.GetManager().ListRuleNames(),
	})
}

// GetQuota 查看配额使用情况
func (h *AdminHandler) GetQuota(c *gin.Context) {
	manager := h.currentManager(c)
	if manager == nil {
		return
	}

	identity := c.Query("identity")
	if identity == "" {
		c.JSON(400, gin.H{
			"code":    400,
			"message": "identity 参数必填",
		})
		return
	}

	ruleName := c.DefaultQuery("rule", "")
	if ruleName == "" {
		statuses, err := manager.GetAllStatus(identity)
		if err != nil {
			h.respondManagerError(c, "查询配额失败", err)
			return
		}

		c.JSON(200, gin.H{
			"identity": identity,
			"quotas":   statuses,
			"rules":    manager.ListRuleNames(),
		})
		return
	}

	status, err := manager.GetStatus(ruleName, identity)
	if err != nil {
		h.respondManagerError(c, "查询配额失败", err)
		return
	}

	c.JSON(200, gin.H{
		"identity":      identity,
		"rule":          status.RuleName,
		"window":        status.Window,
		"period_key":    status.PeriodKey,
		"success_count": status.Success,
		"pending_count": status.Pending,
		"limit":         status.Limit,
		"remaining":     status.Remaining,
		"rules":         manager.ListRuleNames(),
	})
}

// ListQuotas 查看当前活跃额度
func (h *AdminHandler) ListQuotas(c *gin.Context) {
	manager := h.currentManager(c)
	if manager == nil {
		return
	}

	page := parsePositiveInt(c.DefaultQuery("page", "1"), 1)
	pageSize := parsePositiveInt(c.DefaultQuery("page_size", "20"), 20)
	identity := c.Query("identity")
	rule := c.Query("rule")

	items, total, err := manager.ListActiveStatuses(identity, rule, page, pageSize)
	if err != nil {
		h.respondManagerError(c, "查询活跃配额失败", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"filters": gin.H{
			"identity": identity,
			"rule":     rule,
		},
	})
}

// GetConfig 返回可编辑配置
func (h *AdminHandler) GetConfig(c *gin.Context) {
	runtime := h.currentRuntime(c)
	if runtime == nil {
		return
	}

	payload := editableConfigFrom(runtime.Config)
	c.JSON(http.StatusOK, gin.H{
		"config":      payload,
		"config_path": config.ConfigPath(),
	})
}

// ValidateConfig 校验配置
func (h *AdminHandler) ValidateConfig(c *gin.Context) {
	cfg, err := h.bindEditableConfig(c)
	if err != nil {
		h.respondBadRequest(c, err)
		return
	}
	if err := config.Validate(cfg); err != nil {
		h.respondBadRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "配置校验通过"})
}

// SaveConfig 保存配置文件
func (h *AdminHandler) SaveConfig(c *gin.Context) {
	cfg, err := h.bindEditableConfig(c)
	if err != nil {
		h.respondBadRequest(c, err)
		return
	}
	if err := config.Save(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "保存配置失败", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "配置已保存", "config_path": config.ConfigPath()})
}

// ReloadConfig 重新加载配置
func (h *AdminHandler) ReloadConfig(c *gin.Context) {
	cfg, err := config.Load()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "重新加载配置失败", "error": err.Error()})
		return
	}
	if err := h.store.Reload(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "更新运行时失败", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "配置已重新加载"})
}

// ResetQuota 重置配额
func (h *AdminHandler) ResetQuota(c *gin.Context) {
	manager := h.currentManager(c)
	if manager == nil {
		return
	}

	var req struct {
		Identity string `json:"identity" binding:"required"`
		Rule     string `json:"rule"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{
			"code":    400,
			"message": "identity 参数必填",
		})
		return
	}

	if req.Rule == "" {
		if err := manager.ResetAll(req.Identity); err != nil {
			h.respondManagerError(c, "重置配额失败", err)
			return
		}

		c.JSON(200, gin.H{
			"code":     200,
			"message":  "所有配额已重置",
			"identity": req.Identity,
			"rules":    manager.ListRuleNames(),
		})
		return
	}

	if err := manager.Reset(req.Rule, req.Identity); err != nil {
		h.respondManagerError(c, "重置配额失败", err)
		return
	}

	c.JSON(200, gin.H{
		"code":     200,
		"message":  "配额已重置",
		"identity": req.Identity,
		"rule":     req.Rule,
	})
}

type editableConfig struct {
	Server      editableServerConfig     `json:"server"`
	Upstream    editableUpstreamConfig   `json:"upstream"`
	Redis       editableRedisConfig      `json:"redis"`
	Identity    config.IdentityConfig    `json:"identity"`
	Quota       config.QuotaConfig       `json:"quota"`
	SuccessRule config.SuccessRuleConfig `json:"success_rule"`
	Logging     config.LoggingConfig     `json:"logging"`
	Admin       config.AdminConfig       `json:"admin"`
}

type editableServerConfig struct {
	Port        int    `json:"port"`
	ReadTimeout string `json:"read_timeout"`
	IdleTimeout string `json:"idle_timeout"`
}

type editableUpstreamConfig struct {
	Target          string `json:"target"`
	ResponseTimeout string `json:"response_timeout"`
}

type editableRedisConfig struct {
	Addr string `json:"addr"`
	DB   int    `json:"db"`
}

func editableConfigFrom(cfg *config.Config) editableConfig {
	return editableConfig{
		Server: editableServerConfig{
			Port:        cfg.Server.Port,
			ReadTimeout: formatDuration(cfg.Server.ReadTimeout),
			IdleTimeout: formatDuration(cfg.Server.IdleTimeout),
		},
		Upstream: editableUpstreamConfig{
			Target:          cfg.Upstream.Target,
			ResponseTimeout: formatDuration(cfg.Upstream.ResponseTimeout),
		},
		Redis: editableRedisConfig{
			Addr: cfg.Redis.Addr,
			DB:   cfg.Redis.DB,
		},
		Identity:    cfg.Identity,
		Quota:       cfg.Quota,
		SuccessRule: cfg.SuccessRule,
		Logging:     cfg.Logging,
		Admin:       cfg.Admin,
	}
}

func (h *AdminHandler) bindEditableConfig(c *gin.Context) (*config.Config, error) {
	var req editableConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		return nil, err
	}
	current := h.store.Current()
	if current == nil || current.Config == nil {
		return nil, errors.New("运行时未初始化")
	}

	readTimeout, err := time.ParseDuration(strings.TrimSpace(req.Server.ReadTimeout))
	if err != nil {
		return nil, errors.New("server.read_timeout 格式不正确，应为如 10s、500ms、1m")
	}
	idleTimeout, err := time.ParseDuration(strings.TrimSpace(req.Server.IdleTimeout))
	if err != nil {
		return nil, errors.New("server.idle_timeout 格式不正确，应为如 120s、500ms、1m")
	}

	responseTimeout, err := time.ParseDuration(strings.TrimSpace(req.Upstream.ResponseTimeout))
	if err != nil {
		return nil, errors.New("upstream.response_timeout 格式不正确，应为如 120s、500ms、1m")
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:        req.Server.Port,
			ReadTimeout: readTimeout,
			IdleTimeout: idleTimeout,
		},
		Upstream: config.UpstreamConfig{
			Target:          req.Upstream.Target,
			ResponseTimeout: responseTimeout,
		},
		Redis: config.RedisConfig{
			Addr:     req.Redis.Addr,
			DB:       req.Redis.DB,
			Password: current.Config.Redis.Password,
		},
		Identity:    req.Identity,
		Quota:       req.Quota,
		SuccessRule: req.SuccessRule,
		Logging:     req.Logging,
		Admin:       req.Admin,
	}
	return cfg, nil
}

func (h *AdminHandler) currentRuntime(c *gin.Context) *middleware.Runtime {
	runtime := h.store.Current()
	if runtime == nil || runtime.Config == nil || runtime.QuotaMiddleware == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "运行时未初始化"})
		return nil
	}
	return runtime
}

func (h *AdminHandler) currentManager(c *gin.Context) *quota.Manager {
	runtime := h.currentRuntime(c)
	if runtime == nil {
		return nil
	}
	return runtime.QuotaMiddleware.GetManager()
}

func (h *AdminHandler) currentAdminAPIKey(c *gin.Context) (string, bool) {
	runtime := h.store.Current()
	if runtime == nil || runtime.Config == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "运行时未初始化"})
		return "", false
	}
	return strings.TrimSpace(runtime.Config.Admin.APIKey), true
}

func (h *AdminHandler) respondManagerError(c *gin.Context, message string, err error) {
	status := http.StatusInternalServerError
	if strings.Contains(err.Error(), "quota rule not found") {
		status = http.StatusBadRequest
	}
	c.JSON(status, gin.H{"code": status, "message": message, "error": err.Error()})
}

func (h *AdminHandler) respondBadRequest(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "配置校验失败", "error": err.Error()})
}

func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
