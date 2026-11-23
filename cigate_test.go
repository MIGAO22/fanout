package main

import (
	"os"
	"strings"
	"testing"
)

// 钉住 test.yml 的退出码判据。
//
// 起因是这个门曾经是假的：`go test ./... 2>&1 | tee x.log` 在 bash -e 下只认流水线里
// 最后一条命令，而那一条是 tee，永远成功。netns 那组用例在非 root runner 上真失败
// （EPERM），连着几次发布 CI 都报绿，连「失败时把日志贴进 summary」那一步都没触发过
// ——也就是说这套单测实际上从没拦下过任何东西，而交叉编译又抓不到任何运行期缺陷。
//
// 判据写成测试而不是靠人 review workflow：手滑把 pipefail 删掉之后的症状是"CI 又绿了"，
// 比红着更难发现。
func TestWorkflowTestStepsPropagateFailure(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/test.yml")
	if err != nil {
		t.Skipf("读不到 test.yml（不在仓库根跑的），跳过: %v", err)
	}
	seen, gated := 0, 0
	name, pipefail, tolerates := "(作业头)", false, false
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "      - "):
			// 新步骤开始；注释块里也写着 go test，所以逐行扫时必须跟着步骤换状态
			name, pipefail, tolerates = strings.TrimPrefix(s, "- "), false, false
		case s == "set -o pipefail":
			pipefail = true
		case strings.HasPrefix(s, "continue-on-error:"):
			tolerates = strings.Contains(s, "true")
		case strings.HasPrefix(strings.TrimPrefix(s, "run: "), "go test"):
			seen++
			if strings.Contains(s, "| tee") && !pipefail {
				t.Errorf("%s：用 tee 存日志却没开 pipefail，go test 报 FAIL 作业照样绿", name)
			}
			if tolerates {
				t.Errorf("%s：continue-on-error 让它不构成门槛", name)
				continue
			}
			gated++
		}
	}
	if seen != 2 {
		t.Fatalf("期望两条 go test（普通 + -race），实际 %d 条", seen)
	}
	if gated != seen {
		t.Errorf("%d 条 go test 应当全是硬门槛，现在只有 %d 条拦得住", seen, gated)
	}
}
