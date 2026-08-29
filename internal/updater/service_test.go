package updater

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"ssh-manager-mcp/internal/buildinfo"
)

// fakeProber satisfies the statusProber seam surface with a canned Status()
// result — the unit-test stand-in for a kardianos service.
type fakeProber struct {
	status service.Status
	err    error
}

func (f fakeProber) Status() (service.Status, error) { return f.status, f.err }

// setServiceNew swaps the serviceNew seam for the duration of one test and
// restores it on cleanup (same save/restore discipline as replace_test.go's
// currentGOOS/osRename seams).
func setServiceNew(t *testing.T, fn func(service.Interface, *service.Config) (statusProber, error)) {
	t.Helper()
	orig := serviceNew
	serviceNew = fn
	t.Cleanup(func() { serviceNew = orig })
}

func TestProbeServiceTrichotomy(t *testing.T) {
	kardianosFailedErr := errors.New("service in failed state") // verbatim kardianos v1.3.0 systemd backend

	cases := []struct {
		name      string
		prober    statusProber
		newErr    error
		wantState ProbeState
		wantDesc  []string // substrings that must all appear in Desc
	}{
		{
			name:      "not installed -> ProbeNotInstalled",
			prober:    fakeProber{err: service.ErrNotInstalled},
			wantState: ProbeNotInstalled,
		},
		{
			name:      "running -> ProbeInstalled",
			prober:    fakeProber{status: service.StatusRunning},
			wantState: ProbeInstalled,
			wantDesc:  []string{"running"},
		},
		{
			name:      "stopped -> ProbeInstalled",
			prober:    fakeProber{status: service.StatusStopped},
			wantState: ProbeInstalled,
			wantDesc:  []string{"stopped"},
		},
		{
			name:      "unknown status but no error -> ProbeInstalled (any answerable status = installed)",
			prober:    fakeProber{status: service.StatusUnknown},
			wantState: ProbeInstalled,
		},
		{
			// spec §3.2: the systemd failed state surfaces as an ERROR from
			// kardianos even though the unit exists — exactly the crash-loop
			// that most needs an update, so it must classify as Installed.
			name:      "systemd failed-state error string -> ProbeInstalled",
			prober:    fakeProber{err: kardianosFailedErr},
			wantState: ProbeInstalled,
			wantDesc:  []string{"failed"},
		},
		{
			name:      "unclassifiable status error -> ProbeMechanismErr (fail-closed)",
			prober:    fakeProber{err: errors.New("systemctl: dbus connection refused")},
			wantState: ProbeMechanismErr,
			wantDesc:  []string{"dbus connection refused"},
		},
		{
			name:      "no service system (container/CI) -> ProbeMechanismErr + skip marker for the CLI",
			prober:    nil,
			newErr:    service.ErrNoServiceSystemDetected,
			wantState: ProbeMechanismErr,
			wantDesc:  []string{DescNoServiceSystem},
		},
		{
			name:      "service.New other error -> ProbeMechanismErr",
			prober:    nil,
			newErr:    service.ErrNameFieldRequired,
			wantState: ProbeMechanismErr,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setServiceNew(t, func(_ service.Interface, _ *service.Config) (statusProber, error) {
				if tc.newErr != nil {
					return nil, tc.newErr
				}
				return tc.prober, nil
			})
			got := ProbeService(buildinfo.ServeServiceName)
			if got.State != tc.wantState {
				t.Fatalf("ProbeService state = %v, want %v (Desc=%q)", got.State, tc.wantState, got.Desc)
			}
			for _, sub := range tc.wantDesc {
				if !strings.Contains(got.Desc, sub) {
					t.Errorf("ProbeService Desc = %q, want substring %q", got.Desc, sub)
				}
			}
		})
	}
}

// TestProbeServiceBothNamesIndependent pins the 双服务并存 semantics boundary:
// probing the legacy name and the new name are two independent calls, each
// carrying its own answer. The COMBINATION policy (legacy present -> migrate
// block + abort) is the caller's (T8); this seam-level test only locks that
// the two calls cannot bleed into each other.
func TestProbeServiceBothNamesIndependent(t *testing.T) {
	byName := map[string]statusProber{
		buildinfo.ServeServiceName: fakeProber{status: service.StatusRunning}, // new name: installed
		legacyServiceName:          fakeProber{status: service.StatusStopped}, // old name: still installed
		"ghost-serve":              fakeProber{err: service.ErrNotInstalled},  // neither
	}
	setServiceNew(t, func(_ service.Interface, cfg *service.Config) (statusProber, error) {
		p, ok := byName[cfg.Name]
		if !ok {
			t.Errorf("ProbeService propagated unexpected name %q", cfg.Name)
			return fakeProber{err: service.ErrNotInstalled}, nil
		}
		return p, nil
	})

	if got := ProbeService(buildinfo.ServeServiceName); got.State != ProbeInstalled {
		t.Errorf("new name state = %v, want ProbeInstalled (Desc=%q)", got.State, got.Desc)
	}
	if got := ProbeService(legacyServiceName); got.State != ProbeInstalled {
		t.Errorf("legacy name state = %v, want ProbeInstalled (Desc=%q)", got.State, got.Desc)
	}
	if got := ProbeService("ghost-serve"); got.State != ProbeNotInstalled {
		t.Errorf("unregistered name state = %v, want ProbeNotInstalled (Desc=%q)", got.State, got.Desc)
	}
}

func TestRegisteredBinaryPathDispatch(t *testing.T) {
	restoreGOOS := setGOOS(t, "linux")
	t.Cleanup(restoreGOOS)

	t.Run("linux branch reads the unit file via the dir seam", func(t *testing.T) {
		dir := t.TempDir()
		orig := systemdUnitDir
		systemdUnitDir = dir
		t.Cleanup(func() { systemdUnitDir = orig })

		unit := filepath.Join(dir, buildinfo.ServeServiceName+".service")
		body := "[Service]\n" +
			"ExecStart=/usr/local/bin/sshmgr serve --addr 0.0.0.0:7878\n" +
			"Restart=on-failure\n"
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := RegisteredBinaryPath(buildinfo.ServeServiceName)
		if err != nil {
			t.Fatalf("RegisteredBinaryPath: %v", err)
		}
		if got != "/usr/local/bin/sshmgr" {
			t.Errorf("RegisteredBinaryPath = %q, want /usr/local/bin/sshmgr", got)
		}
	})

	t.Run("linux branch missing unit file -> error", func(t *testing.T) {
		systemdUnitDir = t.TempDir()
		if _, err := RegisteredBinaryPath(buildinfo.ServeServiceName); err == nil {
			t.Fatal("want error for missing unit file, got nil")
		}
	})

	t.Run("darwin branch reads the plist via the dir seam", func(t *testing.T) {
		setGOOS(t, "darwin")
		dir := t.TempDir()
		orig := launchdPlistDir
		launchdPlistDir = dir
		t.Cleanup(func() { launchdPlistDir = orig })

		plist := filepath.Join(dir, buildinfo.ServeServiceName+".plist")
		body := plistTemplate("/usr/local/bin/sshmgr", "serve")
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := RegisteredBinaryPath(buildinfo.ServeServiceName)
		if err != nil {
			t.Fatalf("RegisteredBinaryPath: %v", err)
		}
		if got != "/usr/local/bin/sshmgr" {
			t.Errorf("RegisteredBinaryPath = %q, want /usr/local/bin/sshmgr", got)
		}
	})

	t.Run("windows branch delegates to the SCM seam", func(t *testing.T) {
		setGOOS(t, "windows")
		orig := scmQueryBinaryPath
		scmQueryBinaryPath = func(name string) (string, error) {
			if name != buildinfo.ServeServiceName {
				t.Errorf("scmQueryBinaryPath got name %q", name)
			}
			return `C:\Tools\sshmgr.exe`, nil
		}
		t.Cleanup(func() { scmQueryBinaryPath = orig })

		got, err := RegisteredBinaryPath(buildinfo.ServeServiceName)
		if err != nil {
			t.Fatalf("RegisteredBinaryPath: %v", err)
		}
		if got != `C:\Tools\sshmgr.exe` {
			t.Errorf("RegisteredBinaryPath = %q", got)
		}
	})

	t.Run("windows branch without SCM wiring -> error (fail-closed)", func(t *testing.T) {
		setGOOS(t, "windows")
		orig := scmQueryBinaryPath
		scmQueryBinaryPath = nil
		t.Cleanup(func() { scmQueryBinaryPath = orig })

		if _, err := RegisteredBinaryPath(buildinfo.ServeServiceName); err == nil {
			t.Fatal("want error when SCM wiring is absent, got nil")
		}
	})

	t.Run("unsupported platform -> error", func(t *testing.T) {
		setGOOS(t, "plan9")
		if _, err := RegisteredBinaryPath(buildinfo.ServeServiceName); err == nil {
			t.Fatal("want error on unsupported platform, got nil")
		}
	})
}

func TestSystemdRegisteredBinaryPathErrors(t *testing.T) {
	dir := t.TempDir()
	orig := systemdUnitDir
	systemdUnitDir = dir
	t.Cleanup(func() { systemdUnitDir = orig })

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".service"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("unit without ExecStart -> error", func(t *testing.T) {
		write("noexec", "[Service]\nType=simple\n")
		if _, err := systemdRegisteredBinaryPath("noexec"); err == nil {
			t.Fatal("want error for unit without ExecStart, got nil")
		}
	})

	t.Run("missing unit file -> error", func(t *testing.T) {
		if _, err := systemdRegisteredBinaryPath("absent"); err == nil {
			t.Fatal("want error for missing unit, got nil")
		}
	})
}

// TestExecStartBinary covers the systemd ExecStart first-token parse: plain,
// quoted (paths with spaces), systemd exec-prefix modifiers (- + !), args and
// indented/commented lines.
func TestExecStartBinary(t *testing.T) {
	cases := []struct {
		name   string
		unit   string
		want   string
		wantOK bool
	}{
		{name: "plain absolute + args", unit: "ExecStart=/usr/local/bin/sshmgr serve --addr 0.0.0.0:7878", want: "/usr/local/bin/sshmgr", wantOK: true},
		{name: "bare path no args", unit: "ExecStart=/usr/bin/sshmgr", want: "/usr/bin/sshmgr", wantOK: true},
		{name: "double-quoted path with spaces", unit: `ExecStart="/opt/my app/sshmgr" serve`, want: "/opt/my app/sshmgr", wantOK: true},
		{name: "single-quoted path", unit: `ExecStart='/opt/app/sshmgr' serve`, want: "/opt/app/sshmgr", wantOK: true},
		{name: "ignore-failure prefix -", unit: "ExecStart=-/usr/bin/sshmgr serve", want: "/usr/bin/sshmgr", wantOK: true},
		{name: "privilege prefix +", unit: "ExecStart=+/usr/bin/sshmgr serve", want: "/usr/bin/sshmgr", wantOK: true},
		{name: "prefix combo !+", unit: "ExecStart=!+/usr/bin/sshmgr serve", want: "/usr/bin/sshmgr", wantOK: true},
		{name: "indented key", unit: "[Service]\n  ExecStart=/opt/sshmgr serve", want: "/opt/sshmgr", wantOK: true},
		{name: "full kardianos unit", unit: "[Unit]\nDescription=x\n\n[Service]\nType=simple\nExecStart=/usr/local/bin/sshmgr serve --addr 0.0.0.0:7878\nRestart=on-failure\n\n[Install]\nWantedBy=multi-user.target\n", want: "/usr/local/bin/sshmgr", wantOK: true},
		{name: "ExecStartPre must not match", unit: "ExecStartPre=/bin/echo hi\nExecStart=/bin/sshmgr\n", want: "/bin/sshmgr", wantOK: true},
		{name: "commented line ignored", unit: "# ExecStart=/bin/false\nExecStart=/bin/sshmgr\n", want: "/bin/sshmgr", wantOK: true},
		{name: "no ExecStart", unit: "[Service]\nType=simple\n", want: "", wantOK: false},
		{name: "empty value", unit: "ExecStart=\n", want: "", wantOK: false},
		{name: "only prefixes no path", unit: "ExecStart=- !\n", want: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := execStartBinary(tc.unit)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("execStartBinary = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLaunchdRegisteredBinaryPathErrors(t *testing.T) {
	dir := t.TempDir()
	orig := launchdPlistDir
	launchdPlistDir = dir
	t.Cleanup(func() { launchdPlistDir = orig })

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".plist"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("plist without ProgramArguments -> error", func(t *testing.T) {
		write("noargs", plistTemplate("", ""))
		if _, err := launchdRegisteredBinaryPath("noargs"); err == nil {
			t.Fatal("want error for plist without ProgramArguments, got nil")
		}
	})

	t.Run("missing plist -> error", func(t *testing.T) {
		if _, err := launchdRegisteredBinaryPath("absent"); err == nil {
			t.Fatal("want error for missing plist, got nil")
		}
	})
}

// TestPlistProgramArgumentsFirst covers the launchd ProgramArguments
// first-item parse, including XML entity unescaping and the guard against
// stealing a <string> from a later key when ProgramArguments is empty.
func TestPlistProgramArgumentsFirst(t *testing.T) {
	cases := []struct {
		name   string
		plist  string
		want   string
		wantOK bool
	}{
		{
			name:   "kardianos shape",
			plist:  plistTemplate("/usr/local/bin/sshmgr", "serve"),
			want:   "/usr/local/bin/sshmgr",
			wantOK: true,
		},
		{
			name: "escaped entity in path",
			plist: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>sshmgr-serve</string>
<key>ProgramArguments</key>
<array>
    <string>/opt/a&amp;b/sshmgr</string>
    <string>serve</string>
</array>
</dict></plist>`,
			want:   "/opt/a&b/sshmgr",
			wantOK: true,
		},
		{
			name: "empty array must not leak a later key's string",
			plist: `<?xml version="1.0"?>
<plist version="1.0"><dict>
<key>Label</key><string>sshmgr-serve</string>
<key>ProgramArguments</key>
<array></array>
<key>StandardOutPath</key>
<string>/var/log/sshmgr.log</string>
</dict></plist>`,
			wantOK: false,
		},
		{name: "missing ProgramArguments", plist: plistTemplate("", ""), wantOK: false},
		{name: "self-closing empty array", plist: `<plist><dict><key>ProgramArguments</key><array/></dict></plist>`, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := plistProgramArgumentsFirst(tc.plist)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("plistProgramArgumentsFirst = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParseWindowsBinaryPath covers the lpBinaryPathName / ImagePath剥取 —
// pure string logic, tested on every platform (the SCM calls themselves are
// exercised on real machine G in T8, never by unit tests).
func TestParseWindowsBinaryPath(t *testing.T) {
	cases := []struct {
		name      string
		imagePath string
		want      string
	}{
		{
			// The brief's pinned example — kardianos writes exactly this shape
			// when the install path contains no spaces.
			name:      "quoted exe + args",
			imagePath: `"C:\path\sshmgr.exe" serve --addr 0.0.0.0:7878`,
			want:      `C:\path\sshmgr.exe`,
		},
		{
			name:      "quoted exe path with spaces + args",
			imagePath: `"C:\Program Files\sshmgr\sshmgr.exe" serve`,
			want:      `C:\Program Files\sshmgr\sshmgr.exe`,
		},
		{
			name:      "unquoted exe + args",
			imagePath: `C:\path\sshmgr.exe serve`,
			want:      `C:\path\sshmgr.exe`,
		},
		{name: "bare exe", imagePath: `C:\Tools\sshmgr.exe`, want: `C:\Tools\sshmgr.exe`},
		{name: "unterminated quote degrades to prefix strip", imagePath: `"C:\Tools\sshmgr.exe`, want: `C:\Tools\sshmgr.exe`},
		{name: "leading whitespace", imagePath: `  "C:\x\a.exe"  args`, want: `C:\x\a.exe`},
		{name: "tab separator", imagePath: "C:\\x\\a.exe\tserve", want: `C:\x\a.exe`},
		{name: "empty", imagePath: "", want: ""},
		{name: "whitespace only", imagePath: "   ", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWindowsBinaryPath(tc.imagePath); got != tc.want {
				t.Errorf("parseWindowsBinaryPath(%q) = %q, want %q", tc.imagePath, got, tc.want)
			}
		})
	}
}

// TestSameBinaryPath runs within one platform and asserts that platform's
// comparison semantics (Windows: case-insensitive via EqualFold; POSIX:
// byte-equal) — spec §4.4 比对口径.
func TestSameBinaryPath(t *testing.T) {
	dir := t.TempDir()

	casesInsensitive := runtime.GOOS == "windows"

	t.Run("identical absolute paths", func(t *testing.T) {
		p := filepath.Join(dir, "sshmgr.exe")
		if !SameBinaryPath(p, p) {
			t.Errorf("SameBinaryPath(%q, %q) = false, want true", p, p)
		}
	})

	t.Run("case difference per platform semantics", func(t *testing.T) {
		p := filepath.Join(dir, "SshMgr.EXE")
		upper := strings.ToUpper(p)
		if got, want := SameBinaryPath(p, upper), casesInsensitive; got != want {
			t.Errorf("SameBinaryPath(%q, %q) = %v, want %v (GOOS=%s)", p, upper, got, want, runtime.GOOS)
		}
	})

	t.Run("relative path normalizes against cwd", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		rel := filepath.Join("internal", "updater", "phantom-sshmgr.exe")
		abs := filepath.Join(cwd, rel)
		if !SameBinaryPath(rel, abs) {
			t.Errorf("SameBinaryPath(%q, %q) = false, want true", rel, abs)
		}
	})

	t.Run("symlink resolves to target", func(t *testing.T) {
		target := filepath.Join(dir, "real-sshmgr.exe")
		if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link-sshmgr.exe")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unavailable on this host/privilege level: %v", err)
		}
		if !SameBinaryPath(link, target) {
			t.Errorf("SameBinaryPath(symlink, target) = false, want true")
		}
	})

	t.Run("non-existent paths still compare via Abs fallback", func(t *testing.T) {
		ghost := filepath.Join(dir, "ghost-sshmgr.exe")
		if !SameBinaryPath(ghost, ghost) {
			t.Errorf("SameBinaryPath(ghost, ghost) = false, want true")
		}
		if SameBinaryPath(ghost, filepath.Join(dir, "other.exe")) {
			t.Errorf("SameBinaryPath(ghost, other) = true, want false")
		}
	})

	t.Run("different paths -> false", func(t *testing.T) {
		if SameBinaryPath(filepath.Join(dir, "a.exe"), filepath.Join(dir, "b.exe")) {
			t.Error("different paths compared equal")
		}
	})
}

func TestMigrationBlock(t *testing.T) {
	got := MigrationBlock()
	for _, sub := range []string{
		legacyServiceName,             // 旧服务名
		buildinfo.ServeServiceName,    // 新服务名
		"sc qc " + legacyServiceName,  // 三条命令之一:读旧参数
		"ssh-manager serve uninstall", // 三条命令之二:卸旧
		"sshmgr serve install",        // 三条命令之三:装新
		"先迁 client 后升 serve",          // 顺序铁律(docs 同源措辞)
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("MigrationBlock() missing %q\ngot:\n%s", sub, got)
		}
	}
}

// setGOOS swaps the currentGOOS branch-dispatch seam (shared with replace.go)
// and returns a restore func.
func setGOOS(t *testing.T, goos string) func() {
	t.Helper()
	orig := currentGOOS
	currentGOOS = goos
	return func() { currentGOOS = orig }
}

// plistTemplate renders a kardianos-shaped launchd plist. Empty bin/args
// render a plist whose ProgramArguments array carries no usable first item.
func plistTemplate(bin, firstArg string) string {
	programArguments := "<key>ProgramArguments</key>\n<array>\n"
	if bin != "" {
		programArguments += "\t<string>" + bin + "</string>\n"
		if firstArg != "" {
			programArguments += "\t<string>" + firstArg + "</string>\n"
		}
		programArguments += "</array>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>sshmgr-serve</string>
	` + programArguments + `<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}
