package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些用例守的是"会往用户机器上写东西"那条路的入口条件：版本号能不能进路径、
// 下载的是不是官方那个包、摘要和签名核对得对不对、撤销会不会删到不该删的目录。

func TestOvpnSrcVersionOK(t *testing.T) {
	yes := []string{"2.7.9", "2.6.19", "1.0.0", "2.7.10", "1234.5.6"}
	no := []string{
		"", "2.7", "2", "2.7.9.1", "2.7~alpha1", "2.7.9-1ubuntu1", "2.7.9a",
		"../../etc/passwd", "2.7/9", "v2.7.9", "2.7.", ".2.7", "12345.1.1",
		"2.7.9 ; rm -rf /", "$(reboot)",
	}
	for _, v := range yes {
		if !ovpnSrcVersionOK(v) {
			t.Errorf("ovpnSrcVersionOK(%q)=false，想要 true", v)
		}
	}
	for _, v := range no {
		if ovpnSrcVersionOK(v) {
			t.Errorf("ovpnSrcVersionOK(%q)=true，这种号绝不能进文件路径", v)
		}
	}
}

func TestOvpnSrcPaths(t *testing.T) {
	if got := ovpnSrcTarName("2.7.9"); got != "openvpn-2.7.9.tar.gz" {
		t.Errorf("tar 名不对：%s", got)
	}
	if got := ovpnSrcPrefix("2.7.9"); got != "/usr/local/fanout-openvpn/2.7.9" {
		t.Errorf("安装前缀不对：%s", got)
	}
	if got := ovpnSrcBin("2.7.9"); got != "/usr/local/fanout-openvpn/2.7.9/sbin/openvpn" {
		t.Errorf("二进制路径不对：%s", got)
	}
	// 编出来的东西绝不能落在系统包的路径上，否则"不覆盖 apt 那份"这句就是假话
	if strings.HasPrefix(ovpnSrcPrefix("2.7.9"), "/usr/sbin") ||
		strings.HasPrefix(ovpnSrcBin("2.7.9"), "/usr/bin") {
		t.Fatal("源码版的路径撞上系统包目录了")
	}
	if got := ovpnSrcBuildDir("2.7.9"); !strings.HasPrefix(got, "/var/lib/fanout/") {
		t.Errorf("中间目录应在 fanout 自己的状态下：%s", got)
	}
}

func TestOvpnSrcAssets(t *testing.T) {
	rel := &releaseInfo{TagName: "v2.7.9", Assets: []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	}{
		{
			Name:   "openvpn-2.7.9.tar.gz",
			URL:    "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz",
			Digest: "sha256:c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1",
		},
		{
			Name:   "openvpn-2.7.9.tar.gz.asc",
			URL:    "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz.asc",
			Digest: "sha256:d1938f598c5f03c7d44db49d7567c1be15bdb2f8c29cfe22db465bdc4f0c9acf",
		},
		{Name: "openvpn-2.7.9.tar.gz.sig", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz.sig"},
		{Name: "source.zip", URL: "https://github.com/OpenVPN/openvpn/archive/refs/tags/v2.7.9.zip"},
	}}
	tarURL, ascURL, digest, err := ovpnSrcAssets(rel, "2.7.9")
	if err != nil {
		t.Fatalf("真实 release 的形状应该过：%v", err)
	}
	if !strings.HasSuffix(tarURL, "openvpn-2.7.9.tar.gz") || !strings.HasSuffix(ascURL, ".asc") {
		t.Errorf("挑错资产了：%s / %s", tarURL, ascURL)
	}
	// GitHub 报的是 "sha256:<hex>"，要用时必须剥掉前缀且只剩 64 位 hex
	if !ovpnSrcIsSHA256(digest) {
		t.Errorf("摘要没剥干净：%q", digest)
	}

	cases := []struct {
		name string
		rel  *releaseInfo
		ver  string
	}{
		{"空 release", nil, "2.7.9"},
		{"没有签名文件", &releaseInfo{Assets: []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{{Name: "openvpn-2.7.9.tar.gz", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz",
			Digest: "sha256:c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1"}}}, "2.7.9"},
		{"源码包换到别的 host", &releaseInfo{Assets: []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{
			{Name: "openvpn-2.7.9.tar.gz", URL: "https://evil.example/openvpn-2.7.9.tar.gz",
				Digest: "sha256:c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1"},
			{Name: "openvpn-2.7.9.tar.gz.asc", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz.asc"},
		}}, "2.7.9"},
		{"GitHub 没给摘要", &releaseInfo{Assets: []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{
			{Name: "openvpn-2.7.9.tar.gz", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz"},
			{Name: "openvpn-2.7.9.tar.gz.asc", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz.asc"},
		}}, "2.7.9"},
		{"摘要是别的算法", &releaseInfo{Assets: []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{
			{Name: "openvpn-2.7.9.tar.gz", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz",
				Digest: "sha1:0123456789abcdef0123456789abcdef01234567"},
			{Name: "openvpn-2.7.9.tar.gz.asc", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz.asc"},
		}}, "2.7.9"},
		{"版本号非法", rel, "2.7~alpha1"},
		{"资产名和版本对不上", &releaseInfo{Assets: []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{
			{Name: "openvpn-2.7.8.tar.gz", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.8.tar.gz",
				Digest: "sha256:c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1"},
			{Name: "openvpn-2.7.8.tar.gz.asc", URL: "https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.8.tar.gz.asc"},
		}}, "2.7.9"},
	}
	for _, c := range cases {
		if _, _, _, err := ovpnSrcAssets(c.rel, c.ver); err == nil {
			t.Errorf("%s：这种输入必须报错，不能开始下载", c.name)
		}
	}
}

func TestOvpnSrcDeps(t *testing.T) {
	// rpm / alpine / arch / suse 一律不给：包名各家不同，编到一半缺 pkgconf 是白等几分钟
	for _, fam := range []string{"rpm", "alpine", "arch", "suse", "", "gentoo"} {
		if got := ovpnSrcDeps(fam); got != nil {
			t.Errorf("%s 家族不该给依赖命令表， got %v", fam, got)
		}
	}
	steps := ovpnSrcDeps("deb")
	if len(steps) != 2 {
		t.Fatalf("deb 应该是刷索引 + 装包两步，拿到 %d 步", len(steps))
	}
	// 这张表是 configure.ac 里几处 AC_MSG_ERROR 对出来的，且由 .github/workflows/openvpn-src.yml
	// 在 ubuntu-24.04 上原地跑过一遍：liblzo2-dev 漏装时 configure 直接停在 "lzo enabled but missing"
	wantPkgs := []string{"build-essential", "pkg-config", "libssl-dev", "libcap-ng-dev",
		"liblzo2-dev", "liblz4-dev", "gnupg"}
	allow := map[string]bool{}
	for _, p := range wantPkgs {
		allow[p] = true
	}
	for _, s := range steps {
		line := strings.Join(s, " ")
		if s[0] != "apt-get" {
			t.Errorf("依赖只能走 apt-get：%s", line)
		}
		if strings.Contains(line, "upgrade") || strings.Contains(line, "dist-upgrade") {
			t.Errorf("装编译依赖不该顺手升级整机：%s", line)
		}
		if strings.Contains(line, " openvpn") || strings.HasSuffix(line, "openvpn") {
			// 把 openvpn 混进依赖表就等于让 apt 决定用哪一份，和"独立目录"这个前提冲突
			t.Errorf("依赖表里不该出现 openvpn 本身：%s", line)
		}
		if strings.ContainsAny(line, "&|;$`") {
			t.Errorf("命令是 argv 数组，不该出现 shell 元字符：%s", line)
		}
	}
	last := steps[len(steps)-1]
	var pkgs []string
	for _, tok := range last {
		if allow[tok] {
			pkgs = append(pkgs, tok)
		}
	}
	if strings.Join(last, " ") == "" || strings.Join(pkgs, " ") != strings.Join(wantPkgs, " ") {
		t.Errorf("依赖表必须正好是 %v，实际抓到 %v（%s）", wantPkgs, pkgs, strings.Join(last, " "))
	}
	if !strings.Contains(strings.Join(last, " "), "--no-install-recommends") {
		t.Error("必须 --no-install-recommends：装编译器不该顺手拖进一堆推荐包")
	}
}

// testRepoRoot 从当前工作目录往上找 go.mod，返回仓库根。
func testRepoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	return "", false
}

// 配方在 workflow 和 README 里还各有一份给人看的副本。漂移一次的代价是用户在自己 VPS 上
// 等十几分钟、configure 才报 "lzo enabled but missing"，所以让单测去比对字面量。
func TestOvpnSrcRecipeMirrorsWorkflow(t *testing.T) {
	steps := ovpnSrcDeps("deb")
	if len(steps) < 2 || len(steps[1]) <= 4 {
		t.Fatalf("deb 依赖表形状不对：%v", steps)
	}
	want := strings.Join(steps[1][4:], " ")
	// 两份副本各自写自己的 --prefix（$PREFIX / 具体版本），所以只比开关序列
	flags := strings.Join(ovpnSrcConfigureArgv("x")[2:], " ")
	// 副本在仓库根，而 go test 的工作目录是包目录（现在是 cmd/fanout）。往上找 go.mod
	// 定仓库根，比写死 ../../ 更耐挪动；只有"把测试二进制单独拷到别处跑"才会找不到，
	// CI 里永远找得到——所以读到了却打不开要报错，不能跳过。
	root, found := testRepoRoot()
	if !found {
		t.Skip("往上找不到 go.mod（不在仓库里跑），跳过副本比对")
	}
	for _, f := range []string{
		filepath.Join(root, ".github", "workflows", "openvpn-src.yml"),
		filepath.Join(root, "README.md"),
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读不到副本 %s：%v，两份清单的比对不能跳过", f, err)
		}
		// workflow 里那行用反斜杠续行，压平空白再比
		flat := strings.Join(strings.Fields(strings.ReplaceAll(string(b), `\`, " ")), " ")
		if !strings.Contains(flat, want) {
			t.Errorf("%s 里的依赖清单和 ovpnSrcDeps 不一致，应为：%s", f, want)
		}
		if !strings.Contains(flat, flags) {
			t.Errorf("%s 里的 configure 开关和 ovpnSrcConfigureArgv 不一致，应为：%s", f, flags)
		}
	}
}

func TestOvpnSrcStepOrder(t *testing.T) {
	// 标签按下标取用（job.Set(bStepX, …)），标签表和常量错位就会把进度显示到别的步骤上
	if len(openvpnBuildSteps) != int(bStepInstall)+1 {
		t.Fatalf("标签有 %d 条，常量到 %d，对不上", len(openvpnBuildSteps), bStepInstall)
	}
	if bStepDeps > bStepSign {
		t.Errorf("装依赖（gnupg 在这里）必须排在验签前面，得到 deps=%d sign=%d", bStepDeps, bStepSign)
	}
	if openvpnBuildSteps[bStepDeps] != "装编译依赖" {
		t.Errorf("第 %d 步的标签不对：%s", bStepDeps, openvpnBuildSteps[bStepDeps])
	}
	if openvpnBuildSteps[bStepSign] != "用 OpenVPN 官方钥匙验签" {
		t.Errorf("第 %d 步的标签不对：%s", bStepSign, openvpnBuildSteps[bStepSign])
	}
}

// 编好一次之后，官方再出 2.7.10 / 2.8.0 时这条路必须还能走：按钮重新出现、装去新目录、
// 旧目录被清理。最怕的是版本号进位被当成字符串比（2.7.10 < 2.7.9），那样按钮会永久消失。
func TestOvpnSrcNextReleaseStillWorks(t *testing.T) {
	for _, c := range [][2]string{
		{"2.7.9", "2.7.10"}, {"2.7.10", "2.7.11"}, {"2.7.99", "2.8.0"}, {"2.7.9", "2.8.0"},
	} {
		have, next := c[0], c[1]
		if !ovpnSrcVersionOK(next) {
			t.Errorf("%s 该被版本号白名单收下", next)
			continue
		}
		if !versionLess(have, next) {
			t.Errorf("versionLess(%s, %s) 必须是 true，否则按钮再也不出现", have, next)
		}
		// apt 仍然够不到、编好的就是上一版 → 该给按钮
		if !openvpnCanBuildSource("deb", have, "2.6.19", next, have) {
			t.Errorf("手上是自编译的 %s、官方出了 %s，按钮该再出现", have, next)
		}
		// 已经编到这一版了就不该再让点一次（一次是十几分钟）
		if openvpnCanBuildSource("deb", next, "2.6.19", next, next) {
			t.Errorf("已经编好 %s，按钮该消失", next)
		}
		if ovpnSrcPrefix(have) == ovpnSrcPrefix(next) {
			t.Errorf("%s 和 %s 的前缀不该相同", have, next)
		}
		if openvpnSrcPlan(next, "deb") == "" {
			t.Errorf("%s 该有摊开的步骤", next)
		}
	}
}

func TestOvpnSrcConfigureArgv(t *testing.T) {
	argv := ovpnSrcConfigureArgv(ovpnSrcPrefix("2.7.9"))
	joined := strings.Join(argv, " ")
	if argv[0] != "./configure" {
		t.Errorf("必须在解包目录里跑那份 configure，得到 %s", argv[0])
	}
	if !strings.Contains(joined, "--prefix=/usr/local/fanout-openvpn/2.7.9") {
		t.Errorf("--prefix 必须指向独立目录：%s", joined)
	}
	// --disable-dco：不引入内核模块这条路（同时要装 libnl 才行）
	if !strings.Contains(joined, "--disable-dco") {
		t.Errorf("必须 --disable-dco：%s", joined)
	}
	// --disable-plugin-auth-pam：auth-pam 是服务端插件，fanout 用不到；开着它
	// configure 会硬要 libpam（CI 实测："libpam required but missing"）
	if !strings.Contains(joined, "--disable-plugin-auth-pam") {
		t.Errorf("必须 --disable-plugin-auth-pam：%s", joined)
	}
	if !strings.Contains(joined, "--disable-unit-tests") {
		t.Errorf("必须 --disable-unit-tests：%s", joined)
	}
	if strings.ContainsAny(joined, "&|;$`*") {
		t.Errorf("configure 参数不该有 shell 元字符：%s", joined)
	}
	if !ovpnSrcVersionOK(strings.TrimPrefix(strings.Split(argv[1], "=")[1], ovpnSrcPrefixBase+"/")) {
		t.Errorf("拼出来的前缀里版本号不合法：%s", argv[1])
	}
}

func TestOvpnSrcMakeJobsClamp(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 2: 2, 4: 4, 8: 4, 64: 4} {
		if got := ovpnSrcMakeJobs(in); got != want {
			t.Errorf("ovpnSrcMakeJobs(%d)=%d，想要 %d", in, got, want)
		}
	}
}

// 下面是 gpg 真实输出的形状（2026-11-24 用 gpg 2.4.9 验 OpenVPN/openvpn v2.7.9 抓下来的）。
// 解析规则一改就会让验签静默失灵，所以原文钉在这儿。

const gpgColonsFixture = `tru::1:1795455854:0:3:1:5
pub:-:4096:1:12F5F7B42F2B01E7:1486642505:1801998002::-:::scESC::::::23::0:
fpr:::::::::F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7:
uid:-::::1681940617::B8CF41916373C41518C22DFC73DD57DAE9F55A95::OpenVPN - Security Mailing List <security@openvpn.net>::::::::::0:
sub:-:4096:1:822F6DF096D04874:1782904337:1814349583:::::s::::::23:
fpr:::::::::33DA8C9EAE0EAE8C73172C10822F6DF096D04874:
sub:e:4096:1:389251C768B5BC16:1750345845:1782813590:::::s::::::23:
fpr:::::::::381794B484BBAC271E44B750389251C768B5BC16:
`

const gpgVerifyStatusFixture = `[GNUPG:] NEWSIG
gpg: Signature made Thu Nov  5 21:25:01 2026
gpg:                using RSA key 33DA8C9EAE0EAE8C73172C10822F6DF096D04874
[GNUPG:] KEY_CONSIDERED F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7 0
[GNUPG:] SIG_ID JGpQXpqUUd6xvpm/RGwVXDiuNvk 2026-11-05 1793885101
[GNUPG:] GOODSIG 822F6DF096D04874 OpenVPN - Security Mailing List <security@openvpn.net>
gpg: Good signature from "OpenVPN - Security Mailing List <security@openvpn.net>" [unknown]
[GNUPG:] VALIDSIG 33DA8C9EAE0EAE8C73172C10822F6DF096D04874 2026-11-05 1793885101 0 4 0 1 10 00 F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7
[GNUPG:] TRUST_UNDEFINED 0 pgp
`

func TestGpgKeyFingerprints(t *testing.T) {
	fprs := gpgKeyFingerprints(gpgColonsFixture)
	want := []string{
		"F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7",
		"33DA8C9EAE0EAE8C73172C10822F6DF096D04874",
		"381794B484BBAC271E44B750389251C768B5BC16",
	}
	if len(fprs) != len(want) {
		t.Fatalf("取到 %d 条指纹，想要 %d 条：%v", len(fprs), len(want), fprs)
	}
	for i, w := range want {
		if fprs[i] != w {
			t.Errorf("第 %d 条指纹不对：%s != %s", i, fprs[i], w)
		}
	}
	// 主钥匙必须能认出来——verifyOpenVPNTarball 靠这条判断下载到的钥匙对不对
	if !ovpnSrcPinnedSigner(fprs[0]) {
		t.Errorf("钉住的主钥匙指纹和真实钥匙环对不上：%s", fprs[0])
	}
	// uid 行里那串 40 位是用户 ID 校验和，不能被抓成指纹
	for _, f := range gpgKeyFingerprints("uid:-::::1681940617::B8CF41916373C41518C22DFC73DD57DAE9F55A95::X:::") {
		t.Errorf("uid 行的校验和被当成指纹了：%s", f)
	}
	if got := gpgKeyFingerprints("fpr:::::::::TOOSHORT:\nfpr:::"); len(got) != 0 {
		t.Errorf("残缺行不该产出指纹：%v", got)
	}
}

func TestGpgSignerFingerprint(t *testing.T) {
	if got := gpgSignerFingerprint(gpgVerifyStatusFixture); got != "33DA8C9EAE0EAE8C73172C10822F6DF096D04874" {
		t.Errorf("签名子钥匙指纹取错：%q", got)
	}
	// 改坏了的文件：只有 BADSIG，没有 GOODSIG/VALIDSIG —— 必须当作"没验过"
	bad := "[GNUPG:] NEWSIG\n[GNUPG:] BADSIG 822F6DF096D04874 OpenVPN - Security Mailing List\n"
	if got := gpgSignerFingerprint(bad); got != "" {
		t.Errorf("BADSIG 不该解析出签名者：%q", got)
	}
	// 只有 VALIDSIG 却没有 GOODSIG（gpg 一般不会这么发，但判定顺序不能反）
	onlyValid := "[GNUPG:] VALIDSIG 33DA8C9EAE0EAE8C73172C10822F6DF096D04874 2026-11-05 1 0 4 0 1 10 00 F554A3687412CFFEBDEFE0A312F5F7B42F2B01E7\n"
	if got := gpgSignerFingerprint(onlyValid); got != "" {
		t.Errorf("没有 GOODSIG 就不能认：%q", got)
	}
	// 陌生钥匙签的：NO_PUBKEY，什么都取不到
	noKey := "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG CAFE00000000BEEF 1 10 00 1793885101 9 0\n[GNUPG:] NO_PUBKEY CAFE00000000BEEF\n"
	if got := gpgSignerFingerprint(noKey); got != "" {
		t.Errorf("NO_PUBKEY 不该给出签名者：%q", got)
	}
}

func TestGpgSigPrimaryFingerprint(t *testing.T) {
	if got := gpgSigPrimaryFingerprint(gpgVerifyStatusFixture); !ovpnSrcPinnedSigner(got) {
		t.Errorf("VALIDSIG 行末的主钥匙指纹应是钉住那把，得到 %q", got)
	}
	if got := gpgSigPrimaryFingerprint("[GNUPG:] VALIDSIG 33DA8C9EAE0EAE8C73172C10822F6DF096D04874 2026-11-05 1793885101 0 4 0 1 10 00 CAFE00000000000000000000000000000000BEEF"); got == "" || ovpnSrcPinnedSigner(got) {
		t.Errorf("别的主钥匙应被取出来交给调用方拒绝，得到 %q", got)
	}
	if got := gpgSigPrimaryFingerprint("[GNUPG:] VALIDSIG 33DA8C9EAE0EAE8C73172C10822F6DF096D04874"); got != "" {
		t.Errorf("字段不够时不该硬取末位：%q", got)
	}
	if got := gpgSigPrimaryFingerprint(""); got != "" {
		t.Errorf("空输入应得空串：%q", got)
	}
}

func TestOvpnSrcDeletable(t *testing.T) {
	ok := [][2]string{
		{"/usr/local/fanout-openvpn/2.7.9", "2.7.9"},
		{"/usr/local/fanout-openvpn/2.6.19", "2.6.19"},
	}
	for _, c := range ok {
		if !ovpnSrcDeletable(c[0], c[1]) {
			t.Errorf("%s / %s 应该可删", c[0], c[1])
		}
	}
	// 记录文件是 root 可写的：撤销这一步不能照着它删任意路径
	no := [][2]string{
		{"/", ""}, {"/usr", "2.7.9"}, {"/etc", "2.7.9"},
		{"/usr/sbin/openvpn", "2.7.9"},
		{"/usr/local/fanout-openvpn", "2.7.9"},
		{"/usr/local/fanout-openvpn/2.6.19", "2.7.9"}, // 版本和目录不是一个号
		{"/usr/local/fanout-openvpn/../etc", "2.7.9"},
		{"/var/lib/fanout/openvpn", "2.7.9"},
		{"", "2.7.9"},
	}
	for _, c := range no {
		if ovpnSrcDeletable(c[0], c[1]) {
			t.Errorf("%q / %q 绝不能删", c[0], c[1])
		}
	}
}

func TestOvpnSrcPlan(t *testing.T) {
	plan := openvpnSrcPlan("2.7.9", "deb")
	if plan == "" {
		t.Fatal("deb 家族应给出摊开的步骤")
	}
	for _, want := range []string{
		"https://github.com/OpenVPN/openvpn/releases/download/v2.7.9/openvpn-2.7.9.tar.gz",
		"openvpn-2.7.9.tar.gz.asc",
		"apt-get update -qq",
		"--no-install-recommends",
		"./configure --prefix=/usr/local/fanout-openvpn/2.7.9 --disable-dco",
		"sha256",
		"/usr/local/fanout-openvpn/2.7.9/sbin/openvpn",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("确认框里必须看得见 %q，实际：%s", want, plan)
		}
	}
	if openvpnSrcPlan("2.7~alpha1", "deb") != "" {
		t.Error("非法版本号不该生成计划")
	}
	// 摊给人看的顺序必须就是真跑的顺序：gnupg 在依赖表里，装依赖就得排在验签前面
	iDeps := strings.Index(plan, "apt-get update -qq")
	iDl := strings.Index(plan, "下载 https://github.com")
	iSign := strings.Index(plan, "验签名")
	if !(iDeps >= 0 && iDeps < iDl && iDl < iSign) {
		t.Errorf("计划顺序应是一条直线 装依赖→下载→验签，实际 %d/%d/%d", iDeps, iDl, iSign)
	}
	// 非 deb 没有依赖表，就不该编出一份"看起来能跑"的步骤
	if p := openvpnSrcPlan("2.7.9", "rpm"); strings.Contains(p, "apt-get") {
		t.Errorf("rpm 家族不该出现 apt 命令：%s", p)
	}
}

func TestOpenvpnCanBuildSource(t *testing.T) {
	// family, have, aptTarget, upstream, built -> 该不该出现"自己编译"
	cases := []struct {
		what string
		f    [5]string
		want bool
	}{
		// noble：仓库 2.6.19、官方 apt 源只到 2.6.10（比本机旧，target 为空）→ 只能编
		{"noble 够不到 2.7.9", [5]string{"deb", "2.6.19", "", "2.7.9", ""}, true},
		// 同一个号已经编好在用 → 不许再出现按钮，点一次是十几分钟
		{"已编好 2.7.9", [5]string{"deb", "2.7.9", "", "2.7.9", "2.7.9"}, false},
		// 官方又出了小版本 → 重新给路（自己编的这份没人跟安全更新）
		{"已编 2.7.9，官方 2.7.10", [5]string{"deb", "2.7.9", "", "2.7.10", "2.7.9"}, true},
		// stonking 上官方 apt 源就有 2.7.9：升级按钮能装到，不该去编
		{"apt 就能装到官方最新", [5]string{"deb", "2.6.19", "2.7.9", "2.7.9", ""}, false},
		// resolute：apt 最高 2.7.1，仍比官方 2.7.9 旧 → 给编（两颗按钮并存）
		{"apt 只到 2.7.1", [5]string{"deb", "2.6.19", "2.7.1", "2.7.9", ""}, true},
		// 本机已经不比官方旧，没有理由编
		{"本机已是官方最新", [5]string{"deb", "2.7.9", "", "2.7.9", ""}, false},
		{"本机比官方还新", [5]string{"deb", "2.8.0", "", "2.7.9", ""}, false},
		// GitHub 没答上来时不判断，更不诱导编译
		{"没问到官方版本", [5]string{"deb", "2.6.19", "", "", ""}, false},
		{"本机没装 openvpn", [5]string{"deb", "", "", "2.7.9", ""}, false},
		// 依赖包名表只给 deb（和 ovpnSrcDeps 同一个口径）
		{"rpm 家族", [5]string{"rpm", "2.6.19", "", "2.7.9", ""}, false},
		{"alpine 家族", [5]string{"alpine", "2.6.19", "", "2.7.9", ""}, false},
		{"家族认不出", [5]string{"", "2.6.19", "", "2.7.9", ""}, false},
	}
	for _, c := range cases {
		if got := openvpnCanBuildSource(c.f[0], c.f[1], c.f[2], c.f[3], c.f[4]); got != c.want {
			t.Errorf("%s：openvpnCanBuildSource(%v)=%v，想要 %v", c.what, c.f, got, c.want)
		}
	}
}

func TestOvpnSrcSigners(t *testing.T) {
	if len(ovpnSrcSigners) != 1 {
		t.Fatalf("钉住的钥匙应当只有一把，实际 %v", ovpnSrcSigners)
	}
	fpr := ovpnSrcSigners[0]
	if !ovpnSrcIsFingerprint(strings.ToLower(fpr)) {
		t.Errorf("主钥匙指纹形状不对：%s", fpr)
	}
	if strings.ToUpper(fpr) != fpr {
		t.Errorf("指纹应统一大写十六进制：%s", fpr)
	}
	// 摘要和指纹是两个长度，判据不能共用一个：把 64 位的规则套在 40 位上恒为假，
	// 验签会安静地一个钥匙都不认（这个坑真踩过）
	if ovpnSrcIsFingerprint("c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1") {
		t.Error("40 位的判据不该认下 64 位摘要")
	}
	if !ovpnSrcIsSHA256("c42a6ed892ddfe2d6e3317a5040f19bec5827564affb2fe3f2b3904a94daacb1") {
		t.Error("真实 sha256 应被认下")
	}
	if ovpnSrcIsSHA256("f554a3687412cffebdefe0a312f5f7b42f2b01e7") {
		t.Error("64 位的判据不该认下 40 位指纹")
	}
	if ovpnSrcIsFingerprint(strings.ToUpper(fpr[:39] + "g")) {
		t.Error("非十六进制的 40 位串不该被认下")
	}
	if !ovpnSrcIsFingerprint(strings.ToLower(fpr)) {
		t.Error("指纹判定应先统一大小写")
	}
}
