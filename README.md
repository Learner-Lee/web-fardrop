# FarDrop

FarDrop 通过个人博客分享临时大文件。上传者用管理令牌登录，每个文件可生成多个独立的限时链接。接收者打开链接即可下载。

- 入口是 `https://web.alenli.cn/about/fardrop/`，About 作品卡片也链接到此处。
- VPS 的 Nginx 将请求交给监听 `127.0.0.1:18171` 的 Go Edge，关闭请求体与下载响应的代理缓冲。
- Ubuntu 上的 Go Origin 监听 `127.0.0.1:18172`，在 `/storage/Project/web-fardrop/data` 保存 SQLite 和文件。Origin 经博客现有的 HTTPS 入口主动建立 WebSocket 长连接，VPS 只用这条连接访问 FarDrop 存储服务。连接和存储请求都校验服务令牌。
- 单文件上限是 5 GiB，新上传按 1 MiB 分块并校验 SHA-256。重新选择同一文件会接着传未确认的分块。
- 分享默认有效 24 小时，可随时撤销。新文件在上传完成 7 天后自动删除，分享有效期最多到文件的删除时间。管理页可提前永久删除已上传文件，删除后所有分享链接失效。
- VPS 初始数据预算为 512 MiB 热点缓存和 128 MiB 在途缓冲。Go Edge 以 4 MiB 为单位缓存，在允许的预算内合并同块请求并预读下一块。

## 管理令牌

将 `ORIGIN_HOST` 替换为本地服务器的 SSH 地址。读取管理令牌后，将其粘贴到 FarDrop 管理页。分享给接收者的地址使用单独生成的下载令牌。

```sh
ssh root@ORIGIN_HOST 'sed -n "s/^FARDROP_ADMIN_TOKEN=//p" /etc/web-fardrop/origin.env'
```

## 构建与运行

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o fardrop .
```

Origin 和 Edge 分别由 `web-fardrop-origin.service` 和 `web-fardrop-edge.service` 管理。Origin 会自动重连。部署文件位于 `deploy/`。博客路由和 About 数据修改前的副本位于 VPS 的 `/storage/blog/web-fardrop/backups`。
