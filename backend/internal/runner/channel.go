package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/shota-urano/evaloom/backend/internal/contract"
	"github.com/shota-urano/evaloom/backend/internal/model"
	"github.com/shota-urano/evaloom/backend/internal/sandbox"
)

const channelLineLimit = 64 << 10 // spec 05, 3.7 (excluding the newline)

type stageChannel struct {
	runner      *Runner
	executionID string
	stage       model.Stage
	listener    *channelListener
	log         *Log
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	closed      bool
	workers     sync.WaitGroup
}

// A stage executor returns only after the entire sandbox has stopped. Keep the
// listener alive until then, including while heartbeat cancellation is being
// handled, so cancel_check can observe the instruction rather than ctx.Err().
func (runner *Runner) runStageWithChannel(ctx context.Context, id string, in StageInvocation, executor StageExecutor) StageOutcome {
	if in.Stage == model.StageWork {
		return executor.RunStage(ctx, in)
	}
	channel, err := runner.openStageChannel(ctx, id, in.Stage, filepath.Join(in.Directory, "channel.sock"))
	if err != nil {
		code := "sandbox_error"
		return StageOutcome{RunnerError: &code}
	}
	defer channel.close()
	in.ChannelPath = channel.listener.Addr().String()
	ctx = sandbox.WithStageChannel(ctx, sandbox.SandboxKind(in.Stage), channel.listener)
	outcome := executor.RunStage(ctx, in)
	channel.close()
	// Rejections are constant diagnostics, never request contents. Retain their
	// runner-stream log locally on delivery failure, using the existing retry
	// and source-ID machinery without repeating the producing stage.
	if err := channel.log.Finish(false); err == nil && channel.log.size > 0 {
		_ = channel.log.Deliver(ctx)
	}
	return outcome
}

func (runner *Runner) openStageChannel(ctx context.Context, id string, stage model.Stage, path string) (*stageChannel, error) {
	if stage != model.StagePrepare && stage != model.StageCheck && stage != model.StagePublish {
		return nil, fmt.Errorf("runner: channel is unavailable for this stage")
	}
	log, err := runner.NewLog(id, "runner", nil)
	if err != nil {
		return nil, err
	}
	listener, err := listenStageChannel(path)
	if err != nil {
		_ = log.Finish(false)
		return nil, fmt.Errorf("runner: cannot create stage channel")
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		_ = log.Finish(false)
		return nil, fmt.Errorf("runner: cannot protect stage channel")
	}
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &stageChannel{runner: runner, executionID: id, stage: stage, listener: listener,
		log: log, ctx: serveCtx, cancel: cancel, connections: make(map[net.Conn]struct{})}
	c.workers.Add(1)
	go c.accept()
	return c, nil
}

func (c *stageChannel) accept() {
	defer c.workers.Done()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			conn.Close()
			return
		}
		c.connections[conn] = struct{}{}
		c.workers.Add(1)
		c.mu.Unlock()
		go c.serve(conn)
	}
}

func (c *stageChannel) serve(conn net.Conn) {
	defer c.workers.Done()
	defer func() {
		conn.Close()
		c.mu.Lock()
		delete(c.connections, conn)
		c.mu.Unlock()
	}()
	// ReadSlice bounds memory even when a peer never sends a newline. An LF
	// is required for every request; an unterminated final JSON is rejected.
	reader := bufio.NewReaderSize(conn, channelLineLimit+1)
	encoder := json.NewEncoder(conn)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) || len(line) != 0 {
				c.reject("incomplete or oversized request")
			}
			return
		}
		var request contract.ChannelRequest
		if len(line)-1 > channelLineLimit || json.Unmarshal(line, &request) != nil {
			c.reject("invalid request")
			return
		}
		var reply any
		switch request.Op {
		case "cancel_check":
			c.runner.mu.Lock()
			cancelled := c.runner.cancelledExecutions[c.executionID]
			c.runner.mu.Unlock()
			reply = struct {
				CancelRequested bool `json:"cancelRequested"`
			}{cancelled}
		case "authorize":
			if c.stage != model.StagePublish {
				c.reject("operation forbidden for stage")
				return
			}
			reply = c.runner.Authorize(c.ctx, c.executionID, request.Authorization)
		}
		if encoder.Encode(reply) != nil {
			return
		}
	}
}

func (c *stageChannel) reject(reason string) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return // Closing the stage is not a malformed peer request.
	}
	_ = c.log.appendLine(fmt.Sprintf("channel %s: %s", c.stage, reason))
}

func (c *stageChannel) close() {
	c.mu.Lock()
	c.closed = true
	c.listener.Close()
	for conn := range c.connections {
		conn.Close()
	}
	c.cancel()
	c.mu.Unlock()
	c.workers.Wait()
	_ = c.log.Finish(false)
}
