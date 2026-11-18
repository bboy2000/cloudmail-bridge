# cloudmail-bridge

把 [cloud-mail](https://github.com/maillab/cloud-mail) 网页邮箱桥接成标准 IMAP / SMTP 服务的网关。部署后可用任意邮件客户端（Outlook、Thunderbird、Apple 邮件、手机自带邮箱等）收发 cloud-mail 里的邮件。

## 特性

- 支持邮箱域名下的**所有账号**（含以后新增的），无需逐个配置；
- 账号密码由 cloud-mail 网站验证，改密码后客户端旧密码立即失效；
- 客户端与 cloud-mail 邮箱同步收信、发信、已读状态；
- 只有客户端在线时才同步，关闭客户端即停止拉取，节省云端数据库额度；
- 支持公网直连（隐式 TLS）与内网 + DDNS 两种部署方式。

## 快速开始

完整安装指南见 **[docs/INSTALL-v1.05.md](docs/INSTALL-v1.05.md)**（含 token 生成、证书申请、compose 配置、DDNS、客户端配置、FAQ）。

简述：

1. 生成 cloud-mail 的 public token（命令见指南第二节）；
2. 准备证书（可用附带的交互式脚本 `tools/cert.sh`）；
3. 编写 `docker-compose.yml`，填 token、域名、端口映射，用环境变量配置；
4. `docker compose up -d` 启动，然后在邮件客户端里手动添加账户。

## 客户端配置

| 项目 | 值 |
| --- | --- |
| IMAP / SMTP 服务器 | 你的网关域名 |
| 加密 | SSL/TLS（隐式 TLS） |
| 端口 | docker-compose 里映射的外部端口 |
| 用户名 | 完整邮箱地址 |
| 密码 | 该邮箱在 cloud-mail 网站的登录密码 |

## 配置方式

所有配置项既可通过 `docker-compose.yml` 的 `environment` 传入（推荐，无需 config.json），也可用 `config.json` 提供；环境变量优先。字段说明见指南第六节。参考模板：[config.example.json](config.example.json)。

## 镜像

Docker Hub：`<你的DockerHub用户名>/cloudmail-bridge:latest`，支持 amd64 / arm64。

## 许可

MIT
