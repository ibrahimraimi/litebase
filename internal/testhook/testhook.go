package testhook

import (
	"os"
	"sync"
	"syscall"
)

const FailpointEnvVar = "LITEBASE_FAILPOINT"

var (
	mu    sync.RWMutex
	hooks = make(map[string]func())
)

func Set(name string, fn func()) {
	mu.Lock()
	defer mu.Unlock()
	if fn == nil {
		delete(hooks, name)
		return
	}
	hooks[name] = fn
}

func At(name string) {
	if os.Getenv(FailpointEnvVar) == name {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		os.Exit(137)
	}
	mu.RLock()
	fn := hooks[name]
	mu.RUnlock()
	if fn != nil {
		fn()
	}
}
