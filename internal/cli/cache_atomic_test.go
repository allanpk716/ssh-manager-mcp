package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// errTorn is returned when a read produces content that is neither complete payload
type errTorn struct{ len int }

func (e errTorn) Error() string { return "content is neither payload (torn)" }

// TestAtomicWriteUnique_ConcurrentWritersNeverTear: 8 个并发写者交替写两个等长
// payload，一个读者不断采样。固定 ".tmp" 名的实现会让两个写者在同一临时文件上
// O_TRUNC 交错，rename 落下撕裂内容（xcheck 2026-08-14 三家共识 bug）；
// 唯一临时名下读者任何时候看到的都是完整的 A 或完整的 B。
func TestAtomicWriteUnique_ConcurrentWritersNeverTear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.bin")
	a := bytes.Repeat([]byte("A"), 4096)
	b := bytes.Repeat([]byte("B"), 4096)
	if err := os.WriteFile(path, a, 0o600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil {
				continue // 与 rename 竞争的瞬时 ENOENT，重读即可
			}
			if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
				readErr = &os.PathError{Op: "torn-read", Path: path,
					Err: errTorn{len: len(got)}}
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			blob := a
			if i%2 == 1 {
				blob = b
			}
			for j := 0; j < 200; j++ {
				if err := atomicWriteUnique(path, blob); err != nil {
					t.Errorf("atomicWriteUnique: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-done

	if readErr != nil {
		t.Fatalf("torn read detected (fixed-name tmp bug): %v", readErr)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Fatalf("final content torn: %d bytes", len(got))
	}
}
