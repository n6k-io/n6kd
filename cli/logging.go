package cli

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// gcpSeverity maps zap levels onto Cloud Logging's severity values, which is
// what makes a JSON line render at the right level in the GCP console.
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
	// The file:line of the log statement is noise for an operator; the message
	// says where it came from. A stack trace on an error line is the same.
	cfg.DisableCaller = true
	cfg.DisableStacktrace = true
	if !jsonLogs {
		cfg.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
	}
	// stderr for both, so stdout stays free for anything a later subcommand
	// wants to emit as data.
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}

	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	// Installed globally too, so a package reaching for zap.L() rather than
	// taking a logger still writes in the same format.
	zap.ReplaceGlobals(log)
	return log.Sugar(), nil
}
