package lsp

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

// stopBefore shares the client's total budget, including its handshake. Stop
// only signals the os.Process returned by this transport's own cmd.Start; no
// numeric PID lookup, process-name match or unverified group signal is used.
func (s *SpawnTransport) stopBefore(deadline time.Time) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		s.stopDone = make(chan struct{})
		go func() {
			defer close(s.stopDone)
			s.stopErr = s.stopChild(deadline)
		}()
	})
	<-s.stopDone
	return s.stopErr
}

func (s *SpawnTransport) stopChild(deadline time.Time) error {
	// Closing a pipe can wait behind an OS write. Start it as an owned
	// operation, independently of the child escalation timer (notably Windows).
	inputClosed := make(chan error, 1)
	go func() {
		var err error
		if s.stdin != nil {
			err = s.stdin.Close()
		}
		inputClosed <- err
	}()
	joinInput := func() error {
		err := <-inputClosed
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return err
	}
	finish := func(waitErr error) error {
		inputErr := joinInput()
		if time.Now().After(deadline) {
			return errors.Join(fmt.Errorf("%w: actual child/pipe join", errLSPStopBudget), waitErr, inputErr)
		}
		return errors.Join(waitErr, inputErr)
	}
	waited := make(chan error, 1)
	go func() { waited <- s.cmd.Wait() }() // The only Wait for this exact child.
	grace := min(lspChildExitGrace, max(time.Until(deadline)-lspReapReserve, 0))
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waited:
		return finish(err)
	case <-timer.C:
	}
	// The child may have exited just as the grace timer fired.
	select {
	case err := <-waited:
		return finish(err)
	default:
	}
	killErr := s.cmd.Process.Kill()
	killed := killErr == nil
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil // Natural exit raced escalation; still join its actual Wait.
	}
	waitErr := <-waited
	if s.Logger != nil {
		s.Logger.Warn("LSP: owned child grace expired; reap complete",
			zap.Int("pid", s.cmd.Process.Pid), zap.String("command", s.Command),
			zap.Bool("killed", killed), zap.Error(errors.Join(killErr, waitErr)))
	}
	// An OS reap can outlive the budget. It must stay an actual ownership join,
	// with an error receipt, never a timeout reported as successful completion.
	return errors.Join(fmt.Errorf("%w: owned child pid %d", errLSPStopBudget, s.cmd.Process.Pid), killErr, waitErr, joinInput())
}
