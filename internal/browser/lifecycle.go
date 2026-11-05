package browser

// Lazy launch, idle stop and teardown of the browser (0.4.4, B1–B3).
//
// Lifecycle: NewManager launches nothing. The first operation calls beginOp,
// which launches Chromium through the launcher; every operation pins the
// browser (inflight counter) so the idle timer never stops it under a running
// call; endOp re-arms the timer. Stop (idle timer, /new, chat end) and Close
// (Wakil exit) tear the browser down; the next operation after a Stop launches
// a fresh browser in a new profile dir.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
	"github.com/treeol/wakil/internal/safe"
)

// launchTimeout bounds one browser launch (process start + CDP ready +
// first navigation).
const launchTimeout = 30 * time.Second

// stopInflightWait bounds how long Stop waits for running operations before it
// stops the browser under them (those operations then fail with a ctx error).
const stopInflightWait = 5 * time.Second

// session is one running browser: the chromedp contexts and the teardown that
// stops the process group, closes the tunnel and removes the profile dir.
type session struct {
	ctx         context.Context
	ctxCancel   context.CancelFunc
	allocCancel context.CancelFunc
	// teardown stops the browser process(es) and removes the profile dir. It
	// must be safe to call once; errors are reported, not fatal.
	teardown func() error
}

// launcher starts one browser. docker: Chromium inside the container reached
// through a loopback tunnel; local: a host child via chromedp's ExecAllocator.
type launcher interface {
	// launch starts a browser and returns the CDP endpoint URL
	// ("http://127.0.0.1:<port>") the remote allocator connects to, plus the
	// teardown for everything it started. A nil allocator URL means the
	// launcher manages the allocator itself (local ExecAllocator): it then
	// returns a ready *session instead (second result).
	launch(ctx context.Context) (cdpURL string, teardown func() error, ready *session, err error)
}

// beginOp admits one operation: it launches the browser if none is running,
// pins it against the idle timer and returns the browser context plus the
// function that ends the operation. Launch and Stop are serialised.
func (m *Manager) beginOp() (context.Context, func(), error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("browser: manager is closed")
	}
	if m.sess != nil {
		m.pinLocked()
		ctx := m.sess.ctx
		m.mu.Unlock()
		return ctx, m.endOp, nil
	}
	m.mu.Unlock()

	sess, err := m.launchSession()
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	m.sess = sess
	m.ctx = sess.ctx
	m.pinLocked()
	m.mu.Unlock()
	return sess.ctx, m.endOp, nil
}

// pinLocked marks one more operation in flight and stops the idle timer.
// Caller holds m.mu.
func (m *Manager) pinLocked() {
	m.inflight++
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
}

// endOp ends one operation and re-arms the idle timer when none remain.
func (m *Manager) endOp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight > 0 {
		m.inflight--
	}
	if m.inflight == 0 && m.sess != nil && !m.closed {
		m.armIdleLocked()
	}
}

func (m *Manager) armIdleLocked() {
	if m.idle != nil {
		m.idle.Stop()
	}
	sess := m.sess
	m.idle = time.AfterFunc(m.idleTimeout, func() { m.idleStop(sess) })
}

// idleStop is the timer callback: stop the browser iff it is still the same
// session, nothing is in flight and no launch is running.
func (m *Manager) idleStop(sess *session) {
	safe.Go("browser-idle-stop", func() {
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
		m.mu.Lock()
		if m.sess != sess || m.inflight > 0 || m.closed {
			m.mu.Unlock()
			return
		}
		m.sess, m.ctx, m.idle = nil, nil, nil
		m.mu.Unlock()
		_ = sess.close()
	})
}

// launchSession runs one launch under launchTimeout. Caller holds lifeMu. Any
// partial start is torn down before the error is returned.
func (m *Manager) launchSession() (*session, error) {
	lctx, cancel := context.WithTimeout(context.Background(), launchTimeout)
	defer cancel()
	cdpURL, teardown, ready, err := m.launcher.launch(lctx)
	if err != nil {
		if teardown != nil {
			_ = teardown()
		}
		return nil, err
	}
	if ready != nil {
		return ready, nil
	}

	// Remote allocator over the loopback endpoint. The first chromedp.Run uses
	// the manager-owned context directly (never an operation context): see the
	// note on newLocalLauncher — canceling a context used for the first Run can
	// tear the browser down.
	allocCtx, allocCancel := remote.NewAllocator(context.Background(), cdpURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	sess := &session{ctx: browserCtx, ctxCancel: browserCancel, allocCancel: allocCancel, teardown: teardown}

	done := make(chan error, 1)
	safe.Go("browser-launch", func() { _, err := chromedp.Run(browserCtx, chromedp.Navigate("about:blank")); done <- err })
	select {
	case err := <-done:
		if err != nil {
			_ = sess.close()
			return nil, fmt.Errorf("cannot connect to Chromium at %s (%w)", cdpURL, err)
		}
	case <-lctx.Done():
		_ = sess.close()
		return nil, fmt.Errorf("timed out connecting to Chromium at %s", cdpURL)
	}
	return sess, nil
}

// close cancels the chromedp contexts and runs the launcher's teardown.
func (s *session) close() error {
	if s == nil {
		return nil
	}
	if s.ctxCancel != nil {
		s.ctxCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	if s.teardown != nil {
		return s.teardown()
	}
	return nil
}

// Stop stops the browser (if running) and removes its profile dir; the next
// operation launches a fresh one. It waits up to stopInflightWait for running
// operations, then stops the browser under them. Safe on a nil Manager.
func (m *Manager) Stop() error {
	if m == nil {
		return nil
	}
	return m.stop(false)
}

func (m *Manager) stop(final bool) error {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if final {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
	}
	// Give running operations a bounded chance to finish.
	deadline := time.Now().Add(stopInflightWait)
	for {
		m.mu.Lock()
		n := m.inflight
		m.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	sess := m.sess
	m.sess, m.ctx = nil, nil
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
	m.mu.Unlock()
	return sess.close()
}

// Running reports whether a browser is currently launched (for tests and
// status lines).
func (m *Manager) Running() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess != nil
}

// Launcher is the seam NewManagerForTest uses: a function that launches a
// browser session. Production code never uses it.
type Launcher interface {
	// Launch starts a session; Teardown (returned) stops it.
	Launch(ctx context.Context) (teardown func() error, err error)
}

// NewManagerForTest returns a Manager whose browser is the given launcher's
// fake (no Chromium, no executor). It exists so other packages' tests can
// assert that lifecycle hooks (/new, chat end) stop the browser.
func NewManagerForTest(l Launcher, idle time.Duration) *Manager {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	return &Manager{launcher: testLauncher{l}, idleTimeout: idle}
}

// TouchForTest launches the browser (as a tool call would) and ends the op.
func (m *Manager) TouchForTest() error {
	_, end, err := m.beginOp()
	if err != nil {
		return err
	}
	end()
	return nil
}

type testLauncher struct{ l Launcher }

func (t testLauncher) launch(ctx context.Context) (string, func() error, *session, error) {
	td, err := t.l.Launch(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	bctx, cancel := context.WithCancel(context.Background())
	return "", nil, &session{ctx: bctx, ctxCancel: cancel, teardown: td}, nil
}
