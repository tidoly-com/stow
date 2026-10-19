//go:build unix

package thumbs

import "syscall"

// behind lowers a process's priority, so that the pages someone is looking at go first and the small
// pictures are made with what is left.
func behind(pid int) { syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10) }
