// Package logger provides shared logging utilities for the Litebase planes.
package logger

import "log"

// Info logs an informational message.
func Info(msg string) {
	log.Println("[INFO]", msg)
}
