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
	// 规则一：微服务返回了 200，但身体是空的或者只有个单词 "null"（Java 微服务常见大坑）
	// 前端直接解析会爆反序列化异常，网关本地确定性将其修补为标准的合法空 JSON
	if statusCode == http.StatusOK && (len(bodyBytes) == 0 || string(bodyBytes) == "null") {
		return []byte(`{}`), true
	}
	// 规则二：404 路由丢失异常
	// 统一将各类后端的 404 HTML 页面，强行收敛为网关标准 API 错误 JSON 契约

	if statusCode == http.StatusNotFound {
		return []byte(`{"code": 40400, "error": "Not Found", "message": "SmartShield 确定性自愈：请求的后端资源或路由不存在"}`), true
	}

	// 规则三：502 / 503 崩塌型异常，且返回的不是合法 JSON（通常是 Nginx 或网关级的 HTML 报错白页）
	if (statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable) && !json.Valid(bodyBytes) {
		return []byte(`{"code": 50200, "error": "Bad Gateway", "message": "SmartShield 确定性自愈：检测到微服务节点脱机或宕机，已由网关本地兜底"}`), true
	}

	// 规则库未命中 -> 说明这个坏数据很复杂，必须交由下一层的 AI 语义引擎去“动脑子”修复
	return nil, false
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
	//钩子
	proxy.ModifyResponse = func(resp *http.Response) error {
		traceID, _ := resp.Request.Context().Value(traceIDKey).(string)

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("读取后端响应失败: %v", err)
		}
		resp.Body.Close()

		// 【核心演进】：第一层：尝试走确定性自愈（快路径）
		if fixedBody, matched := tryDeterministicHeal(resp.StatusCode, bodyBytes); matched {
			log.Printf("[%s] 🛡️ [快路径自愈成功] 成功命中本地确定性自愈规则！状态码已从 %d 修正为 200", traceID, resp.StatusCode)
			bodyBytes = fixedBody
			resp.StatusCode = http.StatusOK
			resp.Status = "200 OK"
		} else if resp.StatusCode >= 400 {
			// 第二层：如果确定性自愈没能处理，而且状态码依然大于 400
			log.Printf("[%s] 🤖 [准备降级至AI层] 确定性自愈未命中，数据非标准化，准备交由 AI 语义引擎自愈...", traceID)

			// 【未来 Phase 3：此处放真正的 DeepSeek 大模型网络调用逻辑】
			// 目前如果没有 AI，我们先给个占位提示
			bodyBytes = []byte(`{"code": 50000, "message": "确定性规则未匹配，等待 AI 语义自愈模块激活"}`)
			resp.StatusCode = http.StatusOK
			resp.Status = "200 OK"
		} else {
			// 一切正常，放行
			isValidJSON := json.Valid(bodyBytes)
			log.Printf("[%s] 🔙 流量正常通过 -> 状态码: %d, 是否合法JSON: %t", traceID, resp.StatusCode, isValidJSON)
		}
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		resp.ContentLength = int64(len(bodyBytes))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
		resp.Header.Set("Content-Type", "application/json") // 既然都被治成了 JSON，统一返回头

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
