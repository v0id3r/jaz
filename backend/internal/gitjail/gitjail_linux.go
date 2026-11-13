//go:build linux && (amd64 || arm64)

package gitjail

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

const supported = true

// hostSystem is what the helper needs to know about git, found once by Jaz
// before any repository is involved.
type hostSystem struct {
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

// start confines a fresh thread and starts git from it, so git inherits the
// thread's Landlock domain and seccomp filter. The thread stays locked: the
// runtime ends it with the goroutine instead of reusing it, and threads it
// would spawn from here come from its clean template thread. The main thread
// is never confined: the runtime cannot end it and would park it for good.
func (c *Cmd) start() error {
	errc := make(chan error, 1)
	go c.startOnOwnThread(errc)
	return <-errc
}

func (c *Cmd) startOnOwnThread(errc chan<- error) {
	runtime.LockOSThread()
	if unix.Gettid() == unix.Getpid() {
		// Holding the main thread keeps the retry off it.
		errc <- c.start()
		runtime.UnlockOSThread()
		return
	}
	if err := confine(c.policy, c.system); err != nil {
		errc <- fmt.Errorf("jaz git jail: %w", err)
		return
	}
	errc <- c.Cmd.Start()
}

const (
	accessRead  = ll.AccessFSReadFile | ll.AccessFSReadDir
	accessWrite = ll.AccessFSWriteFile | ll.AccessFSTruncate | ll.AccessFSRemoveDir | ll.AccessFSRemoveFile |
		ll.AccessFSMakeDir | ll.AccessFSMakeReg | ll.AccessFSMakeSym | ll.AccessFSRefer
	accessFile = ll.AccessFSReadFile | ll.AccessFSWriteFile | ll.AccessFSTruncate | ll.AccessFSExecute | ll.AccessFSIoctlDev
)

// handled are the Landlock rights each ABI adds; the jail handles all the
// kernel knows, so whatever no rule grants is denied.
func handled(abi int, network bool) ll.RulesetAttr {
	var attr ll.RulesetAttr
	if abi < 1 {
		return attr
	}
	attr.HandledAccessFS = ll.AccessFSExecute | ll.AccessFSWriteFile | ll.AccessFSReadFile | ll.AccessFSReadDir |
		ll.AccessFSRemoveDir | ll.AccessFSRemoveFile | ll.AccessFSMakeChar | ll.AccessFSMakeDir | ll.AccessFSMakeReg |
		ll.AccessFSMakeSock | ll.AccessFSMakeFifo | ll.AccessFSMakeBlock | ll.AccessFSMakeSym
	for _, step := range []struct {
		abi    int
		fs     uint64
		net    uint64
		scoped uint64
	}{
		{abi: 2, fs: ll.AccessFSRefer},
		{abi: 3, fs: ll.AccessFSTruncate},
		{abi: 4, net: ll.AccessNetBindTCP | ll.AccessNetConnectTCP},
		{abi: 5, fs: ll.AccessFSIoctlDev},
		{abi: 6, scoped: ll.ScopeAbstractUnixSocket | ll.ScopeSignal},
		{abi: 9, fs: ll.AccessFSResolveUnix},
		{abi: 10, net: ll.AccessNetBindUDP | ll.AccessNetConnectSendUDP},
	} {
		if abi >= step.abi {
			attr.HandledAccessFS |= step.fs
			attr.HandledAccessNet |= step.net
			attr.Scoped |= step.scoped
		}
	}
	if network {
		attr.HandledAccessNet = 0
	}
	return attr
}

// confine restricts the calling thread, which must be locked to its goroutine
// and never run anything else.
func confine(policy Policy, sys hostSystem) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if attr := handled(ABI(), policy.Network); attr.HandledAccessFS != 0 {
		if err := restrict(attr, policy, sys); err != nil {
			return fmt.Errorf("landlock: %w", err)
		}
	}
	return denySockets(policy.Network)
}

func restrict(attr ll.RulesetAttr, policy Policy, sys hostSystem) error {
	ruleset, err := ll.LandlockCreateRuleset(&attr, 0)
	if err != nil {
		return err
	}
	defer unix.Close(ruleset)
	add := func(access uint64, paths ...string) error {
		for _, path := range paths {
			fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			allowed := access & attr.HandledAccessFS
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
				allowed &= accessFile
			}
			err = ll.LandlockAddPathBeneathRule(ruleset, &ll.PathBeneathAttr{AllowedAccess: allowed, ParentFd: fd}, 0)
			unix.Close(fd)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
		return nil
	}
	var systemAccess uint64 = accessRead
	if policy.Shell {
		systemAccess |= ll.AccessFSExecute
	}
	for _, rule := range []struct {
		access uint64
		paths  []string
	}{
		{systemAccess, systemPaths},
		{accessRead, sys.Read},
		{accessRead, policy.Readable},
		{accessRead | ll.AccessFSExecute, []string{sys.Git}},
		{accessRead | ll.AccessFSExecute, sys.Exec},
		{accessRead | ll.AccessFSExecute, shell(policy, sys)},
		{accessRead | accessWrite, policy.Writable},
		{ll.AccessFSReadFile | ll.AccessFSWriteFile, []string{os.DevNull}},
	} {
		if err := add(rule.access, rule.paths...); err != nil {
			return err
		}
	}
	return ll.LandlockRestrictSelf(ruleset, 0)
}

func shell(policy Policy, sys hostSystem) []string {
	if policy.Shell {
		return []string{sys.Shell}
	}
	return nil
}

// denySockets keeps git from opening unix sockets, which Landlock only
// controls from ABI 9, and without network from opening IP sockets at all.
// The filter is the calling thread's alone.
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
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp: %w", errno)
	}
	return nil
}
