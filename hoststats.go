package main

// 顶部状态条的主机实时读数：CPU、内存、交换、磁盘、负载、TCP/UDP 在用数。
//
// 一律读 /proc 而不是问 free/ss/vmstat：这个接口每几秒就要被拉一次，
// 起子进程既慢又得走一遍 inMainNetns 的 exec 护栏（见 netnsguard.go）。
//
// /proc/net/* 是网络命名空间相关的，所以读它之前先切回母机：
// 处理请求的 goroutine 有可能正跑在一条被 dialerInNetns 带进隧道的线程上，
// 那时读到的是那条隧道自己的连接表，同一个数字会在几个数量级之间乱跳。

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostStats 是顶部胶囊要显示的全部数值。
//
// 每组都同时给"比例"和"原始量"：胶囊画比例，title 里写原始量。
// 只给比例的话，64G 内存的机器和 1G 的机器显示成同一个绿条，看不出区别。
type hostStats struct {
	// CPU 是两次采样之间的平均占用（0-100），不是瞬时值——
	// 瞬时值要靠一段窗口算，而这个接口的窗口就是轮询间隔。
	CPU float64 `json:"cpu"`

	MemPct   float64 `json:"mem_pct"`
	MemUsed  int64   `json:"mem_used"`  // 字节
	MemTotal int64   `json:"mem_total"` // 字节

	SwapPct   float64 `json:"swap_pct"`
	SwapUsed  int64   `json:"swap_used"`
	SwapTotal int64   `json:"swap_total"`

	DiskPct   float64 `json:"disk_pct"`
	DiskUsed  int64   `json:"disk_used"`
	DiskTotal int64   `json:"disk_total"`
	// DiskMount 是这几个数真正来自哪个挂载点：磁盘占用必须标明是哪一个分区，
	// 不然 / 很空、/var 很满的机器会让人看不出红色是从哪来的。
	DiskMount string `json:"disk_mount"`

	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	Cores  int     `json:"cores"`
	// LoadPct 把 load1 按核数归一：4 核上 load 4 是 100%（刚好排满），
	// 直接拿 load1 当百分比的话，多核机器会画成爆表。
	LoadPct float64 `json:"load_pct"`

	TCP    int     `json:"tcp"` // 在用 TCP socket，含 TIME_WAIT，IPv4 + IPv6 合计
	TCPPct float64 `json:"tcp_pct"`
	UDP    int     `json:"udp"`
	UDPPct float64 `json:"udp_pct"`
	// TCP6 / UDP6 是其中来自 IPv6 那张表的部分。分开回给界面是因为：
	// 只看到一个合计的话，"UDP 怎么是 0" 和 "IPv4 真的一个都没有" 分不清。
	TCP6 int `json:"tcp6"`
	UDP6 int `json:"udp6"`
	// SockCap 是这两个百分比共同的分母：本机临时端口区间的宽度。
	// 它只是个刻度，不是硬上限（监听套接字并不占这段），
	// 画出来是为了让"TCP 用了多少"这台代理机上有个可比的参照。
	SockCap int `json:"sock_cap"`
}

// cpuFirstWindow 是首屏没有前值可减时，就地等的一个短窗口。
const cpuFirstWindow = 200 * time.Millisecond

// cpuMinWindow 是可信读数的最小间隔：连着点两次刷新时，
// 间隔只有几毫秒的差值基本是噪声，宁可多等一会儿。
const cpuMinWindow = 500 * time.Millisecond

type cpuTimes struct{ idle, total uint64 }

var cpuSample = struct {
	mu   sync.Mutex
	prev cpuTimes
	at   time.Time
	has  bool
}{}

// parseCPUTimes 解析 /proc/stat 里那行汇总的 "cpu "。
//
// idle 要把 iowait 一起算进去：等 IO 的时候 CPU 是闲的，
// 只算第 4 列的话一台疯狂读盘的机器会显示成满载。
func parseCPUTimes(blob []byte) (cpuTimes, error) {
	for _, line := range strings.Split(string(blob), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var out cpuTimes
		for i, s := range f[1:] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return out, fmt.Errorf("第 %d 列 %q 不是数字", i+1, s)
			}
			// guest / guest_nice 是 user / nice 里已经算过的部分，不能重复加
			if i < 8 {
				out.total += v
			}
			if i == 3 || i == 4 {
				out.idle += v
			}
		}
		return out, nil
	}
	return cpuTimes{}, fmt.Errorf("没找到汇总的 cpu 行")
}

// cpuPct 由前后两次采样算占用百分比。窗口内没走动就返回 0。
func cpuPct(prev, cur cpuTimes) float64 {
	dt := int64(cur.total) - int64(prev.total)
	if dt <= 0 {
		return 0
	}
	di := int64(cur.idle) - int64(prev.idle)
	return clampPct(100 * float64(dt-di) / float64(dt))
}

func readCPUTimes() (cpuTimes, error) {
	blob, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, fmt.Errorf("读 /proc/stat 失败: %w", err)
	}
	return parseCPUTimes(blob)
}

// readCPUPct 给出距上次调用的平均占用。
//
// 状态记在包级变量里：这个值是差分出来的，每次请求独立读一遍 /proc/stat
// 只能拿到开机以来的累计占用，看不出"现在"忙不忙。
func readCPUPct() (float64, error) {
	cpuSample.mu.Lock()
	defer cpuSample.mu.Unlock()

	if !cpuSample.has {
		p, err := readCPUTimes()
		if err != nil {
			return 0, err
		}
		cpuSample.prev, cpuSample.at, cpuSample.has = p, time.Now(), true
		time.Sleep(cpuFirstWindow)
	} else if w := cpuMinWindow - time.Since(cpuSample.at); w > 0 {
		time.Sleep(w)
	}

	cur, err := readCPUTimes()
	if err != nil {
		return 0, err
	}
	pct := cpuPct(cpuSample.prev, cur)
	cpuSample.prev, cpuSample.at = cur, time.Now()
	return pct, nil
}

// parseMeminfo 取内存与交换的总量/可用量（kB）。
//
// MemAvailable 比 MemFree 准：cache/buffer 是可以让出来的，
// 拿 MemFree 算会把一台正常的机器画成内存快满了。老内核没这一项，退回 MemFree。
func parseMeminfo(blob []byte) (memTotal, memAvail, swapTotal, swapFree int64) {
	var memFree int64
	seen := false
	for _, line := range strings.Split(string(blob), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			memTotal = v
		case "MemFree":
			memFree = v
		case "MemAvailable":
			memAvail, seen = v, true
		case "SwapTotal":
			swapTotal = v
		case "SwapFree":
			swapFree = v
		}
	}
	if !seen {
		memAvail = memFree
	}
	return memTotal, memAvail, swapTotal, swapFree
}

// parseLoadavg 取 1/5/15 分钟负载。
func parseLoadavg(blob []byte) (l1, l5, l15 float64, err error) {
	f := strings.Fields(string(blob))
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("负载行字段太少: %q", strings.TrimSpace(string(blob)))
	}
	if l1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return
	}
	if l5, err = strconv.ParseFloat(f[1], 64); err != nil {
		return
	}
	l15, err = strconv.ParseFloat(f[2], 64)
	return
}

// parseSockstat 取 TCP 与 UDP 的在用套接字数。入参是 /proc/net/sockstat
// 或 /proc/net/sockstat6 都行：两张表的行名前缀一样、只是 IPv6 那份带个 6，
// 而内核把它们分开记（绑 [::] 的双栈监听只出现在 IPv6 表里），
// 所以调用方要把两份加起来看。
//
// TCP 把 tw 一起算上：TIME_WAIT 同样占着端口和内核对象，
// 一台跑代理的机器上它经常比已建立的连接还多，只看在用数会低估。
// IPv6 表里内核没印 tw，那份就只是在用数。
//
// 内核把这行印成 "TCP: in use 37 orphan 0 tw 128"，数值在关键字后面隔一个词的位置，
// 所以取数时要往后找到第一个整数，而不是照着关键字取下一格。
func parseSockstat(blob []byte) (tcp, udp int) {
	for _, line := range strings.Split(string(blob), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		val := func(keys ...string) int {
			for i, s := range f {
				if s != keys[0] {
					continue
				}
				// keys 是多词关键字（"in use"）时，后面那几个词也要对上
				matched := true
				for k, want := range keys[1:] {
					if i+1+k >= len(f) || f[i+1+k] != want {
						matched = false
						break
					}
				}
				if !matched {
					continue
				}
				for j := i + len(keys); j < len(f); j++ {
					if v, err := strconv.Atoi(f[j]); err == nil {
						return v
					}
				}
			}
			return 0
		}
		switch f[0] {
		case "TCP:", "TCP6:":
			tcp = val("in", "use") + val("tw")
		case "UDP:", "UDP6:":
			udp = val("in", "use")
		}
	}
	return tcp, udp
}

// parsePortRange 取临时端口区间的宽度，作为 TCP/UDP 胶囊的刻度。
func parsePortRange(blob []byte) int {
	f := strings.Fields(string(blob))
	if len(f) < 2 {
		return 0
	}
	lo, err1 := strconv.Atoi(f[0])
	hi, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || hi < lo {
		return 0
	}
	return hi - lo + 1
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func pctOf(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return clampPct(100 * float64(used) / float64(total))
}

// diskStat 把 statfs 的块数换算成已用/总量/百分比，口径跟 df 一致：
// 已用 = (blocks - bfree) × frsize，而百分比分母是"已用 + 普通用户还拿得到的"
// （bavail），把 root 预留的那截剔掉。
//
// 预留不剔的话，一块格式化时留了 5% 的盘会在实际用到 95% 时才亮，
// 而那时普通用户早就写不进去东西了——胶囊要的是"还能不能写"。
func diskStat(blocks, bfree, bavail, frsize int64) (used, total int64, pct float64) {
	if blocks <= 0 || frsize <= 0 {
		return 0, 0, 0
	}
	// statfs 的这几个计数正常情况是递减的，真读反了就当没有，
	// 不能让负数流到胶囊上画出一条倒着走的绿条
	if bfree > blocks {
		bfree = blocks
	}
	if bavail > blocks {
		bavail = blocks
	}
	used = (blocks - bfree) * frsize
	total = blocks * frsize
	if room := used + bavail*frsize; room > 0 {
		pct = clampPct(100 * float64(used) / float64(room))
	}
	return used, total, pct
}

// readDiskUsage 采一次磁盘占用，按给定顺序试挂载点，取第一个 stat 得动的。
//
// 先看 fanout 自己的工作目录（state.json、openvpn 配置、日志都写在那儿），
// 目录还没建出来或跨了挂载点就退回根分区。挂载点名会一并回给界面，
// 因为"存储 90%"不写是哪个分区的话，没法照着处理。
func readDiskUsage(paths ...string) (used, total int64, pct float64, mount string, err error) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		blocks, bfree, bavail, frsize, serr := statfsBlocks(p)
		if serr != nil {
			err = serr
			continue
		}
		u, t, pc := diskStat(blocks, bfree, bavail, frsize)
		if t <= 0 {
			// 伪文件系统（tmpfs 之外的 proc/sysfs 之类）会给出 0 块，跳过
			continue
		}
		return u, t, pc, p, nil
	}
	if err == nil {
		err = fmt.Errorf("没有可统计的挂载点")
	}
	return 0, 0, 0, "", err
}

// statsDataDir 是磁盘胶囊优先统计的挂载点所在目录，由 main 在解析完 -dir 之后填。
// 只在启动时写一次、之后只读，所以不需要锁；留空就退化成只统计根分区。
var statsDataDir string

// readHostStats 采一次本机负载。
//
// CPU 之外任一项读失败都不致命：能读到几项就显示几项，
// 只有全部读不出来（比如 /proc 没挂）才向上报错。
func readHostStats() (hostStats, error) {
	var st hostStats
	var errs []string

	if pct, err := readCPUPct(); err != nil {
		errs = append(errs, err.Error())
	} else {
		st.CPU = pct
	}

	if blob, err := os.ReadFile("/proc/meminfo"); err != nil {
		errs = append(errs, err.Error())
	} else {
		total, avail, sTotal, sFree := parseMeminfo(blob)
		st.MemTotal, st.MemUsed = total*1024, (total-avail)*1024
		st.MemPct = pctOf(total-avail, total)
		st.SwapTotal, st.SwapUsed = sTotal*1024, (sTotal-sFree)*1024
		st.SwapPct = pctOf(sTotal-sFree, sTotal)
	}

	// 磁盘读的是 fanout 自己写数据的那个分区，读不到才退回根分区
	if used, total, pc, mnt, derr := readDiskUsage(statsDataDir, "/"); derr != nil {
		errs = append(errs, derr.Error())
	} else {
		st.DiskUsed, st.DiskTotal, st.DiskPct, st.DiskMount = used, total, pc, mnt
	}

	st.Cores = runtime.NumCPU()
	if blob, err := os.ReadFile("/proc/loadavg"); err != nil {
		errs = append(errs, err.Error())
	} else if l1, l5, l15, err := parseLoadavg(blob); err != nil {
		errs = append(errs, err.Error())
	} else {
		st.Load1, st.Load5, st.Load15 = l1, l5, l15
		st.LoadPct = clampPct(100 * l1 / float64(max(st.Cores, 1)))
	}

	// 这一段必须在母机命名空间里读，理由见文件头
	if err := inMainNetns(func() error {
		blob, err := os.ReadFile("/proc/net/sockstat")
		if err != nil {
			return fmt.Errorf("读 /proc/net/sockstat 失败: %w", err)
		}
		st.TCP, st.UDP = parseSockstat(blob)
		// IPv4 和 IPv6 是两张独立的表：/proc/net/sockstat 里只有 AF_INET。
		// 入站一般绑 [::]（双栈），客户端从 IPv6 连进来时那些 socket 全记在
		// sockstat6 上——只读 IPv4 那份，UDP 胶囊就会一直停在 0。
		// 没编进 IPv6 的内核没有这个文件，读不到不报错。
		if v6, err := os.ReadFile("/proc/net/sockstat6"); err == nil {
			st.TCP6, st.UDP6 = parseSockstat(v6)
			st.TCP += st.TCP6
			st.UDP += st.UDP6
		}
		if rb, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
			st.SockCap = parsePortRange(rb)
		}
		st.TCPPct, st.UDPPct = pctOf(int64(st.TCP), int64(st.SockCap)), pctOf(int64(st.UDP), int64(st.SockCap))
		return nil
	}); err != nil {
		errs = append(errs, err.Error())
	}

	if st.CPU == 0 && st.MemTotal == 0 && st.Load1 == 0 && st.TCP == 0 {
		return st, fmt.Errorf("读不到本机负载：%s", strings.Join(errs, "；"))
	}
	return st, nil
}

// apiHostStats 给顶部胶囊供数。
//
// 只对登录后的界面开放（挂在 auth.Wrap 之内），且不做缓存：
// 每次请求就是四次小文件读，加一次差分，比缓存逻辑更便宜。
func apiHostStats(w http.ResponseWriter, r *http.Request) {
	st, err := readHostStats()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
