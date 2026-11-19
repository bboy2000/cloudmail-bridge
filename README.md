# cloudmail-bridge 安装指南 v1.12

一个把 [cloud-mail](https://github.com/maillab/cloud-mail) 网页邮箱桥接成标准 IMAP/SMTP 服务的网关。部署后可用任意邮件客户端（Outlook、Thunderbird、Apple 邮件、手机自带邮箱等）收发 cloud-mail 里的邮件。

> 相关链接：[cloud-mail 源码仓库](https://github.com/maillab/cloud-mail) · [cloud-mail 官方文档](https://docs.skymail.ink)（部署 cloud-mail 站点请看这里，本仓库只负责 IMAP/SMTP 网关）

- 支持 cloud-mail 站点里的**所有账号**（含以后新增的），无需在网关里逐个配置；
- 账号密码由 cloud-mail 网站实时验证，改密码后客户端旧密码立即失效；
- 客户端与 cloud-mail 邮箱同步收信、发信、已读状态，支持附件收发、图片显示；
- 只有客户端在线时才同步，关闭客户端即停止拉取，节省云端数据库额度；
- **客户端删除邮件只影响客户端，不会删除 cloud-mail 服务器上的邮件**；想找回时在客户端点"收取历史邮件 / 下载全部邮件"即可重新拉回；
- **网关不需要 public token、也不需要存管理员密码**——每个客户端用它自己的邮箱账号密码登录即可。

> 本指南中的域名、邮箱、Token 全部用占位符表示，请替换成你自己的值。示例域名统一写作 `mail.example.com`（邮箱域名 `example.com`）。示例端口为 `9933` / `4653`，请按实际情况修改。

---

## 目录

- [一、准备工作](#一准备工作)
- [二、工作原理与账号准备](#二工作原理与账号准备)
- [三、部署方案A：VPS / 固定公网服务器（推荐）](#三部署方案avps--固定公网服务器推荐)
- [四、部署方案B：内网动态 IP 环境](#四部署方案b内网动态-ip-环境)
- [五、客户端配置（两方案通用）](#五客户端配置两方案通用)
- [六、多邮箱域名配置](#六多邮箱域名配置)
- [七、配置项说明](#七配置项说明)
- [八、客户端删除与重新下载](#八客户端删除与重新下载)
- [九、常见问题](#九常见问题)
- [十、安全提醒](#十安全提醒)
- [十一、卸载](#十一卸载)

---

## 一、准备工作

两种方案都要先做下面这些，本节只写一次，后面不再重复。

### 1. 一台运行 Docker 的机器

- 部署方案A：有 VPS 或固定公网服务器的用户，机器上已安装 Docker 和 Docker Compose；
- 部署方案B：一台内网机器（NAS / 小主机），已安装 Docker，且处于动态公网 IP 网络。

### 2. 一个 cloud-mail 实例

你需要有一个自己搭建好的 [cloud-mail](https://github.com/maillab/cloud-mail) 站点（它的根地址就是下面 compose 里的 `BASE_URL`，例如 `https://mail.example.com`），以及在网站上建好的邮箱账号（邮箱地址 + 密码）。客户端用哪个邮箱登录，就是哪个账号——网关不需要管理员账号。

> 还没有 cloud-mail 站点？先按官方文档部署：<https://docs.skymail.ink> （源码：<https://github.com/maillab/cloud-mail>）。本仓库只是 IMAP/SMTP 网关，不包含 cloud-mail 本体。

### 3. 创建安装目录

```bash
mkdir -p cloudmail-bridge && cd cloudmail-bridge
```

后续所有命令都在这个目录里执行。两个方案都会用到 `docker-compose.yml`；方案A 还会用到 `certs/`（证书）。

---

## 二、工作原理与账号准备

### 网关怎么收发邮件

网关本身**不保存任何邮箱密码、也不需要 public token**。它的工作方式是：

1. 客户端（Outlook / 手机邮箱等）用自己的邮箱地址 + 密码登录网关；
2. 网关把这个密码交给 cloud-mail 网站验证（调 `/api/login`），密码错就拒绝登录——所以网站上改了密码，客户端旧密码立刻失效；
3. 验证通过后，网关拿该账号的登录态，调用 cloud-mail 的标准接口收发邮件、附件、已读状态；
4. 网关自动发现这个登录账号名下的所有邮箱（`/api/account/list`），全部同步到客户端。

### 账号准备

- 在 cloud-mail 网站里创建邮箱账号（或确认已有账号），记下**完整邮箱地址**和**网站登录密码**；
- 客户端配置时，"用户名"填这个完整邮箱地址，"密码"填该邮箱在 cloud-mail 网站的登录密码；
- 一个账号可以有多个邮箱地址（在网站"邮箱地址"里添加），登录后都会被自动同步，无需在网关里登记；
- 多域名的特殊情况见[第六节](#六多邮箱域名配置)。

---

## 三、部署方案A：VPS / 固定公网服务器（推荐）

适用：有 VPS 或固定公网服务器的用户。

### 第一步：域名解析与证书

1. 准备域名（例如 `mail.example.com`），A 记录指向你的服务器公网 IP。若用 Cloudflare，代理状态必须设为**"仅 DNS"（灰云）**——IMAP/SMTP 不能走 CDN 代理。

2. **提前准备好这几样**，然后运行一键脚本，全程按提示输入即可，不用改脚本内容：
   - 你的域名（如 `mail.example.com`）；
   - 该域名解析服务商的 API 凭证（域名托管在哪家就用哪家的，见下方"DNS 服务商对照表"）；
   - 服务器能访问互联网。

   下载脚本并执行：

   ```bash
   curl -fsSL -o cert.sh \
     https://raw.githubusercontent.com/bboy2000/cloudmail-bridge/main/tools/cert.sh
   bash cert.sh
   ```

   脚本会依次询问：证书目录 → 选 DNS 服务商（1 Cloudflare / 2 腾讯云 DNSPod / 3 阿里云 / 4 腾讯云）→ 输入 API 凭证 → 输入域名 → 续期后要重启的容器名。确认后它会自动完成：安装 acme.sh（国内走 Gitee 镜像）→ 申请 ECC 证书 → 安装到 `./certs/fullchain.pem` 和 `./certs/key.pem` → 配置自动续期，续期后自动重启容器。

   > 证书目录直接回车即用当前目录下的 `certs/`，与下面 compose 里的 `./certs:/app/certs:ro` 对应，保持默认即可。
   >
   > 首次部署时容器还没创建，脚本最后那步"重启容器"会因容器不存在而被跳过，属正常现象，不影响证书；容器起来后自动读取证书。

3. 常用管理命令（直接拷贝粘贴）：

   ```bash
   # 查看已签发的证书及到期时间
   ~/.acme.sh/acme.sh --list

   # 查看自动续期任务（应看到 acme.sh --cron 那一行）
   crontab -l 2>/dev/null | grep acme.sh

   # 测试续期是否正常（只检查，不实际执行）
   # 注意 --ecc：脚本签发的是 ECC 证书，不加这个参数会报找不到证书
   ~/.acme.sh/acme.sh --renew -d "mail.example.com" --ecc --test
   ```

#### DNS 服务商对照表（通俗说明）

acme.sh 申请证书时，需要证明"这个域名是你的"。最省事的方式是让脚本直接在你的 DNS 服务商那里加一条临时解析记录来验证——这就要求给它一个能管理 DNS 的 API 凭证。你**不用管技术细节**，只要按下面找到凭证，运行时告诉脚本你在哪家、把凭证填进去就行：

| 你的域名解析托管在 | 脚本里选 | 凭证去哪里拿 | 需要填什么 |
| --- | --- | --- | --- |
| **Cloudflare** | `1` | 登录 `dash.cloudflare.com` → 头像 → My Profile → API Tokens → Create Token，选 `Edit zone DNS` 模板，Zone Resources 选你的域名 | `CF API Token` + `Zone ID`（域名 Overview 页右侧 API 区域） |
| **DNSPod** | `2` | 登录 `console.dnspod.cn` → 账号 → API 密钥 → 创建密钥 | `API ID` + `API Token` |
| **阿里云** | `3` | 登录 `ram.console.aliyun.com` → 用户 → 新建用户并授予 `AliyunDNSFullAccess` 权限 → 生成 AccessKey | `AccessKey ID` + `AccessKey Secret` |
| **腾讯云** | `4` | 登录 `console.cloud.tencent.com/cam/capi` → 创建密钥，并确保有 DNSPod 读写权限 | `SecretId` + `SecretKey` |

### 第二步：编写 docker-compose.yml

新建 `docker-compose.yml`，把站点地址、域名、端口映射写在里面：

```yaml
services:
  cloudmail-bridge:
    image: <你的DockerHub用户名>/cloudmail-bridge:latest
    container_name: cloudmail-bridge
    restart: unless-stopped
    ports:
      - "9933:993"   # 外部端口 : 容器内 IMAPS
      - "4653:465"   # 外部端口 : 容器内 SMTPS
    environment:
      TZ: Asia/Shanghai
      BASE_URL: "https://mail.example.com"     # 你的 cloud-mail 站点根地址
      DOMAIN: "example.com"                    # 主邮箱域名（见下方说明）
      TLS_MODE: "implicit"
      CERT_FILE: "/app/certs/fullchain.pem"
      KEY_FILE: "/app/certs/key.pem"
      POLL_INTERVAL: "60"
      SYNC_INTERVAL: "2"
    volumes:
      - ./certs:/app/certs:ro
      - ./data:/app/data          # 记录客户端已删除的邮件，必须持久化，否则容器重启后会重新下载
```

说明：

- `ports` 里的 `9933:993`：冒号左边 `9933` 是**外部端口**（客户端连服务器时填这个），冒号右边 `993` 是**容器内 IMAP 服务端口**（固定不用改）；`4653:465` 同理，`465` 是容器内 SMTP 服务端口。外部端口可改成任意未占用端口，但要保证客户端和防火墙填的是同一个外部端口。本示例端口为占位示例，请按实际修改。
- 服务器的**安全组 / 防火墙**要放行上面填的外部 IMAP 端口（9933）和外部 SMTP 端口（4653），否则客户端连不上。
- `DOMAIN` 只用于生成邮件的 Message-ID 后缀和服务标识，**不限制能登录哪些邮箱**——任意账号都能登录，多域名也不用改这里（详见[第六节](#六多邮箱域名配置)）。
- 这个 compose 里**没有 token、也没有管理员密码**，因为收发邮件用的是每个客户端自己的邮箱账号密码。
- `volumes` 里的 `./data:/app/data` 不要删：网关在里面记录"客户端已经删除的邮件"，容器重启后靠它避免把删掉的邮件又下载回来。

### 第三步：启动与更新

```bash
docker compose pull
docker compose up -d
docker compose logs -f
```

看到 IMAP/SMTP 监听且无报错即成功。更新时重跑 `docker compose pull && docker compose up -d`。

然后跳到[第五节](#五客户端配置两方案通用)配置邮件客户端。

---

## 四、部署方案B：内网动态 IP 环境

适用：机器在家里/公司内网、公网 IP 会变动。这种环境下没有固定公网 IP，也没有现成证书，需要自己解决动态域名和加密——网关本身不处理这些，但可以按下面做。

**做方案B前，前面的"准备工作"、"工作原理与账号准备"、"compose 写法与启动更新"照做**，本节只写差异部分。

### 1. 动态域名（DDNS）

公网 IP 会变，需要一个动态域名始终指向你家/公司的出口 IP。

- 推荐用 **ddns-go** 安装到内网机器上，由它自动检测 IP 变化并更新域名解析。
- ddns-go 的安装与配置教程见其 GitHub 仓库：**https://github.com/jeessy2/ddns-go**（README 里有各 NAS / 系统的详细安装步骤，这里不重复）。

### 2. 证书

方案B 的域名由 ddns-go 动态更新，同样用 DNS API 验证（前提是域名托管在有 API 的服务商，比如 Cloudflare / DNSPod / 阿里云 / 腾讯云），**直接复用第三节的 `cert.sh` 一键脚本**即可——把"选 DNS 服务商"那一步按你的托管商选，其余完全一样，证书同样装到 `./certs/`。

只有一种情况需要额外操作：你的域名托管商**不在**上面的对照表里（没有 DNS API）。这时用 DNS 别名（CNAME）方式委托验证。以"要签 `dynamic.example.com`、但只有 `mail.example.com` 的 DNS 能加记录"为例：

```bash
# 1) 在 mail.example.com 的 DNS 里加一条 CNAME（指向 mail.example.com 本身）
#    _acme-challenge.dynamic.example.com  ->  mail.example.com
#    注意：左边是【要签的域名】，别写反。

# 2) 用别名模式申请（--domain-alias 填能加记录的那个域名）
export CF_Token="你的主域名服务商Token"
export CF_Zone_ID="你的主域名ZoneID"
~/.acme.sh/acme.sh --issue --dns dns_cf \
  --domain-alias "mail.example.com" \
  -d "dynamic.example.com" --keylength ec-256

# 3) 安装证书到 ./certs（与方案A 相同）
~/.acme.sh/acme.sh --install-cert -d "dynamic.example.com" --ecc \
  --key-file "$(pwd)/certs/key.pem" \
  --fullchain-file "$(pwd)/certs/fullchain.pem" \
  --reloadcmd "docker restart cloudmail-bridge 2>/dev/null || true"
```

DNS API 验证不要求开放 80 端口，内网环境也能签发。

> `--domain-alias` 与 `--challenge-alias` 作用相同，区别只在 CNAME 的写法：用 `--domain-alias mail.example.com` 时 CNAME 指向 `mail.example.com`；用 `--challenge-alias mail.example.com` 时 CNAME 要写成 `_acme-challenge.mail.example.com`。
>
> 这条 CNAME 记录**签发后不要删**，续期时还要用。

### 3. compose 差异

方案B 沿用方案A 的 `docker-compose.yml`，按下面改：

- 证书部分（`TLS_MODE`、`CERT_FILE`、`KEY_FILE`、`volumes`）**保留**，不要用明文——外网访问必须加密；
- `ports` 改成你路由器要转发的那组端口，例如：

  ```yaml
    ports:
      - "1143:993"   # NAS 对外端口 : 容器内 IMAPS 端口（容器端口 993 固定）
      - "1025:465"   # NAS 对外端口 : 容器内 SMTPS 端口（容器端口 465 固定）
  ```

### 4. 端口转发与客户端端口（三层端口，务必分清）

内网部署时，一个请求要经过三层，每层端口数字不同，千万别弄混。以 IMAP 为例（`1143:993` 是 NAS 上 docker 的写法：**1143 是 NAS 对外端口，993 是容器内端口**）：

| 层次 | IMAP | SMTP |
| --- | --- | --- |
| ① 路由器公网端口（客户端填这个） | `10112` | `10125` |
| ② NAS 对外端口 | `1143` | `1025` |
| ③ 容器内端口 | `993` | `465` |

数据流向：客户端 → ① 路由器公网端口 → ② NAS 对外端口 → ③ 容器内端口 → 网关。

**路由器**上配置两条转发规则（内部 IP 填 NAS 的内网 IP）：

| 协议 | 外部端口（公网） | 内部 IP | 内部端口（NAS 对外端口） |
| --- | --- | --- | --- |
| IMAP | `10112` | NAS 的内网 IP | `1143` |
| SMTP | `10125` | NAS 的内网 IP | `1025` |

三处对应关系（记牢这三点就不会错）：

- **客户端**：IMAP 填 `10112`、SMTP 填 `10125`（都是路由器公网端口）；
- **路由器**：`10112` → NAS 的 `1143`、`10125` → NAS 的 `1025`；
- **docker-compose.yml**：`ports` 写 `1143:993`、`1025:465`（左边是 NAS 对外端口，右边是容器内端口，固定不动）。

举例：客户端填 `mail.example.com:10112` → 请求到路由器公网 IP 的 `10112` 端口 → 路由器转发到 NAS 的 `1143` 端口 → docker 再转进容器的 `993` 端口 → 网关处理。发信同理：`10125` → NAS `1025` → 容器 `465`。

注意：客户端/路由器打交道的端口（10112、10125）和 NAS 上的端口（1143、1025）是两回事——1143 只是"NAS 对外端口"，不是公网端口。端口数字都可自行更换，但上述三层要同步对应。

### 5. 其它外网访问方式（自行选择）

上面是"动态域名 + 端口转发"这条路。如果你不想在路由器上暴露端口，也可以换成下面任一种，网关配置不用动：

- **Tailscale / WireGuard 组网**：设备装客户端连回内网，按内网地址访问，无需公网 IP 和端口转发；
- **IPv6 直连**：若有公网 IPv6，用 AAAA 记录指向网关机器，绕过 IPv4 动态 IP；
- **云服务器中转 / 反向代理**：内网机器不直接暴露，由有固定 IP 的 VPS 转发流量（四层转发，不能用 HTTP 反向代理代理 IMAP/SMTP）。

只要客户端最终能连到网关的 IMAP/SMTP 端口即可，选哪种由你决定。

---

## 五、客户端配置（两方案通用）

在邮件客户端里"手动添加账户"，按下面填：

| 项目 | 部署方案A（VPS） | 部署方案B（内网动态 IP） |
| --- | --- | --- |
| IMAP 服务器 | 你的域名，如 `mail.example.com` | DDNS 域名，如 `mail.example.com` |
| IMAP 端口 | 服务器防火墙放行、客户端填的外部端口，如 `9933` | 客户端填**路由器公网端口**，如 `10112`（见第 4 节①） |
| IMAP 加密 | SSL/TLS | SSL/TLS |
| SMTP 服务器 | 同 IMAP 服务器 | 同 IMAP 服务器 |
| SMTP 端口 | 如 `4653` | 客户端填**路由器公网端口**，如 `10125`（见第 4 节①） |
| SMTP 加密 | SSL/TLS | SSL/TLS |
| 用户名 | 完整邮箱地址，如 `name@example.com` | 同左 |
| 密码 | 该邮箱在 cloud-mail 网站的登录密码 | 同左 |

说明：

- 支持 cloud-mail 站点里的**任意账号**，用户名填完整邮箱地址、密码填该邮箱在网站的登录密码即可，不需要在网关里预先登记；
- 邮箱是什么域名后缀都能登录（`@example.com`、`@other.com` 等都可以），前提是这些域名已在 cloud-mail 站点里配好，详见[第六节](#六多邮箱域名配置)；
- 收件箱、发件箱（Sent）、已读状态、附件都会与网页版双向同步；
- 支持 IMAP IDLE 推送的客户端（Outlook、Thunderbird、系统自带邮件）可秒级收信；不支持 IDLE 的客户端靠自身轮询，会有 1-2 分钟延迟，这是客户端行为，与网关无关。

---

## 六、多邮箱域名配置

网关支持同一站点下的多邮箱域名（例如 `@example.com`、`@mail2.example`、`@other.com` 同时存在），`docker-compose.yml` 不用为每个域名单独配置，`DOMAIN` 填一个主域名即可，各域名的邮箱都能正常登录收发。域名的添加要在 cloud-mail 站点侧完成。

---

## 七、配置项说明

配置通过 compose 的 `environment` 传入（不依赖单独的 config.json）。全部环境变量如下：

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `BASE_URL` | cloud-mail 站点根地址 | 必填 |
| `DOMAIN` | 主邮箱域名，用于生成 Message-ID 和服务标识；不限制可登录的邮箱域名 | 必填 |
| `IMAP_ADDR` | IMAP 监听地址 | `0.0.0.0:993` |
| `SMTP_ADDR` | SMTP 监听地址 | `0.0.0.0:465` |
| `TLS_MODE` | `implicit` 隐式 TLS / `starttls` / `plain` 明文 | `plain` |
| `CERT_FILE` / `KEY_FILE` | 证书链 / 私钥路径（`implicit`、`starttls` 用） | 空 |
| `INSECURE_AUTH` | 明文模式下是否允许认证（内网用 `true`） | `true` |
| `POLL_INTERVAL` | 后台轮询间隔（秒），有客户端在线时才拉取 | `60` |
| `SYNC_INTERVAL` | 同一邮箱两次同步的最小间隔（分钟） | `2` |
| `PAGE_SIZE` | 每次拉取的邮件条数 | `100` |
| `MAX_PAGES` | 最多拉取页数，`0` 表示全量 | `0` |
| `JWT_CACHE_TTL` | 网站验证后 JWT 的有效小时数 | `12` |
| `PROBE_ENABLED` | 增量探测：每轮先用 1 条请求检查有没有新邮件，没变就跳过全量拉取。**强烈建议开启**，能把云端读取量降到原来的 1/5 以下 | `true` |
| `PROBE_FORCE_FULL` | 连续跳过多少轮后强制全量一次，用于校正网页版改动的已读状态。数值越大越省额度，但已读同步越慢 | `5` |
| `ATTACHMENT_BASE_URL` | 附件下载地址前缀；留空 = 用 `BASE_URL` + `/` + 附件的 key（本站代理路径）。若站点把附件放在独立对象存储域名，填那个域名 | 空 |
| `TZ` | 时区，建议 `Asia/Shanghai` | 容器默认 |

> 旧版本的 `PUBLIC_TOKEN` / `ADMIN_EMAIL` / `ADMIN_PASSWORD` 已不再必需：网关改用每个客户端自己的账号密码登录。若你从旧版升级，可删掉这几项。

关于 `POLL_INTERVAL` 与 `SYNC_INTERVAL`：为节省云端免费额度，网关只在有客户端连接时同步。轮询周期不宜设得过短（建议 ≥ 30 秒），否则可能触发云端数据库的免费读取上限，导致登录接口报错。

关于 `PROBE_ENABLED`：开启后每轮同步先用一条 `size=1` 的轻量请求读取最新邮件 ID，若与上轮相同则跳过该账号的全量拉取。实测可把每轮读取量从"全部邮件"降到 1 行，降幅约 80%。首次同步（建立基线）和 `PROBE_FORCE_FULL` 到点时仍会全量，用于同步网页版改动的已读状态。

> **关于邮件标志**：网关只支持 `\Seen`（已读）和 `\Deleted`（删除）两种标志，这两种都会与网页版双向同步。`\Flagged`（星标）、`\Answered`、`\Draft` 不支持——cloud-mail 的星标是独立数据表，读取它要额外请求，会多耗云端额度，因此没有实现。客户端若显示星标按钮，点击后不会保存（网关不会声明支持该标志，多数客户端会自动隐藏）。

> 关于 cloud-mail 站点后台的 **Webhook / 回调**设置：那是把收到的邮件推送给外部系统（如自建服务、Bot）用的，与本网关无关。网关是通过 cloud-mail 的标准 API 主动拉取邮件的，不需要开启 Webhook，开了也不会影响网关。不用的功能建议关闭，少一分暴露面。

---

## 八、客户端删除与重新下载

这一节说明在客户端里删邮件会发生什么，以及怎么把删掉的邮件找回来。

### 删除只影响客户端

在客户端里删除邮件，**不会删除 cloud-mail 服务器上的邮件**：

- 客户端上不再显示，之后同步也不会再下载回来；
- 网页版里邮件**仍然存在**，照常可以看、可以搜；
- 换句话说，客户端的"删除"相当于把邮件从客户端视图里隐藏，是一层本地过滤。

网关把这些"已删除"的邮件 ID 记在 `data/deleted-ids.json` 里（由 compose 的 `./data:/app/data` 挂载到宿主机），所以容器重启后也不会把删掉的邮件重新拉回来。

### 想找回：在客户端点"收取历史邮件"

把邮件找回来不需要重启网关、也不需要动服务器，只要在客户端里**重新下载全部邮件**即可。各客户端的叫法不同：

| 客户端 | 操作 |
| --- | --- |
| 网易邮箱大师 | 设置里的「收取历史邮件」 |
| Outlook | 文件夹右键 → 重新同步 / 更新文件夹 |
| Thunderbird | 右键文件夹 → 属性 → 修复文件夹（Repair Folder） |
| Apple 邮件 | 邮箱菜单 → 同步全部账户 |

原理：客户端"下载全部邮件"时，实际会向网关发一条覆盖全部邮件的正文拉取请求（IMAP `UID FETCH 1:* (BODY[])`）。网关识别到这种请求，就清空该账号的已删记录、重新从服务器拉一遍，被删的邮件自然就回来了。

只拉邮件列表、只更新已读状态的常规同步**不会**触发这个行为——所以删掉的邮件不会自己冒出来。

### 注意

- 恢复的是**该账号**的已删记录，不影响其他账号；
- 恢复后如果又删了，同样只是客户端侧删除，可再次用同样方法找回；
- 服务器上的邮件从头到尾没被动过，网页版始终能看到全部邮件。

---

## 九、常见问题

**Q：客户端提示"密码错误"，但我没改密码？**
A：网关的认证是实时调用 cloud-mail 网站完成的。网站若因数据库超限、服务异常等原因返回错误（如 Cloudflare 1102 资源超限），网关也会表现为认证/发信失败。请先看网关日志（`docker compose logs --tail=50`）里的具体报错，不要盲目改密码；这类站点临时故障通常稍后重试即恢复。

**Q：还需要生成 public token 吗？**
A：不需要。从本版本起网关改用标准接口 + 每个账号自己的登录密码，不再依赖 public token。旧配置里如果还留着 `PUBLIC_TOKEN`，可以删掉。

**Q：我有多个邮箱域名（@a.com、@b.com），要怎么配？**
A：网关不用改，`DOMAIN` 填一个主域名即可，各域名的邮箱都能登录收发。多域名要配在 **cloud-mail 站点**的 `wrangler.toml` 的 `domain` 数组里，并在 Cloudflare Email Routing 添加对应路由。详见[第六节](#六多邮箱域名配置)。

**Q：能收到但发不出去？**
A：检查 SMTP 端口和加密方式是否与配置一致；确认该邮箱在 cloud-mail 网站能正常发信。发信走 cloud-mail 原有的发信通道，两边都会留痕。

**Q：附件看不到 / 发出去的附件别人收不到？**
A：收附件需要站点能正常访问对象存储直链（默认走 `BASE_URL/attachments/...`，可用 `ATTACHMENT_BASE_URL` 改地址）；发附件网关已按标准接口上传，若对方收到的附件名乱码，升级到本版本已修复（RFC2047 文件名解码）。

**Q：在客户端删了邮件，刷新后怎么又回来了？**
A：本版本已修复。删除的邮件 ID 记在 `./data/deleted-ids.json`，容器重启后也会保留。请确认 compose 里有 `./data:/app/data` 这行挂载，否则容器重建后记录会丢、邮件会重新下载。详见[第八节](#八客户端删除与重新下载)。

**Q：客户端删了邮件，网页版怎么还在？**
A：这是设计如此。客户端的删除只在客户端生效，不会删除 cloud-mail 服务器上的邮件。想找回就在客户端点"收取历史邮件"[第八节](#八客户端删除与重新下载)。

**Q：新邮件要等一两分钟才出现？**
A：先看客户端是否支持 IDLE 推送。不支持的客户端靠自身轮询，属正常现象。网关侧从邮件到达到进入收件箱通常几秒内完成。

**Q：邮件时间差 8 小时？**
A：早期版本有过时区问题，请使用本版本或更新版本。

---

## 十、安全提醒

1. 公网部署务必用 TLS（`TLS_MODE=implicit` + 有效证书），不要在公网用 `plain`；
2. 客户端密码就是邮箱在 cloud-mail 的网站密码，请使用强密码；
3. 网关不保存邮箱密码：每次登录都交给网站验证，仅在内存里临时持有该会话的登录态；
4. 若compose 里仍保留了 `ADMIN_PASSWORD` 等凭据，请把 compose 文件当作凭据对待，权限设为仅属主可读（`chmod 600 docker-compose.yml`），不要提交到公开仓库或发给他人；新版本无需这些字段；
5. 定期更新镜像（`docker compose pull && docker compose up -d`）以获取修复。

---

## 十一、卸载

### 一键卸载（推荐）

在安装目录里执行：

```bash
curl -fsSL -o uninstall.sh \
  "https://raw.githubusercontent.com/bboy2000/cloudmail-bridge/main/tools/uninstall.sh?t=$(date +%s)"
bash uninstall.sh --dry-run   # 先演练，只看会做什么，不动任何东西
bash uninstall.sh             # 确认无误后实际卸载
```

脚本会依次完成：

1. 停止并删除容器
2. 删除镜像
3. 注销本项目的证书续期（**只动指向本安装目录的证书**，不会误删其他项目的）
4. 删除安装目录

可选参数：

| 参数 | 作用 |
| --- | --- |
| `--dry-run` | 只打印将执行的操作，不做任何改动。建议先跑一次 |
| `--keep-data` | 保留 `data/`（客户端已删邮件记录），其余照删 |
| `-y` / `--yes` | 跳过交互确认 |

安全措施：

- 实际执行前必须手工输入 `yes` 才继续；
- 证书清理是**按路径匹配**的——只有续期配置指向本安装目录的证书才会被注销。如果机器上还有别的域名证书在用，脚本会保留 acme.sh 的定时任务；
- 目录删不掉时（`data/` 由容器以 root 写入）会明确报错并以非零退出，不会谎报成功。

### 卸载不会影响什么

- **cloud-mail 服务器上的邮件一封都不会动**，网页版照常收发；
- 其他 Docker 容器、镜像、其他域名的证书都不受影响。

### 还需要你手动处理

脚本没有权限改这些，卸载后请自行检查：

- 路由器端口转发规则（IMAP / SMTP）
- 云服务器安全组放行规则
- DNS 解析记录（域名不再使用时）

### 手动卸载（不想用脚本）

```bash
cd 你的安装目录
docker compose down                    # 停并删容器
docker rmi cloud-mail-bridge-cloudmail-bridge:latest   # 删镜像

# 注销证书续期（域名换成你自己的；不加会每天 cron 报错）
~/.acme.sh/acme.sh --remove -d mail.example.com --ecc
crontab -l | grep -v acme.sh | crontab -   # 仅当没有其他证书时

sudo rm -rf 你的安装目录                # data/ 是 root 写的，需要 sudo
```

---

## 镜像版本

- `latest`：始终指向最新稳定版（compose 示例默认用这个）；
- 目前只提供 `latest` 一个标签，没有按版本号发布的固定标签。

如果你需要锁定某个版本（避免 `pull` 后自动升级），可以先给仓库打一个 `v*` 形式的 Git tag（例如 `v1.08`），CI 会自动构建并推送同名镜像标签，之后就能在 compose 里写 `:v1.08`：

```bash
git tag v1.08 && git push origin v1.08
```

Dockerfile 与镜像内均带有版本号（`org.opencontainers.image.version`），可用 `docker inspect` 查看。

---

## 致谢

本项目的实现离不开以下开源项目：

- **[cloud-mail](https://github.com/maillab/cloud-mail)**（MIT）—— 邮件服务本体。网关通过它的 HTTP API 收发邮件；接口字段与附件地址规则参照其官方源码实现，代码注释中标注了对应位置。
- **[go-imap](https://github.com/emersion/go-imap)**（MIT）—— IMAP 协议实现，网关的服务端框架。
- **[go-smtp](https://github.com/emersion/go-smtp)**（MIT）—— SMTP 协议实现。
- **[go-sasl](https://github.com/emersion/go-sasl)**（MIT）—— SASL 认证机制。
- **[go-message](https://github.com/emersion/go-message)**（MIT）—— 邮件消息解析。
- **[hydroxide](https://github.com/emersion/hydroxide)**（MIT）—— 同类的 ProtonMail 桥接网关。本项目的 `SEARCH` 条件匹配、`STATUS` 未读计数等实现参考了它的设计思路（未复制代码）。它在这些方面也有未完成的 TODO，本项目在其基础上补齐了。

本仓库的代码为独立实现，未包含上述项目以外的第三方源码。

---

## 许可证

本项目基于 [MIT 许可证](LICENSE) 开源。

---

*文档版本：v1.12*
