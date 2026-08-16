# Blogger Proxy

Blogger 博客反向代理加速服务 —— 支持将 Google 静态资源缓存到代理，解决墙内无法的问题。

---

## 架构图

```
                              ┌─────────────────────────┐
                              │    用户浏览器              │
                              │  blog.your-domain.com    │
                              └────────────┬────────────┘
                                           │
                                           ▼
                              ┌─────────────────────────┐
                              │   Nginx / Caddy (可选)    │
                              │   TLS 终止 + HTTPS       │
                              └────────────┬────────────┘
                                           │ :443 → :8080
                                           ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                        Blogger Proxy (:8080)                             │
│                                                                          │
│  请求进入 ───► ServeHTTP (路由分发)                                        │
│                    │                                                     │
│         ┌──────────┴──────────┐                                          │
│         │                     │                                          │
│    /static-cache/         其他路径                                        │
│         │                     │                                          │
│         ▼                     ▼                                          │
│  ┌──────────────┐    ┌──────────────┐                                    │
│  │  缓存模块      │    │  域名白名单    │                                    │
│  │              │    │  检查         │                                    │
│  │              │    └──────┬───────┘                                    │
│  │  ┌─────────┐ │           │ 通过                                        │
│  │  │ 缓存命中? │ │           ▼                                            │
│  │  └────┬────┘ │    ┌──────────────┐                                    │
│  │  是   │  否  │    │  反向代理模块  │                                    │
│  │   │   │   │  │    │              │                                    │
│  │   ▼   │   ▼  │    │  Director    │──► 设置 Host, 移除 Accept-Encoding  │
│  │ 直接  │ 下载  │    │              │                                    │
│  │ 服务  │ 缓存  │    │  Transport   │──► 超时控制, 连接池                 │
│  │       │      │    │              │                                    │
│  │       │ ┌───┐│    │  ModifyResp  │──► HTML 响应拦截                    │
│  │       │ │SF ││    └──────┬───────┘                                    │
│  │       │ └───┘│           │                                            │
│  │       │      │           ▼                                            │
│  │       │  ┌───▼────┐  ┌───────────────────────┐                        │
│  │       │  │原子写入 │  │    HTML 重写器          │                        │
│  │       │  │.tmp→文件│  │                       │                        │
│  │       │  └────────┘  │  ┌─────────────────┐   │                        │
│  │       └──────────────┤  │ HTML Tokenizer  │   │ 遍历标签和属性            │
│  └──────────────────────┤  └────────┬────────┘   │                        │
│                         │           │            │                        │
│  ┌──────────────────┐   │  ┌────────▼────────┐   │                        │
│  │  后台清理任务      │   │  │   URL 重写规则   │   │                        │
│  │  · 每小时执行      │   │  │                │   │                        │
│  │  · TTL 过期清理    │   │  │ <link href>    │   │ 重写为                  │
│  │  · 容量淘汰 (LRU)  │   │  │ <script src>   │   │ /static-cache/          │
│  │  · .tmp 残留清理   │   │  │ <img src/srcset│   │ ?url=base64(...)        │
│  └──────────────────┘   │  │ <video poster>  │   │                        │
│                         │  │ style="url()"   │   │                        │
│                         │  │ <style> 内容     │   │                        │
│                         │  └─────────────────┘   │                        │
│                         └───────────────────────┘                        │
└─────────────────────────────────────────────────────────────────────────┘
           │                                │
           ▼                                ▼
  ┌─────────────────┐            ┌──────────────────────┐
  │   本地磁盘缓存    │            │  Google Blogger 上游   │
  │  static_cache/   │            │  ghs.google.com │
  │                 │            │                      │
  │  a3f2b1c9.css  │            │  ┌────────────────┐   │
  │  d4e5f6a7.js   │            │  │ bp.blogspot.com│   │
  │  g8h9i0j1.png  │            │  │ fonts.google...│   │
  │  ...           │            │  │ lh3.google...  │   │
  └─────────────────┘            │  └────────────────┘   │
                                 └──────────────────────┘
```

---

## 数据流详解

### 场景 1：首次访问博客页面

```
用户浏览器                    Blogger Proxy                    Google 上游
    │                             │                               │
    │  GET / (blog.example.com)    │                               │
    │─────────────────────────────►│                               │
    │                             │  域名白名单检查 ✓                │
    │                             │  GET / (ghs.google.com)   │
    │                             │───────────────────────────────►│
    │                             │                               │
    │                             │      HTML (含原始资源URL)        │
    │                             │◄───────────────────────────────│
    │                             │                               │
    │                             │  HTML 重写：                    │
    │                             │  <link href="https://fonts.googleapis.com/...">
    │                             │  → <link href="/static-cache/?url=...">
    │                             │                               │
    │  重写后的 HTML               │                               │
    │◄────────────────────────────│                               │
    │                             │                               │
    │  GET /static-cache/?url=... │                               │
    │─────────────────────────────►│                               │
    │                             │  缓存未命中 → 下载              │
    │                             │  GET https://fonts.googleapis.com/...
    │                             │───────────────────────────────►│
    │                             │      CSS 文件                  │
    │                             │◄───────────────────────────────│
    │                             │  写入本地磁盘 + 返回客户端        │
    │  CSS 文件                   │                               │
    │◄────────────────────────────│                               │
```

### 场景 2：再次访问（缓存命中）

```
用户浏览器                    Blogger Proxy
    │                             │
    │  GET /static-cache/?url=... │
    │─────────────────────────────►│
    │                             │  SHA256 查文件 → 存在 ✓
    │                             │  设置 Cache-Control: max-age=31536000
    │  304 Not Modified / 200     │
    │◄────────────────────────────│
    │                    (不访问上游，极快)
```

### 场景 3：并发请求同一资源（Singleflight）

```
请求1 ──► 缓存未命中 ──► singleflight.Do ──► 执行下载（只有1次HTTP请求）
请求2 ──► 缓存未命中 ──► 等待 ◄────────────┤
请求3 ──► 缓存未命中 ──► 等待 ◄────────────┘
                                            │
                         下载完成 ──────────┘
                                            │
请求1 ◄── 流式响应（下载时已返回）            │
请求2 ◄── serveFileFromCache ──────────────┘
请求3 ◄── serveFileFromCache ──────────────┘
```

---

## 快速开始

### 前置条件

- Go 1.26+ 或 Docker
- 一个 Blogger 博客（已绑定自定义域名）

### 1. 修改配置文件

编辑 `config.yaml`：

```yaml
server:
  listen: ":8080"
  proxy_domains:
    - "www.your-blog-domain.com"   # ← 改成你的博客域名
  # ... 其他配置保持默认即可
```

### 2. 启动服务

**方式 A：直接运行**

```bash
# 编译
go build -o blogger-proxy .

# 运行
./blogger-proxy config.yaml
```

**方式 B：Docker**

```bash
# 构建镜像
docker build -t blogger-proxy .

# 运行容器
docker run -d \
  --name blogger-proxy \
  -p 8080:8080 \
  -v $(pwd)/static_cache:/app/static_cache \
  -v $(pwd)/config.yaml:/app/config.yaml \
  blogger-proxy
```

### 3. 配置 DNS 或反向代理

**方案 A：直接指向（测试用）**

将博客域名 DNS 解析到你的服务器 IP，访问 `http://www.your-blog-domain.com:8080`。

**方案 B：Nginx 反向代理（推荐生产环境）**

```nginx
server {
    listen 80;
    server_name www.your-blog-domain.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

**方案 C：Cloudflare Workers / CDN**

将域名 CNAME 到你的服务器，开启 CDN 代理。

---

## 配置参考

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `listen` | string | — | 监听地址，如 `:8080` 或 `127.0.0.1:8080` |
| `cache_dir` | string | — | 缓存目录，如 `./static_cache` |
| `cache_ttl` | duration | `1m` | 缓存有效期，超时自动删除 |
| `max_cache_size` | size | `1GB` | 缓存容量上限，超出后淘汰旧文件 |
| `proxy_domains` | []string | — | 允许代理的博客域名白名单 |
| `target_address` | string | — | Blogger 上游地址，默认 `https://ghs.google.com` |
| `cache_domains` | []string | — | 需要缓存加速的资源域名列表 |

### cache_ttl 格式

```
1m    = 1分钟
168h  = 7天
720h  = 30天
24h   = 1天
```

### max_cache_size 格式

```
1GB   = 1073741824 字节
500MB = 524288000 字节
100KB = 102400 字节
```

### 推荐的 cache_domains

```yaml
cache_domains:
  - "bp.blogspot.com"              # Blogger 图片托管
  - "resources.blogblog.com"       # Blogger 主题 JS/CSS
  - "www.blogger.com"              # Blogger 自身资源
  - "fonts.googleapis.com"         # Google 字体 CSS
  - "fonts.gstatic.com"            # Google 字体文件
  - "lh3.googleusercontent.com"    # Google 用户内容
```

---

## 模块说明

```
blogger-proxy/
├── main.go              # 入口：加载配置 → 初始化 → 启动服务
├── config.go            # YAML 配置解析 + 默认值
├── config.yaml          # 配置文件（部署时修改）
├── proxy.go             # 核心：反向代理 + HTML 资源链接重写
├── cache.go             # 缓存：下载/存储/清理 + Singleflight
├── config_test.go       # 配置加载测试
├── cache_test.go        # 缓存逻辑测试（parseSize, cleanCache）
├── proxy_test.go        # HTML 重写测试（15+ 场景）
├── Dockerfile           # Docker 多阶段构建
└── .dockerignore        # 排除构建产物
```

### 关键设计决策

| 决策 | 原因 |
|------|------|
| HTML tokenizer 而非正则 | 正则无法区分属性值和普通文本，会误改写 `<p>Visit fonts.googleapis.com</p>` |
| 先写 .tmp 再 Rename | 原子操作，避免"写了一半"的文件被读取 |
| SHA256 作为文件名 | 内容不变则 URL 不变，可安全设置 1 年缓存 |
| 移除 Accept-Encoding | 强制上游返回未压缩内容，以便解析 HTML |
| `<a href>` 不重写 | 导航链接不需要缓存，也不在 `urlAttrs` 映射中 |
| Singleflight 下载 | 防止同一资源被并发请求下载多次 |
| 小时级清理而非请求时 | 避免每次请求都检查文件时间，减少磁盘 I/O |

---

## 监控与运维

### 查看日志

```bash
# 直接运行
./blogger-proxy config.yaml

# Docker
docker logs -f blogger-proxy
```

日志示例：
```
🚀 Starting Blogger Proxy on :8080
📁 Cache Directory: ./static_cache
🌐 Proxy Domains: [www.my-blog.com]
⏰ Cache TTL: 1m, Max Size: 1GB
📊 Cache stats: 1423 files, 234.5 MB used
📥 Downloading resource: https://fonts.googleapis.com/css?...
✅ Cached resource: https://fonts.googleapis.com/css?...
⚡ Served from cache: https://fonts.googleapis.com/css?...
🧹 Removed expired cache: a3f2b1c9.css
🧹 Evicted 15 files to meet size limit
```

### 健康检查

```bash
curl -I http://localhost:8080/
# 如果返回 HTTP 200，说明服务正常运行
```

### 缓存目录管理

```bash
# 查看缓存大小
du -sh ./static_cache/

# 手动清空缓存
rm -rf ./static_cache/*

# 查看缓存文件数量
ls ./static_cache/ | wc -l
```

---

## 常见问题

**Q: 为什么访问博客返回 403？**
A: 检查 `config.yaml` 中的 `proxy_domains` 是否包含了你的博客域名。

**Q: 某些图片仍然加载缓慢？**
A: 检查 `cache_domains` 是否包含了该图片的域名。可以按 F12 打开开发者工具 → Network 面板，查看慢资源的域名，添加到配置中。

**Q: 缓存占用太多磁盘空间？**
A: 调整 `max_cache_size` 为更小的值，或减小 `cache_ttl`。重启后会自动清理。

**Q: 如何让缓存中的资源也走 HTTPS？**
A: 在 Blogger Proxy 前面部署 Nginx/Caddy 做 TLS 终止，或者使用 Cloudflare CDN 代理。

**Q: Docker 容器内缓存目录权限错误？**
A: Dockerfile 中已设置 `chmod 777`，也可以挂载一个宿主机目录：
```bash
docker run -v /host/path/cache:/app/static_cache ...
```