# cloudmail-bridge 安装指南 v1.05

一个把 cloud-mail 网页邮箱桥接成标准 IMAP/SMTP 服务的网关。部署后可用任意邮件客户端（Outlook、Thunderbird、Apple 邮件、手机自带邮箱等）收发 cloud-mail 里的邮件。

- 支持该邮箱域名下的**所有账号**（含以后新增的），无需逐个配置；
- 账号密码由 cloud-mail 网站验证，改密码后客户端旧密码立即失效；
- 客户端与 cloud-mail 邮箱同步收信、发信、已读状态；
- 只有客户端在线时才同步，关闭客户端即停止拉取，节省云端数据库额度。

> 本指南中的域名、邮箱、Token 全部用占位符表示，请替换成你自己的值。示例域名统一写作 `mail.example.com`（邮箱域名 `example.com`）。示例端口为 `9933` / `4653`，请按实际情况修改。

---

## 目录

- [一、准备工作](#一准备工作)
- [二、生成 Token](#二生成-token)
- [三、部署方案A：VPS / 固定公网服务器（推荐）](#三部署方案avps--固定公网服务器推荐)
- [四、部署方案B：内网动态 IP 环境](#四部署方案b内网动态-ip-环境)
- [五、客户端配置（两方案通用）](#五客户端配置两方案通用)
- [六、配置项说明](#六配置项说明)
- [七、常见问题](#七常见问题)
- [八、安全提醒](#八安全提醒)

---

## 一、准备工作

两种方案都要先做下面这些，本节只写一次，后面不再重复。

### 1. 一台运行 Docker 的机器

- 部署方案A：有 VPS 或固定公网服务器的用户，机器上已安装 Docker 和 Docker Compose；
- 部署方案B：一台内网机器（NAS / 小主机），已安装 Docker，且处于动态公网 IP 网络。

### 2. 一个 cloud-mail 实例

你需要有一个自己搭建好的 cloud-mail 站点（它的根地址下称 `CLOUDMAIL_URL`，例如 `https://mail.example.com`），并且知道它的**管理员邮箱**。

### 3. 创建安装目录

```bash
mkdir -p cloudmail-bridge && cd cloudmail-bridge
```

后续所有命令都在这个目录里执行。两个方案都会用到 `docker-compose.yml`；方案A/A 还会用到 `certs/`（证书）。

---

## 二、生成 Token

网关需要一个 **public token** 才能读取各邮箱的收件。这个 token 由你自己生成。

在本机终端执行下面的命令（把三处尖括号换成你自己的值）：

```bash
curl -X POST "https://mail.example.com/api/public/genToken" \
  -H "Content-Type: application/json" \
  -d '{"email":"<管理员邮箱>","password":"<管理员密码>"}'
```

返回 JSON 里 `data.token` 的值就是你要的 token，复制保存好，下一步要填进 `docker-compose.yml`。

> 管理员邮箱/密码只在这一步用来换取 token，不会写进服务器配置或网关程序。

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
     https://raw.githubusercontent.com/<你的用户名>/cloudmail-bridge/main/tools/cert.sh
   bash cert.sh
   ```

   脚本会依次询问：证书目录 → 选 DNS 服务商（1 Cloudflare / 2 腾讯云 DNSPod / 3 阿里云 / 4 腾讯云）→ 输入 API 凭证 → 输入域名 → 续期后要重启的容器名。确认后它会自动完成：安装 acme.sh（国内走 Gitee 镜像）→ 申请 ECC 证书 → 安装到 `./certs/fullchain.pem` 和 `./certs/key.pem` → 配置自动续期，续期后自动重启容器。

3. 常用管理命令（直接拷贝粘贴）：

   ```bash
   # 查看已签发的证书及到期时间
   ~/.acme.sh/acme.sh --list

   # 查看自动续期任务（应看到 acme.sh --cron 那一行）
   crontab -l | grep acme.sh

   # 测试续期是否正常（只检查，不实际执行）
   ~/.acme.sh/acme.sh --renew -d "mail.example.com" --test
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

新建 `docker-compose.yml`，把 token、域名、端口映射都写在里面：

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
      BASE_URL: "https://mail.example.com"     # 你的 cloud-mail 地址
      DOMAIN: "example.com"                    # 你的邮箱域名
      PUBLIC_TOKEN: "<第二节生成的token>"      # 只放 token，不放管理员密码
      TLS_MODE: "implicit"
      CERT_FILE: "/app/certs/fullchain.pem"
      KEY_FILE: "/app/certs/key.pem"
      POLL_INTERVAL: "60"
      SYNC_INTERVAL: "2"
    volumes:
      - ./certs:/app/certs:ro
```

说明：

- `ports` 里的 `9933:993`：冒号左边 `9933` 是**外部端口**（客户端连服务器时填这个），冒号右边 `993` 是**容器内 IMAP 服务端口**（固定不用改）；`4653:465` 同理，`465` 是容器内 SMTP 服务端口。外部端口可改成任意未占用端口，但要保证客户端和防火墙填的是同一个外部端口。本示例端口为占位示例，请按实际修改。
- 服务器的**安全组 / 防火墙**要放行上面填的外部 IMAP 端口（9933）和外部 SMTP 端口（4653），否则客户端连不上。
- token 填写方式二选一：
  - 只填 `PUBLIC_TOKEN`：用第二节生成的值替换。token 失效时需手动重新生成、替换并重建容器；
  - 填 `ADMIN_EMAIL` + `ADMIN_PASSWORD` 代替 `PUBLIC_TOKEN`：网关会在 token 失效时自动重新生成，**不用每次手动更换**。代价是管理员密码会保存在服务器的 compose 文件里，请自行权衡。

### 第三步：启动与更新

```bash
docker compose pull
docker compose up -d
docker logs -f cloudmail-bridge
```

看到 IMAP/SMTP 监听且无报错即成功。更新时重跑 `docker compose pull && docker compose up -d`。

然后跳到[第五节](#五客户端配置两方案通用)配置邮件客户端。

---

## 四、部署方案B：内网动态 IP 环境

适用：机器在家里/公司内网、公网 IP 会变动。这种环境下没有固定公网 IP，也没有现成证书，需要自己解决动态域名和加密——网关本身不处理这些，但可以按下面做。

**做方案B前，前面的"准备工作"、"生成 Token"、"compose 写法与启动更新"照做**，本节只写差异部分。

### 1. 动态域名（DDNS）

公网 IP 会变，需要一个动态域名始终指向你家/公司的出口 IP。

- 推荐用 **ddns-go** 安装到内网机器上，由它自动检测 IP 变化并更新域名解析。
- ddns-go 的安装与配置教程见其 GitHub 仓库：**https://github.com/jeessy2/ddns-go**（README 里有各 NAS / 系统的详细安装步骤，这里不重复）。

### 2. 证书（方案B 专用命令）

方案B 和方案A 的证书申请**命令不一样**，区别在于：方案A 的域名解析托管在 DNS 服务商，用 DNS API 自动验证；方案B 的域名由 ddns-go 动态更新，同样可以用 DNS API 验证（前提是域名托管在那家有 API 的服务商，比如 Cloudflare / DNSPod / 阿里云 / 腾讯云），所以**可以直接复用第三节的 `cert.sh` 一键脚本**——把脚本里"选 DNS 服务商"那一步按你的托管商选即可，其余完全一样，证书同样装到 `./certs/`。

只有一种情况需要额外操作：你的域名托管商**不在**上面的对照表里（没有 DNS API）。这时用 DNS 别名（CNAME）方式委托验证：

```bash
# 1) 在你的主域名下加一条 CNAME（以 Cloudflare 托管主域名为例）
#    _acme-challenge.mail.example.com  ->  mail.example.com

# 2) 用 CNAME 别名模式申请（--dns dns_cf 换成你主域名托管商的插件）
export CF_Token="你的主域名服务商Token"
export CF_Zone_ID="你的主域名ZoneID"
~/.acme.sh/acme.sh --issue --dns dns_cf \
  --domain-alias "mail.example.com" \
  -d "dynamic.example.com" --keylength ec-256

# 3) 安装证书到 ./certs（与方案A 相同）
~/.acme.sh/acme.sh --install-cert -d "dynamic.example.com" --ecc \
  --key-file "$(pwd)/certs/key.pem" \
  --fullchain-file "$(pwd)/certs/fullchain.pem" \
  --reloadcmd "docker restart cloudmail-bridge"
```

DNS API 验证不要求开放 80 端口，内网环境也能签发。

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

- 支持该邮箱域名下的**任意账号**，用户名填完整邮箱、密码填该邮箱自己的网站密码即可，不需要在网关里预先登记；
- 收件箱、发件箱（Sent）、已读状态都会与网页版双向同步；
- 支持 IMAP IDLE 推送的客户端（Outlook、Thunderbird、系统自带邮件）可秒级收信；不支持 IDLE 的客户端靠自身轮询，会有 1-2 分钟延迟，这是客户端行为，与网关无关。

---

## 六、配置项说明

配置通过 compose 的 `environment` 传入（不依赖单独的 config.json）。全部环境变量如下：

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `BASE_URL` | cloud-mail 站点根地址 | 必填 |
| `DOMAIN` | 你的邮箱域名，用于生成 Message-ID | 必填 |
| `PUBLIC_TOKEN` | 第二节生成的 token；与 `ADMIN_EMAIL`+`ADMIN_PASSWORD` 二选一 | 二选一必填 |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | 管理员邮箱/密码，代替 `PUBLIC_TOKEN`，token 失效时网关自动续期；密码会保存在服务器上 | 空 |
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
| `TZ` | 时区，建议 `Asia/Shanghai` | 容器默认 |

关于 `POLL_INTERVAL` 与 `SYNC_INTERVAL`：为节省云端免费额度，网关只在有客户端连接时同步。轮询周期不宜设得过短（建议 ≥ 30 秒），否则可能触发云端数据库的免费读取上限，导致登录接口报错。

---

## 七、常见问题

**Q：客户端提示"密码错误"，但我没改密码？**
A：网关的认证是实时调用 cloud-mail 网站完成的。网站若因数据库超限、服务异常等原因返回错误，网关也会表现为认证失败。请先看网关日志（`docker logs cloudmail-bridge`）里的具体报错，不要盲目改密码。

**Q：Token 会不会过期？需要经常换吗？**
A：不需要经常换。token 生成后长期有效，日常放着不管即可。只有以下情况才会失效：你在命令行重新生成了一次 token（新 token 会覆盖旧的）、cloud-mail 重建或清空过数据、管理员在后台重置。
- 只填 `PUBLIC_TOKEN`：失效后重新执行第二节命令生成新 token，替换 compose 里的值，`docker compose up -d` 重建容器即可；
- 填了 `ADMIN_EMAIL` + `ADMIN_PASSWORD`：失效时网关自动重新生成，无需手动更换（代价是管理员密码保存在服务器上）。

**Q：能收到但发不出去？**
A：检查 SMTP 端口和加密方式是否与配置一致；确认该邮箱在 cloud-mail 网站能正常发信。发信走 cloud-mail 原有的发信通道，两边都会留痕。

**Q：新邮件要等一两分钟才出现？**
A：先看客户端是否支持 IDLE 推送。不支持的客户端靠自身轮询，属正常现象。网关侧从邮件到达到进入收件箱通常几秒内完成。

**Q：邮件时间差 8 小时？**
A：早期版本有过时区问题，请使用本版本或更新版本。

**Q：可以只给某个邮箱用吗？**
A：网关对域名下所有邮箱生效。不想让某个账号使用，直接在 cloud-mail 网站停用该账号即可。

---

## 八、安全提醒

1. token 等同于该站点所有邮箱的收件权限，请把 compose 文件当作凭据对待，权限设为仅属主可读（`chmod 600 docker-compose.yml`），不要提交到公开仓库或发给他人；
2. 公网部署务必用 TLS（`TLS_MODE=implicit` + 有效证书），不要在公网用 `plain`；
3. 客户端密码就是邮箱在 cloud-mail 的网站密码，请使用强密码；
4. 网关不保存邮箱密码：每次登录都交给网站验证，仅在内存里临时持有该会话的登录态；
5. 定期更新镜像（`docker compose pull && docker compose up -d`）以获取修复。

---

*文档版本：v1.05*
