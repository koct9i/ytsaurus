package slog

import (
	"context"
	"fmt"
	"log/slog"

	"go.ytsaurus.tech/library/go/core/log"
)

// SlogifyLevel turns interface log level to slog log level.
func SlogifyLevel(level log.Level) slog.Level {
	switch level {
	case log.TraceLevel:
		return LevelTrace
	case log.DebugLevel:
		return slog.LevelDebug
	case log.InfoLevel:
		return slog.LevelInfo
	case log.WarnLevel:
		return slog.LevelWarn
	case log.ErrorLevel:
		return slog.LevelError
	case log.FatalLevel:
		return LevelFatal
	default:
		panic(fmt.Sprintf("unknown log level: %d", level))
	}
}

// UnslogifyLevel turns slog log level to interface log level.
func UnslogifyLevel(level slog.Level) log.Level {
	switch {
	case level <= LevelTrace:
		return log.TraceLevel
	case level < slog.LevelInfo:
		return log.DebugLevel
	case level < slog.LevelWarn:
		return log.InfoLevel
	case level < slog.LevelError:
		return log.WarnLevel
	case level < LevelFatal:
		return log.ErrorLevel
	default:
		return log.FatalLevel
	}
}

func slogifyContextAndFields(fields []log.Field) (context.Context, []slog.Attr) {
	ctx := context.Background()
	attrs := make([]slog.Attr, 0, len(fields))
	for _, field := range fields {
		switch field.Type() {
		case log.FieldTypeContext:
			ctx = field.Interface().(context.Context)
		case log.FieldTypeRawContext, log.FieldTypeSkip:
			continue
		default:
			attrs = append(attrs, slogifyField(field))
		}
	}
	return ctx, attrs
}

func slogifyFields(fields []log.Field) []any {
	attrs := make([]any, 0, len(fields))
	for _, field := range fields {
		if field.Type() == log.FieldTypeContext || field.Type() == log.FieldTypeRawContext || field.Type() == log.FieldTypeSkip {
			continue
		}
		attrs = append(attrs, slogifyField(field))
	}
	return attrs
}

func slogifyField(field log.Field) slog.Attr {
	switch field.Type() {
	case log.FieldTypeNil:
		return slog.Any(field.Key(), nil)
	case log.FieldTypeString:
		return slog.String(field.Key(), field.String())
	case log.FieldTypeBinary, log.FieldTypeByteString:
		return slog.Any(field.Key(), field.Binary())
	case log.FieldTypeBoolean:
		return slog.Bool(field.Key(), field.Bool())
	case log.FieldTypeSigned:
		return slog.Int64(field.Key(), field.Signed())
	case log.FieldTypeUnsigned:
		return slog.Uint64(field.Key(), field.Unsigned())
	case log.FieldTypeFloat:
		return slog.Float64(field.Key(), field.Float())
	case log.FieldTypeTime:
		return slog.Time(field.Key(), field.Time())
	case log.FieldTypeDuration:
		return slog.Duration(field.Key(), field.Duration())
	case log.FieldTypeStringer:
		return slog.String(field.Key(), field.Interface().(fmt.Stringer).String())
	case log.FieldTypeLazyCall:
		return slog.Any(field.Key(), lazyValue{fn: field.Interface()})
	default:
		return slog.Any(field.Key(), field.Any())
	}
}

type lazyValue struct{ fn any }

func (l lazyValue) LogValue() slog.Value {
	switch fn := l.fn.(type) {
	case func() any:
		return slog.AnyValue(fn())
	case func() (any, error):
		v, err := fn()
		if err != nil {
			return slog.AnyValue(err)
		}
		return slog.AnyValue(v)
	default:
		return slog.AnyValue(fn)
	}
}
