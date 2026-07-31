<p align="center">
  <img src="./assets/logo.svg" width="96" alt="CloudFlareManager logo">
</p>

<h1 align="center">CloudFlareManager</h1>

<p align="center">
  <strong>把多个 Cloudflare 账号的 R2、D1 与 Workers AI，收进一个安静、清晰的私人控制台。</strong>
</p>

<p align="center">
  <img alt="Latest release" src="https://img.shields.io/github/v/release/MengStar-L/CloudFlareManager?style=for-the-badge&label=Release&color=c13b2a">
  <img alt="Linux" src="https://img.shields.io/badge/Linux-systemd-34495e?style=for-the-badge&logo=linux&logoColor=white">
  <img alt="Architecture" src="https://img.shields.io/badge/Arch-amd64%20%7C%20arm64%20%7C%20armv7-2563eb?style=for-the-badge">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26-00ADD8?style=for-the-badge&logo=go&logoColor=white">
  <img alt="React" src="https://img.shields.io/badge/React-19-087EA4?style=for-the-badge&logo=react&logoColor=white">
  <img alt="License" src="https://img.shields.io/badge/License-Apache--2.0-8a6122?style=for-the-badge">
</p>

<p align="center">
  <a href="#快速部署">快速部署</a>
  ·
  <a href="#功能特性">功能特性</a>
  ·
  <a href="#api-token-权限">API Token 权限</a>
  ·
  <a href="#配置说明">配置说明</a>
  ·
  <a href="#软件内更新">软件内更新</a>
  ·
  <a href="#常见问题">常见问题</a>
</p>

---

CloudFlareManager（服务名 `cf-r2-manager`）是一个适合长期运行在私人 Linux 服务器上的 Cloudflare 自托管控制台。它把多个 Cloudflare 账号的 **R2 对象存储**聚合成一个统一的存储阵列，通过标准 **S3 / WebDAV 协议**对外提供服务；同时内置 **D1 数据库工作台**、**Workers AI 网关**、访问密钥管理与完整的任务审计。

前端控制台、后台任务与协议网关都编译在同一个二进制里。服务器上只需要一个 systemd 服务，日常升级直接在网页里点一下完成。

## 功能特性

| R2 存储阵列 | D1 工作台 | Workers AI |
| --- | --- | --- |
| 跨账号统一对象索引、桶用量与免费额度总览、应用内建桶、接管扫描、孤儿清理与再均衡 | SQL 控制台（语法高亮）、表数据浏览、慢查询分析、一键备份到 R2 | Playground、模型目录、用量与 Neuron 估算、请求日志、AI Gateway 管理 |

| 协议前端 | 访问密钥 | 安全与审计 |
| --- | --- | --- |
| S3 SigV4（含预签名 URL、分片上传）与 WebDAV（含锁），可直接对接 rclone、备份工具与网盘挂载 | S3 / WebDAV / AI 三类凭据的签发、轮换、撤销与记录清理 | 主密钥加密存储 Cloudflare 凭据，管理密码 + CSRF 防护，全量审计事件，systemd 沙箱加固 |

> 存储阵列语义：只有被"纳入阵列"的桶才由本程序接管调度；账号里的其他桶仅展示名称、用量与状态，不会被读写。

## 快速部署

开始前请准备：

- 一台使用 systemd 的 Linux 服务器（amd64 / arm64 / armv7）。
- 一个 Cloudflare 账号，以及按下文 [API Token 权限](#api-token-权限) 创建的令牌。
- root 或 sudo 权限。

执行下面的命令：

```bash
curl --proto '=https' --tlsv1.2 -fsSL \
  https://github.com/MengStar-L/CloudFlareManager/releases/latest/download/install-linux.sh \
  -o /tmp/install-cfm.sh && \
sudo sh /tmp/install-cfm.sh
```

安装器会自动识别架构、下载最新稳定版、校验 SHA-256、创建服务用户、注册 systemd 服务，并提示你设置管理员登录密码。完成后打开：

```text
http://<服务器IP>:14325
```

默认目录与端口如下：

```text
/opt/CloudFlareManager/
├── cf-r2-manager     程序本体（自升级时原地替换）
├── config.yaml       配置文件
└── data/             数据库、主密钥与临时文件

14325  管理控制台、S3、WebDAV 与 Workers AI 统一入口
14329  Prometheus 指标（仅本机）
```

无人值守安装可以使用参数：`sudo sh /tmp/install-cfm.sh --port 14325 --password '你的密码' --install-dir /opt/CloudFlareManager`。

### 完成首次初始化

1. 打开 `http://<服务器IP>:14325`，用安装时设置的管理员密码登录。
2. 进入「账号」页添加 Cloudflare 账号（Account ID + API Token，R2 密钥可选）。保存后会自动检测各项能力。
3. 进入「R2 存储 → 物理桶」，从自动列出的真实桶中选择「纳入阵列」，或直接在页面里新建存储桶。
4. 进入「访问密钥」签发 S3 / WebDAV / AI 凭据。三类客户端都连接面板使用的 `14325` 端口；AI Base URL 在地址末尾加 `/v1`。

> 建议参考 [`packaging/nginx`](./packaging/nginx) / [`packaging/caddy`](./packaging/caddy) 为统一入口配置 HTTPS。S3 签名依赖原始 Host，反向代理不得改写 Host。

## API Token 权限

在 Cloudflare 创建自定义 Token 时勾选（用户级或账户级 Token 均支持）：

| 权限（Account 级） | 级别 | 支撑的功能 |
| --- | --- | --- |
| Workers R2 Storage | **Edit** | 桶列表、应用内建桶（只读功能仅需 Read） |
| D1 | **Edit** | 建库 / 删库 / SQL / 备份 / 恢复 |
| Workers AI | **Read + Edit** | 模型目录与推理 |
| AI Gateway | **Read + Edit** | Gateway 管理与日志 |
| Account Analytics | **Read** | 桶用量与免费额度统计 |

R2 的对象读写走 S3 协议，需要另在 **R2 → Manage R2 API Tokens** 创建一对 Access Key（Object Read & Write）。留空时 D1 与 AI 功能不受影响，仅对象操作不可用。

## 配置说明

配置文件默认位于 `/opt/CloudFlareManager/config.yaml`，改动后 `sudo systemctl restart cf-r2-manager` 生效。常用项：

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `listeners.http` | `0.0.0.0:14325` | 面板、S3、WebDAV 与 AI 的统一监听地址 |
| `data_dir` | `/opt/CloudFlareManager/data` | 数据库、主密钥与临时文件 |
| `r2.storage_soft_limit_bytes` | `9000000000` | 阵列写入软限额（默认 9 GB，贴合免费层） |
| `r2.account_storage_soft_limit_bytes` | `9000000000` | 单个 Cloudflare 账号的总存储软限额（纳管桶、未纳管桶与预留之和） |
| `r2.upload_chunk_bytes` | `67108864` | 服务端强制分片块大小：大文件上传的本地磁盘峰值仅为一块（小磁盘可调小，最小 5MiB） |
| `r2.logical_bucket` | `storage` | S3/WebDAV 对外暴露的逻辑桶名 |

完整字段见 [`docs/configuration.md`](./docs/configuration.md)。

从旧版本升级时，未配置 `listeners.http` 会自动继承原 `listeners.admin`。原 `listeners.s3`、`listeners.webdav` 和 `listeners.ai` 不再启动，现有客户端需把 endpoint 改为面板地址；WebDAV 使用根地址，AI 使用 `<面板地址>/v1`。

## 软件内更新

概览页的「软件更新」面板会检查本仓库的最新 GitHub Release。发现新版本时，管理员可以查看更新说明并点击「更新并重启」：

1. 下载当前平台的二进制并校验 SHA-256（不匹配立即中止，不动原程序）；
2. 原地替换 `/opt/CloudFlareManager/cf-r2-manager`（旧版本保留为 `.old`，可手动回滚）;
3. 自动重启服务，页面等待就绪后自动刷新。

发布新版本只需推送一个 `v*` 标签，GitHub Actions 会自动构建三个架构的产物并发布 Release。

## 备份

需要备份的只有 `/opt/CloudFlareManager/data` 目录（SQLite 数据库 + 主密钥）与 `config.yaml`。也可以使用内置命令做数据库快照：

```bash
sudo -u cf-r2-manager cf-r2-manager backup \
  --config /opt/CloudFlareManager/config.yaml \
  --output /opt/CloudFlareManager/data/backups/manager-$(date +%F).db
```

## 本地开发

需要 Go 1.26+、Node.js 22+。

```bash
make web    # 构建前端（嵌入二进制）
make test   # 全量测试
make build  # 产出 bin/cf-r2-manager

bin/cf-r2-manager init --config ./config.example.yaml
bin/cf-r2-manager server --config ./config.example.yaml
```

前端热更新开发：`npm run dev --prefix web`（代理目标可用 `web/.env.local` 中的 `CF_R2_MANAGER_BACKEND` 覆盖）。

## 常见问题

<details>
<summary><strong>添加账号后能力检测显示 Unauthorized？</strong></summary>

多数是 Token 类型或权限问题。本程序同时支持用户级与账户级 API Token；若个别能力标红，对照 <a href="#api-token-权限">权限表</a> 补齐后，在「账号」页点 🛡「重新检测能力」即可，无需删除重加。
</details>

<details>
<summary><strong>「纳入阵列」会动我桶里的数据吗？</strong></summary>

不会写入或删除。纳入阵列 = 本地登记 + 接管扫描（把已有对象读入索引）。「移出阵列」也只解除本地登记，Cloudflare 中的桶与对象原样保留。真正的对象写入只发生在你通过 S3/WebDAV 接口主动操作时。
</details>

<details>
<summary><strong>自动更新失败了怎么办？</strong></summary>

校验不通过或下载失败时原程序不受影响。若新版本启动异常，程序目录里保留着上一版：<code>mv /opt/CloudFlareManager/cf-r2-manager.old /opt/CloudFlareManager/cf-r2-manager && sudo systemctl restart cf-r2-manager</code>。
</details>

<details>
<summary><strong>可以安装到其他目录或端口吗？</strong></summary>

可以。安装时使用 <code>--install-dir</code> 与 <code>--port</code> 参数；已安装的实例直接修改 <code>config.yaml</code> 中的监听地址后重启服务。
</details>

<details>
<summary><strong>Cloudflare 凭据如何保存？</strong></summary>

API Token 与 R2 密钥使用服务器上的 32 字节主密钥（<code>data/master.key</code>，权限 0600）加密后写入 SQLite。请妥善备份主密钥——丢失后凭据无法解密，需要重新录入。
</details>

## 更多文档

- [完整配置字段](./docs/configuration.md)
- [HTTP API](./docs/api.md)
- [运维、备份与恢复](./docs/operations.md)
- [安全模型](./docs/security.md)
- [最新稳定版本](https://github.com/MengStar-L/CloudFlareManager/releases/latest)

---

<p align="center">
  <sub>Apache-2.0 · 让每个 Cloudflare 账号各司其职，让所有资源在一个地方安静地就位。</sub>
</p>
