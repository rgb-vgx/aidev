package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// deletedExeSuffix is what the kernel appends to /proc/<pid>/exe when the file
// it points at was replaced or removed after the process started.
const deletedExeSuffix = " (deleted)"

// staleServers describes each aidev process still running a binary that has
// since been replaced on disk. An MCP server keeps the executable it was
// started from, so a Claude Code session left open across `make install` keeps
// creating tasks with the old checks until it is restarted.
//
// Only Linux exposes the /proc entries this needs; elsewhere the check reports
// errors.ErrUnsupported and doctor says it was skipped rather than claiming all
// is well.
func staleServers() ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.ErrUnsupported
	}
	installed, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(installed); err == nil {
		installed = resolved
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		proc := filepath.Join("/proc", entry.Name())
		if !ownedByCurrentUser(proc) {
			continue
		}
		exe, err := os.Readlink(filepath.Join(proc, "exe"))
		if err != nil {
			// The process ended between the listing and the lookup, or it
			// belongs to something this user may not inspect.
			continue
		}
		if isStaleAidev(exe, installed, filepath.Join(proc, "cmdline")) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)

	stale := make([]string, 0, len(pids))
	for _, pid := range pids {
		stale = append(stale, describeProcess(pid))
	}
	return stale, nil
}

// ownedByCurrentUser reads the owner from /proc/<pid>/status instead of a
// Stat_t, so the same file compiles where /proc does not exist.
func ownedByCurrentUser(proc string) bool {
	body, err := os.ReadFile(filepath.Join(proc, "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return false
		}
		uid, err := strconv.Atoi(fields[0])
		return err == nil && uid == os.Getuid()
	}
	return false
}

// isStaleAidev decides whether a process runs the installed binary that has
// since been replaced, or an aidev at another path (an older install or a
// copy), which argv[0] identifies when the exe link alone cannot.
func isStaleAidev(exe, installed, cmdline string) bool {
	if target, ok := strings.CutSuffix(exe, deletedExeSuffix); ok && target == installed {
		return true
	}
	if exe == installed {
		return false
	}
	body, err := os.ReadFile(cmdline)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(body), "\x00")
	return filepath.Base(first) == "aidev"
}

// describeProcess says when the process started if the kernel tells us, so the
// reader can tell the session they opened this morning from stray ones.
func describeProcess(pid int) string {
	if started, ok := processStart(pid); ok {
		return fmt.Sprintf("pid %d (started %s)", pid, started.Local().Format("2006-01-02 15:04"))
	}
	return fmt.Sprintf("pid %d", pid)
}

// processStart reads field 22 of /proc/<pid>/stat, in USER_HZ ticks since boot,
// and adds it to the boot time from /proc/stat. Linux reports /proc times in
// hundredths of a second regardless of the kernel's CONFIG_HZ.
func processStart(pid int) (time.Time, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, false
	}
	// The comm field is parenthesised and may contain spaces or ')', so the
	// fields after it start at the last ')'.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is field 3 (state); starttime is field 22.
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	btime, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(btime+ticks/100), 0), true
}

// bootTime is the epoch second at which the machine booted.
func bootTime() (uint64, bool) {
	body, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		btime, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		return btime, err == nil
	}
	return 0, false
}
