#!/usr/bin/env bash
# cloudmail-bridge 证书一键申请脚本
# 交互式引导：选 DNS 服务商 -> 填域名和凭证 -> 自动申请、安装、配置续期
# 全程无需手动修改脚本内容。凭证只保存在本机 acme.sh 配置里。

set -e

# 颜色（终端不支持时自动降级）
if [ -t 1 ]; then
  C_RED='\033[31m'; C_GREEN='\033[32m'; C_YELLOW='\033[33m'; C_BLUE='\033[36m'; C_OFF='\033[0m'
else
  C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_OFF=''
fi

info() { printf "${C_BLUE}[信息]${C_OFF} %s\n" "$*"; }
ok()   { printf "${C_GREEN}[完成]${C_OFF} %s\n" "$*"; }
warn() { printf "${C_YELLOW}[提示]${C_OFF} %s\n" "$*"; }
err()  { printf "${C_RED}[错误]${C_OFF} %s\n" "$*" >&2; }
ask()  { printf "${C_GREEN}>>> ${C_OFF}%s" "$*"; }

RETURN_VAL=""
read_value() {
  # $1=提示 $3=是否密钥(1=输入不回显)；结果写入全局 RETURN_VAL
  local prompt="$1" secret="$3" val
  if [ "$secret" = "1" ]; then
    printf "${C_GREEN}>>> ${C_OFF}%s" "$prompt"
    stty -echo 2>/dev/null || true
    read -r val
    stty echo 2>/dev/null || true
    printf '\n'
  else
    ask "$prompt"
    read -r val
  fi
  RETURN_VAL="$val"
}

require() {
  # $1=值 $2=名称
  if [ -z "$1" ]; then err "$2 不能为空，已退出。"; exit 1; fi
}

echo ""
echo "=============================================="
echo "  cloudmail-bridge  SSL 证书一键申请"
echo "=============================================="
echo ""
info "开始前请先准备好："
echo "  1) 你的域名（例如 mail.example.com），并把它的解析托管在下面某家 DNS；"
echo "  2) 该 DNS 服务商的 API 凭证（下面选择后会告诉你在哪里拿）；"
echo "  3) 服务器能访问互联网。"
echo ""

# ---------- 安装目录 ----------
DEFAULT_DIR="$(pwd)/certs"
ask "证书安装目录（直接回车用默认值：$DEFAULT_DIR）："
read -r CERT_DIR
[ -z "$CERT_DIR" ] && CERT_DIR="$DEFAULT_DIR"
mkdir -p "$CERT_DIR"
ok "证书目录：$CERT_DIR"

# ---------- 选 DNS 服务商 ----------
echo ""
info "选择你的域名解析服务商（DNS API 用于自动验证域名）："
echo "  1) Cloudflare"
echo "  2) 腾讯云 DNSPod / EdgeOne（域名解析在 DNSPod 的都选这个）"
echo "  3) 阿里云"
echo "  4) 腾讯云（DNS 解析在腾讯云控制台，非 DNSPod）"
echo ""
ask "请输入序号 [1/2/3/4]："
read -r DNS_CHOICE

case "$DNS_CHOICE" in
  1)
    DNS_PLUGIN="dns_cf"
    warn "Cloudflare Token 获取：登录 dash.cloudflare.com → 右上角头像 → My Profile → API Tokens → Create Token → 用『Edit zone DNS』模板，Zone Resources 选你的域名。"
    ask "输入 CF API Token："; read -r CF_Token; require "$CF_Token" "CF API Token"
    ask "输入 CF Zone ID（域名 Overview 页右侧 API 区域）："; read -r CF_Zone_ID; require "$CF_Zone_ID" "CF Zone ID"
    export CF_Token CF_Zone_ID
    DNS_ENV_CODE="export CF_Token=\"$CF_Token\"\nexport CF_Zone_ID=\"$CF_Zone_ID\""
    ;;
  2)
    DNS_PLUGIN="dns_dp"
    warn "DNSPod Token 获取：登录 console.dnspod.cn → 右上角账号 → API 密钥 → 创建密钥，得到 ID 和 Token（EdgeOne 若把域名解析放在 DNSPod，也用这个）。"
    ask "输入 DNSPod API ID："; read -r DP_Id; require "$DP_Id" "DNSPod API ID"
    ask "输入 DNSPod API Token："; read_value "输入 DNSPod API Token：" x 1; DP_Key="$RETURN_VAL"; require "$DP_Key" "DNSPod API Token"
    export DP_Id DP_Key
    DNS_ENV_CODE="export DP_Id=\"$DP_Id\"\nexport DP_Key=\"$DP_Key\""
    ;;
  3)
    DNS_PLUGIN="dns_ali"
    warn "阿里云 Key 获取：登录 ram.console.aliyun.com → 用户 → 创建用户并授予 AliyunDNSFullAccess 权限 → 生成 AccessKey ID/Secret。"
    ask "输入阿里云 AccessKey ID："; read -r Ali_Key; require "$Ali_Key" "AccessKey ID"
    ask "输入阿里云 AccessKey Secret："; read_value "输入阿里云 AccessKey Secret：" x 1; Ali_Secret="$RETURN_VAL"; require "$Ali_Secret" "AccessKey Secret"
    export Ali_Key Ali_Secret
    DNS_ENV_CODE="export Ali_Key=\"$Ali_Key\"\nexport Ali_Secret=\"$Ali_Secret\""
    ;;
  4)
    DNS_PLUGIN="dns_tencent"
    warn "腾讯云 API 密钥获取：登录 console.cloud.tencent.com/cam/capi → 创建密钥，得到 SecretId/SecretKey（需有 DNSPod 读写权限）。"
    ask "输入腾讯云 SecretId："; read -r Tencent_SecretId; require "$Tencent_SecretId" "SecretId"
    ask "输入腾讯云 SecretKey："; read_value "输入腾讯云 SecretKey：" x 1; Tencent_SecretKey="$RETURN_VAL"; require "$Tencent_SecretKey" "SecretKey"
    export Tencent_SecretId Tencent_SecretKey
    DNS_ENV_CODE="export Tencent_SecretId=\"$Tencent_SecretId\"\nexport Tencent_SecretKey=\"$Tencent_SecretKey\""
    ;;
  *)
    err "无效序号，已退出。"; exit 1
    ;;
esac

# ---------- 域名 ----------
echo ""
ask "输入要申请证书的域名（如 mail.example.com）："
read -r DOMAIN
require "$DOMAIN" "域名"
info "如果想同时签泛域名（*.example.com），可在域名后加空格再填泛域名；直接回车跳过："
read -r EXTRA_DOMAIN
if [ -n "$EXTRA_DOMAIN" ]; then
  DOMAIN_ARGS="-d $DOMAIN -d $EXTRA_DOMAIN"
else
  DOMAIN_ARGS="-d $DOMAIN"
fi

# ---------- 续期后重启的容器 ----------
ask "证书续期后要重启的容器名（直接回车用默认 cloudmail-bridge）："
read -r CTAINER
[ -z "$CTAINER" ] && CTAINER="cloudmail-bridge"

echo ""
info "确认信息："
echo "  域名        : $DOMAIN ${EXTRA_DOMAIN:++ $EXTRA_DOMAIN}"
echo "  DNS 插件    : $DNS_PLUGIN"
echo "  证书目录    : $CERT_DIR"
echo "  续期后重启  : $CTAINER"
ask "以上正确吗？输入 y 开始申请："
read -r CONFIRM
case "$CONFIRM" in
  y|Y|yes|YES) ;;
  *) info "已取消。"; exit 0 ;;
esac

# ---------- 安装 acme.sh ----------
echo ""
info "第 1 步：安装 acme.sh ..."
if command -v acme.sh >/dev/null 2>&1; then
  ok "acme.sh 已安装，跳过。"
else
  # 优先 Gitee 镜像（国内可用），失败再试官方源
  if git clone --depth 1 https://gitee.com/neilpang/acme.sh.git /tmp/acme.sh-src >/dev/null 2>&1; then
    cd /tmp/acme.sh-src && ./acme.sh --install -m "admin@$DOMAIN" >/dev/null 2>&1 && ok "acme.sh 安装完成（Gitee 镜像）。"
  elif curl -fsSL https://get.acme.sh | sh -s "email=admin@$DOMAIN" >/dev/null 2>&1; then
    ok "acme.sh 安装完成（官方源）。"
  else
    err "acme.sh 安装失败，请检查网络后重试。"; exit 1
  fi
  export PATH="$HOME/.acme.sh:$PATH"
  source ~/.bashrc 2>/dev/null || true
fi
# 兼容 alias：用绝对路径兜底
ACME="$HOME/.acme.sh/acme.sh"
[ -x "$ACME" ] || ACME="$(command -v acme.sh)"

# ---------- 切换 CA ----------
info "第 2 步：设置 CA 为 Let's Encrypt ..."
"$ACME" --set-default-ca --server letsencrypt >/dev/null 2>&1 || true
ok "CA 已设置。"

# ---------- 申请证书 ----------
info "第 3 步：申请证书（DNS 自动验证，无需开放 80 端口）..."
# shellcheck disable=SC2086
"$ACME" --issue --dns "$DNS_PLUGIN" $DOMAIN_ARGS --keylength ec-256
ok "证书申请成功。"

# ---------- 安装证书 ----------
info "第 4 步：安装证书到 $CERT_DIR ..."
"$ACME" --install-cert -d "$DOMAIN" --ecc \
  --key-file "$CERT_DIR/key.pem" \
  --fullchain-file "$CERT_DIR/fullchain.pem" \
  --reloadcmd "docker restart $CTAINER"
ok "证书已安装到：$CERT_DIR/fullchain.pem 和 $CERT_DIR/key.pem"

# ---------- 自动续期 ----------
info "第 5 步：配置自动续期 ..."
if crontab -l 2>/dev/null | grep -q 'acme.sh.*--cron'; then
  ok "自动续期任务已存在。"
else
  "$ACME" --install-cronjob >/dev/null 2>&1 && ok "自动续期任务已安装（每天检查，到期前自动续）。"
fi

echo ""
echo "=============================================="
ok "全部完成！"
echo "  证书文件  ：$CERT_DIR/fullchain.pem"
echo "  私钥文件  ：$CERT_DIR/key.pem"
echo "  自动续期  ：已开启，续期后自动重启 $CTAINER"
echo "  查看证书  ：$ACME --list"
echo "=============================================="
echo ""
