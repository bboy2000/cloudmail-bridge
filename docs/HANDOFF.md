# cloudmail-bridge 开发交接文档

**文档版本：v1.02**
**编写日期：2026-11-24**
**对应代码提交：`6424333`**

---

## 0. 这份文档给谁看

给「换到新电脑后继续开发这个项目的人」（也可能是接手的新 AI 会话）。

目标：不读历史聊天记录，只看这份文档 + 仓库代码，就能接着干。

配套阅读顺序：本文档 → `README.md`（面向使用者的安装指南）→ 代码注释。

---

## 1. 项目是什么

**cloudmail-bridge** 是一个 IMAP/SMTP 网关，把 [cloud-mail](https://github.com/maillab/cloud-mail)（一个基于 Cloudflare Workers + D1 的自建邮件服务，只有 Web 界面和 HTTP API）包装成标准邮件协议服务。

```
邮件客户端(Outlook/网易邮箱大师/Apple Mail)
        │  IMAPS 993 / SMTPS 465
        ▼
  cloudmail-bridge  ←── 本项目（Go）
        │  HTTPS + JWT
        ▼
   cloud-mail 实例（Web/API，数据在 D1）
```

解决的问题：cloud-mail 本身不提供 IMAP/SMTP，只能开网页收发。网关让任意邮件客户端正常收发，同时**尽量少消耗 D1 读取额度**（这是核心约束，见 §5.1）。

- 语言：Go 1.27.4，单 module、单 main 包（约 4100 行）
- 协议库：`go-imap/v2`（服务端）、`go-smtp`、`go-sasl`
- 分发：Docker 多阶段构建 + GitHub Actions 多架构镜像
- 许可证：MIT（含 LICENSE 与致谢）

---

## 2. 现状快照（2026-11-24）

| 项目 | 状态 |
|---|---|
| 代码 | 本地 `master` 分支，工作区干净，`go vet` 无告警，`go test` 通过 |
| 远端 | `github.com/bboy2000/cloudmail-bridge`，默认分支 `main` |
| 同步状态 | **已同步**，本地 `master` 与远端 `main` 均为 `d0d51e0`。代码内容自 `1da951b`（README v1.14）起未变；`6424333`、`d0d51e0` 两次提交仅改动本交接文档（v1.00→v1.01） |
| 生产实例 | NAS 上运行中（Docker，host 网络，993/465），域名 `cm.hou.sh.cn` |
| 已废弃实例 | 曾在一台 VPS 上部署 `kk.hou.sh.cn` 做验证，**已用 `tools/uninstall.sh` 彻底删除**（容器/镜像/目录/证书全清） |
| CI | `.github/workflows/docker-publish.yml`，push 到 `main` 或打 `v*` tag 时构建 amd64+arm64 推 Docker Hub |

### 接手第一件事

从远端 clone 即为最新代码：

```bash
git clone https://github.com/bboy2000/cloudmail-bridge.git
```

或者拿迁移包（含未入库的 `config.json`、`deploy/` 与 `.git` 历史）解压后直接开始。

推送时注意分支名不一致：**本地是 `master`，远端是 `main`**。直接 `git push` 会失败，必须写完整的 `origin master:main`，或者先建跟踪关系：

```bash
git push origin master:main
```

> 如果新电脑网络到 GitHub 不稳（本机曾长期 `CONNECT tunnel failed, response 502`，靠反复重试才成功），只能多试几次；`api.github.com` 通常比 `github.com` 稳定，可用 `curl -s https://api.github.com/repos/bboy2000/cloudmail-bridge/commits/main` 核实远端状态。

---

## 3. 环境与构建

### 3.1 工具链

| 项 | 值 |
|---|---|
| Go | **1.27.4**（`go.mod` 已钉死版本，低版本会拒绝构建） |
| 依赖 | `go-imap/v2 v2.0.0-beta.9`、`go-smtp v0.26.0`、`go-sasl`、`go-message`（间接） |
| 平台 | 开发机 Windows + Git Bash；产物跑 Linux |

新电脑需要装 Go 并配置 `GOROOT` / `GOPATH`，然后 `go mod download` 拉依赖（本机的模块缓存不会跟着迁移包走）。

### 3.2 常用命令

```bash
# 编译当前平台（验证代码能否通过）
go build -o /tmp/cmb.exe .

# 交叉编译 Linux（实际部署用的就是这个）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o cloudmail-bridge-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o cloudmail-bridge-linux-arm64 .

# 静态检查 + 测试
go vet .
go test .            # 只测根包：criteria_test.go + store_test.go
```

测试目录（`smtptest/`、`readtest/` 等）各有 `package main`，是**独立的手工验收小工具**，不是 `_test.go`，不要用 `go test ./...` 去跑（会各自编译成程序）。都是 `go run ./<目录>` 用法。

### 3.3 镜像

- `Dockerfile`（根目录）：多阶段构建，`golang:1.27-alpine` 编译 → `alpine:3.20` 运行。
- `deploy/Dockerfile`：**单阶段**，直接把现成二进制 `COPY` 进 alpine。用于「本机交叉编译 + 上传二进制」的手工部署路径（见 `deploy/upload.py`）。
- 两条路径并存，改代码时注意别只更新一条。

---

## 4. 代码地图

| 文件 | 行数 | 职责 |
|---|---:|---|
| `main.go` | 866 | IMAP 服务端主干：TLS 配置、连接处理、各类命令（SELECT/FETCH/SEARCH/STORE/STATUS/EXPUNGE/IDLE 等）的实现与分发 |
| `store.go` | 770 | 内存邮箱状态：消息列表、标志位、`\Deleted` 集合落盘、未读计数、已读回写 |
| `send.go` | 504 | 发信链路：构造 MIME、回复/转发、附件编码，调 cloud-mail 发信 API |
| `smtp.go` | 414 | SMTP 服务端（隐式 TLS 465）：AUTH、MAIL/RCPT/DATA |
| `cloudmail.go` | 307 | cloud-mail HTTP API 客户端：登录、邮件列表、已读、删除、发信 |
| `fetchfix.go` | 302 | **FETCH 的 `BODY[]` section 正确切片** + `BODYSTRUCTURE` 构造（踩过大坑，见 §5.3） |
| `auth.go` | 297 | 认证与账号管理：邮箱密码→网站校验→JWT 缓存；**增量探测**逻辑也在这里 |
| `mail.go` | 241 | 把 cloud-mail 的 `emailItem` 组装成 RFC822 原文（正文刻意用 8bit 明文） |
| `config.go` | 136 | 配置结构、默认值、环境变量覆盖（环境变量优先于 config.json） |
| `attach.go` | 112 | 附件下载：按官方 `cvtR2Url(att.key)` 规则拼 URL 并取字节 |
| `criteria_test.go` | 101 | SEARCH 条件匹配单测（12 个用例） |
| `store_test.go` | 82 | 存储标志位 / 未读计数单测 |

辅助目录：

| 目录 | 用途 |
|---|---|
| `deploy/` | 生产部署材料：`docker-compose.yml`、`Dockerfile`、`config.json`、`nas.py`（NAS 操作）、`upload.py`（上传二进制）、`ddns.sh`、`upload_ddns.py` |
| `tools/cert.sh` | 证书一键申请（交互式选 DNS 服务商，acme.sh 签发 + 安装 + 续期） |
| `tools/uninstall.sh` | 一键卸载（识别本项目容器/镜像/证书后清理，支持 `--dry-run`、`-y`、`--keep-data`） |
| `docs/` | 历史版安装文档 `INSTALL-v1.00/01/02.md`（已归档，最新文档是根目录 `README.md`） |
| `smtptest/` `readtest/` `idletest/` `multiaccounttest/` `tlscheck/` `testclient/` | 阶段性验收小工具，各含 `main.go`，用 `go run` 单个执行 |

---

## 5. 关键设计决策（改动前必读）

这一节记录的都是「踩过坑才定下来的规矩」，不了解背景就改会翻车。

### 5.1 增量探测：一切为了省 D1 读取额度

**背景**：cloud-mail 跑在 Cloudflare D1 上，读取按行计费。早期实现每 `poll_interval`（默认 60 秒）就把每个账号的邮件列表 `size=50&full=1` 全量翻页拉一遍，40 封邮件的账号每小时约 2400 行，额度烧得很快。用户明确要求"别太浪费 D1 额度"。

**做法**（`auth.go`）：
- 每轮先发一个 `size=1&full=0` 的轻量探测，只取最新 `emailId`。
- 未变化 → 跳过该账号全量拉取（1 行）。
- 有变化 → 正常全量同步。
- 连续跳过 `probe_force_full`（默认 5）轮 → 强制全量一次，用于校正「网页端改了已读、但探测发现不了」的情况。
- 探测失败 → 回退全量（宁可多花额度也不能不同步）。
- `force=true`（用户主动点"收取历史邮件"）永不跳过。

线上实测跳过/全量约 4:1，读取量降约 80%。

**改这块要注意**：`ProbeEnabled` / `ProbeForceFull` 两个配置项的注释里写清了语义，`0` 表示关闭探测、恢复旧的全量行为。别把「探测」逻辑挪到全量路径里去，会白烧额度。

### 5.2 已读状态只允许单向回写（未读 → 已读）

cloud-mail 只有 `PUT /api/email/read`（body `{emailIds:[...]}`），**没有标记为未读的接口**。

早期实现双向同步，导致客户端打开邮件后刷新又变回未读（用户报的 bug）。现在的规则：

- 客户端 STORE `\Seen` → 调 API 通知服务端已读。
- `applyUnread` 改成**单向**：只允许未读→已读，用 `keptRead` 计数保留本地已读状态。
- 不要试图实现"标为未读"，服务端没有这个能力。

### 5.3 FETCH 必须按 section 正确切片（网易邮箱大师空白事件）

早期 `Fetch` 不管客户端请求哪个 section 都返回整封 raw。松散的客户端能忍，**网易邮箱大师**这种严格客户端在 `BODY.PEEK[HEADER]` 里收到"头+正文"后协议层校验失败，**静默丢弃**——表现是能连、能拉 66 封、界面全空白。

修复（`fetchfix.go`）：
- 按 `HEADER` / `HEADER.FIELDS` / `TEXT` / `MIME` / `BODY[n]` 等正确切片。
- 补上 `BODYSTRUCTURE`（此前完全没实现）。

同类坑：`go-imap v2 beta.9` 在客户端请求 BODYSTRUCTURE 时要求结构体 `Extended` 字段**非 nil**，否则 panic 断连。构造 singlePart/multiPart 时都必须给一个空 `Extended`。

**另一个必须记住的 RFC 点**：`BODY[n]` 的标准语义就是返回「part 头 + 正文」，不要"优化"成只返回正文。编码信息靠 `BODYSTRUCTURE` 的 `Encoding` 字段传递——曾漏写该字段，默认 7bit，导致客户端把 base64 原文当文本显示成一屏乱码。

### 5.4 正文一律用 8bit 明文，不要 base64

即使正确声明了 `Encoding=base64`，网易邮箱大师渲染 `multipart/alternative` 时仍会把编码串露出来变成乱码。最终方案：`mail.go` 组装的正文用 8bit 明文。

### 5.5 只有 `\Seen` 和 `\Deleted` 两个标志

`Select` 的 `Flags` / `PermanentFlags` **只声明这两个**。此前声明了 `\Answered` `\Flagged` `\Draft` 但 Store 不处理，用户点星标毫无反应，属于「声明了做不到的能力」。

**星标故意不实现**：cloud-mail 的星标在独立的 `star` 表，`emailItem` 结构里根本没有该字段，要实现得额外调 `/star/list`，与省 D1 额度的目标冲突。

### 5.6 附件地址不走 API，按官方源码的 `cvtR2Url` 规则拼

附件下载在官方 Web 端是 `<a :href="cvtR2Url(att.key)" download>` 直链，不是 API。

`cvtR2Url(key)` 规则（`mail-vue/src/utils/convert.js`）：
- key 以 `http(s)://` 开头 → 原样返回
- 站点没配 `r2Domain` → 返回 `/<key>`（走本站 `/attachments` 代理）
- 配了 → `https://<r2Domain>/<key>`

对应配置项 `attachment_base_url`：留空则用 `base_url + "/" + att.key`；站点配了 R2/S3 自定义域名就填那个域名（不带尾斜杠）。

**注意**：API 文档的邮件列表示例里附件对象只列了 `attId/filename/mimeType/size`，实际还有个 `att.key` 字段——文档没写，但代码要用。

### 5.7 已删除邮件要落盘

客户端删掉的邮件 ID 集合持久化到 `./data`（容器里 `/app/data`）。不挂这个卷的话，容器重建后已删邮件会被重新拉回来（客户端看到"删了又回来"）。compose 里 `./data:/app/data` 这一行不能删。

---

## 6. 配置项要点

完整字段见 `config.go` 和 `config.example.json`，这里只讲容易搞错的。

| 配置项 | 说明 |
|---|---|
| `base_url` | cloud-mail 站点根地址（如 `https://hou.sh.cn`），**不是网关自己的域名** |
| `domain` | 邮箱域名，只用于生成 `Message-ID`。README 里强调过：别填成网关域名 |
| `admin_email` + `admin_password` | 推荐。网关自动登录续 token |
| `public_token` | 与上面二选一。写死的 token，不自动续期 |
| `jwt_cache_ttl` | JWT 缓存小时数（默认 12） |
| `tls_mode` | `implicit`（993/465 隐式 TLS）/ `starttls` / `plain` |
| `probe_enabled` | 默认 `true`。`false` = 每轮全量，烧额度 |
| `probe_force_full` | 默认 5。连续跳过几轮后强制全量校正 |
| `attachment_base_url` | 见 §5.6 |
| `users` / `login_password` | **旧架构遗留字段，现在运行时忽略**，别被误导 |

配置来源优先级：**环境变量 > config.json > 内置默认值**（`config.go:applyEnv`）。

---

## 7. 测试与验收

### 7.1 单测

```bash
go test .        # criteria_test.go（SEARCH 条件 12 例）+ store_test.go（标志位）
```

### 7.2 手工验收工具

都是独立 main 包，`go run ./<目录>` 执行，参数各自看源码顶部注释：

| 目录 | 验证什么 |
|---|---|
| `testclient` | 用真 IMAP 协议连网关登录，确认能读到真实邮件（阶段2 验收） |
| `smtptest` | 用真 SMTP 客户端经网关发信，验证发信链路（阶段3） |
| `readtest` | 对指定 UID 发 `STORE \Seen`，验证已读回写（阶段4a） |
| `idletest` | 连上发 IDLE，等服务器推 EXISTS（阶段4c，配 `-inject-after`） |
| `multiaccounttest` | 多账号并发 |
| `tlscheck` | 隐式 TLS 握手 + 协议 greeting |

### 7.3 真实客户端回归清单

改完协议层代码，至少用**网易邮箱大师**过一遍（这是最严格的客户端，历史上两个 bug 都是它先暴露的）：

1. 收件夹能看到邮件（不是空白）
2. 正文不乱码（没有 base64 串、没有 `>>z{Sʗ` 这类残留）
3. 打开邮件后刷新，不会变回未读
4. 附件能下载、文件名正确
5. 发信、回复、转发正常（含附件）

---

## 8. 部署现状

### 8.1 NAS 生产实例

- 目录：`/vol1/1000/Docker/Cloud-mail-bridge`
- 域名：`cm.hou.sh.cn`（DDNS 由 `ddns-go` 维护；注意还有一套老的 `ddns.sh` 定时器在跑，IP 变化可能把记录名改回 `mail.hou.sh.cn`，**这是个未处理的隐患**）
- 容器：`cloudmail-bridge`，`network_mode: host`，直接监听 NAS 的 993/465
- 证书：acme.sh 签发（注意默认 CA 已变 ZeroSSL 会失败，**必须加 `--server letsencrypt`**），安装到 `./certs`，续期后 `reloadcmd` 重启容器
- 挂载：`./config.json`（只读）、`./certs`（只读）、`./data`（可写）

### 8.2 已删除的 VPS 实例

曾在 `kk.hou.sh.cn` 部署过一套用于验证。已用 `tools/uninstall.sh` 清干净：容器、镜像、安装目录、证书和 acme 续期配置全部删除，993/465 端口已释放。

⚠️ 该域名的 **DNS A 记录可能还指着旧 VPS IP**，如未清理需手工在域名服务商处删除（脚本无权改 DNS）。

同机曾有无关项目证书 `dedi.sh021.dpdns.org`，卸载时已确认未受影响——`uninstall.sh` 靠 acme 配置里的 `Le_RealCertPath` / `Le_RealKeyPath` / `Le_RealFullChainPath` 三个字段识别本项目证书，避免误删他人证书。

---

## 9. 已知限制与待办

| 项 | 说明 |
|---|---|
| **`deploy/` 整个目录未入库** | `.gitignore` 里写了 `deploy/`，导致 `git ls-files deploy/` 为空 —— 仓库里**没有** `docker-compose.yml` 和 `deploy/Dockerfile`。别人 clone 下来只能照 README 手抄 compose。compose 与 deploy/Dockerfile 本身不含密钥，建议后续把它们从忽略规则里放出来（`config.json` 仍必须忽略）。 |
| 星标不支持 | 见 §5.5，设计取舍 |
| 标记未读不支持 | 服务端无接口，见 §5.2 |
| 老的 `ddns.sh` 定时器 | 与新 `ddns-go` 可能打架，待全部测通后清理 |
| 附件走 KV 时的性能 | 默认存储是 KV，大附件会慢；可切 R2/S3（官方文档 `system/object-storage.html`，R2 绑定名 `r2`） |
| `docs/INSTALL-v1.00~1.02.md` | 历史归档，内容已过时，改动请只改 `README.md` |

---

## 10. 项目纪律（来自真实教训，别破）

1. **先读官方资料再动手**。cloud-mail 的 API 文档：<https://docs.skymail.ink/api/api-doc.html>，源码：<https://github.com/maillab/cloud-mail>。排障时按文档定义的字段和接口判断，**禁止凭空猜测接口结构**（曾因臆测附件 API 被用户纠正）。
2. **Authorization 头不加 `Bearer` 前缀**，直接填令牌。
3. **邮件列表要 `full=1` 才返回 HTML 正文和 `attList`**。
4. **不要动别人的东西**。NAS 上还有其他服务，VPS 上有无关证书。`uninstall.sh` 之类破坏性脚本只能在明确的测试机上验证——**不许拿生产 NAS 做实验**。
5. **省额度是硬指标**。任何增加 cloud-mail API 请求量的改动，先算每轮读取行数。
6. **敏感文件绝不入库**。`config.json` / `deploy/config.json` / `*.pem` / `*.key` / `certs/` / `deploy/` / 各 test 目录都在 `.gitignore` 里，配置里含明文 `admin_password`、`public_token`、`jwt_secret`、`login_password`。
7. **文档每次修改必须升版本号**（README 当前 v1.14，本文档 v1.02）。

---

## 11. 版本沿革

| README 版本 | 对应提交 | 内容 |
|---|---|---|
| v1.00~v1.08 | 早期 | 初版、附件收发、JWT 架构、多域名、收件标准化 |
| v1.09 | `fd6dc7c` | 修正部署命令 5 处错误（cert.sh 地址、`--ecc`、镜像标签、CNAME 方向、reloadcmd 容错） |
| v1.10 | `9d79e68` | 补 cloud-mail 官方链接 |
| v1.11 | `abe15a4` | 加 MIT LICENSE 与致谢章节 |
| v1.12~v1.13 | `3141f31` 起 | 一键卸载脚本 + 卸载章节 |
| v1.14 | `1da951b` | 精简致谢（去掉 go-sasl / go-message / hydroxide 三条） |

本交接文档版本沿革：

| 文档版本 | 对应提交 | 内容 |
|---|---|---|
| v1.00 | `6424333` | 初版 |
| v1.01 | `d0d51e0` | 修正同步状态：由「未推送 `1da951b`」改为「已同步」；接手第一件事改为 clone/解压迁移包 |
| v1.02 | （本地待提交） | 修正 §10 版本号残留（v1.00→v1.02）、§2 同步状态表述（区分 HEAD `d0d51e0` 与文档基线 `6424333`） |

代码层面同期修复（不在 README 版本里体现）：
- `1f5d570` 打开邮件后刷新又变回未读
- `e61626f` SEARCH 条件补全（`UNSEEN` 等此前完全没处理）
- `d3d0e04` 标志声明收敛、STATUS 补未读数、支持覆盖式 STORE
- `baa1146` 增量探测（省 D1 额度，见 §5.1）
- `50f637d` / `3c2c6d4` 卸载脚本证书识别与目录清理

---

## 12. 新电脑接手检查表

- [ ] 装 Go 1.27.4，配好 `GOROOT` / `GOPATH`
- [ ] `go mod download` 拉依赖
- [ ] `go build .` 能过、`go vet .` 无告警、`go test .` 通过
- [ ] 恢复 `config.json`（迁移包里有，含真实凭据，注意别外泄）与 `deploy/config.json`
- [ ] `git log` 能看到 `6424333`（交接文档）与 `1da951b`（README v1.14）
- [ ] 确认远端已同步（`curl -s https://api.github.com/repos/bboy2000/cloudmail-bridge/commits/main`）
- [ ] 确认磁盘上没有遗留的旧编译产物干扰（`*.exe`、`cloudmail-bridge-linux-*`）
- [ ] 想连生产实例调试时，先确认 NAS 上的容器状态，别直接重启生产
