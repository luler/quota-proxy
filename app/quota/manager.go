package quota

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gin_base/app/config"
	"gin_base/app/helper/log_helper"

	"github.com/go-redis/redis/v8"
)

type windowSpec struct {
	name      string
	count     int
	duration  time.Duration
	dayWindow bool
}

type ActiveQuotaRow struct {
	Identity     string `json:"identity"`
	IdentityType string `json:"identity_type"`
	RuleName     string `json:"rule_name"`
	Success      int    `json:"success_count"`
	Pending      int    `json:"pending_count"`
	Limit        int    `json:"limit"`
	Remaining    int    `json:"remaining"`
	Window       string `json:"window"`
	WindowCount  int    `json:"window_count"`
	PeriodKey    string `json:"period_key"`
}

func formatWindow(rule *config.QuotaRuleConfig) string {
	return strings.ToLower(rule.Window)
}

// QuotaStatus 配额状态
type QuotaStatus struct {
	RuleName    string `json:"rule_name"`
	Success     int    `json:"success_count"`
	Pending     int    `json:"pending_count"`
	Limit       int    `json:"limit"`
	Remaining   int    `json:"remaining"`
	Window      string `json:"window"`
	WindowCount int    `json:"window_count"`
	PeriodKey   string `json:"period_key"`
}

// Manager 配额管理器
type Manager struct {
	client   *redis.Client
	config   *config.QuotaConfig
	timezone *time.Location
}

// NewManager 创建配额管理器
func NewManager(cfg *config.Config) (*Manager, error) {
	loc, err := time.LoadLocation(cfg.Quota.Timezone)
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}

	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		if cfg.Quota.FailOpen {
			log_helper.Error("Redis connection failed, fail-open mode enabled", "error", err)
		} else {
			return nil, fmt.Errorf("redis connection failed: %w", err)
		}
	}

	return &Manager{
		client:   client,
		config:   &cfg.Quota,
		timezone: loc,
	}, nil
}

func (m *Manager) buildKey(ruleName, periodKey, identity string) string {
	// 直接字符串拼接比 fmt.Sprintf 省一次 reflect + interface 装箱
	return "quota:" + ruleName + ":" + periodKey + ":" + identity
}

func (m *Manager) now() time.Time {
	return time.Now().In(m.timezone)
}

func (m *Manager) normalizedWindow(window string) string {
	return strings.ToLower(window)
}

func (m *Manager) normalizedWindowCount(rule *config.QuotaRuleConfig) int {
	if rule.WindowCount < 1 {
		return 1
	}
	return rule.WindowCount
}

func (m *Manager) getWindowSpec(rule *config.QuotaRuleConfig) windowSpec {
	window := m.normalizedWindow(rule.Window)
	count := m.normalizedWindowCount(rule)

	switch window {
	case "minute":
		return windowSpec{name: window, count: count, duration: time.Duration(count) * time.Minute}
	case "hour":
		return windowSpec{name: window, count: count, duration: time.Duration(count) * time.Hour}
	default:
		return windowSpec{name: "day", count: count, dayWindow: true}
	}
}

func (m *Manager) getWindowStart(now time.Time, spec windowSpec) time.Time {
	if spec.dayWindow {
		reference := time.Date(1970, 1, 1, 0, 0, 0, 0, m.timezone)
		daysSinceReference := int(now.Sub(reference) / (24 * time.Hour))
		bucketDays := daysSinceReference / spec.count * spec.count
		return reference.AddDate(0, 0, bucketDays)
	}

	return now.Truncate(spec.duration)
}

func (m *Manager) getWindowEnd(start time.Time, spec windowSpec) time.Time {
	if spec.dayWindow {
		return start.AddDate(0, 0, spec.count)
	}

	return start.Add(spec.duration)
}

func (m *Manager) getPeriodTTL(rule *config.QuotaRuleConfig) int {
	now := m.now()
	spec := m.getWindowSpec(rule)
	start := m.getWindowStart(now, spec)
	end := m.getWindowEnd(start, spec)
	ttl := int(end.Sub(now).Seconds())
	if ttl < 1 {
		return 1
	}
	return ttl
}

func (m *Manager) getPeriodKey(rule *config.QuotaRuleConfig) string {
	now := m.now()
	spec := m.getWindowSpec(rule)
	start := m.getWindowStart(now, spec)

	switch spec.name {
	case "minute":
		return start.Format("2006-01-02-15-04")
	case "hour":
		return start.Format("2006-01-02-15")
	default:
		return start.Format("2006-01-02")
	}
}

// TryReserve 尝试预占名额
func (m *Manager) TryReserve(rule *config.QuotaRuleConfig, identity string) (bool, int, int, error) {
	key := m.buildKey(rule.Name, m.getPeriodKey(rule), identity)
	ttl := m.getPeriodTTL(rule)

	ctx := context.Background()
	result, err := m.client.Eval(ctx, TryReserveScript, []string{key}, rule.SuccessLimit, ttl).Result()
	if err != nil {
		return false, 0, 0, err
	}

	res, ok := result.([]interface{})
	if !ok || len(res) != 3 {
		return false, 0, 0, fmt.Errorf("invalid redis result")
	}

	successFlag := res[0].(int64) == 1
	successCount := int(res[1].(int64))
	pendingCount := int(res[2].(int64))

	return successFlag, successCount, pendingCount, nil
}

// Confirm 确认成功
func (m *Manager) Confirm(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(rule.Name, m.getPeriodKey(rule), identity)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, ConfirmScript, []string{key}).Result()
	return err
}

// Rollback 回滚 pending
func (m *Manager) Rollback(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(rule.Name, m.getPeriodKey(rule), identity)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, RollbackScript, []string{key}).Result()
	return err
}

// GetStatus 获取配额状态
func (m *Manager) GetStatus(ruleName string, identity string) (*QuotaStatus, error) {
	rule := m.GetRule(ruleName)
	if rule == nil {
		return nil, fmt.Errorf("quota rule not found: %s", ruleName)
	}

	return m.getStatusByRule(rule, identity)
}

// GetAllStatus 获取所有规则的配额状态
func (m *Manager) GetAllStatus(identity string) ([]*QuotaStatus, error) {
	statuses := make([]*QuotaStatus, 0, len(m.config.Rules))
	for i := range m.config.Rules {
		status, err := m.getStatusByRule(&m.config.Rules[i], identity)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (m *Manager) getStatusByRule(rule *config.QuotaRuleConfig, identity string) (*QuotaStatus, error) {
	key := m.buildKey(rule.Name, m.getPeriodKey(rule), identity)
	ctx := context.Background()
	result, err := m.client.Eval(ctx, GetQuotaScript, []string{key}).Result()
	if err != nil {
		return nil, err
	}

	res, ok := result.([]interface{})
	if !ok || len(res) != 2 {
		return nil, fmt.Errorf("invalid redis result")
	}

	successCount := int(res[0].(int64))
	pendingCount := int(res[1].(int64))
	remaining := rule.SuccessLimit - successCount
	if remaining < 0 {
		remaining = 0
	}

	return &QuotaStatus{
		RuleName:    rule.Name,
		Success:     successCount,
		Pending:     pendingCount,
		Limit:       rule.SuccessLimit,
		Remaining:   remaining,
		Window:      formatWindow(rule),
		WindowCount: m.normalizedWindowCount(rule),
		PeriodKey:   m.getPeriodKey(rule),
	}, nil
}

// Reset 重置配额
func (m *Manager) Reset(ruleName string, identity string) error {
	rule := m.GetRule(ruleName)
	if rule == nil {
		return fmt.Errorf("quota rule not found: %s", ruleName)
	}

	return m.resetByRule(rule, identity)
}

// ResetAll 重置所有规则配额
func (m *Manager) ResetAll(identity string) error {
	for i := range m.config.Rules {
		if err := m.resetByRule(&m.config.Rules[i], identity); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) resetByRule(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(rule.Name, m.getPeriodKey(rule), identity)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, ResetScript, []string{key}).Result()
	return err
}

func (m *Manager) ListActiveStatuses(identityFilter, ruleFilter string, page, pageSize int) ([]ActiveQuotaRow, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	rules := m.config.Rules
	if ruleFilter != "" {
		rule := m.GetRule(ruleFilter)
		if rule == nil {
			return nil, 0, fmt.Errorf("quota rule not found: %s", ruleFilter)
		}
		rules = []config.QuotaRuleConfig{*rule}
	}

	seen := make(map[string]struct{})
	rows := make([]ActiveQuotaRow, 0)
	identityFilter = strings.ToLower(identityFilter)

	for i := range rules {
		rule := &rules[i]
		periodKey := m.getPeriodKey(rule)
		identities, err := m.scanRuleIdentities(rule.Name, periodKey)
		if err != nil {
			return nil, 0, err
		}

		for _, identity := range identities {
			if identityFilter != "" && !strings.Contains(strings.ToLower(identity), identityFilter) {
				continue
			}
			key := rule.Name + "\x00" + identity
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			status, err := m.getStatusByRule(rule, identity)
			if err != nil {
				return nil, 0, err
			}
			rows = append(rows, ActiveQuotaRow{
				Identity:     identity,
				IdentityType: parseIdentityType(identity),
				RuleName:     status.RuleName,
				Success:      status.Success,
				Pending:      status.Pending,
				Limit:        status.Limit,
				Remaining:    status.Remaining,
				Window:       status.Window,
				WindowCount:  status.WindowCount,
				PeriodKey:    status.PeriodKey,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Remaining != rows[j].Remaining {
			return rows[i].Remaining < rows[j].Remaining
		}
		if rows[i].Identity != rows[j].Identity {
			return rows[i].Identity < rows[j].Identity
		}
		return rows[i].RuleName < rows[j].RuleName
	})

	total := len(rows)
	start := (page - 1) * pageSize
	if start >= total {
		return []ActiveQuotaRow{}, total, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return rows[start:end], total, nil
}

func (m *Manager) scanRuleIdentities(ruleName, periodKey string) ([]string, error) {
	ctx := context.Background()
	pattern := m.buildKey(ruleName, periodKey, "*")
	cursor := uint64(0)
	identities := make([]string, 0)
	seen := make(map[string]struct{})
	prefix := "quota:" + ruleName + ":" + periodKey + ":"

	for {
		keys, next, err := m.client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			identity, ok := strings.CutPrefix(key, prefix)
			if !ok || identity == "" {
				continue
			}
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			identities = append(identities, identity)
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	sort.Strings(identities)
	return identities, nil
}

func parseIdentityType(identity string) string {
	parts := strings.SplitN(identity, ":", 2)
	if len(parts) == 2 && parts[0] != "" {
		return parts[0]
	}
	return "unknown"
}

// GetRule 获取规则
func (m *Manager) GetRule(ruleName string) *config.QuotaRuleConfig {
	for i := range m.config.Rules {
		if m.config.Rules[i].Name == ruleName {
			return &m.config.Rules[i]
		}
	}
	return nil
}

// ListRuleNames 获取所有规则名
func (m *Manager) ListRuleNames() []string {
	names := make([]string, 0, len(m.config.Rules))
	for _, rule := range m.config.Rules {
		names = append(names, rule.Name)
	}
	return names
}

// IsRedisError 判断是否为 Redis 错误
func (m *Manager) IsRedisError(err error) bool {
	return err != nil && err != redis.Nil
}

// IsFailOpen 是否为 fail-open 模式
func (m *Manager) IsFailOpen() bool {
	return m.config.FailOpen
}

// Close 关闭 Redis 连接
func (m *Manager) Close() error {
	return m.client.Close()
}

// GetLimit 获取规则限制
func (m *Manager) GetLimit(rule *config.QuotaRuleConfig) int {
	return rule.SuccessLimit
}

// IsEnabled 是否启用配额
func (m *Manager) IsEnabled() bool {
	return m.config.Enabled
}
