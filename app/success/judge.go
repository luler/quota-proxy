package success

import (
	"encoding/json"
	"gin_base/app/config"
	"net/http"
	"strconv"
)

// Judge 成功判定器接口
type Judge interface {
	IsSuccess(resp *http.Response, body []byte) bool
}

// StatusCodeJudge HTTP 状态码判定器
type StatusCodeJudge struct {
	requireHTTP2xx bool
}

// NewStatusCodeJudge 创建状态码判定器
func NewStatusCodeJudge(requireHTTP2xx bool) *StatusCodeJudge {
	return &StatusCodeJudge{
		requireHTTP2xx: requireHTTP2xx,
	}
}

// IsSuccess 判断是否成功
func (j *StatusCodeJudge) IsSuccess(resp *http.Response, body []byte) bool {
	if j.requireHTTP2xx {
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	return resp.StatusCode < 400
}

// JSONFieldJudge JSON 字段判定器
type JSONFieldJudge struct {
	requireHTTP2xx bool
	jsonField      string
	expectedValue  int
}

// NewJSONFieldJudge 创建 JSON 字段判定器
func NewJSONFieldJudge(requireHTTP2xx bool, jsonField string, expectedValue int) *JSONFieldJudge {
	return &JSONFieldJudge{
		requireHTTP2xx: requireHTTP2xx,
		jsonField:      jsonField,
		expectedValue:  expectedValue,
	}
}

// IsSuccess 判断是否成功
func (j *JSONFieldJudge) IsSuccess(resp *http.Response, body []byte) bool {
	// 先检查 HTTP 状态码
	if j.requireHTTP2xx {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return false
		}
	}

	// 解析 JSON
	if len(body) == 0 {
		return false
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return false
	}

	// 获取字段值
	value, exists := data[j.jsonField]
	if !exists {
		return false
	}

	// 比较值
	switch v := value.(type) {
	case float64:
		return int(v) == j.expectedValue
	case int:
		return v == j.expectedValue
	case string:
		intVal, err := strconv.Atoi(v)
		if err != nil {
			return false
		}
		return intVal == j.expectedValue
	default:
		return false
	}
}

// NewJudge 根据配置创建判定器
func NewJudge(cfg *config.SuccessRuleConfig) Judge {
	switch cfg.Mode {
	case "json_field":
		return NewJSONFieldJudge(cfg.RequireHTTP2xx, cfg.JSONField, cfg.ExpectedValue)
	default:
		return NewStatusCodeJudge(cfg.RequireHTTP2xx)
	}
}