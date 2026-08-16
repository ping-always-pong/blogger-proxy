// =============================================================================
// 配置加载测试
// =============================================================================
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_Defaults 验证：未配置的字段使用默认值。
func TestLoadConfig_Defaults(t *testing.T) {
	// 创建临时目录和配置文件
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  listen: ":9090"
  cache_dir: "./cache"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	// 验证显式配置的值
	if cfg.Server.Listen != ":9090" {
		t.Errorf("expected listen :9090, got %s", cfg.Server.Listen)
	}
	if cfg.Server.CacheDir != "./cache" {
		t.Errorf("expected cache_dir ./cache, got %s", cfg.Server.CacheDir)
	}
	// 验证默认值：CacheTTL 应为 "1m"（1分钟）
	if cfg.Server.CacheTTL != "1m" {
		t.Errorf("expected default CacheTTL 1m, got %s", cfg.Server.CacheTTL)
	}
	// 验证默认值：MaxCacheSize 应为 "1GB"
	if cfg.Server.MaxCacheSize != "1GB" {
		t.Errorf("expected default MaxCacheSize 1GB, got %s", cfg.Server.MaxCacheSize)
	}
}

// TestLoadConfig_CustomValues 验证：所有字段都可以从配置文件正确读取。
func TestLoadConfig_CustomValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  listen: ":3000"
  cache_dir: "/tmp/cache"
  cache_ttl: "24h"
  max_cache_size: "500MB"
  proxy_domains:
    - "example.com"
  same_site_domains:
    - "m.example.com"
    - "www.example.com"
  target_address: "https://target.example.com"
  cache_domains:
    - "cdn.example.com"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	// 验证自定义值覆盖了默认值
	if cfg.Server.CacheTTL != "24h" {
		t.Errorf("expected CacheTTL 24h, got %s", cfg.Server.CacheTTL)
	}
	if cfg.Server.MaxCacheSize != "500MB" {
		t.Errorf("expected MaxCacheSize 500MB, got %s", cfg.Server.MaxCacheSize)
	}
	// 验证列表字段
	if len(cfg.Server.ProxyDomains) != 1 || cfg.Server.ProxyDomains[0] != "example.com" {
		t.Errorf("expected proxy_domains [example.com], got %v", cfg.Server.ProxyDomains)
	}
	if len(cfg.Server.SameSiteDomains) != 2 || cfg.Server.SameSiteDomains[0] != "m.example.com" || cfg.Server.SameSiteDomains[1] != "www.example.com" {
		t.Errorf("expected same_site_domains [m.example.com www.example.com], got %v", cfg.Server.SameSiteDomains)
	}
	if len(cfg.Server.CacheDomains) != 1 || cfg.Server.CacheDomains[0] != "cdn.example.com" {
		t.Errorf("expected cache_domains [cdn.example.com], got %v", cfg.Server.CacheDomains)
	}
}

// TestLoadConfig_FileNotFound 验证：配置文件不存在时返回错误。
func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("nonexistent.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}