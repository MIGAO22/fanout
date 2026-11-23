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
