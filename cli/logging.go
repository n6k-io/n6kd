package cli

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func gcpSeverity(level zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	switch level {
	case zapcore.DebugLevel:
		enc.AppendString("DEBUG")
	case zapcore.InfoLevel:
		enc.AppendString("INFO")
	case zapcore.WarnLevel:
		enc.AppendString("WARNING")
	case zapcore.ErrorLevel:
		enc.AppendString("ERROR")
	case zapcore.DPanicLevel:
		enc.AppendString("CRITICAL")
	case zapcore.PanicLevel:
		enc.AppendString("ALERT")
	default:
		enc.AppendString("EMERGENCY")
	}
}

func installLogger(debug, jsonLogs bool) (*zap.SugaredLogger, error) {
	cfg := zap.NewDevelopmentConfig()
	if jsonLogs {
		cfg = zap.NewProductionConfig()
		// Sampling drops repeats, which is wrong for an operator's log of record.
		cfg.Sampling = nil
		cfg.EncoderConfig.TimeKey = "time"
		cfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
		cfg.EncoderConfig.LevelKey = "severity"
		cfg.EncoderConfig.EncodeLevel = gcpSeverity
		cfg.EncoderConfig.MessageKey = "message"
	}
	if debug {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	} else {
		cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}
	cfg.DisableCaller = true
	cfg.DisableStacktrace = true
	if !jsonLogs {
		cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
	}
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}

	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	zap.ReplaceGlobals(log)
	return log.Sugar(), nil
}
