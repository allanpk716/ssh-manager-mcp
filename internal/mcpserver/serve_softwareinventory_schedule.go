package mcpserver

import (
	"context"
	"fmt"
	"os"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// startSoftwareCollectionScheduler 启动权威端 serve 的软件清单采集调度服
// 务：启动即执行一轮，补上停机期间错过的，之后周期检查到期设备。返回
// 的函数等待调度服务随上下文退出。测试替换这个变量验证启动与收尾接线。
var startSoftwareCollectionScheduler = func(ctx context.Context, st *store.Store) func() {
	scheduler := softwareinventory.NewScheduler(st)
	scheduler.Logf = func(format string, arguments ...any) {
		fmt.Fprintf(os.Stderr, "sshmgr serve: 软件清单采集调度："+format+"\n", arguments...)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = scheduler.Run(ctx)
	}()
	return func() { <-done }
}
