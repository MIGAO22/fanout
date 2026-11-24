package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 内置客户端：install.sh 替 fanout 下载到工作目录、之后由 fanout 进程拉起的那份 Xray。
//
// 先说清楚 VPS 到出口那一跳用的不是它。那段用的是官方 openvpn 客户端，装在系统里、
// 跟着发行版的包管理器走（install.sh 只检查在不在，不在就 apt/apk 装），fanout 只是
// 在 netns 里 exec 一下，出口机器上什么都不用装。所以这里能"更新"的只有 Xray。
//
// Xray 之所以需要一个更新入口，是因为 install.sh 里那段逻辑是"已存在就跳过"：
//
//	elif [[ -x "${WORK_DIR}/bin/xray" ]]; then echo "已有 $(...version...)"
//
// 装完那天它确实是最新版，之后无论重装多少次、升级多少次 fanout，这份内核都原地不动。
// REALITY、XHTTP 这些新特性都只在新版里有，旧版会一路用到用户手动删文件为止。

// xrayReleaseRepo 是内置 Xray 的取包来源，和 install.sh 保持一致。
const xrayReleaseRepo = "XTLS/Xray-core"

// openvpn 的官方发布仓库。它只发源码包（openvpn-2.7.9.tar.gz + .asc），没有能直接装到
// 机器上的二进制，所以这里给的是"上游已经走到哪"这个号；apt/dnf 真能装到的还是下面
// 那个仓库版本。两个号都摆出来，用户才看得出"我落后可了"和"我能升什么"是两件事。
const openvpnReleaseRepo = "OpenVPN/openvpn"

// releaseTag 把 GitHub release 的 tag 归一成能跟本机版本并排显示的号：去掉 v 前缀，
// 并且只认 "26.11.9" 这种纯数字点分号。OpenVPN 的候选版 tag（v2.7_rc6）压根没有 release
// 对象、/releases/latest 拿不到，但哪天它们改成发 release，这里也不至于把一个 rc 报成稳定版。
func releaseTag(tag string) (string, bool) {
	t := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if t == "" {
		return "", false
	}
	for _, seg := range strings.Split(t, ".") {
		if seg == "" {
			return "", false
		}
		for _, r := range seg {
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}
	return t, true
}

// releaseLatestTag 问某个仓库官方最新 release 的号，回来已经归一化过。
func releaseLatestTag(repo string) (string, error) {
	rel, err := fetchReleaseLatest(repo)
	if err != nil {
		return "", err
	}
	t, ok := releaseTag(rel.TagName)
	if !ok {
		return "", fmt.Errorf("%s 的最新版 tag %q 不像稳定版号", repo, rel.TagName)
	}
	return t, nil
}

// ovpnGapNote 解释「上游已经 2.7.9 了，可这台机器上装不到」这个差。
// 差的原因必须是查出来的、不是猜的：deb 家族两边都问过了（发行版仓库 + OpenVPN
// 官方 apt 源），两个都不够才敢说这台机器装不到。rpm/alpine/arch 只问了自己仓库，
// 官方在非 deb 上另有发布渠道（RHEL 系是 Fedora Copr 的 @OpenVPN/openvpn-release-2.7，
// 见 community.openvpn.net 的 software repos 页），所以那句只能说"没在本机源里"，
// 不能替人家宣布没有二进制包。本机已经追平上游时一句都不说——那时候没有差要解释。
func ovpnGapNote(family, have, target, upstream string) string {
	if upstream == "" || !versionLess(have, upstream) {
		return ""
	}
	if target != "" && !versionLess(target, upstream) {
		return "" // 按钮就能装到官方最新，没有差要解释
	}
	if family == "deb" {
		if target != "" {
			return fmt.Sprintf("这台机器上装得到的最高是 %s；官方 %s 的 apt 源里没有适配本机发行版的包", plainVersion(target), upstream)
		}
		return fmt.Sprintf("发行版仓库和 OpenVPN 官方 apt 源里都没有比 %s 更新的包，官方 %s 这台机器装不到", have, upstream)
	}
	if target != "" {
		return fmt.Sprintf("本机源里最高只有 %s；官方 %s 得另接 OpenVPN 的源（RHEL 系在 Copr @OpenVPN/openvpn-release-2.7）", plainVersion(target), upstream)
	}
	return fmt.Sprintf("本机源里没有比 %s 更新的 openvpn；官方 %s 得另接 OpenVPN 的源（RHEL 系在 Copr @OpenVPN/openvpn-release-2.7）", have, upstream)
}

// plainVersion 去掉 Debian 版本号跟在 - / + 后面的发行版修订（2.6.20-1ubuntu1 → 2.6.20）。
// 那串是打包者写的，跟"哪个版本"无关，贴进界面只会占地方。
func plainVersion(v string) string {
	s, _, _ := strings.Cut(v, "-")
	s, _, _ = strings.Cut(s, "+")
	return s
}

// ClientBinStatus 是界面「内置客户端」这张卡要的全部信息。
type ClientBinStatus struct {
	Backend   string `json:"backend"`    // 当前节点后端：native / 3x-ui / xray-cf-lite
	Path      string `json:"path"`       // 正在用的 xray 可执行文件
	Xray      string `json:"current"`    // 本机 xray 版本，读不到为空
	OpenVPN   string `json:"openvpn"`    // **正在用的那份** openvpn 版本（可能是 fanout 编的）
	Latest    string `json:"latest"`     // Xray-core 最新版（只有点了检查才有）
	HasUpdate bool   `json:"has_update"` // Latest 比 Xray 新
	Updatable bool   `json:"updatable"`  // 这份 xray 归 fanout 管、可以就地替换
	Reason    string `json:"reason,omitempty"`
	CheckErr  string `json:"check_error,omitempty"` // 问 GitHub 失败的原因，不影响上面的字段

	// openvpn 有两个"最新版"，得分开给，之前只给前者才闹出三台机器三个"最新版"：
	//   upstream 是 GitHub 上 OpenVPN/openvpn 的官方 release，跟机器无关，三台问出来同一个号；
	//   target 是这台机器真能装到的最高号——发行版仓库和 OpenVPN 官方 apt 源两边问完取高的，
	//   「升级 openvpn」装的就是它。之前只问发行版仓库，才会出现官方源明明有 2.7.1、
	//   界面却指着 2.6.x 说"升不动"。
	OvpnFamily string `json:"openvpn_family,omitempty"` // deb / rpm / alpine / arch / suse
	OvpnCmd    string `json:"openvpn_cmd,omitempty"`
	OvpnUpdate bool   `json:"openvpn_updatable"` // 认得出家族、机器上有对应的包管理器
	OvpnWhy    string `json:"openvpn_reason,omitempty"`
	OvpnLatest string `json:"openvpn_latest,omitempty"`
	OvpnHas    bool   `json:"openvpn_has_update"` // 有比本机更新的、且装得到
	// 索引没刷动时仓库版本号只能来自本机那份旧索引，这时"已是最新"是句假话，
	// 界面必须换个说法（VPS 上几个月不 apt-get update、或者 apt 正被自动更新占用）
	OvpnStale bool `json:"openvpn_stale,omitempty"`

	OvpnUpstream string `json:"openvpn_upstream,omitempty"` // 官方 release 版本号，已去掉 tag 的 v
	OvpnBehind   bool   `json:"openvpn_behind_upstream"`    // 本机比官方 release 旧
	// 官方 release 比这台机器能装到的还新时那句解释，界面直接摆出来，不在前端比版本号
	OvpnNote string `json:"openvpn_note,omitempty"`

	OvpnTarget   string `json:"openvpn_target,omitempty"`     // 点升级会装到的号，空=没有可升的
	OvpnOfficial string `json:"openvpn_official,omitempty"`   // OpenVPN 官方 apt 源在本机发行版上的最高稳定号
	OvpnFromOff  bool   `json:"openvpn_from_official"`        // target 来自官方源而不是发行版仓库
	OvpnWired    bool   `json:"openvpn_repo_wired,omitempty"` // 官方源已经在这台机器上配着
	// 还没配、而升级又必须配时，将要写进 sources.list.d 的那一行；确认框原样摊开给人看
	OvpnRepoLine string `json:"openvpn_repo_line,omitempty"`

	// 源码编译这条路：apt 和官方源都够不到官方最新时才给。
	// can_build 的判据全在这边算，前端只按布尔显示按钮——按钮藏不住不该按的情况。
	OvpnCanBuild bool `json:"openvpn_can_build,omitempty"`
	// build_ver 是要编的那个号（= 官方 release），build_cmd 是准备在你机器上跑的命令，
	// 确认框原样摊开，和「升级 openvpn」那颗按钮一个规矩。build_path 是它会装进去的
	// 独立目录，由后端给——前端不该自己猜路径，否则哪天改了常量就成了界面在骗人。
	OvpnBuildVer  string `json:"openvpn_build_ver,omitempty"`
	OvpnBuildCmd  string `json:"openvpn_build_cmd,omitempty"`
	OvpnBuildPath string `json:"openvpn_build_path,omitempty"`
	// src_active 表示隧道当前 exec 的是 fanout 编的那份；aptVer 是系统包那份的号，
	// 两者不一样时必须都写出来，否则用户看到"2.7.9"会以为 apt 升到 2.7.9 了。
	OvpnSrcActive bool   `json:"openvpn_source_active,omitempty"`
	OvpnSrcVer    string `json:"openvpn_source_ver,omitempty"`
	OvpnSrcBin    string `json:"openvpn_source_bin,omitempty"`
	OvpnApt       string `json:"openvpn_apt,omitempty"`

	// WARP 中转这一行。fanout 不跑官方 warp-cli——它在每条出口的 netns 里
	// 用内核 WireGuard 直连 Cloudflare，机器上没有属于 fanout 的 WARP 二进制
	// 可以替换。所以这里给的是"上报给 Cloudflare 的客户端版本"和最新稳定号，
	// 按钮做的是按最新号重新注册（换密钥、换接入），而不是升级程序。
	// 把这件事写成"升级 WARP"是骗人的，字段和文案都得照真实动作来。
	WarpEnabled  bool   `json:"warp_enabled"` // 中转开关
	WarpReady    bool   `json:"warp_ready"`   // 有条件跑（有 wg 命令、记录可写）
	WarpWhy      string `json:"warp_reason,omitempty"`
	WarpVersion  string `json:"warp_version,omitempty"` // 当前上报的版本号
	WarpLatest   string `json:"warp_latest,omitempty"`  // 官方 apt 源的最新稳定号
	WarpUpdate   bool   `json:"warp_has_update"`        // 上报号比官方号旧
	WarpCount    int    `json:"warp_count"`             // 已注册的出口数
	WarpStale    int    `json:"warp_stale"`             // 其中按旧号注册的
	WarpCanApply bool   `json:"warp_can_apply,omitempty"`
	WarpCheckErr string `json:"warp_check_error,omitempty"` // 问官方源失败，不影响其它字段
}

// xrayAssetForArch 把 GOARCH 换成 Xray-core release 的资产名。
// 命名不带规律可循（amd64 叫 64、arm64 叫 v8a），只能照抄官方。
func xrayAssetForArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "Xray-linux-64.zip"
	case "386":
		return "Xray-linux-32.zip"
	case "arm64":
		return "Xray-linux-arm64-v8a.zip"
	case "arm":
		return "Xray-linux-arm32-v7a.zip"
	case "mips64le":
		return "Xray-linux-mips64le.zip"
	case "ppc64le":
		return "Xray-linux-ppc64le.zip"
	default:
		return "Xray-linux-" + goarch + ".zip"
	}
}

// parseXrayVersion 从 `xray version` 的输出里抠出版本号。
// 首行形如 "Xray 26.2.27 (Ruyi MCP)"，有些构建会写成 "Xray-core v1.2.3"。
func parseXrayVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(f[0]), "xray") {
			return strings.TrimPrefix(strings.TrimPrefix(f[1], "core"), "v")
		}
	}
	return ""
}

// parseOpenVPNVersion 从 `openvpn --version` 首行抠出版本号，形如 "OpenVPN 2.6.8 x86_64-..."。
func parseOpenVPNVersion(out string) string {
	f := strings.Fields(firstLine(out))
	if len(f) >= 2 && strings.HasPrefix(strings.ToLower(f[0]), "openvpn") {
		return f[1]
	}
	return ""
}

// probeVersion 执行一条取版本的命令。8 秒足够，卡在这里说明二进制本身有问题
// （比如架构不对、缺动态库），那种情况要当读不到处理，不是当报错抛给用户。
func probeVersion(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}

// xrayUpdateTarget 判断这份 xray 能不能由 fanout 就地替换。
//
// 只替换自己装在 workDir/bin 下的那一份。3x-ui、xray-cf-lite、包管理器给的 xray
// 都有各自的升级路径，覆盖它们等于替别人的面板改运行时，出了问题没人想得起来
// 是这里动的。不能替换时把原因一并返回，界面直接印出来，比一个灰按钮好。
func xrayUpdateTarget(workDir, bin, backend string) (target string, why string) {
	owned := filepath.Join(workDir, "bin", "xray")
	switch {
	case backend == "":
		return "", "还没定下用哪种节点后端，等探测完成后再看"
	case backend != "native":
		return "", fmt.Sprintf("现在入站由 %s 接管，那份 Xray 跟着它自己的升级路径走，fanout 不动它", backend)
	case strings.TrimSpace(bin) == "":
		return "", "自建模式下没找到 Xray 可执行文件"
	case filepath.Clean(bin) != filepath.Clean(owned):
		return "", fmt.Sprintf("现在跑的是系统里的 Xray（%s），由包管理器负责升级；fanout 只替换自己装在 %s 的那一份", bin, owned)
	default:
		return owned, ""
	}
}

// clientBinStatus 汇总本机这几份客户端的版本。
//
// askLatest 才去问 GitHub：未登录的 Releases API 每个 IP 一小时只有 60 次，
// 打开设置页顺手打一发会很快把额度吃光，所以只在点「检查更新」时问。
func clientBinStatus(askLatest bool) *ClientBinStatus {
	wd := panelWorkDir()
	st := &ClientBinStatus{Backend: currentPanelMode()}
	if bin, err := findXray(wd); err == nil {
		st.Path = bin
		st.Xray = parseXrayVersion(probeVersion(bin, "version"))
	} else {
		st.Reason = err.Error()
	}
	var osRel string
	// 探测的是**隧道真正会 exec 的那一份**（openvpnBinary），不是 PATH 上那份。
	// 这里的版本会喂给 dataCipherArgs 决定参数名，和实际跑的二进制必须同源。
	active := openvpnBinary()
	st.OpenVPN = parseOpenVPNVersion(probeVersion(active, "--version"))
	if r := readOpenvpnSrcRecord(); r != nil && isExecutableFile(r.Bin) {
		st.OvpnSrcVer, st.OvpnSrcBin = r.Version, r.Bin
		st.OvpnSrcActive = active == r.Bin
		if st.OvpnSrcActive {
			// 用的是编出来的那份，系统包那份是另一个号——两个都得写出来，
			// 否则卡片上那个号会让人以为 apt 升到 2.7.9 了。
			st.OvpnApt = parseOpenVPNVersion(probeVersion("openvpn", "--version"))
		}
	}
	if st.OpenVPN == "" {
		st.OvpnWhy = "本机没有 openvpn，出口隧道起不来；先装一个（apt/apk install openvpn）"
	} else if blob, err := os.ReadFile(osReleasePath); err != nil {
		st.OvpnWhy = fmt.Sprintf("读 %s 失败，不知道该怎么升：", osReleasePath) + err.Error()
	} else {
		osRel = string(blob)
		steps, family, err := openvpnUpgradePlan(osRel, hasCmd)
		if err != nil {
			st.OvpnWhy = err.Error()
		} else {
			st.OvpnFamily, st.OvpnUpdate, st.OvpnCmd = family, true, describeSteps(steps)
		}
	}

	target, why := xrayUpdateTarget(wd, st.Path, st.Backend)
	st.Updatable = target != ""
	if !st.Updatable {
		st.Reason = why
	}
	st.WarpEnabled, st.WarpVersion = warpOn(), warpReportVersion()
	st.WarpReady, st.WarpWhy = warpReady()
	if warpSvc != nil {
		for _, a := range warpSvc.all() {
			st.WarpCount++
			if a.Version != st.WarpVersion {
				// 手上有旧号注册的线路：说明上次「更新」没走完，界面要能看出来
				st.WarpStale++
			}
		}
	}
	st.WarpCanApply = st.WarpReady && st.WarpEnabled && st.WarpCount > 0
	if !askLatest {
		return st
	}
	// WARP 的最新版只问官方 apt 源：cloudflare 没有发 WARP 的 GitHub release，
	// 拿 apt 索引里的 Version 当基准是唯一实测读得到、又不依赖签名密钥的路子。
	if lv, err := warpLatestVersion(); err != nil {
		st.WarpCheckErr = err.Error()
	} else {
		st.WarpLatest = lv
		st.WarpUpdate = warpVersionLess(st.WarpVersion, lv) || st.WarpStale > 0
	}
	// 先问本机包管理器，再打 GitHub：openvpn 的答案不依赖 GitHub，额度用尽时
	// 这张卡至少还有一半是活的。
	if st.OvpnUpdate && st.OpenVPN != "" {
		d := decideOpenVPNUpgrade(osRel, st.OpenVPN, hasCmd, fetchDebPackages, true)
		st.OvpnLatest, st.OvpnStale = d.RepoVer, d.Stale
		if d.RepoWhy != "" {
			st.OvpnWhy = d.RepoWhy
		}
		st.OvpnOfficial, st.OvpnWired = plainVersion(d.Official), d.Wired
		st.OvpnTarget, st.OvpnFromOff = plainVersion(d.Target), d.Channel != ""
		st.OvpnHas = d.Target != ""
		if d.NeedsWrite {
			// 升级要先接官方源，确认框就得把这一行和 apt 命令一起摊开
			st.OvpnRepoLine = ovpnAptRepoLine(d.Channel, d.Arch, d.Codename)
			st.OvpnCmd = fmt.Sprintf("写入 %s（OpenVPN 官方签名密钥）\n写入 %s：\n  %s\n%s",
				ovpnAptKeyring, ovpnAptSources, st.OvpnRepoLine, describeSteps(d.Steps))
		}
	}
	// 两份客户端的"最新版"都对接 GitHub 官方 release：Xray 是 XTLS/Xray-core，
	// openvpn 是 OpenVPN/openvpn。openvpn 这个号跟机器上的发行版无关，三台 VPS 问出来
	// 是同一个——之前只报各台自己仓库里的号，才会出现"三个不同版本各自都叫最新"。
	xv, xerr := releaseLatestTag(xrayReleaseRepo)
	ov, oerr := releaseLatestTag(openvpnReleaseRepo)
	if xerr != nil {
		st.CheckErr = xerr.Error()
	} else {
		st.Latest = xv
		// HasUpdate 只在 fanout 真能替换那份二进制时才有意义。接管模式下它恒为 false，
		// 但 Latest 照样带出去——界面不能因此显示"已是最新"，那是把"轮不到我管"
		// 说成"你不用升"，三台版本不同的 3x-ui 机器就是这么同时报最新的。
		st.HasUpdate = st.Updatable && versionLess(st.Xray, xv)
	}
	if oerr != nil {
		// 两个仓库各自失败各自说，别让 Xray 那边的失败把 openvpn 的号也吞掉
		if st.CheckErr == "" {
			st.CheckErr = oerr.Error()
		}
	} else if st.OpenVPN != "" {
		st.OvpnUpstream = ov
		st.OvpnBehind = versionLess(st.OpenVPN, ov)
		st.OvpnNote = ovpnGapNote(st.OvpnFamily, st.OpenVPN, st.OvpnTarget, ov)
		// 只有"正在用的那份就是编出来的那份"才算编好；记录在但文件被删了得能重新编回来
		builtVer := ""
		if st.OvpnSrcActive {
			builtVer = st.OvpnSrcVer
		}
		// 源码编译这条路：判据全在 openvpnCanBuildSource（含"只给 deb"，
		// 和 ovpnSrcDeps 的家族口径必须一致，否则按钮点下去第一步就红）。
		if openvpnCanBuildSource(st.OvpnFamily, st.OpenVPN, st.OvpnTarget, ov, builtVer) {
			st.OvpnBuildVer = ov
			if plan := openvpnSrcPlan(ov, st.OvpnFamily); plan != "" {
				st.OvpnCanBuild, st.OvpnBuildCmd = true, plan
				st.OvpnBuildPath = ovpnSrcPrefix(ov)
				if st.OvpnNote != "" {
					st.OvpnNote += "；可以从官方源码包编一份（装进独立目录，不动系统那份）"
				}
			}
		}
	}
	return st
}

// extractFromZip 从 zip 里取出指定文件名的成员写到 dst。
// 只取根目录下那个，Xray 的包里还带着 geoip.dat 和一堆 config/，用不上。
func extractFromZip(zipPath, member, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("打开压缩包失败: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, rc); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("压缩包里没有 %s", member)
}

// xrayApplyMu 串行化替换过程：连点两次更新会让第二次把第一次刚换上的
// 二进制当旧版备份走，中间那一刻 Xray 的落点是不确定的。
var xrayApplyMu sync.Mutex

// openvpnApplyMu 同理：两个 apt-get 同时跑会撞死在 dpkg 的锁上，
// 报错还报得像是磁盘坏了。
var openvpnApplyMu sync.Mutex

// applyXrayUpdate 把内置 Xray 升到 Xray-core 最新版，并让正在跑的自建入站切到新版。
//
// 顺序上处处留退路：先确认下载到的这份在本机真的跑得起来、确实是目标版本，
// 再确认它认现有配置，两步都过了才动真文件；替换用同目录 rename，中途失败
// 一律把 .prev 放回原位并把旧版重新拉起——宁可不更新，也不能让入站停在半路。
func applyXrayUpdate() (string, error) {
	xrayApplyMu.Lock()
	defer xrayApplyMu.Unlock()

	wd := panelWorkDir()
	bin, err := findXray(wd)
	if err != nil {
		return "", err
	}
	target, why := xrayUpdateTarget(wd, bin, currentPanelMode())
	if target == "" {
		return "", fmt.Errorf("%s", why)
	}
	from := parseXrayVersion(probeVersion(target, "version"))

	rel, err := fetchReleaseLatest(xrayReleaseRepo)
	if err != nil {
		return "", fmt.Errorf("查 %s 最新版失败: %w", xrayReleaseRepo, err)
	}
	want := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if want == "" {
		return "", fmt.Errorf("%s 最新版返回了一个空版本号", xrayReleaseRepo)
	}
	asset := xrayAssetForArch(runtime.GOARCH)
	var assetURL string
	for _, a := range rel.Assets {
		if a.Name == asset {
			assetURL = a.URL
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("%s %s 里没有适配 %s 的包", xrayReleaseRepo, want, runtime.GOARCH)
	}

	tmp, err := os.MkdirTemp("", "fanout-xray-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, asset)
	if err := downloadFile(assetURL, zipPath); err != nil {
		return "", fmt.Errorf("下载 %s 失败: %w", asset, err)
	}
	pulled := filepath.Join(tmp, "xray")
	if err := extractFromZip(zipPath, "xray", pulled); err != nil {
		return "", fmt.Errorf("解包失败: %w", err)
	}
	if got := parseXrayVersion(probeVersion(pulled, "version")); got != want {
		return "", fmt.Errorf("新版在本机跑不起来或版本对不上（期望 %s，实际 %s），旧版仍在运行", want, got)
	}
	// 新版可能收紧了配置校验，真不认这份配置的话要在替换之前就停手
	if err := verifyXrayConfigNow(pulled, filepath.Join(wd, "xray.json")); err != nil {
		return "", fmt.Errorf("Xray %s 不认现有配置，已放弃更新：%v", want, err)
	}

	staged := target + ".new"
	if err := copyFileMode(pulled, staged, 0755); err != nil {
		return "", fmt.Errorf("写入新版失败: %w", err)
	}
	prev := target + ".prev"
	if err := os.Rename(target, prev); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("备份旧版失败: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		os.Remove(staged)
		_ = os.Rename(prev, target)
		return "", fmt.Errorf("替换失败: %w", err)
	}

	msg := fmt.Sprintf("Xray 已从 %s 升到 %s", orUnknown(from), want)
	if n, ok := currentNativePanel(); !ok {
		return msg + "。当前没有自建 Xray 在跑，下次重载配置时生效", nil
	} else if err := n.swapXray(); err != nil {
		// 新版起不来：把旧版放回原位再拉一次，至少让入站恢复
		_ = os.Remove(target)
		if rb := os.Rename(prev, target); rb != nil {
			return "", fmt.Errorf("新版起不来（%v），而且旧版也没放回去（%v），请检查 %s", err, rb, target)
		}
		_ = n.swapXray()
		return "", fmt.Errorf("新版起不来，已退回 %s：%v", orUnknown(from), err)
	}
	return msg + "，自建入站已用新版重新拉起（旧版留在 " + filepath.Base(prev) + "）", nil
}

// verifyXrayConfigNow 用指定那份二进制校验配置文件，把错误原文带回来。
// verifyXrayConfig 只回一句笼统的话，这里要拿它判断"是新版不认配置"还是"文件没了"。
func verifyXrayConfigNow(bin, cfgPath string) error {
	if _, err := os.Stat(cfgPath); err != nil {
		// 还没写过配置（一个入站都没建）：没有可比对的东西，跳过这道检查
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return verifyXrayConfig(bin, cfgPath)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知版本"
	}
	return s
}

// ───────────── openvpn：母机连出口那一跳用的客户端 ─────────────

// osReleasePath 是发行版身份的唯一可靠来源，各家都有。
const osReleasePath = "/etc/os-release"

// parseOSRelease 解析 /etc/os-release。这里只用到 ID 和 ID_LIKE，
// 但按 KEY=VALUE 全收，免得遇到引号、注释、export 前缀就懵。
func parseOSRelease(blob string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(blob, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(v, `"'`)
	}
	return out
}

// distroFamily 按 ID / ID_LIKE 认包管理器家族。ID_LIKE 用来接住衍生版：
// Ubuntu 底下写 debian，Mint 底下写 ubuntu，Rocky 底下写 rhel fedora。
func distroFamily(kv map[string]string) string {
	for _, field := range []string{kv["ID_LIKE"], kv["ID"]} {
		for _, name := range strings.Fields(field) {
			switch strings.ToLower(name) {
			case "debian", "ubuntu":
				return "deb"
			case "fedora", "rhel", "centos", "rocky", "almalinux", "amzn", "ol":
				return "rpm"
			case "suse", "opensuse-tumbleweed", "opensuse-leap", "sles":
				return "suse"
			case "alpine":
				return "alpine"
			case "arch", "manjaro", "endeavouros":
				return "arch"
			}
		}
	}
	return ""
}

// openvpnUpgradePlan 把发行版身份换成"该跑哪几条命令"。
//
// 刻意不复述"哪个版本算新"的判断：那归发行版的仓库管，我们只负责点名
// 只升 openvpn 这一个包。用 --only-upgrade / 单包 update 而不是全量升级，
// 是因为这个按钮没承诺"顺手把整台机器的包都升一遍"——那会连带升内核、
// 会重启一堆无关服务，出了事没人往这里查。
func openvpnUpgradePlan(osRelease string, have func(string) bool) (steps [][]string, family string, err error) {
	kv := parseOSRelease(osRelease)
	switch distroFamily(kv) {
	case "deb":
		if !have("apt-get") {
			return nil, "deb", fmt.Errorf("认出来是 Debian/Ubuntu 系，但机器上没有 apt-get")
		}
		return [][]string{
			{"apt-get", "update", "-qq"},
			{"apt-get", "install", "-y", "--only-upgrade", "openvpn"},
		}, "deb", nil
	case "rpm":
		// RHEL 8+ 只有 dnf，CentOS 7 那批只有 yum，参数一致
		switch {
		case have("dnf"):
			return [][]string{{"dnf", "-y", "upgrade", "openvpn"}}, "rpm", nil
		case have("yum"):
			return [][]string{{"yum", "-y", "update", "openvpn"}}, "rpm", nil
		}
		return nil, "rpm", fmt.Errorf("认出来是 RHEL/CentOS/Fedora 系，但机器上没有 dnf 或 yum")
	case "alpine":
		if !have("apk") {
			return nil, "alpine", fmt.Errorf("认出来是 Alpine，但机器上没有 apk")
		}
		return [][]string{
			{"apk", "update", "--no-cache"},
			{"apk", "add", "--no-cache", "--upgrade", "openvpn"},
		}, "alpine", nil
	case "arch":
		if !have("pacman") {
			return nil, "arch", fmt.Errorf("认出来是 Arch 系，但机器上没有 pacman")
		}
		return [][]string{{"pacman", "-Sy", "--noconfirm", "openvpn"}}, "arch", nil
	case "suse":
		if !have("zypper") {
			return nil, "suse", fmt.Errorf("认出来是 openSUSE 系，但机器上没有 zypper")
		}
		return [][]string{
			{"zypper", "--non-interactive", "refresh"},
			{"zypper", "--non-interactive", "update", "openvpn"},
		}, "suse", nil
	}
	return nil, "", fmt.Errorf("认不出这台机器的发行版（%s 里没有已支持的 ID），请手动升级 openvpn", osReleasePath)
}

// openvpnRepoLatest 问本机包管理器：仓库里现在能给到的 openvpn 是哪个版本。
//
// 不打 GitHub：openvpn 的新旧由发行版的仓库说了算，Debian 稳定版会把它锁在
// 发布那天选定的版本上，拿上游最新版比只会天天报"你有新版"却永远装不上。
//
// 查之前必须先刷一次索引。`apt-cache policy` 读的是 /var/lib/apt/lists 里那份
// 本地索引，VPS 上几个月不跑 apt-get update 太常见，这时 Candidate 就等于已装版本，
// 于是每台机器都"已是仓库最新"——三台机器三个不同的 openvpn 版本同时报最新，
// 就是这么来的。刷不动索引时不假装查到了：版本号照给，但标 stale，界面换个说法。
func openvpnRepoLatest(family string, have func(string) bool) (ver, why string, stale bool) {
	var refresh, query []string
	switch family {
	case "deb":
		if !have("apt-cache") {
			return "", "机器上没有 apt-cache，问不到仓库里的版本", false
		}
		if have("apt-get") {
			refresh = []string{"apt-get", "update", "-qq"}
		}
		query = []string{"apt-cache", "policy", "openvpn"}
	case "rpm":
		switch {
		case have("dnf"):
			// --refresh 让 dnf 自己重取元数据；查询和刷新合成一条命令
			query = []string{"dnf", "-q", "--refresh", "list", "openvpn"}
		case have("yum"):
			// 老 yum 没有 --refresh，删掉缓存元数据让下一条查询自己重新取
			refresh = []string{"yum", "-q", "clean", "metadata"}
			query = []string{"yum", "-q", "list", "openvpn"}
		default:
			return "", "机器上没有 dnf 或 yum，问不到仓库里的版本", false
		}
	case "alpine":
		if !have("apk") {
			return "", "机器上没有 apk，问不到仓库里的版本", false
		}
		refresh = []string{"apk", "update"}
		query = []string{"apk", "policy", "openvpn"}
	case "arch":
		if !have("pacman") {
			return "", "机器上没有 pacman，问不到仓库里的版本", false
		}
		refresh = []string{"pacman", "-Sy", "--noconfirm"}
		query = []string{"pacman", "-Si", "openvpn"}
	case "suse":
		if !have("zypper") {
			return "", "机器上没有 zypper，问不到仓库里的版本", false
		}
		refresh = []string{"zypper", "--non-interactive", "refresh"}
		query = []string{"zypper", "-n", "info", "openvpn"}
	default:
		return "", "认不出这台机器的包管理器，问不到仓库里的版本", false
	}

	if len(refresh) > 0 {
		if _, err := runRefresh(refresh); err != nil {
			// 刷不动也继续问：拿旧索引里的号给出去，但要说清这个号的来历
			stale = true
			why = refreshWhy(refresh[0], err)
		}
	}

	out, err := runRepoQuery(query, repoQueryTO)
	if err != nil {
		// 查不到版本在包管理器这里很常见（仓库里没这个包、镜像源连不上），
		// 输出尾巴里就有答案，别只回一句"失败了"
		if stale {
			return "", why, true
		}
		// rpm 家族刷新和查询是一条命令，撞锁就落在这里而不是上面
		if looksLocked(err) {
			return "", fmt.Sprintf("%s 正被别的进程占用（自动更新常在跑），仓库版本号问不到", query[0]), false
		}
		return "", fmt.Sprintf("%s 没答上来：%v", query[0], err), false
	}
	switch family {
	case "deb":
		ver = parseAptCandidate(out)
		if ver == "" {
			return "", "apt 的仓库索引里没有 openvpn 这个包", stale
		}
	case "rpm":
		ver = parseDnfListRow(out)
		if ver == "" {
			return "", "dnf/yum 的仓库里查不到 openvpn 这个包", stale
		}
	case "alpine":
		ver = parseApkPolicy(out)
		if ver == "" {
			return "", "apk 的仓库索引里没有 openvpn 这个包", stale
		}
	default: // arch / suse：都是 "Version : x.y" 那一行
		ver = parseColonVersion(out)
		if ver == "" {
			return "", "包管理器没给出 openvpn 的版本号", stale
		}
	}
	return ver, why, stale
}

// 查询命令各给多久的额度。刷新索引那条更短：镜像源连不上时 apt-get update 会
// 挨个源重试很久，而那一步恰恰是最该快点承认"刷不动"的——它一超时界面就标出
// 来下面这个版本号是从旧索引里读的，比让人盯着"查询中"干等有用。
const (
	repoRefreshTO = 45 * time.Second
	repoQueryTO   = 60 * time.Second
	// 刷索引撞锁后隔多久再试一次。只试一次，等太久不如让人改点别的地方看。
	refreshRetryWait = 6 * time.Second
)

// runRefresh 刷一次包索引。撞锁是这一步最常见的失败：unattended-upgrades 或用户自己
// 开的 apt 正占着 /var/lib/apt/lists/lock，apt 立刻就会 exit 100 走人。这时等一下再试
// 一次比直接标 stale 有用——锁一放开就拿到新索引，界面也就不用报"旧索引"。
func runRefresh(argv []string) (string, error) {
	out, err := runRepoQuery(argv, repoRefreshTO)
	if err == nil || !looksLocked(err) {
		return out, err
	}
	time.Sleep(refreshRetryWait)
	return runRepoQuery(argv, repoRefreshTO)
}

// looksLocked 判断这条失败是不是"别人正占着包管理器"。几家印法不同，但都带 lock，
// 而且都顺带说出占着它的那个进程 pid。
func looksLocked(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{"could not get lock", "unable to lock", "failed to get lock", "waiting for process"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// refreshWhy 把刷索引的失败翻成一句人话。撞锁不该带上 apt 那一大段原始输出，
// 那不是这台机器的问题，也不该让用户以为是 fanout 把系统搞坏了。
func refreshWhy(cmd string, err error) string {
	if looksLocked(err) {
		return fmt.Sprintf("%s 正被别的进程占用（自动更新常在跑），索引没刷新，仓库版本号可能偏旧", cmd)
	}
	return fmt.Sprintf("%s 没能刷新索引，仓库版本号可能偏旧：%v", cmd, err)
}

// runRepoQuery 跑一条只读的查询命令。比升级命令的超时短得多：查询卡住说明
// 镜像源连不上，那种时候让用户看到"查不到"比让请求挂十分钟有用。
func runRepoQuery(argv []string, to time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	// 和升级一样必须在母机命名空间里跑：请求线程可能正挂在某条隧道的 netns 上，
	// 在那里面问镜像源会走 VPN，甚至根本不通（见 netnsguard.go）。
	var out string
	err := inMainNetns(func() error {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "LANG=C", "DEBIAN_FRONTEND=noninteractive")
		blob, err := cmd.CombinedOutput()
		out = string(blob)
		if err != nil {
			return fmt.Errorf("%v：%s", err, tailLines(out, 3))
		}
		return nil
	})
	return out, err
}

// parseAptCandidate 取 apt-cache policy 里的 Candidate 行。
// "(none)" 是索引里真没有这个包，不是解析失败。
func parseAptCandidate(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key != "Candidate" {
			continue
		}
		v := strings.Trim(strings.TrimSpace(rest), "()")
		if v == "" || v == "none" {
			return ""
		}
		return v
	}
	return ""
}

// parseDnfListRow 取 `dnf/yum list openvpn` 里 openvpn 那一行的版本。
//
// 用 list 而不是 list available：available 只列"没装着的更新版"，本机已经装到
// 仓库最新时它一句 "No matching Packages" 就走了，那样根本问不出"仓库是多少"。
// list 会把 Installed 和 Available 两段都印出来，形状是
// "openvpn.x86_64   1:2.6.11-1.el8   baseos"，版本号在第二列，
// 前缀的 epoch（"1:"）不是版本的一部分，去掉。Available 段排在 Installed 段后面，
// 所以取**最后**那一行才是仓库版本；只有一段时说明仓库里没有更新的，取它也对。
func parseDnfListRow(out string) string {
	ver := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// 认 "openvpn.x86_64" 和 "openvpn-2.4.11-1.el7.x86_64" 两种写法，
		// 但别把 openvpn-auth-ldap 这种同前缀的插件当成 openvpn 本体的版本
		head, _, _ := strings.Cut(f[0], ".")
		if head != "openvpn" && !strings.HasPrefix(f[0], "openvpn-") {
			continue
		}
		v := f[1]
		if _, after, ok := strings.Cut(v, ":"); ok && after != "" {
			v = after
		}
		ver = v
	}
	return ver
}

// parseApkPolicy 从 `apk policy openvpn` 里挑出仓库能给的最高版本。
//
// 输出长这样，两种形状都带版本号：
//
//	openvpn-2.6.11-r0:
//	  2.6.11-r0:
//	    V: https://dl-cdn.alpinelinux.org/alpine/v3.21/main
//
// 首行是 apk 选中的那份（装了且仓库没更新时就是已装版本），缩进行是各个仓库候选。
// 只取首行会把"已装版本"当成"仓库版本"，所以把所有版本号收齐再取最高的那个。
func parseApkPolicy(out string) string {
	best := ""
	consider := func(v string) {
		if v == "" {
			return
		}
		if best == "" || versionLess(best, v) {
			best = v
		}
	}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasSuffix(l, ":") {
			continue // V:/A:/P: 那些字段行，不是版本号
		}
		l = strings.TrimSuffix(l, ":")
		if head, _, ok := strings.Cut(l, "-"); ok && head == "openvpn" {
			consider(strings.TrimPrefix(l, "openvpn-"))
			continue
		}
		if r := []rune(l); len(r) > 0 && r[0] >= '0' && r[0] <= '9' {
			consider(l) // 缩进的纯版本号行
		}
	}
	return best
}

// parseColonVersion 取 "Version        : 2.6.11-1" 这种键值行（pacman -Si / zypper info）。
func parseColonVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "Version" {
			continue
		}
		if v := strings.TrimSpace(rest); v != "" {
			return v
		}
	}
	return ""
}

// describeSteps 把待执行的命令拼成一行给人看。按钮点下去之前得先知道要点什么。
func describeSteps(steps [][]string) string {
	joined := make([]string, 0, len(steps))
	for _, argv := range steps {
		joined = append(joined, strings.Join(argv, " "))
	}
	return strings.Join(joined, " && ")
}

// runUpgradeSteps 依次执行包管理器命令。任何一条非零退出就停下，把那几条的
// 输出尾巴带回去——包管理器失败时原因总在最后几行。
//
// 必须在母机命名空间里跑：请求线程有可能已经被某条隧道的 dialer 带进 netns，
// 在那里面刷镜像源会走 VPN，甚至直接不通（见 netnsguard.go）。
func runUpgradeSteps(steps [][]string) error {
	return inMainNetns(func() error {
		for _, argv := range steps {
			out, err := runPackageCmd(argv)
			if err != nil {
				return fmt.Errorf("%s 失败: %v\n%s", strings.Join(argv, " "), err, tailLines(out, 8))
			}
		}
		return nil
	})
}

// runPackageCmd 跑一条包管理器命令。非交互环境变量是关键：Ubuntu 的
// needrestart 和 dpkg 在无人值守时提问，答案没人给，命令会一直挂到超时。
func runPackageCmd(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("空命令")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(),
		"DEBIAN_FRONTEND=noninteractive",
		"NEEDRESTART_MODE=a",
		"APT_LISTCHANGES_FRONTEND=none",
		"LANG=C",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tailLines 取输出末尾 n 行，用于把包管理器的报错带进界面。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ───────────── OpenVPN 官方 apt 源（只有 deb 家族有二进制包） ─────────────
//
// OpenVPN 项目自己给 Debian/Ubuntu 发包，仓库形状写在官方 wiki
// 「Debian / Ubuntu: Using OpenVPN apt repositories」那一节：
//
//	deb [arch=amd64 signed-by=<keyring>] https://build.openvpn.net/debian/openvpn/<channel> <codename> main
//
// 别把它当"最新版"：这份源比 GitHub release 滞后得很厉害。实测（2026-11）
// Ubuntu 26.04(resolute) 的 release/2.7 有 2.7.1，可 Ubuntu 24.04(noble) 的 stable
// 只到 2.6.10 而 Ubuntu 自己发的是 2.6.19，Debian 12(bookworm) 的 stable 更是停在 2.6.0
// ——不比版本就接上源装，会把后两台机器**降级**。所以这里只把它当另一个候选版本来源问，
// 问到的号确实比本机高才用它，并且只碰 fanout 自己那两份文件。
const (
	ovpnAptBase   = "https://build.openvpn.net/debian/openvpn"
	ovpnAptKeyURL = "https://swupdate.openvpn.net/repos/repo-public.gpg"
	// 官方那把签名钥匙的指纹是 30EBF4E73CCE63EEE124DD278E6DA8B4E158C569（2011 年生成，
	// 2030 年到期）。这里钉的是文件摘要：apt 一旦认了这把钥匙，源里发什么它就装什么，
	// HTTPS 只证明没被改道、证明不了拿到的就是 OpenVPN 那把，所以先比摘要再交给 apt。
	ovpnAptKeySHA256 = "1937b44fbdf4cbe0e187d25fda81dff000bfc9a614975cfb8db83e8945ce710d"
	// 文件名带 fanout 前缀：用户自己照 wiki 手动配过的话两份东西互不影响，
	// 撤销时也删得动我们写的那两份。
	ovpnAptKeyring = "/etc/apt/keyrings/fanout-openvpn-repo-public.asc"
	ovpnAptSources = "/etc/apt/sources.list.d/fanout-openvpn.list"
	// 扫 sources 时用它判断"这台机器上已经有人配过官方源了"
	ovpnAptMarker = "build.openvpn.net/debian/openvpn"
)

// ovpnAptChannels：release/2.7 是 2.7 线的发布通道，stable 只收稳定版。
// 两条都问、取更高的稳定号——老 codename 上 release/2.7 只有 alpha/rc，
// 新 codename 上 stable 还停在 2.6.x，只认一条都会漏。
var ovpnAptChannels = []string{"release/2.7", "stable"}

// debArch 把 GOARCH 换成 Debian 的架构名，官方源没建的一律返回空。
func debArch(goarch string) string {
	switch goarch {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "386", "i386", "i686":
		return "i386"
	}
	return ""
}

// validDebCodename 这个值会进 URL、也会写进 apt 的配置文件，只放行 /etc/os-release
// 里正常会出现的形状（noble、bookworm、sid 这类），别把外面的字符串原样拼进去。
func validDebCodename(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return s[0] != '-' && s[0] != '.'
}

// debCodename 取发行版代号。VERSION_CODENAME 是正主，衍生版（Mint、Pop!_OS）
// 常常只写 UBUNTU_CODENAME——那恰好就是它们 apt 基座该用的代号。
func debCodename(kv map[string]string) string {
	for _, k := range []string{"VERSION_CODENAME", "UBUNTU_CODENAME"} {
		if v := strings.ToLower(strings.TrimSpace(kv[k])); validDebCodename(v) {
			return v
		}
	}
	return ""
}

// debStableVer 把索引里的 Version（"2.7.1-resolute1"）换成能跟本机比的上游号。
// Debian 的修订号在**最后一个**连字符后面，所以只能从那儿切：这个源上有
// "2.6-beta1-focal0" 这种写法，从第一个切会把 "-beta1" 剪掉、把一个测试版
// 读成稳定版 2.6。带 ~ 的（"2.7~alpha1-bookworm0"）和带 epoch 的不认——
// 这些都该由人决定要不要装，不该由一个"升到官方最新"的按钮悄悄装上机器。
func debStableVer(v string) (string, bool) {
	up := v
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		up = v[:i]
	}
	if up == "" || strings.ContainsAny(up, "~:") {
		return "", false
	}
	for _, seg := range strings.Split(up, ".") {
		if seg == "" {
			return "", false
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < '0' || seg[i] > '9' {
				return "", false
			}
		}
	}
	return up, true
}

// parseDebPkgVersion 从 apt 的 Packages 索引里取指定包那条记录的 Version。
// 记录之间空行分隔，必须整段认 Package 名：同一份索引里还有
// openvpn-systemd-resolved、openvpn-auth-ldap，按行找会抓到插件的版本。
func parseDebPkgVersion(blob, pkg string) string {
	for _, blk := range strings.Split(strings.ReplaceAll(blob, "\r\n", "\n"), "\n\n") {
		name, ver := "", ""
		for _, line := range strings.Split(blk, "\n") {
			switch {
			case strings.HasPrefix(line, "Package:"):
				name = strings.TrimSpace(line[len("Package:"):])
			case strings.HasPrefix(line, "Version:"):
				ver = strings.TrimSpace(line[len("Version:"):])
			}
		}
		if name == pkg {
			return ver
		}
	}
	return ""
}

// fetchDebPackages 拉一份 apt 索引。Go 的默认 transport 自带 Accept-Encoding
// 并自动解压，所以直接读明文 Packages 就行（这个源上约 80 KB）。
func fetchDebPackages(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fanout-clientbin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return "", err
	}
	if len(blob) == 0 {
		return "", fmt.Errorf("索引是空的")
	}
	return string(blob), nil
}

// ovpnAptRepoVer 问 OpenVPN 官方源：本机这个 codename/arch 上最高能给到哪个稳定号。
//
// official 只要问到了就带回来（界面要说清"官方源在这里只到 2.6.10"这种话），
// channel 只在它确实比本机新时才给——没东西可升却让用户去接一个第三方源是最坏的默认。
func ovpnAptRepoVer(codename, arch, have string, fetch func(string) (string, error)) (channel, official, why string) {
	switch {
	case fetch == nil:
		return "", "", ""
	case !validDebCodename(codename):
		return "", "", "/etc/os-release 里没有能用的发行版代号，不去接第三方源"
	case arch == "":
		return "", "", "OpenVPN 官方源没有本机这个 CPU 架构的构建"
	}
	for _, ch := range ovpnAptChannels {
		blob, err := fetch(ovpnAptBase + "/" + ch + "/dists/" + codename + "/main/binary-" + arch + "/Packages")
		if err != nil {
			continue // 这条通道没有这个 codename（404）是常态，不算失败
		}
		v, ok := debStableVer(parseDebPkgVersion(blob, "openvpn"))
		if !ok || v == "" {
			continue
		}
		if official == "" || versionLess(official, v) {
			official, channel = v, ch
		}
	}
	switch {
	case official == "":
		return "", "", fmt.Sprintf("OpenVPN 官方源没有 %s/%s 的稳定版构建", codename, arch)
	case !versionLess(have, official):
		// 号照报，但不给通道：为了一个更低的版本去人家机器上写第三方源，
		// 装下去还是降级
		return "", official, fmt.Sprintf("OpenVPN 官方源在 %s 上最高只到 %s，不比本机新", codename, official)
	}
	return channel, official, ""
}

// openvpnPickTarget 在「发行版仓库」和「OpenVPN 官方源」两个号里挑最高的那个，
// 并说清它来自哪边（channel 非空表示要先接官方源）。两个都不比本机新时返回空：
// 宁可不给按钮，也不会给人一条把 openvpn 降级的命令。
// 号一样时选发行版仓库——同样的结果，不必在机器上多留一个第三方源。
func openvpnPickTarget(have, repoVer, official, officialCh string) (target, channel string) {
	better := func(v string) bool { return v != "" && versionLess(have, v) }
	if better(official) && (!better(repoVer) || versionLess(repoVer, official)) {
		return official, officialCh
	}
	if better(repoVer) {
		return repoVer, ""
	}
	return "", ""
}

// ovpnAptRepoLine 是要写进 sources.list.d 的那一行，形状照官方 wiki。
// arch 和 signed-by 都钉死：不钉 arch 会在混装机器上白拉一堆索引，
// 不钉 signed-by 就等于把钥匙塞进全局信任，别人机器的其它源也跟着受影响。
func ovpnAptRepoLine(channel, arch, codename string) string {
	return fmt.Sprintf("deb [arch=%s signed-by=%s] %s/%s %s main\n", arch, ovpnAptKeyring, ovpnAptBase, channel, codename)
}

// openvpnAptRepoWired 看官方源是否已经配在这台机器上，谁配的都算。
// 已经配过还再写一份，apt 会重复拉同一份索引并警告，那是白添乱。
func openvpnAptRepoWired() bool {
	files := []string{ovpnAptSources, "/etc/apt/sources.list"}
	if more, err := filepath.Glob("/etc/apt/sources.list.d/*.list"); err == nil {
		files = append(files, more...)
	}
	for _, p := range files {
		if blob, err := os.ReadFile(p); err == nil && strings.Contains(string(blob), ovpnAptMarker) {
			return true
		}
	}
	return false
}

// openvpnAptRepoHasChannel 看 fanout 那份文件里是不是已经写着这个通道的行。
// 已经写过还再写一遍，等于把用户可能手工调过的那行覆盖掉。
func openvpnAptRepoHasChannel(channel, codename string) bool {
	blob, err := os.ReadFile(ovpnAptSources)
	if err != nil {
		return false
	}
	return strings.Contains(string(blob), ovpnAptBase+"/"+channel+" "+codename+" main")
}

// openvpnAptRepoOurs 只看 fanout 自己写的那份 sources 文件在不在——撤销只能撤自己写的。
func openvpnAptRepoOurs() bool {
	blob, err := os.ReadFile(ovpnAptSources)
	return err == nil && strings.Contains(string(blob), ovpnAptMarker)
}

// installOpenvpnAptRepo 把官方源写进机器；调用方接着跑 apt-get update 才生效。
func installOpenvpnAptRepo(channel, arch, codename string) error {
	switch {
	case !validDebAptChannel(channel):
		return fmt.Errorf("未知的 OpenVPN 官方源通道 %q", channel)
	case !validDebCodename(codename):
		return fmt.Errorf("发行版代号 %q 不像话，拒绝写进 apt 配置", codename)
	case arch == "":
		return fmt.Errorf("没有对应的 Debian 架构名，拒绝写进 apt 配置")
	}
	dir := filepath.Dir(ovpnAptKeyring)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建 %s 失败: %w", dir, err)
	}
	// 密钥先落到同目录的临时文件：同目录 Rename 才是原子的，而且校验不过时
	// 那份正在被 apt 用的钥匙压根没被动过。
	tmp, err := os.CreateTemp(dir, ".fanout-openvpn-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Close()
	if err := downloadFile(ovpnAptKeyURL, tmp.Name()); err != nil {
		return fmt.Errorf("下载 OpenVPN 官方源签名密钥失败: %w", err)
	}
	sum, err := fileSHA256(tmp.Name())
	if err != nil {
		return err
	}
	if sum != ovpnAptKeySHA256 {
		return fmt.Errorf("官方源密钥的摘要对不上（期望 %s…，实际 %s…）：官方换过钥匙的话请升级 fanout，也可以按官方 wiki 手动配置",
			ovpnAptKeySHA256[:12], shortDigest(sum))
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), ovpnAptKeyring); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", ovpnAptKeyring, err)
	}
	return os.WriteFile(ovpnAptSources, []byte(ovpnAptRepoLine(channel, arch, codename)), 0o644)
}

// shortDigest 取哈希前 12 位用于报错——整串贴进界面没人看得下去。
func shortDigest(s string) string {
	if len(s) < 12 {
		return s
	}
	return s[:12]
}

// removeOpenvpnAptRepo 撤销：只删 fanout 自己写的那两份文件，别人按 wiki 配的不动。
// 删完再刷一次索引，否则 apt 缓存里还留着这个源的包，看着像没撤干净。
func removeOpenvpnAptRepo() error {
	for _, p := range []string{ovpnAptSources, ovpnAptKeyring} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删 %s 失败: %w", p, err)
		}
	}
	if !hasCmd("apt-get") {
		return nil
	}
	_, err := runRefresh([]string{"apt-get", "update", "-qq"})
	return err
}

func validDebAptChannel(ch string) bool {
	for _, c := range ovpnAptChannels {
		if c == ch {
			return true
		}
	}
	return false
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// openvpnDecision 是这张卡和「升级」按钮共用的唯一决策点：目标版本、要不要接官方源、
// 跑哪几条命令，一次算清，两边读同一份结果。之前状态和执行各算各的，
// 界面说的号和按钮真装上去的号可以不是同一个。
type openvpnDecision struct {
	Family      string
	Steps       [][]string
	RepoVer     string // 发行版仓库里的 openvpn 版本
	RepoWhy     string
	Stale       bool
	Codename    string
	Arch        string
	Official    string // OpenVPN 官方源能给到的最高稳定号（可能不比本机新，界面要说这个数）
	OfficialWhy string
	Channel     string // 非空表示目标版本来自官方源
	NeedsWrite  bool   // 需要接源、且这台机器上还没写着这一行
	Target      string
	Wired       bool
	Why         string // 认不出家族/机器上没有对应的包管理器
}

func decideOpenVPNUpgrade(osRel, have string, hasCmd func(string) bool, fetch func(string) (string, error), ask bool) openvpnDecision {
	steps, family, err := openvpnUpgradePlan(osRel, hasCmd)
	d := openvpnDecision{Family: family, Steps: steps}
	if err != nil {
		d.Why = err.Error()
		return d
	}
	if !ask {
		return d
	}
	d.RepoVer, d.RepoWhy, d.Stale = openvpnRepoLatest(family, hasCmd)
	if family == "deb" {
		kv := parseOSRelease(osRel)
		d.Codename, d.Arch = debCodename(kv), debArch(runtime.GOARCH)
		ch, off, why := ovpnAptRepoVer(d.Codename, d.Arch, have, fetch)
		d.Official, d.OfficialWhy, d.Channel = off, why, ch
		d.Wired = openvpnAptRepoWired()
	}
	d.Target, d.Channel = openvpnPickTarget(have, d.RepoVer, d.Official, d.Channel)
	// 别人（或上一次点按钮）已经把这一行写进去了就别再写一遍：
	// 覆盖用户手工调过的 apt 配置是这里最不该干的事
	d.NeedsWrite = d.Channel != "" && !openvpnAptRepoHasChannel(d.Channel, d.Codename)
	return d
}

// applyOpenVPNUpdate 把 openvpn 升到本机装得到的最高版本。
//
// 「最高」是发行版仓库和 OpenVPN 官方 apt 源两边问完取高的：只看发行版仓库时，
// Ubuntu 26.04 上官方明明发了 2.7.1，界面却只能报仓库里的 2.6.20，那还是
// "明明有新版却说没有"。目标确实比本机新才会动接源那一步，其余情况一条 apt
// 命令就够，不往机器上多留文件。
//
// 升完再读一次版本，前后一致就是"已经是本机能装到的最新版"，这个结论有依据。
func applyOpenVPNUpdate() (string, error) {
	openvpnApplyMu.Lock()
	defer openvpnApplyMu.Unlock()

	before := parseOpenVPNVersion(probeVersion("openvpn", "--version"))
	if before == "" {
		return "", fmt.Errorf("本机没有可用的 openvpn，先在系统里装一个（apt/apk install openvpn）再回来点这个按钮")
	}
	// 有 fanout 编的那份在用时，升级系统包并不会改变隧道用的二进制——
	// 不写这句，用户会以为按下去之后出口就跑在 2.7 上了。
	shadow := ""
	if r := readOpenvpnSrcRecord(); r != nil && isExecutableFile(r.Bin) && openvpnBinary() == r.Bin {
		shadow = fmt.Sprintf("（隧道当前用的是 fanout 编的 %s，在 %s；这次只动系统包，换完仍不会用它。"+
			"要让它回到系统那份，点「撤销源码版」。）", r.Version, r.Bin)
	}
	blob, err := os.ReadFile(osReleasePath)
	if err != nil {
		return "", fmt.Errorf("读 %s 失败: %w", osReleasePath, err)
	}
	osRel := string(blob)
	d := decideOpenVPNUpgrade(osRel, before, hasCmd, fetchDebPackages, true)
	if d.Why != "" {
		return "", fmt.Errorf("%s", d.Why)
	}
	wired := ""
	if d.NeedsWrite {
		if err := installOpenvpnAptRepo(d.Channel, d.Arch, d.Codename); err != nil {
			return "", err
		}
		wired = fmt.Sprintf("（已接上 OpenVPN 官方源 %s，最高 %s；不想要就删掉 %s 和 %s 再 apt-get update）",
			d.Channel, d.Official, ovpnAptSources, ovpnAptKeyring)
	}
	if err := runUpgradeSteps(d.Steps); err != nil {
		return "", err
	}
	after := parseOpenVPNVersion(probeVersion("openvpn", "--version"))
	switch {
	case after == "":
		return "", fmt.Errorf("升级后 openvpn 跑不起来了（升级前是 %s），请检查包管理器的输出", before)
	case after == before:
		return fmt.Sprintf("系统包里的 openvpn 已是本机装得到的最新版 %s，没有变化%s。%s",
			before, wired, orFallback(shadow, "已在跑的隧道本来就用着它，不用管。")), nil
	case d.Target != "" && versionLess(after, d.Target):
		// 包管理器升了一截却没到问到的那个号（依赖不满足、索引还没生效都可能），
		// 这时候说"已升到目标版本"就是假话，按失败报出来并把当前版本写清
		return "", fmt.Errorf("openvpn 只升到 %s（升级前 %s，预期 %s），包管理器的输出里有原因%s", after, before, d.Target, shadow)
	default:
		return fmt.Sprintf("openvpn 已从 %s 升到 %s%s。%s已在跑的隧道仍挂在旧进程上（进程不会被包管理器动到），"+
			"新建出口、换节点或重启服务之后自然切到新版。", before, after, wired, shadow), nil
	}
}

// orFallback 让"没有别的话要说"时补一句默认文案，避免出现两个空格加空括号。
func orFallback(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
