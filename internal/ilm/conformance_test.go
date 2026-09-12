package ilm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConformance replays the fetched fixture sessions through the emitter
// → POST /v1/events → POST /v1/conformance/check. Expects zero diffs.
//
// This test requires a live ilm-stack at $ILM_ENDPOINT with $ILM_TOKEN.
// It is skipped when those env vars are not set.
func TestConformance(t *testing.T) {
	endpoint := os.Getenv("ILM_ENDPOINT")
	token := os.Getenv("ILM_TOKEN")
	if endpoint == "" || token == "" {
		t.Skip("ILM_ENDPOINT/ILM_TOKEN not set — skipping conformance test")
	}

	// Load the fixture files.
	fixtureDir := filepath.Join("..", "..", "testdata", "ilm-stack", "fixtures")
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Skipf("no fixtures directory: %v", err)
	}

	// Read all fixtures into a single array and send as the conformance body.
	var allFixtures []json.RawMessage
	totalSent := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			path := filepath.Join(fixtureDir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v", entry.Name(), err)
			}

			var fixture struct {
				SessionID string  `json:"session_id"`
				Events    []Event `json:"events"`
			}
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatalf("unmarshal fixture %s: %v", entry.Name(), err)
			}

			// Send events in batches via /v1/events.
			batchSize := 64
			for i := 0; i < len(fixture.Events); i += batchSize {
				end := i + batchSize
				if end > len(fixture.Events) {
					end = len(fixture.Events)
				}
				batch := fixture.Events[i:end]

				body, _ := json.Marshal(struct {
					Events []Event `json:"events"`
				}{Events: batch})

				req, _ := http.NewRequest("POST", endpoint+"/v1/events", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("POST events for %s: %v", fixture.SessionID, err)
				}
				var result struct {
					Inserted   int      `json:"inserted"`
					Duplicates int      `json:"duplicates"`
					Rejected   []string `json:"rejected"`
				}
				json.NewDecoder(resp.Body).Decode(&result)
				resp.Body.Close()
				totalSent += result.Inserted
				if len(result.Rejected) > 0 {
					t.Errorf("rejected events: %v", result.Rejected)
				}
			}

			// Collect the raw fixture for the conformance check body.
			allFixtures = append(allFixtures, json.RawMessage(data))
		}
	}

	t.Logf("sent %d events", totalSent)

	// Wait for the server to process the events.
	time.Sleep(500 * time.Millisecond)

	// Call conformance check with the raw fixtures array as the body.
	confBody, _ := json.Marshal(allFixtures)
	req, _ := http.NewRequest("POST", endpoint+"/v1/conformance/check", bytes.NewReader(confBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conformance check: %v", err)
	}
	defer resp.Body.Close()

	var confResult struct {
		Passed          bool            `json:"passed"`
		Diffs           json.RawMessage `json:"diffs"`
		SessionsChecked int             `json:"sessions_checked"`
		TotalDiffs      int             `json:"total_diffs"`
	}
	json.NewDecoder(resp.Body).Decode(&confResult)

	t.Logf("conformance result: passed=%v sessions_checked=%d total_diffs=%d",
		confResult.Passed, confResult.SessionsChecked, confResult.TotalDiffs)
	t.Logf("diffs: %s", string(confResult.Diffs))

	if !confResult.Passed {
		t.Errorf("conformance check FAILED: %s", string(confResult.Diffs))
	}
}
