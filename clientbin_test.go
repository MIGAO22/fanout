package main

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXrayAssetForArch(t *testing.T) {
	cases := map[string]string{
		"amd64":    "Xray-linux-64.zip",
		"386":      "Xray-linux-32.zip",
		"arm64":    "Xray-linux-arm64-v8a.zip",
		"arm":      "Xray-linux-arm32-v7a.zip",
		"riscv64":  "Xray-linux-riscv64.zip",
		"mips64le": "Xray-linux-mips64le.zip",
		"ppc64le":  "Xray-linux-ppc64le.zip",
	}
	for arch, want := range cases {
		if got := xrayAssetForArch(arch); got != want {
			t.Errorf("%s 应该是 %s，得到 %s", arch, want, got)
		}
	}
}

func TestParseXrayVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Xray 26.2.27 (Ruyi MCP)\n", "26.2.27"},
		{"Xray 1.5.1 (Ruyi EMC)\nsome noise\n", "1.5.1"},
		{"Xray-core v25.10.5\n", "25.10.5"},
		{"  Xray 26.2.27 (Ruyi MCP)  \n", "26.2.27"},
		{"", ""},
		{"xray\n", ""}, // 只认得到版本号，认不到就留空
		{"openvpn: not found\n", ""},
	}
	for _, c := range cases {
		if got := parseXrayVersion(c.in); got != c.want {
			t.Errorf("parseXrayVersion(%q) = %q，想 %q", c.in, got, c.want)
		}
	}
}

func TestParseOpenVPNVersion(t *testing.T) {
	first := "OpenVPN 2.6.8 x86_64-pc-linux-gnu [SSL (OpenSSL)] [LZO] [LZ4] [EPOLL] [PKCS11]"
	if got := parseOpenVPNVersion(first + "\nlibrary versions:\n  OpenSSL 3.0.13\n"); got != "2.6.8" {
		t.Errorf("版本应该是 2.6.8，得到 %q", got)
	}
	for _, in := range []string{"", "OpenVPN\n", "bash: openvpn: command not found\n"} {
		if got := parseOpenVPNVersion(in); got != "" {
			t.Errorf("parseOpenVPNVersion(%q) = %q，想要空", in, got)
		}
	}
}

// 能不能替换必须只看"这份是不是 fanout 自己装的那一份"：
// 面板和包管理器的 xray 有自己的升级路径，覆盖它们是给别人改运行时。
func TestXrayUpdateTarget(t *testing.T) {
	wd := filepath.Join("/var", "lib", "fanout")
	owned := filepath.Join(wd, "bin", "xray")

	cases := []struct {
		name     string
		bin      string
		backend  string
		wantSelf bool   // 期望 target == owned
		wantWhy  string // 不能替换时期望理由里出现的词
	}{
		{"后端未定", owned, "", false, "后端"},
		{"3x-ui 接管", owned, "3x-ui", false, "3x-ui"},
		{"xray-cf-lite 接管", owned, "xray-cf-lite", false, "接管"},
		{"找不到二进制", "", "native", false, "没找到"},
		{"系统里那份", filepath.Join("/usr", "local", "bin", "xray"), "native", false, "包管理器"},
		{"自己那份", owned, "native", true, ""},
		// findXray 在某些路径下会给出带 .. 的写法，判定要按规范化后的路径比
		{"未规范化的自己那份", filepath.Join(wd, "bin", "..", "bin", "xray"), "native", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, why := xrayUpdateTarget(wd, c.bin, c.backend)
			if c.wantSelf {
				if filepath.Clean(target) != filepath.Clean(owned) {
					t.Fatalf("应该指向 %s，得到 %q（理由 %q）", owned, target, why)
				}
				if why != "" {
					t.Errorf("可以替换时不该有理由，得到 %q", why)
				}
				return
			}
			if target != "" {
				t.Fatalf("不该允许替换，却给出了 %q", target)
			}
			if !strings.Contains(why, c.wantWhy) {
				t.Errorf("理由里该有 %q，实际是 %q", c.wantWhy, why)
			}
		})
	}
}

func TestExtractFromZipPicksBinary(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "Xray-linux-64.zip")
	writeZip(t, zp, map[string]string{
		"xray":               "#!/bin/sh\necho new\n",
		"geoip.dat":          "无关内容",
		"config.json":        "{}",
		"config/health.json": "{\"a\":1}",
	})

	dst := filepath.Join(dir, "xray")
	if err := extractFromZip(zp, "xray", dst); err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	blob, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读不出解出来的文件: %v", err)
	}
	if !strings.Contains(string(blob), "echo new") {
		t.Errorf("解出来的内容不对: %q", blob)
	}

	if err := extractFromZip(zp, "no-such-member", filepath.Join(dir, "out2")); err == nil {
		t.Error("缺成员时应该报错")
	}
}

func TestExtractFromZipRejectsNonZip(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.zip")
	if err := os.WriteFile(bad, []byte("这不是 zip"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := extractFromZip(bad, "xray", filepath.Join(filepath.Dir(bad), "out")); err == nil {
		t.Fatal("坏包应该报错")
	} else if !strings.Contains(err.Error(), "压缩包") {
		t.Errorf("错误应该说是打开压缩包失败，实际: %v", err)
	}
}

// 没配过面板的机器上（包括 CI）这份状态要能安全返回：不 panic、不擅自认为可更新。
func TestClientBinStatusWithoutPanel(t *testing.T) {
	st := clientBinStatus(false)
	if st == nil {
		t.Fatal("不该返回空")
	}
	if st.Updatable {
		t.Errorf("后端还没定就报可更新，等于让界面亮一个会失败的按钮：%+v", st)
	}
	if st.Latest != "" {
		t.Errorf("没要求比对就不该打 GitHub，Latest = %q", st.Latest)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestParseOSRelease(t *testing.T) {
	kv := parseOSRelease(`# Ubuntu 22.04.4 LTS
NAME="Ubuntu"
VERSION_ID="22.04"
ID=ubuntu
ID_LIKE=debian
export HOME_URL="https://www.ubuntu.com/"
PRETTY_NAME='Ubuntu 22.04.4 LTS'
JUNK_WITHOUT_EQUALS
`)
	if kv["ID"] != "ubuntu" {
		t.Errorf("不带引号的值也要认出来，得到 %q", kv["ID"])
	}
	if kv["ID_LIKE"] != "debian" {
		t.Errorf("ID_LIKE = %q", kv["ID_LIKE"])
	}
	if kv["HOME_URL"] != "https://www.ubuntu.com/" {
		t.Errorf("export 前缀要跳过，得到 %q", kv["HOME_URL"])
	}
	if kv["PRETTY_NAME"] != "Ubuntu 22.04.4 LTS" {
		t.Errorf("单引号也要剥掉，得到 %q", kv["PRETTY_NAME"])
	}
	if _, ok := kv["JUNK_WITHOUT_EQUALS"]; ok {
		t.Error("没有等号的行不该收进来")
	}
}

func TestDistroFamily(t *testing.T) {
	cases := []struct{ id, like, want string }{
		{"ubuntu", "debian", "deb"},
		{"debian", "", "deb"},
		{"linuxmint", "ubuntu debian", "deb"},
		{"centos", "rhel fedora", "rpm"},
		{"rocky", "rhel", "rpm"},
		{"fedora", "", "rpm"},
		{"amzn", "", "rpm"},
		{"alpine", "", "alpine"},
		{"arch", "arch", "arch"},
		{"manjaro", "arch", "arch"},
		{"opensuse-leap", "suse", "suse"},
		{"sles", "", "suse"},
		{"openwrt", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		got := distroFamily(map[string]string{"ID": c.id, "ID_LIKE": c.like})
		if got != c.want {
			t.Errorf("ID=%q ID_LIKE=%q 应该认成 %q，得到 %q", c.id, c.like, c.want, got)
		}
	}
}

// refreshSteps 是只刷索引、不动任何包的命令，允许里面没有 openvpn。
var refreshSteps = map[string]bool{
	"apt-get update -qq":               true,
	"apk update --no-cache":            true,
	"zypper --non-interactive refresh": true,
	"pacman -Sy --noconfirm openvpn":   true, // Arch 的 -Sy 和安装合成一条
}

// 这个按钮只该升 openvpn 一个包：全量升级会连带升内核、会把无关服务重启一遍，
// 那不是"升级 openvpn"这个按钮承诺的事。
func TestOpenVPNUpgradePlanOnlyTouchesOpenVPN(t *testing.T) {
	plans := map[string]string{
		"ID=ubuntu\nID_LIKE=debian\n":          "deb",
		"ID=centos\nID_LIKE=\"rhel fedora\"\n": "rpm",
		"ID=alpine\n":                          "alpine",
		"ID=arch\n":                            "arch",
		"ID=opensuse-leap\nID_LIKE=suse\n":     "suse",
	}
	for blob, family := range plans {
		steps, got, err := openvpnUpgradePlan(blob, allTools)
		if err != nil {
			t.Fatalf("%s: %v", blob, err)
		}
		if got != family {
			t.Errorf("家族应该是 %s，得到 %s", family, got)
		}
		if len(steps) == 0 {
			t.Fatalf("%s 的计划是空的", family)
		}
		for _, argv := range steps {
			line := strings.Join(argv, " ")
			if refreshSteps[line] {
				continue
			}
			if !strings.Contains(line, "openvpn") {
				t.Errorf("%s 系里出现了没点名 openvpn 的一步：%s", family, line)
			}
			for _, a := range argv {
				if a == "-u" || a == "--asdeps" {
					t.Errorf("%s 里出现了会影响整台机器的参数：%s", family, line)
				}
			}
		}
	}
}

func allTools(string) bool { return true }

func TestOpenVPNUpgradePlanPrefersDnfThenYum(t *testing.T) {
	rhel8 := "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n"
	steps, family, err := openvpnUpgradePlan(rhel8, func(n string) bool { return n == "dnf" })
	if err != nil || family != "rpm" {
		t.Fatalf("family=%s err=%v", family, err)
	}
	if steps[0][0] != "dnf" {
		t.Errorf("有 dnf 就该用 dnf，得到 %v", steps[0])
	}

	steps, _, err = openvpnUpgradePlan(rhel8, func(n string) bool { return n == "yum" })
	if err != nil {
		t.Fatal(err)
	}
	if steps[0][0] != "yum" {
		t.Errorf("CentOS 7 只有 yum，得到 %v", steps[0])
	}
	if strings.Contains(describeSteps(steps), "dnf") {
		t.Errorf("描述里不该出现没在用的工具：%s", describeSteps(steps))
	}

	if _, _, err := openvpnUpgradePlan(rhel8, func(string) bool { return false }); err == nil {
		t.Error("dnf 和 yum 都没有时应该报错，而不是给一条跑不通的命令")
	}
}

func TestOpenVPNUpgradePlanRejectsUnknownDistro(t *testing.T) {
	_, family, err := openvpnUpgradePlan("ID=openwrt\nNAME=\"OpenWrt\"\n", allTools)
	if err == nil {
		t.Fatal("认不出发行版时必须报错")
	}
	if family != "" {
		t.Errorf("认不出来就不该有家族，得到 %s", family)
	}
	if !strings.Contains(err.Error(), "手动") {
		t.Errorf("错误里要告诉用户怎么办，得到 %v", err)
	}
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := tailLines(in, 2); got != "d\ne" {
		t.Errorf("只要末尾两行，得到 %q", got)
	}
	if got := tailLines("只有一行\n", 5); got != "只有一行" {
		t.Errorf("不足 n 行时原样返回，得到 %q", got)
	}
	if got := tailLines("", 3); got != "" {
		t.Errorf("空输出应该是空，得到 %q", got)
	}
}

func TestParseVersionParts(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"2.6.8", 2, 6, true},
		{"2.4.11", 2, 4, true},
		{"2.5.8-0ubuntu1", 2, 5, true}, // 发行版后缀要能容忍
		{"2.6", 2, 6, true},
		{"3.0.1", 3, 0, true},
		{"", 0, 0, false},
		{"2", 0, 0, false},
		{"v2.6.0", 0, 0, false}, // 版本号里没有 v，出现了就当读不出
		{"openvpn", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseVersionParts(c.in)
		if ok != c.ok || maj != c.major || min != c.minor {
			t.Errorf("parseVersionParts(%q) = %d.%d,%v，想 %d.%d,%v", c.in, maj, min, ok, c.major, c.minor, c.ok)
		}
	}
}

// 同一个意思在三个版本段里名字不一样，喂错名字 openvpn 是直接退出而不是警告。
func TestDataCipherArgs(t *testing.T) {
	cases := []struct {
		ver      string
		wantFlag string
		wantNil  bool
	}{
		{"2.6.8", "--data-ciphers", false},
		{"2.5.8", "--data-ciphers", false},
		{"3.1.0", "--data-ciphers", false},
		{"2.4.11", "--ncp-ciphers", false},
		{"2.3.20", "", true},
		{"", "", true},
		{"junk", "", true},
	}
	for _, c := range cases {
		got := dataCipherArgs(c.ver)
		if c.wantNil {
			if len(got) != 0 {
				t.Errorf("%s 不该给算法参数（默认表更安全），得到 %v", c.ver, got)
			}
			continue
		}
		if len(got) == 0 || got[0] != c.wantFlag {
			t.Errorf("%s 应该用 %s，得到 %v", c.ver, c.wantFlag, got)
		}
		if !strings.Contains(strings.Join(got, " "), "AES-256-CBC") {
			t.Errorf("%s 的算法表里要留 AES-256-CBC，Gate 上一大批老节点只讲它：%v", c.ver, got)
		}
	}
	// fallback 只在认得这个参数的版本上给，否则同样是"未识别选项"
	if s := strings.Join(dataCipherArgs("2.4.11"), " "); strings.Contains(s, "fallback") {
		t.Errorf("2.4 不认 --data-ciphers-fallback：%s", s)
	}
	if s := strings.Join(dataCipherArgs("2.6.8"), " "); !strings.Contains(s, "--data-ciphers-fallback") {
		t.Errorf("2.6 应该带 fallback，不然只讲 AES-256-CBC 的老节点连不上：%s", s)
	}
}

// 这几个解析函数是「检查更新」里 openvpn 那个版本号的唯一出处，而各家包管理器的
// 输出形状差得很远。夹具按真机输出抄，改坏了要在这里红，而不是在界面上
// 显示一个"2.6.11-1"被截断成"openvpn.x86_64"这种荒唐数字。
func TestParseAptCandidate(t *testing.T) {
	out := `openvpn:
  Installed: 2.5.8-2ubuntu2
  Candidate: 2.6.11-0ubuntu0.1
  Version table:
 *** 2.5.8-2ubuntu2 500
        100 /var/lib/dpkg/status
`
	if got := parseAptCandidate(out); got != "2.6.11-0ubuntu0.1" {
		t.Errorf("Candidate = %q, 想 2.6.11-0ubuntu0.1", got)
	}
	if got := parseAptCandidate("openvpn:\n  Installed: (none)\n  Candidate: (none)\n"); got != "" {
		t.Errorf("索引里没有这个包时应给空，得 %q", got)
	}
	if got := parseAptCandidate("N: Unable to locate package openvpn\n"); got != "" {
		t.Errorf("查无此包时应给空，得 %q", got)
	}
}

// `dnf list openvpn` 会把已装和仓库两段都印出来，仓库那段排在后面。取第一行等于
// 拿已装版本跟自己比——三台机器三个不同版本同时报"已是仓库最新"，就是这个取法造成的。
func TestParseDnfListRow(t *testing.T) {
	both := `Last metadata expiration check: 0:12:01 ago on Mon 23 Nov 2026 09:12:01 AM UTC.
Installed Packages
openvpn.x86_64          1:2.5.8-1.el8          @System
Available Packages
openvpn.x86_64          1:2.6.11-1.el8         appstream
`
	if got := parseDnfListRow(both); got != "2.6.11-1.el8" {
		t.Errorf("两段都该取仓库那段，得 %q, 想 2.6.11-1.el8", got)
	}
	// epoch（"1:"）不是版本的一部分，留着的话界面会显示成 1:2.5.8-1.el8
	installed := `Installed Packages
openvpn.x86_64          1:2.5.8-1.el8          @System
`
	if got := parseDnfListRow(installed); got != "2.5.8-1.el8" {
		t.Errorf("仓库里没有更新的时只有已装一段，得 %q, 想 2.5.8-1.el8", got)
	}
	// 老 yum 的 "name-version-release.arch" 写法，不带 epoch
	legacy := "openvpn-2.4.11-1.el7.x86_64    2.4.11-1.el7    base\n"
	if got := parseDnfListRow(legacy); got != "2.4.11-1.el7" {
		t.Errorf("不带 epoch 的行 = %q, 想 2.4.11-1.el7", got)
	}
	plugin := `Installed Packages
openvpn-auth-ldap.x86_64     2.0.4-1.el8     @System
openvpn.x86_64               1:2.5.8-1.el8   @System
`
	if got := parseDnfListRow(plugin); got != "2.5.8-1.el8" {
		t.Errorf("别把同前缀的插件当成 openvpn 本体，得 %q, 想 2.5.8-1.el8", got)
	}
	if got := parseDnfListRow("Error: No matching Packages to list\n"); got != "" {
		t.Errorf("查无此包时应给空，得 %q", got)
	}
}

func TestParseApkPolicy(t *testing.T) {
	// 首行是 apk 当前选中的那份（已装且仓库没更新时就是已装版本），缩进的才是各仓库候选
	out := `openvpn-2.6.11-r0:
  2.6.11-r0:
    V: https://dl-cdn.alpinelinux.org/alpine/v3.20/main
    A: x86_64
`
	if got := parseApkPolicy(out); got != "2.6.11-r0" {
		t.Errorf("apk 首行解析 = %q, 想 2.6.11-r0", got)
	}
	older := `openvpn-2.5.8-r0:
  2.6.11-r0:
    V: https://dl-cdn.alpinelinux.org/alpine/v3.21/main
    A: x86_64
  2.5.8-r0:
    V: https://dl-cdn.alpinelinux.org/alpine/v3.20/main
    A: x86_64
`
	if got := parseApkPolicy(older); got != "2.6.11-r0" {
		t.Errorf("候选里有更高版本时应取最高的，得 %q, 想 2.6.11-r0", got)
	}
	if got := parseApkPolicy("ERROR: unable to select packages:\n  openvpn (no such package):\n"); got != "" {
		t.Errorf("查无此包时应给空，得 %q", got)
	}
}

func TestParseColonVersion(t *testing.T) {
	pacman := `Repository      : extra
Name            : openvpn
Version         : 2.6.11-1
Description     : An easy to use, robust, and flexible SSL VPN daemon
`
	if got := parseColonVersion(pacman); got != "2.6.11-1" {
		t.Errorf("pacman 解析 = %q, 想 2.6.11-1", got)
	}
	zypper := `Information for package openvpn:
-------------------------------------
Repository     : openSUSE-Tumbleweed-Oss
Name           : openvpn
Version        : 2.6.11-1.1
Arch           : x86_64
`
	if got := parseColonVersion(zypper); got != "2.6.11-1.1" {
		t.Errorf("zypper 解析 = %q, 想 2.6.11-1.1", got)
	}
	// 只有 Installed Version 这种键时不该乱认成候选版本
	if got := parseColonVersion("Versioning    : rpm\n"); got != "" {
		t.Errorf("不该把别的键当成版本，得 %q", got)
	}
}

// 界面判断"有没有新版"用的是 versionLess，仓库版本号带发行版后缀也必须比得对：
// 比错了要么永远提示升级，要么永远说你是最新。
func TestOpenVPNRepoVersionCompare(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"2.5.8", "2.6.11-0ubuntu0.1", true},
		{"2.6.11", "2.6.11-0ubuntu0.1", false},
		{"2.6.11-2ubuntu1", "2.6.11-0ubuntu0.1", false},
		{"2.4.11", "2.4.12-1.el7", true},
		{"2.6.9-r0", "2.6.11-r0", true},
		{"", "2.6.11", true}, // 读不出版本时宁可说"有新版"，让用户去升一次看结果
	}
	for _, c := range cases {
		if got := versionLess(c.cur, c.latest); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, 想 %v", c.cur, c.latest, got, c.want)
		}
	}
}

// 官方 release 的 tag 形状各家不一样，而界面拿它跟本机版本并排显示，
// 报一个 rc 出去就是"让你去升一个预览版"。
func TestReleaseTagNormalizes(t *testing.T) {
	ok := map[string]string{
		"v26.11.9": "26.11.9", // Xray-core 的 tag 带 v，本机 xray version 不带，得归一
		"v2.7.9":   "2.7.9",
		"2.7.9":    "2.7.9",
		" v1.2.3 ": "1.2.3",
	}
	for in, want := range ok {
		got, good := releaseTag(in)
		if !good || got != want {
			t.Errorf("releaseTag(%q) = %q,%v, 想 %q,true", in, got, good, want)
		}
	}
	bad := []string{"", "v", "v2.7_rc6", "v2.7.9_rc1", "2.7.9-rc1", "latest", "v2..7", "vx.y.z"}
	for _, in := range bad {
		if got, good := releaseTag(in); good {
			t.Errorf("%q 不该被当成稳定版号，却给了 %q", in, got)
		}
	}
}

// 官方比包管理器能给到的新时那句解释：不说清就成了"明明有新版却说没有"，
// 说错了又成了"你能升"结果装出来还是旧号。
func TestOvpnUpstreamNote(t *testing.T) {
	cases := []struct {
		installed, repo, upstream, want string
	}{
		{"2.6.19", "2.6.19-0ubuntu0.24.04.5", "2.7.9",
			"包管理器最高只到 2.6.19-0ubuntu0.24.04.5，官方 2.7.9 得走 OpenVPN 自己的源（swupdate.openvpn.net）"},
		{"2.6.14", "2.6.14-0+deb12u2", "2.7.9",
			"包管理器最高只到 2.6.14-0+deb12u2，官方 2.7.9 得走 OpenVPN 自己的源（swupdate.openvpn.net）"},
		{"2.6.14", "2.7.9-1", "2.7.9", ""}, // 仓库已经追平官方，没话要说
		{"2.6.14", "", "2.7.9", ""},        // 仓库号问不到时不猜
	}
	for _, c := range cases {
		if got := ovpnUpstreamNote(c.installed, c.repo, c.upstream); got != c.want {
			t.Errorf("ovpnUpstreamNote(%q,%q,%q) = %q, 想 %q", c.installed, c.repo, c.upstream, got, c.want)
		}
	}
}

// 撞锁是刷索引最常见的失败（自动更新正占着 apt），它跟"镜像源连不上"是两回事：
// 前者等一会儿就有答案，后者等多久都没。判错了措辞就跟着错。
func TestLooksLocked(t *testing.T) {
	yes := []string{
		"exit status 100: E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4150393 (apt-get)",
		"E: Unable to lock directory /var/lib/apt/lists/",
		"E: dpkg is lock frontend (failed to get lock)",
		"waiting for process with pid 123 to finish",
	}
	for _, m := range yes {
		if !looksLocked(errors.New(m)) {
			t.Errorf("%q 该认成撞锁", m)
		}
	}
	no := []string{
		"exit status 100: E: Failed to fetch http://deb.debian.org/debian 连接超时",
		"exit status 1",
		"context deadline exceeded",
	}
	for _, m := range no {
		if looksLocked(errors.New(m)) {
			t.Errorf("%q 不该认成撞锁", m)
		}
	}
	if looksLocked(nil) {
		t.Error("没有错误时不该说撞锁")
	}
}

func TestRefreshWhy(t *testing.T) {
	lock := errors.New("exit status 100: E: Could not get lock /var/lib/apt/lists/lock. It is held by process 4150393 (apt-get)")
	got := refreshWhy("apt-get", lock)
	if !strings.Contains(got, "正被别的进程占用") || !strings.Contains(got, "可能偏旧") {
		t.Errorf("撞锁该说清是别人占着、号可能偏旧，得 %q", got)
	}
	if strings.Contains(got, "4150393") {
		t.Errorf("不该把 apt 的原始 pid 甩到界面上：%q", got)
	}
	slow := errors.New("exit status 124: tail: context deadline exceeded")
	if got := refreshWhy("apt-get", slow); !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("非锁失败要带上原始原因，得 %q", got)
	}
}
