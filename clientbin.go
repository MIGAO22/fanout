package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
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

// ClientBinStatus 是界面「内置客户端」这张卡要的全部信息。
type ClientBinStatus struct {
	Backend   string `json:"backend"`    // 当前节点后端：native / 3x-ui / xray-cf-lite
	Path      string `json:"path"`       // 正在用的 xray 可执行文件
	Xray      string `json:"current"`    // 本机 xray 版本，读不到为空
	OpenVPN   string `json:"openvpn"`    // 系统 openvpn 版本，只读展示
	Latest    string `json:"latest"`     // Xray-core 最新版（只有点了检查才有）
	HasUpdate bool   `json:"has_update"` // Latest 比 Xray 新
	Updatable bool   `json:"updatable"`  // 这份 xray 归 fanout 管、可以就地替换
	Reason    string `json:"reason,omitempty"`
	CheckErr  string `json:"check_error,omitempty"` // 问 GitHub 失败的原因，不影响上面的字段
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
	st.OpenVPN = parseOpenVPNVersion(probeVersion("openvpn", "--version"))

	target, why := xrayUpdateTarget(wd, st.Path, st.Backend)
	st.Updatable = target != ""
	if !st.Updatable {
		st.Reason = why
	}
	if !askLatest {
		return st
	}
	rel, err := fetchReleaseLatest(xrayReleaseRepo)
	if err != nil {
		st.CheckErr = err.Error()
		return st
	}
	st.Latest = strings.TrimSpace(rel.TagName)
	st.HasUpdate = st.Updatable && versionLess(st.Xray, st.Latest)
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
