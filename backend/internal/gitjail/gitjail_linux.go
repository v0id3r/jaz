//go:build linux && (amd64 || arm64)

package gitjail

import (
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

const supported = true

// hostSystem is what the helper needs to know about git, found once by Jaz
// before any repository is involved.
type hostSystem struct {
	self string
	// Git is the git binary.
	Git string `json:"g"`
	// Exec are the files and directories git may execute: its exec path and
	// the ELF interpreter the kernel loads for it.
	Exec []string `json:"x"`
	// Shell is /bin/sh, which git needs to push to a local repository.
	Shell string `json:"sh"`
	// Read are the TLS files git's global config names.
	Read []string `json:"r"`
}

// systemPaths git reads besides its own files. /etc stays closed but for
// these, so host config holding secrets is out of reach.
var systemPaths = []string{
	"/usr", "/lib", "/lib64", "/bin",
	"/etc/gitconfig", "/etc/passwd", "/etc/group", "/etc/hosts", "/etc/resolv.conf",
	"/etc/nsswitch.conf", "/etc/host.conf", "/etc/gai.conf", "/etc/ld.so.cache",
	"/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/localtime", "/etc/ssl", "/etc/ca-certificates",
	"/dev/urandom",
}

var system = sync.OnceValues(func() (hostSystem, error) {
	var sys hostSystem
	var err error
	if sys.self, err = executable(os.Executable()); err != nil {
		return sys, err
	}
	if sys.Git, err = executable(exec.LookPath("git")); err != nil {
		return sys, err
	}
	cmd := exec.Command(sys.Git, "--exec-path")
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return sys, fmt.Errorf("git --exec-path: %w", err)
	}
	execPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return sys, err
	}
	sys.Exec = append(sys.Exec, execPath)
	if loader, err := interpreter(sys.Git); err != nil {
		return sys, err
	} else if loader != "" {
		sys.Exec = append(sys.Exec, loader)
	}
	if sys.Shell, err = filepath.EvalSymlinks("/bin/sh"); err != nil {
		return sys, err
	}
	// Global config is Jaz's own and is read here without a repository.
	cmd = exec.Command(sys.Git, "config", "--global", "--get-regexp", `\.ssl(cainfo|capath|cert|key)$`)
	cmd.Dir = "/"
	out, _ = cmd.Output()
	for line := range strings.Lines(string(out)) {
		if _, path, ok := strings.Cut(strings.TrimSpace(line), " "); ok && filepath.IsAbs(path) {
			sys.Read = append(sys.Read, path)
		}
	}
	return sys, nil
})

func executable(path string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// interpreter is the ELF interpreter of a dynamically linked binary. The
// kernel needs execute access to it to start git, so a repository could ask
// git to run it with a file argument; textconv and external diffs, which do
// that, are turned off where Jaz runs diffs.
func interpreter(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(data, 0); err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(strings.TrimRight(string(data), "\x00"))
	}
	return "", nil
}

// ABI reports the Landlock ABI git runs under; 0 means the kernel has no
// Landlock and git is confined by seccomp alone.
func ABI() int {
	abi, err := ll.LandlockGetABIVersion()
	if err != nil {
		return 0
	}
	return abi
}

// The helper runs in a fresh Jaz process: it confines itself and execs git.
func init() {
	if len(os.Args) > 3 && os.Args[1] == helperArg && os.Args[3] == "--" {
		os.Exit(helper(os.Args[2], os.Args[4:]))
	}
}

func helper(payload string, args []string) int {
	var req request
	err := json.Unmarshal([]byte(payload), &req)
	if err == nil {
		err = confine(req)
	}
	if err == nil {
		err = syscall.Exec(req.System.Git, append([]string{"git"}, args...), os.Environ())
	}
	fmt.Fprintln(os.Stderr, "jaz git jail:", err)
	return 126
}

const (
	accessRead  = ll.AccessFSReadFile | ll.AccessFSReadDir
	accessWrite = ll.AccessFSWriteFile | ll.AccessFSTruncate | ll.AccessFSRemoveDir | ll.AccessFSRemoveFile |
		ll.AccessFSMakeDir | ll.AccessFSMakeReg | ll.AccessFSMakeSym | ll.AccessFSRefer
	accessFile = ll.AccessFSReadFile | ll.AccessFSWriteFile | ll.AccessFSTruncate | ll.AccessFSExecute
)

func confine(req request) error {
	var rules []landlock.Rule
	add := func(access landlock.AccessFSSet, paths ...string) {
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				access &= accessFile
			}
			rules = append(rules, landlock.PathAccess(access, path).IgnoreIfMissing())
		}
	}
	add(accessRead, systemPaths...)
	add(accessRead, req.System.Read...)
	add(accessRead, req.Readable...)
	add(accessRead|ll.AccessFSExecute, req.System.Git)
	add(accessRead|ll.AccessFSExecute, req.System.Exec...)
	if req.Shell {
		add(accessRead|ll.AccessFSExecute, req.System.Shell)
	}
	add(accessRead|accessWrite, req.Writable...)
	add(ll.AccessFSReadFile|ll.AccessFSWriteFile, os.DevNull)
	config := landlock.V11.BestEffort()
	if req.Network {
		config = landlock.MustConfig(landlock.V11.HandledAccessFS, landlock.V11.Scoped).BestEffort()
	}
	if err := config.Restrict(rules...); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	return denySockets(req.Network)
}

// denySockets keeps git from opening unix sockets, which Landlock only
// controls from ABI 9, and without network from opening IP sockets at all.
func denySockets(network bool) error {
	families := []uint32{unix.AF_UNIX}
	if !network {
		families = append(families, unix.AF_INET, unix.AF_INET6)
	}
	const (
		load = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		eq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		// x32 syscalls carry this bit under the x86-64 audit arch.
		x32 = 0x40000000
	)
	n := uint8(len(families))
	filter := []unix.SockFilter{
		{Code: load, K: 4}, // seccomp_data.arch
		{Code: eq, K: auditArch, Jf: 5 + n},
		{Code: load, K: 0}, // seccomp_data.nr
		{Code: ge, K: x32, Jt: 3 + n},
		{Code: eq, K: unix.SYS_SOCKET, Jf: 1 + n},
		{Code: load, K: 16}, // low word of the first argument, the family
	}
	for i, family := range families {
		filter = append(filter, unix.SockFilter{Code: eq, K: family, Jt: n - uint8(i)})
	}
	filter = append(filter,
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},
	)
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
