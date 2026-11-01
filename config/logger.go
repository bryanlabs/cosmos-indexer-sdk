package config

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

type Logger struct{}

// Log is exposed on the config as a drop-in replacement for our old logger
var Log *Logger

// These functions are provided to reduce refactoring.
func (l *Logger) Debug(msg string, err ...error) {
	if len(err) == 1 {
		zlog.Debug().Err(err[0]).Msg(msg)
		return
	}
	zlog.Debug().Msg(msg)
}

func (l *Logger) Debugf(msg string, args ...interface{}) {
	zlog.Debug().Msg(fmt.Sprintf(msg, args...))
}

func (l *Logger) Info(msg string, err ...error) {
	if len(err) == 1 {
		zlog.Info().Err(err[0]).Msg(msg)
		return
	}
	zlog.Info().Msg(msg)
}

func (l *Logger) Infof(msg string, args ...interface{}) {
	zlog.Info().Msg(fmt.Sprintf(msg, args...))
}

func (l *Logger) Warn(msg string, err ...error) {
	if len(err) == 1 {
		zlog.Warn().Err(err[0]).Msg(msg)
		return
	}
	zlog.Warn().Msg(msg)
}

func (l *Logger) Warnf(msg string, args ...interface{}) {
	zlog.Warn().Msg(fmt.Sprintf(msg, args...))
}

func (l *Logger) Error(msg string, err ...error) {
	if len(err) == 1 {
		zlog.Error().Err(err[0]).Msg(msg)
		return
	}
	zlog.Error().Msg(msg)
}

func (l *Logger) Errorf(msg string, args ...interface{}) {
	zlog.Error().Msg(fmt.Sprintf(msg, args...))
}

func (l *Logger) Fatal(msg string, err ...error) {
	if len(err) == 1 {
		zlog.Fatal().Err(err[0]).Msg(msg)
		return
	}
	zlog.Fatal().Msg(msg)
}

func (l *Logger) Fatalf(msg string, args ...interface{}) {
	zlog.Fatal().Msg(fmt.Sprintf(msg, args...))
}

func DoConfigureLogger(logPath string, logLevel string, prettyLogging bool) {
	writers := io.MultiWriter(os.Stdout)
	if len(logPath) > 0 {
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			file, err := os.Create(logPath)
			if err != nil {
				panic(err)
			}
			writers = io.MultiWriter(os.Stdout, file)
		} else {
			file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
			if err != nil {
				panic(err)
			}
			writers = io.MultiWriter(os.Stdout, file)
		}
	}
	protected := redactingLogWriter{Writer: writers}
	if prettyLogging {
		zlog.Logger = zlog.Output(zerolog.ConsoleWriter{Out: protected})
	} else {
		zlog.Logger = zlog.Output(protected)
	}

	// Set the log level (default to info)
	switch strings.ToLower(logLevel) {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case "fatal":
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	case "panic":
		zerolog.SetGlobalLevel(zerolog.PanicLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}
