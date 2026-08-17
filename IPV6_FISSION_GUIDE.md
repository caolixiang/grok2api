# IPv6 裂变出口搭建教程（grok2api）

> 目标：让 grok2api 对 Grok 的出站流量，每次连接从一段 `/64` IPv6 子网里**随机换一个源地址**出去，规避大平台基于 IP 的风控（"降智"）。
>
> 本教程示例网段 `2a01:4ff:1f0:c500::/64`、示例端口 `41080`、示例用户 `fission`，均为占位示例。**请替换为你自己的 `/64`、端口、密码。**

---

## 0. 背景与原理

### 0.1 什么是"降智"
Grok 等平台会对请求来源 IP 做风控。机房 IPv4、脏 IP、多人共享的代理 IP 容易被暗中降级：不输出深度思考、功能受限、回答变蠢、搜索被阉割等。俗称"降智"。

### 0.2 什么是 IPv6 裂变
很多 VPS 商家会给一段 `/64` IPv6 子网（包含 $2^{64}$ 个地址）。"裂变"指让 VPS 启用该网段内所有地址，并在每次发起请求时**随机轮换使用不同的 IPv6 源地址**。平台无法把你的请求与单一 IP 绑定，且多数大厂对 IPv6 的风控比 IPv4 宽松。

### 0.3 关键前提：grok2api 自身不支持裂变
读完 grok2api 源码后确认（当前代码库）：

- **直连出站用的是裸 `net.Dialer`**（`backend/internal/infra/egress/buildclient.go`），没有随机绑定源地址的逻辑。所以论坛"方法1 系统级 AnyIP 裂变"在直连模式下不生效。
- **`qualityGuard.rotationURL` / `rotatableNodeIDs` 这套配置是"孤儿配置"**：schema、校验（`config.go`）、序列化进 bootstrap.json（`bootstrap.go`）都在，但当前实际运行的 Go `QualityWorker`（`quality_worker.go`）**从不调用 rotationURL**，只做节点隔离/冷却。真正调用换 IP 的代码只在已废弃的 Python sidecar（`tools/egress-quality-guard/quality_guard.py`）里。
- **出口代理走 `golang.org/x/net/proxy`**（`buildclient.go:74`）：`socks5` / `socks5h` 走同一个 dialer，把**域名以 FQDN 原样转给代理**（远程 DNS，见 `internal/socks/client.go:96-103`）。这点决定了我们能在代理端强制走 v6。

**结论**：裂变必须**外挂一个 SOCKS5 代理**（每次连接随机绑 `/64` 源），然后在 grok2api 管理端加一个出口节点指向它。grok2api 本身不用改代码。

### 0.4 整体架构与数据流
本教程是**跨机部署**（grok2api 与裂变代理不在同一台）：

```
┌──────────────────────┐         socks5h://fission:***@<hetzner-ip>:41080        ┌──────────────────────────┐
│  grok2api 主机        │ ──────────────────────────────────────────────────────▶ │  Hetzner 裂变代理主机     │
│  (任意 IP，如         │   每条连接：把域名丢给代理                                │  Xray + AnyIP /64         │
│   107.182.173.206)    │                                                          │  random balancer × 128    │
│  grok2api + 出口节点   │ ◀──────────── 不同 v6 源出口（2a01:4ff:1f0:c500::xx） ─ │  sendThrough + UseIPv6     │
└──────────────────────┘                                                            └────────────┬─────────────┘
                                                                                                 │ v6
                                                                                                 ▼
                                                                                         Grok 上游（Cloudflare 双栈）
```

两台机器的分工：

- **Hetzner 裂变代理主机**：拿到能裂变的 `/64`，开 AnyIP，跑 Xray，对外暴露一个带认证的 SOCKS5 端口。
- **grok2api 主机**：跑 grok2api，在管理端加一个出口节点指向裂变代理。它自己**不需要**能裂变（甚至不需要公网 v6）。

> 同机部署变体见 [附 C](#附-c同机部署变体)。

### 0.5 方案边界：fission 解决什么、不解决什么
**解决**：
- IP 维的降智 / 风控关联——每连接换源 IP，平台无法按 IP 把你钉死。
- 多账号共享同一出口 IP 互相连累的问题——根本不再有"同一出口 IP"。

**不解决**：
- **账号维**的限制/封禁（账号本身被标记，与 IP 无关）。
- **模型维**的退化（Grok 侧模型质量问题、灰度差异）。
- Grok 服务条款层面的违规风险——本教程仅供技术研究。

**需要你接受的一个取舍**：本方案建议**关掉 grok2api 的 Quality Guard**（见第 5.1 节）。代价是放弃自动"流口水"检测；在"fission 能解决 IP 维降智"成立时可接受，真出现非 IP 维退化就手动处理或重新打开 Quality Guard。

---

## 1. 选机器（最关键，最容易踩坑）

**不是"有 IPv6"就行，必须是"整段 `/64` 路由给你、且不在网关做源地址校验（uRPF）"的商家。**

很多廉价 VPS 给你的只是**一个 SLAAC 地址**，接口上的 `/64` 只是"在网段内自动配置"，**不代表整段路由给你**，网关普遍做 uRPF，只放那个分配的地址出去。

实测记录：

| 机器 | 情况 | 能否裂变 |
|---|---|---|
| WebNX（AS18450，`mmzz-us-01`） | 有 v6，`::a` 能用，但 /64 整段绑源超时 | ❌ 只能单地址走 v6 |
| Netcup（`v220...`） | 有 v6（SLAAC），/64 整段绑源超时（显式加地址也超时） | ❌ 网关拦源 |
| NAT 小鸡（`192-168-1-127`） | 只有 `fe80::` 链路本地，没公网 v6 | ❌ 商家没给 v6 |
| **Hetzner Cloud（`hetzner01`）** | /64 整段路由给你，绑任意源都通 | ✅ **可用** |

**推荐的商家**（社区公认能把整段路由给你、可做裂变）：
- **Hetzner Cloud**：给 `/64` 并路由给该 VM ✅（本教程用的）
- **Vultr**：给 `/64`，路由给你 ✅
- **Oracle Cloud 免费小鸡**：给 routed `/56`，fission 社区公认最好用且免费 ✅
- **BuyVM**：给 `/48` ✅

> 选定机器后，**这台机器只用来跑裂变代理**；grok2api 留在它原本的机器上。

---

## 2. 判断一台机器能不能裂变（绑定测试）

无论用哪家，都先跑这个测试确认 `/64` 全段可作源地址。**这是裂变的门，过不了后面都白做。**

### 2.1 先看有没有公网 v6
```bash
ip -6 addr show
```
- 有 `scope global` 的 v6 → 继续 2.2。
- 只有 `fe80::`（链路本地）→ 没公网 v6，先按第 3 节在商家控制台开/申请，再手配。

### 2.2 绑定测试（核心）
```bash
# 基线：默认 v6 出口能不能通
curl -6 --max-time 10 https://v6.ipinfo.io/json

# 开 AnyIP，让整段 /64 可在本地绑定
sysctl -w net.ipv6.ip_nonlocal_bind=1
ip -6 route add local <你的网段>::/64 dev lo     # 例: 2a01:4ff:1f0:c500::/64

# 用网段内几个不同源地址分别对外请求
for ip in <网段>::abcd <网段>::1234 <网段>::face; do
  echo "=== bind $ip ==="
  curl -6 --interface "$ip" --max-time 10 https://v6.ipinfo.io/json
  echo
done

# 测完清理
ip -6 route del local <你的网段>::/64 dev lo 2>/dev/null
```

判读：
- **基线通 + 每次返回的 `ip` 都等于你绑的那个地址** → `/64` 全段可路由，**裂变可行**。✅
- **基线通，但绑定别的地址全超时** → 被网关 uRPF 挡了，只有单地址可用，做不了裂变。换商家。
- **基线也不通** → v6 出口本身有问题，先解决连通。

### 2.3 进阶判定（当 2.2 全超时时，区分"NDP 没人应答"还是"网关拦源"）
plain AnyIP 失败有两种根因。用"显式把地址加到网卡上再绑"来区分：
```bash
ip -6 route del local <你的网段>::/64 dev lo 2>/dev/null
ip -6 addr add <网段>::abcd/128 dev eth0
curl -6 --interface <网段>::abcd --max-time 10 https://v6.ipinfo.io/json
ip -6 addr del <网段>::abcd/128 dev eth0
```
- 返回 `::abcd` → /64 路由给你了，是 NDP 回程问题（可救，开 `proxy_ndp` 或动态加地址）。
- 仍超时 → 网关在出口就拦了非 SLAAC 源（uRPF），这台做不了裂变。可用 tcpdump 确认方向：
  ```bash
  ip -6 addr add <网段>::abcd/128 dev eth0
  tcpdump -ni eth0 -c 10 'ip6 and host <网段>::abcd' &
  T=$!; sleep 1
  curl -6 --interface <网段>::abcd --max-time 6 https://v6.ipinfo.io/json 2>&1 | tail -1
  sleep 1; kill $T 2>/dev/null
  ip -6 addr del <网段>::abcd/128 dev eth0 2>/dev/null
  ```
  - 有出向包、无回包 → 回程被丢。
  - 连出向包都没有 → 上行就被拦。

---

## 3. Hetzner 上的具体操作（实测通过）

Hetzner Cloud 给的是**一段路由给你的 `/64`**，但**默认不自动配**，要自己把地址加上去。正因为整段路由给你，裂变才能成。

### 3.1 控制台拿到 /64 子网
Hetzner Cloud 控制台 → Servers → 这台 → Networking → IPv6，找到分配的子网，形如 `2a01:4ff:1f0:c500::/64`。Hetzner 的 IPv6 网关是 `fe80::1`。

### 3.2 手配地址 + 默认路由（先临时验证，重启失效）
```bash
SUB=2a01:4ff:1f0:c500        # 替换成你的前四段

ip -6 addr add ${SUB}::1/64 dev eth0
ip -6 route add default via fe80::1 dev eth0

curl -6 --max-time 10 https://v6.ipinfo.io/json
# 期望 "ip" = 2a01:4ff:1f0:c500::1
```
- 返回 json 且 `ip` 是 `${SUB}::1` → v6 配通了，继续 3.3。
- 报 `File exists` → 地址/路由已存在，忽略，直接看 curl。
- curl 超时 → 网关可能不是 `fe80::1` 或子网抄错，回控制台核对。

### 3.3 绑定测试（按第 2.2 节，用本机网段）
```bash
SUB=2a01:4ff:1f0:c500
sysctl -w net.ipv6.ip_nonlocal_bind=1
ip -6 route add local ${SUB}::/64 dev lo

for ip in ${SUB}::abcd ${SUB}::1234 ${SUB}::face; do
  echo "=== bind $ip ==="
  curl -6 --interface "$ip" --max-time 10 https://v6.ipinfo.io/json
  echo
done

ip -6 route del local ${SUB}::/64 dev lo 2>/dev/null
```
Hetzner 预期三个都返回各自绑的地址（`::abcd`/`::1234`/`::face`）。通过 → 裂变可行。

### 3.4 持久化（重启不丢）
```bash
# 内核开关：允许绑定非本地 v6 源地址（裂变前提）
cat > /etc/sysctl.d/99-fission.conf <<'EOF'
net.ipv6.ip_nonlocal_bind=1
EOF
sysctl -w net.ipv6.ip_nonlocal_bind=1

# 开机自动配 v6 地址、默认路由、AnyIP 本地路由
cat > /etc/systemd/system/ipv6-fission.service <<'EOF'
[Unit]
Description=IPv6 /64 fission setup
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'ip -6 addr add 2a01:4ff:1f0:c500::1/64 dev eth0 2>/dev/null || true; ip -6 route add default via fe80::1 dev eth0 2>/dev/null || true; ip -6 route add local 2a01:4ff:1f0:c500::/64 dev lo 2>/dev/null || true'
ExecStop=/bin/sh -c 'ip -6 route del local 2a01:4ff:1f0:c500::/64 dev lo 2>/dev/null || true; ip -6 route del default via fe80::1 dev eth0 2>/dev/null || true; ip -6 addr del 2a01:4ff:1f0:c500::1/64 dev eth0 2>/dev/null || true'

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ipv6-fission
systemctl status ipv6-fission --no-pager
```
> 想更"正统"可以把地址写进 netplan/networkd，但 AnyIP 那条 `local /64 dev lo` 无论如何得靠这个 unit 来加，用 systemd oneshot 一并管最省事。
> **把上面所有 `2a01:4ff:1f0:c500` 和 `fe80::1` 换成你的网段/网关。**

### 3.5 验证持久化
```bash
# 地址、默认路由在 main 表
ip -6 addr show eth0 | grep c500
ip -6 route show default

# AnyIP 路由在 local 表（注意：ip -6 route show 默认不显示 local 表！）
ip -6 route show table local | grep c500
# 期望看到: local 2a01:4ff:1f0:c500::/64 dev lo ...

# 不手动加路由，直接绑一个没用过的地址测（能通 = AnyIP 持久化生效）
curl -6 --interface 2a01:4ff:1f0:c500::beef --max-time 10 https://v6.ipinfo.io/json
# 期望 "ip" = 2a01:4ff:1f0:c500::beef
```

> **踩坑点**：`ip -6 route show`（不带 `table`）只看 main 表，看不到 `local ... dev lo` 这条 AnyIP 路由。必须用 `ip -6 route show table local` 才看得到。别因为 `ip -6 route show` 没输出就以为没配上——用上面的绑定测试直接验证最准。

---

## 4. 部署裂变代理（Xray）

地基已稳（v6 + AnyIP 持久化、绑定测试通过）。这一步部署一个 SOCKS5 代理：每次连接随机绑一个 `/64` 内源地址出去。

### 4.0 安装 Xray
```bash
# 官方安装脚本（装到 /usr/local/bin/xray、/usr/local/etc/xray/、systemd xray.service）
bash -c "$(curl -L https://github.com/XTLS/Xray-install/raw/main/install-release.sh)" @ install

xray version   # 期望: Xray 26.x.x ...
```
> 安装脚本会放一个示例 `config.json`，下一步我们用自己的覆盖它。

### 4.1 为什么选 Xray
裂变代理的核心需求是"每次连接换一个 `/64` 内源 v6 出去 + 强制走 v6"。评估常见工具：

- **Xray**：`freedom` 出站的 `sendThrough`（绑源 IP）+ `domainStrategy: UseIPv6`（强制 v6 解析）+ `balancer` `strategy:random`（在多个出口间随机轮换）= 原生随机源轮换。**唯一能干净实现的。** ✅
- sing-box / Clash / 3proxy：都没有"每连接随机换本地源地址 + 强制 v6"的组合，做不了干净裂变。

### 4.2 生成 Xray 配置
```bash
mkdir -p /opt/fission /usr/local/etc/xray
cat > /opt/fission/gen-xray.py <<'PYEOF'
import json, secrets, os

PREFIX = "2a01:4ff:1f0:c500"   # 你的 /64 前四段
PORT   = 41080
USER   = "fission"
N      = 128   # 出口数量（random balancer 在这 128 个源里随机选）

# 密码：环境变量传入则用之，否则随机生成并落盘
PASS = os.environ.get("FISSION_PASS", "").strip()
if not PASS:
    PASS = secrets.token_urlsafe(18)
    with open("/opt/fission/auth.txt", "w") as f:
        f.write(f"{USER}\n{PASS}\n")
    os.chmod("/opt/fission/auth.txt", 0o600)

outbounds, tags = [], []
for i in range(1, N + 1):
    tag = f"v6-{i:03d}"
    tags.append(tag)
    outbounds.append({
        "tag": tag,
        "protocol": "freedom",
        "settings": {"domainStrategy": "UseIPv6"},
        "sendThrough": f"{PREFIX}::{i:x}",
    })

config = {
  "log": {"loglevel": "warning"},
  "inbounds": [{
    "tag": "socks-in",
    "listen": "0.0.0.0",
    "port": PORT,
    "protocol": "socks",
    "settings": {"auth": "password", "accounts": [{"user": USER, "pass": PASS}], "udp": True},
  }],
  "outbounds": outbounds,
  "routing": {
    "balancers": [{"tag": "rand", "selector": tags, "strategy": {"type": "random"}}],
    "rules": [{"type": "field", "network": "tcp,udp", "balancerTag": "rand"}],
  },
}
with open("/usr/local/etc/xray/config.json", "w") as f:
    json.dump(config, f, indent=2)

print("=== Xray fission config generated ===")
print(f"outbounds   : {N} (random balancer, UseIPv6)")
print(f"listen      : 0.0.0.0:{PORT}  (socks5, password auth)")
print(f"user        : {USER}")
print(f"password    : {PASS}")
PYEOF

python3 /opt/fission/gen-xray.py
xray run -test -config /usr/local/etc/xray/config.json   # 期望: Configuration OK.
```

> 密码自动写入 `/opt/fission/auth.txt`，**记下来**，grok2api 出口节点 URL 要用。若要自定密码：`FISSION_PASS='你的密码' python3 /opt/fission/gen-xray.py`。
>
> 配置要点：N 个 `freedom` 出站，每个 `sendThrough` 绑 `/64` 内一个不同源；`domainStrategy: UseIPv6` 强制代理解析 AAAA；`balancer` `random` 每连接随机选一个出口 = 每次源 IP 不同。

### 4.3 防火墙白名单（跨机部署必须）
代理监听 `0.0.0.0`，必须用防火墙限定只让 grok2api 那台连。把 `107.182.173.206` 换成你 grok2api 机器的公网 IP，`41080` 换成你的端口：

```bash
cat > /opt/fission/firewall.sh <<'EOF'
#!/bin/sh
iptables -C INPUT -i lo -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -i lo -j ACCEPT
iptables -C INPUT -p tcp --dport 41080 -s 107.182.173.206 -j ACCEPT 2>/dev/null \
  || iptables -I INPUT 1 -p tcp --dport 41080 -s 107.182.173.206 -j ACCEPT
iptables -C INPUT -p tcp --dport 41080 -j DROP 2>/dev/null \
  || iptables -A INPUT -p tcp --dport 41080 -j DROP
EOF
chmod +x /opt/fission/firewall.sh
/opt/fission/firewall.sh
iptables -L INPUT -n --line-numbers | grep -E '41080|lo '
```

持久化（重启自动恢复这 3 条）：

```bash
cat > /etc/systemd/system/fission-firewall.service <<'EOF'
[Unit]
Description=Fission proxy firewall rules
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/fission/firewall.sh

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now fission-firewall
```

> 这套只对 `41080` 端口生效，不影响 SSH 等其它端口。若这台机器还跑了 Docker，Docker 会改 iptables 的 `DOCKER` 链；本规则在 `INPUT` 链，不冲突，但建议裂变代理机**只跑 Xray、不跑 Docker**，避免链路复杂化。

### 4.4 启动 Xray
```bash
systemctl enable --now xray
systemctl restart xray          # 改过配置后必须 restart，--now 对已运行服务不会重载配置
sleep 1
systemctl is-active xray        # 期望: active
ss -tlnp | grep 41080           # 期望: LISTEN 0.0.0.0:41080
journalctl -u xray -n 20 --no-pager
```

> 踩坑：`systemctl enable --now xray` 对已经在跑的 xray **不会重启**，新配置不会加载。改配置后必须显式 `systemctl restart xray`。
>
> 那条 `Special user nobody configured, this is not safe!` 只是 systemd 提醒 xray 以 `nobody` 跑，不致命；`ip_nonlocal_bind=1` 是全局开关，`nobody` 也能绑非本地 v6 源，不影响裂变。

### 4.5 本机验证裂变
```bash
USER_PASS=$(paste -sd: /opt/fission/auth.txt)   # user:pass
for i in 1 2 3 4; do
  echo "=== req $i ==="
  curl -s --max-time 15 --socks5-hostname "$USER_PASS@127.0.0.1:41080" \
    https://v6.ipinfo.io/json | grep -o '"ip": "[^"]*"'
done
```

**期望**：4 次各返回一个不同的 `"ip": "2a01:4ff:1f0:c500::XXXX"` → random balancer 在 128 个源里随机轮换，裂变生效。

### 4.6 跨机验证（在 grok2api 那台机器上跑）
先在 Hetzner 上拿公网 IPv4：`curl -s https://ipinfo.io/ip`（示例 `5.78.101.22`）。然后在 **grok2api 那台**执行：

```bash
for i in 1 2 3 4; do
  echo "=== req $i ==="
  curl -s --max-time 20 --socks5-hostname fission:<密码>@5.78.101.22:41080 \
    https://v6.ipinfo.io/json | grep -o '"ip": "[^"]*"'
done
```

**期望**：仍返回不同 v6 → 跨机链路通 + 防火墙白名单生效。若全超时/拒绝 → 防火墙没放对 IP，或 grok2api 出口 IP 不是你以为的那个（可能前面有 NAT），在 grok2api 那台 `curl -s https://ipinfo.io/ip` 核对真实出口 IP 后改防火墙。

---

## 5. 接入 grok2api

### 5.1 关掉 Quality Guard（采用本方案的前提）
本方案的判断：fission 每连接换 IP，已经在 L4 根治"IP 维降智"，所以质量守护里围绕"稳定出口 IP"建的那套——分层冷却（二级）、字面量 IP 黑名单（三级）、`rotationURL` 换 IP——对 fission 节点全是 no-op，**直接关掉，不启用**。

`config.yaml`：
```yaml
qualityGuard:
  enabled: false
```
重启：`docker compose restart grok2api`（或你的启动方式）。

> qualityGuard 默认就是 `false`，从没开过则无需改动。关掉后保留的基座是**出口管理器**（节点探测、健康标记、fallback、账号分配）——这不是"二级三级"，是让"配一个代理节点就能用"的基础设施，要留着。
>
> **关掉后放弃的**：自动"流口水"检测。在"fission 能解决 IP 维降智"成立的前提下可接受；若真出现非 IP 维的账号/模型退化，手动处理或重新打开 qualityGuard。

### 5.2 加出口节点

**方式 A：管理端 UI**
管理端 → 出口 → 作用域选 `grok_build`（降智主要影响 Build 推理；Web/Console 按需另建）→ 新建节点：

- 名称：`hetzner-fission-v6`
- 代理 URL：`socks5h://fission:<密码>@<hetzner-public-ip>:41080`
- 账号容量：按需（如 `4`）
- 启用：✓

**方式 B：管理 API**（响应统一包在 `.data` 下）
```bash
ADMIN="http://127.0.0.1:8000"   # 改成你的管理端地址
TOK="<你的 admin JWT>"

# 建节点
curl -s -X POST "$ADMIN/egress-nodes" \
  -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
  -d '{
    "name":"hetzner-fission-v6",
    "scope":"grok_build",
    "enabled":true,
    "proxyURL":"socks5h://fission:<密码>@<hetzner-public-ip>:41080",
    "accountCapacity":4
  }' | jq '.data.id'
# 记下返回的节点 id（.data.id，字符串）
```

### 5.3 探测验证
**UI**：保存后对该节点点"探测"/"测试"，连点几次。

**API**：
```bash
NID="<上一步的节点 id>"
curl -s -X POST "$ADMIN/egress-nodes/$NID/test" \
  -H "Authorization: Bearer $TOK" \
  | jq '.data | {status, exitIp, ipv4:.ipv4.status, ipv6:.ipv6.status, error}'
```

**判读**：
- `status = "healthy"`
- `exitIp` 是 `2a01:4ff:1f0:c500::xxxx` 形式 v6，多次探测**每次不同**
- `ipv4 = "unhealthy"`、`ipv6 = "healthy"` —— **这是预期的**（Xray `UseIPv6` 不走 v4，但任一家通节点就判 healthy；见 `manager.go:457-460`）

> `socks5h://`（h = 远程 DNS）很关键：让代理来做域名解析并强制走 v6。grok2api 用 `golang.org/x/net/proxy`，`socks5`/`socks5h` 走同一个 dialer、把域名以 FQDN 原样转给代理（远程 DNS），叠加 Xray `UseIPv6` 解析 AAAA → 从随机 `sendThrough` v6 出去。填 `socks5://` 也等效，但 `socks5h://` 是最稳写法。

### 5.4 配 fallback（唯一该加的保险）
fission 节点挂了/被探测判 unhealthy 时，流量去哪。给 `grok_build` 作用域配回退：

- `none`：无回退，fission 挂了就报错（不推荐）
- `direct`：回退本机 IP（降智路，仅兜底）
- `fixed`：回退到另一个代理节点（最稳，需要先有第二个节点）

**UI**：管理端"运行设置 → 出口 / 回退"里给 `grok_build` 选回退模式（`fixed` 时指定节点）。

**API**：`PUT /egress-operations` 是**整对象替换**（`ProbeIntervalSeconds` 等字段会按传入值覆盖，且要求 60–86400）。所以先 GET 当前值，改 `fallbacks` 后整体 PUT 回去：
```bash
# 1) 取当前配置
curl -s "$ADMIN/egress-operations" -H "Authorization: Bearer $TOK" > /tmp/ops.json
jq '.data' /tmp/ops.json

# 2) 只改 fallbacks.grok_build（这里示例设为 direct；fixed 则 mode:"fixed" + nodeId:"<第二个节点id>"）
jq '.data | {
  probeProvider, probeIntervalSeconds, autoAssignEnabled, autoBalanceEnabled,
  directAccountCapacity, assignmentIntervalSeconds,
  fallbacks: (.fallbacks | .grok_build = {mode:"direct"} | .)
}' /tmp/ops.json > /tmp/ops_put.json

# 3) 整体 PUT
curl -s -X PUT "$ADMIN/egress-operations" \
  -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
  --data @/tmp/ops_put.json | jq '.data.fallbacks'
```
至少给 `grok_build` 配一个 fallback，别让 fission 单点。

### 5.5 绑定账号到节点
让 Build 账号走这个出口：把账号**手动绑定**到该节点（绑定后不会被自动调配改走别处）。

**UI**：出口节点详情 → 分配账号 → 选 Build 账号 → 绑定（手动模式）。

**API**：
```bash
curl -s -X POST "$ADMIN/egress-nodes/$NID/accounts" \
  -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
  -d '{"provider":"grok_build","ids":["<账号id1>","<账号id2>"],"mode":"manual"}' \
  | jq
```

### 5.6 端到端验证
用绑定的账号发一个真实推理请求（如 `/v1/chat/completions` 流式），确认能正常流式输出。至此链路闭环。

---

## 6. 运维与故障排查

### 6.1 日常检查（在 Hetzner 裂变机上）
```bash
systemctl is-active xray fission-firewall ipv6-fission
ss -tlnp | grep 41080
ip -6 route show table local | grep -c c500   # 期望 >=1
# 抽查出口仍随机
USER_PASS=$(paste -sd: /opt/fission/auth.txt)
curl -s --socks5-hostname "$USER_PASS@127.0.0.1:41080" https://v6.ipinfo.io/json | grep -o '"ip":"[^"]*"'
```

### 6.2 常见问题

| 现象 | 排查 | 修复 |
|---|---|---|
| `curl ... socks5h` 全部 `Connection refused` | Xray 没监听端口 | `systemctl restart xray`；改过配置后必须 restart，`enable --now` 不会重载 |
| 本机 socks 通、跨机不通 | 防火墙没放对 IP | 在 grok2api 机 `curl -s https://ipinfo.io/ip` 拿真实出口 IP，改 `firewall.sh` 里的 `-s` 后 `systemctl restart fission-firewall` |
| 出口 IP 不再随机（每次相同） | balancer 或 sendThrough 失效 | `journalctl -u xray -n 50`；确认 AnyIP 路由在：`ip -6 route show table local \| grep c500`；`sysctl net.ipv6.ip_nonlocal_bind` 应为 1 |
| 节点探测 `status=unhealthy` | v6 出口断了 | 回 Hetzner 跑 4.5 本机验证；不通则查 `ipv6-fission` 服务、地址、默认路由 |
| 节点探测 `ipv4=unhealthy` | **正常现象** | Xray `UseIPv6` 不走 v4，v6 通则节点 healthy，无需处理 |
| 重启 Hetzner 后出口失效 | 持久化 unit 没起来 | `systemctl status ipv6-fission fission-firewall xray`；`systemctl enable` 三个服务 |
| `ip -6 route show` 看不到 AnyIP 路由 | 看错表 | 用 `ip -6 route show table local`，main 表看不到 local 路由 |
| 改了 `gen-xray.py` 重跑后没生效 | Xray 没重载 | `systemctl restart xray`（重新读 config.json） |

### 6.3 改端口 / 改密码 / 改出口数量
改 `gen-xray.py` 顶部的 `PORT`/`USER`/`N`/`PREFIX`，重跑 `python3 /opt/fission/gen-xray.py`，`xray run -test -config ...` 校验，`systemctl restart xray`。若改了端口，同步改 `firewall.sh` 里的 `--dport` 并 `systemctl restart fission-firewall`，以及 grok2api 出口节点 URL。

---

## 附 A：关键参数速查

| 项 | 值 |
|---|---|
| Hetzner /64 子网 | `2a01:4ff:1f0:c500::/64`（示例，换成你的） |
| Hetzner v6 网关 | `fe80::1` |
| 裂变代理监听 | `0.0.0.0:41080`（socks5，密码认证） |
| 裂变代理用户 | `fission` |
| 裂变代理密码 | 见 Hetzner 上 `/opt/fission/auth.txt` |
| Hetzner 公网 IPv4 | `<hetzner-public-ip>`（`curl -s https://ipinfo.io/ip`） |
| grok2api 出口 URL | `socks5h://fission:<密码>@<hetzner-public-ip>:41080` |
| grok2api 机器白名单 IP | `<grok2api-public-ip>` |
| grok2api 作用域 | `grok_build` |

## 附 B：持久化清单（重启 Hetzner 后自动恢复）

- `/etc/sysctl.d/99-fission.conf` — `ip_nonlocal_bind=1`
- `/etc/systemd/system/ipv6-fission.service` — v6 地址 + 默认路由 + AnyIP 本地路由
- `/opt/fission/gen-xray.py` + `/usr/local/etc/xray/config.json` — Xray 配置
- `/opt/fission/firewall.sh` + `/etc/systemd/system/fission-firewall.service` — 防火墙
- `xray.service` — Xray 守护

## 附 C：同机部署变体

如果 grok2api **就跑在 Hetzner 这台裂变机上**，可简化：
- Xray 仍按第 4 节配（监听 `0.0.0.0:41080` 带认证），grok2api 直接用 `socks5h://fission:<密码>@127.0.0.1:41080`，经回环走代理。
- 防火墙那 3 条规则可不要（只有本机连）；或保留仅放 lo。
- 其余步骤不变。

> `0.0.0.0` 监听已含 `127.0.0.1`，所以同机不用改 Xray 配置，只是跨机才需要防火墙白名单。