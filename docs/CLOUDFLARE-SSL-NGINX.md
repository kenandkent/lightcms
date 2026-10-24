# Cloudflare 解析 + 代理（黄云）与 HTTPS 配置指南

本文档说明如何将域名通过 Cloudflare 解析到服务器、开启代理（黄云），
并在服务器上使用 Certbot 申请证书、配置 Nginx HTTPS。适用于 MediaWeb Admin
等需对外提供 HTTPS 访问的场景。

## 目录

- [前置说明](#前置说明)
- [一、Cloudflare 域名解析与代理](#一cloudflare-域名解析与代理)
- [二、Cloudflare SSL/TLS 模式](#二cloudflare-ssltls-模式)
- [三、服务器安装 Certbot 并申请证书](#三服务器安装-certbot-并申请证书)
- [四、Nginx HTTPS 配置](#四nginx-https-配置)
- [（补充）同一服务器新增子域名证书](#补充同一服务器新增子域名证书)
- [五、验证与续期](#五验证与续期)

---

## 前置说明

- 已拥有域名，且域名 DNS 已托管在 Cloudflare（或已添加站点到 Cloudflare）。
- 服务器（如 Ubuntu 22）已安装 Nginx，且 80/443 端口可被 Cloudflare 回源访问。
- 下文以 `share.ibreeze.agency` 为例，请替换为你的实际域名。

---

## 一、Cloudflare 域名解析与代理

1. 登录 [Cloudflare Dashboard](https://dash.cloudflare.com)，选择对应站点（域名）。

2. 进入 **DNS** → **Records**，添加或编辑一条 **A** 记录：
   - **Type**: A
   - **Name**: `@`（根域名）或子域名（如 `admin` 表示 admin.ibreeze.agency）
   - **IPv4 address**: 填写你的**服务器公网 IP**
   - **Proxy status**: 勾选 **Proxied**（即开启「黄云」代理）

3. 若需子域名（如 `admin.ibreeze.agency`），可再添加一条 A 记录，Name 填 `admin`，
   同样指向服务器 IP，并开启 Proxied。

**说明**：开启 Proxied 后，用户访问会先经 Cloudflare 再回源到你的服务器，
可隐藏源站 IP、享受 CDN 与 DDoS 防护；回源时 Cloudflare 会访问你服务器的 80/443。

---

## 二、Cloudflare SSL/TLS 模式

1. 在 Cloudflare 左侧进入 **SSL/TLS**。

2. **Overview** 中 **Encryption mode** 建议选择：
   - **Full (strict)**：Cloudflare 到源站使用 HTTPS，需源站已配置有效证书（Certbot 申请后即可）。
   - 若尚未在源站配置证书，可先选 **Full**（CF 到源站 HTTPS，允许自签），
     证书配置完成后再改为 **Full (strict)**。

3. **Edge Certificates** 中可勾选 **Always Use HTTPS**，
   **Minimum TLS Version** 选 1.2 或以上。

---

## 三、服务器安装 Certbot 并申请证书

在**服务器**上执行（以 Ubuntu 22 为例）。

### 1. 安装 Certbot 与 Nginx 插件

```bash
apt update
apt install -y certbot python3-certbot-nginx nginx
```

### 2. 确保 Nginx 已监听 80 且 server_name 为你的域名

先配置好 HTTP 站点（如 `server_name share.ibreeze.agency;`、`listen 80;`），
以便 Certbot 通过 HTTP-01 验证域名。可参考 DEPLOY.md 中的「方式一：仅 HTTP」，
将 `server_name` 改为你的域名。

```bash
nginx -t
systemctl reload nginx
```

### 3. 申请证书（Nginx 插件自动配置,需要提前配置域名解析，并在命令行输入cf账号的邮箱）

```bash
Cflareproton@proton.me
certbot --nginx --cert-name trader -d mm.ibreeze.agency -d trader.ibreeze.agency -d mmtrader.ibreeze.agency

certbot certificates 
```

按提示输入邮箱、同意条款；验证通过后证书会写入
`/etc/letsencrypt/live/worker/`，并可由 Certbot 自动插入 Nginx 的
`ssl_certificate` / `ssl_certificate_key` 配置。

证书路径为：

- 证书：`/etc/letsencrypt/live/worker/fullchain.pem`
- 私钥：`/etc/letsencrypt/live/worker/privkey.pem`

后续需在 Nginx 中手动指定上述路径（见第四节）。

---

## （补充）同一服务器新增子域名证书

已在源站为 `share.ibreeze.agency` 配好 Certbot + Nginx 时，**再部署其他项目、使用新的子域名**（如 `app.ibreeze.agency`），按下面做即可；**每个子域名各自一张 Let’s Encrypt 证书**是常见做法，互不影响。

1. **Cloudflare DNS**  
   为该子域名新增 **A** 记录，指向**同一台服务器公网 IP**；若与现有站点一致，保持 **Proxied（黄云）** 与 **SSL/TLS → Full (strict)** 即可。

2. **Nginx 先加 HTTP 站点**  
   为新项目增加 `server` 块：`listen 80;`、`server_name app.ibreeze.agency;`（示例），`root` / `proxy_pass` 指到新项目；`nginx -t` 后 `systemctl reload nginx`。  
   若 Certbot 用 HTTP-01 验证，需保证该域名在 **80 端口**能访问到本机（与现有站点并存，用不同 `server_name` 区分）。

3. **为该子域名单独申请证书**  

   ```bash
   certbot --nginx -d app.ibreeze.agency
   ```

   Certbot 会新增或扩展配置，证书通常位于  
   `/etc/letsencrypt/live/app.ibreeze.agency/`（与 `share.ibreeze.agency` 目录并列）。

4. **续期**  
   系统上的 `certbot renew`（或 timer）会对**已申请的所有证书**一起续期，一般无需为每个子域名单独配置定时任务。

**在同一张已有证书上扩充子域名（多 SAN）**  

可以。Let’s Encrypt 允许一张证书里列多个域名；**扩充**后仍使用原证书在  
`/etc/letsencrypt/live/<证书名>/` 下的 `fullchain.pem` / `privkey.pem`（证书名多为**首次申请时的第一个 `-d`**）。

1. 为新子域名做好 DNS，并在 Nginx 里为该子域名配置好 `server`（至少 80，便于验证）。
2. 执行（将 `share.ibreeze.agency` 换成你当前证书 lineage 名称，可用 `certbot certificates` 查看）：

   ```bash
   certbot certificates   # 确认 Certificate Name 与已有域名
   certbot --nginx --cert-name music-upload -d main.hostmails.de -d worker.hostmails.de -d admin.hostmails.de
   certbot --nginx --cert-name workers -d worker1.hostmails.de -d worker2.hostmails.de -d worker3.hostmails.de -d worker4.hostmails.de -d worker5.hostmails.de -d worker6.hostmails.de -d worker7.hostmails.de -d worker8.hostmails.de -d worker9.hostmails.de -d worker10.hostmails.de
   ```

   Certbot 会提示**是否扩展**该证书以包含新域名；选 **Expand**（或命令行加 `--expand` 视版本而定）。  
   若用 `certonly`：

   ```bash
   certbot certonly --nginx --cert-name share.ibreeze.agency \
     -d share.ibreeze.agency -d app.ibreeze.agency --expand
   ```

3. 在 Nginx 里让 **所有**这些 `server_name` 的 `443` 块**共用**同一组  
   `ssl_certificate` / `ssl_certificate_key`（指向上述 `live/<证书名>/` 路径）。

**取舍**：一张证书多域名便于统一管理、少几条续期记录；任一域名验证失败可能影响整证续期。子域名多、项目彼此独立时，**每域名单独一张证书**往往更简单。

### 排错：证书已签发但 `Could not install certificate`

**现象**：日志里已有 `Successfully received certificate`、`Certificate is saved at .../live/<证书名>/`，但随后出现  
`Could not install certificate`、`Could not automatically find a matching server block for <某域名>`。

**原因**：`certbot --nginx` 会在现有 Nginx 配置里查找与 **`-d` 中每个域名** 对应的 `server_name`；某个域名在配置里**完全没有** `server` / `server_name` 时，Certbot 就无法把证书自动写进该站点（其它域名若写在 `default` 里则可能已成功）。

**处理**：

1. 编辑 Nginx，为缺失的域名增加 `server`（至少含 `listen 80;` 与 `server_name 该域名;`，若已上 HTTPS 则再加 `listen 443 ssl`），或把该域名加入已有站点的 `server_name`。
2. 手动指定与其它同证书站点**相同**的证书路径，例如：  
   `ssl_certificate /etc/letsencrypt/live/vm.hostmails.de/fullchain.pem;`  
   `ssl_certificate_key /etc/letsencrypt/live/vm.hostmails.de/privkey.pem;`  
   （`<证书名>` 以 `certbot certificates` 中 **Certificate Name** 为准。）
3. 执行 `nginx -t`，通过后 `systemctl reload nginx`。
4. 若仍希望由 Certbot 自动改配置，可在修正 Nginx 后执行：  
   `certbot install --cert-name vm.hostmails.de`（将证书名换成你的 **Certificate Name**）。若已手动写好 `ssl_certificate`，通常**不必**再 install。

---

## 四、Nginx HTTPS 配置

### 方式 A：Certbot 已用 `--nginx` 自动配置

若已执行 `certbot --nginx -d share.ibreeze.agency`，Certbot 通常已为对应 `server`
块添加了 `ssl_certificate` 与 `ssl_certificate_key`，并可能已添加 80→443 跳转。
检查并微调即可：

```bash
nano /etc/nginx/sites-available/mediaweb-admin
```

确认存在 `listen 443 ssl`、证书路径为
`/etc/letsencrypt/live/share.ibreeze.agency/fullchain.pem` 与 `privkey.pem`。

### 方式 B：手动编写完整 HTTPS 配置

若使用 `certonly --webroot` 或希望统一管理配置，可自行编写。

下面为 **生产**（`share.ibreeze.agency` → 后端 `3001`、静态 `/var/www/mediaWeb-admin`）与 **开发**（示例子域名 `dev.share.ibreeze.agency` → 后端 `33001`、静态 `/var/www/mediaWeb-admin-dev`）两套 `server`。开发环境需：

- 在 Cloudflare DNS 增加 **A** 记录：`dev` → 同服务器 IP（可开黄云）；
- 为该子域名单独申请证书，例如：  
  `certbot certonly --nginx -d mmtrader.ibreeze.agency`
  （或 `certbot --nginx -d dev.share.ibreeze.agency`，与现有流程一致）；
- 将示例中证书路径换为 `certbot certificates` 里对应名称（常见为 `/etc/letsencrypt/live/dev.share.ibreeze.agency/`）。

端口与目录与 [DEPLOY.md](./DEPLOY.md) 中 prod/dev 约定一致；若你本地 `configs/config.yaml` 使用其它端口，请同步修改 `proxy_pass`。

```nginx
rm -rf /etc/nginx/sites-enabled/default && nano /etc/nginx/sites-enabled/default
# ---------------------------------------------------------------------------
# 开发：worker007.hostmails.de
# ---------------------------------------------------------------------------
server {
    listen 443 ssl http2;
    server_name worker007.hostmails.de;
    ssl_certificate /etc/letsencrypt/live/worker/fullchain.pem; # managed by Certbot
    ssl_certificate_key /etc/letsencrypt/live/worker/privkey.pem; # managed by Certbot
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256;
    ssl_prefer_server_ciphers off;

    location / {
        proxy_pass http://127.0.0.1:36501;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}

# HTTP重定向到HTTPS（所有域名）
server {
    if ($host = worker007.hostmails.de) {
        return 301 https://$host$request_uri;
    } # managed by Certbot

    listen 80;
    server_name worker007.hostmails.de;
    return 301 https://$server_name$request_uri;

}

```

保存后执行：

```bash
nginx -t
systemctl reload nginx
```

---

## 五、验证与续期

### 验证

- 浏览器访问 `https://share.ibreeze.agency/`，应看到 HTTPS 且证书有效。
- 若已配置开发子域名与证书，访问 `https://dev.share.ibreeze.agency/`（或你实际使用的 dev 域名）同样应为 HTTPS 且静态/API 分别指向 dev 目录与 `33001`。
- Cloudflare 仪表盘 **SSL/TLS** 可保持 **Full (strict)**。

### 证书续期

Let's Encrypt 证书有效期为 90 天，建议使用系统定时任务自动续期：

```bash
certbot renew --dry-run
```

若通过，可添加 cron（通常 Certbot 安装时会添加）：

```bash
# 每天检查，到期前自动续期
0 3 * * * certbot renew --quiet --post-hook "systemctl reload nginx"
```

---

**文档版本**: v1.1  
**更新日期**: 2026-03-21
