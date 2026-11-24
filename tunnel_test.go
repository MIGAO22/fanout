package main

import (
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
