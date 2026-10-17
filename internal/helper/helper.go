// Package helper runs a separate program the photo server asks things of — the face helper, the coupon
// helper. The program is started on first use and spoken to over its standard input and output, one JSON
// object per line each way. A helper that dies or hangs is ended, and the next question starts a new one.
package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Process struct {
	Name    string        // for messages: "face helper"
	Command []string      // program and arguments
	Idle    time.Duration // ended after this long without a question, to give its memory back; 0: kept running

	mu    sync.Mutex // one question at a time
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	ended chan struct{}
	last  time.Time   // when the last question was answered
	timer *time.Timer // ends an idle program
}

func (p *Process) start() error {
	cmd := exec.Command(p.Command[0], p.Command[1:]...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	p.cmd, p.in, p.out, p.ended = cmd, in, bufio.NewReaderSize(out, 1<<20), make(chan struct{})
	go func(ended chan struct{}) { cmd.Wait(); close(ended) }(p.ended)
	return nil
}

func (p *Process) stop() {
	if p.cmd != nil {
		p.in.Close()
		p.cmd.Process.Kill()
		<-p.ended
		p.cmd = nil
	}
}

// restAfter arranges for the program to end once it has had nothing to do for p.Idle. Called with p.mu held.
func (p *Process) restAfter() {
	if p.Idle <= 0 || p.cmd == nil {
		return
	}
	p.last = time.Now()
	if p.timer == nil {
		p.timer = time.AfterFunc(p.Idle, p.rest)
	} else {
		p.timer.Reset(p.Idle)
	}
}

func (p *Process) rest() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && time.Since(p.last) >= p.Idle {
		p.stop() // the next question starts it again
	}
}

// Close ends the program, if it is running.
func (p *Process) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop()
}

// Ask sends one question and reads the answer into reply. An answer with an "error" field fails the call
// with that error; the program keeps running.
func (p *Process) Ask(ctx context.Context, question, reply any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.restAfter()
	if p.cmd == nil {
		if err := p.start(); err != nil {
			return err
		}
	}
	request, err := json.Marshal(question)
	if err != nil {
		return err
	}
	type answer struct {
		line []byte
		err  error
	}
	got := make(chan answer, 1)
	in, out := p.in, p.out
	go func() {
		if _, err := in.Write(append(request, '\n')); err != nil {
			got <- answer{err: err}
			return
		}
		line, err := out.ReadBytes('\n')
		got <- answer{line, err}
	}()
	var a answer
	select {
	case a = <-got:
	case <-ctx.Done():
		p.stop()
		return ctx.Err()
	case <-time.After(2 * time.Minute):
		p.stop()
		return errors.New(p.Name + " did not answer")
	}
	if a.err != nil {
		p.stop()
		return fmt.Errorf("%s stopped: %w", p.Name, a.err)
	}
	var failed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(a.line, &failed) != nil || json.Unmarshal(a.line, reply) != nil {
		p.stop()
		return fmt.Errorf("%s said something that is not an answer: %.100s", p.Name, a.line)
	}
	if failed.Error != "" {
		return errors.New(p.Name + ": " + failed.Error)
	}
	return nil
}
