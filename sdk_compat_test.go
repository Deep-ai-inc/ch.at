package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// Optional consumer-contract test using the same adapter family as issue #49.
// Install test-only packages in a scratch directory, not production dependencies:
// npm install --prefix .sdk-check --ignore-scripts ai @ai-sdk/openai-compatible
// CHAT_SDK_CHECK_DIR="$PWD/.sdk-check" go test -tags boardtest -run TestOpenAICompatibleSDK
func TestOpenAICompatibleSDK(t *testing.T) {
	dir := os.Getenv("CHAT_SDK_CHECK_DIR")
	if dir == "" {
		t.Skip("optional AI SDK consumer test; set CHAT_SDK_CHECK_DIR")
	}
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleChatCompletionsWithLLM(w, r, fixedChatLLM(&calls, "hello", " world"))
	}))
	defer s.Close()
	file, err := os.CreateTemp(dir, "compat-*.mjs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(`import assert from 'node:assert/strict';
import { streamText, generateText } from 'ai';
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';
const provider = createOpenAICompatible({ name: 'chat', baseURL: process.argv[2] + '/v1' });
const model = provider('default');
const streamed = streamText({ model, prompt: 'hello', maxRetries: 0 });
assert.equal(await streamed.text, 'hello world');
assert.equal(await streamed.finishReason, 'stop');
const plain = await generateText({ model, prompt: 'hello', maxRetries: 0 });
assert.equal(plain.text, 'hello world');
assert.equal(plain.finishReason, 'stop');
console.log('AI SDK streaming and non-streaming both terminate with stop');
`)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "node", file.Name(), s.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("SDK contract failed: %v\n%s", err, output)
	}
	if calls.Load() != 2 {
		t.Fatalf("SDK made %d model calls, want 2", calls.Load())
	}
	t.Log(string(output))
}
