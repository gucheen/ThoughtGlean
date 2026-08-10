# 拾念 · ThoughtGlean

拾念是一个属于个人的思考记忆库：念头出现时轻轻留下，日后只凭半句话或大概时间重新找到，并从那里继续想下去。

它不是个人版团队知识库，也不要求先建立文件夹、卡片体系或知识图谱。第一版只专注一条可靠链路：

1. 正文之外没有必填项，快速留下一段原话。
2. 服务端确认持久化之前，输入与本地草稿不会清空。
3. 使用中文、中英文混合词语搜索旧记录。
4. 回到当时前后的记录，并从旧念头创建一条续记。
5. 编辑使用 revision 防止多个窗口静默互相覆盖。
6. 删除先进入回收站，可以恢复。

完整取舍见 [docs/product-principles.md](docs/product-principles.md)。

## 当前状态

这是首个可运行切片，包含：

- Go 单进程服务与内嵌 Web UI
- SQLite 持久化、WAL、`FULL` synchronous 和版本快照
- 创建幂等键、编辑冲突保护、软删除与恢复
- 标题与正文的 Unicode NFKC、大小写不敏感、多词 AND 检索
- 时间流、星标、搜索、详情编辑、“回到当时”和续记
- 浏览器本地草稿保护与响应式界面

身份验证尚未进入这一切片。服务默认只监听 `127.0.0.1`，请勿在未加可信反向代理认证的情况下将它直接暴露到公网。

## 本地运行

需要 Go 1.26、CGO 和可用的 C 编译器。

```bash
go run ./cmd/thoughtglean
```

然后打开 <http://127.0.0.1:8080>。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `THOUGHTGLEAN_ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `THOUGHTGLEAN_DATA_DIR` | `./data` | SQLite 数据目录 |

## 验证

```bash
go test ./...
go vet ./...
go build ./cmd/thoughtglean
node --check internal/webui/assets/app.js
```

## Docker

Compose 只把端口发布到宿主机回环地址：

```bash
docker compose up --build
```

数据保存在 `thoughtglean-data` volume 中。

## 数据语义

- SQLite 是当前权威数据源，位置为 `$THOUGHTGLEAN_DATA_DIR/thoughtglean.db`。
- 每次正文、标题或星标修改都会增加 revision，并把该版本写入 `note_revisions`。
- 创建操作可以携带 `requestId`；相同请求重复送达只返回原笔记，同一标识若携带不同内容则返回 HTTP `409`，不会误清新草稿。
- 更新必须携带 `expectedRevision`；版本不符返回 HTTP `409` 和当前服务端版本。
- 删除只写入 `deletedAt`，普通列表和搜索会立即排除，但回收站可以恢复。

Markdown/JSON 导出、可校验备份、Passkey 和可读 Markdown 镜像属于下一阶段的数据可靠性工作，不能以“已有 SQLite 文件”代替验收。
