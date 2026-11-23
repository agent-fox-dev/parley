//go:build darwin && cgo

package spawn

/*
#include <signal.h>
#include <spawn.h>
#include <stdlib.h>

// posix_spawn_file_actions_addchdir_np is deprecated from macOS 26 for
// posix_spawn_file_actions_addchdir, which the macOS releases before it do
// not have.
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

// parley_spawn starts path with argv and envp, in a process group of its
// own when group is not 0, in dir unless it is empty, with in, out and err
// as its standard input, output and error and no other descriptor of the
// parent, every signal at its default disposition and none blocked.
static int parley_spawn(pid_t *pid, const char *path, char *const argv[], char *const envp[], const char *dir,
	int in, int out, int err, int group) {
	posix_spawn_file_actions_t fa;
	posix_spawnattr_t attr;
	int rc = posix_spawn_file_actions_init(&fa);
	if (rc != 0) return rc;
	rc = posix_spawnattr_init(&attr);
	if (rc != 0) {
		posix_spawn_file_actions_destroy(&fa);
		return rc;
	}
	sigset_t none, all;
	sigemptyset(&none);
	sigfillset(&all);
	sigdelset(&all, SIGKILL);
	sigdelset(&all, SIGSTOP);
	rc = posix_spawn_file_actions_adddup2(&fa, in, 0);
	if (rc == 0) rc = posix_spawn_file_actions_adddup2(&fa, out, 1);
	if (rc == 0) rc = posix_spawn_file_actions_adddup2(&fa, err, 2);
	if (rc == 0 && dir != NULL && dir[0] != 0) rc = posix_spawn_file_actions_addchdir_np(&fa, dir);
	if (rc == 0) rc = posix_spawnattr_setsigmask(&attr, &none);
	if (rc == 0) rc = posix_spawnattr_setsigdefault(&attr, &all);
	short flags = POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF | POSIX_SPAWN_CLOEXEC_DEFAULT;
	if (group) {
		flags |= POSIX_SPAWN_SETPGROUP;
		if (rc == 0) rc = posix_spawnattr_setpgroup(&attr, 0);
	}
	if (rc == 0) rc = posix_spawnattr_setflags(&attr, flags);
	if (rc == 0) rc = posix_spawn(pid, path, &fa, &attr, argv, envp);
	posix_spawnattr_destroy(&attr);
	posix_spawn_file_actions_destroy(&fa);
	return rc;
}
*/
import "C"

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// available says that commands are started by posix_spawn here.
const available = true

// supported reports whether a asks nothing of the process but what spawn
// does: at most a process group of its own.
func supported(a *syscall.SysProcAttr) bool {
	return a == nil || *a == (syscall.SysProcAttr{Setpgid: a.Setpgid})
}

// spawn starts path with argv and env in dir, in a process group of its
// own when group is set, with files as its standard input, output and
// error, and returns its pid.
func spawn(path string, argv, env []string, dir string, files [3]*os.File, group bool) (int, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	cDir := C.CString(dir)
	defer C.free(unsafe.Pointer(cDir))
	cArgv := cStrings(argv)
	defer freeStrings(cArgv)
	cEnv := cStrings(env)
	defer freeStrings(cEnv)
	g := C.int(0)
	if group {
		g = 1
	}
	var pid C.pid_t
	rc := C.parley_spawn(&pid, cPath, &cArgv[0], &cEnv[0], cDir,
		C.int(files[0].Fd()), C.int(files[1].Fd()), C.int(files[2].Fd()), g)
	runtime.KeepAlive(files)
	if rc != 0 {
		return 0, syscall.Errno(rc)
	}
	return int(pid), nil
}

// cStrings is ss as a NULL-terminated array of C strings.
func cStrings(ss []string) []*C.char {
	out := make([]*C.char, len(ss)+1)
	for i, s := range ss {
		out[i] = C.CString(s)
	}
	return out
}

func freeStrings(cs []*C.char) {
	for _, c := range cs {
		if c != nil {
			C.free(unsafe.Pointer(c))
		}
	}
}
