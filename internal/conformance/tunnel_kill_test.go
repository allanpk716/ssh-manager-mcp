package conformance

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestTunnelKillRealSSH is the Plan-35 kill emergency-stop end-to-end against
// a REAL OpenSSH container (spec §8 #19's conformance case — the mcpserver
// suite pins the control loop hermetically via runControlTick; THIS is the
// real-wire evidence with the production pieces in the loop): the broker path
// (ForwardForProfile) opens a mirrored tunnel, the OWNER CLI runs as a REAL
// subprocess against the same vault (`tunnels kill <id>`), the broker's live
// 15s control loop (StartSweeper, not a direct tick) applies the order, and
// the forwarded port becomes unreachable + `tunnels ls` shows no row. The
// forward target is the container's OWN sshd (127.0.0.1:22 from the server's
// perspective — the container-loopback nuance), so the pre-kill sanity probe
// reads the sshd banner straight through the tunnel.
func TestTunnelKillRealSSH(t *testing.T) {
	requireConformance(t)
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{}) // password-only container (sshuser:testpw123)
	defer cleanup()

	// Vault in a tempdir; the CLI subprocess reaches it via SSHMGR_STORE +
	// SSHMGR_MASTERKEY_HEX (the vault's own test/scripting tier — threat-model
	// §5), which keeps the fixture to one file and one env pair.
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	storePath := filepath.Join(dir, "store.db")
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("testpw123")})
	if err != nil {
		t.Fatalf("set credential: %v", err)
	}
	srv := &models.Server{
		Name: "gpu", Host: host, Port: port, User: "sshuser",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	srvID, err := st.AddServer(srv)
	if err != nil {
		t.Fatalf("add server: %v", err)
	}
	_ = st.SaveHostKey(host, port, hostKey.Marshal()) // TOFU pre-trust the container's fresh key
	pid, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{srvID}); err != nil {
		t.Fatal(err)
	}
	projID, _, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}

	// Broker side: a manager with the PRODUCTION control loop (StartSweeper
	// launches the sweepLoop whose 15s control ticker applies kill orders —
	// the CLI's 45s poll budget covers up to three ticks).
	mgr := mcpserver.NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	mgr.StartSweeper()
	defer mgr.CloseAll()

	out, err := mcpserver.ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", 22, 0, "", mgr)
	if err != nil {
		t.Fatalf("ForwardForProfile: %v", err)
	}
	localAddr := fmt.Sprintf("127.0.0.1:%d", out.LocalPort)

	// Pre-kill sanity: the tunnel really forwards — the container's sshd
	// answers through it with its identification banner. Small retry loop:
	// the listener is up, but the first direct-tcpip dial races the channel
	// setup on a cold connection.
	banner := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("tcp", localAddr, 2*time.Second); derr == nil {
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 64)
			n, rerr := c.Read(buf)
			c.Close()
			if rerr == nil && n > 0 {
				banner = string(buf[:n])
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.HasPrefix(banner, "SSH-2.0-") && !strings.HasPrefix(banner, "SSH-1.99-") {
		t.Fatalf("pre-kill sanity: no sshd banner through the tunnel at %s (got %q)", localAddr, banner)
	}

	// Owner side: build the real binary once, run the real CLI against the
	// same vault (same env shape the production owner uses — store path +
	// key material, no token: owner commands are not project-scoped).
	binPath := filepath.Join(dir, conformanceBinName())
	build := exec.Command("go", "build", "-o", binPath, "ssh-manager-mcp/cmd/sshmgr")
	if bout, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("go build: %v\n%s", berr, bout)
	}
	cliEnv := append(os.Environ(),
		"SSHMGR_STORE="+storePath,
		"SSHMGR_MASTERKEY_HEX="+hex.EncodeToString(mk),
	)
	runCLI := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Env = cliEnv
		outBytes, rerr := cmd.CombinedOutput()
		if rerr != nil {
			t.Fatalf("sshmgr %s: %v\n%s", strings.Join(args, " "), rerr, outBytes)
		}
		return string(outBytes)
	}

	// The kill itself — the CLI polls ≤45s for a broker tick to apply.
	killOut := runCLI("tunnels", "kill", out.TunnelID)
	if !strings.Contains(killOut, "applied") {
		t.Fatalf("tunnels kill output = %q, want \"applied\"", killOut)
	}

	// Post-kill assertions: port unreachable + registry row gone from ls.
	if _, derr := net.DialTimeout("tcp", localAddr, 2*time.Second); derr == nil {
		t.Fatalf("port %s must be unreachable after tunnels kill", localAddr)
	}
	lsOut := runCLI("tunnels", "ls")
	if !strings.Contains(lsOut, "no open tunnels") {
		t.Fatalf("tunnels ls after kill = %q, want the no-open-tunnels line", lsOut)
	}
	t.Logf("kill e2e ok: tunnel %s banner-through-tunnel=%q kill=%q", out.TunnelID, banner, strings.TrimSpace(killOut))
}

// conformanceBinName returns the platform-correct binary name for the local
// `go build` (mirrors eval's binName — the two packages keep their own copies
// to avoid exporting a helper for a test-only concern).
func conformanceBinName() string {
	if runtime.GOOS == "windows" {
		return "sshmgr.exe"
	}
	return "sshmgr"
}
