package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 应用配置结构
type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Upstream    UpstreamConfig    `mapstructure:"upstream"`
	Redis       RedisConfig       `mapstructure:"redis"`
	Identity    IdentityConfig    `mapstructure:"identity"`
	Quota       QuotaConfig       `mapstructure:"quota"`
	SuccessRule SuccessRuleConfig `mapstructure:"success_rule"`
	Logging     LoggingConfig     `mapstructure:"logging"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// UpstreamConfig 上游服务配置
type UpstreamConfig struct {
	Target string `mapstructure:"target"`
}

// RedisConfig Redis 配置
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// IdentityExtractorConfig 请求头提取身份配置
type IdentityExtractorConfig struct {
	Header string `mapstructure:"header"`
	Regex  string `mapstructure:"regex"`
	Group  int    `mapstructure:"group"`
	Name   string `mapstructure:"name"`
}

// IdentityConfig 身份识别配置
type IdentityConfig struct {
	Strategy     string                    `mapstructure:"strategy"`
	Extractors   []IdentityExtractorConfig `mapstructure:"extractors"`
	FallbackToIP bool                      `mapstructure:"fallback_to_ip"`
}

// QuotaConfig 配额配置
type QuotaConfig struct {
	Enabled      bool              `mapstructure:"enabled"`
	Timezone     string            `mapstructure:"timezone"`
	ExcludePaths []string          `mapstructure:"exclude_paths"`
	FailOpen     bool              `mapstructure:"fail_open"`
	Rules        []QuotaRuleConfig `mapstructure:"rules"`
}

// RequestRegexMatchConfig 正则匹配配置
type RequestRegexMatchConfig struct {
	Include []string `mapstructure:"include"`
	Exclude []string `mapstructure:"exclude"`
}

// QuotaRuleRequestMatchConfig 请求内容匹配配置
type QuotaRuleRequestMatchConfig struct {
	QueryForm *RequestRegexMatchConfig `mapstructure:"query_form"`
	JSONBody  *RequestRegexMatchConfig `mapstructure:"json_body"`
	Headers   *RequestRegexMatchConfig `mapstructure:"headers"`
}

// QuotaRuleConfig 路径配额规则
type QuotaRuleConfig struct {
	Name              string                      `mapstructure:"name"`
	Window            string                      `mapstructure:"window"`
	WindowCount       int                         `mapstructure:"window_count"`
	SuccessLimit      int                         `mapstructure:"success_limit"`
	IncludePaths      []string                    `mapstructure:"include_paths"`
	RequestMatch      QuotaRuleRequestMatchConfig `mapstructure:"request_match"`
	QuotaExceededBody *string                     `mapstructure:"quota_exceeded_body"`
}

// SuccessRuleConfig 成功判定规则配置
type SuccessRuleConfig struct {
	Mode           string `mapstructure:"mode"`
	RequireHTTP2xx bool   `mapstructure:"require_http_2xx"`
	JSONField      string `mapstructure:"json_field"`
	ExpectedValue  int    `mapstructure:"expected_value"`
}

// LoggingConfig 日志配置
type LoggingConfig struct {
	Level     string `mapstructure:"level"`
	AccessLog bool   `mapstructure:"access_log"`
}

var appConfig *Config

// Load 加载配置
func Load() (*Config, error) {
	v := viper.New()

	// 设置默认值
	setDefaults(v)

	// 从配置文件读取
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.yaml"
	}

	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// 允许环境变量覆盖
	v.SetEnvPrefix("QUOTA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		// 配置文件不存在时使用默认值
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	if err := validateIdentityConfig(&cfg); err != nil {
		return nil, err
	}

	if err := validateQuotaRules(&cfg); err != nil {
		return nil, err
	}

	appConfig = &cfg
	return &cfg, nil
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
}

// GetConfig 获取配置
func GetConfig() *Config {
	return appConfig
}
