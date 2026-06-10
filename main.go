package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"golang.org/x/time/rate"
)

var globalLimiter = rate.NewLimiter(rate.Every(330*time.Millisecond), 3)

type contextKey string

const traceIDKey contextKey = "traceID"

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

		// 【高能模拟】：我们故意人为追加一条 strict 规则：必须包含 "user_id"
		// 因为 httpbin.org 的 /get 接口绝对不会返回 user_id，这必然会触发我们的契约警报！
		if _, hasUser := rawData["user_id"]; !hasUser {
			return fmt.Errorf("契约异常(Schema Drift): 缺失预期的关键业务字段 'user_id'")
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

		// 🔍 【分层自愈架构升级】
		// 🌟 第一层：状态码快路径检测
		if fixedBody, matched := tryDeterministicHeal(resp.StatusCode, bodyBytes); matched {
			log.Printf("[%s] 🛡️ [第一层：快路径自愈成功] 触发状态码本地规则，完成修正", traceID)
			bodyBytes = fixedBody
			resp.StatusCode = http.StatusOK
		} else if resp.StatusCode == http.StatusOK {
			// 🌟 第二层：状态码为 200，进入契约 Schema 校验层
			log.Printf("[%s] 🔍 [第二层：结构契约校验] 正在对路径 %s 的响应数据进行 Schema 校验...", traceID, currentPath)

			if schemaErr := checkSchemaContract(currentPath, bodyBytes); schemaErr != nil {
				// 契约塌陷！拉响警报，准备将数据移交给第三层（AI 语义层）
				log.Printf("[%s] 🚨 [契约校验失败！] 错误原因: %v", traceID, schemaErr)
				log.Printf("[%s] 🤖 [准备降级至AI层] 200 OK 伪劣数据已被拦截，准备交由 AI 大模型进行语义修复与字段对齐...", traceID)

				// 暂时给个占位数据，下一版在这里放 DeepSeek 强力修复逻辑
				bodyBytes = []byte(fmt.Sprintf(`{"code": 50010, "message": "Schema契约塌陷 (%v)，等待 AI 语义自愈模块接管修复"}`, schemaErr.Error()))
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
