package common

import "fmt"

// Logger provides common logging functionality
type Logger struct {
	prefix string
}

// NewLogger creates a new logger with the given prefix
func NewLogger(prefix string) *Logger {
	return &Logger{prefix: prefix}
}

// Log logs a message with the configured prefix
func (l *Logger) Log(message string) {
	// This is the function we'll test changing
	fmt.Printf("[%s] %s\n", l.prefix, message)
}

// LogWithLevel logs a message with a level
func (l *Logger) LogWithLevel(level, message string) {
	// Uses Log internally
	l.Log(fmt.Sprintf("[%s] %s", level, message))
}

// LogMessage is a standalone function for easier testing
// This function is called by both services
func LogMessage(message string) {
	fmt.Printf("[COMMON] %s\n", message)
}

// LogMessageWithPrefix calls LogMessage internally
func LogMessageWithPrefix(prefix, message string) {
	LogMessage(fmt.Sprintf("[%s] %s", prefix, message))
}

// Runner interface - similar to grace.Server interface
type Runner interface {
	Run() error
}

// RunServer runs a server - this is the shared package function
// that acts as a bridge between internal packages and main
func RunServer(r Runner) error {
	fmt.Println("Starting server via common runner...")
	return r.Run()
}
