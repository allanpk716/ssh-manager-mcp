package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"ssh-manager-mcp/internal/roles"
)

type fakeSystemctl struct {
	results []systemctlResult
	calls   [][]string
}

type fakePlatformService struct {
	service.Service
	platform string
}

func (s fakePlatformService) Platform() string { return s.platform }

func (f *fakeSystemctl) run(args ...string) systemctlResult {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.calls) > len(f.results) {
		return systemctlResult{
			exitCode: 1,
			stderr:   "unexpected systemctl call",
			err:      errors.New("unexpected systemctl call"),
		}
	}
	return f.results[len(f.calls)-1]
}

func (f *fakeSystemctl) commandLines() []string {
	lines := make([]string, 0, len(f.calls))
	for _, args := range f.calls {
		lines = append(lines, strings.Join(args, " "))
	}
	return lines
}

func useSystemdUninstaller(t *testing.T, results ...systemctlResult) (*fakeSystemctl, string) {
	t.Helper()

	unitDir := t.TempDir()
	fake := &fakeSystemctl{results: results}
	origDir := systemdSystemUnitDir
	origRun := runSystemctlCommand
	systemdSystemUnitDir = unitDir
	runSystemctlCommand = fake.run
	backend := newSystemdServeUninstaller(serveServiceName)
	origNew := newServeServiceUninstaller
	newServeServiceUninstaller = func(service.Interface, *service.Config) (serveServiceUninstaller, error) {
		return backend, nil
	}
	t.Cleanup(func() {
		systemdSystemUnitDir = origDir
		runSystemctlCommand = origRun
		newServeServiceUninstaller = origNew
	})
	return fake, unitDir
}

func TestUninstallServeService_SystemdMissingUnitIsIdempotent(t *testing.T) {
	notFound := systemctlResult{stdout: "LoadState=not-found\n"}
	fake, unitDir := useSystemdUninstaller(t, notFound, notFound)

	output := &strings.Builder{}
	if err := uninstallServeService(output); err != nil {
		t.Fatalf("uninstall missing systemd serve unit: %v", err)
	}

	wantCommands := []string{
		"show -p LoadState -- " + serveServiceName + ".service",
		"show -p LoadState -- " + serveServiceName + ".service",
	}
	if got := fake.commandLines(); !equalStrings(got, wantCommands) {
		t.Fatalf("systemctl calls = %q, want %q", got, wantCommands)
	}
	if _, err := os.Stat(filepath.Join(unitDir, serveServiceName+".service")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unit file status = %v, want not exist", err)
	}
	if !strings.Contains(output.String(), "is not installed") {
		t.Fatalf("output missing not-installed message:\n%s", output.String())
	}
}

func TestUninstallServeService_SystemdExitCodeFiveMeansMissingUnit(t *testing.T) {
	exitFive := systemctlResult{
		exitCode: 5,
		err:      errors.New("exit status 5"),
	}
	fake, _ := useSystemdUninstaller(t, exitFive, exitFive)

	output := &strings.Builder{}
	if err := uninstallServeService(output); err != nil {
		t.Fatalf("uninstall systemd unit reported as exit status five: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("systemctl calls = %q, want two LoadState checks", fake.commandLines())
	}
}

func TestNewServeServiceUninstallerSelectsLinuxSystemdBackend(t *testing.T) {
	origOperatingSystem := currentServiceOperatingSystem
	origNewService := serviceNewForUninstaller
	origUnitDir := systemdSystemUnitDir
	origRun := runSystemctlCommand
	currentServiceOperatingSystem = "linux"
	systemdSystemUnitDir = t.TempDir()
	fakeRunner := fakeSystemctl{}
	runSystemctlCommand = fakeRunner.run
	serviceNewForUninstaller = func(service.Interface, *service.Config) (service.Service, error) {
		return fakePlatformService{platform: "linux-systemd"}, nil
	}
	t.Cleanup(func() {
		currentServiceOperatingSystem = origOperatingSystem
		serviceNewForUninstaller = origNewService
		systemdSystemUnitDir = origUnitDir
		runSystemctlCommand = origRun
	})

	backend, err := newServeServiceUninstaller(nil, &service.Config{Name: serveServiceName})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.(*systemdServeUninstaller); !ok {
		t.Fatalf("backend type = %T, want *systemdServeUninstaller", backend)
	}
}

func TestClear_SystemdMissingServeUnitContinuesServerTeardown(t *testing.T) {
	vaultDir, userDir := withClearDirs(t)
	seedClearVault(t, vaultDir)
	if err := os.WriteFile(filepath.Join(vaultDir, "cache-dek.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	prevTTY, prevInstalled, prevDeleteTimer, prevTimerPresent, prevHome :=
		clearStdinIsTTY, serveInstalledFn, deleteLegacyTimerFn, legacyTimerPresentFn, safetyNetHomeDir
	clearStdinIsTTY = func() bool { return true }
	serveInstalledFn = func() bool { return false }
	deleteLegacyTimerFn = func() error { return nil }
	legacyTimerPresentFn = func() bool { return false }
	backupHome := t.TempDir()
	safetyNetHomeDir = func() (string, error) { return backupHome, nil }
	t.Cleanup(func() {
		clearStdinIsTTY, serveInstalledFn, deleteLegacyTimerFn, legacyTimerPresentFn, safetyNetHomeDir =
			prevTTY, prevInstalled, prevDeleteTimer, prevTimerPresent, prevHome
	})

	notFound := systemctlResult{stdout: "LoadState=not-found\n"}
	fake, _ := useSystemdUninstaller(t, notFound, notFound)
	output, err := driveClear(t, "DELETE\ny\n")
	if err != nil {
		t.Fatalf("clear with missing systemd serve unit: %v\n%s", err, output)
	}

	for _, path := range []string{
		filepath.Join(vaultDir, "store.db"),
		filepath.Join(vaultDir, "master.key.plain"),
		filepath.Join(vaultDir, "cache-dek.key"),
		filepath.Join(userDir, "role.json"),
	} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("expected deleted: %s (%v)", path, statErr)
		}
	}
	if backups := backupFilesIn(t, backupHome); len(backups) != 1 {
		t.Fatalf("safety net backup count = %d, want 1", len(backups))
	}
	if got := fake.commandLines(); len(got) != 2 {
		t.Fatalf("systemctl calls = %q, want only two LoadState checks", got)
	}
	if !strings.Contains(output, "已清理") {
		t.Fatalf("output missing completion message:\n%s", output)
	}
}

func TestUninstallServeService_SystemdExistingUnitStopsAndDeletes(t *testing.T) {
	fake, unitDir := useSystemdUninstaller(
		t,
		systemctlResult{},
		systemctlResult{},
		systemctlResult{},
	)
	unitPath := filepath.Join(unitDir, serveServiceName+".service")
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := &strings.Builder{}
	if err := uninstallServeService(output); err != nil {
		t.Fatalf("uninstall existing systemd serve unit: %v", err)
	}

	unit := serveServiceName + ".service"
	wantCommands := []string{
		"stop " + unit,
		"disable " + unit,
		"daemon-reload",
	}
	if got := fake.commandLines(); !equalStrings(got, wantCommands) {
		t.Fatalf("systemctl calls = %q, want %q", got, wantCommands)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unit file status = %v, want not exist", err)
	}
	if !strings.Contains(output.String(), "uninstalled service") {
		t.Fatalf("output missing uninstall message:\n%s", output.String())
	}
}

func TestUninstallServeService_SystemdStopPermissionErrorFails(t *testing.T) {
	fake, unitDir := useSystemdUninstaller(t, systemctlResult{
		exitCode: 1,
		stderr:   "Access denied\n",
		err:      errors.New("exit status 1"),
	})
	unitPath := filepath.Join(unitDir, serveServiceName+".service")
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := &strings.Builder{}
	err := uninstallServeService(output)
	if err == nil {
		t.Fatal("stop permission error was swallowed")
	}
	if !strings.Contains(err.Error(), "stop service") || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("error = %v, want systemd stop permission failure", err)
	}
	if len(fake.calls) != 1 || fake.calls[0][0] != "stop" {
		t.Fatalf("systemctl calls = %q, want only stop", fake.commandLines())
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit file must remain after failed stop: %v", err)
	}
}

func TestUninstallServeService_SystemdDisablePermissionErrorFails(t *testing.T) {
	fake, unitDir := useSystemdUninstaller(t, systemctlResult{
		exitCode: 1,
		stderr:   "Access denied\n",
		err:      errors.New("exit status 1"),
	})
	unitPath := filepath.Join(unitDir, serveServiceName+".service")
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uninstaller := newSystemdServeUninstaller(serveServiceName)
	uninstaller.unitPath = unitPath
	uninstaller.run = runSystemctlCommand

	err := uninstaller.Uninstall()
	if err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("error = %v, want systemd disable permission failure", err)
	}
	if _, statErr := os.Stat(unitPath); statErr != nil {
		t.Fatalf("unit file must remain after failed disable: %v", statErr)
	}
	if len(fake.calls) != 1 || fake.calls[0][0] != "disable" {
		t.Fatalf("systemctl calls = %q, want only disable", fake.commandLines())
	}
}

func TestClearUninstallFailureMentionsAdminOnlyForPermissionDenied(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantAdmin  bool
		wantDetail string
	}{
		{
			name:       "operating system permission error",
			err:        &fs.PathError{Op: "remove", Path: "/etc/systemd/system/sshmgr-serve.service", Err: fs.ErrPermission},
			wantAdmin:  true,
			wantDetail: "请以管理员/root 身份重新运行",
		},
		{
			name:       "systemctl access denied",
			err:        errors.New("systemctl stop sshmgr-serve.service: Access denied"),
			wantAdmin:  true,
			wantDetail: "请以管理员/root 身份重新运行",
		},
		{
			name:       "other systemd failure",
			err:        errors.New("systemctl stop sshmgr-serve.service: unit failed to stop"),
			wantAdmin:  false,
			wantDetail: "已完成步骤会自动跳过",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := clearUninstallFailure(tc.err).Error()
			if strings.Contains(message, "请以管理员/root 身份重新运行") != tc.wantAdmin {
				t.Fatalf("error = %q, admin mention want %v", message, tc.wantAdmin)
			}
			if !strings.Contains(message, tc.wantDetail) {
				t.Fatalf("error = %q, want detail %q", message, tc.wantDetail)
			}
		})
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
