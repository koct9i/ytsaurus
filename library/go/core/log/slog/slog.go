package slog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"go.ytsaurus.tech/library/go/core/log"
)

const callerSkip = 3

const (
	LevelTrace = slog.LevelDebug - 4
	LevelFatal = slog.LevelError + 4
)

// Logger implements log.Logger interface using standard library slog.Logger.
type Logger struct {
	L          *slog.Logger
	callerSkip int
}

var _ log.Logger = &Logger{}
var _ log.Structured = &Logger{}
var _ log.Fmt = &Logger{}
var _ log.LoggerWith = &Logger{}
var _ log.LoggerAddCallerSkip = &Logger{}

// New constructs slog-based logger from provided slog.Logger.
func New(l *slog.Logger) *Logger {
	if l == nil {
		l = slog.Default()
	}
	return &Logger{L: l, callerSkip: callerSkip}
}

// Logger returns general logger.
func (l *Logger) Logger() log.Logger { return l }

// Fmt returns fmt logger.
func (l *Logger) Fmt() log.Fmt { return l }

// Structured returns structured logger.
func (l *Logger) Structured() log.Structured { return l }

// With returns logger that always adds provided key/value to every log entry.
func (l *Logger) With(fields ...log.Field) log.Logger {
	return &Logger{L: l.L.With(slogifyFields(fields)...), callerSkip: l.callerSkip}
}

// AddCallerSkip returns logger that adds caller skip to each log entry.
func (l *Logger) AddCallerSkip(skip int) log.Logger {
	return &Logger{L: l.L, callerSkip: l.callerSkip + skip}
}

// WithName adds name to logger.
func (l *Logger) WithName(name string) log.Logger {
	return &Logger{L: l.L.WithGroup(name), callerSkip: l.callerSkip}
}

func (l *Logger) Trace(msg string, fields ...log.Field) { l.write(LevelTrace, msg, fields...) }
func (l *Logger) Debug(msg string, fields ...log.Field) { l.write(slog.LevelDebug, msg, fields...) }
func (l *Logger) Info(msg string, fields ...log.Field)  { l.write(slog.LevelInfo, msg, fields...) }
func (l *Logger) Warn(msg string, fields ...log.Field)  { l.write(slog.LevelWarn, msg, fields...) }
func (l *Logger) Error(msg string, fields ...log.Field) { l.write(slog.LevelError, msg, fields...) }
func (l *Logger) Fatal(msg string, fields ...log.Field) {
	l.write(LevelFatal, msg, fields...)
	os.Exit(1)
}

func (l *Logger) Tracef(format string, args ...interface{}) { l.writef(LevelTrace, format, args...) }
func (l *Logger) Debugf(format string, args ...interface{}) {
	l.writef(slog.LevelDebug, format, args...)
}
func (l *Logger) Infof(format string, args ...interface{}) { l.writef(slog.LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...interface{}) { l.writef(slog.LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.writef(slog.LevelError, format, args...)
}
func (l *Logger) Fatalf(format string, args ...interface{}) {
	l.writef(LevelFatal, format, args...)
	os.Exit(1)
}

func (l *Logger) writef(level slog.Level, format string, args ...interface{}) {
	if !l.L.Enabled(context.Background(), level) {
		return
	}
	l.write(level, fmt.Sprintf(format, args...))
}

func (l *Logger) write(level slog.Level, msg string, fields ...log.Field) {
	ctx, attrs := slogifyContextAndFields(fields)
	if !l.L.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(l.callerSkip, pcs[:])
	record := slog.NewRecord(time.Now(), level, msg, pcs[0])
	record.AddAttrs(attrs...)
	_ = l.L.Handler().Handle(ctx, record)
}
