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
// 2.5 起多给一条 fallback：VPN Gate 上一大批老节点关了 NCP、只讲 AES-256-CBC，
// 新版客户端没有 fallback 会因为算法谈不拢而拒绝连接。
func dataCipherArgs(ver string) []string {
	major, minor, ok := parseVersionParts(ver)
	if !ok {
		// 读不出版本就不赌，让 openvpn 用自己默认的算法表
		return nil
	}
	list := "AES-128-CBC:AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305:AES-256-CBC"
	switch {
	case major > 2 || (major == 2 && minor >= 5):
		return []string{"--data-ciphers", list, "--data-ciphers-fallback", "AES-256-CBC"}
	case major == 2 && minor == 4:
		return []string{"--ncp-ciphers", list}
	default:
		return nil
	}
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
