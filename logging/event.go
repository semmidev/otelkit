package logging

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

type contextKey struct{}

// event accumulates the fields of one canonical request event. Handlers may
// enrich it from other goroutines, so every access is guarded by mu.
type event struct {
	mu      sync.Mutex
	done    bool
	message string
	attrs   []slog.Attr
	index   map[string]int
	err     error
}

func newEvent(message string, fields []slog.Attr) *event {
	ev := &event{message: message}
	for _, attr := range fields {
		ev.addLocked(attr)
	}
	return ev
}

func eventFromContext(ctx context.Context) *event {
	ev, _ := ctx.Value(contextKey{}).(*event)
	return ev
}

func (ev *event) add(attr slog.Attr) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.addLocked(attr)
}

func (ev *event) addLocked(attr slog.Attr) {
	if ev.done {
		return
	}
	if ev.index == nil {
		ev.index = make(map[string]int, len(ev.attrs)+1)
		for i, existing := range ev.attrs {
			ev.index[existing.Key] = i
		}
	}
	if i, ok := ev.index[attr.Key]; ok {
		ev.attrs[i] = attr
		return
	}
	ev.index[attr.Key] = len(ev.attrs)
	ev.attrs = append(ev.attrs, attr)
}

func (ev *event) addArgs(args []any) {
	for len(args) > 0 {
		if len(args) == 1 {
			ev.add(slog.Any("!BADKEY", args[0]))
			return
		}
		key, ok := args[0].(string)
		if !ok {
			ev.add(slog.Any("!BADKEY", args[0]))
			args = args[1:]
			continue
		}
		ev.add(slog.Any(key, args[1]))
		args = args[2:]
	}
}

func (ev *event) setError(err error) {
	if err == nil {
		return
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if ev.done {
		return
	}
	ev.err = err
	ev.addLocked(slog.Group("error",
		slog.String("type", fmt.Sprintf("%T", err)),
		slog.String("message", err.Error()),
	))
}

func (ev *event) getError() error {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return ev.err
}

func (ev *event) setMessage(message string) {
	if message == "" {
		return
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if ev.done {
		return
	}
	ev.message = message
}

func (ev *event) finish(attrs ...slog.Attr) (string, []slog.Attr, error) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if ev.done {
		return ev.message, nil, ev.err
	}
	for _, attr := range attrs {
		ev.addLocked(attr)
	}
	ev.done = true
	return ev.message, append([]slog.Attr(nil), ev.attrs...), ev.err
}
