package main

import (
	"strconv"
	"strings"
)

// 按 openvpn 版本挑选项名。单独一个文件是为了让这两个纯函数能在本机真跑测试：
// tunnel.go 那半边要建 netns、拉进程，Windows 上根本执行不了。

// dataCipherArgs 按本机 openvpn 的版本给出数据通道算法参数。
//
// 同一件事在三个版本段里名字不一样：2.4 叫 --ncp-ciphers，2.5 起改名
// --data-ciphers，2.3 及更早两个都没有（那会儿数据通道跟着配置里的 --cipher）。
// 名字喂错不是警告而是当场退出：2.4 看到 --data-ciphers 会报
// "Unrecognized option" 直接失败，于是 Ubuntu 20.04、Debian 10 这些还在
// 2.4 的机器上一条出口都连不上，界面上只显示"连接失败"，看不出原因。
//
// 2.5 起默认多给一条 fallback：VPN Gate 上一大批老节点关了 NCP、只讲 AES-256-CBC，
// 新版客户端没有 fallback 会因为算法谈不拢而拒绝连接。
//
// aeadOnly 是"只要 AEAD"那一档（设置里的开关）：CBC 从表里去掉、fallback 也不给。
// 2.7 的二进制里写死了原因——`--data-ciphers-fallback` 一出现就禁用内核
// data channel offload，想拿到内核加解密就只能放弃那批老节点。所以这一档
// 只在认得 --data-ciphers 的版本上生效，2.4 及读不出版本时一律维持现状。
func dataCipherArgs(ver string, aeadOnly bool) []string {
	major, minor, ok := parseVersionParts(ver)
	if !ok {
		// 读不出版本就不赌，让 openvpn 用自己默认的算法表
		return nil
	}
	switch {
	case major > 2 || (major == 2 && minor >= 5):
		if aeadOnly {
			return []string{"--data-ciphers", aeadCipherList}
		}
		list := "AES-128-CBC:AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC"
		return []string{"--data-ciphers", list, "--data-ciphers-fallback", "AES-256-CBC"}
	case major == 2 && minor == 4:
		return []string{"--ncp-ciphers", "AES-128-CBC:AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC"}
	default:
		return nil
	}
}

// aeadCipherList 是 AEAD-only 那一档给的表：一个 CBC 都不留。
const aeadCipherList = "AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305"

// compressionOffArgs 是 AEAD-only 那一档顺手要补的另一条：openvpn 明写
// "--allow-compression 不是 no 就不启用 offload"。只在 2.5+ 给（更早的版本
// 没有这个选项，喂上去是 Unrecognized option 当场退出）。
func compressionOffArgs(ver string) []string {
	major, minor, ok := parseVersionParts(ver)
	if !ok || major < 2 || (major == 2 && minor < 5) {
		return nil
	}
	return []string{"--allow-compression", "no"}
}

// ovpnConfigOffersAEAD 看节点那份 .ovpn 自己声明的数据通道算法里有没有 AEAD。
//
// 这一条决定了 AEAD-only 那个开关到底该不该对这条出口生效。原因在真机上量到的：
// VPN Gate 的配置是 SoftEther/PacketiX 自动生成的，里面直接写着
// `data-ciphers AES-128-CBC`（老一点的还配 `cipher AES-128-CBC`）——
// 服务端只会这一种。这时候客户端硬上 AEAD-only 不是"少连几个节点"，
// 是一个节点都连不上（openvpn 报 failed to negotiate cipher）。
//
// 所以判断保守：配置里没写算法、或者写来写去只有 CBC，都算"不支持"，
// 那条出口继续走带 fallback 的原有参数。开关只在节点自己证明支持 AEAD 时才改行为。
func ovpnConfigOffersAEAD(cfg string) bool {
	seen := ""
	for _, line := range strings.Split(cfg, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(strings.TrimPrefix(f[0], "--")) {
		case "data-ciphers", "ncp-ciphers", "cipher":
			// 取最后一条：配置里出现多次时后面的是生效的那条
			seen = strings.ToLower(f[1])
		}
	}
	if seen == "" {
		return false
	}
	for _, c := range strings.FieldsFunc(seen, func(r rune) bool {
		return r == ':' || r == ',' || r == ' '
	}) {
		if strings.Contains(c, "-gcm") || strings.Contains(c, "chacha20") {
			return true
		}
	}
	return false
}

// ovpnConfigEnablesCompression 看节点那份 .ovpn 自己有没有开数据通道压缩。
//
// 开着就不能再补 --allow-compression no：openvpn 把这当成配置冲突直接退出
// （"'--allow-compression no' conflicts with enabling compression"），
// 不是降级处理——那条出口就一个节点都连不上了。宁可少要一点性能，
// 也不能因为一个开关把所有隧道点不着。
func ovpnConfigEnablesCompression(cfg string) bool {
	for _, line := range strings.Split(cfg, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch strings.ToLower(strings.TrimPrefix(f[0], "--")) {
		case "comp-lzo", "comp-lz4", "comp-noadapt", "compress", "allow-compression":
			return true
		}
	}
	return false
}

// parseVersionParts 把 "2.6.8" 拆成主/次版本。多出来的段（-ubuntu1、.1）忽略。
func parseVersionParts(v string) (major, minor int, ok bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	// 尾段可能挂着发行版后缀，只取开头的数字
	digit := func(s string) (int, bool) {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, false
		}
		n, err := strconv.Atoi(s[:i])
		return n, err == nil
	}
	if major, ok = digit(parts[0]); !ok {
		return 0, 0, false
	}
	if minor, ok = digit(parts[1]); !ok {
		return 0, 0, false
	}
	return major, minor, true
}
