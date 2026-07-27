package agent

import (
	"context"
	"fmt"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/safe"
)

// SideQuestionID is a unique identifier for a side-question stream.
type SideQuestionID string

// StartSideQuestion launches a concurrent side-question stream on a cloned
// proxy.Client. It snapshots the current Conv (sanitized to the last complete
// protocol boundary), appends the user's question, and streams the response
// via EventSink as SideQuestionChunkMsg / SideQuestionDoneMsg. The side
// question does NOT modify Conv — it's ephemeral, for the user only.
//
// Returns a cancellation function the caller can use to abort the stream.
// The TUI should call it when the user hits Esc or the main turn ends.
//
// Safety:
//   - proxy.Client.Stream is safe for concurrent calls on DIFFERENT client
//     instances (dispatchSubagent already does this).
//   - Conv is snapshotted under convMu.RLock before the goroutine starts.
//   - tools=nil — side questions don't invoke tools.
//   - Egress consent is checked before starting (same gate as the main turn).
func (a *App) StartSideQuestion(ctx context.Context, question string) context.CancelFunc {
	sideCtx, cancel := context.WithCancel(ctx)

	// Snapshot Conv under lock.
	convSnapshot := a.ConvSnapshot()

	// Append the user's question to the snapshot (not to a.Conv).
	msgs := append(convSnapshot, proxy.Message{
		Role:    "user",
		Content: StrPtr(question),
	})

	// Clone the proxy client (same pattern as dispatchSubagent).
	sideClient := a.cloneClientForSideQuestion()
	if sideClient == nil {
		a.sendEvent(SideQuestionDoneMsg{Err: fmt.Errorf("no client available for side question")})
		return cancel
	}

	// Check egress consent — same gate as the main turn.
	if a.SelectedBackend != "" && IsExternalBackend(a.BackendList, a.Cfg, a.SelectedBackend) {
		if a.consentedBackends == nil || !a.consentedBackends[a.SelectedBackend] {
			a.sendEvent(SideQuestionDoneMsg{Err: fmt.Errorf("external backend %q not consented — use /backend to consent first", a.SelectedBackend)})
			return cancel
		}
	}

	id := SideQuestionID(NewChatID())

	safe.Go("side-question", func() {
		// Stream with tools=nil — side questions don't invoke tools.
		sink := func(chunk string) {
			a.sendEvent(SideQuestionChunkMsg{ID: id, Text: chunk})
		}
		_, err := sideClient.Stream(sideCtx, msgs, nil, sink, nil)
		a.sendEvent(SideQuestionDoneMsg{ID: id, Err: err})

		// Record cost from the side-question client.
		if a.Costs != nil && sideClient != nil {
			u := sideClient.LastUsage()
			if u.InputTok > 0 || u.OutputTok > 0 {
				source := proxy.CostSourceInference
				usd, priced := a.Cfg.Costs.InferenceCost(u.InputTok + u.OutputTok)
				a.Costs.Record(source, u.InputTok, u.OutputTok, usd, priced, proxy.ConfModeled, config.TokenDetail{})
			}
		}
	})

	return cancel
}

// cloneClientForSideQuestion creates a fresh *proxy.Client pointing at the same
// backend as the parent, sharing only the goroutine-safe *http.Client transport.
// Same pattern as dispatchSubagent (subagent.go:810-829).
func (a *App) cloneClientForSideQuestion() *proxy.Client {
	if a.Client == nil {
		return nil
	}
	return &proxy.Client{
		BaseURL:         a.Client.BaseURL,
		HTTP:            a.Client.HTTP,
		Model:           a.Client.Model,
		AuthHeader:      a.Client.AuthHeader,
		Backend:         a.Client.Backend,
		Kind:            a.Client.Kind,
		ConfiguredModel: a.Client.ConfiguredModel,
		AuxModel:        a.Client.AuxModel,
		Temperature:     a.Client.Temperature,
		TopP:            a.Client.TopP,
		MaxTokens:       a.Client.MaxTokens,
		CachePrompt:     a.Client.CachePrompt,
		CacheControl:    a.Client.CacheControl,
		AppReferer:      a.Client.AppReferer,
		AppTitle:        a.Client.AppTitle,
		AppCategories:   a.Client.AppCategories,
		NoMemoryWrite:   true,
		MaxRequestBytes: a.Client.MaxRequestBytes,
		ChatID:          NewChatID(),
	}
}
