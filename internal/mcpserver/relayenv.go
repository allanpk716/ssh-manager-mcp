package mcpserver

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// ---- Plan 47: relay env seam + constants (spec §4) ----

// relayChunkDefault / relayChunkMin / relayChunkMax bound SSHMGR_TRANSFER_CHUNK
// (spec §4): unset → 256 MiB; anything outside [16 MiB, 1 GiB] is a
// CONSTRUCTION failure (fail-closed), never a silent clamp. The floor keeps
// the manifest chunk count within relayMaxChunks for TB-scale files; the
// ceiling mirrors Plan 33's 1 GiB cap style (keeps derived sizes far from
// int64 overflow).
const (
	relayChunkDefault int64 = 256 << 20 // 256 MiB
	relayChunkMin     int64 = 16 << 20  // 16 MiB
	relayChunkMax     int64 = 1 << 30   // 1 GiB
)

// resolveRelayChunk parses SSHMGR_TRANSFER_CHUNK (resolveUploadContentCap's
// fail-closed shape, Plan 33): unset/empty → relayChunkDefault; unparsable /
// non-positive / below relayChunkMin / above relayChunkMax → error. Called at
// every NewServerFromSource construction (per-construction read, spec §4 —
// cross-project resume drift is blocked downstream by the manifest
// chunk_bytes check, not claimed away here).
func resolveRelayChunk() (int64, error) {
	v := os.Getenv("SSHMGR_TRANSFER_CHUNK")
	if v == "" {
		return relayChunkDefault, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("SSHMGR_TRANSFER_CHUNK: invalid value %q (want positive integer)", v)
	}
	if n > relayChunkMax {
		return 0, fmt.Errorf("SSHMGR_TRANSFER_CHUNK: %d exceeds the 1 GiB ceiling (1073741824)", n)
	}
	if n < relayChunkMin {
		return 0, fmt.Errorf("SSHMGR_TRANSFER_CHUNK: %d is below the 16 MiB floor (16777216)", n)
	}
	return n, nil
}

// resolveRelayParallel validates SSHMGR_TRANSFER_PARALLEL (spec §4): v1 is
// single-stream, so only unset/empty or the literal "1" is accepted — ANY
// other value is a construction failure whose text names the reserved status.
// The seam name and the final v2 clamp domain [1,8] are frozen now so opening
// parallel chunk transfer in v2 is a zero-migration change.
func resolveRelayParallel() (int64, error) {
	v := os.Getenv("SSHMGR_TRANSFER_PARALLEL")
	if v == "" || v == "1" {
		return 1, nil
	}
	return 0, fmt.Errorf("SSHMGR_TRANSFER_PARALLEL: invalid value %q (v1 accepts only \"1\" or unset — parallel chunk transfer is reserved for a future version)", v)
}

// relayRunCap is the hard per-task wall clock for one relay transfer (spec §4):
// the task deadline is Insert-time + 72h; at expiry the engine's ctx cancels →
// chunk-boundary stop, resumable, terminal timeout (50 GB @ 2 Mbps ≈ 58 h —
// the cap covers ADSL-grade bandwidth with margin). A CONSTANT, deliberately
// NOT an env and NOT linked to SSHMGR_BG_RUN_CAP: relay duration is bounded
// physically (bandwidth × bytes) while exec duration is bounded semantically —
// one env silently moving both faces is a trap (an own seam would go to the
// backlog first).
const relayRunCap = 72 * time.Hour

// relayMaxChunks caps ⌈source_size/chunk_bytes⌉ per transfer (spec §2③b): a
// preflight fail-closed gate against manifest O(chunks²) write amplification
// and unbounded manifest growth (16384 chunks covers 4 TB @ 256 MiB; at 16 MiB
// chunks it bounds the worst-case cumulative manifest rewrite at ≈7% of the
// data). Journal-ized manifest structures for TB scale are backlog material.
const relayMaxChunks int64 = 16384
