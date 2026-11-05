package mcpserver

import (
	"fmt"
	"sort"
	"sync"

	"ssh-manager-mcp/internal/secretmask"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// backgroundMaskingSource 按 store 实例与机密项值集合变化计数缓存遮蔽器。
// 后台任务不能使用工具调用的 context；它的输出协程在这里按「当前 store」取
// 值集合。缓存命中时只读一个原子计数，不查询数据库，也不解密机密项。
type backgroundMaskingSource struct {
	storeFn func() *store.Store

	mu       sync.Mutex
	store    *store.Store
	version  uint64
	masker   secretmask.Masker
	hasCache bool
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasCache && s.store == st && s.version == version {
		return s.masker, nil
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
	s.store, s.version, s.masker, s.hasCache = st, version, masker, true
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
	results := w.maskInput(masker, p)
	w.writeResults(results)
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
	results := w.flushWithMasker(masker)
	w.writeResults(results)
	w.after()
}

func (w *notifyWriter) maskInput(masker secretmask.Masker, input []byte) []secretmask.MaskResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if masker.LongestMatchLength() == 0 {
		if w.stream == nil {
			return []secretmask.MaskResult{{Output: input}}
		}
		result := w.stream.SwitchMasker(masker)
		w.stream = nil
		w.recordNames(result.MaskedNames)
		return []secretmask.MaskResult{result}
	}
	if w.stream == nil {
		streamValue := masker.NewStream(w.serverID)
		w.stream = &streamValue
	}
	switchResult := w.stream.SwitchMasker(masker)
	writeResult := w.stream.Write(input)
	w.recordNames(switchResult.MaskedNames)
	w.recordNames(writeResult.MaskedNames)
	return []secretmask.MaskResult{switchResult, writeResult}
}

func (w *notifyWriter) flushWithMasker(masker secretmask.Masker) []secretmask.MaskResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stream == nil {
		return nil
	}
	switchResult := w.stream.SwitchMasker(masker)
	flushResult := w.stream.Flush()
	w.stream = nil
	w.recordNames(switchResult.MaskedNames)
	w.recordNames(flushResult.MaskedNames)
	return []secretmask.MaskResult{switchResult, flushResult}
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

func (w *notifyWriter) writeResults(results []secretmask.MaskResult) {
	for _, result := range results {
		if len(result.Output) == 0 {
			continue
		}
		_, _ = w.buf.Write(result.Output)
	}
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
