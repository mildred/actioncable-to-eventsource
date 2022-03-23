package main

import (
	"log"
)

type StandardLibLogger struct {
	Logger     *log.Logger
	DebugLevel bool
	InfoLevel  bool
}

func NewStandardLibLogger(l *log.Logger) *StandardLibLogger {
	return &StandardLibLogger{Logger: l}
}

func (l *StandardLibLogger) Debug(message string) {
	if l.DebugLevel {
		l.Logger.Println(message)
	}
}

func (l *StandardLibLogger) Debugf(message string, args ...interface{}) {
	if l.DebugLevel {
		l.Logger.Printf(message, args...)
	}
}

func (l *StandardLibLogger) Info(message string) {
	if l.InfoLevel {
		l.Logger.Println(message)
	}
}

func (l *StandardLibLogger) Infof(message string, args ...interface{}) {
	if l.InfoLevel {
		l.Logger.Printf(message, args...)
	}
}

func (l *StandardLibLogger) Warn(message string) {
	l.Logger.Println(message)
}

func (l *StandardLibLogger) Warnf(message string, args ...interface{}) {
	l.Logger.Printf(message, args...)
}

func (l *StandardLibLogger) Error(message string) {
	l.Logger.Println(message)
}

func (l *StandardLibLogger) Errorf(message string, args ...interface{}) {
	l.Logger.Printf(message, args...)
}
