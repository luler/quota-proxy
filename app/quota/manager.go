package quota

import (
	"context"
	"errors"
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
	Rejected429  int    `json:"rejected_429_count"`
	Limit        int    `json:"limit"`
	Remaining    int    `json:"remaining"`
	Window       string `json:"window"`
	WindowCount  int    `json:"window_count"`
	PeriodKey    string `json:"period_key"`
}

var (
	ErrRuleNotFound       = errors.New("quota rule not found")
	ErrInvalidRedisResult = errors.New("invalid redis result")
)

const quotaStatusPipelineBatchSize = 200

func formatWindow(rule *config.QuotaRuleConfig) string {
	return strings.ToLower(rule.Window)
}

// QuotaStatus 配额状态
type QuotaStatus struct {
	RuleName    string `json:"rule_name"`
	Success     int    `json:"success_count"`
	Pending     int    `json:"pending_count"`
	Rejected429 int    `json:"rejected_429_count"`
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
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		MaxRetries:   2,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
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

func (m *Manager) buildKey(periodKey, identity string) string {
	return "quota:" + periodKey + ":" + identity
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
func (m *Manager) TryReserve(rule *config.QuotaRuleConfig, identity string) (bool, int, int, int, error) {
	periodKey := m.getPeriodKey(rule)
	key := m.buildKey(periodKey, identity)
	ttl := m.getPeriodTTL(rule)

	ctx := context.Background()
	result, err := m.client.Eval(ctx, TryReserveScript, []string{key},
		rule.SuccessLimit, ttl, rule.Name, formatWindow(rule), m.normalizedWindowCount(rule), periodKey,
	).Result()
	if err != nil {
		return false, 0, 0, 0, err
	}

	res, ok := result.([]interface{})
	if !ok || len(res) != 4 {
		return false, 0, 0, 0, ErrInvalidRedisResult
	}

	successFlag := res[0].(int64) == 1
	successCount := int(res[1].(int64))
	pendingCount := int(res[2].(int64))
	rejected429Count := int(res[3].(int64))

	return successFlag, successCount, pendingCount, rejected429Count, nil
}

// Confirm 确认成功
func (m *Manager) Confirm(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(m.getPeriodKey(rule), identity)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, ConfirmScript, []string{key}).Result()
	return err
}

// Rollback 回滚 pending
func (m *Manager) Rollback(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(m.getPeriodKey(rule), identity)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, RollbackScript, []string{key}).Result()
	return err
}

// GetStatus 获取配额状态
func (m *Manager) GetStatus(ruleName string, identity string) (*QuotaStatus, error) {
	rule := m.GetRule(ruleName)
	if rule != nil {
		return m.getStatusByRule(rule, identity)
	}

	status, err := m.getActiveStatusByIdentity(identity)
	if err != nil {
		return nil, err
	}
	if status != nil && (ruleName == "" || status.RuleName == ruleName) {
		return status, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrRuleNotFound, ruleName)
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
	key := m.buildKey(m.getPeriodKey(rule), identity)
	ctx := context.Background()
	result, err := m.client.Eval(ctx, GetQuotaScript, []string{key}).Result()
	if err != nil {
		return nil, err
	}

	return m.quotaStatusFromRedisResult(rule, result)
}

func (m *Manager) quotaStatusFromRedisResult(rule *config.QuotaRuleConfig, result interface{}) (*QuotaStatus, error) {
	res, ok := result.([]interface{})
	if !ok || len(res) < 3 {
		return nil, ErrInvalidRedisResult
	}

	successCount, ok := redisInt(res[0])
	if !ok {
		return nil, ErrInvalidRedisResult
	}
	pendingCount, ok := redisInt(res[1])
	if !ok {
		return nil, ErrInvalidRedisResult
	}
	rejected429Count, ok := redisInt(res[2])
	if !ok {
		return nil, ErrInvalidRedisResult
	}

	ruleName := ""
	limit := 0
	window := ""
	windowCount := 1
	periodKey := ""

	if len(res) >= 8 {
		if s, ok := res[3].(string); ok {
			ruleName = s
		}
		if n, ok := redisInt(res[4]); ok {
			limit = n
		}
		if s, ok := res[5].(string); ok {
			window = s
		}
		if n, ok := redisInt(res[6]); ok {
			windowCount = n
		}
		if s, ok := res[7].(string); ok {
			periodKey = s
		}
	}

	if rule != nil {
		ruleName = rule.Name
		limit = rule.SuccessLimit
		window = formatWindow(rule)
		windowCount = m.normalizedWindowCount(rule)
		periodKey = m.getPeriodKey(rule)
	}

	remaining := limit - successCount
	if remaining < 0 {
		remaining = 0
	}

	return &QuotaStatus{
		RuleName:    ruleName,
		Success:     successCount,
		Pending:     pendingCount,
		Rejected429: rejected429Count,
		Limit:       limit,
		Remaining:   remaining,
		Window:      window,
		WindowCount: windowCount,
		PeriodKey:   periodKey,
	}, nil
}

func redisInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

// Reset 重置配额
func (m *Manager) Reset(ruleName string, identity string) error {
	rule := m.GetRule(ruleName)
	if rule != nil {
		return m.resetByRule(rule, identity)
	}
	return m.ResetAll(identity)
}

// Reject 耗尽剩余额度
func (m *Manager) Reject(ruleName string, identity string) error {
	rule := m.GetRule(ruleName)
	if rule != nil {
		return m.rejectByRule(rule, identity)
	}
	return m.rejectByIdentity(identity)
}

// RejectAll 耗尽所有规则的剩余额度
func (m *Manager) RejectAll(identity string) error {
	for i := range m.config.Rules {
		if err := m.rejectByRule(&m.config.Rules[i], identity); err != nil {
			return err
		}
	}
	return nil
}

// ResetAll 重置所有规则配额
func (m *Manager) ResetAll(identity string) error {
	ctx := context.Background()
	keys, err := m.scanKeysByIdentity(identity)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return m.client.Del(ctx, keys...).Err()
	}
	return nil
}

func (m *Manager) rejectByRule(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(m.getPeriodKey(rule), identity)
	ttl := m.getPeriodTTL(rule)
	ctx := context.Background()
	_, err := m.client.Eval(ctx, RejectScript, []string{key}, rule.SuccessLimit, ttl).Result()
	return err
}

func (m *Manager) rejectByIdentity(identity string) error {
	keys, err := m.scanKeysByIdentity(identity)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, key := range keys {
		limitVal, err := m.client.HGet(ctx, key, "limit").Int()
		if err != nil || limitVal <= 0 {
			limitVal = 1
		}
		ttl, err := m.client.TTL(ctx, key).Result()
		ttlSec := int(ttl.Seconds())
		if ttlSec <= 0 {
			ttlSec = 86400
		}
		if _, err := m.client.Eval(ctx, RejectScript, []string{key}, limitVal, ttlSec).Result(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) resetByRule(rule *config.QuotaRuleConfig, identity string) error {
	key := m.buildKey(m.getPeriodKey(rule), identity)
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

	identityFilter = strings.ToLower(identityFilter)

	keys, err := m.scanAllQuotaKeys()
	if err != nil {
		return nil, 0, err
	}

	type keyMeta struct {
		key       string
		periodKey string
		identity  string
	}
	filteredKeys := make([]keyMeta, 0, len(keys))
	for _, k := range keys {
		rem := strings.TrimPrefix(k, "quota:")
		pKey, ident, ok := strings.Cut(rem, ":")
		if !ok || pKey == "" || ident == "" {
			continue
		}
		if identityFilter != "" && !strings.Contains(strings.ToLower(ident), identityFilter) {
			continue
		}
		filteredKeys = append(filteredKeys, keyMeta{key: k, periodKey: pKey, identity: ident})
	}

	if len(filteredKeys) == 0 {
		return []ActiveQuotaRow{}, 0, nil
	}

	ctx := context.Background()
	statuses := make([]*QuotaStatus, 0, len(filteredKeys))
	for start := 0; start < len(filteredKeys); start += quotaStatusPipelineBatchSize {
		end := start + quotaStatusPipelineBatchSize
		if end > len(filteredKeys) {
			end = len(filteredKeys)
		}
		batch := filteredKeys[start:end]
		pipe := m.client.Pipeline()
		cmds := make([]*redis.Cmd, len(batch))
		for i, km := range batch {
			cmds[i] = pipe.Eval(ctx, GetQuotaScript, []string{km.key})
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, 0, err
		}
		for i, cmd := range cmds {
			km := batch[i]
			status, err := m.quotaStatusFromRedisResult(nil, cmd.Val())
			if err != nil {
				return nil, 0, err
			}
			if cfgRule := m.GetRule(status.RuleName); cfgRule != nil {
				status.Limit = cfgRule.SuccessLimit
				status.Window = formatWindow(cfgRule)
				status.WindowCount = m.normalizedWindowCount(cfgRule)
				status.Remaining = status.Limit - status.Success
				if status.Remaining < 0 {
					status.Remaining = 0
				}
			}
			if status.PeriodKey == "" {
				status.PeriodKey = km.periodKey
			}
			statuses = append(statuses, status)
		}
	}

	rows := make([]ActiveQuotaRow, 0, len(filteredKeys))
	for i, km := range filteredKeys {
		status := statuses[i]
		if ruleFilter != "" && status.RuleName != ruleFilter {
			continue
		}
		rows = append(rows, ActiveQuotaRow{
			Identity:     km.identity,
			IdentityType: parseIdentityType(km.identity),
			RuleName:     status.RuleName,
			Success:      status.Success,
			Pending:      status.Pending,
			Rejected429:  status.Rejected429,
			Limit:        status.Limit,
			Remaining:    status.Remaining,
			Window:       status.Window,
			WindowCount:  status.WindowCount,
			PeriodKey:    status.PeriodKey,
		})
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

func (m *Manager) scanAllQuotaKeys() ([]string, error) {
	ctx := context.Background()
	cursor := uint64(0)
	keys := make([]string, 0)
	seen := make(map[string]struct{})

	for {
		batch, next, err := m.client.Scan(ctx, cursor, "quota:*", 200).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range batch {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *Manager) scanKeysByIdentity(identity string) ([]string, error) {
	keys, err := m.scanAllQuotaKeys()
	if err != nil {
		return nil, err
	}
	matched := make([]string, 0)
	for _, k := range keys {
		rem := strings.TrimPrefix(k, "quota:")
		_, ident, ok := strings.Cut(rem, ":")
		if ok && ident == identity {
			matched = append(matched, k)
		}
	}
	return matched, nil
}

func (m *Manager) getActiveStatusByIdentity(identity string) (*QuotaStatus, error) {
	keys, err := m.scanKeysByIdentity(identity)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	ctx := context.Background()
	result, err := m.client.Eval(ctx, GetQuotaScript, []string{keys[0]}).Result()
	if err != nil {
		return nil, err
	}
	return m.quotaStatusFromRedisResult(nil, result)
}

func parseIdentityType(identity string) string {
	segments := strings.Split(identity, "|")
	types := make([]string, 0, len(segments))
	for _, segment := range segments {
		name, _, ok := strings.Cut(segment, ":")
		if ok && name != "" {
			types = append(types, name)
		}
	}
	if len(types) > 0 {
		return strings.Join(types, "|")
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
	return err != nil && !errors.Is(err, redis.Nil)
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
