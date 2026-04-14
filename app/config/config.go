package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 应用配置结构
type Config struct {
	Server      ServerConfig      `mapstructure:"server" yaml:"server" json:"server"`
	Upstream    UpstreamConfig    `mapstructure:"upstream" yaml:"upstream" json:"upstream"`
	Redis       RedisConfig       `mapstructure:"redis" yaml:"redis" json:"redis"`
	Identity    IdentityConfig    `mapstructure:"identity" yaml:"identity" json:"identity"`
	Quota       QuotaConfig       `mapstructure:"quota" yaml:"quota" json:"quota"`
	SuccessRule SuccessRuleConfig `mapstructure:"success_rule" yaml:"success_rule" json:"success_rule"`
	Logging     LoggingConfig     `mapstructure:"logging" yaml:"logging" json:"logging"`
	Admin       AdminConfig       `mapstructure:"admin" yaml:"admin" json:"admin"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port         int           `mapstructure:"port" yaml:"port" json:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout" yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" yaml:"write_timeout" json:"write_timeout"`
}

// UpstreamConfig 上游服务配置
type UpstreamConfig struct {
	Target string `mapstructure:"target" yaml:"target" json:"target"`
}

// RedisConfig Redis 配置
type RedisConfig struct {
	Addr     string `mapstructure:"addr" yaml:"addr" json:"addr"`
	Password string `mapstructure:"password" yaml:"password" json:"password,omitempty"`
	DB       int    `mapstructure:"db" yaml:"db" json:"db"`
}

// IdentityExtractorConfig 请求头提取身份配置
type IdentityExtractorConfig struct {
	Header string `mapstructure:"header" yaml:"header" json:"header"`
	Regex  string `mapstructure:"regex" yaml:"regex" json:"regex"`
	Group  int    `mapstructure:"group" yaml:"group" json:"group"`
	Name   string `mapstructure:"name" yaml:"name" json:"name"`
}

// IdentityConfig 身份识别配置
type IdentityConfig struct {
	Strategy     string                    `mapstructure:"strategy" yaml:"strategy" json:"strategy"`
	Extractors   []IdentityExtractorConfig `mapstructure:"extractors" yaml:"extractors" json:"extractors"`
	FallbackToIP bool                      `mapstructure:"fallback_to_ip" yaml:"fallback_to_ip" json:"fallback_to_ip"`
}

// QuotaConfig 配额配置
type QuotaConfig struct {
	Enabled      bool              `mapstructure:"enabled" yaml:"enabled" json:"enabled"`
	Timezone     string            `mapstructure:"timezone" yaml:"timezone" json:"timezone"`
	ExcludePaths []string          `mapstructure:"exclude_paths" yaml:"exclude_paths" json:"exclude_paths"`
	FailOpen     bool              `mapstructure:"fail_open" yaml:"fail_open" json:"fail_open"`
	Rules        []QuotaRuleConfig `mapstructure:"rules" yaml:"rules" json:"rules"`
}

// RequestRegexMatchConfig 正则匹配配置
type RequestRegexMatchConfig struct {
	Include []string `mapstructure:"include" yaml:"include" json:"include"`
	Exclude []string `mapstructure:"exclude" yaml:"exclude" json:"exclude"`
}

// QuotaRuleRequestMatchConfig 请求内容匹配配置
type QuotaRuleRequestMatchConfig struct {
	QueryForm *RequestRegexMatchConfig `mapstructure:"query_form" yaml:"query_form" json:"query_form"`
	JSONBody  *RequestRegexMatchConfig `mapstructure:"json_body" yaml:"json_body" json:"json_body"`
	Headers   *RequestRegexMatchConfig `mapstructure:"headers" yaml:"headers" json:"headers"`
}

// QuotaRuleConfig 路径配额规则
type QuotaRuleConfig struct {
	Name              string                      `mapstructure:"name" yaml:"name" json:"name"`
	Window            string                      `mapstructure:"window" yaml:"window" json:"window"`
	WindowCount       int                         `mapstructure:"window_count" yaml:"window_count" json:"window_count"`
	SuccessLimit      int                         `mapstructure:"success_limit" yaml:"success_limit" json:"success_limit"`
	IncludePaths      []string                    `mapstructure:"include_paths" yaml:"include_paths" json:"include_paths"`
	RequestMatch      QuotaRuleRequestMatchConfig `mapstructure:"request_match" yaml:"request_match" json:"request_match"`
	QuotaExceededBody *string                     `mapstructure:"quota_exceeded_body" yaml:"quota_exceeded_body" json:"quota_exceeded_body"`
}

// SuccessRuleConfig 成功判定规则配置
type SuccessRuleConfig struct {
	Mode           string `mapstructure:"mode" yaml:"mode" json:"mode"`
	RequireHTTP2xx bool   `mapstructure:"require_http_2xx" yaml:"require_http_2xx" json:"require_http_2xx"`
	JSONField      string `mapstructure:"json_field" yaml:"json_field" json:"json_field"`
	ExpectedValue  int    `mapstructure:"expected_value" yaml:"expected_value" json:"expected_value"`
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Level     string `mapstructure:"level" yaml:"level" json:"level"`
	AccessLog bool   `mapstructure:"access_log" yaml:"access_log" json:"access_log"`
}

// AdminConfig 管理面板配置
type AdminConfig struct {
	APIKey string `mapstructure:"api_key" yaml:"api_key" json:"api_key,omitempty"`
}

var appConfig *Config

func Load() (*Config, error) {
	return loadFromPath(configPath())
}

func loadFromPath(filePath string) (*Config, error) {
	v := viper.New()

	setDefaults(v)
	v.SetConfigFile(filePath)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("QUOTA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	appConfig = &cfg
	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	if err := validateIdentityConfig(cfg); err != nil {
		return err
	}
	if err := validateQuotaRules(cfg); err != nil {
		return err
	}
	return nil
}

func configPath() string {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		return "./config.yaml"
	}
	return path
}

func ConfigPath() string {
	return configPath()
}

func Validate(cfg *Config) error {
	return validateConfig(cfg)
}

func MarshalYAML(cfg *Config) ([]byte, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(data), nil
}

func LoadFromYAML(data []byte) (*Config, error) {
	v := viper.New()
	setDefaults(v)
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func Save(cfg *Config) error {
	data, err := MarshalYAML(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), append(data, '\n'), 0644)
}

func validateIdentityConfig(cfg *Config) error {
	for i, extractor := range cfg.Identity.Extractors {
		if extractor.Header == "" {
			return fmt.Errorf("invalid identity.extractors[%d].header: cannot be empty", i)
		}
		if extractor.Name == "" {
			return fmt.Errorf("invalid identity.extractors[%d].name: cannot be empty", i)
		}
		if extractor.Group < 0 {
			return fmt.Errorf("invalid identity.extractors[%d].group: %d, must be >= 0", i, extractor.Group)
		}
		if extractor.Regex == "" {
			if extractor.Group != 0 {
				return fmt.Errorf("invalid identity.extractors[%d].group: %d, direct extractor cannot set group", i, extractor.Group)
			}
			continue
		}

		re, err := regexp.Compile(extractor.Regex)
		if err != nil {
			return fmt.Errorf("invalid identity.extractors[%d].regex: %w", i, err)
		}
		if extractor.Group > re.NumSubexp() {
			return fmt.Errorf("invalid identity.extractors[%d].group: %d, regex has %d capture groups", i, extractor.Group, re.NumSubexp())
		}
	}

	return nil
}

func validateQuotaRules(cfg *Config) error {
	for _, rule := range cfg.Quota.Rules {
		switch strings.ToLower(rule.Window) {
		case "minute", "hour", "day":
		default:
			return fmt.Errorf("invalid quota.rules[%s].window: %q, must be one of minute/hour/day", rule.Name, rule.Window)
		}

		if rule.WindowCount < 1 {
			return fmt.Errorf("invalid quota.rules[%s].window_count: %d, must be >= 1", rule.Name, rule.WindowCount)
		}

		if err := validateRequestMatchRule(rule.Name, &rule.RequestMatch); err != nil {
			return err
		}
	}

	return nil
}

func validateRequestMatchRule(ruleName string, matcher *QuotaRuleRequestMatchConfig) error {
	if matcher == nil {
		return nil
	}

	if err := validateRegexMatcher(ruleName, "query_form", matcher.QueryForm); err != nil {
		return err
	}
	if err := validateRegexMatcher(ruleName, "json_body", matcher.JSONBody); err != nil {
		return err
	}
	if err := validateRegexMatcher(ruleName, "headers", matcher.Headers); err != nil {
		return err
	}

	return nil
}

func validateRegexMatcher(ruleName, fieldName string, matcher *RequestRegexMatchConfig) error {
	if matcher == nil {
		return nil
	}

	if len(matcher.Include) == 0 && len(matcher.Exclude) == 0 {
		return fmt.Errorf("invalid quota.rules[%s].request_match.%s: include/exclude cannot both be empty", ruleName, fieldName)
	}

	for i, pattern := range matcher.Include {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid quota.rules[%s].request_match.%s.include[%d]: %w", ruleName, fieldName, i, err)
		}
	}

	for i, pattern := range matcher.Exclude {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid quota.rules[%s].request_match.%s.exclude[%d]: %w", ruleName, fieldName, i, err)
		}
	}

	return nil
}

// setDefaults 设置默认值
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.port", 3000)
	v.SetDefault("server.read_timeout", "10s")
	v.SetDefault("server.write_timeout", "30s")

	// Upstream
	v.SetDefault("upstream.target", "http://localhost:8080")

	// Redis
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)

	// Identity
	v.SetDefault("identity.strategy", "header_priority")
	v.SetDefault("identity.fallback_to_ip", true)

	// Quota
	v.SetDefault("quota.enabled", true)
	v.SetDefault("quota.timezone", "Asia/Shanghai")
	v.SetDefault("quota.exclude_paths", []string{"/health", "/metrics"})
	v.SetDefault("quota.fail_open", true)
	v.SetDefault("quota.rules", []map[string]interface{}{
		{
			"name":          "default",
			"window":        "day",
			"window_count":  1,
			"success_limit": 10,
			"include_paths": []string{"/**"},
		},
	})

	// SuccessRule
	v.SetDefault("success_rule.mode", "status_code")
	v.SetDefault("success_rule.require_http_2xx", true)
	v.SetDefault("success_rule.json_field", "code")
	v.SetDefault("success_rule.expected_value", 0)

	// Logging
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.access_log", true)

	// Admin
	v.SetDefault("admin.api_key", "")
}

// GetConfig 获取配置
func GetConfig() *Config {
	return appConfig
}
