// =============================================================================
// 配置管理模块
// =============================================================================
//
// 负责读取和解析 YAML 格式的配置文件（默认 config.yaml）。
// 配置项包括：监听地址、缓存目录、代理域名白名单、上游地址、资源域名列表、
// 缓存过期时间和容量上限。
//
// 配置示例见 config.yaml 文件。
// =============================================================================

package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是顶层配置结构体，对应 YAML 文件的根节点。
// 所有配置项嵌套在 server 字段下。
type Config struct {
	Server struct {
		// Listen 是 HTTP 服务器监听的地址和端口，如 ":8080" 或 "0.0.0.0:8080"
		Listen string `yaml:"listen"`

		// CacheDir 是静态资源缓存的本地目录路径，支持相对路径和绝对路径
		CacheDir string `yaml:"cache_dir"`

		// ProxyDomains 是允许代理的博客域名白名单。
		// 只有此列表中的域名才会被代理，防止被滥用为开放代理。
		// 例如：["www.my-blog.com", "blog.example.com"]
		ProxyDomains []string `yaml:"proxy_domains"`

		// SameSiteDomains 是与当前请求域名视为同一站点的额外域名列表。
		// 指向这些域名的 <a href> 导航链接也会被重写为相对路径。
		// 例如博客同时通过 www.example.com 和 example.com 访问时，可将另一个填入此列表。
		SameSiteDomains []string `yaml:"same_site_domains"`

		// TargetAddress 是上游 Blogger 托管服务的地址。
		// 默认使用 Google Hosted 服务：https://ghs.googlehosted.com
		TargetAddress string `yaml:"target_address"`

		// CacheDomains 是需要重写并缓存的静态资源域名列表。
		// HTML 中指向这些域名的资源链接会被替换为 /static-cache/?url=... 的本地路径。
		// 同时支持子域名匹配（如配置 bp.blogspot.com 也会匹配 sub.bp.blogspot.com）。
		// 典型配置包括：
		//   - bp.blogspot.com         — Blogger 图片托管
		//   - resources.blogblog.com  — Blogger JS/CSS 资源
		//   - www.blogger.com         — Blogger 自身资源
		//   - fonts.googleapis.com    — Google 字体 CSS
		//   - fonts.gstatic.com       — Google 字体文件
		//   - lh3.googleusercontent.com — Google 用户内容（图片等）
		CacheDomains []string `yaml:"cache_domains"`

		// CacheTTL 是缓存文件的存活时间（Time To Live）。
		// 超过此时间的缓存文件会被自动清理。
		// 格式为 Go duration 字符串：1m（1分钟）、168h（7天）、720h（30天）等。
		// 默认值：24h（1天）
		CacheTTL string `yaml:"cache_ttl"`

		// MaxCacheSize 是缓存目录的最大容量限制。
		// 超出此限制时，按文件修改时间从旧到新淘汰，直到低于限制。
		// 支持格式：1GB、500MB、100KB、TB 等。
		// 默认值：1GB
		MaxCacheSize string `yaml:"max_cache_size"`
	} `yaml:"server"`
}

// LoadConfig 从 YAML 文件加载配置。
//
// 参数：
//   filename — YAML 配置文件的路径
//
// 返回值：
//   *Config — 解析后的配置对象
//   error   — 文件读取或解析失败时返回错误
//
// 默认值处理：
//   - CacheTTL 为空时默认 "1m"（1分钟）
//   - MaxCacheSize 为空时默认 "1GB"
func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// 应用默认值：如果配置文件中未指定，使用合理的默认值
	if config.Server.CacheTTL == "" {
		config.Server.CacheTTL = "24h" // 默认 1 天
	}
	if config.Server.MaxCacheSize == "" {
		config.Server.MaxCacheSize = "1GB" // 默认 1GB
	}
	return &config, nil
}