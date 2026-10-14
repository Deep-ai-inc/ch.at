package main

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const protocolReplyLimit = 64 << 10

type protocolAnswer struct {
	done chan struct{}
	text []byte // Immutable after done closes.
}

// Like HTTP, charge one request to the real peer IP; like SSH, generate only
// one answer at a time in a session. No accounts or separate quota system.
func startProtocolAnswer(parent context.Context, peer, prompt string, llm llmFunc) (*protocolAnswer, error) {
	if len(prompt) > boardMaxURL || !utf8.ValidString(prompt) || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt must be nonempty UTF-8, at most 8192 bytes")
	}
	if !rateLimitAllow(peer) {
		return nil, errors.New("rate limit exceeded; retry later")
	}
	answer := &protocolAnswer{done: make(chan struct{})}
	go func() {
		defer close(answer.done)
		ctx, cancel := context.WithTimeout(parent, 60*time.Second)
		defer cancel()
		ch, result := streamLLM(ctx, prompt, llm)
		var text strings.Builder
		for {
			select {
			case chunk, ok := <-ch:
				if !ok {
					select {
					case err := <-result:
						if err != nil {
							answer.text = []byte("Error: model request failed.\n")
							return
						}
					case <-ctx.Done():
						answer.text = []byte("Error: request timed out.\n")
						return
					}
					answer.text = []byte(text.String())
					return
				}
				remaining := protocolReplyLimit - text.Len()
				if len(chunk) >= remaining {
					text.WriteString(chunk[:remaining])
					s := text.String()
					for !utf8.ValidString(s) && len(s) > 0 {
						s = s[:len(s)-1]
					}
					answer.text = []byte(s)
					return
				}
				text.WriteString(chunk)
			case <-ctx.Done():
				answer.text = []byte("Error: request timed out.\n")
				return
			}
		}
	}()
	return answer, nil
}
