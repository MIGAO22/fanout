package main

// OpenVPN 源码编译：apt 够不到官方最新时的最后一条路。
//
// 官方 apt 源只在最新的 Ubuntu 系列上给稳定版 2.7.x。实测（2026-11，直接读
// build.openvpn.net 的 Packages 原文）：stonking（Ubuntu 26.10）是 2.7.9、
// resolute（26.04）是 2.7.1，而 noble / bookworm / jammy / plucky 的 2.7 通道里躺的是
// 2.7~alpha1，trixie 是 2.7~beta1，questing 是 2.7~rc1。预发布号数字上比 2.6.19 大，
// 实际比本机那份旧两年，debStableVer 见到 ~ 就拒——拒了之后 noble / bookworm 想上
// 2.7.9 就只剩源码编译这一条。
//
// 风险写在界面上也写在这里：编出来的那份装进 /usr/local/fanout-openvpn/<版本> 这棵
// 独立的树，不覆盖 apt 的 /usr/sbin/openvpn。fanout 在自己的记录文件里记住"隧道该
// exec 哪一个"，撤销就是删掉这棵树加这条记录，apt 那份原地还在。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	ovpnSrcPrefixBase = "/usr/local/fanout-openvpn"
	ovpnSrcStateDir   = "/var/lib/fanout/openvpn"
	ovpnSrcRecordFile = "/var/lib/fanout/openvpn/source.json"
	ovpnSrcKeyFile    = "/var/lib/fanout/openvpn/release-signing.pgp"
	ovpnSrcGnuDir     = "/var/lib/fanout/openvpn/gnupg"
	ovpnSrcKeyURLBase = "https://keys.openpgp.org/vks/v1/by-fingerprint/"
)

// ovpnSrcSigners 钉的是 OpenVPN 的**主钥匙**指纹，不是签名用的子钥匙。
//
// 子钥匙一年一换：v2.6.11 用 533A6860529F23C5、v2.6.19 用 389251C768B5BC16、
// v2.7.9 用 822F6DF096D04874，实测三次的 .asc 都验证到 2017-02-09 生成的这把主钥匙。
// 钉子钥匙等于每年让这条路断一次；钉主钥匙既稳，又挡得住"官方换了把别的钥匙"这种
// 偷梁换柱。主钥匙 UID 是 security@openvpn.net，2027-02-07 到期——官方换主钥匙时这里
// 必须跟着换，届时验签会明确报出实际签名指纹，不会静默放过一把陌生的钥匙。
var ovpnSrcSigners = []string{"F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7"}

// openvpnSrcBuild 是"这台机器上有一份 fanout 自己编的 openvpn"这条记录。
type openvpnSrcBuild struct {
	Version string `json:"version"`
	Prefix  string `json:"prefix"`
	Bin     string `json:"bin"`
	SHA256  string `json:"sha256"`
	Signer  string `json:"signer"`
	BuiltAt string `json:"built_at"`
}

// ovpnSrcVersionOK：版本号要能安全拼进路径、URL 和目录名。
// GitHub 的 tag 是外部输入，这里只收纯数字三段、每段最多 4 位；
// 带斜杠、带点、带 ~ 的号一律不许进文件系统路径。
func ovpnSrcVersionOK(v string) bool {
	if v == "" || len(v) > 15 {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 4 {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func isHexLower(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func ovpnSrcIsSHA256(s string) bool { return len(s) == 64 && isHexLower(s) }

// ovpnSrcIsFingerprint 认的是 OpenPGP v4 钥匙指纹：40 位十六进制。
// 别拿摘要那个判据凑数——64 位的规则套在 40 位上恒为假，验签会安静地一个都不认。
func ovpnSrcIsFingerprint(s string) bool { return len(s) == 40 && isHexLower(s) }

func ovpnSrcTarName(ver string) string  { return "openvpn-" + ver + ".tar.gz" }
func ovpnSrcPrefix(ver string) string   { return ovpnSrcPrefixBase + "/" + ver }
func ovpnSrcBin(ver string) string      { return ovpnSrcPrefix(ver) + "/sbin/openvpn" }
func ovpnSrcBuildDir(ver string) string { return ovpnSrcStateDir + "/build-" + ver }

// ovpnSrcAssets 从 release 里挑出源码包、它的签名，以及 GitHub 报的 sha256。
//
// 名字必须精确等于版本号，地址必须还在 OpenVPN/openvpn 的 release 下载路径下——
// 少一条判据，哪天资产被人改名或挪去别的 host，这里就会去下一份没人签过的东西。
func ovpnSrcAssets(rel *releaseInfo, ver string) (tarURL, ascURL, digest string, err error) {
	if rel == nil {
		return "", "", "", fmt.Errorf("没有拿到 release")
	}
	if !ovpnSrcVersionOK(ver) {
		return "", "", "", fmt.Errorf("版本号 %q 不能用来拼下载路径", ver)
	}
	tarName := ovpnSrcTarName(ver)
	wantPrefix := "https://github.com/" + openvpnReleaseRepo + "/releases/download/"
	for _, a := range rel.Assets {
		switch a.Name {
		case tarName:
			if !strings.HasPrefix(a.URL, wantPrefix) {
				return "", "", "", fmt.Errorf("源码包地址不像官方 release：%s", a.URL)
			}
			tarURL = a.URL
			digest = strings.ToLower(strings.TrimPrefix(a.Digest, "sha256:"))
		case tarName + ".asc":
			if !strings.HasPrefix(a.URL, wantPrefix) {
				return "", "", "", fmt.Errorf("签名文件地址不像官方 release：%s", a.URL)
			}
			ascURL = a.URL
		}
	}
	if tarURL == "" || ascURL == "" {
		return "", "", "", fmt.Errorf("release %s 里找不到 %s 或它的 .asc", ver, tarName)
	}
	if !ovpnSrcIsSHA256(digest) {
		return "", "", "", fmt.Errorf("GitHub 给的源码包摘要不是 sha256（%q）", digest)
	}
	return tarURL, ascURL, digest, nil
}

// ovpnSrcConfigureArgv 里 --disable-dco 是刻意的：DCO 要么靠内核 6.16+ 自带的 ovpn
// 模块，要么装 out-of-tree 的 kmod 包，两条都不该由一个"升级客户端"的按钮顺手引入；
// 顺带也免掉 libnl-genl-3-dev（configure.ac 里它是启用 DCO 时的硬要求）。
// --disable-unit-tests 单纯省几分钟。两个选项都对照 2.7.9 tarball 里预生成的
// configure --help 核过，不存在的选项会让 configure 报 unrecognized。
func ovpnSrcConfigureArgv(prefix string) []string {
	return []string{"./configure", "--prefix=" + prefix, "--disable-dco", "--disable-unit-tests"}
}

// ovpnSrcMakeJobs 给并发度封顶 4：出口机常常是 1-2G 内存的小 VPS，
// -j$(nproc) 在 8 核机器上能把编译吃进 swap，反而比少开几个线程更慢。
func ovpnSrcMakeJobs(n int) int {
	if n < 1 {
		return 1
	}
	if n > 4 {
		return 4
	}
	return n
}

// ovpnSrcDeps 是编译 openvpn 2.7 需要的包，只给 deb 家族。
//
// 依据是 tarball 里 INSTALL 的 SYSTEM REQUIREMENTS 和 configure.ac：OpenSSL 必需；
// Linux 上缺 libcap-ng 是 AC_MSG_ERROR（硬失败，不是警告），所以 libcap-ng-dev 必装；
// gnupg 用来验官方签名。
//
// rpm 家族故意不给：pkgconf / openssl-devel / libcap-ng-devel 的包名在 RHEL、Alma、
// Amazon Linux 上各不相同，装错一个就是白等几分钟，那种情况不如让人手动来。
func ovpnSrcDeps(family string) [][]string {
	if family != "deb" {
		return nil
	}
	return [][]string{
		{"apt-get", "update", "-qq"},
		{"apt-get", "install", "-y", "--no-install-recommends",
			"build-essential", "pkg-config", "libssl-dev", "libcap-ng-dev", "gnupg"},
	}
}

// gpgKeyFingerprints 从 gpg --with-colons 的输出里取所有指纹（主钥匙和子钥匙都要）。
// 形状是 pub: 行后面紧跟 fpr: 行，第 10 个字段就是指纹。
func gpgKeyFingerprints(colons string) []string {
	var out []string
	for _, l := range strings.Split(colons, "\n") {
		f := strings.Split(l, ":")
		if len(f) < 11 || f[0] != "fpr" {
			continue
		}
		fp := strings.ToUpper(strings.TrimSpace(f[9]))
		if ovpnSrcIsFingerprint(strings.ToLower(fp)) {
			out = append(out, fp)
		}
	}
	return out
}

// gpgSignerFingerprint 从 gpg --status-fd 1 的输出里取签名用的那把钥匙指纹。
// 必须同时看到 GOODSIG 和 VALIDSIG：一把钥匙都没导入时也能有 ERDSIG/NO_PUBKEY 之类
// 的噪音，只按 VALIDSIG 找会把"验不过"说成"验过了"。
func gpgSignerFingerprint(status string) string {
	good := false
	fpr := ""
	for _, l := range strings.Split(status, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "[GNUPG:] GOODSIG"):
			good = true
		case strings.HasPrefix(l, "[GNUPG:] VALIDSIG "):
			f := strings.Fields(l)
			if len(f) >= 4 {
				fpr = strings.ToUpper(f[2])
			}
		}
	}
	if !good {
		return ""
	}
	return fpr
}

func ovpnSrcPinnedSigner(fpr string) bool {
	for _, p := range ovpnSrcSigners {
		if strings.EqualFold(p, fpr) {
			return true
		}
	}
	return false
}

// gpgSigPrimaryFingerprint 取 VALIDSIG 行末尾那个"主钥匙指纹"字段。
// 实测形状（gpg 2.4）：
//
//	[GNUPG:] VALIDSIG <子钥匙fpr> <日期> <ts> 0 4 0 1 10 00 <主钥匙fpr>
//
// 拿它和钉住的主钥匙再对一遍，是为了不单纯依赖"钥匙环里恰好只有这一把"这个前提。
func gpgSigPrimaryFingerprint(status string) string {
	for _, l := range strings.Split(status, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "[GNUPG:] VALIDSIG ") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 4 {
			return ""
		}
		last := strings.ToUpper(f[len(f)-1])
		if ovpnSrcIsFingerprint(strings.ToLower(last)) {
			return last
		}
		return ""
	}
	return ""
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return fi.Mode().Perm()&0111 != 0
}

func readOpenvpnSrcRecord() *openvpnSrcBuild {
	blob, err := os.ReadFile(ovpnSrcRecordFile)
	if err != nil {
		return nil
	}
	var r openvpnSrcBuild
	if err := json.Unmarshal(blob, &r); err != nil {
		return nil
	}
	if !ovpnSrcVersionOK(r.Version) || r.Bin == "" {
		return nil
	}
	return &r
}

func writeOpenvpnSrcRecord(r *openvpnSrcBuild) error {
	if !ovpnSrcVersionOK(r.Version) {
		return fmt.Errorf("版本号 %q 不能写进记录", r.Version)
	}
	if err := os.MkdirAll(ovpnSrcStateDir, 0755); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := ovpnSrcRecordFile + ".new"
	if err := os.WriteFile(tmp, blob, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, ovpnSrcRecordFile)
}

// openvpnBinary 返回隧道真正该 exec 的那一份 openvpn。
//
// 有 fanout 编过的记录且文件还在就用它，否则照旧按 PATH 找系统那份。版本探测
// （喂给 dataCipherArgs 的那个号）必须走同一个入口，否则会出现"按 2.7 的参数名
// 喂给实际是 2.6 的二进制"这种对不上号的事故。
func openvpnBinary() string {
	if r := readOpenvpnSrcRecord(); r != nil && isExecutableFile(r.Bin) {
		return r.Bin
	}
	return "openvpn"
}

// gpgRun 在 fanout 自己的 GNUPGHOME 里跑一条 gpg。
// 用独立 homedir 是不去动 root 的钥匙环，也不让一把只为验源码包而下下来的钥匙
// 留在系统信任里。
func gpgRun(timeout time.Duration, args ...string) (string, error) {
	if err := os.MkdirAll(ovpnSrcGnuDir, 0700); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	base := []string{"--homedir", ovpnSrcGnuDir, "--batch", "--no-tty", "--quiet"}
	cmd := exec.CommandContext(ctx, "gpg", append(base, args...)...)
	cmd.Env = append(os.Environ(), "LANG=C")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// verifyOpenVPNTarball 用钉住的主钥匙验这份 tarball，返回实际签名的子钥匙指纹。
//
// 顺序是刻意的：先把按指纹下载的钥匙导进一个全新的 GNUPGHOME，再确认列出来的指纹里
// 确实有钉住的那把——钥匙服务器给错东西时这一步就红，不会拿一把陌生钥匙去"验通"。
// 然后才验签，并要求签名用的子钥匙属于刚导入的这串指纹，不接受"GOODSIG 但来自别的钥匙"。
func verifyOpenVPNTarball(tarPath, ascPath string) (string, error) {
	if err := os.RemoveAll(ovpnSrcGnuDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(ovpnSrcGnuDir, 0700); err != nil {
		return "", err
	}
	if !hasCmd("gpg") {
		return "", fmt.Errorf("本机没有 gpg，验不了官方签名")
	}
	if err := downloadFile(ovpnSrcKeyURLBase+ovpnSrcSigners[0], ovpnSrcKeyFile); err != nil {
		return "", fmt.Errorf("下载 OpenVPN 签名公钥失败：%v", err)
	}
	if _, err := gpgRun(60*time.Second, "--import", ovpnSrcKeyFile); err != nil {
		return "", fmt.Errorf("导入 OpenVPN 签名公钥失败：%v", err)
	}
	colons, err := gpgRun(60*time.Second, "--with-colons", "--list-keys", "--with-subkey-fingerprints")
	if err != nil {
		return "", fmt.Errorf("列出签名公钥失败：%v", err)
	}
	fprs := gpgKeyFingerprints(colons)
	pinned := false
	for _, f := range fprs {
		if ovpnSrcPinnedSigner(f) {
			pinned = true
			break
		}
	}
	if !pinned {
		return "", fmt.Errorf("下载到的钥匙指纹里没有钉住的那把（期望 %s，实际 %v）——不敢用它验包",
			ovpnSrcSigners[0], fprs)
	}
	status, err := gpgRun(2*time.Minute, "--status-fd", "1", "--verify", ascPath, tarPath)
	if err != nil {
		return "", fmt.Errorf("openvpn 源码包验签没通过：%s", firstLine(status))
	}
	signer := gpgSignerFingerprint(status)
	if signer == "" {
		return "", fmt.Errorf("验签输出里没有 GOODSIG/VALIDSIG，看不出是谁签的")
	}
	known := false
	for _, f := range fprs {
		if strings.EqualFold(f, signer) {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("签名用的子钥匙 %s 不属于钉住的那把主钥匙，拒绝这份包", signer)
	}
	// VALIDSIG 行末的主钥匙指纹再单独核一遍：钥匙环里只有钉住那把时这一步本该总是过，
	// 过不了就说明 gpg 的输出形状变了或被别的东西插进来，宁可不编。
	if primary := gpgSigPrimaryFingerprint(status); primary != "" && !ovpnSrcPinnedSigner(primary) {
		return "", fmt.Errorf("签名挂在主钥匙 %s 上，不是钉住的 %s", primary, ovpnSrcSigners[0])
	}
	return signer, nil
}

// openvpnBuildSteps 是作业步骤的标签，顺序和 runOpenVPNSourceBuild 里的下标一一对应。
var openvpnBuildSteps = []string{
	"问本机包管理器能装到哪",
	"下载源码包与签名",
	"核对 GitHub 给的摘要",
	"用 OpenVPN 官方钥匙验签",
	"装编译依赖",
	"解包",
	"configure",
	"make 编译",
	"安装并自检",
}

const (
	bStepRepo = iota
	bStepDownload
	bStepDigest
	bStepSign
	bStepDeps
	bStepUnpack
	bStepConfigure
	bStepMake
	bStepInstall
)

var (
	ovpnBuildMu sync.Mutex
	ovpnBuildOn bool
)

func ovpnBuildStart() bool {
	ovpnBuildMu.Lock()
	defer ovpnBuildMu.Unlock()
	if ovpnBuildOn {
		return false
	}
	ovpnBuildOn = true
	return true
}

func ovpnBuildFinish() {
	ovpnBuildMu.Lock()
	ovpnBuildOn = false
	ovpnBuildMu.Unlock()
}

// startOpenVPNSourceBuild 起一次源码编译，把活儿交给后台。
//
// 界面上那颗按钮的显示条件不是把关——这里会重新算一遍，尤其重新问一次本机包管理器：
// apt 就能升到官方最新时绝不该去编译（包管理器那份有人跟安全更新）。
func startOpenVPNSourceBuild(jobs *JobStore) (*Job, error) {
	if !ovpnBuildStart() {
		return nil, fmt.Errorf("已经有一个编译在跑了，等它结束")
	}
	rel, err := fetchReleaseLatest(openvpnReleaseRepo)
	if err != nil {
		ovpnBuildFinish()
		return nil, err
	}
	ver, ok := releaseTag(rel.TagName)
	if !ok || !ovpnSrcVersionOK(ver) {
		ovpnBuildFinish()
		return nil, fmt.Errorf("OpenVPN 官方最新 tag %q 不是能编译的三位版本号", rel.TagName)
	}
	tarURL, ascURL, digest, err := ovpnSrcAssets(rel, ver)
	if err != nil {
		ovpnBuildFinish()
		return nil, err
	}
	if r := readOpenvpnSrcRecord(); r != nil && r.Version == ver {
		ovpnBuildFinish()
		return nil, fmt.Errorf("openvpn %s 已经编好在用了（%s），不用重复编", ver, r.Bin)
	}
	job := jobs.New("源码编译 openvpn "+ver, openvpnBuildSteps)
	go runOpenVPNSourceBuild(job, ver, tarURL, ascURL, digest)
	return job, nil
}

// runOpenVPNSourceBuild 是真正干活的那一段。
// 任何一步失败都收尾干净：不写记录、删掉这棵树，隧道继续用 apt 那份。
func runOpenVPNSourceBuild(job *Job, ver, tarURL, ascURL, digest string) {
	defer job.Finish()
	defer ovpnBuildFinish()

	fail := func(i int, format string, a ...any) {
		job.Set(i, "failed", fmt.Sprintf(format, a...))
		cleanupOpenVPNSourceBuild(ver)
	}
	done := func(i int, detail string) { job.Set(i, "ok", detail) }

	// 1) 本机包管理器够不够得着
	job.Set(bStepRepo, "running", "")
	old := readOpenvpnSrcRecord()
	blob, err := os.ReadFile(osReleasePath)
	if err != nil {
		fail(bStepRepo, "读 %s 失败：%v", osReleasePath, err)
		return
	}
	aptVer := parseOpenVPNVersion(probeVersion("openvpn", "--version"))
	if aptVer == "" {
		fail(bStepRepo, "本机没有可用的 openvpn，先按系统包装一个再回来")
		return
	}
	d := decideOpenVPNUpgrade(string(blob), aptVer, hasCmd, fetchDebPackages, true)
	if d.Why != "" {
		fail(bStepRepo, "%s", d.Why)
		return
	}
	if d.Family != "deb" {
		fail(bStepRepo, "%s 家族的编译依赖包名各家不同，这条只给 Debian / Ubuntu 用", d.Family)
		return
	}
	if d.Target != "" && !versionLess(d.Target, ver) {
		fail(bStepRepo, "包管理器就能装到 %s（不比官方 %s 旧），请用「升级 openvpn」那个按钮", d.Target, ver)
		return
	}
	done(bStepRepo, fmt.Sprintf("%s 家族装得到的最高是 %s，官方 %s 够不到", d.Family, orNothing(d.Target), ver))

	// 2) 下载
	job.Set(bStepDownload, "running", "")
	if err := os.MkdirAll(ovpnSrcStateDir, 0755); err != nil {
		fail(bStepDownload, "%v", err)
		return
	}
	tarPath := filepath.Join(ovpnSrcStateDir, ovpnSrcTarName(ver))
	ascPath := tarPath + ".asc"
	if err := downloadFile(tarURL, tarPath); err != nil {
		fail(bStepDownload, "下载源码包失败：%v", err)
		return
	}
	if err := downloadFile(ascURL, ascPath); err != nil {
		fail(bStepDownload, "下载签名失败：%v", err)
		return
	}
	done(bStepDownload, ovpnSrcTarName(ver)+" + .asc")

	// 3) 摘要
	job.Set(bStepDigest, "running", "")
	got, err := sha256File(tarPath)
	if err != nil {
		fail(bStepDigest, "%v", err)
		return
	}
	if !strings.EqualFold(got, digest) {
		fail(bStepDigest, "源码包摘要对不上 GitHub 给的（期望 %s，实际 %s）", shortDigest(digest), shortDigest(got))
		return
	}
	done(bStepDigest, shortDigest(got))

	// 4) 验签
	job.Set(bStepSign, "running", "下载官方签名公钥")
	signer, err := verifyOpenVPNTarball(tarPath, ascPath)
	if err != nil {
		fail(bStepSign, "%v", err)
		return
	}
	done(bStepSign, "主钥匙 "+shortFingerprint(ovpnSrcSigners[0])+" 的子钥匙 "+shortFingerprint(signer))

	// 5) 依赖
	job.Set(bStepDeps, "running", "apt-get update && install build-essential libssl-dev libcap-ng-dev gnupg")
	deps := ovpnSrcDeps(d.Family)
	if len(deps) == 0 {
		fail(bStepDeps, "%s 家族没有给定的依赖包名表", d.Family)
		return
	}
	if err := runUpgradeSteps(deps); err != nil {
		fail(bStepDeps, "%v", err)
		return
	}
	done(bStepDeps, "build-essential / pkg-config / libssl-dev / libcap-ng-dev / gnupg")

	// 6) 解包
	job.Set(bStepUnpack, "running", "")
	bdir := ovpnSrcBuildDir(ver)
	if err := os.RemoveAll(bdir); err != nil {
		fail(bStepUnpack, "%v", err)
		return
	}
	if err := os.MkdirAll(bdir, 0755); err != nil {
		fail(bStepUnpack, "%v", err)
		return
	}
	if out, err := runBuildCmd(5*time.Minute, bdir, "tar", "-xzf", tarPath, "-C", bdir, "--no-same-owner"); err != nil {
		fail(bStepUnpack, "解包失败：%v\n%s", err, tailLines(out, 6))
		return
	}
	srcDir := filepath.Join(bdir, "openvpn-"+ver)
	if !dirExists(srcDir) {
		fail(bStepUnpack, "包里没有 openvpn-%s/ 这一层", ver)
		return
	}
	done(bStepUnpack, srcDir)

	// 7) configure
	argv := ovpnSrcConfigureArgv(ovpnSrcPrefix(ver))
	job.Set(bStepConfigure, "running", strings.Join(argv, " "))
	if out, err := runBuildCmd(10*time.Minute, srcDir, argv...); err != nil {
		fail(bStepConfigure, "configure 失败：%v\n%s", err, tailLines(out, 12))
		return
	}
	done(bStepConfigure, strings.Join(argv[1:], " "))

	// 8) make
	nj := ovpnSrcMakeJobs(runtime.NumCPU())
	job.Set(bStepMake, "running", fmt.Sprintf("make -j%d（几分钟到几十分钟）", nj))
	if out, err := runBuildCmd(45*time.Minute, srcDir, "make", "-j"+fmt.Sprint(nj)); err != nil {
		fail(bStepMake, "编译失败：%v\n%s", err, tailLines(out, 12))
		return
	}
	done(bStepMake, fmt.Sprintf("-j%d", nj))

	// 9) make install + 自检
	job.Set(bStepInstall, "running", "")
	if out, err := runBuildCmd(10*time.Minute, srcDir, "make", "install"); err != nil {
		fail(bStepInstall, "安装失败：%v\n%s", err, tailLines(out, 12))
		return
	}
	bin := ovpnSrcBin(ver)
	if !isExecutableFile(bin) {
		fail(bStepInstall, "装完了但 %s 不在，记录不写", bin)
		return
	}
	got2 := parseOpenVPNVersion(probeVersion(bin, "--version"))
	if got2 != ver {
		fail(bStepInstall, "编出来跑的是 %q，不是预期的 %s，记录不写", got2, ver)
		return
	}
	if err := writeOpenvpnSrcRecord(&openvpnSrcBuild{
		Version: ver,
		Prefix:  ovpnSrcPrefix(ver),
		Bin:     bin,
		SHA256:  digest,
		Signer:  ovpnSrcSigners[0],
		BuiltAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		fail(bStepInstall, "记录没写进去：%v", err)
		return
	}
	_ = os.RemoveAll(bdir)
	if old != nil && old.Version != ver && ovpnSrcVersionOK(old.Version) &&
		strings.HasPrefix(old.Prefix, ovpnSrcPrefixBase+"/") && old.Prefix != ovpnSrcPrefix(ver) {
		// 上一版编的那棵树已经没人引用了（记录被这次覆盖），留着只会占地方；
		// 已在跑的隧道挂在旧 inode 上，删目录不影响它，下次新建出口才用新版。
		_ = os.RemoveAll(old.Prefix)
	}
	done(bStepInstall, fmt.Sprintf("%s → %s（apt 那份 %s 原地没动）", ver, bin, aptVer))
}

// openvpnCanBuildSource 是「自己编译」这颗按钮的唯一判据，后端算，前端只看结果。
//
// 四个条件缺一不可：deb 家族（依赖包名只给这一家）、确实落后于官方、
// 包管理器和官方 apt 源都够不到那个号（够得到就不该编），以及那个号还没编好。
// 最后一条防的是"编完按钮还在"——点一次跑十几分钟的事，不能反复怼用户。
func openvpnCanBuildSource(family, have, aptTarget, upstream, built string) bool {
	if family != "deb" || upstream == "" || have == "" {
		return false
	}
	if !versionLess(have, upstream) {
		return false
	}
	if aptTarget != "" && !versionLess(aptTarget, upstream) {
		return false
	}
	return built != upstream
}

// openvpnSrcPlan 把"打算在你机器上做什么"原样摊开：下什么、怎么验、装哪些包、装到哪、
// 怎么切、怎么退。确认框直接显示这段，和「升级 openvpn」那颗按钮同一个规矩——
// 一个会往 /usr/local 写东西、还会 apt 装编译器的按钮，不能只写一句"确定要编译吗"。
func openvpnSrcPlan(ver, family string) string {
	if !ovpnSrcVersionOK(ver) {
		return ""
	}
	lines := []string{
		fmt.Sprintf("下载 https://github.com/%s/releases/download/v%s/%s 和签名 %s",
			openvpnReleaseRepo, ver, ovpnSrcTarName(ver), ovpnSrcTarName(ver)+".asc"),
		"核对 GitHub 给的 sha256，再用 OpenVPN 官方主钥匙 " + shortFingerprint(ovpnSrcSigners[0]) + " 验签名",
	}
	for _, s := range ovpnSrcDeps(family) {
		lines = append(lines, strings.Join(s, " "))
	}
	lines = append(lines,
		strings.Join(ovpnSrcConfigureArgv(ovpnSrcPrefix(ver)), " "),
		fmt.Sprintf("make -j%d && make install", ovpnSrcMakeJobs(runtime.NumCPU())),
		fmt.Sprintf("装进 %s，**不覆盖**系统那份；自检跑到 %s 才登记启用", ovpnSrcPrefix(ver), ovpnSrcBin(ver)),
		"任何一步失败都不登记、删掉这棵树，隧道继续用系统那份",
		fmt.Sprintf("撤销 = 删 %s 和这条记录，系统那份原地还在", ovpnSrcPrefix(ver)),
	)
	return strings.Join(lines, "\n")
}

func cleanupOpenVPNSourceBuild(ver string) {
	if !ovpnSrcVersionOK(ver) {
		return
	}
	_ = os.RemoveAll(ovpnSrcPrefix(ver))
	_ = os.RemoveAll(ovpnSrcBuildDir(ver))
}

// runBuildCmd 在指定目录跑一条构建命令。
// 不经 shell：argv 全部来自上面常量的拼接，版本号先过 ovpnSrcVersionOK，
// 路径参数里没有花活可做。
func runBuildCmd(timeout time.Duration, dir string, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LANG=C", "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ovpnSrcDeletable 判断记录里的安装前缀是不是 fanout 有权删的那棵树。
// 必须是 <base>/<同一个版本号> 这种形状：记录文件 root 可写，万一被改成 / 或 /etc，
// 撤销这一步不能照着它删。
func ovpnSrcDeletable(prefix, ver string) bool {
	return ovpnSrcVersionOK(ver) &&
		strings.HasPrefix(prefix, ovpnSrcPrefixBase+"/") &&
		prefix == ovpnSrcPrefixBase+"/"+ver
}

// removeOpenVPNSourceBuild 撤销源码版：只删 fanout 自己那棵树和那条记录。
func removeOpenVPNSourceBuild() (string, error) {
	r := readOpenvpnSrcRecord()
	if r == nil {
		return "本机没有 fanout 编过的 openvpn，系统那份照旧在用", nil
	}
	if !ovpnSrcDeletable(r.Prefix, r.Version) {
		return "", fmt.Errorf("记录里的安装前缀 %q 不在 %s 下面，不敢删", r.Prefix, ovpnSrcPrefixBase)
	}
	if err := os.RemoveAll(r.Prefix); err != nil {
		return "", fmt.Errorf("删 %s 失败：%w", r.Prefix, err)
	}
	_ = os.RemoveAll(ovpnSrcBuildDir(r.Version))
	if err := os.Remove(ovpnSrcRecordFile); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("记录删不掉：%w", err)
	}
	apt := parseOpenVPNVersion(probeVersion("openvpn", "--version"))
	return fmt.Sprintf("已删掉源码编译的 openvpn %s（%s），接下来用系统那份 %s。"+
		"已在跑的隧道挂在旧进程上，新建出口或重启服务之后才换。", r.Version, r.Prefix, orNothing(apt)), nil
}

func orNothing(s string) string {
	if s == "" {
		return "没有"
	}
	return s
}

func shortFingerprint(fpr string) string {
	if len(fpr) <= 12 {
		return fpr
	}
	return fpr[:8] + "…" + fpr[len(fpr)-4:]
}
