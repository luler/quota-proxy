package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"gin_base/app/config"
	"gin_base/app/helper/log_helper"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ReverseProxy 反向代理
type ReverseProxy struct {
	target      *url.URL
	transport   *http.Transport
	maxBodySize int64
}

var ErrBodyTooLarge = errors.New("body size exceeds limit")

// NewReverseProxy 创建反向代理
func NewReverseProxy(cfg *config.UpstreamConfig, maxBodySize int64) (*ReverseProxy, error) {
	target, err := url.Parse(cfg.Target)
	if err != nil {
		return nil, err
	}

	if maxBodySize <= 0 {
		maxBodySize = 100 << 20 // 100MB
	}

	responseTimeout := cfg.ResponseTimeout
	if responseTimeout <= 0 {
		responseTimeout = 120 * time.Second
	}

	return &ReverseProxy{
		target:      target,
		maxBodySize: maxBodySize,
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: responseTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}, nil
}

// ProxyResult 代理结果
type ProxyResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Forward 转发请求到上游
func (p *ReverseProxy) Forward(c *gin.Context) (*ProxyResult, error) {
	resp, err := p.Do(c)
	if err != nil {
		return nil, err
	}

	return p.ReadResponse(resp)
}

// Do 发起上游请求并返回原始响应
func (p *ReverseProxy) Do(c *gin.Context) (*http.Response, error) {
	// 读取请求体
	var bodyBytes []byte
	if c.Request.Body != nil {
		lr := io.LimitReader(c.Request.Body, p.maxBodySize+1)
		data, _ := io.ReadAll(lr)
		if int64(len(data)) > p.maxBodySize {
			return nil, fmt.Errorf("%w: request body exceeds %d bytes", ErrBodyTooLarge, p.maxBodySize)
		}
		bodyBytes = data
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}

	// 构建目标 URL
	targetURL := p.target.ResolveReference(&url.URL{
		Path:     c.Request.URL.Path,
		RawQuery: c.Request.URL.RawQuery,
	})

	// 创建上游请求
	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL.String(), bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, err
	}

	// 复制请求头（排除 Host）
	for key, values := range c.Request.Header {
		if strings.ToLower(key) == "host" {
			continue
		}
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}

	// 设置上游 Host
	upstreamReq.Host = p.target.Host

	// 发送请求
	resp, err := p.transport.RoundTrip(upstreamReq)
	if err != nil {
		log_helper.Error("Upstream request failed", "error", err)
		return nil, err
	}

	return resp, nil
}

// ReadResponse 读取完整响应
func (p *ReverseProxy) ReadResponse(resp *http.Response) (*ProxyResult, error) {
	defer resp.Body.Close()

	lr := io.LimitReader(resp.Body, p.maxBodySize+1)
	respBody, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(respBody)) > p.maxBodySize {
		return nil, fmt.Errorf("%w: response body exceeds %d bytes", ErrBodyTooLarge, p.maxBodySize)
	}

	return &ProxyResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       respBody,
	}, nil
}

// IsSSE 判断响应是否为 SSE
func (p *ReverseProxy) IsSSE(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// StreamSSE 流式转发 SSE 响应，并在首个事件成功写出后回调
func (p *ReverseProxy) StreamSSE(c *gin.Context, resp *http.Response, onFirstEvent func() error) (bool, error) {
	defer resp.Body.Close()

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false, http.ErrNotSupported
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	c.Status(resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	var frame bytes.Buffer
	frameHasEventField := false
	confirmed := false

	flushFrame := func() error {
		if frame.Len() == 0 {
			return nil
		}

		if _, err := c.Writer.Write(frame.Bytes()); err != nil {
			return err
		}
		flusher.Flush()

		if frameHasEventField && !confirmed && onFirstEvent != nil {
			if err := onFirstEvent(); err != nil {
				return err
			}
			confirmed = true
		}

		frame.Reset()
		frameHasEventField = false
		return nil
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			frame.Write(line)
			trimmed := strings.TrimRight(string(line), "\r\n")
			if isSSEFieldLine(trimmed) {
				frameHasEventField = true
			}
			if trimmed == "" {
				if err := flushFrame(); err != nil {
					return confirmed, err
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				if flushErr := flushFrame(); flushErr != nil {
					return confirmed, flushErr
				}
				return confirmed, nil
			}
			return confirmed, err
		}
	}
}

func isSSEFieldLine(line string) bool {
	return strings.HasPrefix(line, "data:") ||
		strings.HasPrefix(line, "event:") ||
		strings.HasPrefix(line, "id:") ||
		strings.HasPrefix(line, "retry:")
}

// WriteResponse 写入响应到客户端
func (p *ReverseProxy) WriteResponse(c *gin.Context, result *ProxyResult) {
	// 复制响应头
	for key, values := range result.Headers {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 写入状态码和响应体，保留上游原始 Content-Type
	c.Status(result.StatusCode)
	_, _ = c.Writer.Write(result.Body)
}
