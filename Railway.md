# Railway 部署指南

推荐直接从 GitHub Fork 部署。仓库根目录已经包含 `Dockerfile` 和 `railway.toml`，不需要先构建并发布 Docker 镜像。

## 1. 推送代码

Railway 只能构建已经推送到 GitHub 的代码。部署前先将当前修改提交到自己的 Fork：

```bash
git add .
git commit -m "feat: support Railway deployment"
git push origin main
```

## 2. 创建 Railway Service

1. 在 Railway 创建一个 Empty Project。
2. 选择 `New -> GitHub Repo`。
3. 选择自己的 grok2api Fork。
4. 不设置 Root Directory。
5. 不覆盖 Build Command 和 Start Command。

Railway 会自动使用仓库根目录的 `Dockerfile` 构建镜像，并读取 `railway.toml` 中的健康检查配置。

首次自动部署可能因为 Volume 和管理员密码尚未配置而失败，这是预期行为。完成后续配置后重新部署即可。

## 3. 添加持久卷

在 grok2api Service 中添加 Volume，挂载路径填写：

```text
/data
```

Volume 用于保存：

```text
/data/config.yaml
/data/media/
```

使用 SQLite 时还会保存：

```text
/data/backend.db
```

如果检测到 Railway 环境但没有 Volume，程序会拒绝启动，避免配置、数据库和加密密钥被写入临时文件系统后在重新部署时丢失。

## 4. 选择数据库

SQLite 和 PostgreSQL 二选一。数据库类型在卷内 `config.yaml` 首次生成时决定。

### SQLite

SQLite 适合个人测试和轻量单实例部署。

不要为 grok2api Service 添加 `DATABASE_URL`。首次启动时会生成：

```yaml
database:
  driver: sqlite
  sqlite:
    path: /data/backend.db
```

### PostgreSQL

PostgreSQL 更适合长期运行和需要独立数据库备份的部署。

1. 在 Railway 项目画布选择 `New -> Database -> PostgreSQL`。
2. 打开 grok2api Service 的 Variables。
3. 添加 Reference Variable：

```text
DATABASE_URL=${{Postgres.DATABASE_URL}}
```

首次启动时会生成：

```yaml
database:
  driver: postgres
  postgres:
    dsn: postgres://...
    maxOpenConns: 50
    maxIdleConns: 10
```

正式部署推荐使用 PostgreSQL。即使使用 PostgreSQL，仍需要 `/data` Volume 保存 `config.yaml` 和本地媒体文件。

## 5. 配置环境变量

必须设置初始管理员密码，值至少 8 个字符：

```text
GROK2API_BOOTSTRAP_ADMIN_PASSWORD=你的强密码
```

可选变量：

```text
GROK2API_BOOTSTRAP_ADMIN_USERNAME=admin
PORT=8000
```

`PORT` 可以不设置，Railway 会自动注入。自行设置后，程序监听地址和健康检查都会使用该端口。

使用 PostgreSQL 时额外设置：

```text
DATABASE_URL=${{Postgres.DATABASE_URL}}
```

普通 Railway 部署不需要设置 `GROK2API_DATABASE_URL`。该变量仅用于显式覆盖通用的 `DATABASE_URL`。

## 6. 配置公网域名

进入 grok2api Service 的 Networking 设置，点击 `Generate Domain`。

程序首次初始化时会读取 Railway 提供的 `RAILWAY_PUBLIC_DOMAIN`，写入公开 API 地址并启用安全 Cookie。建议在首次成功部署前生成域名。

如需显式指定公网地址，可以在首次初始化前设置：

```text
GROK2API_PUBLIC_API_BASE_URL=https://你的域名
```

## 7. 部署

完成 Volume、环境变量、数据库和域名配置后，点击 `Deploy` 或 `Redeploy`。

健康检查路径已经配置为：

```text
/healthz
```

启动成功后，日志会包含类似内容：

```text
server_started listen=0.0.0.0:8000
deployment_topology database=postgres
```

如果选择 SQLite，则数据库日志字段为：

```text
deployment_topology database=sqlite
```

## 8. 首次登录

打开 Railway 生成的公网域名，使用以下信息登录：

```text
用户名：GROK2API_BOOTSTRAP_ADMIN_USERNAME，默认 admin
密码：GROK2API_BOOTSTRAP_ADMIN_PASSWORD 的值
```

首次初始化成功后，可以删除 Railway 中的 `GROK2API_BOOTSTRAP_ADMIN_PASSWORD` 变量。密码和加密密钥已经保存在卷内配置中，后续部署不会重新生成或覆盖该文件。

## 9. 数据库切换

数据库选择规则：

- 首次生成配置时存在 `GROK2API_DATABASE_URL`：使用该 PostgreSQL DSN。
- 否则存在 `DATABASE_URL`：使用该 PostgreSQL DSN。
- 两者都不存在：使用 SQLite。

`config.yaml` 一旦生成，后续以文件中的 `database.driver` 为准。后来添加或删除 `DATABASE_URL` 不会自动切换数据库，也不会迁移已有数据。

如果尚无业务数据，可以挂载一个全新 Volume 重新初始化。已有数据时必须先完成 SQLite 与 PostgreSQL 之间的数据迁移，再修改卷内配置。

## 10. 多副本限制

使用 SQLite 时必须保持一个 Replica。

即使使用 PostgreSQL，当前默认运行态仍是内存存储，媒体仍保存在本地 Volume。多副本部署还需要 Redis 和所有副本可访问的共享媒体存储，因此普通 Railway 部署建议保持一个 Replica。

## 11. 备份

需要备份以下数据：

- `/data/config.yaml`
- `/data/media/`
- SQLite 模式下的 `/data/backend.db`
- PostgreSQL 模式下的数据库备份

不能丢失 `config.yaml` 中的 `credentialEncryptionKey`。只保留数据库而丢失该密钥，会导致已有账号凭据无法解密。

## 12. 常见问题

### 提示 Railway deployment requires a persistent Volume

为 grok2api Service 添加 Volume，并将挂载路径设置为 `/data`，然后重新部署。

### 提示首次初始化需要管理员密码

设置至少 8 个字符的变量：

```text
GROK2API_BOOTSTRAP_ADMIN_PASSWORD=你的强密码
```

### 已添加 DATABASE_URL，但仍然使用 SQLite

说明当前 Volume 中已经存在使用 SQLite 的 `config.yaml`。环境变量不会覆盖已有持久配置。无旧数据时使用全新 Volume；有旧数据时先迁移数据库。

### Railway 健康检查失败

确认以下项目：

- Service 已挂载 `/data` Volume。
- `GROK2API_BOOTSTRAP_ADMIN_PASSWORD` 已在首次部署前设置。
- PostgreSQL 模式下 `DATABASE_URL` 是指向 PostgreSQL Service 的 Reference Variable。
- 没有覆盖镜像的 Start Command。
- `PORT` 是有效端口，或直接让 Railway 自动注入。
