// =============================================================================
// Blogger Proxy — Blogger 博客反向代理加速服务
// =============================================================================
//
// 功能概述：
//   1. 反向代理 Blogger 博客页面（通过 Google Hosted 服务）
//   2. 拦截 HTML 中的静态资源链接（CSS/JS/图片/字体等），重写为本地缓存路径
//   3. 首次访问时下载资源到本地磁盘，后续请求直接从缓存服务
//   4. 自动清理过期缓存和超出容量限制的文件
//
// 使用场景：
//   国内用户访问 Blogger 博客时，Google 静态资源域名（bp.blogspot.com、
//   fonts.googleapis.com 等）加载缓慢或不可用。部署此代理后，所有静态资源
//   会被缓存到本地服务器，实现加速访问。
//
// 部署方式：
//   - 直接运行：go build && ./blogger-proxy config.yaml
//   - Docker：   docker build -t blogger-proxy . && docker run -p 8080:8080 ...
//
// 架构说明：
//   main.go      — 程序入口，加载配置，启动 HTTP 服务器
//   config.go    — YAML 配置文件解析
//   proxy.go     — 反向代理核心 + HTML 资源链接重写
//   cache.go     — 静态资源下载、缓存、清理
// =============================================================================

package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	// ---- 第 1 步：加载配置文件 ----
	// 默认读取当前目录下的 config.yaml，也可以通过命令行参数指定
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("❌ Failed to load config: %v", err)
	}

	// ---- 第 2 步：初始化缓存目录 ----
	// 确保缓存目录存在，权限 0755（所有者可读写执行，其他用户可读执行）
	if err := os.MkdirAll(cfg.Server.CacheDir, 0755); err != nil {
		log.Fatalf("❌ Failed to create cache directory: %v", err)
	}

	// ---- 第 3 步：启动缓存清理任务 ----
	// 启动时立即执行一次清理（删除过期文件、淘汰超出容量限制的旧文件），
	// 之后每小时自动执行一次。
	StartCacheCleanup(cfg)

	// ---- 第 4 步：初始化代理处理器 ----
	// 创建反向代理 + HTML 重写器，配置上游超时等参数
	proxyHandler, err := NewProxyHandler(cfg)
	if err != nil {
		log.Fatalf("❌ Failed to initialize proxy: %v", err)
	}

	// ---- 第 5 步：启动 HTTP 服务器 ----
	log.Printf("🚀 Starting Blogger Proxy on %s", cfg.Server.Listen)
	log.Printf("📁 Cache Directory: %s", cfg.Server.CacheDir)
	log.Printf("🌐 Proxy Domains: %v", cfg.Server.ProxyDomains)
	log.Printf("⏰ Cache TTL: %s, Max Size: %s", cfg.Server.CacheTTL, cfg.Server.MaxCacheSize)

	server := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: proxyHandler,
	}

	// ListenAndServe 会阻塞直到服务出错或主动关闭
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("❌ Server error: %v", err)
	}
}