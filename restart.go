package main

import (
	"log"
	"sync"
	"time"
)

// restartGate 把"配置已改完，让 Xray 重载一次"合并成一次。
//
// 三种后端的写配置路径最后都要重载 Xray 才生效：自建模式重启自己拉起的进程、
// 3x-ui 调 restartXrayService、xray-cf-lite 重启它的服务。一次批量开 N 条出口
// 就是 N 次写入 + N 次重载，而每次重载都会把当时已经连上的客户端全打断一遍。
// 实测 8 条出口的批量操作里，客户端要被踢下 8 次。
//
// 做法是只合并重载、不合并写入：写入照旧立即落盘/落库（崩了也不丢），
// 重载延后一个合并窗口，窗口内又来请求就重新计时。因为每次重载都是
// "从当前完整配置重新加载"，中间态没人需要看见，合并成最后一次即可。
type restartGate struct {
	mu       sync.Mutex
	timer    *time.Timer
	run      func() error
	owner    string
	attempts int
	exec     sync.Mutex // 串行化真正的重载，别叠着重启
}

// 重载失败后的重试：面板偶尔会在我写配置的瞬间正好也在重启，
// 这时 reload 会连接被拒。不重试的话配置就卡在"已写未生效"，
// 而且没人会再触发一次——用户看到的是"改了没反应"。
const (
	retryMax     = 4
	retryBaseWin = 2 * time.Second
	retryCap     = 30 * time.Second
)

var gate = &restartGate{}

// deferRestart 登记一次待执行的重载。run 必须是幂等的"按当前状态重载"。
// 合并窗口关掉（PanelDebounce<=0）时退成立即执行。
func deferRestart(what string, run func() error) error {
	window := time.Duration(connConf().PanelDebounce) * time.Millisecond
	gate.mu.Lock()
	gate.run = run
	gate.owner = what
	if window <= 0 {
		gate.mu.Unlock()
		return flushRestart(what)
	}
	if gate.timer == nil {
		gate.timer = time.AfterFunc(window, func() { _ = flushRestart(what) })
	} else {
		gate.timer.Reset(window)
	}
	gate.mu.Unlock()
	return nil
}

// takeRestart 取出待办的重载并撤销定时器。
func takeRestart() func() error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return takeRestartLocked()
}

func takeRestartLocked() func() error {
	run := gate.run
	gate.run = nil
	if gate.timer != nil {
		gate.timer.Stop()
		gate.timer = nil
	}
	return run
}

// cancelRestart 丢掉属于 what 的待办重载。
//
// 切换后端时自建模式要停掉自己的 Xray，此时还在排队的定时器若随后触发，
// 会把一个新 Xray 拉起来和面板的 Xray 抢端口——进程看着正常，客户端全连不上。
func cancelRestart(what string) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.owner != what {
		return
	}
	takeRestartLocked()
	gate.attempts = 0
}

// flushRestart 立刻执行待办的重载，失败则按退避重试。
//
// 排上重试之后返回 nil：调用方是批量操作里的某一步，
// 让它因为一次可恢复的重载失败而中断整批没有意义，
// 真实结果由后面的日志和界面轮询反映。
func flushRestart(what string) error {
	run := takeRestart()
	if run == nil {
		return nil
	}
	err := callRestart(run)
	if err == nil {
		return nil
	}

	gate.mu.Lock()
	gate.attempts++
	n := gate.attempts
	if n <= retryMax {
		delay := retryBaseWin * time.Duration(1<<uint(n-1))
		if delay > retryCap {
			delay = retryCap
		}
		gate.run = run
		gate.owner = what
		gate.timer = time.AfterFunc(delay, func() { _ = flushRestart(what) })
	}
	gate.mu.Unlock()

	if n <= retryMax {
		log.Printf("%s 重载失败（第 %d 次，稍后自动重试）: %v", what, n, err)
		return nil
	}
	log.Printf("%s 重载连续失败 %d 次，放弃自动重试：配置已写入但可能尚未生效: %v", what, retryMax, err)
	gate.mu.Lock()
	gate.attempts = 0
	gate.mu.Unlock()
	return err
}

// flushRestartNow 同样立刻执行待办的重载，但不安排重试、把错误原样返回。
// 新建入站要用它判断 Xray 是否真的起得来，起不来得当场撤销这条入站。
func flushRestartNow(what string) error {
	run := takeRestart()
	if run == nil {
		return nil
	}
	err := callRestart(run)
	if err == nil {
		gate.mu.Lock()
		gate.attempts = 0
		gate.mu.Unlock()
	}
	return err
}

func callRestart(run func() error) error {
	gate.exec.Lock()
	defer gate.exec.Unlock()
	return run()
}

// pendingRestart 报告是否有待执行的重载，用于退出前收尾。
func pendingRestart() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.run != nil
}
