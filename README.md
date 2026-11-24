# fanout

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 的公共节点变成本地 SOCKS5 端口：一个端口一个出口 IP。
再给每个出口挂一个节点链接，客户端连哪个端口就从哪个国家出去。

节点链接有三种管法：同机装了 3x-ui 或 xray-cf-lite 就接管它们的入站，都没装则
fanout 自己跑 Xray——建站、改站、发链接都在同一个界面里。

> **这是 [byJoey/fanout](https://github.com/byJoey/fanout) 的衍生版本。**
> 原始项目、架构设计和绝大部分代码都属于 [@byJoey](https://github.com/byJoey)，
> 本仓库只做了两件事：**重写管理界面**和**加固连接层**。
> 上游仍在活跃维护，功能性的问题请先看上游或直接去
> [byJoey/fanout](https://github.com/byJoey/fanout) 提 issue。
> 逐版本的改动记录在 [CHANGELOG.md](CHANGELOG.md)。

## 功能一览

- **一出口一 netns**：openvpn 的路由劫持只影响自己的命名空间，母机网络不受影响。
- **管理界面重写**：侧栏导航 + 出口 / 节点链接 / 设置 / 日志四个视图，浅色默认、
  深色可切，前端资源用 `go:embed` 打进二进制，部署仍然只有一个可执行文件。
- **顶栏实时负载**：CPU、内存、交换、存储、负载、TCP、UDP、上行、下行，全部直接取自 `/proc`。
- **出口组**：一条节点链接绑几条出口，组内单一 / 轮询 / 负载均衡 / 并发选路，
  还能打开「掉线自动补新出口」——先拿此刻在线的出口顶替，在线的不够才新建。
- **WARP 中转（可选）**：链路变成「母机 → Cloudflare WARP → 节点」，节点日志里记的来源
  不再是你的 VPS。
- **三种后端同源**：3x-ui 接管、自建 Xray、xray-cf-lite 接管，操作一致。
- **内置客户端**：Xray / openvpn / WARP 的版本基准与就地升级，含 openvpn 源码编译这条路。

## 原理

每个节点跑在独立的 network namespace 里，netns 内启动官方 openvpn 客户端。
SOCKS5 监听在母机，出站连接用 `setns` 切进对应 netns 建立。

```
客户端 ──> 母机 SOCKS5 :端口 ──> netns foN ──> openvpn ──> VPN Gate 节点

开了 WARP 中转，netns 里多一跳，出去找节点的不再是母机 IP：
客户端 ──> 母机 SOCKS5 ──> netns foN ── WireGuard ──> Cloudflare WARP ──> VPN Gate 节点
```

## 安装

需要 root，Linux（依赖 netns）。Windows / macOS 不能跑，只能交叉编译。

### 一行装（预编译，推荐）

```bash
REPO=MIGAO22/fanout bash <(curl -fsSL https://raw.githubusercontent.com/MIGAO22/fanout/main/install.sh)
```

拿的是 [Releases](https://github.com/MIGAO22/fanout/releases/latest) 里由
[release workflow](.github/workflows/release.yml) 构建的
`fanout-linux-amd64.tar.gz` / `fanout-linux-arm64.tar.gz`，机器上不用装 Go。

`REPO=` 要带：`bash <(curl …)` 这种跑法不在 git 目录里，脚本推断不出来源仓库会直接停手，
**绝不退回上游**——上游二进制没有本仓库的界面重写和连接层加固，装上后界面长得像、
版本号也像，事后根本发现不了。

### 从源码装

```bash
git clone https://github.com/MIGAO22/fanout.git
cd fanout && bash install.sh
```

在源码目录里脚本自动改走编译路径（需要 Go 1.21+），仓库名从 `origin` 推断，不用带 `REPO=`。

两条路径的依赖处理一样：openvpn / curl / openssl / iproute / iptables 按发行版自动装，
apt、dnf、yum、pacman、apk、zypper 都认。没装 3x-ui 时顺带下载一份 Xray 到
`/var/lib/fanout/bin/`，装了则跳过（入站交给面板管）。只有开「WARP 中转」才需要的
`wireguard-tools` 也顺手装，但**装不上不会让安装失败**——那是可选功能，拨开关时程序自己还会再装。
服务用 systemd 或 OpenRC 都能装，装完自动开机自启。

**Alpine** 默认不带 bash：`apk add bash curl`。

另外 fanout 要在 netns 里跑 openvpn，**宿主必须放开 `/dev/net/tun`**。不少 LXC 小鸡没给这个
权限，`ls /dev/net/tun` 不存在且 `mknod` 报 Operation not permitted 的话，这台机器用不了，
跟发行版无关。

装完会打印管理界面地址、访问路径和口令：

```
管理界面  http://<你的IP>:8899/gwPuWHvaNr/
访问口令  f81120ac328d11c11b
```

路径和口令都是随机生成的，分别存在 `/var/lib/fanout/basepath` 和 `/var/lib/fanout/password`。
路径不对一律返回 404，扫端口的看不到这里跑着什么。

### 版本、更新与自更新来源

- 推一个 `v*` tag 就触发 release workflow 出 amd64 / arm64 两个包和 `checksums.txt`；
  不打 tag 就没有 Release，`releases/latest/download/...` 是 404，一行装会直接失败。
- `fanout -version` 用来分清手上是哪一份：本仓库构建的显示 `v2.1.5`，上游原版是 `v1.3.1`。
- 自更新的默认目标就是本仓库（`updateRepo`），构建时 release workflow 还会再用
  `-X main.updateRepo=$GITHUB_REPOSITORY` 钉一遍。自己手动 `go build` 又不想带 ldflags 时，
  用环境变量兜住：`FANOUT_UPDATE_REPO=MIGAO22/fanout`。
- 界面用 `go:embed` 打在二进制里，**换二进制必须重启服务才生效**：`f` → `3) 重启`。
  `install.sh` 重装时会显式 `systemctl restart`，不会出现「装完没变化」。

## 使用

界面以**出口**为单位：一行就是一条隧道加上挂在它上面的节点链接。

点「新建出口」，选地区和数量，再选一个已有节点作模板，提交后 fanout 并行拉起隧道、
为每个出口复制一份节点链接并绑好，进度按目标逐条回报。地区里还有个「每个国家」：
每个有空闲节点的国家各开几条，一次把所有地区铺开；槽位不够时先开节点多的地区。

每行右侧两个按钮：换一个节点（出口 IP 变、端口不变，已分发的客户端配置不用改），
或者停掉这个出口。换节点会避开这条出口之前用过的，连点几次每次都是新 IP。
节点自己声明支持 AEAD 的那几条，卡片上会挂一个 `AEAD` 标记（见下面「AEAD 与内核 offload」）。

「节点链接」视图是全部入站的表格：备注、协议、端口、走哪条出口、状态、详情。
点进详情可以改端口、备注、启停、管理客户端，以及改绑到别的出口。一个入站可以挂多套
客户端凭据，分发给不同的人；每套都能单独重置，重置后旧链接立即失效。
没绑出口的入站单独列在末尾，附「批量清理」和就地改绑下拉。

### 订阅

节点多了就别一条条复制了。点「订阅」拿一条地址填进客户端，以后加出口、删出口都会自己跟上。
换节点不影响它——端口不变，链接也不变。

地址形如 `http://<服务器IP>:<端口>/<访问路径>/sub?token=<口令>`。默认出 base64，各家客户端通吃；
想看明文加 `&target=links`；默认只放绑了出口的节点，想全都要加 `&bound=0`。

订阅要给客户端直接拉，所以这条地址不要登录——**那串 token 就等于密码**，别发群里。
泄露了点「换一串口令」，旧地址立刻失效。

### 只用家宽

VPN Gate 的清单里混着一批它自己的机房服务器（`public-vpn-*` 和 `219.100.37.0/24`），
出口一眼看得出是数据中心，还更容易满员。设置里「只用家宽节点」默认开着，挑节点、
地区可用数、自动重连的备选都只看志愿者家宽。想连机房的一起用就关掉。
开关只管新挑的节点，已经跑着的出口不会因为改设置被换掉。

### 顶栏负载

顶栏那一排胶囊看的是**母机**（跑 fanout 的这台），不是隧道内部，也不是你的客户端。
每 2.5 秒自己走一次，鼠标停上去有完整口径，数值全部直接来自 `/proc`，和 SSH 里看到的能对上：

| 胶囊 | 取数 | 说明 |
| - | - | - |
| CPU | `/proc/stat` 两次采样的差值，`idle` 含 `iowait` | 差值口径，所以第一次要等一个采样窗口（200 ms）；单次读只能拿到开机以来的累计值，那个数没意义 |
| 内存 / 交换 | `/proc/meminfo` 的 `Total - Available` / `SwapTotal - SwapFree` | 老内核没有 `MemAvailable` 时退回 `MemFree`，会把 cache 算成占用；没配交换则胶囊置灰显示「无」 |
| 存储 | `statfs` 取**工作目录所在分区**，取不到才退回 `/` | 百分比分母剔掉 root 预留块，和 `df -h` 的 `Use%` 一致；悬停写明统计的是哪个挂载点 |
| 负载 | `/proc/loadavg` 的 1 分钟值 | 按核心数归一，100% = 可运行任务刚好排满 CPU |
| TCP / UDP | `/proc/net/sockstat` **与** `sockstat6` 的在用数，TCP 再加 `tw` | IPv4/IPv6 是两张独立的表，只读 IPv4 那份会把客户端全从 IPv6 进来的机器显示成一个不动的 0；内核这两行有的印 `inuse`、老内核印 `in use`，两种都认。`TIME_WAIT` 算进 TCP 是有意的：它占的正是这台机器往外连新目标时最先耗尽的临时端口 |
| 上行 / 下行 | `/proc/net/dev` 两次采样的差值 ÷ 间隔 | 只算默认路由那块网卡，是网卡上的量（含 openvpn 封装开销、含 IPv6 流量），不是客户端看到的量；首次采样没有前值可减，显示 `—` 而不是 0 |

超过 75% 转黄、90% 转红。这一排在任何宽度下都不藏东西：宽屏一行，放不下折两行，
手机（≤860 px）压成一行可以左右滑。

### 出口组：一条链接同时绑几个出口

VPN Gate 的志愿者家宽说掉就掉。一条链接只绑一条出口时，客户端只能跟着遭殃。
「出口组」把几条出口编在一起，入站绑到组上，组内怎么选路由 fanout 负责：

| 策略 | 行为 | 适合 |
| --- | --- | --- |
| 单一出口 | 咬住一条，不通才换组内下一条 | 想要一个尽量不变的出口 IP |
| 轮询出口 | 每 N 个新目标换一条（N 默认 5） | 把流量摊到几条出口上 |
| 负载均衡出口 | 按实测拨号延迟挑最快的，当前这条明显变差就切走 | 在意速度，能接受偶尔换 IP |
| 并发出口 | 同时向组内所有可用出口发起，谁先通用谁 | 只要最快的那一次，愿意多占几路会话 |

轮询和负载均衡**按目标粘性**：同一个站点（不看端口）反复访问始终走同一条出口，否则刷一个
页面就会在同一站点上跳出好几个来源 IP。粘性表有上限，按最早未使用淘汰。

实现上的几条约定：

- 每个组是 fanout 自己监听在 `127.0.0.1` 的一个 SOCKS5。Xray 侧看到的仍是普通 socks 出站，
  三种后端的配置格式一个字都不用改，组内换成员也不需要重载 Xray。组只监听回环，不对外服务。
- 组员记的是节点主机名，不是槽位号。某条出口自动换了节点，组会跟着改名，绑定关系不动。
- 组内一条可用出口都没有时连接**直接失败**，绝不退回母机直连——那等于把真实出口 IP 暴露出去。
- 单条成员的拨号预算是 8 秒（单出口那条 15 秒）：保活靠的就是快速降级。刚失败的成员进冷却、
  排到降级队列最后，但不会被永久排除——全组都在冷却时照样挨个再试。
- 负载均衡不只看真实流量：没有近期样本的成员会被后台测速补样，否则闲一会儿就变成盲选。

#### 掉线自动补新出口

组编辑框里的开关。打开后组会维持你指定的可用条数：某条成员不行了就自动补一条能用的顶替它，
绑在组上的节点链接一个字节都不用改。关着就是纯手动，组员少一条就是少一条。

- **先拿此刻在线的出口顶上，新建是最后手段**。顶替只是改一份成员名单——瞬时完成，
  不等 openvpn 握手、不占新槽位。在线的不够才新建，且**不限地区**：要的是「数量立刻回来」，
  跟着死成员的地区挑恰恰最容易挑到同一批一起挂的机房。优先挑没挂在任何组里的那些。
- **你设的维持条数不会因为掉线变小**，也不会因为补不满变小；补不齐就在组上写明还差几条、
  为什么差着，下一轮接着试。
- **掉线分三种，等不等分开看**。还在自救（`starting`）的等 90 秒——家宽节点掉一下大多几十秒
  自己回来，立刻补是白烧槽位；重连跑完一轮仍没回来（`failed`/`stopped`）的不等；
  隧道整个没了的也不等，没有任何东西会把它带回来。
- **补位名额只给确定没人会把它带回来的坑**（已判死的、名单本身比目标短的）。还在自救窗口里的
  不给：提前占位会让组员名单比设的条数长，刚重启时组员全在这一档，不给就等于把别的组的
  在线出口整批抢成自己的组员。
- **顶替原位，不重排**（单一模式的默认首选就是成员列表第一条）。换掉之后把原来那条收掉，
  除非它自己连回来了、还有别的组指着它、或有入站直接绑着它。
- **先连上才算数**：新建那条要真的起了 tun0、状态到 `up` 才写进组里，否则垃圾隧道会一轮轮填满槽位。
- 给槽位留水位（默认留 2 个空槽），余量不够时先补能补的，剩下的写在组上。
  试过的死节点冷却 10 分钟。后台每 10 秒巡检一次，上一轮没跑完就跳过这一轮。

补位是程序里唯一会「自己开出口」的路径，所以它只在你打开开关时动作，并且每条补上来的出口
和手动开的完全一样：独立 netns、独立 SOCKS5 端口，界面上看得见，也能单独停掉。

### WARP 中转：让节点看到的是 Cloudflare

**它解决什么。** 默认链路是「母机 → 节点」，VPN Gate 的节点在握手时就拿到你母机的公网 IP。
开了中转，netns 里的默认路由先进 Cloudflare，节点那侧的来源变成 Cloudflare 的出口。

**开关在哪。** 出口总览页 KPI 下面那条「WARP 中转」。拨开之后 fanout 给每条出口向 Cloudflare
注册一份独立的免费 WARP（各一对 WireGuard 密钥，落盘 `/var/lib/fanout/warp.json`，0600），
在它的 netns 里造一台 WireGuard 设备，把默认路由换成它，再把 Cloudflare 接入点和 DNS 那几个
地址钉回直连侧，然后**逐条重连已有的出口**——只作用于新建出口是没用的：界面显示「已开启」、
出口照旧是绿的、出口 IP 也没变，用户以为流量已经进 Cloudflare 了，实际一条都没进。

**装上就能用。** 唯一的硬依赖是 `wg`（包名 `wireguard-tools`），由程序自己补：拨开关时本机没有，
作业第一步就按 `/etc/os-release` 认出发行版、用对应包管理器装上，装完才注册、才动出口。
三条路都补这一步：翻开开关、按新版本重新注册、建出口前的预检。额度分开给（开关 5 分钟、
建出口时 90 秒），失败后 3 分钟内不再重试，且和 openvpn 的升级共用同一把锁——dpkg 锁只有一把。
认不出 OpenWrt 这类发行版时不假装能自动装：开关标「不可用」，把「请手动安装 wireguard-tools」
那句原话写在说明里。另一条前提是够得着 `api.cloudflareclient.com`，够不着时注册那一步会红，
开关自动退回关闭，一条出口都不会被动过。

**每条出口卡片上的「中转」。** 那串是此刻真正在用的 Cloudflare 接入点，取自 netns 里
`wg show dump` 的内核实况，不是注册响应里存的那个；右边「已握手 / 未握手」是握手新鲜度。
它和「出口」那行是两件事：中转只是中间一跳，对外暴露的身份仍然是节点的出口 IP。

**代价，说全。**

- 多一跳就多花时间：单节点连接预算自动加 12 秒，否则节点会因为这一跳被误判成「连不上」而白白换掉。
- MTU 逐层缩，而且**按母机那块真网卡算**，不是按 1500 写死。链路正常时 WireGuard 1420 /
  tun 1380 / mssfix 1340；母机出口是 1450 的隧道或 1400 的 overlay 时三层一起往下缩，
  下限夹在 WireGuard 规定的 1280。必须跟着缩的原因：外层包带 DF，一超过链路能扛的大小就静默丢掉，
  而 openvpn 最吃大包的 TLS 握手正好卡在那儿——症状是「所有节点都连不上」，看着像 VPN Gate 全死了，
  其实是我们把包发大了。关掉中转时一个参数都不加。
- 服务器推送的 MTU 会被挡掉（`--pull-filter ignore tun-mtu` / `link-mtu` / `mssfix`，openvpn 2.4 起；
  2.3 没有这个选项，所以按版本给）。推送的优先级比命令行高，而 Gate 上确实有节点推 `tun-mtu 1500`。
- openvpn 侧强制 `--route-nopull`。Gate 的配置几乎都推 `redirect-gateway`，让它照推就会拿 tun0
  跟 WARP 抢默认路由。分流改由 fanout 自己装：Gate 的地址钉在 WireGuard 设备上，
  用户流量走 `0.0.0.0/1` + `128.0.0.0/1` 进 tun0。
- **失败即闭合**：Cloudflare 没握手时这条出口就是连不上，而不是悄悄从母机出去。
- **反方向也闭合**：openvpn 没连着时 netns 里的默认路由会被撤掉，用户流量不许只走到 Cloudflare
  就出去；探测到的出口 IP 落在 Cloudflare 名下（按 ARIN/RIPE 登记的 CLOUDFLARENET 段判）时，
  直接判成「只走到中转，没走节点」。节点地址那条 /32 提前钉好，所以 openvpn 自己照样连得回来。

**连不上时会说清是哪一种。** 同一条「没连上」背后可能是节点死了、认证被拒、证书过期、
外层包发大了，也可能只是 WARP 那一跳没握手。现在 openvpn 日志里那句关键报错
（`AUTH_FAILED`、`Message too long`、`Options error`、`SSL alert` …）和 WARP 实况一起写进出口卡片
与作业那一步：`WARP 已握手（接入点 …，3 秒前）` 说明该怀疑节点，
`WARP 未握手（接入点 …）——是 Cloudflare 这一跳不通，换节点不会有用` 说明换节点没用。
翻开开关后如果第一条就撞上「未握手」，剩下的出口不再一条条去撞同一堵墙：作业当场停下、
开关退回关闭，并把这句原因留在标题上。

**这算保护隐私吗。** 只算一半。它挡住的是「节点那侧记到你母机 IP」，换来的是 Cloudflare
这一侧能看到连接时间和流量元数据，它的对端仍然是它自己的机房。要防的是内容，那得靠端到端
加密（HTTPS），跟这一层无关。

**别拿来干什么。** 免费 WARP 的条款面向个人设备使用，把它当服务器端的常驻中转线路属于灰色地带，
责任在你；同一条出口每次重连都重新注册会被判成滥用，之后拿不到接入点。所以注册记录一律落盘复用，
只有你按「重新注册」时才换新的；关掉开关会把名下注册全部退掉，不留僵尸设备。

### 内置客户端（Xray / openvpn / WARP）

「设置与连接 → 内置客户端」把本机这几份客户端的来历和版本一次列清楚，并且只更新该由它更新的：

| 组件 | 装在哪 | 谁负责升级 |
| --- | --- | --- |
| Xray（自建模式） | `/var/lib/fanout/bin/xray` | 这张卡里的「更新到 vX」 |
| openvpn（母机连出口那一跳） | 系统包，`PATH` 里那份 | 这张卡里的「升级 openvpn」，实际执行本机包管理器 |
| 3x-ui / xray-cf-lite 的 Xray | 各自的目录 | 跟着它们自己的面板或脚本 |
| WARP 中转（可选那一跳） | 内核 WireGuard，没有二进制 | 「按 <版本> 重新注册 WARP」，改的是上报版本号和注册记录 |

先说清两件容易混的事：母机连出口用的**不是** Xray，是系统里的 openvpn，跑在 netns 内；
出口那侧什么都不用装（连的是别人开着的 OpenVPN 服务）。真正被内置进来、又一直没人负责更新的
只有 Xray——`install.sh` 是「已存在就跳过」，装它那天是最新版，之后无论重装多少次都不碰它，
REALITY、XHTTP 这些只在新版里有的东西就会一直缺席。

**Xray 的更新顺序**（任何一步不对都不动真文件）：拉本机架构的包解出 `xray` → **先试跑**新二进制
（下错架构、缺动态库在这里就拦掉）→ 用它校验当前配置 → 同目录 `rename` 原子替换、旧版留在
`xray.prev` → 用同一份配置重新拉起，起不来就把 `xray.prev` 放回原位。第 5 步会重启一次 Xray，
已连上的节点链接会掉一下再自己重连，所以点下去前有确认框。接管模式下这张卡只显示版本、
不给按钮，并说明那份归谁管。

**两个「最新版」。** Xray 和 openvpn 的最新版基准都对接 GitHub **官方 release**
（`XTLS/Xray-core`、`OpenVPN/openvpn`），跟本机 `openvpn --version` 读出来的号一个写法。
之前拿各台机器自己的发行版仓库当基准，才出现「三台机器三个 openvpn 版本、个个都叫最新」。

openvpn 的升级按钮上那个号是**问出来的**：发行版仓库和 OpenVPN 官方 apt 源各问一次、取更高的，
而且必须严格比本机新才给按钮（一样高或更旧就不给，否则一次「升级」其实是往回降）。问法是先刷
一次索引再读（`apt-get update -qq` + `apt-cache policy` 等，各家族一套）——`apt-cache policy`
看的是磁盘上那份本地索引，几个月不刷时 Candidate 永远等于已装版本。撞锁会隔几秒重试一次，
还不行就照实写「索引没刷新，仓库版本号可能偏旧」，不假装查到了。

官方二进制仓库**只在 deb 上**，形状是
`deb … https://build.openvpn.net/debian/openvpn/<channel> <codename> main`，channel 只碰
`release/2.7` 和 `stable`。fanout 直接读它的 `Packages` 索引比版本，判到确实比本机新才写源，
落盘前后都有闸：公钥先核 sha256 与指纹（`1937b44f…45ce710d` /
`30EB F4E7 3CCE 63EE E124 DD27 8E6D A8B4 E158 C569`）才对得上；`signed-by` 只指向这把钥匙；
codename / channel / arch 全部白名单校验通过才写文件，写是 `os.WriteFile` + 同目录 `rename`，
不经过 shell。这个源比想象中旧（实测 noble 只到 `2.6.10`、bookworm `2.6.0`、jammy `2.5.7`，
全都低于 Ubuntu 自己仓库的 `2.6.19`；resolute 才有 `2.7.1`），所以「接官方源」永远是结果比本机新
才发生。接了还给一条等号的原路退回：「撤销官方源」删掉 fanout 写的那两份文件并刷一次索引，
已装好的 openvpn 不动。RHEL 系有官方 Fedora Copr（`@OpenVPN/openvpn-release-2.7`），fanout
不替你启用，但卡片上会把这句说出来，免得把「我的源里没有」讲成「官方没有」。

真按下去之前，确认框会把**打算在你机器上执行的命令原样摊开**。除刷索引那步，每条命令都只点
`openvpn` 这一个包（有单测守着：命令里出现 `upgrade` 而不含包名就红）。升级不会打断已经在跑的
隧道——替换二进制只是换目录项，旧进程还挂在原 inode 上，所以立刻生效的是新建出口、换节点、
重启服务这些新起的进程，这句也写在确认框里。

**apt 到不了的地方还有源码编译这条路。** 官方 apt 源的 `release/2.7` 通道在不少发行版上只喂到
预发布（实测 noble / bookworm / jammy / plucky 是 `2.7~alpha1`，trixie `2.7~beta1`，
questing `2.7~rc1`）——数字上比 2.6.19 大，实际比本机那份旧两年，所以 fanout 认 Debian 语义、
见 `~` 就拒。这种机器想上官方最新只剩自己编，于是多一颗**「自己编译 openvpn <官方版本号>」**，
只在四个条件同时成立时出现：deb 家族、确实比官方 release 旧、发行版仓库加官方源都够不到那个号、
那个号还没编好。实际动作：

```
apt-get install -y --no-install-recommends build-essential pkg-config libssl-dev libcap-ng-dev liblzo2-dev liblz4-dev gnupg
下载 openvpn-<版本>.tar.gz 和它的 .asc，核对 GitHub 在 release 资产上自己报的 sha256
用 OpenVPN 官方主钥匙验签（F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7）
./configure --prefix=/usr/local/fanout-openvpn/<版本> --disable-dco --disable-plugin-auth-pam --disable-unit-tests
make -j<N> && make install
```

几个决定是有意为之的：**装进独立目录树**，`/usr/sbin/openvpn` 一个字不改，隧道 exec 哪一份记在
`/var/lib/fanout/openvpn/source.json` 里，版本探测和 netns 那一跳走同一个解析入口（分头查就会
出现「按 2.7 的参数名喂给实际是 2.6 的二进制」）；**钉主钥匙而不是子钥匙**（子钥匙一年一换，
实测三个版本的 `.asc` 都验到同一把 2017 年生成的主钥匙，主钥匙 2027-02-07 到期）；
**`--disable-dco`**（DCO 要么靠内核 6.16+ 的 `ovpn` 模块要么装 out-of-tree kmod，都不该由一个
装客户端的按钮顺手引入）；**`--disable-plugin-auth-pam`**（那是服务端用来对本机 PAM 认证连进来的人的
插件，fanout 只出站连接，用不上）；libcap-ng / lzo / lz4 三个 `-dev` 是硬要求，缺任何一个是
`AC_MSG_ERROR`。**任何一步失败都不登记、删掉那棵树**，跑完还会核一次 `openvpn --version`。
卡片上有「撤销源码版」；自己编的这份**没人跟安全更新**，官方再出新版时这颗按钮会重新出现。
配方本身由 [`openvpn-src.yml`](.github/workflows/openvpn-src.yml) 在 ubuntu-24.04 上把整条链跑一遍钉住，
资产改名、钥匙轮换、依赖变硬要求都会先在那里红，而不是在用户的 VPS 上。

### AEAD 与内核 offload

openvpn 2.7 只要看见 `--data-ciphers-fallback` 就自己禁用 data channel offload（那句
`Note: --data-ciphers-fallback … disables data channel offload` 是它自己打的），而 fallback 正是
为了连上只讲 AES-256-CBC 的老节点才给的。所以「设置与连接 → 重试与面板」里有个**默认关着**的
「数据通道只要 AEAD（放开内核 offload）」：打开后算法表只留 AES-256-GCM / AES-128-GCM /
CHACHA20-POLY1305、不给 fallback、并补 `--allow-compression no`（同样是 offload 的前置条件），
加解密就落到内核里。

**这一档按节点的能力自动收敛，不是一刀切。** 真机捞到的 VPN Gate 配置是 SoftEther/PacketiX
自动生成的，里面直接写着 `data-ciphers AES-128-CBC`（实测一台机器上 18 份配置全是这一种），
服务端只会这一种——一刀切会把整个节点池关掉，报出来还只是「连不上」。所以只有节点配置自己声明了
AEAD、且没自己开压缩的出口才改参数。界面上这类出口挂一个 `AEAD` 标记（出口卡片）并在组员 chip 的
悬浮说明里写明，让你看得出开了这个开关之后**哪些**出口真会吃到内核加解密；Gate 那批不亮，
因为对它们这个开关本来就不起作用。2.4 及读不出版本时这个开关完全不起作用，行为与之前一致。

数据通道算法的参数名本身也按本机 openvpn 版本喂：2.4 叫 `--ncp-ciphers`，2.5 起叫
`--data-ciphers`（外加 `--data-ciphers-fallback`），2.3 及更早两个都没有。喂错名字不是警告而是
`Unrecognized option` 当场退出，所以先读 `openvpn --version` 再挑名字，读不出来就一个都不传。

### 节点链接从哪来

同机装了 3x-ui 就直接接管面板里的入站，面板端口、路径、API token 全自动探测，开了 SSL 也能识别。
没装 3x-ui 时 fanout 自己跑一个 Xray，界面上多一个「新建节点」按钮，可以选协议
（VLESS / VMess / Trojan）、传输（TCP / WebSocket / gRPC / HTTPUpgrade / XHTTP）和安全层
（无 / TLS / REALITY）。REALITY 的密钥对和 shortId 自动生成；TLS 不填证书就生成自签的，
分享链接会带上证书指纹让客户端固定信任，也可以填自己的证书路径。

装了 [xray-cf-lite](https://github.com/byJoey/xray-cf-lite) 的机器会自动接管它生成的三个节点。
这个模式下节点归 xray-cf-lite 管，fanout 只负责给每个节点指定走哪条出口，所以界面上不提供
新建、删除和改节点的入口。两边共用同一份 Xray 配置，fanout 只往里加自己前缀的出站和分流规则，
互不覆盖。

后端在设置面板里可以随时切换，本机没装的会置灰并说明原因；也可以用
`-panel 3x-ui` / `-panel native` / `-panel xray-cf-lite` 固定。界面里选过之后会记住，重启仍然生效。

## 连接参数

「设置与连接」里可调的东西：

| 参数 | 默认 | 作用 |
| --- | --- | --- |
| 健康检查间隔 | 10 s | 多久比对一次出口 IP 是否还是建立隧道时那个 |
| 连续失败判定 | 3 次 | 连续几次不符才判掉线，防抖 |
| 探活超时 | 6 s | 单次探测的超时 |
| 重连退避下限 / 上限 | 5 s / 60 s | 自动重连的指数退避区间 |
| 连接预算 | 35 s | 单个节点从拉起 openvpn 到拿到出口 IP 的总预算，超了就换下一个候选 |
| 每轮候选节点数 | 6 | 一轮里最多试几个候选 |
| 面板写入合并 | 1500 ms | Xray 重载的合并窗口 |
| 节点清单有效期 | 600 s | VPN Gate 那份 CSV 的缓存时长 |
| 挑节点排序 | 均衡 | 均衡 / 偏爱速度 / 偏爱空闲 / 偏爱延迟 |
| 一轮最多同时重连 | 2 条 | Gate 的机房常常整段一起掉线，全放开了重连等于自己打自己 |
| 数据通道只要 AEAD | 关 | 见上面「AEAD 与内核 offload」，只对声明支持 AEAD 的节点生效 |

填 0 或离谱值会夹回可用区间——界面上写错不该让程序退化成「每 0 秒全量重连」这种自伤行为。
开着 WARP 中转时「连接预算」会自动多给 12 秒，表里那个数不用自己改。

## 运维

装完后敲 `f` 打开管理菜单：

```
   1) 启动          2) 停止
   3) 重启          4) 查看日志
   5) 隧道列表      6) 连接信息
   7) 改端口        8) 改口令
   9) 改访问路径   10) 开机自启开关
  11) 更新         12) 卸载
```

也可以直接带参数用：

```bash
f info       # 连接信息
f list       # 隧道列表
f restart    # 重启
f log        # 跟踪日志
f update     # 从 MIGAO22/fanout 的 Release 更新（顺带把菜单脚本自己换掉）
f uninstall  # 卸载（服务、二进制、netns 和 /var/lib/fanout 全删）
```

隧道状态存在 `/var/lib/fanout/state.json`，重启后自动恢复，端口保持不变。
健康检查会比对出口 IP 是否还是建立隧道时那个——openvpn 挂掉后 netns 仍能经母机 NAT 出网，
只看通不通会漏判。连续几次不符就自动换节点重连，槽位和端口不变，原先指向它的节点链接会自动改绑过去。

## 本地开发

仓库根只有十来项：`cmd/fanout/` 是全部 Go 源码和前端资源（一个 `package main`），
`worker/` 是 VPN Gate 反代的 Cloudflare Worker，`install.sh` / `f.sh` / `fanout.service`
是装机与运维脚本，`.github/workflows/` 是 CI 与发版。程序只能在 Linux 上跑
（netns + `setns`），Windows / macOS 上只能交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o fanout ./cmd/fanout
GOOS=linux GOARCH=amd64 go vet ./...
go test ./...          # 需要 Linux 运行时，部分用例依赖 netns
```

纯函数那批（`ovpncipher.go`、`pool.go` 里的选路判定等）不碰 netns，但它们和整个包在一起，
而非 Linux 上这个包根本编译不过（`hostdisk.go` 用 `unix.Statfs_t`）。所以非 Linux 机器要验
纯逻辑，就把那一个文件连同 `go.mod` 拷进临时 module 单跑；正式回归一律在 Linux 上
`go test ./...`，CI 也是这么跑的。

```bash
# 在 Linux 上单独跑那批纯算法用例
go test -run 'OvpnSrc|Gpg|OpenvpnCanBuildSource|DebStableVer|DataCipher|NodeSupportsAEAD|PoolGoneHosts|PoolReusePick' ./...
```

前端在 `cmd/fanout/web/`（`index.html` / `app.js` / `login.html`），样式和结构都在
`index.html` 的 `<style>` 里。它必须和 Go 源码同目录：`go:embed` 的路径是相对源文件所在目录的，
挪动 `web/` 而不同步挪源码，二进制里就没有界面了。想在非 Linux 机器上看界面效果，
仓库外的静态托管 + 假接口即可，不必真起隧道。

## 已知限制

- SOCKS5 支持 CONNECT 和 UDP ASSOCIATE，DNS/QUIC 这类 UDP 也走隧道
  （感谢 [@zsawi](https://github.com/zsawi) 的 [#22](https://github.com/byJoey/fanout/pull/22)）。
  域名仍在母机解析。
- 隧道内只有 IPv4，IPv6 目标直接拒绝——放行只会让这条连接绕开隧道暴露真实地址。
- VPN Gate 是志愿者节点，相当比例已下线或满员（`AUTH_FAILED`）。连不上会自动顺着候选往下试，
  默认最多 6 个；先同地区，一整轮都失败之后才跨区。跨区之后出口 IP 的国家就变了。
- 管理界面只有随机路径 + 口令登录，没有 HTTPS，放公网建议前面套一层反代；订阅地址同理，
  token 是明文走的。会话 12 小时自动过期，侧栏底部「退出登录」会把本机 token 从会话表里收回。
- 换节点会避开这条出口之前用过的（最多记 16 个），同地区都换过一轮后从头开始。
  一个地区只有一个可用节点时换不动，界面会直接说明原因。
- 出口组的并发策略只对 TCP 生效；UDP ASSOCIATE 那一路会退回按序挑一条。
  选路以「一次拨号」为粒度，一条已建立的 TCP 从头到尾走同一条出口。
- 自动补齐补上来的是**新节点**，那一路的出口 IP 一定变。它保的是「组里始终有可用出口」，
  不是「IP 不变」；在意 IP 稳定就用单一模式、多绑几条组员，别开补齐。
- 「自己编译 openvpn」只在 deb 家族提供，编译期间不拦别的 apt 事务（`unattended-upgrades`
  在刷索引时那条 `apt-get update -qq` 会撞锁，稍等重试即可，不会留下半截状态）。
- WARP 中转是**失败即闭合**的，Cloudflare 出问题出口就跟着出问题；它只改「节点看到的来源 IP」，
  不给内容加密，也不动母机自己的出网。

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），本工具只是调用其公开的
节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。使用时请遵守 VPN Gate 的条款和你
所在地的法律。

## 上游与致谢

- 原始项目：**[byJoey/fanout](https://github.com/byJoey/fanout)** —— 架构、功能和绝大部分代码
  都来自这里。本仓库的改动集中在 `cmd/fanout/web/`（界面重写）、`restart.go`（重载合并）
  以及连接层的健壮性修补。
- 上游作者的其他项目：[xray-cf-lite](https://github.com/byJoey/xray-cf-lite)。
- 上游的交流群、教程和博客见 [byJoey/fanout 的 README](https://github.com/byJoey/fanout#readme)。
