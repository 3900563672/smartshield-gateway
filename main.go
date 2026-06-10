package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

// 令牌桶
type TokenBucketLimiter struct {
	tokens chan struct{}
}

func NewTokenBucketLimiter(maxTokens int, refillRate time.Duration) *TokenBucketLimiter {

	limiter := &TokenBucketLimiter{
		tokens: make(chan struct{}, maxTokens),
	}

	for i := 0; i < maxTokens; i++ {
		limiter.tokens <- struct{}{}
	}

	go func() {
		ticker := time.NewTicker(refillRate)
		for range ticker.C {
			select {
			case limiter.tokens <- struct{}{}:
			default:
			}
		}
	}()
	return limiter
}

func (tl *TokenBucketLimiter) Allow() bool {
	select {
	case <-tl.tokens:
		return true
	default:
		return false
	}
}

var globalLimiter = NewTokenBucketLimiter(3, 330*time.Millisecond)

func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !globalLimiter.Allow() {
			// 没令牌了，直接拦截，返回 429 状态码
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "Too Many Requests", "message": "网关流量触发熔断，请稍后再试"}`))
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

	http.HandleFunc("/", traceMiddleware((rateLimitMiddleware(handleProxy))))

	log.Println("🚀 SmartShield Gateway MVP 正在启动，监听端口 :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("网关启动异常: %v", err)
	}
}
