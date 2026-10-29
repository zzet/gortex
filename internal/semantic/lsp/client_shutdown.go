package lsp

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

const (
	lspShutdownBudget  = 2 * time.Second
	lspHandshakeBudget = 500 * time.Millisecond
	lspChildExitGrace  = time.Second
	lspReapReserve     = 250 * time.Millisecond
)

var errLSPStopBudget = errors.New("LSP shutdown exceeded its graceful stop budget")

// Shutdown permanently refuses ordinary requests, sends the owned server's
// shutdown/exit frames before closing the connection, then joins transport and
// reader ownership once. Dialed IDE servers are only disconnected. A timeout
// is diagnostic, never evidence that a still-live owner has been drained.
func (c *Client) Shutdown() error {
	c.shutdownOnce.Do(func() {
		c.shutdownDone = make(chan struct{})
		c.mu.Lock()
		alreadyClosed := c.closed
		c.closing = true
		c.mu.Unlock()
		go func() {
			defer close(c.shutdownDone)
			c.shutdownErr = c.shutdownBefore(time.Now().Add(lspShutdownBudget), alreadyClosed)
		}()
	})
	<-c.shutdownDone
	return c.shutdownErr
}

func (c *Client) markClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.done != nil {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
}

func (c *Client) shutdownBefore(deadline time.Time, alreadyClosed bool) error {
	var handshakeDone chan error
	var budgetErr error
	if !alreadyClosed && c.transport != nil && c.transport.SendsShutdown() {
		handshakeDone = make(chan error, 1)
		go func() {
			err := c.call("shutdown", nil, nil, true, lspHandshakeBudget)
			// Best effort even when the peer refused shutdown. Only this
			// private shutdown path may write while ordinary admission closes.
			exitErr := c.sendFrame(jsonRPCNotification{JSONRPC: "2.0", Method: "exit"}, true)
			handshakeDone <- errors.Join(err, exitErr)
		}()
		timer := time.NewTimer(min(lspHandshakeBudget, max(time.Until(deadline), 0)))
		select {
		case err := <-handshakeDone:
			if c.logger != nil && err != nil {
				c.logger.Debug("LSP: graceful shutdown handshake failed", zap.Error(err))
			}
			handshakeDone = nil
		case <-timer.C:
			budgetErr = fmt.Errorf("%w: shutdown/exit handshake", errLSPStopBudget)
		}
		timer.Stop()
	}
	// Closing the exact owned write handle interrupts a blocked sender without
	// waiting for writeMu or holding the state mutex. No partial stream is reused.
	inputClosed := make(chan error, 1)
	go func() {
		var err error
		if c.stdin != nil {
			err = c.stdin.Close()
		}
		inputClosed <- err
	}()
	c.markClosed()
	var stopErr error
	if bounded, ok := c.transport.(interface{ stopBefore(time.Time) error }); ok {
		stopErr = bounded.stopBefore(deadline)
	} else if c.transport != nil {
		stopErr = c.transport.Stop()
	}
	if handshakeDone != nil {
		// Production transport closure must unblock its owned pipe writer.
		// Retain the actual join even for a custom writer that violates that
		// contract rather than abandoning a goroutine and claiming completion.
		<-handshakeDone
	}
	// A closing fd is not proof that its writer returned. Join the frame
	// currently owning writeMu; queued ordinary senders recheck closed after
	// acquiring it and therefore can never write after this barrier.
	c.writeMu.Lock()
	c.writeMu.Unlock() //nolint:staticcheck // Physical writer-completion barrier after transport closure; queued senders must observe closed.
	if c.readerDone != nil {
		<-c.readerDone
	}
	inputErr := <-inputClosed
	if errors.Is(inputErr, os.ErrClosed) {
		inputErr = nil
	}
	if time.Now().After(deadline) {
		budgetErr = errors.Join(budgetErr, fmt.Errorf("%w: actual client/reader join", errLSPStopBudget))
	}
	return errors.Join(budgetErr, stopErr, inputErr)
}
