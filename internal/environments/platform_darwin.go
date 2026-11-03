package environments

import "syscall"

// unameFacts is platform.system(), platform.release(), platform.machine().
func unameFacts() (system, release, machine string) {
	system, _ = syscall.Sysctl("kern.ostype")
	release, _ = syscall.Sysctl("kern.osrelease")
	machine, _ = syscall.Sysctl("hw.machine")
	return system, release, machine
}
