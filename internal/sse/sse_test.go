package sse

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"CRLF and multiline", "\uFEFF: keepalive\r\nevent: update\r\ndata: one\r\ndata:  two\r\n\r\nid: ignored\r\ndata:tail", []string{"one\n two", "tail"}},
		{"empty payload", "data:\n\n", []string{""}},
		{"no frames", ": ping\nevent: ignored\n\n", nil},
		{"large payload", "data: " + strings.Repeat("x", 128<<10) + "\n\n", []string{strings.Repeat("x", 128<<10)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			stopped, err := Read(context.Background(), strings.NewReader(tc.input), func(s string) bool { got = append(got, s); return true })
			if err != nil || stopped || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stopped=%v err=%v frame count=%d", stopped, err, len(got))
			}
		})
	}
}

type failReader struct{ err error }

func (r failReader) Read([]byte) (int, error) { return 0, r.err }

func TestStopAndErrors(t *testing.T) {
	failure := errors.New("reader failure")
	r := io.MultiReader(strings.NewReader("data: first\n\n"), failReader{failure})
	stopped, err := Read(context.Background(), r, func(string) bool { return false })
	if !stopped || err != nil {
		t.Fatalf("consumer stop: %v %v", stopped, err)
	}
	_, err = Read(context.Background(), failReader{failure}, func(string) bool { return true })
	if !errors.Is(err, failure) {
		t.Fatalf("reader error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Read(ctx, strings.NewReader("data: value\n\n"), func(string) bool { t.Error("callback after cancel"); return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, err = Read(ctx, strings.NewReader("data: value\n\n"), func(string) bool { cancel(); return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel at EOF: %v", err)
	}
	_, err = Read(context.Background(), strings.NewReader("data: "+strings.Repeat("x", 16<<20)), func(string) bool { t.Error("oversized frame delivered"); return true })
	if err == nil {
		t.Fatal("expected oversized line error")
	}
}
