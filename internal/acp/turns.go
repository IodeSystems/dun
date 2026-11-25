package acp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// Prompts and turns (docs/acp.md, "Prompts while a turn runs"). A
// session/prompt is a dun user message; its response waits for the end of
// the turn that gave the message to the model:
//
//   - sent while idle, it starts a turn (user, not queued): that turn's end
//     answers it;
//   - sent while a turn runs, dun queues it (user, queued) and delivers it
//     into that turn's next tool result (delivered): that turn's end answers
//     it; when the turn ends first, the next turn takes it as its input
//     (delivered, no call): that turn's end answers it;
//   - taken back before the model read it (withdrawn): cancelled.
//
// So one turn's end can answer several prompts, each with the turn's stop
// reason. A prompt is matched to its user event by text, oldest first:
// dun names a message only when it takes it.

// prompt is one session/prompt waiting for its answer.
type prompt struct {
	text   string // the message sent; "" for an answer or a command
	cid    string // the command that sent it: an error ack fails it
	id     string // dun's id for the message, from its user event
	echoed bool
	inTurn bool // the model has it: the running turn's end answers it
	slash  bool // a command: its reply answers it
	gap    bool // in a turn when a snapshot stood in for missed events (gapIdle)
	done   chan outcome
}

type outcome struct {
	stop  sdk.StopReason
	err   error
	usage *sdk.Usage // the turn's totals, when dun said
}

func (p *prompt) resolve(o outcome) {
	select {
	case p.done <- o:
	default:
	}
}

// slashWait is how long a command with no reply is waited for: some
// change a setting and say nothing.
const slashWait = 3 * time.Second

// cancelWait is how long a session/cancel waits for the interrupted turn's
// done before it answers the prompts itself: a cancel never hangs.
var cancelWait = 3 * time.Second

// submit sends a prompt's text: a slash command, the answer to the open
// question, or a user message. It returns the prompt to wait on.
func (s *session) submit(text string, imgs ...proto.Image) (*prompt, error) {
	p := &prompt{done: make(chan outcome, 1)}
	isCmd := s.isCommand(text)
	s.mu.Lock()
	ask := s.ask
	if ask != nil && !isCmd {
		s.ask = nil
	}
	s.mu.Unlock()
	var cmd any
	switch {
	case isCmd:
		p.slash = true
		cmd = &proto.ControlCmd{ID: "slash", Action: strings.TrimPrefix(text, "/")}
		time.AfterFunc(slashWait, func() { s.slashDone(p) })
	case ask != nil:
		// The question the turn waits on: this prompt is its answer, and the
		// turn that goes on with it answers the prompt.
		p.inTurn = true
		cmd = &proto.Answer{Value: text, AskID: ask.AskID}
	default:
		p.text = text
		cmd = &proto.User{Content: text, Images: imgs}
		// A goal chosen in the editor is this prompt's text: the /until
		// line goes first, so the turn it starts is the run's first.
		s.mu.Lock()
		goal := s.goalPrompt(text)
		s.mu.Unlock()
		if goal != nil {
			if err := s.send("", goal); err != nil {
				return nil, err
			}
		}
	}
	p.cid = s.link0().nextCID("p")
	s.mu.Lock()
	s.prompts = append(s.prompts, p)
	s.mu.Unlock()
	if err := s.send(p.cid, cmd); err != nil {
		s.drop(p)
		return nil, err
	}
	return p, nil
}

// link0 is the current link, or a placeholder whose ids are still unique.
func (s *session) link0() *link {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link == nil {
		return &link{}
	}
	return s.link
}

// wait blocks until the prompt is answered or the ACP connection ends.
func (s *session) wait(ctx context.Context, p *prompt) (sdk.PromptResponse, error) {
	select {
	case o := <-p.done:
		if o.err != nil {
			return sdk.PromptResponse{}, o.err
		}
		return sdk.PromptResponse{StopReason: o.stop, Usage: o.usage}, nil
	case <-s.ctx.Done():
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	}
}

func (s *session) drop(p *prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, q := range s.prompts {
		if q == p {
			s.prompts = append(s.prompts[:i], s.prompts[i+1:]...)
			return
		}
	}
}

// finish answers the prompts keep does not keep, and keeps the rest.
// Callers hold no lock.
func (s *session) finish(o outcome, take func(*prompt) bool) {
	s.mu.Lock()
	var done []*prompt
	kept := s.prompts[:0]
	for _, p := range s.prompts {
		if take(p) {
			done = append(done, p)
		} else {
			kept = append(kept, p)
		}
	}
	s.prompts = kept
	s.mu.Unlock()
	for _, p := range done {
		p.resolve(o)
	}
}

// promptParts is the prompt's text and images, its images numbered after
// the session's earlier ones.
func (s *session) promptParts(blocks []sdk.ContentBlock) (string, []proto.Image) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return promptParts(blocks, s.cwd, &s.imgN)
}

// onImageRefused: dun did not send a message because of its images (no
// vision and no describer, or an unreadable file). The reason is shown as
// the agent's text, and the prompt that sent the message ends with it: no
// turn runs for it, so nothing else would answer it.
func (s *session) onImageRefused(r *proto.ImageRefused) {
	s.text(r.Message)
	s.mu.Lock()
	var mine *prompt
	for _, p := range s.prompts {
		if !p.echoed && !p.slash && p.text != "" && strings.TrimSpace(p.text) == strings.TrimSpace(r.Content) {
			mine = p
			break
		}
	}
	s.mu.Unlock()
	if mine != nil {
		s.finish(outcome{stop: sdk.StopReasonEndTurn}, func(p *prompt) bool { return p == mine })
	}
}

// onEcho: dun took a message. The oldest prompt of that text not yet
// matched is its sender; a message from another client is shown as the
// user's.
func (s *session) onEcho(u *proto.UserEcho) {
	s.mu.Lock()
	var mine *prompt
	for _, p := range s.prompts {
		if !p.echoed && !p.slash && p.text != "" && strings.TrimSpace(p.text) == strings.TrimSpace(u.Content) {
			mine = p
			break
		}
	}
	if mine != nil {
		mine.echoed, mine.id = true, u.ID
		if !u.Queued {
			mine.inTurn = true
		}
	}
	s.mu.Unlock()
	if mine == nil {
		s.update(sdk.UpdateUserMessageText(u.Content))
	}
}

func (s *session) onDelivered(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.prompts {
		if p.id != "" && slices.Contains(ids, p.id) {
			p.inTurn = true
		}
	}
}

func (s *session) onWithdrawn(ids []string) {
	s.finish(outcome{stop: sdk.StopReasonCancelled}, func(p *prompt) bool { return p.id != "" && slices.Contains(ids, p.id) })
}

// onTurn: a turn started. A command that started it is answered by its end;
// so are the prompts a gap held (gapIdle): the events are followed again.
func (s *session) onTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamed, s.gap = false, false
	for _, p := range s.prompts {
		p.gap = false
		if p.slash {
			p.slash, p.inTurn = false, true
		}
	}
}

// onDone: the turn ended. Interrupted, or ended after a session/cancel,
// is cancelled; held (an automatic turn that did not start) ends nothing.
func (s *session) onDone(d *proto.Done) {
	if d.Reason == "held" {
		return
	}
	// The engine stopped with a question open (a kill, a restart, a model
	// switch): the turn is not over. The prompt stays held, a form stays
	// open, and the next engine asks the question again (ask.go); the turn
	// that goes on with its answer ends the prompt.
	if d.Reason == "suspended" {
		s.mu.Lock()
		s.suspended = true
		s.mu.Unlock()
		return
	}
	stop := sdk.StopReasonEndTurn
	s.mu.Lock()
	if d.Reason == "interrupted" || s.cancel {
		stop = sdk.StopReasonCancelled
	}
	// A stop condition's run goes on (until.go): its prompts are answered
	// when it stops.
	if hold := stop == sdk.StopReasonEndTurn && s.untilRunning(); hold {
		s.untilHeld, s.streamed, s.ask = true, false, nil
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.endTurn(stop, nil)
}

// onError: the turn failed; its prompts get the error.
func (s *session) onError(e *proto.Error) {
	s.endTurn("", errors.New("dun: "+e.Error))
}

// endTurn answers the prompts the turn held and closes its question.
func (s *session) endTurn(stop sdk.StopReason, err error) {
	s.endPrompts(outcome{stop: stop, err: err}, func(p *prompt) bool { return p.inTurn })
}

// endPrompts ends the turn for the prompts take takes: the turn's state is
// reset, its question closed, and they get o with the turn's usage.
func (s *session) endPrompts(o outcome, take func(*prompt) bool) {
	s.mu.Lock()
	s.cancel, s.streamed, s.ask, s.untilHeld, s.suspended, s.gap = false, false, nil, false, false, false
	stopPerm := s.permStop
	s.mu.Unlock()
	if stopPerm != nil {
		stopPerm()
	}
	s.resendPlan()
	o.usage = s.takeTurnUsage()
	s.finish(o, take)
}

// failAll answers every prompt with err: the session is gone.
func (s *session) failAll(err error) {
	s.finish(outcome{err: err}, func(*prompt) bool { return true })
}

// failCID fails the prompt sent by command cid; false when none was.
func (s *session) failCID(cid string, err error) bool {
	found := false
	s.finish(outcome{err: err}, func(p *prompt) bool {
		if p.cid == cid {
			found = true
			return true
		}
		return false
	})
	return found
}

// slashReply shows a command's answer and answers its prompt.
func (s *session) slashReply(msg string) {
	s.mu.Lock()
	var p *prompt
	for _, q := range s.prompts {
		if q.slash {
			p = q
			break
		}
	}
	s.mu.Unlock()
	if p == nil {
		return
	}
	if strings.TrimSpace(msg) != "" {
		s.text(msg)
	}
	s.slashDone(p)
}

// slashDone answers a command prompt that is still one (no turn took it).
func (s *session) slashDone(p *prompt) {
	s.finish(outcome{stop: sdk.StopReasonEndTurn}, func(q *prompt) bool { return q == p && q.slash })
}

// stop is session/cancel: queued messages of the turn are taken back, the
// turn is interrupted, and every prompt is answered cancelled — at once
// for those dun has not taken, at the turn's end for the rest. That end
// is not waited for past cancelWait, nor when the engine says no turn runs
// (cancelIdle), nor when the interrupt cannot be sent: a cancel never
// hangs, whatever the bridge missed.
func (s *session) stop() {
	s.mu.Lock()
	s.cancel = true
	queued := false
	var held []*prompt
	for _, p := range s.prompts {
		switch {
		case p.inTurn:
			held = append(held, p)
		case p.echoed:
			queued = true
			held = append(held, p)
		}
	}
	s.mu.Unlock()
	s.finish(outcome{stop: sdk.StopReasonCancelled}, func(p *prompt) bool { return !p.echoed && !p.inTurn })
	if queued {
		_ = s.send("", &proto.ControlCmd{ID: "unqueue"})
	}
	if len(held) == 0 {
		s.mu.Lock()
		s.cancel = false // no turn of ours to end
		s.mu.Unlock()
		return
	}
	if err := s.send("", &proto.Interrupt{}); err != nil {
		s.cancelHeld(held)
		return
	}
	time.AfterFunc(cancelWait, func() { s.cancelHeld(held) })
}

// cancelHeld answers cancelled those of held a session/cancel left waiting
// and still waiting.
func (s *session) cancelHeld(held []*prompt) {
	s.mu.Lock()
	left := slices.ContainsFunc(s.prompts, func(p *prompt) bool { return slices.Contains(held, p) })
	s.mu.Unlock()
	if left {
		s.endPrompts(outcome{stop: sdk.StopReasonCancelled}, func(p *prompt) bool { return slices.Contains(held, p) })
	}
}

// cancelIdle: the engine's reply to an interrupt when no turn runs. With a
// session/cancel waiting, its prompts are answered cancelled now (no done
// will come) and the reply is not shown.
func (s *session) cancelIdle() bool {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if !cancel {
		return false
	}
	s.endPrompts(outcome{stop: sdk.StopReasonCancelled}, func(p *prompt) bool { return p.inTurn || p.echoed })
	return true
}

// questionOnly: the session's prompts all wait on a question the engine
// holds (it is open, and each prompt's turn has the model's message), so
// the engine is idle and a restart suspends the question.
func (s *session) questionOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perm == "" && s.ask == nil {
		return false
	}
	for _, p := range s.prompts {
		if !p.inTurn || p.slash {
			return false
		}
	}
	return true
}
