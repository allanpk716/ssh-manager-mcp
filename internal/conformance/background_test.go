package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"

	"golang.org/x/crypto/ssh"
)

// bgLifecycleTrio drives the Plan-32 background lifecycle (start → incremental
// output → stop → terminal) against a REAL OpenSSH container through the
// production TaskManager: Start connects via ConnectKeepAlive (30s interval /
// 3-strike keepalive — the 24h-long-connection variant), the engine runs the
// command through runSession over the wire, and Output/Stop exercise the
// long-poll + terminal semantics exactly as the broker serves them. The
// mcpserver suite covers the same lifecycle against the in-process testsshd;
// THIS test is the real-OpenSSH wire evidence (the §13 conformance role), and
// the zero-change foreground interop/differential suites re-running green
// alongside it is the runSession-extraction SSH-layer evidence Plan 32 calls
// for (differences-ledger rows registered in docs by Plan 32 T9).
//
// The ForProfile tool layer (profile gate, wait clamps, encoding) is unit-
// covered in mcpserver — driving it here would need a full seeded vault, so
// the TaskManager seam is used directly: it is the layer that owns the SSH
// session lifetime, which is what real-OpenSSH conformance is about.
func TestBackgroundLifecycleRealSSH(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	// Real store for the audit rows Start/Insert write (exec-bg-start /
	// exec-bg-end). Audit CONTENT is unit-covered in mcpserver
	// (TestBackgroundAuditEndRows); this test needs the store only so the
	// production Start path runs unmodified — audit rows are not asserted here.
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mgr, err := mcpserver.NewTaskManager()
	if err != nil {
		t.Fatalf("new task manager: %v", err)
	}
	defer mgr.CloseAll()

	auth := mustPrivAuth(t, privPath, "")
	server := &models.Server{Host: host, Port: port, User: "sshuser"}
	hostKeyCb := ssh.FixedHostKey(hostKey)
	ctx := context.Background()

	// ---- Phase 1: start → incremental output → natural done ----
	// The script paces one "line N" per second for ~5s, so long-polls observe
	// genuinely incremental chunks while the task is still running.
	const loopCmd = `for i in 1 2 3 4 5; do echo "line $i"; sleep 1; done`
	loopID, _, err := mgr.Start(ctx, st, mcpserver.BgStartSpec{
		ProjectID: "conformance", ServerID: "srv-conf", Command: loopCmd,
		TimeoutSec: 60, Server: server, Auth: auth, HostKeyCb: hostKeyCb,
	})
	if err != nil {
		t.Fatalf("start loop task: %v", err)
	}

	var collected strings.Builder
	var offsets []int64
	runningWithOutput := false // an increment arrived while the task was still running (the incremental signal)
	off := int64(0)
	var final mcpserver.BgView
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v, ok, oerr := mgr.Output(loopID, off, 0, 10*time.Second, ctx)
		if oerr != nil || !ok {
			t.Fatalf("output poll: ok=%v err=%v", ok, oerr)
		}
		offsets = append(offsets, v.NextStdout)
		if v.Status == "running" && len(v.Stdout) > 0 {
			runningWithOutput = true
		}
		collected.Write(v.Stdout)
		off = v.NextStdout
		final = v
		if v.Status != "running" {
			break
		}
	}
	if final.Status == "running" {
		t.Fatalf("loop task never left running within 30s (collected %q)", collected.String())
	}
	if final.Status != "done" || final.ExitCode != 0 {
		t.Fatalf("loop task terminal = status %q exit %d err %q, want done/0", final.Status, final.ExitCode, final.ErrText)
	}

	// All 5 lines arrived, in stream order.
	all := collected.String()
	pos := 0
	for i := 1; i <= 5; i++ {
		line := fmt.Sprintf("line %d", i)
		idx := strings.Index(all[pos:], line)
		if idx < 0 {
			t.Fatalf("line %d missing or out of order in collected stdout %q", i, all)
		}
		pos += idx + len(line)
	}
	// Incremental collection: an output chunk arrived while still running, and
	// the cursor strictly advanced across polls (≥2 polls with a later offset
	// greater than an earlier one — the same adjacent-scan argument as scoreT9).
	if !runningWithOutput {
		t.Fatalf("no output observed while the task was running — collection was not incremental (offsets=%v)", offsets)
	}
	advanced := false
	for i := 1; i < len(offsets); i++ {
		if offsets[i] > offsets[i-1] {
			advanced = true
			break
		}
	}
	if !advanced {
		t.Fatalf("next offset never advanced across polls (offsets=%v)", offsets)
	}
	// Drain anchor (spec §7): at terminal, a final poll from the last cursor
	// returns zero new bytes and the cursor equals the whole-stream total.
	drain, ok, oerr := mgr.Output(loopID, off, 0, 0, ctx)
	if oerr != nil || !ok {
		t.Fatalf("drain poll: ok=%v err=%v", ok, oerr)
	}
	if len(drain.Stdout) != 0 || drain.NextStdout != drain.StdoutTotal || drain.Status != "done" {
		t.Fatalf("drain anchor violated: stdout=%q next=%d total=%d status=%q",
			drain.Stdout, drain.NextStdout, drain.StdoutTotal, drain.Status)
	}

	// ---- Phase 2: stop path → terminal stopped ----
	sleepID, _, err := mgr.Start(ctx, st, mcpserver.BgStartSpec{
		ProjectID: "conformance", ServerID: "srv-conf", Command: "sleep 300",
		Server: server, Auth: auth, HostKeyCb: hostKeyCb,
	})
	if err != nil {
		t.Fatalf("start sleep task: %v", err)
	}
	// Immediate snapshot (wait=0): the just-started task is running.
	snap, ok, oerr := mgr.Output(sleepID, 0, 0, 0, ctx)
	if oerr != nil || !ok {
		t.Fatalf("sleep snapshot: ok=%v err=%v", ok, oerr)
	}
	if snap.Status != "running" {
		t.Fatalf("sleep task snapshot status = %q, want running", snap.Status)
	}
	// Stop returns the TRIGGER-TIME status (running — the stop was set in
	// motion; the terminal state is observed, never blocked for, here).
	trigger, ok := mgr.Stop(sleepID)
	if !ok || trigger != "running" {
		t.Fatalf("stop returned (%q, %v), want (running, true)", trigger, ok)
	}
	// Long-poll observes the terminal state: the condition fires the moment
	// the task leaves running (session close → SIGHUP → sleep dies).
	deadline = time.Now().Add(30 * time.Second)
	var stopped mcpserver.BgView
	for time.Now().Before(deadline) {
		v, ok2, oerr2 := mgr.Output(sleepID, 0, 0, 10*time.Second, ctx)
		if oerr2 != nil || !ok2 {
			t.Fatalf("terminal poll: ok=%v err=%v", ok2, oerr2)
		}
		stopped = v
		if v.Status != "running" {
			break
		}
	}
	if stopped.Status != "stopped" {
		t.Fatalf("sleep task terminal = %q (err %q), want stopped", stopped.Status, stopped.ErrText)
	}
}

// TestBackgroundKeepAliveRealSSH is the optional Plan-32 keepalive smoke
// (真连接冒烟): a background `sleep 90` task holds an IDLE SSH session across
// ≥3 keepalive windows (30s interval, CountMax=3 — a misbehaving keepalive
// would judge a healthy idle connection dead within ~90s and flip the task to
// failed), then completes naturally. The observable: the task reaches done /
// exit 0 — never failed — proving the keepalive loop does not tear down
// healthy idle background connections.
//
// Double-gated: besides SSHMGR_CONFORMANCE=1 it needs
// SSHMGR_CONFORMANCE_KEEPALIVE=1 because it burns ~90s of wall time (three
// 30s budget-expiry polls + the terminal poll); the main lifecycle test stays
// fast. No new docker plumbing — TaskManager.Start already connects via
// ConnectKeepAlive.
func TestBackgroundKeepAliveRealSSH(t *testing.T) {
	requireConformance(t)
	if os.Getenv("SSHMGR_CONFORMANCE_KEEPALIVE") != "1" {
		t.Skip("set SSHMGR_CONFORMANCE_KEEPALIVE=1 (plus SSHMGR_CONFORMANCE=1) to run the 90s keepalive smoke — a sleep 90 background task across ≥3 keepalive windows")
	}
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mgr, err := mcpserver.NewTaskManager()
	if err != nil {
		t.Fatalf("new task manager: %v", err)
	}
	defer mgr.CloseAll()

	server := &models.Server{Host: host, Port: port, User: "sshuser"}
	ctx := context.Background()
	id, _, err := mgr.Start(ctx, st, mcpserver.BgStartSpec{
		ProjectID: "conformance", ServerID: "srv-conf", Command: "sleep 90",
		TimeoutSec: 300, Server: server,
		Auth: mustPrivAuth(t, privPath, ""), HostKeyCb: ssh.FixedHostKey(hostKey),
	})
	if err != nil {
		t.Fatalf("start sleep 90: %v", err)
	}

	// Three 30s polls over an idle stream: each returns at budget expiry with
	// the task STILL running (no bytes ever arrive; if keepalive had killed
	// the connection the status would flip to failed mid-window).
	for i := 0; i < 3; i++ {
		v, ok, oerr := mgr.Output(id, 0, 0, 30*time.Second, ctx)
		if oerr != nil || !ok {
			t.Fatalf("idle poll %d: ok=%v err=%v", i+1, ok, oerr)
		}
		if v.Status != "running" {
			t.Fatalf("idle poll %d: status = %q (err %q), want running — keepalive tore down a healthy idle connection", i+1, v.Status, v.ErrText)
		}
	}
	// Terminal poll: sleep 90 exits naturally → done/0 (failed here would
	// mean the connection died before the command completed).
	deadline := time.Now().Add(60 * time.Second)
	var final mcpserver.BgView
	for time.Now().Before(deadline) {
		v, ok, oerr := mgr.Output(id, 0, 0, 30*time.Second, ctx)
		if oerr != nil || !ok {
			t.Fatalf("terminal poll: ok=%v err=%v", ok, oerr)
		}
		final = v
		if v.Status != "running" {
			break
		}
	}
	if final.Status != "done" || final.ExitCode != 0 {
		t.Fatalf("sleep 90 terminal = status %q exit %d err %q, want done/0", final.Status, final.ExitCode, final.ErrText)
	}
}
