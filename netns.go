package main

import (
	"net"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// socksDialTimeout 是给隧道内一次拨号的硬超时。
//
// 原来用无超时的 net.Dial：隧道刚掉但 netns 还没拆时，目标地址会被
// 内核一直挂着 SYN，拨号要等到 TCP 自己的重试上限（两分钟以上）。
// 这段时间这个客户端连接既不报错也不动，界面看起来就是"卡住"。
// 超时同样覆盖 DNS 查询——拨号的 deadline 会传解析器，
// 所以隧道内解析不了主机名也不会无限挂着。
const socksDialTimeout = 15 * time.Second

// dialerInNetns 返回一个在指定 netns 内建立出站连接的 dial 函数。
// 每次拨号都要切一次 netns，因为 socket 的归属在创建时确定。
func dialerInNetns(nsName string) func(network, addr string) (net.Conn, error) {
	return dialerInNetnsTimeout(nsName, socksDialTimeout)
}

// dialerInNetnsTimeout 同 dialerInNetns，但能指定单次拨号的预算。
//
// 出口组要用更短的预算：一条成员卡住时得尽快降级到下一条，
// 否则光第一条就等满 15 秒，"保活"成了空话。
func dialerInNetnsTimeout(nsName string, budget time.Duration) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)

		go func() {
			runtime.LockOSThread()

			origin, err := os.Open("/proc/self/ns/net")
			if err != nil {
				ch <- result{nil, err}
				return
			}
			defer origin.Close()

			target, err := os.Open("/var/run/netns/" + nsName)
			if err != nil {
				ch <- result{nil, err}
				return
			}
			defer target.Close()

			if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
				ch <- result{nil, err}
				return
			}

			// 隧道内只有 IPv4 路由。不限定的话会选中 AAAA 记录，
			// 那条连接绕开隧道从母机的 IPv6 出去，暴露真实地址。
			conn, dialErr := net.DialTimeout(forceIPv4Network(network), addr, budget)

			if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
				if conn != nil {
					conn.Close()
				}
				ch <- result{nil, err}
				return
			}

			runtime.UnlockOSThread()
			ch <- result{conn, dialErr}
		}()

		r := <-ch
		return r.conn, r.err
	}
}

// forceIPv4Network 把 tcp/udp 收敛成 tcp4/udp4，已经指定版本的原样返回。
func forceIPv4Network(network string) string {
	switch network {
	case "tcp":
		return "tcp4"
	case "udp":
		return "udp4"
	}
	return network
}
