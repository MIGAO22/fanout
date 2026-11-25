package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// 出口连不上时，界面和作业里那句必须能指向下一步。这里钉的是"从 openvpn
// 日志里挑哪一句"——挑错或挑不到，用户看到的就永远是"等待 tun0 就绪超时"，
// 而那句话对排查没有任何信息量。
func TestOvpnErrorLinePicksTheTellingLine(t *testing.T) {
	cases := []struct {
		name, log, want string
	}{
		{"认证被拒", "Tue AUTH_FAILED: [none]\nInitialization Sequence Completed\n", "节点拒绝了用户名/密码"},
		// 两种印法都要认：openvpn 在不同版本里把这条打成 write UDPv4: … 或 sendto …: …
		{"MTU 发大了 write 形", "write UDPv4: Message too long [nonblocking:1]\n", "外层包超过链路 MTU"},
		{"MTU 发大了 sendto 形", "sendto 1.2.3.4:1194 failed: Message too long\n", "外层包超过链路 MTU"},
		{"MTU 和超时同段", "write UDP: message too long\nConnection timed out: retrying with next remote\n", "外层包超过链路 MTU"},
		{"参数不认识", "Options error: Unrecognized option or missing parameter(s) [--route-nopull]\n", "参数没被接受"},
		{"证书过期", "VERIFY ERROR: depth=1: certificate has expired\n", "节点证书过期"},
		{"只有校验不过", "VERIFY ERROR: depth=0, error=self signed certificate\n", "节点证书校验不过"},
		{"域名解析不出", "RESOLVE: Cannot resolve host address: node.example:443\n", "节点域名解析不出来"},
		{"端口没应答", "Connection timed out (errno=110)\n", "节点的端口没应答"},
		{"节点已应答", "Peer Connection Initiated with [AF_INET]1.2.3.4:1194\n", "节点已应答，卡在后面的步骤"},
		// 2.7 起 openvpn 会把对端的 TLS alert 记成 "… SSL alert: …"，
		// 这比"握手没走完"具体：是节点主动拒了，不是包没到。
		{"TLS alert", "TLS Read TLS SSL alert: fatal: handshake failure\n", "节点在 TLS 层直接拒绝（见原文）"},
		// AEAD-only 那一档的两条反噬：算法谈不拢、节点坚持要压缩。
		// 两句都以 OPTIONS ERROR 开头，不能被通用的"参数没被接受"抢先。
		{"算法谈不拢", "OPTIONS ERROR: failed to negotiate cipher with server.  Add the server's cipher ('DES-EDE3-CBC') to --data-ciphers\n", "数据通道算法和节点谈不拢（开着 AEAD-only 就先关掉再试）"},
		{"没有 fallback 就不连", "ERROR: failed to negotiate cipher with peer and --data-ciphers-fallback not enabled. No usable data channel cipher\n", "数据通道算法和节点谈不拢（开着 AEAD-only 就先关掉再试）"},
		{"节点硬要压缩", "OPTIONS ERROR: server pushed compression settings that are not allowed and will result in a non-working connection.\n", "节点要求开压缩，AEAD-only 不让开"},
		{"没有关键句", "Control Channel: TLSv1.3, cipher TLSv1.3\n", ""},
		{"空日志", "", ""},
	}
	for _, c := range cases {
		got := ovpnErrorLine(c.log)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: 不该挑出东西，得到 %q", c.name, got)
			}
			continue
		}
		if !strings.HasPrefix(got, c.want+"：") {
			t.Errorf("%s: 得到 %q，想要以 %q 开头", c.name, got, c.want)
		}
	}
}

// 日志里同一类错出现多次时，最后一句才是这一轮的结论：openvpn 会按顺序
// 试好几个 remote，前面那些是过程。
func TestOvpnErrorLineTakesLastMatch(t *testing.T) {
	log := "Connection timed out with 1.1.1.1:1194\n" +
		"AUTH_FAILED: [none] with 2.2.2.2:443\n"
	got := ovpnErrorLine(log)
	if !strings.HasPrefix(got, "节点拒绝了用户名/密码") {
		t.Errorf("该取最后匹配的那句，得到 %q", got)
	}
	if strings.Contains(got, "1.1.1.1") {
		t.Errorf("不该把上一个候选带进来：%q", got)
	}
}

// 同一行里"Message too long"和"Connection timed out"会一起出现，
// 而只有前者说明是我们把包发大了——优先级顺序不能被字母序或位置打乱。
func TestOvpnErrorLinePrefersActionableCause(t *testing.T) {
	line := "write UDP, message too long, Connection timed out: retrying\n"
	if got := ovpnErrorLine(line); !strings.HasPrefix(got, "外层包超过链路 MTU") {
		t.Errorf("得到 %q，想要先说 MTU", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\n\tb   c", 100); got != "a b c" {
		t.Errorf("换行/制表没压成一行：%q", got)
	}
	long := strings.Repeat("节", 400)
	got := oneLine(long, 300)
	if len([]rune(got)) != 301 || !strings.HasSuffix(got, "…") {
		t.Errorf("截断没生效，长度 %d", len([]rune(got)))
	}
	if oneLine("", 10) != "" {
		t.Error("空串该还是空串")
	}
}

// openvpn 进程退出在隧道就绪之后也必须被看见。
//
// 原来只有 waitTunUp 在建隧道那段时间等 done，就绪之后没人收，于是进程秒退
// （被 OOM 杀掉、节点端口不该答、证书过期）在界面上仍然是一句"绿着"，
// 判死要等满 HealthFailures × HealthInterval（默认 30 秒）。
// 现在退出即记原因，健康检查拿它当确定判据。
func TestWatchOvpnExitRecordsWhy(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/fo1.log"
	blob := "Fri Nov 25 22:40:01 2026 AUTH_FAILED,OpenVPN server: \n" +
		"Fri Nov 25 22:40:01 2026 SIGTERM[hard,] received, process exiting\n"
	if err := os.WriteFile(logPath, []byte(blob), 0600); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}

	p := newOvpnProc(nil, make(chan error, 1))
	if _, dead := p.deadReason(); dead {
		t.Fatal("刚建好的句柄不该已经是死的")
	}
	p.done <- errors.New("exit status 1")
	watchOvpnExit(p, logPath) // 直接调，别靠 goroutine 的时序
	why, dead := p.deadReason()
	if !dead {
		t.Fatal("进程退了必须记成已死，否则健康检查还得等三轮防抖")
	}
	for _, want := range []string{"openvpn 进程已退出", "exit status 1", "节点拒绝了用户名/密码"} {
		if !strings.Contains(why, want) {
			t.Errorf("原因里缺 %q：%s", want, why)
		}
	}
	if len([]byte(why)) > 600 {
		t.Errorf("那句要能进界面，太长了：%s", why)
	}

	// 重复收口只认第一次：换节点时旧句柄可能被 kill 和 watcher 同时碰到
	p.markDead("第二次")
	if again, _ := p.deadReason(); again != why {
		t.Errorf("原因被覆盖了：%s -> %s", why, again)
	}
}

// fanout 自己收掉进程（换节点、停出口、重连）不能算成"节点掉了"：
// 调用方已经改过状态，这里再记一次会把一次正常换节点显示成一次掉线。
func TestWatchOvpnExitStaysQuietWhenWeKillIt(t *testing.T) {
	p := newOvpnProc(nil, make(chan error, 1))
	p.stopOnce.Do(func() { close(p.stopped) })
	watchOvpnExit(p, "/nonexistent/fo1.log")
	if _, dead := p.deadReason(); dead {
		t.Error("主动收手不该被记成进程意外退出")
	}
}

// kill() 仍然要幂等且不报错：重连路径上会对同一个句柄调用两次
// （reconnect 里一次、换节点时 old.kill() 又一次）。
func TestOvpnProcKillIsIdempotent(t *testing.T) {
	p := newOvpnProc(nil, make(chan error, 1))
	p.kill()
	p.kill()
	select {
	case <-p.stopped:
	default:
		t.Error("kill 之后 stopped 应已关闭，等退出的 goroutine 才能收工")
	}
	var nilProc *ovpnProc
	nilProc.kill() // 不能 panic：setProc(nil) 之后老代码路径还会调一次
	if _, dead := nilProc.deadReason(); dead {
		t.Error("空句柄不该报死")
	}
}
