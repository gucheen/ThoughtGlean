# 部署与维护

首次启动步骤见 [README](../README.md#部署)。Compose 使用 GHCR 的 `thoughtglean-server` 和 `thoughtglean-web` 镜像，当前发布平台为 `linux/amd64`。

## 域名、HTTPS 与 OIDC

生产环境通过 Caddy、Nginx 或 Cloudflare Tunnel 提供 HTTPS，并在身份服务中创建 confidential OIDC client，启用 Authorization Code 和 PKCE S256。配置示例：

```env
THOUGHTGLEAN_OIDC_ISSUER=https://identity.example.com/realms/personal
THOUGHTGLEAN_OIDC_CLIENT_ID=thoughtglean
THOUGHTGLEAN_OIDC_CLIENT_SECRET=身份服务生成的客户端密钥
THOUGHTGLEAN_OIDC_REDIRECT_URL=https://notes.example.com/api/auth/oidc/callback
THOUGHTGLEAN_OIDC_OWNER_SUBJECT=你的账号在此客户端中的sub
```

Issuer 填写身份服务发布的 issuer 地址，服务端会读取其 `/.well-known/openid-configuration`。回调地址必须与身份服务登记的地址完全一致，路径固定为 `/api/auth/oidc/callback`。本地 Docker 使用 `http://localhost:8080/api/auth/oidc/callback`，Vite 开发使用 `http://localhost:5173/api/auth/oidc/callback`。除回环地址外，Issuer 和回调地址必须使用 HTTPS。

`OWNER_SUBJECT` 是该账号 ID Token 的 `sub` claim，请从身份服务的账号信息或客户端 Token 预览中取得；它不一定等于用户名、邮箱或管理后台的用户 ID。只有这一账号可以访问原有笔记，其他账号会被拒绝。更改 issuer、client ID 或 owner subject 会使现有 OIDC 会话失效。

五项 OIDC 配置必须一起填写。配置缺失或 discovery 失败时，服务端会拒绝启动；启动和登录时服务端必须能够访问身份服务。不使用 OIDC 时，将这五项全部留空并配置个人访问密钥。密钥可由 `openssl rand -hex 32` 生成；启用 OIDC 时密钥可以留空，也可以保留为备用入口。

从 Passkey 版本升级后，记录和图片保留，原 Passkey 登录及管理入口移除，旧 Passkey 会话不再接受，需要重新登录。浏览器仍保留本机离线数据；首次在线登录成功后恢复同步。退出登录会撤销拾念当前设备的 OIDC 会话，身份服务中的会话由身份服务管理。

## 配置

以下变量在 `.env` 中设置，修改后运行 `docker compose up -d`。

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `THOUGHTGLEAN_OWNER_TOKEN` | 空 | 可选备用密钥，至少 32 个字符；未配置 OIDC 时必填 |
| `THOUGHTGLEAN_HTTP_BIND` | `127.0.0.1` | 宿主机监听地址；需直接对外监听时改为 `0.0.0.0` |
| `THOUGHTGLEAN_HTTP_PORT` | `8080` | 宿主机端口 |
| `THOUGHTGLEAN_OIDC_ISSUER` | 空 | 身份服务 issuer 地址 |
| `THOUGHTGLEAN_OIDC_CLIENT_ID` | 空 | OIDC 客户端 ID |
| `THOUGHTGLEAN_OIDC_CLIENT_SECRET` | 空 | OIDC 客户端密钥，仅存于服务端 |
| `THOUGHTGLEAN_OIDC_REDIRECT_URL` | 空 | 完整回调地址，路径为 `/api/auth/oidc/callback` |
| `THOUGHTGLEAN_OIDC_OWNER_SUBJECT` | 空 | 唯一允许访问笔记的账号的 `sub` |
| `THOUGHTGLEAN_DATA_PATH` | `./data` | 数据挂载目录 |
| `THOUGHTGLEAN_BACKUP_PATH` | `./backups` | 备份挂载目录，建议使用另一块磁盘并同步到异地 |
| `THOUGHTGLEAN_BACKUP_INTERVAL` | `24h` | 备份间隔，至少 `1m`；`0` 禁用 |
| `THOUGHTGLEAN_BACKUP_KEEP` | `14` | 保留的备份份数 |

目录默认值来自 `.env.example`。路径留空时改用 Docker 命名卷；绑定目录时，数据与备份目录应由同一 UID 持有，无需设置 `777` 权限。数据和备份包含明文内容，应限制访问。

直接运行 Go 服务时，可设置 `THOUGHTGLEAN_ADDR`（默认 `127.0.0.1:8080`）、`THOUGHTGLEAN_DATA_DIR`（默认 `./data`）和 `THOUGHTGLEAN_BACKUP_DIR`（默认 `<数据目录>/backups`）。OIDC 回调地址需按浏览器访问的前端地址配置。

## 更新与停止

更新前先生成备份，再拉取镜像并重建容器：

```bash
docker compose exec server /app/server-entrypoint.sh backup-now
docker compose pull
docker compose up -d
docker compose ps
```

启动异常时用 `docker compose logs --tail=100 server web` 查看日志。`docker compose down` 停止服务并保留数据；`down -v` 会删除命名卷中的数据。

## 自动备份与恢复演练

默认启动时备份一次，之后每 24 小时备份，保留最近 14 份。ZIP 包含记录、历史、关联关系和图片，默认位于宿主机 `./backups/thoughtglean-auto-*.zip`。手动备份使用上面的 `backup-now` 命令。

选择实际备份文件进行恢复演练：

```bash
docker compose exec server /app/server-entrypoint.sh restore-drill /backups/thoughtglean-auto-YYYYMMDDTHHMMSS.NNNNNNNNNZ.zip
```

退出码 `0` 表示校验及恢复比较通过。演练在临时目录进行，不会恢复或改写正式数据。网页 JSON 备份的恢复入口见[同步与数据管理](server-sync.md#备份与恢复)。

## 从源码构建镜像

Compose 没有配置构建步骤。需要部署本地修改时，先在仓库目录构建两个目标，再启动：

```bash
docker build --target server -t ghcr.io/gucheen/thoughtglean-server:latest .
docker build --target web -t ghcr.io/gucheen/thoughtglean-web:latest .
docker compose up -d --pull never
```

服务端构建可用 `--build-arg THOUGHTGLEAN_VERSION=<版本号>` 指定显示版本。仓库工作流在 `main`、`v*` 标签推送或手动触发时发布镜像；固定版本可修改 `compose.yml` 中两个镜像的标签。
