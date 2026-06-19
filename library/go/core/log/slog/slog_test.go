package slog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.ytsaurus.tech/library/go/core/log"
)

func TestLoggerWritesFields(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: LevelTrace})
	l := New(slog.New(h))

	l.Info("hello", log.String("string", "value"), log.Int("int", 42), log.Bool("bool", true))

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.Equal(t, "hello", entry["msg"])
	require.Equal(t, "INFO", entry["level"])
	require.Equal(t, "value", entry["string"])
	require.Equal(t, float64(42), entry["int"])
	require.Equal(t, true, entry["bool"])
}

func TestWithAndWithName(t *testing.T) {
	var buf bytes.Buffer
	l := New(slog.New(slog.NewJSONHandler(&buf, nil)))
	l = log.With(l, log.String("component", "test")).(*Logger).WithName("group").(*Logger)

	l.Infof("hello %s", "world")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.Equal(t, "hello world", entry["msg"])
	require.Equal(t, "test", entry["component"])
}

func TestTraceAndFatalLevels(t *testing.T) {
	require.Equal(t, LevelTrace, SlogifyLevel(log.TraceLevel))
	require.Equal(t, LevelFatal, SlogifyLevel(log.FatalLevel))
	require.Equal(t, log.TraceLevel, UnslogifyLevel(LevelTrace))
	require.Equal(t, log.FatalLevel, UnslogifyLevel(LevelFatal))
}

func TestLazyCallEvaluatedOnlyWhenEnabled(t *testing.T) {
	var buf bytes.Buffer
	l := New(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	called := false
	l.Debug("debug", log.Lazy("lazy", func() (any, error) {
		called = true
		return "value", nil
	}))
	require.False(t, called)
	require.Empty(t, buf.String())

	l.Info("info", log.Lazy("lazy", func() (any, error) {
		called = true
		return "value", nil
	}))
	require.True(t, called)
	require.Contains(t, buf.String(), "value")
}

func TestContextFieldPassedToHandler(t *testing.T) {
	h := &contextHandler{}
	l := New(slog.New(h))
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")

	l.Info("msg", log.Context(ctx), log.String("key", "value"))

	require.Equal(t, "value", h.ctx.Value(ctxKey{}))
	require.Len(t, h.attrs, 1)
	require.Equal(t, "key", h.attrs[0].Key)
}

func TestFieldTypes(t *testing.T) {
	var buf bytes.Buffer
	l := New(slog.New(slog.NewJSONHandler(&buf, nil)))
	now := time.Unix(10, 20).UTC()

	l.Info("msg", log.Time("time", now), log.Duration("duration", time.Second), log.Stringer("stringer", testStringer("stringer value")))

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	require.Equal(t, now.Format(time.RFC3339Nano), entry["time"])
	require.Equal(t, float64(time.Second), entry["duration"])
	require.Equal(t, "stringer value", entry["stringer"])
}

type ctxKey struct{}

type contextHandler struct {
	ctx   context.Context
	attrs []slog.Attr
}

func (h *contextHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	h.ctx = ctx
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *contextHandler) WithGroup(name string) slog.Handler       { return h }

type testStringer string

func (s testStringer) String() string { return string(s) }
