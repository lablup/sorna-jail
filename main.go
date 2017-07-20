// This ptrace-based jail assumes linux/amd64 platforms.

/*
Command-line usage:
	./jail <child_args ...>  // use default policy
	./jail -policy <policy_file> <child_args ...>

Example:
	./jail -policy python3.yml /bin/sh /home/sorna/run.sh
*/
package main

import (
	"flag"
	"fmt"
	"github.com/fatih/color"
	"github.com/lablup/sorna-jail/policy"
	"github.com/lablup/sorna-jail/utils"
	seccomp "github.com/seccomp/libseccomp-golang"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
)

type Exit struct{ Code int }

var (
	myExecPath, _  = utils.GetExecutable(os.Getpid())
	myPath         = filepath.Dir(myExecPath)
	intraJailPath  = myExecPath
	arch, _        = seccomp.GetNativeArch()
	id_Open, _     = seccomp.GetSyscallFromNameByArch("open", arch)
	id_Access, _   = seccomp.GetSyscallFromNameByArch("access", arch)
	id_Clone, _    = seccomp.GetSyscallFromNameByArch("clone", arch)
	id_Fork, _     = seccomp.GetSyscallFromNameByArch("fork", arch)
	id_Vfork, _    = seccomp.GetSyscallFromNameByArch("vfork", arch)
	id_Execve, _   = seccomp.GetSyscallFromNameByArch("execve", arch)
	id_Kill, _     = seccomp.GetSyscallFromNameByArch("kill", arch)
	id_Killpg, _   = seccomp.GetSyscallFromNameByArch("killpg", arch)
	id_Tkill, _    = seccomp.GetSyscallFromNameByArch("tkill", arch)
	id_Tgkill, _   = seccomp.GetSyscallFromNameByArch("tgkill", arch)
	id_Chmod, _    = seccomp.GetSyscallFromNameByArch("chmod", arch)
	id_Fchmodat, _ = seccomp.GetSyscallFromNameByArch("fchmodat", arch)
)

var debug bool = false
var watch bool = false
var policyInst policy.SandboxPolicy
var policyFile string
var childMode bool = false
var execCount int = 0
var forkCount int = 0
var childCount uint = 1
var maxChildCount uint = 0

// Ref: https://github.com/torvalds/linux/blob/master/include/uapi/linux/ptrace.h
const PTRACE_SEIZE uintptr = 0x4206     /* Linux >= 3.4 */
const PTRACE_INTERRUPT uintptr = 0x4207 /* Linux >= 3.4 */
const PTRACE_LISTEN uintptr = 0x4208    /* Linux >= 3.4 */
const PTRACE_EVENT_SECCOMP uint = 7
const PTRACE_EVENT_STOP uint = 128

const ourPtraceOpts int = (1 << PTRACE_EVENT_SECCOMP /*PTRACE_O_TRACESECCOMP*/) |
	(1 << 20 /*PTRACE_O_EXITKILL, Linux >= 3.4 */) |
	syscall.PTRACE_O_TRACECLONE |
	syscall.PTRACE_O_TRACEFORK |
	syscall.PTRACE_O_TRACEVFORK

func ptraceSeize(pid int, opts int) (uintptr, error) {
	ret, _, err := syscall.Syscall6(syscall.SYS_PTRACE, PTRACE_SEIZE, uintptr(pid), 0, uintptr(opts), 0, 0)
	return ret, err
}

func ptraceInterrupt(pid int) (uintptr, error) {
	ret, _, err := syscall.Syscall6(syscall.SYS_PTRACE, PTRACE_INTERRUPT, uintptr(pid), 0, 0, 0, 0)
	return ret, err
}

func ptraceListen(pid int, sig int) (uintptr, error) {
	ret, _, err := syscall.Syscall6(syscall.SYS_PTRACE, PTRACE_LISTEN, uintptr(pid), 0, uintptr(sig), 0, 0)
	return ret, err
}

type WaitResult struct {
	pid    int
	err    error
	status syscall.WaitStatus
}

func waitChildStop(pid int) syscall.WaitStatus {
	var status syscall.WaitStatus
	for {
		p, err := syscall.Wait4(pid, &status, syscall.WSTOPPED, nil)
		if err == syscall.EINTR || p < 0 {
			continue
		} else {
			break
		}
	}
	return status
}

func traceProcess(l *log.Logger, pid int) {

	mySignals := make(chan os.Signal)
	childrenWaits := make(chan WaitResult)

	signal.Notify(mySignals, os.Interrupt, syscall.SIGTERM)
	signal.Ignore(syscall.SIGSTOP)
	signal.Ignore(syscall.SIGTTOU)
	signal.Ignore(syscall.SIGTTIN)
	signal.Ignore(syscall.SIGTSTP)

	// Child is first-stopped.
	status := waitChildStop(pid)
	if !status.Stopped() || status.StopSignal() != syscall.SIGSTOP {
		color.Set(color.FgRed)
		l.Printf("Unexpected wait status 0x%x", uint(status))
		color.Unset()
		return
	}

	ret, seizeErr := ptraceSeize(pid, ourPtraceOpts)
	if ret != 0 {
		color.Set(color.FgRed)
		l.Printf("ptraceSeize error: %d\n", seizeErr)
		color.Unset()
		return
	}
	if debug {
		color.Set(color.FgBlue)
		l.Printf("attached child %d\n", pid)
		color.Unset()
	}

	syscall.Kill(pid, syscall.SIGCONT)

	go func() {
		for {
			var status syscall.WaitStatus
			traceePid, err := syscall.Wait4(-1, &status, syscall.WALL, nil)
			if err != nil {
				switch err.(syscall.Errno) {
				case syscall.EINTR:
					// Retry the wait system call.
					continue
				case syscall.ECHILD:
					// No child processes found. Terminate.
					break
				default:
					color.Set(color.FgCyan)
					l.Printf("unexpected errno %s", err)
					color.Unset()
					break
				}
			}
			childrenWaits <- WaitResult{int(traceePid), err, status}
			if status.Exited() && traceePid == pid {
				break
			}
		}
		if debug {
			color.Set(color.FgBlue)
			l.Printf("monitoring goroutine terminating.")
			color.Unset()
		}
	}()

loop:
	for {
		select {
		case mysig := <-mySignals:

			switch mysig {
			case os.Interrupt, syscall.SIGTERM:
				// Terminate all my children.
				// Since we set Setsid: true in SysProcAttr of syscall.ForkExec(),
				// the signals we receive are NOT automatically delivered to children.
				// We control the SIGINT/SIGTERM behaviour gracefully for later
				// extension of Sorna.
				pgid, _ := syscall.Getpgid(pid)
				syscall.Kill(pgid, syscall.SIGKILL)

				var status syscall.WaitStatus
				syscall.Wait4(pid, &status, syscall.WALL, nil)
				break loop
			}

		case result := <-childrenWaits:

			var signalToChild syscall.Signal = 0

			if result.status.Exited() {
				if debug {
					color.Set(color.FgBlue)
					l.Printf("EXIT (pid %d) status %d\n", result.pid, result.status.ExitStatus())
					color.Unset()
				}
				if pid == result.pid {
					if debug {
						color.Set(color.FgBlue)
						l.Printf("Our very child has exited. Done.")
						color.Unset()
					}
					if watch {
						color.Set(color.FgBlue)
						l.Printf("Max child count: %d.", maxChildCount)
						color.Unset()
					}
					break loop
				} else if result.pid == -1 {
					if debug {
						color.Set(color.FgRed)
						l.Printf("waitpid error: %s (exit status %d). Terminating.", result.err, result.status.ExitStatus())
						color.Unset()
					}
					break loop
				} else {
					// If we attach grand-children processes, this may be the case.
					childCount--
					if debug {
						color.Set(color.FgBlue)
						l.Printf("childCount is now %d\n", childCount)
						color.Unset()
					}
				}
			}

			if result.status.Signaled() {
				continue loop
			}

			if !result.status.Stopped() {
				continue loop
			}

			// Okay, we now have to deal with tracing stops.

			stopsig := result.status.StopSignal()

			if debug {
				color.Set(color.FgYellow)
				l.Printf("Received signal: 0x%x (%d) \"%s\"", uint(stopsig), uint(stopsig), stopsig)
				color.Unset()
			}

			childStopped := false
			event := uint(result.status) >> 16

			switch event {
			case 0:
				// pass
			case PTRACE_EVENT_STOP:
				switch stopsig {
				case syscall.SIGSTOP, syscall.SIGTSTP, syscall.SIGTTOU, syscall.SIGTTIN:
					childStopped = true
					if debug {
						color.Set(color.FgYellow)
						l.Printf("group-stop detected")
						color.Unset()
					}
				}
			default:
				// pass
			}

			switch stopsig {
			case syscall.SIGSTOP:
				// pass
			case syscall.SIGTRAP:
				eventCause := ((uint(result.status) >> 8) & (^uint(syscall.SIGTRAP))) >> 8
				if debug {
					color.Set(color.FgYellow)
					l.Printf("event-cause: %d\n", eventCause)
					color.Unset()
				}

				switch eventCause {
				case PTRACE_EVENT_SECCOMP:
					var extraInfo string = ""
					allow := true
					// Linux syscall convention for x86_64 arch:
					//  - rax: syscall number
					//  - rdi: 1st param
					//  - rsi: 2nd param
					//  - rdx: 3rd param
					//  - r10: 4th param
					//  - r8: 5th param
					//  - r9: 6th param
					var regs syscall.PtraceRegs
					for {
						err := syscall.PtraceGetRegs(result.pid, &regs)
						if err != nil {
							errno := err.(syscall.Errno)
							if errno == syscall.EBUSY || errno == syscall.EFAULT || errno == syscall.ESRCH {
								continue
							}
						}
						break
					}
					syscallId := uint(regs.Orig_rax)
					if debug {
						color.Set(color.FgYellow)
						sn, _ := seccomp.ScmpSyscall(syscallId).GetName()
						l.Printf("seccomp trap (%d %s)", syscallId, sn)
						color.Unset()
					}
					switch seccomp.ScmpSyscall(syscallId) {
					case id_Fork, id_Vfork, id_Clone:
						execPath, _ := utils.GetExecutable(result.pid)
						if execPath == myExecPath {
							allow = true
						} else if execPath == intraJailPath {
							allow = true
						} else {
							maxForks := policyInst.GetForkAllowance()
							allow = (maxForks == -1 || forkCount < maxForks)
							forkCount++
						}
						maxCount := policyInst.GetMaxChildProcs()
						allow = allow && (maxCount == -1 || childCount < maxCount)
						if debug {
							l.Printf("fork owner: %s\n", execPath)
						}
					case id_Tgkill:
						targetTgid := int(regs.Rdi)
						targetTid := int(regs.Rsi)
						signum := syscall.Signal(uint(regs.Rdx))
						switch signum {
						case syscall.SIGKILL, syscall.SIGINT, syscall.SIGTERM:
							allow = (targetTgid != os.Getpid() &&
								targetTid != pid &&
								targetTid != os.Getpid() &&
								!(targetTid == 0 && result.pid == pid) &&
								targetTid != 1)
						default:
							allow = true
						}
					case id_Kill, id_Killpg, id_Tkill:
						targetPid := int(regs.Rdi)
						signum := syscall.Signal(uint(regs.Rsi))
						switch signum {
						case syscall.SIGKILL, syscall.SIGINT, syscall.SIGTERM:
							allow = (targetPid != pid &&
								targetPid != os.Getpid() &&
								!(targetPid == 0 && result.pid == pid) &&
								targetPid != 1)
						default:
							allow = true
						}
					case id_Execve:
						execPath, _ := utils.GetExecutable(result.pid)
						if execPath == myExecPath {
							allow = true
						} else if execPath == intraJailPath {
							allow = true
						} else if policyInst.CheckPathOp(execPath, policy.OP_EXEC, 0) {
							allow = true
						} else {
							maxExec := policyInst.GetExecAllowance()
							allow = (maxExec == -1 || execCount < maxExec)
							execCount++
						}
						extraInfo = fmt.Sprintf("execve from %s", execPath)
					case id_Open:
						pathPtr := uintptr(regs.Rdi)
						path := utils.ReadString(result.pid, pathPtr)
						path = utils.GetAbsPathAs(path, result.pid)
						// rsi is flags
						mode := int(regs.Rdx)
						allow = policyInst.CheckPathOp(path, policy.OP_OPEN, mode)
						extraInfo = path
					case id_Access:
						pathPtr := uintptr(regs.Rdi)
						path := utils.ReadString(result.pid, pathPtr)
						path = utils.GetAbsPathAs(path, result.pid)
						mode := int(regs.Rsi)
						allow = policyInst.CheckPathOp(path, policy.OP_ACCESS, mode)
						extraInfo = path
					case id_Fchmodat:
						pathPtr := uintptr(regs.Rsi)
						path := utils.ReadString(result.pid, pathPtr)
						path = utils.GetAbsPathAs(path, result.pid)
						mode := int(regs.Rdx)
						allow = policyInst.CheckPathOp(path, policy.OP_CHMOD, mode)
						extraInfo = fmt.Sprintf("%s 0o%o", path, mode)
					case id_Chmod:
						pathPtr := uintptr(regs.Rdi)
						path := utils.ReadString(result.pid, pathPtr)
						path = utils.GetAbsPathAs(path, result.pid)
						mode := int(regs.Rsi)
						allow = policyInst.CheckPathOp(path, policy.OP_CHMOD, mode)
						extraInfo = fmt.Sprintf("%s 0o%o", path, mode)
					default:
						allow = true
					}
					if !allow {
						if debug || watch {
							syscallName, _ := seccomp.ScmpSyscall(syscallId).GetName()
							color.Set(color.FgRed)
							if extraInfo != "" {
								l.Printf("blocked syscall %s (%s)", syscallName, extraInfo)
							} else {
								l.Printf("blocked syscall %s", syscallName)
							}
							color.Unset()
						}
						// If we are not in the watch mode...
						if !watch {
							// Block the system call with permission error
							regs.Orig_rax = 0xFFFFFFFFFFFFFFFF // -1
							regs.Rax = 0xFFFFFFFFFFFFFFFF - uint64(syscall.EPERM) + 1
							syscall.PtraceSetRegs(result.pid, &regs)
						}
					} else {
						if debug {
							syscallName, _ := seccomp.ScmpSyscall(syscallId).GetName()
							color.Set(color.FgGreen)
							if extraInfo != "" {
								l.Printf("allowed syscall %s (%s)", syscallName, extraInfo)
							} else {
								l.Printf("allowed syscall %s", syscallName)
							}
							color.Unset()
						}
					}
				case syscall.PTRACE_EVENT_CLONE,
					syscall.PTRACE_EVENT_FORK,
					syscall.PTRACE_EVENT_VFORK:
					childPid, _ := syscall.PtraceGetEventMsg(result.pid)
					ptraceSeize(int(childPid), ourPtraceOpts)
					childCount++
					if maxChildCount < childCount {
						maxChildCount = childCount
					}
					if debug {
						color.Set(color.FgBlue)
						l.Printf("Attached to new child %d\n", childPid)
						l.Printf("childCount is now %d\n", childCount)
						color.Unset()
					}
				case PTRACE_EVENT_STOP:
					// already processed above
				case 0:
					// ignore
				default:
					if debug || watch {
						color.Set(color.FgRed)
						l.Printf("Unknown trap cause: %d\n", result.status.TrapCause())
						color.Unset()
					}
				}
			//case syscall.SIGCHLD:
			// SIGCHLD is not a reliable method to determine grand-children exits,
			// because multiple signals generated in a short period time may be merged
			// into a single one.
			// Instead, we use TRACE_FORK ptrace options and attaching grand-children
			// processes manually.
			default:
				// Transparently deliver other signals.
				if !childStopped {
					signalToChild = stopsig
					if debug {
						color.Set(color.FgCyan)
						l.Printf("Injecting unhandled signal: %s", signalToChild)
						color.Unset()
					}
				}
			}

			var err error
			if childStopped && stopsig != syscall.SIGTRAP {
				// may be a group-stop; we need to keep the child stopped.
				if debug {
					color.Set(color.FgYellow)
					l.Printf("ptrace-listen")
					color.Unset()
				}
				_, err = ptraceListen(result.pid, 0)
			} else {
				if debug {
					color.Set(color.FgYellow)
					l.Printf("ptrace-cont")
					color.Unset()
				}
				err = syscall.PtraceCont(result.pid, int(signalToChild))
			}
			if err != nil && err.(syscall.Errno) != 0 {
				color.Set(color.FgRed)
				l.Printf("ptrace-continue error %s", err)
				color.Unset()
				errno := err.(syscall.Errno)
				if errno == syscall.EBUSY || errno == syscall.EFAULT || errno == syscall.ESRCH {
					break loop
				}
			}
		} /* endselect */
	} /* endloop */
}

func init() {
	flag.BoolVar(&childMode, "child-mode", false, "Used to run the child mode to initiate tracing.")
	flag.StringVar(&policyFile, "policy", "[default]", "Path to policy config file. If set \"[default]\", it uses the embedded default policy.")
	flag.BoolVar(&debug, "debug", false, "Set the debug mode. Shows every detail of syscalls.")
	flag.BoolVar(&watch, "watch", false, "Set the watch mode. Shows syscalls blocked by the policy.")
}

func handleExit() {
	if e := recover(); e != nil {
		color.Unset()
		// When log.Panic is used, recover() returns the printed string.
		if _, ok := e.(string); ok == true {
			os.Exit(1)
		}
		// When panic(Exit{N}) is used.
		if exit, ok := e.(Exit); ok == true {
			os.Exit(exit.Code)
		}
		// Otherwise bubble up.
		panic(e)
	}
}

func main() {
	var err error
	defer handleExit()

	l := log.New(os.Stderr, "", 0)
	flag.Parse()

	if debug {
		watch = false
		color.Set(color.FgYellow)
		l.Printf("DEBUG MODE: showing all details")
		color.Unset()
	}
	if watch {
		color.Set(color.FgYellow)
		l.Printf("WATCH MODE: all syscalls are ALLOWED but it shows which ones will be blocked by the current policy.")
		color.Unset()
	}

	if !childMode {
		/* The parent. */

		if flag.NArg() < 1 {
			color.Set(color.FgRed)
			l.Panic("Main: Not enough command-line arguments. See the docs.")
		}

		policyInst, err = policy.GeneratePolicyFromYAML(l, policyFile)
		if err != nil {
			color.Set(color.FgRed)
			l.Panic("GeneratePolicy: ", err)
		}

		/* Initialize fork/exec of the child. */
		runtime.GOMAXPROCS(1)
		runtime.LockOSThread()
		// Locking the OS thread is required to let syscall.Wait4() work correctly
		// because waitpid() only monitors the caller's direct children, not
		// siblings' children.
		args := append([]string{
			intraJailPath,
			"-child-mode",
			"-policy",
			policyInst.(policy.FileBasedPolicy).FileName,
		}, flag.Args()[0:]...)
		cwd, _ := os.Getwd()
		envs := utils.FilterEnvs(os.Environ(), policyInst.GetPreservedEnvKeys())
		envs = append(envs, policyInst.GetExtraEnvs()...)
		if debug {
			color.Set(color.FgBlue)
			l.Printf("Environment:")
			for _, e := range envs {
				l.Println(e)
			}
			color.Unset()
		}

		var pid int
		pid, err = syscall.ForkExec(args[0], args, &syscall.ProcAttr{
			cwd,
			envs,
			[]uintptr{0, 1, 2},
			&syscall.SysProcAttr{
				Ptrace: false, // should be disabled when using ptraceSeize
			},
		})
		if err != nil {
			color.Set(color.FgRed)
			l.Panicf("ForkExec(\"%s\"): %s", args[0], err)
		}
		traceProcess(l, pid)

	} else {
		/* The child. */

		policyInst, err = policy.GeneratePolicyFromYAML(l, policyFile)
		if err != nil {
			color.Set(color.FgRed)
			l.Panic("GeneratePolicy: ", err)
		}

		// Deleting policy file may be the simplest way to deny further
		// access to the policy by the child process. To preserve
		// policy file in development phase, we delete policy files in
		// /home/sorna/ directory only (since runtime image will place
		// their policy files to the dir). Of course, this is not a
		// general method, and there may be a need in the future to
		// deny access in the basis of syscall filter (open). This will
		// make tracing loop a little more complex.
		var abspath, dir string
		if filepath.IsAbs(policyFile) {
			abspath = policyFile
		} else {
			cwd, _ := os.Getwd()
			abspath = filepath.Join(cwd, policyFile)
		}
		dir, _ = filepath.Split(abspath)
		if dir == "/home/sorna/" {
			os.Remove(abspath)
		}

		syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PTRACER, uintptr(os.Getppid()), 0)

		arch, _ := seccomp.GetNativeArch()
		laterFilter, _ := seccomp.NewFilter(seccomp.ActErrno.SetReturnCode(int16(syscall.EPERM)))
		for _, syscallName := range policyInst.GetAllowedSyscalls() {
			syscallId, _ := seccomp.GetSyscallFromNameByArch(syscallName, arch)
			laterFilter.AddRuleExact(syscallId, seccomp.ActAllow)
		}
		for _, syscallName := range policyInst.GetTracedSyscalls() {
			syscallId, _ := seccomp.GetSyscallFromNameByArch(syscallName, arch)
			laterFilter.AddRuleExact(syscallId, seccomp.ActTrace)
		}
		killSyscalls := []string{"kill", "killpg", "tkill", "tgkill"}
		for _, syscallName := range killSyscalls {
			scId, _ := seccomp.GetSyscallFromNameByArch(syscallName, arch)
			laterFilter.AddRuleExact(scId, seccomp.ActTrace)
		}
		laterFilter.SetNoNewPrivsBit(true)

		// Inform the parent that I'm ready to continue.
		// Any code before this line code must use only non-traced system calls in
		// the filter because the tracer has not set up itself yet.
		// (traced syscalls will cause ENOSYS "function not implemented" error)
		syscall.Kill(os.Getpid(), syscall.SIGSTOP)

		// Now we have the working tracer parent.
		// Make kill() syscall to be traced as well for more sophisticated filtering.
		err := laterFilter.Load()
		if err != nil {
			color.Set(color.FgRed)
			l.Panic("ScmpFilter.Load (2): ", err)
		}
		laterFilter.Release()

		// NOTE: signal.Reset() here causes race conditions with the tracer.
		// (syscall tracing doesn't work deterministically with it.)

		// Replace myself with the language runtime.
		err = syscall.Exec(flag.Arg(0), flag.Args()[0:], os.Environ())

		// NOTE: "function not implemented" errors here may be due to above codes.
		color.Set(color.FgRed)
		l.Panicf("Exec(\"%s\"): %s\nNOTE: You need to provide the absolute path.", flag.Arg(0), err)

	}
}
