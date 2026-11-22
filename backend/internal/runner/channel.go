package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	log         *Log
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	requests    sync.WaitGroup
}

var _ sandbox.ChannelHandler = (*stageChannel)(nil)

// A stage executor returns only after the entire sandbox (including its
// channel relay sidecar) has stopped. Keep the handler open until then,
// including while heartbeat cancellation is being handled, so cancel_check
// can observe the instruction rather than ctx.Err().
func (runner *Runner) runStageWithChannel(ctx context.Context, id string, in StageInvocation, executor StageExecutor) StageOutcome {
	if in.Stage == model.StageWork {
		return executor.RunStage(ctx, in)
	}
	channel, err := runner.openStageChannel(ctx, id, in.Stage)
	if err != nil {
		code := "sandbox_error"
		return StageOutcome{RunnerError: &code}
	}
	defer channel.close()
	in.Channel = channel
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

func (runner *Runner) openStageChannel(ctx context.Context, id string, stage model.Stage) (*stageChannel, error) {
	if stage != model.StagePrepare && stage != model.StageCheck && stage != model.StagePublish {
		return nil, fmt.Errorf("runner: channel is unavailable for this stage")
	}
	log, err := runner.NewLog(id, "runner", nil)
	if err != nil {
		return nil, err
	}
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &stageChannel{runner: runner, executionID: id, stage: stage, log: log, ctx: serveCtx, cancel: cancel}, nil
}

// HandleChannelRequest answers one request line relayed by the stage's
// channel sidecar (spec 09 3.6). nil closes the stage's connection without
// a reply. The sidecar checked only the line's form; every rule of spec 05
// 3.7 is applied here again.
func (c *stageChannel) HandleChannelRequest(line []byte) []byte {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.requests.Add(1)
	c.mu.Unlock()
	defer c.requests.Done()
	var request contract.ChannelRequest
	if len(line) > channelLineLimit || bytes.ContainsAny(line, "\n") || json.Unmarshal(line, &request) != nil {
		c.reject("invalid request")
		return nil
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
			return nil
		}
		reply = c.runner.Authorize(c.ctx, c.executionID, request.Authorization)
	default:
		c.reject("invalid request")
		return nil
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	data, err := json.Marshal(reply)
	if closed || err != nil {
		return nil // A closed stage never answers, even an in-flight request.
	}
	return data
}

func (c *stageChannel) reject(reason string) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return // Closing the stage is not a malformed peer request.
	}
	_ = c.log.appendLine(fmt.Sprintf("channel %s: %s", c.stage, reason), false)
}

func (c *stageChannel) close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.requests.Wait()
	_ = c.log.Finish(false)
}
