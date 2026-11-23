package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// statfsBlocks 取一个挂载点的块计数，交给 diskStat 换算。
//
// 单独放一个文件是因为 golang.org/x/sys/unix 在 Windows 上整个包都不可用，
// 而 diskStat 那套 df 口径的算术是要能在任何地方跑测试的：
// 磁盘胶囊算错是最容易被当成"显示问题"放过去的错。
//
// 用 statfs 而不是问 df：df 要解析 /proc/mounts、挑掉伪文件系统、
// 还得跟它的输出格式较劲，这里只关心一个挂载点。
func statfsBlocks(path string) (blocks, bfree, bavail, frsize int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("statfs %s 失败: %w", path, err)
	}
	// 字段宽度按平台而定（32 位上是 int32），一律先抬到 int64 再算
	return int64(st.Blocks), int64(st.Bfree), int64(st.Bavail), int64(st.Frsize), nil
}
