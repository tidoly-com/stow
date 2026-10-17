package helper

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// A helper with nothing to do ends after Idle, giving its memory back, and the next question starts it again.
func TestIdleHelperEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	p := &Process{Name: "echo helper", Command: []string{"sh", "-c", `while read line; do echo '{"ok":true}'; done`}, Idle: 100 * time.Millisecond}
	defer p.Close()
	var reply struct{ OK bool }
	if err := p.Ask(context.Background(), map[string]int{"n": 1}, &reply); err != nil || !reply.OK {
		t.Fatalf("ask: %v %v", reply, err)
	}
	time.Sleep(400 * time.Millisecond)
	p.mu.Lock()
	running := p.cmd != nil
	p.mu.Unlock()
	if running {
		t.Fatal("the helper still runs after being idle")
	}
	if err := p.Ask(context.Background(), map[string]int{"n": 2}, &reply); err != nil || !reply.OK {
		t.Fatalf("ask after rest: %v %v", reply, err)
	}
}
