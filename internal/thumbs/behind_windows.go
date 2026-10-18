package thumbs

import "golang.org/x/sys/windows"

// behind lowers a process's priority, so that the pages someone is looking at go first and the small
// pictures are made with what is left.
func behind(pid int) {
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	windows.SetPriorityClass(h, windows.BELOW_NORMAL_PRIORITY_CLASS)
}
