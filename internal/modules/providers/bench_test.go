package providers

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildBenchStreamLine builds a stream line without requiring *testing.T.
func buildBenchStreamLine(payloads ...[]interface{}) string {
	entries := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		payloadJSON, _ := json.Marshal(payload)
		entryJSON, _ := json.Marshal([]interface{}{"wrb.fr", nil, string(payloadJSON)})
		entries = append(entries, string(entryJSON))
	}
	return fmt.Sprintf("[%s]", strings.Join(entries, ","))
}

// makeLargeStreamBuffer creates a ~256 KB stream buffer with many lines
// containing text, simulating a long Gemini response.
func makeLargeStreamBuffer(numLines int) []byte {
	var b strings.Builder
	b.WriteString(")]}'\n\n")
	for i := 0; i < numLines; i++ {
		text := strings.Repeat("A", 256)
		line := buildBenchStreamLine([]interface{}{
			nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
				[]interface{}{"rc_1", []interface{}{text}, nil, nil, nil, nil, true},
			},
		})
		b.WriteString(line)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// BenchmarkExtractStreamTextFromBuffer measures the cost of parsing a
// large stream buffer for text extraction — the hot path in streaming.
func BenchmarkExtractStreamTextFromBuffer(b *testing.B) {
	buf := makeLargeStreamBuffer(500) // ~256 KB
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractStreamTextFromBuffer(buf)
	}
}

// BenchmarkExtractConversationMetadataFromBuffer measures metadata extraction
// from a large stream buffer.
func BenchmarkExtractConversationMetadataFromBuffer(b *testing.B) {
	buf := makeLargeStreamBuffer(500)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractConversationMetadataFromBuffer(buf)
	}
}

// BenchmarkRecentBytesCopy measures the recentBytes slicing operation.
func BenchmarkRecentBytesCopy(b *testing.B) {
	buf := makeLargeStreamBuffer(500)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = recentBytes(buf, maxStreamParseBufferBytes)
	}
}

// BenchmarkPruneConversationsLocked measures conversation pruning with
// a moderate number of entries (1000).
func BenchmarkPruneConversationsLocked(b *testing.B) {
	now := time.Now()
	conversations := make(map[string]*SessionMetadata, 1000)
	conversationSeen := make(map[string]time.Time, 1000)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("thread-%d", i)
		conversations[id] = &SessionMetadata{ConversationID: fmt.Sprintf("c_%d", i)}
		// Half expired, half fresh
		if i%2 == 0 {
			conversationSeen[id] = now.Add(-conversationCacheTTL - time.Minute)
		} else {
			conversationSeen[id] = now
		}
	}
	client := &Client{
		conversations:     conversations,
		conversationSeen:  conversationSeen,
		conversationMu:    sync.RWMutex{},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset state for each iteration since prune mutates the maps
		if i > 0 {
			// Rebuild for subsequent iterations
			for j := 0; j < 1000; j++ {
				id := fmt.Sprintf("thread-%d", j)
				if _, ok := client.conversations[id]; !ok {
					client.conversations[id] = &SessionMetadata{ConversationID: fmt.Sprintf("c_%d", j)}
					if j%2 == 0 {
						client.conversationSeen[id] = now.Add(-conversationCacheTTL - time.Minute)
					} else {
						client.conversationSeen[id] = now
					}
				}
			}
		}
		client.conversationMu.Lock()
		client.pruneConversationsLocked(now)
		client.conversationMu.Unlock()
	}
}

// BenchmarkHasConversationStateRLock measures the read-path cost of
// HasConversationState (which now uses RLock instead of Lock).
func BenchmarkHasConversationStateRLock(b *testing.B) {
	conversations := make(map[string]*SessionMetadata, 1000)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("thread-%d", i)
		conversations[id] = &SessionMetadata{ConversationID: fmt.Sprintf("c_%d", i)}
	}
	client := &Client{
		conversations:  conversations,
		conversationMu: sync.RWMutex{},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.HasConversationState("thread-500")
	}
}


