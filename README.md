# 拾念 · ThoughtGlean

拾念是一个属于个人的思考记忆库：念头出现时轻轻留下，日后只凭半句话或大概时间重新找到，并从那里继续想下去。

它不是个人版团队知识库，也不要求先建立文件夹、卡片体系或知识图谱。产品取舍见 [docs/product-principles.md](docs/product-principles.md)。

## 当前能力

- React/Vite 前端、History API 路由与可安装 PWA；
- Dexie / IndexedDB 离线副本，断网时仍可记录、搜索与编辑；
- Go + SQLite 服务端权威存储和跨设备同步；
- Passkey 优先登录，个人访问密钥作为首次设置和恢复入口；
- 随机 ID、revision 冲突保护、软删除与恢复；
- 时间流、星标、多词搜索、来源、续记与 Markdown 正文；
- 多图片粘贴/上传、编辑模式删除与大图查看；
- JSON 完整备份、恢复和 Markdown 导出；
- Docker Compose 单入口部署。

服务端保存权威数据，浏览器保留离线副本和待上传操作。首次用个人访问密钥登录后，可在设置中添加 Passkey；完整设计见 [docs/server-sync.md](docs/server-sync.md)。

## 本地开发

需要 Go 1.26、Node.js 20+ 和 pnpm 11。

先启动服务端：

```bash
mkdir -p data
THOUGHTGLEAN_OWNER_TOKEN='仅供本机开发的长随机字符串' go run ./cmd/thoughtglean
```

再在另一个终端启动前端：

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm dev
```

Vite 默认使用 `http://localhost:5173`，并把 `/api` 代理到 `http://127.0.0.1:8080`。若服务端端口不同，可以设置 `THOUGHTGLEAN_DEV_API_URL`：

```bash
THOUGHTGLEAN_DEV_API_URL=http://127.0.0.1:18080 pnpm dev
```

Go 服务只提供 `/api/*`，不依赖预生成的前端文件。开发时访问 Vite 地址；Docker 部署时由 Nginx 提供前端静态文件并代理 API。

## Docker 部署

首次部署先生成配置：

```bash
cp .env.example .env
openssl rand -hex 32
```

把生成值写入 `.env` 的 `THOUGHTGLEAN_OWNER_TOKEN`，然后运行：

```bash
docker compose up -d --build
docker compose ps
```

默认入口为 `http://127.0.0.1:8080`。`web` 容器提供静态前端并把 `/api/*` 代理到内部 `server`；服务端不会直接暴露到宿主机。

本机默认 Passkey 地址是 `http://localhost:8080`。正式域名部署时，必须把 `.env` 中 `THOUGHTGLEAN_PASSKEY_RP_ID` 改为域名（不带协议和端口），把 `THOUGHTGLEAN_PASSKEY_ORIGIN` 改为浏览器访问的完整 HTTPS Origin，例如 `https://notes.example.com`。Passkey 绑定 Origin，修改域名后需要重新注册。

`.env.example` 默认将 `/data` 绑定到仓库的 `./data`，将自动备份绑定到独立的 `./backups`。若删除对应路径配置，Compose 会使用 `thoughtglean-data` 和 `thoughtglean-backups` 命名卷。容器会匹配挂载目录的 UID/GID，无需使用 `777` 权限。

## 自动备份与恢复演练

服务启动后会立即生成一份完整 ZIP 备份，之后默认每 24 小时生成一次，并保留最近 14 份。每份备份包含记录、修订历史、来源和所有图片；写入采用临时文件、磁盘同步和原子重命名，未完成的备份不会出现在正式文件名中。

可以随时手动生成一份备份：

```bash
docker compose exec server /app/server-entrypoint.sh backup-now
```

输出包含备份路径、记录数、附件数和清单 SHA-256。查看宿主机上的备份：

```bash
ls -lh backups/thoughtglean-auto-*.zip
```

建议定期选择最新备份执行恢复演练：

```bash
docker compose exec server \
  /app/server-entrypoint.sh restore-drill \
  /backups/thoughtglean-auto-YYYYMMDDTHHMMSS.NNNNNNNNNZ.zip
```

演练会在容器临时目录创建全新的 SQLite 数据库和附件库，验证 ZIP、备份清单、修订历史和每张图片的哈希，完成恢复后逐项比较；不会读写正式 `/data`。命令以退出码 `0` 表示成功，其他退出码表示该备份不能可靠恢复。

自动备份主要防止误删除、错误同步和数据库逻辑损坏。若 `data` 与 `backups` 位于同一块磁盘，它不能防止整盘故障；正式使用时应将 `THOUGHTGLEAN_BACKUP_PATH` 指向另一块磁盘，并把备份目录再同步到异地存储。

生产环境必须在入口前配置 HTTPS。若仅在可信局域网临时测试手机访问，可将 `THOUGHTGLEAN_HTTP_BIND` 改为 `0.0.0.0`；但移动浏览器在普通局域网 HTTP 地址上不会开放 Passkey，需使用 HTTPS。长期部署仍应使用 HTTPS 和不可猜测的个人访问密钥。

`docker compose down` 不会删除数据；不要执行 `docker compose down -v`，除非明确要永久删除命名卷中的全部数据。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `THOUGHTGLEAN_ADDR` | `127.0.0.1:8080` | Go 服务监听地址 |
| `THOUGHTGLEAN_DATA_DIR` | `./data` | SQLite 与附件数据目录 |
| `THOUGHTGLEAN_BACKUP_DIR` | `<数据目录>/backups` | 自动备份输出目录；Docker 中为 `/backups` |
| `THOUGHTGLEAN_BACKUP_INTERVAL` | `24h` | 自动备份间隔；设为 `0` 可禁用，其他值至少为 `1m` |
| `THOUGHTGLEAN_BACKUP_KEEP` | `14` | 自动备份保留份数 |
| `THOUGHTGLEAN_OWNER_TOKEN` | 必填 | 至少 32 个字符的单人网页登录密钥 |
| `THOUGHTGLEAN_PASSKEY_RP_ID` | `localhost` | Passkey 依赖方域名，不含协议和端口 |
| `THOUGHTGLEAN_PASSKEY_ORIGIN` | `http://localhost:5173` | 浏览器访问应用的完整 Origin；Docker 默认在 `.env.example` 中设为 `http://localhost:8080` |
| `THOUGHTGLEAN_DEV_API_URL` | `http://127.0.0.1:8080` | Vite 开发代理目标 |

## 验证

```bash
go test ./...
go vet ./...
go build ./cmd/thoughtglean
pnpm check
pnpm build
docker compose config
```
