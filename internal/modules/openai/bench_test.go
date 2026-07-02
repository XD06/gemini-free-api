package openai

import "testing"

// BenchmarkGenerateChatID measures the new crypto/rand-based ID generation
// to verify it's fast enough for production use.
func BenchmarkGenerateChatID(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = generateChatID()
	}
}
