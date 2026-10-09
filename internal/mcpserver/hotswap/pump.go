package hotswap

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Pump 是退位后的首代桥:在宿主侧句柄与泵侧管道之间双向搬运字节。
// 它不解析任何内容(纯字节层),是规格实施决策第 3 条 F4/F5 的泵体。
//
// 两个方向各一个搬运循环:
//
//	宿主侧读 → 泵侧写:喂继任标准输入;
//	泵侧读   → 宿主侧写:转继任标准输出。
//
// 终态与返回(收场语义,规格 F4):
//   - 继任死亡:泵侧读 EOF,或往泵侧写失败 → 返回(继任侧异常时带错误);
//   - 宿主侧写失败 → 返回错误;
//   - 宿主侧读端关闭 → 关闭泵侧写端,继任读标准输入 EOF 自行退出
//     (不用进程句柄杀),等泵侧读自然收干后返回 nil(干净收场)。
//
// Pump 接管继任侧两端(toSuccessor/fromSuccessor)的关闭责任,返回时
// 两者均已关闭;宿主侧句柄不关——泵返回即进程应当退出,句柄随进程回收。
// 返回后仍可能有一个搬运循环阻塞在对端句柄上,靠数据面 EOF 自行退出,
// 不影响调用方。
func Pump(hostIn io.Reader, hostOut io.Writer, toSuccessor io.WriteCloser, fromSuccessor io.ReadCloser) error {
	st := &pumpState{done: make(chan struct{})}

	// 方向一:宿主侧读 → 泵侧写(喂继任标准输入)。
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := hostIn.Read(buf)
			if n > 0 {
				if _, werr := toSuccessor.Write(buf[:n]); werr != nil {
					st.terminal(fmt.Errorf("hotswap pump: write to successor: %w", werr))
					return
				}
			}
			if rerr != nil {
				// 宿主侧读端关闭:关泵侧写端,继任读标准输入 EOF 自行退出。
				// 非终态——由方向二等继任收干后统一收场。
				toSuccessor.Close()
				return
			}
		}
	}()

	// 方向二:泵侧读(继任标准输出)→ 宿主侧写。
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := fromSuccessor.Read(buf)
			if n > 0 {
				if _, werr := hostOut.Write(buf[:n]); werr != nil {
					st.terminal(fmt.Errorf("hotswap pump: write to host: %w", werr))
					return
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					st.terminal(fmt.Errorf("hotswap pump: read from successor: %w", rerr))
					return
				}
				// 继任标准输出已关:继任退出——正常收场。
				st.terminal(nil)
				return
			}
		}
	}()

	<-st.done
	// 释放泵侧句柄(重复关闭无害):让仍在飞的另一方向尽快感知关闭。
	toSuccessor.Close()
	fromSuccessor.Close()
	return st.err()
}

// pumpState 协调两个搬运循环:第一个到达终态者触发收场并记下首个错误。
type pumpState struct {
	stopOnce sync.Once
	done     chan struct{}
	mu       sync.Mutex
	firstErr error
}

func (p *pumpState) terminal(err error) {
	if err != nil {
		p.mu.Lock()
		if p.firstErr == nil {
			p.firstErr = err
		}
		p.mu.Unlock()
	}
	p.stopOnce.Do(func() { close(p.done) })
}

func (p *pumpState) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr
}
