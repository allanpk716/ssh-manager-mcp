package cli

import (
	"path/filepath"
	"strings"
)

// serveCommandLineHasServeSubcommand reports whether commandLine is a sshmgr
// invocation whose first non-option argument after the program name is serve.
// A long option without an embedded value is treated as consuming the next
// argument, matching the value-taking global options that may precede a Cobra
// subcommand.
func serveCommandLineHasServeSubcommand(commandLine []string) bool {
	if len(commandLine) < 2 {
		return false
	}
	programName := filepath.Base(commandLine[0])
	if position := strings.LastIndexAny(programName, `/\`); position >= 0 {
		programName = programName[position+1:]
	}
	if !strings.EqualFold(programName, "sshmgr") && !strings.EqualFold(programName, "sshmgr.exe") {
		return false
	}

	terminatorSeen := false
	optionExpectsValue := false
	for _, argument := range commandLine[1:] {
		if optionExpectsValue {
			optionExpectsValue = false
			continue
		}
		if terminatorSeen {
			return argument == "serve"
		}
		if argument == "--" {
			terminatorSeen = true
			continue
		}
		if len(argument) > 1 && argument[0] == '-' {
			if !strings.HasPrefix(argument, "--") || strings.Contains(argument, "=") {
				continue
			}
			optionExpectsValue = true
			continue
		}
		return argument == "serve"
	}
	return false
}

// windowsServeServiceProcessRunning is the testable Windows decision seam: a
// service process number is live only when both the service query and the
// process query succeed.
func windowsServeServiceProcessRunning(
	queryProcessID func() (uint32, error), processIsAlive func(uint32) bool,
) bool {
	processID, err := queryProcessID()
	return err == nil && processID != 0 && processIsAlive(processID)
}
