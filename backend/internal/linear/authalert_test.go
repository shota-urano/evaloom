package linear

import (
	"sync"
	"sync/atomic"
	"testing"
)

// 受け入れ基準: 照会と送信が同時に認証エラーを検出しても通知は1回
// （確かめて掛ける操作が1つの操作、spec 16 3.4.3 step 3）。
func TestAuthAlertGateConcurrentAcquireOnce(t *testing.T) {
	for range 50 {
		var gate AuthAlertGate
		var won atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if gate.Acquire() {
					won.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("Acquire succeeded %d times; want 1", won.Load())
		}
	}
}

// 受け入れ基準: 抑止は成功で解け、解いた後の認証エラーでもう一度出る。
func TestAuthAlertGateReleaseAllowsNextAlert(t *testing.T) {
	var gate AuthAlertGate
	if !gate.Acquire() || gate.Acquire() || gate.Acquire() {
		t.Fatal("held gate must refuse until released")
	}
	gate.Release()
	gate.Release() // releasing twice is harmless
	if !gate.Acquire() || gate.Acquire() {
		t.Fatal("released gate must allow exactly one more alert")
	}
}
