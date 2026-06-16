package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func New(logLevel string) (*zap.Logger, error) {
	var zapConfig zap.Config
	
	if os.Getenv("APP_ENV") == "production" {
		zapConfig = zap.NewProductionConfig()
		zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	} else {
		zapConfig = zap.NewDevelopmentConfig()
		zapConfig.EncoderConfig.EncodeLevel = zapcore.LowercaseColorLevelEncoder
		zapConfig.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05")
		zapConfig.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
		zapConfig.EncoderConfig.ConsoleSeparator = "|" 
		zapConfig.EncoderConfig.EncodeName = zapcore.FullNameEncoder
	}

	if logLevel != "" {
		if level, err := zapcore.ParseLevel(logLevel); err == nil {
			zapConfig.Level = zap.NewAtomicLevelAt(level)
		}
	}

	return zapConfig.Build()
}
