# 部署与维护

首次启动步骤见 [README](../README.md#部署)。Compose 使用 GHCR 的 `thoughtglean-server` 和 `thoughtglean-web` 镜像，当前发布平台为 `linux/amd64`。

## 域名与 HTTPS

默认仅监听宿主机 `127.0.0.1:8080`。正式部署时，用反向代理提供 HTTPS，并在 `.env` 设置实际访问域名：

```dotenv
THOUGHTGLEAN_PASSKEY_RP_ID=notes.example.com
THOUGHTGLEAN_PASSKEY_ORIGIN=https://notes.example.com
```

RP ID 不含协议或端口，Origin 包含协议及非默认端口。修改域名后需重新注册 Passkey；本机使用默认地址 `http://localhost:8080`。手机通过局域网普通 HTTP 访问时无法使用 Passkey。

## 配置

以下变量在 `.env` 中设置，修改后运行 `docker compose up -d`。

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `THOUGHTGLEAN_OWNER_TOKEN` | 必填 | 至少 32 个字符的登录密钥，妥善保存供首次登录和恢复访问 |
| `THOUGHTGLEAN_HTTP_BIND` | `127.0.0.1` | 宿主机监听地址；需直接对外监听时改为 `0.0.0.0` |
| `THOUGHTGLEAN_HTTP_PORT` | `8080` | 宿主机端口 |
| `THOUGHTGLEAN_PASSKEY_RP_ID` | `localhost` | Passkey 域名 |
| `THOUGHTGLEAN_PASSKEY_ORIGIN` | `http://localhost:8080` | 浏览器访问的完整 Origin |
| `THOUGHTGLEAN_DATA_PATH` | `./data` | 数据挂载目录 |
| `THOUGHTGLEAN_BACKUP_PATH` | `./backups` | 备份挂载目录，建议使用另一块磁盘并同步到异地 |
| `THOUGHTGLEAN_BACKUP_INTERVAL` | `24h` | 备份间隔，至少 `1m`；`0` 禁用 |
| `THOUGHTGLEAN_BACKUP_KEEP` | `14` | 保留的备份份数 |

目录默认值来自 `.env.example`。路径留空时改用 Docker 命名卷；绑定目录时，数据与备份目录应由同一 UID 持有，无需设置 `777` 权限。数据和备份包含明文内容，应限制访问。

直接运行 Go 服务时，可设置 `THOUGHTGLEAN_ADDR`（默认 `127.0.0.1:8080`）、`THOUGHTGLEAN_DATA_DIR`（默认 `./data`）和 `THOUGHTGLEAN_BACKUP_DIR`（默认 `<数据目录>/backups`）。Passkey Origin 默认是本地前端地址 `http://localhost:5173`。

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
