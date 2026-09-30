# 拾念 · ThoughtGlean

个人随手记：随手记下此刻的想法，需要时再找回来。

## 功能

- 快速记录、自动保存草稿、Markdown 编辑、续写和多图片粘贴上传。
- 按创建时间浏览日常记录，支持星标、多词搜索、图片和时间筛选。
- 用空间聚合相关记录，支持批量加入、空间内置顶、从日常收起和归档。
- 接收分享文本，保存为记录或追加到草稿；支持安装为 PWA。
- 离线记录与跨设备同步，保留编辑冲突，删除后可从回收站恢复。
- 个人访问密钥和 Passkey 登录，支持完整备份、恢复与 Markdown 导出。

## 部署

需要 Docker 和 Docker Compose。在仓库目录执行：

```bash
cp .env.example .env
openssl rand -hex 32
```

将生成值填入 `.env` 的 `THOUGHTGLEAN_OWNER_TOKEN`，再启动：

```bash
docker compose pull
docker compose up -d
docker compose ps
```

打开 **http://localhost:8080**，使用该访问密钥登录。默认数据保存在 `./data`，自动备份保存在 `./backups`。

域名与 HTTPS、配置、更新和备份操作见[部署与维护](docs/deployment.md)。

## 使用

1. 在首页输入正文，点击「保存」；标题和空间均可选，未提交内容会自动保存为本机草稿。
2. 在「日常」浏览记录，用星标或搜索找回内容；详情页可编辑、续写、添加图片。
3. 需要按场景集中使用时创建空间。一条记录可加入多个空间，移出空间不会删除原文。
4. 空间收起或归档后，其中记录会移出日常，仍可在「全部记录」和「星标」中找回。
5. 在「设置」中添加 Passkey、查看同步状态、打开回收站、备份或导出数据。

离线使用、冲突处理和恢复注意事项见[同步与数据管理](docs/server-sync.md)。

## 本地运行

需要 Go 1.26、Node.js 22 和 pnpm 11。在两个终端分别运行：

```bash
THOUGHTGLEAN_OWNER_TOKEN='替换为至少32个字符的随机密钥' go run ./cmd/thoughtglean
```

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm dev
```

访问 `http://localhost:5173`。前端默认代理 API 到 `http://127.0.0.1:8080`，可用 `THOUGHTGLEAN_DEV_API_URL` 更改目标。
