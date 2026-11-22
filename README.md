# fanout

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 的公共节点变成本地 SOCKS5 端口：一个端口一个出口 IP。
再给每个出口挂一个节点链接，客户端连哪个端口就从哪个国家出去。

节点链接有三种管法：同机装了 3x-ui 或 xray-cf-lite 就接管它们的入站，
都没装则 fanout 自己跑 Xray，建站、改站、发链接都在同一个界面里完成。

> **这是 [byJoey/fanout](https://github.com/byJoey/fanout) 的衍生版本。**
> 原始项目、架构设计和全部核心功能都属于
> [@byJoey](https://github.com/byJoey)，本仓库只做了两件事：
> **重写管理界面**和**加固连接层**。
> 上游仍在活跃维护，遇到问题请先看上游，也欢迎直接去
> [byJoey/fanout](https://github.com/byJoey/fanout) 提 issue。

## 这个版本改了什么

### 界面整体重写

原来的界面是单个超长页面，所有信息按功能分区竖着堆下来，出口一多就要一直滚。
现在改成侧栏导航 + 四个视图，前端资源用 `go:embed` 打进二进制，
部署仍然只有一个可执行文件，没有额外的静态目录。

- **默认浅色**，另配一套深色主题，右上角随时切。
- 顶部一行 KPI：在线出口（带完成度圆环）、可用节点、覆盖地区、母机 IP、节点链接。
- 批量开出口时下面跟着作业卡片，每条目标的状态单独显示，不用刷整页。
- 出口列表支持卡片 / 表格两种密度，可按「已连通 / 连接中 / 失败」筛，能搜 IP、国家、节点名。
- 每条出口一行讲完一件事：状态色条、国家城市、节点名、出口 IP、槽位、已连通时长、
  SOCKS5 地址（带「鉴权」标记，可一键复制）、挂在上面的协议链接。
  失败时错误原因直接铺在这一行下面，不用点进去找。
- 没绑出口的入站单独列在末尾，附「批量清理」和就地改绑下拉——
  以前它们混在列表里，看不出和 fanout 有没有关系。

### 连接层加固

都是实际跑起来会咬人的问题，不是重构癖：

- **Xray 重载合并**。批量开 N 条出口时，每条 up 都请求重建配置，
  自建模式会连着重启 N 次 Xray，刚连上的客户端被反复打断。
  现在配置仍然同步落盘，只有**重载**进合并窗口（默认 1500 ms，可调），
  失败按指数退避重试。三种后端（3x-ui / 自建 / xray-cf-lite）统一走这条路径。
- **面板调用会自己恢复**。3x-ui 的 API token 失效后原来只能重启 fanout，
  现在遇到 401/403 会重新要一个 token 并重试这一次请求；
  网络类失败只对幂等接口重试，`inbounds/add` 不重试，避免重复建出站。
- **节点列表不再被 hammer**。那份 CSV 有几十 MB，多条隧道同时重连时原来各拉各的。
  现在单飞 + TTL 缓存 + 失败后 45 秒冷却，过期也先拿旧数据顶着。
- **挑节点看的是当下的拥挤程度**。只按 VPN Gate 的 `Speed` 排序会把「没测过速」
  当成最慢，好节点压到最后，而且那列完全不反映此刻有多挤。
  改成速度（饱和曲线）+ 实时会话数 + 延迟三项加权，
  可选 `均衡 / 偏爱速度 / 偏爱空闲 / 偏爱延迟` 四种策略。
- **隧道内拨号有硬超时**。原来是无超时 `net.Dial`，隧道刚掉但 netns 还没拆时
  目标会挂着 SYN 两分多钟，那条客户端连接既不报错也不动，界面上看就是卡住。
  现在 15 秒超时，DNS 查询也一起覆盖。
- **一轮里换过几个节点之后，刚失败的那个降到队尾**，不再每条出口都从头再撞一遍。
- **子进程一律从母机网络命名空间拉起**。这是上游那次
  「Xray 活得好好的、配置校验通过、日志干净，但入站端口在母机上全都看不到」
  事故的同类排查——凡是「在当前命名空间里生效」的命令都必须显式切回去。
  本版本把这条规则补齐到了面板 CLI、服务重启和自更新路径上。
- 上面这些阈值全部进了「设置与连接」，填错会夹回可用区间，不会把自己调死。

## 原理

每个节点跑在独立的 network namespace 里，netns 内启动官方 openvpn 客户端。
SOCKS5 监听在母机，出站连接用 `setns` 切进对应 netns 建立。

这样做的好处：VPN 的路由劫持只影响自己的 netns，不会切断母机的网络；
多个节点互不干扰，各自一个出口 IP。

```
客户端 ──> 母机 SOCKS5 :随机端口 ──> netns foN ──> openvpn ──> VPN Gate 节点
```

## 安装

需要 root，Linux（依赖 netns）。Windows / macOS 不能跑，只能交叉编译。

### 从源码装（本仓库，推荐）

```bash
git clone https://github.com/MIGAO22/fanout.git
cd fanout
REPO=MIGAO22/fanout bash install.sh
```

带源码目录时 `install.sh` 会从源码编译（需要 Go 1.21+）。
依赖（openvpn / curl / openssl / iproute / iptables）按发行版自动装，
apt、dnf、yum、pacman、apk、zypper 都认。没装 3x-ui 时还会顺带下载一份
Xray 到 `/var/lib/fanout/bin/`，装了则跳过，入站交给面板管。

服务用 systemd 或 OpenRC 都能装，装完自动开机自启。

> 想直接用预编译二进制，得先在本仓库打一个 `v*` tag 触发
> [release workflow](.github/workflows/release.yml)（amd64 / arm64 都出），
> 之后 `REPO=MIGAO22/fanout bash <(curl -fsSL https://raw.githubusercontent.com/MIGAO22/fanout/main/install.sh)`
> 就能拉到这里构建的版本。不打 tag 的话请走上面包源码安装。

> **自更新指向哪个仓库。** `f update` 和设置里的「更新」卡片默认查上游
> `byJoey/fanout`，直接把上游二进制换上来会静默覆盖掉本仓库的界面改动。
> release workflow 构建时已经用 `-X main.updateRepo=$GITHUB_REPOSITORY`
> 把自己指到自己的仓库；自己 `go build` 的话，要么加同样的 ldflags，
> 要么用环境变量兜住：`FANOUT_UPDATE_REPO=MIGAO22/fanout`。
> 想保持和上游一致、不介意丢掉这些改动，就别设这两个值。

**Alpine** 默认不带 bash，先装一下：

```bash
apk add bash curl
```

另外 fanout 要在 netns 里跑 openvpn，**宿主必须放开 `/dev/net/tun`**。
不少 LXC 小鸡没给这个权限，`ls /dev/net/tun` 不存在且 `mknod` 报
Operation not permitted 的话，这台机器用不了，跟发行版无关。

装完会打印管理界面地址、访问路径和口令：

```
管理界面  http://<你的IP>:8899/gwPuWHvaNr/
访问口令  f81120ac328d11c11b
```

路径和口令都是随机生成的，分别存在 `/var/lib/fanout/basepath` 和
`/var/lib/fanout/password`。路径不对一律返回 404，扫端口的看不到这里跑着什么。

## 使用

界面以**出口**为单位：一行就是一条隧道加上挂在它上面的节点链接。

点「新建出口」，选地区和数量，再选一个已有节点作模板，提交后 fanout 会并行
拉起隧道、为每个出口复制一份节点链接并绑好，进度按目标逐条回报。

地区里还有个「每个国家」：每个有空闲节点的国家各开几条，一次把所有地区铺开。
槽位不够时先开节点多的地区，不会中途报错。

每行右侧两个按钮：换一个节点（出口 IP 变、端口不变，已分发的客户端配置不用改），
或者停掉这个出口。换节点会避开这条出口之前用过的，连点几次每次都是新 IP。

切到「节点链接」视图是全部入站的表格：备注、协议、端口、走哪条出口、状态、详情。
点进详情可以改端口、备注、启停，管理客户端，以及改绑到别的出口。
一个入站可以挂多套客户端凭据，分发给不同的人；每套都能单独重置，
重置后旧链接立即失效。

### 订阅

节点多了就别一条条复制了。点「订阅」拿一条地址，填进客户端的订阅里，
以后加出口、删出口都会自己跟上。换节点不影响它——端口不变，链接也不变。

地址形如 `http://<服务器IP>:<端口>/<访问路径>/sub?token=<口令>`。
默认出 base64，各家客户端通吃；想看明文在后面加 `&target=links`。
默认只放绑了出口的节点，走直连的不往里混；想全都要就加 `&bound=0`。

订阅要给客户端直接拉，所以这条地址不要登录，**那串 token 就等于密码**，别发群里。
泄露了点「换一串口令」，旧地址立刻失效。

### 只用家宽

VPN Gate 的清单里混着一批它自己的机房服务器（`public-vpn-*` 和
`219.100.37.0/24`），出口一眼看得出是数据中心，还更容易满员。
设置里「只用家宽节点」默认开着，挑节点、地区可用数、自动重连的备选都只看
志愿者家宽。想连机房的一起用就把它关掉。

开关只管新挑的节点，已经跑着的出口不会因为改设置被换掉。

### 节点链接从哪来

同机装了 3x-ui 就直接接管面板里的入站，面板端口、路径、API token 全自动探测，
开了 SSL 也能识别。没装 3x-ui 时 fanout 自己跑一个 Xray，界面上多一个「新建节点」
按钮，可以选协议（VLESS / VMess / Trojan）、传输（TCP / WebSocket / gRPC /
HTTPUpgrade / XHTTP）和安全层（无 / TLS / REALITY）。

REALITY 的密钥对和 shortId 自动生成；TLS 不填证书就生成自签的，分享链接会带上
证书指纹让客户端固定信任。也可以填自己的证书路径。

接管 3x-ui 和自建这两种模式下，改端口、启停、加删客户端、绑定出口的操作完全一致，
用起来没有区别。

装了 [xray-cf-lite](https://github.com/byJoey/xray-cf-lite) 的机器会自动接管它生成的
三个节点。这个模式下节点归 xray-cf-lite 管，fanout 只负责给每个节点指定走哪条出口，
所以界面上不提供新建、删除和改节点的入口——想改端口或 UUID 去 xray-cf-lite 那边改。
两边共用同一份 Xray 配置，fanout 只往里加自己前缀的出站和分流规则，互不覆盖。

后端在设置面板里可以随时切换，本机没装的会置灰并说明原因；也可以用
`-panel 3x-ui` / `-panel native` / `-panel xray-cf-lite` 启动参数固定。
界面里选过之后会记住，重启仍然生效。

## 连接参数

「设置与连接」里可调的东西，以及它们各自在管什么：

| 参数 | 默认 | 作用 |
| --- | --- | --- |
| 健康检查间隔 | 10 s | 多久比对一次出口 IP 是否还是建立隧道时那个 |
| 连续失败判定 | 3 次 | 连续几次不符才判掉线，防抖 |
| 探活超时 | 6 s | 单次探测的超时 |
| 重连退避下限 / 上限 | 5 s / 60 s | 自动重连的指数退避区间 |
| 连接预算 | 35 s | 单个节点从拉起 openvpn 到拿到出口 IP 的总预算，超了就换下一个候选 |
| 每轮候选节点数 | 6 | 一轮里最多试几个候选 |
| 面板写入合并 | 1500 ms | Xray 重载的合并窗口，见上文 |
| 节点清单有效期 | 600 s | VPN Gate 那份 CSV 的缓存时长 |
| 挑节点排序 | 均衡 | 均衡 / 偏爱速度 / 偏爱空闲 / 偏爱延迟 |

填 0 或离谱值会夹回可用区间——界面上写错不该让程序退化成「每 0 秒全量重连」这种自伤行为。

## 运维

装完后敲 `f` 打开管理菜单：启停、看日志、查隧道、改端口/口令/访问路径、更新、卸载。

```
  状态      运行中
  版本      fanout v0.1.1
  开机自启  enabled

  管理地址  http://1.2.3.4:8899/gwPuWHvaNr/
  访问口令  f81120ac328d11c11b

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
f update     # 更新到最新版
f uninstall  # 卸载
```

隧道状态存在 `/var/lib/fanout/state.json`，重启后自动恢复，端口保持不变。

健康检查会比对出口 IP 是否还是建立隧道时那个——openvpn 挂掉后 netns 仍能经母机
NAT 出网，只看通不通会漏判。连续几次不符就自动换节点重连，槽位和端口不变，
原先指向它的节点链接会自动改绑过去。

## 本地开发

程序只能在 Linux 上跑（netns + `setns`），Windows / macOS 上只能交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o fanout .
GOOS=linux GOARCH=amd64 go vet ./...
```

前端在 `web/`（`index.html` / `app.js` / `login.html`），改完重新 build 就会
通过 `go:embed` 带进二进制。想在非 Linux 机器上看界面效果，仓库外的
`preview-server.mjs` 之类静态托管 + 假接口即可，不必真起隧道。

Go 测试需要 Linux 运行时（部分用例依赖 netns），在 Windows / macOS 上
只能 `go vet` 做类型检查，跑不起来：

```bash
# 在 Linux 上执行
go test ./...
```

## 已知限制

- SOCKS5 支持 CONNECT 和 UDP ASSOCIATE，DNS/QUIC 这类 UDP 也走隧道
  （感谢 [@zsawi](https://github.com/zsawi) 的
  [#22](https://github.com/byJoey/fanout/pull/22)）。域名仍在本机解析。
- 隧道内只有 IPv4，IPv6 目标直接拒绝——放行只会让这条连接绕开隧道暴露真实地址。
- VPN Gate 是志愿者节点，有相当比例已下线或满员（`AUTH_FAILED`）。
  连不上会自动顺着同地区候选往下试，默认最多 6 个。
- 管理界面只有随机路径 + 口令登录，没有 HTTPS。放公网建议前面套一层反代。
  订阅地址同理，token 是明文走的。
- 换节点会避开这条出口之前用过的节点（最多记 16 个），同地区都换过一轮后
  从头开始。一个地区只有一个可用节点时换不动，界面会直接说明原因。
- 本仓库的界面截图与交互仍在补充，`README` 里描述的是当前代码的实际行为。

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），
本工具只是调用其公开的节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。
使用时请遵守 VPN Gate 的条款和你所在地的法律。

## 上游与致谢

- 原始项目：**[byJoey/fanout](https://github.com/byJoey/fanout)** —— 架构、功能和
  绝大部分代码都来自这里。本仓库的改动集中在 `web/`（界面重写）、
  `restart.go`（重载合并）以及连接层的健壮性修补。
- 上游作者的其他项目：[xray-cf-lite](https://github.com/byJoey/xray-cf-lite)。
- 上游的交流群、教程和博客见
  [byJoey/fanout 的 README](https://github.com/byJoey/fanout#readme)。

用着有问题，优先去上游提 issue；只和本仓库界面改动有关的，也欢迎在这里提。
