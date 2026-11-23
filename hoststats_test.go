package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 这几个解析函数是顶部负载条的全部数据来源。它们只在真机上被间接触发，
// 一旦解析错位，界面上会一直显示一个"看起来挺正常"的错数字——
// 所以拿真实 /proc 文本钉住，而不是靠肉眼在机器前面对。

func TestParseCPUTimesAndPct(t *testing.T) {
	prev, err := parseCPUTimes([]byte("cpu  100 10 50 800 40 0 10 0 5 5\nintr 1\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// total 只加前 8 列：guest 那两列是 user/nice 里已经算过的，重复加会低估占用
	if want := uint64(100 + 10 + 50 + 800 + 40 + 0 + 10 + 0); prev.total != want {
		t.Errorf("total = %d, 想 %d", prev.total, want)
	}
	// idle 要含 iowait：疯狂读盘的机器不该显示成满载
	if want := uint64(840); prev.idle != want {
		t.Errorf("idle = %d, 想 %d（idle + iowait）", prev.idle, want)
	}

	cur, err := parseCPUTimes([]byte("cpu  200 10 100 880 60 0 10 0 5 5\n"))
	if err != nil {
		t.Fatalf("解析第二次失败: %v", err)
	}
	// 增量：total 1010→1260（走 250），idle 840→940（闲 100）→ 占用 60%
	if got := cpuPct(prev, cur); math.Abs(got-60) > 0.01 {
		t.Errorf("cpuPct = %v, 想 60", got)
	}
}

func TestParseCPUTimesRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "intr 1\ntxt 2\n", "cpu 1 2 3\n", "cpu a b c d e\n"} {
		if _, err := parseCPUTimes([]byte(bad)); err == nil {
			t.Errorf("这段没报错: %q", bad)
		}
	}
}

func TestCPUPctClampsAndZeroWindow(t *testing.T) {
	if got := cpuPct(cpuTimes{idle: 10, total: 10}, cpuTimes{idle: 10, total: 10}); got != 0 {
		t.Errorf("窗口内没走动应给 0, got %v", got)
	}
	// 计数器回绕或被别的采样抢先更新过时差值会是负的，不能画成 -30%
	if got := cpuPct(cpuTimes{idle: 500, total: 900}, cpuTimes{idle: 100, total: 400}); got != 0 {
		t.Errorf("负窗口应给 0, got %v", got)
	}
	if got := cpuPct(cpuTimes{idle: 0, total: 10}, cpuTimes{idle: 0, total: 20}); got != 100 {
		t.Errorf("全忙应是 100, got %v", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	blob := []byte(`MemTotal:       16384000 kB
MemFree:          204800 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
SwapTotal:       4096000 kB
SwapFree:        3072000 kB
`)
	total, avail, sTotal, sFree := parseMeminfo(blob)
	if total != 16384000 || avail != 8192000 {
		t.Fatalf("MemTotal/Available = %d/%d", total, avail)
	}
	if sTotal != 4096000 || sFree != 3072000 {
		t.Fatalf("SwapTotal/Free = %d/%d", sTotal, sFree)
	}
	// 用 MemAvailable 而不是 MemFree：否则 12.8G 的占用会画成 98%
	if got := pctOf(total-avail, total); math.Abs(got-50) > 0.01 {
		t.Errorf("mem pct = %v, 想 50", got)
	}
}

// 老内核（<3.14）没有 MemAvailable，退回 MemFree 而不是报 0。
func TestParseMeminfoWithoutAvailable(t *testing.T) {
	blob := []byte("MemTotal: 1000 kB\nMemFree: 250 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	total, avail, sTotal, _ := parseMeminfo(blob)
	if total != 1000 || avail != 250 {
		t.Fatalf("total/avail = %d/%d, 想 1000/250", total, avail)
	}
	if sTotal != 0 {
		t.Fatalf("SwapTotal = %d, 想 0", sTotal)
	}
}

func TestParseSockstat(t *testing.T) {
	// 这份是 Ubuntu 22.04 /proc/net/sockstat 的原样抄录：在用数是 "inuse" 一个 token。
	// 早先这里的夹具写成了 "in use" 两个 token，解析器照着它写，结果真机上一律读不到，
	// UDP 胶囊永远停在 0，而 TCP 因为还能命中 tw 看着像好的。
	blob := []byte(`sockets: used 512
TCP: inuse 20 orphan 0 tw 128 alloc 150 mem 34
UDP: inuse 9 memory 4
UDPLITE: inuse 0
RAW: inuse 0
FRAG: inuse 0 memory 0
`)
	tcp, udp := parseSockstat(blob)
	// tw 要算进来：代理机上 TIME_WAIT 常常比已建立连接还多
	if tcp != 148 {
		t.Errorf("tcp = %d, 想 148（20 在用 + 128 tw）", tcp)
	}
	if udp != 9 {
		t.Errorf("udp = %d, 想 9", udp)
	}
}

func TestParseSockstatLegacySpelling(t *testing.T) {
	// 老内核印 "in use" 两个 token，这种写法也得继续认，别把修好的 bug 换成新的
	tcp, udp := parseSockstat([]byte("TCP: in use 37 orphaned 0 tw 128\nUDP: in use 9\n"))
	if tcp != 165 || udp != 9 {
		t.Errorf("tcp/udp = %d/%d, 想 165/9", tcp, udp)
	}
}

func TestParseSockstatPartial(t *testing.T) {
	tcp, udp := parseSockstat([]byte("TCP: inuse 4\n"))
	if tcp != 4 || udp != 0 {
		t.Errorf("tcp/udp = %d/%d, 想 4/0（缺 tw 与 UDP 行不该炸）", tcp, udp)
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg([]byte("0.52 0.41 0.30 3/214 8891\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if l1 != 0.52 || l5 != 0.41 || l15 != 0.30 {
		t.Fatalf("load = %v/%v/%v", l1, l5, l15)
	}
	if _, _, _, err := parseLoadavg([]byte("0.52 0.41\n")); err == nil {
		t.Error("字段太少应报错")
	}
}

func TestParsePortRange(t *testing.T) {
	if got := parsePortRange([]byte("32768\t60999\n")); got != 28232 {
		t.Errorf("宽度 = %d, 想 28232", got)
	}
	for _, bad := range []string{"", "32768", "b a\n", "60999 32768\n"} {
		if got := parsePortRange([]byte(bad)); got != 0 {
			t.Errorf("这段应给 0（前端据此显示未知刻度）: %q → %d", bad, got)
		}
	}
}

// 分母为 0 时不能给 NaN：界面上一个 NaN% 的胶囊比不显示还难查。
func TestPctOfGuardsZeroDenominator(t *testing.T) {
	if got := pctOf(5, 0); got != 0 {
		t.Errorf("pctOf(5,0) = %v, 想 0", got)
	}
	if got := pctOf(3, 4); math.Abs(got-75) > 0.001 {
		t.Errorf("pctOf(3,4) = %v, 想 75", got)
	}
	if got := clampPct(140); got != 100 {
		t.Errorf("clampPct(140) = %v, 想 100", got)
	}
}

// 解析函数各自测过之后，还得真的走一遍 /proc：路径写错、少读一个文件、
// 字段顺序变了，这些在纯解析用例里都看不出来，但会让顶栏胶囊一直显示 0。
//
// 这里不断言具体数值（机器一直在动），钉的是接口形状：状态码 200、
// 字段名跟前端读的那几个对得上、百分比不越界。
func TestHostStatsReadsProc(t *testing.T) {
	// 这条要真的 /proc，只在 Linux（也就是跑构建和部署的那边）验证
	if runtime.GOOS != "linux" {
		t.Skip("只在 Linux 上验证 /proc 读法")
	}
	rec := httptest.NewRecorder()
	apiHostStats(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 想 200（/proc 读不出来？）: %s", rec.Code, rec.Body.String())
	}

	var got hostStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("返回不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	for _, f := range []struct {
		name string
		pct  float64
	}{
		{"cpu", got.CPU}, {"mem_pct", got.MemPct}, {"swap_pct", got.SwapPct},
		{"load_pct", got.LoadPct}, {"tcp_pct", got.TCPPct}, {"udp_pct", got.UDPPct},
	} {
		if math.IsNaN(f.pct) || f.pct < 0 || f.pct > 100 {
			t.Errorf("%s = %v, 应在 0..100", f.name, f.pct)
		}
	}
	// 内存和核数在任意 Linux 上都该读得到；给出 0 就是取错了键
	if got.MemTotal <= 0 {
		t.Errorf("mem_total = %d, /proc/meminfo 的 MemTotal 不该是 0", got.MemTotal)
	}
	if got.Cores <= 0 {
		t.Errorf("cores = %d, 应是 CPU 核心数（负载条拿它当分母）", got.Cores)
	}
	if got.MemUsed > got.MemTotal {
		t.Errorf("mem_used %d 超过了 mem_total %d", got.MemUsed, got.MemTotal)
	}
}

// 磁盘胶囊的口径必须和 SSH 里 df -h 的 Use% 一致，否则用户按页面上的数字
// 判断"还能不能写"会判错。这两件事要钉住：扣掉 root 预留块、以及读坏了不炸。
func TestDiskStatMatchesDfSemantics(t *testing.T) {
	const frsize = int64(4096)
	// df -h 会看到的一块 100G 盘：已用 60G，空闲 40G，其中 5G 是 root 预留，
	// 所以普通用户实际只剩 35G 可写
	gib := func(n int64) int64 { return n * 1024 * 1024 * 1024 / frsize }
	used, total, pct := diskStat(gib(100), gib(40), gib(35), frsize)
	if total != 100*1024*1024*1024 {
		t.Errorf("total = %d, 想 %d（100G）", total, 100*1024*1024*1024)
	}
	if used != 60*1024*1024*1024 {
		t.Errorf("used = %d, 想 %d（已用按 blocks-bfree，不是 blocks-bavail）",
			used, 60*1024*1024*1024)
	}
	// df 报的是 64%：60/(60+35)，而不是 60/100 = 60%
	if want := 100 * 60.0 / 95.0; pct < want-0.01 || pct > want+0.01 {
		t.Errorf("pct = %v, 想 %v（分母要剔掉 root 预留）", pct, want)
	}
}

func TestDiskStatGuards(t *testing.T) {
	// 伪文件系统、statfs 返回全零，都不能让胶囊画出个倒着走的比例
	if used, total, pct := diskStat(0, 0, 0, 4096); used != 0 || total != 0 || pct != 0 {
		t.Errorf("空盘应全 0, got %d/%d/%v", used, total, pct)
	}
	if _, total, pct := diskStat(100, 40, 40, 0); total != 0 || pct != 0 {
		t.Errorf("frsize 为 0 时不该算出东西, total=%d pct=%v", total, pct)
	}
	// 计数读反了（bfree > blocks）时夹住，别给个负的已用
	if used, _, pct := diskStat(100, 120, 100, 1024); used != 0 || pct != 0 {
		t.Errorf("bfree 超过 blocks 应夹成 0, got used=%d pct=%v", used, pct)
	}
	// 满盘（bavail=0）就是 100%，不该因为分母退化成 used 而变 0
	if _, _, pct := diskStat(100, 0, 0, 4096); pct != 100 {
		t.Errorf("写满时应是 100, got %v", pct)
	}
}

// readDiskUsage 的挂载点挑选：坏路径跳过、全坏才报错。
// statfs 本身要真内核，这里只在 Linux 上跑。
func TestReadDiskUsageFallsBack(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("statfs 需要 Linux")
	}
	used, total, pct, mount, err := readDiskUsage("/definitely-not-a-mount-point", "/")
	if err != nil {
		t.Fatalf("退回根分区该成功: %v", err)
	}
	if mount != "/" {
		t.Errorf("mount = %q, 想 /（第一个路径 stat 不动就该换下一个）", mount)
	}
	if total <= 0 || used < 0 || pct <= 0 || pct > 100 {
		t.Errorf("根分区读数不合理: used=%d total=%d pct=%v", used, total, pct)
	}
	if _, _, _, _, err := readDiskUsage(""); err == nil {
		t.Error("一个挂载点都不给时应报错，别让胶囊显示 0/0")
	}
}

// 现场反馈：UDP 胶囊一直是 0。这里两张表都要覆盖到——IPv4 表里确实可能一个
// UDP socket 都没有（绑 [::] 的双栈监听只记在 sockstat6），而且两张表的
// 在用数都印成 "inuse" 一个 token，照着 "in use" 写解析就两边都读不出来。
func TestParseSockstatIPv6Table(t *testing.T) {
	// 这份是 /proc/net/sockstat6 的真实形状：行名带 6，而且没有 tw 列
	blob := []byte(`TCP6: inuse 8
UDP6: inuse 6
UDPLITE6: inuse 0
RAW6: inuse 0
FRAG6: inuse 0 memory 0
`)
	tcp, udp := parseSockstat(blob)
	if tcp != 8 {
		t.Errorf("tcp = %d, 想 8（TCP6 行）", tcp)
	}
	if udp != 6 {
		t.Errorf("udp = %d, 想 6（UDP6 行，IPv4 表里没有的那份）", udp)
	}
}

// 两张表要相加才是本机总数：少加任何一张，代理机上都会明显低估。
func TestParseSockstatBothTablesSum(t *testing.T) {
	v4 := []byte("TCP: inuse 37  orphan 0  tw 128  mem 5\nUDP: inuse 2 mem 2\n")
	v6 := []byte("TCP6: inuse 8\nUDP6: inuse 6\n")
	t4, u4 := parseSockstat(v4)
	t6, u6 := parseSockstat(v6)
	if t4+t6 != 173 {
		t.Errorf("TCP 相加 = %d, 想 173（37 在用 + 128 tw + 8 IPv6）", t4+t6)
	}
	if u4+u6 != 8 {
		t.Errorf("UDP 相加 = %d, 想 8（IPv4 的 2 加 IPv6 的 6）", u4+u6)
	}
}

// 上下行速率的原料是 /proc/net/dev 的累计字节，夹具按真机原样抄：
// 收在第 1 列，发在第 9 列，名字在冒号左边且带缩进。
func TestParseNetDev(t *testing.T) {
	blob := []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1048576    9012    0    0    0     0          0         0  1048576    9012    0    0    0     0       0          0
  eth0: 524288000  400000    0    0    0     0          0         0 262144000  300000    0    0    0     0       0          0
`)
	got := parseNetDev(blob)
	if len(got) != 2 {
		t.Fatalf("读到 %d 块网卡，想 2：%v", len(got), got)
	}
	if e := got["eth0"]; e[0] != 524288000 || e[1] != 262144000 {
		t.Errorf("eth0 收发 = %v, 想 [524288000 262144000]", e)
	}
	if e := got["lo"]; e[0] != 1048576 {
		t.Errorf("lo 收 = %v，名字要去掉缩进", e)
	}
}

// 只数默认路由那块卡：把 docker0/veth 一起加起来会把同一段流量算两遍。
func TestParseDefaultIfaces(t *testing.T) {
	blob := []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
		"wg0\t00000000\tFEA9B60A\t0003\t0\t0\t50\t00000000\t0\t0\t0\n")
	got := parseDefaultIfaces(blob)
	if strings.Join(got, ",") != "eth0,wg0" {
		t.Errorf("默认路由网卡 = %v, 想 [eth0 wg0]（表头与非默认那行都不能混进来）", got)
	}
	if len(parseDefaultIfaces([]byte(""))) != 0 {
		t.Error("空输入应给空列表，让调用方走兜底")
	}
}

func TestFallbackIfacesSkipsVirtual(t *testing.T) {
	devs := map[string][2]uint64{
		"lo": {}, "eth0": {}, "docker0": {}, "veth1a2b3c": {}, "br-9f8e7d": {}, "tun0": {},
	}
	got := strings.Join(fallbackIfaces(devs), ",")
	if got != "eth0,tun0" {
		t.Errorf("兜底网卡 = %q, 想 eth0,tun0", got)
	}
}

func TestRatePct(t *testing.T) {
	// 100 MB/s 跑在 1 Gbps 的口上 = 800 Mbps = 80%
	if p := ratePct(100*1024*1024, 1000); p < 79 || p > 85 {
		t.Errorf("pct = %v, 想 80 上下", p)
	}
	if p := ratePct(12345, 0); p != 0 {
		t.Errorf("网卡不报速率时应给 0（界面据此不画条），得 %v", p)
	}
}

func TestNetRates(t *testing.T) {
	restore := func() {
		netSample.mu.Lock()
		netSample.devs, netSample.has, netSample.ready = nil, false, false
		netSample.up, netSample.down = 0, 0
		netSample.mu.Unlock()
	}
	defer restore()
	restore() // 起始状态也要清干净：这组测试依赖"第一次调用没有前值"

	// 第一次没有前值可减：ready=false，界面显示"—"，不能报 0 B/s
	if up, down, ready := netRates(map[string][2]uint64{"eth0": {100, 40}}, []string{"eth0"}); ready || up != 0 || down != 0 {
		t.Fatalf("首个窗口 = %v/%v ready=%v, 想 0/0 false", up, down, ready)
	}

	// 造一个 1 秒前的采样点：收 0→500，发 0→250
	seed := func(devs map[string][2]uint64, ago time.Duration, ready bool) {
		netSample.mu.Lock()
		netSample.devs = devs
		netSample.at, netSample.has, netSample.ready = time.Now().Add(-ago), true, ready
		netSample.mu.Unlock()
	}
	seed(map[string][2]uint64{"eth0": {0, 0}}, time.Second, true)

	up, down, ready := netRates(map[string][2]uint64{"eth0": {500, 250}}, []string{"eth0"})
	if !ready {
		t.Fatal("有前值、窗口够长时应给得出速率")
	}
	if up < 200 || up > 300 {
		t.Errorf("上行（发送）= %v, 想 250 上下", up)
	}
	if down < 450 || down > 550 {
		t.Errorf("下行（接收）= %v, 想 500 上下", down)
	}

	// 新出现的网卡这一轮不算，否则它的累计总量会被当成一个窗口的增量，画出一根冲顶的假条
	seed(map[string][2]uint64{"eth0": {0, 0}}, time.Second, true)
	up, down, _ = netRates(map[string][2]uint64{"eth0": {100, 100}, "wg0": {9e9, 9e9}}, []string{"eth0", "wg0"})
	if up > 101 || down > 101 {
		t.Errorf("新网卡被算进来了：up/down = %v/%v", up, down)
	}

	// 计数器归零（网卡重置）时不能算出负数
	seed(map[string][2]uint64{"eth0": {1e6, 1e6}}, time.Second, true)
	up, down, _ = netRates(map[string][2]uint64{"eth0": {10, 10}}, []string{"eth0"})
	if up < 0 || down < 0 {
		t.Errorf("计数归零应跳过而不是给负数：%v/%v", up, down)
	}
}

// 连着点两次刷新时窗口只有几毫秒，差出来的速率是噪声，要沿用上一次的。
func TestNetRatesShortWindowKeepsPrevious(t *testing.T) {
	defer func() {
		netSample.mu.Lock()
		netSample.devs, netSample.has, netSample.ready = nil, false, false
		netSample.up, netSample.down = 0, 0
		netSample.mu.Unlock()
	}()
	netSample.mu.Lock()
	netSample.devs = map[string][2]uint64{"eth0": {0, 0}}
	netSample.at, netSample.has, netSample.ready = time.Now(), true, true
	netSample.up, netSample.down = 111, 222
	netSample.mu.Unlock()

	up, down, ready := netRates(map[string][2]uint64{"eth0": {1 << 30, 1 << 30}}, []string{"eth0"})
	if up != 111 || down != 222 || !ready {
		t.Errorf("窗口太短时应沿用上一次：%v/%v ready=%v, 想 111/222 true", up, down, ready)
	}
}

// 胶囊的刻度要有出处：链路速率读不到就别画条。
func TestLinkMbpsUnreadable(t *testing.T) {
	if got := linkMbps([]string{"iface-that-does-not-exist-xyz"}); got != 0 {
		t.Errorf("不存在的网卡应给 0（没有分母），得 %d", got)
	}
}
