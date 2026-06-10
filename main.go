package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

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

	http.HandleFunc("/", traceMiddleware(handleProxy))

	log.Println("🚀 SmartShield Gateway MVP 正在启动，监听端口 :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("网关启动异常: %v", err)
	}
}
