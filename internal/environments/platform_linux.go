package environments

import "syscall"

func cstr[T int8 | uint8](b []T) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// unameFacts is platform.system(), platform.release(), platform.machine().
func unameFacts() (system, release, machine string) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "Linux", "", ""
	}
	return cstr(u.Sysname[:]), cstr(u.Release[:]), cstr(u.Machine[:])
}
