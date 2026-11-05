package mcpserver

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"ssh-manager-mcp/internal/secretmask"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// backgroundMaskingSource 按 store 实例与机密项值集合变化计数缓存遮蔽器。
// 后台任务不能使用工具调用的 context；它的输出协程在这里按「当前 store」取
// 值集合。缓存命中时只读一个原子计数，不查询数据库，也不解密机密项。
type backgroundMaskingSource struct {
	storeFn func() *store.Store
	cache   atomic.Pointer[backgroundMaskerCache]
	mu      sync.Mutex
}

type backgroundMaskerCache struct {
	store   *store.Store
	version uint64
	masker  secretmask.Masker
}

func newBackgroundMaskingSource(storeFn func() *store.Store) *backgroundMaskingSource {
	return &backgroundMaskingSource{storeFn: storeFn}
}

func (s *backgroundMaskingSource) currentMasker() (secretmask.Masker, error) {
	st := s.storeFn()
	if st == nil {
		return secretmask.Masker{}, fmt.Errorf("current store is unavailable")
	}
	version := st.SecretValuesForMaskingVersion()
	if cached := s.cache.Load(); cached != nil && cached.store == st && cached.version == version {
		return cached.masker, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if cached := s.cache.Load(); cached != nil && cached.store == st && cached.version == version {
		return cached.masker, nil
	}
	rows, err := st.SecretValuesForMasking()
	if err != nil {
		return secretmask.Masker{}, fmt.Errorf("read secret values for background masking: %w", err)
	}
	var secrets []secretmask.Secret
	if len(rows) > 0 {
		secrets = make([]secretmask.Secret, 0, len(rows))
		for _, row := range rows {
			secrets = append(secrets, secretmask.Secret{
				Name:              row.Name,
				Current:           row.Current,
				Previous:          row.Previous,
				WritableServerIDs: row.WritableServerIDs,
			})
		}
	}
	masker := secretmask.New(secrets)
	s.cache.Store(&backgroundMaskerCache{store: st, version: version, masker: masker})
	return masker, nil
}

// notifyWriter 在远端字节进入环形缓冲前做流式遮蔽。maskedNames 记录整个
// 任务生命周期内这个通道实际遮蔽到的名字；即使对应字节后来被环形窗口丢弃，
// 后续读取仍会在 masked 里看到名字。这是有意选择「任务累计」而不是「本次
// 读取区间」，因为它不需要把名字与每个字节区间的生命周期绑定在一起。
type notifyWriter struct {
	buf   *sshbroker.RollingBuffer
	after func()

	source              *backgroundMaskingSource
	serverID            string
	mu                  sync.Mutex
	stream              *secretmask.Stream
	names               []string
	readFailureReported bool
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	if w.source == nil {
		return w.writeDirect(p)
	}
	masker, err := w.source.currentMasker()
	if err != nil {
		w.reportMaskingUnavailable()
		return len(p), nil
	}
	w.writeMasked(masker, p)
	w.after()
	return len(p), nil
}

// Flush 在任务结束路径调用，必须在任务状态改为终态之前完成，保证 agent 看
// 到「已结束」时暂留尾部已经进入缓冲。
func (w *notifyWriter) Flush() {
	if w.source == nil {
		return
	}
	masker, err := w.source.currentMasker()
	if err != nil {
		w.reportMaskingUnavailable()
		return
	}
	w.flushMasked(masker)
	w.after()
}

func (w *notifyWriter) writeMasked(masker secretmask.Masker, input []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if masker.LongestMatchLength() == 0 {
		if w.stream == nil {
			_, _ = w.buf.Write(input)
			return
		}
		result := w.stream.SwitchMasker(masker)
		w.stream = nil
		w.recordNames(result.MaskedNames)
		w.writeResult(result)
		_, _ = w.buf.Write(input)
		return
	}
	if w.stream == nil {
		streamValue := masker.NewStream(w.serverID)
		w.stream = &streamValue
	}
	switchResult := w.stream.SwitchMasker(masker)
	writeResult := w.stream.Write(input)
	w.recordNames(switchResult.MaskedNames)
	w.recordNames(writeResult.MaskedNames)
	w.writeResult(switchResult)
	w.writeResult(writeResult)
}

func (w *notifyWriter) flushMasked(masker secretmask.Masker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stream == nil {
		return
	}
	switchResult := w.stream.SwitchMasker(masker)
	flushResult := w.stream.Flush()
	w.stream = nil
	w.recordNames(switchResult.MaskedNames)
	w.recordNames(flushResult.MaskedNames)
	w.writeResult(switchResult)
	w.writeResult(flushResult)
}

func (w *notifyWriter) maskedNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.names) == 0 {
		return nil
	}
	return append([]string(nil), w.names...)
}

func (w *notifyWriter) recordNames(names []string) {
	if len(names) == 0 {
		return
	}
	w.names = append(w.names, names...)
	sort.Strings(w.names)
	w.names = deduplicateBackgroundMaskedNames(w.names)
}

func (w *notifyWriter) writeDirect(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.after()
	return n, err
}

func (w *notifyWriter) writeResult(result secretmask.MaskResult) {
	if len(result.Output) == 0 {
		return
	}
	_, _ = w.buf.Write(result.Output)
}

func (w *notifyWriter) reportMaskingUnavailable() {
	w.mu.Lock()
	alreadyReported := w.readFailureReported
	w.readFailureReported = true
	w.mu.Unlock()
	if alreadyReported {
		return
	}
	const marker = "sshmgr: output dropped because secret values could not be read\n"
	_, _ = w.buf.Write([]byte(marker))
	w.after()
}

func deduplicateBackgroundMaskedNames(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func mergeBackgroundMaskedNames(groups ...[]string) []string {
	var names []string
	for _, group := range groups {
		names = append(names, group...)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return deduplicateBackgroundMaskedNames(names)
}
