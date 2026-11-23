package main

import (
	"archive/zip"
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
