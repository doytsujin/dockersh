package main

import (
	"fmt"
	"github.com/coreos/go-namespaces/namespace"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func nsenterdetect() (found bool, err error) {
	cmd := exec.Command("/usr/local/bin/nsenter")
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	/* TODO: Figure out how to get the actual error code from here */
	if e, ok := err.(*exec.ExitError); ok && strings.HasSuffix(e.String(), "1") {
		return false, nil
	}
	return false, err
}

// from /usr/include/linux/sched.h
const (
	CLONE_VFORK = 0x00004000 /* set if the parent wants the child to wake it up on mm_release */
	SIGCHLD     = 0x14       /* Should set SIGCHLD for fork()-like behavior on Linux */
)

func nsenterexec(pid int, uid int, gid int, wd string, shell string) (err error) {
	// sudo nsenter --target "$PID" --mount --uts --ipc --net --pid --setuid $DESIRED_UID --setgid $DESIRED_GID --wd=$HOMEDIR -- "$REAL_SHELL"
	//cmd := exec.Command("sudo", "/usr/local/bin/nsenter",
	//	"--target", strconv.Itoa(pid), "--mount", "--uts", "--ipc", "--net", "--pid",
	//	"--setuid", strconv.Itoa(uid), "--setgid", strconv.Itoa(gid), fmt.Sprintf("--wd=%s", wd),
	//	"--", shell)
	//cmd.Stdin = os.Stdin
	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr
	//err = cmd.Run()
	//return err

	rootfd, rooterr := os.Open(fmt.Sprintf("/proc/%s/root", strconv.Itoa(pid)))
	if rooterr != nil {
		panic(fmt.Sprintf("Could not open fd to root: %s", rooterr))
	}
	cwdfd, cwderr := os.Open(fmt.Sprintf("/proc/%s/cwd", strconv.Itoa(pid)))
	if cwderr != nil {
		panic("Could not open fd to cwd")
	}

	/* FIXME: Make these an array and loop through them, as this is gross */

	/* --ipc */
	ipcfd, ipcerr := namespace.OpenProcess(pid, namespace.CLONE_NEWIPC)
	if ipcfd == 0 || ipcerr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWIPC)")
	}

	/* --uts */
	utsfd, utserr := namespace.OpenProcess(pid, namespace.CLONE_NEWUTS)
	if utsfd == 0 || utserr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWUTS)")
	}

	/* --net */
	netfd, neterr := namespace.OpenProcess(pid, namespace.CLONE_NEWNET)
	if netfd == 0 || neterr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWNET)")
	}

	/* --pid */
	pidfd, piderr := namespace.OpenProcess(pid, namespace.CLONE_NEWPID)
	if pidfd == 0 || piderr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWPID)")
	}

	/* --mount */
	mountfd, mounterr := namespace.OpenProcess(pid, namespace.CLONE_NEWNS)
	if mountfd == 0 || mounterr != nil {
		panic("namespace.OpenProcess(pid, namespace.CLONE_NEWNS)")
	}

	namespace.Setns(ipcfd, namespace.CLONE_NEWIPC)
	namespace.Setns(utsfd, namespace.CLONE_NEWUTS)
	namespace.Setns(netfd, namespace.CLONE_NEWNET)
	namespace.Setns(pidfd, namespace.CLONE_NEWPID)
	namespace.Setns(mountfd, namespace.CLONE_NEWNS)

	_, _, echrootdir := syscall.Syscall(syscall.SYS_FCHDIR, rootfd.Fd(), 0, 0)
	if echrootdir != 0 {
		panic("chdir to new root failed")
	}
	chrooterr := syscall.Chroot(".")
	if chrooterr != nil {
		panic(fmt.Sprintf("chroot failed: %s", chrooterr))
	}
	// FIXME - this cwds to the cwd of the 'root' process inside the container, we probably want to cwd to user's homedir instead?
	_, _, ecwd := syscall.Syscall(syscall.SYS_FCHDIR, cwdfd.Fd(), 0, 0)
	if ecwd != 0 {
		panic("cwd to working directory failed")
	}

	namespace.Close(ipcfd)
	namespace.Close(utsfd)
	namespace.Close(netfd)
	namespace.Close(pidfd)
	namespace.Close(mountfd)

	/* END FIXME */

	// see go/src/pkg/syscall/exec_unix.go
	syscall.ForkLock.Lock()

	// Stolen from https://github.com/tobert/lnxns/blob/master/src/lnxns/nsfork_linux.go
	var flags int = SIGCHLD | CLONE_VFORK
	r1, _, err1 := syscall.RawSyscall(syscall.SYS_CLONE, uintptr(flags), 0, 0)

	syscall.ForkLock.Unlock()

	if err1 == syscall.EINVAL {
		panic("OS returned EINVAL. Make sure your kernel configuration includes all CONFIG_*_NS options.")
	} else if err1 != 0 {
		panic(err1)
	}

	// parent will get the pid, child will be 0
	if int(r1) != 0 {
		// Parent
		fmt.Fprintf(os.Stderr, "In Parent waiting for %s\n", strconv.Itoa(int(pid)))
		proc, procerr := os.FindProcess(int(pid))
		if procerr != nil {
			fmt.Fprintf(os.Stderr, "Failed waiting for child: %s\n", strconv.Itoa(int(pid)))
			panic(procerr)
		}
		pstate, err := proc.Wait()
		if err != nil {
			panic(fmt.Sprintf("proc.Wait failed %s", err))
		}
		fmt.Fprintf(os.Stderr, "parent wait finished\n")
		if pstate.Exited() {
			fmt.Fprintf(os.Stderr, "Child has exited\n")
		} else {
			fmt.Fprintf(os.Stderr, "Child has NOT exited\n")
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "In child\n")
	// Child

	if gid > 0 {
		err = syscall.Setgroups([]int{}) /* drop supplementary groups */
		if err != nil {
			panic("setgroups failed")
		}
		err = syscall.Setgid(gid)
		if err != nil {
			panic("setgid failed")
		}
	}
	if uid > 0 {
		err = syscall.Setuid(uid)
		if err != nil {
			panic("setuid failed")
		}
	}

	args := []string{shell}
	env := os.Environ()
	fmt.Fprintf(os.Stderr, "Child exec\n")
	execErr := syscall.Exec(shell, args, env)
	if execErr != nil {
		panic(execErr)
	}
	fmt.Fprintf(os.Stderr, "Exec error\n")
	return execErr
}
