package cli

import "testing"

// newRootForTest 构造根命令但不执行它，供只检查命令注册形态的测试使用。
func newRootForTest(t *testing.T) *rootCommand {
	t.Helper()
	return NewRootCmd()
}

func TestServeCmd_RegisteredWithFlags(t *testing.T) {
	root := newRootForTest(t)
	srv, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatal("serve subcommand not registered:", err)
	}
	for _, flag := range []string{"addr", "tls-cert", "tls-key", "pairing", "discovery"} {
		if srv.Flags().Lookup(flag) == nil {
			t.Errorf("serve missing --%s flag", flag)
		}
	}
}

// TestServeSwitchOpts pins the T6 explicitness channel (Plan 42 批1): an
// unpassed flag yields nil (env → store → default decide); a passed flag
// yields a non-nil *bool with its value — INCLUDING false, which is the whole
// point of the tri-state.
func TestServeSwitchOpts(t *testing.T) {
	root := newRootForTest(t)
	srv, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatal("serve subcommand not registered:", err)
	}
	if opts := serveSwitchOpts(srv); opts.PairingFlag != nil || opts.DiscoveryFlag != nil {
		t.Fatalf("absent flags must resolve to nil, got %+v", opts)
	}
	if err := srv.ParseFlags([]string{"--discovery=false", "--pairing=true"}); err != nil {
		t.Fatal(err)
	}
	opts := serveSwitchOpts(srv)
	if opts.DiscoveryFlag == nil || *opts.DiscoveryFlag {
		t.Fatalf("--discovery=false must yield non-nil false, got %+v", opts.DiscoveryFlag)
	}
	if opts.PairingFlag == nil || !*opts.PairingFlag {
		t.Fatalf("--pairing=true must yield non-nil true, got %+v", opts.PairingFlag)
	}
}
