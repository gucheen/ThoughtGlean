# 拾念 · ThoughtGlean

拾念是一个属于个人的思考记忆库：念头出现时轻轻留下，日后只凭半句话或大概时间重新找到，并从那里继续想下去。

它不是个人版团队知识库，也不要求先建立文件夹、卡片体系或知识图谱。第一版只专注一条可靠链路：

1. 正文之外没有必填项，快速留下一段原话。
2. 浏览器确认写入本地数据库之前，输入与本地草稿不会清空。
3. 使用中文、中英文混合词语搜索旧记录。
4. 回到当时前后的记录，并从旧念头创建一条续记。
5. 编辑使用 revision 防止多个窗口静默互相覆盖。
6. 删除先进入回收站，可以恢复。

完整取舍见 [docs/product-principles.md](docs/product-principles.md)。

## 当前状态

这是首个可运行切片，包含：

- React/Vite Web UI 与可安装 PWA
- Dexie / IndexedDB 本地副本：笔记、图片、同步队列和游标均按同步库隔离
- 随机 ID、编辑版本、软删除与恢复
- 标题与正文的大小写不敏感、多词 AND 检索
- 时间流、星标、搜索、详情编辑、来源与续记
- 响应式界面、图片粘贴上传与大图查看
- 已配置同步库时的恢复代码解锁页：锁定后隐藏侧边栏、新记录入口与笔记内容
- 产品上每人仅使用一个“我的同步库”；底层分库仅用于防止意外换库时混入数据
- 端到端加密同步：浏览器内存密钥、密文盲中继、实时订阅、周期同步、冲突保留与加密图片 blob

浏览器端是独立静态应用：手机和桌面只需访问其 HTTPS 地址，不需要在设备上运行 Go。生产环境必须使用可信 HTTPS；加密同步中继经由可信反向代理发布。

## 构建与本地预览

需要 Node.js 20+ 和 pnpm 11。浏览器端构建结果位于 `internal/webui/assets/`，可由任意静态站点服务发布。

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm dev
```

开发服务器会显示本地访问地址。生产构建使用：

```bash
pnpm build
```

将 `internal/webui/assets/` 部署到静态 HTTPS 域名即可。浏览器以 IndexedDB 保存本地笔记和图片，并会请求持久化存储以降低系统清理离线数据的概率。

前端使用 History API 路由：`/`、`/starred`、`/all`、`/trash` 和 `/notes/:id`。生产静态服务器需要将不存在的无扩展名路径回退到 `index.html`，同时继续正常提供 `/dist/*`、`manifest.webmanifest` 和 `sw.js`；否则直接刷新笔记详情链接会返回服务器 404。

## Docker 部署

仓库提供一个多目标 `Dockerfile` 和 `compose.yaml`：

- `web` 使用 Nginx 提供 React/PWA、前端路由回退和静态资源缓存。
- `relay` 以非 root 用户运行 Go 密文中继，并把 SQLite 数据保存在命名卷中。
- Nginx 将 `/api/health` 和 `/api/sync/v1/*` 转发到内部 relay；relay 不直接暴露到宿主机。

首次部署先创建环境文件：

```bash
cp .env.example .env
openssl rand -hex 32
```

将第二条命令生成的内容填入 `.env` 的 `THOUGHTGLEAN_RELAY_ENROLLMENT_TOKEN`，然后启动：

```bash
docker compose up -d --build
docker compose ps
```

默认入口为 `http://127.0.0.1:8080`。前端“中继地址”填写这个入口最终对应的公共来源，例如 `https://notes.example.com`，无需附加 `/api` 路径。

生产环境应继续在 Docker 入口之前配置 HTTPS 反向代理。默认仅绑定回环地址，适合 Caddy、宿主机 Nginx 或 Cloudflare Tunnel；若仅在可信局域网临时测试手机访问，可在 `.env` 中将 `THOUGHTGLEAN_HTTP_BIND` 改为 `0.0.0.0`，但 Passkey、PWA 和浏览器加密能力仍应使用 HTTPS。

常用维护命令：

```bash
docker compose logs -f
docker compose pull
docker compose up -d --build
docker compose down
```

`docker compose down` 不会删除同步数据卷。不要执行 `docker compose down -v`，除非明确要永久删除 relay 中的全部密文数据。

## 加密同步中继

中继需要 Go 1.26、CGO 和可用的 C 编译器。它不提供 Web UI，也不存储明文笔记。

```bash
go run ./cmd/thoughtglean
```

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `THOUGHTGLEAN_ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `THOUGHTGLEAN_DATA_DIR` | `./data` | SQLite 数据目录 |
| `THOUGHTGLEAN_RELAY_ENROLLMENT_TOKEN` | 必填 | 仅创建新同步库时需要的 relay 注册密钥 |

## 验证

```bash
go test ./...
go vet ./...
go build ./cmd/thoughtglean
pnpm check
pnpm build
```

## 端到端加密同步

同步中继与本机笔记库是两个部署角色：本机库用于离线可读写和搜索；远端中继只保存 AES-GCM 密文操作包。加密密钥从仅由用户保管的恢复代码派生，未解锁时浏览器不保存密钥或中继访问令牌，也就不会进行远端读写。

同步事件覆盖笔记、来源和图片的创建、更新、删除、恢复及续记关系。并发版本会保留为可见的“同步冲突”续记；服务端仅保存 AES-GCM 密文操作和密文图片 blob。完整协议、恢复取舍和接口说明见 [docs/encrypted-sync.md](docs/encrypted-sync.md)。

### 部署中继

远端中继应使用独立的数据目录和域名，并启用只读中继模式：

```bash
THOUGHTGLEAN_ADDR=127.0.0.1:8081 \
THOUGHTGLEAN_DATA_DIR=/var/lib/thoughtglean-relay \
THOUGHTGLEAN_SYNC_RELAY_ONLY=true \
THOUGHTGLEAN_RELAY_ENROLLMENT_TOKEN='自行生成的长随机字符串' go run ./cmd/thoughtglean
```

再由 HTTPS 反向代理将该端口发布为同步地址。在此模式下，进程只开放 `/api/health` 与 `/api/sync/v1/`：没有 Web UI、Passkey、笔记、附件、备份、Markdown 镜像或本机同步 API。因此远端数据库可以只保存 vault 的令牌哈希、密文操作包和密文图片 blob；不要将它与任一本机笔记库共用数据目录。同步客户端填入的是该 HTTPS 地址。注册密钥只在首次创建同步库时填写，加入已有库只需恢复配对码。
