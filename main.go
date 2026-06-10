package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"golang.org/x/time/rate"
)

var globalLimiter = rate.NewLimiter(rate.Every(330*time.Millisecond), 3)

type contextKey string

const traceIDKey contextKey = "traceID"

type DeepSeekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type DeepSeekResponseFormat struct {
	Type string `json:"type"` // 用于强行约束大模型只返回纯 JSON 对象
}

type DeepSeekRequest struct {
	Model          string                 `json:"model"`
	Messages       []DeepSeekMessage      `json:"messages"`
	ResponseFormat DeepSeekResponseFormat `json:"response_format"`
}

type DeepSeekResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func healWithAI(ctx context.Context, errReason string, brokenBody []byte) ([]byte, error) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("未检测到环境变量 DEEPSEEK_API_KEY，AI自愈引擎处于挂起状态")
	}
	systemPrompt := "你是一个高性能微服务网关的自愈代理。下游服务返回的 JSON 数据残缺，未通过 Schema 契约校验。请基于你的语义理解，帮我补充缺失的业务核心字段。你必须保证返回一个结构完整、完全合法的纯 JSON 对象，不要包含任何 markdown 标记（如 ```json）。"
	userPrompt := fmt.Sprintf("【契约崩塌原因】: %s\n【残缺原始JSON】: %s\n请立刻修复并补齐缺失字段，输出对齐后的完美JSON：", errReason, string(brokenBody))

	payload := DeepSeekRequest{
		Model: "deepseek-chat",
		Messages: []DeepSeekMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		ResponseFormat: DeepSeekResponseFormat{Type: "json_object"}, // 强约束底层输出
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "[https://api.deepseek.com/chat/completions](https://api.deepseek.com/chat/completions)", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("大模型API响应异常，状态码: %d, 详情: %s", resp.StatusCode, string(respBytes))
	}

	var dsResp DeepSeekResponse
	if err := json.Unmarshal(respBytes, &dsResp); err != nil {
		return nil, err
	}

	if len(dsResp.Choices) == 0 {
		return nil, fmt.Errorf("大模型未返回任何有效 Choices")
	}

	return []byte(dsResp.Choices[0].Message.Content), nil
}
func tryDeterministicHeal(statusCode int, bodyBytes []byte) ([]byte, bool) {
	if statusCode == http.StatusOK && (len(bodyBytes) == 0 || string(bodyBytes) == "null") {
		return []byte(`{}`), true
	}
	if statusCode == http.StatusNotFound {
		return []byte(`{"code": 40400, "error": "Not Found", "message": "SmartShield 确定性自愈：请求的后端资源或路由不存在"}`), true
	}
	if (statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable) && !json.Valid(bodyBytes) {
		return []byte(`{"code": 50200, "error": "Bad Gateway", "message": "SmartShield 确定性自愈：检测到微服务节点脱机或宕机，已由网关本地兜底"}`), true
	}
	return nil, false
}

func checkSchemaContract(path string, bodyBytes []byte) error {
	if !json.Valid(bodyBytes) {
		return fmt.Errorf("响应数据根本不是合法的 JSON 格式")
	}

	var rawData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawData); err != nil {
		return fmt.Errorf("JSON 反序列化失败")
	}

	// 模拟针对特定业务接口的 Schema 契约检查
	// 假设我们规定：凡是请求 /get 接口，返回的 JSON 必须包含 "url" 和 "origin"（客户端IP）两个核心字段
	if path == "/get" {
		if _, hasUrl := rawData["url"]; !hasUrl {
			return fmt.Errorf("契约校验失败: 缺失核心业务字段 'url'")
		}
		if _, hasOrigin := rawData["origin"]; !hasOrigin {
			return fmt.Errorf("契约校验失败: 缺失核心审计字段 'origin'")
		}
		// 故意引发契约塌陷，导流至第三层 AI
		if _, hasUser := rawData["user_id"]; !hasUser {
			return fmt.Errorf("契约异常: 缺失预期的关键业务字段 'user_id'")
		}
	}
	return nil
}
func NewGatewayProxy(target string) *httputil.ReverseProxy {
	targetURL, err := url.Parse(target)
	if err != nil {
		log.Fatalf("解析后端目标地址失败: %v", err)
	}

	proxy := &httputil.ReverseProxy{}
	proxy.Rewrite = func(pr *httputil.ProxyRequest) {
		pr.SetURL(targetURL)
		pr.Out.Host = targetURL.Host
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		traceID, _ := resp.Request.Context().Value(traceIDKey).(string)
		currentPath := resp.Request.URL.Path

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("读取后端响应失败: %v", err)
		}
		resp.Body.Close()

		// 🌟 第一层：状态码快路径检测
		if fixedBody, matched := tryDeterministicHeal(resp.StatusCode, bodyBytes); matched {
			log.Printf("[%s] 🛡️ [第一层自愈成功] 触发状态码本地规则，完成静态重写", traceID)
			bodyBytes = fixedBody
			resp.StatusCode = http.StatusOK
		} else if resp.StatusCode == http.StatusOK {
			// 🌟 第二层：进入契约 Schema 校验层
			log.Printf("[%s] 🔍 [第二层契约校验] 正在对路径 %s 的响应数据进行 Schema 校验...", traceID, currentPath)

			if schemaErr := checkSchemaContract(currentPath, bodyBytes); schemaErr != nil {
				log.Printf("[%s] 🚨 [契约校验失败！] 错误原因: %v", traceID, schemaErr)
				log.Printf("[%s] 🤖 [降级激活] 准备将请求交由第三层 AI 语义引擎进行全自动智能对齐...", traceID)

				// 🌟 第三层：终极大招——大模型智能自愈（设置 5 秒超时保护，防止拖垮微服务网关）
				aiCtx, cancel := context.WithTimeout(resp.Request.Context(), 5*time.Second)
				defer cancel()

				aiHealedBody, aiErr := healWithAI(aiCtx, schemaErr.Error(), bodyBytes)
				if aiErr != nil {
					// 如果大模型层也挂了，网关启动最后的兜底悲观策略
					log.Printf("[%s] ❌ [AI自愈遭遇挫败] 大模型调用失败: %v", traceID, aiErr)
					bodyBytes = []byte(`{"code":50099,"error":"Gateway Panic","message":"网关分层自愈全部耗尽，数据彻底无法对齐"}`)
				} else {
					log.Printf("[%s] 🎉🎉🎉 [AI自愈神话诞生！] DeepSeek 成功修补数据契约，生成合法对齐响应！", traceID)
					bodyBytes = aiHealedBody
				}
			} else {
				log.Printf("[%s] ✅ [契约校验通过] 数据流完全符合生产标准，准予放行", traceID)
			}
		}
		// 重新装流回填
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		resp.ContentLength = int64(len(bodyBytes))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
		resp.Header.Set("Content-Type", "application/json")

		return nil
	}

	return proxy
}

//限流器

func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !globalLimiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "Too Many Requests", "message": "网关流量触发熔断，官方高精度限流器已拦截"}`))
			return
		}
		next(w, r)
	}
}

// 编写核心代码处理器(自愈)
func handleProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "UP", "message": "SmartShield MVP Gateway core is active!"}`))
}

// 编写中间件
// traceID
func traceMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		traceID := fmt.Sprintf("trace-%d", time.Now().UnixNano())

		log.Printf("[%s] 网关收到请求 -> 方法: %s, 路径: %s", traceID, r.Method, r.URL.Path)

		w.Header().Set("X-Trace-ID", traceID)

		next(w, r)
	}
}
func main() {

	targetBackend := "http://httpbin.org"
	gatewayProxy := NewGatewayProxy(targetBackend)

	http.HandleFunc("/", traceMiddleware(rateLimitMiddleware(gatewayProxy.ServeHTTP)))
	log.Printf("🚀 SmartShield Gateway v0.3.0 正在启动...")
	log.Printf("监听端口 :8080 -> 自动反向代理至: %s", targetBackend)

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("网关启动异常: %v", err)
	}
}
