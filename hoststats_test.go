package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
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
	blob := []byte(`sockets: used 512
TCP: in use 37 orphaned 0 tw 128
UDP: in use 9
UDPLITE: in use 0
RAW: in use 1
`)
	tcp, udp := parseSockstat(blob)
	// tw 要算进来：代理机上 TIME_WAIT 常常比已建立连接还多
	if tcp != 165 {
		t.Errorf("tcp = %d, 想 165（37 在用 + 128 tw）", tcp)
	}
	if udp != 9 {
		t.Errorf("udp = %d, 想 9", udp)
	}
}

func TestParseSockstatPartial(t *testing.T) {
	tcp, udp := parseSockstat([]byte("TCP: in use 4\n"))
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
